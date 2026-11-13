# PRD #4205: Worker hibernation: sleep and wake a persistent hosted worker

**Epic**: #4204. **Status**: Implemented (M1, M2); live hosted-k8s acceptance pending. **Blocked by**: none.

## Problem

Every persistent hosted worker keeps one pod running, with its memory and CPU requests (plus a DinD sidecar on the Docker lane), whether or not its owner has run anything for weeks. On a shared cluster that idle capacity competes with live runs and has contributed to node memory pressure and evictions. Today the only way to stop an idle worker is to delete it, which destroys its data volumes, or for an operator to scale its Deployment by hand, which the controller does not understand.

## Outcome

An admin can **hibernate** an idle persistent hosted worker: its Deployment goes to zero pods while the worker row, its join-token Secret and all its PVCs stay. A hibernated worker **wakes automatically** when a run needs it, and an admin can wake it by hand. While asleep it is shown as hibernated, not offline or broken. The feature sits behind a new admin setting, `worker_hibernation_enabled`, which defaults to off. This PRD adds no automatic idle policy: that is a later child PRD of the epic.

Acceptance examples:

1. **Manual sleep, then wake on demand.** The admin enables hibernation in Admin → Settings and clicks *Hibernate* on an idle worker. The controller sets replicas to 0. Once Kubernetes has terminated the pod and the controller has confirmed it is absent, the Workers page shows `hibernated`; until then it shows `hibernating`. The PVCs and the Secret remain. The owner then starts a run: the run waits as `queued` with health "your worker is waking from hibernation", the worker scales back to one pod, registers, and claims the run.
2. **Refused while protected.** *Hibernate* on a worker that has an assigned non-terminal run, an open recovery custody hold, a manual or controller cordon, DinD maintenance in progress, or a roll in progress is refused with the blocking reason. Nothing changes.
3. **Claim race.** A run that becomes claimable by the worker while the hibernation request is being prepared cancels the hibernation before scale-down commits. Demand that arrives after scale-down commits wakes the worker. The run is never left queued with only a sleeping worker and no wake.
4. **Turning the feature off wakes everyone, paced.** Disabling `worker_hibernation_enabled` wakes every hibernated worker, at most 10 per pass, so a fleet-wide wake cannot recreate the memory pressure this epic relieves. Demand-driven wakes are never paced or gated by the setting.

## Out of scope

These are later child PRDs of the epic, or not planned:

- Automatic hibernation after an idle threshold, and the Admin health "could hibernate, blocked by X" check.
- Disk cleanup at hibernation.
- PVC retirement after a hibernation period.
- Capability-aware warm coverage per user.
- Ephemeral (run-bound) workers. They already have an idle lease and are never hibernated.
- Compose and local workers.
- A CLI write verb. CLI admin tokens are read-only (`admin_ro`), so the actions are web-only. The CLI only shows the state.

## Modules and seams

Facts verified on main `3c4a5ed7b`:

- **Desired-state snapshot.** The api sends the controller a full snapshot every poll (`controller/internal/protocol/protocol.go` `PollResponse` / `DesiredWorker`, built in `api/internal/hostedsvc/service.go` `Poll` from `ListHostedWorkersForController`). - **Teardown if absent.** The desired-id set is built at `controller/internal/kube/materializer.go:619-621`. Pass 3 (`:692-707`) and `teardown()` (`:1288-1300`) delete the Deployment, the Secret and all PVCs of any worker missing from that set. **Invariant: a hibernated worker always stays in the snapshot.**
- **Zero replicas means maintenance today.**
  - When the Deployment is absent, `:1064-1094` creates it from `RenderDeployment` (`render.go:1030`, replicas 1).
  - `patchFor` writes `replicas: 1` (`:1201`).
  - The paused arm (`:1100`) restores a zero-replica Deployment that carries a roll marker.
  - The no-drift early return (`:1106`) leaves alone a zero-replica Deployment without a marker.

  Hibernation therefore needs its own explicit intent, covering **create too**: a hibernated worker whose Deployment is missing is created at replicas 0. Drift, ForceRoll, recycle and a controller restart must not wake a hibernated worker.
- **Idle and protected predicates.**
  - `DesiredWorker.Busy` means any assigned non-terminal run (queued, paused and waits included).
  - `CustodyHeld` means an open `recovery_custody_holds` row whose `live_worker_id` is this worker.
  - `fn_worker_accepts_new_intake` (migration `00355`) is the effective-cordon and maintenance definition. It is false during manual or controller cordon, a maintenance fence, and DinD maintenance phases.
  - The api's roll state is display-only, so the **controller** is the authority on rolls (see *Authoritative transitions*).

  `Busy`, `CustodyHeld` and the intake fence must be clear at api admission. The controller then confirms that no roll marker, pacing entry, disk-pressure recycle or DinD maintenance is in flight.

**Store / state machine (api).** Add additive `workers` columns:

| column | values / purpose |
|---|---|
| `hibernation_state` | `none` (default) / `preparing` / `hibernated` / `waking` |
| `hibernation_op_id` | uuid of the current transition |
| `hibernation_requested_at`, `hibernated_at`, `wake_requested_at` | timestamps of each transition |
| `wake_error` | nullable, last wake failure |

A new hibernation module owns the state machine:

- **Hibernate (`none` to `preparing`).**
  - Refused unless the controller advertises hibernation support in a status report fresher than 2 poll intervals (see *Version skew*).
  - In one transaction, lock the worker row, taking the same row lock `ClaimRun`'s claimant CTE takes, so admission serializes against claims.
  - Recheck `Busy`, `CustodyHeld` and the intake fence; refuse with a typed reason if any fails.
  - Set `preparing` with a fresh op id.
  - **The `preparing` deadline (10 min) and a controller `refused` never simply restore `none` once a mutation may have happened.**
    - **Direct revert.** `none` is restored directly only when the controller positively reports `refused` *before* any patch. In that case no `scaling_down` was sent and the Deployment's resourceVersion is unchanged since admission.
    - **Recovery otherwise.** A deadline, or a refusal after a possible patch, moves to `waking`. Claims stay fenced until the confirmed-awake settlement below.
    - The reason is recorded either way.
- **Claims.**
  - `CREATE OR REPLACE` `fn_worker_accepts_new_intake` to also require `hibernation_state = 'none'`. It has about 32 call sites, including the spread-peer mirror (`runtime.sql` ~1627), and this change covers them all.
  - Also close the own-run arm `OR r.worker_id = c.id` in `runtime.sql:1173-1175` and in `ClaimChatRun` (`chat.sql`), so a resume or affinity pin cannot claim either. Ordinary cordon does not exclude a pin.
- **Wake (any state to `waking`).** Mint a fresh op id.
- **Settle.** Settlement depends on what the controller reports. Writing the replica count is never enough; it waits for a `ready` transition with a named pod.
  - **Evidence, in either order.** Settlement persists two pieces of identity-bound evidence on the row, and settles as soon as both are present and match, whichever arrives first:
    - `ready_evidence`: the controller's `ready` transition for this op, carrying a Ready pod UID and the Deployment resourceVersion produced by the wake patch;
    - `register_evidence`: a worker **Register** after `wake_requested_at`.

    Register locks the row (`GetWorkerForUpdate`, `runtime.sql:530-535`) and rotates `snapshot_register_nonce` (`:334`).
  - **Withdrawn preparation.** The absence of a `scaled_down` transition does not prove the original pod survived: the patch may have committed and its acknowledgement been lost. Settlement therefore accepts either case:
    - the `ready` pod UID equals the pod UID recorded at admission, with a fresh heartbeat, so the original pod never stopped;
    - a replacement pod UID, which requires `register_evidence` as for a full wake.
  - **Wake after confirmed scale-down.** It always requires both pieces of evidence, with a new pod UID. A heartbeat carries no pod identity, so it never settles a wake.
- **Stale operations.** Every transition carries the op id. A transition whose op id is not the row's current one is rejected (409), and the row is unchanged.

**Wire (api to controller).** `DesiredWorker` gains `hibernation: {state, op_id}`. The poll-wire golden `api/internal/hostedsvc/testdata/controller_poll_wire.json`, the api and controller protocol structs and both contract tests change together.

**Authoritative transitions.** Controller status reports stay display-only: by contract, the `StatusReport` comment in `protocol.go` says no claim, heartbeat or register path reads them, and `handler.go:1507` agrees. Lifecycle transitions instead use a dedicated control-write route, `POST /api/controller/workers/{workerID}/hibernation`.

- **Mounting:** next to `drain` and `dind-maintenance` (`handler.go:1511-1512`), in the same pattern as `api/internal/handler/controller_dind_maintenance.go`, with `DisallowUnknownFields`.
- **Request body:** `{op_id, transition, deployment_uid, pod_uid?, resource_version?, reason?}`.
- **Transitions:**
  - `refused`: the controller found a roll marker, pacing entry, disk-pressure recycle or DinD maintenance in flight, and carries the reason.
    - The api reverts to `none` only when the refusal is proven pre-patch: no `scaling_down` was sent and the resourceVersion is unchanged since admission.
    - Otherwise it goes to `waking`, per the state machine above.
  - `scaling_down`: sent when the replicas 0 patch is applied. It records the pod UID observed.
  - `scaled_down`: sent only after the controller observed zero worker pods (the same pod-absence check `recycleWorkerVolumes` uses).
  - `ready`: sent with a Ready pod UID.

**Version skew.** The controller decodes the poll leniently (`DisallowUnknownFields` appears only at `controller/internal/config/config.go:844`), so an old controller ignores the new field.

- **Advertisement.** The controller advertises `hibernation: 1` in its status report. Api admission refuses unless a fresh advertisement is present, and the `preparing` deadline covers a controller rolled back mid-operation.
- **Downgrade.** An older api drops the field, so a zero-replica Deployment with no roll marker hits the no-drift return (`:1106`) and never wakes. The operator doc therefore says: before downgrading the api, disable the setting and wait for every worker to wake.
- **Validation:** the api checks the op id, and that the transition is legal from the current state.
- **State changes:** it moves `preparing` to `hibernated` only on `scaled_down`, and settles `waking` per the rules above.

**Stale Kubernetes mutations.**

- The controller stamps each hibernation patch with the op id, as a Deployment annotation `uzi.dev/hibernation-op`.
- **Every hibernation patch is resourceVersion-conditioned.** A conflict on an old op's scale-down is final for that op. The controller never re-reads and retries it against a newer resourceVersion without first refreshing the snapshot and revalidating that the desired intent is still that op. It names the Deployment UID and the resourceVersion the controller last observed.
  - **Hibernate patches** (`replicas: 0`) are sent only while the latest poll snapshot still requests that op.
  - **The wake patch** is itself conditioned. Its success changes the resourceVersion, so any older scale-down patch still in flight fails with a conflict. Kubernetes object identity fences it, not ordering.
- **Claims stay fenced until earlier mutations are fenced.**
  - Wake settlement requires `ready_evidence` whose resourceVersion is the one returned by the wake patch, or a later one, observed by the controller.
  - Before sending `ready`, the controller verifies it has no unresolved scale-down request in flight for that worker: every earlier patch has returned success, conflict or not-found, or is fenced by the resourceVersion change above.
  - An ambiguous result (a timeout or a lost response) is resolved by re-reading the Deployment before `ready` is sent.
- After a restart the controller rebuilds everything from the snapshot and the annotations; it holds no local state.
- The chart runs one controller with `strategy: Recreate` (`deploy/chart/templates/controller-deployment.yaml:56`, enforced by `scripts/assert-controller-strategy.sh`). The resourceVersion conditioning above is what fences a stale mutation from an earlier controller instance as well.

**Controller.** A hibernation arm runs before drift and roll handling:

- **`preparing` / `hibernated`:** hold replicas at 0.
  - **No drift patch while asleep.** Template drift is applied once, on wake.
  - A ForceRoll does not scale up.
  - A missing Deployment is created at replicas 0.
  - No roll marker or pacing slot is taken.
- **`waking`:** one `patchFor` write applies the current template and replicas 1. Then report `ready` with the Ready pod's UID.
- It never deletes the worker's PVCs or Secret.
- **Pacing and roll health.**
  - Hibernated and preparing workers are left out of the `planRoll` pools and coverage (`roll_pacing.go` ~181-238).
  - No roll-health entry is sent for them. A podless Deployment otherwise reads `rolling` with a null `PhaseSince` forever.

**Wake demand (api sweeper pass).** Shaped like `hostedsvc.EphemeralProvisioner.ProvisionPass` (`api/internal/hostedsvc/ephemeral.go` ~160), and run **before** ephemeral provisioning on the same tick:

- **Demand is per run.** For each queued run, of every kind (issue, prompt, chat, judge, cross-check, CI-fix, rework, task), that no online worker can claim, pick a hibernated or preparing worker of the same owner that would be eligible for **that run**.
  - **Eligibility is not reimplemented.** Build it on the predicates of `ListUnplaceableQueuedRunsForEphemeral` (`api/internal/hostedsvc/ephemeral.go:196`), evaluated as if the worker were online and accepting intake.
  - A LiveDB differential test against `ClaimRun` pins the equivalence. Cases: Docker allowlist, capabilities, cross-check lane, chat, judge, forge-origin cooldown, and a run pinned to the hibernated worker.
- **The wake pass always runs.** It runs whatever `ephemeral_workers_enabled` says (`ProvisionPass` returns early when that is off, `:161`) and whatever `worker_hibernation_enabled` says.
- **Saturation does not wake.** Capable workers that are online but full do not wake a sleeper in this PRD. The run waits, as it does today.
- **Deterministic choice:** the worker with the oldest `hibernated_at`, then lowest id.
- **Withdrawal:** a `preparing` worker with demand is withdrawn (it goes to `waking`).
- **One wake per pending run.** Record a `hibernation_wakes` row (run id, worker id, first wake time). Repeated passes for the same pending run reuse it and never wake a sibling. One woken worker may serve several runs it is eligible for.
- **Suppression is demand-specific.** Ephemeral provisioning skips only runs that have a recorded wake whose worker is still `waking` and eligible for that run. The suppression lasts up to `worker_wake_timeout` (a fixed 15 min, not a setting in this PRD), measured from the **first** wake time, which retries do not reset. After that, that run may get an ephemeral worker as today. Runs needing another capability or lane are never suppressed.

**Admin surface.**

- **Routes:** `POST /api/admin/workers/{id}/hibernate` and `/wake`.
  - They are cookie admin writes with CSRF, mounted in the admin-write group of `routes_admin.go`. The cordon routes are owner routes under `RequireUser` (`routes_workers.go:32-33`) and are not the model here.
  - Hibernate is refused with 409 and the reason when blocked.
- **Audit:** both actions go to a new sibling table, `worker_hibernation_events` (actor, action, op id, reason). `worker_cordon_events.action` has a CHECK limited to `('cordon','uncordon')` (`00355:22`), so it is left unchanged.
- Admin → Workers adds buttons and a `hibernated` / `waking` status chip.
- Admin → Settings adds a hibernation card with the `worker_hibernation_enabled` toggle. The pattern is `ephemeral_workers_enabled`: `api/internal/settings/keys.go`, the validation switch in `settings.go`, `settings_hosted.go`, and `web/src/pages/adminSettings/EphemeralWorkersCard.tsx`.
- The CLI `uzi worker list` and `uzi admin workers` show the state read-only. CLI work belongs in this PRD per the AGENTS.md CLI-parity rule.

**Health and display.**

- `MarkStaleWorkersOffline` keeps marking a hibernated worker offline, but every consumer reads `hibernation_state` first. The Workers page, `uzi worker list` and the roll report show `hibernating` (preparing), `hibernated` or `waking`.
- The owner health reason for a queued run waiting on a waking worker is "your worker is waking from hibernation".
- `fleet.capacity` is owner-scoped (`healthsvc/checks.go:45`), and its `ListOwnersWaitingNoCapacity` treats a waking worker within `worker_wake_timeout` as transient: no alarm.
- Admin-facing wake failures go on a new **instance-scoped** `fleet.hibernation_wake` check, with bounded evidence and a capped worker list.
- **Every reader is named and updated:**
  - the worker DTO, as a three-file edit: Go struct, `fixtures/api-contract`, `web/src/lib/apiTypes.ts`;
  - the TUI `api/cmd/uzi/tui_workers.go`;
  - the web mock data;
  - `uzi worker list`.
- A wake past `worker_wake_timeout` sets `wake_error` and raises the instance-scoped `fleet.hibernation_wake` warning. The wake is retried on each pass.

**Resource bounds.**

- The admin routes are cookie admin only.
- The wake pass is bounded by the queued-run set it scans, which is already bounded for `ProvisionPass`. It wakes at most one worker per pending run, and setting-disable wakes are paced at 10 per pass.
- The fleet stays within the existing 512-worker poll cap.

## Testing decisions

- **LiveDB (`workersvc` / `hostedsvc` / `store`).**
  - Admission refusal for each of the four predicates.
  - The claim-versus-hibernate race: two goroutines on the locked row, so exactly one wins and a claimed worker never reaches `preparing`.
  - Claims return nothing for every non-`none` state, including a resume pin.
  - A stale op id is rejected.
  - Each illegal transition is refused.
  - Demand while `preparing` withdraws the hibernation, in both cases:
    - before `scaled_down`, where the same pod UID settles without a new pod;
    - after `scaled_down`, where only a new pod UID settles.
  - The wake pass runs before ephemeral provisioning and suppresses a duplicate ephemeral worker only for the demanding run, within the timeout from its first wake.
  - A run needing another capability is not suppressed.
  - Repeated passes do not wake a sibling.
  - Chat, judge and cross-check demand each wake an eligible worker.
  - Disabling the setting wakes all hibernated workers, at most 10 per pass. Demand wakes are not paced.
  - Admission is refused without a fresh controller advertisement.
  - A `preparing` deadline, or a post-patch refusal, goes to `waking`, not `none`, and claims stay fenced until the confirmed-awake settlement.
  - Only a pre-patch refusal restores `none` directly.
  - Settlement works when Register arrives before the `ready` transition, and when it arrives after.
  - Withdrawn preparation whose scale-down acknowledgement was lost settles via a replacement pod.
  - A delayed old-op replicas 0 patch, sent after apparent readiness, fails its resourceVersion precondition. This holds both before and after a new claim is admitted; the worker is never interrupted.
  - A controller `refused` transition reverts to `none`.
  - The intake function closes every claim lane, including the own-run arm and `ClaimChatRun`.
  - The wake-eligibility differential test against `ClaimRun` covers every case listed under *Wake demand*.
  - A Register, not a heartbeat, settles a wake after scale-down.
  - An owner with one awake drifted worker and one hibernated worker still rolls the awake one.
- **Controller (`controller/internal/kube`).**
  - Hibernated replicas hold at 0 under drift, ForceRoll and a restart (rebuild from the snapshot).
  - `scaled_down` is sent only after pods are observed absent.
  - A delayed old-op replicas 0 patch fails its resourceVersion precondition after the wake patch.
  - An ambiguous patch result is resolved by re-reading the Deployment before `ready` is sent.
  - After a restart, state is rebuilt from the snapshot and the annotations.
  - PVCs and the Secret survive.
  - Waking patches replicas to 1.
  - Follow the existing materializer recycle tests.
- **Contract.** The poll-wire golden is re-recorded (never hand-authored) and both module contract tests run with `-count=1`.
- **Web.** Vitest for the settings card, the Workers buttons and the status chip, covering a refusal message.
- **CLI.** A state-column test.
- **Regression.** A LiveDB test proving a hibernated worker stays in `ListHostedWorkersForController`. The teardown-if-absent invariant is the data-loss risk.

## Milestones

**M1: An admin hibernates and wakes a worker by hand, safely.** Blocked by: none.

Covers the setting and card, the migration, the admission state machine and claim fencing, the wire field, acknowledgements and golden, the controller sleep/wake arm, the admin routes, Workers buttons and chips, CLI display, docs and tests.

Acceptance: examples 1 (manual wake), 2 and 4 pass in the tests above.

**M2: Demand wakes a hibernated worker.** Blocked by: M1.

Covers the wake pass, `preparing` withdrawal, ephemeral-provisioning suppression, `worker_wake_timeout` with `wake_error` and the Admin health warning, and the owner health reason.

Acceptance: examples 1 (on-demand wake) and 3 pass in the tests above.

Docs ride with each milestone: `docs/hosted-workers.md` (a hibernation section), the operator settings docs, `docs/cli.md`, and `task docs:sync`.

## Owner acceptance

A live hosted-k8s check (hibernate, wake on demand, PVCs intact) is filed as a linked `acceptance` issue, owned by the maintainer. It does not block merge.

## Decision Log

- **D1. Durable intent on the worker row, carried in the snapshot.** The rejected alternative was dropping the worker from the snapshot, which tears down every PVC and the Secret. Another rejected alternative was reusing the roll marker, which the paused arm restores.
- **D2. A four-state machine with op-bound acknowledgements.** The rejected alternative was a boolean. A boolean cannot tell a stale acknowledgement from a current one, and cannot withdraw a hibernation that is still preparing.
- **D3. Hibernation takes the same row lock as the claim** and closes every claim lane, including pins. Ordinary cordon still admits pinned continuations, so it is insufficient.
- **D4. Wake completes on a new pod's registration,** not on the replica patch, so health never claims capacity that is not there.
- **D5. The wake pass runs before ephemeral provisioning, with a timeout,** so sleeping capacity is used first. A failed wake still falls back to provisioning.
- **D6. The actions are web-only.** `admin_ro` CLI tokens are read-only by design, and adding a write scope is a trust-boundary change this PRD does not need.
- **D7. Waking is never gated by the setting.** Turning the feature off must never strand a worker asleep.
- **D10. The controller is authoritative for rolls and confirms support.** The api only sees roll state through display-only reports, and an old controller would silently ignore the intent. Hence the advertisement, `refused` and the `preparing` deadline.
- **D11. Settle on Register.** Register is row-locked and nonce-rotating. A heartbeat cannot identify the new pod.
- **D8. Transitions go through a dedicated controller route, not the status report.** Status reports are display-only today, and lifecycle authority must be validated per op. A stale Kubernetes mutation is fenced by resourceVersion-conditioned patches. Convergence alone could interrupt a worker after it has claimed a run. Claims stay fenced until earlier mutations are resolved.
- **D9. Wake suppression is per run.** Owner-wide suppression would starve runs needing another capability, and it would wake siblings repeatedly.
- **D12. Controls live on Admin → Health (AI decision, 2026-11-13).** Admin → Workers did not exist (PRD #4212 is Draft), so the controls sit in the "Fleet, all users" table and move with #4212.
- **D13. Roll admission and cordon (AI decision, 2026-11-13).** Waking workers are also excluded from roll admission, and workers in any state but `none` read `effective_cordoned`.
- **D14. Audit scope (AI decision, 2026-11-13).** The audit table records only admin hibernate and wake. System transitions are on the row (`hibernation_reason`) and in logs.
- **D15. Pre-patch proof for `refused` (AI decision, 2026-11-13).** The controller writes a unique annotation-only fence patch, which advances the resourceVersion, and refuses only when the fenced object lacks the op annotation. This stands in for "resourceVersion unchanged since admission", which the api never learns. `resource_version` carries the fence resourceVersion.
- **D16. Roll evidence (AI decision, 2026-11-13).** Hibernating workers stay in the roll snapshot so reservation and marker evidence is readable, and are excluded after it. Preparing fails closed on any roll evidence, and a missing Deployment under roll evidence is neither created nor refused.
- **D17. Re-entrant audit table (AI decision, 2026-11-13).** Down keeps it, and a re-Up validates it only when it pre-existed.
- **D18. Op scoping (AI decision, 2026-11-13).** Resets are op-scoped and settle evidence is op-bound. A Register while preparing re-targets the admission pod until a scale-down is recorded.
- **D19. Demand-wake recording (AI decision, 2026-11-13).** One transaction: worker lock, run `FOR SHARE`, insert `ON CONFLICT DO NOTHING`, transition. A commit error is an unknown outcome resolved by the next pass. This is atomicity only, with no storage-durability claim.
- **D20. One wake per pending run (AI decision, 2026-11-13).** The wake row is kept until the run leaves the queue while its worker is waking or has settled awake, so a settled worker that cannot take the run never causes a sibling wake. A row whose worker is hibernated or preparing again is deleted and the run is demand again (the approved stale-row recovery). Hibernate is refused (`pending_wake_demand`) while a queued run's wake names the worker, so that recovery path is rare and a run is never stranded. (Revised 2026-11-13 in the cross-check repair.)
- **D21. Provisioning suppression (AI decision, 2026-11-13).** Applied at the shared `provisionOne` boundary for all triggers, only while the worker is waking and within 15 minutes of the first wake.
- **D22. Shared placement (AI decision, 2026-11-13).** The placement predicate is extracted into the set-returning inlinable function `fn_placement_admitting_workers`. The demand query adds `fn_worker_can_claim` and `fn_forge_origin_eligible`. Documented divergences from `ClaimRun`: custody-admission limit, recovery spread, released nonce, pending overflow, weekly reserve and Codex quarantine.
- **D23. Candidate selection (AI decision, 2026-11-13).** Candidates include an already-waking worker (with no failed wake), so one woken worker serves several runs. A sleeper pre-filter keeps the pass cheap when nothing hibernates, and the scan is `LIMIT`ed to the ephemeral batch.
- **D24. Chat continue (AI decision, 2026-11-13).** It loses its pin to a hibernated worker, and demand wakes one.
- **D25. Downgrade (AI decision, 2026-11-13).** Disable, wait until all workers are awake, stop the api, Down 00438, 00437, 00436, then 00435, and start the older api. An older api cannot claim on the new schema.
- **D26. LiveDB sweep (AI decision, 2026-11-13).** `hostedsvc` is added to the LiveDB sweep list.
