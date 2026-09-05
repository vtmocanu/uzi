# PRD #1137: TUI run detail loads progressively — run first, tail-first history with background backfill, payload cap, memoized transcript, incremental refetch

> Anchors verified against main at 9550deb on 2026-09-05 (the commit that merged PR #1135 / PRD #1130, whose request-id guards and `boardPollTimeout` this PRD builds on). Reviewed by three lenses (design, anchors, testability) before publishing; their findings are folded in.

**GitHub Issue**: [#1137](https://github.com/vtmocanu/uzi/issues/1137)
**Status**: Complete (created 2026-09-05; all milestones M1–M8 landed 2026-09-05). The worker-scoped implementation is done; the uxlab PNG pass, the `tui-ux` agent review, a dev-cluster slow-link timing session (SC1/SC2), and the web's adoption of `?tail` (D8) are deferred to a maintainer post-merge per M8.
**Priority**: Medium
**Related**: PRD #1130 / PR #1135 (board-poll resilience on slow links, merged 9550deb: the sibling for the *board*; this PRD is the *detail view* and reuses its guard shapes), issue #160 (`run logs` paging, the origin of `?limit=` and `RunLogs`' all-or-nothing loop), PRD #325 (TUI redesign, the detail view's pane model), PRD #1064 (`current_activity`, milestones on `RunDTO`: why the header/rail can render from `GetRun` alone).

## Problem

Opening a run in `uzi tui` (enter on the board, or `uzi tui --run <id>`) shows a bare header and `loading…` until the **entire** transcript has been fetched, and on a slow link that is a long, blank wait. Three independent causes, all measured on 2026-09-05 against the dev cluster (`https://uzi.dev.metaminds.com`, CLI v0.77.0) — none of them is the compression, which is already on (`chimw.Compress(5)` on `/{id}/messages`, `api/internal/handler/handler.go:727`).

### 1. The load is whole-transcript, serial, and gates every pixel

`loadDetailCmd` (`api/cmd/uzi/tui.go:465-475`) does `GetRun`, then `RunLogs(ctx, runID, 0)`. `RunLogs` (`api/internal/uzicli/client_runs.go:49-84`) pages from seq 0 in `logsPageSize = 200` pages (`client.go:488`), **serially**, stopping only on an empty page, and returns one slice or nothing (all-or-nothing, by design for `uzi run logs`). The single `detailLoadedMsg` (`tui.go:130-135`) lands only when the last page has, and `renderDetail` (`tui_detail.go:491-494`) draws `loading…` until `d.loaded`. The header (`detailHeaderLines`, `tui_detail.go:368`) reads `d.run`, which is zero until that same message, so even the title, status, milestones and accounts wait for the transcript. Live socket frames that arrive meanwhile (`openStreamCmd` runs in the same `tea.Batch`, `tui_board.go:283`, `tui.go:297`) are folded in by `applyEvents` but hidden behind the same gate.

| run | messages | raw JSON | gzipped | requests | fetch, fast link |
|---|---|---|---|---|---|
| #1064 (`02854d5e`) | 2996 | 6.7 MB | 2.1 MB | 16 | 2.2 s |
| #836 (`8c62fd9a`) | 2560 | 7.7 MB | 3.1 MB | 14 | 4.0 s |
| #966 (`d8648b31`) | 2461 | 5.1 MB | 1.4 MB | 14 | 5.1 s |

On a 2 Mbit/s link the gzipped bytes alone are 6–12 s, plus 14–16 serial round trips, before the first transcript line can appear. The web SPA has the same unbounded fetch (`web/src/lib/api.ts:948-954`) but in **one** request, and it renders only the newest 500 past 1000 messages behind a "Show n earlier messages" expander (`web/src/components/ActivityFeed.tsx:387-388`, `specs/ai.md` §"Performance / a11y"); the TUI has no such cap and 14+ round trips on top.

### 2. ~90% of the bytes are never drawn

Per-kind payload bytes for #966 (2461 messages):

| kind | n | bytes | share |
|---|---|---|---|
| tool_result | 1171 | 3,345,007 | 65% |
| tool_use | 1152 | 1,147,325 | 22% |
| text | 103 | 68,531 | 1.3% |
| plan / status / finding | 35 | 23,194 | <1% |

The TUI renders a `tool_result` as ONE line, `resultSummary` (`api/cmd/uzi/tui_detail_transcript.go:239-264`) → `compactText` (`render.go:204-219`, **capped at 200 runes**), and a `tool_use` as its name plus `toolArgPreview` (`tui_detail_transcript.go:215-233`, same 200-rune cap). So the two kinds that are 87% of the download are drawn from at most ~400 runes each. `text`/`thinking`/`plan` (rendered in full through `renderer.Markdown`) are 1–2% of the bytes.

Capping only those two kinds' bulky fields on the server (measured on #836, 7.09 MB of payload): a 2048-byte cap → 2.28 MB (3.1×), 1024 → 1.65 MB (4.3×), 4096 → 3.05 MB (2.3×).

### 3. Every redraw rebuilds every transcript line

`renderTranscript` (`tui_detail_transcript.go:356-398`) calls `buildTranscriptLines(lane)` (`:37-180`) on **every** `View()`, and a scroll key calls it twice (`transcriptExtent`, `:348-354`, then the render). Nothing is memoized except the lead context meter (`leadCtx`, `tui_detail.go:48-49`). A throwaway in-process harness on the 2996-frame run (`tuiModel` at 200×50, deleted after measuring, Apple silicon — **indicative figures, not gates**, see M6):

| operation | cost |
|---|---|
| `applyLoaded` (2996 msgs → frames → 40 lanes) | 0.4 ms |
| `View()`, all-agents lane (2996 frames) | **94 ms** |
| `View()`, lead lane (215 frames) | 16 ms |
| ↑ keypress + `View()` on the all-agents lane | **188 ms** |
| one live frame (`applyEvents` + `View()`) | 94 ms |

The blink tick redraws every 500 ms (`blinkInterval`, `tui.go:345`) while any milestone is in progress, so an open big run burns ~20% of a core doing nothing, and every keypress lags ~190 ms — on a small SSH box, more.

### 4. Three paths refetch the whole transcript from seq 0

- The D8 poll fallback (`pollFallbackMsg`, `tui.go:782-786`; entered from `streamReadyMsg` err at `:738` / `streamEventsMsg` closed at `:757`) calls `loadDetailCmd` every 2 s → `RunLogs(…, 0)`: the full 2–7 MB transcript every 2 seconds, precisely on the links where the socket is most likely to fail. The comment says it mirrors `uzi run logs --follow`, but that loop advances `seq` (`run_get.go:97-110`). It also issues a fresh `GetRun` per tick with none of the #1130 guards (the board tick's meta path is skipped while `polling`, so the fallback is the sole meta driver and can pile up exactly as #1130's board poll did).
- The `r` key (`tui_detail.go:251-252`) → the same full reload.
- `RunStream.replay` (`api/internal/uzicli/stream.go:473-500`) replays from `s.lastSeq`, which starts at **0** and only advances on frames the stream itself emitted (`:181`, `:425-426`, `:493`). A socket that drops before its first live frame replays the entire history on reconnect; the TUI dedups by seq (`addFrame`, `tui_detail.go:156-170`) so it is correct, just a full download. `StreamRun` has one production consumer, `tui.go:495`.

## Solution

Make the detail view **progressive** and every refetch **incremental**, on a small additive API surface that the web can adopt later (D8):

1. **Run first.** `GetRun` becomes its own message. The header, the crew-rail milestones/accounts and the "now" line render at one round trip; only the transcript pane waits.
2. **Tail-first history, background backfill.** The transcript fetches the newest page (`?tail=200`) and renders it, then walks older pages (`?before=<lowest seq>&limit=200`) in the background until the log is complete, prepending as they land. A badge on the pane title shows progress (`⇡ loading earlier · 1,240 of 2,996`; the count is free because `seq` is gapless from 1, so the highest seq held is the total).
3. **Payload cap on the wire.** An opt-in `?payload_max=<bytes>` on `GET /api/runs/{id}/messages` trims the two fields the TUI never draws in full (`tool_result.content`, `tool_use.input` string values, with the three identity keys the "now" line reads exempt), preserving every other key, and marks trimmed messages `payload_truncated: true`. The TUI asks for 2048.
4. **Memoized transcript lines.** A per-lane line cache keyed on the render inputs (lane, width, theme, identities, frame extent), with an append fast path for live frames and a full rebuild on a prepend. An unchanged `View()` costs a map lookup; a scroll key one lookup, not two builds.
5. **Incremental everywhere.** The poll fallback, the `r` key and the stream's reconnect replay all continue from the highest seq the view holds (`RunStream.NoteSeen`), guarded so they cannot pile up on a slow link (the #1130 pattern, reused as-is for the meta refresh).
6. **CLI parity.** `uzi run logs --tail N` on the same page contract.

## API contract — `GET /api/runs/{id}/messages` (additive)

Existing (unchanged): `?after=<seq>` (default 0; ascending, unbounded) and the opt-in `?limit=<n>` bounded page (`ListRunMessages`, `api/internal/handler/runs_lifecycle.go:412-475`; `maxRunMessagesPage = 1000` at `:415`).

New query parameters, all validated in the handler:

| param | meaning | rules |
|---|---|---|
| `tail=<n>` | the newest `n` messages, returned in **ascending** seq | `n ≥ 1`, clamped to `maxRunMessagesPage`; **400** if combined with `after`, `before` or `limit` |
| `before=<seq>` + `limit=<n>` | the newest ≤`n` messages with `seq < before`, ascending | `before ≥ 1`; `limit` **required** (400 without it, same clamp); **400** if combined with `after` or `tail` |
| `payload_max=<bytes>` | trim bulky payload fields (below) | `≥ 1`; combinable with every form above; never changes which rows are returned |

Response shape is unchanged (`{"messages":[MessageDTO…]}`). `MessageDTO` (`api/internal/apitypes/run.go:523-537`) gains `PayloadTruncated bool \`json:"payload_truncated,omitempty"\``, present only on a message whose payload was actually trimmed. The web keeps sending neither param and sees no change (an absent `omitempty` key).

**Store.** One new sqlc query in `api/internal/store/queries/runtime.sql`, next to `ListRunMessagesAfterPage` (`:2224-2235`):

```sql
-- name: ListRunMessagesBeforePage :many
-- <carry the column-identity warning the two siblings carry at :2213-2219 / :2227-2232,
--  naming all THREE queries: new columns are APPENDED to every one of them in table order,
--  or sqlc mints a per-query Row type and breaks workersvc.Store's []store.RunMessage>
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq < @before_seq
ORDER BY seq DESC
LIMIT @lim;
```

Same column list and order as the existing two. **The DESC order is reversed to ascending in Go by the service**, not by a SQL subquery (D3). `UNIQUE (run_id, seq)` (`00020_workers_runs.sql:78`) serves the backward scan; no migration, no new index (`idx_run_messages_tool_use_seq` from 00187 is partial on `kind='tool_use'` and irrelevant here). `tail` is `before_seq = math.MaxInt32` (`seq` is `int32` in the DTO and `int` in the table; `2147483647` is a valid upper bound for both). Run `sqlc generate` (pinned `sqlc@v1.31.1`, `.claude/rules/go.md`) and commit `runtime.sql.go`.

**Service.** `workersvc.ListRunMessagesForViewerBefore(ctx, userID, isAdmin, runID, beforeSeq, limit)` beside `ListRunMessagesForViewerPage` (`api/internal/workersvc/service.go:3746-3755`): the same `GetRunForViewer` owner-or-admin gate, then the query, then reverse. Add `ListRunMessagesBeforePage` to the `workersvc.Store` interface (`service.go:575-576`) and to the handler test's store fake (`api/internal/handler/runs_test.go`, the one with `lastPageLim` at `:63,112`). **The fake returns rows DESC by seq**, mirroring `ORDER BY seq DESC`, and captures `lastBeforeSeq` / `lastBeforeLim`: with an ASC fake the service reverse is untested and the mutation seam inverts (the handler test wraps the real `workersvc.New` over the fake store, `runs_test.go:193`).

**Payload trim (`payload_max`)** — a pure handler-side function `trimPayload(raw json.RawMessage, kind string, max int) (out json.RawMessage, truncated bool)`, applied per message when the param is set and `len(raw) > max`:

- `tool_result`: `content` as a string → cut to `max` bytes at a rune boundary, append `…`; `content` as an SDK block array → keep `{type:"text",text}` blocks only (drop image/other blocks, which are the largest payloads), cutting the cumulative text to `max`. Everything else (`tool_use_id`, `is_error`, and any other key) verbatim.
- `tool_use`: each **string** value of `input` cut to `max` bytes at a rune boundary with `…`, **except the identity keys `subagent_type`, `description` and `file_path`, which are never cut**: `runactivity.FromFrame` (`api/internal/runactivity/runactivity.go:119-121,137`) reads them for the "now" line, and the TUI runs it client-side over the frames it holds (`railActivity`, `api/cmd/uzi/tui_detail_rail.go:245-266`), so a cut there would corrupt the now-line agent/detail whenever `detailPayloadMax` is small (tests shrink it). Non-string values and every other key (`id`, `name`, `usage`, …) verbatim.
- Every other kind: never trimmed (text/thinking/plan/status/finding are drawn in full).
- Output is always valid JSON. Any unmarshal failure returns the payload untouched with `truncated=false` — a malformed model-authored payload never fails the request. `truncated` is true only when bytes were removed.
- **Invariants, tested**: `usage` and `context` objects survive on every kind (measured in #836: `usage` rides on 1208 `tool_use` + 86 `text` + 8 `status` frames and `context` on 8 `text` frames; `leadContextFill`, `api/cmd/uzi/tui_context.go:52-88`, reads both), and a trimmed `tool_use` fed to `runactivity.FromFrame` yields the same agent/label/detail as the untrimmed one.

**Contract fixtures.** The new DTO field is recorded in `fixtures/api-contract/message.full.json` (`apitypestest.Populate` sets every bool `true`, so the key appears there; `message.zero.json` is unchanged because a false `omitempty` bool is dropped) and the TS `RunMessage` (`web/src/lib/apiTypes.ts:2729`) gains `payload_truncated?: boolean`. Re-record per `fixtures/api-contract/README.md` ("Recorded, not authored": copy the Go test's print-on-mismatch output; there is no `-update` flag), then run **both** halves with `-count=1` (the README's cache note: `fixtures/` is outside the Go module) — `api/internal/apitypes/contract_test.go` and `web/src/lib/apiContract.test.ts`.

## Client contract — `uzicli`

- `Client` interface (`api/internal/uzicli/client.go:25`; insert beside `RunLogs` at `:29-34`): add `RunLogsPage(ctx, id string, q LogsPageQuery) ([]apitypes.MessageDTO, error)` with `LogsPageQuery{After, Before, Tail, Limit, PayloadMax int32}` — **one request, no loop**, mirroring the server's exclusivity rules client-side (a bad combination returns `ExitUsage` before any request). `RunLogs` (the all-or-nothing loop) is unchanged and still what `uzi run logs` and `RunStream.replay` use.
- `FakeClient` (`fake.go`, `fake_runs.go`): `RunLogsPage` filters the seeded `LogsByID[id]` slice (`fake.go:36`; the same source `RunLogs` filters at `fake_runs.go:46`, and the one `newDemoClient` seeds at `demo.go:117`) by the query (tail = last n; before = the n newest below; after = ascending from), and records every call in `RunLogsPageCalls []LogsPageQuery` so a test can assert **positive counts and exact queries** (the #1130 `ListRunsCalls` style — never a vacuous "not called"). Because the demo drives the real model through this fake, `uzi tui --demo` pages for free once M3 lands; M4/M5 only need to seed enough messages to exercise more than one page.
- `RunStream.NoteSeen(seq int32)` (`stream.go`): raises `lastSeq` to `max(lastSeq, seq)`. **`lastSeq` is today confined to the pump goroutine and touched lock-free at `:425-426` (live frame), `:474`, `:477` and `:493` (replay); `s.mu` guards only `err`/`lastStatus`.** A `NoteSeen` from the TUI goroutine with only its own write locked establishes no happens-before edge, and `-race` (on in `gate:api`) reddens. So M3 makes `lastSeq` an `atomic.Int32` (or moves **all five** existing accesses plus `NoteSeen` under `s.mu`) in the same commit. Test: a stream seeded with `NoteSeen(200)` that reconnects calls `RunLogs` with `after == 200` (assert the recorded `after`; on unfixed code it is 0), green under `-race`.

## TUI design (`api/cmd/uzi/tui*.go`)

Messages (all carry `runID`; keep the existing stale-run guard, `tui.go:656-670`):

- `detailRunMsg{runID, run, err}` — the first `GetRun`. Applies `run` **including status** (nothing to preserve yet) and sets `runLoaded`. The periodic meta refresh stays on `detailMetaMsg` / `refreshRunMetaCmd(runID, reqID)` / `startDetailMetaReq` exactly as #1135 left them (`tui.go:416`, `:480`, `:677`; request-id guard, `boardPollTimeout` at `:38`), with one change: while `polling` (socket down) `applyMeta` **adopts** the DTO status, because the stream that normally owns it is gone (today the fallback owns status via `applyLoaded`; that path goes away).
- `detailPageMsg{runID, kind (pageTail | pageBackfill | pageCatchup), msgs, err}` — one page. `tail` sets `tailLoaded`; a non-empty `backfill` page prepends and chains the next `before = lowSeq`; an empty backfill page sets `historyComplete`; a non-empty `catchup` page appends and chains `after = highSeq` until empty. A failed page is **not** retried in a loop: the badge reads `earlier history unavailable · r`, `historyComplete` stays false, and `r` resumes from `lowSeq`. A backfill page whose max seq is not `< before` (a hostile or broken server) stops the chain the same way (the mirror of `RunLogs`' "did not advance" guard, `client_runs.go:80-82`).

State (`detailState`, `tui_detail.go:34-84`): replace the single `loaded` with `runLoaded`, `tailLoaded`, `historyComplete`, `backfilling`; add `lowSeq`/`highSeq` (0 = none held) and an in-flight marker for the catch-up chain (`catchupWaitID`, minted like #1135's `metaWaitID` at `:77-78`). `frames` stays **seq-sorted**: `addFrames(page)` prepends when `max(page) < lowSeq`, appends when `min(page) > highSeq`, and otherwise merges with a stable sort by seq; `seen` dedup is unchanged. `loaded` today gates the transcript only; after this PRD the header renders from `runLoaded` and the pane from `tailLoaded || len(frames) > 0` (a live frame that beats the tail is shown, not hidden).

Commands: `loadRunCmd`, `loadTailCmd` (`Tail: detailPageSize`, `PayloadMax: detailPayloadMax`; both package vars so tests shrink them), `backfillCmd(before)`, `catchupCmd(after)`. Drill-in (`tui_board.go:283`) and `initCmds` (`tui.go:297`) batch `loadRunCmd + loadTailCmd + openStreamCmd`. `detailPageSize = 200` (= `logsPageSize`), `detailPayloadMax = 2048` (D4).

Retirement: `detailLoadedMsg` (`tui.go:130-135`), `loadDetailCmd` (`:465-475`) and `applyLoaded` (`tui_detail.go:90-102`) are **deleted with every caller in the same commit; compilation is the guard** (not `deadcode`, which runs with `-test` and treats a test-only caller as live). `detailLoadedMsg{` is constructed at 54 sites in 9 files at 9550deb: `tui.go` (2, production), `tui_model_test.go` (30), `uxlab_gen_test.go` (6), `tui_steer_test.go` (5), `tui_ratelimit_test.go` (3), `tui_detail_fixes_test.go` (3), `tui_blink_test.go` (3), `tui_osc_test.go` (2), `tui_detail_test.go` (1), `tui_all_lane_test.go` (1). Migrate them through **one test helper** `applyDetail(t, m, run, msgs) tuiModel` that sends `detailRunMsg{run}` then `detailPageMsg{kind: pageTail, msgs}` through `Update`, so every existing transcript assertion keeps its channel: `tui_model_test.go` has tests that pass `msgs` and then assert transcript content/viewport/scroll (e.g. the "frame 8" / "frame 1 body" cases), and a mechanical swap to `detailRunMsg` alone would leave `tailLoaded=false`, the pane at `loading…`, and those assertions silently testing nothing. `demo.go` constructs no message — it drives the real model's load commands through its `FakeClient` — so it changes only via the fake.

Stream: on `streamReadyMsg` and after every applied tail/catch-up page, call `stream.NoteSeen(highSeq)`. The socket-before-tail race (a reconnect in the window before the tail lands still replays from 0) is accepted: the window is one round trip and dedup keeps it correct.

Poll fallback (`pollFallbackMsg`): the meta refresh goes through `startDetailMetaReq` **only while `metaWaitID == 0`** (the #1135 guard, so a slow `GetRun` never stacks — today's `loadDetailCmd`-per-tick has no guard at all), plus `catchupCmd(highSeq)` **only when no catch-up chain is in flight** (`catchupWaitID == 0`); the backfill chain is independent of the socket and keeps running. `r`: the same pair, plus `backfillCmd(lowSeq)` when `!historyComplete && !backfilling`.

Render: the pane-title badge slot (`followBadge`, `tui_detail_transcript.go:400-414`) gains the backfill state, left of the follow badge: `⇡ loading earlier · <held> of <highSeq>` while backfilling, the unavailable form above on a failed page, nothing once complete. The rail: lanes appear as their first frame lands; a prepend can move an older lane above (first-seen order over seq-sorted frames), the selection is by key (`rebuild`, `tui_detail.go:172-206`) so it never jumps. **Scroll anchoring**: when a backfill page prepends while `!follow`, shift `scroll` by the selected lane's line-count delta (the cache knows both counts) so the line under the cursor stays put; with `follow` the window is bottom-anchored and nothing moves.

Memo (`transcriptCache`, D6): a pointer field on `detailState` (allocated in `newDetailState`, `tui_detail.go:86`; nil-safe), holding per-lane `{key: laneKey, width, dark, profile, identitiesSig, n, firstSeq, lastSeq, lines []string}` plus an unexported `builds int` bumped on every miss (the test's only channel, since `View` has a value receiver and cannot report through the model). `buildTranscriptLines` becomes the miss path behind `transcriptLines(lane)`: a full-key hit returns the cached slice; a hit on everything but `(n, lastSeq)` where the cached frames are a prefix (same `firstSeq`, `n_cached ≤ n`, `frames[n_cached-1].Seq` equal) re-renders **from frame `n_cached-1`** (its separator depends on frame `n_cached` — `tightNext`, `:63-81`) and appends; anything else rebuilds. `identitiesSig` is the sorted `key=tag` join of `laneIdentities()` (`tui_detail_rail.go:93-103`) so a new lane that re-suffixes an existing role (`tester` → `tester·1`) invalidates the aggregated lane. `transcriptExtent` reads the same cache, so a scroll key is one lookup.

CLI: `uzi run logs --tail N` (`run_get.go:183-184` flags) prints the newest N via one `RunLogsPage{Tail: N}`; with `--follow` the existing drain loop continues from the highest printed seq. `--tail` with `--after` is a usage error (`ExitUsage`, `uzicli/output.go:24`). Update `docs/cli.md` (the paging paragraph, `:493-500`) and the embedded skill's usage line (`api/internal/uzicli/skill/SKILL.md:147`), then `task docs:sync` and commit the mirror (`TestEmbeddedDocsMatchSource`).

## Milestones

Each milestone: regression tests that fail on the unfixed seam and pass with the fix, watched in both directions (`.claude/rules/go.md` mutation discipline); the component gate run once to a log; no `.github/workflows/**` in the branch diff, implementation or validation. TUI seam tests use the `Update→msg` / `View().Content` string conventions of `tui_model_test.go`.

- [x] **M1 — API: `tail` / `before` pages.** The sqlc query (with the three-way column note), `sqlc generate`, the service method + `Store` interface + the handler-test store fake (DESC rows, `lastBeforeSeq`/`lastBeforeLim` captures), and the handler parsing/validation table above. Tests in `handler/runs_test.go` beside `TestListRunMessagesLimit` (`:471`) and `TestListRunMessagesPagedViewerAuthz` (`:423`): on a 5-message run `tail=2` → `[4,5]` (ascending — this pins the Go reverse because the fake returns `[5,4]`); `before=4&limit=2` → `[2,3]`; `before=2&limit=2` → `[1]`; `before=1&limit=2` → `[]`; `tail` above `maxRunMessagesPage` is clamped **before** the store call (assert the forwarded `lastBeforeLim`, as `:535-550` does for `lastPageLim`); every forbidden combination and `before` without `limit` → 400 with **zero** store calls; the owner/admin/other authz triple on the `tail` path. `task gate:api` green.
- [x] **M2 — API: `payload_max` + `payload_truncated`.** `trimPayload` with table-driven tests: string content, block-array content (image block dropped, text kept), `tool_use` input strings cut at a rune boundary (a multi-byte rune straddling the cut is dropped whole, output is valid UTF-8 JSON), the three identity keys left intact even when longer than `max`, non-string input values untouched, other kinds untouched, malformed payload returned verbatim with `truncated=false`, `usage`/`context` preserved on `tool_use`/`text`/`status`, `is_error`/`tool_use_id`/`id`/`name` preserved, flag set only when bytes were removed, and `runactivity.FromFrame` over a trimmed `tool_use` equal to the untrimmed result. Handler test: `payload_max=64` on a seeded 5-message run returns the same seqs with only the oversized ones flagged. Re-record `message.full.json` and add the TS field; both contract tests green with `-count=1`. `task gate:api` + `task gate:web` green.
- [x] **M3 — `uzicli`: `RunLogsPage`, `FakeClient`, `RunStream.NoteSeen`, `run logs --tail`.** Client tests on an `httptest` server asserting the exact query string per `LogsPageQuery` form and the client-side `ExitUsage` on a forbidden combination (no request made — assert the server saw zero hits, paired with a positive hit count on the valid forms). `lastSeq` made atomic (or fully mutex-guarded) and the `NoteSeen` reconnect test as specified, green under `-race`. `--tail` on `uzi run logs` with its command test (`run_get`'s existing tests are the pattern), `docs/cli.md` + skill usage line + `task docs:sync`. `task gate:api` green.
- [x] **M4 — TUI: run-first + tail-first, retire `detailLoadedMsg`.** `detailRunMsg`, `detailPageMsg{pageTail}`, `loadRunCmd`/`loadTailCmd`, the `runLoaded`/`tailLoaded` state, the `applyDetail` helper and the 54-site migration, `detailLoadedMsg`/`loadDetailCmd`/`applyLoaded` deleted in the same commit. Seam tests: after `detailRunMsg` alone the header shows title + status and the pane shows `loading…`; after the tail page the pane shows the newest frames; the fake records exactly one `Tail{detailPageSize}` and no `After: 0`; a live frame arriving before the tail renders instead of hiding; every pre-existing transcript test still passes through the helper (the migration's own check: a test that asserted transcript content must still find it). `task gate:api` green.
- [x] **M5 — TUI: background backfill, prepend, scroll anchoring, badge.** `detailPageMsg{pageBackfill}`, `backfillCmd`, `addFrames` (prepend / append / merge), `lowSeq`/`highSeq`, `historyComplete`/`backfilling`, the badge states, the failed-page and did-not-advance stops, `r` resuming. Seam tests: the fake records the exact chain `Tail{200}` → `Before{2797,200}` → … → an empty page, after which the badge is gone and `historyComplete` is set; a backfill page prepends (frames stay seq-sorted, lanes gain the older agent, the selection key is unchanged); a page interleaved with a live frame merges in seq order with no duplicate; a paused viewport keeps its top line across a prepend (assert the same line text at the window top); a failed backfill page shows the unavailable badge and `r` issues `Before{lowSeq}`; a page that does not go below `before` stops the chain. Seed the demo fake with more than `detailPageSize` messages and drive-test with `go run ./cmd/uzi tui --demo`. `task gate:api` green.
- [x] **M6 — TUI: memoized transcript lines.** `transcriptCache` as specified. Tests, all on the `builds` counter (the deterministic channel; **no timing assertion**): two consecutive `View()` calls with no state change build once; a live frame appended to a 3-frame lane re-renders from the previous last frame only (assert via the tool_use→tool_result tight pairing: the separator of the previously-last `tool_use` becomes tight when its result lands, which a naive append would get wrong); a resize, a theme flip and a re-suffixed identity each miss; a prepend rebuilds; a scroll key triggers one build. Record in the PR, as evidence only, `View()` on the 2996-frame all-agents lane with no change and a scroll key against the 94 ms / 188 ms baselines. `task gate:api` green (`-race` covers the pointer-shared cache).
- [x] **M7 — TUI: incremental fallback, `r`, stream floor.** Poll fallback and `r` as specified, with the meta refresh under the #1135 guard and the catch-up chain under its own. Tests: two consecutive `pollFallbackMsg` ticks with no reply issue **exactly one** `RunLogsPage{After: highSeq}` and **exactly one** `GetRun` (positive counts via `RunLogsPageCalls` and the #1135 `LastGetRunCtx`/call counters), and never a `Tail`/`After: 0`; a `detailMetaMsg` while `polling` adopts the DTO status, while streaming it preserves the stream's status (the existing `applyMeta` rule); `r` issues `After: highSeq` (+ `Before: lowSeq` when incomplete); `streamReadyMsg` after a tail page calls `NoteSeen(highSeq)` (fake stream records it). `task gate:api` green.
- [x] **M8 — specs, docs, full gate.** `specs/ai.md`: append a new section numbered **max existing + 1** (`grep -n '^## [0-9]' specs/ai.md | tail`; §619 is PRD #1130's and §620 was taken by PRD #1136 while this ran, so it landed as §622 (§621 went to PRD #1140 meanwhile); `task check:spec-numbering` in `gate:repo` validates), terse contract: the three new query params, the trim invariants incl. the exempt identity keys, the TUI progressive-load invariants (run-first, tail-first, seq-sorted frames, incremental refetch, one meta refresh and one catch-up chain in flight, cache keyed not invalidated). `specs/human.md`: under *Feature #325* append the one user-stated line quoted in D9. `docs/cli.md` from M3 already synced. Full `task gate` green; `git diff --name-only origin/main..HEAD -- .github/workflows/` empty. **Out of scope for the worker (maintainer post-merge):** the uxlab PNG pass and the `tui-ux` agent review (need the local devbox env), a slow-link session on the dev cluster (SC1/SC2 timing), and the web's adoption of `tail` (D8).

## Success criteria

1. Opening a 3000-message run over a slow link shows the header (title, status, milestones, accounts, "now" line) after **one** `GetRun` round trip and the newest transcript page after one more, independent of the rest of the history (proven by the M4 seam test on message order; timed post-merge on the dev cluster).
2. The bytes fetched to first transcript paint drop from the whole gzipped transcript (1.4–3.1 MB on the three measured runs) to one page of ≤200 trimmed messages (expected ≤ 150 KB gzipped); the full history, when complete, is ~3× smaller than today (measured 7.09 MB → 2.28 MB payload at `payload_max=2048` on #836).
3. An unchanged `View()` performs **zero** line builds and a scroll key **one** (the M6 counter tests); the millisecond figures are PR evidence, not a gate.
4. With the socket down, no request ever carries `after=0` or `tail` after the first load; at most one meta refresh and one catch-up chain are in flight; a reconnecting stream replays from the highest seq held.
5. A run whose backfill is interrupted shows an explicit badge and `r` resumes it; frames are always seq-sorted and deduped; a paused viewport does not jump when older pages land.
6. The web SPA's behaviour and wire shape are unchanged (no new params sent; `payload_truncated` absent); both API contract fixtures and tests green.
7. `uzi run logs --tail N` prints the newest N and `--follow` continues from there; the embedded docs mirror matches.
8. `task gate` green under `-race`; no `.github/workflows/**` in the diff.

## Decision Log

- **D1 — tail-first with background backfill, not on-demand paging.** On-demand ("load earlier" on scroll-up) would leave the crew rail lying: lanes are derived from the frames held (`buildLanes`, `tui_lanes.go:196-226`), so an agent that finished early would be absent until the user scrolled up. Backfilling in the background makes the rail converge on its own within seconds while the user already reads the tail; the badge says it is still converging. A scroll-up past the top does not need to trigger anything.
- **D2 — `tail`/`before` as query params on the existing endpoint, not a new route.** Same authz (`GetRunForViewer`), same gzip wrapper (`handler.go:727`), same DTO; the web's `getRunMessages` keeps working unchanged. Mutual exclusion is enforced with 400s rather than defined precedence, so an ambiguous request never silently returns the wrong window.
- **D3 — reverse in Go, not in SQL.** A `SELECT … FROM (… ORDER BY seq DESC LIMIT n) ORDER BY seq ASC` subquery risks sqlc minting a per-query Row type; the store queries carry an explicit note that the column list must stay identical so the row remains `store.RunMessage` (`runtime.sql:2213-2219`). A ten-line reverse in the service keeps the contract, and the handler-test fake returning DESC keeps the reverse under test.
- **D4 — trim only `tool_result.content` and `tool_use.input` strings, exempt the three identity keys; TUI asks for 2048.** These two fields are 87% of the bytes and are drawn from ≤200 runes each. `subagent_type`, `description` and `file_path` are exempt because the "now" line (`runactivity.FromFrame`, run server-side over the store *and* client-side over the held frames in `railActivity`) reads them verbatim; they are short by nature, so exempting them costs nothing. Trimming any other key risks the lead context meter (`usage`/`context` ride on `tool_use`/`text`/`status` frames) and the tool pairing (`id`/`tool_use_id`). 2048 bytes leaves headroom over the 200-rune cap (≤800 bytes of UTF-8) for leading whitespace and newlines `compactText` folds away before capping. The flag lives on the DTO (`payload_truncated`), not inside the model-authored payload, so the payload's shape stays the SDK's.
- **D5 — no `last_seq` on `RunDTO`.** It is deliberately omitted as worker-internal (`apitypes/run.go:71-72`). The TUI does not need it: `seq` is caller-supplied on insert (`InsertRunMessage`, `runtime.sql:2201-2206`) and measured gapless from 1 on every sampled run, so the highest seq held after the tail page is the total, which is what the progress badge shows.
- **D6 — cache keyed by inputs behind a pointer, never explicitly invalidated.** `tuiModel.View` has a value receiver (`tui.go:709` region; verified against `charm.land/bubbletea/v2@v2.0.9`: `Update` and `View` run sequentially on the one event-loop goroutine, and the renderer only flushes an already-computed frame), so a cache written in `View` on a value field is lost; a pointer field persists across copies and is never shared across goroutines. Keying on every render input (lane, width, theme, profile, identities, frame extent) means a missed invalidation site is impossible by construction — a stale entry simply never matches. `-race` in `gate:api` stands guard.
- **D7 — the meta refresh and the catch-up chain are guarded, the backfill chain is not.** Both the fallback's `GetRun` and the catch-up are *re-issued* by a 2 s tick, so they pile up on a slow link exactly as #1130's board poll did: the meta refresh reuses #1135's `startDetailMetaReq`/`metaWaitID` guard unchanged, the catch-up gets its own request id. A backfill page is only ever issued by the previous page's reply, so it self-serialises. Pages use the client's default 30 s timeout, not `boardPollTimeout` (10 s): a 200-message trimmed page on a very slow link can legitimately take longer than a `GetRun`.
- **D8 — web adoption deferred.** The web would benefit from `tail=1000` + `before` behind its existing "Show n earlier" expander (`ActivityFeed.tsx:387-388`), replacing the unbounded fetch. It is a separate PRD: `useRunStream` (`web/src/lib/useRunStream.ts`) owns replay/catch-up with its own seq bookkeeping and deserves its own tests. This PRD keeps the web byte-for-byte unaffected (SC6).
- **D9 — `specs/human.md` line (needs the user's approval; approving this PRD is that approval).** Under *Feature #325*: "The run detail opens on the run and its newest messages first and fills older history in the background, so a slow link never sits on an empty pane; refetches continue from what is already held. [user, #1137]".
- **D10 — `RunLogs` stays all-or-nothing.** `uzi run logs` (issue #160) and `RunStream.replay` keep the loop; the TUI needs per-page delivery for progressive rendering, which is why `RunLogsPage` is a separate single-request verb rather than a mode on `RunLogs`.
- **D11 — one test helper for the retirement, not 54 hand edits.** `detailLoadedMsg` is the seed of most TUI tests; retiring it by hand at 54 sites is where a transcript assertion silently loses its channel. `applyDetail` reproduces the old one-shot (run + full transcript) through the two new messages, so the migration is mechanical and every assertion keeps meaning the same thing.

## Dependencies & conflicts

- **PR #1135 (PRD #1130) is merged (9550deb); branch from main at or after it.** This PRD reuses its shapes rather than working around them: `refreshRunMetaCmd(runID, reqID)` + `startDetailMetaReq` + `metaWaitID` for the meta refresh, `boardPollTimeout`, the `FakeClient` call counters / captured contexts, and `specs/ai.md` §619 (this PRD landed as §622; #1136 took §620 and #1140 §621 first). Extend those fields; do not rename them.
- No migration, no schema change, no new DB index.
- No `.github/workflows/**` change, implementation or validation (`.claude/rules/prds.md`).

## Risks & mitigations

- **A trimmed payload breaks a consumer that expects the full shape.** Only the TUI sends `payload_max`; the trim touches two keys, exempts the identity keys, output is always valid JSON, and the DTO flag lets any future consumer know. M2's tests pin the preserved keys per kind and the now-line equality.
- **Prepend races a live frame or a catch-up page.** `addFrames` merges with a stable sort when a page is neither strictly below nor strictly above what is held; `seen` dedups. M5 tests the interleaving explicitly.
- **The memo returns stale lines.** Keyed on every input, including `identitiesSig` and `profile`; the append fast path re-renders the previous last frame (the only block whose separator depends on its successor). M6 tests each miss condition and the tight-pairing case.
- **Backfill never completes on a hostile or broken server.** The did-not-advance stop plus the unavailable badge (M5).
- **`NoteSeen` introduces a data race.** M3 changes every `lastSeq` access, not just the new one, and the reconnect test runs under `-race`.
- **The retirement silently defangs existing transcript tests.** D11's helper; M4's acceptance is that every migrated transcript assertion still finds its content.
- **The fallback's `GetRun` piles up on a slow link.** It goes through the #1135 guard (D7); M7 asserts exactly one `GetRun` across two ticks.
