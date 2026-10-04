package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/pulls"
	"onegit/internal/store"
)

const pullsPerPage = 50

func pullPath(id int64, sub ...string) string {
	p := "/pulls/" + strconv.FormatInt(id, 10)
	for _, s := range sub {
		p += "/" + s
	}
	return p
}

// pullFail shows validation errors as a flash on the given page and
// everything else as a server error.
func (w *Web) pullFail(rw http.ResponseWriter, r *http.Request, back string, err error) {
	var ue *pulls.UserError
	if errors.As(err, &ue) {
		w.redirectFlash(rw, r, back, ue.Msg)
		return
	}
	w.fail(rw, r, err)
}

func (w *Web) loadPull(rw http.ResponseWriter, r *http.Request) *store.Pull {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		w.notFound(rw, r)
		return nil
	}
	p, err := w.Store.PullByID(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return nil
	}
	return p
}

// ---- list ----

func (w *Web) pullList(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	if state != "closed" {
		state = "open"
	}
	project, dir := w.projectDir(r)
	pq := pageQuery(r, pullsPerPage)
	list, err := w.Store.FindPulls(ctx, store.PullFilter{State: state, Dir: dir, Page: pq})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	list, pg := paginate(list, pullsPerPage, pq, func(p *store.Pull) int64 { return p.ID })
	open, closed, err := w.Store.CountPullsIn(ctx, dir)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	ids := make([]int64, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	queued, err := w.Store.QueuedPulls(ctx, ids)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	projectList, _, _ := w.Projects.List(ctx)
	w.render(rw, r, http.StatusOK, "pulls", &Page{Title: "Pull requests · " + w.Cfg.Repo.Name, Tab: "pulls", Data: map[string]any{
		"Pulls": list, "State": state, "Open": open, "Closed": closed, "Pager": pg,
		"Project": project, "Projects": projectList, "Queued": queued,
	}})
}

// ---- create ----

func (w *Web) pullNew(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !currentUser(r).CanWrite() {
		w.errorPage(rw, r, http.StatusForbidden, "You need write access to open pull requests.")
		return
	}
	q := r.URL.Query()
	base, head := q.Get("base"), q.Get("head")
	if base == "" {
		base = w.defaultBranch(ctx)
	}
	// Suggestions only: any branch can be typed in.
	branches, _, err := w.Repo.ListRefs(ctx, git.KindBranch, git.RefQuery{Limit: 200})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	data := map[string]any{"Branches": branches, "Base": base, "Head": head}
	render := func() {
		w.render(rw, r, http.StatusOK, "pull_new", &Page{Title: "New pull request · " + w.Cfg.Repo.Name, Tab: "pulls", Data: data})
	}
	if head == "" || head == base {
		render()
		return
	}
	baseSHA, err1 := w.Repo.ResolveCommit(ctx, "refs/heads/"+base)
	headSHA, err2 := w.Repo.ResolveCommit(ctx, "refs/heads/"+head)
	if err1 != nil || err2 != nil {
		data["Invalid"] = true
		render()
		return
	}
	if existing, err := w.Store.OpenPullFor(ctx, head, base); err == nil {
		data["Existing"] = existing
	}
	mb, err := w.Repo.MergeBase(ctx, baseSHA, headSHA)
	if err != nil {
		data["Unrelated"] = true
		render()
		return
	}
	commits, err := w.Repo.Log(ctx, mb+".."+headSHA, git.LogOptions{Limit: 250})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	data["Commits"] = commits
	if len(commits) == 0 {
		render()
		return
	}
	if mr, err := w.Pulls.MergeCheck(ctx, baseSHA, headSHA); err == nil {
		data["Conflicts"] = mr.Conflicts
		data["Checked"] = true
	}
	diff, err := w.Repo.DiffRange(ctx, mb, headSHA, git.DiffOptions{})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	data["Diff"] = diff
	title, body := humanizeBranch(head), ""
	if len(commits) == 1 {
		title, body = commits[0].Subject, commits[0].Body
	}
	data["Title"], data["Body"] = title, body
	render()
}

func humanizeBranch(b string) string {
	if i := strings.LastIndex(b, "/"); i >= 0 {
		b = b[i+1:]
	}
	b = strings.NewReplacer("-", " ", "_", " ").Replace(b)
	if b == "" {
		return b
	}
	return strings.ToUpper(b[:1]) + b[1:]
}

func (w *Web) pullCreate(rw http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	head, base := r.FormValue("head"), r.FormValue("base")
	back := "/pulls/new?base=" + url.QueryEscape(base) + "&head=" + url.QueryEscape(head)
	if !u.CanWrite() {
		w.errorPage(rw, r, http.StatusForbidden, "You need write access to open pull requests.")
		return
	}
	p, err := w.Pulls.Create(r.Context(), u, head, base, r.FormValue("title"), r.FormValue("body"))
	if err != nil {
		w.pullFail(rw, r, back, err)
		return
	}
	w.CI.OnPull(r.Context(), p, &u.ID)
	http.Redirect(rw, r, pullPath(p.ID), http.StatusSeeOther)
}

// ---- view ----

type thread struct {
	Path      string
	Side      string
	Line      int
	CommitSHA string
	DiffHunk  string
	Outdated  bool
	Comments  []*store.PullComment
}

func (t *thread) key() string { return lineKey(t.Path, t.Side, t.Line) }

func lineKey(path, side string, line int) string {
	return path + "\x00" + side + "\x00" + strconv.Itoa(line)
}

type timelineItem struct {
	When    time.Time
	Comment *store.PullComment
	Thread  *thread
	Review  *store.PullReview
	Event   *store.PullEvent
}

func (w *Web) pullView(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	view := "conversation"
	switch {
	case strings.HasSuffix(r.URL.Path, "/commits"):
		view = "commits"
	case strings.HasSuffix(r.URL.Path, "/files"):
		view = "files"
	}
	u := currentUser(r)
	data := map[string]any{
		"Pull": p, "View": view,
		"CanWrite":  u.CanWrite(),
		"CanEdit":   u.CanWrite() || p.IsAuthor(u),
		"IsAuthor":  p.IsAuthor(u),
		"HeadAlive": w.Pulls.HeadBranchExists(ctx, p),
	}
	data["CommitCount"], _ = w.Repo.CountCommits(ctx, p.MergeBase, p.HeadSHA)
	data["FileCount"], _ = w.Repo.CountChangedFiles(ctx, p.MergeBase, p.HeadSHA)

	comments, err := w.Store.ListPullComments(ctx, p.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	reviews, err := w.Store.ListPullReviews(ctx, p.ID)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	threads := w.buildThreads(ctx, p, comments)

	switch view {
	case "conversation":
		events, err := w.Store.ListPullEvents(ctx, p.ID)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		data["Timeline"] = buildTimeline(comments, threads, reviews, events)
		if p.IsOpen() {
			st, err := w.Pulls.Status(ctx, p, reviews)
			if err != nil {
				w.fail(rw, r, err)
				return
			}
			data["Status"] = st
			type styleOpt struct {
				Style       pulls.MergeStyle
				Title, Body string
			}
			var styles []styleOpt
			for _, s := range pulls.MergeStyles {
				t, b := w.Pulls.DefaultMessage(ctx, p, s)
				styles = append(styles, styleOpt{s, t, b})
			}
			data["Styles"] = styles
			if e, err := w.Store.ActiveQueueEntry(ctx, p.ID); err == nil {
				data["Queue"] = e
				if list, err := w.Store.ActiveQueue(ctx, p.BaseBranch); err == nil {
					for i, x := range list {
						if x.ID == e.ID {
							data["QueuePos"], data["QueueLen"] = i+1, len(list)
						}
					}
				}
				if e.TestSHA != "" {
					data["QueueChecks"], _ = w.Store.CommitStatuses(ctx, e.TestSHA)
				}
			}
		}
	case "commits":
		commits, err := w.Repo.Log(ctx, p.MergeBase+".."+p.HeadSHA, git.LogOptions{Limit: 250})
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		data["Commits"] = commits
	case "files":
		diff, err := w.Repo.DiffRange(ctx, p.MergeBase, p.HeadSHA, git.DiffOptions{})
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		data["Diff"] = diff
		current := map[string][]*thread{}
		for _, t := range threads {
			if !t.Outdated {
				current[t.key()] = append(current[t.key()], t)
			}
		}
		data["Threads"] = current
	}
	w.render(rw, r, http.StatusOK, "pull", &Page{Title: p.Title + " · #" + strconv.FormatInt(p.ID, 10), Tab: "pulls", Data: data})
}

// buildThreads groups line comments by anchor. A thread is outdated when
// the file has changed since the comment's commit.
func (w *Web) buildThreads(ctx context.Context, p *store.Pull, comments []*store.PullComment) []*thread {
	byKey := map[string]*thread{}
	var list []*thread
	var specs []string
	for _, c := range comments {
		if !c.IsLine() {
			continue
		}
		k := lineKey(*c.Path, *c.Side, *c.Line) + "\x00" + *c.CommitSHA
		t, ok := byKey[k]
		if !ok {
			t = &thread{Path: *c.Path, Side: *c.Side, Line: *c.Line, CommitSHA: *c.CommitSHA, DiffHunk: c.DiffHunk}
			byKey[k] = t
			list = append(list, t)
			if t.CommitSHA != p.HeadSHA {
				specs = append(specs, t.CommitSHA+":"+t.Path, p.HeadSHA+":"+t.Path)
			}
		}
		t.Comments = append(t.Comments, c)
	}
	blobs := w.Repo.BlobIDs(ctx, specs)
	for _, t := range list {
		if t.CommitSHA != p.HeadSHA {
			then, now := blobs[t.CommitSHA+":"+t.Path], blobs[p.HeadSHA+":"+t.Path]
			t.Outdated = then == "" || then != now
		}
	}
	return list
}

func buildTimeline(comments []*store.PullComment, threads []*thread, reviews []*store.PullReview, events []*store.PullEvent) []timelineItem {
	var items []timelineItem
	for _, c := range comments {
		if !c.IsLine() {
			items = append(items, timelineItem{When: c.CreatedAt, Comment: c})
		}
	}
	for _, t := range threads {
		items = append(items, timelineItem{When: t.Comments[0].CreatedAt, Thread: t})
	}
	for _, r := range reviews {
		items = append(items, timelineItem{When: r.CreatedAt, Review: r})
	}
	for _, e := range events {
		items = append(items, timelineItem{When: e.CreatedAt, Event: e})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].When.Before(items[j].When) })
	return items
}

// ---- actions ----

func (w *Web) pullComment(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	body := strings.TrimSpace(r.FormValue("body"))
	back := pullPath(p.ID)
	if body != "" {
		c := &store.PullComment{PullID: p.ID, AuthorID: &u.ID, Body: body}
		if path := r.FormValue("path"); path != "" {
			back = pullPath(p.ID, "files")
			side, commit := r.FormValue("side"), r.FormValue("commit")
			line, _ := strconv.Atoi(r.FormValue("line"))
			if side != "old" && side != "new" || line <= 0 {
				w.redirectFlash(rw, r, back, "Invalid comment position.")
				return
			}
			if commit == "" {
				commit = p.HeadSHA
			}
			hunk, ok := w.lineContext(ctx, p, commit, path, side, line)
			if !ok {
				w.redirectFlash(rw, r, back, "That line is not part of the diff.")
				return
			}
			c.Path, c.Side, c.Line, c.CommitSHA, c.DiffHunk = &path, &side, &line, &commit, hunk
		}
		if err := w.Store.AddPullComment(ctx, c); err != nil {
			w.fail(rw, r, err)
			return
		}
		back += "#comment-" + strconv.FormatInt(c.ID, 10)
	}
	switch r.FormValue("action") {
	case "close", "reopen":
		if !u.CanWrite() && !p.IsAuthor(u) {
			w.errorPage(rw, r, http.StatusForbidden, "You cannot change the state of this pull request.")
			return
		}
		reopen := r.FormValue("action") == "reopen"
		if err := w.Pulls.SetState(ctx, p.ID, u, reopen); err != nil {
			w.pullFail(rw, r, pullPath(p.ID), err)
			return
		}
		if reopen {
			if fresh, err := w.Store.PullByID(ctx, p.ID); err == nil {
				w.CI.OnPull(ctx, fresh, &u.ID)
			}
		}
	}
	http.Redirect(rw, r, back, http.StatusSeeOther)
}

// lineContext validates that side/line exists in the PR diff as of commit
// and returns up to four lines of context ending at it (as "+", "-" or " "
// prefixed lines) to keep with the comment.
func (w *Web) lineContext(ctx context.Context, p *store.Pull, commit, path, side string, line int) (string, bool) {
	if commit != p.HeadSHA {
		// Replies to older threads reuse the thread's anchor; trust it only
		// if such a thread exists.
		comments, err := w.Store.ListPullComments(ctx, p.ID)
		if err != nil {
			return "", false
		}
		for _, c := range comments {
			if c.IsLine() && *c.CommitSHA == commit && *c.Path == path && *c.Side == side && *c.Line == line {
				return c.DiffHunk, true
			}
		}
		return "", false
	}
	diff, err := w.Repo.DiffRange(ctx, p.MergeBase, p.HeadSHA, git.DiffOptions{Paths: []string{path}})
	if err != nil {
		return "", false
	}
	for _, f := range diff.Files {
		if f.Path() != path && f.OldPath != path {
			continue
		}
		for _, h := range f.Hunks {
			for i, l := range h.Lines {
				// Deleted lines are addressed on the old side, all others on the new.
				if (side == "old") != (l.Kind == git.LineDel) || lineNo(l) != line {
					continue
				}
				var b strings.Builder
				for _, cl := range h.Lines[max(0, i-3) : i+1] {
					b.WriteByte(byte(cl.Kind))
					b.WriteString(cl.Content)
					b.WriteByte('\n')
				}
				return b.String(), true
			}
		}
	}
	return "", false
}

func lineNo(l git.DiffLine) int {
	if l.Kind == git.LineDel {
		return l.OldNo
	}
	return l.NewNo
}

func pathMatchValid(pattern string) (bool, error) { return path.Match(pattern, "") }

func (w *Web) pullDeleteComment(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	cid, _ := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	c, err := w.Store.PullCommentByID(r.Context(), p.ID, cid)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	u := currentUser(r)
	if !u.IsAdmin() && (c.AuthorID == nil || *c.AuthorID != u.ID) {
		w.errorPage(rw, r, http.StatusForbidden, "You can only delete your own comments.")
		return
	}
	if err := w.Store.DeletePullComment(r.Context(), p.ID, cid); err != nil {
		w.fail(rw, r, err)
		return
	}
	back := pullPath(p.ID)
	if c.IsLine() {
		back = pullPath(p.ID, "files")
	}
	w.redirectFlash(rw, r, back, "Comment deleted.")
}

func (w *Web) pullReview(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	back := pullPath(p.ID)
	if !p.IsOpen() {
		w.redirectFlash(rw, r, back, "This pull request is not open.")
		return
	}
	state := store.ReviewState(r.FormValue("state"))
	body := strings.TrimSpace(r.FormValue("body"))
	switch state {
	case store.ReviewApproved, store.ReviewChangesRequested:
		if !u.CanWrite() {
			w.redirectFlash(rw, r, back, "Only users with write access can approve or request changes.")
			return
		}
		if p.IsAuthor(u) {
			w.redirectFlash(rw, r, back, "You cannot approve or request changes on your own pull request.")
			return
		}
		if state == store.ReviewChangesRequested && body == "" {
			w.redirectFlash(rw, r, back, "Please explain which changes you are requesting.")
			return
		}
	case store.ReviewCommented:
		if body == "" {
			w.redirectFlash(rw, r, back, "A review comment cannot be empty.")
			return
		}
	default:
		w.redirectFlash(rw, r, back, "Choose a review type.")
		return
	}
	rev := &store.PullReview{PullID: p.ID, ReviewerID: &u.ID, State: state, Body: body, CommitSHA: p.HeadSHA}
	if err := w.Store.AddPullReview(r.Context(), rev); err != nil {
		w.fail(rw, r, err)
		return
	}
	w.Pulls.Kick() // an approval may complete auto-merge or a queued PR
	http.Redirect(rw, r, back+"#review-"+strconv.FormatInt(rev.ID, 10), http.StatusSeeOther)
}

func (w *Web) pullMerge(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	back := pullPath(p.ID)
	merged, err := w.Pulls.Merge(r.Context(), p.ID, u, pulls.MergeOptions{
		Style: pulls.MergeStyle(r.FormValue("style")), Title: r.FormValue("title"), Message: r.FormValue("message"),
	})
	if err != nil {
		w.pullFail(rw, r, back, err)
		return
	}
	msg := "Pull request merged."
	if r.FormValue("delete_branch") == "on" {
		if err := w.Pulls.DeleteHeadBranch(r.Context(), merged, u); err != nil {
			var ue *pulls.UserError
			if !errors.As(err, &ue) {
				w.fail(rw, r, err)
				return
			}
			msg += " " + ue.Msg
		} else {
			msg += " Branch " + merged.HeadBranch + " deleted."
		}
	}
	w.redirectFlash(rw, r, back, msg)
}

func mergeOptions(r *http.Request) pulls.MergeOptions {
	return pulls.MergeOptions{Style: pulls.MergeStyle(r.FormValue("style")), Title: r.FormValue("title"), Message: r.FormValue("message")}
}

func (w *Web) pullAutoMerge(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	var err error
	msg := "Auto-merge enabled: the pull request merges once every requirement is met."
	if r.FormValue("action") == "disable" {
		if !u.CanWrite() && !p.IsAuthor(u) {
			w.errorPage(rw, r, http.StatusForbidden, "You cannot change auto-merge here.")
			return
		}
		err, msg = w.Pulls.DisableAutoMerge(r.Context(), p.ID, u), "Auto-merge disabled."
	} else {
		err = w.Pulls.EnableAutoMerge(r.Context(), p.ID, u, mergeOptions(r), r.FormValue("delete_branch") == "on")
	}
	if err != nil {
		w.pullFail(rw, r, pullPath(p.ID), err)
		return
	}
	w.redirectFlash(rw, r, pullPath(p.ID), msg)
}

func (w *Web) pullQueue(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	if r.FormValue("action") == "remove" {
		if err := w.Pulls.Dequeue(r.Context(), p.ID, u); err != nil {
			w.pullFail(rw, r, pullPath(p.ID), err)
			return
		}
		w.redirectFlash(rw, r, pullPath(p.ID), "Removed from the merge queue.")
		return
	}
	if err := w.Pulls.Enqueue(r.Context(), p.ID, u, mergeOptions(r), r.FormValue("delete_branch") == "on"); err != nil {
		w.pullFail(rw, r, pullPath(p.ID), err)
		return
	}
	w.redirectFlash(rw, r, pullPath(p.ID), "Added to the merge queue.")
}

// mergeQueue shows every branch's queue and recent results.
func (w *Web) mergeQueue(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	branches, err := w.Store.QueueBranches(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	type queueView struct {
		Branch  string
		Entries []*store.QueueEntry
		Checks  map[int64][]*store.CommitStatus
	}
	var queues []queueView
	for _, b := range branches {
		list, err := w.Store.ActiveQueue(ctx, b)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		q := queueView{Branch: b, Entries: list, Checks: map[int64][]*store.CommitStatus{}}
		for _, e := range list {
			if e.TestSHA != "" {
				q.Checks[e.ID], _ = w.Store.CommitStatuses(ctx, e.TestSHA)
			}
		}
		queues = append(queues, q)
	}
	recent, err := w.Store.RecentQueueEntries(ctx, 30)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	open, closed, _ := w.Store.CountPulls(ctx)
	w.render(rw, r, http.StatusOK, "merge_queue", &Page{Title: "Merge queue · " + w.Cfg.Repo.Name, Tab: "pulls", Data: map[string]any{
		"Queues": queues, "Recent": recent, "Open": open, "Closed": closed,
	}})
}

func (w *Web) pullDeleteBranch(rw http.ResponseWriter, r *http.Request) {
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	if err := w.Pulls.DeleteHeadBranch(r.Context(), p, currentUser(r)); err != nil {
		w.pullFail(rw, r, pullPath(p.ID), err)
		return
	}
	w.redirectFlash(rw, r, pullPath(p.ID), "Branch "+p.HeadBranch+" deleted.")
}

func (w *Web) pullEdit(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := w.loadPull(rw, r)
	if p == nil {
		return
	}
	u := currentUser(r)
	if !u.CanWrite() && !p.IsAuthor(u) {
		w.errorPage(rw, r, http.StatusForbidden, "You cannot edit this pull request.")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		w.redirectFlash(rw, r, pullPath(p.ID), "Title is required.")
		return
	}
	var oldTitle string
	_, err := w.Store.UpdatePullLocked(ctx, p.ID, func(p *store.Pull) error {
		oldTitle = p.Title
		p.Title = title
		if _, ok := r.Form["body"]; ok {
			p.Body = strings.TrimSpace(r.FormValue("body"))
		}
		return nil
	})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if oldTitle != title {
		w.Store.AddPullEvent(ctx, p.ID, &u.ID, "retitled", map[string]any{"from": oldTitle, "to": title})
	}
	http.Redirect(rw, r, pullPath(p.ID), http.StatusSeeOther)
}

// ---- admin: branch protection ----

func (w *Web) adminBranches(rw http.ResponseWriter, r *http.Request) {
	rules, err := w.Store.ListBranchProtections(r.Context())
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "admin_branches", &Page{Title: "Branch protection", Tab: "admin", Data: map[string]any{
		"Rules": rules, "Default": w.defaultBranch(r.Context()),
	}})
}

func (w *Web) adminSaveBranch(rw http.ResponseWriter, r *http.Request) {
	b := &store.BranchProtection{
		Pattern:                 strings.TrimSpace(r.FormValue("pattern")),
		RequirePullRequest:      r.FormValue("require_pull_request") == "on",
		DismissStaleApprovals:   r.FormValue("dismiss_stale_approvals") == "on",
		RequireCodeOwnerReview:  r.FormValue("require_code_owner_review") == "on",
		BlockOnChangesRequested: r.FormValue("block_on_changes_requested") == "on",
		AllowForcePush:          r.FormValue("allow_force_push") == "on",
		AllowDeletion:           r.FormValue("allow_deletion") == "on",
		RequireMergeQueue:       r.FormValue("require_merge_queue") == "on",
		Owners:                  strings.TrimSpace(strings.ReplaceAll(r.FormValue("owners"), "\r\n", "\n")),
	}
	for _, c := range strings.Split(r.FormValue("required_checks"), "\n") {
		if c = strings.TrimSpace(c); c != "" {
			b.RequiredChecks = append(b.RequiredChecks, c)
		}
	}
	b.ID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, err := strconv.Atoi(r.FormValue("required_approvals"))
	if err != nil || n < 0 || n > 20 {
		w.redirectFlash(rw, r, "/admin/branches", "Required approvals must be a number between 0 and 20.")
		return
	}
	b.RequiredApprovals = n
	if v := r.FormValue("merge_queue_depth"); v != "" {
		d, err := strconv.Atoi(v)
		if err != nil || d < 1 || d > 50 {
			w.redirectFlash(rw, r, "/admin/branches", "Merge queue depth must be a number between 1 and 50.")
			return
		}
		b.MergeQueueDepth = d
	}
	if b.Pattern == "" || strings.ContainsAny(b.Pattern, " \t") {
		w.redirectFlash(rw, r, "/admin/branches", "Enter a branch name or pattern.")
		return
	}
	if _, err := pathMatchValid(b.Pattern); err != nil {
		w.redirectFlash(rw, r, "/admin/branches", "Invalid pattern: "+err.Error())
		return
	}
	if err := w.Store.SaveBranchProtection(r.Context(), b); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			w.redirectFlash(rw, r, "/admin/branches", "A rule for "+b.Pattern+" already exists.")
			return
		}
		w.fail(rw, r, err)
		return
	}
	w.Pulls.Kick()
	w.redirectFlash(rw, r, "/admin/branches", "Rule for "+b.Pattern+" saved.")
}

func (w *Web) adminDeleteBranch(rw http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := w.Store.DeleteBranchProtection(r.Context(), id); err != nil {
		w.fail(rw, r, err)
		return
	}
	w.redirectFlash(rw, r, "/admin/branches", "Rule deleted.")
}
