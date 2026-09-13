# PRD #1297: Prevent and salvage finalization blockers

**Issue**: [#1297](https://github.com/vtmocanu/uzi/issues/1297) · **Priority**: High
**Status**: Reviewed; held until #1296 is merged and accepted. Implementation has not started.
**Dependency**: [PRD #1296](1296-durable-run-recovery.md), fully merged and validated before this run starts. No dependency on enabling #1226/#1246's completion-interlock rollout: partial authority must be correct and tested with that interlock both OFF and ON.
**Evidence baseline**: `43f3fd61` (2026-09-12); recheck named symbols after #1296 lands.
**Handoff**: User-selected Auto mode with MR rework enabled, after #1296 and after lead/watcher review of both written PRDs.

## Problem and outcome

A run can report all implementation milestones complete before a publication blocker makes it terminally fail. In the motivating run, the local scanner supplied exact commit/file/line findings, but the worker immediately failed rather than returning a bounded correction opportunity. A maintainer eventually recovered and repaired the committed work outside uzi.

The user requested all four parts of one feature:

1. Detect publication blockers earlier, after milestones rather than only at finalization.
2. Attempt bounded, safe in-run correction while there is budget and context.
3. If full delivery remains blocked, automatically publish a valid, independently checked partial draft PR when the blocked work is separable.
4. Make incompleteness unmistakable in uzi and on the PR: what was omitted, why, and what remains for a human. Never automatically close the original issue or claim full completion.

The user also explicitly approved a clean independent partial when the excluded work contains a suspected real secret. That approval is NOT permission to obfuscate, reintroduce or classify the suspicious value as fake. The original stays quarantined under #1296; handling that secret remains a human action.

## Scope

### In

- Early committed-history secret checks and forge-specific workflow/capability checks at safe milestone/turn boundaries.
- A bounded blocker-feedback/repair loop with fresh rescans, normal budget accounting and an explicit finalization reserve.
- Safe repair of non-secret implementation/alignment blockers and owner-attested synthetic fixtures. No path-name-based or model-only secret attestation.
- Construction of a new, run-owned partial candidate that excludes blocked content AND blocked unpublished history, followed by real pre-publication checks.
- Native draft/WIP creation on supported forge configurations, a first-class terminal `partial` outcome and structured delivered/omitted/human-action metadata.
- Web, CLI, TUI, Slack/inbox, lifecycle, scheduling and rework handling for that outcome.
- Synchronous capture of full original H through #1296 before any partial publication; model-free retry/custody remains #1296's responsibility.

### Out

- Relaxing PAT scopes, force-pushing, bypassing push protection, adding `gitleaks:allow` to hide a finding, or changing the default branch directly.
- An automatic assertion that a secret is fake because it appears in a test, uses a variable named `fake`, or was described as synthetic by an LLM.
- Rewriting already-published history, replacing an existing MR with a different contract, or silently turning an MR-less task into an MR-producing task.
- Arbitrary scope reduction because the model ran out of time. A structural completion-interlock failure alone is not a publication blocker or permission to declare a partial.
- Claiming that every blocker admits a useful subset. If no candidate survives the required checks, archive and report the remaining human action without publishing a broken PR.
- Any real `.github/workflows/**` file creation/modification/commit in implementation or validation.
- The separate misleading secret-blocked "diff preserved" message fix, unless its already-merged wording must be integrated with the new UI. Never restore the unsafe patch to match old text.

## Binding decisions

### D1: Earlier detection is a worker responsibility

Reuse the existing committed-range scanner and workflow-path classifier; do not substitute a working-tree-only scan or a prompt asking the agent to run `gate:repo`. Scan from the verified publication base through the captured committed source head, so a secret introduced and later deleted still counts. A cheap delta may identify new findings, but the receipt that permits publication must cover the entire range being published.

Attach at safe, reaped milestone checkpoint boundaries, before publishing that checkpoint where the existing control flow permits it. Non-milestone code-producing runs get the equivalent end-of-implementation-turn boundary. Do not add authenticated forge fetches while the untrusted agent remains alive. The existing `reap:false` live checkpoint path cannot acquire new credential-bearing operations merely to run this feature.

Persist bounded sanitized blocker metadata keyed to source SHA and a stable finding fingerprint: class, commit, path/line/rule where available, first/last observation and disposition. No secret sample. `fail_origin` is diagnostic, not the classifier's authority: local scan, generic GH013 and an unknown remote ruleset rejection must remain distinguishable.

An unreadable/untrusted scanner is not a clean receipt. It blocks automatic repair acceptance and partial publication, with an explicit diagnostic. This feature does not silently rewrite the existing normal full-publication fallback policy; any change to that policy needs its own reviewed requirement.

Add concise worker-owned preventive guidance for credential tests: construct clearly synthetic values at runtime and never commit complete provider-shaped credentials. This supplements real scanning; it does not replace it or modify the development-agent roster.

### D2: Bounded repair is implemented, not deferred

On a repairable blocker, return a structured, nonce-fenced correction task to the same run/session instead of immediately terminally failing. The worker, not the model, decides the allowed repair class and budget. The task names the captured source/finding identity, required unchanged behavior and exact checks to rerun. Treat paths, rules and summaries as untrusted data, never shell instructions.

Default maximum: two repair rounds per run across all milestones and finalization attempts, persisted across resume so reclaiming cannot reset the cap. They consume the existing turn/iteration/cost/wall limits; do not grant unlimited extra execution.

Reserve archive/publication overhead PLUS the affected gate time, not a fixed ten-minute cap. Default overhead is 120 seconds; add 1.5 times the sum of worker-observed successful durations for the exact required gates in this run, counting serial component gates serially. When a gate has no trustworthy timing, use its approved bounded timeout; when none is specified, use an explicit configurable five-minute planning allowance per gate, not a claim that it will finish that fast. Estimate candidate construction and full-range scan with the same tiers: 1.5 times trustworthy observed duration, otherwise the approved operation timeout, otherwise an explicit configurable two-minute planning allowance for each operation. Record the chosen estimate and its inputs so a guessed model duration cannot become a receipt; the caller may configure a larger allowance before the run.

Carve this reserve from the worker's claim-time HARD wall/remaining allowance, not a mutable API `deadline_at`; raising a server budget is not evidence that the armed worker deadline moved. Stop starting implementation/repair work before consuming the reserve. Validation may use it, but never exceed the actual remaining run/turn/cost budget. If the required safe path cannot fit, go to explicit archive-only/action-required outcome with no partial push. M5 must test this branch with an observed slow gate, missing timing and exhausted budget; it is a real outcome, not a hidden skip. A fresh repair round is admitted only if it AND the remaining safe finalization path fit.

Each repair must produce a new head and pass a full-range rescan plus the affected approved gates. A repeated finding fingerprint/no changed relevant tree consumes no endless re-prompt loop: stop that repair class and attempt eligible partial delivery. Preserve source snapshots needed for recovery before rewriting unpublished history.

Non-secret repair may resolve implementation conflicts and dependencies using the existing isolation and Git primitives, then rerun checks. Reuse existing workflow-subtree/merge/rebase alignment for behind-only branches; do not rebuild a competing aligner or discard authored workflow work while calling the result fully complete.

For a suspected secret, the worker must NOT auto-obfuscate the value. A fixture-specific repair is allowed only after an explicit owner attestation bound to the exact source H and finding fingerprint, using a dedicated typed owner action. A test path, inline marker, ordinary model message or `auto_approve` cannot supply it. The action can arrive while the run remains active; a stale or terminal request conflicts rather than authorizing a different head. Replace an attested fixture with newly generated synthetic test data/runtime construction and remove the offending literal from every affected unpublished commit; a tip-only fix is insufficient. The worker still rescans and reruns tests afterward.

No new autonomous "this must be fake" classifier is introduced. Without an owner attestation, skip secret repair and proceed only to independently clean partial selection under D3-D5. Do not wait indefinitely for an answer on an Auto run. The partial outcome records the unresolved secret action for a human.

### D3: Candidate selection has explicit safety boundaries

Automatic partial creation is available only when the existing run contract permits a fresh MR and the worker can identify a run-owned, unpublished source range. This includes ordinary issue/prompt/self-improvement output and other existing profiles only when those conditions are actually met. Existing-MR `mr_rework`/branch-repair work and MR-less tasks retain their contract; they get early detection/repair/archive, not a silently substituted PR. `chat` and `judge` produce no code candidate.

Prefer a coherent retained set of the original run's NET changes, excluding the blocked files/hunks and any dependent additions that would make the remainder invalid. Whole-file exclusion is conservative but not automatically sufficient or mandatory: dependencies, renames, binary files, schema changes and their tests must be considered together. Record what was retained/omitted rather than assuming a clean prefix is always useful or buildable.

A last-blocker-clean milestone snapshot is a fallback candidate, not a proof. Persist such snapshots by run/capture identity, not a reused branch name, and validate the chosen prefix on the current target base like every other candidate. No useful retained changes means no partial PR.

Workflow-authored changes remain omitted from the partial and archived for a maintainer with the appropriate authority. Suspected-secret work may be omitted without classifying it as synthetic, including exclusion of all affected unpublished history. An unseparable secret or unresolved candidate scan means no publication. Base-align conflicts may admit a clean rebased subset; do not categorically declare them separable or inseparable by error label alone.

### D4: Build a fresh candidate, not a cosmetic tip fix

Let H be the full original source, B its proven fork/base and T the freshly fetched trusted forge target. Capture H using #1296's shared producer and synchronous reserve/build/verify/upload sequence; require its durable ready ACK before publishing a reduced result. Retry a lost ACK under the same capture ID. Do not add a second archive store or recompute bundle prerequisites from the runner's private `origin/main`.

Create candidate P on a new server-approved run-specific branch rooted at T. Apply the retained NET delta B..H onto T, not H's entire tree; copying the old tree would revert unrelated changes that landed on the default branch after the run began. Resolve conflicts only within the bounded repair budget. Do not overwrite the original tracking/capture ref.

A synthetic/squashed candidate commit is acceptable and preferred when it guarantees excluded unpublished commits are not reachable from P. The full original commit identities remain in the downloadable archive. Restoring `.github/workflows` or deleting a secret only in a new tip commit on top of H is not accepted as sanitizing the published history.

The synthetic commit message and PR body are worker-generated partial-delivery templates. No automatic closing references, inherited `Closes #N` footer, or agent-supplied closing instruction may survive in commit/title/body metadata. Reference the original issue non-closingly and identify H, P and the archive's owner-only location without embedding archive bytes or secret samples.

### D5: Validate P before pushing it

The archive ACK proves recoverability, not candidate quality. Before any partial push:

1. Verify the source/capture/claim binding and that the complete original remains available under #1296's retention contract.
2. Verify the branch is fresh and run-owned, its target is allowed and not the default branch, and the candidate includes no excluded source commits/content.
3. Run trustworthy secret scanning on the entire new publication range and the forge-specific workflow/capability check. A scanner error, unknown result or retained blocker fails closed for partial publication.
4. Run the affected approved build/test/format/lint/contract gates on P itself. Use the original approved gate contract, not commands weakened by the candidate. Retain baseline tests and do not delete assertions or gate configuration to make the reduced tree green.
5. Record the tested SHA, actual commands and results plus the retained/omitted assessment. H's earlier green gate does not transfer to P; merely opening a draft so CI can discover whether it compiles is insufficient.

Check out P in the run's untrusted workspace and execute its gates through a bounded validation phase of the EXISTING execution harness, retaining that harness's command-policy/hooks, environment filtering, UID/profile and lifecycle controls. A worker-owned gate allowlist and actual tool completion/exit receipts decide whether every required command ran; a model saying "green" is not evidence. Deny unrelated validation-phase tools, reject a changed HEAD or tracked tree, then reap the validation process tree BEFORE network Git. Validation consumes existing budget; it is not a new unrestricted worker `spawn` after the reap.

Name and preserve the underlying seams: `runnerCommand`/`runnerSpawn` in `agent/src/runner-uid.ts` (and the provider's `commandRootCommand` where applicable), with a scrubbed environment, bounded process-group lifecycle and `killRunnerGroup`. `self-improve.ts`'s check runner is an existing example, not proof of an always-active UID split. Partial publication is permitted on supported legacy single-UID profiles ONLY through the existing harness/policy path, accepting its already-documented isolation limits. Do not claim UID separation or add a raw hook-free gate runner; profile inability to execute that existing path yields archive-only, not an invented fallback. Never select the PAT-bearing `workerBoundaryCommand`, inherit credentialed `GIT_CONFIG_*`, or run repo gates/hooks/filters in the credential-holder's execution path or the API/broker.

Freeze gate COMMAND DEFINITIONS from trusted target T/the approved validation contract, not agent-weakened definitions in P; run them against P's code. If a candidate changes a gate definition, require its explicit validation-contract approval or exclude that change; never silently execute its relaxed replacement. Set the candidate workspace's comparison refs to verified T for diff-aware gates, not the runner's private resumed `origin/main`, without modifying the real default branch. Record command identity, tested SHA, exit result and duration plus a bounded trusted summary, never raw gate stdout/stderr in new DTO/log/WS/preview or validation trace reporting. Preserve diagnostic privacy when adapting an output-discarding runner. Missing dependencies/tools, timeout, interruption or a reused helper's `skipped` result is NOT a passing candidate gate; test each explicit failure path.

After a successful push, forge CI remains additional evidence and may still fail. Report that as `partial` with failing/pending CI, not as proof the original request is complete. Local candidate failure must not produce an unvalidated partial PR just to give the run a URL.

### D6: Separate partial authority from full completion

Do not route a partial through ordinary full-completion approval or fabricate complete milestone declarations. Add a narrowly scoped partial-publication authorization bound to current claim generation, original H, acknowledged capture ID, candidate P, server-generated partial branch and the omission-manifest digest. Its only authority is fresh draft partial publication for a confirmed supported blocker; it cannot authorize a normal PR, default-branch write, arbitrary repo/ref or issue-closing action.

Keep the existing structural completion interlock and its full-completion permit unchanged. Partial authority is independently correct when that rollout is OFF and when it is ON; M1 tests both and must not turn the rollout on as a shortcut. A denied full permit or missing milestone alone cannot obtain partial authority. A partial receipt never counts as a full permit, and cannot be replayed against a changed head/manifest or later claim. New workers and the API negotiate support; old workers cannot manufacture partial state through a loosely accepted extra field.

Authorize against a currently ready, unexpired capture, not a stale client ACK or a `ready` flag awaiting the expiry sweeper. Require enough remaining archive retention to cover the bounded publication operation and at least a 24-hour owner recovery window. If insufficient, refresh capture through the active run's authorized archive flow before publishing, or return actionable archive-only failure. An explicitly confirmed owner discard remains outside the automatic-retention guarantee.

Persist a first-class terminal status `partial` only once the partial PR actually exists at the expected candidate head and native draft/WIP state. A push/create/read-back failure instead preserves truthful failed/publication-pending metadata and the archive, with idempotent reconciliation of a possible already-created PR before any retry. Do not create duplicate drafts on a lost create response.

The structured partial-delivery record binds source/base/candidate SHAs, capture ID, PR identity, blocker classes, retained/omitted scope, delivered/omitted milestone IDs where known, checked SHA/receipts, outstanding human actions and optional unresolved-secret action. Unknown scope is explicitly unknown, never an invented complete milestone list.

`milestones_completed` remains the truthful monotone history of what the lead declared, not a delivered-code counter. Store delivered/omitted scope separately. Clear current in-progress markers on the terminal transition, release worker slots/active-run uniqueness, and preserve the archive. A later human merge of this partial PR does not retroactively turn the run into full completion or automatically close its source issue.

### D7: Forge draft semantics are resolved before handoff

The worker uses the TypeScript `ForgeClient.createMergeRequest` seam and its three drivers, not a new Go-side finalize implementation. Extend the neutral create/result types with draft intent and verified draft result; ordinary non-partial creation remains unchanged.

Authoring-time public documentation/schema checks, 2026-09-12:

| Forge | Create-time mechanism | Required verification and boundary |
|---|---|---|
| GitHub | `POST /repos/{owner}/{repo}/pulls` accepts `draft: true`; response has `draft`. | Require `draft === true` at the expected head. Availability depends on repository/account plan; refusal is not permission to create a normal PR. [REST documentation](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request) |
| GitLab | A title beginning `Draft:` marks an MR as draft; the MR response has `draft`. | Use the documented title form, verify response `draft === true`. Title-validation policy may refuse it; do not strip the prefix and retry as normal. [Draft documentation](https://docs.gitlab.com/user/project/merge_requests/drafts/) and [MR API](https://docs.gitlab.com/api/merge_requests/) |
| Forgejo | WIP is title-prefix-based; documented defaults are `WIP:` and `[WIP]`, case-insensitive and configurable. Current public OpenAPI `CreatePullRequestOption` has no draft request boolean; `PullRequest` does have a response `draft` boolean. | Use an explicitly configured/verified WIP prefix for that forge connection, then verify returned `draft === true`; do not pretend an unsupported `draft:true` request field works. Unknown draft capability disables automatic partial creation for that connection and surfaces action, rather than silently creating an ordinary PR. [Configuration](https://forgejo.org/docs/latest/admin/config-cheat-sheet/) and [OpenAPI schema](https://codeberg.org/swagger.v1.json) |

Forgejo's public `/settings/repository` schema does not expose its WIP-prefix configuration. Do not assign the offline worker an investigation to discover it or infer it from a title alone. The implementation must expose the explicit connection-level prefix/capability setting and document its verification requirement. Test both standard configured WIP behavior and unknown/mismatched configuration; ordinary forge use must remain available when partial drafting is unsupported.

Create-time intent is mandatory; create-normal-then-mark-draft is not the chosen mechanism. Verify native draft state after creation as defense in depth. If a supposedly configured forge returns a normal PR unexpectedly, fail closed: close only the newly created run-owned PR, surface the mismatch and keep the archive. Never adopt or alter an unrelated existing PR to hide the failure. Document that external forge/config changes remain outside uzi's atomic transaction.

### D8: Incomplete must be visible everywhere

Render `Partial delivery` as its own terminal outcome on the board/run page, CLI list/get/watch, TUI, Slack notification and inbox. Do not hide it behind a generic success icon, a green complete badge, or a failure filter that makes the useful draft disappear. Show the PR link, what remains, why, and how the owner can retrieve the full original.

On title-prefix forges, the PR title's literal FIRST token is `Draft: ` or the configured WIP prefix; the partial marker follows it, never precedes it. GitHub uses its native draft flag plus an explicit partial title marker. Extend the neutral `MergeRequest` result and `parseGitlabMr`/`parseForgejoMr`/`parseGitHubMr` to preserve verified draft state rather than inspecting a title alone. The body leads with an incomplete banner, retained and omitted scope, fresh candidate validation, outstanding human actions and a non-closing link to the source issue. Secret notices identify sanitized file/line/rule where available, never secret samples. Describe the archive as owner-only AND time-bounded, with its expiry/discard semantics; the permanent PR is not a promise of permanent archive retention. Do not imply every PR reader can download it.

A real or unproven secret gets explicit revoke/rotate/review-history guidance. An owner may attest a genuine synthetic fixture for repair, but neither `auto_approve` nor an ordinary model/steering response can perform that attestation. No new automatic provider credential checks or secret-validity network requests are introduced.

Update the entire current run-status vocabulary and every terminal/active/wait predicate, including API/SQL, protocol fixtures, worker, web, CLI/TUI, controller busy/drain, notifications, judge eligibility, usage/history and scheduling. Pin the status vocabulary to the live DB CHECK constraint and its agent/web mirrors, following the existing run-kind parity discipline rather than copying its values. M1's inventory must name the constraint and BOTH mirrors, not only Go consumers. Do not copy an outdated enumerated status list from prose. `partial` counts as terminal execution but neither full success nor failure; expose it separately in outcome counts and explicitly update any affected rate denominator rather than silently counting it as `completed`.

### D9: Automation must not erase the partial contract

Automatic issue sweeps, CI-autofix and MR-rework must not treat an unresolved partial as a successful completed run or silently reintroduce omitted work. Default automatic follow-on runs for a partial lineage are suppressed with an actionable reason. An owner can deliberately start new work; this PRD does not make an automatic loop that repeatedly attempts the same impossible workflow/secret publication every night.

If an already-queued follow-on reaches claim after the partial outcome was recorded, recheck lineage and refuse unsafe automatic work. Ordinary full-run rework settings are unchanged, including the enabled setting used to implement this PRD itself. A human editing/removing a draft title does not clear the persisted partial contract or authorize uzi to close the original issue.

Manual work on a partial must preserve its declared omissions unless the owner explicitly commissions the remainder. Do not widen a generic `run rework` command into automatic full completion merely by accepting `partial` in a status list. The owner may recover/scrub the full original through #1296 and land remaining work manually or via a new explicitly authorized run.

### D10: Rollout and offline validation

Ship the API status/schema/authorization and controller/client understanding before enabling partial publication on workers. Capability negotiation keeps old workers on their existing execution path; unsupported clients render an explicit upgrade/unknown state rather than misclassifying partial as success. Do not weaken status-schema checks to accept arbitrary new strings.

All implementation investigation is repository-local or already resolved in D7. New workflow-shaped fixture histories are built as bare Git objects or synthetic path data, never real files under `.github/workflows/`. Synthetic scanner fixtures are assembled at runtime; never commit a complete provider-shaped credential. Run `task scan:secrets` with its positive canary evidence alongside affected component gates.

Every actual implementation bug fix needs an observed failing-old/passing-fixed regression. A negative-only assertion, skipped required case, zero matched tests, or a local mock presented as live forge/k8s proof is not acceptance.

## Milestones and dependency plan

Eight WORKER milestones, all four requested stages included, followed by unnumbered maintainer acceptance. Freeze only M1-M8 into the worker plan. M8 completes with deterministic integration proof and a runnable exact-revision handoff recording unrun live proof as owed; the worker can then open its implementation PR. Overall PRD acceptance and merge still require the lead's real hosted/forge evidence, not a fabricated worker checkbox. Migration numbering is assigned at merge above the live head. New filenames below are proposed ownership boundaries, not existing artifacts.

| Phase | Milestone | Depends on | Files/ownership | Repo |
|---|---|---|---|---|
| 0 (dependency) | #1296 accepted | Entire #1296 | Published archive/custody/claim contract | uzi |
| 1 (sequential) | M1: partial outcome and authorization contracts | #1296 | New partial-delivery store/service/protocol types, migration, status registry/fixtures; freeze decision and receipt schemas | uzi |
| 2 (parallel) | M2: early blocker observation | M1 | New agent blocker module, checkpoint/turn hook, worker-owned preventive guidance and agent tests | uzi |
| 2 (parallel) | M3: native draft adapter support | M1 | `agent/src/forge.ts` and driver tests; Forgejo prefix/capability contract and connection configuration owned here | uzi |
| 3 (sequential) | M4: bounded in-run repair and owner attestation | M2 | Agent repair/session flow, persistent round/budget accounting, typed owner decision handlers/CLI; serialize shared runner/protocol edits | uzi |
| 3 (sequential) | M5: archived, validated partial publication | M3, M4 | Agent candidate Git plumbing and finalizer, partial-authority API integration, archive receipt consumption and per-class tests | uzi |
| 4 (parallel) | M6: unmistakable partial UX | M5 contract | Web, CLI/TUI display and Slack/inbox RENDERING/templates/tests; owns `slacksvc` rendering edits, no notification/poller/query policy edits | uzi |
| 4 (parallel) | M7: lifecycle and downstream policy closure | M5 contract | Workersvc/poller/rework/scheduler/query POLICY (when to notify, judge eligibility, counts), controller terminal/busy and lineage guards; no M6 template edits | uzi |
| 5 (sequential) | M8: integrated tests, docs and acceptance handoff | M6, M7 | Deterministic cross-component scenarios, docs/ADR/spec sync, Task targets and exact-revision runnable proof package with owed entries | uzi |
| 6 (maintainer acceptance, not a worker milestone) | Live hosted/forge proof before merge | M8 candidate | Lead-owned isolated runtime, native draft verification and exact-head CI evidence | uzi |

Parallel phases require separate files. Assign shared registrations and fixture updates to one milestone; serialize an overlapping edit rather than treating two writers as independent. If a notification file needs both M6 rendering and M7 policy changes, M7 owns that shared file and M6's consuming integration follows it; do not run two writers against it. Join before serial component gates. Do not rerun a gate on the same tree to obtain another milestone's copy of its output.

- [ ] **M1: Freeze the truthful outcome contract.** Add `partial` and its structured record, claim/head/capture-bound partial authority, owner-attestation schema and supported lifecycle transitions. Inventory current status consumers and required updates so later phases cannot omit one. Prove terminal partial requires an actual verified run-owned draft and cannot obtain full completion authority. Gate: `task gate:api`, live-store transition tests, `task scan:secrets`.
- [ ] **M2: Catch blockers before finalization.** Scan committed publication history at safe milestone/end-turn boundaries, preserve sanitized head-bound findings, record genuinely clean snapshots and add preventive fixture guidance. Test introduced-then-deleted secrets, authored versus merely stale workflows, unreadable scans, resumed generations and the live-agent/no-credentialed-fetch boundary. Gate: `task gate:agent`, `task scan:secrets` with positive canary evidence.
- [ ] **M3: Correct draft creation on each forge.** Extend the existing agent forge seam, preserve normal-request behavior, and implement native request/result checks for D7. Add explicit Forgejo prefix/capability configuration rather than pretending the public API advertises it. Test three drivers, unknown capability, rejected draft request, unexpected normal response and idempotent own-PR reconciliation. Gate: agent and any touched API/web component gates serially, plus `task scan:secrets`.
- [ ] **M4: Actually repair in-run within bounds.** Implement same-session feedback/resume, two-round persisted cap, finalization reserve and fresh full-range validation. Add strict owner head/finding-bound fixture attestation, never an auto-approved ordinary question. Prove non-secret correction, authorized fixture replacement, unknown-secret refusal, stale attestation rejection, repeat/no-progress exit, restart cap preservation and normal budget exhaustion. Gate: API and agent gates serially, CLI/router owner-action tests, `task scan:secrets`.
- [ ] **M5: Publish only a verified retained candidate.** Capture original H through #1296 and require its durable ACK. Build P from T plus retained B..H delta, excluding prohibited history as well as content; acquire only partial authority, run P's actual approved gates, then push a fresh branch and create/verify the draft. Test both clean-subset and checked-prefix paths, unrelated default advancement, omitted dependencies/migrations/tests, secret-containing deleted commits, archive outage, duplicate-create response loss and no-useful-subset fallback. Gate: API/agent gates and the deterministic cross-component publication scenario, serially; `task scan:secrets`.
- [ ] **M6: Make partial delivery impossible to mistake for completion.** Implement board/run, CLI, TUI, Slack/inbox and PR-body presentation with retained/omitted scope, human actions, archive link authorization and fresh validation SHA. Preserve historical milestone declarations and show delivered scope separately. Test narrow/dark/light/no-color rendering as relevant, unknown scope, malicious labels/paths, no raw secret sample and no closing-reference metadata. Gate: `task gate:web`, `task gate:api`, `task scan:secrets`.
- [ ] **M7: Close lifecycle and automation gaps.** Update the complete terminal/active/wait vocabulary, uniqueness/claim/drain semantics, notifications/judge/outcome statistics and unresolved-partial automation gates. Prove Auto-mode question handling cannot attest a secret, follow-on rework cannot silently convert partial to full, queued follow-ons recheck lineage, and future sweeps do not loop on the same unresolved partial. Normal full-run rework remains unchanged. Gate: API/controller/agent gates as touched, serially, with live-store consumer tests and `task scan:secrets`.
- [ ] **M8: Prove integration and hand off the four-stage live journey.** Execute deterministic positive controls and discriminating mutations for early detection, real bounded repair, archive-before-partial, exact P preflight/gates, draft request/result validation, terminal partial reporting and suppressed unsafe follow-ons. Document supported/unsupported split cases and owner secret decisions; write ADR 1297-partial-delivery-contract, sync approved specs and `ARCHITECTURE.md`, and run `task docs:sync` for user docs. Run touched component gates serially, `task gate:repo` and `task check-docs:web`. Package the runnable exact-revision hosted/native-forge scenarios with an honest owed ledger; that completes the worker's M8 responsibility. The lead's separate acceptance phase verifies actual native draft behavior, exact-head CI and primary hosted-k8s operation on isolated resources before merge. Compose is additional, not a replacement; no worker checkbox claims unrun live proof.

## Success criteria

1. A blocker first introduced during implementation is detected at the next eligible checkpoint/turn boundary, not only after the final completion signal. Detection identifies the actual committed source range.
2. A repairable non-secret case completes a real bounded correction round; an owner-attested synthetic fixture is repaired without an unknown/real secret being silently blessed. All accepted repairs rescan the full publication range and consume existing budgets.
3. At least one workflow-blocked case and one suspected-secret case produce an independently clean, locally gate-passing partial draft while preserving the full original archive. This is an observed path, not merely a helper unit test.
4. A blocked commit removed only at the tip, an unreadable scan, an invalid reduced tree, an archive without a durable ACK, or missing partial authority prevents publication.
5. Candidate P preserves unrelated changes newly landed on T; blocked source commits are not reachable from P; original H remains exactly recoverable through #1296.
6. uzi, CLI/TUI, Slack/inbox and the PR all show partial delivery and remaining work. No full-success badge, automatic closing reference, fabricated delivered milestone or replayable full permit appears.
7. `partial` is terminal for resource/claim/wait purposes, but never silently treated as completed/full success. Existing full-run behavior and guardrails remain intact.
8. Native draft intent/result is tested on all supported configurations; unsupported draft creation produces an explicit recovery/action outcome, never an ordinary PR presented as a draft.
9. Unresolved partials do not trigger repeated autonomous repair/sweep loops or silently reintroduce omitted work. A deliberate human action is required to commission the remainder.
10. Both implementation and validation keep real workflow files and real credentials out of this branch. Required tests/gates, exact-revision CI and primary hosted acceptance are documented before this PRD is accepted.

## Risks and accepted boundaries

- **Partial is not automatically useful.** Candidate assessment plus fresh gates decide; splitting may be impossible. Archive-only is the honest fallback.
- **Secret removal is not secret remediation.** Excluding suspect work protects the partial publication, but does not revoke a credential or prove it was never exposed elsewhere. The owner action remains prominent.
- **Automated scope extraction can remove too much or too little.** Preserve the original, disclose exact omissions, require current-candidate gates, and refuse uncertain safety instead of replacing user intent with a quietly reduced deliverable.
- **Draft support differs by forge.** D7 pins verified semantics and a fail-closed unsupported path; the offline worker must not guess new API fields or title-prefix policy.
- **A new terminal outcome has a broad integration surface.** The status-consumer inventory and M7's real lifecycle tests are mandatory, not a search-and-replace of the main enum.
- **A finished worker PR is not live acceptance.** Maintainer k8s/forge proof may remain owed while the worker completes its implementation; the lead must not claim that evidence ran remotely when it did not.

## Decision and progress log

- 2026-09-12: The user explicitly requested all four stages, both PRDs, sequential Auto dispatch with MR rework enabled, automatic safe partial delivery and lead-led watcher review after both drafts are written.
- 2026-09-12: The user explicitly allowed an independently validated partial that excludes suspected-real-secret work entirely. Repair/reintroduction of that work remains a human decision; obfuscation is not an authorized fix.
- 2026-09-12: Lead decisions preserve full original H under #1296, build a fresh candidate rather than cosmetically fixing H's tip, rerun local candidate gates before push, retain truthful historical milestone declarations and use a first-class terminal partial outcome.
- 2026-09-12: Written-PRD review fixed actual harness-based validation, measured reserve inputs, interlock-off/on proof, native draft-prefix/result handling and render/policy file ownership. The lead chose explicit owner-attested fixture repair rather than an unproven autonomous synthetic classifier. Eight worker milestones end in a runnable acceptance handoff; live hosted/native-forge proof remains required before merge.
