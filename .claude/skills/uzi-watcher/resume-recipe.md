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

1. **Point `BUNDLE` at the bundle and VERIFY the bytes — for BOTH sources.** A truncated
   bundle still lists its ref by name, so check before trusting it:
   ```sh
   # Source A (snapshot): the .tgz is gzip — test it end-to-end, then extract.
   gzip -t SNAP/STEM.tgz && tar tzf SNAP/STEM.tgz >/dev/null       # both must pass
   W=$(mktemp -d); tar xzf SNAP/STEM.tgz -C "$W"
   BUNDLE="$W/STEM.bundle"; FETCH_REF=BRANCH

   # Source B (live PVC): BUNDLE is the raw bundle you kubectl-cp'd out (created from REF,
   # per "Recovering a failed run's work from the worker PVC"); no gzip layer to test. A
   # tracking-ref bundle carries COMMITTED work ONLY, so W stays empty and step 5 restores
   # nothing — correct for a committed run. For a task run's UNCOMMITTED work, prefer a
   # snapshot (source A); or first capture the pod's `git diff HEAD` + untracked files into
   # a W of your own (the exact commands are backup-runs.sh's CAPTURE block).
   #   BUNDLE=/path/to/r.bundle; FETCH_REF=REF; W=""
   ```
2. **Prove the bundle is restorable** from INSIDE the real repo (it has the prerequisite
   base commit the bundle excludes) — this runs for either source:
   ```sh
   git bundle verify "$BUNDLE"                # "…is okay"; names the ref + the required base
   ```
3. **Fetch into a recovery branch + an ISOLATED worktree** (never `main`):
   ```sh
   git fetch "$BUNDLE" "$FETCH_REF:refs/heads/recover/STEM"
   git worktree add DIR recover/STEM
   cd DIR
   ```
4. **Rebase onto current main FIRST, while the tree is still clean.** Only the bundle's
   COMMITTED work is present so far, and `git rebase` refuses a dirty tree — so the rebase
   MUST precede any uncommitted restore (step 5). Adopts main's workflow files → clears any
   base-staleness:
   ```sh
   git fetch origin main && git rebase origin/main
   ```
   Common conflicts: a `specs/ai.md` section-number collision (keep both, renumber the
   incoming one); a new goose migration (rename to the next free number above the live head
   in `api/internal/store/migrations/`, sequenced after any sibling PR's migration); a
   hand-edited shared doc (keep both sides).
5. **Restore the uncommitted state** the tracking ref never held — the whole point for a
   task run. Skip cleanly when the run had none, but do NOT mask a real apply failure (a
   `|| true` would turn a corrupt archive or a patch conflict into silent data loss):
   ```sh
   [ -f "$W/STEM.uncommitted.patch" ] && git apply --3way "$W/STEM.uncommitted.patch"  # tracked edits
   [ -f "$W/STEM.untracked.tar.gz" ]  && tar xzf "$W/STEM.untracked.tar.gz" -C .        # new files
   git status --short && git diff --stat    # eyeball what was restored before committing it
   ```
6. **Commit the restored work.** `git push` sends only commits, and the step-7 pre-flight
   diffs `origin/main..HEAD` — so anything left in the working tree is silently dropped:
   ```sh
   git add -A && git commit -m "chore: recover work from run RUN"   # skip if step 5 restored nothing
   ```
7. **Pre-flight (both `.github/workflows` checks must be empty, and gitleaks over the RANGE clean)** — confirms no workflow
   surprise (YOUR token has `workflow` scope, so an intended workflow edit is fine to push;
   an empty result just means a plain rebase already landed main's copy):
   ```sh
   git diff --name-only origin/main..HEAD -- .github/workflows/
   git log  --name-only origin/main..HEAD -- .github/workflows/
   git diff --name-only origin/main..HEAD -- api/internal/store/migrations/   # renumber if non-empty
   go run github.com/zricethezav/gitleaks/v8@v8.30.1 git --log-opts="origin/main..HEAD" --no-banner --redact   # EVERY commit in the range (push protection scans them all); must print "no leaks found". Same pinned route as scripts/scan-secrets.sh; --redact is a no-op without -v, harmless
   ```
   After a push-protection rejection specifically, also run `git log -S 'THE_LITERAL' origin/main..HEAD` (the literal
   GitHub named) — it must print nothing. GitHub's pattern set is not gitleaks',
   so a clean range scan alone does not prove the push will be accepted; see SKILL.md's
   *Push protection* subsection for the fold-into-the-introducing-commit step that precedes it.
8. **Gate the touched components** (only the ones the diff touches): `task gate:api`,
   `gate:web`, `gate:agent`, `gate:controller`, and `./e2e/run-store-it.sh` if a `*_livedb`
   test changed. Use the repo's pinned toolchains (`GOTOOLCHAIN`, node@24 for web — see the
   toolchain-skew memory).
9. **Push + open a maintainer PR** that says it is a recovery and why:
   ```sh
   git push -u origin recover/STEM
   gh pr create --base main --title '…(recovered)' --body '…recovered from RUN…'   # --repo defaults to origin
   ```
10. **Review, land, clean up** — wait for CodeRabbit, triage its findings (see *Reviewing
    the diff* / *Triaging CodeRabbit findings*), fix the real ones, admin-merge, watch
    post-merge CI (if a sibling PR must land first for migration ordering, merge it, then
    `gh pr update-branch` this one and re-wait). Then, **back in your normal checkout** —
    `cd` out of `DIR` first, since you cannot remove the worktree you are standing in nor
    delete its checked-out branch — `git worktree remove DIR`, `git branch -D recover/STEM`,
    and delete the snapshot once the PR is merged and CI is green.

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
