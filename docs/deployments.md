# Deployments

A deployment is a request to run a commit's deploy recipe against one or
more **targets**. Who may deploy what where is decided on the server, from
rules stored in the database and edited by administrators, so no commit can
grant itself production access.

## Targets and dimensions

A target has one value per **dimension**. Dimensions are defined in
**Admin → Deploy rules**:

- **list**: fixed values, e.g. `environment` = `development, staging, production`;
- **paths**: discovered on the default branch, e.g. `project` from
  `projects/*/project.yaml` gives one value per matching directory.

A target is written `project=api, environment=production`. Rules, secrets and
deploy runners select targets with **selectors**: `environment=prod*;
project=api,web` (globs; a dimension that is left out matches everything).

## Recipes

Recipes say *how* to deploy. They are read only from the **default branch**
and pinned when the deployment is requested, so a feature branch cannot
change them. Each target must be served by exactly one recipe:

```yaml
# .onegit/deploy/services.yml
targets:
  environment: [staging, production]
runs-on: [deploy]
artifact:
  image: "app/${project}"          # expanded with the target's dimensions
  tag: "${sha}-${environment}"     # default: ${sha}
  optional: false
tooling: true                      # also check out the recipe commit into $ONEGIT_TOOLING_DIR
timeout: 20m
env:
  STACK: app
steps:
  - run: ./deploy/swarm.sh "$PROJECT" "$ENVIRONMENT" "$ONEGIT_ARTIFACT"
```

Deploy jobs get the pipeline variables ([ci.md](ci.md#job-environment)) plus:

| Variable | |
|---|---|
| `PROJECT`, `ENVIRONMENT`, ... | Each dimension, upper-cased |
| `ONEGIT_TARGET_<DIMENSION>` | The same, prefixed |
| `ONEGIT_ARTIFACT` | `registry/image@sha256:...`, the image resolved and pinned when the deployment was approved |
| `ONEGIT_DEPLOYMENT_ID`, `ONEGIT_RECIPE`, `ONEGIT_RECIPE_SHA` | |
| `ONEGIT_TOOLING_DIR` | With `tooling: true`: the recipe commit's checkout |
| secrets | See below |

Deploy jobs of the same target never run concurrently; different targets run
in parallel on whichever deploy runners accept them.

## Rules

**Grants** say who may request a deployment. A request needs a grant for
every target; any matching grant is enough. Principals: `user:alice`,
`team:sre`, `role:write`, and `codeowners` (the CODEOWNERS owners of the
target's paths, for "paths" dimensions).

**Requirements** add conditions; every matching requirement applies:

| Requirement | |
|---|---|
| Branches | The commit must be on one of these branches (`main`, `release/*`) |
| Checks | Every commit status of the commit must pass (and there must be one) |
| Built by CI | The recipe's artifact must exist and have been pushed by a CI job from this commit |
| Approvals | N approvals from principals (empty: anyone with write access). Never the requester; not the commit author unless allowed |
| Freeze | Refuse all deployments, with a message |
| Does not apply to commits on | Skip this requirement for commits already on these branches, e.g. "unreviewed code needs one approval, main deploys straight away" |

A request that fails a hard check is refused and shows exactly which checks
failed. Otherwise it waits for approvals or starts at once. Everything is
checked again right before the jobs are queued. Approvers see pending
deployments on **Deployments**; a rejection ends the request.

Example setup:

```
grant    environment=staging      principals: role:write
grant    environment=production   principals: team:sre, codeowners
require  environment=production   branches: main; checks; built by CI; 1 approval from team:sre
require  environment=staging      1 approval; does not apply to commits on: main
```

## Secrets

**Admin → Secrets**: name, value and a target selector. Values are encrypted
with AES-256-GCM (key from `ONEGIT_SECRET_KEY`), never shown again, given
only to deploy jobs whose target matches, and masked (`***`) in job logs.
When several secrets share a name, the one whose selector names the most
dimensions wins.

If an orchestrator pulls images after the job ends (Docker Swarm
`--with-registry-auth`, Kubernetes), give it a long-lived read-only access
token of a bot user as a secret: job tokens expire with the job.

## History and audit

**Deployments** shows the latest deployment per target and each target's
history; redeploying an older deployment is a rollback. Requests, approvals,
rule and secret changes are recorded in **Admin → Audit log**.
