# PRD #1231: Independent completion auditor and semantic permit

**Issue:** [#1231](https://github.com/vtmocanu/uzi/issues/1231)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226 and #1230; the one-at-a-time sequence lands #1227-#1229 first.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1230 merges and main CI is green.

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
- One per-repo setting that enables #1230 coverage review and this audit together, default off.
- Web/CLI audit, evidence, head and cost visibility.

## Excluded

- Structured finding storage/dispositions and MR-rework gating, delivered by #1233.
- Provider-context export/restore and #1214 retention.
- Moving ADR-456 alignment out of `phasePublish`.
- Live-model quality validation as a worker-gated milestone.
- Open-web evidence, workflow changes, automatic merge or reviewer-policy expansion.

## Decisions

### D1: Audit in one tool-less read-only turn

The auditor runs in the trusted worker process using the owner's provider credential but no lead transcript, no tools, no edit capability and no write-capable agents. It is a caller of `runReadOnlyModelPass` (`agent/src/model-pass.ts`, the judge/review/summary primitive: one tool-less, repo-isolated, wall-capped turn), not a new harness or agent loop. The primitive returns text and its `AdviceRequest` label union in `agent/src/harness.ts` is `judge | review | summary`; this child adds an `audit` label (#1230 adds `coverage`) and parses a strict, schema-validated JSON verdict from the text turn the way `judge-runner.ts` extracts its JSON. A verdict that fails schema validation consumes the single D4 retry; a second failure holds with `audit_unavailable`. Its prompt receives the frozen contract/source snapshots from the server as authority and the evidence packet as untrusted data. A `satisfied` statement found only in repository text is not evidence.

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

The ceilings are concrete defaults, owner-configurable per repo and frozen at run creation:

| Bound | Default |
|---|---|
| Model | `sonnet` (or the provider's balanced equivalent), medium effort where supported |
| Wall | 5 minutes per model invocation; 10 minutes aggregate model-invocation wall per run (fallback gate re-execution is not included) |
| Passes | two verdict-bearing full audits maximum per run |
| Retry | one automatic transport/schema retry per audit attempt; it does not reset wall or token budgets, and owner-triggered attempts share the same immutable aggregate wall/token budgets |
| Tokens | 100,000 normalized provider tokens total across completion-audit calls, counting uncached input, cache read/write and output; 4,000 output tokens maximum per verdict |

Both the consumed and remaining bounds are exposed on the feed. An exhausted pass, wall or token cap routes to #1226's completion hold with reason `audit_cap`; no further automatic audit loop runs, and the owner path is #1227 `accept` (with the unverified criteria named) or cancel and re-run; `continue` cannot repair an exhausted frozen cap and is not offered on that hold, and a per-repo cap change applies to future runs only. A run-scoped cap top-up is not in this child. A transport retry exhausted without any verdict holds with reason `audit_unavailable`, never disguised as a semantic finding. That hold has two exits: an owner-triggered retry (`completion_decision: continue` on this hold re-arms exactly one initial-call-plus-one-retry audit attempt under the remaining immutable wall/token budget; when that budget is exhausted the hold becomes `audit_cap`), or cancel and re-run. `accept` is not offered on `audit_unavailable`: no verdict names any criterion to accept against, and acceptance is never an outage waiver. A cap cannot convert unmet/unverified to satisfied.

The expected cost of the happy path is one sonnet turn over a bounded packet, on the order of minutes and low single-digit dollars; a run that consumed five hours of implementation does not earn a proportionally long audit.

### D5: Activate semantic permits only on capable workers

For new runs, switch the accepted contract to `profile=semantic` only after the server/fleet advertise `completion_audit_v1`. The hard claim predicate lives alongside #1226's interlock predicate, outside `required_capabilities`, owner override and capability kill-switch:

- structural contract requires `completion_interlock_v1`;
- semantic contract requires both `completion_interlock_v1` and `completion_audit_v1`.

The queue reason names the missing audit capability. Permit requires every in-scope non-maintainer criterion `satisfied` or owner-accepted, matching contract revision and exact evidence/final head. #1233 later adds the no-open-blocker predicate through fields reserved by #1226.

### D6: Semantic review is a per-repo opt-in, structural is the default

One per-repo setting, whose default-off storage and read seam #1230 created with no write path and no runtime effect, gains its API/CLI/web writes and its activation fence here. Behind that fence it switches a repo's new issue runs from `profile=structural` to `profile=semantic`. It enables #1230's plan coverage review and this completion audit together: coverage review without enforcement would be hidden spend, and an audit without source-backed criteria has nothing to judge. The fence opens only after `completion_audit_v1` fleet support (D5) is advertised; before that, setting intent alone does nothing, every new run stays structural and no reviewer or auditor call is made. Behind the fence the setting selects the profile and the capability gates the claim.

The structural interlock from #1226 stays on for every new issue run regardless of this setting and already contains the #1220 failure at zero model cost. The independent-audit guarantee in #1225, the `specs/human.md` requirement in M5 and #1232's default-enablement criteria apply to semantic-profile runs only.

## Milestones and dependency plan

Five sequential milestones; web and CLI presentation can proceed in parallel after M4 freezes DTOs but component gates remain serial.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: auditor protocol, isolation and evidence capture | #1226 + #1230 | auditor/check runner, protocol | Read-only verdicts and gate evidence bind to one head. |
| 2 | M2: two-audit loop, evidence packet and cost bounds | M1 | executor/runner attempt loop, packet builder | Audit spend is bounded and proportional to the diff; final authorization is complete. |
| 3 | M3: head-drift and outage recovery | M1-M2 | `phasePublish`, checkpoint/recovery store | Stale semantic evidence cannot authorize a changed head. |
| 4 | M4: semantic permit, hard audit capability and setting write seam | M1-M3 | completion service/store/claim SQL, repo settings API | Only capable workers and accepted exact-head audits can permit; the semantic setting becomes writable behind the fence. |
| 5 | M5: web, CLI, docs and adversarial proof | M1-M4 | web/CLI/docs/specs/tests | Users see verdict/cost and load-bearing guards are proven. |

- [ ] **M1: auditor protocol, isolation and evidence capture.** Add the tool-less auditor as a `runReadOnlyModelPass` caller with structured bounded output, and worker gate capture at exact head: the worker spawns the canonical gate, captures outside the repo and binds argv, environment policy, head, tree fingerprint, exit, timing, completeness and digest; re-execution only for missing/corrupt/incomplete/noncanonical/mismatched artifacts. Treat server contract as authority and repository content as untrusted. Test denied tools, prompt injection, missing result, skipped/truncated gates, positive counts required for test gates only, generic shell output rejected and head/tree mismatch. Gate: `task gate:agent`, `task scan:secrets`.
- [ ] **M2: two-audit loop, evidence packet and cost bounds.** Build the deterministic evidence packet (complete diff manifest, bounded criterion-linked patches/context, gate summaries, digests; contract/source prefix first) under a fixed input budget with `context_truncated` verdicts on overflow. Run one full-contract audit per structurally complete candidate, a second only when the D2 reuse key changes (a new head, or new evidence at the same head), and reuse the standing verdict while the key is identical. Enforce the D4 table: per-call and aggregate wall, two verdict-bearing passes, one non-resetting automatic transport/schema retry per audit attempt with owner-triggered attempts drawing on the same aggregate budgets, 100k total / 4k output tokens, `audit_cap` and `audit_unavailable` holds. Test that an oversized diff yields `unverified` not `satisfied`, that a third audit never fires, that the retry resets no budget, that gate re-execution does not consume audit wall, and that cap exhaustion cannot permit. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M3: head-drift and outage recovery.** Detect post-evidence ADR-456 head change, checkpoint-publish the aligned tip, preserve tree/session, park with `recovery_reason=base_aligned` and zero/minimal delay without advancing provider-outage backoff, then resume for fresh gates/audit. Separately route auditor transport/schema failures through the D4 initial-call-plus-one-retry path into the `audit_unavailable` hold, with no recovery backoff loop. Test no owner question, no failed state, no stale permit and capture/ACK/requeue races with real Git and LiveDB. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M4: semantic permit, hard audit capability and setting write seam.** Extend the hard claim rule with non-bypassable `completion_audit_v1`; add the server/API write seam for #1230's per-repo semantic setting and the activation fence gated on `completion_audit_v1` fleet support; select and enforce the semantic profile only when both the stored setting and the fence hold. Require complete exact-head audit plus owner decisions in the server permit. LiveDB proves a #1226-era worker cannot claim a semantic run through override/kill-switch, a repo with the setting off, or any repo before the fence opens, creates structural contracts and never calls the auditor or coverage review, and every unsatisfied/unverified/head/revision combination denies. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M5: web, CLI, docs and adversarial proof.** Render criteria/audit/evidence head, consumed/remaining pass/wall/token bounds and recovery/hold reasons without raw transcripts/logs; add the web repo-settings control and the `uzi repo` write flag for the per-repo setting on top of M4's API write seam (#1230 shipped read-only storage). Amend ADR 1225 and add the user-approved requirement to `specs/human.md`: "On a repo with semantic completion review enabled, an issue run may publish only after an independent full-contract audit accepts the exact final head; the audit is bounded to two passes and a fixed token and wall budget; an incomplete attempt returns to the same lead or parks without discarding Git or supported provider context. [user, #1231]" Update audit/plan/CLI docs and run `task docs:sync`. Mutations remove audit capability, audit requirement, head binding, the pass cap and the setting gate individually and must redden named assertions.

## Acceptance criteria

- A lead declaring every milestone cannot obtain a semantic permit while one criterion is unsatisfied or unverified.
- Every semantic permit uses a full-contract audit of the exact final head; no incremental audit exists, at most two verdict-bearing audits run per run, and a verdict is reused only under the identical D2 reuse key.
- The auditor is tool-less and consumes a worker-built packet; an overflowed criterion is `unverified` with `context_truncated`, never `satisfied`.
- Gate evidence is a worker-captured artifact bound to argv, environment policy, head, tree fingerprint, exit, timing, completeness and digest; re-execution is the fallback only, runs under gate timeouts and consumes no audit wall.
- The D4 ceilings (model, per-call and aggregate wall, passes, retry, tokens) are enforced, visible and owner-configurable; exhaustion holds with `audit_cap` or `audit_unavailable`.
- With the per-repo semantic setting off, new issue runs are structural only and no coverage review or audit call is made.
- Repository prompt injection cannot redefine server contract or auditor authority.
- ADR-456 head drift republishes the aligned checkpoint and automatically resumes for new evidence without stale-audit reuse.
- Auditor outage holds with `audit_unavailable` after one retry and exits only through an owner-triggered retry under the remaining budget or cancel-and-rerun; cap exhaustion holds with `audit_cap` and offers no `continue`; neither silently accepts or loses work.
- Workers lacking `completion_audit_v1` cannot claim semantic runs through any override or kill-switch.
- Audit cost/pass bounds and results are visible without exposing raw content.
- #1233 can add blockers through reserved permit fields without changing this wire contract.

## Review record

- 2026-09-09: Split from the original #1231 scope after final deliverability review found auditor, findings and MR rework together too large. This child owns semantic audit/permit only; #1233 owns blockers/rework.
- 2026-09-09: Cost/time bounding pass after the maintainer asked that a long implementation run not earn a proportionally long review. Agreed between the lead Claude session and `@vasile` (Codex): tool-less auditor on the advice-lane primitive, worker-built evidence packet, captured gate artifacts with re-execution as fallback only, no incremental audits, two-audit cap, the D4 ceiling table, and the semantic profile as a per-repo opt-in (D6) with the guarantee wording scoped to semantic-profile runs. `@vasile` then required, and this PRD adopted: a multi-field verdict reuse key rather than head alone, an explicit `audit` label plus JSON parsing on the text-returning primitive, initial-call-plus-one-retry into `audit_unavailable` with no `recovery_wait` loop, no `continue` remedy on an exhausted frozen cap, and cache ordering described as eligibility rather than a guarantee. Structural #1226 is unchanged.
