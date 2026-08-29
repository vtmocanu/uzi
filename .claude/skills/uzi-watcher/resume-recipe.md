# Resume recipe — land a recovered run's work as a merged, green PR

A generic, reusable recipe for taking a run's work that never reached a mergeable PR
(timed out, push-rejected, or lost) and landing it yourself. It is **source-agnostic**
(a backup snapshot OR the live worker PVC) and **run-kind-agnostic** (issue OR task
runs). It picks up where *Recovering a failed run's work from the worker PVC* and
*Proactive backups* leave off — those get the work OUT; this lands it.

Placeholders below: `RUN` = run id, `STEM`/`BRANCH`/`REF` per the kind table, `DIR` = an
isolated worktree path (NEVER the `main` worktree).

## Run-kind coordinates (issue vs task)

| | issue run | task run (`uzi handoff`) |
|---|---|---|
| `STEM` (backup file prefix / working-clone dir) | `issue-<iid>` | `task-<RUN>` |
| `BRANCH` | `agent/issue-<iid>` | `uzi/task/<RUN>` |
| worker tracking `REF` | `refs/uzi-runner/agent/issue-<iid>` | `refs/uzi-runner/uzi/task/<RUN>` |

`backup-runs.sh` names every file by `STEM` (task-run support landed in #789). A task run
has no issue iid, so its work often sits **entirely uncommitted** in the working clone —
the `uncommitted.patch` + `untracked.tar.gz` are what save it, not the bundle.

## Pick a source

- **A) A backup snapshot** (`scripts/backup-runs.sh` / `backup-loop.sh`) — preferred when
  one exists: it needs no kube access and it captured uncommitted work too. The snapshot
  dir holds `STEM.tgz` (bundle + `uncommitted.patch` + `untracked.tar.gz` + `meta.txt`)
  plus `run.json` / `plan.md` / `progress.txt` / `log-tail.ndjson`.
- **B) The live PVC** (the worker pod still exists) — bundle `REF` out of the bare clone
  per *Recovering a failed run's work from the worker PVC* above, then continue at step 3.

## Steps

```sh
cd <the repo>                              # your normal checkout; work happens in DIR, not here
```

1. **Extract + VERIFY the snapshot** (source A). A truncated `.tgz` still lists its bundle
   member's NAME, so check the bytes end-to-end before trusting it:
   ```sh
   gzip -t SNAP/STEM.tgz && tar tzf SNAP/STEM.tgz >/dev/null   # both must pass
   W=$(mktemp -d); tar xzf SNAP/STEM.tgz -C "$W"
   ```
2. **Prove the bundle is restorable** from INSIDE the real repo (it has the prerequisite
   base commit the bundle excludes):
   ```sh
   git bundle verify "$W/STEM.bundle"        # "…is okay"; lists the ref + the required base
   ```
3. **Fetch into a recovery branch + an ISOLATED worktree** (never `main`):
   ```sh
   git fetch "$W/STEM.bundle" 'BRANCH:refs/heads/recover/STEM'   # source B: fetch the kubectl-cp'd bundle
   git worktree add DIR recover/STEM
   cd DIR
   ```
4. **Restore uncommitted state** the tracking ref never held (source A; skip if absent —
   a committed-only run has neither file):
   ```sh
   git apply "$W/STEM.uncommitted.patch"   2>/dev/null || true   # tracked edits
   tar xzf   "$W/STEM.untracked.tar.gz" -C . 2>/dev/null || true  # new, not-yet-added files
   ```
5. **Rebase onto current main** (adopts main's workflow files → clears any base-staleness):
   ```sh
   git fetch origin main && git rebase origin/main
   ```
   Common conflicts: a `specs/ai.md` section-number collision (keep both, renumber the
   incoming one); a new goose migration (rename to the next free number above the live head
   in `api/internal/store/migrations/`, sequenced after any sibling PR's migration); a
   hand-edited shared doc (keep both sides).
6. **Pre-flight (both must be empty)** — confirms no `.github/workflows` surprise (the
   worker PAT lacks `workflow` scope, but YOUR token has it, so a workflow edit is fine to
   push here; an empty result just means a plain rebase already landed it):
   ```sh
   git diff --name-only origin/main..HEAD -- .github/workflows/
   git log  --name-only origin/main..HEAD -- .github/workflows/
   git diff --name-only origin/main..HEAD -- api/internal/store/migrations/   # renumber if non-empty
   ```
7. **Gate the touched components** (only the ones the diff touches): `task gate:api`,
   `gate:web`, `gate:agent`, `gate:controller`, and `./e2e/run-store-it.sh` if a `*_livedb`
   test changed. Use the repo's pinned toolchains (`GOTOOLCHAIN`, node@24 for web — see the
   toolchain-skew memory).
8. **Push + open a maintainer PR** that says it is a recovery and why:
   ```sh
   git push -u origin recover/STEM
   gh pr create --repo OWNER/REPO --base main --title '…(recovered)' --body '…recovered from RUN…'
   ```
9. **Review + land** — wait for CodeRabbit, triage its findings (see *Reviewing the diff* /
   *Triaging CodeRabbit findings*), fix the real ones, then admin-merge and watch post-merge
   CI. If a sibling PR must land first (migration ordering), merge it, `gh pr update-branch`
   this one, re-wait for CI.
10. **Clean up** — `git worktree remove DIR`, `git branch -D recover/STEM`, and delete the
    backup snapshot once the PR is merged and CI is green.

## Notes

- **Verify restorability BEFORE you rely on a snapshot, not after** (steps 1-2). The #764
  handoff task hit a transient `kubectl exec` truncation that logged `OK` on a half-written
  `.tgz`; `gzip -t` caught it and a re-capture produced a clean, `git bundle verify`-able
  archive. `backup-runs.sh` now verifies each capture and retries (#790), but a snapshot
  from an older copy, or one hand-copied, still deserves the check.
- **A task run's HEAD can move under you** (the agent amends its top commit late in the
  run). Re-snapshot right before you rely on it, and confirm `meta.txt`'s `head=` matches
  the tip you fetched.
- **Everything runs in `DIR`, never the `main` worktree** — same rule the rest of this skill
  and the repo's worktree guidance state.
