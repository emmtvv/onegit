package git

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"time"
)

type Signature struct {
	Name  string
	Email string
	When  time.Time
}

type Commit struct {
	SHA       string
	Parents   []string
	Author    Signature
	Committer Signature
	Subject   string
	Body      string
}

func (c *Commit) Short() string { return c.SHA[:10] }

const (
	logFormat = "%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%s%x00%b%x1e"
	recSep    = "\x1e"
)

func parseCommits(out string) []*Commit {
	var res []*Commit
	for _, rec := range strings.Split(out, recSep) {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.Split(rec, "\x00")
		if len(f) < 10 {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		ct, _ := strconv.ParseInt(f[7], 10, 64)
		res = append(res, &Commit{
			SHA:       f[0],
			Parents:   strings.Fields(f[1]),
			Author:    Signature{Name: f[2], Email: f[3], When: time.Unix(at, 0)},
			Committer: Signature{Name: f[5], Email: f[6], When: time.Unix(ct, 0)},
			Subject:   f[8],
			Body:      strings.TrimSpace(f[9]),
		})
	}
	return res
}

type LogOptions struct {
	Path  string // limit to commits touching this path
	Skip  int
	Limit int
}

// Log lists commits reachable from rev (or in range "a..b").
func (r *Repo) Log(ctx context.Context, rev string, o LogOptions) ([]*Commit, error) {
	args := []string{"log", "--format=" + logFormat}
	if o.Limit > 0 {
		args = append(args, "-n", strconv.Itoa(o.Limit))
	}
	if o.Skip > 0 {
		args = append(args, "--skip", strconv.Itoa(o.Skip))
	}
	args = append(args, "--end-of-options", rev, "--")
	if o.Path != "" {
		args = append(args, o.Path)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseCommits(string(out)), nil
}

func (r *Repo) GetCommit(ctx context.Context, rev string) (*Commit, error) {
	cs, err := r.Log(ctx, rev, LogOptions{Limit: 1})
	if err != nil || len(cs) == 0 {
		return nil, ErrNotExist
	}
	return cs[0], nil
}

// LastCommitFor returns the most recent commit touching path.
func (r *Repo) LastCommitFor(ctx context.Context, rev, path string) (*Commit, error) {
	cs, err := r.Log(ctx, rev, LogOptions{Path: path, Limit: 1})
	if err != nil || len(cs) == 0 {
		return nil, ErrNotExist
	}
	return cs[0], nil
}

// LastCommits finds, for each entry name in dir, the most recent commit that
// touched it. It walks history once and stops when all entries are resolved
// or after maxCommits commits; unresolved entries are absent from the map.
func (r *Repo) LastCommits(ctx context.Context, rev, dir string, names []string, maxCommits int) (map[string]*Commit, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	args := []string{"-c", "core.quotepath=false", "log", "--no-renames", "--name-only",
		"--format=%x1e%H%x00%an%x00%ae%x00%at%x00%s", "--end-of-options", rev, "--"}
	prefix := ""
	if dir != "" {
		args = append(args, dir)
		prefix = dir + "/"
	}
	cmd := r.Cmd(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Stopping early kills git, so its exit status is meaningless here.
	defer func() { _ = cmd.Wait() }()

	res := make(map[string]*Commit, len(names))
	var cur *Commit
	seen := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, recSep); ok {
			seen++
			if seen > maxCommits || len(res) == len(want) {
				break
			}
			f := strings.Split(rest, "\x00")
			if len(f) != 5 {
				cur = nil
				continue
			}
			at, _ := strconv.ParseInt(f[3], 10, 64)
			cur = &Commit{SHA: f[0], Author: Signature{Name: f[1], Email: f[2], When: time.Unix(at, 0)}, Subject: f[4]}
			continue
		}
		if cur == nil || line == "" {
			continue
		}
		rel, ok := strings.CutPrefix(unquote(line), prefix)
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rel, "/")
		if want[name] && res[name] == nil {
			res[name] = cur
		}
	}
	cancel()
	return res, nil
}
