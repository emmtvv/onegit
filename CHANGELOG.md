# Changelog

Notable changes to onegit are recorded here, following
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[semantic versioning](https://semver.org/); before 1.0 minor versions may
include breaking changes, always described here.

## [Unreleased]

## [1.3.0] - 2026-10-04

### Changed

- The image runs as root and ships bash, make and the Docker CLI with
  buildx, so it runs CI and deploy runners against the host's Docker socket
  without a derived image. Existing repository volumes owned by uid 1000
  keep working.
- Restyled the merge box on pull requests: a requirements counter with a
  progress bar, compact rows and a one-line merge form.

## [1.2.0] - 2026-10-04

### Added

- **Blame** for every file: who last changed each line, coloured by age, with
  a link to the blame from before a change.
- **Code search** from the top bar: text or regular expressions over any
  branch, tag or commit, file name search, and `path:` / `-path:` filters
  with globs.
- **Projects**: directories of the monorepo (`services/*`, set in Admin →
  Projects or taken from a deploy dimension) get a page with their code
  owners, open pull requests, CI runs, README and what is deployed to each
  target. Pull requests and pipeline runs can be filtered by project; the
  code view shows the owners of the current directory or file.
- **Auto-merge**: a pull request merges (or joins the merge queue) once its
  approvals, code owners and checks are in.
- **Merge queue**, required per branch in its protection rule: queued pull
  requests are tested on top of the branch and of the ones ahead of them,
  several at once (merge queue depth), and land together as soon as a
  candidate passes. Pipelines run for it with a `merge_queue` or
  `pull_request` trigger.
- **Scheduled pipelines** (`on: schedule: [{cron: ...}]`, with time zones),
  listed with their next run on the Actions page.
- **Matrix jobs**: a job runs once per combination of values, with
  `${matrix.key}` substitution and `MATRIX_*` variables; `needs` waits for
  the whole matrix.
- **Artifacts**: files a job keeps (`artifacts: {paths: [...]}`) are put
  into the workspace of the jobs that need it and can be downloaded from the
  run and job pages until they expire.
- **Caches** keyed by name and a hash of key files, restored before the
  steps and saved after them, scoped per branch so that branches cannot
  poison the default branch's caches.
- Settings `ONEGIT_CI_MAX_ARTIFACT_SIZE`, `ONEGIT_CI_ARTIFACT_RETENTION_DAYS`,
  `ONEGIT_CI_MAX_CACHE_SIZE` and `ONEGIT_CI_CACHE_RETENTION_DAYS`.

### Fixed

- Merging a pull request in the web UI did not start the push pipelines of
  the base branch.

## [1.1.1] - 2026-10-04

### Fixed

- The deployment page now refreshes itself while the deployment is pending
  or running, so its status, jobs and reviews no longer stay as they were
  when the page was opened.

### Removed

- The table of last deployed versions per target on the Deployments page.

## [1.1.0] - 2026-09-28

### Fixed

- MinIO answered `400 Bad Request` at startup when `ONEGIT_S3_ENDPOINT`
  named a host with an underscore: it rejects such a Host header. onegit now
  sends the name with hyphens and still connects to the real host.

## [1.0.1] - 2026-09-28

### Changed

- **PostgreSQL 18** in the compose stack. Its data now lives in the new
  `pgdata` volume, mounted at `/var/lib/postgresql`. Existing installations
  must dump and restore the database; see
  [PostgreSQL major versions](docs/operations.md#postgresql-major-versions).

### Fixed

- A runner that hung up while a job was being claimed could leave the job
  running with no one to execute it until it timed out as lost.

## [1.0.0] - 2026-09-27

First public release.

### Added

- **Git hosting** for a single repository over smart HTTP (protocol v2,
  gzip, chunked pushes of any size), with password, personal access token or
  job token authentication.
- **Code browsing**: tree and file views with syntax highlighting, rendered
  Markdown READMEs, raw files, history with path filter, commit diffs,
  branches with ahead/behind counts, tags. File icons per file type.
- **Pull requests**: compare and create, conversation with line comments and
  outdated-thread detection, reviews (approve / request changes), conflict
  detection without a worktree, squash (default), merge and rebase styles,
  close/reopen, delete branch after merge, detection of PRs merged by a plain
  push.
- **Branch protection**: require pull requests, required approvals,
  dismissal of stale approvals, code owner review, block on requested
  changes, required status checks, server-side code owners, force-push and
  deletion control.
- **CODEOWNERS** (GitHub syntax, read from the base branch), with
  `@user`, email, `@team` and `@org/team` owners.
- **CI pipelines** from `.onegit/pipelines/*.yml`: push, pull request and
  manual triggers with branch, tag and path filters, job dependencies,
  per-event jobs, commit statuses, re-runs and cancellation, live logs.
- **Runners**: the same binary (`onegit runner`), polling over HTTP, with an
  isolated checkout and Docker config per job and process-group cancellation.
- **Deployments** with server-side policy: deploy targets from admin-defined
  dimensions, grants and requirements stored in the database (branches,
  passing checks, CI-built artifacts, approvals, freezes, exceptions for
  reviewed branches), recipes read only from the default branch, encrypted
  and scoped secrets masked in logs, per-target serialisation, audit log.
- **Container registry** (OCI distribution API) with S3 storage: chunked and
  monolithic uploads, cross-repository mounts, multi-platform indexes,
  deletes, garbage collection, cleanup rules, size limits, CI provenance with
  immutable CI-built tags, and a Gitea-compatible packages API.
- **Accounts**: local users with roles (read, write, admin), forced change of
  temporary passwords, login rate limiting, personal access tokens, OIDC
  single sign-on with group-to-role and group-to-team mapping.
- **Operations**: stateless server (Postgres, Redis, S3 and one repository
  volume), several replicas, `/healthz` and `/readyz`, Docker image for
  amd64 and arm64, `docker-compose.yml` with the full stack, admin CLI
  (`list-users`, `reset-password`, `registry-gc`).

[Unreleased]: https://github.com/emmtvv/onegit/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/emmtvv/onegit/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/emmtvv/onegit/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/emmtvv/onegit/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/emmtvv/onegit/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/emmtvv/onegit/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/emmtvv/onegit/releases/tag/v1.0.0
