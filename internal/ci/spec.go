// Package ci runs pipelines and deployments: it parses .onegit/pipelines and
// .onegit/deploy files, turns pushes, pull requests and approved deployments
// into runs of jobs, hands jobs to runners and records their results.
package ci

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
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

// Duration parses "30m", "1h" and whole days ("7d").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if days, ok := strings.CutSuffix(n.Value, "d"); ok {
		if v, err := strconv.Atoi(days); err == nil && v >= 0 {
			*d = Duration(time.Duration(v) * 24 * time.Hour)
			return nil
		}
	}
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

// ScheduleTrigger runs the pipeline from the default branch at the times of
// a cron expression.
type ScheduleTrigger struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"` // IANA name; default UTC
}

// Triggers is the "on:" block. A key that is present enables the event,
// even with an empty value ("push:"), as in GitHub Actions.
type Triggers struct {
	Push        *PushTrigger
	PullRequest *PRTrigger
	// MergeQueue runs the pipeline on merge queue candidates only. Pipelines
	// with a pull_request trigger run there as well.
	MergeQueue *PRTrigger
	Schedule   []ScheduleTrigger
	Manual     bool
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
		case "merge_queue", "merge_group":
			t.MergeQueue = &PRTrigger{}
			if v != nil && v.Tag != "!!null" {
				return v.Decode(t.MergeQueue)
			}
		case "schedule":
			if v == nil || v.Kind != yaml.SequenceNode {
				return fmt.Errorf("line %d: schedule must be a list of {cron: \"...\"}", n.Line)
			}
			if err := v.Decode(&t.Schedule); err != nil {
				return err
			}
			for _, sc := range t.Schedule {
				if _, err := ParseCron(sc.Cron, sc.Timezone); err != nil {
					return fmt.Errorf("line %d: %w", v.Line, err)
				}
			}
		case "manual", "workflow_dispatch":
			t.Manual = true
		default:
			return fmt.Errorf("line %d: unknown event %q (use push, pull_request, merge_queue, schedule, manual)", n.Line, name)
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
	RunsOn    StringList        `yaml:"runs-on"`
	Needs     StringList        `yaml:"needs"`
	Events    StringList        `yaml:"events"` // run only for these events
	Env       map[string]string `yaml:"env"`
	Timeout   Duration          `yaml:"timeout"`
	Matrix    *Matrix           `yaml:"matrix"`
	Artifacts *ArtifactsSpec    `yaml:"artifacts"`
	Cache     *CacheSpec        `yaml:"cache"`
	Steps     []Step            `yaml:"steps"`
}

// ArtifactsSpec: files a job keeps after it ends. Jobs that need it get them
// in their workspace; people can download them from the job page.
type ArtifactsSpec struct {
	Paths    StringList `yaml:"paths" json:"paths"`
	When     string     `yaml:"when" json:"when,omitempty"` // on_success (default), on_failure, always
	ExpireIn Duration   `yaml:"expire-in" json:"expire_in,omitempty"`
}

// CacheSpec: directories restored before the steps and saved after them.
// The key is extended with a hash of key-files; when that exact key is
// missing, the newest cache with the same base key is restored.
type CacheSpec struct {
	Key      string     `yaml:"key" json:"key"`
	KeyFiles StringList `yaml:"key-files" json:"key_files,omitempty"`
	Paths    StringList `yaml:"paths" json:"paths"`
	Policy   string     `yaml:"policy" json:"policy,omitempty"` // pull-push (default), pull, push
}

// Matrix runs a job once per combination of values. Keys keep their order
// for job names ("test (1.22, linux)").
type Matrix struct {
	Keys    []string
	Values  map[string][]string
	Include []map[string]string
	Exclude []map[string]string
}

var matrixKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// maxMatrix bounds the jobs one matrix may produce.
const maxMatrix = 256

func (m *Matrix) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: matrix must be a mapping of lists", n.Line)
	}
	m.Values = map[string][]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		switch k {
		case "include", "exclude":
			var list []map[string]string
			if err := v.Decode(&list); err != nil {
				return fmt.Errorf("line %d: matrix %s must be a list of mappings", v.Line, k)
			}
			if k == "include" {
				m.Include = list
			} else {
				m.Exclude = list
			}
			continue
		}
		if !matrixKeyRe.MatchString(k) {
			return fmt.Errorf("line %d: invalid matrix key %q", v.Line, k)
		}
		var vals []string
		if err := v.Decode(&vals); err != nil || len(vals) == 0 {
			return fmt.Errorf("line %d: matrix %s must be a non-empty list of values", v.Line, k)
		}
		m.Keys = append(m.Keys, k)
		m.Values[k] = vals
	}
	return nil
}

// Combinations expands the matrix: the cross product of the values minus
// the exclude entries (an entry matches when all its keys match), plus the
// include entries as extra combinations.
func (m *Matrix) Combinations() []map[string]string {
	combos := []map[string]string{{}}
	for _, k := range m.Keys {
		var next []map[string]string
		for _, c := range combos {
			for _, v := range m.Values[k] {
				n := make(map[string]string, len(c)+1)
				for ck, cv := range c {
					n[ck] = cv
				}
				n[k] = v
				next = append(next, n)
			}
		}
		combos = next
	}
	if len(m.Keys) == 0 {
		combos = nil
	}
	var out []map[string]string
	for _, c := range combos {
		excluded := false
		for _, ex := range m.Exclude {
			match := len(ex) > 0
			for k, v := range ex {
				if c[k] != v {
					match = false
				}
			}
			excluded = excluded || match
		}
		if !excluded {
			out = append(out, c)
		}
	}
	return append(out, m.Include...)
}

// Label names a combination: values in key order, then extra include keys
// sorted.
func (m *Matrix) Label(c map[string]string) string {
	var parts []string
	seen := map[string]bool{}
	for _, k := range m.Keys {
		if v, ok := c[k]; ok {
			parts = append(parts, v)
			seen[k] = true
		}
	}
	var extra []string
	for k := range c {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		parts = append(parts, c[k])
	}
	return strings.Join(parts, ", ")
}

// ExpandMatrix substitutes ${matrix.key} in s.
func ExpandMatrix(s string, c map[string]string) string {
	if len(c) == 0 || !strings.Contains(s, "${matrix.") {
		return s
	}
	for k, v := range c {
		s = strings.ReplaceAll(s, "${matrix."+k+"}", v)
	}
	return s
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
		if j.Matrix != nil {
			combos := j.Matrix.Combinations()
			switch {
			case len(combos) == 0:
				return nil, fmt.Errorf("%s: job %s: the matrix has no combinations", file, name)
			case len(combos) > maxMatrix:
				return nil, fmt.Errorf("%s: job %s: the matrix has %d combinations (max %d)", file, name, len(combos), maxMatrix)
			}
			labels := map[string]bool{}
			for _, c := range combos {
				l := j.Matrix.Label(c)
				if labels[l] {
					return nil, fmt.Errorf("%s: job %s: the matrix repeats the combination (%s)", file, name, l)
				}
				labels[l] = true
			}
		}
		if a := j.Artifacts; a != nil {
			if len(a.Paths) == 0 {
				return nil, fmt.Errorf("%s: job %s: artifacts need paths", file, name)
			}
			if err := checkRelPaths(a.Paths); err != nil {
				return nil, fmt.Errorf("%s: job %s: artifacts: %w", file, name, err)
			}
			switch a.When {
			case "", "on_success", "on_failure", "always":
			default:
				return nil, fmt.Errorf("%s: job %s: artifacts when must be on_success, on_failure or always", file, name)
			}
		}
		if c := j.Cache; c != nil {
			if strings.TrimSpace(c.Key) == "" || len(c.Paths) == 0 {
				return nil, fmt.Errorf("%s: job %s: cache needs a key and paths", file, name)
			}
			if err := checkRelPaths(append(append([]string{}, c.Paths...), c.KeyFiles...)); err != nil {
				return nil, fmt.Errorf("%s: job %s: cache: %w", file, name, err)
			}
			switch c.Policy {
			case "", "pull-push", "pull", "push":
			default:
				return nil, fmt.Errorf("%s: job %s: cache policy must be pull-push, pull or push", file, name)
			}
		}
	}
	if cyc := findCycle(p.Jobs); cyc != "" {
		return nil, fmt.Errorf("%s: jobs depend on each other in a cycle (%s)", file, cyc)
	}
	return &p, nil
}

// checkRelPaths accepts paths inside the workspace only.
func checkRelPaths(paths []string) error {
	for _, p := range paths {
		c := path.Clean(p)
		if p == "" || path.IsAbs(p) || c == ".." || strings.HasPrefix(c, "../") {
			return fmt.Errorf("path %q must be relative to the workspace", p)
		}
	}
	return nil
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
	Name    string // push, pull_request, merge_queue, schedule, manual
	Ref     string // refs/heads/x, refs/tags/y
	BaseRef string // pull_request, merge_queue: base branch
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
		return prMatches(p.On.PullRequest, ev)
	case "merge_queue":
		return prMatches(p.On.MergeQueue, ev) || prMatches(p.On.PullRequest, ev)
	}
	return false
}

func prMatches(t *PRTrigger, ev Event) bool {
	if t == nil {
		return false
	}
	if len(t.Branches) > 0 && !MatchAny(t.Branches, ev.BaseRef) {
		return false
	}
	return pathsMatch(t.Paths, t.PathsIgnore, ev)
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
