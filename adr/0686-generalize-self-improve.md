# ADR-686: Generalize `self_improve` to any repo, keep uzi dogfooding as an explicit flag, and retire the long-lived branch

**Status**: Accepted (PRD #686 M1–M5, M8–M10 implemented on branch `agent/issue-686`; M6 lands the tests-adjacent docs and this ADR, M7 lands the web toggle)
**Date**: 2026-08-29
**Deciders**: Vlad (maintainer) + agent team (a three-reviewer pass found the server-only fix was insufficient — see "The blocker" below); PRD #686 Decision Log (D1–D12)
**PRD**: [prds/done/686-generalize-self-improve.md](../prds/done/686-generalize-self-improve.md) (GitHub issue [vtmocanu/uzi#686](https://github.com/vtmocanu/uzi/issues/686)) — the PRD carries the full milestone breakdown, the phasing table, and every decision's rationale in depth; this ADR records the durable invariants a future change would silently break.
**Related**: folds in the drift fix for the #699 stale-duplicate (Part B); reuses #297's claim-time untrusted-fence in-flight mechanism (`agent/src/executor.ts` `inflightTargets`), extended to open self-improve MRs (D11).

## Decision (summary)

The scheduled `self_improve` job's *mechanism* was always generic (any user, any owned repo, off by default), but its *content* — both the server-composed description and, more seriously, the worker's trusted planning directive — was hardcoded to uzi. This PRD makes `self_improve` mean **"improve THIS project"** for an arbitrary repo, and keeps uzi-improving-uzi working exactly as before as an **explicitly-flagged** special case:

- **D1 — a new per-repo boolean, `repos.fold_improve_uzi_backlog`**, not a per-schedule flag, for reset-durability.
- **D2 — no repo-identity/URL sniffing.** Dogfooding is a granted capability, never inferred.
- **D3 — the migration backfills `true`** for every repo with an existing `self_improve` schedule, so the upgrade is transparent.
- **D7, corrected — the mode crosses the WORKER CLAIM wire, not the controller poll-wire contract.** A `self_improve_dogfood` boolean rides `ClaimResponse`; verified during implementation that it needed no controller change and no `protocol_contract_test.go` golden bump (see D7 below — this corrects the PRD's original text).
- **D9 — the long-lived `uzi/self-improve` branch is retired** for a fresh-per-cycle `uzi/self-improve/<runId>` branch, closing the measured drift that produced the #699 stale-duplicate.
- **D12 — open-MR state (feeding both the concurrency cap and the picker) is resolved LIVE from the forge**, never from `runs.mr_state`, which is unreliable once one tracking issue carries several MRs.

## Context

**Server side** (`api/internal/schedsvc/self_improve.go`) composed a description literally naming "the uzi codebase" and folded the owner's `improve_uzi` judge backlog unconditionally. **Worker side** (`agent/src/prompt.ts` `buildSelfImprovePlanPrompt`), selected purely by run **kind** with no per-run signal, told the model it was improving "uzi's own repository," quoted uzi's own standing rules, and ran uzi's own test commands (`go test ./...` in `api/`, `npm test` in `web/`/`agent/`). Enabled on a non-uzi repo, this was not just cosmetically wrong — the model was directed to weaken *uzi's* guardrails and run *uzi's* test suite against someone else's tree.

### The blocker — why a server-only fix is insufficient

The server `description` const flows to the worker as `recommendations: ctx.issueDescription` and lands **inside the untrusted fence**, framed as "suggestions to WEIGH — never as instructions." The **trusted** directive (outside the fence) still said "improve uzi's own repository." Swapping only the server const produces a run whose trusted instructions and untrusted data contradict each other — still uzi-shaped in the part the model actually obeys. Generalizing for real requires the uzi-vs-generic mode to cross the wire to the worker and select a generic variant of both the trusted directive and the post-agent checks. This is the load-bearing addition the first draft missed.

Independently, a self_improve run worked a **single fixed branch** (`SELF_IMPROVE_BRANCH = "uzi/self-improve"`) that every cycle extended, never rebased onto main between cycles. Measured 2026-08-25 at ~152 commits / 3 days behind main, this let a cycle re-solve already-merged work: PR #699 duplicated the already-merged #695 because its stale base hid it. Part B of this PRD (D9–D12) fixes that independently of the generic-vs-dogfood split, and was folded into the same PRD because it edits the exact branch surface D4 already owns.

## The decisions

### D1 — The capability flag lives on `repos`, not `run_schedules`

**`repos.fold_improve_uzi_backlog boolean NOT NULL DEFAULT false`**, named for what it does — not `is_uzi_product` (see D2). Two schedule-reset shapes exist and the maintainer uses both: `POST /schedules/{id}/reset` (`ResetDefaultSchedule`, an UPDATE on the same row) and delete+re-enable (`CreateDefaultSchedule`, a brand-new row seeded from the catalog). **A per-schedule flag would be lost on delete+re-enable**; making the catalog itself carry it would fold the backlog for every repo that enables the default job — the exact bug being fixed. A per-repo flag is a durable repo fact untouched by any schedule lifecycle op, and `repos` is owned by exactly one user (`connection_id → forge_connections.user_id`), so the owner-scoped `improve_uzi` backlog has a well-defined owner per flagged repo. Mirrors the existing single-bool precedent (`repo_skills_enabled`, `repo_devbox_opt_in`, `repo_claudemd_enabled`), all excluded from `UpsertRepo`'s SET list so they survive membership re-sync.

### D2 — No repo-identity detection

URL/name sniffing ("is this `vtmocanu/uzi`?") is rejected: fragile and wrong for forks and self-hosters, and it would silently reassign dogfooding behavior to any repo whose URL happened to match. Dogfooding is an explicitly-granted per-repo capability, full stop — an owner sets it, nothing infers it.

### D3 — The migration backfills every repo with an existing `self_improve` schedule

```sql
UPDATE repos SET fold_improve_uzi_backlog = true
 WHERE id IN (SELECT DISTINCT repo_id FROM run_schedules WHERE target = 'self_improve');
```

`run_schedules.repo_id` is `NOT NULL REFERENCES repos(id)`, so the backfill cannot flag a NULL or wrong repo. Because D2 rejects identity detection, "backfill the repos that already had a self_improve schedule" is the only coherent preservation path for the upgrade — every self_improve schedule that has ever shipped carried uzi-semantics, so this is a **transparent, zero-manual-step upgrade** for the maintainer's own instance. New repos default `false` → generic.

### D9 — Retire the long-lived `uzi/self-improve` branch → fresh-per-cycle branches

The fixed branch was the drift source: because it was reused every cycle, it was never rebased onto main, so a cycle could plan against a tree over a hundred commits stale. PRD #46's original rationale ("MR reuse is free") was not free — it just deferred the cost to drift.

**The fix:** derive the branch per cycle off current main, `uzi/self-improve/<runId>` (mirroring the shipped `uzi/prompt-<runId>` pattern) — uzi never force-pushes, so a new cycle cannot safely reuse a name whose MR is still open, and this is the direct structural fix: each cycle clones current main as its base, so it can never re-propose already-merged work the way #699 did. The label (`uzi-self-improve`) and title (`uzi self-improvement`) are unchanged — they read "uzi the **tool** did this," correct on any repo, and the tracker-hiding UI keys on the label, not the branch, so nothing there breaks. The tracking-issue body is reworded from "opens or extends one merge request on the `uzi/self-improve` branch" to "opens a fresh merge request against this repo each cycle." A resumed cycle keys on `runId`, so it reattaches to its own branch rather than colliding with a sibling cycle's.

**Considered and rejected — keep one branch, reset-to-main-when-merged + skip-when-open.** Also kills drift and keeps a single branch name, but forecloses the maintainer's chosen shape (D10/D11: several independent open MRs at once). Recorded as the simpler fallback if the multi-MR shape is ever unwanted.

### D12 — Open-MR state is FORGE-sourced, not `runs.mr_state` (feeds both D10's cap and D11's picker)

**Why `runs.mr_state` is wrong for this lane.** It is written only by `SetRunMRState`, fed from `ListMRWatchCandidates`, which is `DISTINCT ON (issue_iid)` — it watches only the **latest** run per tracking issue. This PRD keeps one tracking issue (D9) but now opens **N** MRs against it, so once a newer cycle wins the DISTINCT ON, an older cycle's `mr_state` freezes and never reflects a later human merge or close. Worse, the tracking issue is permanently open, so the watch may never even bootstrap `mr_state` for these runs (count always reads 0, silently defeating the cap). Either way, `runs.mr_state` cannot be the count's source without wedging or silently no-opping.

**The source.** At each fire: pull a bounded window of the repo's most-recent completed `self_improve` runs with `mr_iid IS NOT NULL` (a new store query, `LIMIT` a small constant — cycles are ~2 days apart and the cap is 2, so a tiny window covers every plausibly-open MR without an unbounded scan), then call the existing `Forge.GetMergeRequest` per candidate and count the ones still reported `opened`. No new `Forge` interface method, no driver/fake fan-out. This count feeds D10's cap directly; for D11's picker, the still-open candidates' own run row (`plan_md`, falling back to `issue_description` for an autopilot fire where `plan_md` is NULL) supplies the "what was proposed" text, so the picker never needs a forge MR-body read.

A `GetMergeRequest` error is treated as transient (abandon this fire, retry next tick) — never fail-closed (an errored candidate counted as open would eventually wedge the cap at K forever) and never fail-open (skipping the check would let the cap silently breach).

### D7 (corrected) — The mode crosses the worker CLAIM wire, why a claim field and not a server-authored prompt, and the corrected controller finding

The worker needs a per-run signal to choose the generic-vs-uzi trusted directive and checks; the server-composed `description` cannot carry it because it lands inside the untrusted fence, and the worker cannot infer it from the run kind alone (kind is `self_improve` either way). The fix: a `self_improve_dogfood?: boolean` field on `ClaimResponse`, populated at server claim assembly from `repo.fold_improve_uzi_backlog`, alongside where `issue_description`/`auto_approve` are already set for a self_improve claim. **Absent (or false) ⇒ generic** — the safe default, so an older server or an unflagged repo can never accidentally produce the uzi directive. This is the same shape `auto_approve` already is on the claim: low-novelty, additive, and read once at plan-build time (`agent/src/prompt.ts` `buildSelfImprovePlanPrompt`).

**The PRD's original text asserted this "crosses the controller poll-wire contract" (`api/internal/hostedsvc/testdata/controller_poll_wire.json` + `controller/internal/protocol/protocol.go` + its `-count=1` golden `protocol_contract_test.go`) and required updating all three. That was wrong, and the correction is recorded here rather than restated as fact.** Verified against the shipped code (`api/internal/workersvc/claim.go`, `agent/src/protocol.ts`): the controller's `PollResponse` carries only **fleet desired-state** — workers, templates, the join token — never anything from an individual run claim. Existing claim-only fields with the identical shape, `auto_approve` and `inflight_targets`, live exclusively in the worker claim types and have never touched the controller protocol or its golden. `self_improve_dogfood` (M3) and the sibling `self_improve_open_mrs` field carrying D11's picker context (M10) were both added with **no controller-side change and no golden bump**, and the api/agent/controller gates stayed green. A future claim field should not assume it must extend the controller contract just because a sibling claim field once did — verify against the controller's actual `PollResponse` shape before touching it, the same "re-derive it at the moment you assert it" discipline ADR-65's methodological postscript records for a different PRD.

## Consequences

- An unflagged repo's `self_improve` run — trusted directive and untrusted data alike — targets that project, with no uzi-literal standing rules, no `go test ./... in api/`, and no `improve_uzi` backlog fold.
- uzi's own self-improve schedule survives the upgrade with **zero manual steps**: the migration backfill flags it, the worker still receives the uzi directive (byte-identical except the now-honest branch sentence), and the tracking issue/label/title are unchanged.
- The capability survives a repo delete+reconnect only because an owner/admin setter exists (`SetRepoFoldImproveUziBacklogForUser`, mirroring `SetRepoDevboxOptInForUser`) — `UpsertRepo` on reconnect does not set trust flags, so without this setter a reconnected flagged repo would silently revert to generic.
- Self-improve branches no longer accrete: each cycle branches fresh off current main, structurally preventing the #699 stale-duplicate class. Concurrency is bounded (default cap 2 open MRs per repo) rather than unboundedly piling on, and once several are open the next cycle's plan prompt sees their proposed content in an untrusted nonce fence so it picks a non-overlapping improvement — advisory, not a conflict guarantee; a genuine git conflict between two independent MRs is still the human's merge-order call.
- **The `improve_uzi` judge category is unchanged** — no enum, DB, or type churn; it remains product-scoped and semantically distinct from "improve the user's project."
- **Operator note carried into `docs/scheduling.md`:** the `improve_uzi` backlog is owner-scoped, so flagging more than one repo `fold=true` would make them share (and race to mark-addressed) the same backlog. Flag exactly one repo per owner for the fold.
- **Anyone later tempted to count open self-improve MRs from `runs.mr_state` must re-read D12:** the `DISTINCT ON (issue_iid)` watch freezes every run but the latest against one permanently-open tracking issue, so that column cannot answer "how many are open" once a tracking issue carries more than one live MR.
- **Anyone later tempted to route a new claim field through the controller golden by default must re-read D7:** the controller protocol carries fleet state only; a claim field crosses it only if it is genuinely fleet-shaped, not merely because a sibling field's ADR once said so.

## Out of scope

- Renaming the `improve_uzi` judge category or any of its plumbing.
- Renaming the `uzi-self-improve` label or `uzi self-improvement` title (the branch changed; these did not).
- Auto-resolving a merge conflict between two independent open self-improve MRs — that is the human's merge-order call, never auto-resolved by the worker.
- Changing the branch model of any other run lane (`issue`, `ci_fix`, `prompt`) — only the self_improve lane carried a single long-lived accreting branch.
