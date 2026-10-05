# PRD #2275: Workers in the TUI

**Status**: Implemented with automated acceptance coverage. M1 and M2 are implemented; ANSI scenes are generated and asserted in both themes. D12 PNG rendering, the `tui-ux` screenshot review and the maintainer's `--demo` and live drive are done; the maintainer's review round is recorded as D14-D21. Resolved facts below were read at `main` `10d18291`. Reviewed by an architect, a tui-ux reviewer and a Codex peer before landing.

**Design mock**: `prds/mockups/2275-workers-tui-mock.sh` (run `bash prds/mockups/2275-workers-tui-mock.sh` in a terminal; its header lists the keys). The mock is the agreed visual and navigation reference. It is a throwaway bash script with static fixture data. It is not shipped code and never a parallel model for the TUI (`.claude/rules/tui.md`). Where the mock and this PRD disagree, this PRD wins. The mock's `w` (width 80/120) and `z` (summary on/off) keys exist only for comparing layouts; they are not product keys (`w` is the shipped `keyRework`).

## Problem

`uzi tui` shows the floor (runs), pulls and CI, but not the workers that execute the runs. When a run sits queued, or a worker stops taking work, the user must leave the TUI for `uzi worker list` or the web Workers page to learn why. The facts are already on the wire (`apitypes.WorkerDTO` and `AdminWorkerDTO`, `api/internal/apitypes/worker.go`):

- state, drain/cordon and custody holds;
- reported runs and their phases, and pending terminal outcomes;
- outbox depth, upgrade health, and resource and disk samples.

`uzi worker list` prints many of these as a flat table with no attention ordering. Nothing in the TUI shows any of them.

## Outcome

`uzi tui` gains a `workers` view, second in the tab strip: `floor  workers  pulls  ci` (keys `1`-`4`; the digits appear only in the `?` help, D14).

- **List.** Sorted so the workers needing attention come first, it answers "why can't this worker take work?" before it shows resource figures.
- **Drill-in.** Explains one worker.
- **Split view.** Workers is a top-pane view alongside the floor; pulls and ci stay in the bottom pane.
- **Floor.** The fleet status rides right-aligned on the title line (D15), and each run's worker shows on wide terminals.

Acceptance examples (counts are from the `demo.go` seed this PRD adds, which mirrors the mock's fixtures):

1. A 40×120 terminal, own token, `uzi tui --demo`. Press `2`.
   - **List order.** One row per worker, worst severity first, then by name:
     - danger rows: `forge-large` (`✕ data 92%`), `forge-small` (offline, `✕ upgrade failed +2`), `laptop` (`✕ outbox blocked +3`);
     - warn rows: `forge-docker` (`◷ outcome pending 12m +2`), `forge-m-2` (draining), `recovery` (holding, `⚑ unpublished work`);
     - info rows (lease, chat);
     - then workers with no attention item.
   - Within a row, items are listed worst first.
   - **Fleet status.** Right-aligned on the title line (D15), narrowed to fit: `workers · 9 · 8 online · 5/12 slots in use +1 ?cap · 1 holding · 1 draining · 6 need attention` in full; the count includes only workers with a danger or warn item (D11).
   - **Detail.** `enter` on `forge-small` opens its detail. Attention comes first (blocking container `seed-nix`, reason `ImagePullBackOff`, target vs running version), then reported runs, resources (`stale, last-known` label, `?` for absent readings), then configuration. `esc` returns to the list with `forge-small` still selected, even after a poll reorders rows.
2. A 60×120 terminal (above `splitMinHeight`) with the split drawn.
   - **Workers on top.** Press `2`. The top pane shows the workers list with focus, in the wide layout at ≥120 columns (D17), and the bottom pane keeps CI.
   - **Pane keys.** `ctrl+w` moves focus to CI and back. `4`/`3` switch the bottom pane between ci and pulls, and `1` returns the top pane to the floor.
   - **`tab` order.** `tab` cycles floor → workers → pulls → ci, each view landing in its own pane.
   - **Drill-in and back.** `enter` on a focused worker opens its detail full-screen; `esc` returns to the split with workers on top, the same worker selected and the top pane focused.
   - **Collapse.** `s` collapses to the top pane's view (workers), and `s` again restores both panes.
3. **Floor, 120 columns, demo seed.**
   - Each run row ends with its worker name.
   - The title line carries the fleet status right-aligned (D15), and the account-meter line carries the run summary right-aligned.
   - From the floor, open the run on `forge-large` and press `W`: that worker's detail opens, and `esc` returns to the run.
   - In worker detail, `enter` on a reported run opens that run, and `esc` returns to the worker.
   - **Unclaimed run.** On a queued run (no worker), `W` shows `no worker yet` in the footer and stays put.
   - **At 80 columns** the worker cell is dropped from floor rows.
4. **Admin scope.** With an admin-scoped (`uza_`) token, `a` toggles the shared admin flag:
   - The floor shows `active runs` as it does today.
   - Workers shows `factory workers` (`AdminListWorkers`). At 120 columns an OWNER column appears; at 80 the owner is on the selected-row readout.
   - A worker with sustained disk pressure (`DiskPressureVolumes = ["data"]`) shows `✕ pressure: data` as an attention item.
   - With a `uzc_` token, `a` shows the board's existing admin-denied note and keeps own scope on both views.

## Out of scope

- **Any API, DTO or route change.** Every field used is already in `WorkerDTO`, `AdminWorkerDTO` or `RunListItemDTO`/`RunDTO`, reachable through the existing `uzicli.Client.ListWorkers`, `AdminListWorkers` and `GetRun`.
- **Showing which run an ephemeral worker is bound to.** `WorkerDTO` deliberately omits `ephemeral_run_id` (server-internal), and `ReportedRuns` is not an authoritative binding. The view shows `ephemeral`, the reported runs and the idle lease remaining, and never infers a binding.
- **Worker write actions** from the TUI (remove, set-token, cordon). They stay CLI verbs.
- **Changing `uzi worker list` / `uzi admin workers` output.** CLI parity holds: the CLI already prints these facts and this PRD adds no route or DTO.
- **Workers in the bottom pane.** Workers belongs to the top pane (user decision, D2).
- **A server disk-pressure verdict on the own list** (D6).
- **Rendering uxlab PNGs inside the implementation run** (D12).

## Modules and seams

### View enum, tab strip and keymap (`tui.go`, `tui_keys.go`, `tui_board.go`, `tui_split.go`)

- **Views.** Add `viewWorkers` (list) and `viewWorker` (drill-in) to `tuiView`. `listView()` includes `viewWorkers`.
- **Tab strip.** `tabStrip` (`tui_board.go:377`) renders `floor · workers · pulls · ci`. The admin relabel of the floor tab is unchanged.
- **Keys.** `keyViewFloor = "1"`, `keyViewWorkers = "2"`, `keyViewPulls = "3"`, `keyViewCI = "4"`. Every handler that switches on the old `2`/`3` moves to the new constants:
  - `tui_board.go:360-366`
  - `tui_pulls.go:481-492`
  - `tui_ci.go:902-912`
  - the split block, `tui.go:1593-1630`
- **`tab` / `shift+tab`** cycle the strip order floor → workers → pulls → ci in both layouts (D3).
- **Stale comment.** Fix `tui_keys.go:34-37`: it still says CI is future work and that `tab` cycles floor ↔ pulls.
- **Help overlay.** `helpLines` (`tui_keys.go:78-136`) hard-codes `1 / 2 / 3` and `tab … (floor · pulls · ci)` in its common, pulls and ci cases. The split rewrite (`tui.go:1700-1715`) matches the `"1 / 2 / 3 "` prefix and writes `cycle floor, ci, pulls`. Update all of them, and add `viewWorkers` and `viewWorker` cases (`enter` detail, `/` filter, `a` scope, `r` refresh, `esc` back, `W` in run detail).
- **Footer hints.** Update the hint strings at `tui_ci.go:771` and `tui_pulls.go:922`. Add workers list and detail footers. No footer, summary or list line exceeds 80 columns at an 80-column width.
- **Filter.** `/` filters the workers list by name, case-insensitive, matching pulls/ci behaviour; `m.filtering()` already gates the digit keys. `h` (hide done) is a no-op on workers.

### Split ownership (`tui_split.go`, `tui.go`, and every return-to-top site)

- **Panes.** The split keeps two panes. The top pane hosts `viewBoard` or `viewWorkers` (new `topTab`, mirroring `bottomTab`; default `viewBoard`); the bottom pane hosts `viewCI` or `viewPulls`. Invariant: workers and floor are never bottom views; ci and pulls are never top views.
- **Keys in split.**
  - `1`/`2` set `topTab` and focus the top pane; `3`/`4` set `bottomTab` and focus the bottom pane.
  - `ctrl+w` switches focus and keeps each pane's view.
  - `s` collapses and restores, as today.
- **Collapse goes to `topTab`** (D13), not the focused pane's view. With `topTab = viewBoard` that is today's behaviour exactly. Each site below currently calls `setListView(viewBoard)` and is classified as follows:
  - **Go to `topTab`:**
    - `collapseSplit`, `tui_split.go:54-62`;
    - the resize collapse, `tui.go:903`;
    - the split block's `esc` and `ctrl+w` back-to-top keys, `tui.go:1620/1624`. The split block's `tab` wrap (`tui.go:1603`) is not one of these: per D3 it goes to the floor (ci → floor), and `shift+tab` from the floor (`tui.go:1613`) goes to ci.
  - **Explicit floor** (the user asked for the floor):
    - `case keyTab, keyViewFloor` on `tui_ci.go:904` and `keyViewFloor` on `tui_pulls.go:483` (under D3, `tab` from ci wraps to the floor; `tab` from pulls now goes to ci and `tab` from the floor to workers, `tui_board.go:360`);
    - the full-screen `keyEsc` handlers on `tui_ci.go:912` and `tui_pulls.go:492`;
    - `exitToBoard`, `tui_detail.go:457`;
    - `tui_cirun.go:212`, `tui_pr.go:284`. These return from their drill-ins to the origin, which is the floor today; they keep doing so unless `fromSplit` recorded `topTab = viewWorkers`, in which case they return there.
- **Split chrome** gains a workers case wherever the floor is special-cased:
  - `displayedForge` returns the bottom view only when the bottom pane is drawn;
  - the `[floor]` bracket in `splitHeader` (`tui_split.go:269`) brackets the top pane's tab;
  - `splitFooterLine` (`tui_split.go:222-230`) gets workers hints;
  - `splitSeparatorAt`'s focus test (`tui_split.go:151`) treats either top view as top-focused.
- **Pane height.** The workers top pane uses the floor's height allocation and the same width-driven layout as full screen: the wide table at ≥120 columns (D17).
- **Drill-in from the top pane.** `enter` on a top-pane worker opens `viewWorker` full-screen through the existing `fromSplit` path (`tui_board.go:352`). `esc` restores the split with `topTab = viewWorkers`, the same selected worker ID and the top pane focused.

### Fleet summary line and height accounting (`tui_board.go`, `tui_split.go`)

- **Placement.** Superseded by D15: the fleet status is right-aligned on the title line on the floor and the workers views, full screen and split, narrowed to the space left after the tabs; it takes its own row only when even its shortest form does not fit (full-screen 80 columns). The workers view has no separate summary line.
- **Height.** Charged only when drawn: the fleet status's fallback row (when even its shortest form does not fit beside the tabs) and the run summary's fallback row (when it does not fit beside the account meters). `splitSharedChrome` counts the worst case of the shared header, so the split thresholds stay at 41/43 rows; tiny full-screen viewports crop the body and keep the footer.
- **Shown only when the viewer has at least one worker.** With zero workers, the fleet status is not drawn.
- **Narrowing.** The status never wraps: it drops, in order, the unknown-cap note, then the holding/draining/cordoned counts, then the worker count and the online count. It always keeps the scope, slots in use and the attention count.

### Workers fetch and polling (new `tui_workers.go`)

- **One shared request chain** feeds the floor summary, the workers list and worker detail.
  - It is active whenever any of them is displayed, including an unfocused visible top pane. The floor is the default view, so an open TUI makes one workers list call every 5 s (D5).
  - It stops when none is displayed (run detail, PR and CI drill-ins, the help overlay).
  - Entering a workers-bearing view or changing scope fetches immediately.
- **Pattern** is the same as the board, pulls and ci polls:
  - `workersMsg{rows, admin, err, reqID}`, and a `workersTickMsg{gen}` with a tick generation like `tickGen` (`tui_board.go:43-61`);
  - a pure `workersTickInterval(failures)` backoff like `pullsTickInterval` (`tui_pulls.go:68`), and an in-flight guard;
  - a per-call timeout covered like `tui_poll_timeout_test.go`.
  - A reply whose `admin` flag differs from the current scope is dropped, so a stale factory reply never populates own scope or the reverse.
  - `r` always fetches and supersedes.
- **Scope.** The admin flag is the existing `m.board.admin` / `adminDenied` (`tui_board.go:24-27`), shared by floor and workers. `a` on either view flips it, refetches both, and the summary follows it. Admin mode calls `AdminListWorkers`; a `uzc_` token gets the existing denied note. The denied flag is set today only in the board reply handler (`tui_board.go:90-94`); an `AdminListWorkers` 403 must also clear `board.admin` and set `adminDenied`, so `a` pressed on workers shows the note without waiting for a board reply.
- **Row type.** `workerRow{w WorkerDTO; workerOwner string; pressureText []string}`. Own rows leave both empty; admin rows carry `OwnerEmail` and `DiskPressureVolumes`. The fields are distinctly named because the existing `OwnerEmail` guard entry does not cover a value once it is copied under another name (D7 guard below).
- **Resource bounds.** Both list endpoints return the whole set in one response with no pagination, as `uzi worker list` does today. The TUI holds one slice (the latest reply) and no history. Rendering is windowed by cursor and scroll, as on the floor. Server cost is one list call per 5 s while a workers-bearing view is displayed.

### Worker state, attention and ordering (pure helpers in `tui_workers.go`)

- **`workerState(r workerRow) string`.** Precedence offline > holding > draining | cordoned > busy > idle:
  - `offline`: `Status != "online"`.
  - `holding`: `RetainingUnpublishedWork`.
  - `draining`: `DrainingSince != nil && ActiveRuns > 0`. `cordoned`: `DrainingSince != nil && ActiveRuns == 0`. Same as `statusCell`, `worker.go:276`.
  - `busy`: `Busy`, which also covers a lone chat at `active_runs = 0`.
- **`workerAttention(r workerRow, now) []attnItem`** returns ordered items. Each has a severity (`danger`, `warn` or `info`), a short list text and a detail sentence:
  - **danger:**
    - upgrade failed (`UpgradeStatus == "upgrade_failed"`), with blocking container, reason and exit code;
    - outbox blocked: `OutboxBlocked != nil`. Queue depth alone is never reported as blocked;
    - admin `pressure: <volumes>` from `DiskPressureVolumes`. It covers reported pressure only, and dind pressure is labelled display-only;
    - a data or nix disk reading ≥ 90% (D6).
  - **warn:**
    - holding unpublished work;
    - outcome pending: a `ReportedRuns[i].TerminalPending` entry aged from `TerminalPendingSince`, worded "not yet delivered/acknowledged";
    - outbox queued: `OutboxPendingMessages > 0`;
    - offline with heartbeat age;
    - draining or cordoned;
    - outdated: `UpgradeStatus == "outdated"`;
    - template drift: both `TemplateDeclared` and `TemplateReported` set and different.
    - a dind disk or any inode reading ≥ 90%: warn only, labelled display-only, because it never gates admission (D6).
  - **info:**
    - ephemeral lease left: `EphemeralLeaseExpiresAt` in the future;
    - chat active: `Busy && ActiveRuns == 0`.
- **Severity glyphs** are shown on every item, so severity survives without colour: `✕` danger, a state-specific glyph (`⚑`, `◷`, `⇡`, `◐`, `◌`, `↑`) or else `▲` warn, `·` info. `!` is not used: the floor already uses it for `awaiting`. "N need attention" counts workers with at least one danger or warn item (D11).
- **Ordering.** Rows sort by worst severity (danger, warn, info, none), then name. The cursor is tracked by worker `ID`, so a poll that reorders rows keeps the selection, and `esc` from detail restores by ID. A worker that disappeared from the list selects the row at its old index.
- **Occupancy.** Run slots render as `ActiveRuns/MaxConcurrentRuns`, with `?` for a null cap, never `0`. The summary reports occupancy ("N/M slots in use" over online workers, plus an unknown-cap count), never schedulable capacity. Holding, draining and cordoned counts are listed separately (D8).
- **Phase and stage.** `ReportedRuns[i].Phase` (closed enum `running | awaiting_approval | awaiting_input | awaiting_followup`) is the worker's phase. The run's stage comes from the board cache, shown as `run stage`, and the run id stands in when the run is not cached (D9).

### Rendering (`tui_workers.go`)

- **List at ≥120 columns:** NAME, [OWNER in factory scope], STATE (glyph + word), KIND (`host·L`, `host·L+dk`, `eph·M`, `ext`), RUNS, CPU, MEM, DISK (worst reading, labelled; `?` when absent), VERSION, HB age, ATTENTION (glyph + first item + `+N`).
  - VERSION carries `↑` when outdated and `✕` when the upgrade failed.
  - Stale readings (the worker is offline, so its stats are last-known) are dimmed and prefixed `~`.
- **List at 80 columns:** NAME, STATE, KIND, RUNS, ATTENTION.
- **Selected-row readout** under the list: every attention item, plus the owner in factory scope. One item per line at 80 columns.
- **Detail sections, in order:**
  - **Attention.**
  - **Reported runs:** `#iid title engine`, then the worker phase, outcome pending, run stage and generation; `j`/`k` select a reported run.
    - `enter` preflights the run by id with `GetRun`, so the run need not be cached. The successful DTO enters through the normal guarded detail handler, including input-fetch and blink side effects, then tail and stream loading start without a second initial `GetRun`.
    - A 404 shows `run not visible` in the footer and stays put.
  - **Resources:**
    - cpu; memory, labelled `process only` when `StatsSource == "process"`;
    - data, nix and dind disks with inodes, where dind and inodes are labelled display-only;
    - the largest run's HOME from `RunDisk` (one line), with `≥` when `Truncated` and the `SampledAt` age;
    - a `stale, last-known` label when offline.
  - **Configuration:** version and `UpgradeTarget`; capabilities; template declared and reported; token mode (`default`, `auto`, or the pinned label); `ephemeral` with the lease remaining; owner in factory scope.
- **Untrusted text (D7 guard).** The bare wire names `Name`, `Version` and friends collide with existing draws (`tui_d7_guard_test.go:179-213`). So, following `ciRowText` / `ciTextOf` (`tui_ci.go`):
  - **Projection.** Project each worker's free-text fields into a distinctly named struct (`workerRowText`):
    - `workerName`, `workerVersion`;
    - `templateDeclared`, `templateReported`;
    - `upgradeDetail`, `upgradeTarget`;
    - `blockingContainer`, `blockingReason`;
    - `outboxBlockedText`;
    - `tokenLabel` (from `AnthropicSecretLabel`);
    - `hostedSize`;
    - `capabilityText` (server-filtered, but projected anyway).
    - `workerOwner` (from `OwnerEmail`) and `pressureText` (from `DiskPressureVolumes`, server-set, projected anyway).
  - **`attnItem`** holds `attnShort` and `attnDetail` built from those projections.
  - **Run-side names.** `RunListItemDTO.WorkerName` and `RunDTO.WorkerName` project to `runWorkerName`.
  - **Rule.** Every projected field is drawn only through `m.renderer.Plain` and registered in `d7UntrustedFields`. The existing `OwnerEmail` entry does not cover `workerOwner`, so it is registered separately.
  - Closed enums (`Status`, `UpgradeStatus`, `Phase`, `Kind`) need no scrub.
- **Colour fallback.** Every colour signal also carries a glyph or word: state glyph plus word, severity glyph, VERSION marker, stale `~`.

### Floor additions (`tui_board_rows.go`, `tui_detail.go`)

- **Worker cell.** At ≥120 columns each floor run row ends with `runWorkerName`; at 80 the cell is dropped.
- **Run detail rail** shows the worker name. `W` (shift+w, unbound everywhere at `10d18291`) opens that worker's `viewWorker`; `esc` reopens the originating run in a fresh session with its original return target.
  - One worker-origin context retains list selection, scroll and pane state, or the originating run ID and return target. Further cross-links update that context instead of accumulating history. A reported run returns to the current worker without overwriting its origin.
  - Every departing run closes its stream, invalidates its session and discards loaded detail, transcript buffers, guards and fallback handles. Returning uses the normal newest-first entry and background history backfill; no loaded session is suspended or restored. PR round-trips from a run likewise retain only a run ID and return target.
  - If `WorkerID` is nil (queued, unclaimed or wall-parked), `W` shows `no worker yet` and stays put.
  - If the worker is not in the current list (for example the admin board while the shared list is own-scope, or a worker removed since), `W` refetches once. If the worker is still absent it shows `worker not in your list` and stays put.

### Demo, uxlab, docs

- **`demo.go`.** Seed the mock's workers: all six states; every attention item, including admin pressure and a genuine `OutboxBlocked`; an idle ephemeral worker with a lease; a pinned token label.
- **`uxlab_gen_test.go`.** Add scenes for:
  - the workers list at 120 and at 80;
  - factory scope;
  - worker detail (failed upgrade; outcome pending plus outbox; holding);
  - the split with workers on top;
  - the floor with the fleet line.

  Under the env-gated `go test`, scene generation writes the ANSI frames. Rendering them to PNG needs devbox `charm-freeze` and is a maintainer step (D12).
- **`docs/cli.md`.** Update the split prose (lines 1440-1443) and the key block (lines 1526-1530): the new tab order and keys, the split ownership rule, the workers view, `/` on workers, and `W`. Run `task docs:sync` and commit the mirror.

## Testing decisions

- **Pure helpers.** Table tests for `workerState`, `workerAttention` and the sort:
  - draining vs cordoned;
  - offline, draining and upgrade_failed together: offline wins the state, and both other facts show as attention;
  - busy at `active_runs = 0`;
  - a null cap;
  - outbox depth without `OutboxBlocked` (no "blocked");
  - terminal pending aging;
  - template drift needs both values;
  - admin pressure, independent of the 90% cue;
  - severity counting for "need attention";
  - sort order and its name tie-break.
- **Update→msg / View()→string seam** (`tui_model_test.go` / `tui_render_test.go` pattern):
  - keys 1-4 in both layouts, and the `tab` order, including the split wrap ci → floor even with workers on top, and `shift+tab` floor → ci;
  - `ctrl+w` focus;
  - `s` collapse to `topTab` and restore;
  - resize collapse and restore keeping both panes' views;
  - drill-in from the top pane, and `esc` restoring the split, the selected ID and focus;
  - cursor kept by ID across a reordering poll;
  - `W` from run detail, plus its nil-worker and not-in-list cases;
  - `enter` on a reported run, plus its 404 case;
  - `a` on a `uzc_` fake (denied, both views stay own);
  - summary wording (`slots in use`, `?cap`) and segment dropping at 80;
  - no list, summary, footer or help line wider than 80 at width 80;
  - help overlay text in full-screen and split for every list view.
- **Polling.** Following `tui_poll_guard_test.go`, `tui_poll_timeout_test.go` and `tui_backfill_test.go`:
  - the tick is armed only while a workers-bearing view is displayed, including the floor;
  - a stale `reqID` or tick gen is dropped;
  - a scope-mismatched reply is dropped;
  - backoff grows on failure;
  - `r` supersedes.
- **D7.** Register every projected name; extend the hostile-value render test with a hostile worker name, version, blocking reason, `OutboxBlocked`, token label, run `WorkerName` and owner (factory list, selected-row readout and detail), covering the list, detail, floor worker cell and run-detail rail.
- **Colour fallback.** Under the Ascii colorprofile, the state word and glyph, the severity glyph, the VERSION marker and the stale `~` all survive.
- **Gate.** `task gate:api`, and the env-gated uxlab scene generation as a smoke test of the new scenes.

## Milestones

### M1: the workers list, in its tab and the split's top pane, with the floor fleet summary

M1 includes the split top pane: once `viewWorkers` joins `listView()`, a tall terminal draws it in the split, so the split must handle workers in the same slice.

Contents:

- the strip and keys 1-4, `tab` order, help and footers, `/` filter;
- the shared poll and scope;
- `workerRow`, state, attention, sort, and cursor by ID;
- the list at both widths, in own and factory scope;
- `topTab` and the split semantics, including collapse to `topTab` and every return-to-top site;
- the title-line fleet status, the meter-line run summary and their height accounting;
- the stale `tui_keys.go` comment;
- D7 projections and guard entries for list text;
- the demo seed, the list, split and floor uxlab scenes, and `docs/cli.md` with `task docs:sync`.

- Blocked by: none.
- Acceptance: example 1 (list half), example 2 (everything except the drill-in), and example 4. The test-plan items for M1 pass. `task gate:api` is green.

### M2: worker detail and run ↔ worker cross-links

Contents:

- `viewWorker` with its four sections;
- `enter` from the list and from the top pane, and `esc` restore;
- `enter` on a reported run;
- `W` from run detail and its fallbacks;
- the worker cell on wide floor rows;
- D7 entries for detail and run-side text;
- uxlab detail scenes.

- Blocked by: M1.
- Acceptance: example 1 (detail half), example 2 (drill-in), and example 3. The test-plan items for M2 pass.

## Implementation progress

- [x] M1: list, shared polling and scope, split top pane, fleet summary, demo and docs.
- [x] M2: four-section detail, single-origin cross-links and fresh run sessions, floor/rail worker names.
- [x] Automated navigation, polling, untrusted text, resource and dimension acceptance tests.
- [x] Nine feature ANSI scenes generated in dark and light themes; content and bounds asserted.
- [x] D12: render uxlab PNGs.
- [x] D12: `tui-ux` screenshot review against the mock.
- [x] D12: drive `uzi tui --demo` manually (maintainer, live and demo, 2026-10-05).
- [x] Maintainer review round: D14-D21 applied, mock updated to match.

The factory-only demo worker is cordoned to cover all six primary states; the nine own workers and their acceptance totals remain unchanged. Nix shows no inode reading because the existing DTO carries no nix inode fields.

## Acceptance (live, maintainer)

No live-environment behaviour: everything is observable through `uzi tui --demo`. Before merge the maintainer:

- renders the uxlab PNGs (`cd api/cmd/uzi/uxlab && devbox run build`);
- has the `tui-ux` agent review the new scenes against the mock;
- drives `--demo` by hand.

The implementation run does not do this step (D12).

## Decision Log

- **D1, workers is second in the strip and the keys renumber to match** (user decision 2026-10-05; Codex peer concurred). `1 floor · 2 workers · 3 pulls · 4 ci`. Rejected: keeping `2`/`3` for pulls/ci and putting workers on `4` while drawing it second, because the digits would disagree with the strip. This changes shipped muscle memory for `2`/`3`, and `docs/cli.md` records the change.
- **D2, workers lives in the top pane, not in the bottom rotation** (user decision). The top pane shows what is running and where (runs and the machines running them); the bottom pane shows forge state (CI, PRs).
- **D3, `tab` follows the strip order in both layouts.** Today the full-screen cycle is floor → pulls → ci and the split cycle is floor → ci → pulls (PRD #2171). One cycle that matches the strip is simpler than two orders that both differ from it. Rejected: keeping the split's CI-first order.
- **D4, `splitMinHeight` grows by one row for the fleet summary.** The split and board chrome count rows exactly. The alternatives were folding the summary into an existing row (no row has room at 80 columns) or not charging it (pushes the footer off-screen). No other sizing rule changes; the workers top pane reuses the floor's allocation.
- **D5, one shared 5 s poll while the floor or a workers view is displayed.** The fleet summary sits on the floor, which is the default view, so an open TUI makes one workers list call every 5 s, alongside the existing 2 s board poll. Heartbeat-derived fields change over seconds. Rejected: polling only from the workers view, which would leave the floor summary stale, and a slower floor-only cadence, which would add a second cadence for little saving.
- **D6, no server-threshold claim on the own list.** `DiskPressureThreshold` is set only on the heartbeat response, so the own-list cue (danger at ≥90%) is a fixed visual cue, and the legend says so. dind and inode readings are labelled display-only because they never gate admission. In factory scope, `AdminWorkerDTO.DiskPressureVolumes` (sustained fresh pressure) is the authoritative signal and becomes its own `pressure:` item.
- **D7, one primary state word plus attention items, not one exclusive status enum** (Codex peer). Status, admission (drain, cordon, custody), activity and upgrade are separate facts; an offline worker can also be draining and upgrade-failed.
- **D8, occupancy, not capacity.** "N/M slots in use", with holding, draining and cordoned listed separately and `?` for a null cap. A "free capacity" number was rejected because the list cannot know claim eligibility (capabilities, templates, ephemeral binding).
- **D9, the worker's phase and the run's stage are separately sourced.** `ReportedRuns.Phase` is the worker's report; the stage comes from the board cache. Mixing them would present one source's claim as the other's.
- **D10, the bash mock lives in `prds/mockups/`, beside the HTML mocks.** It is a design artifact, not a TUI sketch. The sketch harness (`sketch.go`) previews a branch-local feature on the real renderer and never lands on `main`; this mock must stay reviewable from `main`.
- **D11, three attention severities, and only danger and warn count as "need attention".** An ephemeral lease or a lone chat is information, not a problem. Counting them would make a healthy fleet read as needing attention.
- **D12, PNG rendering and the `tui-ux` visual review are a maintainer pre-merge step.** uxlab's PNG half needs devbox `charm-freeze` (`api/cmd/uzi/uxlab/devbox.json`), which an egress-restricted uzi worker cannot be relied on to fetch. The run still generates the scenes under `go test` and asserts on the frames.
- **D13, collapse goes to the top pane's view.** With the floor on top, this is today's behaviour exactly. Rejected: collapsing to whichever pane has focus, which would change shipped behaviour for ci and pulls.
- **D1 amended by D14** (strip text only; the key numbering stands).

Maintainer review of the implementation (user decisions 2026-10-05, prototyped, rendered and reviewed by `tui-ux`; the mock was updated to match):

- **D14, no digits in the strip or footers.** The strip reads `floor  workers  pulls  ci`, as before this PRD; `1`-`4` still select the tabs and are listed in the `?` help only. Rejected: D1's `1 floor · 2 workers …` labels, which the maintainer found noisy.
- **D15, the fleet status lives on the title line, right-aligned,** on the floor and workers views in both layouts, with the fuller content (`workers · 9 · 8 online · 5/12 slots in use +1 ?cap · 1 holding · 1 draining · 6 need attention`, `factory workers` in factory scope). It narrows against the width left after the tabs and takes its own row only as a last resort. The floor's run summary (`$… 7d · N runs · a–b`) moves to the account-meter line, right-aligned. Rejected: a separate fleet row (costs a row on every screen) and the stats inside the `workers` tab label (blurs the tab boundaries).
- **D16, split titles per pane.** The split's title line shows only the top pane's tabs, `floor · workers`, mirroring the bottom divider's `pulls · ci`; the focused pane's selected tab is bracketed (`[workers]`), the other pane's selected tab is plain. Worker detail opened from the split is full screen and shows the full four-tab strip. With workers on top, the floor's account meters and run summary are hidden.
- **D17, the wide table in split.** The workers top pane follows the terminal width like full screen, replacing "80-column layout regardless of width". VERSION is sized to the longest visible version (cap 18), so `0.85.1+gba846d7` is not cut.
- **D18, colour on the shared ANDON tokens.** busy sage (a running run), idle faint, holding/⚑ amber, warn items, offline, outdated, ◷, ⇡, drift and 75-89% stall, danger items and ≥90% alarm, draining/cordoned wait, healthy readings default ink. Every signal keeps its glyph or word for NO_COLOR.
- **D19, the detail follows the mock.** One header line `worker › <name>  <state>  <kind>  up … · heartbeat …` (the uptime part drops when unknown and wraps below when narrow), lowercase tungsten section headings, an aligned key column, `▮▯` usage bars, coloured attention items, runs as `#iid title engine` with the worker phase, run stage and generation.
- **D20, navigation and spacing like the other tabs.** `→` opens (list → detail, detail → run) and `←` goes back. Blank lines, not rules, separate the header and the `selected …` readout from the table. An empty attention cell reads `—`; the owner column reads `you` for the viewer's own workers.
- **D21, the floor's worker cell never shows `?`.** `worker_name` is null for a run not yet claimed and for a finished run whose ephemeral worker was deleted (`runs.worker_id` is `ON DELETE SET NULL`). The cell reads `no worker yet` and `—` respectively. Rejected for this PRD: snapshotting the worker name on the run, which needs a migration.
- **Bug found in review:** the hosted size arrives lowercase (`l`), so KIND showed `host·?` for every live hosted worker while the uppercase demo seed hid it. The size is matched case-insensitively and the demo seed is lowercase.
