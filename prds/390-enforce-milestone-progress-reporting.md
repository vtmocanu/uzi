# PRD #390: Enforce mid-run milestone progress reporting (the lead must actually mark milestones)

**Issue**: #390
**Priority**: Medium
**Status**: draft

## Problem

On a gated, milestone-structured `issue` run, the live tracker
(`runs.milestones_completed` / `runs.milestones_in_progress`) only advances when
the lead **voluntarily** calls the optional, informational `report_progress`
tool (or declares completed ids on `signal_done` at the very end). The prompt
frames it as `MAY` (`agent/src/prompt.ts` `milestoneStatusNote`, ~L911: *"On a
multi-turn run you MAY call `report_progress`"*), the tool is explicitly
non-gating ("does NOT end your turn ... purely informational", `agent/src/signals.ts`
~L295-299), and nothing in the executor loop requires it. So a run that is
genuinely working shows **0/N for its whole life**.

Observed live on run `64a7f916` (issue #381): status `running`, 3
implement/review iterations, 351 tool calls, ~$22 spent — and
`milestones_completed = []`, `milestones_in_progress = []`. The lead never
marked a single milestone.

There is a second, sharper failure folded into the same observation. The wire
value is a **non-null empty array `[]`**, not `null`. Per the documented design
(`ARCHITECTURE.md` "Milestone tracker reconciliation", L681-683) the web renders
a **null** tracker as neutral `M–/N` ("not reported"), *distinct* from a genuine
`0/N`. Because this run holds `[]` and not `null`, it renders as **`M0/N`**,
which reads as failure on a run that is doing its job. The `[]` gets there
because the lead called `report_progress` at least once with empty/default
arrays (the tool's zod schema defaults both sides to `[]`, `agent/src/signals.ts`
L304/L310), `scanSignals` sets `out.progress = {completed: [], in_progress: []}`
(L628-631), and that empty snapshot is persisted on the running report. There are
**two** writers of the milestone fields on a `status:"running"` report — the
per-iteration `reportIteration` (`agent/src/runner.ts` L811-815) and the
**checkpoint closure** (`agent/src/runner.ts` L943-956) — and both funnel the same
`latestProgress`/`opts.progress` through `progressParams`/`SetRunRunning`, which
encode a non-nil empty slice as `'[]'`. For run #381 (which never checkpointed) the
writer was `reportIteration`; the D3 fix at the `scanSignals` **source** neutralizes
both by construction.

### Why it is this way (prior art, deliberately)

PRD #122 (`prds/done/122-milestone-structured-runs.md`) made these tools
**optional by design**: `report_progress` is "purely informational", and
`checkpoint` is "a durability boundary, NOT a quality gate". PRD #265
(`prds/done/265-milestone-completion-fidelity.md`) added **completion-time**
reconciliation (M1: `signal_done.milestones_completed` unions in at the end) and
the null-vs-`0/N` display (M2), and explicitly left *"optionally lighten mid-run
progress reporting"* as future work. This PRD is that future work, taken in the
"strong" direction: make mid-run reporting an enforced part of the loop the
executor already owns, rather than a courtesy the model can skip. Related open
issues: #260 (approve-time freeze root-cause), #261 (seeded plans get no
milestones) — both distinct from this.

## Solution

Enforce mid-run progress reporting in the **executor's implement/review loop**
(`agent/src/sdk-executor.ts`, the `for (;;)` at L1197), where the loop cadence and
per-turn prompt already live — without reversing PRD #122's durability-vs-gate
decisions and without ever failing a run merely for a missing report. Four moves:

1. **Require, don't suggest.** Change the prompt from `MAY` to a required
   per-turn declaration, and add an **escalation** line the executor turns on
   when the previous turn reported nothing.
2. **Detect + escalate + surface, in the executor.** Track whether each turn
   produced a *real* progress signal; if not, escalate the next turn's prompt
   and, after K consecutive misses, emit a visible status signal so a silent
   non-reporter becomes observable.
3. **Stop empty reports from lying.** Treat an all-empty `report_progress` as
   **no signal** (do not set `out.progress`), so it never overwrites real
   progress and never persists `[]`. This keeps the column `null` when nothing
   was really reported, which is exactly what the `M–/N` neutral render needs.
4. **Fix the display honesty (scope add).** Guarantee a never-really-reported run
   renders neutral `M–/N` — not `M0/N` — end to end (web badge, run view, runs
   list, CLI), and audit that no other write path persists `[]` on a plain
   running heartbeat.

The design keeps PRD #265's principle intact: **completion is declared, not
inferred.** We do not guess which milestone the lead is on from commits or
iteration count; we *require the lead to say so* and make its silence loud.

### Resolved facts (offline-verified from this repo at HEAD — no open web needed)

- **Enforcement point exists.** `agent/src/sdk-executor.ts`: implement/review loop
  `for (;;)` (L1197); `latestProgress` declared `undefined` (L444); sent on the
  running report via `ctx.reportIteration?.(iteration, latestProgress)` (L1203);
  rendered every turn via `buildImplementPrompt({ ..., progress: latestProgress })`
  (L1233/L1272); updated only on a real signal `if (turn.progress) latestProgress = turn.progress`
  (L1294); an executor-owned **iteration-boundary** already runs without the lead's
  cooperation (fallback checkpoint, L1311). `scanSignals` sets `result.progress`
  last-wins (L1801-1804).
- **The empty-`[]` chain.** `report_progress` zod defaults both arrays to `[]`
  (`signals.ts` L304/L310); `scanSignals` sets `out.progress` whenever the tool_use
  is seen, even if both are empty (L620-631); `runner.ts` sends the fields when
  `progress` is truthy — from **two** writers, `reportIteration` (L811-815) and the
  checkpoint closure (L943-956), both fed by `latestProgress`;
  `progressParams`/`validateProgressIDs`
  (`api/internal/workersvc/milestones.go` L125/L147) encode a non-nil empty slice as
  `'[]'`; `SetRunRunning` writes it (narg non-NULL → union → `'[]'`); the read DTO
  (`api/internal/handler/workers.go` L479-487 via `DecodeMilestoneIDs`) preserves
  `null` vs `[]`; `apitypes.Run.MilestonesCompleted` is `[]string` with no
  `omitempty` (nil→`null`, `[]`→`[]`). So `null` renders neutral and `[]` renders
  `M0/N`.
- **The display split.** `web/src/lib/runBadge.ts` L462 `reported = milestones_completed != null`;
  `milestoneBadgeText` L472-475 (`M{done}/{total}` when reported, else `M–/{total}`);
  render sites `web/src/pages/RunView.tsx` L175/L212, `web/src/pages/RunsList.tsx`
  L301, CLI `api/cmd/uzi/run.go` L1366. Migration `00099` keeps the column nullable
  and **deliberately** avoids `DEFAULT '[]'` (its own comment).
- **Gating.** `report_progress`/`checkpoint` are exposed on **issue runs only**
  (`sdk-executor.ts` L608-610, `signals.ts` `SignalServerOptions`), and the scaled
  budget exists only for **≥2 frozen milestones** (`SetRunRunning` budget CASE). So
  the enforcement naturally scopes to exactly the runs that have a tracker to move.
- **Test surface is offline.** The signal/loop machinery is provable with the
  scripted `queryFn` fake (no live token, no network) — `signals.ts`'s own header
  and the existing `agent/test/signals.test.ts` / `sdk-executor.test.ts` /
  `prompt.test.ts` establish the pattern.

## Decisions

**D1 — Enforce in the executor loop (agent-side), not by new server gating.** The
executor owns the implement/review loop and the per-turn prompt; it can require a
declaration and escalate with no new server round-trip. The API stays the
authoritative validator/unioner (`progressParams` subset-checks against the frozen
list) and is unchanged except for the display audit in D5.

**D2 — Mechanism = required declaration + per-turn escalation + observability, NOT
mandatory checkpoint.** This preserves PRD #122 M6's decision that `checkpoint` is a
*durability* boundary, not a gate. Coupling progress to a mandatory checkpoint would
reverse that decision and could burn iterations. *(Alternative considered and
rejected: make `checkpoint` mandatory at each boundary and carry a `milestone_id`.
Heavier, reverses a shipped decision, and this run never checkpointed anyway.)*

**D3 — An all-empty `report_progress` is NO SIGNAL.** When both `completed` and
`in_progress` parse to empty, `scanSignals` does **not** set `out.progress`; so it
never overwrites `latestProgress`, neither running-report writer (`reportIteration`
or the checkpoint closure) sends the fields, and the column stays `null`. This fixes
the misleading `M0/N` **at its source** and stops a no-op call from masking a
non-reporting lead. As a bonus it also fixes the *intra-turn* case: today's
last-wins merge (`scanSignals` L1804) lets an all-empty call *after* a real one in
the same turn wipe the real snapshot; post-M1 the empty call is not a signal, so it
cannot. *(Edge: this narrows the ability to explicitly clear `in_progress` — see
Risks R7. A call carrying any real completed id is kept as today, and the truly-lost
case is only "clear in_progress when nothing is completed yet.")* **Note M1 inverts a
currently-pinned invariant** (`agent/test/signals.test.ts` ~L459-469 asserts "a
completely empty call still sets progress with two empty arrays"), so M1 rewrites
that test rather than only adding coverage.

**D4 — Never fail a run for a missing report.** Progress is informational (PRD #122);
enforcement escalates and surfaces, but a run that finishes its work still finishes.
The re-ask is bounded (escalate the *next* turn's prompt — no dedicated wasted turn,
no loop), and the visibility signal is a `status`/health reason, not a failure.

**D5 — Display honesty (scope add).** A run that genuinely reported nothing renders
neutral `M–/N`, not `M0/N`, everywhere: web badge, run view, runs list, CLI. The two
halves differ: the **web** already routes through `milestoneBadgeText` (neutral-aware
since #265 M2), so with D3 keeping the column `null` it renders correctly and only
needs **tests** at the three sites. The **CLI** (`api/cmd/uzi/run.go` L1340-1369) has
**no** null-vs-`[]` branch — it prints `%d/%d reported complete` unconditionally — so
it needs an actual **code change** to render the neutral state, not just a test. M4's
job is therefore (a) add the CLI neutral branch, (b) audit that no write path persists
`[]` on a plain heartbeat (both running-report writers, see Problem), (c) prove the
column stays `null` for a never-reported run with a live-DB regression test, (d) guard
the three web sites with tests.

**D6 — Scope: three distinct gates, do not conflate them.** The tools, the tracker,
and the budget gate on *different* conditions, so the enforcement is scoped per
concern rather than to one milestone count:
- **Tool exposure** (`report_progress`/`checkpoint`): `isIssueRun` only (any milestone
  count, including 0) — `sdk-executor.ts` L607-610.
- **Tracker render + prompt note + this PRD's honesty/escalation** (M1/M3/M4/D5): fire
  on **≥1 frozen milestone** (`milestoneStatusNote`/`MilestoneChecklist` guard only on
  `milestones.length === 0`). A **1-milestone** frozen run *is* reachable
  (`validateMilestones` accepts len 1), renders `M0/1`, and is exactly where the `M0/N`
  bug bites — so it is **in scope**, not excluded.
- **Budget scaling**: **≥2 frozen milestones** — unrelated to tracker honesty; do not
  anchor scope to it.
So only **0-milestone issue runs and every non-issue run** are byte-for-byte
unchanged (no frozen list → `milestoneStatusNote` returns `""` → nothing to key on).
An earlier draft said "≥2 milestones only / 0-1 unchanged"; that was wrong about the
1-milestone case and is corrected here.

**D7 — Complementary to #265, not a replacement.** `signal_done.milestones_completed`
still reconciles at completion (PRD #265 M1); this PRD makes the **mid-run** tracker
truthful too, so a run is honest throughout, not only at the end. "Declared, not
inferred" is preserved — we require the lead to declare and make silence loud; we do
not infer completion.

**D8 — No new CLI verb.** `uzi run get`/`list` already carry the milestone fields;
D5 only brings the CLI render (`api/cmd/uzi/run.go`) to display parity for the neutral
state. Recorded to satisfy the "new functionality ⇒ check `api/cmd/uzi/`" convention.

## Milestones

Dependency graph: **M1 ⟂ M2** (independent) **→ M3**; **M1 → M4**; **{M3, M4} → M5**.
M1 and M2 can run in parallel; M3 (executor) needs M1's no-signal semantics and M2's
escalation slot; M4 (display) needs M1; M5 is the cross-cutting close.

- [ ] **M1 — All-empty `report_progress` is no signal (agent, D3).** In
  `agent/src/signals.ts` `scanSignals`, only set `out.progress` when at least one
  side has ≥1 id after parsing; an all-empty/defaulted call yields no `progress`.
  Keep last-wins for real reports. Update `agent/test/signals.test.ts`. **Success:**
  a `report_progress` with both sides empty scans to `{}` (no progress); one with a
  real id still yields it; `task gate:agent` green. Offline (scripted tool_use).

- [ ] **M2 — Prompt: require, don't suggest + escalation slot (agent).** In
  `agent/src/prompt.ts`, change `milestoneStatusNote` from `MAY` to a required
  per-turn declaration ("at the start of each implement turn, call `report_progress`
  with the milestone id you are working on; call it again when one completes"), and
  add an optional escalation line rendered when a new `ImplementPromptInput` flag
  (e.g. `progressMissedLastTurn`) is set. A no-milestone / non-issue run renders
  byte-for-byte as today. Update `agent/test/prompt.test.ts`. **Success:** the note
  carries the required wording; with the flag set it renders the escalation; the
  comment-less/no-milestone path is unchanged.

- [ ] **M3 — Executor enforcement + observability (agent, D2/D4).** In
  `agent/src/sdk-executor.ts`'s implement/review loop, key enforcement on **the state
  of the tracker, not on "did this turn call the tool"** — escalate when, after a work
  turn, **no milestone is marked in progress** (`latestProgress?.in_progress` empty)
  while work is ongoing. This deliberately does NOT nag a lead that declared a
  milestone in progress and is spending several turns on it (it stays in progress, no
  re-report needed), which is what keeps a compliant lead from being burned (SC4).
  When the trigger fires, set M2's escalation flag on the next `buildImplementPrompt`;
  after **K=2** consecutive triggering turns, emit a visible `status` message naming
  that the lead is not reporting milestones (**feed-only via `ctx.emit`; NOT a health
  reason** — the worker has no path to write `runs.health_reason`, which is
  server-only, and D1 forecloses a new API field). **Exclude non-work turns from the
  count**: the checkpoint `continue` (L1300-1303) and the question-park `continue`
  (`iteration--`, L1342-1349) are not misses — a checkpoint counts as progress
  evidence and a park makes no progress by definition. Bounded, never fails the run
  (D4). Update `agent/test/sdk-executor.test.ts` with a scripted `queryFn`.
  **Success:** a scripted run whose lead never marks any milestone in progress shows
  escalation from the next turn and a `status` visibility signal after K=2 triggers; a
  run that declares a milestone in progress (even one spanning several turns) shows
  neither; a checkpoint turn and a park turn do not trigger; the run still completes.

- [ ] **M4 — Display honesty end-to-end (api + web + cli, D5).** Add the missing
  null-vs-`[]` branch to the **CLI** (`api/cmd/uzi/run.go` L1340-1369) so a `null`
  tracker prints a neutral `–/N` ("not reported") instead of `0/N` (this is a code
  change, not just a test — the CLI has no such branch today). Add a live-DB
  regression test proving the column stays `null` for a never-reported milestone run —
  which asserts the **server contract** (`SetRunRunning`: a NULL progress param leaves
  the column untouched), the complement to M1's agent-side no-signal proof. Audit that
  **neither** running-report writer (`reportIteration`, the checkpoint closure)
  persists `'[]'` post-M1. Guard the three neutral-aware **web** sites
  (`web/src/lib/runBadge.ts`, `RunView.tsx`, `RunsList.tsx`) with tests. **Success:** a
  running milestone run with no real report shows `M–/N` in web **and** the CLI shows
  `–/N`; a genuinely-reported 0-complete run still shows `M0/N` / `0/N`; `task gate:api`
  + `task gate:web` green.

- [ ] **M5 — Cross-cutting tests + docs.** Coverage beyond the per-milestone tests;
  update `ARCHITECTURE.md` "Milestone tracker reconciliation" (L667+) to state that
  mid-run reporting is now enforced (required + escalated + surfaced) and that an
  all-empty report is a no-op, and refresh any user-facing docs page that describes
  run progress. Record the PRD #122 → #265 → #390 lineage. **Success:** `task gate:agent`
  + `gate:api` + `gate:web` + the docs check (`web/scripts/check-docs.mjs`) all green;
  the ARCHITECTURE + docs edits land in this PRD's MR(s).

## Success Criteria

1. On a milestone-bearing issue run (≥1 frozen milestone) where the tracker shows no
   milestone in progress, the lead is **re-prompted with an escalating directive** and
   repeated silence becomes a **visible `status` signal** on the run. (This is the
   guaranteed outcome: enforcement is soft — see Out of scope — so it does not
   *guarantee* the lead marks real progress, but non-reporting is no longer silent, and
   a working run no longer displays as `M0/N`.)
2. An all-empty `report_progress` call **never** persists `[]` and never masks a
   non-reporting lead (D3), proven by test.
3. A run that genuinely reported nothing renders **`M–/N`** (neutral) in web and CLI,
   distinct from a genuine `M0/N`; the deviation observed on run #381 cannot recur.
4. A lead that declares a milestone in progress (even one spanning several turns) is
   **not** nagged, and every **0-milestone issue run and non-issue run** behaves
   byte-for-byte as today (no regression, no wasted turns, no run failed for a missing
   report — D4/D6). *(A 1-milestone run is in scope per D6 and gets the tracker-honesty
   behaviour.)*
5. PRD #122's decisions are preserved: `checkpoint` stays a durability boundary,
   `report_progress` stays non-run-failing, completion stays **declared, not inferred**.
6. `task gate:agent` + `gate:api` + `gate:web` + the docs check are green.

## Out of scope / non-goals

- **A hard mid-turn gate.** The single-session SDK loop cannot interrupt the lead
  mid-turn, so enforcement is "required + escalating + visible", not a mechanism that
  blocks work until a report arrives. Recorded as a constraint, not a gap.
- **Making `checkpoint` mandatory / carrying a `milestone_id` on it** (D2's rejected
  alternative).
- **Inferring progress from commits, subagent dispatches, or iteration count** —
  contradicts "declared, not inferred" (D7).
- **New CLI verbs or a new server route** (D1/D8).
- **The completion-time reconciliation** (#265 M1) — unchanged and relied upon.
- **Enforcement on the resume and seeded paths.** `frozenMilestones` is assigned only
  in the plan-and-gate block and stays `undefined` on a pre-approved/resumed run
  (`sdk-executor.ts` L449) and on seeded runs (which have no milestones, #261). With no
  frozen list, `milestoneStatusNote` returns `""` and M3 has nothing to key on, so a
  run that crashes/parks and resumes loses enforcement for the resumed session. This is
  a pre-existing PRD #122 limitation M3 inherits; called out here rather than fixed.
  Completion reconciliation (#265 M1) still applies on the resumed run's `signal_done`.

## Risks & mitigations

- **Budget waste from re-asking** — mitigated by D4: escalate the *next* turn's prompt
  (no dedicated round-trip), never loop, never fail the run.
- **The model games the requirement** (calls `report_progress` with empty/garbage to
  satisfy the wording) — mitigated by M1 (an all-empty call is no signal, so it does
  not count as "reported") and M3's after-K-misses visibility signal, which surfaces
  persistent non-compliance even if the model keeps calling the tool emptily.
- **R7 — D3 narrows the ability to explicitly clear `in_progress` to empty** —
  accepted, and narrower than it first looks: a lead can still clear `in_progress` by
  re-reporting any already-completed id (`{completed:["m1"], in_progress:[]}` is not
  all-empty → still a signal → `in_progress` overwritten to `[]`). The truly-lost case
  is only "clear `in_progress` when nothing is completed yet," which carries no positive
  information. If ever needed, a sentinel could be added later; out of scope now.
- **R4 — nagging a compliant lead** — mitigated by M3 keying on **"no milestone marked
  in progress"** rather than "no `report_progress` call this turn": a lead that declared
  a milestone in progress and spends several turns on it stays in progress and is not
  re-nagged, so it does not have to re-emit `in_progress` every turn (which would burn
  tokens and contradict SC4). Checkpoint and park turns are excluded from the count.
- **Over-tightening the prompt regresses good runs** — mitigated by M2 keeping the
  0-milestone / non-issue path byte-for-byte unchanged and by Success Criterion 4's
  no-regression assertion.

## Decision Log

- **2026-08-19 — created.** Origin: investigating run `64a7f916` (issue #381), which
  showed 0/6 milestones despite doing real work; root-caused to (a) `report_progress`
  being optional and skipped, and (b) an all-empty report persisting `[]` and
  defeating the documented `M–/N` neutral render. Scope set to the "strong" behavioral
  fix **plus** the display-honesty fix at the user's direction. Design chose
  executor-side enforcement (D1/D2) over mandatory checkpoint or server gating,
  preserving PRD #122's durability-vs-gate decisions and PRD #265's "declared, not
  inferred" principle. All citations offline-verified against HEAD.
- **2026-08-19 — revised after two parallel reviews** (scope/feasibility + adversarial
  citations/risks), both of which confirmed every file:line citation and the causal
  chain against HEAD. Changes: **M3's visibility signal is a feed-only `status` emit,
  not a health reason** (the worker cannot write `runs.health_reason`; a health field
  would need a new API round-trip D1 forbids); **M3's trigger re-keyed to "no milestone
  marked in progress" instead of "no report this turn"**, and checkpoint/park turns
  excluded, so a lead mid-way through a multi-turn milestone is not nagged (R4);
  **D6 corrected** — the three gates (tool exposure = `isIssueRun`; tracker/honesty =
  ≥1 frozen milestone; budget = ≥2) were conflated, and a **1-milestone run is in
  scope**, not excluded; **the second `[]` writer (checkpoint closure, `runner.ts`
  L943-956) named** in Problem/Resolved facts/M4 audit; **D5/M4 corrected** — the CLI
  needs a real neutral-branch code change (web only needs tests), and the live-DB test
  proves the *server* contract, not D3; **SC1 restated** to claim only the guaranteed
  soft-enforcement outcomes; **K defaulted to 2**; the **resume/seeded enforcement gap**
  (R3) added to Out of scope; the D3 clear-`in_progress` note narrowed (R7); noted M1
  inverts a currently-pinned `signals.test.ts` invariant.

## Notes for an offline (uzi) worker

Everything this PRD needs is **in-repo** — the executor loop, the signal scanner, the
prompt builder, the `progressParams`/`SetRunRunning` write path, the read DTO, and the
four render sites are all cited above with file:line. No milestone depends on the open
web, and every test is provable with the scripted `queryFn` fake (no live Anthropic
token, no network) plus the existing live-DB harness for M4. Any goose migration is
not needed (no schema change); the column `runs.milestones_completed` already exists
and is nullable by design (migration `00099`).
