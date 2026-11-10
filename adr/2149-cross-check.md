# ADR-2149: Server-enforced plan cross-check

**Status**: Accepted (implementation); hosted acceptance closed by maintainer decision on #2152 (2026-10-23)
**Date**: 2026-10-06
**Issue**: [vtmocanu/uzi#2149](https://github.com/vtmocanu/uzi/issues/2149)
**Extends**: [PRD #2149](../prds/done/2149-plan-cross-check.md), especially D1, D4–D7 and D16. Its hosted acceptance was closed by maintainer decision on #2152 (2026-10-23).

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

## Amendment 2026-10-26 — #3304: code-head reads settle at the snapshot boundary

A Codex code cross-check snapshot attempt (60 s soft / 90 s hard) failed the run
after 600 s in `checkpoint-code-head`: a supervised git read inside the boundary
scope never settled, and only the read's independent 600 s timer ended it with a
plain error. Supervised git output in a boundary scope now settles on the scope's
hard signal and, where cooperation is not masked, its soft signal. The gate path's
clean soft stop or ordinary failure still gives `snapshot_failed` ("code snapshot
unavailable") with SHA-free incomplete evidence; the done body's soft stop still
ends the attempt `soft_stop`. Hard errors, unsafe cleanup and authority revocation
stay terminal, and the one advisory check and one repair policy is unchanged. The
code-head phase is reported as five named sub-steps (`checkpoint-code-head-runner-tip`,
`-tracking`, `-default-tip`, `-pin`, `-changed-files`). Mechanism, accepted races and
residuals: [ADR-1597's code-head amendment](1597-midturn-checkpoint-durability.md#amendment-2026-10-26--3304-code-head-reads-settle-at-the-snapshot-boundary).
Source and local test evidence do not establish hosted acceptance.

## Planning-diff capture: symlinks, gitlinks and budgets (2026-10-07, #2410)

#3338 replaces the object-store snapshot copy and its budget with direct reads from the explicit worker-held bare repository. `capturePlanningDiff` receives that authority from the worker; the command-identity helper reads base objects only from its pinned object directory, never from the clone's object store or configuration. Git runs in neutral private scratch, excluding clone configuration and replacement refs, with hooks and transports disabled and neutral attributes suppressing filters and text conversion. Bare `alternates` and `http-alternates` are rejected, including before each object query. Required objects are checked for type, size and content hash; missing or detectably corrupt required objects yield `base_unavailable`. Other Git failures, including a concurrent repack race, may yield diagnostic `git_refused` (`diff_failed`). Capture takes no long-lived bare lock.

The immutable base commit/tree walk supplies the baselines below. One batched sizing query over its blob identities validates ordered object ids, blob types and sizes, with independently derived input/output pipe bounds and checked arithmetic. Duplicate blob ids may share a query record, but each path occurrence contributes credit. Observed current-file/link bytes must agree with the credited size before an unchanged-hash return; fetched before-images must also agree. Clone inputs, including index metadata, grant no credit. A prior agent commit recovered into the worker bare is an eligible base; this authority is not restricted to forge-origin commits.

| Baseline | Definition |
|---|---|
| B | Sum of base blob sizes per leaf path, including duplicate blob ids at different paths. |
| C | Verified commit and tree bytes read during discovery, including repeated tree occurrences. |
| N | Base leaf-path count. |
| D | Base directory occurrences, including the root. |
| E | Base entry occurrences, excluding the checkout's root `.git` entry. |
| I | Canonical v2/v3 padded index size: 32 header/checksum bytes plus each base leaf's 8-byte-padded entry (63 bytes plus full relative UTF-8 path bytes). |
| G | Base regular `.gitignore` bytes per path; symlink ignores contribute no credit. |
| Q | Full relative UTF-8 path bytes for each base entry, plus a slash for directories and a NUL per record. |

After discovery, independent clone counters start at zero except source reads, which start at C:

| Counter | Bound |
|---|---|
| Source reads: current worktree bytes and verified before-images | B + C + 128 MiB. Clean or deleted paths fit within base credit; changed paths charge roughly their new size against the allowance, in addition to their base before-image. |
| Index bytes / index entries | I + 2 MiB / N + 20,000. |
| Union of base/index paths and final comparison candidates | N + 20,000. |
| Walk entries / directories | E + 1 root `.git` + 30,000 / D + 2,048. |
| Ignore/exclude reads, outside the source counter | G + 128 MiB. |
| Cumulative ignore-query input / output | Q + 2 MiB each, independently. |

Base discovery has per-object, depth and deadline bounds plus two absolute aggregate caps (tree-entry occurrences and cumulative path bytes; see the #3543 amendment below), and still no cache. The sizing query does not extend the deadlines. Current regular files are read completely through pinned descriptors with `O_NOFOLLOW`; a clean-looking index stat is no shortcut. Per-file reads remain capped at 4 MiB, commit/tree objects at 2 MiB each, depth at 64, each ignore/exclude read at 64 KiB, patch/output at 512 KiB, and non-ignored untracked files at 200. The helper retains its 28 s deadline, the outer capture its 30 s deadline, and individual Git commands at most 5 s within the helper deadline. Discovery, walks, comparisons and chunked reads yield on a roughly 20 ms clock so cancellation can run cleanup within the production 2 s SIGTERM kill grace.

There is no object-store snapshot or snapshot budget. The `snapshot_cap` refusal token remains for persisted compatibility, not as an active budget. The #2410 timing at 24f28b1d was a single historical snapshot-implementation sample, not a benchmark of this implementation. Existing `selfContained` switches remain in place. Under split UIDs the helper can read the bare but cannot write it; single-UID operation retains the accepted lack of OS write isolation. This adds no provider-command or model grants.

Hosted post-merge acceptance remains maintainer-owned and unverified here: measure clean HEAD capture including base sizing and report the sample count (one sample is not a p95); verify bare read/write posture on Docker and kube-native workers; and observe an auto-approved scheduled run proceeding without a gate. Capture refusal retains the existing human fallback.

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

Code cross-check was out of scope here and is now implemented by #2170 (see [PRD #2170](../prds/done/2170-code-cross-check.md) and [Cross-check](../docs/cross-check.md#code-cross-check)). A Claude child claims through the dedicated cross-check lane of #2169 like a Codex child (a legacy plan-stage run slot only during a mixed-image roll), and additionally needs `cross_check_codex_lead_v1`. Hosted authenticated Claude model acceptance is not proven by local tests.

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
accepted as out of scope. At this amendment, code cross-check's existing inline-environment wording
was unchanged and code-stage prompt changes were deferred; the 2026-10-27 amendment below supersedes that deferral.

Compared with the old default, the worst case adds two checker waits, each up
to `PLAN_CROSS_CHECK_TIMEOUT` (normally 30 minutes), holding the lead slot but
excluded from its wall budget, plus two lead revision turns charged to that
budget. Deterministic fixtures/mock tests are not model convergence proof.
Live hosted/model attempts, cost and latency are unmeasured; no universal
convergence guarantee or new hosted acceptance is claimed. Earlier provenance
and acceptance limits above remain historical records.

## #1230 coverage-park and semantic digest amendment (2026-10-20)

This amendment records the dormant [PRD #1230](../prds/done/1230-semantic-completion-contract.md) M4 additions to the plan gate. Everything in it is reachable only through a run with a `semantic_planning` enrollment; production enrollment is off and not planned (#1231 was closed not planned), no production code writes one, and only tests inject the path. The structural and legacy meanings of the sections above are unchanged. Nothing here is hosted-acceptance proof.

### The `semantic_v1` candidate digest

For a semantic proposal the exact-candidate identity includes the canonical criteria set and the frozen source identity, and the checker payload carries them, under candidate identity version `semantic_v1`. The run's frozen semantic enrollment selects the digest scheme, never the claiming worker's capability and never the #2150 per-sequence snapshot. Older stored structural digests are not reinterpreted. A changed candidate, source or criteria set invalidates both the coverage verdict and any checker authorization bound to the old digest.

### Hard clause and the deadline path

`cross_check_semantic_plan_v1` is a hard claim clause on the plan-checker child of a semantic-enrolled lead, mirrored in the claim, placement, saturation (spread) and health copies beside `cross_check_codex_lead_v1` and outside the override paths. With no capable checker the child stays unclaimable until its deadline, and the lead takes the existing deadline-to-human path. There is no fallback to the structural digest or payload. The lead worker needs the separate `completion_semantic_publication_v1`; the two capabilities neither imply each other nor `completion_non_closing_v1`, and neither implies #1231 audit readiness.

### `semantic_park` without checker provenance

When an auto-approved semantic run cannot proceed on its own, it parks in `awaiting_approval` through the existing `SetRunAwaitingApproval` path with a `semantic_park` reason: `coverage_incomplete`, `coverage_uncertain`, `coverage_unavailable` (exhausted budgets count here) or `maintainer_authorization`. The reason is validated against the server's single coverage authority and stored only in the presented gate payload; it is never written to `plan_cross_check_gate_reason`, and no checker REVISE, attempt or verdict is fabricated for it. A complete, maintainer-free candidate gated only by the checker keeps the existing cross-check gate reason or deadline-to-human path and carries no `semantic_park`. The park is a one-way latch for that worker execution, so no later candidate auto-approves, and it causes no synthetic REVISE and no replan. Coverage-cap exhaustion takes the human gate without another coverage model call (the plan checker, including its automatic revise rounds, still runs before the coverage step), and a provider safety refusal of a coverage pass is coverage-unavailable, not a #3068 lead-policy hold.

### D16 and the revised candidate

Consistent with D16, a human revision of the plan triggers no new cross-family checker pass. That revised candidate still receives its own coverage report at its human decision point within the non-resetting coverage budgets; an earlier candidate's verdict is never reused.

### Enrollment boundary for #1231

#1231 was closed not planned (2026-10-23), so nothing enrolls today. The boundary binds any future enrollment: it must happen inside the run-creation transaction, before the first claim, so the digest scheme and capability clauses above are non-vacuous from the first planning claim.

### Coverage transport limitation

Production coverage review reaches no model today: see the [coverage transport limitation](1225-structural-completion-interlock-invariants.md#coverage-transport-limitation) in ADR-1225. Because no enforceable bounds exist (#1231 is not planned), a production call would settle as coverage-unavailable (`semantic_park: coverage_unavailable`) rather than reach a model.

## Disposition evidence amendment (2026-11-01, #3097)

The maintainer chose Option B: checkers assess dispositions and inspectable
evidence supplied in their input. Previous findings are not supplied separately,
so a checker cannot establish completeness against the preceding check's items.
This narrows the checker instruction in the 2026-10-17 amendment: missing or
incomplete defect dispositions identifiable from supplied input call for REVISE,
never BLOCK solely for omission. The lead still includes dispositions for each
immediately preceding delivered item. Supported declines cannot waive mandatory
defect requirements, and #3168's advisory coverage exception remains unchanged.

No prior-findings transport, candidate identity, replay input, capability or
privilege changes are added. Option A (bounded server-derived prior findings)
is deferred unless a controlled dropped-defect-disposition evaluation shows
benefit with that input. Instruction tests establish this contract, not improved
live-model convergence, cost or latency; those outcomes remain unmeasured.

## Advisory plan coverage amendment (2026-10-23, #3334 / #3168 M1)

This amendment records [PRD #3168](../prds/3168-plan-cross-check-coverage.md)
M1's advisory coverage contract, implemented through child #3334. It narrows
the universal missing-disposition REVISE rule in the 2026-10-17 amendment for
coverage notes; defect disposition rules remain. It does not activate the
dormant semantic coverage profile above.

The shared `crossCheckBrief` asks both checker families to enumerate
requirements and acceptance criteria from the supplied issue snapshot and
repository PRDs linked by the issue. Existing read/search tools, deadlines
and limits apply; issue comments are not supplied, and no URL fetching or new
tool access is added. Omitted or partly covered requirements use warning with
the mandatory summary prefix `Requirement gap:`. Requirements verifiable only
on hosted, live or maintainer-only surfaces use info with `Coverage uncertainty:`.
Every coverage item uses one of these prefixes; defects never use either.
Missing, external, unreadable or oversized sources receive a summary
`Coverage uncertainty:` note and are never reported as covered. No defect
found does not mean requirement covered.

Coverage alone never justifies REVISE or BLOCK, including a missing or declined
prior coverage disposition. An omitted requirement stated by the issue as a
safety, security or data-integrity constraint remains an error defect, with no
coverage prefix, requiring REVISE under existing defect rules. Missing or
incomplete defect dispositions still call for REVISE, never BLOCK solely for
that omission; a supported decline cannot waive a mandatory defect requirement.
Defects come first within the existing 20-item cap. Coverage fills spare space;
overflow is counted in the summary as `N coverage notes omitted`. Coverage
items and the omitted count never displace a defect.

The runner passes approving findings to both lead executors.
`renderPlanApprovalAdvice` in `agent/src/prompt.ts` delivers every item,
including noncoverage warning/info, plus the complete bounded summary,
including summary-only findings. Absent/null findings or an empty summary
with no items leave implementation-prompt bytes unchanged. Approval and
automatic revision advice share nonce fencing, with eight nonce attempts and
entity escaping on exhaustion. The fenced text is untrusted, non-gating advice:
verify against issue, code and uzi rules, decline conflicts with reasons, and
retain existing permissions and constraints. The approved canonical plan
remains authoritative. An omitted requirement may be implemented only within
the issue's stated scope and must be named in the PR description. Code
cross-check reviews the implementation when enabled; scope, permissions and
canonical authority apply independently.

No additional gate, re-plan or revision round is required. There is no new
wire/API/schema field, capability or persistence. Advice is worker-local and
lost on reclaim; the dormant semantic human-gate path drops it when it drops
`checkedApproval`. Mixed workers retain that boundary: old leads discard
approving findings, old checkers supply no new coverage account, and new leads
render received findings. These are instruction and delivery contracts, not
behavioral convergence guarantees. Code-stage revision prompt bytes stay
unchanged. Later amendment (2026-11-03, #3832): the Code repair prompt appends
a batching clarification (one list call, one entry per original finding ID);
per-ID dispositions, the dispositions tool and the Plan stage are unchanged,
and the PR section groups `Coverage uncertainty:` findings as verification
caveats.

Instruction-contract tests and captured provider requests verify guidance and
both-executor delivery; they do not prove model behavior. Fixed-nonce goldens
pin unchanged code-stage revision prompts. Required validation and its observed
results are tracked in PRD #3168's M1 progress section. Child #3334 requires no
maintainer replay; parent #3168 remains open for M2. No live model convergence,
cost or latency measurement is claimed.

### Advisory code criteria and environment effects amendment (2026-10-27, #3167 M3a)

This amendment supersedes the code-stage environment deferral above. Both checker
families share the code brief: correctness against the issue and approved plan,
contracts and safety invariants, tests unable to fail on unfixed code, scope,
docs and changelog. Criteria come from the issue, approved plan and frozen
milestones. The code prompt serializes milestones once as whole JSON in a
nonce-fenced approved_milestones untrusted DATA block. Absent/null lists become
[]; nonarrays, more than 64 entries or more than 256 KiB UTF-8 JSON are refused,
with the exact byte boundary accepted. No API, claim or digest changes.

Criterion titles must start with Requirement gap: for missing/partial delivery
or Coverage uncertainty: for verification unavailable here, including informational
notes; defect titles use neither prefix. Use the relevant code or PRD/plan path,
and line 0 when none applies. Hosted/live/maintainer-only verification is minor;
missing worker implementation/tests are graded by impact. An issue-stated safety,
security or data-integrity requirement that is missing is a critical/major defect
without either prefix. Defects come first within 20 findings, 2 KiB per finding
and 32 KiB total. Criteria use spare room only; at most one minor omission note
counts unreported criteria if room remains. Never evict a defect. A full defect
report may omit criterion notes; criterion-only findings are valid. Empty findings
mean no reportable defect, criterion or omission note.

Judge overrides by actual effects, inherited children and the effects relied on.
Temporary scratch/TMPDIR relocation, including inherited Go build/test temporary
files, alone is not bypass. Neither name nor destination proves harmlessness;
configuration, credentials and test behavior require inspectable evidence. Flag
skipping, selecting, narrowing or weakening required validation and explicit
restriction violations. This introduces no plan dispositions or scratch-location
instruction into the code brief.

M2 changed-path coverage instructions and locked-brief precedence over owner
guidance remain intact. This is advisory instruction policy, with one check and
one fix pass, no recheck or publication block, no new schema field or programmatic
finding rewrite. Instruction fixtures pin text, not model behavior. Frozen replay
inputs and baseline hashes are unchanged; only revised brief hashes are repinned.
M1 baseline and M3b replay/hosted verification remain maintainer follow-ups; this
amendment claims neither detection improvement nor hosted acceptance.

#### Worker run evidence (2026-11-09, self-improvement)

The code brief now states that the worker's own command results (gate, build,
lint, test, red/green and mutation runs, and their logs) are not supplied. Their
absence alone is neither a defect nor a criterion finding; run results claimed in
committed text or the plan are unverified claims, not evidence. A criterion that
itself requires a run result gets a minor Coverage uncertainty: finding, and
tests are judged from the committed code. Motive: findings that only reported
invisible run evidence cost a repair turn without a code change. Supplying gate
records stays parked (PRD #3167 "Gate evidence parked"). Only the revised brief
hashes are repinned; frozen replay inputs and baseline hashes are unchanged.

## Amendment 2026-10-31 (#3708)

[PRD #3708](../prds/3708-gated-plan-cross-check.md) revisits the "Human-gated runs"
out-of-scope line: a run that is not auto-approved, whose owner enabled Plan cross-check,
now goes through the checked-plan loop before its first human gate. Decisions:

- **Gated mode.** A second INSERT-time snapshot, `runs.plan_cross_check_gated`, is mutually
  exclusive with the required mode (CHECK `runs_plan_cross_check_mode_check`; kinds `issue`,
  `prompt`, `ci_fix`; non-seeded; no isolated lane). The required predicate is unchanged and
  existing rows are not backfilled. Enrollment records the owner's frozen choice;
  availability is a gate-time outcome.
- **APPROVE carries no authority.** Every checker outcome presents the ordinary human gate.
  The agent returns `gated_pass`, never an approval grant, and the server widens the existing
  bound human-approval arms from required to required-or-gated, narrows the "any prior
  approve" shortcut to neither mode, and refuses `SetRunAutopilotPlan` for a gated run. A
  "passed" verdict binds to the candidate digest presented at the gate.
- **Gated recovery.** A same-generation retry reuses the check row, child and deadline.
  Custody loss on a pending check ends it (the gate opens interrupted or timed out, with no
  new round and no renewed deadline); a decided REVISE may start a new round; an unpresented
  APPROVE derives `interrupted`; a presented gate never re-runs discovery.
- **Version matrix.** The capability `cross_check_gated_v1` gates the mode. The server records
  `worker_unsupported` itself for a worker lacking it, since an old worker cannot report a new
  outcome.
- **Trigger and rollback boundary.** The trigger `enforce_gated_plan_approval` refuses
  `awaiting_approval` to `running` on a gated row without a current bound approve input, so the
  rule holds for that edge under any api binary. The `claimed` to `running` reclaim edge is not
  guarded by the trigger; under an older api it is closed by the rollback drain (stopped api plus
  a terminal drain), which is the admission barrier for that path. A Down guard (also in
  `scripts/gated-cross-check-drain-check.sql`) refuses rollback while any gated run is
  non-terminal; the operator drains first with the upgraded api stopped. The per-owner toggle
  is not an admission barrier.
- **D16 unchanged.** A human revision still triggers no fresh checker pass; earlier results
  stay historical. Per-round lead responses are read from the next candidate's "Cross-check
  dispositions" prose (8 KiB cap) as untrusted claims; no structured disposition store is
  added and dispositions never gate approval.

Not shipped: live k8s acceptance of the wait and a hosted rollback rehearsal (PRD #3708 M3).

## Amendment 2026-11-03 — #3841: owner steering in plan checker input

A plan checker could not verify a plan claim attributed to the run owner (for example "the
owner answered that the migration may be skipped"), so such claims stayed uninspectable and
unverified. Each new plan-stage round now freezes a bounded snapshot of the run owner's own
answers and revise feedback for the checker. Decisions:

- **Source and attribution.** Owner attribution comes from the write path, not from content.
  Answers come from the owner-scoped answer writer (`CreateRunAnswerInput` via the owner's
  `submitAnswer`); revise feedback comes from `CreateRunReviseInputIfUnderCap`. System
  completion-continue answers are excluded through a `completion_decision` audit row carrying
  the `question_id`, written before the answer so no answer is ever visible without its mark.
  The question text is context, attributed to the lead or worker that asked it, never to the
  owner. Documented gap: a generic `CreateRunInput` call with a variable kind is not guarded at
  Go level; today's callers are the two writers above.
- **One-statement cutoff.** One SQL statement (`SnapshotOwnerSteering`, one MVCC snapshot)
  takes the cutoff as the highest committed input id visible to that statement and reads the
  entries up to it. An insert still in flight lands in the next round. The snapshot is written
  in the same transaction as the round row; a retry writes nothing, so every claim of a round
  delivers identical bytes with a sha256 digest. The digest is an integrity check, not
  tamper-proofing.
- **Shape guards and caps.** A malformed answer or question context gets a status marker and
  never fails the round. Owner text is scrubbed before the cap. Caps: the 32 newest entries,
  8 KiB per answer or revise entry, 4 KiB per question context, 64 KiB per snapshot; overflow
  is marked `entry_cap` or `byte_cap`.
- **Context binding.** An answer is paired with its question context by `question_id`: no
  occurrence is `unavailable`; more than one distinct variant is `ambiguous`; a context whose
  status is not ok is `malformed` or `oversized`; identical re-emissions share one context.
  Only a bound, complete context lets a checker rely on what an answer meant.
- **Revise binding.** Each revise entry carries its gate binding: `bound`, `unbound`, or null
  when none applies, so the checker can tell which plan the feedback addressed.
- **Child table.** The snapshot lives in the child table `cross_check_owner_steering`, not on
  the hot `cross_checks` row: the existing row is untouched, no row means "none supplied" for
  legacy rounds, and the immutability trigger is scoped to the child. Migration files are
  drafts renumbered at landing, so they are not named here.
- **Owner decision 2026-11-03: capability rollout accepted.** Only plan-stage checker children
  with an owner-steering row require the new capability `cross_check_owner_steering_v1`.
  Legacy rows and code-stage checks keep today's eligibility. Availability cost: the api rolls
  first and from then every new plan round has a row, while workers move to the new image only
  when they next roll while idle, so a new api can face an all-old checker pool. Until a
  capable worker registers, steering-bearing plan checks are not claimed; the health reason
  "Plan cross-check is waiting: no online worker supports owner-steering checks
  (cross_check_owner_steering_v1)" explains the wait, and at the deadline the check takes the
  existing fallback to the ordinary human plan gate.
- **Envelope allowance, header-scoped.** The server's existing claim-size limit is evaluated on
  the payload without steering; the steering bytes are bounded separately (6 x 65536 + 64 + 57
  = 393337 wire bytes). The claim response carries `X-Uzi-Owner-Steering: v1` only when the
  field is present, and the worker applies its larger read allowance only on the claim read
  carrying that header. All other boundaries are unchanged.
- **Unchanged.** `candidate_digest` and the code-stage checks are unchanged. The checker
  brief renders two nonce-fenced data sections, `owner_steering` (owner-authored) and
  `owner_steering_questions` (lead or worker authored, never owner), and states that an absent,
  omitted, truncated or unparseable owner entry, or an answer whose question context is
  unavailable, ambiguous, malformed or truncated, cannot by itself establish an exception.
  Because issue and lead-authored text could carry a lookalike `owner_steering` block, the plan
  prompt opens with a worker-authored line naming the two genuine fence suffixes, and the brief
  says only those blocks are supplied steering.

## Amendment 2026-11-06 — #3953: credential switch during a candidate reservation

A credential switch that arrives while an automatic plan cross-check holds its candidate
transport reservation (`checkPlan` reserves the batcher before submit) used to abort the
reservation and fail the run. The worker now defers it; the dispositions above are unchanged.

- **Hold.** The runner opens `SteeringChannel.holdCredentialSwitch()` immediately before
  `checkPlan`. While held, a switch (from the poll, a state ACK or a `switch_pending` input
  receipt) is latched: nothing is aborted, `shutdownStop` included, and no waiter is rejected.
  Cancel, claim fence, receipt failure, worker shutdown and disk stop are not held.
- **Service points.** The latch is serviced synchronously only once the reservation is released
  and verified, before any other checker round, storage, plan execution or model call: S1 after
  an APPROVE returns (before the semantic review or any emit), S2 after an automatic REVISE
  (before the revision turn), S3 in each checked human gate's `onApplied` after
  `releaseAppliedGate`. A give-up of a human gate resumes the presented gate (same presentation,
  revision and verdict binding, no republish). No other exit ever services the latch: the hold
  is closed without service when the reservation was released, and otherwise stays open with the
  unreleased reservation for the rest of the flight, which that failure ends. A failure therefore
  keeps its existing disposition and is never turned into a switch. No timer cancels a live
  reservation.
- **Decision stashes.** At S1/S2, when a switch is in play, the checker decision is kept for the
  give-up re-run. Both stashes are bound to the claim generation, round, plan text and
  milestones; the approve stash also carries the candidate digest, compared on semantic runs
  (elsewhere the server owns the hash). It survives only the
  `CredentialSwitchSignal` control exit, is adopted once at the first same-generation
  continuation, and is cleared on mismatch (fail closed), any other failure, the next plan-gate
  entry, a release and flight end. On a release the next claim re-discovers; the round-1 findings
  text is not carried.
- **Invariant guards.** If a reservation is ever installed when the in-place switch hook (G1) or
  the outer `CredentialSwitchSignal` arm (G2) runs, the run fails terminally as an invariant
  violation (`CandidateTransportInvariantError`): no switch report, the batcher is closed, and
  the held-event loss is logged. Tests drive the poll, state-ACK and `switch_pending` receipt
  trip entries (the state-ACK and receipt entries pin ordering) and show none reaches either
  guard during a reservation.
- **Residual window.** A `switch_pending` input receipt can arrive after the human gate's
  presentation; a ready input is parked and a held batch is left unapplied for the next claim,
  so no input is lost.

### Deferral bound (audited at 3fc80e34; gate checkpoint bounded and re-audited at 8ac83f39, #3980)

The switch can wait only as long as the reservation lives. Bounds are read from code with the
production defaults (request 3 s, preparation ≤3 s, poll ≤15 s, HTTP 30 s, server deadline
`PLAN_CROSS_CHECK_TIMEOUT` default 30 m, worker-side cap 2 h in `checkPlan`):

| Path | Components | Default deadline | Worst case (2 h deadline) |
|---|---|---|---|
| APPROVE / automatic REVISE | prepare ≤3 s, submit stage ≤30 s, poll window to the deadline, final transport check ≤3 s | ≈31 min | ≈2 h 1 min (7,236 s) |
| Checked human gate (timed out, block, gated pass), first gate | the above + 3 s, semantic review ≤60 s, resumed-delivery wait ≤153 s (failure path), `planChangedFiles` ≤600 s, receipt settlement ≤60 s, presentation report ≤422 s, preparation ≤9 s | ≈53 min (rounded up) | ≈2 h 23 min (8,543 s, rounded up) |
| Checked human gate on a re-plan gate (frozen milestones changed) | the first-gate row + gate checkpoint ≤60 s + post-deadline tail ≤≈16 s (≈1 s child kill and join, ≤10 s private temp-dir cleanup, ≤5 s helper overrun) = ≈76 s when those operations return (see Limits below) | ≈54 min (3,143 s + 76 s = 3,219 s, rounded up; when those operations return) | ≈2 h 24 min (8,543 s + 76 s = 8,619 s, rounded up; when those operations return) |

**Gate checkpoint on a re-plan gate (#3980).** `prepareGateCheckpoint` runs only when the
claim carries frozen execution milestones and the candidate's milestones differ from them, so a
first plan gate never reaches it (the first-gate row). On such a re-plan gate it runs with the
reservation still installed. On Claude the step now has an enforced 60 s deadline from step
entry, threaded explicitly through each seam: the sink-gate queue wait and tick-survivor prelude
(no signal or second wait after the deadline), every GitCache git command and bare-repository
lock wait (a recovery operation: timeout capped, child killed and joined), the attributed-process
reap joins (capped; expiry is an unconfirmed reap, nothing aborted) and reap passes (not admitted
after the deadline), the quiescence helper setup, run and cleanup, procfs scans, and Docker
teardown (skipped after the deadline). Expiry fails closed like a failed checkpoint (for the error
shapes listed below; the sentences after the Limits name the exceptions): a
`TransientRecoveryError`, the source clone kept as the authoritative copy, and the candidate
reservation neither released nor cancelled by the step. An owner Cancel or shutdown keeps its own
reason. The measured post-deadline tail is about 1 s without the runner/agent uid split (git
child SIGTERM, SIGKILL after 1 s, join) and up to about 10 s more with it (awaited private
temp-dir cleanup, `HELPER_TMP_REMOVE_TIMEOUT_MS` = 10 s), plus a quiescence or reap helper
overrun of at most `HELPER_SLACK_MS` 5 s. The bound is therefore about 76 s
(60 s + 5 s + 10 s + about 1 s), typically about 61 s, provided the untimed operations listed under
Limits return; it is not unconditional. Limits: the guarantee covers a live
worker process (no crash, storage or power-loss claim); "every child the step started has
exited" holds for direct children, so a descendant spawned by an agent-planted git filter can
outlive the step and is left to a later quiesce; a single kernel-blocked procfs read cannot be
interrupted; a killed child that never closes is still joined; and the worker-owned synchronous
process-group probe and kill (`processGroupAlive`, `signalProcessGroup` and `killRunnerGroup` via
`spawnSync`, under the uid split) carry no timer of their own, as elsewhere in the worker. Under the
split a run-process scan or reap pass helper runs as `runner`, which the worker cannot signal
(EPERM), so a helper that outlives its deadline-capped timer is killed as that uid
(`killRunnerGroup`) before it is joined; if that same-uid kill fails too, the helper is still
joined (fail-closed) and the bound does not hold for that pass. Codex deadline mechanics are unchanged (their
capture scope already bounds the step near 60 s). An owner Cancel or shutdown reason now survives
when the capture lane returns incomplete, instead of being remapped to the transient failure
(issue #3980); a recovery deadline or timeout that git wraps is mapped to the transient failure,
as is an abortable wait the step's own deadline stops, such as the attempt-path source-authority
lock retry (an `AbortError` whose `cause` is, by identity, that deadline signal's reason).
This does not cover every capture-lane abort: an owner Cancel or shutdown landing during a git
child, or during that authority-lock retry, is still rethrown as a git error (the `git … failed`
wrapper, or a permit-held git abort for a child the abort stops) or as the raw `AbortError`
rather than its own reason (this predates the change), and a git child whose
own timeout kills it a few ms before the deadline can still fail as a plain git error rather than
the transient failure. Likewise the authority-lock wait is clamped to the same deadline, so its own
expiry can surface as `AuthorityLockBusy` just before the step's deadline fires, and contention that
outlasts the shorter boundary wait earlier in the step is a genuine `AuthorityLockBusy`; both stay
generic errors (mapping them would need an elapsed-time check). Rationale: an explicit deadline per seam rather
than an ambient `MaintenanceBudget` (the #3181 precedent is for a no-custody settlement, and an
ambient budget would be adopted as a cancellation budget) or `reportBudget` (which forces a
single reap pass).

**Accepted limit: an owner Cancel during the hold.** While the switch stamp is pending, the
server's input consume returns only the switch signal and withholds every input row, and with a
live poller an owner Cancel is queued as such a row rather than applied server-side. So a Cancel
pressed during the hold takes effect only after the reservation settles and the switch is
serviced: on a give-up the held flight applies it; after a release it waits for the next claim.
Its bound is the hold's own: the audited bounds in the table above for the listed paths; on a
re-plan gate it is that row plus the gate checkpoint's bound of about 76 s when its untimed operations return (#3980, above). Meanwhile the checker child may keep running until
its verdict or deadline. A server-side terminal cancel still ends the held flight at once, and a
second Cancel after the release applies at once. The owner accepted this limit for #3953; prompt
Cancel during the hold is #3974.

**Amendment (#3974): an owner Cancel during the hold is observed promptly.** The server still
withholds the cancel row. While the switch is withheld, by this hold or by the M5b revision-turn
defer, each worker poll that sees the current-generation switch signal makes one bounded,
read-only ownership probe for `cancellation_requested` (the #3860 field). A positive answer,
re-checked synchronously, commits the cancel exactly as a delivered cancel input would. A sticky
flag then makes every later switch trip a no-op, and the runner's switch entry points take the
ordinary "run cancelled" route instead: no switch capture, no `credential_switch*` report, no
plan storage or gate presentation. The worker's `failed` report then settles the lead, the stamp,
the cancel row, and, through the exit trigger, the pending round and the checker child in one
transaction. Custody is the ordinary owner-cancel disposition; observing the Cancel starts no
switch-specific custody operation. A probe failure, timeout or `false` counts as unknown and
commits nothing, so an api without the field keeps the limit above. Remaining limits: on a
re-plan gate the worker latches the Cancel promptly, but an in-flight gate checkpoint step delays
the terminal report by about 16 s past the step's 60 s deadline (about 76 s from step entry, when the untimed operations return; #3980, above); it presents no gate
and services no switch afterwards. Graceful stop and auto-stop rows stay withheld (the probe reads
only `stop_kind = 'cancelled'`); auto-stop still escalates server-side on its own timer. A Cancel
pressed during the hold that a release beats still waits for the reclaim; one pressed after the
release applies at once. A cancel controller already spent by a refused "now" pause
stops the SDK turn only at its next boundary.

These rest on stated assumptions: request timeouts propagate through `fetch` and body reads; the
poll compares against the worker clock; executor-sequenced sink callbacks do not run during the
plan gate; and the resumed-delivery wait, which predates this change, is bounded by its failure
and stall counters rather than a wall-clock timer.

## Amendment 2026-11-06 — #3543: aggregate base-discovery bound

The security-audit rule grades as High any read of externally controlled input with no declared
size or item cap; a timeout alone is insufficient. This ADR said base discovery had only
per-object, depth and deadline bounds, with no aggregate base cap or cache. Measurement (#3991)
showed wide shared trees growing memory with work before the deadline: up to 663 MiB on the
production path (uninstrumented, 1536 MiB / 1 CPU) and 782 MiB in an instrumented run at
4 GiB / 2 CPU. A deliberately undersized 256 MiB harness check of the same shape was OOM-killed.

- **Decision (2026-11-06, delegated by the maintainer to the evaluating sessions).** Add an
  aggregate bound, rather than amend the audit rule, which would have left memory bounded only
  by elapsed time. The audit rule is now satisfied, not excepted.
- **`baseDiscoveryEntryCap` = 1,200,000.** Charged once per tree entry (leaf or directory, the
  root's entries included) on every walk occurrence. A shared subtree is charged each time it
  is walked, because there is no cache. It is checked immediately after the entry is counted,
  before the entry is parsed, recursed into or retained.
- **`baseDiscoveryPathByteCap` = 100,663,296 (96 MiB).** Charged for every entry, directory and
  leaf, as the byte length of its full relative path, after the entry check and before the path
  string is built. Directories are charged because each recursion frame keeps its path prefix
  alive and a name's length is otherwise bounded only by the 2 MiB per-object cap: a 64-deep
  chain of near-2 MiB names with no leaves would pass leaf-only charging.
- **Refusals.** `base_discovery_entry_cap` and `base_discovery_path_byte_cap` fail closed and
  map to `diff_too_large`. Like `base_source_cap`, they bound how much of the base the capture
  processes; the gate treats `diff_too_large` and `diff_failed` the same, so there is no API or
  schema change. The per-object (2 MiB), depth (64) and deadline (28 s helper, 30 s outer, 5 s
  per command) bounds are unchanged. Both keys are absolute caps, not allowances, and are
  validated with the helper's sentinel convention.
- **Thresholds.** Each is at least 2x the largest realistic trees counted on 2026-11-06 from the
  default branches with `git ls-tree -r -t`: Linux 103,697 entries and 4,018,329 path bytes;
  the Chromium GitHub mirror 563,282 entries and 42,616,138 path bytes. The caps are 2.13x and
  2.36x Chromium.

**Structural claims**, for discovery that completes without a cap refusal:

- E <= entryCap. D <= E + 1 <= entryCap + 1, where D counts the root (not an entry) plus each
  descended directory entry. N <= E.
- Cumulative charged path bytes P <= pathByteCap. P includes every retained leaf path and every
  recursion-stack prefix. The ignore-query byte counter adds one or two bytes per entry on top:
  Q = P + E + (D - 1) <= pathByteCap + 2 x entryCap.
- In-flight tree objects are at most 65 frames (depth 0 to 64) x 2 MiB, plus the 2 MiB base
  commit object. The object being fetched is briefly held twice (the command output chunks and
  their concatenated copy), so a loose upper bound is about 2 x 65 x 2 MiB.

**Post-discovery structures** are bounded by the existing derived limits plus allowances, not by
the caps alone. The caps bound N, D, E, P and Q, not B: the summed base blob sizes, and so the
source budget B + C + 128 MiB, still have no absolute ceiling (blob contents are read later, per
file, within the per-file and source budgets).

- tracked paths <= PATH_LIMIT = N + pathAllowance, which includes index paths; index bytes <=
  INDEX_LIMIT = I + indexAllowance.
- DIR_LIMIT = D + directoryAllowance <= entryCap + 1 + directoryAllowance.
- `trackedParents` holds ancestor prefixes (up to depth x tracked paths); candidates are at most
  the tracked paths plus 200 untracked; the blob sizing batch (ids, seenIds, sizes, input) is at
  most N.

**Measured memory** is empirical: one host, one image, one run per configuration. These are
estimates, not proofs.

- M1 per-unit coefficients, for base discovery only: about 240 B per entry occurrence with an
  intercept of about 134 MiB, and about 1.1 to 1.9 B per path byte.
- M3, with the caps in force: the largest measured whole-capture container peak at production
  timing is 575.5 MiB (4 GiB / 2 CPU) and 543.4 MiB on the 1536 MiB / 1 CPU slot. A base at
  99.94 % of the entry cap and 99.98 % of the path-byte cap, with the maximal distinct-blob
  sizing batch, was measured with the helper's wall-clock bounds lifted for validation
  (validation, deadline lifted; the harness rewrites the spawned helper source, no production
  flag): the 1536 MiB / 1 CPU row (unmetered) peaked at 985.1 MiB and the 4 GiB / 2 CPU row
  (metered) at 1,111.9 MiB, both below 1536 MiB. For the metered 4 GiB / 2 CPU row, discovery completed and the sizing
  batch was recorded (sizingBytes 57,507,840 = 48 x 1,198,080 ids); for the unmetered 1536 MiB / 1
  CPU row, which has no discovery record, completion is inferred from phase evidence. The capture then ended on the `--no-checkout` fixture's post-discovery `git_pipe_cap`. The
  rows are not production timing and not an unchanged production-path control. An earlier ×4
  deadline row (767.5 MiB, partial) and a sizing-batch extrapolation (about 1,166 MiB) are
  superseded context. Both over-cap fixtures refuse on their own cap at both configurations.
- Clean controls (this repository, a 50k-file tree) capture. One 50k-file 4 GiB / 2 CPU row hit
  the post-discovery deadline under host load and passed on rerun, disclosed in RESULTS.md.
- On the measuring host, discovery speed usually reaches the 28 s deadline before the entry cap
  on near-cap trees. The caps bound discovery's per-entry memory regardless of host speed.

**Residuals.** Compose enforces memory per worker, not per run. The measurements are single-run.
The Compose sizing formula (`docker-compose.yml`: `mem_limit = (WORKER_MAX_CONCURRENT_RUNS +
WORKER_CHAT_SESSIONS) x per-slot budget + headroom`) does not count cross-check lane slots
(`WORKER_CROSS_CHECK_SLOTS`, default 1), so a cross-check lane child can run beside a run slot's
capture and the chat slots, sharing the one worker cgroup. The 985.1 and 1,111.9 MiB figures are
for a capture alone; they are not additive-safe against a full set of other slots. They cover
discovery and the sizing batch only: the post-discovery phase on a real checkout was not measured.
The lifted rows do not show that a base this size finishes inside the production 28 s deadline.
The soft-deadline overrun is out of scope. The negative-timer misclassification (#3993) was
fixed separately (#3996), before the M3 measurements.

Measurement and harness: [RESULTS.md](../e2e/planning-base-discovery-measure/RESULTS.md) and its
[README](../e2e/planning-base-discovery-measure/README.md).

## Amendment 2026-11-06 — #3985: clarification during a human revision

A plan cross-check run (gated, or required on a claim issued after a forced human gate cleared
`auto_approve`) that is not auto-approved stays at `awaiting_approval` while its lead works a
HUMAN revise. When the lead asks a clarification, the run parks at `awaiting_input`. (In the
same flight that hit the forced gate the claim still carries `auto_approve`, so the lead's
question resolves through the autopilot sentinel without parking; that pre-existing behaviour
is unchanged here, and in required mode only a reclaimed claim reaches this settlement; a gated
claim never carries `auto_approve`, so its first flight does.) An answer is not plan approval,
so `SetRunRunning`'s bound-approval clause refuses an ordinary `running` settlement there, and
an issue run (or any run with `plan_revision_retry` negotiated) failed with "clarification
running settlement was not confirmed"; other callers' unconfirmed `running` was refused
silently. Decisions:

- **Dedicated settlement, not a relaxed `running`.** A worker that negotiated
  `clarification_revision_settlement_v1` reports `clarification_settled` with the question,
  the pending revise input and its gate revision. A claim-fenced writer returns the run to
  `awaiting_approval`. It requires an `issue`, `prompt` or `ci_fix` run with no open
  completion question, the applied answer to the open question, and the pending, bound,
  unapplied, non-empty revise consumed by this worker and claim generation. It refuses when a bound
  approval was already applied. It writes only status, timestamps, the question markers and the
  banked question wait. `SetRunRunning` is unchanged, because its SET can freeze candidate
  milestones, contract and budget fields.
- **The revise stays pending.** The settlement neither applies nor supersedes the revise; the
  lead's next presentation does, at gate revision N+1, which only a newly bound approval passes.
- **D16 unchanged.** No checker pass and no new presentation; plan text, candidate, gate
  revision and payload are untouched.
- **Capability matrix.** Old worker or old api: fail-closed (a new worker on an old api awaits
  its `running` report for every caller and stops on the refusal). A new worker sends the
  settlement only after the api advertised it, never strips it or retries it as an unqualified
  `running`, and accepts only an applied `awaiting_approval` ACK carrying the
  `clarification_settled` disposition.
- **Known limit: a switch stamped before the answer.** A human revision turn runs with the
  credential switch deferred, so a clarification asked inside it parks while the switch waits.
  If a switch is stamped before the owner answers, the api withholds every input, the answer
  included, so the run waits for the question timeout or a Cancel (which #3974 probes while the
  switch is withheld, at `awaiting_input` and `awaiting_approval` alike; a committed Cancel ends
  the answer wait before any settlement is sent). A switch stamped after the answer applied does
  not block the settlement. This limit predates #3985 and is not changed by it.

## Amendment 2026-11-07 — #4038: structured-output verdict carrier

**Context.** A Claude check (Plan or Code) whose final message failed JSON verdict validation ended `malformed` after a full paid review, with no chance to correct the shape.

**Decision.** The Claude checker requests its verdict through the Agent SDK's structured output (`outputFormat` of type `json_schema`, using the verdict schemas in `agent/src/codex/cross-check.ts`). The SDK then re-asks the model within the same session, up to 5 attempts, under the same turn limit, deadline and usage.

- The SDK's end-turn `StructuredOutput` tool carries only the final verdict and touches no files. It is admitted by exact name, only when the check requests structured output, in the isolated tool gate, the init check and the Code tool budget (it does not consume the Read/Grep/Glob budget). This qualifies the confinement list above: the SDK `tools` option stays `Read`, `Grep` and `Glob`; `settingSources`, the disallowed tools and the path and pattern guards are unchanged.
- The verdict is taken from the result's `structured_output` and still passes the unchanged validators, which remain authoritative. Structured output is a retry aid, not a trust boundary.
- The Code schema leaves coverage unconstrained, because the validator accepts any coverage and the normalizer degrades it.
- If the SDK exhausts its retries the check fails `malformed` with log subreason `structured_output_retries` and a fixed message.
- The Codex checker was unchanged by #4038; [#4103 below](#amendment-2026-11-09--4103-codex-plan-output-schema) subsequently adds a Plan-only output schema.

**Evidence.** `agent/test/fixtures/claude-structured-output-0.3.316.json` was recaptured on 2026-11-10 from SDK 0.3.316 / CLI 2.1.316 with scripted model turns. Replay tests in `agent/test/claude-structured-output-replay.test.ts` and `agent/test/claude-structured-output.test.ts` exercise it.

**Rejected alternative.** A manual follow-up turn asking the model to re-emit only the JSON would add a resume path and separate usage accounting; the SDK retry stays inside one session.

## Amendment 2026-11-09 — #4103: Codex Plan output schema

**Context.** A Codex Plan checker could return prose or fenced JSON that failed verdict validation after the review. The #4038 amendment above addressed Claude's verdict carrier; this amendment addresses Codex Plan output separately.

**Decision.** The Codex checker's `turn/start` sends `outputSchema: PLAN_VERDICT_SCHEMA` for the Plan stage in `agent/src/codex/cross-check.ts`. It sends the exact shared schema, retaining `maxLength` (4096 for the summary, 2048 for finding strings) and `maxItems` (20 findings). Code-stage output is unchanged. There is no added retry, checker round or fallback, and Claude's structured-output policy is unchanged.

- The unchanged `validateVerdict` remains authoritative: the summary is capped at 4096 UTF-8 bytes, each serialized finding at 2048 bytes, and the sum of serialized finding sizes at 32 KiB. Schema character limits do not enforce these byte limits. Prose, fences and invalid or oversized verdicts still fail as `malformed`.
- Only a complete final text rejected at the Codex Plan `validateVerdict` seam gets shape diagnostics attached to the same `CrossCheckMalformedError`, preserving its message and subreason. `final_text_bytes` is its UTF-8 byte length; `final_text_empty` tests exact emptiness; `final_text_starts_with_code_fence` uses the literal `startsWith` test for three backticks, without trimming. Leading whitespace before a fence therefore yields false.
- The existing malformed-verdict warning in `agent/src/cross-check-runner.ts` is the only production reader of these fields. This diagnostic stores and logs no final text or snippet. Lifecycle, partial-output, cleanup and runner parse/key failures get no shape fields, nor do Claude or Code checks.

**Evidence and limits.** These are separate evidence sources:

- **M1 capture and replay.** [The committed fixture](../agent/test/fixtures/codex-plan-output-schema-0.164.0.json) captures the real pinned Codex 0.164.0 app-server through checker callbacks, including a usable Read, with a scripted Responses provider and an OS identity exception (direct spawn under the capturing identity). Its capture-local adapter injected the schema before production wiring and selected the scripted provider. It proves schema propagation and callback/replay behavior, not live-provider acceptance of schema keywords or operating-system confinement. [Replay tests](../agent/test/codex-plan-output-schema-replay.test.ts) exercise the capture; [validation derivatives](../agent/test/codex-plan-output-schema-validation.test.ts) exercise malformed text, byte caps and the shape fields. The historical fixture is not evidence that production sent the schema at capture time.
- **Production wiring.** [Checker tests](../agent/test/codex-cross-check.test.ts) cover the Plan `turn/start` schema without the capture adapter and its omission for Code. The production wiring landed in PR #4112; the M1 capture remains unchanged.
- **Owner-reported direct live probes.** The owner reports that Codex 0.164.0 with `gpt-6.1-sol`, outside the checker, accepted the exact schema including its length and item constraints, returned bare JSON despite a request for fenced prose, and returned a long summary of exactly 4096 ASCII bytes. These are owner-reported observations, not probes repeated for this documentation change or evidence of the confined checker path.
- **Owner acceptance pending.** A hosted live Plan check through the confined checker on the final candidate remains pending and not verified. The owner must run that check and inspect its result; neither the scripted capture nor the direct live probes satisfy it. Delivery for #4103 is non-closing while this owner acceptance remains outstanding.
