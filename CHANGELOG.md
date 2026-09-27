# Changelog

Notable changes to onegit are recorded here, following
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[semantic versioning](https://semver.org/); before 1.0 minor versions may
include breaking changes, always described here.

## [Unreleased]

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

[Unreleased]: https://github.com/emmtvv/onegit/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/emmtvv/onegit/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/emmtvv/onegit/releases/tag/v1.0.0
