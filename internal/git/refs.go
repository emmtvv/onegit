package git

import (
	"bytes"
	"context"
	"sort"
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

func (r *Repo) listRefs(ctx context.Context, prefix string, kind RefKind) ([]Ref, error) {
	// For annotated tags *objectname is the peeled commit; fall back to objectname.
	out, err := r.run(ctx, nil, "for-each-ref", "--sort=-creatordate",
		"--format=%(refname:strip=2)%00%(objectname)%00%(*objectname)%00%(subject)%00%(authorname)%00%(creatordate:unix)",
		prefix)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.Split(line, "\x00")
		if len(f) != 6 {
			continue
		}
		sha := f[1]
		if f[2] != "" {
			sha = f[2]
		}
		ts, _ := strconv.ParseInt(f[5], 10, 64)
		refs = append(refs, Ref{Name: f[0], Kind: kind, SHA: sha, Subject: f[3], Author: f[4], When: time.Unix(ts, 0)})
	}
	return refs, nil
}

func (r *Repo) Branches(ctx context.Context) ([]Ref, error) {
	return r.listRefs(ctx, "refs/heads/", KindBranch)
}

func (r *Repo) Tags(ctx context.Context) ([]Ref, error) {
	return r.listRefs(ctx, "refs/tags/", KindTag)
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

// SplitRefPath splits "feature/x/src/main.go" into the longest prefix that is
// a branch or tag, and the remaining path. Full or abbreviated commit SHAs
// are also accepted as the first segment.
func (r *Repo) SplitRefPath(ctx context.Context, s string) (ref Ref, path string, err error) {
	s = strings.Trim(s, "/")
	branches, err := r.Branches(ctx)
	if err != nil {
		return Ref{}, "", err
	}
	tags, err := r.Tags(ctx)
	if err != nil {
		return Ref{}, "", err
	}
	all := append(branches, tags...)
	// Longest match first so "release/1.0" wins over "release".
	sort.SliceStable(all, func(i, j int) bool { return len(all[i].Name) > len(all[j].Name) })
	for _, ref := range all {
		if s == ref.Name {
			return ref, "", nil
		}
		if strings.HasPrefix(s, ref.Name+"/") {
			return ref, s[len(ref.Name)+1:], nil
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
