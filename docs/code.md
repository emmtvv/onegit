# Code, search and projects

## Browsing

The **Code** tab shows the tree and files of any branch, tag or commit with
syntax highlighting, rendered Markdown and the code owners of the directory
or file being viewed. **Blame** (on every file) shows, for each run of lines,
the commit that last changed them, colour-coded by age; the history icon
next to a commit opens the blame from just before it changed those lines.

## Search

The search box in the top bar searches the files of the default branch
(another branch, tag or commit can be chosen on the results page):

- **Code**: lines containing the text, case-insensitive unless *Match case*
  is set; *Regular expression* switches to extended regular expressions.
- **Files**: paths containing every word of the query.

Narrow a search with path filters in the query: `path:services/api` (a
directory or file), `path:**/*.go` (a glob, `**` crossing directories) and
`-path:vendor` to exclude. For example
`NewServer path:services -path:**/*_test.go`.

Search reads the repository directly (`git grep` over the commit), so it is
always exact and needs no index. It stops after 1000 matching lines or
15 seconds; narrow it with `path:` on very large repositories.

## Projects

A project is a directory of the monorepo, for example every directory in
`services/`. Set it up in **Admin → Projects** with a pattern that has one
`*` for the project name: `services/*`, or `projects/*/project.yaml` to count
only directories with that file. Without settings, the first deploy dimension
that comes from directories ([deployments.md](deployments.md)) defines the
projects.

The **Projects** tab lists them with their code owners, the last CI run on
the default branch that touched them, their open pull requests and, when a
deploy dimension holds the project names, what is deployed in each
environment, one column per environment. Each deployment shows how far it is
**behind**: the commits under the project's directory on the default branch
that it does not have. It turns red from 10 commits, or when the oldest
missing commit is a week old. **Mine** keeps the projects you own in
CODEOWNERS (directly or through a team); **Needs attention** keeps those whose
CI fails on the default branch or that are far behind.
A project's page adds its README, its open pull requests, its recent CI runs
and links to its code, history and search. Pull requests and pipeline runs
can be filtered by project (`/pulls?project=api`, `/actions?project=api`): a
pull request or run belongs to every project it changes files in.

## Home

Signed-in users get a **Home** tab, and signing in from the front page lands
there. It gathers what waits on you:

- **Waiting for your review**: open pull requests whose changed files you own
  in CODEOWNERS (or the branch protection's owners) while no owner has
  approved the current head, and pull requests with new commits since you
  requested changes (or since your approval, when the branch dismisses stale
  approvals). Owners are worked out in the background when a PR's files or
  the owner rules change, so a new PR appears here a few seconds after it
  opens.
- **Your pull requests** with their next step: failed check, conflicts,
  changes requested, waiting for approvals or code owners, checks running, in
  the merge queue, or ready to merge.
- **Approve a deployment**: pending deployments you may approve and have not
  reviewed yet.
- Your recent pipeline runs, and the projects you own with their CI status
  and how far each environment is behind.
