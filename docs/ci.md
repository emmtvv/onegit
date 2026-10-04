# CI pipelines and runners

onegit separates **building** from **deploying**. Pipelines build and test;
they come from the repository, run for any branch or pull request, and never
receive secrets, so editing them is harmless. Deployments are a separate,
server-side process with their own rules ([deployments.md](deployments.md)).

## Pipelines

Each `.onegit/pipelines/*.yml` file is a pipeline, read from the commit being
built:

```yaml
name: ci                      # default: the file name
on:
  push:
    branches: [main, "release/*"]
    tags: ["v*"]              # tags ignore path filters
    paths: ["services/**"]
    paths-ignore: ["**/*.md"]
  pull_request:
    branches: [main]          # base branches
  merge_queue:                # merge queue candidates (pull_request pipelines run there too)
    branches: [main]
  schedule:                   # from the default branch
    - cron: "0 3 * * *"       # minute hour day month weekday, UTC
    - cron: "@hourly"
      timezone: Europe/Berlin
  manual:                     # "Run" button on the Actions page
env:
  GOFLAGS: -mod=readonly
jobs:
  test:
    runs-on: [linux]          # labels a runner must have
    timeout: 30m              # default 1h
    steps:
      - run: make test
  image:
    needs: [test]             # runs after test succeeded; skipped otherwise
    events: [push]            # only for these events
    runs-on: [linux, docker]
    env: {IMAGE: app}
    steps:
      - name: Build and push
        run: |
          echo "$ONEGIT_TOKEN" | docker login "$ONEGIT_REGISTRY" -u ci --password-stdin
          docker build -t "$ONEGIT_REGISTRY/$IMAGE:$ONEGIT_SHA" .
          docker push "$ONEGIT_REGISTRY/$IMAGE:$ONEGIT_SHA"
      - run: ./scripts/smoke.sh
        working-directory: services/api
        continue-on-error: true
```

- A trigger key without a value (`pull_request:`) enables the event for
  everything; `on: push` and `on: [push, pull_request]` work too.
- Path filters use globs where `*` stays within a directory and `**`
  crosses directories. A push compares with the previous tip; a new branch
  compares with where it forked from the default branch.
- Invalid pipeline files are reported as a failed `onegit / pipelines`
  status on the commit.
- `events:` on a job limits it to some events (`push`, `pull_request`,
  `merge_queue`, `schedule`, `manual`); a job for `pull_request` also runs in
  the merge queue.

### Schedules

`schedule` runs the pipeline from the tip of the default branch whenever its
cron expression comes due: five fields (minute, hour, day of month, month,
day of week) with lists, ranges, steps and names (`*/15 9-18 * * mon-fri`),
or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`. Times are UTC
unless `timezone` names an IANA zone. A new schedule waits for its next time;
times missed while the server was down collapse into one run. The Actions
page lists the schedules with their next run. Jobs see
`ONEGIT_EVENT=schedule`.

### Matrix

```yaml
jobs:
  test:
    runs-on: ["${matrix.os}"]
    matrix:
      go: ["1.22", "1.23"]
      os: [linux, arm64]
      exclude: [{go: "1.22", os: arm64}]
      include: [{go: "1.24", os: linux}]
    steps:
      - run: go${matrix.go} test ./...
  report:
    needs: test               # waits for every job of the matrix
    steps: [{run: ./report.sh}]
```

A job runs once per combination: the cross product of the lists, minus the
`exclude` entries, plus the `include` entries. The jobs are named
`test (1.22, linux)` and report the status `<pipeline> / test (1.22, linux)`.
`${matrix.<key>}` is replaced in `runs-on`, `env`, the cache key and the
steps' `run`, `name` and `working-directory`; the values are also in
`MATRIX_<KEY>` (upper case). A job that `needs` a matrix job waits for all of
its combinations and is skipped if one of them does not succeed. A matrix
has at most 256 combinations.

### Artifacts

```yaml
jobs:
  build:
    artifacts:
      paths: [dist/, "reports/*.xml"]   # files, directories or globs in the workspace
      when: on_success                  # on_failure or always
      expire-in: 7d                     # default ONEGIT_CI_ARTIFACT_RETENTION_DAYS
    steps: [{run: make dist}]
  e2e:
    needs: build                        # gets dist/ and reports/ in its workspace
    steps: [{run: ./e2e.sh dist/}]
```

After its steps a job packs the paths into a tar.gz and uploads it to S3.
Jobs that `needs` it find the files at the same paths in their workspace
before their steps start. Run and job pages offer the archive for download
until it expires. A failed upload fails the job.

### Caches

```yaml
jobs:
  test:
    cache:
      key: go-${matrix.go}
      key-files: [go.sum]               # their hash is added to the key
      paths: [.cache/go]
      policy: pull-push                 # pull (restore only) or push (save only)
    env: {GOMODCACHE: "$ONEGIT_WORKSPACE/.cache/go"}
    steps: [{run: go test ./...}]
```

Before the steps the runner restores the cache whose key is `key` plus a hash
of `key-files`; without an exact match it takes the newest cache whose key
starts with `key-`. After successful steps it saves the paths under the exact
key, unless that key was restored. Caches are immutable and scoped to the
ref that saved them: a job reads its own branch's (or pull request's)
caches and the default branch's, and writes only its own, so a branch cannot
change what `main` restores. Caches unused for
`ONEGIT_CI_CACHE_RETENTION_DAYS` are removed, and the least recently used go
when the total exceeds `ONEGIT_CI_MAX_CACHE_SIZE`.

Paths of artifacts and caches stay inside the workspace: archives whose
entries would land outside it are refused.

Every job reports a **commit status** named `<pipeline> / <job>` (skipped
counts as passing). Branch protection can require them, and pull requests
show them.

### Job environment

Steps run with `bash -eo pipefail` (or `sh -e`) in a fresh checkout of the
commit. Environment:

| Variable | |
|---|---|
| `CI`, `ONEGIT` | `true` |
| `ONEGIT_SERVER_URL`, `ONEGIT_REPOSITORY` | Base URL and repository name |
| `ONEGIT_REGISTRY` | Registry host for `docker login` / image names |
| `ONEGIT_EVENT` | `push`, `pull_request`, `merge_queue`, `schedule`, `manual` or `deploy` |
| `ONEGIT_REF`, `ONEGIT_REF_NAME` | `refs/heads/main` and `main`; `refs/pull/7/head` for pull requests, `refs/merge-queue/<n>` in the merge queue |
| `ONEGIT_SHA`, `ONEGIT_BEFORE_SHA` | The commit, and the previous tip (pushes) or merge base (pull requests) |
| `ONEGIT_PULL_REQUEST` | Pull request number (pull requests and the merge queue) |
| `MATRIX_<KEY>` | Matrix values of the job |
| `ONEGIT_CHANGED_FILES` | Path of a file listing the changed paths, one per line |
| `ONEGIT_RUN_ID`, `ONEGIT_JOB_ID`, `ONEGIT_JOB`, `ONEGIT_PIPELINE`, `ONEGIT_ACTOR` | |
| `ONEGIT_TOKEN` | Job token, valid while the job runs |
| `ONEGIT_WORKSPACE` | The checkout |
| `DOCKER_CONFIG` | A per-job Docker config: `docker login` stays inside the job |

`ONEGIT_TOKEN` can fetch the repository over git and pull from the registry.
Pipeline jobs may also push images; the registry records the job, commit and
ref that built them, and tags built by CI can't be overwritten by people.
Deployment rules use this to accept only images CI built from the deployed
commit.

## Runners

A runner is the same binary: `onegit runner`. It polls the server over HTTP
(no inbound connections), keeps a cache of the repository and makes a cheap
local clone per job.

1. **Admin → Runners → New runner**: a name, the kind (`ci` for pipeline
   jobs, `deploy` for deployments) and labels (`linux, docker`). Deploy
   runners also get a target selector, for example `environment=production`.
   The token is shown once.
2. Start it on a machine with git and bash (and Docker if your steps use it):

```sh
onegit runner -url https://git.example.com -token ogrun_... -workdir /var/lib/onegit-runner -capacity 2
```

or with Docker:

```sh
docker run -d --name onegit-runner --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e ONEGIT_RUNNER_URL=https://git.example.com -e ONEGIT_RUNNER_TOKEN=ogrun_... \
  -e ONEGIT_RUNNER_WORKDIR=/root/work \
  emmtvv/onegit runner
```

The image runs as root and has git, bash, make and the Docker CLI with
buildx. Steps talk to the host's Docker daemon through the mounted socket;
leave it out if your steps don't use Docker. For other tools (compilers, ...),
derive an image from this one or run the binary on the host.

A runner takes jobs whose `runs-on` labels are a subset of its own. Steps
run **directly on the runner host**, in their own process group: cancelling
a run stops the whole group. Jobs whose runner stops reporting for two
minutes fail. Use separate machines for CI and deploy runners: CI runs code
from any branch, deploy runners hold production credentials.
