package git

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Sorting refs by date makes git read every ref's commit or tag: seconds
// with tens of thousands of branches. Listing names and object ids is
// cheap, so a sorted snapshot is kept per repository and kind, and only
// refs that appeared or moved since are read again.

// refRereadMax is how many changed refs are re-read one by one; beyond
// that the whole sorted list is listed again.
const refRereadMax = 500

type refSnapshot struct {
	tips   map[string]string // name → object id (unpeeled)
	sorted []Ref             // newest first
}

type refCacheKey struct {
	path string
	kind RefKind
}

var (
	refCacheMu sync.Mutex
	refCaches  = map[refCacheKey]*refSnapshot{}
	// refLocks serialises refreshes per key, so concurrent requests after a
	// push don't all re-read the same refs.
	refLocks sync.Map // refCacheKey → *sync.Mutex
)

// sortedRefs returns every branch or tag, newest first.
func (r *Repo) sortedRefs(ctx context.Context, kind RefKind) ([]Ref, error) {
	out, err := r.run(ctx, nil, "for-each-ref", "--format=%(refname:strip=2)%00%(objectname)", refPrefix(kind))
	if err != nil {
		return nil, err
	}
	tips := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if name, sha, ok := strings.Cut(line, "\x00"); ok {
			tips[name] = sha
		}
	}

	key := refCacheKey{r.Path, kind}
	l, _ := refLocks.LoadOrStore(key, &sync.Mutex{})
	l.(*sync.Mutex).Lock()
	defer l.(*sync.Mutex).Unlock()
	refCacheMu.Lock()
	snap := refCaches[key]
	refCacheMu.Unlock()

	var changed []string
	if snap != nil {
		for name, sha := range tips {
			if snap.tips[name] != sha {
				changed = append(changed, name)
			}
		}
		if len(changed) == 0 && len(snap.tips) == len(tips) {
			return snap.sorted, nil
		}
	}
	var sorted []Ref
	if snap != nil && len(changed) <= refRereadMax {
		fresh, err := r.RefsByName(ctx, kind, changed)
		if err != nil {
			return nil, err
		}
		sorted = make([]Ref, 0, len(tips))
		for _, ref := range snap.sorted {
			if _, still := tips[ref.Name]; still && fresh[ref.Name].Name == "" {
				sorted = append(sorted, ref)
			}
		}
		for _, ref := range fresh {
			sorted = append(sorted, ref)
		}
		sort.SliceStable(sorted, func(i, j int) bool {
			if !sorted[i].When.Equal(sorted[j].When) {
				return sorted[i].When.After(sorted[j].When)
			}
			return sorted[i].Name < sorted[j].Name
		})
	} else {
		if sorted, err = r.listAllRefs(ctx, kind); err != nil {
			return nil, err
		}
	}
	refCacheMu.Lock()
	refCaches[key] = &refSnapshot{tips: tips, sorted: sorted}
	refCacheMu.Unlock()
	return sorted, nil
}

// listAllRefs lists every ref of a kind, newest first, by name among equals
// (git sorts by the last --sort key first).
func (r *Repo) listAllRefs(ctx context.Context, kind RefKind) ([]Ref, error) {
	out, err := r.run(ctx, nil, "for-each-ref", "--sort=refname", "--sort=-creatordate", "--format="+refFormat, refPrefix(kind))
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if ref, ok := parseRef(line, kind); ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}
