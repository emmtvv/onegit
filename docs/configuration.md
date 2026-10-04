# Configuration

Every setting is an `ONEGIT_*` environment variable. A YAML file passed with
`-config` (or `ONEGIT_CONFIG`) can hold the same settings, nested as in
[internal/config/config.go](../internal/config/config.go); environment
variables win over the file.

## Server

| Variable | Default | |
|---|---|---|
| `ONEGIT_BASE_URL` | `http://localhost:3000` | Public URL: links, clone URLs, OIDC redirect, registry host and token realm |
| `ONEGIT_HTTP_ADDR` | `:3000` | Listen address |
| `ONEGIT_TRUST_PROXY` | `false` | Take the client IP from `X-Real-IP` / the last `X-Forwarded-For` hop. Enable only behind a reverse proxy |
| `ONEGIT_REPO_DIR` | `/data/repo` | Directory of the bare repository (the only on-disk state) |
| `ONEGIT_REPO_NAME` | `monorepo` | Clone path `/<name>.git` |
| `ONEGIT_DEFAULT_BRANCH` | `main` | Default branch of a new repository |
| `ONEGIT_PUBLIC_READ` | `false` | Anonymous browsing, cloning and image pulls |
| `ONEGIT_LOG_FORMAT` | text | `json` for structured logs |

## Backing services

| Variable | Default | |
|---|---|---|
| `ONEGIT_DATABASE_HOST`, `ONEGIT_DATABASE_USER` | required | PostgreSQL (tested with 17) |
| `ONEGIT_DATABASE_PORT` | `5432` | |
| `ONEGIT_DATABASE_PASSWORD` | | |
| `ONEGIT_DATABASE_NAME` | `onegit` | |
| `ONEGIT_DATABASE_SSL_MODE` | `prefer` | `disable`, `require`, `verify-ca`, `verify-full` |
| `ONEGIT_DATABASE_MAX_CONNS` | `10` | Pool size per replica |
| `ONEGIT_REDIS_URL` | required | `redis://[:password@]host:6379/0` (`rediss://` for TLS) |
| `ONEGIT_REDIS_KEY_PREFIX` | `onegit:` | Namespace for all keys; lets several installations share a Redis database |
| `ONEGIT_S3_ENDPOINT` | required | `https://s3.amazonaws.com`, `https://<account>.r2.cloudflarestorage.com`, `http://minio:9000`, ... A host with an underscore (a Swarm service such as `infra_minio`) works too |
| `ONEGIT_S3_BUCKET` | required | |
| `ONEGIT_S3_ACCESS_KEY`, `ONEGIT_S3_SECRET_KEY` | | |
| `ONEGIT_S3_REGION` | `us-east-1` | |
| `ONEGIT_S3_USE_SSL` | `false` | Only for `host:port` endpoints; a URL's scheme decides otherwise |
| `ONEGIT_S3_CREATE_BUCKET` | `true` | Create the bucket at startup if it is missing |

Migrations run automatically at startup (under a Postgres advisory lock, so
replicas can start together).

## Accounts

| Variable | Default | |
|---|---|---|
| `ONEGIT_ADMIN_USERNAME` | `admin` | Initial admin, created while there are no users |
| `ONEGIT_ADMIN_EMAIL` | | |
| `ONEGIT_ADMIN_PASSWORD` | required on first start | Temporary: must be changed at first login |
| `ONEGIT_PASSWORD_LOGIN` | `true` | Local username/password sign-in in the UI |

Passwords need at least 10 characters. Failed logins are limited to 10 per
15 minutes per username and client IP.

### Single sign-on (OIDC)

| Variable | Default | |
|---|---|---|
| `ONEGIT_OIDC_ENABLED` | `false` | |
| `ONEGIT_OIDC_DISPLAY_NAME` | `SSO` | Button label on the sign-in page |
| `ONEGIT_OIDC_ISSUER` | | e.g. `https://keycloak.example.com/realms/main` |
| `ONEGIT_OIDC_CLIENT_ID`, `ONEGIT_OIDC_CLIENT_SECRET` | | |
| `ONEGIT_OIDC_SCOPES` | `openid,profile,email,groups` | |
| `ONEGIT_OIDC_AUTO_REGISTER` | `true` | Create a local user on first sign-in; otherwise an admin links accounts |
| `ONEGIT_OIDC_GROUPS_CLAIM` | `groups` | Read from the ID token, or from userinfo when absent |
| `ONEGIT_OIDC_ADMIN_GROUP` | | Members get `admin` |
| `ONEGIT_OIDC_WRITE_GROUP` | | Members get `write` |
| `ONEGIT_OIDC_DEFAULT_ROLE` | `read` | Role of everyone else when a group mapping is set |

Register `<ONEGIT_BASE_URL>/login/oidc/callback` as the redirect URI. The
code flow uses PKCE and a nonce. Users are matched by the `sub` claim. When
`ONEGIT_OIDC_ADMIN_GROUP` or `ONEGIT_OIDC_WRITE_GROUP` is set, roles are
re-synced on every sign-in. Teams with an "OIDC group" (Admin → Teams) get
their membership from the groups claim the same way. SSO users use access
tokens for git and the registry.

## CI, deployments and registry

| Variable | Default | |
|---|---|---|
| `ONEGIT_SECRET_KEY` | (generated, in the database) | Encrypts deploy secrets. Set it in production and keep it out of database backups; changing it makes stored secrets unreadable |
| `ONEGIT_REGISTRY_ENABLED` | `true` | Container registry at `/v2/` |
| `ONEGIT_REGISTRY_MAX_BLOB_SIZE` | unlimited | Per layer: `500MiB`, `2GB`, or bytes |
| `ONEGIT_REGISTRY_MAX_TOTAL_SIZE` | unlimited | Whole registry |
| `ONEGIT_CI_MAX_ARTIFACT_SIZE` | `1GiB` | Per job's artifact archive; empty = unlimited |
| `ONEGIT_CI_ARTIFACT_RETENTION_DAYS` | `30` | How long artifacts are kept when a job sets no `expire-in` |
| `ONEGIT_CI_MAX_CACHE_SIZE` | `20GiB` | Total size of CI caches; the least recently used are evicted |
| `ONEGIT_CI_CACHE_RETENTION_DAYS` | `7` | Caches unused this long are removed |

Runners are configured separately (see [ci.md](ci.md#runners)):

| Variable / flag | |
|---|---|
| `ONEGIT_RUNNER_URL` / `-url` | onegit base URL |
| `ONEGIT_RUNNER_TOKEN` / `-token` | Runner token from Admin → Runners |
| `ONEGIT_RUNNER_WORKDIR` / `-workdir` | Work directory (default `./onegit-runner`) |
| `-capacity` | Jobs run at the same time (default 1) |
| `-keep-workspaces` | Keep job directories after they finish, for debugging |
