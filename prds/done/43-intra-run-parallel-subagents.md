# PRD #43: Intra-run parallel subagents — parallel validators and disjoint-scope parallel coders

**GitLab Issue**: [vtmocanu/uzi#43](https://github.com/vtmocanu/uzi/-/issues/43)
**Status**: Complete (2026-07-12) — M0 ✅ (verdict: **OVERLAP**), M2 ✅, M1 ✅, M5 ✅, M4 ✅ landed on `feature/prd-43-intra-run-parallel-subagents`; **M3 waived by user decision 2026-07-12 (no human-driven tests for this PRD)** — the parallel path's live behavior is validated by the M0 probe + unit/e2e guards only; Decision 6's feed-legibility question stays open until a real parallel run happens organically. Draft was reviewed 2026-07-10/11 by 3 agents (fact-check, security, design); all blocking/major findings folded in (marked ↳review where the design changed). The headline sequencing change: **the SDK-concurrency premise is proven first (M0) before any template prose ships.**
**Priority**: Medium
**Created**: 2026-07-10
**Depends on**: PRD #3 (agent templates, done), PRD #4 (worker runtime + SDK executor, done). **Independent of PRD #42** (worker *run* concurrency — that PRD parallelizes across runs; this one parallelizes inside one run's SDK session), except the **multiplicative token-pressure interaction** named in Decision 5 (↳review). **Interacts with**: PRD #37 (template allocations decide which subagents a run has — the prose must not hardcode names), PRD #41 (a revised plan must re-declare the parallel split).
**Inspiration**: the `agent-team` skill's orchestration patterns (parallel read-only validators in one wave; parallel writers only with explicit scope isolation; lead integrates and owns the gates) — adapted to the in-run reality that subagents are same-session tool calls sharing one worktree, not separate git-capable sessions.

## Problem

A run's lead orchestrator delegates strictly serially. The builtin `lead` template (`api/internal/agenttmpl/builtins/lead.md`) says:

> "Give each one enough context to succeed, **run it and wait for its result in the same turn**, then integrate the result and verify it."

So reviewer and auditor always run one after the other, a tester waits for both, and two independent implementation units (say, an API handler and a web page) are built sequentially — doubling or tripling wall-clock time on the most expensive part of a run (model turns), while little in the architecture requires it:

- The SDK **appears to support** multiple in-flight subagents in one turn (`sdk.d.ts:2127` "PostToolUse … may run concurrently for parallel tool calls"; `sdk.d.ts:2866` "Backgrounds in-flight foreground tasks (Bash commands and subagents)" — plural in-flight foreground subagents), but no repo-provable guarantee exists that same-turn Agent calls dispatch concurrently — **and the claim is in direct tension with the B1 guard**: `buildAgentGuardHook` validates subagent names AND rewrites `run_in_background: false` on every allowed call (`guardrails.ts:453-464`) to keep delegation in-turn. Whether two *forced-foreground* subagents run concurrently within the turn is exactly the open question (↳review design, blocking): **M0 proves or refutes it before anything else ships.** If the SDK serializes them, this PRD rescopes to the M2 hardening only.
- Read-only subagents (reviewer, auditor, tester, fact-checker — read-only via their declared `tools` allowlists, `agents.ts:19-27`) can all read the same worktree concurrently with zero conflict potential.
- Parallel *coders* are feasible in the shared worktree when their ownership is disjoint **at the package/module level, not just file level** (↳review design, major — Go compiles per-package and `tsc`/`node --test` are project-wide, so file-disjointness inside one package is not independence): subagents are tool-call executions inside one SDK session (shared `cwd: ctx.worktreePath`, `sdk-executor.ts:209`), and edits to disjoint packages don't interfere. This is unlike the cross-session two-writers problem: there is no separate HEAD to diverge. (↳review fact-check: "the lead is the only committer" was overstated — `coder.md:18` expects coders to report "commits made (if any)" and nothing in the deny-list blocks `git commit`; the *enforced* facts are narrower: the agent never pushes, and the worker opens the MR. Parallel mode therefore explicitly forbids coder commits in the prose, because concurrent commits contend on the git index.)

The behavior we already trust in this repo's own dev workflow (the `agent-team` skill: validators fan out in one wave; writers parallelize only with explicit non-overlapping scopes; the lead merges and runs the gates) has no equivalent in the product's builtin templates.

## Solution Overview

Prove the premise, then teach the builtin `lead` to fan out where independence is provable, keep it serial where it is not, and make the coder safe to run as one of several:

1. **M0 premise spike first** (↳review): demonstrate two same-turn foreground subagents actually overlapping under the real guard hook, before any prose lands.
2. **Parallel validators by default**: after an implementation unit lands, dispatch all *allocated* read-only validators together in a single turn (not a hardcoded reviewer+auditor list — allocations vary per run, PRD #37).
3. **Parallel coders when — and only when — the plan splits into units with disjoint package/module-level ownership and no dependency blockers**: explicit non-overlapping scopes, no coder commits, no repo-wide gates from coders — the lead integrates, commits, and runs the gate once.
4. **The lead integrates and runs the gates; the worker owns the push and the MR** — unchanged except that parallel coders don't commit (serial coders may, as today — ↳review).
5. **One small guardrail hardening, valuable independently of everything above** (↳review security major + design N7): subagents must not reach the run's workflow-signal tools (`mcp__uzi__submit_plan` / `mcp__uzi__signal_done`) — a *serial* prompt-injected coder can already end the loop and push a partial tree today; parallel mode just multiplies odds and blast radius. Lands regardless of M0's outcome.

## The prompt change (the core of this PRD)

### `lead.md` — replace the serial-delegation sentence

Current (`api/internal/agenttmpl/builtins/lead.md`):

> "Prefer delegating focused, well-scoped units of work to the available subagents over doing everything on the main thread; the set of subagents you can delegate to is provided to you each turn. Give each one enough context to succeed, run it and wait for its result in the same turn, then integrate the result and verify it. Iterate between implementation and review until the review is clean."

Replacement (draft — exact wording tuned in M1; restructured as a list per ↳review design N1, since a nine-imperative run-on sentence drops middle clauses and makes phrase-pinning tests brittle):

> "Prefer delegating focused, well-scoped units of work to the available subagents over doing everything on the main thread; the set of subagents you can delegate to is provided to you each turn. Dispatch independent subagents in parallel in a single turn:
>
> - Read-only work always fans out: after an implementation unit lands, send all allocated read-only validators together in one wave.
> - Implementation work fans out only when your plan splits it into units with no dependency between them and disjoint ownership at the package or module level — two parallel units must never touch the same Go package, the same TypeScript project, or any shared file. Shared wiring is never disjoint: go.mod, go.sum, lockfiles, generated code, routers and registration files, compose or config files — if a unit needs one of those, run it serially or make that edit yourself during integration. The same coder subagent may be invoked several times in parallel, one invocation per unit.
> - Give each parallel implementer an explicit, non-overlapping list of files and directories it owns, stated in its delegation prompt, and tell it not to commit and not to run repo-wide build or test commands.
> - After all parallel results are in, you integrate: diff the working tree against the last commit and confirm only the declared scopes changed, commit, run the quality gates once yourself, and include the declared scope map when you dispatch the review wave so an out-of-scope change surfaces as a finding.
> - When in doubt — overlapping scopes, same package, uncertain dependencies — run them serially. Anything sequential by nature (a unit that needs another unit's output, a fix on a reviewer finding) stays serial.
>
> Give each one enough context to succeed and wait for the results in the same turn, then integrate and verify. Iterate between implementation and review until the review is clean."

(↳review: "all allocated read-only validators" instead of naming reviewer/auditor — an allocation may omit either (PRD #37); the explicit "same coder subagent may be invoked several times" clause prevents the lead from reading a one-coder allocation as unparallelizable; the diff baseline is named ("the last commit" = HEAD); shared-wiring exclusions are enumerated.)

### `coder.md` — add the parallel-mode contract

Append one paragraph:

> "You may be dispatched as one of several coders working in parallel in the same worktree. When your delegation prompt assigns you a file scope, treat it as a hard boundary: create and edit files only within it, and if the task genuinely requires touching anything outside it — including shared files like go.mod, lockfiles, generated code, or wiring/registration files — stop and report that instead of editing it. In parallel mode do not run `git commit`, and do not run build or test commands unless they cover only code you exclusively own; otherwise just report your edits — the lead integrates, commits, and runs the repo-wide gate after all parallel units land."

(↳review design M-1: "targeted compile of what you changed" was largely non-actionable in a per-package/project-wide toolchain; the contract is now "verify only what you exclusively own, else don't verify — report".)

Read-only role templates (`reviewer.md`, `auditor.md`, `tester.md`, `fact-checker.md`) need no change — they are already parallel-safe by construction.

## Design Decisions

1. **Single shared worktree; scope isolation is a prompt-level contract with a prompt-level integration check (↳review design N3 — nothing here is mechanical except the path jail and the human-merged MR).** Per-coder worktrees (the agent-team skill's writer isolation) are rejected for the in-run case: subagents are same-session tool calls, not git sessions — there is no per-writer HEAD/index to protect, the path-guard hook jails all file tools to the run worktree (`buildPathGuardHook(ctx.worktreePath)`, `sdk-executor.ts:244`), and per-subagent worktrees would require widening the most security-sensitive hook plus lead-side merge machinery for little gain over disjoint scopes. Honest limits: **mid-flight cross-contamination has no serial analog** (↳review security — a compromised unit A can plant content in a file unit B reads mid-flight; caught only at the lead's integration diff, the scope-aware review wave, and the MR), and the integration diff itself is an instruction the lead-model could skip. Residuals accepted and documented.
2. **The parallelization unit is the package/module, not the file (↳review design M-1/M-2).** Two units are parallelizable only when no Go package, no TS project scope, and no shared file (incl. go.mod/go.sum, lockfiles, sqlc/generated output, routers/registration, compose/config) is touched by both. Consequence stated honestly: **same-language units frequently are not parallelizable** — the sweet spot is cross-area splits (api ↔ web ↔ agent ↔ docs), which is also where uzi PRDs naturally split (this repo's own PRD phase tables). The lead owns shared-wiring edits during integration. Parallel coders don't commit (index contention) and don't run repo-wide gates (concurrent `go test ./...` over a tree two agents are editing is flaky, misleading signal); one gate, one committer, after integration.
3. **One guardrail hardening; everything else unchanged (↳review security major — the original "no guardrail changes" claim was wrong; ↳review design N7 — the hardening is valuable standalone).** The run's workflow-signal MCP tools (`mcp__uzi__submit_plan`, `mcp__uzi__signal_done`) are registered at the session top level (`sdk-executor.ts:229`) with only an aspirational comment gating them to the lead — but the coder inherits ALL tools (no `tools:` line ⇒ SDK inherit-all, `sdk.d.ts:44`), subagents disallow only `Agent` (`agents.ts:73`), and `scanSignals` honors a signal from ANY assistant frame without checking `subagent_type` (`signals.ts:91-112`, applied at `sdk-executor.ts:429-431`). A coder tripping `signal_done` ends the implement loop and hands a **partial, unreviewed** tree to the worker's push+MR — already possible in serial flow today; N parallel coders multiply odds and impact. Hardening (both, belt-and-suspenders): (a) subagent `disallowedTools` grows to `[NESTED_AGENT_TOOL, "mcp__uzi"]` (server-level MCP denial, `sdk.d.ts:48`); (b) `scanSignals`/`driveTurn` honor signals only from main-thread frames. The remaining guardrails (name allowlist + `run_in_background:false` rewrite, async-deferral denial, path jail, Bash deny-list) apply to parallel subagents unchanged; B1 is untouched — verified: one CLI process per turn, all subagents inside its process group, the pre-push `killAgentTree` group-kill covers them all (`sdk-executor.ts:379-386,145-148`, `sdk-spawn.ts:22-30`).
4. **Builtin propagation must be designed, not assumed — and reset CLOBBERS admin customizations (↳review design M-3).** Boot seeding is insert-only (`api/internal/store/queries/agent_templates.sql:74` `ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING`), so editing `builtins/lead.md` reaches **new databases only**. The only upgrade path for existing deployments is `ResetAgentTemplate` (`handler/agent_templates.go:389`), which re-applies the embedded builtin **verbatim — discarding any admin customization of that template**. The population most likely to reset (admins who customized `lead` and want the new parallel prose) is exactly the one that loses edits. Decision: keep insert-only seeding; `docs/agent-templates.md` + release notes state plainly that reset discards customizations and give the safe recipe (diff your current body against the new builtin, reset, re-apply your edits — or hand-merge the new paragraphs without resetting). A UI drift indicator ("builtin changed since seeded") stays a stretch goal.
5. **Token pressure: multiplicative with PRD #42, and named as such (↳review design M-5).** N parallel subagents multiply concurrent requests on the one per-user Anthropic token — and once #42 lands, a cap-N worker running M-wide fan-outs is N×M concurrent streams on one token; neither PRD's "the SDK's own backoff handles it" acceptance accounts for the product. Recorded consequence: the per-user token concurrency budget (out of scope in both PRDs) becomes materially more pressing once **both** land; until then the practical width is bounded by plan shape (2-3 units) times #42's default cap of 1, and the lead prose's "when in doubt, run them serially" is the brake. No cap knob in MVP; revisit on evidence of pathological fan-out.
6. **Activity feed: validate under interleaving, and honestly — two parallel same-named coders are indistinguishable today (↳review design M-4).** Attribution is envelope-derived and unforgeable (`subagent_type`, `sdk-messages.ts:28-30`) and per-run `seq` stays gapless/total (`runtime.sql:363-374`) — but the feed groups purely by agent NAME (`ActivityFeed.tsx:40-49`), so two concurrent invocations of `coder` merge into one section with interleaved, mutually-contradictory narratives, and every speaker change fragments blocks. M3's feed check therefore has a defined bar: (a) two same-named parallel coders, (b) heavy interleave fragmentation — legible or not? If per-invocation attribution is required, that is a real (small) code change (a per-invocation id on the frame → feed keys on it), filed as a follow-up, not hand-waved as already handled.
7. **The plan is the parallelization contract — including revised plans (↳review design N8).** The plan approved at the gate shows the split ("units A, B in parallel — disjoint scopes; unit C after A"), so parallel execution is never a surprise; a plan revision (PRD #41's revision gate) must re-declare the split and scopes as part of the revised plan.
8. **Accepted residual: the `setsid` escape scales with parallel width (↳review security).** The one process-survival class the reap can't cover (`docs/proc-hardening.md`) now has N concurrent potential escapees racing the short PAT push window instead of one. No new survival class; same proc-hardening endgame; noted in the docs where the parallel behavior is described.

## Technical Design

### Premise spike (M0 — before everything)

A throwaway live probe (dev stack, real executor, cheap task): a lead prompt that dispatches two trivial foreground subagents in one turn under the real `buildAgentGuardHook` (with its `run_in_background: false` rewrite). Evidence of concurrency = overlapping `tool_use` timestamps across the two subagent frames within one turn (each subagent runs a short `sleep`-then-echo so overlap is observable). Outcome A (they overlap): proceed to M1. Outcome B (the SDK serializes forced-foreground subagents): stop the parallelism track, land only M2 (the standalone hardening), and record the finding in `specs/ai.md`. No prose or template change ships before this resolves.

### api/ (template bodies + tests)

- `api/internal/agenttmpl/builtins/lead.md`: the prompt change above (list form).
- `api/internal/agenttmpl/builtins/coder.md`: the parallel-mode paragraph above.
- Existing parse/validity tests keep passing; extend them to pin the load-bearing phrases (parallel-dispatch bullets in `lead`; scope-boundary + no-commit-in-parallel contract in `coder`) so a future rewording that drops the behavior fails loudly — mirroring how `TestCoderInheritsAllTools` (`render_test.go:104`) pins the coder's no-`tools:`-line invariant today. The list structure makes per-phrase pinning robust (↳review design N1).
- No schema change, no new endpoints. (`ResetAgentTemplate` already exists for propagation.)

### agent/ (the signal hardening — ↳review; no other code change expected)

- `agents.ts` `toDefinition`: subagent `disallowedTools` grows from `[NESTED_AGENT_TOOL]` to `[NESTED_AGENT_TOOL, "mcp__uzi"]` (server-level denial).
- `signals.ts` / `sdk-executor.ts` `driveTurn`: honor `submit_plan`/`signal_done` only from main-thread frames (no `subagent_type` on the frame).
- Tests: a subagent frame carrying a signal does not latch `done`/plan; the definition assembly emits the MCP denial for every subagent (builtin and custom).
- Validation-only (no change expected): the per-run message batcher under interleaved subagent messages; `spawnedPids` is one CLI process per turn regardless of subagent count (the group-kill already covers parallel subagents' children).
- If PRD #42 lands first, its per-run executor/HOME isolation composes with this PRD; neither depends on the other.

### web/ + docs

- Feed: no planned code change; M3's feed check (Decision 6) decides whether per-invocation attribution (a small frame-id + feed-key change) becomes a filed follow-up.
- `docs/agent-templates.md`: the new lead behavior, the builtin-reset-discards-customizations caveat + safe recipe (Decision 4), and the `setsid`/token-width residual notes.
- `specs/ai.md`: decision entry (and the M0 outcome).

## Milestones

- [x] **M0 — SDK concurrency spike (gates the whole parallelism track)**: prove two same-turn foreground subagents overlap under the real guard hook (overlapping tool_use timestamps), or record that they serialize. Manual, human-driven (real token; no CI in this repo). Outcome decides whether M1/M3/M4 proceed or the PRD collapses to M2. Validation: a recorded probe transcript showing overlap (or not). **Done 2026-07-12 — verdict: OVERLAP (both runs).** Standalone probe (SDK 0.3.201, guard-hook Agent-guard replicated verbatim incl. the `run_in_background:false` rewrite, `settingSources:[]`, subagent `disallowedTools:['Agent']`): filesystem-marker ground truth showed subagent B starting 0.36s/0.43s after A with ~19.6s overlap of the two 20s sleep windows; total wall ≈ 20s, not 40s. Notably the two Agent tool_use calls landed in separate assistant frames ~0.3-0.6s apart and STILL overlapped. Evidence: `prds/43-m0-probe/` (probe.ts, per-run findings.json + markers.log).
- [x] **M2 — Subagent signal hardening (agent/) — unconditional, lands regardless of M0**: `mcp__uzi` denied to all subagents, main-thread-only signal scanning, tests incl. a subagent `signal_done` attempt that must NOT terminate the run; `cd agent && npm run typecheck && npm test` green. (↳review security major + design N7 — fixes a real hole in today's serial flow too.) **Done 2026-07-12, commits `8fe11c4` + `bcaa46f`; review APPROVE + audit clean over the full range.** Implementation note: the frame discriminator gates on non-empty `subagent_type` OR non-null `parent_tool_use_id` (both SDK-stamped), centralized in `scanSignals` (the sole signal consumer); the worker-side scan is the load-bearing layer, the `mcp__uzi` server-level denial is defense-in-depth (disallowedTools-vs-explicit-allowlist precedence is unproven from SDK types).
- [x] **M1 — Template changes + pinned tests (api/) — only if M0 = overlap**: `lead.md` + `coder.md` prose landed (list form, package-level granularity, shared-wiring exclusions, allocation-agnostic naming), agenttmpl tests extended to pin the load-bearing phrases; `go build ./... && go test ./...` green. Validation: parse tests pass; a fresh DB seeds the new bodies. **Done 2026-07-12, commit `cc320ea`; review APPROVE (full PRD fidelity, 21 phrase pins: 14 lead + 7 coder) + audit clean (no guarantee weakened; plan gate/review wave stay non-optional; gating relocated to the lead, not removed).**
- [~] **M3 — Live validation of the parallel path — only if M0 = overlap; manual, human-driven**: on a real run with a hand-crafted two-disjoint-unit (cross-area) issue: (a) two same-turn coder dispatches execute concurrently (overlapping frame timestamps); (b) the validator wave runs in one turn; (c) guardrails hold incl. the M2 signal denial exercised live; (d) the feed renders two same-named coders + heavy interleave against the Decision 6 bar; (e) the lead diffs against HEAD, confirms only declared scopes changed, commits once, runs the gate once. Named owner: the developer running the PRD. Findings that need code get filed/fixed here or split out. **WAIVED 2026-07-12 by user decision ("we won't do any human-driven tests"). Coverage that stands without it: M0 probe (SDK concurrency under the real guard, twice), M2 unit tests (signal denial), M5 e2e (interleaved persistence/replay). NOT covered: live guardrail behavior under a real parallel run and the Decision 6 feed-legibility bar — to be observed on the first organic parallel run instead.**
- [x] **M4 — Propagation + docs**: `docs/agent-templates.md` (reset-discards-customizations recipe, new lead behavior, residual notes), release-note snippet, `specs/ai.md` decision entry + M0 outcome. Validation: `cd web && npm run build` (check-docs) green. **Done 2026-07-12, commits `04baf9b` + `0eccf31` + fixups `b0e37e5`/`17a98bc`; review APPROVE + fact-check 0 refuted (all claims verified against code + probe evidence).** Release-note snippet ratified out-of-scope: the repo has no release-notes/CHANGELOG infra (same precedent as PRD #42); the what's-new content lives in the doc sections themselves and the reset recipe points at the builtins' git history instead.
- [x] **M5 — E2E guard**: extend an e2e scenario (stub executor scripts multiple agent messages — it writes markers + scripted messages and never calls the SDK, `agent/src/executor.ts`, so interleaved-stream persistence is the piece the isolated stack CAN exercise) to assert interleaved multi-agent message streams persist and replay correctly (seq gapless, per-agent attribution intact). Validation: `./e2e/run-e2e.sh` green. **Done 2026-07-12, commit `41a2a97`; e2e exit 0 (4 PASS lines), review APPROVE (assertions proven non-vacuous; replay pivot matches the server's strict `seq > after`) + audit clean (UZI_STUB_INTERLEAVE sentinel unreachable on the production sdk path; no signal frames; guardrail files untouched).** Scope note: proves persistence + REST `?after` replay; the /api/ws leg is web-vitest scope (specs §194).

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched | Repo area |
|---|---|---|---|---|
| 0 | M0 (spike), M2 (hardening — parallel, unconditional), M5 (e2e stream guard — parallel) | — | probe (throwaway) · `agent/src/{agents,signals,sdk-executor}.ts` + tests · `e2e/` | dev stack · agent · e2e |
| 1 | M1 (templates+tests) | M0 = overlap | `api/internal/agenttmpl/builtins/{lead,coder}.md` + tests | api |
| 2 | M3 (live validation) | M0+M1+M2 | none expected (findings may touch `agent/`) | dev stack |
| 3 | M4 (docs/specs) | M3 | `docs/`, `specs/` | docs |

## Out of Scope

- Per-subagent worktrees, branch-per-coder, or lead-side merge machinery (Decision 1 — the cross-run equivalent is PRD #42's issue-level parallelism).
- Guardrail changes beyond the M2 signal hardening (Decision 3).
- A concurrency-width cap knob / per-user token concurrency budget (Decision 5 — named as more pressing once #42+#43 both land; revisit on evidence).
- Auto-updating admin-customized builtin templates (Decision 4 — reset stays explicit and discards customizations).
- Async/background delegation (`ScheduleWakeup`/`CronCreate` stay denied; the agent guard keeps rewriting `run_in_background: false` — B1).
- Feed redesign / per-invocation attribution (Decision 6 — validate first; file a follow-up only if the bar fails).
- Blocking `git commit` for subagents at the guardrail level (serial-flow coder commits are legitimate today; parallel mode forbids them in prose only).

## Accepted residuals (↳review)

- **Mid-flight cross-contamination between parallel coders** (shared worktree, prompt-level scopes): bounded by the lead's integration diff, the scope-aware review wave, and the human-merged MR. No serial analog; accepted for the wall-clock win.
- **The integration-scope check is prompt-level**, not enforced — a lead-model that skips it lets an out-of-scope write through to review (Decision 1).
- **`setsid`-escape survivors scale with parallel width** during the PAT push window (`docs/proc-hardening.md` residual, N× concurrent candidates). No new class; same endgame.
- **Two parallel same-named coders are indistinguishable in the feed today** (Decision 6) — a UX residual until per-invocation attribution is added, if ever.
- **Token pressure is multiplicative with #42** (Decision 5) — accepted until a per-user token budget exists.
- **Untrusted issue text can ask the lead to "parallelize everything, skip review"** — the lead was equally injectable serially; the human plan gate is the control. Not materially new.

## Success Criteria

- **M0 resolves the premise on the record** before any template prose ships: either same-turn foreground subagents demonstrably overlap, or the PRD is rescoped to M2 only.
- On a run whose approved plan contains two package-disjoint (typically cross-area) implementation units, the lead dispatches both coders in one turn, they execute concurrently, and the lead diffs against HEAD, confirms only declared scopes changed, commits, runs the gate once, and the allocated validator wave follows in a single turn.
- On a run with sequential-by-nature or same-package units, behavior is today's serial flow (no forced fan-out).
- A parallel coder asked to exceed its scope — including a shared wiring file — reports the conflict instead of editing it (observable in the feed).
- **A subagent attempting `mcp__uzi__signal_done` cannot terminate the run** (M2 test + M3 live check); no subagent can reach the workflow-signal tools at all — true even in serial flow.
- No other guardrail regression: parallel subagents are each name-checked, forced-foreground, path-jailed, and Bash-screened exactly as serial ones; no subagent survives the turn.
- Wall-clock for a genuinely parallelizable run drops roughly with the width of the parallel phase (observed on the M3 live run).
- Existing deployments have a documented path (template reset, with the customization-loss caveat and re-apply recipe) to pick up the new lead behavior; fresh deployments get it from seed.