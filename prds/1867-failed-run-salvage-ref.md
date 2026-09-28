# PRD #1867: Keep a failed run's published checkpoint under a run-scoped salvage ref

**Issue**: #1867
**Status**: Draft
**Priority**: High
**Related**: #1856 (the motivating run), #1864, ADR-122 (checkpoint push broker), ADR-1597 (mid-turn checkpoint durability), PRD #1030 M4 (terminal checkpoint cleanup), #1296 (recovery captures), #1418 (`landing_state`)

## Problem

Issue and self-improve runs publish checkpoints to origin as `refs/uzi-checkpoints/<branch>` (ADR-122). The publish points are milestones, iteration boundaries and the mid-turn tick (ADR-1597).

When such a run reaches a terminal state, the api deletes that ref (PRD #1030 M4), because a stale branch-scoped ref would block the next run on the same branch with a `not_descendant` skip.

For a **failed** run, that delete can remove the only remote copy of its checkpointed work. Run `ec4eedfa` (#1856) is the verified case:

1. At 13:30:10Z it published its fix, tip `16745474` ("checkpoint published to origin (time-based)"; the run's `checkpoint_tip_at` is still `13:30:10.8859Z`).
2. At 13:40:38Z it failed at a Codex boundary (#1864).
3. By 13:42Z the ref was gone.

Recovering the work then needed kube access to the worker volume and a hand-made git bundle.

## Current state (verified on `main`, 2026-09-28)

- **Eligibility.** `checkpointBranch` (`api/internal/workersvc/checkpoint_branch.go`) makes two run kinds checkpoint-eligible:
  - `issue` runs, on branch `agent/issue-<n>`;
  - `self_improve` runs, on branch `uzi/self-improve/<run-id>`.
- **The tip is recorded.** The published tip is stored in `runs.checkpoint_tip` (migration `00185`). `runs.checkpoint_tip_at` records when it was published (it is exposed on the run DTO). The DTO does not expose `checkpoint_tip` itself.
- **Terminal cleanup today.** `deleteCheckpointBestEffort` (`api/internal/workersvc/service.go`) is called from three places:
  - the worker-reported terminal transition (`service.go`, the terminal-report path);
  - the two server-side cancel and plan-reject paths (`api/internal/workersvc/submit.go`).

  It runs detached and best-effort, and it is CAS-guarded on `runs.checkpoint_tip` through `pushbroker.DeleteOptions.ExpectedOldTip`.
- **Sweeper failure paths never clean up.** The sweeper paths that fail a run do not call the cleanup, e.g. `FailRunsOfStaleWorkersOverCap` in `api/internal/workersvc/sweep.go` (worker lost, requeue budget exhausted). Those runs keep their branch-scoped ref today, and they are the runs most likely to have lost their worker's volume.
- **The broker.** `pushbroker` (`api/internal/pushbroker/pushbroker.go`) has `Publish` (with the internal `forwardPack` for a manual receive-pack command) and `Delete` (a CAS path over a manual receive-pack command). Its ref prefix is the constant `checkpointRefPrefix = "refs/uzi-checkpoints/"`. It has no operation that creates a ref pointing at an object the remote already has.
- **Pushing without new objects is normal git.** `git push origin <sha>:refs/<new>` for an object the remote already holds sends an empty pack ("Total 0") with a create command (old = zero id). Receive-pack's connectivity check accepts it because the object is present. uzi has never exercised this path, so M6 verifies it on each forge.
- **The existing sweep.** `Service.Sweep` (`api/internal/workersvc/sweep.go`) already runs periodic passes, among them the recovery passes `ExpireStalledUploads` and `ExpireReadyCaptures`. `UZI_RECOVERY_READY_RETENTION` (`api/internal/config/config.go`, default 168 h) is the operator-configurable recovery retention.
- **Where recovery shows today.**
  - Web: `web/src/pages/RunView.tsx` and `web/src/components/RecoveryArchives.tsx`.
  - Docs: `docs/run-recovery.md` and `docs/cli.md`.
  - Skills: `.agents/skills/uzi-watcher/SKILL.md` and `.agents/skills/uzi-watcher/resume-recipe.md`.
  - The CLI's embedded skill: `api/internal/uzicli/skill/SKILL.md`.
- **Exposure.** ADR-1597: GitHub milestone checkpoints, and park, shutdown, pause and capture checkpoints, are published **without** the pre-publish secret scan. Deleting a ref also does not reliably purge its objects from a forge, so the extra exposure from keeping a ref is the added discoverability during the retention window.

## Decisions

0. **Off by default, enabled per forge.**
   - A new setting `UZI_SALVAGE_FORGES` (a comma list of `github`, `gitlab`, `forgejo`; default empty) turns promotion on for the listed forge kinds only.
   - With the setting empty, or for an unlisted forge, every path behaves exactly as today (immediate delete).
   - Merging this work therefore changes nothing until a forge has passed M6 and the maintainer enables it.
1. **The sweep promotes; the terminal path defers only non-secret failures.**
   - A new pass in `Service.Sweep` finds checkpoint-eligible runs with status `failed`, a recorded `checkpoint_tip`, and no salvage row yet, and records each one as `pending`. It does this only on enabled forges.
   - For a worker-reported `failed` transition on an enabled forge, the terminal path stops calling `deleteCheckpointBestEffort` and leaves the ref to the sweep, **except** when `fail_origin` is `push_secret_blocked`. That case keeps today's immediate CAS delete, so a known secret is never kept waiting for a sweep.
   - Completed, cancelled and plan-rejected runs keep today's immediate delete.

   The sweep also applies the secret policy (decision 4) to server-side failures. One place then covers every failure path and retries for free. There is no new loop and no new detached goroutine.
2. **Promotion is identity-bound and idempotent** (a new `pushbroker.Promote`). The operation, in order:
   1. List the remote's `refs/uzi-checkpoints/<branch>` and `refs/uzi-salvage/<run-id>`.
   2. If the salvage ref exists at the recorded tip, it is an idempotent success. If it exists at any other tip, it is `refused`: never overwrite.
   3. If the branch ref does not equal the recorded tip (missing, or moved by a sibling run), the result is `unavailable`, and no salvage is created.
   4. Otherwise, create `refs/uzi-salvage/<run-id>` with old = zero id, new = tip, and an empty pack, reusing `forwardPack`'s manual receive-pack machinery.
   5. Only after a confirmed create, CAS-delete the branch ref with the existing `Delete` (`ExpectedOldTip` = tip).

   Promote runs through the same endpoint, auth, SSRF gate (`forgeBaseURLAllowed`) and bot-PAT handling as `Publish` and `Delete`.
3. **Failure handling, partial success, and bounds.**
   - **Bounded pass.** Each sweep tick handles at most 5 salvage candidates within a 20 s total wall budget **including** broker calls. Pass each call only the remaining budget, and leave unprocessed candidates for the next tick, so a forge outage cannot stall the other sweep passes.
   - **Retry.** A failed promotion stays `pending`, keeps the branch ref, and retries on later sweeps with the attempt count recorded.
   - **Partial success.** If the salvage ref was created but the branch-ref delete hit a transport error, the row stays `pending`. The next pass recognizes the same-tip salvage ref (an idempotent success) and retries the delete. A row becomes `promoted` only after the branch-ref delete succeeds, or after the CAS proves another owner moved the branch ref.
   - **Attempt cap.** After 10 failed attempts the row becomes `failed`. The branch ref is kept, because a stale ref causes a recoverable `not_descendant` skip while deleting it would lose the work. The run page then shows the retained branch ref and an actionable recovery command (`git fetch origin refs/uzi-checkpoints/<branch>`), and says it can still block a later run on the same branch.
   - **Visibility.** The state and a bounded, scrubbed last error are shown on the run.
4. **Exposure policy.**
   - A run whose `fail_origin` is `push_secret_blocked` is never promoted; its state is `skipped_secret`. Its branch ref is deleted as today.
   - Every other failed run is promoted, and its salvage ref expires after `UZI_RECOVERY_READY_RETENTION`. No new knob.
   - The tradeoff, stated in the ADR: up to the retention period of added discoverability for an already-published tip, in exchange for recovery without worker access.
5. **Expiry.**
   - When a row becomes `promoted`, set `expires_at = promoted_at + UZI_RECOVERY_READY_RETENTION`. If the retention is zero or negative, `expires_at = promoted_at`, so the next sweep removes the salvage ref: effectively today's behavior.
   - The same sweep pass CAS-deletes expired salvage refs (a `Delete` generalized to take the ref name, still CAS on the recorded tip), marks them `expired`, and retries a failed remote delete on later sweeps.
   - The retention bound applies to **promoted** salvage refs only. A promotion that never succeeds (`pending` or `failed`) can leave the old branch-scoped checkpoint ref in place for longer; the run page says so.
6. **Custody and wording.**
   - A salvage tip is the **last published checkpoint**, which may be behind the failed run's final local work.
   - Salvage never releases, discards or changes a custody hold.
   - Every surface says "checkpointed commits saved", never that the run's full state was recovered.
7. **Visibility.**
   - The run DTO gains flat scalar fields so `uzi run get --field` works: `salvage_state`, `salvage_ref`, `salvage_tip`, `salvage_expires_at`, all null when there is no row.
   - The web failed-run view (`RunView`, next to `RecoveryArchives`) shows the state, the ref, and a copyable `git fetch origin refs/uzi-salvage/<run-id>`.
   - Add web mock data.
   - `landing_state` is unchanged: it stays scoped to the human-landable fail origins (#1418).
8. **Recovery order.** The recovery docs and the uzi-watcher skill say: check whether salvage exists, then compare its tip with any available recovery archive, live clone and worker tracking ref. Restore the freshest verified source; never prefer an older salvage checkpoint merely because it is remotely available.

## Out of scope

- A "retry a failed run" action. That is the separate idea this PRD enables; it is not filed.
- Promoting on completed, cancelled or plan-rejected runs.
- Scanning checkpoint ranges retroactively.
- Changing ADR-1597's scan boundary.
- Custody changes (#1349, #1848).

## Milestones

Every milestone runs its component gate (`task gate:api`, plus `task gate:web` for M4). Documentation milestones also run `task check-docs:web` and `task docs:sync`. Nothing touches `.github/workflows/**`: the worker PAT lacks the `workflow` scope (`.claude/rules/prds.md`).

- [ ] **M1 Broker operations.**
  - `pushbroker.Promote` per decision 2, and a ref-name-generalized CAS `Delete`, both behind the existing SSRF, auth and timeout handling.
  - Tests against a local bare repository cover: create, idempotent re-create at the same tip, refusal at a different tip, `unavailable` when the branch ref is missing or moved, and branch delete only after a confirmed create.
  - A credential never appears in errors (`secretscrub`).
- [ ] **M2 Store.**
  - Migration: a `run_salvage` table keyed by `run_id`:
    - `tip`;
    - `state`, checked against `pending`, `promoted`, `unavailable`, `refused`, `failed`, `skipped_secret`, `expired`;
    - `attempts`, `last_error` (bounded, scrubbed);
    - `promoted_at`, `expires_at`, `updated_at`.
  - **No cascade** from `runs`, repos or owners. This follows ADR-1296's "nothing recovery-related cascades from owner, repo, or run": a cascaded row would vanish while its public remote ref survived, leaving nothing for the sweep to expire.
  - **Fail-closed deletion.** A live salvage ref keeps a database backstop against deleting its run and repository until the CAS delete of the ref is confirmed.
    - Use the ADR-1296 custody pattern: a `RESTRICT` live pointer, cleared when the remote ref is deleted, plus immutable provenance in case expired metadata stays visible.
    - Add owner-facing 409 guards, naming the ref, on both **repo removal** and **forge-connection removal**. `DeleteConnection` currently cascades repos and runs without going through the repo-removal guard.
    - Serialize salvage-row creation against those deletes, so a sweep cannot insert an orphan row after a count-then-delete check.
    - The guards lift once the ref is CAS-deleted or confirmed absent (the row is then `expired`, its live pointer cleared).
  - **Regression tests:** `pending` and `promoted` salvage against both delete routes (repo removal and forge-connection removal), including a sweep inserting a salvage row concurrently with the delete.
  - The migration is drafted as `00264` and renumbered at landing per the repo rule. Add the sqlc queries.
- [ ] **M3 Sweep pass and terminal path.**
  - Enqueue, promote with retries, apply the secret policy, delete the branch ref after promotion, expire salvage refs and retry failed remote deletes.
  - Add the `UZI_SALVAGE_FORGES` setting (default empty = today's behavior everywhere).
  - Stop the immediate delete on the worker-reported `failed` terminal path, only on enabled forges and never for `push_secret_blocked`, which keeps the immediate CAS delete.
  - Enforce the per-tick bounds and the partial-success rules (decision 3).
  - Fake-broker tests cover:
    - each state transition;
    - that `completed`, `cancelled` and plan-rejected keep the immediate delete;
    - sweeper-failed runs (stale worker over cap);
    - that a run that never published makes no broker call;
    - that with the setting empty, or the forge unlisted, behavior is byte-for-byte today's;
    - a salvage create followed by a failed branch delete stays `pending`, and the next pass retries only the delete;
    - the 5-candidate and 20 s bounds;
    - `expires_at` with a positive and with a non-positive retention.
  - The regression test for the #1856 shape (a worker-reported failure with a recorded tip, with the test enabling the forge) must fail on current `main`, where the ref is deleted, and pass with the fix.
- [ ] **M4 Visibility: DTO, web and CLI.**
  - The flat DTO fields, the `RunView` salvage panel with mock data, and the CLI fields (`uzi run get`).
  - Update `api/internal/uzicli/skill/SKILL.md` and `docs/cli.md`.
  - Web and CLI tests.
- [ ] **M5 Docs, ADR, skill.**
  - `docs/run-recovery.md` gains a salvage section with the decision 8 order.
  - Update `.agents/skills/uzi-watcher/SKILL.md` and `.agents/skills/uzi-watcher/resume-recipe.md`.
  - A new ADR named for this issue (ADR-1867) covers the namespace, lifecycle, secret policy and exposure tradeoff, and ADR-122 links to it.
  - Document `UZI_SALVAGE_FORGES` where the other `UZI_RECOVERY_*` settings are documented, stating that it is default-off and gated per forge by M6.
  - A `specs/human.md` line tagged `(AI-synced YYYY-MM-DD)`.
  - A CHANGELOG `[Unreleased]` entry.
- [ ] **M6 Real-forge gate (maintainer-owned; the worker stops at M5).**
  - A forge kind may be added to `UZI_SALVAGE_FORGES` only after it passes on a **controlled test repository on a real instance of that forge**: a deliberately failed run's checkpoint is promoted, the branch ref is deleted, `git fetch origin refs/uzi-salvage/<run-id>` returns the tip, and the salvage ref expires.
  - The compose e2e harness does **not** count: its GitLab and Forgejo lanes use `forge-fake` (real git smart HTTP, not the real products).
  - Initial support is whichever forges pass. Each forge that cannot be tested stays off, with today's behavior.
  - The switch is default-off, so this gate can run after merge without any forge being affected before it passes.

## Dependency plan

| Phase | Milestones | Depends on | Files |
|---|---|---|---|
| 1 (parallel) | M1, M2 | none | M1: `api/internal/pushbroker/`; M2: `api/internal/store/migrations/`, `api/internal/store/queries/` |
| 2 | M3 | M1, M2 | `api/internal/workersvc/` (`sweep.go`, `service.go`) |
| 3 | M4 | M2, M3 | `api/internal/apitypes/`, `api/internal/handler/`, `api/cmd/uzi/`, `web/src/` |
| 4 | M5 | M3, M4 | `docs/`, `adr/`, `.agents/skills/uzi-watcher/`, `specs/human.md`, `CHANGELOG.md` |
| 5 (maintainer) | M6 | M1–M5 merged with the switch default-off | controlled test repos on each real forge; then `UZI_SALVAGE_FORGES` per passing forge |

All of it is in one repo. M1 and M2 touch disjoint files. A single uzi run executes the phases serially.

## Success criteria

- With `UZI_SALVAGE_FORGES` empty, nothing changes anywhere.
- On an enabled forge:
  - an eligible failed run whose branch ref still matches its recorded tip leaves `refs/uzi-salvage/<run-id>` after confirmed promotion, and the run page shows how to fetch it;
  - other outcomes expose their state and the retained ref;
  - no next run on the same branch is blocked by a promoted salvage ref;
  - a `push_secret_blocked` run is never promoted, and its branch ref is deleted immediately as today;
  - promoted salvage refs disappear after the retention period;
  - custody holds are unaffected.

## Risks

- **Forge rejects the packless create:** M6 verifies each forge; failure is recorded and the branch ref is kept, never lost.
- **A sibling run moves the branch ref before promotion:** the result is `unavailable` by design. Never overwrite or delete the moved ref; the failed run's recorded tip is unavailable for automatic promotion.
- **Delay between failure and promotion (one sweep interval):** a new run started in that window may see the old branch ref and take a `not_descendant` skip, as it already can today.
- **Exposure:** promoted salvage refs expire after the retention period. A `pending` or `failed` promotion can leave the old checkpoint ref public longer. `push_secret_blocked` keeps its immediate delete. See decision 4.

## Decision Log

- 2026-09-28: Filed from the #1856 recovery. The user proposed pushing checkpoints to git; investigation showed uzi already does, and that the terminal cleanup deleted the only remote copy.
- 2026-09-28: Reviewed with the maintainer's buddy session. It added:
  - identity-bound, idempotent promotion;
  - durable state with expiry and retries;
  - the custody and wording limits;
  - the unscanned-publish exposure (ADR-1597);
  - forge proof;
  - the freshest-source recovery rule.
- 2026-09-28: Promotion moved from the detached terminal path into the periodic sweep, because the sweeper's own failure paths never call the terminal cleanup.
- 2026-09-28: Second buddy review, which added:
  - immediate delete kept for `push_secret_blocked`;
  - a bounded sweep with partial-success rules;
  - `expires_at` defined, including non-positive retention;
  - the sibling-ref wording;
  - M6 made a real-forge gate, since `run-e2e.sh`'s GitLab and Forgejo lanes are `forge-fake`.

  To keep merge safe without that proof, promotion ships default-off behind `UZI_SALVAGE_FORGES`.
- 2026-09-28: Third buddy review, which added:
  - no cascade from runs, repos or owners (ADR-1296), with fail-closed run and repo deletion while a salvage ref may exist;
  - qualified success and exposure claims;
  - a sweep wall budget that includes broker calls.
- 2026-09-28: Fourth buddy review, which added:
  - a `RESTRICT` live pointer with provenance (the ADR-1296 pattern);
  - 409 guards on forge-connection removal as well as repo removal, since `DeleteConnection` cascades past the repo guard;
  - serialized salvage insertion against those deletes.

  A plain non-cascading `run_id` would keep the row but not the connection and PAT the sweep needs to remove the remote ref.
