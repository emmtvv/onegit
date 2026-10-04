package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"onegit/internal/store"
	"onegit/internal/testutil"
)

func TestCaches(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	save := func(scope, key string, size int64) *store.Cache {
		c := &store.Cache{Scope: scope, Key: key, Size: size, BlobKey: scope + "/" + key}
		testutil.Must(t, st.SaveCache(ctx, c))
		return c
	}
	main := "refs/heads/main"
	save(main, "go-aaa", 10)
	time.Sleep(5 * time.Millisecond)
	save(main, "go-bbb", 20)
	save("refs/heads/feat", "go-ccc", 30)
	if err := st.SaveCache(ctx, &store.Cache{Scope: main, Key: "go-aaa", Size: 1, BlobKey: "x"}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate key: %v", err)
	}

	feat := []string{"refs/heads/feat", main}
	for _, c := range []struct {
		scopes      []string
		key, prefix string
		want        string
	}{
		{feat, "go-ccc", "", "go-ccc"},                   // own scope
		{feat, "go-aaa", "", "go-aaa"},                   // default branch
		{feat, "go-zzz", "go-", "go-ccc"},                // prefix: own scope first
		{[]string{main}, "go-zzz", "go-", "go-bbb"},      // newest with the prefix
		{[]string{"refs/heads/other"}, "go-ccc", "", ""}, // another branch's cache is not visible
		{feat, "go-zzz", "", ""},
	} {
		got, err := st.FindCache(ctx, c.scopes, c.key, c.prefix)
		switch {
		case c.want == "" && !errors.Is(err, store.ErrNotFound):
			t.Errorf("%v %s: got %v %v, want none", c.scopes, c.key, got, err)
		case c.want != "" && (err != nil || got.Key != c.want):
			t.Errorf("%v %s/%s: got %+v %v, want %s", c.scopes, c.key, c.prefix, got, err, c.want)
		}
	}
	if n, size, _ := st.CacheUsage(ctx); n != 3 || size != 60 {
		t.Errorf("usage = %d, %d", n, size)
	}
	// The least recently used go first: go-ccc and go-bbb were just used.
	keys, err := st.EvictCaches(ctx, time.Hour, 50)
	if err != nil || !slices.Equal(keys, []string{main + "/go-aaa"}) {
		t.Errorf("evicted %v %v", keys, err)
	}
	keys, _ = st.EvictCaches(ctx, -time.Hour, 0) // everything is "unused for too long"
	if len(keys) != 2 {
		t.Errorf("evicted %v", keys)
	}
}

func TestArtifactsAndSchedules(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	run := newRun(t, st, "pipeline", &store.Job{Name: "build"})
	jobs, _ := st.JobsForRun(ctx, run.ID)
	a := &store.Artifact{JobID: jobs[0].ID, RunID: run.ID, Size: 5, BlobKey: "k1", ExpiresAt: time.Now().Add(time.Hour)}
	if old, err := st.SaveArtifact(ctx, a); err != nil || old != "" {
		t.Fatal(old, err)
	}
	a2 := &store.Artifact{JobID: jobs[0].ID, RunID: run.ID, Size: 6, BlobKey: "k2", ExpiresAt: time.Now().Add(-time.Hour)}
	if old, err := st.SaveArtifact(ctx, a2); err != nil || old != "k1" {
		t.Fatalf("replace: %q %v", old, err)
	}
	list, _ := st.ArtifactsForRun(ctx, run.ID)
	if len(list) != 1 || list[0].JobName != "build" || list[0].Size != 6 || !list[0].Expired() {
		t.Errorf("artifacts = %+v", list)
	}
	if keys, _ := st.ExpiredArtifacts(ctx, 10); !slices.Equal(keys, []string{"k2"}) {
		t.Errorf("expired = %v", keys)
	}

	s := &store.ScheduleState{Pipeline: "p.yml", Cron: "* * * * *"}
	t0 := time.Now().Truncate(time.Microsecond)
	if ok, err := st.AdvanceSchedule(ctx, s, time.Time{}, t0); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := st.AdvanceSchedule(ctx, s, time.Time{}, t0); ok {
		t.Error("inserted twice")
	}
	if ok, _ := st.AdvanceSchedule(ctx, s, t0, t0.Add(time.Minute)); !ok {
		t.Error("advance failed")
	}
	if ok, _ := st.AdvanceSchedule(ctx, s, t0, t0.Add(2*time.Minute)); ok {
		t.Error("stale compare-and-swap succeeded")
	}
}

func TestSettingsAndDirFilters(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	var v struct{ A string }
	if ok, err := st.Setting(ctx, "x", &v); ok || err != nil {
		t.Fatal(ok, err)
	}
	testutil.Must(t, st.SetSetting(ctx, "x", map[string]string{"A": "1"}))
	testutil.Must(t, st.SetSetting(ctx, "x", map[string]string{"A": "2"}))
	if ok, _ := st.Setting(ctx, "x", &v); !ok || v.A != "2" {
		t.Errorf("setting = %v %+v", ok, v)
	}
	testutil.Must(t, st.SetSetting(ctx, "x", nil))
	if ok, _ := st.Setting(ctx, "x", &v); ok {
		t.Error("setting not removed")
	}

	sha := strings.Repeat("b", 40)
	mk := func(head string, files ...string) *store.Pull {
		p := &store.Pull{Title: head, HeadBranch: head, BaseBranch: "main", HeadSHA: sha, MergeBase: sha}
		testutil.Must(t, st.CreatePull(ctx, p))
		testutil.Must(t, st.SetPullFiles(ctx, p.ID, files, sha))
		return p
	}
	mk("a", "services/api/main.go")
	mk("b", "services/apiary/x.go", "docs/a.md")
	mk("c")
	got, _ := st.ListPullsIn(ctx, "open", "services/api", 10, 0)
	if len(got) != 1 || got[0].HeadBranch != "a" {
		t.Errorf("PRs in services/api = %v", got)
	}
	if open, _, _ := st.CountPullsIn(ctx, "services"); open != 2 {
		t.Errorf("open in services = %d", open)
	}
	if need, _ := st.PullsNeedingFiles(ctx, 10); len(need) != 0 {
		t.Errorf("needing files = %d", len(need))
	}

	r := &store.Run{Kind: "pipeline", Name: "p", Event: "push", Ref: "refs/heads/main", SHA: sha, Status: store.JobQueued,
		ChangedFiles: []string{"services/api/main.go"}}
	testutil.Must(t, st.CreateRun(ctx, r, nil))
	for dir, want := range map[string]int{"services/api": 1, "services/apiary": 0, "": 1} {
		runs, _ := st.ListRuns(ctx, store.RunFilter{Dir: dir, Ref: "refs/heads/main", Limit: 10})
		if len(runs) != want {
			t.Errorf("runs in %q: %d, want %d", dir, len(runs), want)
		}
	}
	if runs, _ := st.ListRuns(ctx, store.RunFilter{Ref: "refs/heads/dev", Limit: 10}); len(runs) != 0 {
		t.Errorf("ref filter: %d", len(runs))
	}
}

func TestRunAndPullPaging(t *testing.T) {
	t.Parallel()
	st := newStore(t)
	sha := strings.Repeat("c", 40)
	var ids []int64
	for i := range 5 {
		files := []string{"services/api/x.go"}
		if i%2 == 1 {
			files = []string{"a/b/c/d/e/f/g/deep.go", "README.md"}
		}
		r := &store.Run{Kind: "pipeline", Name: "p", Event: "push", Ref: "refs/heads/main", SHA: sha, Status: store.JobQueued, ChangedFiles: files}
		testutil.Must(t, st.CreateRun(ctx, r, nil))
		ids = append(ids, r.ID)
	}
	idsOf := func(runs []*store.Run) []int64 {
		var out []int64
		for _, r := range runs {
			out = append(out, r.ID)
			if r.ChangedFiles != nil {
				t.Errorf("listed run %d has changed files", r.ID)
			}
		}
		return out
	}
	list := func(f store.RunFilter) []int64 {
		runs, err := st.ListRuns(ctx, f)
		testutil.Must(t, err)
		return idsOf(runs)
	}
	if got := list(store.RunFilter{Limit: 2}); !slices.Equal(got, []int64{ids[4], ids[3]}) {
		t.Errorf("first page = %v", got)
	}
	if got := list(store.RunFilter{Limit: 2, Before: ids[3]}); !slices.Equal(got, []int64{ids[2], ids[1]}) {
		t.Errorf("older page = %v", got)
	}
	if got := list(store.RunFilter{Limit: 2, After: ids[1]}); !slices.Equal(got, []int64{ids[3], ids[2]}) {
		t.Errorf("newer page = %v (newest first)", got)
	}
	for dir, want := range map[string][]int64{
		"services":              {ids[4], ids[2], ids[0]},
		"services/api/":         {ids[4], ids[2], ids[0]},
		"a/b/c/d/e/f":           {ids[3], ids[1]}, // deepest indexed level
		"a/b/c/d/e/f/g":         {ids[3], ids[1]}, // deeper: scans changed_files
		"a/b/c/d/e/f/g/h":       nil,
		"services/api/x.go/foo": nil,
	} {
		if got := list(store.RunFilter{Dir: dir, Limit: 10}); !slices.Equal(got, want) {
			t.Errorf("runs in %q = %v, want %v", dir, got, want)
		}
	}
	if run, _ := st.RunByID(ctx, ids[1]); len(run.ChangedFiles) != 2 {
		t.Errorf("RunByID changed files = %v", run.ChangedFiles)
	}

	last, err := st.LastRunsIn(ctx, "pipeline", "refs/heads/main", []string{"services/api", "a/b/c/d/e/f/g", "docs"})
	if err != nil || len(last) != 2 || last["services/api"].ID != ids[4] || last["a/b/c/d/e/f/g"].ID != ids[3] {
		t.Errorf("LastRunsIn = %v, %v", last, err)
	}
	if last, _ := st.LastRunsIn(ctx, "pipeline", "refs/heads/dev", []string{"services/api"}); len(last) != 0 {
		t.Errorf("LastRunsIn(dev) = %v", last)
	}

	// Pull requests: files can change with the head, and so do their dirs.
	p := &store.Pull{Title: "t", HeadBranch: "h", BaseBranch: "main", HeadSHA: sha, MergeBase: sha}
	testutil.Must(t, st.CreatePull(ctx, p))
	testutil.Must(t, st.SetPullFiles(ctx, p.ID, []string{"services/api/x.go"}, sha))
	testutil.Must(t, st.SetPullFiles(ctx, p.ID, []string{"docs/x.md"}, sha))
	for dir, want := range map[string]int{"services": 0, "docs": 1} {
		if got, _ := st.FindPulls(ctx, store.PullFilter{State: "open", Dir: dir, Page: store.Page{Limit: 10}}); len(got) != want {
			t.Errorf("PRs in %s = %d, want %d", dir, len(got), want)
		}
	}
	if counts, err := st.OpenPullCountsIn(ctx, []string{"docs", "services", "a/b/c/d/e/f/g/h"}); err != nil ||
		counts["docs"] != 1 || counts["services"] != 0 || len(counts) != 3 {
		t.Errorf("OpenPullCountsIn = %v, %v", counts, err)
	}
}
