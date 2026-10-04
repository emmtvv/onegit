# Operations

## Architecture

onegit keeps no state of its own apart from the git repository:

| What | Where |
|---|---|
| Git objects and refs | Bare repository in `ONEGIT_REPO_DIR` |
| Users, tokens, pull requests, CI runs and logs, deploy rules and secrets, registry metadata | PostgreSQL |
| Sessions, caches, login rate limits | Redis |
| Registry layers | S3-compatible storage |

git is served over smart HTTP only (no SSH), on the same port as the UI, the
registry and the runner API. onegit runs the `git` binary for every
repository operation, so the behaviour is exactly git's.

Pushes go through git's `pre-receive` and `post-receive` hooks, which onegit
installs into the repository at startup. The hooks call back into the
replica that received the push: `pre-receive` enforces the push policy and
branch protection (seeing the pushed objects in git's quarantine directory),
`post-receive` updates pull requests and starts pipelines.

## Several replicas

Every replica must mount the same repository volume (ReadWriteMany: NFS,
CephFS, EFS, ...); git's lock files keep concurrent pushes safe. Everything
else is shared through Postgres and Redis: merges and pushes to the same pull
request are serialised with row locks, CI jobs are claimed with
`SELECT ... FOR UPDATE SKIP LOCKED`, and the registry keeps upload state in
the database so chunks of one upload may hit different replicas. Sessions
survive restarts and failovers.

## Repository maintenance

Once an hour one replica (an advisory lock picks it) packs refs, merges
packs geometrically with a multi-pack bitmap and extends the commit-graph
with changed-path Bloom filters; once a week it also runs `git gc`, pruning
unreachable objects older than two weeks. The Bloom filters are what keep
path history, blame and the tree's last-commit column fast on deep
histories. git's automatic gc is turned off so pushes never wait for it.
The first run after an upgrade rewrites the commit-graph and can take a
while on a large repository.

## Health checks

- `GET /healthz`: the process is up (liveness). `onegit healthcheck` probes
  it; the Docker image uses it as `HEALTHCHECK`.
- `GET /readyz`: Postgres, Redis, S3 and the repository are reachable
  (readiness).

## Reverse proxy and HTTPS

Run onegit behind a TLS-terminating proxy for anything but local use: docker
refuses plain-HTTP registries on other hosts, and session cookies are marked
`Secure` when `ONEGIT_BASE_URL` starts with `https://`. Set
`ONEGIT_TRUST_PROXY=true` so rate limits see client addresses. Allow large
request bodies (image layers, big pushes) and long timeouts, and don't
buffer responses (git progress is streamed). With nginx:

```nginx
client_max_body_size 0;
proxy_request_buffering off;
proxy_buffering off;
proxy_read_timeout 3600s;
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header Host $host;
```

## Backups

Back up all four stores, ideally at the same moment:

- the repository volume (`git bundle create` or a file-system snapshot);
- PostgreSQL (`pg_dump`);
- the S3 bucket (registry layers);
- Redis only holds sessions and caches; losing it signs everyone out.

Keep `ONEGIT_SECRET_KEY` outside the database backups: with both, anyone can
decrypt the deploy secrets.

## Admin CLI

```sh
onegit admin list-users
onegit admin reset-password -username NAME -password TEMP   # temporary, must be changed at next login
onegit admin registry-gc [-grace 24h]
onegit version
```

In the compose stack: `docker compose exec onegit onegit admin list-users`.

## Upgrades

Pull the new image and restart. Database migrations run at startup, one
replica at a time (an advisory lock serialises them). Read the
[changelog](../CHANGELOG.md) before upgrading across minor versions while
onegit is below 1.0.

### PostgreSQL major versions

A new PostgreSQL major version can't read the previous one's data files, so
a compose update that bumps the `postgres` image needs a dump and restore.
From 18 on the data lives in a per-version subdirectory of the `pgdata`
volume. Moving from 17 (the `postgres` volume) to 18:

```sh
# 1. Still on the old compose file: stop onegit and dump the database.
docker compose stop onegit
docker compose exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -Fc "$POSTGRES_DB"' > onegit.dump
docker compose down

# 2. Update the compose file, start the new PostgreSQL and restore.
docker compose up -d --wait postgres
docker compose exec -T postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --exit-on-error' < onegit.dump
docker compose up -d
```

The old volume is left untouched; remove it (`docker volume rm
<project>_postgres`) once onegit works on the new one. Don't start onegit
before the restore: it would initialise an empty database.
