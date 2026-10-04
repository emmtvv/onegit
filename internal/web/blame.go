package web

import (
	"context"
	"html/template"
	"net/http"
	"path"
	"time"

	"onegit/internal/git"
)

// maxBlameSize bounds the files blamed in the web UI: blame walks history
// and gets slow on huge files.
const maxBlameSize = 512 << 10

type blameRow struct {
	Hunk  *git.BlameHunk
	Lines []blameLine
	Age   int // 0 (oldest) … 9 (newest), for the age bar
}

type blameLine struct {
	No   int
	Code template.HTML
}

func (w *Web) blame(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rv, err := w.resolve(r)
	if err != nil || rv.Path == "" {
		w.notFound(rw, r)
		return
	}
	if w.Repo.ObjectType(ctx, rv.Ref.SHA, rv.Path) != "blob" {
		w.notFound(rw, r)
		return
	}
	data := map[string]any{"Ref": rv.Ref, "Path": rv.Path}
	size, err := w.Repo.BlobSize(ctx, rv.Ref.SHA, rv.Path)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	data["Size"] = size
	b, err := w.Repo.ReadBlob(ctx, rv.Ref.SHA, rv.Path, maxBlameSize+1)
	switch {
	case err != nil:
		w.fail(rw, r, err)
		return
	case size > maxBlameSize:
		data["TooLarge"] = true
	case git.IsBinary(b):
		data["Binary"] = true
	default:
		hunks, err := w.blameHunks(ctx, rv.Ref.SHA, rv.Path)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		data["Rows"] = blameRows(hunks, highlightLines(path.Base(rv.Path), b))
	}
	w.render(rw, r, http.StatusOK, "blame", &Page{Title: "Blame · " + titleFor(w.Cfg.Repo.Name, rv.Path), Tab: "code", Data: data})
}

// blameHunks is cached per commit and path: the result never changes.
func (w *Web) blameHunks(ctx context.Context, commit, p string) ([]*git.BlameHunk, error) {
	key := "blame:" + commit + ":" + p
	var cached []*git.BlameHunk
	if ok, err := w.KV.GetJSON(ctx, key, &cached); ok && err == nil {
		return cached, nil
	}
	bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hunks, err := w.Repo.Blame(bctx, commit, p)
	if err != nil {
		return nil, err
	}
	if err := w.KV.SetJSON(ctx, key, hunks, 7*24*time.Hour); err != nil {
		w.Log.Warn("cache blame", "err", err)
	}
	return hunks, nil
}

func blameRows(hunks []*git.BlameHunk, code []template.HTML) []blameRow {
	var oldest, newest time.Time
	for _, h := range hunks {
		t := h.Commit.Author.When
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		if t.After(newest) {
			newest = t
		}
	}
	span := newest.Sub(oldest)
	rows := make([]blameRow, len(hunks))
	for i, h := range hunks {
		rows[i].Hunk = h
		if span > 0 {
			rows[i].Age = int(9 * h.Commit.Author.When.Sub(oldest) / span)
		}
		for j, l := range h.Lines {
			no := h.StartLine + j
			bl := blameLine{No: no, Code: template.HTML(template.HTMLEscapeString(l))}
			if no-1 < len(code) {
				bl.Code = code[no-1]
			}
			rows[i].Lines = append(rows[i].Lines, bl)
		}
	}
	return rows
}
