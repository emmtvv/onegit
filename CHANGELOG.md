# Changelog

Notable changes to onegit are recorded here, following
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[semantic versioning](https://semver.org/); before 1.0 minor versions may
include breaking changes, always described here.

## [Unreleased]

## [1.5.2] - 2026-10-05

### Fixed

- The pull request conversation now lists commits. It shows the commits the
  pull request was opened with, and each push lists the commits it added
  (after a force-push, only the new tip). Before, a push appeared as a single
  line naming only its last commit, and the commits the pull request was
  opened with did not appear at all.
- Each listed commit shows the combined state of its checks. Clicking it
  expands the individual checks with links to their runs, so the pipelines a
  new push started are visible in the timeline.

## [1.5.1] - 2026-10-04

### Added

- **Avatars.** Users upload a picture under Settings (PNG, JPEG, GIF or
  WebP, up to 1 MB) or remove it. It is shown in the top bar, the admin
  user pages, and next to commits, pull requests and comments whose author
  email belongs to the user; others still get initials. Images are stored
  in the S3 bucket under `avatars/<user id>`; a new migration adds the
  table that records each avatar's source.
- With single sign-on, the identity provider's `picture` claim becomes the
  user's avatar on every sign-in and cannot be changed in onegit. Users
  registered earlier get it at their next sign-in. Without the claim users
  pick their own; unlinking the SSO identity keeps the picture and makes it
  editable.

### Changed

- The `picture` claim, like the groups claim, is read from userinfo when
  the ID token lacks it. Userinfo claims are now used only when their `sub`
  matches the ID token's, which also applies to groups.

## [1.5.0] - 2026-10-04

### Added

- A **Home** tab for signed-in users, and where signing in from the front page
  lands: pull requests waiting on your review as a code owner, your pull
  requests with their next step (fix a check, resolve conflicts, waiting for
  approvals, in the merge queue, ready to merge), deployments you can
  approve, your recent runs and the projects you own.
- **Projects** shows one column per environment, and each deployment says
  how many of the project's commits on the default branch it lacks, in red
  from 10 commits or a week. The list filters by **Mine** and **Needs
  attention** (failing CI or far behind) and has a Deploy button per project;
  the project page shows the same per target.

### Changed

- Navigation tab colours no longer depend on which tabs are shown.
- The code owners of open pull requests are worked out in the background
  when their files or the owner rules change, and stored for lookup by
  owner, so the review list on Home needs no git work. New migrations add
  the table and indexes; they run on start-up.

## [1.4.1] - 2026-10-04

### Changed

- The new deployment page is redesigned. Targets are picked with toggles per
  dimension, and the version with the newest branches and tags one click
  away; the chosen commit is shown with its author and age.
- A deployment plan beside the form lists every target as you pick it: what
  runs there now, the new commit, how many commits it adds or rolls back,
  and when the target was last deployed. After Check each target shows
  ready, needs approval or blocked with the reasons, and the plan says whose
  approval is missing. Changing the choice afterwards marks the check as
  out of date.
- The deploy button reads "Deploy now" when the checks pass and no approval
  is needed, and is disabled when a check blocks the deployment.

## [1.4.0] - 2026-10-04

### Changed

- Scales to large repositories and histories: tens of thousands of
  branches and tags, millions of commits, CI runs and pull requests,
  thousands of projects.
  - Pages under a branch or tag resolve it in one git call instead of
    listing every ref; the branch/tag picker loads matching refs on demand.
  - Branches and tags pages are paginated and searchable, with ahead/behind
    counted in one pass per page. Sorted ref lists are cached and updated
    incrementally.
  - Actions and pull request lists page with cursors (`?before=`/`?after=`)
    instead of offsets; commit history pages stay on the commit the first
    page showed.
  - Project, user and package lists are paginated; the projects page uses a
    fixed number of queries however many projects there are.
  - Filtering runs and pull requests by project uses an index of changed
    directories (migration 009 builds it from existing data).
  - Job pages open with the end of the log and keep at most 20,000 lines;
    raw logs are streamed.
  - Pull request counts stop at 10,000 ("10000+").
  - Code search, blame and the tree's last-commit column share a bounded
    number of slots per replica; when all stay busy the page says so
    instead of piling up git processes.
- The repository is maintained in the background (packed refs, geometric
  repacks with bitmaps, commit-graph with changed-path Bloom filters, a
  weekly gc) by one replica at a time; git no longer runs gc during pushes.

### Added

- `ONEGIT_CI_LOG_RETENTION_DAYS` removes old job logs (off by default).

## [1.3.2] - 2026-10-04

### Added

- CI statuses update without a page reload: the pull request merge box,
  Actions, run and job pages, projects, the merge queue and deployments
  re-fetch themselves every few seconds while something is in flight, and
  every 15 seconds otherwise. Edited merge fields and open sections survive
  the refresh.

## [1.3.1] - 2026-10-04

### Fixed

- The runner reads its capacity from `ONEGIT_RUNNER_CAPACITY`; before only
  the `-capacity` flag worked.

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

[Unreleased]: https://github.com/emmtvv/onegit/compare/v1.5.2...HEAD
[1.5.2]: https://github.com/emmtvv/onegit/compare/v1.5.1...v1.5.2
[1.5.1]: https://github.com/emmtvv/onegit/compare/v1.5.0...v1.5.1
[1.5.0]: https://github.com/emmtvv/onegit/compare/v1.4.1...v1.5.0
[1.4.1]: https://github.com/emmtvv/onegit/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/emmtvv/onegit/compare/v1.3.2...v1.4.0
[1.3.2]: https://github.com/emmtvv/onegit/compare/v1.3.1...v1.3.2
[1.3.1]: https://github.com/emmtvv/onegit/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/emmtvv/onegit/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/emmtvv/onegit/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/emmtvv/onegit/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/emmtvv/onegit/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/emmtvv/onegit/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/emmtvv/onegit/releases/tag/v1.0.0
