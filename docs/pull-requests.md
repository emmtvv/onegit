# Pull requests and branch protection

## Pull requests

Push a branch and git prints a link to open a pull request (or to the open
one). A pull request compares `merge-base..head`; its number is also a ref,
`refs/pull/<N>/head`, which keeps the commits reachable after the branch is
deleted.

- **Conversation**: description, comments, reviews, pushes and merges in one
  timeline. Anyone who can read the repository can comment.
- **Files**: the diff, with comments on any line. A comment stays on the diff
  while its file is unchanged; after the file changes it is marked outdated
  and shown (with its code snippet) in the conversation.
- **Reviews**: comment, approve or request changes. Per reviewer the latest
  approval or change request counts. An approval counts only from someone
  with write access who is not the author, and, with "dismiss stale
  approvals", only for the current head commit.
- **Merge**: squash (default; authored by the PR author, committed by the
  merger), merge commit, or rebase (fast-forward when possible, otherwise the
  commits are replayed one by one keeping their authors). Merges happen on
  the server without a worktree and refuse to run if the base moved in the
  meantime. The head branch can be deleted in the same step.
- A plain push that makes the base contain the head marks the pull request
  merged ("manual").
- Conflicts are detected with `git merge-tree`; resolve them by merging or
  rebasing the base into the branch and pushing.

## Branch protection

**Admin → Branches** holds rules matched by exact name or glob
(`release/*`; `*` does not cross `/`). An exact name wins, then the longest
pattern. Options:

| Option | Effect |
|---|---|
| Require a pull request | No direct pushes; changes arrive through merges |
| Required approvals | Number of counting approvals |
| Dismiss stale approvals | New commits reset approvals |
| Require code owner review | Every changed path with owners needs an approval from one of them |
| Block on requested changes | An open change request blocks the merge |
| Required status checks | Commit statuses (`<pipeline> / <job>`) that must succeed on the head commit |
| Code owners (server-side) | CODEOWNERS rules kept in the database and always enforced, whatever the repository says |
| Allow force-push / deletion | Off by default |

Creating a protected branch is allowed; administrators get no bypass. The
default branch can never be deleted, and only `refs/heads/*` and
`refs/tags/*` can be pushed.

## CODEOWNERS

onegit reads the first of `.github/CODEOWNERS`, `.gitea/CODEOWNERS`,
`CODEOWNERS` and `docs/CODEOWNERS` from the **base** branch, so a pull request
cannot change its own reviewers. The syntax is GitHub's: gitignore-like
patterns, the last matching line wins, owners are `@user`, `@team` or
`@org/team` (teams from Admin → Teams) or an email address. A pattern whose
owners are all unknown doesn't block.

```
*                 @lead
/projects/api/    @api-team
*.sql             @org/dba dba@example.com
/docs/            # no owners
```

To protect paths that the repository must not control, for example the
deploy recipes, put the rules into the branch protection's server-side code
owners:

```
/.onegit/         @sre
```
