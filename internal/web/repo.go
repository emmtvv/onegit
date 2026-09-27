package web

import (
	"bytes"
	"context"
	"html/template"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"onegit/internal/git"
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
	branches, _ := w.Repo.Branches(ctx)
	tags, _ := w.Repo.Tags(ctx)
	w.render(rw, r, http.StatusOK, "tree", &Page{
		Title: titleFor(w.Cfg.Repo.Name, rv.Path), Tab: "code",
		Data: map[string]any{
			"Ref": rv.Ref, "Path": rv.Path, "Rows": rows, "Head": head,
			"Readme": readme, "ReadmeHTML": readmeHTML,
			"Branches": branches, "Tags": tags,
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
	list, err := w.Repo.Log(ctx, rv.Ref.SHA, git.LogOptions{
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
	branches, _ := w.Repo.Branches(ctx)
	tags, _ := w.Repo.Tags(ctx)
	w.render(rw, r, http.StatusOK, "commits", &Page{Title: "Commits · " + w.Cfg.Repo.Name, Tab: "commits", Data: map[string]any{
		"Ref": rv.Ref, "Path": rv.Path, "Groups": groups, "Page": page, "HasNext": hasNext,
		"Branches": branches, "Tags": tags,
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

type branchRow struct {
	git.Ref
	Ahead, Behind int
	IsDefault     bool
	Pull          *store.Pull // open PR from this branch, if any
}

func (w *Web) branches(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := w.Repo.Branches(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	def := w.defaultBranch(ctx)
	var defSHA string
	for _, b := range list {
		if b.Name == def {
			defSHA = b.SHA
		}
	}
	byHead, err := w.Store.OpenPullsByHead(ctx)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	rows := make([]branchRow, 0, len(list))
	var defRow *branchRow
	for _, b := range list {
		row := branchRow{Ref: b, IsDefault: b.Name == def}
		if ps := byHead[b.Name]; len(ps) > 0 {
			row.Pull = ps[0]
		}
		if !row.IsDefault && defSHA != "" {
			row.Ahead, row.Behind, _ = w.Repo.AheadBehind(ctx, b.SHA, defSHA)
		}
		if row.IsDefault {
			defRow = &row
			continue
		}
		rows = append(rows, row)
	}
	w.render(rw, r, http.StatusOK, "branches", &Page{Title: "Branches · " + w.Cfg.Repo.Name, Tab: "branches", Data: map[string]any{
		"Default": defRow, "Branches": rows,
	}})
}

func (w *Web) tags(rw http.ResponseWriter, r *http.Request) {
	list, err := w.Repo.Tags(r.Context())
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	w.render(rw, r, http.StatusOK, "tags", &Page{Title: "Tags · " + w.Cfg.Repo.Name, Tab: "tags", Data: map[string]any{"Tags": list}})
}

func titleFor(repo, p string) string {
	if p == "" {
		return repo
	}
	return p + " · " + repo
}
