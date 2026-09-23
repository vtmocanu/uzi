# Merge mechanics: the ruleset, the admin merge, a red main, post-merge CI

Moved from the `uzi-watcher` skill (2026-09-17); this is the canonical home. Paths like
`scripts/...` are relative to `.agents/skills/uzi-lander/`.

## Merging past branch protection

`main` is guarded by a **ruleset** (`protect-main`, not classic protection, so a
`gh api …/branches/main/protection` call 404s while rules are still enforced; read the live
rules with `gh api repos/OWNER/REPO/rules/branches/main`): 1 approving non-author review
(`require_last_push_approval`, `dismiss_stale_reviews_on_push`) + up-to-date branch
(`strict`) + required status checks + no force-push/deletion. The admin role bypasses the
whole ruleset (`bypass_mode: always`). If a push or merge is refused, read the refusal and
stop rather than forcing.

- **Convention: squash** for every PR (the commit carries the PR title and `(#PR)`, which is
  what the release CHANGELOG cites). Add `--delete-branch`. `merge.sh --method merge` exists
  for the rare case that wants a merge commit.
- **The PR author is the bot account** (e.g. `vtmocanu-uzi`), distinct from your `gh`
  identity, so a human review from you satisfies the review rule — it is not a self-review.
- **Renovate PRs are the exception: Renovate authors as the repo owner** (verified
  2026-08-20). GitHub forbids self-approval, and a granted approval is dismissed by the next
  update-branch push, so the approval rule can never be met: `--admin` is the only path.
  Reach for it directly rather than update-branching first.
- **Lockfile stacking.** Back-to-back merges of PRs sharing a generated lockfile
  (`api/go.mod`+`api/go.sum`, `agent/package-lock.json`) conflict once one lands, though each
  was clean alone; GitHub refuses the next with `Pull Request has merge conflicts`. For your
  own branch, `git fetch origin main` first (a stale `origin/main` silently omits a sibling's
  bump), `git merge origin/main`, take the union of the bumps, regenerate (`go mod tidy`, or
  `npm install --package-lock-only` in the package dir), `go build ./...`, push, merge. CI
  runs `npm ci`, so a lockfile regenerated under a newer local npm is fine while consistent
  with `package.json`. A Renovate branch you never push to: Renovate rebases it itself
  (references/renovate.md). Right after a resolution push, a stale `merge conflicts` is
  GitHub's async mergeability lag: re-check `mergeable` after a few seconds and retry.
- **`gh pr merge` is intermittently blocked by the harness auto-mode classifier.** It is
  not deterministic; a retry often succeeds. When the user has authorized admin merges,
  merge with `--admin` (it clears the review, up-to-date, and status-check gates at once).
  `scripts/merge.sh OWNER/REPO PR --expect-head SHA` does exactly this, re-checking the head
  and the rework lane at the last moment and confirming the PR actually reads `MERGED`:

  ```
  gh pr merge PR --repo OWNER/REPO --squash --delete-branch --admin
  ```

  **Never route around a classifier denial by other means** — retry, or hand the exact
  command to the user to run via a `!`-prefixed shell line, or ask them to add a
  `gh pr merge` allow rule.
- **`BEHIND` after an earlier merge** is expected (main moved). `--admin` bypasses the
  strict check; otherwise `gh pr update-branch` the PR and re-wait for CI. A BEHIND PR that
  also carries a migration-number collision or a conflict needs `scripts/land-prep.sh`
  (rebase + renumber + gates + lease push) first.
- Merging is **outward-facing**: unless the user pre-authorized it (they chose Auto mode,
  or said "merge as admin"), surface the MR + your review and get their OK first.
- **Merge each PR the moment it is review-clean and CI-green — do NOT hold the whole
  batch to the end.** A landed PR exercises `main` CI while you work the rest, so an
  integration break surfaces early instead of all at once at the finish. Keep the phase
  order (our PRs before the routine renovate batch — references/batch.md).
- **`--admin` does NOT bypass a real git conflict.** It clears the ruleset gates (review,
  up-to-date, status checks), but a `gh pr merge` returning `Pull Request has merge
  conflicts` or `the merge commit cannot be cleanly created` is a git-level conflict —
  resolve it locally (`scripts/land-prep.sh`, or `git merge origin/main` in the branch's
  own worktree, fix, push), then merge.
- **Parallel PRs collide on hand-edited shared files (ARCHITECTURE.md, a shared handler),
  and each merge re-conflicts the next.** A two-PR edit of DIFFERENT regions three-way-merges
  clean; only overlapping hunks conflict. `specs/ai.md` is frozen (issue #1317) and no
  longer a conflict site.
- **After merging PRs that edited `docs/`, watch for the embedded-docs drift guard.**
  `TestEmbeddedDocsMatchSource` requires `api/internal/uzidocs/embed/*.md` to mirror
  `docs/*.md` byte-for-byte (PRD #567). A PR that changed `docs/` but branched before the
  mirror existed lands without regenerating it, so `main` goes red post-merge even though
  every PR was green. Fix on `main`: `task docs:sync` + commit (docs-only, direct to main
  is the norm here).

## A red `main` blocks every open PR

GitHub tests each PR as branch **merged with base**, so a broken `main` fails `validate-*`
on every PR at once. A `[skip ci]` doc/PRD commit is the classic cause — it never ran CI,
so a `check-docs` break (e.g. a backticked `adr/…` or `prds/…` path that does not exist
yet) sits on `main` unseen. Diagnose from a PR's failing job log (`gh run view --log-failed`,
or `gh api --allow-escape-sequences …/jobs/JOB_ID/logs`), confirm the fault is on `main` (not the PR's own diff),
then fix it. A **docs-only** fix direct to `main` is the norm here (releases land that way);
the `check-docs` opt-out for a forward-referenced artifact is a `check-docs:ignore-path`
HTML-comment marker on the line. **Never push non-doc code to `main`.**

## Post-merge CI, and fixing failures

Poll every run for the merge SHA with **`scripts/watch-run-ci.sh --sha "$MERGE_SHA"`**,
launched with `run_in_background`. `MERGE_SHA` is the exact full SHA emitted by `merge.sh`;
never type or hand-complete a prefix. The watcher resolves it through GitHub before polling
and exits 3 when it is invalid or unknown. It exits on the first confirmed failing job rather
than waiting for the whole run (the same reaping reason as `watch-run.sh`; do not re-author a
heredoc per merge, which is how a path typo crept in on 2026-08-23):

```
<this skill's directory>/scripts/watch-run-ci.sh --sha "$MERGE_SHA" --interval 60
```

It exits **0** when every run that exists for the SHA is green, **1** on a real red
(`failure`/`timed_out`/`startup_failure`/`action_required`), **2** on timeout, **3** when
no run ever appeared (or gh failed), and **4** when the runs were only `cancelled`
(supersession — see below).

**Exit 0 means "every run that EXISTS for the SHA is green" — NOT "the full expected
workflow set ran."** Measured 2026-09-02 (a docs-only fix commit to `main`, `f015f1f`): only
the `CodeQL` run existed for the SHA, and `ci.yml`/`kind-smoke.yml` never dispatched, so
the watcher saw one green run and exited 0 — a *partial* dispatch read as a full green. A
`[skip ci]` commit landing on top can also leave the current HEAD with no full CI run at all.
So a green on a **prds/docs-only or `[skip ci]`-adjacent** push does **not** prove
`validate-web`/`validate-api` ran. When you pushed a fix whose whole point is a gate
(e.g. a `check-docs` fix), confirm it another way: run the gate locally (`task check-docs:web`
etc.), OR wait for the next real code-change dispatch (the fix rides into a following PR's
merged-with-base CI) to be the authoritative green. The reliable authority for merge-readiness
stays the PR's OWN checks (`watch-pr.sh`), which run `pull_request`-triggered full CI; a bare
green `main` badge can be a subset. *(A future improvement: derive the EXPECTED workflow set
from the last known-good `main` commit's runs and fail closed until each expected workflow
has a completed run for the target SHA.)*

On red, read each failed job and classify: **code / conflict / missing-file** → fix on a
branch, PR, merge, re-watch (never push code to `main`); **flaky** (passes on isolated
re-run; `gh run rerun <run-id> --failed`) → file an issue, do not chase; **infra / can't-fix**
→ report and stop. Green = done. This is the local session fixing CI, NOT uzi's `ci_autofix`
(which only touches pre-merge `agent/*` branches). The watcher prints two commands per
failed job. Its `gh api --allow-escape-sequences repos/O/R/actions/jobs/JOB/logs` command
returns the completed job's full log immediately, even while sibling jobs keep the workflow
run in progress; `gh run view RUN --job JOB --log-failed` may refuse until that whole run is
terminal. When only the failed step name is needed, use
`gh api repos/O/R/actions/jobs/JOB --jq '.steps[]|select(.conclusion=="failure")|.name'`.

**`conclusion == cancelled` is almost never a failure — it is concurrency
supersession.** The CI workflows run with `concurrency: cancel-in-progress` on the `main`
branch, so when a NEWER commit lands (another session's merge, or your own next merge) the
in-progress run of the older SHA is cancelled mid-flight. This is common on this repo's
shared, fast-moving `main` (measured 2026-08-20: `759199c8`'s CI was cancelled when a
renovate merge landed on top seconds later). **Do not read `cancelled` as red.** On exit 4,
your merge is fine — re-point at the CURRENT `origin/main` HEAD (`git fetch origin main`)
and confirm *that* commit's run goes green, since it exercises your change plus whatever
superseded it. If a peer session owns that newer commit (coordinate via SendMessage), its
green is theirs to watch and report.
