package web

import (
	"net/http"
	"strconv"

	"onegit/internal/store"
)

// pager is a keyset page of a list ordered newest first: links carry
// ?before=<id> (older) or ?after=<id> (newer) instead of page numbers, so
// deep pages are as cheap as the first and don't shift as rows are added.
type pager struct {
	First bool  // no cursor: the newest rows
	Newer int64 // ?after= for the newer page, 0 when there is none
	Older int64 // ?before= for the older page, 0 when there is none
}

// pageQuery reads the cursor and asks for one row more than n, which tells
// whether the list goes on.
func pageQuery(r *http.Request, n int) store.Page {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if before > 0 {
		after = 0
	}
	return store.Page{Limit: n + 1, Before: max(before, 0), After: max(after, 0)}
}

// paginate trims rows (newest first, as returned for p) to n and works out
// the cursors of the neighbouring pages.
func paginate[T any](rows []T, n int, p store.Page, id func(T) int64) ([]T, pager) {
	pg := pager{First: p.Before == 0 && p.After == 0}
	more := len(rows) > n
	if more {
		if p.After > 0 {
			rows = rows[1:] // the extra row is the newest one
		} else {
			rows = rows[:n]
		}
	}
	if len(rows) == 0 {
		return rows, pg
	}
	if p.After > 0 && more || p.Before > 0 {
		pg.Newer = id(rows[0])
	}
	if p.After > 0 || more {
		pg.Older = id(rows[len(rows)-1])
	}
	return rows, pg
}
