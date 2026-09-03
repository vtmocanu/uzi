# PRD #1064: Live milestone progress — immediate `report_progress` push, a blinking in-progress cell, and the "now" line on web, TUI and CLI

**Issue**: #1064
**Priority**: Medium
**Status**: complete (all milestones shipped on `agent/issue-1064`)
**Base**: `main` @ `7435327` (every `file:line` below was read at that commit; re-derive before citing)

## Status

Created 2026-09-03 from a live investigation of two running milestone runs (`9c672af9`,
issue #908; `c923693e`, issue #1061). Both had a coder lane editing files seconds earlier
and both reported `milestones_in_progress: []`. The mocks the user approved live in this
PRD as the target renderings (see *The target renderings*).

## Problem

A milestone-structured issue run (PRD #122) carries three live columns on `runs`:
`milestones_frozen` (the approved list), `milestones_completed` (monotone union) and
`milestones_in_progress` (an overwritten snapshot of the ids the lead says it is working on
now). All three surfaces already render the third one — the web `MilestoneChecklist`
(`web/src/pages/RunView.tsx:184-243`, `◐` via `MilestoneMark`), the TUI crew rail
(`api/cmd/uzi/tui_detail_rail.go:226`, `◐` in the wait colour) and `uzi run get`
(`api/cmd/uzi/run_render.go:290`, "in progress"). **None of them ever light up**, for a
reason that is in the worker, not the UI:

1. **`report_progress` is captured mid-turn but persisted only at turn boundaries.** The
   scan loop records it into the turn result, last-wins (`agent/src/sdk-executor.ts:2576-2579`),
   and the value reaches the api only through `ctx.reportIteration` at the top of the NEXT
   iteration (`:1725`) or through the checkpoint closure (`agent/src/runner.ts:2562`).
2. **The prompt tells the lead to do exactly the thing that makes the snapshot empty at the
   boundary.** `milestoneStatusNote` (`agent/src/prompt.ts:1297`, wording at `:1327`) says:
   at the start of the turn call `report_progress` with the id in `in_progress`; when done,
   call it again with the id in `completed`; then `checkpoint` and end the turn. So the
   turn's LAST snapshot has `in_progress: []`. For any milestone that fits in one turn —
   the normal shape, since `checkpoint` ends the turn per milestone — the api never sees it
   in progress at all. The checkpoint path then clears the latch anyway (`:1884`).
3. **PRD #390's "not marked in progress" nag cannot fire on this.** It checks the executor's
   in-memory `latestProgress` (`:2039`), which DOES hold the id; only the server is behind.
4. **Nothing on the feed marks a transition.** Signal tool frames are deliberately not
   emitted (`:2515`), so the feed has `implement/review iteration N` status lines and
   nothing that says "m2 started".

The consequence the user hit: a board of running runs where every milestone bar reads
`▰▱▱▱▱▱` and every checklist shows one ✓ and five ○, while a crew is visibly working — and
no surface says on WHAT. The lead and worker know both the declared milestone and the live
subagent dispatch; the human sees neither.

## Solution

Three pieces, each independently valuable, composed into one feature:

1. **Push progress the moment it is observed** (worker). When the scan loop sees a
   `report_progress` signal, send a `running` state report immediately, carrying the
   milestone fields and no `iteration_count` — the exact shape the checkpoint closure
   already sends mid-run. Emit a worker `status` frame per transition so the feed reads
   `milestone m2 started — <title>` / `milestone m1 reported complete — <title>`.
2. **A server-derived `current_activity`** on the run DTO: the run's newest `tool_use`
   frame folded into `{agent, agent_label, tool, detail, at}`. One rule, one Go package,
   consumed by the api (get + list), the TUI and the CLI; mirrored in TS for the web run
   view with a shared golden fixture so the two cannot drift.
3. **The "now" line** on every surface: under the in-progress milestone (or unattached
   under the header when nothing is declared), the active lane's role, its task label, its
   last tool and an age. On the TUI the parallelogram bar is kept and the in-progress cell
   BLINKS `▰⇄▱` in the wait colour on a 500 ms tick, with a static fallback frame for
   non-tty renders; the rail's in-progress row uses the same cell, so no circle/box mix.

### The target renderings (approved mocks, 2026-09-03)

Web run view, `MilestoneChecklist` (dark, ember tokens):

```
Milestones (reported complete)                       1/6  ◐ m2
 ✓ Thread per-schedule mr_rework toggle through self_improve          (muted, line-through)
 ◐ Decouple both detectors from agent/issue-N branch convention
   ┃ ● coder  Decouple ci_fix detector from branch naming · Edit api/internal/poller/ci_autofix.go   40s ago
 ○ Board-free scheduled MR-state recorder (SyncScheduledMRStates)
 ○ …
```

The now line is a left-bordered, info-tinted strip: pulsing ok-green dot (static under
`prefers-reduced-motion`), role in ok-green semibold, task label italic muted, last tool in
mono faint, age right-aligned tabular. Three variants: lead alone (`● lead · Read
api/internal/poller/mr_rework.go · 12s ago`), crew waiting (warn border, `waiting on rate
limit · 6m`, when the run is `limit_wait`/`pool_wait`), nothing declared (the strip sits
under the card header, faint border, no ◐ to attach to). Board card and runs-list row: the
`M1/6` badge gains a `◐` suffix, and one now line under the title
(`● coder  m2 · Decouple both detectors… 40s`).

TUI board (own board, selected row on the warm bar; `[cell]` = the blinking cell):

```
ON THE FLOOR · 2
● 9c672af9  running  14m  ▰[▱]▱▱▱▱  personal  $4  #908 PRD: Autofix (MR rework + CI autofix) for scheduled ru…
              ▸ m2 Decouple both detectors from agent/issue-N branch convention · coder  Decouple ci_fix detector from branch naming · 40s
● c923693e  running  25m  ▰▰[▱]▱▱   personal  $6  #1061 PRD: TUI sketch harness — preview a new TUI feature b…
```

TUI crew rail (26 cols):

```
MILESTONES ▰[▱]▱▱▱▱ 1/6 · m2
 ✓ Thread per-schedule m…
 [▱] Decouple both detecto…
   ↳ coder · 40s
     Decouple ci_fix det…
 ○ Board-free scheduled …
```

Static frame (non-tty, offline render, blink disabled): the cell is `▱` in the wait colour
and the `· m2` suffix carries the state without motion. Nothing declared: the eyebrow stays
`1/6`, and an unattached `↳ coder · 40s` line sits directly under it.

`uzi run get`: a `NOW` row after the `MILESTONES` block
(`coder · Decouple ci_fix detector from branch naming · Edit api/internal/poller/ci_autofix.go · 40s ago`).

## Resolved facts (offline-verified at `7435327`; the worker needs no internet)

- **A `running` state report without `iteration_count` is safe mid-run.** The checkpoint
  closure already sends one (`agent/src/runner.ts:2562`, comment "NO iteration_count: a
  checkpoint is not an iteration-boundary report"). Server-side, `SetRunRunning` only ever
  RAISES `iteration_count` (`api/internal/store/queries/runtime.sql:832` is the query
  header; the executable `iteration_count = GREATEST(iteration_count, @iteration_count)`
  is at `:899`; `status_since` stamps on entry only, `:871`; `session_id` is
  `COALESCE`d, so an omitted one is preserved), and `runningStateParams` (`api/internal/workersvc/service.go:2325`) unions
  `milestones_completed` and overwrites `milestones_in_progress` via `progressParams`,
  membership-checked against the frozen list. The budget config it re-supplies is
  COALESCE'd and inert after the first freeze (`:1903-1912`). So the immediate push needs
  NO api change.
- **A late push cannot corrupt a parked, cancelled or finished run.** `SetRunRunning`'s
  WHERE clause (`runtime.sql:~1000-1023`) requires `worker_id = @worker_id AND status NOT
  IN ('completed','failed','cancelled')` and excludes `limit_wait`/`pool_wait` explicitly
  (PRD #35), and `started_at`/`status_since`/`health` are preserved on a running→running
  report (`:871`, `:898`, `:983`). So a progress push still in flight when the turn parks
  (`ask_user` → `awaiting_input`, a rate limit → `limit_wait`) or after a cancel is a
  no-op — the guard that makes D1's fire-and-forget chain safe at turn end and on abort.
- **Every applied `running` report also drives the Slack notifier.** `PublishState` →
  `Notifier.handle` (`api/internal/slacksvc/notifier_state.go:27`) calls `UpdateBlocks`
  on the run's root message unconditionally (`:101`) and `handleMilestone` on a
  completed-count advance (`:109`, `:400`, count-guarded by
  `milestones_notified_completed` so it never double-posts). D1 fires these earlier and a
  few more times per milestone; no new post, no duplicate.
- **The strip-and-cap precedent for model-authored display text is
  `sanitizePlanChangedLine`** (`api/internal/workersvc/service.go:2302`, `termsafe.Unsafe`
  + length cap). Milestone titles are the WRONG precedent for `Detail`/`AgentLabel`:
  `validateMilestones` REJECTS a list on an unsafe char (`milestones.go:77`), and
  rejecting the whole now line because a repo has a control-char filename would be wrong.
- **The wire client is `WorkerClient.reportState` (`agent/src/client.ts:182`)**, exposed
  to the runner as `ctx.reportState` (`agent/src/runner.ts:273`), with bounded retries.
- **`scanSignals` (`agent/src/signals.ts:552`) already drops an all-empty call** (PRD #390
  D3, `:643-645`): `out.progress` is set only when at least one id parsed. The immediate
  push inherits that: an empty call is still no signal.
- **Frames are already persisted with the fields the now line needs.** `InsertRunMessage`
  (`runtime.sql:2164`, INSERT at `:2167-2169`) stores `kind, agent, agent_instance,
  agent_label, payload`, with `created_at` filled by the table default
  (`00020_workers_runs.sql:77`); `run_messages` carries `UNIQUE (run_id, seq)`
  (`api/internal/store/migrations/00020_workers_runs.sql:78`), which is the index a
  `DISTINCT ON (run_id) … ORDER BY run_id, seq DESC` walks. `MessageDTO`
  (`api/internal/apitypes/run.go:471`) is the wire shape; a subagent's frames carry
  `agent=<role>`, `agent_instance=<sdk id>`, `agent_label=<the Agent dispatch
  description>`; the lead's carry `agent="lead"`, no instance, no label.
- **The list handler already batches a per-run lookup by run ids**
  (`api/internal/handler/runs.go:55-71`: `JudgeTodoCountsForRuns` `:59`,
  `PlanRevisingForRuns` `:67`). `current_activity` follows that pattern. `RunListItemDTO`
  EMBEDS `RunDTO` (`apitypes/run.go:437-438`), so a field added to `RunDTO` (built in
  `runToDTO`, `api/internal/handler/runs_dto.go:63`, milestone decode at `:208-237`)
  reaches the board, the list (`runs.go:73-89` and the admin list `:116-124`), the TUI
  (`r.RunDTO`) and the CLI without a second DTO change.
- **A `RunDTO` field is a FIVE-file edit here, not one** (`.claude/rules/go.md`, "A DTO
  field change is a THREE-file edit"): the Go struct, the api-contract goldens
  `fixtures/api-contract/run.{zero,full}.json` AND, because `RunListItemDTO` embeds it,
  `run_list_item.{zero,full}.json` (byte-checked by `api/internal/apitypes/contract_test.go`,
  case at `:73`, and `api/internal/handler/contract_test.go`), and `web/src/lib/apiTypes.ts`
  (checked by `web/src/lib/apiContract.test.ts`). The goldens are **re-recorded from the
  JSON the failing Go test prints, never hand-authored**. Forgetting any one reddens
  `gate:api` or `gate:web` by name.
- **The TUI already ticks.** `tickCmd` (`api/cmd/uzi/tui.go:249-251`) is a `tea.Tick` on
  `boardPollInterval` (2 s, `:32`); there is no spinner and no blink today. The palette's
  wait colour is `ld(#0369a1, #38bdf8)` (`api/cmd/uzi/tui_render.go:170`, field at `:155`,
  `crewWaiting` mapped to it at `:191`), the same hue the rail's `◐` uses
  (`renderMilestones` `:261`). The board's micro-bar is `milestoneMarker`
  (`api/cmd/uzi/tui_board_rows.go:438`, ▰/▱ only, counts via `milestoneProgress`
  `tui_detail_rail.go:181`); the rail block is `renderMilestones`
  (`tui_detail_rail.go:226`); the italic label line under a lane row is the precedent for
  a second line under a row (`laneRow`, `tui_detail_rail.go:113`, label at `:155-160`).
  Lanes are `agentLane{Key, Role, Label, Frames, LastActivity}` (`tui_lanes.go:184-192`).
- **Unicode has no half-filled parallelogram.** `▰` U+25B0 / `▱` U+25B1 are the family's
  only members, so an in-progress state in this family is either colour-only (fails
  NO_COLOR) or motion. Families with a real half glyph exist (`■◧□`, `█▒░`, `●◐○`, `◆◈◇`)
  and were shown to the user, who chose to keep parallelograms and blink (Decision 4).
- **Native SGR-5 blink is unreliable**: Ghostty and kitty ignore it. The redraw tick is the
  portable mechanism (the bubbles spinner pattern) and it is what the mock uses.
- **Web surfaces and their anchors.** `MilestoneChecklist` and `MilestoneMark`
  (`web/src/pages/RunView.tsx:184-243`, rendered at `:1392`, run + messages from
  `useRunStream` at `:759`); badge fold `milestoneBadge`/`milestoneBadgeText`
  (`web/src/lib/runBadge.ts:512-560`), used by `Dashboard.tsx:349` and `RunsList.tsx:259`;
  lane derivation for the activity pane (`web/src/components/ActivityFeed.tsx:79-145`);
  run type fields at `web/src/lib/apiTypes.ts:1934-1937`, `RunMessage` at `:2655`. Mock
  fixtures with milestone runs live in `web/src/mocks/data/runs.ts` (`run-live` at
  `:415-420` is the streaming + milestone demo advanced by `engine.ts`).
- **Docs that describe today's `◐`.** `docs/cli.md:723-725` (TUI rail glyphs), `:688`
  (micro-bar), and `docs/run-activity.md` (sections at `:99` Lanes, `:142` Crew roster,
  `:272` Stopping or narrowing).
- **Tool inputs are already visible verbatim on the activity pane**, so surfacing a file
  path or an `Agent`/`Bash` description on the now line exposes nothing new; the line
  must still never carry raw `Bash` command text (Decision 3).

## Decisions

**D1 — Push on observation, in the worker; no new api route.** Add
`reportProgress?(progress)` to `ExecutorContext` (`agent/src/executor.ts:310`, beside
`reportIteration` and `checkpoint` `:328`), called the moment the scan loop sets
`sig.progress` (`sdk-executor.ts:2576-2579`; the call site of `reportIteration` is
`:1725`). The runner wires it to `reportState({status:"running",
milestones_completed, milestones_in_progress})` with NO `iteration_count`, mirroring the
checkpoint closure. **Every** `running` report of the run — the immediate pushes, the
turn-boundary `reportIteration` and the checkpoint report — is enqueued on ONE per-run
promise chain in the runner, so writes reach the api in the order they were made: without
that, a late immediate push (`in_progress: [m2]`) still inside `reportState`'s retry loop
could overtake the next turn's `reportIteration` (`in_progress: []`) and resurrect a
finished milestone as in-progress until the following report. At turn end a still-pending
push is awaited by the chain, never dropped and never blocking the scan loop. A failed push
is logged (`could not report progress`) and never fails the run (#122 additive-optional).
The turn-boundary path otherwise stays as is — it re-sends the same value, which the
server unions/overwrites idempotently.
*Rejected:* a dedicated `POST /worker/runs/{id}/progress` route — the `running` report
already carries the fields and is proven mid-run; a new route is surface for no gain.

**D2 — Transition frames are the worker's words, not the raw tool call.** Diff the observed
progress against the previously observed one: ids newly in `in_progress` emit `status`
`milestone <id> started — <title>`; ids newly in `completed` emit `milestone <id>
reported complete — <title>` (title from the frozen list, D7-safe through the existing
untrusted-text path). Repeats emit nothing. **Scope:** the frozen list and the diff base
live in the implement loop (`frozenMilestones`, `latestProgress` `:1553`, both read at
`:2039`), while the scan loop runs inside the streaming function (its own `latestProgress`
binding at `:719`). So the streaming function gains an `onProgress(progress)` callback
parameter, and the implement loop passes a closure per turn that owns the diff base
(`lastObserved`, seeded from `latestProgress`), has the frozen titles in scope, emits the
frames, and enqueues the push via `ctx.reportProgress`. The streaming scope stays
ignorant of milestones. **Resumed runs:** `frozenMilestones` is assigned at the plan gate
(`:1492`) and may be undefined on a re-claimed run; take the titles from the claim's
frozen list when the loop-scope one is absent, and degrade to a titleless frame
(`milestone m2 started`) rather than skip or throw. Wording says "reported
complete", never "done"/"verified" (PRD #122 D6). Signal tool frames stay suppressed
(`:2515`).

**D3 — One rule for "now", server-derived, in a stdlib-only Go package.**
The WIRE type is `apitypes.RunActivity{Agent, AgentLabel, Tool, Detail string; At
time.Time; Seq int32}` (json `agent, agent_label, tool, detail, at, seq`), in the
stdlib-only leaf the CLI already links; `RunDTO.CurrentActivity *RunActivity`. The RULE
lives in `api/internal/runactivity` (imports `apitypes`, nothing else outside stdlib),
`FromFrame(kind, agent, agent_label *string, payload json.RawMessage, created_at, seq)
*apitypes.RunActivity` — one struct, no mirror. (`apitypestest.Populate` sets `time.Time`
to a fixed instant and recurses pointers, `apitypestest/populate.go:22-53`; if it does
not allocate a nil nested struct pointer, extend it there rather than hand-editing the
golden.) The rule: the run's newest `kind='tool_use'` frame; `Tool` = `payload.name`;
`Detail` = the repo-relative `file_path` for `Read`/`Edit`/`Write`/`MultiEdit`, the
`description` for `Agent` and `Bash`, empty otherwise — **never `Bash`'s `command`**.
One rule for the lead's own `Agent` dispatch, because on a live run the newest frame is
often exactly that (the lead's `Agent` tool_use, while the subagent it started has not
yet written its first frame): for a `tool_use` named `Agent` the activity's `Agent` is
`input.subagent_type` (fallback the frame's own agent) and `AgentLabel` is
`input.description` — i.e. "coder · Decouple ci_fix detector…" from the dispatch itself,
which is the truth at that instant. Otherwise the frame's `agent`/`agent_label` are used
verbatim. The rule is "newest frame, whoever made it": do NOT narrow it to subagent
frames only; a lead working alone is a legitimate "now" (the mock's second variant).
`Detail`/`AgentLabel` are strip-and-capped (200 runes) the way `sanitizePlanChangedLine`
does it (Resolved facts) — stripped, never rejected. **The package exposes the SELECTION
as well as the fold:** `Latest(frames []Frame) *apitypes.RunActivity` walks a run's frames
newest-first, skips `tool_result`/`status`/every non-`tool_use` kind, folds the first
`tool_use` via `FromFrame`, and returns nil when none exists. The api computes it with one
batched `DISTINCT ON` query for the list page and one for `get` (the SQL is the one copy
of the selection that `Latest` cannot cover; M2's live-DB test pins it), and returns `null`
for terminal statuses (a finished run has no "now"). The TUI rail calls `Latest` on its own
frames, so the board (DTO) and rail (frames) cannot disagree in Go; the web run view
mirrors `Latest` in TS (`web/src/lib/runActivity.ts`, `latestActivity(messages)`) for
zero-latency updates off the WS frames. The golden fixture `fixtures/run-activity/cases.json`
is a list of **frame LISTS → expected activity** — pinning the selection, not only the fold
(a single-frame fixture would agree on the fold while three selection copies drift) — and
is asserted by BOTH the Go test and the vitest test (the cross-module fixture pattern this
repo already runs `-count=1` for), with each case asserted exercised. Required cases:
newest-of-several wins; a newer `tool_result` present but skipped; a newer non-tool frame
(`status`, `text`) skipped; subagent frame (agent + label) vs lead frame (no label); the
lead's `Agent` dispatch (role/label from `subagent_type`/`description`); `Bash` → detail is
`description`, never `command`; `Read`/`Edit` → repo-relative `file_path`; two subagents
interleaved (deterministic by `seq`, the newest wins — accepted flicker, R9); a hostile
label/path (control chars, over-long) stripped and capped; no `tool_use` at all → null.
Board, list and CLI read the DTO field.

**D4 — Parallelograms stay; in-progress is motion, not a new glyph.** User decision
2026-09-03 after seeing the alternatives. A `blinkTickMsg` on a 500 ms `tea.Tick` toggles
`m.blinkOn`; the tick is armed only while the model holds ≥1 visible run with a non-empty
`MilestonesInProgress` and disarmed otherwise, so an idle board wakes on nothing new. The
cell renders `▰` (on) / `▱` (off) in `pal.wait`; the initial phase is OFF, so a single
non-tty render (the tui-ux renderer, the #1061 sketch harness) shows the static frame.
`UZI_TUI_NO_BLINK=1` pins the static frame (reduced-motion opt-out), read ONCE at model
init with the CLI's `os.Getenv("UZI_*")` idiom (`api/cmd/uzi/root.go:249`, `:313`,
`:339`) — not per render. NO_COLOR is a `colorprofile` detection (`tui.go:10`, `:173`),
not an env read, so it is orthogonal: under an Ascii profile the alternation is
shape-based and survives. A `blinkArmed` flag guards the tick so a 2 s board refresh that
reveals an in-progress run never stacks a second 500 ms `tea.Tick` (double renders); the
cmd re-arms itself only from its own message. **Selection rule, all surfaces:** when several
ids are in progress, the FIRST by frozen order is the one that blinks / carries `◐` / is
named in `· <id>` and the header; the others render as today's in-progress mark
(`◐` on web rows, `▱` in wait colour on the rail) and the tooltip lists them all. The rail eyebrow appends `· <id>` for the
in-progress milestone; the rail's in-progress row mark becomes the same cell (replacing
`◐`); `✓`/`○` unchanged. The board's selected row gains a second line (`▸ <id> <title> ·
<role> <label> · <age>`), the lane-label precedent; unselected rows change only the cell.

**D5 — Back-compat contract, byte-for-byte, and where the line lives per run kind.** A run
with no frozen milestones AND no `current_activity` renders exactly as today on every
surface (the #122/#265/#390 contract). **On the web run view and the TUI rail the now
line exists ONLY for milestone runs** — it is hosted by `MilestoneChecklist` (which
returns `null` on an empty frozen list, `RunView.tsx:201`) and by `renderMilestones` (same
guard); a chat/self-improve/non-milestone run shows no strip there, its activity pane and
crew lanes already carry the lanes. A milestone run with activity but nothing declared
renders the unattached line under the header/eyebrow. **The board card, the runs-list
row, the TUI board second line and the CLI `NOW` row render activity for ANY non-terminal
run** — they read `current_activity` directly and have no milestone precondition. The
`M–/N` neutral state (#390 D5) is untouched.

**D6 — Web freshness comes from the frames, not from polling the DTO.** The run view
already holds every frame via `useRunStream`; the run DTO is re-read on run-updated WS
frames (`RunView.tsx:266`) and after user mutations (`refreshRun` at `:966`, `:1110`,
`:1173`…), not continuously, so a DTO-only now line would lag the transcript between
refreshes. Hence the TS mirror in D3. Board/list poll the DTO as they do today.

**D7 — CLI parity (convention: new functionality ⇒ check `api/cmd/uzi/`).** `uzi run get`
gains a `NOW` row; `current_activity` is an object, so `--field current_activity` on a
populated run is the documented usage error (exit 2), while on a run where it is `null`
it prints an empty line and exits 0 (`scalarField`'s existing null rule) — `--json`
carries it. Documented, not special-cased.

**D8 — No Slack code changes, but Slack timing shifts.** The notifier's `✓ N/M` thread
line (`slack_run_messages.milestones_notified_completed`, migration `00101`) and its
root-message `UpdateBlocks` are driven by every applied `running` report (Resolved facts),
so with D1 the thread line lands seconds after the lead reports instead of at the next
turn boundary, and the root is edited a few more times per milestone. Count-guarded, so
no duplicate thread line; no new post. A `▶ started` line is a follow-up.

## Milestones

Phase table (which milestones can run as parallel agents; files are disjoint per phase):

| Phase | Milestone | Depends on | Component / files | Parallel with |
|---|---|---|---|---|
| 1 | M1 worker push + transition frames | — | `agent/src/sdk-executor.ts`, `runner.ts`, `executor.ts`, `agent/test/**` | M2 |
| 1 | M2 `current_activity` package, query, DTO | — | `api/internal/runactivity/`, `store/queries/runtime.sql` (+ `sqlc generate`), `handler/runs*.go`, `apitypes/run.go`, `fixtures/api-contract/run*.json` (re-recorded), `web/src/lib/apiTypes.ts` (type only), `fixtures/run-activity/` | M1 |
| 2 | M3 web | M2 (field), M1 (◐ populated) | `web/src/pages/RunView.tsx`, `Dashboard.tsx`, `RunsList.tsx`, `lib/runBadge.ts`, `lib/runActivity.ts`, `lib/apiTypes.ts`, `mocks/**` | M4, M5 |
| 2 | M4 TUI blink + now line | M2 | `api/cmd/uzi/tui*.go` | M3, M5 |
| 2 | M5 CLI `run get` NOW row | M2 | `api/cmd/uzi/run_render.go`, `run_get.go` | M3, M4 |
| 3 | M6 docs, specs, changelog | M1-M5 | `docs/run-activity.md`, `docs/cli.md`, `specs/ai.md`, `CHANGELOG.md` | — |

### M1 — Worker: push progress on observation, emit transition frames

- [x] `ExecutorContext.reportProgress?(progress: MilestoneProgress): Promise<void>` in
      `agent/src/executor.ts`; called from the scan loop right where `sig.progress` is
      folded (`sdk-executor.ts:2576-2579`), awaited off the loop via a per-run promise chain.
- [x] Runner wiring next to `reportIteration` (`runner.ts:2507`): a `running` report with
      the milestone fields and no `iteration_count`; log-and-continue on failure.
- [x] Transition frames per D2, emitted from the executor with `agent: "worker"`,
      `kind: "status"`, diffed against the previous observed progress.
- [x] **Regression tests that go red on the unfixed code**: (a) a fake `reportState`
      receives `milestones_in_progress: ["m2"]` BEFORE the turn's result frame is processed
      (today: only at the next `reportIteration`); (b) a turn whose lead reports
      `in_progress: [m2]` and later `completed: [m2]` leaves the fake having SEEN the
      in-progress snapshot (today: never) — **the two calls MUST be two separate SDK
      messages within the one turn**: `scanSignals` is last-wins *per message*
      (`signals.ts:644` assigns once per `report_progress` block), so a pair inside one
      assistant message collapses to `completed` on fixed and unfixed code alike and the
      test would be vacuous; (c) transition frames appear once per transition
      and not on repeats; (d) an all-empty call still emits nothing (#390 D3 preserved);
      (e) `reportProgress` rejection does not fail the run. Verify each fails on a
      mutated/unfixed tree (mutation discipline, `.claude/rules/go.md` / `agent-team.md`).
- [x] `task gate:agent` green.

### M2 — API: `current_activity` on the run DTO

- [x] `api/internal/runactivity` (stdlib-only) with `FromFrame` per D3 and unit tests
      covering each tool family, the cap, the strip, and `Bash` command exclusion.
- [x] sqlc query `LatestToolUseForRuns(run_ids uuid[])` (`DISTINCT ON (run_id)`, `kind =
      'tool_use'`, `ORDER BY run_id, seq DESC`) + `sqlc generate`; a live-DB test asserting
      the newest frame wins, that a newer `tool_result` is skipped, and that a run with no
      tool_use returns no row.
- [x] Migration adding the partial index `run_messages (run_id, seq DESC) WHERE kind =
      'tool_use'` (goose number assigned at merge, above the live head) — the existing
      `UNIQUE (run_id, seq)` makes the `DISTINCT ON` walk back over every trailing non-tool
      frame, and the board polls every 2 s; land the index with the query rather than
      after a measurement, and record the `EXPLAIN` from the live-DB test in the MR.
- [x] `RunDTO.CurrentActivity *apitypes.RunActivity` (`json:"current_activity"`, null when
      absent or terminal), populated in `runToDTO`'s callers: the get path and both list
      builders (`handler/runs.go:73`, `:116`) via the batched query — `runToDTO` itself
      stays pure (`runs_dto.go:59-63`), so the GET handler sets the field too, not only
      the two list builders. Terminal = `completed`/`failed`/`cancelled` (the server's own
      set, e.g. `slacksvc/replier.go:574`); lift ONE shared predicate rather than add a
      fourth copy (`uzicli.IsTerminalRunStatus`, `stream.go:133`, is the CLI's; the
      handler should not import the CLI package for it). **Five-file edit**
      (Resolved facts): re-record `fixtures/api-contract/run.{zero,full}.json` and
      `run_list_item.{zero,full}.json` from the failing contract test's output, and add the
      field to `web/src/lib/apiTypes.ts` (its `apiContract.test.ts` half reddens otherwise;
      the TS type lands here so `gate:web` is green before M3 starts).
- [x] `Latest(frames)` in `runactivity` plus the golden fixture
      `fixtures/run-activity/cases.json` (frame LIST → expected activity, the D3 case list)
      asserted by the Go unit test with an every-case-exercised check; M3 asserts the same
      file from vitest.
- [x] `task gate:api` green; the lint ratchet in `.golangci.yml` applies (read the file, not
      the gate's capped output; `--max-same-issues=0` before counting).

### M3 — Web: the now line on run view, board and list

- [x] `web/src/lib/runActivity.ts`: `latestActivity(messages)` mirroring `Latest` (D3:
      selection AND fold), tested against `fixtures/run-activity/cases.json` with the same
      every-case-exercised check.
- [x] `MilestoneChecklist` signature grows from `{ run }` to `{ run, activity }`, where
      `activity = latestActivity(messages)` is computed in `RunView` from the frames
      `useRunStream` holds (`RunView.tsx:759`) and passed down (memoised on the newest
      seq); now-line strip under the `◐` row (or unattached under the header), header
      count `1/6 ◐ m2` — the attach point and the header id follow the D4 selection rule
      (first in-progress id by frozen order) — three variants (lead alone, waiting, nothing
      declared), `prefers-reduced-motion` respected, all text via `stripUnsafeChars`.
- [x] `milestoneBadgeText` gains the `◐` suffix when `milestones_in_progress` is non-empty;
      board card (`Dashboard.tsx`) and runs-list row (`RunsList.tsx`) render one now line
      from `run.current_activity`.
- [x] Mock mode: `run-live` fixture and the `engine.ts` sim carry `current_activity` so the
      line is demonstrable without a stack; the null-milestone fixtures prove D5.
- [x] vitest tests: render with/without activity, with/without declared milestone, terminal
      run hides the line, badge suffix, byte-identical output for null-milestone runs.
- [x] `task gate:web` green (knip zero-tolerance on unused exports — export only what is
      consumed); `web-ux` pass in mock mode.

### M4 — TUI: blinking cell, rail rows, board second line

- [x] `blinkTickMsg` + `blinkCmd` (500 ms) armed/disarmed per D4; `m.blinkOn` initial
      `false`; `UZI_TUI_NO_BLINK=1` honoured.
- [x] `milestoneMarker`: the in-progress cell (first frozen id in `MilestonesInProgress`,
      by frozen order) renders `▰`/`▱` in `pal.wait`; bar otherwise unchanged; width rules
      (`boardMileWidth`, `boardMileCap`, `boardShowMile`) unchanged.
- [x] `renderMilestones`: eyebrow `· <id>` suffix, in-progress row mark = the same cell,
      `↳ <role> · <age>` + italic label line beneath it (or unattached under the eyebrow),
      all through `renderer.Plain` (D7 untrusted fields, add `AgentLabel`/`Detail` to
      `d7UntrustedFields`); rail derives the activity from its own frames via
      `runactivity`.
- [x] Board: selected-row second line per D4 from `RunDTO.CurrentActivity`, width-clamped,
      riding the selection bg like the lane label line. The lane-label line is a DETAIL-rail
      precedent, not a board one: the board's selection/scroll math must tolerate one
      variable-height row — test with the top and the bottom row selected, and with the
      selection moving across a run that gains/loses activity between polls.
- [x] Tests: static frame equals today's frame plus `▱`+`· m2` (golden), tick armed only
      with in-progress runs and never double-armed across board refreshes (`blinkArmed`),
      `UZI_TUI_NO_BLINK=1` pins the static frame, Ascii-profile output alternates by
      shape, null-milestone run byte-identical (D5). `tui-ux` offline render light/dark/NO_COLOR.
- [x] `task gate:api` green (the TUI lives in the api module).

### M5 — CLI: `uzi run get` NOW row

- [x] `NOW` row after the `MILESTONES` block in `milestoneRows`'s caller, `cellText`-safe,
      hidden when `current_activity` is null; `--json` carries the object; `--field` on it
      is the documented exit 2.
- [x] Table test for present/absent/terminal; `task gate:api` green.

### M6 — Docs, specs, changelog

- [x] `docs/run-activity.md`: a "Milestones and the now line" section (what ◐/the blinking
      cell mean, where the line's words come from, "reported complete" wording).
- [x] `docs/cli.md`: the TUI section (`:688`, `:723-725`) and `uzi run get` gain the new
      rows/cell; `UZI_TUI_NO_BLINK` documented.
- [x] `specs/ai.md`: one decision entry (D1-D4); `specs/human.md` hygiene only.
- [x] `CHANGELOG.md` `[Unreleased]` entries per component. `web/scripts/check-docs.mjs`
      passes (frontmatter, links).

## Success criteria

1. **In-progress is visible within one turn.** On a milestone run whose milestone completes
   inside a single turn, `uzi run get --field` cannot read arrays, so: `uzi run get <id>
   --json | jq .milestones_in_progress` is non-empty while the lead works on it (today:
   `[]` for the run's whole life). Proven offline by M1 test (b); observed live after
   release on the dev cluster (not a gate).
2. **The feed names transitions**: `milestone m2 started — …` and `milestone m1 reported
   complete — …` status frames exist, once each.
3. **`current_activity`** is present on `run get`/`run list` for non-terminal runs with a
   tool_use frame, `null` otherwise, and `Detail` never contains a `Bash` command.
4. **Web**: checklist strip under ◐ (or unattached), badge `◐`, board/list now line; the
   `run-live` mock demonstrates it; reduced-motion stops the dot.
5. **TUI**: on a tty the cell alternates at 500 ms; a non-tty/`UZI_TUI_NO_BLINK=1` render
   shows `▱` + `· m2`; NO_COLOR still alternates; no tick while nothing is in progress.
6. **CLI**: `NOW` row present/absent per D7.
7. **D5 holds**: golden renders of a null-milestone, null-activity run are byte-identical
   before/after on web (snapshot), TUI (golden) and CLI (golden).
8. **All gates green** (`task gate`), the Go lint ratchet clean, knip clean, and the branch
   diff contains **zero** entries under `.github/workflows/`.

## Out of scope

- Per-milestone timestamps and durations (started/completed per id) — follow-up; needs the
  store shape widened from bare id arrays.
- Inferring the in-progress milestone when nothing is declared (contradicts #390 D7,
  "declared, not inferred"); the unattached line covers the visibility need.
- Stamping frames with the milestone in progress at insert time (per-milestone cost and
  grouping).
- A Slack `▶ started` thread line (D8).
- Changing the bar's glyph family (D4 records the alternatives).

## Risks & mitigations

- **R1 More `running` reports.** One per `report_progress` call (a few per milestone),
  serialized, bounded retries — negligible; measured in M1 tests by counting calls.
- **R2 Mid-turn `SetRunRunning` side effects.** Precedent-proven (checkpoint closure);
  `GREATEST` on `iteration_count`, COALESCE'd budget config, and the WHERE guard that makes
  a late push a no-op on a parked/cancelled/finished run (Resolved facts). M1's tests cover
  a pending push at turn end; M2's live-DB test asserts a progress-only report leaves
  `iteration_count`, `status_since` and `started_at` untouched.
- **R3 Query cost on the list page.** `DISTINCT ON` batched per page over the partial index
  M2 lands (`(run_id, seq DESC) WHERE kind='tool_use'`); the EXPLAIN in the live-DB test
  proves the index is chosen. Denormalising onto `runs` (the `last_seq` pattern,
  `UpdateRunLastSeq` `runtime.sql:1875`) was rejected: it would put a THIRD copy of the
  selection rule into the write path plus a migration, i.e. more drift, not less.
- **R9 Parallel subagents flicker.** "Newest frame" shows one lane at a time and can
  alternate between two concurrent subagents between polls. Accepted (it is still true at
  each instant) and pinned deterministic-by-`seq` in the fixture.
- **R4 Blink as an accessibility hazard.** One cell, 1 Hz, wait colour; opt-out env; static
  initial phase; web dot honours reduced-motion.
- **R5 Go/TS rule drift.** The shared golden fixture is asserted from both modules.
- **R6 Untrusted text on new surfaces.** Server cap + strip, then D7 at every render
  (`Plain`, `stripUnsafeChars`, `cellText`).
- **R7 A stale now line.** Age is computed client-side from `at`, so a stalled lane reads
  `12m` rather than lying; the TUI board re-polls every 2 s.
- **R8 The #1061 sketch harness lands mid-way.** No dependency: M4 renders via the tui-ux
  offline renderer; if the harness is on `main` by then, use it for the preview too.

## Dependencies

- None on other in-flight PRDs. #1061 (TUI sketch harness) is a convenience if merged.
- This PRD MUST NOT touch `.github/workflows/**` in implementation or validation
  (`.claude/rules/prds.md`); no CI change is needed.

## Validation

- Unit + live-DB tests per milestone under `task gate:<component>`; the mutation check on
  each M1 regression test (watch it red first).
- Mock-mode web pass (`web-ux`) on `run-live`; `tui-ux` offline render (light, dark,
  NO_COLOR, `UZI_TUI_NO_BLINK=1`) for M4.
- Post-release live read on the dev cluster of one milestone run's `milestones_in_progress`
  and feed frames (maintainer; never a gate, per the specs' testing-credentials policy).

## Notes for the offline (uzi) worker

- Everything needed is in this file and the repo; no web lookups. Re-derive every
  `file:line` at your base commit before editing.
- Gates: `task gate:agent`, `task gate:api`, `task gate:web`; `task gate` runs all. Read
  `.claude/rules/go.md`, `web.md`, `agent.md` when you open those trees (a rule fires on
  a file READ).
- Throwaway Postgres containers must be named OUTSIDE the `uzi-` namespace
  (root `CLAUDE.md`, *Destructive operations*).
- Wording: "reported complete", never "verified" (#122 D6). Nothing here changes what the
  lead is asked to do; the prompt note is untouched.

## Decision Log

- **2026-09-03 — Investigation.** Live runs `9c672af9` and `c923693e` (dev cluster) both
  showed `milestones_in_progress: []` with coder lanes active minutes earlier; the feed had
  no signal frames (suppressed at `sdk-executor.ts:2515`) and only `implement/review
  iteration N` lines. Root cause traced to turn-boundary persistence + last-wins +
  prompt wording (Problem 1-2), not to the lead ignoring the prompt.
- **2026-09-03 — Glyphs.** Showed `■◧□`, `█▒░`, `●◐○`, `◆◈◇` alternatives with a real half
  state; user chose to keep `▰▱` and blink the cell. Native SGR-5 rejected as unreliable
  (Ghostty/kitty ignore it); tick-driven redraw chosen; static frame + `· <id>` suffix for
  non-tty renders.
- **2026-09-03 — Review wave (three parallel lenses: facts, milestones, design).** One
  wrong anchor fixed (wait colour `tui_render.go:170`), four imprecisions tightened.
  Folded in: the five-file DTO rule (api-contract goldens + `apiTypes.ts` land in M2); one
  per-run ordering chain for ALL running reports (a late push could otherwise resurrect a
  finished milestone); the `Agent`-dispatch rule in D3; the fixture pins selection
  (`Latest`) not only the fold; strip-and-cap via `sanitizePlanChangedLine`, not the
  milestone REJECT path; partial index lands with the query; Slack timing shift recorded;
  run-view/rail now line scoped to milestone runs, board/list/CLI for any run; resumed-run
  title fallback; `blinkArmed`; variable-height board row tests. Predecessor contracts
  (#122 D6, #390 D3/D5/D7, #265) re-checked and honoured.
- **2026-09-03 — Where "now" is computed.** Server-derived, one Go package linked by api +
  TUI, TS mirror for the run view pinned by a shared golden fixture (D3/D6). Rejected:
  client-only derivation everywhere (the board/list DTO carries no frames) and
  server-only (the run view would lag the transcript).
