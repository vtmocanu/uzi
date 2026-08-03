# PRD #209 — Seed an externally-authored plan onto a run (plan locally, implement in uzi)

**Issue**: [#209](https://gitlab.example.com/vtmocanu/uzi/-/issues/209) · **Label**: PRD · **Priority**: Medium
**Area**: `agent/src/runner.ts` + `agent/src/sdk-executor.ts` + `agent/src/executor.ts` (the three-way plan state) · `api/internal/handler/workers.go` + `api/internal/workersvc/service.go` (create-time seeding) · one migration on `runs` · `api/cmd/uzi/run.go` + `api/internal/uzicli/skill/SKILL.md` (the CLI surface).
**Line references** are against `5ea4d2f8`.
**Status**: not started. **M1 and M2 are the whole feature**; M3-M7 are surface and proof.

## Problem

A uzi run cannot be told what to do. It can only be told *where to look*.

`POST /api/repos/{id}/runs` takes an `issue_iid` and nothing else of substance
(`api/internal/handler/workers.go:671-695`). The worker claims the run, clones,
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
`approve_plan` body (`agent/src/protocol.ts:753-758`) — and git cannot express it
without a sidecar format. (c) The API call is authenticated as a user with a scoped
token; a commit author is not an authz fact. (d) `api/cmd/uzi/` already exists as a
second API consumer of exactly this shape, so the client cost is one flag.

**D2 — Reuse `kind='issue'`. Do not add a run kind.**
A `kind='seeded'` would drag `runs_kind_check` + `runs_kind_shape`
(`00058_run_judge_self_improve_kinds.sql:26-35`), the claim payload, the judge, the
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

**D7 — The plan text must be self-sufficient, and the docs must say so.**
Phase 1 today does more than emit text: it warms the SDK session the implement
phase continues from. A seeded run starts **cold**, with `plan_md` as its only
instructions. This is the proven resume shape, not a new risk — but a plan written
conversationally ("as we discussed", "the file we looked at") is useless to it.
Docs carry this as a stated constraint; M4 adds the mechanical half.

## Milestones

- [ ] **M1 — Seeded plan reaches the worker.** Migration adds `plan_source text CHECK (plan_source IN ('agent','seeded'))` (default `'agent'`) to `runs`; `POST /api/repos/{id}/runs` accepts optional `plan_md` + `agent_selection`, size-capped and scrubbed (D5), persisted with the selection through the same columns the human gate writes. Claim payload ships them with no new field beyond `plan_source`. **Validated by**: a live-DB test showing the claim for a seeded run carries `plan_md`, `plan_approved: true` and the selection, with no `approve_plan` input row anywhere.
- [ ] **M2 — The worker implements it.** The three-way discriminator of D4 lands in `runner.ts` and both executors; a feed line records that the plan was supplied externally, so it is visible in the transcript rather than inferable. **Validated by**: all four rows of D4's table exercised as tests, row 3 (approved, no session, not seeded ⇒ re-plans) included as a named regression test.
- [ ] **M3 — CLI surface.** `uzi run create --plan-file <path>` (`-` for stdin), plus `--agent-source` / `--exclude-agents` reusing `run.go:454-470`'s existing validation. Exit codes per the documented contract; `--exclude-agents` without `--agent-source` stays a usage error. Bundled `SKILL.md` updated, drift test green.
- [ ] **M4 — Staleness guard.** The client sends the commit it planned against; the worker compares it to the clone's resolved base (`runnerClone.baseCommit`, `runner.ts:439`) and surfaces a divergence. **Open question 3** decides refuse-vs-warn.
- [ ] **M5 — Web surface.** The run page states that a run's plan was externally supplied and renders it; the board start button is unchanged (no start-run modal — the user ruled against one on 2026-07-27, `handler/workers.go:683-685`).
- [ ] **M6 — Proof, end to end.** e2e coverage through the stub executor (which already honours the pre-approved path, `executor.ts:605`) for a full seeded run: create → claim → implement → MR, never passing `awaiting_approval`. Confirm the judge reads a trace with no planning phase without degrading — asserted, not assumed.
- [ ] **M7 — Docs.** `docs/cli.md`, `ARCHITECTURE.md` (Agent runtime), and the D7 constraint written where a plan author will read it before writing one.

## Success criteria

1. A user with a written plan reaches "worker is implementing" in **one command**, with no approval gate and no planning turn.
2. A run created with no `--plan-file` behaves **byte-identically** to today. This is the anti-regression criterion and it outranks every other item here.
3. Row 3 of D4's table still re-plans. A session dropped mid-flight is not a seeded plan and must never be treated as one.
4. The four guardrail layers are unchanged, and the MR still comes from a branch that is not `main`.
5. `uzi run create --plan-file p.md` with no roster flag runs the repo's own agents.

## Open questions

1. **Does a seeded run still offer a gate?** Skipping it entirely is the point of the feature. But the gate is also where the server validates the selection against the run's **live** roster — which the local planner cannot know, since it reads `.claude/agents/` on the laptop rather than in the clone. *Recommendation*: skip the gate by default (that is the user value), validate the selection at create time, and fall back to `own` with a feed note if the clone's roster does not contain it. Revisit only if that fallback fires in practice.
2. **Structured plan or free text?** D7 makes self-sufficiency a documented constraint, which is the weakest possible enforcement. A schema (goal / files / steps / done-when) would make it checkable at create time. *Recommendation*: ship free text in M1-M3 — it is what the executor already consumes — and let M6's e2e tell us whether cold-start quality is actually a problem before inventing a format.
3. **Base-commit mismatch: refuse or warn?** Refusing is safe and will be infuriating on a busy `main`. *Recommendation*: warn into the feed by default, `--require-base` to refuse. Decided in M4.

## Risks

- **Cold start produces worse work than a warm plan.** Mitigated by this being the proven resume shape, and measured by M6 rather than argued.
- **A stale local plan describes files that moved.** M4, open question 3.
- **New untrusted input path.** D5. Bounded, but it is genuinely new surface and should be reviewed as such rather than waved through because the guardrails hold.
- **`auto_approve` overload creeps back in.** D3 exists because it is the cheap-looking wrong answer; a reviewer should check the diff does not quietly take it.
