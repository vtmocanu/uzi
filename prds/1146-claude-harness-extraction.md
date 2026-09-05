# PRD #1146: Extract the Claude harness boundary with zero behavior change

**Parent:** [#1106](https://github.com/vtmocanu/uzi/issues/1106), M2 only.
**Status:** Ready; requires merged M0 contract before execution.
**Execution:** Auto mode; MR rework remains enabled.
**Effort:** high.

Implement **only M2 of #1106**: put the existing Claude run lane, advice lane and message mapping behind the accepted neutral interfaces. Claude remains the sole production implementation. This is a behavior-preserving extraction that prepares a later Codex adapter; it does not enable Codex.

Start from current `main` **after the M0 acceptance and its contract companion have merged**. Check that [ADR-1106](https://github.com/vtmocanu/uzi/blob/main/adr/1106-codex-harness.md) is accepted and [the neutral contract](https://github.com/vtmocanu/uzi/blob/main/e2e/codex-m0/harness-contract.md) is present. If either prerequisite is absent, stop and report it. Use a new working branch; never write to `main`.

The parent [PRD #1106](https://github.com/vtmocanu/uzi/issues/1106) supplies rationale, not permission to execute its other milestones. Its provisional full child-issue chain remains blocked. This child authorizes the independently ready M2 extraction only. On completion, open a PR for this child and stop; do not resume, implement, close or move the parent PRD, or start M1/M3/M4/M5/M6/M7. The worker finishes at its PR handoff; the authorized Auto-mode orchestration may review, rework and merge this child. This does not authorize any other parent milestone.

The contract's source inventory is pinned to `bc5a0a8b11f5c98a7067c1fc4202d37a0f27f92e`; implementation must preserve the actual starting `main`. At preparation, `origin/main` was `2e3d0c56e8eb7dd28a61f34be53fb84ff1eabf6e`: `runner.ts` had since gained checkpoint ACK-loss reconciliation (#1086) and preservation of `push_secret_blocked` failure origin (#1077). Keep both fixes and their tests. Recheck source drift offline at start; do not revert to the inventory snapshot or rely on its historical test-file count.

## Binding scope

Preserve D0 from `prds/1106-codex-harness-phase1.md`: the default Claude path retains the same SDK options, prompts, in-process tools, hooks, environment, emitted messages, errors, accounting and lifecycle timing. **The complete `agent/test/` tree must remain byte-identical to the starting base: no edits, additions, deletions, renames or fixture rewrites.** Do not weaken another gate to accommodate the extraction.

Use the accepted type surface and ownership rules in `e2e/codex-m0/harness-contract.md`, especially:

- **Contracts** and **Accounting and wire preservation**: keep provider events inside the adapter, preserve outgoing accounting objects including unknown fields and explicit `undefined` keys, preserve session-versus-call usage basis, and do not invent unavailable measurements or prices.
- **Event, reducer and error authority**: retain grouped filtering, first-surviving-item usage/model attachment, display attribution distinct from signal authority, first run callback versus last turn session ID, immediate progress delivery, display-based stall signals, and the exact context-read trigger and bounded timing. Preserve terminal accounting before failure, local-trip > iterator/close throw > terminal failure precedence, deferred subtype conversion, Error identity and clean-EOF behavior.
- **Rate limits and advice**: preserve terminal-only classification, latest observation and the lane's existing `Date.now()` policy point. Keep synchronous advice callback execution inside terminal iteration before closure, callback-versus-close error precedence and callback replacement of the default. Judge retains its typed inner limit error and deterministic completed fallback on every model failure; review retains failed-review fallback; summary retains null fallback. Preserve timeout abort **and** rejecting race, settlement/grace before HOME cleanup, and cleanup warnings that do not replace the primary failure.
- **Skills and policy inputs** and **Exact source map and M2 handoff**: retain skill survivor allocation/order, explicit empty lists, Claude qualification, tool inheritance versus allowlists, independent deny lists, explicit MCP reach, plan roster copies and post-approval implement selection. Keep constructor/query injection compatibility for current callers and tests, including `SdkQueryFn`, `ReadOnlyModelPassOpts.onResult`, chat and the judge stub. Keep one mapper implementation behind compatibility exports.
- **Lifecycle operation order and evidence**: preserve synchronous legacy `killAgentTree?.()` dispatch at its current call sites, ordinary `reap:false`, per-run PID ownership and setup's current position before `try/finally`. No unconditional `await` of an optional safety hook on Claude paths. Legacy lifecycle results must remain honest (`legacy_unobserved`, `legacy_dispatched`, `legacy_in_process`); they establish no observed-empty guarantee.

The shared advice ceiling permits isolated pure in-memory calculation for both Claude and Codex, but **M2 adds no calculator**. Claude's current `buildDenyAllHook` still denies every tool. Shell/commands, filesystem access, network tools, delegation, run/worker callbacks and credential access remain denied in advice.

## Implementation milestones

- [ ] **1. Extract neutral records, decoding and reduction.**

Create the neutral contract module and reducer (the companion proposes `agent/src/harness.ts` and `agent/src/harness-reducer.ts`). Move Claude decoding behind the adapter boundary; keep `sdk-messages.ts` callable for existing chat/tests without a second independent mapper. Move existing signal parsers rather than changing their rules. Provider SDK imports and raw events must not enter the neutral reducer. Preserve the accepted uzi wire capsule; normalized fields do not replace it.

- [ ] **2. Route the existing Claude run implementation through the seam.**

Create the Claude adapter (the companion proposes `agent/src/claude-harness.ts`) and retain `sdk-executor.ts` as the workflow/constructor compatibility owner. Move query options, process ownership and session inspection; keep planning, approval, watchdog policy, checkpoints, git and run-state transitions with their existing uzi owners.

Split run tool handlers from Claude `createSdkMcpServer` registration in the signal, memory, forge and findings modules. Keep registration in-process with identical names, schemas, descriptions, response objects, error text, role reach and call timing. Reuse the existing memory/findings handler seams. Preserve the forge server's single per-run shared call budget and run-bound closures; do not reset that budget per tool or turn. Signal handlers still return guidance and the stream reducer still owns workflow capture. Chat-only `uzi-tools.ts` behavior stays intact.

- [ ] **3. Extract the Claude advice adapter with existing caller policies.**

Keep `model-pass.ts` as the timeout/callback compatibility owner around the advice adapter. Judge/review/summary retain their outer parsing, fallback, usage and reporting policies. Preserve sparse environment, no run cwd, ephemeral HOME permissions, detached runner-uid spawn, deny-all hook, literal `settingSources: []`, model omission rules and exact label/error strings. All existing advice requests remain text output; prompts requesting JSON do not gain native output-schema enforcement.

- [ ] **4. Validate the extraction and stop at M2.**

Run `task gate:agent` and `task sast:semgrep` against the final tree. Record the command exit status and actual executed test results; a zero failure tally alone is insufficient. Confirm dead-code analysis actually ran, and preserve literal `settingSources: []` at each Claude options construction site. Demonstrate that the Semgrep rule still detects a temporary explicit widening in each moved options site, then restore and inspect the diff; also inspect options coverage for omission, which that rule cannot detect.

Use the unchanged existing tests as the D0 baseline. Where the new seam needs coverage absent from them, add focused credential-free contract/differential tests under **`e2e/harness-m2/`**, leaving `agent/test/` untouched. Add any repeatable gate recipe only to the root `Taskfile.yml`, with explicit typechecking of those additional TypeScript tests, the existing Node/tsx toolchain and `--test-timeout=120000`; run and report that target. Do not maintain a copied legacy production implementation as the test oracle. Include discriminating controls for changed authority/order boundaries; typecheck mutations before interpreting results and restore them before final validation.

Run `task gate:repo` if repo-level files or the additional harness/Taskfile are touched (it includes the security scanners); avoid rerunning `sast:semgrep` separately on an identical tree when that successful repo gate already ran it. Run each gate once to a log per tree and read its result. Require no weakened/skipped check. Record final diff evidence that `agent/test/` is unchanged and `.github/workflows/` has no changes, along with the base and final commit IDs.

## Exclusions and downstream dependencies

No Codex production adapter, app-server launcher, runtime selection, credential handling, refresh/CAS, seat locking, pricing, API/DB/schema migration, UI/CLI feature, image/deployment change, calculator, or prompt redesign. No migration numbers. Do not implement the Codex outer safety facade or grant permits using Claude legacy states; its real supervisor registry, held-epoch enforcement and adversarial conformance belong to M3/M4. Contract types may describe those future obligations, but no fake runtime guarantee or unused production scaffolding may be added merely to appear complete.

Do not create, edit or commit any real file under `.github/workflows/`, including during validation. Do not expose credentials, account identifiers, private hosts, machine paths or raw runtime artifacts in code, commits, issue reports or PR evidence. Use dummy data and assemble any provider-token-shaped fixture at runtime.

The parent's credential-kind/row identity, per-seat identity, CAS/recovery, routing/old-worker compatibility, API-key pricing and M1/M5 dependency cycle remain unresolved. They block later provider enablement, not this Claude-only extraction: M2 selects no Codex credential, creates no new run binding and changes no API wire/accounting contract. If implementing M2 would require deciding one of those contracts or changing Claude behavior, stop and report the specific boundary instead of extending this child.

## Decision and progress log

- 2026-09-06: User authorized M2-only Auto mode with MR rework enabled. Scope inherits accepted M0 contracts; wider provider enablement remains blocked.
