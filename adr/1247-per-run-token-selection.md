# ADR-1247: per-run credential override rides the existing claim generation, never the live claim

**Status**: Accepted (implemented, issue #1247, M1–M9; M10 docs in progress)
**Date**: 2026-09-17
**Deciders**: architect (design + Decision Log D1–D14); two review passes (2026-09-15, 2026-09-16) that folded the switch protocol, the fence and the attribution journal; coder (implementation).
**PRD**: [prds/1247-per-run-token-selection.md](../prds/1247-per-run-token-selection.md) — the full milestone breakdown, code anchors and Decision Log (D1–D14) live there; this ADR restates the four seams a future edit is most likely to break silently, and the one seam it deliberately leaves half-built.
**Related**: reuses [adr/1296-durable-run-recovery.md](1296-durable-run-recovery.md)'s custody `runs.claim_generation` as the switch fence rather than minting a second one; the pool ranking is [PRD #111](../prds/done/111-auto-select-anthropic-token.md)'s `autoselect.NextAvailable`/`autoChoice`; the per-leg usage fold this PRD extends is recorded in [adr/1079-run-usage-per-leg-fold.md](1079-run-usage-per-leg-fold.md).

## Decision (summary)

A run's owner can now choose the Anthropic credential for **one run** (or one scheduled job), overriding the worker's own binding, and can change that choice while the run is queued, parked, or even mid-flight. The design leans on four load-bearing seams, each easy to erode one edit at a time:

1. **The resolution order is a ladder in exactly one function** (D2): `self_improve`/judge binding, then the run's override, then the worker's bind mode — nowhere else decides this.
2. **A switch only ever takes effect at a claim boundary** (D3): no endpoint hands a live run a new token in place; the fence that makes this safe reuses the *existing* custody `runs.claim_generation` rather than adding a second notion of "which flight is this".
3. **Attribution is a journal with provenance stamped on the frames, not inferred after the fact** (D7): every claim gets an epoch row, every message frame carries the generation it was produced under, and the usage fold keys off that, not off the run's current token or a timestamp.
4. **Auto-failover while parked is a bounded, mode-gated re-evaluation** (D8): it only ever promotes a run whose *next* claim would resolve through the pool, and it never moves more than one run per owner per sweep tick.

A fifth item is not a seam to preserve but a **known incompleteness to not paper over**: the switch-state stamp's *successful-application* clear is deferred to issue #1422 (D14).

## Why this needs its own record

Every invariant below is either a decision this PRD reused rather than reinvented (the claim generation), or a boundary a future "just add a field" edit could quietly cross (a second place that resolves the credential, a mid-run token swap that bypasses the claim, a fold that reads the run's *current* label instead of the epoch that produced a leg, an auto-failover pass that also promotes pinned runs). None of these show up in a diff of "what changed" the way a new endpoint does — this ADR is where the next editor of `claimSecretID`, `assembleClaim`, or the usage fold actually looks.

## The seams

### Seam 1 — the ladder lives in exactly one function (D2)

`claimSecretID` (`api/internal/workersvc/secretchoice.go`) is the *only* place that decides which Anthropic credential a claim spends, in this order:

1. `self_improve` → the owner's **judge** binding (`judgeChoice`), checked first so the run-override rung below can never reach a `self_improve` claim.
2. **The run's own credential override**, if one is set — `pinned` (a `staticChoice` naming the pinned secret, reason `run_pinned`), `auto` (the same pool ranker the worker lane uses, `autoChoice`), or `default` (an explicit `secretChoice{reason: run_default}` — not `staticChoice(nil, …)`, which always reports plain `default` and would make a run-level default choice indistinguishable from an unset worker).
3. Otherwise, the claiming **worker's** own bind mode (`pinned`/`auto`/`default`), unchanged from before this PRD.

`openAnthropic` and `assembleClaim` never learn that an override exists — they open whatever `claimSecretID` names, exactly as they did before this PRD (PRD #104's R4: resolution lives in one place, or a wrong fallback spends the wrong account silently). A parallel, *read-only* policy — `effectiveNextClaimMode(run, ownerJudgeMode, worker)` — answers "what would the *next* claim resolve to" without performing a claim; it is the one function that Decision 6e's park-time check (Seam 4), the D8 duration pass, and the D6 warnings all call, specifically so none of them re-derives the ladder from `runs.anthropic_select_reason` (which describes the *previous* claim and goes stale the moment a binding changes while the run is parked).

**What would break silently:** adding a fifth rung to the ladder in `assembleClaim` or `openAnthropic` directly, instead of inside `claimSecretID`; or teaching `effectiveNextClaimMode` to read `anthropic_select_reason` instead of the live override + worker row. A mutation test that swaps the override and worker-bind rungs is expected to redden the ladder unit test — if that test does not exist for a future rung, add it alongside the rung.

### Seam 2 — a switch happens only at a claim boundary, fenced by the generation this PRD did NOT mint (D3)

No endpoint hands a running turn a new token in place. `agent/src/protocol.ts`'s claim payload stays "the only delivery path" for `anthropic_oauth_token` — a switch instead **quiesces the current flight, releases its claim, and lets the next claim deliver the new token**, so "mid-run" really means "at the very next claim after this one lets go."

The fence that makes a release-then-reclaim safe is **not a new column**. It is `runs.claim_generation` — the same counter `ClaimRun` already owns together with the recovery custody hold (migration `00223`, PRD #1349, [adr/1296-durable-run-recovery.md](1296-durable-run-recovery.md)) — plus one new column, `runs.claim_released_at`, that marks the window between "the old flight let go" and "a new claim has landed":

- **Request**: the verb stamps `credential_switch_requested_at` / `credential_switch_generation = runs.claim_generation`. Nothing else changes; the run stays exactly where it was.
- **Release**: the worker reports `credential_switch`; `ReleaseCredentialSwitch` is a *single*, exactly-fenced statement requiring `claim_generation = @gen AND claim_released_at IS NULL AND worker_id = @worker AND status IN (<held set>)` — a 0-row result is `ErrCredentialSwitchRaced`, never silently accepted. The gap is banked like a pause; the wall is not reset.
- **Fence**: every worker-driven mutating transition after that — a state report, `SetRunRunning`, `AppendMessages`, a heartbeat write — is predicated **atomically in SQL** on `claim_generation = @gen AND claim_released_at IS NULL`. A rejected write returns the typed `stale_claim` disposition, on which the *old* flight stops reporting rather than retrying into a claim it no longer owns.
- **Reclaim**: `ClaimRun` clears `claim_released_at` and increments the generation, exactly as it already did for every ordinary reclaim — this PRD adds no second increment path.

**Why reuse the custody generation instead of minting a new one**: a second "which flight is this" counter would need its own fencing story, its own interaction with the custody hold PRD #1349 already built, and its own answer to "which one wins when both disagree." Reusing `claim_generation` means a credential switch and a custody-driven recovery reason about the *same* number — a hold opened under generation 3 and a switch fenced at generation 3 are provably talking about the same flight.

**What would break silently:** a new worker-driven write path that forgets to carry `claim_generation` in its predicate (it would then apply after a release, to a claim nobody holds anymore); or a "quick fix" that lets the server push a token to a live claim directly, bypassing the release/reclaim cycle entirely — that reintroduces exactly the race this fence exists to close, and it would do so invisibly until two flights raced on the same run.

### Seam 3 — attribution is a journal with provenance on the frames, never inferred after the fact (D7)

`runs.anthropic_secret_id`/`label`/`select_reason` keep meaning exactly what they meant before this PRD: the credential of the *current* claim. What's new is a journal that survives past "current":

- **`run_credential_epochs (run_id, claim_generation, secret_id, label, select_reason, applied_at)`** — one row per claim, written by `recordRunCredential` right after a successful credential open. This is the fact "generation *G* of this run spent token *T*."
- **`run_messages.claim_generation`** — every canonical frame is stamped, server-side, with the generation it was produced under, by the same fenced append that Seam 2 already gates.
- **`run_usage.claim_generation`** — an *output* of the fold, never its evidence.

The incremental fold and a full `RefoldRunUsage` replay both attribute a leg to *its own* epoch by joining on the frame's stamped generation — never by reading the run's current `anthropic_secret_id`, and never by timestamp proximity. This is what makes "spend before a switch stays attributed to the old token after a replay or a refold" a property of the data, not of when you happen to look: a refold that ran a week after three switches still lands each leg on the epoch that actually produced it.

**Why not infer the generation from timestamps or the latest run row**: a refold would relabel every already-attributed leg onto whatever the run's *current* token happens to be, silently rewriting history on every replay. The leg key itself is unchanged by this PRD — only its epoch join is new.

**What would break silently:** a new message-producing path that skips the fenced append (and so never gets `claim_generation` stamped) — its usage would then fall through to whatever the fold's fallback does, misattributing spend across a switch boundary without any test catching it unless that path is specifically checked against a multi-epoch run.

### Seam 4 — auto-failover re-evaluation only ever helps a run whose next claim would use the pool, at most one per owner per tick (D8)

`reEvaluateParkedLimitWaitRuns` (`api/internal/workersvc/sweep.go`) runs immediately before `PromoteLimitWaitRuns` each sweep. For every still-parked `limit_wait` run it calls the *same* `effectiveNextClaimMode` from Seam 1 and **only** lowers the retry stamp when that mode is `auto` — a `pinned`, `default`, or `unknown` next claim is left alone, because promoting either would only re-park it on the exact token that just failed. The one-per-owner-per-tick bound (shared with the existing `resumePoolWaitRuns` guard) exists because this pass, unlike the original park-time check, bypasses the jitter that is the only thing that normally staggers a wave of runs off the same account — without the bound, every parked run on one owner's account would promote in the same tick and immediately thunder-herd the one token that just gained headroom.

Decision 6e (the original park-time lowering in `decideLimitPark`, `api/internal/workersvc/limitwait.go`) gets the identical fix for the identical reason: before this PRD it lowered the park from *any* pooled alternative even when the claiming worker was pinned or default-bound, so a promoted run could re-park on the very same account. It now gates on `effectiveNextClaimMode(...) == BindModeAuto` too, so park-time and duration-time share one predicate.

**What would break silently:** a future promoter that reads `runs.anthropic_select_reason` (the *previous* claim's reason) instead of `effectiveNextClaimMode` to decide whether to promote — the moment an owner repoints a worker or sets a per-run override while the run sits parked, that reason goes stale and a `pinned` run could be promoted as if it were still `auto`.

## The known incompleteness: the switch stamp's successful-application clear (D14, deferred to #1422)

`credential_switch_requested_at`/`credential_switch_generation` are meant to read three states over a switch's lifecycle: **requested** (stamped, not yet released), **released** (the worker let go, awaiting reclaim), and — once the reclaim actually lands and a fresh `run_credential_epochs` row is written for the new generation — cleared, because the switch has now been **applied**. That third transition, "clear the stamp exactly when the next epoch write proves the new token is live," is **not implemented in this landing**. Issue #1422 tracks it.

What **is** implemented, so this gap is a stale-read wart and never a stuck-run or a signal leak: every terminal transition (`SetRunCompleted`, `SetRunFailed`, `CancelRunServerSide`, and the rest of the eleven terminal writers) clears both switch columns alongside the pre-existing PRD #1190 pause-columns clear. So the only way to observe the stale state is a same-run release-then-reclaim on a run that is *still going* — the DTO can read `credential_switch: "released"` for a beat (or longer, if nothing has re-read and re-applied it) after the reclaim actually happened and the run resumed on the new token. There is no renderer for this today (`credentialOverrideCell`/`credentialSwitchText` render whatever the DTO says, so a caller would have to specifically compare `credential_switch` against `credential_epochs` to notice), and `ClaimRun` never filters on this stamp, so it cannot wedge a claim.

**A future implementer of #1422 should clear the stamp from inside the same write Seam 3 already makes atomic** — the epoch insert in `recordRunCredential`/`recordRunCredentialTx` — rather than adding a second, separately-fenced write: the epoch write already proves "this generation's credential is now live," which is precisely the fact D14 wants the clear to depend on.

## Consequences

- A future rung added to the credential ladder (a repo-level override, say) gets audited against Seam 1's single-function rule the same way the run override itself was audited against the worker bind mode.
- A future worker-driven write path is obligated to carry `claim_generation` in its predicate, the same way `SetRunRunning`/`AppendMessages`/every state report already do (Seam 2).
- A future usage-fold change is obligated to key on the frame's stamped generation, never on the run's current token (Seam 3).
- A future promoter (park-time or duration-time) is obligated to consult `effectiveNextClaimMode`, never `anthropic_select_reason` (Seam 4).
- Issue #1422 remains open; until it lands, a same-run release-then-reclaim can leave `credential_switch` reading `"released"` past the point the switch actually applied — cosmetic only, per the incompleteness section above.

## Linked from ARCHITECTURE.md

Linked from ARCHITECTURE.md's run-lane credential paragraph (the `claimSecretID` description, just above the Run lifecycle section), per repo convention.
