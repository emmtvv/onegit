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
deploy dimension holds the project names, what is deployed to each target.
A project's page adds its README, its open pull requests, its recent CI runs
and links to its code, history and search. Pull requests and pipeline runs
can be filtered by project (`/pulls?project=api`, `/actions?project=api`): a
pull request or run belongs to every project it changes files in.
