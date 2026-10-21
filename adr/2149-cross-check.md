# ADR-2149: Server-enforced plan cross-check

**Status**: Accepted (implementation); hosted acceptance pending
**Date**: 2026-10-06
**Issue**: [vtmocanu/uzi#2149](https://github.com/vtmocanu/uzi/issues/2149)
**Extends**: [PRD #2149](../prds/2149-plan-cross-check.md), especially D1, D4–D7 and D16. The PRD remains in `prds/` until maintainer-owned hosted acceptance is complete.

## Context

An opted-in auto-approved Claude lead needs a Codex second opinion before implementation. A model verdict alone cannot authorize a different plan, a later claim, or a human revision. The checker also needs repository reads without implementation tools or access to another run's data.

## Decisions

### Separate child, existing custody

The server creates a report-only `cross_check` child and its plan-stage `cross_checks` row atomically through the existing harness resolver. The Claude lead keeps its Claude credential; the Codex child uses its own resolved Codex credential and existing capability, epoch, revocation and usage paths. The worker holds the forge credential for checkout. The child checks the immutable base commit in a checkout keyed by its own run id; it does not publish or enter the lead's implement/review loop (`agent/src/cross-check-runner.ts`).

The child uses ordinary run slots, with expedite priority 2. The lead retains its slot while waiting. A pending check's elapsed time is excluded from the lead's wall budget and banked on settlement by the existing cross-check lifecycle writers in `api/internal/store/queries/runtime.sql`. This does not establish a dedicated checker lane or a durable slot-releasing wait.

### Exact-plan authority stays on the server

`api/internal/workersvc/cross_check.go` normalizes and stores the candidate and computes its digest over plan, milestones, capabilities, tools, size class, base commit and planning diff. In the original #2149 round-1 scope, same-generation retries recovered the immutable attempt, including a decided attempt; a mismatched candidate or generation could not create a second round. The dated #2150 extension below replaces that round-1 restriction while preserving exact-identity retry checks.

`SetRunAutopilotPlan` in `api/internal/store/queries/runtime.sql` binds the latest plan-stage APPROVE to an opposite-harness `cross_check` child, current lead claim generation, server digest and matching approval-bearing fields. It freezes those fields in the guarded write. `SetRunRunning`, completion and progress guards prevent bypass through adjacent state reports. These are server storage/lifecycle guarantees; they cannot prevent arbitrary execution by a worker that ignores a refusal. Capability claim gates exclude unaware workers, and the worker treats a refused handoff as fatal.

### Bounded fallback, terminal delivery exceptions

In the original #2149 scope, REVISE, BLOCK, verdict deadline, checker model timeout, malformed result, model/confinement error, unavailable checker, interrupted attempt, candidate/diff refusal and submit failure normally forced the human plan gate. #2150 adds bounded automatic REVISE and narrowly eligible recovery as described below; the remaining dispositions stay in force. A checker pass is not a substitute for an acknowledged exact-plan handoff.

`agent/src/plan-cross-check-gate.ts` and `agent/src/runner.ts` fail closed on delivery losses:

- Irrecoverable preparation receipts: `plan cross-check: preparation receipts irrecoverably lost`.
- An unrecoverable human presentation acknowledgement: `plan cross-check: human-presentation ACK unrecoverable`.
- After three preparation attempts and an acknowledged forced human gate, unresolved preparation acknowledgements: `plan cross-check: preparation ACKs unrecoverable`.

These failures terminate execution rather than entering `recovery_wait`, retrying indefinitely, fabricating receipts or approving. D16 preserves execution-local established presentation/revision context: a parked status proof cannot mint, adopt or overwrite a human presentation. Human approve/revise/reject continues through the established gate; revision uses the existing revision path, with the original checker candidate retained as historical evidence. Status reads grant no approval authority. A parked result does not release/reclaim the lead, enter `recovery_wait`, mint a presentation from the checker candidate or trigger a new check of the human-revised plan.

### Read-only repository access has explicit limits

`agent/src/codex/cross-check.ts` exposes Read and bounded Search through immutable broker grants, with a 200-call ceiling, no Bash, patch, skills or delegation. Native shell is disabled and `project_doc_max_bytes = 0` disables automatic repository instruction loading. Issue, candidate and diff are nonce-fenced untrusted data, not privileged guidance.

The fileop helper and provider process both enter the required cross-check confinement posture (`agent/codex/supervisor/cmdsandbox/cross_check.go`, `agent/src/codex/launcher.ts`). Checkout access is read-only; sibling files and escaping symlinks are denied. Broad shared writable filesystem/device access is denied, while the explicitly granted private launch tree remains writable: home, Codex home/auth/config/sessions, XDG config/cache/data/state and tmp. System/toolchain read grants and `/dev/null` writes remain necessary. This is not a claim of zero filesystem writes or network isolation.

### Evidence display does not confer authority

The owner-scoped summary read validates child eligibility, normalizes bounded findings and reports recorded claim model/effort and child usage (`api/internal/workersvc/plan_cross_check_summary.go`). The web shows current gate reason separately from checked-candidate outcome, labels historical evidence, and uses hardened Markdown capped at 16,384 source characters and 20 items with a truncation notice (`web/src/components/PlanCrossCheck.tsx`). Unknown or inconsistent outcome pairs show unavailable. Metadata is bounded/sanitized, child links require a valid UUID, and checker cost shows as an API-equivalent figure; a checker with no cost estimate or with unreported cost shows no dollar figure (PRD #2559). New plan cross-check events trigger a bounded detail refresh (`web/src/pages/RunView.tsx`). CLI fields use `Plain`; Slack carries the reason without findings.

## Proof boundaries and fixture ownership

The existing harness is unchanged and **NOT RUN in this documentation pass**. Its allocation and cleanup contracts are:

- `Taskfile.yml` allocates a unique `mktemp -d .uzi/scratch/cross-check-proof.XXXXXX` directory for fresh sandbox/supervisor/fileop binaries and removes that directory with its own EXIT trap.
- `agent/test/cross-check-confinement-probe.mjs` allocates `confinement-*` under `.uzi/scratch`, builds its own checkout/private/sibling/symlink fixtures and removes its base in `finally`. Its unconfined control must fail the same isolation assertion that the confined direct process passes.
- `agent/test/cross-check-confinement-packaged.probe.ts` allocates `packaged-confinement-*` under `.uzi/scratch`. Its synthetic shared parent permits traversal by the real identities; that parent does not grant the confined child broad sibling access. Private grants belong to each launched identity. Each test disposes its own process handles, removes its own state/base, and, for fileop-owned state, uses the command identity for cleanup.

The three packaged proofs cover different boundaries: fileop Read/write/traversal/symlink enforcement independently of broker admission; direct provider filesystem enforcement through the real launcher with a trusted fixture child; and packaged Codex config/auth/session creation and credential/session custody. The session probe uses a synthetic credential and performs no model turn or external call. It is **not authenticated model-turn or network-isolation proof**. These inherited fixtures are documented as found; new committed tests/scripts must allocate in system temporary storage rather than copy this scratch layout.

## Validation provenance and remaining scope

Maintainer-provided provenance reports PR2358 reviewer/tester/auditor validation at `631b28a1`, 2262 LiveDB tests and capable-host confinement PASS run by the lander. Those results were not observed or rerun here. Web M1 is committed at `e45d6cb` (implementation `bb0efb31`); the dispatch records `gate:web` PASS (5641 unit tests / 299 files, 8 Chromium tests / 2 files; docs checker PASS) and `npm run build` PASS. The initial web-gate lint failure was fixed and the gate rerun. The build ran before the test-only lint fix, which changed no production source.

The lander retains the capable-host confinement merge gate against the PR head. Hosted acceptance remains maintainer-owned in acceptance issue 2152. Automatic checker revision, the Codex-lead direction, dedicated checker slots, checker model/effort pins and code cross-check were outside the original #2149 implementation; #2150 extends automatic revisions below. The other exclusions remain. Observed at documentation commit `ba73044a`: `task docs:sync` completed; `task gate:web` PASS (5641 unit tests and 8 Chromium tests, including `check-docs:web`), `npm --prefix web run build` PASS, `task gate:repo` PASS and `task gate:api` PASS, including the embedded-docs package. Secrets scanning reported zero tracked-file findings and detected both canaries; SAST reported zero findings and detected its canary. The read-only docs reviewer reported no mandatory or optional findings. These are component-gate results from that #2149 documentation pass, not a replay of prior LiveDB or capable-host confinement proof. No runtime source or confinement harness changed.

## Amendment 2026-10-19 — #3123: existing advisory code-snapshot retry

This amendment concerns the existing advisory code cross-check completion path,
not plan-check rounds. If the initial done checkpoint leaves no usable snapshot,
its existing snapshot request can freshly quiesce and reimport once. It retains
the exact pre-import head, or, if that was unreadable, the head supplied by a
successfully settled normal completion-record callback. A null or moved candidate
gives `snapshot_failed`; a fresh current head cannot substitute for it. Before
pinning, the reimport must prove the full producing context, claim generation and
committed tracking receipt. Ordinary snapshot failure and a clean soft stop
produce SHA-free incomplete evidence.

The explicit reaped Codex path with `executor.safety` gives the initial done
checkpoint and this existing fresh request independent absolute 60 s soft /
90 s hard allowances. Their combined duration may approach 180 s, without
resetting the clock per command. This adds no model turn, completion attempt,
checker invocation or repair rerun: one advisory check and one repair remain
the policy. Finalize independently imports authoritative work under its own
guards and unchanged 50-minute boundary; it is not this snapshot retry.

Hard Codex boundary errors, Git hard aborts and unsafe cleanup propagate through
both the import helper and the snapshot advisory catch. Shutdown/lifecycle abort
prevents fresh admission. See [ADR-1597's safe points and residuals](1597-midturn-checkpoint-durability.md#amendment-2026-10-19--3123-completion-budgets-and-safe-stops).
Source and local test evidence do not establish hosted acceptance or resolution
of the reported production timings.

## Planning-diff capture: symlinks, gitlinks and budgets (2026-10-07, #2410)

Capture budgets are split: the object-store snapshot copy has its own 128 MiB budget, streamed in 16 KiB chunks that yield on a roughly 20 ms clock so SIGTERM cleanup runs inside the 2 s kill grace, and the source reads (worktree compares and verified object reads) keep a separate 128 MiB budget. Measured once during #2410 on a clone of this repository at 24f28b1d through a real TickSpawner: snapshot 74.3 MiB, source 99.7 MiB (about 78% of its budget, the tighter headroom), peak temporary tree near 74.3 MiB, wall 6.6 s against the 28 s deadline.

Symlink posture: an unchanged tracked symlink is accepted by hashing its raw target against the base blob through a pinned parent descriptor; targets are never opened or followed. Added, removed, retargeted, file/link-swapped and untracked non-ignored symlinks refuse with `unsupported_entry`, and a symlinked `.gitignore` is treated as absent, as Git does.

Gitlinks (mode 160000 at base or index, changed or not) also refuse with `unsupported_entry`. Support is deferred: equal base and index gitlink ids do not prove `git add -A` publishes nothing, since a moved submodule HEAD or a removed directory is staged. It needs a proof that the worktree preserves the base gitlink under publication. Refusal sub-codes are persisted and shown with the gate reason (see `docs/cross-check.md`).

## Automatic rounds extension (2026-10-07, #2150)

[PRD #2150](../prds/done/2150-plan-cross-check-auto-revise.md) extends Claude-lead/Codex-checker plans with bounded automatic REVISE. It leaves #2149's ordinary slots, credentials, confinement, terminal D7 and established-human-gate D16 contracts intact. Codex leads remain unsupported; stage-specific pins, dedicated slots and Code cross-check remain outside this extension. This is not a recertification of #2149's confinement or hosted acceptance.

### Round protocol and immutable budget

Submit holds the current owning lead/generation lock. An omitted requested round means legacy round 1. Same lead/generation/round and canonical candidate digest replay the immutable attempt even if decided; a conflicting candidate or generation at that round refuses. Identical text in a new round still needs an explicit requested round. The exact-plan storage guard binds the latest eligible APPROVE, current generation, digest and canonical approval-bearing fields; stale child verdicts and stale approved-plan writes refuse.

The first candidate snapshots `automatic_rounds_enabled` and `automatic_revision_limit` from claiming-worker capability and `PLAN_CROSS_CHECK_MAX_REVISIONS` (default 2, integer 0–4). Subsequent candidates copy them; configuration changes affect new checks only. The global server-counted budget is limit + 1 candidates across generations, including superseded attempts, with a fresh deadline per candidate. No automatic round spends human `runs.revise_count` or `PLAN_MAX_REVISIONS`. Older workers snapshot false/0 and park REVISE as changes requested; capable workers with limit 0 park REVISE as `plan cross-check: revisions exhausted`.

`cross_check_rounds_v1` is a durable protocol capability for automatic leads and round > 1 child claimability/provisioning, independently of the runtime kill switch. The current-worker-authorized latest GET returns metadata and next-round recommendation only: no candidate, findings, approval proof/grant or writes. Admission and settlement remain server-side write checks, not authority conveyed by this read.

### Preserve decided fallback

The maintainer's **2026-10-07** decision allows fresh-round recovery before an established human gate or durably approved plan for pending interruption strictly before deadline (including lifecycle-settled failed/superseded with proven pre-deadline origin), decided REVISE, and APPROVE never durably stored, marked `approved_not_stored`. They consume the existing budget. Otherwise eligible exhaustion parks as exhausted. Decided BLOCK, timeout and other check failures create no fresh row/child and retain their own reason even when budget is spent; D7 delivery losses stay terminal. D16 human context cannot be adopted from evidence or turned into a fresh check; durable approved plans use existing resume behavior.

The first custody-invalidating transition supplies the authoritative interruption time: direct DB writers use transaction `now()`; frozen writers use the server-provided `now` of that same transition. Persisted `interrupted_at`/decision evidence controls eligibility after lifecycle settlement. A later sweep clock cannot grant recovery; equality and ambiguous legacy evidence fail closed to timeout. See [the requirements matrix](../specs/human.md) for dispositions.

### Settle before automatic advice

Validated usage/preparation/reconciliation must successfully release the actual `reservation.release` before the checked-state barrier is released and awaited successfully. Only then may the lead revise. The automatic path returns `{kind: "revise", feedback, automatic: true}` with no `inputId`; it does not send `awaiting_approval`, use `releaseAppliedGate` or establish a human presentation. SDK/stub executors count human revisions separately and record automatic checker-round identity in the feed. Human input cannot forge automatic provenance.

`buildAutomaticRevisionPrompt` is separate from the human builder and stage-neutral. Checker advice and optional prior-plan context receive separate nonce fences; trusted framing calls the advice potentially wrong or hostile and requires verification against code, issue and uzi rules, declining conflicts. This shared builder does not imply Code cross-check is implemented. Summary evidence describes the latest checked candidate; earlier rounds remain feed history.

## #2460 addendum: Codex lead, Claude checker

[PRD #2460](../prds/done/2460-codex-lead-plan-cross-check.md) extends the gate to a Codex lead checked by a read-only Claude child. The sections above stay as written; where they say Codex leads are unsupported, this addendum supersedes them for workers advertising `cross_check_codex_lead_v1`. Nothing here is hosted-acceptance proof.

### Opposite family, derived and guarded

The checker is always the opposite family of its lead (`claude` lead, `codex` child; `codex` lead, `claude` child), derived server-side from the lead's harness and never chosen by the caller. There is no fallback across families: a same-family checker would defeat the second opinion. Migration 00314 widens `runs_kind_shape` so a `cross_check` run may carry `claude` as well as `codex`. That the child differs from its lead is a cross-table fact a `CHECK` cannot state, so `CreatePlanCrossCheckChild` and `InsertPlanCrossCheck` are guarded writes that refuse a same-family or unknown-family pair.

### Single-family custody

A child holds only its own family's credential. Claim assembly refuses a Codex child that carries an Anthropic token or lacks Codex credentials, and a Claude child that carries Codex credentials or lacks the Anthropic token, mirroring #2149's Codex-only rule so a checker can never act as the lead's family.

### Checker confinement layers

The Claude checker stacks: SDK `tools` of `Read`, `Grep` and `Glob` only (not `allowedTools`, which does not restrict under `bypassPermissions`); every other tool disallowed; `settingSources` empty with plugins, skills, MCP servers and agents off; a fully isolated environment holding only the Anthropic token; execution as the runner uid via `spawnDetached`; a single path guard that allows reads only inside the checkout plus the session's own SDK spill files under its home; and an init confinement latch that fails the check with `confinement_failed` when confinement cannot be confirmed. The Glob/Grep pattern guard, which denies absolute, home-relative and `..`-segment patterns in every brace expansion (and patterns with a backslash, unbalanced braces, more than 32 brace groups, more than 1024 characters or more than 256 expansions; a guard error denies), is defense in depth: the path guard screens only path fields and cannot see a Glob `pattern` or Grep `glob`, and the guard does not catch a pattern that walks an in-checkout symlink. The runner uid is the boundary, not the pattern guard.

### Claim finalization lock order and empty-pool disposition

Every Claude `cross_check` claim takes the lead lock before the child lock, including a successful assembly, matching the verdict path. A Claude child whose credential is unavailable or disabled, or whose automatic pool is empty, settles the check `checker_unavailable` (child failure `plan cross-check: checker unavailable`): the lead's pending wait is banked once, the lead takes the human gate, and no credential is delivered. This deliberately differs from other lanes, where an empty automatic pool is a transient `pool_wait`; the non-pooled default credential is never spent on a checker. A locked vault stays transient. A pinned Claude model the account cannot use (SDK `model_not_found`, only when the model came from a pin) is likewise checker unavailable.

### Rollout boundary

`cross_check_codex_lead_v1` is advertised by new workers unconditionally and is required to claim a Claude child (all claim, placement, health and provisioning copies) and for a Codex lead's submit. Roll the api and migration first. A new api adds the additive claim field `plan_cross_check_codex_lead` (`PlanCrossCheckCodexLead`, set when the run requires a check and is on the Codex harness). A new worker that does not receive it (an older api) skips round discovery, does not submit, and parks the Codex lead at the human gate with `codex_lead_unsupported`, the same park as a Codex lead on an older worker; no 409 refusal is mapped to this fallback. Accepted rollback boundary: if the api is rolled back while a claim that carried the field is in flight, the worker submits to the older api and hits the old failure (the run fails). The api is a single-replica Recreate deployment (`deploy/chart/templates/api-deployment.yaml`), so only an operator rollback can cause this, and it affects only Codex claims in flight at that moment. Strict goose does not prevent it: an older api boots against a database already at 00314. The worker fallback can be removed once the minimum supported api sends the field. A worker with `WORKER_CROSS_CHECK_SLOTS=0` (hosted: `UZI_WORKER_CROSS_CHECK_SLOTS=0`) takes no checker children; if no other worker of the user can, the check waits to its deadline and the lead takes the human gate (both families). Migration 00314's Down deletes Claude checker runs (history is kept with `checker_run_id` NULL); run it with the new api stopped. The plan-gate Codex executor consumes automatic rounds without spending its human revision budget and implements only the acknowledged checked plan.

### Not shipped

Code cross-check was out of scope here and is now implemented by #2170 (see [PRD #2170](../prds/2170-code-cross-check.md) and [Cross-check](../docs/cross-check.md#code-cross-check)). A Claude child claims through the dedicated cross-check lane of #2169 like a Codex child (a legacy plan-stage run slot only during a mixed-image roll), and additionally needs `cross_check_codex_lead_v1`. Hosted authenticated Claude model acceptance is not proven by local tests.

## Default and plan prompt amendment (2026-10-17, #3091)

This amendment supersedes only the default of 2 in the 2026-10-07 automatic
rounds extension: `PLAN_CROSS_CHECK_MAX_REVISIONS` now defaults to 4, permitting
at most five checked candidates; the integer range remains 0–4. Explicit
choices remain authoritative. Older `.env` files copied from the example with
an explicit value of 2 keep 2 after upgrading until the operator removes the
line or sets 4; only installations without the line take the new default.
Existing sequences retain their immutable first-candidate enabled/limit
snapshots. Superseded/recovery attempts still count across generations, human
revision accounting stays separate, and implementation still needs the latest
current candidate's exact APPROVE and acknowledged handoff. BLOCK, timeout,
unavailable or malformed checker results and delivery failures retain their
distinct existing human/terminal handling.

The decision gives eligible revisions more bounded opportunities while making
the plan/checker exchange inspectable. Complete automatic revised `plan_md`
includes the exact heading `Cross-check dispositions`, covering every immediately
preceding delivered item as addressed (where/how) or declined (rationale and
inspectable evidence); older entries are optional. Round 1 requires none;
round 2+ missing/incomplete entries mean ordinary REVISE, never BLOCK solely
for omission. Checkers engage with rationale and accept supported declines.
Uninspectable instruction claims remain explicitly unverified and alone cannot
establish exceptions to inspectable requirements; instruction precedence is
unchanged.

Lead and checker plan prompts share the bounded static fact that temporary
file-tool and shell artifacts, including test logs and review exports, belong
in worktree `.uzi/scratch/`. Overrides are judged by actual effects, including
inherited children, and the plan names effects it relies on. Moving script
logs and inherited Go build/test temporary files into permitted scratch alone
is not a bypass; neither variable name nor destination proves harmlessness.
Configuration, credentials and test behavior need inspectable evidence. Reject
skipping, selecting, narrowing or weakening required validation and violations
of applicable explicit restrictions.

These are prompt requirements, not parser-enforced submission conditions.
Dispositions remain untrusted plan content within the exact candidate digest
and grant no authorization. No new fields, parser, transport,
history or authority are introduced; server-provided prior-findings history is
accepted as out of scope. Code cross-check's existing inline-environment wording
is unchanged; code-stage prompt changes are deferred.

Compared with the old default, the worst case adds two checker waits, each up
to `PLAN_CROSS_CHECK_TIMEOUT` (normally 30 minutes), holding the lead slot but
excluded from its wall budget, plus two lead revision turns charged to that
budget. Deterministic fixtures/mock tests are not model convergence proof.
Live hosted/model attempts, cost and latency are unmeasured; no universal
convergence guarantee or new hosted acceptance is claimed. Earlier provenance
and acceptance limits above remain historical records.

## #1230 coverage-park and semantic digest amendment (2026-10-20)

This amendment records the dormant [PRD #1230](../prds/1230-semantic-completion-contract.md) M4 additions to the plan gate. Everything in it is reachable only through a run with a `semantic_planning` enrollment; production enrollment is off until #1231, no production code writes one, and only tests inject the path. The structural and legacy meanings of the sections above are unchanged. Nothing here is hosted-acceptance proof.

### The `semantic_v1` candidate digest

For a semantic proposal the exact-candidate identity includes the canonical criteria set and the frozen source identity, and the checker payload carries them, under candidate identity version `semantic_v1`. The run's frozen semantic enrollment selects the digest scheme, never the claiming worker's capability and never the #2150 per-sequence snapshot. Older stored structural digests are not reinterpreted. A changed candidate, source or criteria set invalidates both the coverage verdict and any checker authorization bound to the old digest.

### Hard clause and the deadline path

`cross_check_semantic_plan_v1` is a hard claim clause on the plan-checker child of a semantic-enrolled lead, mirrored in the claim, placement, saturation (spread) and health copies beside `cross_check_codex_lead_v1` and outside the override paths. With no capable checker the child stays unclaimable until its deadline, and the lead takes the existing deadline-to-human path. There is no fallback to the structural digest or payload. The lead worker needs the separate `completion_semantic_publication_v1`; the two capabilities neither imply each other nor `completion_non_closing_v1`, and neither implies #1231 audit readiness.

### `semantic_park` without checker provenance

When an auto-approved semantic run cannot proceed on its own, it parks in `awaiting_approval` through the existing `SetRunAwaitingApproval` path with a `semantic_park` reason: `coverage_incomplete`, `coverage_uncertain`, `coverage_unavailable` (exhausted budgets count here) or `maintainer_authorization`. The reason is validated against the server's single coverage authority and stored only in the presented gate payload; it is never written to `plan_cross_check_gate_reason`, and no checker REVISE, attempt or verdict is fabricated for it. A complete, maintainer-free candidate gated only by the checker keeps the existing cross-check gate reason or deadline-to-human path and carries no `semantic_park`. The park is a one-way latch for that worker execution, so no later candidate auto-approves, and it causes no synthetic REVISE and no replan. Coverage-cap exhaustion takes the human gate without another coverage model call (the plan checker, including its automatic revise rounds, still runs before the coverage step), and a provider safety refusal of a coverage pass is coverage-unavailable, not a #3068 lead-policy hold.

### D16 and the revised candidate

Consistent with D16, a human revision of the plan triggers no new cross-family checker pass. That revised candidate still receives its own coverage report at its human decision point within the non-resetting coverage budgets; an earlier candidate's verdict is never reused.

### Enrollment boundary for #1231

#1231 must enroll inside the run-creation transaction, before the first claim, so the digest scheme and capability clauses above are non-vacuous from the first planning claim.

### Coverage transport limitation

Production coverage review reaches no model today: see the [coverage transport limitation](1225-structural-completion-interlock-invariants.md#coverage-transport-limitation) in ADR-1225. Until #1231 establishes enforceable bounds, a production call would settle as coverage-unavailable (`semantic_park: coverage_unavailable`) rather than reach a model.
