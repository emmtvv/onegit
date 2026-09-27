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
| `ONEGIT_EVENT` | `push`, `pull_request`, `manual` or `deploy` |
| `ONEGIT_REF`, `ONEGIT_REF_NAME` | `refs/heads/main` and `main`; `refs/pull/7/head` for pull requests |
| `ONEGIT_SHA`, `ONEGIT_BEFORE_SHA` | The commit, and the previous tip (pushes) or merge base (pull requests) |
| `ONEGIT_PULL_REQUEST` | Pull request number |
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
  -e ONEGIT_RUNNER_URL=https://git.example.com -e ONEGIT_RUNNER_TOKEN=ogrun_... \
  -e ONEGIT_RUNNER_WORKDIR=/home/onegit/work \
  emmtvv/onegit runner
```

The image only has git and a shell; for real builds, derive an image with
the tools your steps need (compilers, the Docker CLI, ...), or run the binary
on the host.

A runner takes jobs whose `runs-on` labels are a subset of its own. Steps
run **directly on the runner host**, in their own process group: cancelling
a run stops the whole group. Jobs whose runner stops reporting for two
minutes fail. Use separate machines for CI and deploy runners: CI runs code
from any branch, deploy runners hold production credentials.
