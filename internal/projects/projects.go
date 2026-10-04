// Package projects finds the projects of the monorepo: directories matching
// a pattern on the default branch ("services/*"). A project ties together
// its code, owners, pull requests, CI runs and, through a deploy dimension
// with the same values, its deployments.
package projects

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"onegit/internal/config"
	"onegit/internal/git"
	"onegit/internal/pulls"
	"onegit/internal/store"
)

const settingName = "projects"

// Settings say how projects are found.
type Settings struct {
	// Pattern has one "*" segment: "services/*", or
	// "projects/*/project.yaml" to require a marker file.
	Pattern string `json:"pattern"`
	// Dimension is the deploy dimension whose values are project names;
	// empty when deployments are not per project.
	Dimension string `json:"dimension"`
}

// Dir is a project's directory for a name.
func (st Settings) Dir(name string) string {
	prefix, _, _ := strings.Cut(st.Pattern, "*")
	return prefix + name
}

type Project struct {
	Name string
	Dir  string
}

type Service struct {
	Store *store.Store
	Repo  *git.Repo
	Cfg   *config.Config

	mu     sync.Mutex
	cached listCache // the last listing: projects only change with the default branch
}

type listCache struct {
	sha, pattern string
	names        []string
}

// Settings returns the configured settings or, without any, the ones
// implied by the first deploy dimension discovered from paths. ok is false
// when projects are not set up.
func (s *Service) Settings(ctx context.Context) (st Settings, explicit, ok bool, err error) {
	set, err := s.Store.Setting(ctx, settingName, &st)
	if err != nil {
		return st, false, false, err
	}
	if set {
		return st, true, st.Pattern != "", nil
	}
	dims, err := s.Store.ListDeployDimensions(ctx)
	if err != nil {
		return st, false, false, err
	}
	for _, d := range dims {
		if d.Source == "paths" {
			return Settings{Pattern: d.PathPattern, Dimension: d.Name}, false, true, nil
		}
	}
	return st, false, false, nil
}

// ValidatePattern checks a project pattern.
func ValidatePattern(p string) error {
	if strings.Count(p, "*") != 1 || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return errors.New("the pattern needs exactly one *, relative to the repository root (e.g. services/*)")
	}
	prefix, _, _ := strings.Cut(p, "*")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return errors.New("the * must be a whole directory name (services/*, not services/app-*)")
	}
	return nil
}

// SaveSettings stores explicit settings; an empty pattern turns projects
// off, nil goes back to the deploy dimension default.
func (s *Service) SaveSettings(ctx context.Context, st *Settings) error {
	if st == nil {
		return s.Store.SetSetting(ctx, settingName, nil)
	}
	if st.Pattern != "" {
		if err := ValidatePattern(st.Pattern); err != nil {
			return err
		}
	}
	return s.Store.SetSetting(ctx, settingName, st)
}

func (s *Service) defaultSHA(ctx context.Context) (string, error) {
	branch := s.Repo.HeadBranch(ctx)
	if branch == "" {
		branch = s.Cfg.Repo.DefaultBranch
	}
	return s.Repo.ResolveCommit(ctx, "refs/heads/"+branch)
}

// List returns the projects on the default branch, sorted by name.
func (s *Service) List(ctx context.Context) ([]Project, Settings, error) {
	st, _, ok, err := s.Settings(ctx)
	if err != nil || !ok {
		return nil, st, err
	}
	sha, err := s.defaultSHA(ctx)
	if err != nil {
		return nil, st, nil // empty repository
	}
	s.mu.Lock()
	c := s.cached
	s.mu.Unlock()
	names := c.names
	if c.sha != sha || c.pattern != st.Pattern {
		names = s.Repo.MatchDirs(ctx, sha, st.Pattern)
		sort.Strings(names)
		s.mu.Lock()
		s.cached = listCache{sha: sha, pattern: st.Pattern, names: names}
		s.mu.Unlock()
	}
	out := make([]Project, len(names))
	for i, n := range names {
		out[i] = Project{Name: n, Dir: st.Dir(n)}
	}
	return out, st, nil
}

var ErrNotFound = errors.New("no such project")

// Get finds a project by name.
func (s *Service) Get(ctx context.Context, name string) (*Project, Settings, error) {
	list, st, err := s.List(ctx)
	if err != nil {
		return nil, st, err
	}
	for _, p := range list {
		if p.Name == name {
			return &p, st, nil
		}
	}
	return nil, st, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Owners are the code owners of a path at a commit: the CODEOWNERS file
// there plus the server-side owners of the default branch's protection.
// Directories are given without a trailing slash; "" is the root.
func (s *Service) Owners(ctx context.Context, sha, p string, dir bool) []string {
	return s.OwnersAt(ctx, sha).Of(p, dir)
}

// OwnerRules are the parsed code owner rules at one commit.
type OwnerRules struct{ co *pulls.CodeOwners }

// OwnersAt reads and parses the code owner rules once, for looking up many
// paths at the same commit.
func (s *Service) OwnersAt(ctx context.Context, sha string) OwnerRules {
	var src strings.Builder
	for _, f := range pulls.CodeOwnersPaths {
		if b, err := s.Repo.ReadBlob(ctx, sha, f, 1<<20); err == nil {
			src.Write(b)
			src.WriteString("\n")
			break
		}
	}
	branch := s.Repo.HeadBranch(ctx)
	if branch == "" {
		branch = s.Cfg.Repo.DefaultBranch
	}
	if prot, err := s.Store.BranchProtectionFor(ctx, branch); err == nil && prot != nil {
		src.WriteString(prot.Owners)
	}
	if src.Len() == 0 {
		return OwnerRules{}
	}
	return OwnerRules{pulls.ParseCodeOwners(src.String())}
}

// Of returns the owners of a path; see Owners.
func (o OwnerRules) Of(p string, dir bool) []string {
	if o.co == nil {
		return nil
	}
	if dir {
		p += "/"
	}
	owners, _ := o.co.Owners(strings.TrimPrefix(p, "/"))
	return owners
}
