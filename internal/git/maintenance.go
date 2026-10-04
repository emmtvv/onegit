package git

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// fullGCInterval is how often Maintain also prunes unreachable objects.
const fullGCInterval = 7 * 24 * time.Hour

// Maintain keeps a large repository fast to read: refs are packed (tens of
// thousands of loose refs make every ref lookup slow), packs are merged
// geometrically with a multi-pack bitmap (fast clones and fetches), and the
// commit-graph is updated with changed-path Bloom filters, which is what
// keeps `git log -- path`, blame and the tree view's last commits fast on
// deep histories. Every step is incremental. Once a week it also runs a
// full gc that prunes unreachable objects older than two weeks.
func (r *Repo) Maintain(ctx context.Context) error {
	stamp := filepath.Join(r.Path, "onegit-last-gc")
	// Incremental graph layers only get Bloom filters for new commits; after
	// a full gc (and the first time) the whole graph is rewritten with them.
	split := "--split"
	if st, err := os.Stat(stamp); err != nil || time.Since(st.ModTime()) > fullGCInterval {
		if _, err := r.run(ctx, nil, "gc", "--quiet", "--prune=2.weeks.ago"); err != nil {
			return err
		}
		if err := os.WriteFile(stamp, nil, 0o644); err != nil {
			return err
		}
		now := time.Now()
		_ = os.Chtimes(stamp, now, now)
		split = "--split=replace"
	}
	for _, args := range [][]string{
		{"pack-refs", "--all"},
		{"repack", "-d", "-q", "--geometric=2", "--write-midx", "--write-bitmap-index"},
		{"commit-graph", "write", "--reachable", "--changed-paths", split, "--size-multiple=2"},
	} {
		if _, err := r.run(ctx, nil, args...); err != nil {
			return err
		}
	}
	return nil
}
