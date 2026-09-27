# Security Policy

onegit holds your source code, deployment credentials and the rules that
decide who may deploy where, so security reports are taken seriously.

## Reporting a vulnerability

Please do **not** open a public GitHub issue for a security vulnerability.

Use GitHub's private reporting flow instead:
[Security → Report a vulnerability](https://github.com/emmtvv/onegit/security/advisories/new).
This opens a private advisory visible only to the maintainer until a fix is
ready.

Include, if known: the affected version or commit, the impact, and steps to
reproduce. You can expect a first response within a week.

## Supported versions

onegit does not maintain release branches yet: fixes land on `main` and in the
next release. Run the latest release to get security fixes.

## Scope

In scope: the `onegit` binary (server, runner, hooks, admin CLI), the
published `emmtvv/onegit` Docker image, and the example deployment in
`docker-compose.yml`.

Of particular interest:

- authentication and sessions (passwords, access tokens, OIDC, job tokens);
- authorisation: repository roles, branch protection, deploy grants,
  requirements and approvals, registry access;
- anything that lets repository content (pipelines, recipes, CODEOWNERS)
  obtain secrets or bypass the server-side deployment rules;
- secret handling: encryption at rest and masking in job logs.

Out of scope: vulnerabilities in third-party dependencies (report them
upstream; `go.mod` pins the versions in use), attacks that need an
administrator account or direct access to the database, Redis, S3 or the
repository volume, and code you run yourself in CI jobs (runners execute
steps on their host by design: run them on machines you are prepared to
give to your pipelines).
