package web

import (
	"errors"
	"slices"
	"testing"
	"time"

	"onegit/internal/store"
)

func TestPaginate(t *testing.T) {
	id := func(n int64) int64 { return n }
	cases := []struct {
		name  string
		rows  []int64 // as the store returns them for the page: newest first, n+1 at most
		page  store.Page
		want  []int64
		pager pager
	}{
		{"first, more", []int64{9, 8, 7}, store.Page{}, []int64{9, 8}, pager{First: true, Older: 8}},
		{"first, all", []int64{9, 8}, store.Page{}, []int64{9, 8}, pager{First: true}},
		{"older, more", []int64{7, 6, 5}, store.Page{Before: 8}, []int64{7, 6}, pager{Newer: 7, Older: 6}},
		{"older, last", []int64{2}, store.Page{Before: 3}, []int64{2}, pager{Newer: 2}},
		{"newer, more", []int64{9, 8, 7}, store.Page{After: 6}, []int64{8, 7}, pager{Newer: 8, Older: 7}},
		{"newer, newest", []int64{8, 7}, store.Page{After: 6}, []int64{8, 7}, pager{Older: 7}},
		{"empty", nil, store.Page{Before: 1}, nil, pager{}},
	}
	for _, c := range cases {
		got, pg := paginate(c.rows, 2, c.page, id)
		if !slices.Equal(got, c.want) || pg != c.pager {
			t.Errorf("%s: %v %+v, want %v %+v", c.name, got, pg, c.want, c.pager)
		}
	}
}

func TestAcquireHeavy(t *testing.T) {
	w := &Web{heavy: make(chan struct{}, 1)}
	release, err := w.acquireHeavy(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.acquireHeavy(t.Context(), 10*time.Millisecond); !errors.Is(err, errBusy) {
		t.Errorf("second slot: %v, want errBusy", err)
	}
	release()
	if release, err := w.acquireHeavy(t.Context(), 10*time.Millisecond); err != nil {
		t.Errorf("after release: %v", err)
	} else {
		release()
	}
}
