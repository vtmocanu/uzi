# PRD #1233: Structured completion blockers and governed MR rework

**Issue:** [#1233](https://github.com/vtmocanu/uzi/issues/1233)
**Parent epic:** [#1225](https://github.com/vtmocanu/uzi/issues/1225)
**Status:** Planned, blocked on #1227, #1230 and #1231.
**Priority:** High.
**Execution:** Keep `Planned` without `uzi`. Add `uzi` only after #1231 merges and main CI is green.

This child gives completion-relevant review findings server-owned identities and applies them to both issue completion and MR rework. It consumes #1231's auditor/semantic permit and does not change provider-context storage. No implementation or validation may modify `.github/workflows/**`.

## Problem and outcome

Today's internal review findings are prose inside the lead session. A lead can skip them, call them invalid, or finish an MR-rework attempt without a server-owned disposition. Even after semantic criteria are audited, an unresolved correctness blocker can therefore disappear from the completion decision.

Add a bounded structured blocker ledger created by system auditors/reviewers. Fixed findings require evidence at the exact head, invalid findings require independent confirmation, and deferred findings require an exact owner decision. Extend the semantic permit with no-open-blocker policy and make automatic/manual MR rework inherit the current contract plus current finding set.

## Included

- Stable completion-finding identities, classes and bounded evidence/file references.
- System auditor/reviewer report and settle tools.
- `fixed|invalid|deferred` dispositions with distinct authority.
- Semantic permit no-open-blocker rule through #1226's reserved fields.
- Automatic and manual MR-rework contract/finding inheritance.
- A findings-aware MR-rework completion profile.
- Web/CLI finding, disposition and decision visibility.
- Prompt/documentation corrections that remove lead self-dismissal language.

## Excluded

- Replacing incidental-findings issue filing or retrospective judge recommendations.
- Provider-context retention/restore, which remains #1214.
- Changing review trigger debounce, cycle caps or automatic merge.
- Allowing a lead to downgrade/dismiss its own blocker.
- Live-model reviewer quality as a worker completion milestone.

## Decisions

### D1: Findings are system-owned, bounded records

The completion auditor or a worker-launched independent reviewer creates findings with stable ID, run/attempt/contract revision, creation head, blocking class, bounded text, safe file references and status. Repository text is untrusted and cannot mint or close a record. Incidental findings stay in their existing lifecycle unless the system explicitly promotes one.

### D2: Settlement authority depends on disposition

- `fixed`: lead proposes a commit/evidence; the system auditor verifies it at the current exact head.
- `invalid`: a different independent reviewer confirms with bounded rationale; the authoring lead cannot confirm.
- `deferred`: only owner/admin through #1227's completion-decision path, naming exact finding IDs and a reason.

Stale-head evidence, missing reviewer identity, unknown IDs and conflicting dispositions do not settle. Decisions and settlements invalidate prior permits.

### D3: Extend, do not replace, semantic permit policy

Use the optional `open_finding_ids` field reserved by #1226. For `profile=semantic`, the server requires no open blocking finding in addition to #1231's criterion audit. Recompute from server records rather than trusting the request list. Keep structural/legacy permit behavior unchanged.

### D4: Govern both automatic and manual MR rework

`CreateAutoMRReworkRun` and `CreateManualMRReworkRunAndAdvance` copy the source run's current contract identity/revision and a bounded finding snapshot. A NULL legacy source creates legacy rework. A governed rework uses a findings-aware semantic profile: current review findings plus the original current contract, one full exact-head audit before completion, and the same owner decisions.

Keep `target_run_id` semantics and branch exclusivity. #1214 context restoration is optional continuity, never evidence or authority. A fresh session must reach the same permit decision.

### D5: Make every outcome visible without leaking content

Web/CLI show finding ID/class/status, bounded rationale, creation/settlement head, disposition authority and linked owner decision. They do not expose raw reviewer transcripts, arbitrary file contents or storage/session identifiers. MR/PR bodies summarize any owner-deferred blocker and reason.

## Milestones and dependency plan

Five sequential milestones; web and CLI rendering can run in parallel after M3 freezes DTOs.

| Phase | Milestone | Dependencies | Primary areas | Outcome |
|---|---|---|---|---|
| 1 | M1: finding schema and report authority | #1231 | migration/store/service, reviewer tools | Only system review creates bounded blockers. |
| 2 | M2: controlled settlement | M1 + #1227 | settle service/reviewer/owner decisions | Lead cannot self-dismiss; stale evidence cannot settle. |
| 3 | M3: semantic permit integration | M1-M2 | completion service/attempts | Any open blocker denies the permit. |
| 4 | M4: governed automatic/manual MR rework | M1-M3 + #1230 | mr_rework SQL/service/runner | Rework inherits contract/findings and must satisfy them. |
| 5 | M5: web, CLI, docs and adversarial proof | M1-M4 | web/CLI/docs/specs/tests | Users see dispositions and controls; guards are mutation-tested. |

- [ ] **M1: finding schema and report authority.** Add additive tables, bounds and worker-only report tools for system auditor/reviewer sessions. Validate class/text/path/head/contract identity, stale claim and duplicates. Test repository/lead attempts cannot mint or mutate blockers. Gate: `task gate:api`, `task gate:agent`, LiveDB.
- [ ] **M2: controlled settlement.** Implement fixed verification at current head, second-reviewer invalid confirmation and owner-only deferred decisions through the existing endpoint. Record immutable provenance and invalidate permits atomically. Test lead self-dismissal, same-reviewer confirmation, stale commit, concurrent decisions and non-owner hiding. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M3: semantic permit integration.** Recompute open blockers from server records and extend only semantic permit policy. Test every open/settled/disposition/head/revision combination plus response-loss idempotency; mutation removing the open-blocker predicate must permit and fail its target assertion. Gate: `task gate:api`, LiveDB.
- [ ] **M4: governed automatic/manual MR rework.** Copy current contract/revision/finding snapshot in both rework creators, retain legacy NULL behavior and enforce branch/source identity. Require current findings plus full original-contract audit at exact final head. Test auto/manual parity, new findings during a run, external head movement, no-op rework and an unavailable #1214 context fallback. Gates: `task gate:api`, `task gate:agent`.
- [ ] **M5: web, CLI, docs and adversarial proof.** Render ledger and disposition authority in run/MR-review surfaces and CLI JSON; support owner deferral by exact finding IDs/reason. Correct lead/rework prompts that currently allow self-classified invalid skips. Update review/MR-rework/CLI docs, ADR 1225 and specs; run `task docs:sync`. Mutations remove second-reviewer and owner-only checks individually and must redden named assertions. Gate: `task gate:web`, `task gate:api`, `task gate:agent`, `task scan:secrets`.

## Acceptance criteria

- Only system auditor/reviewer sessions create completion blockers.
- A lead cannot self-dismiss, downgrade or defer a blocker.
- Fixed evidence is exact-head verified; invalid needs a different reviewer; deferred needs owner/admin reason.
- Any current open blocking finding denies a semantic permit.
- Both auto and manual MR rework inherit current contract/findings; legacy sources remain legacy.
- A governed rework performs a full exact-head contract/finding audit before completion.
- #1214 context availability cannot change the completion decision.
- Web/CLI expose bounded status/provenance without raw reviewer content.

## Review record

- 2026-09-09: Split from #1231 after final review found one child combining auditor, findings and MR rework too large. Reviewed seam uses the permit fields reserved by #1226.
