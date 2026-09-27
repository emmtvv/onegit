package registry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"onegit/internal/store"
)

// Cleanup rules follow Gitea's package cleanup rules: per image, versions
// newest first; the first KeepCount are kept, then a version is removed
// unless it matches KeepPattern, is younger than RemoveDays, or (when
// RemovePattern is set) does not match RemovePattern. "latest" is never
// removed. Patterns are case-insensitive regular expressions that must match
// the whole version ("<image>/<version>" with MatchFullName).

type CleanupCandidate struct {
	Repo      string
	Version   string
	Tagged    bool
	UpdatedAt time.Time
	Size      int64
}

func compilePattern(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, nil
	}
	return regexp.Compile(`(?i)\A(?:` + p + `)\z`)
}

// ValidateCleanupRule checks the patterns.
func ValidateCleanupRule(r *store.RegistryCleanupRule) error {
	if _, err := compilePattern(r.KeepPattern); err != nil {
		return fmt.Errorf("keep pattern: %w", err)
	}
	if _, err := compilePattern(r.RemovePattern); err != nil {
		return fmt.Errorf("remove pattern: %w", err)
	}
	if r.KeepCount < 0 || r.RemoveDays < 0 {
		return errors.New("counts must not be negative")
	}
	return nil
}

// CleanupPlan lists the versions the rule would remove now.
func (s *Service) CleanupPlan(ctx context.Context, r *store.RegistryCleanupRule) ([]CleanupCandidate, error) {
	keep, err := compilePattern(r.KeepPattern)
	if err != nil {
		return nil, err
	}
	remove, err := compilePattern(r.RemovePattern)
	if err != nil {
		return nil, err
	}
	versions, _, err := s.Store.ListRegistryVersions(ctx, store.RegistryVersionFilter{})
	if err != nil {
		return nil, err
	}
	byRepo := map[string][]store.RegistryVersion{}
	for _, v := range versions {
		byRepo[v.Manifest.Repo] = append(byRepo[v.Manifest.Repo], v)
	}
	olderThan := time.Now().Add(-time.Duration(r.RemoveDays) * 24 * time.Hour)
	var out []CleanupCandidate
	for repo, vs := range byRepo {
		sort.SliceStable(vs, func(i, j int) bool { return vs[i].UpdatedAt.After(vs[j].UpdatedAt) })
		for i, v := range vs {
			if i < r.KeepCount || strings.EqualFold(v.Name, "latest") {
				continue
			}
			match := v.Name
			if r.MatchFullName {
				match = repo + "/" + v.Name
			}
			switch {
			case keep != nil && keep.MatchString(match):
				continue
			case v.UpdatedAt.After(olderThan):
				continue
			case remove != nil && !remove.MatchString(match):
				continue
			}
			out = append(out, CleanupCandidate{Repo: repo, Version: v.Name, Tagged: v.Tagged,
				UpdatedAt: v.UpdatedAt, Size: v.Manifest.TotalSize})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// DeleteVersion removes a tag (and the version behind it once nothing else
// uses it) or an untagged version by digest.
func (s *Service) DeleteVersion(ctx context.Context, repo, version string) error {
	if validDigest(version) {
		return s.Store.DeleteRegistryManifest(ctx, repo, version)
	}
	return s.Store.DeleteRegistryTag(ctx, repo, version, true)
}

// RunCleanup applies the rule if it is enabled (or force is set) and returns
// the number of removed versions. Layers are freed later by GC.
func (s *Service) RunCleanup(ctx context.Context, force bool) (int, error) {
	r, err := s.Store.RegistryCleanupRule(ctx)
	if err != nil || (!r.Enabled && !force) {
		return 0, err
	}
	plan, err := s.CleanupPlan(ctx, r)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, c := range plan {
		err := s.DeleteVersion(ctx, c.Repo, c.Version)
		switch {
		case err == nil:
			removed++
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrReferenced):
		default:
			return removed, err
		}
	}
	return removed, s.Store.MarkRegistryCleanupRun(ctx, removed)
}
