package web

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"onegit/internal/git"
	"onegit/internal/projects"
	"onegit/internal/store"
)

const (
	maxRenderSize  = 1 << 20 // files larger than this are not highlighted
	commitsPerPage = 50
)

type refView struct {
	Ref  git.Ref
	Path string
}

func (w *Web) home(rw http.ResponseWriter, r *http.Request) {
	if w.Repo.IsEmpty(r.Context()) {
		w.render(rw, r, http.StatusOK, "empty", &Page{Title: w.Cfg.Repo.Name, Tab: "code"})
		return
	}
	r.SetPathValue("rest", w.defaultBranch(r.Context()))
	w.tree(rw, r)
}

// resolve parses "{ref}/{path}" from the rest path value.
func (w *Web) resolve(r *http.Request) (refView, error) {
	ref, p, err := w.Repo.SplitRefPath(r.Context(), r.PathValue("rest"))
	return refView{Ref: ref, Path: p}, err
}

type treeRow struct {
	Entry  git.TreeEntry
	Commit *git.Commit
}

func (w *Web) tree(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := w.resolve(r)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	switch w.Repo.ObjectType(ctx, rv.Ref.SHA, rv.Path) {
	case "tree":
	case "blob":
		http.Redirect(rw, r, "/blob/"+pathEscape(rv.Ref.Name, rv.Path), http.StatusFound)
		return
	default:
		w.notFound(rw, r)
		return
	}
	entries, err := w.Repo.ListTree(ctx, rv.Ref.SHA, rv.Path)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	last := w.lastCommits(ctx, rv.Ref.SHA, rv.Path, entries)
	rows := make([]treeRow, len(entries))
	var readme *git.TreeEntry
	for i, e := range entries {
		rows[i] = treeRow{Entry: e, Commit: last[e.Name]}
		if readme == nil && !e.IsDir() && isReadme(e.Name) {
			readme = &entries[i]
		}
	}
	head, _ := w.Repo.LastCommitFor(ctx, rv.Ref.SHA, rv.Path)

	var readmeHTML template.HTML
	if readme != nil && readme.Size < maxRenderSize {
		if b, err := w.Repo.ReadBlob(ctx, rv.Ref.SHA, readme.Path, maxRenderSize); err == nil {
			if isMarkdown(readme.Name) {
				readmeHTML = renderMarkdown(b)
			} else {
				readmeHTML = template.HTML("<pre>" + template.HTMLEscapeString(string(b)) + "</pre>")
			}
		}
	}
	owners := w.Projects.Owners(ctx, rv.Ref.SHA, rv.Path, true)
	var project *projects.Project
	if list, _, err := w.Projects.List(ctx); err == nil {
		for _, p := range list {
			if rv.Path == p.Dir || strings.HasPrefix(rv.Path, p.Dir+"/") {
				project = &p
			}
		}
	}
	w.render(rw, r, http.StatusOK, "tree", &Page{
		Title: titleFor(w.Cfg.Repo.Name, rv.Path), Tab: "code",
		Data: map[string]any{
			"Ref": rv.Ref, "Path": rv.Path, "Rows": rows, "Head": head,
			"Readme": readme, "ReadmeHTML": readmeHTML, "Owners": owners, "Project": project,
		},
	})
}

// lastCommits is cached in Redis per commit+dir: history for a given commit
// never changes, so entries only expire to bound memory.
func (w *Web) lastCommits(ctx context.Context, commit, dir string, entries []git.TreeEntry) map[string]*git.Commit {
	key := "lastcommits:" + commit + ":" + dir
	var cached map[string]*git.Commit
	if ok, err := w.KV.GetJSON(ctx, key, &cached); ok && err == nil {
		return cached
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	// The listing is useful without the column: don't wait long for a slot.
	release, err := w.acquireHeavy(ctx, 2*time.Second)
	if err != nil {
		return nil
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := w.Repo.LastCommits(ctx, commit, dir, names, 5000)
	if err != nil {
		return nil
	}
	if ctx.Err() != nil {
		return res // partial result after timeout: show it, but don't cache
	}
	if err := w.KV.SetJSON(ctx, key, res, 7*24*time.Hour); err != nil {
		w.Log.Warn("cache last commits", "err", err)
	}
	return res
}

func (w *Web) blob(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := w.resolve(r)
	if err != nil || rv.Path == "" {
		w.notFound(rw, r)
		return
	}
	if w.Repo.ObjectType(ctx, rv.Ref.SHA, rv.Path) == "tree" {
		http.Redirect(rw, r, "/tree/"+pathEscape(rv.Ref.Name, rv.Path), http.StatusFound)
		return
	}
	size, err := w.Repo.BlobSize(ctx, rv.Ref.SHA, rv.Path)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	name := path.Base(rv.Path)
	data := map[string]any{"Ref": rv.Ref, "Path": rv.Path, "Name": name, "Size": size}
	commit, _ := w.Repo.LastCommitFor(ctx, rv.Ref.SHA, rv.Path)
	data["Commit"] = commit
	data["Owners"] = w.Projects.Owners(ctx, rv.Ref.SHA, rv.Path, false)

	switch {
	case isImage(name):
		data["Image"] = true
	case size > maxRenderSize:
		data["TooLarge"] = true
	default:
		b, err := w.Repo.ReadBlob(ctx, rv.Ref.SHA, rv.Path, maxRenderSize)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		if git.IsBinary(b) {
			data["Binary"] = true
			break
		}
		lines := bytes.Count(b, []byte("\n"))
		if len(b) > 0 && b[len(b)-1] != '\n' {
			lines++
		}
		data["Lines"] = lines
		if isMarkdown(name) && r.URL.Query().Get("plain") == "" {
			data["Markdown"] = renderMarkdown(b)
		} else {
			data["Code"] = highlight(name, b)
		}
		data["IsMarkdown"] = isMarkdown(name)
	}
	w.render(rw, r, http.StatusOK, "blob", &Page{Title: titleFor(w.Cfg.Repo.Name, rv.Path), Tab: "code", Data: data})
}

func (w *Web) raw(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := w.resolve(r)
	if err != nil || rv.Path == "" || w.Repo.ObjectType(ctx, rv.Ref.SHA, rv.Path) != "blob" {
		w.notFound(rw, r)
		return
	}
	name := path.Base(rv.Path)
	ctype := "text/plain; charset=utf-8"
	if isImage(name) {
		ctype = mime.TypeByExtension(path.Ext(name))
	}
	h := rw.Header()
	h.Set("Content-Type", ctype)
	// Never let repository content execute in our origin.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	if r.URL.Query().Get("download") != "" {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	if rv.Ref.Kind == git.KindCommit {
		h.Set("Cache-Control", "public, max-age=86400, immutable")
	}
	if size, err := w.Repo.BlobSize(ctx, rv.Ref.SHA, rv.Path); err == nil {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if err := w.Repo.StreamBlob(ctx, rv.Ref.SHA, rv.Path, rw); err != nil && ctx.Err() == nil {
		w.Log.Warn("stream blob", "path", rv.Path, "err", err)
	}
}

type commitGroup struct {
	Day     string
	Commits []*git.Commit
}

func (w *Web) commits(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := w.resolve(r)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	// Later pages stay on the commit the first page showed, so pushes in
	// between don't shift them.
	at := rv.Ref.SHA
	if a := r.URL.Query().Get("at"); a != "" && page > 1 {
		if sha, err := w.Repo.ResolveCommit(ctx, a); err == nil {
			at = sha
		}
	}
	list, err := w.Repo.Log(ctx, at, git.LogOptions{
		Path: rv.Path, Skip: (page - 1) * commitsPerPage, Limit: commitsPerPage + 1,
	})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	hasNext := len(list) > commitsPerPage
	if hasNext {
		list = list[:commitsPerPage]
	}
	var groups []*commitGroup
	for _, c := range list {
		day := c.Committer.When.Format("Jan 2, 2006")
		if len(groups) == 0 || groups[len(groups)-1].Day != day {
			groups = append(groups, &commitGroup{Day: day})
		}
		g := groups[len(groups)-1]
		g.Commits = append(g.Commits, c)
	}
	w.render(rw, r, http.StatusOK, "commits", &Page{Title: "Commits · " + w.Cfg.Repo.Name, Tab: "commits", Data: map[string]any{
		"Ref": rv.Ref, "Path": rv.Path, "Groups": groups, "Page": page, "HasNext": hasNext, "At": at,
	}})
}

func (w *Web) commit(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sha, err := w.Repo.ResolveCommit(ctx, r.PathValue("sha"))
	if err != nil {
		w.notFound(rw, r)
		return
	}
	c, err := w.Repo.GetCommit(ctx, sha)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	diff, err := w.Repo.DiffCommit(ctx, c, git.DiffOptions{})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "commit", &Page{Title: c.Subject + " · " + c.Short(), Tab: "commits", Data: map[string]any{
		"Commit": c, "Diff": diff,
	}})
}

const refsPerPage = 30

type branchRow struct {
	git.Ref
	Ahead, Behind int
	IsDefault     bool
	Pull          *store.Pull // open PR from this branch, if any
}

func (w *Web) branches(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	def := w.defaultBranch(ctx)
	defRefs, err := w.Repo.RefsByName(ctx, git.KindBranch, []string{def})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	// The default branch is shown on its own, so page through the others.
	list, hasNext, err := w.Repo.ListRefs(ctx, git.KindBranch, git.RefQuery{
		Query: q, Exclude: def, Offset: (page - 1) * refsPerPage, Limit: refsPerPage,
	})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	names := make([]string, len(list))
	for i, b := range list {
		names[i] = b.Name
	}
	byHead, err := w.Store.OpenPullsByHead(ctx, names)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	var counts map[string][2]int
	defRef, hasDef := defRefs[def]
	if hasDef {
		if counts, err = w.Repo.AheadBehindMany(ctx, defRef.SHA, names); err != nil {
			w.Log.Warn("ahead/behind", "err", err)
		}
	}
	rows := make([]branchRow, len(list))
	for i, b := range list {
		rows[i] = branchRow{Ref: b, Ahead: counts[b.Name][0], Behind: counts[b.Name][1]}
		if ps := byHead[b.Name]; len(ps) > 0 {
			rows[i].Pull = ps[0]
		}
	}
	var defRow *branchRow
	if hasDef && q == "" && page == 1 {
		defRow = &branchRow{Ref: defRef, IsDefault: true}
	}
	w.render(rw, r, http.StatusOK, "branches", &Page{Title: "Branches · " + w.Cfg.Repo.Name, Tab: "branches", Data: map[string]any{
		"Default": defRow, "Branches": rows, "Q": q, "Page": page, "HasNext": hasNext,
	}})
}

func (w *Web) tags(rw http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	list, hasNext, err := w.Repo.ListRefs(r.Context(), git.KindTag, git.RefQuery{Query: q, Offset: (page - 1) * refsPerPage, Limit: refsPerPage})
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "tags", &Page{Title: "Tags · " + w.Cfg.Repo.Name, Tab: "tags", Data: map[string]any{
		"Tags": list, "Q": q, "Page": page, "HasNext": hasNext,
	}})
}

// refMenuSize is how many branches and tags the ref picker shows per kind.
const refMenuSize = 50

type refLink struct {
	Name string `json:"name"`
	Href string `json:"href"`
}

// refsJSON feeds the ref picker: branches and tags matching ?q=, linking to
// the same ?path= under the ?view= ("tree" or "commits").
func (w *Web) refsJSON(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qv := r.URL.Query()
	view := qv.Get("view")
	if view != "commits" {
		view = "tree"
	}
	out := map[string]any{}
	more := false
	for key, kind := range map[string]git.RefKind{"branches": git.KindBranch, "tags": git.KindTag} {
		refs, m, err := w.Repo.ListRefs(ctx, kind, git.RefQuery{Query: strings.TrimSpace(qv.Get("q")), Limit: refMenuSize})
		if err != nil {
			http.Error(rw, "internal error", http.StatusInternalServerError)
			return
		}
		links := make([]refLink, len(refs))
		for i, ref := range refs {
			links[i] = refLink{Name: ref.Name, Href: "/" + view + "/" + pathEscape(ref.Name, qv.Get("path"))}
		}
		out[key], more = links, more || m
	}
	out["more"] = more
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(rw).Encode(out)
}

func titleFor(repo, p string) string {
	if p == "" {
		return repo
	}
	return p + " · " + repo
}
