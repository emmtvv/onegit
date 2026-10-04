package web

import (
	"slices"
	"testing"
	"time"

	"onegit/internal/pulls"
	"onegit/internal/store"
)

func TestNextStep(t *testing.T) {
	ok := func() *pulls.Status { return &pulls.Status{HeadExists: true, BaseExists: true} }
	pr := &store.Pull{BaseBranch: "main"}
	cases := []struct {
		name   string
		st     func() *pulls.Status
		pull   *store.Pull
		queued *store.QueueEntry
		state  string
		text   string
	}{
		{"ready", ok, pr, nil, "ok", "Ready to merge"},
		{"queue required", func() *pulls.Status { s := ok(); s.QueueRequired = true; return s }, pr, nil, "ok", "Ready for the merge queue"},
		{"in the queue", ok, pr, &store.QueueEntry{State: store.QueueTesting}, "wait", "Being tested in the merge queue"},
		{"conflicts", func() *pulls.Status {
			s := ok()
			s.Conflicts, s.Blockers = []string{"a.go"}, []string{"conflicts"}
			return s
		}, pr, nil, "bad", "Resolve conflicts with main"},
		{"failed check beats missing approvals", func() *pulls.Status {
			s := ok()
			s.Protection, s.Required = &store.BranchProtection{}, 1
			s.Checks = []*store.CommitStatus{{Context: "ci/test", State: "failure"}}
			s.Blockers = []string{"x"}
			return s
		}, pr, nil, "bad", "Check ci/test failed"},
		{"changes requested", func() *pulls.Status {
			s := ok()
			s.Verdicts = []*pulls.Verdict{{Name: "dana", State: store.ReviewChangesRequested}}
			return s
		}, pr, nil, "bad", "Changes requested by dana"},
		{"approvals", func() *pulls.Status {
			s := ok()
			s.Protection, s.Required, s.Approvals = &store.BranchProtection{}, 2, 0
			s.Blockers = []string{"x"}
			return s
		}, pr, nil, "wait", "Waiting for 2 more approvals"},
		{"checks running", func() *pulls.Status {
			s := ok()
			s.Checks = []*store.CommitStatus{{Context: "ci/test", State: "pending"}}
			return s
		}, pr, nil, "wait", "Checks are running"},
		{"auto-merge", ok, &store.Pull{AutoMergeBy: new(int64)}, nil, "wait", "Merges automatically"},
	}
	for _, c := range cases {
		got := nextStep(c.pull, c.st(), c.queued)
		if got.State != c.state || got.Text != c.text {
			t.Errorf("%s: got %s %q, want %s %q", c.name, got.State, got.Text, c.state, c.text)
		}
	}
}

func TestEnvColumns(t *testing.T) {
	rows := []projectRow{
		{Deploys: []projectDeploy{{Label: "production"}, {Label: "preview-42"}}},
		{Deploys: []projectDeploy{{Label: "staging"}, {Label: "dev"}}},
	}
	others := []*store.DeployDimension{{Name: "environment", Source: "list", Values: []string{"dev", "staging", "production", "unused"}}}
	got := envColumns(rows, others)
	if want := []string{"dev", "staging", "production", "preview-42"}; !slices.Equal(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
	setCells(rows, got)
	if rows[0].Cells[0] != nil || rows[0].Cells[2].Label != "production" || rows[1].Cells[1].Label != "staging" {
		t.Errorf("cells misplaced: %+v %+v", rows[0].Cells, rows[1].Cells)
	}
	two := []*store.DeployDimension{
		{Name: "region", Source: "list", Values: []string{"eu", "us"}},
		{Name: "environment", Source: "list", Values: []string{"staging", "production"}},
	}
	rows = []projectRow{{Deploys: []projectDeploy{{Label: "us / staging"}, {Label: "eu / production"}, {Label: "eu / staging"}}}}
	if got, want := envColumns(rows, two), []string{"eu / staging", "eu / production", "us / staging"}; !slices.Equal(got, want) {
		t.Errorf("two dimensions: %v, want %v", got, want)
	}
}

func TestDriftLevel(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		d     drift
		level string
		count string
	}{
		{drift{}, "", "0"},
		{drift{Known: true}, "", "0"},
		{drift{Known: true, Commits: 3, Oldest: now.Add(-time.Hour)}, "warn", "3"},
		{drift{Known: true, Commits: 3, Oldest: now.Add(-8 * 24 * time.Hour)}, "bad", "3"},
		{drift{Known: true, Commits: driftBadCommits, Oldest: now}, "bad", "10"},
		{drift{Known: true, Commits: driftLimit, Oldest: now}, "bad", "100+"},
	} {
		if c.d.Level() != c.level || c.d.Count() != c.count {
			t.Errorf("%+v: level %q count %q, want %q %q", c.d, c.d.Level(), c.d.Count(), c.level, c.count)
		}
	}
	row := projectRow{LastRun: &store.Run{Status: store.JobFailure}}
	if !row.Attention() {
		t.Error("failing CI should need attention")
	}
	row = projectRow{Deploys: []projectDeploy{{Drift: drift{Known: true, Commits: 1, Oldest: now}}}}
	if row.Attention() {
		t.Error("one fresh commit behind should not need attention")
	}
}
