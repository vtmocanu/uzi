# PRD #1359: Finalize issue runs against current main before reporting merge-ready

**Issue**: [#1359](https://github.com/vtmocanu/uzi/issues/1359)
**Priority**: High
**Status**: Planned (design approved; implementation gated — see the hard pre-dispatch gate in D8).
**Preserves**: [ADR-0456](../adr/0456-rebase-before-finalize-push.md) (the GitHub workflow-scope base-align and its #627 workflow-subtree overlay) and the reap-before-PAT-git / runner-uid ordering. Reuses the **security posture** of [ADR-1036](../adr/1036-checkpoint-workflow-overlay.md) (no worktree, no filter drivers) — but ADR-1036's `read-tree` subtree overlay is an overlay precedent, **not** a general three-way merge, so forge equivalence is never claimed by citing it (see D3 and M2).
**Shared substrate for**: [PRD #1297](1297-prevent-and-salvage-finalization-blockers.md), which later consumes this PRD's neutral final-candidate-validation + repair-budget + reserve substrate (see D8).

> **Hard pre-dispatch gate: no #1359 implementation milestone may be dispatched until the maintainer approves and lands an amendment to PRD #1297 that makes it consume #1359's shared substrate and resolves the unified counter and reserve ownership.**

## Problem and outcome

A code-publishing run can pass all branch-local gates, report success, and open (or adopt/update) an MR/PR whose **integration result** — the branch merged into the *current* default branch — is unbuildable, because non-workflow changes landed on the default after the run cloned. On GitHub the first merge-ref CI is the concrete detector and is deterministically red. During finalization, `agent/src/runner.ts` (`phasePublish`, base-align at ~1918–2244) fetches the fresh default tip but normally uses it **only** to align `.github/workflows` (the ADR-0456 #627 overlay); it never validates or reconciles the complete merge result. For every forge, no step validates the branch against the current default before reporting merge-ready.

PR #1357 is the concrete occurrence. Its branch added three migrations and generated sqlc code from its original base. Current `main` later added two migrations using the same draft numbers and a new `SELECT * FROM runs` query, and advanced `.github/workflows`. The run reported gates green and finalized via the workflow-subtree overlay (which aligned the workflow subtree but left the migration/sqlc drift untouched). GitHub then tested the real merge result and failed on duplicate goose versions plus sqlc drift (`lint-repo`, `test-api-store-it`, `validate-api` all red while every branch-local gate passed). The first PR pipeline became the integration detector, and automatic CI-fix had to repeat work that should have happened before the initial push.

**Outcome.** Before reporting final validation green and before the run's final publication push (which opens, adopts, or updates an MR/PR), uzi validates the branch against a freshly fetched default-branch tip, and — if the default moved and reconciliation is needed — reconciles before that push rather than knowingly handing the forge an untested merge result. This is not an agent-issued `git pull`: the model keeps no forge credential, and `pull` leaves merge/rebase policy implicit.

## Scope

This is **forge-agnostic**. The collision (colliding migration numbers, sqlc drift against a new default-branch query, any base-integration break) happens on GitHub, GitLab and Forgejo alike; only ADR-0456's workflow-scope overlay is GitHub-specific and is **preserved unchanged**.

### In

- A pre-publish, credential-free validation of the merge result `merge(T, H)` against a freshly fetched default tip `T`, for every run whose output opens, adopts, or updates an MR/PR.
- A provider-neutral, worker-captured validation-command receipt (Claude and Codex adapters) and a per-target-repo validation contract frozen from the trusted `T` tip (D1, D2).
- A worker-owned `RunContext` final-candidate callback that reuses the existing reap-and-continue loop for a bounded, same-session repair turn (D3); no new public `Executor` method.
- A bounded reconciliation state machine (cap = 2 cycles + a final pre-push fetch), per-attempt `(T_i, H_i)` tracking, and the published-head `P_n` pinning invariant (D4, D5).
- New typed, preserved `fail_origin`s and the recorded merge-result **tree** OID plus unified, single-sourced counters and reserve (D6, D7).

### Out

- Relaxing PAT scopes, force-pushing, bypassing push protection, weakening the workflow-edit guard, or importing unrelated default-branch changes beyond ADR-0456's overlay.
- Re-opening the plan approval gate: reconciliation is mechanical integration repair only; drift that invalidates the approved scope fails typed and preserves the work (D7).
- Editing `.github/workflows/**` in implementation or validation (the worker PAT lacks `workflow` scope).
- Any change to PRD #1297's blocker-detection or partial-publication policy; #1297 is cross-referenced read-only and is **not** edited by this PRD or its implementation runs (D8).

### MR-producing-kind matrix (the guarantee applies)

The guarantee is tied to **merge-targeted output**, not to run kind. It applies to every run whose disposition opens, adopts, or updates an MR/PR — concretely the kinds `issue`, `ci_fix` (whose `createMergeRequest` adopts the existing MR, runner.ts:2425–2428), `mr_rework`, `self_improve`, `prompt`, and a handoff task carrying the MR flag (`api/internal/runkind/runkind.go`). **Excluded**: `chat`, `judge`, and — regardless of kind — a `result.reportOnly` completion (runner.ts:1368), a `not_code` verdict (runner.ts:1342), an owner-pause park, a completion-hold, and an empty-diff exit.

## Constraints and known seams (as resolved)

The issue named two seams. Both are resolved below (D2, D3) with two verified facts driving the resolution:

1. **No validation-command receipt exists today.** `guardrails.ts` registers only a `PreToolUse` Bash screen (`buildPreToolUseHook`, ~636); there is **no `PostToolUse` hook** in `agent/src`. `cwd` is available on `BaseHookInput.cwd` (the path guard reads it at guardrails.ts:949; the Bash hook does not). The SDK defines `PostToolUseHookInput{tool_response, duration_ms?}` (sdk.d.ts:2488) and `PostToolUseFailureHookInput` (sdk.d.ts:2470), but does **not** specify that a nonzero Bash exit maps to `PostToolUseFailure` or to a stable structured `tool_response`, and `PostToolUse` does not observe **Codex** tool execution at all. `sdk-env.ts` `buildSdkEnv()` is a full-replacement env with `PROTECTED_ENV_KEYS` (incl. `CLAUDE_CODE_OAUTH_TOKEN`).
2. **The model is reaped before the credentialed publish.** `executor.run()` reaps in its `finally` (`killAgentTree`, sdk-executor.ts:728); the runner reaps again at the security boundary (runner.ts:3930) before `phasePublish` (wrapped in `withCodexBoundaryOnly({boundary:"finalize"})`, runner.ts:748–759). A repair turn cannot be inserted into `phasePublish`. **But** the existing loop already survives a reap and continues: `ctx.checkpoint({reap:true})` calls `reapForSink` (runner.ts:3814), after which the executor recreates the reaped provider epoch (`startProviderEpoch`, `agent/src/codex/codex-executor.ts`:1095/1145/1183) and the next same-session turn keeps its budget, reducer and session. D3 uses that seam.

## Binding decisions

### D1 — Validation set is a per-TARGET-REPO contract, not a global uzi-CI mirror

uzi runs on arbitrary connected repos whose generated-code/integration checks the worker cannot know a priori. Define a **per-target-repo validation contract** frozen from the **trusted `T` tip** (or owner/admin-approved configuration), with the worker allowlist deciding which captured receipts (D2) are eligible. Preserve PRD #1297 D5's rule that a candidate head `H` **cannot weaken** the Taskfile or gate definitions: gate definitions are frozen from trusted `T`, and running `task gate` from an `H`-agent-edited Taskfile is **not** evidence. Configurable extras must name an **owner/admin** authority, never repo text or model text.

**Enforcement.** Validation/gate definitions come from the trusted `T` tip (or owner/admin-approved config), never candidate `H`. Any candidate edit to `Taskfile.yml` or another gate definition either causes replay to use the frozen `T` definition **or** invalidates publication eligibility (no publication-eligible receipt is issued); on detected drift, fail closed. M1 pins a mutation test as a success criterion: an `H_i` that modifies `Taskfile.yml` either replays against `T`'s frozen definition or fails closed with no receipt.

For **this** repo specifically, the contract entry is uzi's own new `check:sqlc-drift` task (added as a source deliverable and regression floor) alongside `gate:repo`'s `check:migration-numbering`, so the #1357 fixture reddens on **both** its failure classes: the migration-number collision (already caught offline by `check:migration-numbering`) **and** the sqlc drift (today caught **only** in `.github/workflows/ci.yml:103–104` via `sqlc generate; git diff --exit-code`, which no `task` target runs).

### D1a — sqlc validation is not assumed offline

sqlc is a pinned `go run` (needs a warm module cache; CI installs a pinned release binary); a cold-cache local run may need network. The implementing PRD milestone must **either** pin sqlc provisioning for the worker toolchain **or** treat a missing/unusable validation tool as a **typed unverified outcome** (fail typed and preserve, never a silent green). It does not promise offline reproducibility by naming a Task target. The offline validation is a strictly-weaker **mirror** of CI (no live DB — `*LiveDB` tests self-skip locally; other CI-only checks enumerated by M6's differential test); success criteria state "the offline-reproducible contract subset was validated," never "CI will pass."

### D2 — Seam 1: a provider-neutral, worker-captured validation receipt (Claude and Codex adapters)

Define a neutral receipt contract capturing, per command: exact command form (shell source; argv where the tool exposes it), cwd (normalized to the worktree root), timeout/cap, source-head OID, **exit status**, and a sanitized non-secret env delta from `buildSdkEnv` (minus `PROTECTED_ENV_KEYS`). Capture is worker-side, never model-authored. Because a `PostToolUse`-only design cannot observe Codex tool execution, and the SDK does not guarantee a nonzero-Bash-exit → `PostToolUseFailure` mapping or a stable structured `tool_response`, exit status is captured at the **command-runner seam where it is authoritative** for each provider. **M0 establishes and fixture-proves this exit-capture mapping for both Claude and Codex before any later step relies on it.** A captured command is replayable only if it passed the guard, exited 0, and matches the worker-owned allowlist of validation command shapes (D1). Neutral receipt names avoid the already-taken `manifest`/`replay` identifiers in `agent/src`.

**Fallback** (the issue's explicit last resort): when no eligible receipt/contract entry applies, one validation-only model turn — still measured by guarded-tool receipts, not the model's word — is used, and the corresponding acceptance criterion is relaxed to "no reconciliation/repair model turn," exactly as the issue permits, rather than contradicted.

### D3 — Seam 2: a worker-owned `RunContext` final-candidate callback (no new public `Executor` method)

Add a new optional `RunContext` member (runner-implemented, e.g. `finalizeCandidate?()`), invoked by the executor loop **before `run()` returns**. The callback runs credential-free, PAT-boundary-correct worker logic:

1. reap the agent tree;
2. PAT-fetch `T_i` with the existing credential-scoped path (`fetchDefaultTip`);
3. ancestry-compare `T_i` to the current candidate head `H_i` by merge-base (not the claim-time base); if `H_i` already contains `T_i`, keep the existing fast path;
4. **construct a non-mutating `merge(T_i, H_i)` result with target-base orientation (base `T_i`, source `H_i`) using an object-level three-way merge primitive selected and differentially proven in M2 (for example `git merge-tree --write-tree`), run under ADR-1036's no-worktree / no-filter-driver security posture only.** ADR-1036's `read-tree` subtree overlay is not a general merge, so forge equivalence is not claimed by citing it. A textual/structural conflict, or a case the primitive does not support or cannot prove equivalent, is a **typed fail and preserve** (`reconcile_conflict`, or a validation-tool-unverified-style typed origin for unsupported/non-equivalent cases) — never a silent green and never a guessed-through merge that drops either parent's content;
5. replay the D1 contract on the merged tree and return one of **{accepted, structured-repair, typed-fail}**.

On **structured-repair**, the **existing loop performs another same-session turn** on the materialized credential-free reconciliation tree, on its existing budget/reducer/session (reusing `reapForSink` + `startProviderEpoch`), yielding a new head `H_{i+1}`; the callback then re-runs. **Materialization boundary**: `H_i` is anchored **immutable**; the reconciliation checkout is bound to a nonce plus `(T_i, H_i, tree(M_i))` and materialized as a **source-fast-forward** merge commit whose parents **retain both `H_i` and `T_i`**; the model's cwd and permissions are confined to that checkout. `H_i` and `T_i` must both remain **ancestors** of the resulting `H_{i+1}`; the materialization is validated then cleaned up, and **no synthetic merge commit is published** (the published head is `P_n` per D5). The #1357 fixture is extended to assert original-`H_i` preservation and the exact `H_{i+1}`→`P_n` publication. This deliberately does **not** reconstruct wall/reducer/iteration state in a second public method across the Sdk/Codex/Stub executors. This callback is the natural shared seam PRD #1297 later consumes (D8). Reap-before-PAT-git is preserved (a reap precedes every credentialed fetch or push); the model never receives the PAT.

A red synthetic gate **triggers the bounded model repair turn** (per the accepted acceptance criterion). Deterministic-first mechanical repair (renumber migrations above head, regenerate sqlc) is recorded only as a deferred Decision-Log alternative, not adopted in the design body.

### D4 — Bounded reconciliation and per-attempt tracking

Track per-attempt pairs `(T_i, H_i)`: `T_i` is the default tip fetched at attempt `i`; `H_i` is the candidate head after any repair for that attempt (`H_0` is the original agent head). The cap is **2 reconciliation cycles plus a final pre-push fetch** (explicit, observable). Fetch once more immediately before push; push `P_n` only while the final fetched default still equals `T_n`. Movement still present at the cap is a typed, preserved failure, not an unbounded retry loop.

### D5 — Forge-agnostic; ADR-0456 preserved in behavior, refactored to consume the pinned `T_n`

The existing align block cannot stay byte-for-byte while its fetch is subsumed — today it independently re-fetches the default (runner.ts:1941–1952), which would let it push `merge(T_{n+1}, H_n)`, a tree never validated (the double-fetch TOCTOU). Preserve its behavior and invariants but **refactor** it to consume the single pinned `T_n` the callback validated. Define the published head `P` for **all three align arms** and prove `tree(merge(T_n, P)) == tree(M_n)` for each:

- **workflow-subtree overlay** (GitHub, under the existing `canOverlay` gate at runner.ts:2108–2109): `P_n = H_n` plus the overlay commit; the overlay only equalizes `.github/workflows` to `T_n` and rewrites no agent commit.
- **whole-tree merge** / **whole-tree rebase fallback**: `P_n` is the aligned tip whose tree already **is** `M_n`.
- non-GitHub, or GitHub with no workflow drift: `P_n = H_n`.

**Non-force publication eligibility (whole-tree rebase fallback).** Publication eligibility binds to the actual **remote source-ref tip `R_i`**, never to the run kind. Immediately before publication, fetch the remote source ref: the whole-tree rebase-fallback output is eligible **only** if that ref is **absent** or `R_i` is an **ancestor** of `P_i` (a non-force, fast-forward update). An existing-MR/PR run normally **fails** this proof after a rebase (its published history was rewritten), while a merge tip stays a fast-forward descendant and passes. On failure uzi does **not** force-push and does **not** substitute an unvalidated alternative — it fails typed and preserves (D7). A source-ref race after the proof is safely caught by the non-forced push and routes to the same typed-preserved failure. Eligibility is **never** inferred from `ci_fix`/`mr_rework` being the run kind; force-push stays prohibited.

What is pushed is `P_n`, **never** the synthetic merge tree, and only while the final fetched default equals `T_n`. Where the forge builds its own merge result from the unchanged `T_n` and the published `P_n` (e.g. GitHub's first merge-ref CI), that forge merge result is tree-equivalent to the validated `M_n`. Default movement after the final fetch but before forge merge-ref creation may legitimately leave the forge pipeline as the next detector; the guarantee is that the published `P_n` was validated as `M_n` against `T_n`. No workflow-scope widening, force-push, agent-visible credential, or weakening of the workflow-edit guard.

### D6 — Record the validated merge TREE OID; unified, single-sourced counters

Record the validated merge-result **tree** OID (`tree(M_i)`), **not** a synthetic merge-commit OID, bound to `(T_i, H_i)`, plus `P_n` and the reconciliation attempt count. Counters are **unified and single-sourced across #1297 and #1359**: one model-repair-rounds counter, one default-movement-attempts counter, and one wall reserve for the final gate plus the final reap and push — none double-counted, and **none reset across resume or budget extend**. Reconciliation consumes the run's remaining iteration/token/wall budget and never resets it; insufficient reserve is a typed, preserved failure. Each new sqlc query needs a static Go caller or test, or `deadcode:api` reddens the gate.

**Publish-intent protocol (crash recovery, resolved before M5).** Publication and its persisted metadata are ordered by an explicit cross-system protocol so a crash between the two is recoverable. Persist a `validated`/`ready_to_publish` **intent** row **first** — it records the decision to publish and **never** claims publication — then push `P_n` non-forced, then verify the remote source ref equals `P`, and **only then** mark the row `published`. Recovery from an intent row fetches the forge ref: equal to `P`, settle idempotently (mark `published`); absent or an ancestor, retry the push **only after re-checking `T`**; diverged, fail typed. A pre-push row must **never** claim publication, and a pushed ref must **never** lack a recoverable validation record.

### D7 — Typed, preserved failures

Add new `fail_origin`s (the implementing milestone pins the minimal set; candidates: `reconcile_conflict`, `reconcile_unreconciled` for a gate still red at the cap, `reconcile_default_moved`, `reconcile_budget_exhausted`, `reconcile_scope_invalidated`, plus a typed validation-tool-unverified origin per D1a and the D3 unsupported/non-equivalent-merge case). Each addition costs the four Go sites (`failOrigins` and the worker-reportable/server-only classification in `api/internal/workersvc/failorigin.go`, plus `preStartInfraFailOrigins` where relevant), a CHECK-widening two-step migration (`NOT VALID` then a separate `VALIDATE`, drafts above the live head `00225`, renumbered at merge), and repointing the hardcoded path in `TestFailOriginVocabularyMatchesCheck`. Preserved work follows the existing `finalize_base_align_conflict` + `runs.preserved_patch` posture. Reconciliation never re-opens the plan approval gate; scope-invalidating drift fails typed rather than silently broadening the plan.

### D8 — Relationship to PRD #1297 (committed decision)

**This PRD owns and lands the neutral substrate**: (i) provider-neutral final-candidate validation (D1–D3), (ii) the bounded repair-budget/rounds counter, and (iii) the wall reserve (D6) — all provider-neutral, forge-agnostic, and target-repo-neutral. **PRD #1297 later consumes this substrate** and layers its blocker-detection and partial-publication policy on top. Rejected alternatives: sequencing #1359 after #1297 (delays a live bug — #1297 is held until #1296 merges and has not started) and validate-only (violates the accepted repair acceptance criterion). #1297 is **cross-referenced read-only** here; `prds/1297-prevent-and-salvage-finalization-blockers.md` is **not edited** by this PRD or its implementation runs.

> **Hard pre-dispatch gate: no #1359 implementation milestone may be dispatched until the maintainer approves and lands an amendment to PRD #1297 that makes it consume #1359's shared substrate and resolves the unified counter and reserve ownership.**

## Milestones and dependency plan

> **Hard pre-dispatch gate: no #1359 implementation milestone may be dispatched until the maintainer approves and lands an amendment to PRD #1297 that makes it consume #1359's shared substrate and resolves the unified counter and reserve ownership.**

Disjoint file ownership per milestone; each artifact is owned exactly once. Migration numbers, the ADR number, and the exact #1297 consumption points are assigned at implementation/merge. New filenames named below are proposed ownership boundaries, not existing artifacts.

| Phase | Milestone | Depends on | Owns (disjoint) |
|---|---|---|---|
| 0 (gate) | **Pre-dispatch gate** | maintainer-landed #1297 amendment consuming the substrate and resolving counter/reserve ownership | blocks M0–M7; nothing is dispatched before it clears |
| 1 | **M0** — Exit-capture proof | gate | The provider-neutral exit-capture seam in the command-runner path (`agent/src/sdk-executor.ts` and `agent/src/codex/codex-executor.ts`) + a Claude/Codex fixture proving nonzero-Bash → captured-exit (`agent/test`). M1/M3 **extend** this seam (sequentially), never re-own it. No store, no state machine. |
| 2 | **M1** — Receipt + target-repo contract | M0 | `agent/src` receipt type + Claude/Codex adapters + the worker allowlist classifier; the trusted-`T`-frozen contract; new `check:sqlc-drift` in `Taskfile.yml` + `scripts/`; the sqlc provisioning decision (D1a). In-worker/HOME receipt only — no DB persistence. |
| 3 | **M2** — Merge primitive + replay harness | M1 | `agent/src/git.ts`: select an object-level three-way merge primitive (e.g. `git merge-tree --write-tree`) and **differentially prove** its tree result and conflict classification against a target-base `git merge` across rename, add/add, gitattributes/filter, binary, submodule, and merge-commit cases (ADR-1036 no-worktree/no-filter security posture only; unsupported/non-equivalent → typed fail, never silent green); the green/red/typed-conflict classifier; the **#1357 fixture** (`agent/test`). No runner/store. |
| 4 | **M3** — `RunContext` final-candidate callback seam | M2 | `agent/src/executor.ts` (the RunContext member) + `agent/src/sdk-executor.ts`/`agent/src/codex/codex-executor.ts`/stub loop wiring reusing reap-and-continue; **no new public method**; no runner `phasePublish` edits. |
| 5 | **M4** — Runner state machine + align refactor | M3 | `agent/src/runner.ts`: the callback implementation + the `phasePublish` pinned-`T_n` refactor of all three align arms + the cap + the MR-producing-kind matrix (D4/D5). No store. |
| 6 | **M5** — Persistence, fail_origins, unified counters | M4 | `api/internal/store` (migrations + queries), `api/internal/workersvc/failorigin.go` + its test repoint, the agent recording calls, and the resume-durable single-sourced counters + reserve (D6/D7). |
| 7 | **M6** — Observability + differential-vs-CI test | M5 | The differential-vs-CI test (single home) + run-record surfacing (minimal web/CLI where warranted). |
| 8 | **M7** — Docs/ADR/spec + integration proof | M6 | The ADR (finalize-integration substrate); the `ARCHITECTURE.md` link; a `specs/human.md` line if the maintainer accepts it as a new user-stated requirement; `task docs:sync`; the deterministic cross-component proof of every acceptance criterion; touched component gates. |

- [ ] **M0: Prove authoritative exit capture for both providers.** Establish the provider-neutral command-runner seam that records a Bash command's exit status for Claude and Codex, with a fixture proving a nonzero exit is captured (not inferred from a `PostToolUse` event kind). Gate: `task gate:agent`.
- [ ] **M1: Capture the receipt and freeze the target-repo contract.** Provider-neutral receipt type + adapters, the worker allowlist classifier, the trusted-`T`-frozen validation contract, uzi's own `check:sqlc-drift`, the D1a sqlc-provisioning decision, and the frozen-gate enforcement + mutation test (an `H_i` `Taskfile.yml` edit replays against `T` or fails closed with no receipt). Gate: `task gate:agent`, `task gate:repo` (for the new Task target), `task scan:secrets`.
- [ ] **M2: Select and prove the merge primitive; build the replay harness.** Object-level three-way merge, differentially proven against a target-base `git merge` across the enumerated cases; the green/red/typed-conflict classifier; the #1357 fixture green after reconcile and red before, asserting original-`H_i` preservation and the exact `H_{i+1}`→`P_n` publication. Gate: `task gate:agent`.
- [ ] **M3: Add the final-candidate callback seam.** New `RunContext` member + executor-loop wiring across Sdk/Codex/Stub reusing reap-and-continue; no new public method. Gate: `task gate:agent`.
- [ ] **M4: Wire the runner state machine and refactor the align block.** Callback implementation; pinned-`T_n` refactor of all three align arms with the per-arm tree-equivalence proof; the remote source-ref (`R_i`) non-force publication eligibility proof; the cap and per-attempt tracking; the MR-producing-kind matrix and exclusions. Gate: `task gate:agent`.
- [ ] **M5: Persist records, add typed origins, unify counters.** Record the validated merge tree OID + `(T_i, H_i, P_n, attempt count)`; the publish-intent row + persist→push→verify→mark ordering (D6); new `fail_origin`s with the CHECK-widening two-step migration and the test-path repoint; the resume-durable single-sourced counters + reserve. Gate: `task gate:api`, live-store tests, `task scan:secrets`.
- [ ] **M6: Make the guarantee observable and pin the CI mirror.** Surface the run-record fields (minimal web/CLI); the differential-vs-CI test enumerating offline-vs-CI divergences. Gate: `task gate:web`, `task gate:api`.
- [ ] **M7: Prove integration; sync docs/ADR/spec.** Deterministic cross-component proof of every acceptance criterion; the ADR (finalize-integration substrate); `ARCHITECTURE.md` link; a `specs/human.md` line if accepted; `task docs:sync`. Gate: touched component gates serially, `task gate:repo`, `task check-docs:web`.

## Success criteria (mapped to the issue's acceptance criteria)

- **Forge-agnostic**: the integration guarantee holds on GitHub, GitLab and Forgejo for every run whose output opens, adopts, or updates an MR/PR (the D5 matrix); report-only and no-MR completions are unaffected.
- **#1357 fixture**: a fixture where the branch and updated default add colliding migration numbers and the default adds a generated-query input is reconciled and gate-green (on the D1 contract, which includes `check:migration-numbering` and `check:sqlc-drift`) before the publication push. The fixture also asserts original-`H_i` preservation and the exact `H_{i+1}`→`P_n` publication.
- **Frozen-gate mutation test**: an `H_i` that edits `Taskfile.yml` (or another gate definition) either replays against `T`'s frozen definition or fails closed with no publication-eligible receipt (D1).
- **Fast path**: a branch whose head already contains the current default tip retains the existing fast path.
- **Clean, non-overlapping default update**: a green synthetic merge result pushes with zero model turns and no whole-tree merge/rebase or repair commit (the GitHub workflow-subtree overlay's SHA-preserving alignment commit may still be appended; no agent commit is rewritten). Relaxed to "no reconciliation/repair model turn" only if the D2 validation-only-turn fallback is exercised.
- **Clean merge, red synthetic gate**: triggers one bounded repair turn, then a full gate re-run, and pushes only when green.
- **Textual/structural merge conflict**: never guessed through and never silently drops either side; the run fails typed with preserved work (D3/D7).
- **Recording and pinning**: uzi records and validates the merge-result tree `tree(M_n)` bound to `(T_n, H_n)`; the published head is `P_n`, one of the three D5 arms (`= H_n`; or `H_n` plus the GitHub workflow-overlay commit; or, on a whole-tree merge/rebase align, the aligned tip whose tree already **is** `M_n`); uzi pushes `P_n`, never the synthetic merge tree, and only while the final fetched default equals `T_n`. When `P_n != H_n`, `tree(merge(T_n, P_n)) == tree(M_n)` is recorded and proven for the fired arm. The run records `T_n`, `H_n`, `P_n`, the validated `tree(M_n)`, and the reconciliation attempt count.
- **Non-force publication**: a whole-tree rebase-fallback push is eligible only when the remote source ref is absent or a fast-forward ancestor of `P_i`; otherwise the run fails typed and preserved, never force-pushed, and eligibility is never inferred from the run kind (D5).
- **Publish-intent recovery**: a crash between push and persisted metadata is recoverable via the D6 intent protocol; no pre-push row claims publication and no pushed ref lacks a recoverable validation record.
- **Bounded and observable**: default movement during reconciliation is bounded by the explicit cap and is observable; movement at the cap is a typed, preserved failure.
- **Budget**: insufficient remaining budget for the reconciliation gate plus the final reap/push fails typed with preserved work rather than pushing an unvalidated tree.
- **No regressions**: no workflow-scope widening, force-push, agent-visible forge credential, or weakening of the existing workflow-edit guard; ADR-0456's GitHub overlay and the runner-uid/reap ordering are preserved.
- **Honest mirror**: the offline validation is a strictly-weaker mirror of CI; success is "the offline-reproducible contract subset was validated," never "CI will pass" (D1a, M6).

## Risks and accepted boundaries

- **Merge-primitive equivalence.** An object-level three-way merge may diverge from the forge's own merge in rename/filter/binary/submodule/merge-commit cases. M2 differentially proves equivalence and fails typed on unsupported/non-equivalent cases; forge equivalence is never assumed from ADR-1036.
- **CI-vs-local mirror gap.** Some CI checks (live-DB `*LiveDB`, and — until `check:sqlc-drift` lands — sqlc drift) are not reproducible offline. M6's differential test enumerates the divergences; the guarantee is scoped to the offline-reproducible subset (D1a).
- **sqlc provisioning.** A cold module cache can make local sqlc need network. Pin worker provisioning or emit a typed unverified outcome; never a silent green (D1a).
- **Hot-main liveness.** The strict `default == T_n` pre-push equality can, on a fast-moving default, force a typed preserved failure even when the new default is merge-irrelevant. Accepted for correctness; a path-scoped narrowing (re-validate only when the new default touches merge-relevant paths) is a possible follow-up, not adopted here.
- **Substrate coupling with #1297.** The unified counters and reserve are single-sourced; the hard pre-dispatch gate ensures #1297 consumes the substrate and resolves ownership before any #1359 implementation milestone is dispatched.

## Decision and progress log

- **2026-09-14** — PRD authored (design approved through the lead's plan-approval gate). Investigation confirmed: the reap/publish ordering (runner.ts:3930, sdk-executor.ts:728), the reap-and-continue seam (`reapForSink` runner.ts:3814 + `startProviderEpoch`), the absence of any `PostToolUse` hook and the SDK's non-guarantee of a Bash exit → `PostToolUseFailure` mapping, the CI-only sqlc-drift check (`ci.yml:103–104`) vs the offline `check:migration-numbering`, and the double-fetch in the existing align block (runner.ts:1941–1952). Anchors verified against HEAD.
- **2026-09-14** — CodeRabbit review folded. Bound replayed gates to the trusted `T` tip with fail-closed enforcement plus a mutation test (D1); added the structured-repair materialization boundary that keeps both `H_i` and `T_i` as ancestors of `H_{i+1}` and publishes no synthetic merge commit (D3); bound whole-tree-rebase-fallback publication eligibility to the remote source-ref tip `R_i` as a non-force fast-forward, never the run kind (D5); and defined the persist-intent, push non-forced, verify, then mark-published protocol with idempotent crash recovery (D6).
- **Deferred alternative (not adopted): deterministic-first mechanical reconcile.** Attempting deterministic repair (renumber migrations above head, regenerate sqlc) before any model turn would minimize model turns, but the accepted acceptance criterion states that a red synthetic gate triggers the bounded model repair turn, and the drift classes extend beyond deterministic renumber/regen (test adjustment, arbitrary base-integration breaks). Kept as a possible optimization within a milestone, not as the design body.
- **Deferred (not adopted): a new public `Executor.reconcileTurn` method.** Rejected in favor of the `RunContext` final-candidate callback (D3), which reuses the existing reap-and-continue loop and avoids reconstructing wall/reducer/iteration state across three executors.
- **Hard pre-dispatch gate: no #1359 implementation milestone may be dispatched until the maintainer approves and lands an amendment to PRD #1297 that makes it consume #1359's shared substrate and resolves the unified counter and reserve ownership.** Amending #1297 is a maintainer-gated action, out of scope for #1359's own runs.
