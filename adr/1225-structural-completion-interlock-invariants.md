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

`runs.completion_contract_version` is the **explicit legacy discriminator**: `NULL` means legacy and never enforced, non-`NULL` means interlocked (migration `00211_completion_contract.sql`). `CreateRun` stamps it to `1` **before the first claim** when `completion_interlock_rollout` is on for a new, unseeded issue run on either Claude or Codex — not only at approval — because stamping only at approval would make I2's hard claim clause vacuous for the plan-phase worker (D1 says this explicitly). A cold settings read (no valid cache at all) is treated as the switch being off for this decision — no stamp — while a failed refresh that still has a valid cached value keeps stamping (or not) on whatever that cached value says, never falling back to the cold-read default just because one refresh attempt failed. The contract itself (`completion_contract`, one server-minted criterion `m<N>.c1` per frozen milestone, text = the milestone title) freezes atomically with `milestones_frozen` at the two existing freeze sites (`CreateApprovePlanInput`, `SetRunRunning`), idempotently and guarded by `completion_contract_version IS NOT NULL AND completion_contract IS NULL`.

`buildCompletionContract` (`api/internal/workersvc/completion_contract.go`) mints a contract with an empty-but-non-null `criteria: []` for a zero-milestone run rather than skipping the freeze — **an interlocked run is never exempted by milestone cardinality**, so a future "skip contracts under N milestones" shortcut would silently reopen a #1220-shaped hole for small runs specifically. `computeUnmetCriteria`'s split-state branch (contract `NULL` while frozen, a corrupt/never-frozen row) is FAIL-CLOSED: it returns every frozen milestone ID as unmet rather than treating an unreadable contract as satisfied. (This zero-milestone freeze is itself a `#1626` fix, and the mechanism that actually infers the empty list is `planMilestonesParam` (`api/internal/workersvc/milestones.go`), not `buildCompletionContract` — the freeze site only ever sees a milestone list already decided. `planMilestonesParam` is a fail-closed inference, precise enough that a future edit could break it silently: `[]` is inferred only on an interlocked issue run's FIRST plan-bearing report (a stored `plan_md` still `NULL`) whose milestones are absent; a stored non-`NULL` candidate is never downgraded BY OMISSION of the SAME plan — a later report that re-presents the stored `plan_md` (compared NUL-stripped, as it is stored) and omits milestones or sends an explicit `[]` keeps it (a re-presented gate) — while a revise that presents a DIFFERENT `plan_md` with no milestones resets a non-empty candidate to `[]` (issue #1626 review, F2), so no criteria from a superseded plan are frozen at approval; a later report that instead carries an INVALID list resets the candidate to `NULL` (the query writes the rejected candidate directly), which is exactly the sticky-rejection case below; a rejected list — the server's own validation, not the worker's intent — is STICKY: once a milestone-less report follows a rejected one, the candidate stays `NULL` beside the stored `plan_md`, even for a later explicit `[]`, so the run holds at finalize and the owner decides; the worker itself sends a partly or entirely malformed, or non-array, `milestones` value as a rejected list rather than as "no milestones"; a resume at the gate (a credential switch, a requeue while parked) replays the stored candidate rather than re-inferring it, for interlocked runs only; and the early claim-time `running` report an autopilot run sends before it has planned (no `plan_md` yet) never freezes anything. An earlier m1 review fix kept the prior candidate across such a revise, which let the contract require milestones the revised plan no longer listed and deny the permit for them; the F2 fix replaced it with the same-plan test above, since the worker omits an empty list and a changed `plan_md` is the only signal that the lead's breakdown was dropped rather than merely not re-sent. A seeded-plan run (PRD #209, `--plan-file`) never sends a plan-bearing report at all, so it is never stamped interlocked in the first place and none of this applies to it.)

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

`completeRunWithPermit` runs the consume-and-complete as one pgx transaction (`SELECT run FOR UPDATE` → consume the exact-identity permit → `SetRunCompleted`), deliberately **not** an isolated SQL update, because later children (#1214, and any reopened #1229) join generation activation to this same transaction. A `nil` `TxBeginner` is fail-closed: an interlocked run errors rather than completing non-atomically. A gated completion report without a matching unconsumed permit updates nothing and stays non-terminal; a retry after response loss is idempotent (the permit is already consumed, the run already `completed` — no double-fire of terminal automation). Legacy (non-interlocked) completion is byte-for-byte unchanged, routed around this whole path.

**The worker must learn `contract_revision` from more than the claim response, or a first-claimed-before-approval run can never resolve its own identity (#1626 fix).** The contract freezes at plan approval (I1), which can happen strictly after a run's first claim — a lead that claims once during planning and only later gets its plan approved saw no further claim response to learn the frozen `contract_revision` from, and held at finalize with "completion identity unresolvable" even though the contract had since frozen cleanly. The worker now also reads `completion_revision` off the `RunDTO` returned by the ordinary `/state` acknowledgement (the same channel that already carries `pause_requested` and the budget-exhausted steer, I7), so a run that freezes its contract after its one and only claim still has a live channel to learn the revision it needs to request a permit against.

**Additive permit-scope authority (#3089).** A granted worker permit carries `completion_scope` from the same locked run revision that authorized it, including `{}` when there is no decision. The worker uses that scope for partial/accept rendering, title, body, `Closes` and finalization guards, rather than a stale claim snapshot. With a new worker and old API, omission at exactly the claim revision (default 1 when absent) retains the claim-scope fallback; omission at any different revision fails closed into the existing hold without opening a PR. A new API with an old worker retains the old worker's behavior: it ignores the additive field, so live-window decisions can still render claim-time scope. Both API and worker upgrades are required for the permit-scope guarantee. This adds no capability, state, endpoint, migration or claim field and does not expand decision admission; the claim fence, exact-head identity and consume-and-complete transaction above remain intact.

### I4 — the permit binds the exact final head, verified against the real PR head, across all three forges (D5)

Historically, #1226 left `phasePublish`'s scan/alignment/push path unchanged; #3090 adds the pre-push guard described in the dated addendum below — D5 deliberately does not move the large ADR-456 alignment finalizer. What changes is what happens **after** that path lands: the permit is requested (or reissued, if alignment rewrote the candidate) for `H`, the exact source-branch tip that path successfully produced, because structural milestone membership is head-independent. After the PR is created/adopted, the worker reads the PR's actual head SHA through a forge-neutral method implemented by all three drivers (`agent/src/forge.ts`, GitHub/GitLab/Forgejo). **A head mismatch invalidates the permit and the run cannot report completed; no `Closes #N` body is ever rendered without a currently-valid permit.** The interlocked MR/PR body is created **without** `Closes #N`, so a newly created, unverified-head MR carries no worker-authored closing line. This prevents the worker's create-then-verify hole; an already-open adopted MR can still be merged before a resumed guard runs (see the #3090 residuals below). The canonical `Closes #N` body is **added** — via the drivers' `updateMergeRequestDescription` — **only after** the PR head is verified to equal `H`, and the head is then **re-read once more** to bind that add to the verified head: a change in the read→add window (or an unreadable re-read) strips `Closes` back off and holds, so `Closes` is never left on a head the interlock did not confirm both before and after the write. If the add itself fails the run **holds** rather than reporting completion (the completion contract requires the merged MR to carry the closing line, except for an explicit issue-only `non_closing` declaration: that delivery still requires the permit, both exact-head checks, and a confirmed non-closing body). **Amendment (2026-09-28, PRD #1798, see [ADR-1798](1798-pr-description-owned-blocks.md)):** "fails" here is narrower than any add attempt going wrong. Specifically, if the add cannot be *confirmed* — the write itself fails, the confirming re-read fails, or the PR's head moves between the write and the re-read — uzi strips `Closes` back out. The run **holds** once that strip is *confirmed*, and also holds when the strip could not be confirmed only because the MR itself was unreadable (the strip then falls back to a *blind* whole-body non-closing rewrite, which carries no closing directive by construction even though nothing confirmed it landed). The run fails closed (`failInterlockedClosed`) only when the strip write is refused outright, or is written but a confirming re-read shows it did not land — a hold is a nominally non-closing parked state uzi cannot vouch for in either of those two cases. **The adopted-MR contract (CodeRabbit !1254):** `createMergeRequest` can *adopt* a pre-existing MR/PR that already carries `Closes #N`, so a hold is only safe once the body has been positively rewritten to a non-closing variant. Every post-publication hold path that cleans up an adopted request therefore strips `Closes` (adding an unverified banner) and **requires that write to succeed**; if the strip fails, the run **fails closed** rather than parking a possibly-closing MR on an unverified head. A future forge driver that omits `updateMergeRequestDescription`, a finalize path that renders `Closes` before the head is verified, or a hold path that strips `Closes` best-effort, reopens exactly the class of hole #1220 was.

### I5 — a missing milestone returns to the SAME lead session, checkpoint-first, and calls the forge zero times (D3)

On a gated `signal_done`, the executor checkpoints the current work **before** any denial is emitted (`sdk-executor.ts`'s `turn.done` attempt loop). It compares the server-recomputed unmet set against the declaration; a non-empty unmet set re-prompts the **same** SDK session via the existing mid-loop follow-up injection — no session reset, and critically, **no create-MR call** on that denied completion-attempt path while milestones remain unmet. This does not promise that earlier checkpoint publication or an already-open MR cannot affect the forge. The completion-attempt fingerprint is `(sorted unmet IDs, head, worktree fingerprint)`, distinct from the #281 prose-response fingerprint but sharing the same `STALL_LIMIT = 3`: without an explicit owner request, three identical attempts (unchanged fingerprint) route to the completion question and, on expiry, the hold (I6), not an arbitrary iteration count. This is the exact mechanism the M6 regression (`agent/test/runner-completion-1220-regression.test.ts`) pins against run `b0ffd0ea`'s shape: frozen five, declared four, and the fixed path both names `m5` back to the same lead and asserts `calls.length === 0` on the forge's create-MR spy. The bundled mutation control (neutering the missing-milestone gate) demonstrates the regression barrier is real, not decorative — deleting the denial makes the create call fire with `Closes` again.

Additive routing (#3089): on an interlocked, noninteractive issue run on either harness, `signal_done({owner_decision_requested:{reason:string}})` can open that same question at the first authoritative unmet result without waiting for the stall threshold. The request object admits only `reason`, trimmed to 1–500 Unicode code points by validation (invalid or extra keys are refused, never truncated). It supplies no scope authority and does not block finalization when the authoritative unmet set is empty. The fingerprint, iteration/wall budgets, deadline, cancellation/pause/completion-budget bounds and I6 capture-before-park ordering remain unchanged; requesting the window resets none of them. The durable question labels and escapes the lead reason as untrusted display text and uses server-provided unmet IDs. Only the existing decision endpoints apply continue/partial/accept, within their existing live completion-question or paused completion-blocked windows; ordinary clarification and plain pause remain ineligible. Ordinary answers, repository docs and `non_closing` carry no scope authority. After any answer the worker asks for a fresh authoritative `signal_done` recheck, without enforcing the old unmet list or inferring a formal decision from prose.

Before any completion attempt exists on a run, legacy stall/wall/budget outcomes are byte-for-byte unchanged — this whole apparatus only engages once a run has entered the completion protocol at least once.

### I6 — `SetRunCompletionHold` is a dedicated sibling of `SetRunPaused`, not a widening of it, with a fixed capture-then-park order (D6)

Exactly like [ADR-1190](1190-run-pause-invariants.md) rejected widening `SetRunPaused` to admit new source statuses, D6 rejected widening it to admit `awaiting_input`. `SetRunCompletionHold` is its own transition: it admits only an **owned, interlocked** run in `running` or `awaiting_input`, requires at least one recorded completion attempt, stamps `hold_reason='completion_blocked'` and `hold_captured_head`, clears the completion-question marker, and transitions to `paused` — reusing the `paused` **status** (so it inherits every one of ADR-1190's six status-list exclusions and three negative-guard protections for free) while remaining a **distinct write path** with its own admission guard.

The normal park order is fixed and must not be reordered by a later child.
The #3090 pre-push exception skips remote checkpoint publication and permits
bounded degraded capture with local retention, as recorded in the dated
addendum below; it does not relax the prior-attempt guard above:

1. reap the agent tree (before any credentialed capture git — REAP-BEFORE-GIT);
2. WIP-commit dirty work;
3. fetch into the worker-owned bare repo and positively verify its tracking ref covers the clone HEAD;
4. publish the checkpoint best-effort;
5. call `captureHoldContext()` (steps 2–4 above, in the shipped implementation, live inside this one function — `agent/src/runner.ts`);
6. call `SetRunCompletionHold` and require the **returned status literally `paused`** — the same positive-ACK contract the pause park uses, never `applied` alone;
7. only then mark the flight parked so normal cleanup runs.

**The preserve flags (`preserveRecoveryClone`, `preserveSession`) are set before capture.** Ordinary capture exhaustion clears them; a definitive hold refusal permits terminal cleanup. An ambiguous hold delivery retains the clone and session and ends non-terminal, without claiming a confirmed park. A literal `paused` ACK or the existing fenced ownership probe confirms the hold. `captureHoldContext` retains [ADR-1197](1197-transient-recovery-park.md)'s verified-capture sequence (dirty check → WIP commit → fetch → positive tracking-ref verification → best-effort remote publish); a prior checkpoint is not proof of the latest capture. The #3090 addendum qualifies that template only for new pre-push holds: exhausted capture can request the existing empty-head degraded hold, preserving live work without claiming a verified recovery copy.

### I7 — the served `budget_exhausted` steer is one-shot and survives an ordinary running report (D3)

`StampCompletionBudgetExhausted` is the sweep's complement, not its replacement: the wall-park sweep — [PRD #1497](../prds/1497-park-at-wall.md) replaced the single `SweepRunningTimeout` statement with two, `RequestWallParks` and `ParkRunsAtWall` — carves out exactly the rows past their wall budget with `completion_attempts > 0` and a live worker heartbeat (the same **narrow**, live-worker-only exemption #1226 always had: a post-attempt run whose worker heartbeat has gone stale is NOT protected by it, and is parked by `ParkRunsAtWall`'s own stale-worker branch instead — that statement runs ahead of the ordinary `RequeueRunsOfStaleWorkers` path in the sweep precisely so this carve-out can never become an unbounded hold on a dead worker; it is parked, not failed, either way). `StampCompletionBudgetExhausted` then arms exactly the complementary set — a live, post-attempt row — with a one-shot `completion_budget_exhausted_at`, delivered to the worker on its next running-report ACK (the same channel `pause_requested` already rides), steering a live lead into the hold rather than leaving it to run forever.

**The deadline relationship #1497 corrected.** `StampCompletionBudgetExhausted`'s own deadline now mirrors the wall-park statements' three-term total — the base wall budget plus `budget_extension_seconds` plus `budget_finalize_seconds` (`budget_paused_seconds` added where the site adds it) — rather than the two-term total it shipped with, which omitted `budget_extension_seconds`. That prior mismatch meant an extended post-attempt run could be steered into the completion hold at its original, un-extended deadline; matching the sweep's deadline (and keeping the same live-heartbeat carve-out) closes that gap. It is a deadline **match**, not a byte-for-byte duplicate of the sweep's SQL: `StampCompletionBudgetExhausted` still only arms the completion-hold flag on the complementary (post-attempt, live-heartbeat) set, while `RequestWallParks`/`ParkRunsAtWall` still request and execute the park on everyone else.

**`SetRunRunning` must clear only `hold_reason`/`hold_captured_head` on the resume→running transition — it must never clear `completion_budget_exhausted_at`.** A regression here (fixed in `a32d3264`, caught by review before ship) had `SetRunRunning` unconditionally clearing the served flag; because `SetState`'s running arm calls `SetRunRunning` and *then* re-reads the row for the same ACK, the flag was disarmed before the worker could ever observe it — a live lead past its wall could never be steered into the hold at all. The flag's only legitimate clears are `SetRunCompletionHold` (the worker acted on it) and `ContinueCompletionDecision` (a new owner decision, D3's third clear) — never an ordinary heartbeat. `TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB` pins this by arming the flag through the real stamp query and asserting a subsequent running report leaves it set.

### I8 — one owner decision, one endpoint, and honest `same_worker_only` durability (D6/D7/D8)

`ContinueCompletionDecision` was the only decision this child (#1226) shipped (`{decision: continue, guidance?}`); #1227 has since added `partial` and `accept` on the same endpoint (see the Consequences section below), so the handler now 400s only a value outside that three-way domain. It dispatches on exactly two windows: the **live** `awaiting_input` completion-question window (identified by the dedicated `completion_question_at` marker — never the old, imprecise "interlocked AND `completion_attempts > 0`" proxy, which could match an ordinary PRD #88 clarification on an interlocked post-attempt run and let a continue-decision resolve the *wrong* question), and the **paused** `hold_reason='completion_blocked'` window. The live window resumes in place (delivered as an `answer` the worker's `steering.awaitAnswer` can resolve, never a `follow_up`); the paused window resumes through `queued` via `ResumePausedRun`, deliberately leaving `hold_reason`/`hold_captured_head` **set** until the first accepted running report clears them (D7) — not prematurely in the resume itself, so a resumed-but-not-yet-running row still visibly carries why it was held.

The live window itself is time-boxed by `completion_hold_window_seconds` (claim-delivered, default 900s, `agent/src/runner.ts`'s `askCompletionQuestion`) — **deliberately separate** from `question_timeout_seconds` and from the `#88` `questionDeadlines`/`questionCounts` bookkeeping, and its expiry **resolves** `{outcome: "expired"}` rather than rejecting — this window has no fail-closed path and must never throw `REASON_QUESTION_TIMEOUT`; expiry always routes to the verified hold (I6), never to `failed`.

**Every one of these mechanisms works only on the current worker.** `captureHoldContext` returns an explicit `mode: "same_worker_only"` result, and #1226 does not claim otherwise: a same-worker resume recovers the verified local restore point and the retained SDK session; a resume that lands on a *different* worker (once the existing `WORKER_AFFINITY_CEILING`, default 2h, expires — the same ceiling [ADR-628](0628-cross-worker-resume-durability.md) already governs, not a new one this child invents) recovers only what the best-effort remote publish actually reached. The wire/UI/CLI surfaces render `hold_context = "unavailable(same_worker_only)"` and D8's copy states the limitation plainly rather than presenting the hold as durable cross-worker recovery. Durable cross-worker context export is #1228's job (#1229 is not planned), not this child's, and the honesty of that gap is itself an acceptance criterion, not an implementation detail to quietly outgrow.

## Consequences

- A future ninth completion-decision kind (#1227's `partial`/`accept`) reuses the **same** `run_user_inputs.kind='completion_decision'` value and the **same** endpoint — it must not spawn a second control plane, per D7.
- A future semantic-completion child (#1230/#1231) fills the reserved `audit`/`finding_ids` slots the contract and permit wire shapes already carry; it must not need a second protocol.
- Widening the hard `ClaimRun` clause's vocabulary (a `completion_interlock_v2`) is additive to `workers.protocol_capabilities`, but the clause's *position* — outside `required_capabilities`/`ClearRunRequiredCapabilities`/`capability_aware` — must not move; that positioning is the whole point of I2.
- The park order in I6 is a checklist for the next hold-adjacent feature the same way ADR-1190's six status lists are: a later child changing capture, publish or the ACK contract should re-verify against this ADR, not re-derive the ordering from scratch.
- Cross-worker completion-context durability (I8's honest gap) remains an accepted gap: #1229 was closed as not planned (2026-10-15) and #1228 stays a separate open primitive. The UI/CLI copy in [docs/run-completion-hold.md](../docs/run-completion-hold.md) is the correct statement of what a hold actually recovers.
- The rollout switch (`completion_interlock_rollout`) is **on by default** for a newly created, unseeded `issue` run on either Claude or Codex; no `app_settings` row resolving to that new default is required, only an explicit `false` row turns it back off, as an admin kill-switch (Admin → Instance settings → **Completion check**). A run created before this default flipped is unaffected — the switch is read once, at `CreateRun`, never retroactively against an existing row. A seeded-plan run (PRD #209, `--plan-file`) is likewise never stamped: it never sends the plan-bearing report the stamp's contract-freeze depends on, so an interlocked seeded run would hold at finalize on every completion. This ADR describes the mechanism; it is not the place to look for whether a *specific* run was interlocked — read that run's own `completion_contract_version`.

## Addendum — Codex completion attempts (2026-09-25, #1627)

A new, unseeded Codex issue run is stamped before its first claim under the same rollout switch and freezes the same structural contract as a Claude run. The Codex executor records a completion attempt, checkpoints work, and returns unmet milestones to the **same Codex thread** for another attempt. Repeated attempts without progress enter the same owner question and recoverable completion hold; a closing PR still requires the exact-head permit. The cold settings-read fallback and the seeded-plan exclusion above remain in force.

For an interlocked Codex run, `ClaimRun` requires both the shared `completion_interlock_v1` protocol and `codex_completion_interlock_v1`, in addition to the separate Codex harness capability. The Codex completion predicate is a dedicated clause outside `required_capabilities`, `ClearRunRequiredCapabilities`, and `capability_aware`; those paths cannot bypass it. Fleet-spread peer eligibility mirrors the clause so a run is not deferred to a peer that cannot claim it. This guards the Codex completion loop even when an older worker advertises the shared completion protocol.

## Addendum — non-closing publication and pending acceptance (2026-10-18, #3090)

This amendment qualifies the original #1226 guarantees without changing I6's
prior-attempt admission requirement. User recovery steps live in
[Completion holds](../docs/run-completion-hold.md) and
[Recovering unpublished work](../docs/run-recovery.md#completion-publication-holds).

### Scope authority and honest delivery

Plans should isolate owner/deployment acceptance in a final acceptance-only
milestone. Implementation, docs, tests, mandatory gates, security and review
remain worker obligations. Plan approval and auto-approval do not waive them.
An ordinary answer and a `non_closing` declaration supply no scope authority.
Formal owner partial records the exact kept/deferred milestone IDs and reason;
it is equally applicable to ordinary coding work.

The worker-owned partial block renders each deferred entry unchecked, with
its ID, frozen title and reason and an explicit not-delivered/not-verified
boundary before owner closure. Checkbox edits authorize nothing; refresh
restores unchecked entries. Actionable next steps belong in the ordinary
summary outside that block. Accept is accept-as-is: the publication warning
continues to name the unmet criteria and reason; unperformed acceptance is
not rendered as passed. Accept is refused when all milestones are done.

Partial/accept irreversibly alter THIS frozen contract. A separately
authorized, explicit approved milestone reconciliation reset can replace it,
clearing scope ceilings and completion decisions. Stored non-closing intent
survives that reset. No ordinary answer, plan approval or stored publication
intent substitutes for that authority.

### Pre-push selection and post-push authority

The source-issue closing-commit guard applies to declared non-closing delivery
on any issue run. On an interlocked issue it additionally applies to owner
partial and Stop-capped delivery. Both scans walk outgoing default-branch..tip,
including merge commits, earlier-flight history, alignment bridge commits and
an adopted own branch: once before alignment/push and again on the exact
candidate immediately before push. Full and accept-closing delivery use the
normal closing path. This **reverses #2815's declared-only exception**:
interlocked partial/Stop with earlier closing commits now holds before push.

Both harnesses' agents are instructed to avoid source-issue closing commit
messages; the worker owns PR-body publication. The enforcement boundary is
the issue-publication path in `agent/src/runner.ts` (`closingCommits`,
`refusePrepush`, and the exact-candidate callback), not arbitrary agent text
or every forge sink.

After existing prechecks, the worker makes one owned publication-start
`running` report. The client strictly decodes a positive safe revision and a
validated, deduplicated, sorted deferred-ID projection. The ACK must be
applied, identify this run and registered worker, and bind the positive
sent claim generation to the current flight. This projection selects the
guard; it is not completion authority. An unknown running projection holds
using fixed diagnostic copy without commit content. A non-running ACK takes
the quiet stale-claim stop; transport errors retain existing finalize-failure
handling.

The normal permit is still requested **after push**, not earlier, and adds no
new ledger. Its revision must equal the publication-start ACK revision and
its resolved deferred IDs must be set-equal to that projection. The granted
full `completion_scope`, from the same locked run revision, remains authority
for rendering and finalization. I3's old-API omission fallback is restricted
to EXACT claim revision (default 1), not merely a revision that is not newer.
Scope corruption outside supported writers is not detected by this pre-push
projection; the later permit comparison fails closed.

### Hold, attempts and capture

An interlocked pre-push refusal holds rather than fails unless hold refusal
is definitive. Accepted and ambiguous hold outcomes emit no failed terminal
report. Work is preserved; closing-message diagnostics show at most five
validated offending SHAs plus a remaining count, or an unreadable result
without SHA. Raw commit text is not diagnostic output.

If the publication-start ACK reports zero attempts or an unknown count,
the worker sends one logical empty completion attempt with bounded transient
retries (at most three sends). Each accepted send can increase the real count
and replace latest-attempt metadata, so ambiguous retries can increment more
than once. The empty declaration changes no completed progress, scope,
stall fingerprint or budget. An exhausted or uncertain attempt still
requests the hold; a stale claim stops the flight. **I6 still requires a
previously recorded attempt**; there is no zero-attempt hold exception.

This pre-push path skips remote checkpoint publication during finalize
capture. After bounded capture exhaustion it may enter a degraded hold
with empty captured head, retaining clone/session rather than claiming a
verified copy. Latest work may exist only on the worker. This qualifies I6's
normal verified-capture sequence; it grants no custody release or deletion
authority and does not claim remote durability.

### Stored intent and mixed versions

Migration `00343_completion_non_closing.sql` adds one nullable
`runs.completion_non_closing_reason`. A completion attempt accepts
`non_closing: {reason}`; the server strips NUL, trims, and validates 1–500
Unicode code points, refusing invalid input rather than truncating it.
The existing owned, interlocked claim and issue-kind fences apply. The
attempt write in `api/internal/store/queries/runtime.sql` uses COALESCE to
retain the first stored value. Claim configuration returns `non_closing`;
the feature is `completion_non_closing_v1`.

The first reason persists across resume, reclaim, hold, owner decision and
reconciliation. It is conservative publication intent, never a scope change,
criterion satisfaction or waiver. Both harnesses carry this flight's latched
declaration on every completion attempt, including unmet attempts. Publication
uses the current flight's declaration, otherwise restored claim intent.
The lead must redeclare on each `signal_done`. A declaring signal re-prompted
before its attempt is not durable until the next recorded attempt; a park,
reclaim or hold before that attempt can lose the in-memory declaration.

Workers send the new attempt field only with feature support. Without it,
a new declared pre-push refusal follows the legacy terminal path rather than
holding with intent the API cannot retain. Existing executor/post-push holds
are unchanged and can lose intent. Old workers ignore claim intent, so
coordinated API/worker rollout is necessary. Retain the nullable column on
application rollback. The additive ALTER TABLE takes a table lock but has
no backfill, default or index.

### Owner recovery and explicit residuals

For an unpublished declared/owner-partial history refusal without Stop,
owner continue guidance can name the SHAs and ask the lead to reword its own
unpublished commits. The worker never rewrites history or force-pushes.
For published history or Stop, cancel and manually deliver preserved work
through existing recovery. Stop resumes directly into finalization with no
lead turn; partial/accept cannot repair history. Plain resume is refused for
`completion_blocked`; formal decide or an eligible set-token switch can
resume. Stop is a running owner directive, wall parking follows #1497, and
provider-policy parking under #3068 remains planned.

Legacy declared refusal remains terminal. Legacy scope caps retain title/body
protection but can still close through commit messages. `mr_rework` and
branch-adopting `ci_fix` are excluded from this issue-publication guard.
Forgejo/Gitea any-branch closing concerns branch pushes; checkpoint refs
`refs/uzi-checkpoints` and held refs `refs/uzi-held` are not branches.
The excluded lanes and this Forgejo/Gitea branch-push residual are recorded
together via finding handoff #3b56e6e5-4352-4694-ae1c-4999d5719e9c.
That identifier is a finding ID, not a forge issue number.

Already-published history cannot be unpushed; an already-open PR may merge
before a resumed guard runs. Claim loss accepted between ACK and push also
remains: there is no API fence around the Git push itself.

#1230 D4 and #1231 remain open. This change introduces no plan-time
preauthorization or semantic activation. Live deployment acceptance has not
been performed; mocked tests and PR existence establish neither that
acceptance nor full end-to-end coverage.

## Semantic default-on amendment (2026-10-17, #1230/#1231; status updated 2026-10-20)

Two statuses are kept apart here. **Dormant-implemented by #1230** means the code exists on `agent/issue-1230` and is exercised only by tests that inject the path; production enrollment is off, no production code writes a `semantic_planning` row, and a real run stays structural or legacy. **Still planned (#1231)** means nothing in that list exists yet. The accepted structural invariants above stay in force except where this amendment names a supersession; current user docs remain the runtime authority.

### Coverage transport limitation

Codex currently lacks an enforceable token cap. Claude's output cap alone does not prove a conservative full-call bound. Complete large sources can exceed the 40,000-token allowance. Unsupported or unbounded transports and inadmissibly large inputs yield unavailable without a model call. #1231 must establish enforceable bounds before activation.

Mechanism: `reviewPlanContract` (`agent/src/plan-contract-review.ts`) runs only through an explicit `BoundedCoverageTestTransport` (`kind: "bounded-test"`), which a test injects and no shipped worker path supplies. Without one the review returns `coverage_unavailable` before any model call, and an input whose conservative upper bound reaches the 40,000-token allowance returns `coverage_exhausted` before any call. Both settle as coverage-unavailable at the human gate (see [Autopilot](../docs/autopilot.md#semantic-runs-park-instead-of-auto-approving-dormant)).

### Dormant-implemented by #1230 (production off)

- **Source-backed criteria and coverage review.** Bounded source snapshots, server-minted criterion IDs and kinds (worker and maintainer), a tool-less coverage review at the exact approval decision point (not each intermediate Plan cross-check revision), and the plan-gate rendering and exact-approval freeze. Coverage stays bounded to at most three reviews, two invocations per review, and 15 minutes and 120,000 tokens per run, independent of Plan cross-check and of #1231's audit budget.
- **Autopilot fallback.** A semantic-profile run auto-approves only with the server's single coverage authority reporting complete, the current checker APPROVE when Plan cross-check is required, and no maintainer criteria. Otherwise it parks in `awaiting_approval` through the existing `SetRunAwaitingApproval` path with a validated `semantic_park` reason, latched one-way for the execution. Detail and the ADR-2149 provenance rule: [ADR-2149 amendment](2149-cross-check.md#1230-coverage-park-and-semantic-digest-amendment-2026-10-20).
- **Capabilities.** `cross_check_semantic_plan_v1` (hard claim clause on the plan-checker child of a semantic lead) and `completion_semantic_publication_v1` (hard claim clause on a semantic lead; also a negotiated API feature). `completion_non_closing_v1` alone is not semantic readiness, and neither capability implies #1231 audit readiness. Old workers keep claiming structural runs.
- **Maintainer outstanding proof (D4).** Only owner-approved maintainer criteria are excluded from worker requirements, and approval authorizes only the IDs and conditions shown. `completion_pending` (`version`, `contract_revision`, `criteria`) rides the claim, the state ACK and the permit; it feeds the existing early and exact-tip closing-commit guards as one more non-closing source without touching the legacy deferred-milestone set. The publication ACK projection must equal the permit's or publication holds; a missing API feature holds and so does the capped-empty path. Nothing marks a criterion satisfied.
- **Drift (D5) as a detector.** The server re-reads the current issue through the authorized forge seam and hashes it like the frozen source; the worker hashes frozen PRD sources at the exact SHA through trusted bare git. Outcomes are unchanged, changed, or unknown with a reason, with at most 32 observations kept and the dropped count recorded. Drift never holds, refuses, revises or satisfies anything.

### Still planned (#1231); none of this is implemented

- #1231 activates coverage and final audit together for new eligible unseeded issue runs on both harnesses. Instance `semantic_completion_rollout` defaults on; an explicit admin disable affects future runs only. Model/cost ceilings are instance-admin configured and frozen per run; repo overrides are deferred.
- Enrollment freezes inside the run-creation transaction, before the first claim, independent of the later detailed contract freeze. Semantic claims require `completion_audit_v1` plus existing structural/Codex/harness predicates outside ordinary overrides. Missing compatible capacity queues the semantic run and names the protocol; no fleet-based downgrade. Existing structural/legacy/seeded runs retain their declared enrollment, and an explicit structural disable remains respected.
- Both rollout keys use one settings snapshot; a failed refresh uses both cached values where valid. Without a valid cache, an unreadable snapshot refuses new unseeded issue creation retriably with zero writes; seeded/non-issue paths stay unchanged. This supersedes, for unseeded issue runs, the cold-read-as-off fallback recorded in I1 and the Codex addendum once #1231 ships. Supported rollback retains semantic-aware profile/claim/permit enforcement and changes only future enrollment. A pre-semantic API downgrade with semantic authority present is unsupported/prohibited; an old binary cannot enforce it. Preserve schema/data, never strip contracts, and pair activating API/worker releases with tested protocol evidence rather than mere tag existence.
- The semantic completion audit and permit-level enforcement of semantic verdicts, and the enforceable coverage-transport bounds above, are #1231's. Coverage and audit budgets stay independently bounded (default coverage 120k normalized tokens/three reviews, audit 100k/two verdict-bearing passes). Exhaustion/outage never certifies uncertain work. #1232 proves integrated rollout/rollback; it does not retire the admin kill-switch or promise unimplemented cross-worker provider-context portability (#1229 remains not planned).

Nothing here is live deployment evidence: no live setting, run profile or dispatch changed, and the injected-path tests establish neither hosted behavior nor model quality. Exact-diff peer and independent architecture review is pending.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's Run lifecycle section (the `running → paused` completion-hold note) and its capability-aware eligibility bullet, per repo convention.
