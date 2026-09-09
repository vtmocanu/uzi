# PRD #1231: Independent completion auditor and semantic permit

**Issue:** [#1231](https://github.com/vtmocanu/uzi/issues/1231)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1226 and #1230; the one-at-a-time sequence lands #1227-#1229 first.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1230 merges and main CI is green.

This child activates semantic completion auditing on top of #1226's structural permit and #1230's source-backed contract. Structured review blockers and governed MR rework are separate child #1233. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

Structural coverage cannot determine whether arbitrary acceptance criteria are truly satisfied. A lead can declare every milestone while part of one remains missing, as PR #1220 did for M3. Plan-time source coverage improves the contract but does not judge the final implementation.

Run a separate read-only completion auditor against the entire frozen semantic contract and exact final evidence head. Gate evidence must be independently reproduced or captured with provenance. A semantic permit requires every in-scope worker criterion satisfied or exactly owner-accepted. Evidence/head drift, auditor outages and pass/cost caps recover without losing the authoring run.

## Included

- Read-only completion auditor isolated from the lead session.
- Trusted server contract/source payload and untrusted repository-data treatment.
- Worker-reexecuted or worker-captured gate evidence bound to an exact head.
- `satisfied|unsatisfied|unverified` per-criterion results.
- Incremental steering audits plus one full-contract audit per permit.
- Audit token/pass bounds and recovery behavior.
- Semantic permit policy and owner-accept integration.
- A non-bypassable `completion_audit_v1` worker capability for semantic profiles.
- Web/CLI audit, evidence, head and cost visibility.

## Excluded

- Structured finding storage/dispositions and MR-rework gating, delivered by #1233.
- Provider-context export/restore and #1214 retention.
- Moving ADR-456 alignment out of `phasePublish`.
- Live-model quality validation as a worker-gated milestone.
- Open-web evidence, workflow changes, automatic merge or reviewer-policy expansion.

## Decisions

### D1: Audit in a separate read-only session

The auditor runs in the trusted worker process using the owner's provider credential but no lead transcript, edit tools or write-capable agents. Its prompt receives the frozen contract/source snapshots from the server and repository/diff at the evidence head as untrusted data. A `satisfied` statement found only in repository text is not evidence.

The worker remains the trust boundary because it holds the PAT. Independence protects against lead assumptions and prompt history, not worker compromise.

### D2: Make evidence reproducible and head-bound

Self-reported gate output is unverified. A gate counts only when the worker/auditor reexecutes the command in the scrubbed environment or reads a complete worker-captured log tied to the same head with command, exit status, positive test count, skip/truncation markers and bounded digest. Current `gatesUnverified`/`gatesDiscoveryTruncated` fields describe dependency discovery, not gate results.

Attempts two through N-1 default to incremental audit for changed/unmet criteria. Every permit requires one full-contract audit at the exact evidence head. File overlap alone never carries a final verdict.

### D3: Handle late ADR-456 head drift through recovery

Keep alignment in `phasePublish`. If the semantic evidence head differs after the GitHub-only ADR-456 workflow-tree alignment, do not reuse the audit. Publish a new checkpoint for the aligned tip, preserve the tree/session, and enter `recovery_wait` with `recovery_reason=base_aligned` and zero/minimal delay that does not increment provider-outage backoff. Resume the same run/session, rerun gates on the aligned tree and require a new signal/audit. The second alignment should be a no-op.

This is an automatic recovery, not an owner acceptance question. Secret-scan/finalization failures retain existing typed behavior.

### D4: Bound cost without accepting uncertainty

Missing results, skipped gates, truncated discovery and non-worker evidence are unverified. Auditor transport errors retry in process and then use ordinary transient `recovery_wait`; they never ask the owner to waive an outage.

Cap auditor passes and total audit token/cost per run, expose both on the feed, and route an exhausted cap to #1226's completion hold with reason `audit_cap`. A cap cannot convert unmet/unverified to satisfied.

### D5: Activate semantic permits only on capable workers

For new runs, switch the accepted contract to `profile=semantic` only after the server/fleet advertise `completion_audit_v1`. The hard claim predicate lives alongside #1226's interlock predicate, outside `required_capabilities`, owner override and capability kill-switch:

- structural contract requires `completion_interlock_v1`;
- semantic contract requires both `completion_interlock_v1` and `completion_audit_v1`.

The queue reason names the missing audit capability. Permit requires every in-scope non-maintainer criterion `satisfied` or owner-accepted, matching contract revision and exact evidence/final head. #1233 later adds the no-open-blocker predicate through fields reserved by #1226.

## Milestones and dependency plan

Five sequential milestones; web and CLI presentation can proceed in parallel after M4 freezes DTOs but component gates remain serial.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: auditor protocol, isolation and evidence capture | #1226 + #1230 | auditor/check runner, protocol | Read-only verdicts and gate evidence bind to one head. |
| 2 | M2: incremental/full audit loop and cost bounds | M1 | executor/runner attempt loop | Steering remains bounded; final authorization is complete. |
| 3 | M3: head-drift and outage recovery | M1-M2 | `phasePublish`, checkpoint/recovery store | Stale semantic evidence cannot authorize a changed head. |
| 4 | M4: semantic permit and hard audit capability | M1-M3 | completion service/store/claim SQL | Only capable workers and accepted exact-head audits can permit. |
| 5 | M5: web, CLI, docs and adversarial proof | M1-M4 | web/CLI/docs/specs/tests | Users see verdict/cost and load-bearing guards are proven. |

- [ ] **M1: auditor protocol, isolation and evidence capture.** Add the separate read-only auditor, structured bounded output and worker gate-execution/log capture at exact head. Treat server contract as authority and repository content as untrusted. Test denied tools, prompt injection, missing result, skipped/truncated gates, positive counts and head mismatch. Gate: `task gate:agent`, `task scan:secrets`.
- [ ] **M2: incremental/full audit loop and cost bounds.** Make incremental audit the default for intermediate steering and require one full-contract audit before every permit. Track pass/token/cost ceilings, reset/continuation rules and hold on cap. Test indirect config/dependency changes force full final review and cap exhaustion cannot permit. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M3: head-drift and outage recovery.** Detect post-evidence ADR-456 head change, checkpoint-publish the aligned tip, preserve tree/session, park with `recovery_reason=base_aligned` and zero/minimal delay without advancing provider-outage backoff, then resume for fresh gates/audit. Separately route auditor transport outages through bounded retry and normal recovery backoff. Test no owner question, no failed state, no stale permit and capture/ACK/requeue races with real Git and LiveDB. Gates: `task gate:agent`, `task gate:api`.
- [ ] **M4: semantic permit and hard audit capability.** Extend the hard claim rule with non-bypassable `completion_audit_v1`; activate semantic profile only after fleet support. Require complete exact-head audit plus owner decisions in the server permit. LiveDB proves a #1226-era worker cannot claim a semantic run through override/kill-switch and every unsatisfied/unverified/head/revision combination denies. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M5: web, CLI, docs and adversarial proof.** Render criteria/audit/evidence head, pass/token/cost bounds and recovery/hold reasons without raw transcripts/logs. Amend ADR 1225 and add the user-approved requirement to `specs/human.md`: "An issue run may publish only after an independent full-contract audit accepts the exact final head; an incomplete attempt returns to the same lead or parks without discarding Git or supported provider context. [user, #1231]" Update audit/plan/CLI docs and run `task docs:sync`. Mutations remove audit capability, audit requirement and head binding individually and must redden named assertions.

## Acceptance criteria

- A lead declaring every milestone cannot obtain a semantic permit while one criterion is unsatisfied or unverified.
- Every semantic permit uses a full-contract audit of the exact final head; incremental results alone cannot authorize it.
- Gate evidence is worker-reproduced/captured with command/exit/count/skip provenance, not trusted lead prose.
- Repository prompt injection cannot redefine server contract or auditor authority.
- ADR-456 head drift republishes the aligned checkpoint and automatically resumes for new evidence without stale-audit reuse.
- Auditor outage uses recovery waiting; audit-cap exhaustion uses completion hold; neither silently accepts or loses work.
- Workers lacking `completion_audit_v1` cannot claim semantic runs through any override or kill-switch.
- Audit cost/pass bounds and results are visible without exposing raw content.
- #1233 can add blockers through reserved permit fields without changing this wire contract.

## Review record

- 2026-09-09: Split from the original #1231 scope after final deliverability review found auditor, findings and MR rework together too large. This child owns semantic audit/permit only; #1233 owns blockers/rework.
