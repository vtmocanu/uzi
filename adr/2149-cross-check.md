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

`api/internal/workersvc/cross_check.go` normalizes and stores the candidate and computes its digest over plan, milestones, capabilities, tools, size class, base commit and planning diff. Same-generation retries recover the immutable attempt, including a decided attempt; a mismatched candidate or generation cannot create a second round.

`SetRunAutopilotPlan` in `api/internal/store/queries/runtime.sql` binds the latest plan-stage APPROVE to an opposite-harness `cross_check` child, current lead claim generation, server digest and matching approval-bearing fields. It freezes those fields in the guarded write. `SetRunRunning`, completion and progress guards prevent bypass through adjacent state reports. These are server storage/lifecycle guarantees; they cannot prevent arbitrary execution by a worker that ignores a refusal. Capability claim gates exclude unaware workers, and the worker treats a refused handoff as fatal.

### Bounded fallback, terminal delivery exceptions

REVISE, BLOCK, verdict deadline, checker model timeout, malformed result, model/confinement error, unavailable checker, interrupted attempt, candidate/diff refusal and submit failure normally force the human plan gate. A checker pass is not a substitute for an acknowledged exact-plan handoff.

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

The lander retains the capable-host confinement merge gate against the PR head. Hosted acceptance remains maintainer-owned in acceptance issue 2152. Automatic checker revision, the Codex-lead direction, dedicated checker slots, checker model/effort pins and code cross-check remain outside this implementation. Observed at documentation commit `ba73044a`: `task docs:sync` completed; `task gate:web` PASS (5641 unit tests and 8 Chromium tests, including `check-docs:web`), `npm --prefix web run build` PASS, `task gate:repo` PASS and `task gate:api` PASS, including the embedded-docs package. Secrets scanning reported zero tracked-file findings and detected both canaries; SAST reported zero findings and detected its canary. The read-only docs reviewer reported no mandatory or optional findings. These are component-gate results from this run, not a replay of prior LiveDB or capable-host confinement proof. No runtime source or confinement harness changed.

## Planning-diff capture: symlinks, gitlinks and budgets (2026-10-07, #2410)

Capture budgets are split: the object-store snapshot copy has its own 128 MiB budget, streamed in 16 KiB chunks that yield on a roughly 20 ms clock so SIGTERM cleanup runs inside the 2 s kill grace, and the source reads (worktree compares and verified object reads) keep a separate 128 MiB budget. Measured once during #2410 on a clone of this repository at 24f28b1d through a real TickSpawner: snapshot 74.3 MiB, source 99.7 MiB (about 78% of its budget, the tighter headroom), peak temporary tree near 74.3 MiB, wall 6.6 s against the 28 s deadline.

Symlink posture: an unchanged tracked symlink is accepted by hashing its raw target against the base blob through a pinned parent descriptor; targets are never opened or followed. Added, removed, retargeted, file/link-swapped and untracked non-ignored symlinks refuse with `unsupported_entry`, and a symlinked `.gitignore` is treated as absent, as Git does.

Gitlinks (mode 160000 at base or index, changed or not) also refuse with `unsupported_entry`. Support is deferred: equal base and index gitlink ids do not prove `git add -A` publishes nothing, since a moved submodule HEAD or a removed directory is staged. It needs a proof that the worktree preserves the base gitlink under publication. Refusal sub-codes are persisted and shown with the gate reason (see `docs/cross-check.md`).
