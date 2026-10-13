# PRD #2789: Run page, one progress card for milestones, live work and Now

**Status**: Done — M1–M4 implemented and validated
**Priority**: Medium
**GitHub issue**: #2789
**Design mock**: `prds/mockups/2789-run-page-unified-progress-mock.html` (open it in a browser; a Before/After toggle and a run-state switch sit at the top)

## Design mock contract

The mock is the presentation reference: layout, order, glyphs, colour roles and wording. Implement it, not only this text.

- Small deviations for consistency with the app are expected: its palette tokens, components, spacing and wording.
- A larger deviation (a dropped element, a different layout, different information) only when necessary, listed in the PR body under `Mock deviations` with its reason. An absent list claims parity.
- Validation compares the result with the mock side by side (mock-mode screenshots, light and dark) and the PR carries that evidence.
- Precedence: this PRD's text wins where it specifies; the mock governs what the text leaves out.
- The mock's failed and completed hero copy, and its Now sentence, are example content. The heroes keep their current content; the Now sentence is model-written.

## Prototype branch

Before dispatch, a throwaway web-only prototype of this design is built in mock mode (no tests) and reviewed by the maintainer in a browser. Once signed off it is pushed as `agent/issue-2789`; an issue run starts from `origin/agent/issue-<N>` when that branch exists, otherwise from `main`.

- The prototype is a **presentation reference, not accepted code**. The run treats every line of it as unreviewed: keep what meets this PRD, rewrite what does not. Its sign-off approves presentation only and completes no milestone.
- **Before dispatch (maintainer):** `git diff --name-only origin/main...HEAD` on the prototype lists only `web/` paths; record the pushed prototype SHA; create the run gated (`auto_approve` off) and confirm the worker seeded from that SHA.
- **Plan gate:** the plan lists every prototype-touched file with keep or rewrite and the reason, the shortcuts to replace, the required tests and all four milestones. A plan that treats the prototype as done is rejected.
- **Integration:** the run first merges `main` into the branch (never a rebase: the branch is already published). Conflicts resolve toward `main` plus this PRD, never toward the prototype.
- **Review scope:** validators, the auditor, the tester and the final review read the whole branch, `git diff $(git merge-base origin/main HEAD)..HEAD`, which includes the prototype commits, never only the commits this run added. The PR names the reviewed base and head and carries that coverage evidence; missing coverage blocks completion.
- **Tests:** every test in Testing decisions is required whatever the prototype contains. Because kept code predates its tests, each test pinning an invariant (sanitising, honesty wording, owner/busy/terminal gates, count source, collapse precedence) is shown failing once against a deliberate break of the kept code, then passing.
- **Milestone honesty:** a milestone is reported complete only with its own tests and gates green on this run; code that already exists from the prototype completes nothing.
- **No mock shortcuts carried over:** shipping components read only `Run`/DTO fields; no `VITE_UZI_MOCK` or `mockScenario()` branching outside `web/src/mocks/`; prototype mock data moves into the named scenario (M1) or is deleted.
- The prototype touches only `web/`. It never touches `.github/workflows/**`, and neither does the run, in implementation or validation (the worker PAT lacks `workflow` scope; a workflow file in the branch diff rejects the whole push).
- Where the prototype and this PRD disagree, this PRD wins; the PR lists any prototype behaviour it dropped.

## Problem

An owner opening a running issue run asks four things: is it healthy, what is it doing, how far along is it, and what has it cost. Today the top of the run page answers each in several places and in conflicting terms:

- **The milestone list renders twice.** `RunProgressCard` (`web/src/components/RunProgressCard.tsx`) labels each track segment with its milestone title; three cards further down, `MilestoneChecklist` (`web/src/pages/RunView.tsx:501`, rendered at `RunView.tsx:3193`) lists the same milestones again with marks and lanes.
- **Three numbering schemes disagree.** The header `MilestoneBadge` (`RunView.tsx:2700`) reads `M3/4` (done count), the card reads `milestone 4 of 4` (active ordinal, `RunProgressCard.tsx:212`), and the checklist reads `3/4 ◐ m9`.
- **"What is it doing" appears five ways**: the header stage chip, the card's Now line, the card's single Active role line, the checklist's per-milestone lane strips, and the Activity agent chips. Active role names one agent while the checklist can show several lanes on one milestone.
- **The phase stepper reads as a sequence** (`review validate ▸ implement`) although only the current phase is known, and needs a caption to say so.
- **The header carries up to eleven chips**, the token up to three times (`RunCredential`, `RunCredentialOverride`, a one-row Token history), and the run toggles sit between progress and the summary.
- **The action you came for is buried.** `PlanPanel` (`RunView.tsx:3119`) renders after the run summary, recovery archives and judge; Pause and Stop (`SteerQueueCard`, `RunView.tsx:3247`) sit below an Activity feed that can hold 1,000+ messages. The completed/failed/stopped heroes (`RunView.tsx:2960+`) render after the run summary.
- **Cost sits far down** (`RunUsagePanel`, `RunView.tsx:3219`).
- **The Now note often restates the tool.** The summarizer prompt (`agent/src/now-summary.ts:78-84`) asks for "what the run is doing right now, using only what the tool calls show". A Bash frame carries only the call's optional description, never the command (`trimToolFrame`, `now-summary.ts:135-136`); when the description is absent, and on Codex lanes that often carry role `unknown` and no label, the model sees little beyond the tool name and writes "Running a bash command."

## Outcome

The top of the run page reads in one pass: a header with identity, state and controls; then whatever needs the owner; then one progress card holding the percent, the plan track, every milestone once, the live work and Now note on the active milestone, and a cost line.

Acceptance examples:

1. **Running run, owner plus a second live lane.** A run at 3 of 4 whose m9 is attributed to documenter (live) with spec-keeper also live on m9: the page has one milestone list (no second checklist card); the head reads `≈77% · 3 of 4 reported complete`; the m9 row expands to show `phase: implement · iteration 4`, the Now note with its age, and the lane strips (one per live lane, the declared owner's strip deduped when it is live, a declared owner with no attributed live lane as the quiet `Assigned owner:` strip). The header shows no `M3/4`, no stage chip and no iteration chip.
2. **Awaiting the initial plan approval (no frozen milestones yet).** The plan panel renders directly under the header, before the progress card. The card shows the existing `waits on you · plan gate` flag with no percent and no milestone list (milestones freeze only at approve).
3. **Completed.** The completed hero renders in the attention slot, before the progress card; the card reads `4 of 4 reported complete` with every row `✓`, no lanes and no Now note; the cost line shows the final cost in the same words the usage panel uses.
4. **Not reported.** A running run whose worker reported nothing keeps the server percent and reads `≈11% · – of 4 reported`, never `0 of 4`.
5. **Now note wording.** For a window of frames with no label and no description (the Codex shape), the captured summarizer prompt directs the model to describe the work from the milestone title, not to name the tool; for a labelled window it uses the label as intended work, never as an outcome.

## Out of scope

- TUI and `uzi run get` layout. Neither renders the run page; `api/cmd/uzi/` changes are comment-only (comments that name `MilestoneChecklist`/`MilestoneBadge` as twins, item in M1). Both display the shared Now note, so M4's wording change reaches them too. The CLI keeps `milestone k of M` in its `PROGRESS` row; that wording divergence from the web is accepted (D5).
- Any API, DTO, schema or migration change. The server percent rule (`runprogress`) is unchanged.
- The Activity feed, Steer composer, judge, recovery archives, roster and full usage tables: they stay below, unchanged except for their order relative to the attention slot.
- New hero or plan-panel content: M3 moves existing panels; it does not redesign them.
- Board, runs list and dashboard (they use `milestoneBadgeText`, not `MilestoneBadge`).

## Modules and seams

- **Milestone rows component (new export, moved code).** `MilestoneChecklist`'s row and lane body (`RunView.tsx:501` onward, with `MilestoneNowStrip` and the attribution logic) moves intact into one exported component that takes `run` and `activity` and returns the rows. Its interface is the checklist's today, so the existing tests change only their import and wrapper, not their assertions.
  - Preserves PRD #1224 (effective attribution, D3 unique-role live tool and age, repeated-role suppression) and PRD #1353 (quiet "assigned" strip for a declared owner with no attributed live lane, owner-lane dedup, a lane pulses only with a live frame).
  - The quiet strip for a declared owner with no attributed live lane reads `Assigned owner: <role> · <task>`. It never says idle, inactive or "no activity": a missing live lane can also mean the activity could not be uniquely attributed (repeated-role suppression). If more is needed, the wording is "No live activity attributed".
  - Row marks and lanes read `milestones_completed`, `milestones_in_progress`, `milestones_agents` and `milestones_live`, as today.
- **`RunProgressCard`** becomes the single progress surface and renders the rows component.
  - Renders its milestone section whenever `run.milestones` is non-empty, whatever `progress` is (null on a terminal or pre-feature run, `none`, or an unknown state during a rolling deploy); the head then shows only the count. `CardRun` widens to carry `milestones_in_progress`, `milestones_agents`, `milestones_live` and `iteration_count`.
  - Count and the reported flag come from `milestoneBadge(run)` (`web/src/lib/runBadge.ts`), pinned equal to the server by the PRD #2602 D17 parity fixture. `progress` supplies only pct, state, phase, `active_milestone_id` and `now_note`.
  - The Now note attaches under the row whose id equals `progress.active_milestone_id`: the api shows a note only for exactly that id (`overlayNowNote`, `api/internal/handler/now_summary.go:43-77`, PRD #2603 D5). If no row matches, it renders unattached under the head.
  - **Doing-what fallback**: the rows component already draws its own fallback strips (unattached under its head when nothing is in progress; under the first in-progress row when nothing is attributed). The card's own activity line (role · label, or tool, through `stripUnsafeChars`) renders only when the rows component renders nothing (no frozen milestones), so the Active role's information is never lost and never drawn twice. The iteration count shows on the active row, or in the head when no row is active.
  - **Run stage**: the header's stage (`stageForMessages`, `RunView.tsx:124`: Starting up, Thinking, Writing, Planning, tool stages) describes the whole run feed, not a milestone, so the card shows it labelled `run: <stage>` after the phase chip on the active row, or in the head when no row is active. The header keeps its stage chip for any run whose card does not render the stage (chat, job and other runs without a card), so no run loses its live doing-what signal.
  - **Possible dependency**: the head keeps today's `may be blocked by <run>` link (`progress.maybe_blocked_by_run_id`, `RunProgressCard.tsx:237-274`), through `stripUnsafeChars`, worded as a hint.
  - Invariants: milestone ids, titles, lane text, activity and Now text stay untrusted plain text through `stripUnsafeChars`, never Markdown; "reported complete", never "done" or "verified"; the track keeps its accessible text equivalent when its segment labels are dropped.
- **Run controls component (extracted).** `PauseMenu` plus Stop, the Pause visibility predicate (running, pausable kind, no pause pending) and the canSteer/busy/terminal gates move out of `SteerQueueCard` into one component rendered in the header only; no second predicate. Job runs keep "Stop this job". Stop's only confirmation is the outcome-pending modal from `cancelRun`. The bottom card keeps its composer and queue, loses its Pause and Stop, and shows a one-line pointer to the header controls (D8).
- **Header (`PageHeader`, `web/src/components/ui.tsx:674`)** uses its `actions` slot (unused by RunView today) for `Run settings ▾` and the run controls.
- **Run settings disclosure**: Switch token, Token history (`CredentialEpochList`, every recorded epoch, a single pin and its time included), the non-parked `LimitWaitPanel` toggle and the `MrReworkPanel` row. A keyboard disclosure (`aria-expanded`; focus returns to the trigger on close). The trigger stays a compact `Run settings ▾` so the controls keep the title line at desktop width; the effective toggle states show as plain neutral chips in the header status row (`waits on limits`, `auto-rework off`), each only when it applies. The parked `LimitWaitPanel` stays an attention panel; the ordering comment at `RunView.tsx:2803-2822` is updated because its "the strip below renders its non-parked toggle" rationale moves.
- **Attention slot**, directly under the header, in this order (a deliberate new order, D13; today's order at HEAD is outcome_pending, pool/recovery/credential/paused/near-timeout/limit panels, heroes, then PlanPanel, QuestionPanel and the completion panels): error and revision alerts; PlanPanel; QuestionPanel/UnreadableQuestion; outcome_pending; CompletionDecisionPanel; CredentialDisabledPanel; PausedPanel; NearTimeoutPanel; PoolWaitPanel, RecoveryWaitPanel, parked LimitWaitPanel (kept adjacent and in that order); CompletionStatePanel; the completed/failed/stopped heroes. `IssueInputHistory` stays directly after the slot.
- **Cost footer**: reuses `costDisplay`, `costHeadline`, `costSubLabel` (`web/src/lib/costStatus.ts`), `cacheDisplayPct` (`web/src/lib/runUsage.ts`) and the same `usage` derivation `RunUsagePanel` reads; no second cost computation. `""` and `unreported` read as cost unavailable; `subscription` and API-equivalent labelling read as the panel words them; live-only usage before a result and side-pass spend follow the panel; an `unpriced` tail reads `+ unpriced tail`, never `+~$0`. The "full usage" link renders only when the usage panel renders.
- **Run summary collapse**: `summaryCollapse.getCollapsed` (`web/src/lib/prefs.ts:71-75`) returns `false` for both "absent" and "explicitly expanded" today. It gains an absent result (`undefined`); the state default (collapsed past the plan gate, expanded at awaiting_approval) applies only when absent, computed at mount and again on the gate exit. The collapsed form shows the title and a plain-text intent excerpt (Markdown stripped, through `stripUnsafeChars`).
- **Now summarizer prompt (`agent/src/now-summary.ts`)**: `SYSTEM_PROMPT` and `buildPrompt` only. The seam (`runReadOnlyModelPass`), triggers, caps, fencing, redaction, gating and usage accounting are unchanged on both harnesses.

## Testing decisions

- **Moved tests keep their assertions**: `RunView.test.tsx`, `RunView.attribution.test.tsx`, `RunView.nowline.test.tsx` and `RunView.baseline.test.tsx` (the #1224 M4 baseline guard) change only import and wrapper. The `describe("MilestoneBadge …")` block is deleted with the component. Existing `RunProgressCard.test.tsx` cases for segment labels, `milestone k of M`, the stepper caption and Active role are repointed; negative assertions on retired copy are repointed per `.claude/rules/web.md`, never left vacuous.
- **New web cases**: one milestone list in the DOM; the count wording, including not reported (`≈11% · – of 4 reported`); Now under the `active_milestone_id` row and unattached when no row matches; owner plus live lanes and the quiet `Assigned owner:` strip; the card's milestone section for progress null, `none` and an unknown state; the doing-what fallback with no milestones, pre-feature, stalled and parked runs; exactly one activity line when nothing is attributed; iteration on the row and in the head; `run: <stage>` on the active row and in the head; the `may be blocked by` link kept and sanitised.
- **Header and controls**: no `MilestoneBadge` or iteration chip; the stage chip absent when the card shows the stage and present for a run without a card (chat, job); header Pause/Stop obey the same gates and predicate (follow `RunView.actions.test.tsx`, `SteerQueueCard.test.tsx`); the Steer card renders no Pause or Stop; disclosure keyboard and focus; a single-epoch token history visible in the disclosure; the toggle-state chips in the status row, absent at defaults.
- **Order**: awaiting_approval renders PlanPanel before the card; completed renders the hero before it (`compareDocumentPosition`).
- **Cost footer**: each `cost_status` (`metered`, `subscription`, `unreported`, `""`), live-only usage, side-pass spend, estimated and unpriced tails, no link when the panel is absent.
- **Collapse**: missing and expired preference, explicit true and false, and the awaiting_approval to running transition without remount.
- **Spinner**: shared by PlanPanel, JudgePanel and others; under `prefers-reduced-motion: reduce` it renders a static frame and starts no interval. jsdom has no `matchMedia`, so the test stubs it.
- **Agent (M4)**: `now-summary.test.ts` asserts through the fake harness's captured `opts.systemPrompt` and prompt on both the Claude and Codex controller paths (`SYSTEM_PROMPT` is not exported): label and title framed as intended work, never an outcome; no prediction of next steps; a leading `[mN]` tag ignored; tool named only when nothing else is known; fencing, redaction, 120-character cap and no-command disclosure unchanged. A prompt assertion proves the instruction, not model output; live wording is a maintainer spot check, not a merge gate.
- **Mock parity**: mock-mode screenshots of running, awaiting approval, failed and completed, light and dark, beside the mock, in the PR.
- **Gates**: `task gate:web`, `task gate:agent` (M4), `task gate:repo` (including `check:changelog-entry`), plus the separate `task check-docs:web` for PRD/mock links in the primary checkout.

## Milestones

### M1: One progress card holds the milestones, live lanes and Now
Blocked by: none

- [x] Extract the milestone rows component; `RunProgressCard` renders it; remove the standalone checklist render at `RunView.tsx:3193` and the `MilestoneChecklist` wrapper if nothing else uses it (`deadcode:web` and knip stay green).
- [x] Head: `≈N% · k of M reported complete` (not-reported form per example 4); the stepper and its caption become `phase: <phase> · iteration <n>` on the active row; Now under the active row; Active role line removed; doing-what fallback kept.
- [x] Track segment labels dropped, accessible text equivalent kept.
- [x] Delete `MilestoneBadge` (only RunView's header uses it) and update the comments that name it or `MilestoneChecklist` as twins: `web/src/lib/runBadge.ts`, `web/src/mocks/store.ts`, `api/cmd/uzi/run_render.go`, `tui_board_rows.go`, `tui_model_test.go`, `tui_detail_rail.go` (comment-only).
- [x] Mock data: add or extend a named mock scenario carrying `milestones_agents` and `milestones_live` (owner plus a second live lane, a declared owner with no attributed live lane), a `now_note`, iteration > 1 and a usage tail; list it in `docs/dev-conventions.md` (Demo scenarios).
- [x] Docs: `docs/run-activity.md` "Milestones and the now line" and the run-page bullet of "Progress estimate" (about lines 401-440 and 504-515); `task docs:sync`, mirror committed. `CHANGELOG.md` `[Unreleased]` line.
- [x] Acceptance: examples 1 and 4; mock parity for the progress card.

Evidence: [M1 validation, assertion migrations and deliberate breaks](../mockups/2789-m1-validation.md), with light/dark browser comparisons. Source reviewed at `bffcbb77`; web gate passed at `b05cc1b7` (identical source), repository gate and production build passed at `60e5ccc1`. M2–M4 remained pending at that M1 checkpoint.

### M2: Header shows identity and state; settings and controls move up
Blocked by: M1 (the iteration count moves into the card first)

- [x] Header chips: status, health flag, live elapsed/budget (parked: clock stopped), harness and model, one token chip; both token chips only while a switch is pending or released (`RunCredentialOverride.tsx:63-72`). Iteration chip removed; the stage chip removed only where the card shows the stage (kept for runs without a card).
- [x] `Run settings ▾` disclosure in the header actions slot (contents per Modules); the inline Token history card and toggle rows leave the page body; the `RunView.tsx:2803-2822` comment updated.
- [x] Run controls component extracted and rendered in the header actions slot only; the Steer card keeps composer and queue, drops its Pause and Stop, and points to the header.
- [x] `Spinner` (`ui.tsx:590`) honours `prefers-reduced-motion`.
- [x] Motion: this PRD adds no new pulsing element; a lane pulses only with a live frame (PRD #1353); every pulse stays neutralised under `prefers-reduced-motion` (`web/src/index.css`).
- [x] `CHANGELOG.md` line.
- [x] Acceptance: example 1's header clauses; mock parity for the header.

Evidence: [M2 validation, old → new assertion inventory and deliberate breaks](../mockups/2789-m2-validation.md), with light/dark header and mobile disclosure comparisons. Cumulative source review is clean through `e5d39e35`; full web/repository gates and production build passed at that revision. At that M2 snapshot, M3–M4 remained pending.

### M3: What needs you comes first, cost in the card, summary collapses after the gate
Blocked by: M1 (the cost footer lives in the card)

- [x] Attention slot directly under the header with the members and order listed in Modules, moved with their existing content and gates.
- [x] Cost footer per Modules.
- [x] `summaryCollapse` tri-state read and the state default per Modules.
- [x] `CHANGELOG.md` line.
- [x] Acceptance: examples 2 and 3; mock parity for the attention slot and footer.

Evidence: [M3 validation and isolated deliberate breaks](../mockups/2789-m3-validation.md), with [light](../mockups/2789-run-page-parity-light.png) and [dark](../mockups/2789-run-page-parity-dark.png) four-state Chromium comparisons. Source review is clean at `f2083e52`; full web gate, production build and uncached docs mirror passed. Thirteen valid deliberate breaks typechecked and failed intended assertions, followed by 72 unmodified passing cases. Final repository gate passed at `df1b5d28`; reviewer, tester and auditor verified the committed proof and screenshots with no mandatory findings. A fresh JSON-report run matched all 29 recorded failing test names to passing results among 72 tests. M4 evidence follows below.

### M4: The Now note describes the work, not the tool
Blocked by: none

- [x] `SYSTEM_PROMPT` in `agent/src/now-summary.ts`: describe the work from the milestone title and the frames' role and label; treat the label and title as intended work, never an outcome; never predict next steps; ignore a leading `[mN]` tag; name a tool only when nothing else is known. PRD #2603 D6 (no inferred success), the single sentence and the 120-character cap stay.
- [x] Both harnesses use the same prompt.
- [x] Docs: `docs/run-activity.md` "What the model sees" (about line 575), and `docs/admin-settings.md` if it quotes the old behaviour; `task docs:sync` when a `docs/*.md` changes. `CHANGELOG.md` line.
- [x] Acceptance: example 5.

Evidence: [M4 source validation and archived deliberate breaks](../mockups/2789-m4-validation.md). Source `9255f834` passed the full agent gate: 12,986 unit passes, 14 conditional skips, zero failures, plus 183 Codex conformance passes and the required clause matrix. All 79 Now cases executed. Twenty-three valid archived breaks typechecked and failed named intended assertions, followed by a fresh unmodified 79-case pass. Source reviewer/tester/auditor examined the unit and cumulative agent/docs scope; the stale demo-doc wording is corrected with its mirror. The repository gate passed at `5d20c1c1`; cumulative reviewer/tester/auditor proof checks passed. The separately detected PR-body link errors are fixed in the lifecycle integration and rechecked directly. Live model wording is a maintainer spot-check, not a merge criterion.

## Decision Log

| # | Decision | Reason | Rejected |
|---|---|---|---|
| D1 | One PRD for all four milestones | The user's choice (2026-10-12): one coherent page change, reviewed at one plan gate | Separate PRDs or issues per item |
| D2 | Extend `RunProgressCard`; move the checklist's row body intact into one exported rows component | Removes the duplicate without a third surface; the moved component keeps its interface, so the attribution tests keep their assertions and M1 fits one run | A new combined component; keeping both |
| D3 | The Now note attaches under the `active_milestone_id` row | The api shows a note only for exactly that id (`overlayNowNote`, PRD #2603 D5), so the attribution is server-truthed | A run-level line detached from the milestones |
| D4 | Row marks and lanes keep their current fields; the track's now segment and the Now note use `active_milestone_id` | Each field keeps the meaning it has today; the api provides no reconciliation and this PRD invents none | One field for everything, which would change what a mark claims |
| D5 | Web count wording `k of M reported complete`; the CLI keeps `milestone k of M` | Removes the web's `M3/4` vs `4 of 4` contradiction and keeps the PRD #265 honesty wording; the CLI is out of scope, divergence accepted | Changing the CLI here |
| D6 | Keep the server percent on a not-reported run (`≈11% · – of 4 reported`) | Hiding it would need a `runprogress` change and break PRD #2602 D3/D17 parity with the TUI and CLI | Hiding the percent on the web only |
| D7 | A single phase chip replaces the three-step stepper | The code knows only the current phase; a strip implies earlier phases passed | Keeping the stepper with its caption |
| D8 | One extracted run-controls component, rendered in the header only; the Steer card points to it (the user's decision, 2026-10-12) | One home for run-level actions, reachable on every state without scrolling a long feed, with one visibility predicate and one set of gates; no duplicate irreversible Stop | Controls in both the header and the Steer card (the first draft); a second copy of the Pause predicate |
| D9 | Existing panels move into an attention slot unchanged | The reason the owner opened the page comes first; redesigning those panels is separate work | Redesigning hero and plan panels here |
| D10 | `summaryCollapse` gains an absent state; the state default applies only when absent | The persisted choice must win, which today's two-state read cannot express. This reverses the `prefs.ts` "absent = expanded" default for runs past the gate | Keeping absent = expanded |
| D11 | Now-note fix is a prompt change only | The input already carries role, label and Bash description; no new data reaches the model, so PRD #2603's bounds hold | Sending more frame detail to the summarizer |
| D12 | A maintainer-approved web prototype seeds `agent/issue-2789`; the run hardens it | Lets the maintainer drive the design before the run spends its budget; the plan gate still reviews the run's plan | Implementing from the mock alone; `uzi handoff --base` (no plan gate, no issue link) |
| D13 | The attention slot uses a new, deliberate order: plan and question first, then outcome and completion decisions, then credential, pause, timeout and park panels, then the heroes | Panels that need a decision from the owner come before panels that only explain a hold; the Pool/Recovery-above-LimitWait pair keeps its order | Keeping HEAD's order, which puts PlanPanel below the heroes |
| D14 | The run stage moves into the card as `run: <stage>`, the header keeps it for runs without a card; the possible-dependency link, a single-epoch token history and the toggle states (as status chips, not trigger text, so the controls stay on the title line; the user's call after seeing the prototype) stay visible | An information-loss audit against today's page (2026-10-12, both reviewers agreed): each was shown today and would otherwise disappear; the stage is run-wide, so it is labelled `run:` rather than implying milestone attribution | Dropping the stage chip everywhere; hiding single-epoch history; toggle state visible only inside the disclosure |
