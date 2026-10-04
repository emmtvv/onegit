package ci

import (
	"slices"
	"testing"
	"time"
)

func TestMatrixAndFilesSpec(t *testing.T) {
	p, err := ParsePipeline("m.yml", []byte(`
on:
  merge_queue:
    branches: [main]
  schedule:
    - cron: "0 3 * * *"
    - cron: "@hourly"
      timezone: Europe/Moscow
jobs:
  test:
    runs-on: ["${matrix.os}"]
    matrix:
      go: ["1.22", "1.23"]
      os: [linux, darwin]
      exclude:
        - {go: "1.22", os: darwin}
      include:
        - {go: "1.24", os: linux, experimental: "true"}
    artifacts:
      paths: [dist/, "reports/*.xml"]
      when: always
      expire-in: 3d
    cache:
      key: go-${matrix.go}
      key-files: [go.sum]
      paths: [.cache/go]
    steps: [{run: go test}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.On.Schedule) != 2 || p.On.Schedule[1].Timezone != "Europe/Moscow" || p.On.MergeQueue == nil {
		t.Errorf("triggers = %+v", p.On)
	}
	j := p.Jobs["test"]
	var labels []string
	for _, c := range j.Matrix.Combinations() {
		labels = append(labels, j.Matrix.Label(c))
	}
	want := []string{"1.22, linux", "1.23, linux", "1.23, darwin", "1.24, linux, true"}
	if !slices.Equal(labels, want) {
		t.Errorf("labels = %q, want %q", labels, want)
	}
	if time.Duration(j.Artifacts.ExpireIn) != 72*time.Hour || j.Artifacts.When != "always" || j.Cache.KeyFiles[0] != "go.sum" {
		t.Errorf("artifacts %+v cache %+v", j.Artifacts, j.Cache)
	}
	if got := ExpandMatrix("go-${matrix.go} on ${matrix.os} ${matrix.none}", map[string]string{"go": "1.23", "os": "linux"}); got != "go-1.23 on linux ${matrix.none}" {
		t.Errorf("expand = %q", got)
	}

	// Merge queue events match merge_queue and pull_request triggers.
	if !p.Matches(Event{Name: "merge_queue", BaseRef: "main"}) || p.Matches(Event{Name: "merge_queue", BaseRef: "dev"}) {
		t.Error("merge_queue branch filter")
	}
	pr, _ := ParsePipeline("pr.yml", []byte("on: pull_request\njobs:\n  a: {steps: [{run: x}]}\n"))
	if !pr.Matches(Event{Name: "merge_queue", BaseRef: "main"}) {
		t.Error("pull_request pipelines must run in the merge queue")
	}
	if pr.Matches(Event{Name: "schedule"}) {
		t.Error("schedule events are not matched by triggers")
	}
	if !eventAllowed([]string{"pull_request"}, "merge_queue") || eventAllowed([]string{"push"}, "merge_queue") || !eventAllowed(nil, "schedule") {
		t.Error("eventAllowed")
	}

	for name, src := range map[string]string{
		"bad cron":          "on:\n  schedule: [{cron: '61 * * * *'}]\njobs:\n  a: {steps: [{run: x}]}\n",
		"schedule not list": "on:\n  schedule: {cron: '* * * * *'}\njobs:\n  a: {steps: [{run: x}]}\n",
		"bad tz":            "on:\n  schedule: [{cron: '* * * * *', timezone: Nowhere/City}]\njobs:\n  a: {steps: [{run: x}]}\n",
		"empty matrix":      "on: push\njobs:\n  a: {matrix: {go: []}, steps: [{run: x}]}\n",
		"all excluded":      "on: push\njobs:\n  a: {matrix: {go: [1], exclude: [{go: 1}]}, steps: [{run: x}]}\n",
		"repeated combo":    "on: push\njobs:\n  a: {matrix: {go: [1], include: [{go: 1}]}, steps: [{run: x}]}\n",
		"bad matrix key":    "on: push\njobs:\n  a: {matrix: {'a b': [1]}, steps: [{run: x}]}\n",
		"huge matrix":       "on: push\njobs:\n  a: {matrix: {x: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17], y: [1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16]}, steps: [{run: x}]}\n",
		"artifacts no path": "on: push\njobs:\n  a: {artifacts: {when: always}, steps: [{run: x}]}\n",
		"artifacts outside": "on: push\njobs:\n  a: {artifacts: {paths: [../x]}, steps: [{run: x}]}\n",
		"artifacts abs":     "on: push\njobs:\n  a: {artifacts: {paths: [/etc]}, steps: [{run: x}]}\n",
		"artifacts when":    "on: push\njobs:\n  a: {artifacts: {paths: [x], when: sometimes}, steps: [{run: x}]}\n",
		"cache no key":      "on: push\njobs:\n  a: {cache: {paths: [x]}, steps: [{run: x}]}\n",
		"cache policy":      "on: push\njobs:\n  a: {cache: {key: k, paths: [x], policy: never}, steps: [{run: x}]}\n",
		"cache key file":    "on: push\njobs:\n  a: {cache: {key: k, paths: [x], key-files: [../go.sum]}, steps: [{run: x}]}\n",
	} {
		if _, err := ParsePipeline("p.yml", []byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
