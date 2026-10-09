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

The owner-scoped summary read validates child eligibility, normalizes bounded findings and reports recorded claim model/effort and child usage (`api/internal/workersvc/plan_cross_check_summary.go`). The web shows current gate reason separately from checked-candidate outcome, labels historical evidence, and uses hardened Markdown capped at 16,384 source characters and 20 items with a truncation notice (`web/src/components/PlanCrossCheck.tsx`). Unknown or inconsistent outcome pairs show unavailable. Metadata is bounded/sanitized, child links require a valid UUID, and subscription/unavailable cost is distinguished from metered spend. New plan cross-check events trigger a bounded detail refresh (`web/src/pages/RunView.tsx`). CLI fields use `Plain`; Slack carries the reason without findings.

## Proof boundaries and fixture ownership

The existing harness is unchanged and **NOT RUN in this documentation pass**. Its allocation and cleanup contracts are:

- `Taskfile.yml` allocates a unique `mktemp -d .uzi/scratch/cross-check-proof.XXXXXX` directory for fresh sandbox/supervisor/fileop binaries and removes that directory with its own EXIT trap.
- `agent/test/cross-check-confinement-probe.mjs` allocates `confinement-*` under `.uzi/scratch`, builds its own checkout/private/sibling/symlink fixtures and removes its base in `finally`. Its unconfined control must fail the same isolation assertion that the confined direct process passes.
- `agent/test/cross-check-confinement-packaged.probe.ts` allocates `packaged-confinement-*` under `.uzi/scratch`. Its synthetic shared parent permits traversal by the real identities; that parent does not grant the confined child broad sibling access. Private grants belong to each launched identity. Each test disposes its own process handles, removes its own state/base, and, for fileop-owned state, uses the command identity for cleanup.

The three packaged proofs cover different boundaries: fileop Read/write/traversal/symlink enforcement independently of broker admission; direct provider filesystem enforcement through the real launcher with a trusted fixture child; and packaged Codex config/auth/session creation and credential/session custody. The session probe uses a synthetic credential and performs no model turn or external call. It is **not authenticated model-turn or network-isolation proof**. These inherited fixtures are documented as found; new committed tests/scripts must allocate in system temporary storage rather than copy this scratch layout.

## Validation provenance and remaining scope

Maintainer-provided provenance reports PR2358 reviewer/tester/auditor validation at `631b28a1`, 2262 LiveDB tests and capable-host confinement PASS run by the lander. Those results were not observed or rerun here. Web M1 is committed at `e45d6cb` (implementation `bb0efb31`); the dispatch records `gate:web` PASS (5641 unit tests / 299 files, 8 Chromium tests / 2 files; docs checker PASS) and `npm run build` PASS. The initial web-gate lint failure was fixed and the gate rerun. The build ran before the test-only lint fix, which changed no production source.

The lander retains the capable-host confinement merge gate against the PR head. Hosted acceptance remains maintainer-owned in acceptance issue 2152. Automatic checker revision, the Codex-lead direction, dedicated checker slots, checker model/effort pins and code cross-check were outside the original #2149 implementation; #2150 extends automatic revisions below. The other exclusions remain. Observed at documentation commit `ba73044a`: `task docs:sync` completed; `task gate:web` PASS (5641 unit tests and 8 Chromium tests, including `check-docs:web`), `npm --prefix web run build` PASS, `task gate:repo` PASS and `task gate:api` PASS, including the embedded-docs package. Secrets scanning reported zero tracked-file findings and detected both canaries; SAST reported zero findings and detected its canary. The read-only docs reviewer reported no mandatory or optional findings. These are component-gate results from that #2149 documentation pass, not a replay of prior LiveDB or capable-host confinement proof. No runtime source or confinement harness changed.

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

Code cross-check (#2170) remains out of scope. A Claude child claims through the dedicated cross-check lane of #2169 like a Codex child (a legacy plan-stage run slot only during a mixed-image roll), and additionally needs `cross_check_codex_lead_v1`. Hosted authenticated Claude model acceptance is not proven by local tests.
