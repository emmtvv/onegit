package ci

import (
	"strings"
	"testing"
)

func TestParsePipeline(t *testing.T) {
	p, err := ParsePipeline(".onegit/pipelines/build.yml", []byte(`
on:
  push:
    branches: [main]
    paths: ["projects/**"]
  pull_request:
  manual:
jobs:
  test:
    runs-on: linux
    steps:
      - run: make test
  build:
    needs: test
    runs-on: [linux, docker]
    events: [push]
    timeout: 20m
    steps:
      - name: build
        run: make build
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "build" || p.On.Push == nil || p.On.PullRequest == nil || !p.On.Manual {
		t.Fatalf("triggers not parsed: %+v", p.On)
	}
	if got := p.Jobs["build"]; len(got.RunsOn) != 2 || got.Needs[0] != "test" || got.Events[0] != "push" {
		t.Fatalf("job not parsed: %+v", got)
	}
	if !p.Matches(Event{Name: "push", Ref: "refs/heads/main", Changed: []string{"projects/a/x.go"}, ChangedKnown: true}) {
		t.Error("push to main touching projects should match")
	}
	if p.Matches(Event{Name: "push", Ref: "refs/heads/main", Changed: []string{"README.md"}, ChangedKnown: true}) {
		t.Error("path filter ignored")
	}
	if p.Matches(Event{Name: "push", Ref: "refs/heads/dev", Changed: []string{"projects/a"}, ChangedKnown: true}) {
		t.Error("branch filter ignored")
	}
	if !p.Matches(Event{Name: "push", Ref: "refs/heads/main"}) {
		t.Error("unknown change set must match")
	}
	if !p.Matches(Event{Name: "pull_request", BaseRef: "anything", Changed: []string{"x"}, ChangedKnown: true}) {
		t.Error("empty pull_request trigger should match every PR")
	}

	for name, src := range map[string]string{
		"cycle":        "on: push\njobs:\n  a: {needs: b, steps: [{run: x}]}\n  b: {needs: a, steps: [{run: x}]}\n",
		"unknown need": "on: push\njobs:\n  a: {needs: zz, steps: [{run: x}]}\n",
		"no steps":     "on: push\njobs:\n  a: {runs-on: x}\n",
		"bad event":    "on: [push, cron]\njobs:\n  a: {steps: [{run: x}]}\n",
		"empty run":    "on: push\njobs:\n  a: {steps: [{name: x}]}\n",
	} {
		if _, err := ParsePipeline("p.yml", []byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestTags(t *testing.T) {
	p, err := ParsePipeline("r.yml", []byte("on:\n  push:\n    tags: ['v*']\njobs:\n  a: {steps: [{run: x}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Matches(Event{Name: "push", Ref: "refs/tags/v1.2"}) || p.Matches(Event{Name: "push", Ref: "refs/heads/main"}) {
		t.Error("tag-only trigger")
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"projects/**", "projects/a/b/c", true},
		{"projects/*", "projects/a/b", false},
		{"**/*.go", "a/b/c.go", true},
		{"**/*.go", "c.go", true},
		{"release/*", "release/1.0", true},
		{"main", "main2", false},
		{"*", "feature/x", true}, // "*" alone means everything
	} {
		if got := MatchAny([]string{c.pat}, c.s); got != c.want {
			t.Errorf("%q vs %q = %v", c.pat, c.s, got)
		}
	}
}

func TestSelectorAndRecipe(t *testing.T) {
	sel := map[string][]string{"environment": {"prod*"}, "project": {"api", "web"}}
	if !SelectorMatches(sel, map[string]string{"environment": "production", "project": "api", "region": "eu"}) {
		t.Error("should match")
	}
	if SelectorMatches(sel, map[string]string{"environment": "production", "project": "db"}) {
		t.Error("project not in list")
	}
	if !SelectorMatches(nil, map[string]string{"x": "y"}) {
		t.Error("empty selector matches everything")
	}
	r, err := ParseRecipe(".onegit/deploy/swarm.yml", []byte(`
targets:
  environment: [development, production]
runs-on: swarm
artifact: {image: "self/${project}", optional: true}
steps:
  - run: make deploy
`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Serves(map[string]string{"environment": "production", "project": "x"}) || r.Serves(map[string]string{"environment": "qa"}) {
		t.Error("recipe targets")
	}
	if r.Name != "swarm" || r.Artifact == nil || !strings.Contains(r.Artifact.Image, "${project}") {
		t.Errorf("recipe parsed wrong: %+v", r)
	}
}

func TestTriggerForms(t *testing.T) {
	for src, want := range map[string][3]bool{ // push, pull_request, manual
		"on: push":                          {true, false, false},
		"on: [push, pull_request]":          {true, true, false},
		"on: {workflow_dispatch: }":         {false, false, true},
		"on:\n  pull_request:\n  manual:\n": {false, true, true},
	} {
		p, err := ParsePipeline("x.yaml", []byte(src+"\njobs:\n  a: {steps: [{run: x}]}\n"))
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		if got := [3]bool{p.On.Push != nil, p.On.PullRequest != nil, p.On.Manual}; got != want {
			t.Errorf("%q: triggers %v, want %v", src, got, want)
		}
		if p.Name != "x" {
			t.Errorf("name from .yaml file = %q", p.Name)
		}
	}
	const job = "jobs:\n  a: {steps: [{run: x}]}\n"
	for name, src := range map[string]string{
		"scalar on":       "on: 5\n" + job,
		"bad push block":  "on: {push: [1]}\n" + job,
		"bad timeout":     "on: push\njobs:\n  a: {timeout: soon, steps: [{run: x}]}\n",
		"runs-on mapping": "on: push\njobs:\n  a: {runs-on: {x: y}, steps: [{run: x}]}\n",
		"bad job name":    "on: push\njobs:\n  'bad name': {steps: [{run: x}]}\n",
		"null job":        "on: push\njobs:\n  a:\n",
		"no jobs":         "on: push\n",
		"not yaml":        "{{{",
	} {
		if _, err := ParsePipeline("p.yml", []byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPathAndBranchFilters(t *testing.T) {
	p, err := ParsePipeline("p.yml", []byte(`
on:
  push:
    paths-ignore: ["docs/**", "*.md"]
    tags: ["v*"]
    branches: ["*"]
  pull_request:
    branches: [main]
    paths: ["src/**"]
jobs:
  a: {steps: [{run: x}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	ev := func(name, ref, base string, files ...string) Event {
		return Event{Name: name, Ref: ref, BaseRef: base, Changed: files, ChangedKnown: true}
	}
	for _, c := range []struct {
		ev   Event
		want bool
	}{
		{ev("push", "refs/heads/x", "", "docs/a.md", "README.md"), false},
		{ev("push", "refs/heads/x", "", "docs/a.md", "src/x.go"), true},
		{ev("push", "refs/tags/v1", "", "docs/a.md"), true}, // path filters don't apply to tags
		{ev("push", "refs/tags/rc1", ""), false},
		{ev("pull_request", "", "main", "src/a.go"), true},
		{ev("pull_request", "", "dev", "src/a.go"), false},
		{ev("pull_request", "", "main", "README.md"), false},
		{ev("manual", "refs/heads/main", ""), false},
		{ev("schedule", "refs/heads/main", ""), false},
	} {
		if got := p.Matches(c.ev); got != c.want {
			t.Errorf("%+v: %v, want %v", c.ev, got, c.want)
		}
	}
	noPush, _ := ParsePipeline("p.yml", []byte("on: manual\njobs:\n  a: {steps: [{run: x}]}\n"))
	if noPush.Matches(ev("push", "refs/heads/main", "")) || noPush.Matches(ev("pull_request", "", "main")) {
		t.Error("pipeline without push/pull_request triggers matched")
	}
}

func TestRecipeErrors(t *testing.T) {
	for name, src := range map[string]string{
		"no steps":  "targets: {env: [a]}\n",
		"empty run": "steps: [{name: x}]\n",
		"bad yaml":  "steps: [",
		"bad list":  "runs-on: {a: b}\nsteps: [{run: x}]\n",
	} {
		if _, err := ParseRecipe("r.yml", []byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r, err := ParseRecipe("dir/r.yaml", []byte("name: custom\nruns-on: deploy\ntimeout: 10m\nsteps: [{run: x}]\n"))
	if err != nil || r.Name != "custom" || r.RunsOn[0] != "deploy" || r.File != "dir/r.yaml" {
		t.Errorf("recipe = %+v, %v", r, err)
	}
	// A recipe without targets serves every target.
	if !r.Serves(map[string]string{"anything": "x"}) {
		t.Error("recipe without targets")
	}
}
