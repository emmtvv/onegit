# Quickstart

## Run the stack

onegit needs PostgreSQL, Redis and S3-compatible storage. `docker-compose.yml`
runs all of them next to onegit:

```sh
curl -LO https://raw.githubusercontent.com/emmtvv/onegit/main/docker-compose.yml
curl -L -o .env https://raw.githubusercontent.com/emmtvv/onegit/main/.env.example
$EDITOR .env          # set every password and ONEGIT_BASE_URL
docker compose up -d
```

From a checkout, `make up` does the same but builds the image from source.

Open `ONEGIT_BASE_URL` (default <http://localhost:3000>) and sign in as `admin`
with `ONEGIT_ADMIN_PASSWORD`. onegit asks for a new password before anything
else; until then git access with the initial password is refused.

The initial admin is created only while the database has no users, so
changing `ONEGIT_ADMIN_PASSWORD` later has no effect. To regain access:

```sh
docker compose exec onegit onegit admin reset-password -username admin -password 'temporary-pass'
```

## Push your repository

```sh
git remote add onegit http://localhost:3000/monorepo.git
git push onegit --all
git push onegit --tags
```

The path is `/<ONEGIT_REPO_NAME>.git`. git asks for a username and password;
use your password or, better, an access token from **Settings → Access
tokens** (SSO users have no password, so they always use a token). With a
token the username is ignored.

The default branch is `ONEGIT_DEFAULT_BRANCH` (`main`); if your first push
doesn't contain it, the first pushed branch becomes the default instead.

## Invite the team

- **Admin → Users**: create users with a role. `read` browses, clones, pulls
  images and comments; `write` also pushes, merges, reviews and deploys
  where the deploy rules allow; `admin` also manages users, branch
  protection, runners and deploy rules. Passwords set by an admin are
  temporary.
- Or enable SSO: see [configuration.md](configuration.md#single-sign-on-oidc).

## Next steps

- Protect `main`: **Admin → Branches** ([pull-requests.md](pull-requests.md)).
- Add a pipeline and a runner ([ci.md](ci.md)).
- Set up deployment rules ([deployments.md](deployments.md)).
- Push images: `docker login localhost:3000` ([registry.md](registry.md)).
- Before production: HTTPS, backups and replicas ([operations.md](operations.md)).

## Build from source

```sh
make build        # bin/onegit, needs Go (see go.mod) and git ≥ 2.40 at runtime
make docker       # emmtvv/onegit:dev
```

Running the binary outside Docker needs the settings from
[configuration.md](configuration.md) as environment variables:

```sh
export ONEGIT_DATABASE_HOST=localhost ONEGIT_DATABASE_USER=onegit ONEGIT_DATABASE_PASSWORD=...
export ONEGIT_REDIS_URL=redis://localhost:6379/0
export ONEGIT_S3_ENDPOINT=http://localhost:8333 ONEGIT_S3_BUCKET=onegit ONEGIT_S3_ACCESS_KEY=... ONEGIT_S3_SECRET_KEY=...
export ONEGIT_REPO_DIR=./data ONEGIT_ADMIN_PASSWORD=change-me-now
bin/onegit serve
```
