# PRD #209 — Seed an externally-authored plan onto a run (plan locally, implement in uzi)

**Issue**: [#209](https://gitlab.example.com/vtmocanu/uzi/-/issues/209) · **Label**: PRD · **Priority**: Medium
**Area**: `agent/src/runner.ts` + `agent/src/sdk-executor.ts` + `agent/src/executor.ts` (the three-way plan state) · `api/internal/handler/workers.go` + `api/internal/workersvc/service.go` + `api/internal/store/queries/runtime.sql` (create-time seeding, the `plan_approved` disjunct, the `SetRunRunning` guard) · **two** migrations on `runs` (M1's `plan_source` + plan/selection columns; M4's planned-against commit) · `web/src/pages/RunView.tsx` (M5 is a new renderer, not a copy tweak) · `api/cmd/uzi/run.go` + `api/internal/uzicli/skill/SKILL.md` (the CLI surface).
**Line references** are against `5ea4d2f8`. They still resolve: the commits since (`31d0d75f`, `8dd67db0`) changed only `api/internal/uzicli/skill/SKILL.md` — cited by name, never by line — and this file. Stated so the next reader can read that rather than re-derive it.
**Status**: not started. **M1 and M2 are the whole feature**; M3-M8 are surface and proof.
**Reviewed** 2026-08-03, two adversarial passes (16 citations re-derived against `5ea4d2f8`, then a second pass over the M8/PRDLESS additions). Thirteen findings; all applied below. The blocking one is folded into D8 — the design as first written had **no path to `plan_approved: true`**, which made D4 row 2 unreachable. Sections carrying a review finding are marked ⟨R⟩.
**Third pass** (repair audit) delivered six further findings, all applied. It found no misstatement in how the earlier findings were transcribed, but it **escalated D8's `awaiting_approval` bullet from a liveness bug to a safety one** — the run does not die at the gate, it gets requeued and re-claimed carrying a worker-authored `plan_md` with `plan_approved` still true. That is the second time this design's central mechanism turned out to be wrong in the direction of "does something bad silently" rather than "does nothing". Worth knowing before implementing M1.

**Standing caveat**: every pass was a static read. Nothing here has been executed — no test, no live-DB sweep, no judge run. D8's requeue chain in particular is reasoned from four guards and has not been reproduced; **M1 should reproduce it before fixing it**, since a fix for an unobserved bug is its own risk.

## Problem

A uzi run cannot be told what to do. It can only be told *where to look*.

`POST /api/repos/{id}/runs` takes an `issue_iid` plus one behavioural toggle
(`WaitOnLimit`, `api/internal/handler/workers.go:677-687`) and nothing that
describes the *work*. The worker claims the run, clones,
runs a Phase-1 planning turn, emits `signal_plan`, and parks at
`awaiting_approval`. Only then does the user get to see a plan, and only then can
they approve it.

For a user who has *already planned the work* — in Claude Code, on the same
repo, watching the plan take shape and arguing with it as it was written — that
sequence is pure latency. They hold a finished plan and uzi makes them wait while
it derives a worse one from the issue description, with no visibility into the
derivation. The planning they can see is the planning that does not count; the
planning that counts is the one they cannot watch.

The gap is not conceptual. uzi already models "this plan is approved, implement
it" as a first-class claim state.

### The seam already exists, and it is one boolean away

The claim payload already carries both halves:

- `plan_md` — `api/internal/workersvc/service.go:1421` (`PlanMd: textPtr(run.PlanMd)`)
- `plan_approved` — `service.go:1437` (`PlanApproved: run.AutoApprove || rc.HumanPlanApproved`)

and both executors already have a path that consumes them, skips the Phase-1
planning turn, skips the approval gate, and goes straight to implement:

- `agent/src/sdk-executor.ts:573-576`
- `agent/src/executor.ts:605` (the stub half)

That path was built for **resume** (PRD #35 Decision 6b): a run that parked after
approval must not re-ask a human to approve the same plan twice. It is exactly the
behaviour this PRD needs, and it refuses to fire on a fresh run for exactly one
reason — both layers AND the flag with a surviving SDK session:

```
agent/src/runner.ts:461      planApproved: (claim.plan_approved ?? false) && !!sessionId
agent/src/sdk-executor.ts:573-576
                             ctx.planApproved === true && !!ctx.sessionId && !!ctx.approvedPlan?.trim()
```

The `&& sessionId` is **correct and must not simply be deleted**. Its stated job
(`runner.ts:454-460`) is to stop the executor skipping planning for a run whose
transcript was dropped — the one case where it *must* plan. A seeded-plan run is a
third case that neither branch models today: approved, no session, and that is
fine, because there was never a session to lose.

## Solution

Let a run be created **with** a plan, and teach the worker that "approved with no
session" is a legitimate state rather than a contradiction.

```
local Claude Code                          uzi
─────────────────                          ───
write PRD, commit, push  ──────────────▶  issue links prds/*.md   (unchanged)
plan against the clone
pick the roster
        │
        └─ uzi run create --repo R --issue N \
               --plan-file plan.md \
               --agent-source repo        ──────▶  runs row: plan_md + plan_source='seeded'
                                                          │
                                                   worker claims, clones,
                                                   SKIPS Phase 1 and the gate,
                                                   implements plan_md
```

The PRD itself keeps travelling through git and the forge issue. Only the **plan**
moves over the API. That split is not a compromise, it is the design (Decision 1).

## Decision log

**D1 — API, not git, as the plan transport.**
Git was the obvious alternative: commit `plan.md`, let a poller notice it. Rejected
on four counts. (a) The plan is a `runs` column, not a repo artifact; git transport
means inventing a path convention and committing planning scratch nobody wants at
`main`. (b) The agent selection is already an API concept — JSON in the
`approve_plan` body (stated at `agent/src/protocol.ts:738-741`, encoded by
`encodeAgentSelection` at `:753-758`) — and git cannot express it
without a sidecar format. (c) The API call is authenticated as a user with a scoped
token; a commit author is not an authz fact. (d) `api/cmd/uzi/` already exists as a
second API consumer of exactly this shape, so the client cost is one flag.

**D2 — Reuse `kind='issue'`. Do not add a run kind.**
A `kind='seeded'` would drag `runs_kind_check` (`00058_run_judge_self_improve_kinds.sql:26-29`)
+ `runs_kind_shape`'s per-kind body (`:36-40`), the claim payload, the judge, the
MR watch and the PRD-link patch pass along with it, for no gain: a seeded-plan run
wants *every* one of those behaviours unchanged. It is an issue run that skipped its
planning turn, which is precisely what a resumed pre-approved run already is.

**D3 — Do NOT express "plan was seeded" by setting `auto_approve`.**
Tempting, because `PlanApproved: run.AutoApprove || rc.HumanPlanApproved`
(`service.go:1437`) makes it a zero-line server change. Rejected: `auto_approve`
carries autopilot semantics beyond the gate — an autopilot run's agent selection
arrives on the **worker's own state report** rather than on a human input
(`protocol.ts:738-742`, Decision 6), so overloading it silently routes the
selection through the wrong path. A seeded run has a real human selection made at
create time. Add a distinct column.

Verified two ways rather than from the comment alone: `runtime.sql:601-602` states
the rule (*"agent_source/agent_exclusions only by an AUTOPILOT run's report (a
human-gated run persists its selection through CreateApprovePlanInput instead)"*),
and the code matches — `runner.ts:1099-1120` is the autopilot branch reporting
`agent_selection` on its `running` state report, while the human path goes
`submitApproval` → `CreateApprovePlanInput` (`service.go:3265-3283`).

**⟨R⟩ One risk this decision must name: the autopilot-only rule is a WORKER
convention, not a SQL one.** `SetRunRunning` COALESCEs `agent_source` against its
own value (`runtime.sql:651`) and will overwrite on any non-null report. A seeded
run takes the pre-approved path and never enters the autopilot branch, so it cannot
clobber its own create-time selection — but that is an invariant M2 should
**assert**, not assume.

**D4 — A third state in the worker, not a relaxed AND.**
`planApproved && sessionId` becomes a three-way discriminator, not a two-way one
with a weaker guard:

| plan_approved | session | seeded | worker does |
|---|---|---|---|
| true | yes | no | resume past approval (today, unchanged) |
| true | no | **yes** | **implement the seeded plan, no gate (new)** |
| true | no | no | **re-plan** (today, unchanged — the case the AND protects) |
| false | any | any | plan as normal (unchanged) |

Row 3 is the whole reason this is not a one-character fix, and any implementation
that cannot show row 3 still re-plans has not done the work.

**D8 ⟨R⟩ — `plan_approved` needs a THIRD disjunct, and the design had none.**

> **⟨M1, 2026-08-04⟩ Reachability correction (audit + live test, both independent).**
> The `awaiting_approval`-trap bullet below names *"a plan D5's scrub reduces to
> whitespace"* as the fall-through trigger. **That trigger is unreachable**, on two
> counts, and neither is a defect in the fix: (a) `secretscrub.Scrub` replaces each
> match with the non-empty marker `[redacted]` and deletes nothing, so it cannot turn
> a non-whitespace plan into a whitespace one; (b) M1's create-time check rejects an
> empty/whitespace plan (`ErrPlanEmpty`) before storage, so a stored `plan_source='seeded'`
> row always carries non-empty `plan_md`. So no path makes a seeded run reach the plan
> gate *as M1 lands*. The `plan_source='agent'` write in `SetRunAwaitingApproval` stays
> — it is **defense in depth** (one column) against any future *worker* path that reaches
> Phase 1, and M1 proves it disarms the chain via `TestSeededRunFallThroughToGateDisarmsLiveDB`
> (with a mutation control). The fix is correct; only the stated *reachability* was wrong.

D4's table asserts a seeded run's claim carries `plan_approved: true`. Nothing
produces it. `service.go:1437` is `PlanApproved: run.AutoApprove || rc.HumanPlanApproved`,
and **both disjuncts are false for a seeded run by construction**: D3 forbids
setting `auto_approve`, and `human_plan_approved` is
`EXISTS(run_user_inputs WHERE kind='approve_plan' AND consumed_at IS NOT NULL)`
(`api/internal/store/queries/runtime.sql:540-543`) — which M1's own validation
criterion demands be empty. Implement M1 as first written and the claim ships
`plan_approved: false`, making row 2 unreachable and the feature inert.

So the one load-bearing server change is a third disjunct
(`run.AutoApprove || rc.HumanPlanApproved || run.PlanSource.String == "seeded"`),
and it is not a detail — it is the mechanism. ⟨R⟩ **Note the `.String`**: M1's DDL
below declares `plan_source` nullable, and sqlc types a nullable text column as
`pgtype.Text`, so the bare `== "seeded"` form does not compile. Both precedents sit
in the very struct this edits — nullable `agent_source` is read as
`run.AgentSource.Valid`/`.String` (`api/internal/workersvc/agent_selection.go:263`),
while NOT NULL-with-default `auto_approve` is a plain bool (`service.go:1422`).
**M1 therefore declares the column `NOT NULL DEFAULT 'agent'`**, which the default
makes safe to backfill, and this disjunct reads a plain string. Two consequences
that must land with it:

- **It extends the 🔴 invariant at `runtime.sql:511-530`.** That block's soundness
  argument is "a revise round sits at `awaiting_approval`, which `SetRunRunning`
  refuses to leave without a consumed `approve_plan`". A seeded run is a fourth
  case that argument does not cover and needs its own clause written there.
- **🔴 A seeded run that falls through to the gate comes back ARMED. This is a
  safety bug, not the liveness bug it was first written as.** ⟨R⟩ The chain, every
  link re-derived:
  1. `plan_source='seeded'` ⇒ `plan_approved: true` via the third disjunct.
  2. `sdk-executor.ts:576` requires `!!ctx.approvedPlan?.trim()`, so a plan D5's
     scrub reduces to whitespace makes `preApproved` false ⇒ the run **plans and
     gates**.
  3. `SetRunAwaitingApproval` writes `plan_md = @plan_md` (`runtime.sql:709`) — the
     worker's **own** Phase-1 plan. Its WHERE (`:746-747`) is
     `id`/`worker_id`/`status NOT IN (terminal)`, with **no `plan_source` guard**.
     The row now holds an unreviewed plan while `plan_source` is still `'seeded'`.
  4. The worker's heartbeat goes stale ⇒ `RequeueRunsOfStaleWorkers`
     (`runtime.sql:1154-1164`), whose source set at `:1159` includes
     `awaiting_approval` ⇒ back to `queued`. It is a **direct UPDATE**, so
     `SetRunRunning`'s consumed-`approve_plan` guard never executes on this path.
  5. Re-claim: `plan_approved` still true, `plan_md` now worker-authored and
     unreviewed, session possibly gone ⇒ **D4 row 2 ⇒ implement a plan no human
     ever saw, with no gate.**

  That is precisely what `runtime.sql:511-530` exists to prevent (*"it would tell
  the worker to skip Phase 1 and IMPLEMENT AN UNREVIEWED plan_md — the same
  residual with a materially worse blast radius"*), reached by a path its argument
  does not contemplate. **And the reason it cannot protect is sharper than "needs
  its own clause": the invariant reads "NO awaiting_approval REPORT REWRITES
  plan_md AFTER THE CONSUMED approve_plan THAT MADE human_plan_approved TRUE"
  (`:527-528`) — and a seeded run has no consumed `approve_plan`, so that sentence
  is VACUOUS. It cannot be violated, which is exactly why it cannot hold.** The
  root cause is that the third disjunct decouples `plan_approved` from `plan_md`'s
  provenance: `plan_source` describes the row's **birth**, while `plan_md` stays
  **mutable**.

  **Fix, and it is one column in an existing statement**: have
  `SetRunAwaitingApproval` set `plan_source = 'agent'` in the same UPDATE that
  rewrites `plan_md`, so the disjunct tracks provenance rather than birth. The
  create-time 422 on a scrub-to-empty plan stays — but it closes only the blank-plan
  *entry* path, and any other fall-through to the gate reopens the hole without
  this. **Both, not either.**

Also for M1: `CreateRun`'s INSERT lists its columns explicitly
(`runtime.sql:302-303`), and its own 🔴 comment at `:295-301` warns that an omitted
field is silently zero-valued by the sqlc params struct. `plan_md`, `plan_source`,
`agent_source` and `agent_exclusions` all need adding there, and nothing in the
compiler catches a miss — it is a per-path test or it is undetected.

**D5 — A seeded plan is untrusted input and is treated as one.**
Today `plan_md` is produced *inside* the worker, under `settingSources: []`, the
`PreToolUse` deny-hook, and worker-held PAT isolation. An externally supplied plan
becomes agent instructions from outside that boundary. None of the four guardrail
layers weaken (the plan cannot grant a tool, push to `main`, or read a credential),
so this is hygiene rather than a hole — but it gets the same treatment the `ci_fix`
snapshot gets: a size cap at create time (mirroring `ErrDescriptionTooLarge`,
`api/internal/workersvc/service.go:72`) and a secret scrub before storage.
The `invalid selection → force own` fallback (`protocol.ts:839-843`) is untouched:
a malformed body must still never resolve toward repo-authored agents.

**D6 — Repo agents stay the default; `--agent-source` stays optional.**
Already true on all three surfaces and not a change this PRD makes:
`resolveAgentSelection` returns `repo` for an absent selection whenever one was
detected (`protocol.ts:844-848`), `--agent-source` defaults to empty
(`api/cmd/uzi/run.go:284`), and the web picker defaults to the repo card
(`web/src/components/AgentPicker.tsx:47`). So `uzi run create --plan-file p.md`
with no roster flag does the right thing, and the flag exists for the override.
*(The bundled skill described this backwards until `5ea4d2f8`; fixed in the same
change that opened this PRD.)*

**D7 ⟨R⟩ — The plan text must be self-sufficient, and this is NOT the proven resume shape.**
Phase 1 today does more than emit text: it warms the SDK session the implement
phase continues from. A seeded run starts **cold**, with `plan_md` as its only
instructions. A plan written conversationally ("as we discussed", "the file we
looked at") is useless to it. Docs carry this as a stated constraint; M4 adds the
mechanical half.

*This decision first claimed "this is the proven resume shape, not a new risk".
That was wrong, and the difference is load-bearing: **the proven resume shape has a
session**, and row 2 is defined by not having one.* Concretely, `ctx.priorWork` is
read at exactly three sites — `agent/src/sdk-executor.ts:687`, `:705`, `:722` —
all three inside the planning branch, feeding the plan-prompt builders.
`ImplementPromptInput` (`agent/src/prompt.ts:576-601`) **has no `priorWork` field**,
so `buildImplementPrompt` cannot receive one.

Today that is safe by construction, because the pre-approved path requires
`!!ctx.sessionId` and the session carries the history. **Row 2 deletes exactly that
guard**, and the resulting hole is on an ordinary path, not a corner:
`RequeueRunsOfStaleWorkers` (`runtime.sql:1154-1164`) returns a `running` run to
`queued` on a stale worker heartbeat. A requeued seeded run whose transcript did
not survive re-enters implement **cold, on a branch that already carries pushed
commits, with no prior-work note** — while row 3, the case it replaces, re-plans
and does get `priorWorkNote` (`prompt.ts:528`). M2 either carries `priorWork` into
the implement prompt or states the limitation out loud; it may not leave it
implied.

## Milestones

- [x] **M1 ⟨R⟩ — Seeded plan reaches the worker.** *(Done 2026-08-04. Migration `00095_run_plan_source.sql`; the third disjunct + the `SetRunAwaitingApproval` provenance write landed as designed; `ClaimPayload` carries `plan_source` for M2. Commits `9ec8c8b1` (transport) → `83168e21` (tests + regenerated claim-wire goldens) → `23e657a5` (scrub-rationale doc fix). `task gate:api` green; the live-DB gate `TestSeededRunClaimCarriesApprovedPlanNoInputRowLiveDB` and the D8 reproduction `TestSeededRunFallThroughToGateDisarmsLiveDB` both PASS with positive + mutation controls. Reviewer + auditor clean. **M2 handoff:** add `plan_source` to `ClaimResponse` in `agent/src/protocol.ts` + a TS parse assertion — the regenerated goldens already carry the key.)* Migration adds `plan_source text NOT NULL DEFAULT 'agent' CHECK (plan_source IN ('agent','seeded'))` to `runs` — **`NOT NULL` deliberately** (D8: it keeps the disjunct a plain string compare rather than a `pgtype.Text` unwrap, and the `DEFAULT` makes the backfill safe) — plus the `plan_source = 'agent'` write in `SetRunAwaitingApproval` that D8's safety fix requires; `POST /api/repos/{id}/runs` accepts optional `plan_md` + `agent_selection`, size-capped and scrubbed (D5), persisted with the selection through the same columns the human gate writes, and **columns added explicitly to `CreateRun`'s INSERT** (`runtime.sql:302-303`, per its own 🔴 warning at `:295-301`). A scrub-to-empty plan is a **422 at create time**, never a stored blank (D8). The `plan_approved` third disjunct lands here (D8) — it is the mechanism, not a detail. **Validated by**: a live-DB test showing the claim for a seeded run carries `plan_md`, `plan_approved: true` and the selection, with no `approve_plan` input row anywhere. That assertion is the one that would have caught the D8 hole, so it is the milestone's real gate.
- [x] **M2 — The worker implements it.** *(Done 2026-08-04. Commits `57b1a858` (three-way D4 discriminator in runner.ts + both executors; `plan_source` on `ClaimResponse`; Decision A seeded-aware opening; Decision B priorWork; feed line; stale-doc fix) → `f4c7233c` (the plan-BODY delivery gap the validation wave found, plus the doc/test hardening) → `27db544b` (the `embedSeededPlan` predicate extraction pinning the defense-in-depth `seeded` clause). `task gate:agent` green (1096 pass). Validation: reviewer confirmed the D4 boolean algebra correct (row 3 provably cannot route to row 2); auditor confirmed guardrails intact + the plan injection SAFE (authoritative `<plan>` framing correct because the block is authoritative and the human checkpoint shifts plan-gate→MR-review, still non-main); tester mutation-proved every new test non-vacuous (the row-3 regression, the plan-body delivery, the absent-`plan_source` fail-safe, the D3 clobber guard, the `seeded`-clause predicate). The plan-body-delivery gap — the seeded plan text never reaching the model on a session-less run — was the wave's key catch; it passed every green gate because the stub executor feeds no real model. **M3/M4/M5 handoff:** the worker now fully implements a seeded run end-to-end through the stub path.)*

  > **⟨M2 validation, 2026-08-04⟩ SCOPE GAP the checklist missed — the plan BODY must reach the implement turn.** The audit's data-flow trace found that `ctx.approvedPlan` (= `claim.plan_md`) is read only by the skip-gate condition and the ci_fix check — it is **never** passed to `buildImplementPrompt`, which has no plan field. The PRD #35 RESUME path (D4 row 1) works because the plan sits in the resumed SDK session; a SEEDED run (row 2) has `resumeId=undefined`, so the first implement turn is fresh and prompt-only and **the model never sees the user's plan** — it falls back to the issue and silently ignores the seeded plan. That defeats M2's stated purpose ("the worker implements it"). The stub executor (M6's harness) never feeds a real model, so no test catches it. **In scope for M2**: inject the seeded plan body into the implement prompt on the session-less seeded path (first turn only; the resume path stays byte-identical), delimited as the plan to implement — it IS agent instructions per D5, with the deny-hook as the backstop (not fenced as "guidance only" like a follow-up). Add a test asserting the plan text reaches the seeded implement turn — the assertion everyone missed.

  The three-way discriminator of D4 lands in `runner.ts` and both executors; a feed line records that the plan was supplied externally, so it is visible in the transcript rather than inferable. Also in scope, all three from the review: the `priorWork` decision of D7; the first implement turn currently opens *"Your plan was approved"* (`agent/src/prompt.ts:612`) when nobody approved a seeded plan, which needs a decision even if the answer is "leave it"; and `agent/src/executor.ts:592-596` **goes stale the moment this lands** — it asserts "a run whose transcript was dropped never reaches this branch", which row 2 falsifies. Per the repo's fix-the-doc rule that correction ships in the same commit. **Validated by**: all four rows of D4's table exercised as tests, row 3 (approved, no session, not seeded ⇒ re-plans) as a named regression test. The harness already exists — `agent/test/sdk-executor.test.ts:2016-2030` is a table-driven "each condition ALONE must not skip" loop whose first row is literally the no-session case; adding a `seeded` axis extends it directly. Stub side: `agent/test/executor.test.ts:295/:328/:346`. **Gap to close**: those are all executor-level, and `runner.ts:461` needs its own row-3 test (hook: `agent/test/runner.test.ts:2155-2242`, `plantTranscript`).
- [ ] **M3 — CLI surface.** `uzi run create --plan-file <path>` (`-` for stdin), plus `--agent-source` / `--exclude-agents` reusing `approveSelection`'s existing validation (`api/cmd/uzi/run.go:461-473`). Exit codes per the documented contract; `--exclude-agents` without `--agent-source` stays a usage error. Bundled `SKILL.md` updated, drift test green.
- [ ] **M4 ⟨R⟩ — Staleness guard.** The client sends the commit it planned against; the worker compares it to the clone's resolved base (`runnerClone.baseCommit`, `runner.ts:439` — the field exists and is already forwarded into `RunContext`) and surfaces a divergence. **This needs its own migration and its own claim field**: there is no column, no claim field and no request field for a *client-supplied* commit anywhere in the tree today. **Open question 3** decides refuse-vs-warn.
- [ ] **M5 ⟨R⟩ — Web surface.** Bigger than it reads. `<PlanPanel>` renders only under `run.status === "awaiting_approval"` (`web/src/pages/RunView.tsx:639`) and `run.plan_md` is read only inside it (`:982-984`) — so a seeded run's plan body is currently **unreachable** on the run page. This is a new renderer, not a copy tweak. Second, smaller item in the same milestone: `AgentRosterSummary` is gated `run.status !== "awaiting_approval" && run.agent_source` (`:680`), and a seeded run has `agent_source` from creation, so for `source: "repo"` the card appears on first load saying *"This run used the repository's own agents"* over an **empty chip list** until the post-checkout report lands `repo_agents` (`:1139`). The board start button is unchanged (no start-run modal — the user ruled against one on 2026-07-27, `handler/workers.go:683-685`).
- [ ] **M6 ⟨R⟩ — Proof, end to end.** e2e coverage through the stub executor (which already honours the pre-approved path, `executor.ts:605`) for a full seeded run: create → claim → implement → MR, never passing `awaiting_approval`. Claim contract has a home already: `agent/test/claim-skills-contract.test.ts:67` asserts `claim.plan_approved === true`. **The judge criterion is stated concretely, because "without degrading" has no predicate**: `judge-runner.ts:352` does deliver `t.plan_md`, so the seeded plan reaches the judge; but `trace.inputs` (`:357`) is **empty** for a seeded run (no `approve_plan`), so the steering log vanishes, and `sampleMessages` (`:392-393`) claims its head/tail keeps "the plan gate and the delivery" when the head now holds no gate. Assert: the verdict schema parses, and no recommendation cites a missing plan.
- [ ] **M7 — Docs.** `docs/cli.md`, `ARCHITECTURE.md` (Agent runtime), and the D7 constraint written where a plan author will read it before writing one.
- [ ] **M8 — The skill teaches plan AUTHORING, not just the flag.** M3 covers the reference half (a new flag gets a line). This is the other half, and it is the one that decides whether the feature works in practice: the bundled `SKILL.md` gains an authoring section covering what makes a plan self-sufficient and why (D7), how to read the run's roster from the clone's `.claude/agents/`, where the base commit comes from (M4), and the exact `uzi run create --plan-file` invocation — including the `PRDLESS` path below. **Validated by**: a Claude Code session holding only the installed skill can take a written PRD and reach an implementing run with no further instruction.

### Why the bundled skill is the right home for M8

The install path is `~/.claude/skills/uzi-cli/SKILL.md` — verified from the
installer rather than from the skill's own prose: `api/internal/uzicli/skill.go:30`
(`skillDirParts = {".claude", "skills", "uzi-cli"}`), `:33` (`skillFileName`),
`:64-67` (`dir()`/`skillPath()`), `:49` (`os.UserHomeDir()`).

**⟨R⟩ The distribution mechanism is stronger than "a command users already run",
which is what this section first claimed.** The CLI installs the skill
best-effort on **every** command, via `root.PersistentPreRun`
(`api/cmd/uzi/root.go:109-113`, skipped only under the `uzi skill` verbs so they do
not race their own report), and staleness keys on **content hash, not version**
(`uzicli/skill.go:74-80`: *"a version bump with an unchanged skill must not
rewrite"*). So an M8 edit propagates on the user's next `uzi` invocation of any
kind, and nobody has to remember `skill install`. That removes the failure mode
rather than relying on a habit.

**Assumption, not verified here**: that a local Claude Code session actually reads
personal skills from that path. It is true, but it is a claim about Claude Code's
discovery, and nothing in this repo proves it.

### Issues with no PRD file are already supported and compose with this

The PRDLESS bypass (PRD #22) is on by default — `DefaultPrdlessEnabled = "true"`,
`DefaultPrdlessLabel = "PRDLESS"` (`api/internal/settings/settings.go:106-107`) —
and exempts an issue from the PRD-**link** gate at
`api/internal/workersvc/service.go:2885-2886`. A seeded plan is what a PRD file
would have supplied anyway, so `PRDLESS` + `--plan-file` is a primary use of this
feature rather than an edge case, and needs no code beyond M1-M3. It is reachable
from the CLI with **no client change**: `allowWithoutPRD` is computed server-side
from a fresh forge read (`api/internal/handler/workers.go:718-720`), on the same
`POST /api/repos/{id}/runs` the web uses.

**⟨R⟩ It takes BOTH labels, and this section said otherwise.** `PRDLESS` does not
mean "no PRD label". A separate gate runs **first** —
`if !isPRDIssue(issue.Labels, s.prdLabel(ctx))` (`service.go:2878`) — and its own
comment (`:2868-2871`) is explicit: *"PRDLESS does NOT bypass this. It is the
escape hatch for a PRD issue with no `prds/*.md` file yet … it was never a claim
about issues that are not uzi's."* So the requirement is `PRD` **and** `PRDLESS`.
Read as first written, this section sends a user straight into the 422 at
`workers.go:738-740` (*"this issue does not carry the PRD label; promote it before
starting a run"*) on its own headline case.

**⟨R⟩ And the two gates read labels from DIFFERENT sources, which is a timing
trap.** `allowWithoutPRD` uses the **fresh** forge read, deliberately, so a
just-added `PRDLESS` works immediately (`workers.go:712-713`). `isPRDIssue` uses the
**cached** labels (`service.go:2873-2877`, Decision 12), and `GetIssueByIID`
(`:2856`) 404s outright if the issue is not cached at all. So adding `PRD` does
**not** take effect until the poller syncs — unless the user goes through Promote,
which writes the label forge-first and updates the cache row in the same request.
Label a fresh issue `PRD`+`PRDLESS` and immediately run `uzi run create
--plan-file`, and you get `ErrNotPRDIssue`. That directly undercuts success
criterion 1 for the case this section calls primary, so **M8's authoring guidance
must carry the promote-or-wait sentence**, not just the label pair.

**What does not go away: you still need an ISSUE.** `runs_kind_shape` pins
`issue_iid NOT NULL` for `kind='issue'`
(`00058_run_judge_self_improve_kinds.sql`), and the MR, the `Closes #N` and the
board card all hang off it. "No PRD" is supported; "no issue" is not, and this PRD
does not propose changing that.

## Success criteria

1. A user with a written plan reaches "worker is implementing" in **one command**, with no approval gate and no planning turn.
2. A run created with no `--plan-file` behaves **byte-identically** to today. This is the anti-regression criterion and it outranks every other item here.
3. Row 3 of D4's table still re-plans. A session dropped mid-flight is not a seeded plan and must never be treated as one.
4. The four guardrail layers are unchanged, and the MR still comes from a branch that is not `main`.
5. `uzi run create --plan-file p.md` with no roster flag runs the repo's own agents. ⟨R⟩ **Verified reachable end to end**: no flag ⇒ `agent_source` stays NULL ⇒ `persistedSelection` returns nil (`agent_selection.go:262-265`) ⇒ the claim carries nothing ⇒ the executor sees `{status:"absent"}` (`sdk-executor.ts:630-632`) ⇒ `resolveAgentSelection` gives repo-when-detected (`protocol.ts:844-847`). This criterion needs no new code, which is why it is worth stating: it is the one place the feature composes with an existing default for free.

## What the review verified clean

Recorded so a later reader can tell a checked claim from an inherited one. All 16
cited spans were re-derived against `5ea4d2f8` by opening each file; none was wrong
or stale. Beyond the citations:

- **D2's "MR watch and PRD-link patch pass come along unchanged"** — both
  (`api/internal/forgesvc/mr_watch.go:29`, `prd_link_patch.go:35`) key on run
  completion plus MR state and are indifferent to how the plan was produced.
- **No `plan_source` identifier exists anywhere in the tree** — the proposed column
  name collides with nothing.
- **The sweeper treats a seeded run like any other issue run.** `SweepRunningTimeout`
  (`runtime.sql:1132`) matches on `status='running' AND started_at < cutoff AND kind <> 'chat'`,
  and `started_at` is stamped on first entry to `running` either way — so a seeded
  run gets strictly *more* effective implement budget, having spent no wall clock
  parked at a gate. A benefit, and one to keep in mind when reading run timings.
- **The three registered sweeper passes** (`api/cmd/server/main.go:416-450`) touch no
  run status.

**Not verified, stated as such**: the review was a static read — nothing was
executed, no test run, no live-DB sweep, no judge run over a planless trace. Every
"confirmed" above means "read in the source at `5ea4d2f8`", never "observed at
runtime". D8's `awaiting_approval` trap in particular is reasoned from two guards
rather than reproduced.

## Open questions

1. **Does a seeded run still offer a gate? ⟨R⟩** Skipping it entirely is the point of the feature. But the gate is also where the server validates the selection against the run's **live** roster — which the local planner cannot know, since it reads `.claude/agents/` on the laptop rather than in the clone. *The first recommendation here was self-contradictory and half of it was unimplementable*: it proposed create-time validation **and** a clone-time fallback for the same check, and create-time validation of `--agent-source repo` **cannot work**. `validateSelection` refuses `repo` against an empty roster (`api/internal/workersvc/agent_selection.go:122`), `rosterFor` reads `run.RepoAgents` (`service.go:2409-2418`), and `runs.repo_agents` is written **only** by the worker's post-checkout report (`runtime.sql:600`) — at create time it is NULL. So the PRD's own headline command on line 72 would be rejected (the status code is not readable from the code — no create-time selection check exists yet, and `ErrInvalidSelection`'s only current mapping is on the SubmitInput path), and exclusions fail harder still (every name misses `known[name]` at `agent_selection.go:138`). *Second recommendation, also wrong* ⟨R⟩: it named "the post-checkout boundary" server-side, which is `runningStateParams` (`api/internal/workersvc/service.go:2365`) — and that function can do **neither** half. It **errors rather than degrades** (`:2387-2392`, both failure arms are bare `return p, err`, mapped to an HTTP error at `handler/workers.go:907`; no fallback arm, no feed emit), and it **cannot see a create-time selection at all** (`:2379`, `if req.AgentSelection == nil { return p, nil }` — it validates only what the worker sent in *this* request, and on the pre-approved path the worker sends none). *Third recommendation, and the machinery for it already exists* **worker-side**: `sdk-executor.ts:876-884` computes `repoAvailable` from the clone's roster, calls `resolveAgentSelection`, and emits `resolved.note` to the feed. What is missing is a branch — `resolveAgentSelection`'s `"ok"` arm (`protocol.ts:836-838`) returns the selection as sent, with no roster check. So: accept unvalidated at create time, and add a **"well-formed but unsatisfiable against this clone's roster"** arm that falls back to `own` and returns a note. One process, one function, and the emit path is already wired. Left unhandled, `selectSubagents(source="repo", …, ctx.repoAgents ?? [], …)` (`sdk-executor.ts:890-897`) yields an empty subagent map against an empty clone roster — exactly what `agent_selection.go:112-114` names: *"a broken run rather than a deliberate lead-only one."*
2. **Structured plan or free text?** D7 makes self-sufficiency a documented constraint, which is the weakest possible enforcement. A schema (goal / files / steps / done-when) would make it checkable at create time. *Recommendation*: ship free text in M1-M3 — it is what the executor already consumes — and let M6's e2e tell us whether cold-start quality is actually a problem before inventing a format.
3. **Base-commit mismatch: refuse or warn?** Refusing is safe and will be infuriating on a busy `main`. *Recommendation*: warn into the feed by default, `--require-base` to refuse. Decided in M4.

## Risks

- **Cold start produces worse work than a warm plan.** ⟨R⟩ *This bullet's mitigation used to read "this being the proven resume shape" — the exact sentence D7 above now retracts, left standing after D7 was rewritten. A reader arriving at Risks first would have taken the retracted claim as current.* The real mitigation is D7's answer: carry `priorWork` into the implement prompt, or state the limitation, and measure it in M6 rather than arguing it.
- **A stale local plan describes files that moved.** M4, open question 3.
- **New untrusted input path.** D5. Bounded, but it is genuinely new surface and should be reviewed as such rather than waved through because the guardrails hold.
- **`auto_approve` overload creeps back in.** D3 exists because it is the cheap-looking wrong answer; a reviewer should check the diff does not quietly take it.
- **M8's SEMANTIC guidance rots silently ⟨R⟩.** *This bullet first claimed the drift test "matches commands and flags — the authoring guidance is prose it cannot see". Half wrong, and the correct half is more useful.* **Flags in prose ARE checked**: `extractDocFlags` runs its regex over the whole frontmatter-stripped document (`api/cmd/uzi/skill_drift_test.go:117-118`), not over code spans. Only **commands** are prose-blind — `codeUnits` (`:164-172`) deliberately strips prose so "run the tool" is not parsed as a command. So what has no test behind it is the *semantic* guidance: self-sufficiency, roster reading, base-commit provenance. That is still the section most responsible for the feature working and still the one that can go stale with every gate green — same shape as the vacuous-negative-assertion class in `CLAUDE.md`, where the failure mode selects for what nobody will look at. If M8's guidance grows load-bearing specifics, it wants a test of its own.
- **⟨R⟩ The same test makes M8 DEPEND ON M3, which the milestone list does not show.** Writing `--plan-file` into `SKILL.md` before M3 defines the flag turns the drift test **red** (`skill_drift_test.go:39`: *"SKILL.md documents flag --%s, which does not exist in the command tree"*). Same for `--require-base` if open question 3 ever puts it in prose. So M8 lands after M3, and any flag the open questions leave undecided stays out of the skill until it exists.
