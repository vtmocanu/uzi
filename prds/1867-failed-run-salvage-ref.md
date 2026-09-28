# PRD #1867: Keep a failed run's published checkpoint under a run-scoped salvage ref

**Issue**: #1867
**Status**: M1–M5 implemented; M6 (real-forge gate) pending, maintainer-owned
**Priority**: High
**Related**: #1856 (the motivating run), #1864, ADR-0122 (checkpoint push broker), ADR-1597 (mid-turn checkpoint durability), PRD #1030 M4 (terminal checkpoint cleanup), #1296 (recovery captures), #1418 (`landing_state`), PRD #1810 / PR #1819 (checkpoint retention follows custody — landed on `main` mid-implementation and reshaped this PRD's design, see the Decision Log), ADR-1867 (this PRD's own ADR)

## Problem

Issue and self-improve runs publish checkpoints to origin as `refs/uzi-checkpoints/<branch>` (ADR-122). The publish points are milestones, iteration boundaries and the mid-turn tick (ADR-1597).

When such a run reaches a terminal state, the api deletes that ref (PRD #1030 M4), because a stale branch-scoped ref would block the next run on the same branch with a `not_descendant` skip.

For a **failed** run, that delete can remove the only remote copy of its checkpointed work. Run `ec4eedfa` (#1856) is the verified case:

1. At 13:30:10Z it published its fix, tip `16745474` ("checkpoint published to origin (time-based)"; the run's `checkpoint_tip_at` is still `13:30:10.8859Z`).
2. At 13:40:38Z it failed at a Codex boundary (#1864).
3. By 13:42Z the ref was gone.

Recovering the work then needed kube access to the worker volume and a hand-made git bundle.

## Current state (verified on `main`, 2026-09-28, BEFORE PRD #1810/PR #1819 landed)

This section is kept as the historical snapshot that motivated the Problem above. It is
**no longer an accurate description of `main`**: PRD #1810 (PR #1819) landed on `main`
while this PRD was in flight and replaced `deleteCheckpointBestEffort`'s three call sites
with `retainOrDeleteCheckpoint`, added the `checkpoint_retentions` table, a
terminal-transition trigger, `pushbroker.CreateRef` and a generalized `Delete`, and its
own `ReconcileCheckpointRetentions` sweep pass. See
[ADR-1810](../adr/1810-checkpoint-retention-follows-custody.md) for what actually ships
on `main` today, and the rewritten Decisions below for how this PRD's design changed in
response.

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

**As built. This section was rewritten (2026-09-28) after PRD #1810 / PR #1819 landed on
`main` mid-implementation; see the Decision Log for what changed and why. Only the
decisions below reflect the shipped code — do not read the milestones as validating the
original promote-then-delete design above.**

0. **Off by default, enabled per forge — unchanged from the original design.**
   - `UZI_SALVAGE_FORGES` (a comma list of `github`, `gitlab`, `forgejo`; default empty)
     turns the sweep's enqueue on for the listed forge kinds only.
   - With the setting empty, or for a forge that was never listed, salvage inserts no row
     and makes no broker call for it: byte-for-byte today's behavior. This holds only
     while no `run_salvage` row exists for that forge — the create/expire phase
     (`processSalvage`) is not gated on the setting, so a row from before a forge was
     de-listed still progresses (its `promoted` ref still expires on schedule; a `pending`
     row with no recorded ref is CAS-deleted of any unrecorded copy and settled
     `disabled`).
   - Merging this work therefore changes nothing until a forge has passed M6 and the
     maintainer enables it.
1. **Salvage is create-only, downstream of PRD #1810's retention, and never touches the
   terminal path.** The original decision 1 ("the sweep promotes; the terminal path
   defers only non-secret failures, except `push_secret_blocked`") does not hold: #1810
   already owns the terminal-path decision (retain while custody is open, delete once
   settled) end to end, and salvage does not modify, defer, or opt out of any part of it.
   Instead, a separate `sweeper.Pass`, `SweepSalvage`, runs independently:
   - it enqueues `pending` rows for failed, checkpoint-eligible runs with a recorded
     checkpoint tip on an enabled forge, excluding `plan_rejected` failures and rows
     already recorded (`ListSalvageCandidates`);
   - a `push_secret_blocked` run is enqueued `skipped_secret` directly (no pointer, no
     broker call ever);
   - every other pending row is later handled by the sweep's broker phase (decision 2).

   `deleteCheckpointBestEffort` (replaced by `retainOrDeleteCheckpoint` at all three call
   sites by #1819, not merely renamed — the new function retains a held run's checkpoint
   ref instead of always deleting it) is untouched
   by this PRD.
2. **Promotion reuses `pushbroker.CreateRef` (#1810's primitive, widened), not a new
   `Promote`.** The PRD's own constraint (prefer reusing `CreateRef` unless it cannot
   serve) held: it can, so no parallel primitive was built. `createSalvageRef`
   (`api/internal/workersvc/salvage.go`):
   1. lists `refs/uzi-salvage/<run-id>`, `refs/uzi-checkpoints/<branch>` and
      `refs/uzi-recovery/<run-id>` in one `ListRefTips` call;
   2. a salvage ref already at the recorded tip is an idempotent success (`salvageCreated`,
      no write); at any other tip it is `salvageRefused` (never overwritten, no write);
   3. the source is whichever of the branch ref or the recovery ref is at the recorded
      tip; neither being at the tip is `salvageUnavailable` (no write) — origin no longer
      vouches for the tip under a uzi-owned ref;
   4. otherwise `CreateRef{Ref: salvage ref, Tip, SourceRef: source}`: `Old = zero`, an
      empty pack, never forced, read back to confirm.

   `CreateRef`'s prefix allowlist and `validateCreateRef` were widened (by this branch,
   #1867, after #1819 merged, per the operator's steering) to accept
   `refs/uzi-salvage/*` as a target with a source under either
   `refs/uzi-checkpoints/` or `refs/uzi-recovery/`. `Delete` and `ListRefTips` were widened
   the same way, for salvage's own CAS delete and its one combined ref lookup. There is no branch-ref delete step at
   all: salvage never deletes or moves the source ref it reads from, only ever its own
   `refs/uzi-salvage/<run-id>`.
3. **Failure handling and bounds, as built (differs from the original numbers).**
   - **Bounded pass.** `salvagePassBudgetDefault` = 10s (not 20s) for the whole
     `SweepSalvage` pass, DB reads and every broker call included; `salvageMaxItems` = 5
     broker items per pass, unchanged from the plan.
   - **Round-robin, alternating lead.** The 5 items are split round-robin between due
     expiries and due pending rows; which list leads **alternates every pass**
     (`Service.salvageLeadPending`), added in review so a hanging item at one list's head
     cannot starve the other list for more than every other tick — not in the original
     plan.
   - **Retry.** A failed create stays `pending` and retries next pass, with `attempts`
     recorded.
   - **No partial-success case exists**, because there is no branch-ref delete to partially
     complete. A create that lands unrecorded (the client lost the response) is cleaned up
     by `deleteUnrecordedSalvage` before the row is ever settled without a recorded ref.
   - **Attempt cap and hard ceiling (differ from the plan's single "10 failed attempts →
     failed").** `salvageAttemptCap` = 10: the attempt that would REACH the cap makes no
     create call at all, only the orphan cleanup, and settles `failed` only once that
     cleanup succeeds (so the row's own create can never starve its capping cleanup's
     budget). Past the cap, a row backs off to at most one retry per
     `salvageRetryBackoff` (1h), and gives up unconditionally (no further forge call) at
     `salvageHardCeiling` = 3× the cap (30 attempts) — a refinement added in review so a
     permanently dead remote cannot hold the RESTRICT pointer, or spend forge calls,
     forever.
   - **Visibility.** The state and a bounded, scrubbed last error are shown on the run —
     unchanged from the plan.
4. **Exposure policy — unchanged from the plan, in substance.**
   - `push_secret_blocked` is never salvaged (`skipped_secret`); its checkpoint ref is
     handled entirely by #1810's own terminal-path policy, unaffected by this PRD.
   - Every other eligible failed run is a salvage candidate, and a created salvage ref
     expires after `UZI_RECOVERY_READY_RETENTION`. No new knob.
   - The tradeoff (stated in [ADR-1867](../adr/1867-failed-run-salvage-ref.md)): up to the
     retention period of added discoverability for an already-published, possibly
     unscanned tip (ADR-1597), in exchange for an archive copy independent of custody
     settling. **Expiry counts from the salvage ref's own creation, not from #1810's
     settlement of the source ref** — the two clocks are unrelated.
5. **Expiry.**
   - A created salvage ref's `expires_at` is set at creation
     (`promoted_at + UZI_RECOVERY_READY_RETENTION`, or `promoted_at` itself for a
     non-positive retention, which the next expiry pass removes immediately).
   - The same `SweepSalvage` pass CAS-deletes due expiries (`deleteSalvageRef`, confirmed
     by a re-list), settles `expired`, and retries a failed remote delete on a later pass.
   - The retention bound applies to a **created** (`promoted`) salvage ref only. A row that
     never reaches a confirmed create (`pending`, or eventually `failed`) holds no public
     ref of its own; the source ref it reads from is #1810's to retain or delete, on
     #1810's own schedule.
6. **Custody and wording — unchanged from the plan.**
   - A salvage tip is the **last published checkpoint**, which may be behind the failed
     run's final local work.
   - Salvage never releases, discards or changes a custody hold, and never deletes or
     moves a ref #1810's retention manages.
   - Every surface says "checkpointed commits saved", never that the run's full state was
     recovered.
7. **Visibility — as planned, delivered.**
   - The run DTO carries flat scalar fields for `uzi run get --field`: `salvage_state`,
     `salvage_ref`, `salvage_tip`, `salvage_expires_at`, `salvage_last_error`, all null
     when there is no row.
   - `RunView`'s `SalvagePanel` (next to `RecoveryArchivesPanel`) shows the state, the ref,
     the tip, the expiry, and a copyable `git fetch origin refs/uzi-salvage/<run-id>`.
   - `landing_state` is unchanged: it stays scoped to the human-landable fail origins
     (#1418).
8. **Recovery order — widened to name #1810's refs explicitly.** The recovery docs and the
   uzi-watcher skill say: check whether salvage exists, then compare its tip against any
   available recovery archive, the run's retained checkpoint or recovery ref (#1810), a
   live worker clone, and the worker's own tracking ref. Restore the freshest verified
   source; never prefer an older salvage checkpoint merely because it is remotely
   available.

## Out of scope

- A "retry a failed run" action. That is the separate idea this PRD enables; it is not filed.
- Promoting on completed, cancelled or plan-rejected runs.
- Scanning checkpoint ranges retroactively.
- Changing ADR-1597's scan boundary.
- Custody changes (#1349, #1848).

## Milestones

Every milestone runs its component gate (`task gate:api`, plus `task gate:web` for M4). Documentation milestones also run `task check-docs:web` and `task docs:sync`. Nothing touches `.github/workflows/**`: the worker PAT lacks the `workflow` scope (`.claude/rules/prds.md`).

- [x] **M1 Broker operations — delivered as reuse, not a new primitive.** The plan's
  `pushbroker.Promote` was **not built**: decision 2 above records why — #1810's own
  `CreateRef` already served once its allowed-prefix and `SourceRef` validation were
  widened to `refs/uzi-salvage/*`, so building a parallel primitive would have duplicated
  the exact CAS/SSRF/auth/redaction machinery `CreateRef` already has. `Delete` was
  already ref-name-generalized by #1819 ahead of this PRD (it takes `o.Ref`), but this
  branch added the `refs/uzi-salvage/*` case to its own prefix check and required a
  non-empty `ExpectedOldTip` for it, the same CAS rule #1819 already required for
  `refs/uzi-recovery/*`; salvage's own calls use it for its own ref only. Landed:
  `pushbroker.SalvageRefPrefix`/`SalvageRef`, the `refs/uzi-salvage/*` case in
  `validateCreateRef`, and coverage in `pushbroker_salvage_test.go` (create, idempotent
  re-create, refusal at a different tip, `ErrSourceMissing`, the CAS delete, plus
  `TestSalvageRefOverSmartHTTP`, which exercises the create over real smart-HTTP receive-pack
  and skips without `git-http-backend`) and `api/internal/workersvc/salvage_test.go`
  (the AST invariant test plus the sweep-level create/expire cases). A credential never
  appears in errors (`secretscrub`), tested.
- [x] **M2 Store.** `run_salvage` (migration draft `00268`, keyed on `run_id`) as planned,
  with two differences from the plan:
  - an extra `disabled` state (a forge left `UZI_SALVAGE_FORGES` before a pending row's
    create was confirmed — not anticipated in the original state list);
  - extra invariant `CHECK`s beyond the plan's bare state enum: `run_salvage_created_keeps_live_check`
    (a created ref keeps its live pointer until settled `expired`/`disabled`),
    `run_salvage_promoted_created_check` (`promoted` implies a confirmed create),
    `run_salvage_created_expires_pair_check` (creation and expiry are recorded together),
    `run_salvage_live_states_pointer_check` (`pending`/`promoted` always hold the live
    pointer), `run_salvage_skipped_secret_no_pointer_check` (a secret-blocked row never
    holds it).

  **No cascade** from `runs`, repos or owners, per plan (ADR-1296). **Fail-closed
  deletion** as planned: the `RESTRICT` `live_run_id` FK
  (`run_salvage_live_run_id_fkey`), the repo-removal and forge-connection-removal 409
  guards (`api/internal/handler/forge.go`), and serialization against a concurrent sweep
  insert — but the serialization is **FK-only** (a 23503 on insert if the run was deleted
  first), not a lock: see decision "The RESTRICT live-pointer guard and FK-only
  serialization" in [ADR-1867](../adr/1867-failed-run-salvage-ref.md), which explains why
  salvage needs no advisory lock where #1810 does. Regression tests cover both delete
  routes and the concurrent-insert race. Renumbered at landing per the repo rule; sqlc
  queries added.
- [x] **M3 Sweep pass and terminal path — the terminal path is untouched.** `SweepSalvage`
  is a separate `sweeper.Pass` (`api/internal/workersvc/salvage.go`), run on the shared
  sweeper tick before the run-liveness sweep. It does NOT touch
  `deleteCheckpointBestEffort`/`retainOrDeleteCheckpoint` or any part of #1810's terminal
  path — the plan's "stop the immediate delete... only on enabled forges" bullet does not
  apply to the shipped design, because salvage never had authority over that delete once
  #1810 landed. **The #1856-shape regression is covered by #1810's retention, not by
  salvage**: a failed run's checkpoint ref is now retained by #1810 while custody is open,
  independent of whether salvage is enabled on that forge at all; salvage's own tests
  cover its create-only behavior (enqueue, create, expire, `plan_rejected` exclusion,
  `skipped_secret`, the forges-empty no-op, the attempt cap and hard ceiling, the
  round-robin alternating lead), not the #1856 scenario itself. Bounds differ from the
  plan: `salvagePassBudgetDefault` = **10s** (not 20s), `salvageMaxItems` = 5 (as
  planned), `salvageAttemptCap` = 10 with a no-create capping attempt,
  `salvageRetryBackoff` = 1h, `salvageHardCeiling` = 30 (added in review; not in the
  original plan). `UZI_SALVAGE_FORGES` added as planned (default empty).
- [x] **M4 Visibility: DTO, web and CLI — as planned.** Flat DTO fields
  (`salvage_state`/`salvage_ref`/`salvage_tip`/`salvage_expires_at`/`salvage_last_error`),
  `RunView`'s `SalvagePanel` next to `RecoveryArchivesPanel` with mock data, and the CLI's
  `SALVAGE` block in `uzi run get` (`api/cmd/uzi/run_salvage_render.go`). Updated
  `api/internal/uzicli/skill/SKILL.md` and `docs/cli.md`. Web and CLI tests landed.
- [x] **M5 Docs, ADR, skill.** `docs/run-recovery.md` gained a "Salvage copies" section
  with the recovery order, and `task docs:sync` was re-run. `.agents/skills/uzi-watcher/SKILL.md`
  and `resume-recipe.md` were updated with the same rule — compare tips across every
  available source (the salvage ref included) and restore the freshest verified one,
  never preferring an older salvage checkpoint merely because it is remotely reachable.
  `resume-recipe.md`'s "Pick a source" list keeps its own ordering (backup snapshot, live
  PVC, retained checkpoint/recovery ref, worker tracking ref, salvage), not an identical
  copy of `docs/run-recovery.md`'s prose order, since it is written as a recipe to follow
  rather than a priority list. `adr/1867-failed-run-salvage-ref.md`
  was written (the namespace, `CreateRef` reuse, the RESTRICT/FK-only serialization, the
  secret and plan-reject policy, the exposure tradeoff, reliance on a single sweeper, the
  retry bounds, and the distinct value/limits/overlap with #1810); `adr/0122-checkpoint-push-broker.md`
  and `adr/1810-checkpoint-retention-follows-custody.md` each gained a one-line cross-link.
  `UZI_SALVAGE_FORGES` is documented in `docs/cli.md`'s salvage section and in
  `docs/run-recovery.md`'s "Salvage copies" section, both stating default-off; the M6
  real-forge gate itself is stated in the `SalvageForges` field comment in
  `api/internal/config/config.go`, not repeated verbatim in either doc page.
  A tagged `specs/human.md` line was added. A CHANGELOG `[Unreleased]` entry was added.
- [ ] **M6 Real-forge gate (maintainer-owned; the worker stops at M5).**
  - A forge kind may be added to `UZI_SALVAGE_FORGES` only after it passes on a **controlled test repository on a real instance of that forge**: a deliberately failed run's checkpoint is copied to `refs/uzi-salvage/<run-id>`, `git fetch origin refs/uzi-salvage/<run-id>` returns the tip, and the salvage ref expires. There is no branch-ref delete step to verify: the shipped design is create-only, and the branch checkpoint ref stays #1810's to retain or delete.
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

## Success criteria (as built)

- With `UZI_SALVAGE_FORGES` empty and no existing `run_salvage` rows, nothing changes
  anywhere: no enqueue, no broker call. A row that already exists from before the setting
  was cleared still reaches a terminal state: a created copy still expires, and a pending
  row settles `disabled` after a CAS delete of any unrecorded copy (decision 0).
- On an enabled forge:
  - an eligible failed run whose checkpoint tip is still verified live under the branch
    checkpoint ref or its recovery ref gets a confirmed `refs/uzi-salvage/<run-id>`, and the
    run page shows how to fetch it;
  - other outcomes (`unavailable`, `refused`, `failed`, `disabled`) expose their state; the
    source ref (branch checkpoint or recovery) they read from is entirely #1810's to keep
    or delete, unaffected by salvage's own state;
  - no next run on the same branch is ever blocked by a salvage ref: salvage never writes
    to `refs/uzi-checkpoints/` or `refs/uzi-recovery/`;
  - a `push_secret_blocked` run is never salvaged (`skipped_secret`); its checkpoint ref
    follows #1810's own terminal-path policy;
  - created salvage refs disappear after the retention period, measured from their own
    creation;
  - custody holds are unaffected: salvage never reads or writes `recovery_custody_holds`.

## Risks (as built)

- **Forge rejects the empty-pack create:** M6 verifies each forge; a failed create is
  recorded and retried (bounded by the attempt cap and hard ceiling); nothing is ever lost,
  because salvage never deletes the source it read from.
- **The source ref moves or is deleted (by #1810's own settlement, or a sibling run) before
  a create lands:** the result is `unavailable` by design. Never overwrite or delete
  anything; the tip is simply unavailable for this salvage attempt. `unavailable` is
  terminal, though, not retried: `SettleSalvage` clears the row's live pointer and
  `ListSalvageDuePending` reads only `pending` rows, so a run with an `unavailable` row is
  never re-enqueued even if a later state (a recovery ref created by supersession) would
  have made the tip available again.
- **A permanently dead forge (revoked PAT, dropped allowlist entry, deleted repo):** bounded
  by `salvageHardCeiling` (30 attempts, with the 1h backoff past the cap) — at most about 20
  extra hourly attempts before the row gives up and releases the RESTRICT pointer.
- **Exposure:** created salvage refs expire after the retention period, from their own
  creation. A row that never reaches a confirmed create (`pending`/`failed`) holds no
  public ref of its own. `push_secret_blocked` is never salvaged. See decision 4.
- **Overlap with #1810 for the common case:** while a failed run's custody hold is open,
  #1810 already keeps its checkpoint or recovery ref reachable, so a salvage copy of it is
  redundant for that window. Accepted, not eliminated — see the Decision Log and
  [ADR-1867](../adr/1867-failed-run-salvage-ref.md)'s "distinct value" section.
- **No hold, no copy:** a failed run with no open custody hold has its branch ref CAS-deleted
  by #1810 soon after the terminal transition (a background settle, or the retention
  reconcile in the same sweep), usually before the salvage pass on a later tick tries the
  create, so salvage almost always settles it `unavailable` and saves nothing. Salvage produces a copy only for
  held runs (and surviving pre-migration refs); for those, its own value is the window
  between the hold settling and the copy's expiry. If a hold stays open longer than the
  retention, the copy expires while #1810 still retains the tip and only duplicated
  exposure. The #1856-shaped loss is prevented by #1810's retention when the run held
  custody, not by salvage.

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
- 2026-09-28: **Redesigned after the #1810/#1819 heads-up, mid-implementation.** The
  operator's constraints named PR #1819 (PRD #1810) as about to merge to `main` and
  changing the same seams this PRD touches: it replaces `deleteCheckpointBestEffort`'s
  three call sites with `retainOrDeleteCheckpoint`, retaining a failed/cancelled run's
  checkpoint ref while any custody hold is open and moving a superseded tip to
  `refs/uzi-recovery/<run-id>`, backed by a new `pushbroker.CreateRef` and a
  ref-name-generalized `Delete`. #1819 then merged onto `main`
  (`4eeef012`), and M1's implementation was rebuilt on top of it per the operator's
  explicit constraints: reuse `CreateRef` (widened to `refs/uzi-salvage/`) rather than a
  parallel `Promote`; never independently delete or move a ref #1810's retention manages
  (the branch checkpoint ref or `refs/uzi-recovery/<run-id>` while a
  `checkpoint_retentions` row is `retained`/`superseding`/`superseded`/`settling`); salvage
  may still CREATE its own separate ref at a verified tip. This is why decisions 1 and 2
  above, and M1 and M3's milestone entries, describe a create-only sweep with no
  branch-ref delete step, rather than the original promote-then-delete design.
- 2026-09-28: **The remaining distinct value, limits and overlap, stated explicitly per
  the operator's constraint 4.** Once #1819 was on `main`, #1810 already keeps a held
  failed run's checkpoint reachable and supplies run-scoped recovery refs during
  supersession — so salvage's premise (a failed run's only remote copy gets deleted) no
  longer holds unconditionally the way it did when this PRD was filed from #1856.
  Salvage's remaining distinct value is: (1) a salvage copy has its own retention clock,
  independent of custody settling — it can still be fetchable after #1810 has already
  deleted the branch/recovery ref once the last hold settled, for up to
  `UZI_RECOVERY_READY_RETENTION` past its own creation; (2) it is a second, independent
  ref location, not new data #1810 did not already publish; (3) it applies uniformly to
  any eligible failed run on an enabled forge, including one that predates #1810's
  migration and so never got a `checkpoint_retentions` row, PROVIDED that run's branch ref
  (or a recovery ref) still happens to survive at the recorded tip when the sweep looks —
  salvage does no special backfill for pre-migration runs, it simply runs the same
  enqueue query uniformly. The remaining limit: for the common case (a failed run whose
  custody hold is still open), #1810 already keeps the exact same tip reachable under the
  branch or recovery ref, so a salvage copy of it is REDUNDANT for as long as that hold
  stays open — this PRD does not run two independent state machines side by side
  pretending the other does not exist; salvage's design is strictly downstream of #1810
  (read-only against its refs, never competing with it for a delete), so the redundancy is
  bounded to salvage's own retention window and one-directional. See
  [ADR-1867](../adr/1867-failed-run-salvage-ref.md)'s "distinct value" section for the
  full statement.
- 2026-09-28: **Plan-gate deviations from the milestones as originally scoped**, recorded
  per the review that steered M3-M5 to completion:
  - `plan_rejected` runs are excluded from the salvage enqueue query entirely
    (`ListSalvageCandidates`), not merely left un-promoted as the original "completed,
    cancelled and plan-rejected runs keep today's immediate delete" bullet implied —
    there is no "immediate delete" for salvage to keep or not keep, since salvage never
    had delete authority over the terminal path once the redesign above landed;
  - `SweepSalvage` is its own `sweeper.Pass`, not a case added to the existing recovery
    passes, so its 10s budget and its round-robin item bounds are independent of
    `ExpireStalledUploads`/`ExpireReadyCaptures` and of #1810's own
    `ReconcileCheckpointRetentions`;
  - the `disabled` state (a forge rollback before a pending row's create was confirmed)
    was not in the original state enum and was added during M2/M3 implementation;
  - the attempt-cap refinement (a capping attempt makes no create, only the orphan
    cleanup) and the `salvageHardCeiling` (30 attempts) were both added in review, not
    part of the original M3 plan;
  - FK-only serialization (no advisory lock) for M2's fail-closed deletion was a design
    choice made once salvage's operations turned out to be single-row and
    re-driveable from persisted state alone, unlike #1810's multi-step, lock-protected
    supersession;
  - M3 and M4 overlapped rather than running strictly serial as the original dependency
    plan implied (`git log`: M4's DTO/api/cli/web commits landed between M3's initial
    commit and M3's later review-round commits): the DTO/web/CLI surface (M4) only reads
    `run_salvage` rows and needed no coordination with the sweep pass's own internal
    logic (M3) beyond the shared schema from M2, so M4 did not have to wait for M3's
    review rounds to finish.
