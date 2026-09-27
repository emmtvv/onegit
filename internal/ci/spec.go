// Package ci runs pipelines and deployments: it parses .onegit/pipelines and
// .onegit/deploy files, turns pushes, pull requests and approved deployments
// into runs of jobs, hands jobs to runners and records their results.
package ci

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	PipelinesDir = ".onegit/pipelines"
	RecipesDir   = ".onegit/deploy"
)

// StringList accepts a scalar or a sequence.
type StringList []string

func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*l = nil
			return nil
		}
		*l = StringList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*l = out
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list", n.Line)
}

// Duration parses "30m", "1h".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

type Step struct {
	Name            string            `yaml:"name" json:"name"`
	Run             string            `yaml:"run" json:"run"`
	Env             map[string]string `yaml:"env" json:"env,omitempty"`
	WorkingDir      string            `yaml:"working-directory" json:"working_dir,omitempty"`
	ContinueOnError bool              `yaml:"continue-on-error" json:"continue_on_error,omitempty"`
}

// ---- pipelines ----

type PushTrigger struct {
	Branches    StringList `yaml:"branches"`
	Tags        StringList `yaml:"tags"`
	Paths       StringList `yaml:"paths"`
	PathsIgnore StringList `yaml:"paths-ignore"`
}

type PRTrigger struct {
	Branches    StringList `yaml:"branches"` // base branches
	Paths       StringList `yaml:"paths"`
	PathsIgnore StringList `yaml:"paths-ignore"`
}

// Triggers is the "on:" block. A key that is present enables the event,
// even with an empty value ("push:"), as in GitHub Actions.
type Triggers struct {
	Push        *PushTrigger
	PullRequest *PRTrigger
	Manual      bool
}

func (t *Triggers) UnmarshalYAML(n *yaml.Node) error {
	enable := func(name string, v *yaml.Node) error {
		switch name {
		case "push":
			t.Push = &PushTrigger{}
			if v != nil && v.Tag != "!!null" {
				return v.Decode(t.Push)
			}
		case "pull_request":
			t.PullRequest = &PRTrigger{}
			if v != nil && v.Tag != "!!null" {
				return v.Decode(t.PullRequest)
			}
		case "manual", "workflow_dispatch":
			t.Manual = true
		default:
			return fmt.Errorf("line %d: unknown event %q (use push, pull_request, manual)", n.Line, name)
		}
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return enable(n.Value, nil)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := enable(c.Value, nil); err != nil {
				return err
			}
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := enable(n.Content[i].Value, n.Content[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("line %d: invalid \"on\"", n.Line)
}

type JobSpec struct {
	RunsOn  StringList        `yaml:"runs-on"`
	Needs   StringList        `yaml:"needs"`
	Events  StringList        `yaml:"events"` // run only for these events
	Env     map[string]string `yaml:"env"`
	Timeout Duration          `yaml:"timeout"`
	Steps   []Step            `yaml:"steps"`
}

type Pipeline struct {
	Name string              `yaml:"name"`
	On   Triggers            `yaml:"on"`
	Env  map[string]string   `yaml:"env"`
	Jobs map[string]*JobSpec `yaml:"jobs"`
}

var jobNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func ParsePipeline(file string, src []byte) (*Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(src, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(strings.TrimSuffix(file[strings.LastIndex(file, "/")+1:], ".yml"), ".yaml")
	}
	if len(p.Jobs) == 0 {
		return nil, fmt.Errorf("%s: no jobs", file)
	}
	for name, j := range p.Jobs {
		if !jobNameRe.MatchString(name) {
			return nil, fmt.Errorf("%s: invalid job name %q", file, name)
		}
		if j == nil || len(j.Steps) == 0 {
			return nil, fmt.Errorf("%s: job %s has no steps", file, name)
		}
		for _, n := range j.Needs {
			if _, ok := p.Jobs[n]; !ok {
				return nil, fmt.Errorf("%s: job %s needs unknown job %s", file, name, n)
			}
		}
		for i, st := range j.Steps {
			if strings.TrimSpace(st.Run) == "" {
				return nil, fmt.Errorf("%s: job %s step %d has no run", file, name, i+1)
			}
		}
	}
	if cyc := findCycle(p.Jobs); cyc != "" {
		return nil, fmt.Errorf("%s: jobs depend on each other in a cycle (%s)", file, cyc)
	}
	return &p, nil
}

func findCycle(jobs map[string]*JobSpec) string {
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(string) string
	visit = func(n string) string {
		switch state[n] {
		case 1:
			return n
		case 2:
			return ""
		}
		state[n] = 1
		for _, d := range jobs[n].Needs {
			if c := visit(d); c != "" {
				return c
			}
		}
		state[n] = 2
		return ""
	}
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if c := visit(n); c != "" {
			return c
		}
	}
	return ""
}

// Event describes what happened, for trigger matching.
type Event struct {
	Name    string // push, pull_request, manual
	Ref     string // refs/heads/x, refs/tags/y
	BaseRef string // pull_request: base branch
	Changed []string
	// ChangedKnown is false when the change set could not be computed (a new
	// branch without history): path filters then match.
	ChangedKnown bool
}

// Matches reports whether the pipeline runs for the event.
func (p *Pipeline) Matches(ev Event) bool {
	switch ev.Name {
	case "manual":
		return p.On.Manual
	case "push":
		t := p.On.Push
		if t == nil {
			return false
		}
		if branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/"); ok {
			if len(t.Branches) == 0 && len(t.Tags) > 0 {
				return false
			}
			if len(t.Branches) > 0 && !MatchAny(t.Branches, branch) {
				return false
			}
		} else if tag, ok := strings.CutPrefix(ev.Ref, "refs/tags/"); ok {
			if len(t.Tags) == 0 || !MatchAny(t.Tags, tag) {
				return false
			}
			return true // path filters do not apply to tags
		}
		return pathsMatch(t.Paths, t.PathsIgnore, ev)
	case "pull_request":
		t := p.On.PullRequest
		if t == nil {
			return false
		}
		if len(t.Branches) > 0 && !MatchAny(t.Branches, ev.BaseRef) {
			return false
		}
		return pathsMatch(t.Paths, t.PathsIgnore, ev)
	}
	return false
}

func pathsMatch(include, ignore []string, ev Event) bool {
	if (len(include) == 0 && len(ignore) == 0) || !ev.ChangedKnown {
		return true
	}
	for _, f := range ev.Changed {
		if len(include) > 0 && !MatchAny(include, f) {
			continue
		}
		if len(ignore) > 0 && MatchAny(ignore, f) {
			continue
		}
		return true
	}
	return false
}

// ---- deploy recipes ----

type ArtifactSpec struct {
	Image    string `yaml:"image" json:"image"` // e.g. self/${project}
	Tag      string `yaml:"tag" json:"tag"`     // default ${sha}
	Optional bool   `yaml:"optional" json:"optional"`
}

// Recipe is how to deploy to the targets it declares. Recipes are always read
// from the default branch, never from the commit being deployed.
type Recipe struct {
	File     string                `yaml:"-"`
	Name     string                `yaml:"name"`
	Targets  map[string]StringList `yaml:"targets"`
	RunsOn   StringList            `yaml:"runs-on"`
	Artifact *ArtifactSpec         `yaml:"artifact"`
	Tooling  bool                  `yaml:"tooling"` // also check out the recipe commit into $ONEGIT_TOOLING_DIR
	Env      map[string]string     `yaml:"env"`
	Timeout  Duration              `yaml:"timeout"`
	Steps    []Step                `yaml:"steps"`
}

func ParseRecipe(file string, src []byte) (*Recipe, error) {
	var r Recipe
	if err := yaml.Unmarshal(src, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	r.File = file
	if r.Name == "" {
		r.Name = strings.TrimSuffix(strings.TrimSuffix(file[strings.LastIndex(file, "/")+1:], ".yml"), ".yaml")
	}
	if len(r.Steps) == 0 {
		return nil, fmt.Errorf("%s: no steps", file)
	}
	for i, st := range r.Steps {
		if strings.TrimSpace(st.Run) == "" {
			return nil, fmt.Errorf("%s: step %d has no run", file, i+1)
		}
	}
	return &r, nil
}

// Serves reports whether the recipe declares the target.
func (r *Recipe) Serves(target map[string]string) bool {
	for dim, pats := range r.Targets {
		if !MatchAny(pats, target[dim]) {
			return false
		}
	}
	return true
}

// ---- globs ----

var globCache sync.Map // pattern → *regexp.Regexp

// Glob matches like path.Match, plus "**" crossing "/" ("dir/**", "**/x").
func Glob(pattern, s string) bool {
	re, ok := globCache.Load(pattern)
	if !ok {
		re, _ = globCache.LoadOrStore(pattern, compileGlob(pattern))
	}
	return re.(*regexp.Regexp).MatchString(s)
}

func compileGlob(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func MatchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == "*" || Glob(p, s) {
			return true
		}
	}
	return false
}
