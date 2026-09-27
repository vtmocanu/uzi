# PRD #1795: Plan-gate verdicts bound to the gate revision they were sent against

**Issue**: #1795
**Priority**: High
**Status**: Ready; reviewed with the Codex peer (3 rounds), sent to uzi in Auto mode.
**Builds on**: PRD #41 (plan-revision loop, gate epochs), issue #1604 (replayable verdict receipts, `resume_plan_at` replay cutoff, `superseded` discard lane), #1772 (resumed-gate epoch bump for executors without `resumesAtGate`), PRD #37 (agent selection on approve), PRD #1247 M5 (`resume_plan_seq`).

## Problem

A plan-gate verdict (`approve_plan`, `reject_plan`, `revise_plan`) is matched to a gate by the worker's **per-claim gate epoch**, taken when the verdict is routed, not by the plan the human actually saw. Two defects follow.

1. **Report-response race (#1795).** `agent/src/runner.ts` sends the `awaiting_approval` report (`await reportState(...)`, ~L10304) and bumps the epoch only after it resolves (`if (this.gatedRuns.has(runId)) steering.bumpEpoch()`, ~L10320). The server makes the gate visible when it persists the report, before its response reaches the worker, while the steering poll loop runs independently (`agent/src/steering.ts` ~L2073, ~L2103, ~L2146). An approve created in that window is routed at the OLD epoch (~L1907), then discarded as epoch-stale when the bump runs (~L1784) and settled `superseded` via `/inputs/discarded`. The human approved the plan on screen and gets a stale-approve notice instead.
   Affected: every gate that bumps the epoch, i.e. a revision gate, and (since #1772) the first gate of a resumed claim with an unapproved plan when the executor lacks `resumesAtGate` (stub, Codex), `resume_plan_at` is absent or unusable, or the resume phase is not `awaiting_approval` (`runner.ts` ~L5163). A fresh first gate does not bump.
2. **Stale client surface.** Nothing tells the server which plan a web tab, CLI invocation or Slack card displayed. A human approving from a tab that still shows plan N while the run already gated plan N+1 approves an unseen plan whenever the verdict happens to route at the new epoch. Slack alone guards this, with its `gate_ts` anchor check (`api/internal/slacksvc/gatekeeper.go` ~L170-176).

The e2e red in #1739 (fixed harness-side by PR #1796) is the same family: an approve posted before a restarted run's new claim re-showed its gate was correctly discarded, but nothing on the wire said which gate it was meant for.

## Goal

Every plan-gate verdict carries the identity of the gate presentation it was sent against, the server stamps it atomically with gate publication, and the worker's gate takes a verdict only on an exact match. Clients that show a plan send the revision they displayed, and the server refuses a mismatch.

Boundaries that must hold (each is a named acceptance test in M6):

- **B1.** An approve of the newly persisted plan survives a delayed report response (the #1795 race).
- **B2.** An approve sent against the previous plan stays stale even when polled after the transition.
- **B3.** A failed, declined or uncertain report never authorizes an unseen plan; a lost response after successful persistence neither double-allocates a revision nor loses a verdict bound to it.
- **B4.** A revise stays unapplied until the revised plan answering it is confirmed durable (`settleRevision` only after a confirmed applied `awaiting_approval` report, `runner.ts` ~L10318).
- **B5.** A verdict submitted while no gate is visible can never approve a gate presented later. Only an unbound **approve** is prohibited: it is disposed of with a notice through the discard lane. An unbound reject or revise keeps today's legacy handling, which is fail-closed (a reject only stops the run, a revise only re-plans and re-gates) (amended 2026-09-27, A2).
- **B6.** A client that displayed revision N cannot approve, reject or revise revision N+1 (409).

### Non-goals

- Changing gate semantics otherwise: the revise cap, reject-with-reason, agent selection, capability override and the autopilot (no-gate) path are unchanged.
- Removing the epoch or the #1604 replay-cutoff machinery. Both stay as the legacy path for rows and peers that predate this PRD (D3). A later cleanup can retire them once every supported worker advertises the capability.
- `.github/workflows/**`: not touched by implementation or validation (`.claude/rules/prds.md`).

## Design

### D1. Gate presentation identity

The worker mints a **presentation id** (UUID) for each gate it shows and sends it on the `awaiting_approval` report. Reuse rules:
- a retry of the same report (lost response) reuses the id;
- an SDK resume that re-presents the persisted unapproved plan at its gate (`resumesAtGate`, resume phase `awaiting_approval`) reuses the persisted id, delivered on the claim;
- every other gate (fresh plan, revision, stub/Codex resumed gate) mints a new id, **even when the plan text is identical** (text or status equality cannot tell these apart).

The server maps a presentation id to a monotonically increasing per-run **gate revision**: a new id allocates `revision + 1` atomically with persisting the report. A retry or re-presentation succeeds only for the run's **current** presentation and only with an unchanged approval-relevant payload (`plan_md`, milestones, and the approval-relevant requirements: required capabilities, required tools and size class); it returns the current revision. `plan_changed_files` is NOT part of presentation identity: it is advisory evidence of the planning turn's worktree writes, recomputed each round, so a same-gate reclaim whose tree listing changed keeps its revision (amended 2026-09-27, A1). The comparison is against an immutable snapshot of the payload as presented, stored at publication, never against the live requirement columns an approval's capability override may have cleared. A **historical** id (any presentation older than the current one) is refused, so a replayed old presentation can never overwrite a newer gate. The current id with a changed approval-relevant payload is a refused conflict, never a silent retry. Existing claim-generation fencing on the report still applies.

A refused report (a historical id, a changed-payload conflict on the current id, or a stale adoption, D4) is a **failed gate report** (amended 2026-09-27, A3). It never publishes the refused payload as a new gate and is never retried under a fresh id. The worker confirms no revision and takes no verdict, parks through the existing transient-recovery path, and the next claim re-presents. Repeated refusals are bounded: the server counts a refusal once per claim generation, under the same run-row lock and claim fencing as the report, and the refusal that would push the count past `RUN_GATE_REFUSAL_MAX` (default 3, 0 = unlimited) fails the run with fail origin `gate_presentation_refused`. A successful publication resets the count.

Plan-gated run paths only: issue runs (and any other kind that reaches `awaiting_approval` through the human gate). Chat, judge and autopilot never allocate a gate revision and never acquire unbound-verdict behaviour; their verdict handling is unchanged.

### D2. Stamping under the run-row lock

Every insert of a gate verdict takes the same run-row lock as gate publication and binds the input to the run's current gate revision **inside that statement** (an UPDATE-backed insert obtains it through `RETURNING`, never a sibling snapshot read), so an insert cannot bind to the old revision across a concurrent publication. Insertion seams as of `main` `75608063` (re-anchor at implementation time; main has moved since):
- plain / nil-selection approve: `api/internal/workersvc/submit.go` ~L515 → `CreateRunInput` (`api/internal/store/queries/runtime.sql` ~L5339);
- selection-bearing approve: `submit.go` ~L699 → `CreateApprovePlanInput` (`runtime.sql` ~L5452);
- revise: `CreateRunReviseInputIfUnderCap` (`runtime.sql` ~L5375);
- reject: `submit.go` ~L337 / ~L448 → `CreateStopVerdictInput` (`runtime.sql` ~L5538);
- Slack reject (`slacksvc/gatekeeper.go` ~L242) and revise replies (`slacksvc/replier.go` ~L255) go through the shared service and inherit the stamping. Slack's own `gate_ts` supersede guard stays.

The `expected_gate_revision` check (D5) runs under the same lock, **before any side effect**: a mismatch inserts nothing and changes nothing (no agent selection written, no milestone freeze, no `revise_count` increment, no `stop_kind` stamp, no capability clear from `override_capabilities`).

### D3. Three input bindings, persisted

A verdict row carries one of three bindings, persisted so replay and rolling upgrades keep the distinction:
- **bound(N)**: stamped with revision N; the worker takes it only at the gate whose revision is N (exact match), and bypasses the epoch and replay-cutoff checks for it (otherwise a matching input could be discarded before revision matching runs);
- **unbound**: created by a new-protocol server while no gate was visible. An unbound **approve** can never act on a later gate: it is disposed of as stale with a notice, through the existing discard lane. An unbound **reject** or **revise** keeps today's legacy handling, which is fail-closed (a reject only stops, a revise only re-plans and re-gates), so a reject sent during initial planning still stops the run at its first gate and pre-gate revise feedback is taken at the first gate. Bound rejects and revises stay subject to exact revision matching and to client expected-revision checks (amended 2026-09-27, A2);
- **legacy**: no binding recorded (rows from before the migration, or an api without this PRD); today's epoch and replay-cutoff behaviour, unchanged.

Absence of the field is legacy; an explicit unbound value is never treated as legacy.

### D4. Worker protocol and compatibility

A new worker protocol capability (e.g. `gate_revision_v1`) gates the new wire fields. The report ACK carries the allocated revision; the claim carries the persisted presentation id and revision for a gate resume.

Inputs across versions:
- Old worker + new api: the api still stamps, the worker ignores the fields; behaviour is today's (epoch matching).
- New worker + old api: no fields arrive; every input is legacy; behaviour is today's.

Gate publication across versions:
- **New api + old worker report** (no presentation id): each accepted `awaiting_approval` report allocates a new revision (the old worker cannot say whether it is a retry; a spurious extra revision only makes earlier-bound verdicts stale, which the old worker ignores anyway because it matches by epoch). The persisted gate has a revision but no presentation id.
- **New worker resuming a gate that has no presentation id** (published by an old worker, or before the migration): the claim carries the revision N with a null id. Adoption is an **explicit operation**, never inferred from an unchanged payload (an identical-text stub/Codex gate would look the same): only an SDK re-presentation of that same persisted plan sends `adopt_gate_revision: N` with its minted id. The server accepts it only under the run-row lock when the current revision is N, its presentation id is null, the approval-relevant payload matches, and claim fencing passes; it then binds the id to N, so a pending approve bound to N still counts. Any other state refuses the adoption (stale adoption). A retry after a lost adoption ACK carries the same id and is answered as a current-id retry. A previously unseen presentation id without `adopt_gate_revision` allocates N+1, including an identical-text stub/Codex resumed gate. Current-id retries follow D1 and retain the revision. Pending legacy rows keep today's epoch and replay-cutoff handling.

Refused reports across versions (amended 2026-09-27, A3): a refused report (historical id, current-id conflict, stale adoption) answers 409 with a reason (`gate_presentation_historical`, `gate_presentation_conflict`, `gate_adoption_stale`) and is a failed gate report per D1: it publishes nothing, the worker parks through transient recovery, and the refusal cap bounds the loop. An id-bearing report without `claim_generation` is refused 400 (`claim_generation_required`), since refusal accounting is fenced on it; an id-less report never refuses. For a worker that advertises `credential_switch_v1`, the existing fail-closed missing-generation check runs first and refuses that report 409 with `disposition: "stale_claim"`, before the 400 check is reached. Both answers refuse the report, and neither publishes or counts anything.

Negotiating server support: a worker's capability advertisement does not tell it that the api accepts the new report fields. The api advertises support in the worker register response (e.g. an api-capabilities list carrying `gate_revision_v1`; follow whatever register-response pattern exists at implementation time). A new worker sends `presentation_id` / `adopt_gate_revision` only when the api advertised it, and omits them otherwise, so an older api's strict request decoding never sees an unknown field.

### D5. Client-supplied expected revision

The run DTO exposes the current gate revision. `POST /api/runs/{id}/inputs` (`RunInputRequest`, `api/internal/apitypes/run.go` ~L917) gains an optional `expected_gate_revision` for the three gate verdict kinds; when present and different from the run's current revision the server answers 409 with a reason that names the current revision. Omitting the field keeps today's behaviour for older clients and scripts. Each consumer binds to the revision it **displayed**, captured when the human starts the action and retained through any confirmation, never refetched at submission:
- **Web**: the gate UI (`web/src/lib/api.ts` ~L1165-1190) captures the revision with the plan it renders; the reject and revise dialogs and the capability-override confirmation carry it through.
- **TUI**: `api/cmd/uzi/tui_steer.go` ~L302; capture the displayed revision when a confirmation opens and keep it through confirm.
- **CLI** (`uzi run approve|reject|revise`, `api/internal/uzicli/client.go` ~L316): new `--expected-gate-revision <n>` for a verdict based on an earlier inspection (`uzi run get` shows the revision). Without the flag the CLI sends the revision it reads at invocation, which only guarantees the verdict targets the gate current at that moment, not a plan the user read earlier; the docs say so. A mismatch exits 5.
- **Slack**: gate cards carry their revision in addition to `gate_ts`; the text-reply flows (revise and reject replies, `slacksvc/replier.go` ~L255) persist the originating card's revision and send it.

### D6. Durable record

This is a seam other code must respect (every future verdict path and gate path must bind to the revision), so it gets an ADR at the numbered-by-issue path for 1795 (created in M7, not before).

## Milestones

- [ ] **M1. Gate revision allocation.** Additive migration (numbered at merge): per-run gate revision and presentation-id mapping, verdict-row binding (bound revision / unbound / legacy). The `awaiting_approval` report path allocates or returns the revision per D1 (new id, current-id retry, historical-id refusal, changed-payload conflict) and D4 (id-less old-worker reports, the `adopt_gate_revision` CAS on a legacy gate resume, api support advertised in the register response), under claim-generation fencing, plan-gated kinds only. Run DTO, report ACK and gate-resume claim expose revision and presentation id. Live-DB tests for each case plus a fenced stale claim.
  - Progress (2026-09-27): the server half is committed on the run branch (`3db26e1e`): migration `api/internal/store/migrations/00259_gate_revision.sql`, classification and allocation in `api/internal/workersvc/gate_revision.go`, the `gate_revision_v1` register feature, the ACK and DTO revision, the gate-resume claim fields, and the `RUN_GATE_REFUSAL_MAX` knob, which is wired into the service params in `api/cmd/server/main.go`. Live-DB tests are in `api/internal/workersvc/gate_revision_livedb_test.go` and `api/internal/handler/worker_gate_revision_livedb_test.go`; a follow-up adds the refusal-class-report-onto-a-parked-or-terminal-run cases. The box is ticked at the end of the run, with the other milestones.
- [ ] **M2. Stamped verdict inserts.** All five insertion seams (D2) bind under the run-row lock via the statement itself; the `expected_gate_revision` check runs under that lock before any side effect. Live-DB tests: bound while gated, unbound while not gated, a concurrent insert racing a publication binds to exactly one revision consistent with commit order, and a mismatched expected revision leaves selection, milestones, `revise_count`, `stop_kind` and capabilities untouched (the `override_capabilities` path included).
  - Progress (2026-09-27): implemented on the run branch, not yet committed. The selection-less approve moved from `CreateRunInput` to a new `CreateGateVerdictInput` (a `SELECT ... FOR UPDATE` CTE the INSERT selects from); `CreateApprovePlanInput`, `CreateStopVerdictInput` (reject only; cancel and stop stay legacy) and `CreateRunReviseInputIfUnderCap` insert `SELECT ... FROM` their update CTE. Each carries the expected-revision predicate, as does `RejectRunServerSide`; the capability-override clear is scoped to the approve's bound revision (0 for a legacy gate) and skipped for an unbound approve. `POST /api/runs/{id}/inputs` answers a mismatch 409 `{error, reason: "gate_revision_mismatch", current_gate_revision}`, and an expected revision on any other kind 400. Live-DB tests: `api/internal/workersvc/gate_verdict_livedb_test.go` and `api/internal/handler/run_input_gate_revision_livedb_test.go`.
- [ ] **M3. Worker gate matching.** Capability advertised; presentation id minted and reused per D1 (report retry, SDK same-gate reclaim, explicit `adopt_gate_revision` on an id-less legacy gate), and the new report fields are sent only when the api advertised support in the register response; the gate takes a bound approve, reject or revise on exact revision match; unbound verdicts disposed of as stale (approve via the discard lane, reject and revise applied on their own per the #1604 receipt rules); legacy rows keep epoch and replay-cutoff behaviour; `settleRevision` ordering unchanged (B4). Agent tests through the real steering channel and runner with a fake api.
- [ ] **M4. Web, TUI and CLI expected revision.** The server contract (`expected_gate_revision`, 409) lands in M2; this milestone wires the consumers per D5: web gate UI plus reject/revise dialogs and the capability-override confirmation, the TUI confirmation, and the CLI flag plus invocation-time default. Web component tests, TUI tests, CLI tests (exit 5 on mismatch).
- [ ] **M5. Slack bound to revision.** Cards carry the revision; a mismatched click is refused with the existing superseded message; text-reply flows carry the originating card's revision; the `gate_ts` guard stays. slacksvc tests for clicks and replies.
- [ ] **M6. Acceptance tests for B1-B6.** Named tests for approve, reject and revise alike, each red on the pre-change code where applicable: a new-plan verdict routed after persistence but before the ACK is taken (B1); an old-plan verdict polled after the transition stays stale (B2); declined report, and lost ACK after successful persistence with a retry reusing the id (B3); revise receipt unapplied until the revised plan is durable (B4); an unbound verdict cannot act on a later gate (B5); client expected-revision mismatch refused with no side effect (B6); historical presentation id refused; identical-text new presentation gets a new revision; SDK same-gate reclaim keeps the revision; version matrix (old worker + new api publication and inputs, new worker + old api, new worker resuming an id-less gate with a pending bound approve, which adoption keeps, and with a pending legacy approve, whose fallback stays intact; stale adoption refused; retry after a lost adoption ACK; new worker against the old api's real strict request decoding sends no new field). The compose e2e suite stays green, including `happy-path-restart`, `plan-revision-loop` and `plan-reject-verbatim` (run `./e2e/run-e2e.sh` on the gitlab lane).
- [ ] **M7. Docs and ADR.** User doc for the plan gate (find the page with `uzi docs search "plan gate"` or `git grep -l awaiting_approval docs/`) states that a verdict applies only to the plan revision it was sent against and what a 409 means; CLI docs (`docs/cli.md`) and the embedded CLI skill (`api/internal/uzicli/skill/SKILL.md`) document the exit-5 mismatch; run `task docs:sync`; write the ADR (D6); update `ARCHITECTURE.md` run-lifecycle notes if they describe gate matching.

## Execution plan

| Phase | Milestones | Depends on | Main files | Parallel? |
|---|---|---|---|---|
| 1 | M1 | none | `api/internal/store/migrations/`, `api/internal/store/queries/runtime.sql`, `api/internal/workersvc/` (report path, claim assembly), `api/internal/apitypes/run.go` | sequential |
| 2 | M2 | M1 | `api/internal/workersvc/submit.go`, `runtime.sql` insert queries, `api/internal/handler/runs_lifecycle.go` | sequential |
| 3 | M3 | M1, M2 | `agent/src/steering.ts`, `agent/src/runner.ts`, `agent/src/protocol.ts`, `agent/src/client.ts` | sequential |
| 4 | M4, M5 | M2 (server contract) | `web/src/`, `api/cmd/uzi/tui_steer.go`, `api/internal/uzicli/`; `api/internal/slacksvc/` | M4 and M5 in parallel |
| 5 | M6, M7 | M1-M5 | tests across the above; `docs/`, `api/internal/uzicli/skill/SKILL.md`, `api/internal/uzidocs/embed/`, ADR | sequential |

## Validation

- `task gate:api`, `task gate:agent`, `task gate:web` green; `task gate:repo` green (migration numbering and additivity).
- The compose e2e gitlab lane green (M6).
- Mutation discipline (`.claude/rules/go.md`): revert the stamping or the exact-match check and watch B1/B2/B5 go red.
- **Maintainer landing requirement (not the worker's):** after merge and release, verify on the hosted k8s workers that a mixed fleet (old worker image still pinned, new api) keeps gating and approving normally, and that a new-image worker takes a verdict on a revision gate. The offline worker does not need cluster access for any milestone.

## Risks

- **Double allocation on retry** would re-open B3; covered by the presentation-id mapping and its live-DB test.
- **Rolling upgrade**: a bound row replayed to an old worker is matched by epoch (today's behaviour), so the fix is only as wide as the new-worker fleet; acceptable, and the capability makes the state observable.
- **Lock contention** on the run row during gate publication is bounded to one run; no cross-run lock.
- **Internet independence**: every fact above comes from this repo; no external lookup is needed by the implementer.

## Decision Log

- 2026-09-27: bind verdicts to a server-allocated gate revision keyed by a worker-minted presentation id, rather than bumping the epoch before the report (unsafe: admits old-plan approves during the report window, `bumpEpoch()` also sets `firstGateShown` and can end the fail-closed replay window early, and a rollback cannot undo concurrent routing or discards). Reviewed with the Codex peer.
- 2026-09-27: explicit unbound is distinct from legacy (a verdict sent with no visible gate must not fall back to epoch matching and approve a later plan).
- 2026-09-27: include client-supplied expected revision (web, CLI, TUI, Slack) in this PRD (user decision). Web, TUI and Slack bind to the revision they displayed; the CLI binds to an explicit `--expected-gate-revision` or, by default, to the revision current at invocation, which does not prove the user read that plan.
- 2026-09-27: only the current presentation may be retried or re-presented; historical ids are refused and the conflict check covers every approval-relevant field, not just `plan_md` (Codex peer review).
- 2026-09-27 (plan decision 1, storage): `runs` gains `gate_revision` (0 = no gate published under an allocating api), `gate_presentation_id`, the immutable presented snapshot `gate_presented_payload` with its sha256 `gate_payload_digest`, and the refusal accounting `gate_refusal_count` / `gate_refusal_generation`; a `run_gate_presentations(run_id, presentation_id, revision)` table (primary key on the id, unique on the revision) lets the server recognise a historical id.
- 2026-09-27 (plan decision 2, id-less publication): an old worker's report without a presentation id allocates `gate_revision + 1`, leaves the id NULL and records no presentation row, but still stores the snapshot and digest so a later adoption compares against it.
- 2026-09-27 (plan decision 3, digest over the presented payload): the snapshot is computed in Go under the run-row lock before any write. Reported fields come from the report. At a publication, the fields the persist query keeps when absent (milestones, required tools, size class) take the value of the live persisted run row, and required capabilities union-merge with it, so the snapshot is exactly what the row holds once the report is applied. The previous snapshot is the base only for comparison: a current-id retry and an adoption compute the effective payload against it and compare digests. The no-snapshot branch of adoption, which initializes from the row's persisted payload under the lock, is defence in depth: every allocation writes a snapshot, and a gate published before the migration stays at revision 0 and is legacy, so it cannot be adopted. The digest covers `plan_md`, milestones, required capabilities, required tools and size class, not `plan_changed_files` (A1). A retained presentation (current-id retry, adoption) passes no requirement params to the persist query and re-writes the presented milestone candidate, so an approval's capability override survives a same-gate reclaim; the gate-resume claim carries the presented requirements as `resume_gate_presented`.
- 2026-09-27 (plan decision 4, verdict binding): `run_user_inputs` gains `gate_binding` and `gate_revision` with a NULL-safe CHECK (outer `IS TRUE`) that admits exactly legacy (NULL/NULL), unbound (no revision) and bound (a positive revision). The verdict inserts stamp the binding from the locked run row (M2): bound while `awaiting_approval` at a revision above 0, legacy at revision 0, unbound in any other status, and always legacy for cancel, stop and follow-up rows and for chat and judge runs.
- 2026-09-27 (plan decision 5, ACK revision): the awaiting_approval ACK carries a top-level `gate_revision` holding the value allocated or returned inside the report's transaction, never re-read after commit; the run DTO exposes `gate_revision` for clients.
- 2026-09-27 (plan decision 6, worker confirmed revision): cleared at gate entry and when a revise is taken, set only from this gate's applied awaiting_approval ACK, so a declined or refused report never confirms anything.
- 2026-09-27 (plan decision 7, id reuse): the worker reuses the claim's persisted presentation id, or sends `adopt_gate_revision` for an id-less gate, only on the no-bump first gate of a claim; every other gate mints a fresh id.
- 2026-09-27 (plan decision 8, refusals): a refused report commits no gate, allocates nothing and is never retried under a new id (A3). Its accounting runs in the same transaction under the run-row lock, fenced on ownership, the report's claim generation, an unreleased claim and a non-terminal status; it counts once per claim generation and past `RUN_GATE_REFUSAL_MAX` fails the run with fail origin `gate_presentation_refused`. A report from a fenced stale claim neither counts nor fails; a refusal from the still-current claim follows the counting policy.
- 2026-09-27 (plan decision 9, autopilot and chat): the worker applies bound/unbound handling only on a human-gated claim; chat and judge rows are always legacy and those runs never allocate.
- 2026-09-27 (plan decision 10, expected revision): a client-supplied `expected_gate_revision` that differs from the run's current revision (or a run not at `awaiting_approval`) answers 409 `gate_revision_mismatch` with `current_gate_revision`; the CLI sends a default only when the run it reads is `awaiting_approval`, so a reject on a queued run keeps today's server-side reject.
