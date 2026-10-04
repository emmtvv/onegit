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
- A merge starts the push pipelines of the base branch, as a push would.
- A plain push that makes the base contain the head marks the pull request
  merged ("manual").
- Conflicts are detected with `git merge-tree`; resolve them by merging or
  rebasing the base into the branch and pushing.

## Auto-merge

When a pull request cannot be merged yet, **Enable auto-merge** (with the
chosen style and message) merges it as you once every requirement is met:
approvals, code owners, required checks, no conflicts. It also deletes the
branch if asked. On a branch that requires the merge queue the button reads
**Queue when ready** and puts the pull request into the queue instead. Pushing
new commits keeps auto-merge on; closing the pull request, or the user losing
write access, turns it off.

## Merge queue

In a busy monorepo two pull requests can each pass their checks and still
break the branch together. With **Require a merge queue** in the branch's
protection rule, pull requests land only through the queue:

1. **Add to merge queue** (the pull request must be mergeable as usual).
2. The queue builds a candidate commit for each of the first *depth* entries
   with the merge style chosen when queueing: entry 1 on top of the branch,
   entry 2 on top of entry 1's candidate, and so on. Candidates live in
   `refs/merge-queue/<n>`.
3. The candidates run the pipelines with a `merge_queue` or `pull_request`
   trigger (`ONEGIT_EVENT=merge_queue`). A candidate passes when the rule's
   required checks succeed on it, or, without required checks, when every
   check on it does.
4. When a candidate passes, the branch fast-forwards to it: that pull request
   and all ahead of it land at once (a later candidate contains the earlier
   ones). Each is marked merged with its own commit, and the push pipelines
   of the branch start.
5. A candidate that fails, or conflicts, takes its pull request out of the
   queue with the reason in the timeline; the entries behind it are rebuilt
   without it. A push to a queued pull request, or closing it, also takes it
   out.

Approvals are checked again before landing. Direct merges are refused on
such branches. **Pull requests → Merge queue** shows the queues with their
candidates' checks and the recent results.

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
| Require a merge queue | Pull requests land only through the merge queue |
| Merge queue depth | How many queued pull requests are tested at once (default 5) |
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
