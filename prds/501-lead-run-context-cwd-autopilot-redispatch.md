# PRD #501 — Lead run-context: cwd persistence, autopilot no-human signal, post-review re-dispatch

**Issue**: [#501](https://github.com/vtmocanu/uzi/issues/501)
**Priority**: Medium
**Status**: Draft — ready for implementation

> **Path convention**: every path is relative to the repo root. This bundle spans the **builtin lead template** (`api/internal/agenttmpl/builtins/lead.md`, validated by `task gate:api`) and the **worker prompt builders** (`agent/src/prompt.ts` + a one-boolean thread through `runner.ts`/`executor.ts`/`sdk-executor.ts`/`protocol.ts`, validated by `task gate:agent`). No migration, no SQL, no web, no controller change. Load `.claude/rules/go.md` before the `lead.md`/Go-test work and `.claude/rules/agent.md` before the `agent/src` work.
>
> **This PRD is destined for an offline uzi worker.** Every fact below was verified on 2026-08-21 with its `file:line`. All three changes are prompt/context text plus one boolean already present in the claim — no network, no DB, no live cluster. It touches **no** file under `.github/workflows/**` in implementation or validation (`.claude/rules/prds.md`).
>
> **Builtin template change** — a `lead.md` edit re-applies to pristine builtin rows on the next boot (`ReconcileBuiltinTemplates`/`RefreshPristineBuiltin`), so this PRD **adds a CHANGELOG `[Unreleased]` line**. `lead` is a product-only role: it lives **only** in `builtins/`, never in `.claude/agents/`, so there is no repo-agent or upstream-`roles.yaml` copy to propagate to.

## Problem

Three recurring lead-agent papercuts, all in the lead's prompt/run-context:

- **REC A (seen in 13 runs) — cwd persistence.** The Bash shell cwd **persists across separate tool calls** and the run starts at the worktree root, but the lead is told this only in one bullet **scoped to the integration gate**. So an early `cd api` leaves the shell in `api/`, a later `cd api` errors `cd: api: No such file or directory`, and a grep with a relative `web/src/...` path returns "No such file or directory" until the lead recovers with absolute `$ROOT`-prefixed paths.
- **REC B (seen in 1 run) — autopilot no-human learned too late.** On an autopilot run (no human in the loop) the lead calls `mcp__uzi__ask_user` to resolve an open decision, ends its turn, and only **then** receives a worker status message telling it there is no human and to proceed on best judgment — after which it re-plans unassisted. The no-human condition is known at run start, so the round-trip is avoidable waste that recurs on every autopilot run hitting an open decision.
- **REC C (seen in 2 runs) — post-review lead edit skips re-dispatch.** After a reviewer approves a commit, the lead sometimes edits the production component and the test itself, commits, and self-runs only the gate — with **no** re-dispatch to the read-only validators over the new commit range. The gate is not a substitute for the review lane; the pattern is a coverage gap on the final committed range.

## Solution

- **A**: broaden/relocate the **existing** cwd-persistence guidance in `lead.md` from a gate-only bullet into a general operating note (start-at-worktree-root, cwd persists across Bash calls, use absolute paths or re-`cd` from root).
- **B**: thread the **existing** `auto_approve` claim flag (`protocol.ts:552`) into the plan-prompt context — it is currently read only inside `runner.execute()` (three sites) and never reaches `RunContext`/prompt-build time — and render a conditional planning note telling the lead there is no human, so it resolves open decisions on best judgment and records the assumption instead of calling `ask_user`.
- **C**: add one `lead.md` sentence requiring validator coverage to follow the **final committed range**: any commit that lands after a clean review — including the lead's own integration edits — re-opens a read-only wave over that new `<base>..<sha>` before `signal_done`.

---

## Background — current state (resolved facts)

### Where the lead's prompt/context is assembled

Two layers feed the lead:
1. `api/internal/agenttmpl/builtins/lead.md` — the builtin `lead` template body (`go:embed`-shipped, DB-seeded via `api/internal/agenttmpl/builtins.go:19`); prepended by `buildLeadSystemPrompt(templateBody, …)` at `agent/src/prompt.ts:192-214` (`parts.unshift(body)`, `:198`). **REC A and REC C guidance lives here.**
2. `agent/src/prompt.ts` — the per-turn builders (`buildPlanPrompt`, `buildImplementPrompt`) and `LEAD_GUARDRAIL_APPEND` (`:38-95`). **REC B's note goes here.**

The SDK cwd IS the worktree — `sdk-executor.ts:726` `cwd: ctx.worktreePath` — but that path string is **never rendered into prompt prose** (`worktreePath`, `executor.ts:76`, is used only for hooks/reads at `sdk-executor.ts:551,645,726,951,1677`).

### REC A — the guidance already exists, scoped too narrowly

`lead.md:100-106` (inside the parallel-dispatch/integration-gate bullet) already says: *"…do not rely on the shell's working directory carrying between separate Bash calls, or on the default being the worktree root: a bare `cd api && …` can fail on a later call with `cd: api: No such file or directory`. Use absolute paths, or `cd` from the worktree root fresh in each command…"*. Two gaps vs. REC A: it is **scoped to "When you run that gate"** (not the general grep/relative-path case), and the **starting cwd (worktree-root absolute path) is not surfaced in prose** (the SDK `claude_code` preset may inject its own "Working directory: <path>" line, so the path may already reach the model — the uzi-authored, missing piece is the general persistence statement, not necessarily the literal path). **Scope REC A as "broaden/relocate existing guidance," not net-new.**

### REC B — the flag exists but is not at prompt-build time (confirmed gap)

- Flag: `agent/src/protocol.ts:552` `auto_approve?: boolean` on `ClaimResponse` ("Autopilot run (PRD #19)… Re-delivered on every resume/requeue").
- It is read in `runner.execute()` at **three sites** — a top-level `ciFixHumanApproved` (`runner.ts:776`), the `gatePlan` closure (`runner.ts:858`, `effectiveAutoApprove = (claim.auto_approve ?? false) && !forceGate`), and the `askUser` closure (`runner.ts:887`) — but **never on `RunContext`**, so it never reaches prompt-build time.
- The lead learns "no human" too late: `runner.askUser` (`runner.ts:2439-2467`) short-circuits on autopilot and returns a sentinel **as the answer to an already-issued `ask_user` call** (`AUTOPILOT_SENTINEL_ANSWER`/`AUTOPILOT_ANSWER_NOTICE`, `runner.ts:2595-2599`).
- Why it is not available at prompt-build time: `Executor.run(ctx: RunContext)` (`executor.ts:328`) receives only `RunContext`, which has **no** `autoApprove`/`humanInLoop` field (`executor.ts:57-252`); `buildPlanPrompt` is called from `sdk-executor.ts:1049-1067` purely from `ctx.*`; `PlanPromptInput` (`prompt.ts:679-707`) has no autopilot field. The existing pre-plan clarification clause the note sits beside is `prompt.ts:747-750` ("If a BLOCKING ambiguity… call `ask_user` BEFORE planning…").

### REC C — dispatch discipline exists, the lead's own post-approval edit is not explicit

`lead.md` already has per-unit review over an immutable range (`:84-89`), "a subagent reporting 'it's green' is not that check" (`:95-100`), iterate-until-clean (`:122-124`), and verifier reservation (`:133-138`). **Not stated**: when the **lead itself** edits code/tests after a clean review and commits, that new committed range must go back through the read-only validators before `signal_done`. `buildImplementPrompt`'s turn text is silent too (`prompt.ts:878-882` continue-text; `:938-942` done-clause).

### Test guards that will bite

- **No snapshot framework.** Agent tests run under `node --test` via `tsx`; assertions are `assert.match` + targeted `assert.strictEqual`. `agent/test/prompt.test.ts` composition pins at `:763-770` reference the **constants** (`LEAD_GUARDRAIL_APPEND`, …), so editing a constant's text updates both sides automatically, but a **new unconditional `parts.push(...)`** in `buildLeadSystemPrompt` reddens them — REC B's note must be a **conditional** in `buildPlanPrompt`, not an unconditional append. The additive-absent baseline pairs (`:270-288`) stay green as long as a new field left absent keeps two calls equal.
- **`lead.md` edits are guarded by Go tests**: `api/internal/agenttmpl/render_test.go` — **region-scoped phrase pins** split at `leadRegionBoundary = "Dispatch independent subagents in parallel in a single turn:"` (`:251`), with landmarks `leadPlanLandmark`/`leadBulletLandmark` (`:270-271`) that must stay on their sides; the existing cwd sentence (`lead.md:100-106`) is in the **bullet** region and is not itself pinned, so it can be reworded **within that region**. `api/internal/agenttmpl/recipient_test.go` — 5 rules banning unreachable addressees; **REC A/C wording must not name a SendMessage recipient** other than `` `main` `` (phrase REC C as "dispatch the read-only validators" / "re-open the review wave", never "SendMessage to the reviewer/team lead").

### Gates

`task gate:agent` (`Taskfile.yml:398-414`): deps-check → lint (oxlint, unratcheted) → deadcode (knip) → typecheck → test (`node --import tsx --test --test-timeout=120000`). `lead.md` + its Go tests run under `task gate:api` (`go test ./internal/agenttmpl`). A change touching both needs both gates. `node --test` caveat (`.claude/rules/agent.md`): read the exit code + named failing tests, not the `fail N` tally.

---

## Design decisions

1. **REC A is a rewording within the bullet region, plus an optional standalone note.** Lift the persistence/absolute-path guidance into a general operating rule (candidate home: near the "operational notes" at `lead.md:157-168`, or broadened in place at `:100-106`), stating the cwd persists across tool calls, the run starts at the worktree root, and paths should be absolute or re-`cd`'d from root. Keep every edit **inside the correct render region** (Background) so `render_test.go` region pins hold. **If placed at the operational-notes paragraph, note it opens with the literal count "Two operational notes."** — adding a third makes that count stale (a silent stale count is exactly the drift the lead body itself warns against), so either update it to "Three" or keep the new note in the bullet region instead. No `prompt.ts` change is required; if the literal worktree path in prose is wanted, thread `ctx.worktreePath` into a note — but the persistence statement is the load-bearing part.
2. **REC B threads one boolean end-to-end and renders a conditional note in ALL THREE plan builders.** (i) `runner.ts:778` ctx literal: add `autoApprove: claim.auto_approve ?? false,`. (ii) `executor.ts` `RunContext` (~`:87`): add `autoApprove?: boolean`. (iii) render a **conditional** no-human note — when `autoApprove`, tell the lead there is no human in the loop, so it must resolve open decisions on best judgment and record the assumption in the plan rather than calling `ask_user`; reuse/point at the frozen sentinel wording (`runner.ts:2595-2599`) for consistency. (iv) **`sdk-executor.ts` selects one of THREE kind-specific plan builders, and `auto_approve` is a per-run fact independent of kind, so all three must carry the note**: `buildCIFixPlanPrompt` (`sdk-executor.ts:1005`, `isCIFix` — ci_fix runs explicitly carry `auto_approve` per `runner.ts:769-776`/`:858`), `buildSelfImprovePlanPrompt` (`:1031`, `isSelfImprove` — unattended by construction), and `buildPlanPrompt` (`:1049`, else/issue — the case the rec observed). Add the `autoApprove` field to each builder's input type (`PlanPromptInput` `~:707` for the issue path, and the CI-fix / self-improve plan-input types), render the conditional note in each adjacent to its own pre-plan clause, and pass `autoApprove: ctx.autoApprove` at all three call sites. `runner.askUser` short-circuits to the sentinel for **any** kind (`runner.ts:2449`), so threading only `buildPlanPrompt` would leave ci_fix/self_improve autopilot runs still wasting the `ask_user` round-trip. Conditional (not unconditional) so the additive-absent test pairs stay green.
3. **REC C is one `lead.md` sentence about the final committed range.** Any commit that lands after a clean review — including the lead's own integration edits — re-opens a read-only wave over that new `<base>..<sha>` before `signal_done`; self-running the gate is not a substitute (reinforces `lead.md:95-100`). Place it in the bullet region near `:84-96` (respect region pins) or as a short clause at `buildImplementPrompt`'s done-instruction (`prompt.ts:938-942`). Wording must avoid the `recipient_test.go` addressee bans.

## Scope

**In scope**:
- `api/internal/agenttmpl/builtins/lead.md`: REC A broaden/relocate; REC C sentence.
- `agent/src/prompt.ts`: REC B conditional no-human note + an `autoApprove` field in **all three** plan builders' input types and bodies (`buildPlanPrompt`/`PlanPromptInput`, `buildCIFixPlanPrompt`, `buildSelfImprovePlanPrompt`); (REC C optional done-clause clause).
- `agent/src/runner.ts` (ctx literal), `executor.ts` (`RunContext` field), `sdk-executor.ts` (the **three** plan-builder call sites at `:1005`/`:1031`/`:1049`), `protocol.ts` (if a type is needed): thread `autoApprove` for REC B.
- Tests: `agent/test/prompt.test.ts` (REC B: assert the note appears when `autoApprove: true`, absent otherwise; REC A/C match assertions); `api/internal/agenttmpl/render_test.go`/`recipient_test.go` stay green (adjust pinned-phrase expectations only if a pinned phrase is reworded — prefer not to touch pinned phrases).
- CHANGELOG `[Unreleased]` entry (builtin change).

**Out of scope**:
- Surfacing the no-human condition anywhere other than the **planning** pass (the rec is about planning).
- Rewriting the SDK preset's own "Working directory" injection (out of uzi's control; REC A adds the persistence statement uzi owns).
- Any repo-agent / `roles.yaml` propagation (`lead` is builtins-only).
- Web/TUI/controller changes.

## Milestones

- [ ] **M1 — REC A: broaden cwd-persistence guidance (`lead.md`).** Reword/relocate `lead.md:100-106` into a general operating note per Decision 1, staying inside the render region so `render_test.go` pins hold. **Validate**: `task gate:api` (`go test ./internal/agenttmpl` green — region/phrase pins and `recipient_test.go` unaffected); read the rendered lead body to confirm the note reads as general, not gate-scoped.
- [ ] **M2 — REC B: thread `autoApprove` and render the no-human note in all three plan builders (`agent/src`).** Implement Decision 2 (i)-(iv) across `buildPlanPrompt`, `buildCIFixPlanPrompt`, and `buildSelfImprovePlanPrompt`. Add `agent/test/prompt.test.ts` cases **for each of the three builders**: with `autoApprove: true` the built prompt contains the no-human/record-the-assumption note; with it absent/false the prompt is byte-identical (additive-absent pair stays equal). **This additive-absent pair is the actual guard for SC2's non-autopilot invariant** — the composition pins at `:763-770` assert `buildLeadSystemPrompt(...).append`, a *different* function from the plan builders where REC B's note lives, and no existing test pins full plan-builder output, so an unconditional note would slip past every existing test; the M2 pair is what catches it. (Do NOT add an unconditional `parts.push` to `buildLeadSystemPrompt` — that WOULD redden the `:763-770` pins.) Prove each new assertion non-vacuous by a call-site mutation (`.claude/agent-team.md`). **Validate**: `task gate:agent` (deps-check + lint + deadcode + typecheck + test); single file `cd agent && node --import tsx --test --test-timeout=120000 test/prompt.test.ts` — read the exit code + named tests, not the tally.
- [ ] **M3 — REC C: final-committed-range re-dispatch sentence (`lead.md`).** Add the Decision 3 sentence in the correct region, phrased to avoid `recipient_test.go` bans (no "SendMessage to <reviewer/team lead>"; use "dispatch the read-only validators"/"re-open the review wave"). **Validate**: `task gate:api` green (`render_test.go` region pins + `recipient_test.go` rules pass); confirm the rendered body carries the sentence.
- [ ] **M4 — CHANGELOG.** Add an `[Unreleased]` entry noting the lead now (A) is told its cwd persists and to use absolute paths, (B) is told up front on autopilot runs to decide open questions on best judgment, and (C) re-runs the review lane over any post-review commit. **Validate**: `web/scripts/check-docs.mjs` (via `npm run build`) passes if it touches docs; otherwise the CHANGELOG format matches existing `[Unreleased]` entries.

## Success criteria

1. The rendered lead body states, as a general operating rule (not gate-scoped), that the run starts at the worktree root, the Bash cwd persists across tool calls, and paths must be absolute or re-`cd`'d from root.
2. On an autopilot run (`auto_approve` true) of **any kind — issue, ci_fix, or self_improve** — the plan prompt (from whichever of the three kind-selected builders runs) contains a note telling the lead there is no human and to resolve open decisions on best judgment and record the assumption, rather than calling `ask_user`; on a non-autopilot run every plan prompt is byte-identical to today.
3. The rendered lead body states that any commit landing after a clean review — including the lead's own edits — re-opens a read-only validator wave over the new range before `signal_done`.
4. `task gate:api` and `task gate:agent` both pass; `render_test.go` region/phrase pins and `recipient_test.go` addressee rules stay green; the `prompt.test.ts` composition pins stay green; new behavior is covered by non-vacuous tests proven via call-site mutation.
5. A CHANGELOG `[Unreleased]` entry is added (builtin template change).
6. No migration/SQL/web/controller change; no `.github/workflows/**` touched; `main` is never touched; delivered on a branch + PR.

## Risks & mitigations

- **REC B rendered as an unconditional `parts.push`** breaks the `prompt.test.ts` composition pins. Mitigation: Decision 2 makes it a **conditional** inside `buildPlanPrompt`; M2 asserts the non-autopilot prompt is unchanged.
- **`lead.md` edit trips `render_test.go` region pins** (landmark on the wrong side of `leadRegionBoundary`). Mitigation: keep every edit inside its render region (Background); run `go test ./internal/agenttmpl` per milestone.
- **REC A/C wording trips `recipient_test.go`** (names a SendMessage recipient / "team lead"). Mitigation: Decision 3 phrasing constraint; validated by the test.
- **REC A treated as net-new** and duplicated. Mitigation: Decision 1 — broaden the existing `:100-106` guidance, don't add a second copy.
- **Vacuous prompt test.** Mitigation: M2/M3 prove each assertion by a call-site fold.
- **Forgetting the CHANGELOG** on a builtin change (it re-applies to pristine rows on boot). Mitigation: M4 is an explicit milestone.

## Dependencies

- **No external / internet dependency.** The autopilot flag, worktree cwd, and lead body are all supplied in the claim/clone; the target tests are pure and offline; no new dependency is added.
- **No shared-file collision** with the other batch PRDs: this owns `lead.md` and the `agent/src` prompt/runner/executor files; the submit_plan PRD (#502) touches only `agent/src/signals.ts`; the api PRDs (#499/#503) touch `api/internal/workersvc`, a different package from `api/internal/agenttmpl`.
- **CHANGELOG `[Unreleased]`** is also touched by #503 (the other user-facing PRD); this is an append-only 2-way merge resolved at land time.

## Decision log

- **2026-08-21**: Bundled three `improve_uzi` recommendations that all touch the lead's prompt/run-context (cwd persistence, autopilot no-human, post-review re-dispatch) into one PRD — they share `lead.md`/`prompt.ts`, so separate PRDs would merge-conflict.
- **2026-08-21**: **REC A corrected to "broaden existing guidance"** — the cwd-persistence sentence already ships at `lead.md:100-106` scoped to the gate; the PRD generalizes it rather than adding a missing note.
- **2026-08-21**: **REC B threads the existing `auto_approve` flag to prompt-build time** (currently trapped in two runner closures) and renders a **conditional** planning note, chosen over a new `buildLeadSystemPrompt` append so the composition pins stay green.
- **2026-08-21**: **REC C phrased to avoid the recipient-ban tests** — "dispatch the read-only validators", never "SendMessage to the reviewer".
- **2026-08-21**: Next step = send to uzi (Auto). PRD authored fully internet-independent and workflow-file-free; CHANGELOG line included because `lead.md` is a builtin.
