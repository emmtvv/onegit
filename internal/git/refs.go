package git

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"
)

type RefKind int

const (
	KindBranch RefKind = iota
	KindTag
	KindCommit
)

type Ref struct {
	Name    string // short name, e.g. "main" or "v1.0"
	Kind    RefKind
	SHA     string // commit the ref points to (tags are peeled)
	Subject string
	Author  string
	When    time.Time
}

const refFormat = "%(refname:strip=2)%00%(objectname)%00%(*objectname)%00%(subject)%00%(authorname)%00%(creatordate:unix)"

func parseRef(line string, kind RefKind) (Ref, bool) {
	f := strings.Split(line, "\x00")
	if len(f) < 6 {
		return Ref{}, false
	}
	// For annotated tags *objectname is the peeled commit; fall back to objectname.
	sha := f[1]
	if f[2] != "" {
		sha = f[2]
	}
	ts, _ := strconv.ParseInt(f[5], 10, 64)
	return Ref{Name: f[0], Kind: kind, SHA: sha, Subject: f[3], Author: f[4], When: time.Unix(ts, 0)}, true
}

func refPrefix(kind RefKind) string {
	if kind == KindTag {
		return "refs/tags/"
	}
	return "refs/heads/"
}

func (r *Repo) listRefs(ctx context.Context, kind RefKind) ([]Ref, error) {
	refs, _, err := r.ListRefs(ctx, kind, RefQuery{})
	return refs, err
}

// Branches lists every branch, newest first. Prefer ListRefs in request
// paths: repositories can have tens of thousands of branches.
func (r *Repo) Branches(ctx context.Context) ([]Ref, error) {
	return r.listRefs(ctx, KindBranch)
}

// Tags lists every tag, newest first.
func (r *Repo) Tags(ctx context.Context) ([]Ref, error) {
	return r.listRefs(ctx, KindTag)
}

// RefQuery selects a page of branches or tags.
type RefQuery struct {
	Query   string // case-insensitive substring of the name
	Exclude string // a name to leave out, e.g. the default branch
	Offset  int
	Limit   int // 0 = all
}

// ListRefs returns the branches or tags, newest first, selected by q; more
// reports that further refs match. See sortedRefs for the cost.
func (r *Repo) ListRefs(ctx context.Context, kind RefKind, q RefQuery) (refs []Ref, more bool, err error) {
	all, err := r.sortedRefs(ctx, kind)
	if err != nil {
		return nil, false, err
	}
	query := strings.ToLower(q.Query)
	skip := q.Offset
	for _, ref := range all {
		if ref.Name == q.Exclude || query != "" && !strings.Contains(strings.ToLower(ref.Name), query) {
			continue
		}
		if skip > 0 {
			skip--
			continue
		}
		if q.Limit > 0 && len(refs) == q.Limit {
			return refs, true, nil
		}
		refs = append(refs, ref)
	}
	return refs, false, nil
}

// RefsByName looks up branches or tags by exact name; missing ones are
// absent from the result.
func (r *Repo) RefsByName(ctx context.Context, kind RefKind, names []string) (map[string]Ref, error) {
	out := map[string]Ref{}
	if len(names) == 0 {
		return out, nil
	}
	args := []string{"for-each-ref", "--format=" + refFormat}
	for _, n := range names {
		args = append(args, refPrefix(kind)+n)
	}
	res, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	// A pattern also matches refs below it ("a" matches "a/b"): keep exact names.
	for _, line := range strings.Split(strings.TrimRight(string(res), "\n"), "\n") {
		if ref, ok := parseRef(line, kind); ok && want[ref.Name] {
			out[ref.Name] = ref
		}
	}
	return out, nil
}

// AheadBehindMany counts, for each named branch, the commits it has that
// base lacks (ahead) and the ones base has that it lacks (behind), in one
// history walk.
func (r *Repo) AheadBehindMany(ctx context.Context, base string, branches []string) (map[string][2]int, error) {
	out := map[string][2]int{}
	if len(branches) == 0 {
		return out, nil
	}
	args := []string{"for-each-ref", "--format=%(refname:strip=2)%00%(ahead-behind:" + base + ")"}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	res, err := r.run(ctx, nil, args...)
	if err != nil {
		// git before 2.41 has no ahead-behind atom: count branch by branch.
		for _, b := range branches {
			a, bh, err := r.AheadBehind(ctx, "refs/heads/"+b, base)
			if err != nil {
				return nil, err
			}
			out[b] = [2]int{a, bh}
		}
		return out, nil
	}
	for _, line := range strings.Split(strings.TrimRight(string(res), "\n"), "\n") {
		name, counts, ok := strings.Cut(line, "\x00")
		f := strings.Fields(counts)
		if !ok || len(f) != 2 {
			continue
		}
		a, _ := strconv.Atoi(f[0])
		b, _ := strconv.Atoi(f[1])
		out[name] = [2]int{a, b}
	}
	return out, nil
}

// BranchesContaining lists the branches whose history includes commit.
func (r *Repo) BranchesContaining(ctx context.Context, commit string) ([]string, error) {
	out, err := r.run(ctx, nil, "for-each-ref", "--sort=-creatordate", "--format=%(refname:strip=2)",
		"--contains", commit, "refs/heads/")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// ResolveCommit resolves a revision to a full commit SHA.
func (r *Repo) ResolveCommit(ctx context.Context, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", ErrNotExist
	}
	out, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", ErrNotExist
	}
	return strings.TrimSpace(string(out)), nil
}

// maxRefDepth bounds how many leading path segments may form a ref name.
const maxRefDepth = 32

// SplitRefPath splits "feature/x/src/main.go" into the longest prefix that is
// a branch or tag, and the remaining path. Full or abbreviated commit SHAs
// are also accepted as the first segment. Every candidate prefix is checked
// in one git process, so the cost does not grow with the number of refs.
func (r *Repo) SplitRefPath(ctx context.Context, s string) (ref Ref, path string, err error) {
	s = strings.Trim(s, "/")
	segs := strings.Split(s, "/")
	type cand struct {
		name string
		kind RefKind
	}
	var cands []cand
	var in strings.Builder
	// Longest first so "release/1.0" wins over "release"; a branch wins over
	// a tag of the same name.
	for i := min(len(segs), maxRefDepth); i > 0; i-- {
		name := strings.Join(segs[:i], "/")
		if !plausibleRefName(name) {
			continue
		}
		for _, k := range []RefKind{KindBranch, KindTag} {
			cands = append(cands, cand{name, k})
			in.WriteString(refPrefix(k) + name + "^{commit}\n")
		}
	}
	if len(cands) > 0 {
		out, err := r.run(ctx, strings.NewReader(in.String()), "cat-file", "--batch-check=%(objectname)")
		if err != nil {
			return Ref{}, "", err
		}
		for i, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if i >= len(cands) || !isHex(line) {
				continue // "<name> missing" or "ambiguous"
			}
			c := cands[i]
			return Ref{Name: c.name, Kind: c.kind, SHA: line}, strings.TrimPrefix(s[len(c.name):], "/"), nil
		}
	}
	first, rest, _ := strings.Cut(s, "/")
	if isHex(first) && len(first) >= 4 {
		sha, err := r.ResolveCommit(ctx, first)
		if err == nil {
			return Ref{Name: sha, Kind: KindCommit, SHA: sha}, rest, nil
		}
	}
	return Ref{}, "", ErrNotExist
}

// plausibleRefName rejects names git would not accept as a ref, in
// particular anything that cat-file would read as a revision expression.
func plausibleRefName(n string) bool {
	if n == "" || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".lock") || strings.Contains(n, "..") ||
		strings.Contains(n, "@{") || strings.Contains(n, "//") || strings.Contains(n, "/.") {
		return false
	}
	for _, c := range []byte(n) {
		if c <= ' ' || c == 0x7f || strings.IndexByte("~^:?*[\\", c) >= 0 {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}

// MergeBase returns the best common ancestor of two commits.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := r.run(ctx, nil, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimSpace(out)), nil
}

// AheadBehind counts commits in a not in b, and in b not in a.
func (r *Repo) AheadBehind(ctx context.Context, a, b string) (ahead, behind int, err error) {
	out, err := r.run(ctx, nil, "rev-list", "--left-right", "--count", a+"..."+b)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(string(out))
	if len(f) == 2 {
		ahead, _ = strconv.Atoi(f[0])
		behind, _ = strconv.Atoi(f[1])
	}
	return ahead, behind, nil
}
