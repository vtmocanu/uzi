# PRD #215 — Pipeline the lead's review lane and overlap its integration gate

**Issue**: [#215](https://github.com/vtmocanu/uzi/-/issues/215) · **Label**: PRD · **Priority**: High
**Area**: `api/internal/agenttmpl/builtins/lead.md` (the prompt change) · **`api/internal/agenttmpl/render_test.go` (the pins M1/M2 retire — not optional, see M2)** · `api/internal/store/agent_templates_builtins.go` + `api/internal/handler/agent_templates.go` + `api/internal/store/skills_builtins.go` (delivery, M6) · `probes/` + `scripts/` (M0 artifacts) · `docs/` (M8).
**Line references** are against `d367653b`. `lead.md` was last touched by `6814a174` (2026-08-02).
**Status**: in progress — the prompt change (M1–M5) and its docs (M8) shipped and
were reviewed on branch `agent/issue-215`; M0's instrument shipped without live
numbers, M6 was descoped to documentation (D10 is stale — see the run note below
the milestones), and M7 is deferred to post-deploy runs. Not moved to `prds/done/`.
**Evidence**: measured 2026-08-03 from the live feeds of `edbc3884-…` (issue #209, uzi) and `a146df98-…` (issue #78, example-app), both on worker `base.l-da4a` (`l` = 4 CPU / 8 GiB, `max_concurrent_runs: 2`).
**Reviewed** 2026-08-03, one pass. Four blocking findings, all applied. **Two were demonstrated by execution, not argued** — the reviewer built `agenttmpl` as a standalone module and ran the PRD's own proposals against it.

**Standing caveat**: the concurrency figures are a two-run sample taken mid-flight, on two different issues in two different repos. They establish the *shape* and are not a baseline. M0 exists to build one.

> **🔴 UNRESOLVED PREREQUISITE — D10 AND THE DIAGNOSIS MAY BE IN TENSION, AND
> NOBODY HAS CHECKED.** D10 establishes that the live `lead` row holds whatever
> `lead.md` said **when it was first inserted**, because the reconcile has never
> overwritten it since. The diagnosis below quotes `lead.md` **at `d367653b`**.
> If D10 is right, **the two observed runs may not have executed the text this
> PRD quotes**, and M2 would be rewriting lines nobody ran.
>
> This was raised by the fact-check and could not be settled from the CLI (there
> is no template subcommand, and the API read was blocked). **It is one request**:
> `GET /api/agent-templates` on the live host, find `name: "lead"`, diff
> `prompt_body` against `git show d367653b:api/internal/agenttmpl/builtins/lead.md`
> (body after the frontmatter), and read `is_builtin`. Differ → re-anchor the
> diagnosis to the live body. Match → D10's "seeded on a past boot" premise needs
> softening. **Do this before M2.**

**Correction record.** The first draft called this "change three sentences and add one". Measured against the tree that is wrong by the larger half: the edit also retires strings pinned by an existing test suite, and its proposed test (old M4) was **demonstrated green over the exact contradiction it forbids**. Both corrections are recorded rather than quietly applied, because a PRD that under-scopes its own test obligation is how a milestone lands red.

## Problem

The lead keeps an average of **1.6 subagents busy** on a worker running at 8-60%
CPU and 3.7 of 8 GiB.

| run | elapsed | 0 agents | exactly 1 | 2+ | avg | peak |
|---|---|---|---|---|---|---|
| #209 | 5685s (95 min) | 1094s (19%) | 2487s (44%) | 2104s (37%) | 1.57 | 4 |
| #78 | 6582s (110 min) | 1153s (18%) | 1851s (28%) | 3578s (54%) | 1.68 | 4 |

Independently reproduced by the fact-check to within 2 seconds on every cell.
**"Peak 4" is an OBSERVED MAXIMUM, not a budget** — #209 had **11** agent roles
allocated, and a search of `agent/src/` and `api/internal/` found no configured
subagent-concurrency cap. *(That is a failed search, not a proof of absence —
this repo's own convention is that a failed grep is not evidence. Treat it as
"no cap located", and if one matters to SC4, find it rather than infer it.)* So
the idle capacity during the solo M1 is larger than "three slots", and SC4 must
not treat 4 as a ceiling.

**63% of #209's wall clock had one subagent or none.** Three structures produce
it, all written down in `lead.md`.

### 1. The review lane is one barrier wave at the end

`lead.md:47` — "After all parallel results are in, you integrate: diff the
working tree against the last commit … commit once, run the quality gates once
yourself, and include the declared scope map **when you dispatch the review
wave**".

#78 finished M1 at t=1703 and dispatched no validator against it until t=3354
(27.5 min). #209's M1 landed at t=3092 and its review wave dispatched at t=4952
— **31 minutes later** (26.7 min from the end of the integration gate at
t=3350). ⟨*corrected: the first draft said 66 minutes, which measured from M1's
**dispatch** (t=917) rather than its landing, contradicting this PRD's own
gap-1 line. The point survives; the number was 2x.*⟩ Findings therefore arrive
after every unit has landed, which is where #78's `Fix code review findings`
agent spent 1522s against accumulated drift.

### 2. The lead's own integration gate runs with every slot idle

Measured in #209's barrier gaps: ~170s of gates in gap 1 (t=3092-3350) and ~270s
in gap 2 (t=4593-4952). Total zero-subagent time: 1094s (#209), 1153s (#78).

**This gate must not be removed.** It is the only check over the *integrated*
tree. ⟨*The first draft justified this as "`CLAUDE.md` says in four places that
'the coder said green' is not evidence". That string appears nowhere, and the
four literal `not evidence` passages in `CLAUDE.md` are all about
**instruments**, not about a subagent's report. The discipline is real and its
home is `.claude/agent-team.md`; the nearest in-repo statements about a **report**
are `CLAUDE.md:181` ("'sqlc regenerated cleanly' must never appear in a report as
though it were a measurement"), `:185`, and `:571`.*⟩

### 3. A contract unit runs solo while its dependents wait

`lead.md:52` — "When in doubt … run them serially." #209's `M1 server: seeded
plan contract` ran **2175s alone** — 38% of the 5685s sampled. ⟨*The seconds are
the number to carry: they need no denominator. The percentage moves, because the
run was still going — at the 7347s it had reached when the fact-check sampled,
the same interval is 29.6%.*⟩ #78's M1 ran 1322s alone.
The dependents genuinely needed M1's contract; the serialization is not wrong,
it is *coarser than the dependency*.

### The defect: `lead.md` is AMBIGUOUS, and its only coherent reading is the slow one

⟨*Sharpened after review. The first draft asserted a flat contradiction; that is
true under one reading and not the other, and the weaker-but-unassailable
framing turns out to be the more useful one, because it tells M2 what is
actually missing.*⟩

`lead.md:32` — "Read-only work fans out again **after an implementation unit
lands**" — is singular, and "unit" is a defined term in the same bullet list
(`:36` "splits it into units with no dependency between them", `:43` "one
invocation per unit"). Read that way it prescribes per-unit dispatch and `:47`'s
"after all parallel results are in" contradicts it.

But there is a second, **self-consistent** reading: "lands" is never defined, and
`:44-46` forbids implementers from committing while `:48` has the lead "commit
once". **So a unit has no mechanism to land independently.** Under that reading
`:32`'s "after an implementation unit lands" simply *is* the integration moment
`:47` describes, and the two are a summary and its detail.

This matters for the fix. If the text were merely contradictory, rewording would
settle it — and the observed behaviour would show the lead picking the expensive
branch. It does not: the lead is equally well explained as taking **the only
coherent reading available**. **The missing thing is a per-unit landing
mechanism, not a clearer sentence**, which is why M2 pairs the wording change
with commit-anchored dispatch (D4) rather than shipping wording alone.

## Solution

Three prompt changes in `lead.md`, **in dependency order rather than in the
order they were discovered** — M2 is cheap and safe, M3 and M4 each need a
mechanism before they are safe at all:

1. **Anchor validator dispatch to a COMMIT** (M2). Resolve the `:32`/`:47`
   contradiction toward per-unit dispatch, and make each validator review an
   immutable artifact (`<base>..<sha>`) rather than "the diff".
2. **Overlap the integration gate with the READ-ONLY wave only** (M4), with a
   stable build subject.
3. **Split a contract unit at its seam** (M5), where the seam has *executed*.

Nothing is deleted. No validator is dropped, no gate is skipped.

## Decision log

**D1 — Product template, not the dev-team roster.** The lead is
`api/internal/agenttmpl/builtins/lead.md`, `go:embed`-shipped and boot-seeded.
`.claude/agents/` holds no `lead.md` and must not gain one. The sibling change
to this repo's own dev team is `vtmocanu/skills#13`.

**D2 — Overlapping the gate is a BOUNDED TRADEOFF, not free. ⟨corrected⟩** The
first draft said "Overlapping costs nothing in verification strength". **That is
false against a recorded decision in this repo.** `Taskfile.yml:54-60`: "SERIAL,
NOT CONCURRENT — a recorded decision, not an oversight … CPU contention is an
already-measured flake source here … **The gate's job is a trustworthy verdict,
not a fast one.**" It cites `web/vite.config.ts:11-22`, where `testTimeout` was
raised to 20000 because "under full-suite CPU contention, THREE unrelated tests
each timed out once across ~20 runs", and which notes "A 1-in-10 red trains
people to re-run instead of read, which is the actual damage".

The measured cost, at the precision the evidence supports ⟨*corrected — the
first draft of this section cited gate commands at "16.3s vs 43.1s" and `npm
test` "from 34s to 68s on an unchanged tree". The tree was **not** unchanged
(those three runs reported 1407, 1455 and 1522 tests, 70 minutes apart with
coders editing between them), and the gate pair is **confounded**: a longer
command is mechanically more likely to intersect another interval, and the
"alone" set is dominated by ~108 short commands. PRD #216 carried this
correction and #215 did not — a merge gap, and it mattered here because these
figures are load-bearing for a **blocking** scoping decision.*⟩:

- Gate commands: **16.3s alone vs 43.1s overlapping** (47.6s vs 12.2s on an
  independent classification). Direction only, for the confound above.
- `npm test` at a **constant suite tally**, which is the clean comparison:
  **36.1 / 43.7 / 43.9 / 52.6 / 73.3 / 79.8 s** at `tests=1474` (2.2x), and
  **33.8 / 34.2 / 34.4 / 43.4 / 50.4 / 69.4 / 78.1 / 89.6 s** at `tests=1541`
  (2.65x).

Both push toward the very timeouts that were raised because of contention.
**A gate that reddens intermittently is weaker verification**, because the
documented human response is to re-run and the retry destroys the evidence.

So the gate keeps its blocking authority *and* the cost is real. M4 overlaps it
with the **read-only wave only** (D3), and M7 reports red-then-green-on-retry
counts so the cost is visible rather than denied.

**D3 — "The next wave" means the READ-ONLY wave. ⟨new, blocking⟩** The first
draft said "dispatch the next wave, then gate", without saying which. That
ambiguity *is* the safety question. All subagents share one worktree —
`tester.md` says so and forbids write-mode formatters for that reason;
`coder.md` forbids parallel coders from committing or running repo-wide gates
for the same reason. `lead.md:47`'s "diff the working tree against the last
commit … commit once" is **undefined over a tree a third writer is editing**: the
commit sweeps up half-written work the scope confirmation cannot separate, and
the gate compiles a tree the lead does not control (`fmt-check:api` lists another
unit's in-flight files, and its shipped form exits 2 on a file that does not
parse — a red the lead will attribute to the wave it just gated).

Overlapping with read-only validators is largely safe. Overlapping with the next
*implementation* wave is not, and needs a stable subject — a `git worktree add`
off the just-made commit is the cheap mechanism. **M4 ships the read-only
overlap; the implementation overlap is explicitly out of scope** and left to a
follow-up that owns the worktree mechanism.

**D4 — Validators review a COMMIT, not "the diff". ⟨new⟩** `lead.md:33`'s "over
the diff" is unambiguous under a barrier because the tree is quiescent, and
ambiguous the moment it is not: a validator's `git diff` would mix its unit with
the next unit's in-flight edits, attaching findings to the wrong unit. Dispatch
against `<base>..<sha>`. Sharper for the `tester`, which is a validator that
*runs the gate* ("Run the whole gate, not just the tests") — a per-unit tester
overlapping the next coder produces build failures caused by code it was not
asked to review. **This is the cheapest change in the PRD and it is what makes
M2 safe.**

**D5 — The seam must have EXECUTED, not merely been committed. ⟨strengthened⟩**
"Committed" closes drift-from-a-described-contract and leaves
drift-from-an-unexecuted-one wide open — which is the failure this repo has
actually recorded: `CLAUDE.md`, "A GREEN `sqlc generate` IS NOT EVIDENCE THE
QUERY RUNS" (clean Go, green build/vet/tests, and Postgres answered 42P08 the
first time the statement was prepared). M5's seam is exactly that artifact class.
So: a live-DB test through at least one query, or a handler test through the
route, **before** dependents are dispatched. And **the lead commits the seam** —
`coder.md` forbids `git commit` in parallel mode, consistent with `lead.md:42`'s
"make that edit yourself during integration".

**D6 — Fan-out stays gated on disjoint ownership.** `lead.md:36-43`'s rule is kept
verbatim, together with the `:44-46` bullet ("explicit, non-overlapping list of
files … not to commit and not to run repo-wide build or test commands", pinned
twice at `render_test.go:169-170`). The shared-wiring list is wider than a
parenthetical suggests: "go.mod, go.sum, lockfiles, generated code, routers and
registration files, compose or config files". M5's seam-split does not relax it.

**D7 — Success is measured on CLASSES and a loop count, not a finding tally.
⟨corrected⟩** `CLAUDE.md`: "no refinement of a count reaches a difference the
count cannot see. Assert on the CONTENT." Under pipelining, false findings from a
mutating tree are indistinguishable from real ones in a tally, so a count is
satisfiable while quality drops — precisely the failure the speed-vs-quality
constraint aims at. It is also in tension with D8, which predicts the end pass
finds *less*. Criteria therefore key on defect classes plus the review→fix loop
count.

**D8 — Per-unit review does not retire the integrated pass.** A validator against
unit N reviews unit N; cross-unit interactions are what a per-unit review cannot
see. The end pass finds less, it is not skipped.

**D9 — M5 changes what the human approves.** The approval gate is the only human
control point (`lead.md:17-28`, `submit_plan`). If a unit splits into seam +
implementation and dependents launch off the seam, the submitted plan must
declare the seam and its consumers, or the approved plan and the executed shape
differ.

**D10 — Delivery is not free and is the part most likely to be forgotten.**
`ReconcileBuiltinTemplates` is "idempotent and **edit-preserving**: … an existing
row (builtin or admin-edited) is **never overwritten**"
(`agent_templates_builtins.go:29-33`, `ON CONFLICT DO NOTHING` at `:66-85`), and
templates reach the worker from DB rows at claim time
(`workersvc/claim.go:249-270`'s `agentsFromTemplates`, fed by
**`service.go:1363`'s `ListClaimAgentTemplates`** — cite both, since the DB query
is the half that makes the argument airtight independently of the reconcile). **So shipping an edited `lead.md` changes nothing
on any existing deployment.** The reset route exists
(`handler/agent_templates.go:396`, wired at `handler.go:735`, reachable from
`web/src/pages/AgentDetail.tsx:58`) and `is_builtin` is immutable on update
(`queries/agent_templates.sql:48`), so an admin-edited builtin stays resettable.
But reset is **per-row, destructive** (it overwrites
description/model/tools/prompt_body with no diff view), **admin-only** (scope
auth at `handler/agent_templates.go:146-152`), and **refuses a non-builtin row**
(`:408` returns 400 unless `t.IsBuiltin`) — so a *shadowing* custom row, the case
the reconcile warns about at `agent_templates_builtins.go:81-83`, is **not
resettable at all**. There is **no drift query** answering "which of
my rows are behind the shipped builtin?", there is **no CLI path**
(`api/cmd/uzi/root.go:141-155` registers no template command), and **the identical
edit-preserving reconcile governs builtin skills**
(`store/skills_builtins.go:89`). M6 owns all four.

**D11 — Seam language stays stack-neutral.** `lead.md` is deliberately
stack-neutral apart from one "Go package / TypeScript project" clause. "migration
+ `sqlc generate` output" names tools most users of this product do not have.
State the seam generically: the schema change, the generated or derived types,
the interface or route shape.

## Milestones

Ordered by dependency and risk. **M2 is the cheap, safe majority of the win**;
M4 and M5 each need a mechanism first.

- [ ] **M0 — Baseline.** A script (in `scripts/`) that, given a run id, emits the
      concurrency profile from `uzi run logs --json` plus per-wave timings.
      Instrument verified: the CLI exists (`api/cmd/uzi/run.go`) and
      `run_messages` carries `created_at`/`agent`/`agent_instance`
      (`store/models.go:310-320`). Baseline numbers land in `probes/` (archived
      evidence, gates nothing) — **not** `fixtures/`, which is read by loaders
      across the module boundary and whose edits move gates. Fix N, report
      variance, and prefer a **paired** comparison (same issue before and after),
      since wall clock is dominated by issue shape rather than by the prompt.
- [x] **M1 — Update the pins BEFORE touching the prompt.** ⟨new, blocking⟩
      `render_test.go` pins the exact strings M2/M4 retire:
      `:172` `"commit once, run the quality gates once yourself"` and `:492`
      `"fans out again after an implementation unit lands"`, inside
      `TestLeadParallelDispatchPhrases` (`:151`, 14 whole-body pins) and
      `TestLeadPlanCritiquePhrases` (`:458`, 8 region-scoped pins behind a
      three-clause guard, `splitLeadRegions` `:322`). `leadBulletLandmark`
      (`:231`, "Do not name a fixed reviewer-then-auditor pair") sits at
      `lead.md:34`, **inside** the bullet M2 rewrites — move or reword it and all
      eight region assertions fatal at guard 3 (`:336-356`) before any of them
      runs, and `render_test.go:484-490` records that pin as load-bearing for the
      split itself. **M1 therefore replaces TWO COUPLED THINGS at once**: if M2
      *deletes* rather than rewords that sentence there is no constant to update,
      so `leadBulletLandmark` must be re-derived from whatever prose sits at the
      new bullet's edge — and the bullet-region pin at `:492` is itself a string
      M2 retires. `:488-490` states what happens if only one is replaced. So this milestone re-derives the guard's
      fold measurements and applies `CLAUDE.md`'s retire-a-string sweep (grep the
      old strings across the test tree; check each negative assertion for a
      pairing).
- [x] **M2 — Per-unit, commit-anchored validator dispatch.** Resolve the
      `:32`/`:47` contradiction toward per-unit dispatch (D3's read-only scope),
      with validators anchored to `<base>..<sha>` (D4). **This is the milestone
      that delivers most of the benefit at the least risk; it can ship alone.**
- [x] **M3 — Honest test posture.** ⟨replaces the old M4⟩ The old milestone
      claimed a test could pin "the file must not contain both procedures". That
      was **demonstrated false**: implemented as specified, it returned
      `perUnit=false barrier=false` and **PASSED over a `lead.md` keeping both
      procedures reworded** — doubly vacuous, since neither half matched. The
      file it would live in already says so (`render_test.go:428-432`: a negative
      assertion "goes vacuous the moment the copy changes and then guards nothing
      forever … Real coverage needs a semantic check on the rendered prompt, which
      is a different instrument"; `:420-427`: `strings.Contains` is monotone under
      insertion, so no substring instrument can see an inserted contradicting
      paragraph). So: either a **positive** pin on the single surviving procedure
      paired with a negative per `CLAUDE.md`'s paired-negative rule, or an
      explicit statement that no test pins this and M7 is the only check.
- [x] **M4 — Overlap the integration gate with the read-only wave.** Gated on
      D3. States the gate's blocking authority over the commit explicitly so it
      is not read as optional. The implementation-wave overlap is out of scope.
- [x] **M5 — Seam-split for contract units.** Gated on D5 (executed, not just
      committed), D9 (declared in the approved plan), D11 (stack-neutral
      wording). **Hold behind M2 and M4.**
- [ ] **M6 — Delivery.** A tested path for applying an edited builtin to a
      running factory, covering all four gaps in D10: the destructive-reset
      caveat written down, a **drift query** ("which rows are behind the shipped
      builtin?"), a CLI path or a stated reason there is none, and the same
      treatment for builtin **skills** — note the drift query needs **two**
      implementations, because `skills_builtins.go:94-97` records a deliberate
      divergence: builtin-ness there is the `scope` value keyed on
      `(name, scope='builtin')`, not an `is_builtin` flag. Includes reading the
      live `lead` row back and seeing the new body. **Hard prerequisite for M7** — without it the PRD
      lands as a no-op that looks shipped.
- [ ] **M7 — Measured validation.** Re-run M0 over post-M6 runs. Report: wall
      clock, avg concurrency, **gate invocation count and total gate wall time**
      (not just per-command duration), **red-then-green-on-retry counts** (D2),
      **usage-limit parks and token spend**, and the defect classes plus
      review→fix loop count (D7).
- [x] **M8 — Docs.** Update the run-lifecycle docs, following the existing
      frontmatter rules.

## Implementation note (run `agent/issue-215`)

What shipped this run, against the milestone boxes above:

- **M1–M5 (done).** `api/internal/agenttmpl/builtins/lead.md` reworded to a single
  per-unit read-only dispatch procedure anchored to `<base>..<sha>` (M2), an
  integration gate overlapped with the read-only wave but retaining full blocking
  authority over the commit (M4), and an executed-seam, plan-declared,
  stack-neutral seam-split (M5). The `render_test.go` pins the reword retired were
  re-derived — the whole-body gate pin became overlap + blocking-authority +
  immutable-range pins, and the load-bearing `bulletCases` pin became a positive
  pin on the surviving procedure (M1/M3). `task`-level `go test ./internal/agenttmpl`
  green; reviewer + auditor + fact-checker + tester wave clean.
- **M8 (done).** `docs/agent-templates.md` "Parallel dispatch" section updated to
  the new behaviour; `node web/scripts/check-docs.mjs` OK.
- **M0 (instrument only, box left unticked).** `scripts/uzi-concurrency-profile.sh`
  + `probes/215-concurrency-baseline.md` ship the baseline instrument
  (shellcheck-clean, unit-checked against a synthetic capture). **No baseline
  numbers were produced** — no live run id exists in an isolated worktree — so the
  "numbers land in `probes/`" half is not done. Folded with M7.
- **M6 (descoped to documentation, box left unticked).** D10's premise is **stale**:
  since the PRD was drafted (`d367653b`), PRD #275/#201 shipped `RefreshPristineBuiltin`
  (`agent_templates_builtins.go` → `queries/agent_templates.sql`), which auto-heals
  pristine builtin rows at boot, plus per-row `differs_from_builtin` and a web
  reset/diff view. So a shipped `lead.md` edit is **not** a no-op, and the bulk
  "drift query" M6 asked for is already answered per-row by the list endpoint + web
  UI. This run therefore documented the delivery reality, the destructive-reset
  caveat, the no-CLI rationale (admin CLI is read-only; reset mints nothing), and
  the builtin-skills asymmetry (no boot refresh) in `docs/agent-templates.md`,
  rather than building a redundant query/CLI. The approving human accepted this
  descope at the plan gate. The literal M6 (a new drift query with two
  implementations + a live-row read) was **not** built, so the box stays open.
- **M7 (deferred).** Measured validation requires re-running M0 over post-deploy
  live runs; it cannot execute from an isolated worktree. Box stays open.

## Success criteria

1. `lead.md` describes exactly one dispatch procedure for the read-only lane.
2. Every validator dispatch names an immutable artifact (a SHA range), never
   "the working tree" (D4).
3. `task gate:api` is green, including the updated `render_test.go` pins (M1).
4. Average subagent concurrency on a multi-milestone run exceeds M0's baseline
   **by more than that baseline's reported spread** — the two-run sample sat at
   1.57-1.68 against a peak of 4, and "materially" needs a yardstick.
5. Time with zero subagents running falls from the ~18-19% observed.
6. The same defect **classes** still surface, and the review→fix loop count does
   not rise (D7). A finding tally is explicitly not the criterion.
7. Gate red-then-green-on-retry does not increase (D2). Gate invocation count and
   total gate CPU are reported, not just per-command latency (R3).
8. The lead's integration gate still runs on every wave and still blocks the
   commit.
9. An operator can apply a `lead.md` change to a running factory, verify it
   applied, and ask which rows are behind the shipped builtin (D10).

## Risks

- **R1 — M5 is the one change that can trade away correctness.** Dependents
  building against a seam that later changes is the harm; D5's executed-seam rule
  is the mitigation. If M5 proves risky, M2 alone delivers most of the benefit.
- **R2 — Prompt changes are not deterministic, and NO TEST PINS THIS.
  ⟨corrected⟩** The first draft said "this is why M4 pins the property in a
  test". That was demonstrated false (M3). Behaviour is measured by M7, not
  asserted by a unit test.
- **R3 — The gate multiplies, and D2 prices it.** Per-unit landing means one gate
  run per unit — 4-6 on these runs, against 2 today. Even fully overlapped, total
  gate CPU rises 2-3x on a 4-CPU worker already running two runs. M7 reports
  invocation count and total gate wall time so a regression is visible rather
  than inferred. PRD #216's fleet-spread is the complementary mitigation.
- **R4 — Token spend and usage parks.** Four opus subagents in flight raises peak
  burn against the user's own Anthropic token and brings the usage-limit park
  (`workersvc/limitwait.go`) closer. **A run that parks is not faster.** M7
  reports parks and usage beside wall clock.
- **R5 — M6 is easy to declare done wrongly.** "We shipped the file" is not
  delivery; reading the live row back is.
- **R6 — Interaction with PRD #216.** #215 raises per-run CPU demand; #216
  spreads runs across workers. Validate together.

## Open questions

- M3: is a positive-plus-negative pin on the surviving procedure worth the
  maintenance, or is "no test pins this" the honest posture?
- M5: seam-split lead-discretionary, or required whenever the plan declares a
  dependency edge?
- M6: does the drift query belong under `uzi admin` (there is no template command
  today), or is web-only correct given reset mints nothing?
