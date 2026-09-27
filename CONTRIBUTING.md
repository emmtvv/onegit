# Contributing

Thanks for taking the time to contribute! Bug reports, fixes and focused
features are welcome.

## Development setup

You need Go (the version in [go.mod](go.mod); the toolchain downloads it if
yours is older), git 2.40 or newer, and Docker for the backing services.

```sh
make build            # bin/onegit
make test             # unit tests; tests needing Postgres/Redis/S3 are skipped
make test-integration # starts docker-compose.test.yml and runs everything with -race
make up               # the full stack from docker-compose.yml on http://localhost:3000
```

See [docs/testing.md](docs/testing.md) for how the tests are organised and how
to run a single package against the test services.

## Before opening a pull request

```sh
make check   # go vet, golangci-lint and the full test suite
```

CI runs the same checks, plus `gofmt`, a Docker image build and
`govulncheck`, on every pull request.

- Add or update tests for the behaviour you change. Bugs get a regression
  test that fails without the fix.
- Database changes go into a new `internal/store/migrations/NNN_*.sql` file;
  never edit a migration that has shipped.
- Keep user-facing changes in [CHANGELOG.md](CHANGELOG.md) under
  "Unreleased".

## Scope

onegit hosts **one** repository and aims to stay small: code browsing, pull
requests, CI, deployments and a container registry. Multiple repositories,
issue trackers, wikis, forks and organisations are out of scope. If you want
to propose a larger feature, especially one that adds a dependency, a
configuration option or a new kind of stored data, open an issue first so the
design can be discussed before you invest time in it.

## Code style

Standard `gofmt` and `go vet`. Comments explain *why*, not *what*; match the
style of the package you are editing. The UI is server-rendered
`html/template` with embedded assets; please don't add a JavaScript build
step.

## Reporting security issues

Do not open a public issue for a vulnerability; see [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE).
