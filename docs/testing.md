# Testing

```sh
make test               # unit tests only
make test-integration   # everything, with the race detector
make cover              # coverage across packages (coverage.out)
make check              # go vet + golangci-lint + test-integration: what CI runs
```

## Layout

Every package has tests next to its code:

| Package | What the tests cover |
|---|---|
| `internal/git` | The git wrapper against real repositories in temp dirs: refs, trees, blobs, history, diff parsing (renames, binaries, quoting, limits), merge-tree, commit-tree, compare-and-swap ref updates |
| `internal/config` | Defaults, YAML + environment layering, validation |
| `internal/auth` | Passwords and policy, rate limiting, access and job tokens, sessions, OIDC against a fake identity provider |
| `internal/store` | Every query against Postgres: migrations, users, pull requests, branch protection, teams, CI queue (job claiming under concurrency, `needs`, cancellation, heartbeats), deploy rules and deployments |
| `internal/pulls` | Creating, syncing and merging pull requests (squash, merge, rebase), conflicts, reviews, branch protection, CODEOWNERS |
| `internal/ci` | Pipeline parsing and trigger matching, runs from pushes and pull requests, commit statuses, the runner protocol |
| `internal/deploy` | Grants, requirements, approvals, freezes, recipes from the default branch, secrets |
| `internal/registry` | The distribution API through HTTP: auth, uploads, manifests, indexes, deletes, cleanup, GC, CI provenance, the Gitea API |
| `internal/runner` | The runner against a fake server: checkout, steps, masking, failures, cancellation, timeouts |
| `internal/transport`, `internal/hooks` | Smart HTTP (clone, push, auth, gzip) and the hook round trip |
| `internal/web` | Template parsing and rendering helpers (Markdown safety, redirects, selectors) |
| `internal/server` | End to end: a full server on a random port with real git pushes through the hooks, the web UI, the registry, and CI and deployments executed by a real runner |
| `cmd/onegit` | The CLI binary |

Shared helpers live in `internal/testutil`.

## Backing services

Integration tests need Postgres, Redis and S3. `docker-compose.test.yml`
starts them on local ports; the tests read their addresses from:

```sh
export ONEGIT_TEST_POSTGRES='postgres://onegit:onegit@127.0.0.1:55432/postgres?sslmode=disable'
export ONEGIT_TEST_REDIS=redis://127.0.0.1:56379/0
export ONEGIT_TEST_S3=http://127.0.0.1:58333
export ONEGIT_TEST_S3_ACCESS_KEY=onegit ONEGIT_TEST_S3_SECRET_KEY=onegit-test-secret
```

Without them those tests are skipped, so `go test ./...` works anywhere;
`ONEGIT_TEST_REQUIRE=1` (set by `make test-integration` and in CI) turns the
skip into a failure. Each test gets its own database, Redis key prefix and S3
bucket, removed afterwards, so packages run in parallel.

Run one package or test:

```sh
make test-deps
ONEGIT_TEST_POSTGRES=... go test -run TestMergeStyles -v ./internal/pulls/
```

`ONEGIT_TEST_LOG=1` prints the server logs of a test.

The server tests use the test binary itself as the hook executable (see
`TestMain` in `internal/server`), exactly like production, where the
repository's hooks run `onegit hook <name>`.
