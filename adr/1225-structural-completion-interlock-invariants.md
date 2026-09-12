# ADR-1225: a structural completion contract, a non-bypassable claim clause, and an exact-head permit close the #1220 hole

**Status**: Accepted (implemented, epic #1225, first child issue #1226, M1–M6)
**Date**: 2026-09-12
**PRD**: [prds/done/1226-structural-completion-interlock.md](../prds/done/1226-structural-completion-interlock.md) — carries the full milestone breakdown, code anchors and Decision Log D1–D8; this ADR restates the negative-space invariants a future edit to a claim clause, a hold's park order or a status list would break silently. ADR-1225 is created by #1226, the epic's first child, per the epic's own numbering convention.
**Related**: reuses [ADR-1190](1190-run-pause-invariants.md)'s `paused`-status discipline for the dedicated hold transition, [ADR-628](0628-cross-worker-resume-durability.md)'s worker-affinity ceiling for the honest `same_worker_only` limitation, and [ADR-1197](1197-transient-recovery-park.md)'s verified-capture-before-park template for `captureHoldContext`.

## Decision (summary)

An issue run could freeze five milestones, declare only four complete, and still open a PR with `Closes #N` (run `b0ffd0ea`, PR #1220). #1226 closes that hole with four load-bearing mechanisms:

1. A **versioned, frozen structural completion contract** (`completion_contract_version`/`contract_revision`/`completion_contract`) stamped before the first claim and frozen with `milestones_frozen` at approval — never exempting a run by milestone cardinality.
2. A **non-bypassable `ClaimRun` capability clause**: an interlocked run can be claimed only by a worker that self-reports `completion_interlock_v1`, and this clause sits entirely outside `required_capabilities`, `ClearRunRequiredCapabilities` and the `capability_aware` kill-switch.
3. A **claim-fenced, idempotent completion permit** bound to `(run, contract_revision, branch, exact head H)`, verified against the real PR head across all three forges, gating every `Closes #N`.
4. A **dedicated `SetRunCompletionHold` transition** with a fixed capture-then-park order, so a run that cannot finish structurally holds recoverably instead of failing or completing dishonestly.

## Why this needs its own record

Every invariant below is a predicate this PRD deliberately kept OUT of an existing bypass path, or a fixed ordering it deliberately did not let a later feature reorder. Neither shows up in a diff of "what changed" the way a new column does — a `git grep -F required_capabilities` from the next capability feature would not surface that the completion clause is deliberately *not* one of them. The PRD's Decision Log (D1–D8) argues each of these once; this ADR is where the next editor of `ClaimRun`, `SetRunRunning` or the hold's park order actually looks.

## The invariants

### I1 — the contract is versioned, frozen with milestones, and never cardinality-exempt (D1)

`runs.completion_contract_version` is the **explicit legacy discriminator**: `NULL` means legacy and never enforced, non-`NULL` means interlocked (migration `00211_completion_contract.sql`). `CreateRun` stamps it to `1` **before the first claim** when `completion_interlock_rollout` is on — not only at approval — because stamping only at approval would make I2's hard claim clause vacuous for the plan-phase worker (D1 says this explicitly). The contract itself (`completion_contract`, one server-minted criterion `m<N>.c1` per frozen milestone, text = the milestone title) freezes atomically with `milestones_frozen` at the two existing freeze sites (`CreateApprovePlanInput`, `SetRunRunning`), idempotently and guarded by `completion_contract_version IS NOT NULL AND completion_contract IS NULL`.

`buildCompletionContract` (`api/internal/workersvc/completion_contract.go`) mints a contract with an empty-but-non-null `criteria: []` for a zero-milestone run rather than skipping the freeze — **an interlocked run is never exempted by milestone cardinality**, so a future "skip contracts under N milestones" shortcut would silently reopen a #1220-shaped hole for small runs specifically. `computeUnmetCriteria`'s split-state branch (contract `NULL` while frozen, a corrupt/never-frozen row) is FAIL-CLOSED: it returns every frozen milestone ID as unmet rather than treating an unreadable contract as satisfied.

### I2 — the completion-protocol `ClaimRun` clause is non-bypassable by construction (D2)

The clause lives in `ClaimRun`'s own `WHERE` (`api/internal/store/queries/runtime.sql`):

```sql
AND (r.completion_contract_version IS NULL
     OR 'completion_interlock_v1' = ANY(@worker_protocol_caps::text[]))
```

with a byte-identical mirror in the fleet-spread peer-eligibility subquery (`p.protocol_capabilities`), so spread can never defer an interlocked run to a peer that could never claim it. Both are **dedicated, standalone predicates deliberately outside**:

- `fn_worker_can_claim` and `required_capabilities` — the ordinary capability match;
- `ClearRunRequiredCapabilities` — the owner override that can clear a run's entire required-capability set;
- the `capability_aware` kill-switch — which, when off, reverts ordinary capability matching to best-effort.

None of those three can authorize a worker that does not implement the completion protocol, because none of them is consulted by this clause. `workers.protocol_capabilities` is a **separate column and vocabulary** from `workers.capabilities` (`capability.CompletionInterlockV1`, filtered by `capability.FilterProtocol` at register) specifically so a protocol string can never leak into the user-facing repo-capability picker or be satisfied by a scheduler capability of the same name. A legacy run (`completion_contract_version IS NULL`) is unaffected — claimable by any worker, exactly as before.

### I3 — the completion permit is claim-fenced, idempotent, and consumed only through one transaction (D4/D5)

`RequestCompletionPermit` (`api/internal/workersvc/completion_permit.go`) never trusts a worker's "milestones done" claim — there is none in `CompletionPermitRequest`. It applies the claim fence (`loadClaimedInterlockedRun`: the run must be this worker's live `running`/`awaiting_input` claim), matches the requested `contract_revision` against the run's **frozen** revision, and recomputes unmet criteria server-side. `UpsertCompletionPermit`'s `UNIQUE (run_id, contract_revision, head)` upsert makes a repeated, unchanged request return the **same permit** rather than minting a new one. Every denial (`stale_claim`, `not_interlocked`, `revision_drift`, `contract_not_frozen`, `missing_milestones`, `empty_head`) is **non-terminal** — the run keeps its status and the worker acts on the reason.

`completeRunWithPermit` runs the consume-and-complete as one pgx transaction (`SELECT run FOR UPDATE` → consume the exact-identity permit → `SetRunCompleted`), deliberately **not** an isolated SQL update, because later children (#1229, #1214) join generation activation to this same transaction. A `nil` `TxBeginner` is fail-closed: an interlocked run errors rather than completing non-atomically. A gated completion report without a matching unconsumed permit updates nothing and stays non-terminal; a retry after response loss is idempotent (the permit is already consumed, the run already `completed` — no double-fire of terminal automation). Legacy (non-interlocked) completion is byte-for-byte unchanged, routed around this whole path.

### I4 — the permit binds the exact final head, verified against the real PR head, across all three forges (D5)

`phasePublish`'s existing scan/alignment/push path is unchanged in this child — D5 deliberately does not move the large ADR-456 alignment finalizer. What changes is what happens **after** that path lands: the permit is requested (or reissued, if alignment rewrote the candidate) for `H`, the exact source-branch tip that path successfully produced, because structural milestone membership is head-independent. After the PR is created/adopted, the worker reads the PR's actual head SHA through a forge-neutral method implemented by all three drivers (`agent/src/forge.ts`, GitHub/GitLab/Forgejo). **A head mismatch invalidates the permit and the run cannot report completed; no `Closes #N` body is ever rendered without a currently-valid permit.** The interlocked MR/PR body is created **without** `Closes #N`, so a held, unverified-head MR can never carry a closing line — the create-then-verify hole (a human merge closing the issue on an unverified head) is structurally impossible, not merely repaired. The canonical `Closes #N` body is **added** — via the drivers' `updateMergeRequestDescription` — **only after** the PR head is verified to equal `H`, and the head is then **re-read once more** to bind that add to the verified head: a change in the read→add window (or an unreadable re-read) strips `Closes` back off and holds, so `Closes` is never left on a head the interlock did not confirm both before and after the write. If the add itself fails the run **holds** rather than reporting completion (the completion contract requires the merged MR to carry the closing line). **The adopted-MR contract (CodeRabbit !1254):** `createMergeRequest` can *adopt* a pre-existing MR/PR that already carries `Closes #N`, so a hold is only safe once the body has been positively rewritten to a non-closing variant. Every hold path therefore strips `Closes` (adding an unverified banner) and **requires that write to succeed**; if the strip fails, the run **fails closed** rather than parking a possibly-closing MR on an unverified head. A future forge driver that omits `updateMergeRequestDescription`, a finalize path that renders `Closes` before the head is verified, or a hold path that strips `Closes` best-effort, reopens exactly the class of hole #1220 was.

### I5 — a missing milestone returns to the SAME lead session, checkpoint-first, and calls the forge zero times (D3)

On a gated `signal_done`, the executor checkpoints the current work **before** any denial is emitted (`sdk-executor.ts`'s `turn.done` attempt loop). It compares the server-recomputed unmet set against the declaration; a non-empty unmet set re-prompts the **same** SDK session via the existing mid-loop follow-up injection — no session reset, and critically, **no forge call of any kind** while milestones remain unmet. The completion-attempt fingerprint is `(sorted unmet IDs, head, worktree fingerprint)`, distinct from the #281 prose-response fingerprint but sharing the same `STALL_LIMIT = 3`: three identical attempts (unchanged fingerprint) is what routes to the hold (I6), not an arbitrary iteration count. This is the exact mechanism the M6 regression (`agent/test/runner-completion-1220-regression.test.ts`) pins against run `b0ffd0ea`'s shape: frozen five, declared four, and the fixed path both names `m5` back to the same lead and asserts `calls.length === 0` on the forge's create-MR spy. The bundled mutation control (neutering the missing-milestone gate) demonstrates the regression barrier is real, not decorative — deleting the denial makes the create call fire with `Closes` again.

Before any completion attempt exists on a run, legacy stall/wall/budget outcomes are byte-for-byte unchanged — this whole apparatus only engages once a run has entered the completion protocol at least once.

### I6 — `SetRunCompletionHold` is a dedicated sibling of `SetRunPaused`, not a widening of it, with a fixed capture-then-park order (D6)

Exactly like [ADR-1190](1190-run-pause-invariants.md) rejected widening `SetRunPaused` to admit new source statuses, D6 rejected widening it to admit `awaiting_input`. `SetRunCompletionHold` is its own transition: it admits only an **owned, interlocked** run in `running` or `awaiting_input`, requires at least one recorded completion attempt, stamps `hold_reason='completion_blocked'` and `hold_captured_head`, clears the completion-question marker, and transitions to `paused` — reusing the `paused` **status** (so it inherits every one of ADR-1190's six status-list exclusions and three negative-guard protections for free) while remaining a **distinct write path** with its own admission guard.

The park order is fixed and must not be reordered by a later child:

1. reap the agent tree (before any credentialed capture git — REAP-BEFORE-GIT);
2. WIP-commit dirty work;
3. fetch into the worker-owned bare repo and positively verify its tracking ref covers the clone HEAD;
4. publish the checkpoint best-effort;
5. call `captureHoldContext()` (steps 2–4 above, in the shipped implementation, live inside this one function — `agent/src/runner.ts`);
6. call `SetRunCompletionHold` and require the **returned status literally `paused`** — the same positive-ACK contract the pause park uses, never `applied` alone;
7. only then mark the flight parked so normal cleanup runs.

**The preserve flags (`preserveRecoveryClone`, `preserveSession`) are set BEFORE any of this is attempted, and cleared on every non-parked exit path** — a failed WIP commit, a failed fetch-back, an unverifiable tracking ref, a hold request that errors, or a hold ACK that is not `paused`. This is the single mechanism behind the acceptance criterion "no hold releases/cleans the only Git work copy when capture or acknowledgement is uncertain": retain-by-default until a **positive** double proof (verified local capture AND a `paused` ACK), never retain-until-something-fails. `captureHoldContext` mirrors [ADR-1197](1197-transient-recovery-park.md)'s verified-capture template exactly (dirty check → WIP commit required → fetch → positive tracking-ref verification → best-effort remote publish) and the same correction that ADR records — a prior checkpoint is not sufficient; only a positively verified capture licenses a park — applies here unchanged.

### I7 — the served `budget_exhausted` steer is one-shot and survives an ordinary running report (D3)

`StampCompletionBudgetExhausted` is the sweep's complement, not its replacement: `SweepRunningTimeout` carves out exactly the rows past their wall budget with `completion_attempts > 0` and a live worker heartbeat (a **narrow**, live-worker-only exemption — a post-attempt run whose worker went stale is NOT protected and still follows the ordinary stale-worker requeue/fail path, so this carve-out can never become an unbounded hold on a dead worker). `StampCompletionBudgetExhausted` then arms exactly the complementary set with a one-shot `completion_budget_exhausted_at`, delivered to the worker on its next running-report ACK (the same channel `pause_requested` already rides), steering a live lead into the hold rather than leaving it to run forever.

**`SetRunRunning` must clear only `hold_reason`/`hold_captured_head` on the resume→running transition — it must never clear `completion_budget_exhausted_at`.** A regression here (fixed in `a32d3264`, caught by review before ship) had `SetRunRunning` unconditionally clearing the served flag; because `SetState`'s running arm calls `SetRunRunning` and *then* re-reads the row for the same ACK, the flag was disarmed before the worker could ever observe it — a live lead past its wall could never be steered into the hold at all. The flag's only legitimate clears are `SetRunCompletionHold` (the worker acted on it) and `ContinueCompletionDecision` (a new owner decision, D3's third clear) — never an ordinary heartbeat. `TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB` pins this by arming the flag through the real stamp query and asserting a subsequent running report leaves it set.

### I8 — one owner decision, one endpoint, and honest `same_worker_only` durability (D6/D7/D8)

`ContinueCompletionDecision` is the **only** decision this child supports (`{decision: continue, guidance?}`); the handler 400s anything else before calling in. It dispatches on exactly two windows: the **live** `awaiting_input` completion-question window (identified by the dedicated `completion_question_at` marker — never the old, imprecise "interlocked AND `completion_attempts > 0`" proxy, which could match an ordinary PRD #88 clarification on an interlocked post-attempt run and let a continue-decision resolve the *wrong* question), and the **paused** `hold_reason='completion_blocked'` window. The live window resumes in place (delivered as an `answer` the worker's `steering.awaitAnswer` can resolve, never a `follow_up`); the paused window resumes through `queued` via `ResumePausedRun`, deliberately leaving `hold_reason`/`hold_captured_head` **set** until the first accepted running report clears them (D7) — not prematurely in the resume itself, so a resumed-but-not-yet-running row still visibly carries why it was held.

The live window itself is time-boxed by `completion_hold_window_seconds` (claim-delivered, default 900s, `agent/src/runner.ts`'s `askCompletionQuestion`) — **deliberately separate** from `question_timeout_seconds` and from the `#88` `questionDeadlines`/`questionCounts` bookkeeping, and its expiry **resolves** `{outcome: "expired"}` rather than rejecting — this window has no fail-closed path and must never throw `REASON_QUESTION_TIMEOUT`; expiry always routes to the verified hold (I6), never to `failed`.

**Every one of these mechanisms works only on the current worker.** `captureHoldContext` returns an explicit `mode: "same_worker_only"` result, and #1226 does not claim otherwise: a same-worker resume recovers the verified local restore point and the retained SDK session; a resume that lands on a *different* worker (once the existing `WORKER_AFFINITY_CEILING`, default 2h, expires — the same ceiling [ADR-628](0628-cross-worker-resume-durability.md) already governs, not a new one this child invents) recovers only what the best-effort remote publish actually reached. The wire/UI/CLI surfaces render `hold_context = "unavailable(same_worker_only)"` and D8's copy states the limitation plainly rather than presenting the hold as durable cross-worker recovery. Durable cross-worker context export is #1228/#1229's job, not this child's, and the honesty of that gap is itself an acceptance criterion, not an implementation detail to quietly outgrow.

## Consequences

- A future ninth completion-decision kind (#1227's `partial`/`accept`) reuses the **same** `run_user_inputs.kind='completion_decision'` value and the **same** endpoint — it must not spawn a second control plane, per D7.
- A future semantic-completion child (#1230/#1231) fills the reserved `audit`/`finding_ids` slots the contract and permit wire shapes already carry; it must not need a second protocol.
- Widening the hard `ClaimRun` clause's vocabulary (a `completion_interlock_v2`) is additive to `workers.protocol_capabilities`, but the clause's *position* — outside `required_capabilities`/`ClearRunRequiredCapabilities`/`capability_aware` — must not move; that positioning is the whole point of I2.
- The park order in I6 is a checklist for the next hold-adjacent feature the same way ADR-1190's six status lists are: a later child changing capture, publish or the ACK contract should re-verify against this ADR, not re-derive the ordering from scratch.
- Cross-worker completion-context durability (I8's honest gap) remains open, tracked by #1228/#1229; until it lands, the UI/CLI copy in [docs/run-completion-hold.md](../docs/run-completion-hold.md) is the correct statement of what a hold actually recovers.
- The rollout switch (`completion_interlock_rollout`, default OFF) means every invariant above is currently **inert for existing runs**: it governs only newly created issue runs once an operator turns it on. This ADR describes the mechanism, not its current default-enabled state.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's Run lifecycle section (the `running → paused` completion-hold note) and its capability-aware eligibility bullet, per repo convention.
