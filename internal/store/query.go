package store

import (
	"slices"
	"strconv"
	"strings"
)

// where collects the conditions and arguments of a dynamically built
// query; leaving out conditions that don't apply (instead of "$1 = ” OR
// ...") lets Postgres pick an index for the ones that do.
type where struct {
	conds []string
	args  []any
}

// add appends cond with each "?" bound to the next argument.
func (w *where) add(cond string, args ...any) {
	for _, a := range args {
		w.args = append(w.args, a)
		cond = strings.Replace(cond, "?", "$"+strconv.Itoa(len(w.args)), 1)
	}
	w.conds = append(w.conds, cond)
}

func (w *where) arg(v any) string {
	w.args = append(w.args, v)
	return "$" + strconv.Itoa(len(w.args))
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// maxDirDepth is how deep ci_run_dirs and pull_dirs index directories;
// deeper filters fall back to scanning changed_files.
const maxDirDepth = 6

// dirsOf returns the directories, with a trailing slash and up to
// maxDirDepth levels, that hold the files: "a/b/c.go" gives "a/" and "a/b/".
func dirsOf(files []string) []string {
	seen := map[string]bool{}
	for _, f := range files {
		for i, n := 0, 0; i < len(f) && n < maxDirDepth; i++ {
			if f[i] == '/' {
				seen[f[:i+1]] = true
				n++
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// touchesDir adds the condition that the row (alias.id) changed a file
// under dir, through the dirs table (dirTable.idCol) when dir is shallow
// enough to be indexed there.
func (w *where) touchesDir(dir, alias, dirTable, idCol string) {
	prefix := dirPrefix(dir)
	if prefix == "" {
		return
	}
	if strings.Count(prefix, "/") <= maxDirDepth {
		w.add(`EXISTS (SELECT 1 FROM `+dirTable+` d WHERE d.`+idCol+` = `+alias+`.id AND d.dir = ?)`, prefix)
		return
	}
	w.add(`EXISTS (SELECT 1 FROM unnest(`+alias+`.changed_files) f WHERE starts_with(f, ?))`, prefix)
}

// Page selects rows by id, newest first: Before (older than) and After
// (newer than) are keyset cursors, so deep pages cost the same as the first.
type Page struct {
	Limit  int
	Offset int   // prefer the cursors
	Before int64 // only ids below
	After  int64 // only ids above; rows still come newest first
}

// apply adds the cursor condition and returns the ORDER BY/LIMIT tail.
// With After the query runs oldest first and the caller reverses it.
func (p Page) apply(w *where, idCol string) string {
	order := idCol + " DESC"
	if p.Before > 0 {
		w.add(idCol+" < ?", p.Before)
	}
	if p.After > 0 {
		w.add(idCol+" > ?", p.After)
		order = idCol + " ASC"
	}
	tail := " ORDER BY " + order
	if p.Limit > 0 {
		tail += " LIMIT " + w.arg(p.Limit)
	}
	if p.Offset > 0 {
		tail += " OFFSET " + w.arg(p.Offset)
	}
	return tail
}

// newestFirst undoes the ascending order of an After page.
func newestFirst[T any](p Page, rows []T) []T {
	if p.After > 0 {
		slices.Reverse(rows)
	}
	return rows
}
