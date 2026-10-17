# PRD #1231: Independent completion auditor and semantic permit

**Issue:** [#1231](https://github.com/vtmocanu/uzi/issues/1231)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on completed #1230 and the actual merged #3089/#3090 authority/publication interfaces. #1226/#1227 and both-harness structural rollout #1626/#1627 are shipped. #1228 is not a semantic technical prerequisite; #1229 remains not planned.
**Priority:** Normal (current issue taxonomy).
**Execution:** Keep `Planned` without `uzi`. Dispatch only after #1230 and its integration prerequisites merge, main CI is green, and a fresh scope/architecture review verifies the real interfaces. This refresh is design-only: no live settings, existing-run changes or dispatch.

This child activates semantic completion auditing on top of #1226's structural permit and #1230's source-backed contract. Structured review blockers and governed MR rework are separate child #1233. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

Structural coverage cannot determine whether arbitrary acceptance criteria are truly satisfied. A lead can declare every milestone while part of one remains missing, as PR #1220 did for M3. Plan-time source coverage improves the contract but does not judge the final implementation.

Run a separate read-only completion auditor against the entire frozen semantic contract and exact final evidence head. Gate evidence must be independently reproduced or captured with provenance. A semantic permit requires every in-scope worker criterion satisfied or exactly owner-accepted. Evidence/head drift, auditor outages and pass/cost caps hold the authoring run rather than losing it; an outage exits through an owner-triggered retry under the remaining budget or cancel-and-rerun.

## Included

- Read-only, tool-less completion auditor isolated from the lead session, built on the existing advice-lane primitive.
- Trusted server contract/source payload and untrusted repository-data treatment.
- Worker-captured gate evidence bound to an exact head, with re-execution only as the fallback.
- A deterministic, worker-built evidence packet with a fixed input budget.
- `satisfied|unsatisfied|unverified` per-criterion results.
- At most two verdict-bearing full-contract audits per run; no incremental audits.
- Concrete audit wall/token/pass ceilings and recovery behavior.
- Semantic permit policy and owner-accept integration.
- A non-bypassable `completion_audit_v1` worker capability for semantic profiles.
- Default-on instance semantic enrollment for new eligible issue runs; explicit admin kill-switch and instance-admin model/cost ceilings frozen per run. No per-repo override at launch.
- Creation-time frozen profiles, hard capability routing and named queued health during version skew; no live-fleet default selection or automatic downgrade.
- Web/CLI audit, evidence, head and cost visibility.

## Excluded

- Structured finding storage/dispositions and MR-rework gating, delivered by #1233.
- Provider-context export/restore and #1214 retention.
- Moving ADR-456 alignment out of `phasePublish`.
- Live-model quality validation as a worker-gated milestone.
- Open-web evidence, workflow changes, automatic merge or reviewer-policy expansion.

## Decisions

### D1: Audit in one tool-less read-only turn

The auditor runs in the trusted worker process using the owner's provider credential but no lead transcript, tools, edit capability or write-capable agents. Reuse `runReadOnlyModelPass` (`agent/src/model-pass.ts`): one tool-less, repo-isolated, wall-capped turn, not a new harness/loop. Current `AdviceRequest` labels include `judge | review | summary | now`; add `audit` (#1230 adds `coverage`) and parse strict bounded JSON as the judge does. Schema failure consumes the single D4 retry; a second failure holds `audit_unavailable`. Server-frozen contract identity is authoritative; source/evidence text is untrusted requirements/evidence data, not executable instructions. A repository `satisfied` assertion is not proof.

The evidence packet is built deterministically by the worker, never by the model: a complete diff manifest (every changed path with stats), bounded criterion-linked patches and context, gate evidence summaries and artifact digests. Raw multi-megabyte gate logs are never sent. The prompt is ordered contract and source snapshots first, packet last, which maximizes provider-cache eligibility for a second pass; the primitive does not guarantee a cache read and no budget assumes one. The builder targets a fixed input budget; a criterion whose linked context does not fit returns `unverified` with reason `context_truncated`, visible in the UI, never a silent trim to `satisfied`. Audit cost therefore scales with the diff and the criteria count, not with implementation wall time.

The worker remains the trust boundary because it holds the PAT. Independence protects against lead assumptions and prompt history, not worker compromise.

### D2: Make evidence reproducible and head-bound

Lead prose about gate output is unverified. The default evidence is a worker-captured artifact of the canonical gate command: the worker spawns the gate itself (`task gate:<component>` or the repo-declared equivalent), captures output outside the repository and binds the artifact to argv, scrubbed-environment policy, committed head, clean-tree fingerprint, exit status, timing, completeness/truncation markers and digest. Generic shell output is not evidence. A positive test count is required for test gates only, not for lint or build gates. Current `gatesUnverified`/`gatesDiscoveryTruncated` fields describe dependency discovery, not gate results.

Re-execution is the fallback, not the default: the worker reruns the gate only when the captured artifact is missing, corrupt, incomplete, noncanonical or head/tree-mismatched. Re-execution runs under the existing gate timeouts and is excluded from the audit wall cap in D4, which covers model invocations only.

There are no incremental audits. A full-contract audit runs when a structurally complete candidate is claimed at a given head. If it rejects, the same lead reworks once; rework or an evidence refresh changes the reuse key, and then the second full audit runs. A completion attempt reuses the standing verdict, and issues no new audit, only when its reuse key is identical: contract revision, exact head, clean-tree fingerprint, evidence-packet digest (gate artifacts included), audit protocol version and the frozen model/config. New evidence at the same head is a new key and permits a new audit, within the pass cap. Every permit requires a full-contract audit at the exact evidence head; file overlap alone never carries a verdict.

### D3: Handle late ADR-456 head drift through recovery

Keep alignment in `phasePublish`. If the semantic evidence head differs after the GitHub-only ADR-456 workflow-tree alignment, do not reuse the audit. Publish a new checkpoint for the aligned tip, preserve the tree/session, and enter `recovery_wait` with `recovery_reason=base_aligned` and zero/minimal delay that does not increment provider-outage backoff. Resume the same run/session, rerun gates on the aligned tree and require a new signal/audit. The second alignment should be a no-op.

This is an automatic recovery, not an owner acceptance question. Secret-scan/finalization failures retain existing typed behavior.

### D4: Bound cost without accepting uncertainty

Missing results, skipped gates, truncated discovery, truncated packet context and non-worker evidence are unverified. Auditor transport or schema errors get the initial call, one retry, then the `audit_unavailable` hold: no `recovery_wait` loop and no further automatic recovery, and they never ask the owner to waive an outage.

The ceilings are concrete instance defaults, configurable by the instance admin and frozen at run creation. Per-repo/user ceiling overrides are deferred, not silently introduced as required opt-in:

| Bound | Default |
|---|---|
| Model | `sonnet` (or the provider's balanced equivalent), medium effort where supported |
| Wall | 5 minutes per model invocation; 10 minutes aggregate model-invocation wall per run (fallback gate re-execution is not included) |
| Passes | two verdict-bearing full audits maximum per run |
| Retry | one automatic transport/schema retry per audit attempt; it does not reset wall or token budgets, and owner-triggered attempts share the same immutable aggregate wall/token budgets |
| Tokens | 100,000 normalized provider tokens total across completion-audit calls, counting uncached input, cache read/write and output; 4,000 output tokens maximum per verdict |

Both consumed and remaining bounds are visible on the feed. Pass/wall/token exhaustion takes #1226's hold with reason `audit_cap`; no automatic loop. The owner may #1227 `accept` exact named unverified criteria or cancel/re-run; `continue` cannot repair an exhausted frozen cap and is not offered there. Admin ceiling changes affect future runs only; no run top-up is added. Transport/schema retry exhaustion without a verdict takes `audit_unavailable`, not a semantic finding. Its exits are one owner-triggered initial-call-plus-one-retry attempt under the remaining immutable budget, or cancel/re-run. Exhausted remaining budget becomes `audit_cap`. Do not offer `accept` on `audit_unavailable`: no verdict names criteria and acceptance is never an outage waiver. No cap turns unmet/unverified into satisfied.

The expected cost of the happy path is one sonnet turn over a bounded packet, on the order of minutes and low single-digit dollars; a run that consumed five hours of implementation does not earn a proportionally long audit.

A provider safety-policy refusal on a tool-less audit advice pass is `audit_unavailable` under D4, not a #3068 lead refusal, automatic harness switch or semantic waiver. Coverage advice refusal separately follows #1230 D3. Preserve the advice lane's authority boundary.

### D5: Freeze semantic enrollment before the first claim

Resolve eligibility and effective D6 policy once at creation. Persist semantic enrollment before the first claim, while its detailed contract freezes later with the exact approved plan; both must agree. Do not infer planning-phase eligibility from a not-yet-existent contract. Existing structural/legacy rows never convert. Later setting edits, worker heartbeats, available capacity and API restarts cannot downgrade or upgrade stored enrollment. Hard claim predicates sit alongside #1226's clauses, outside `required_capabilities`, overrides and capability kill-switches:

- Structural enrollment requires `completion_interlock_v1`.
- Semantic enrollment requires both `completion_interlock_v1` and `completion_audit_v1`.
- Retain `codex_completion_interlock_v1` and existing harness eligibility for Codex. Both harnesses must implement and test semantic coverage/audit at launch before advertising the shared semantic capability.

No compatible worker means visibly queued with the missing protocol named, never structural fallback. Health, spread/peer eligibility and hosted demand/placement mirror the hard requirement even before plan approval. Permit requires every in-scope worker criterion satisfied or exactly owner-accepted at matching revision/evidence head. Maintainer exclusions require #1230 D4's frozen exact human authorization, not a lead kind or auto-approval. Pending acceptance produces non-closing delivery without a redundant decision; consume the actual merged #3089 scope and corrected #3090 pre-push seams. #1233 later adds its blocker predicate through reserved fields, not a duplicate ledger.

M4 switches lead guidance on semantic runs from #3090's separate-final-milestone convention to `kind=maintainer` criteria. Structural runs keep #3090's convention; no stored structural contract is migrated or rewritten. Both paths use their existing authority ledger and cannot double-count the same deferred work.

### D6: Default-on instance policy, explicit rollback and bounded spend

#1230's compiler/reviewer stays production-unreachable until this child activates coverage and audit together. Add instance `semantic_completion_rollout`, code default `true`, no preseeded false rows, editable through the existing admin settings surface. An explicit admin `false` is the emergency kill-switch for new runs, not removal of stored contracts. Admin model/effort and coverage/audit ceilings freeze per run. No repo setting, column, override write seam or owner ceiling controls ship at launch; those are deferred. If a later override is justified, nullable/absent must mean inherit, and it cannot defeat an admin disable; do not create false default rows.

Eligibility remains the shipped structural enrollment domain: new unseeded, non-interactive issue runs on both harnesses with the structural rollout enabled. Preserve seeded/legacy exclusions and explicit structural disable. In that domain semantic is default-on, not a hidden per-repo opt-in. A known explicit semantic disable selects structural without semantic model calls for new runs only. Both rollout keys resolve from one settings snapshot; a key set by ENV overlay counts as known. When that snapshot cannot be read and no valid cache exists, creation of an unseeded issue run is refused with a typed retriable error. This deliberately supersedes #1626's cold-read-as-legacy fallback for that domain once #1231 ships; seeded and non-issue creation paths are unchanged. A failed refresh over a valid cache uses the cached values for both keys.

The refusal contract is `semantic_policy_unavailable`, HTTP 503 with a positive `Retry-After`, and no run/row created. Manual CLI/web creation shows the retriable error without an automatic client retry loop. Scheduler handling reuses existing patterns, with no new retry engine. A single-issue fire (once or recurring) is held un-advanced like the credential-disabled once hold. The `semantic_policy_unavailable` skip is recorded through the held-fire last_fire write, deduplicated, and the fire retries on the next tick. A sweep stops its fan-out at the first refusal. With nothing started it is held un-advanced the same way. With runs already started it records the typed skip and advances: a recurring sweep re-picks the candidate at its next fire. A once sweep accepts that boundary, recorded visibly rather than lost silently, because a held refire would start more than max_issues. `semantic_policy_unavailable` joins the closed SkipReason set with its web/CLI mirrors and the parity test. Preserve idempotency and avoid burst loops. Tests cover both keys from one snapshot, cached refresh failure, explicit structural false yielding legacy, explicit semantic false yielding structural, seeded/non-issue unchanged, uncached unseeded refusal with zero writes, and each single-issue/sweep retry boundary without loss of visibility or duplicate starts.

Enrollment never follows momentary fleet readiness: persist it once under D5. New API/old worker queues semantic rows; old API/new worker before semantic rows exist retains old structural behavior; matched releases prove both harnesses. Activating chart/compose releases must pair the API with a worker image whose semantic capability behavior is proven on that image, not inferred from tag existence. Use existing release-pin machinery and explicit release evidence without workflow-file edits or a new readiness latch.

Supported rollback disables semantic enrollment for future runs and uses only API/worker releases that retain semantic profile/claim/permit enforcement. Existing semantic runs remain semantic and may queue. Downgrading to a pre-semantic API while semantic runs or authority exist is unsupported and prohibited in the runbook: an old binary cannot enforce unknown semantics. A forced unsupported deployment can violate guarantees; do not call it safe rollback or count later cancellation/roll-forward as preservation proof. Retain schema/data and require forward repair or a separately reviewed drained migration, never contract stripping.

Default-on uses bounded model usage on the owner's credential: coverage at most 120k normalized tokens and audit at most 100k per run by their separate defaults. These are maxima, not expected bills. Coverage runs only at #1230's exact approval decision point; intermediate Plan cross-check revisions consume no coverage calls. Budgets remain independent, frozen and non-resetting. #1232 proves integrated default-on compatibility/rollback; it does not flip defaults again. The independent-audit guarantee applies to semantic enrollment, not explicit disables or excluded lanes.

## Milestones and dependency plan

Five sequential milestones; web and CLI presentation can proceed in parallel after M4 freezes DTOs but component gates remain serial.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: auditor protocol, isolation and evidence capture | #1226 + #1230 | auditor/check runner, protocol | Read-only verdicts and gate evidence bind to one head. |
| 2 | M2: two-audit loop, evidence packet and cost bounds | M1 | executor/runner attempt loop, packet builder | Audit spend is bounded and proportional to the diff; final authorization is complete. |
| 3 | M3: head-drift and outage recovery | M1-M2 | `phasePublish`, checkpoint/recovery store | Stale semantic evidence cannot authorize a changed head. |
| 4 | M4: semantic permit, hard audit capability and default-on enrollment | M1-M3 + merged #1230/#3089/#3090 | completion service/store/claim SQL, admin settings API | Enrollment freezes at creation; only capable workers with exact-head evidence can permit. |
| 5 | M5: web, CLI, docs and adversarial proof | M1-M4 | web/CLI/docs/specs/tests | Users see verdict/cost and load-bearing guards are proven. |

- [ ] **M1: auditor protocol, isolation and evidence capture.** Add the tool-less auditor as a `runReadOnlyModelPass` caller with structured bounded output, and worker gate capture at exact head: the worker spawns the canonical gate, captures outside the repo and binds argv, environment policy, head, tree fingerprint, exit, timing, completeness and digest; re-execution only for missing/corrupt/incomplete/noncanonical/mismatched artifacts. Treat server contract as authority and repository content as untrusted. Test denied tools, prompt injection, missing result, skipped/truncated gates, positive counts required for test gates only, generic shell output rejected and head/tree mismatch. Gate: `task gate:agent`, `task scan:secrets`.
- [ ] **M2: two-audit loop, evidence packet and cost bounds.** Build the deterministic evidence packet (complete diff manifest, bounded criterion-linked patches/context, gate summaries, digests; contract/source prefix first) under a fixed input budget with `context_truncated` verdicts on overflow. Run one full-contract audit per structurally complete candidate, a second only when the D2 reuse key changes (a new head, or new evidence at the same head), and reuse the standing verdict while the key is identical. Enforce the D4 table: per-call and aggregate wall, two verdict-bearing passes, one non-resetting automatic transport/schema retry per audit attempt with owner-triggered attempts drawing on the same aggregate budgets, 100k total / 4k output tokens, `audit_cap` and `audit_unavailable` holds. Test that an oversized diff yields `unverified` not `satisfied`, that a third audit never fires, that the retry resets no budget, that gate re-execution does not consume audit wall, and that cap exhaustion cannot permit. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M3: head-drift and outage recovery.** Detect post-evidence ADR-456 head change, checkpoint-publish the aligned tip, preserve tree/session, park with `recovery_reason=base_aligned` and zero/minimal delay without advancing provider-outage backoff, then resume for fresh gates/audit. Separately route auditor transport/schema failures through the D4 initial-call-plus-one-retry path into the `audit_unavailable` hold, with no recovery backoff loop. Test no owner question, no failed state, no stale permit and capture/ACK/requeue races with real Git and LiveDB. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M4: semantic permit, hard audit capability and default-on enrollment.** Persist D5 enrollment before first claim and require non-bypassable `completion_audit_v1` alongside existing structural/Codex/harness clauses. Add instance default-on admin settings/read/write controls and freeze effective model/ceilings per run; no repo controls. Require exact-head audit and exact authorized maintainer exclusions in the server permit. LiveDB proves both-harness defaults without settings rows, explicit disables, retriable unknown-policy reads, valid-cache behavior, frozen profiles across setting edits, legacy/seed exclusions, old-worker refusal through overrides, named capability queue/placement and all unsatisfied/unverified/head/revision denials. Use actual merged #3089/#3090 scope/publication seams. Execute named LiveDB tests through `./e2e/run-store-it.sh`, no SKIP. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M5: web, CLI, docs and adversarial proof.** Render criteria/audit/evidence head, consumed/remaining bounds and hold reasons without raw transcripts/logs. Expose the instance semantic kill-switch/model/ceilings in admin settings; CLI visibility and any authorized writes must use supported authentication, never silently add a cookie-only route to a Bearer command. There is no per-repo enable flag. Update the planned requirement in `specs/human.md` only to match delivered evidence: new eligible issue runs use semantic completion by default; exact-head audits, bounded spend and human-authorized pending acceptance are enforced; existing/disabled/seeded lanes retain declared limitations. Amend ADR-1225, update audit/plan/CLI/upgrade docs, and run `task docs:sync`. Prove API-first/worker-first skew on isolated matching images and both harnesses; record unsupported pre-semantic downgrade separately from supported rollback. Mutations remove capability, audit, head, pass cap and enrollment guards individually and must redden named assertions without workflow edits.

## Acceptance criteria

- A lead declaring every milestone cannot obtain a semantic permit while one criterion is unsatisfied or unverified.
- Every semantic permit uses a full-contract audit of the exact final head; no incremental audit exists, at most two verdict-bearing audits run per run, and a verdict is reused only under the identical D2 reuse key.
- The auditor is tool-less and consumes a worker-built packet; an overflowed criterion is `unverified` with `context_truncated`, never `satisfied`.
- Gate evidence is a worker-captured artifact bound to argv, environment policy, head, tree fingerprint, exit, timing, completeness and digest; re-execution is the fallback only, runs under gate timeouts and consumes no audit wall.
- D4 ceilings (model, per-call/aggregate wall, passes, retry, tokens) are enforced, visible, instance-admin configured and frozen per run; exhaustion holds `audit_cap` or `audit_unavailable`.
- With a known admin semantic disable, new eligible issue runs remain structural with zero coverage/audit calls; unknown policy fails creation retriably rather than silently disabling enforcement. Existing enrollment and frozen model/budgets do not change.
- Repository prompt injection cannot redefine server contract or auditor authority.
- ADR-456 head drift republishes the aligned checkpoint and automatically resumes for new evidence without stale-audit reuse.
- Auditor outage holds with `audit_unavailable` after one retry and exits only through an owner-triggered retry under the remaining budget or cancel-and-rerun; cap exhaustion holds with `audit_cap` and offers no `continue`; neither silently accepts or loses work.
- Workers lacking `completion_audit_v1` cannot claim semantic runs through any override or kill-switch.
- Default-on works on both harnesses without stored settings rows or repo opt-in. Missing capable capacity queues semantic runs with a named reason, including planning claims; seeded/legacy and explicit structural-off lanes remain excluded.
- Coverage runs only at the exact approval decision point. Intermediate checker revisions spend no coverage budget; a changed candidate/source/criteria cannot reuse stale coverage or required checker approval. Both ledgers stay independently capped.
- Only exact frozen human-authorized maintainer criteria can remain outstanding on a truthful non-closing delivery, with no second equivalent owner decision. Auto-approval, ordinary prose and checked display boxes cannot invent an exemption or waive failed worker gates.
- API/worker skew and semantic-aware rollback preserve profiles and head/revision fences. No supported rollback strips authority or downgrades a semantic run; a pre-semantic API downgrade with semantic authority present is explicitly unsupported.
- Audit cost/pass bounds and results are visible without exposing raw content.
- #1233 can add blockers through reserved permit fields without changing this wire contract.

## Review record

- 2026-10-17 (AI-synced, planned refresh): default-on instance enrollment with an admin kill-switch replaces per-repo opt-in; model/ceilings are instance-admin and frozen per run, with repo overrides deferred. Both harnesses launch together; hard capability queues, retriable unknown-policy reads and semantic-aware rollback preserve guarantees. Coverage runs at exact approval decision points under #1230, not intermediate checker rounds. #3089/#3090 remain unmerged integration gates; #1228 is unrelated and #1229 remains not planned. No code/settings/dispatch changed; exact-diff peer/independent review is pending. Historical decisions below record the superseded opt-in design.

- 2026-09-09: Split from the original #1231 scope after final deliverability review found auditor, findings and MR rework together too large. This child owns semantic audit/permit only; #1233 owns blockers/rework.
- 2026-09-09: Cost/time bounding pass after the maintainer asked that a long implementation run not earn a proportionally long review. Agreed between the lead Claude session and `@vasile` (Codex): tool-less auditor on the advice-lane primitive, worker-built evidence packet, captured gate artifacts with re-execution as fallback only, no incremental audits, two-audit cap, the D4 ceiling table, and the semantic profile as a per-repo opt-in (D6) with the guarantee wording scoped to semantic-profile runs. `@vasile` then required, and this PRD adopted: a multi-field verdict reuse key rather than head alone, an explicit `audit` label plus JSON parsing on the text-returning primitive, initial-call-plus-one-retry into `audit_unavailable` with no `recovery_wait` loop, no `continue` remedy on an exhausted frozen cap, and cache ordering described as eligibility rather than a guarantee. Structural #1226 is unchanged.
