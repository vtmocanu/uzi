# PRD #949: agent god-method split — RunRunner.execute() and SdkExecutor.run() phase extraction

**GitHub Issue**: [#949](https://github.com/vtmocanu/uzi/issues/949)
**Status**: Draft (created 2026-09-01)
**Priority**: Medium
**Parent**: epic #915 (Batch 2, P10; findings G1, G2, G5). Depends on P4 (#920, merged in PR #931).
**Line refs**: re-derived at `5e343113` (post-Batch-1 main). The implementer must re-derive every boundary at their clone's base — file drift of a few lines is expected; the anchors below are identifiers, not offsets.

## Problem

The two largest methods in `agent/` are single-function phase pipelines that must be read whole to change safely:

- `RunRunner.execute(claim)` — `agent/src/runner.ts:404-2490`, **2087 lines** (file 3487). One prologue + one giant `try` (:547) / `catch` (:2234) / `finally` (:2346), spanning clone/resume, pre-flight, executor handoff, MR assembly, and the whole push/MR guard stack.
- `SdkExecutor.run(ctx)` — `agent/src/sdk-executor.ts:562-2003`, **1442 lines** (file 2630). Token/provisioning, roster assembly, hooks/options, plan gate + revise loop, the milestone turn loop, and result assembly.

Both classes already have the extracted-helper pattern (`driveTurn` :2217, `drivePlanningTurn` :2121, `handleLimitReached` runner.ts:2606, …); the main bodies were simply never decomposed. The mirror problem is the test side: `agent/test/sdk-executor.test.ts` is 4164 lines, and `agent/test/prompt.test.ts` (1976 lines) carries **75 lines of negative assertions** (41 of them bare `!RE.test(...)`) that have never had a per-assertion positive-control audit — the vacuous-negative risk the agent-team doc names.

This is guardrail-adjacent code. The refactor is strictly *code motion into private methods on the same class*: guards move verbatim, no deny-hook change, no behavior change.

## Solution

Extract sequential phase methods over an explicit state carrier per class, keeping each method's `try`/`catch`/`finally` skeleton in the original method (the catch/finally read carrier state). Existing tests are the characterization suite and must pass **unmodified**.

**Naming constraint (verified): `RunContext` is already taken** — it is the runner→executor contract type in `agent/src/executor.ts:82`. The new carriers need fresh names (suggested: `RunFlight` for runner.ts, `DriveState`-style for sdk-executor.ts; implementer's choice, just not `RunContext`/`ExecutorResult`/any existing name in the touched files — unexported ones collide too, e.g. `RunDrive` at sdk-executor.ts:392).

### execute() phase map (anchors at 5e343113)

| Phase | Lines | Anchor |
|---|---|---|
| P0 prologue: identity, secrets, redactors | 405-546 | `this.makeExecutor(runId)` :417, `makeRedactor` :449 |
| P1 clone + registration | 548-602 | `this.git.ensureClone` :555 |
| P2 resume / checkpoint reconciliation | 603-798 | `runnerClone.wipRecovered` :603 |
| P3 pre-flight context gathering | 799-926 | `evaluateBaseStaleness(` :821, `this.parseRepoAgents(` :839 |
| P4 ctx literal + executor handoff | 927-1371 | `const ctx: RunContext = {` :927, `executor.run(ctx)` :1360 |
| P5 non-MR outcomes | 1372-1461 | `result.fixVerdict === "not_code"` :1372 |
| P6 MR body assembly | 1462-1745 | `this.git.fetchAgentBranch(` :1462 |
| P7 push/MR guard stack | 1746-2233 | see constraints below |
| P8 catch | 2238-2348 | `LimitReachedError` :2238 |
| P9 finally: teardown | 2350-2490 | secret eviction `this.log.removeSecret` :2380 |

### run() phase map (anchors at 5e343113)

| Phase | Lines | Anchor |
|---|---|---|
| P1 token / HOME / provisioning / deps kickoff | 563-620 | `provisionRunTools(` :586 |
| P2 skills + agent roster assembly | 621-742 | `assembleAgents(` :682 |
| P3 hooks + MCP servers + baseOptions | 743-927 | `buildPreToolUseHook(` :743, `settingSources: []` :837 |
| P4 wall clock / watchdog / cancel wiring | 928-959 | `const state: RunDrive = {` :934 |
| P5 plan gate + revise loop | 994-1347 | `ctx.gatePlan(` :1266 |
| P6 post-plan setup | 1348-1475 | `selectSubagents(` :1410 |
| P7 milestone / turn loop | 1476-1898 | `for (;;) {` :1523, `this.driveTurn(` :1588 |
| P8 result assembly | 1899-1978 | `const result: ExecutorResult = {` :1907 |
| P9 finally | 1979-2002 | `this.killAgentTree()` :1994 |

**Correction to the epic's G2 wording, baked in here:** the usage fold is NOT in `run()` — it lives in `driveTurn` (`usageAttached` :2341, `readLeadContext` :2381) and is out of scope for this split.

## Milestones

- [ ] **M1 — characterization baseline + gap check.** Run `task gate:agent` green at base and record it. Verify the phase seams to be cut are exercised: the 20 `agent/test/runner-*.test.ts` suites (8668 lines; 17 drive `execute()` through `agent/test/runner-harness.ts`'s `runner()`/`runnerWith()` :164/:175, `runner-ask-user.test.ts` constructs `RunRunner` directly, and `runner-fail-origin.test.ts` / `runner-uid.test.ts` exercise helpers rather than `execute()`) and `sdk-executor.test.ts` (every `run()` call is `new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(makeCtx().ctx)` with `fakeTurns()` :121 standing in for the SDK). Where a seam to be cut has NO covering test (checked per phase, e.g. by a temporary throw-at-seam probe run locally, never committed), add a minimal characterization test FIRST, in its own commit, passing against the unrefactored code. Probe caveat: a throw inside `execute()`'s giant `try` is CAUGHT at :2234 and routed to the failure/requeue path — it does not crash the harness — so "covered" means the probe turns a specific test RED, not merely that tests ran; a phase whose only covering test asserts a failure/park outcome can misread either way. Bounded: only seams being cut, not general coverage work.
- [ ] **M2 — extract `execute()` phases** (runner.ts only). One phase (or coherent pair) per commit, private methods on `RunRunner` over the new carrier. The carrier holds the cross-phase mutables measured at base — `barePath` :519, `worktreePath` :520, `branch` :531, `active` :534, `parked` :539, `preserveSession` :546, `lastPublish`/`lastPublishedTip` :525-526, `observedSessionId` :488 — plus the pervasive consts (`runLog`, `batcher`, `redact`/`redactText`, `steering`, `reportState`, `runScopedSecrets`, `runHome`). `try`/`catch`/`finally` stays in `execute()`. All runner tests pass unmodified. `task gate:agent` green per commit.
- [ ] **M3 — extract `run()` phases** (sdk-executor.ts only). Same discipline. Carrier holds `resumeId`, `approvedPlan`, `approvedSelection`, `frozenMilestones`, `latestProgress`, `depsResults`/`depsTruncated`, `iteration`/`maxIterations`, `leadModel`, `state: RunDrive`, `baseOptions` (the `SdkOptions` built in P3 :831 carrying `settingSources: []` :837 and both PreToolUse hooks, consumed at :1235/:1310/:1436 — the guardrail-bearing object crossing the P3→P5→P7 seams), the four `declared*` accumulators plus `scopeCapped` (:1509-1522), and the stall triple (:1494-1496). `sdk-executor.test.ts` passes unmodified. `task gate:agent` green per commit.
- [ ] **M4 — G5: positive-control audit of `prompt.test.ts` negatives.** For each of the 75 negative-assertion lines (`assert.doesNotMatch` ×14, `assert.ok(!…)` ×54, `!x.includes(` ×14, `assert.equal(…, false)` ×6, `!RE.test(` ×41 — forms overlap), verify a positive control exists: mutate the prompt builder (locally, never committed) so the forbidden string IS produced and confirm the assertion fails. Fix each vacuous negative found (typically by adding the paired positive assertion or tightening the regex), in commits separate from M2/M3. Note: `prompt.test.ts` is `node:test` + `node:assert/strict` (no vitest spellings) and imports only `../src/prompt.js`, `../src/findings-tools.js`, and a type-only `../src/protocol.js` — it tests prompt builders directly, so this milestone touches no runner/executor code. **M4 is incremental by design**: audit in batches, one commit per batch; partial completion is acceptable. If the budget runs out mid-audit, leave the M4 box unticked (PRD stays in `prds/`) and enumerate the un-audited assertion lines in the PRD so a later run resumes exactly there.

## Success criteria

1. `execute()` and `run()` bodies reduced to phase orchestration + their original `try`/`catch`/`finally` skeletons; every extracted phase is a private method on the same class. No new files required (same-file extraction is the default; a same-directory helper file is acceptable if the class stays intact).
2. **Zero test edits in M2/M3**: all 20 runner suites, `runner-harness.ts`, and `sdk-executor.test.ts` compile and pass byte-identical. (M1/M4 add or fix tests in their own commits.)
3. Guardrail invariants hold, verified by grep + review, not assumption: exactly one `settingSources: []` at the query-options site (sdk-executor.ts, the semgrep rule catches widened values only — an omitted key is invisible, so the literal must survive verbatim); `buildPreToolUseHook`/`buildPathGuardHook` call sites unchanged; the PAT-bearing `pushToOrigin` closure moves verbatim (see Risks). The isolation invariant is also test-enforced, not only grep-enforced: `sdk-executor.test.ts` asserts `settingSources: []` and the full PreToolUse hook wiring on the real captured query options (:1270, :1283, :1728, :1868, :1951; hook-function exercises :515, :1287-1308) — a mis-threaded `baseOptions` goes red, not silent.
4. `task gate:agent` green (includes knip zero-tolerance — a carrier type accidentally exported and unused reddens it).
5. No `.github/workflows/**` in the branch diff (implementation or validation).

## Decision Log

- **D1 — private methods on the same class, not new modules.** The classes' existing helpers (`driveTurn`, `handleLimitReached`, …) establish the pattern; module extraction would churn imports and visibility for no navigability gain, and guardrail review is easiest when the diff is pure motion within a file.
- **D2 — the `try`/`catch`/`finally` skeletons stay in the original methods.** The catch/finally arms read cross-phase state (`parked`, `preserveSession`, `barePath`); moving them would change error-path semantics, which is exactly what this PRD promises not to do.
- **D3 — M4 rides in this PRD** (epic G5): the test files mirror the god-methods, and the audit is the same characterization discipline M1 applies, pointed at the one file with the densest vacuous-negative risk.
- **D4 — usage fold excluded** (it is in `driveTurn`, not `run()`; epic G2's wording was imprecise and is corrected here).

## Risks & mitigations

- **The PAT push closure's type-narrowing capture.** `pushToOrigin` (runner.ts:1800-1812) captures `finalizeBarePath`, a local created because TS drops the narrowing of the outer `let barePath` inside closures. A phase split that passes `barePath` as a parameter must preserve an equivalent narrowed binding — this is the only site in `execute()` where the forge PAT reaches git; it moves verbatim, closure shape included.
- **Secret registration/eviction is a matched pair** (`this.log.addSecret` :433 in the P0 prologue, `this.log.removeSecret` :2380 in the finally) — both ends STAY in `execute()` under D2, so the pair itself never moves. The live risk is an extracted phase referencing `runScopedSecrets`: it must reach the same collection through the carrier, so both ends keep pointing at one list; an eviction miss leaks secrets into post-run logs.
- **`alignPushed` couples P7c to P7d** (:1824 set, :2130 read): the base-align stack and the final push are one seam, not two — extract them together or thread the flag through the carrier explicitly.
- **Vacuous-negative fixes can over-tighten.** An M4 regex fix that makes an assertion fail on legitimate output is a behavior claim, not a test fix — each fix needs its positive control run BOTH ways (fails on mutant, passes on real output).
- **Knip/deadcode on new carriers**: keep carrier types unexported (or used-by-export) so `task deadcode:agent` stays green; the gate's unused-export family is zero-tolerance.
