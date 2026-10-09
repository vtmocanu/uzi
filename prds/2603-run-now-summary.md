# PRD #2603: Model-written "Now" summary for a running run

**Status**: Draft
**Priority**: Low
**Umbrella**: #2601
**Blocked by**: #2602 (the progress card and detail block this line sits in)
**Design mock**: `prds/mockups/2602-run-progress-mock.html` (the dotted "Now" line inside the run page progress card) and `prds/mockups/2602-run-progress-tui-mock.sh` (mock 4).

## Problem

The progress card (#2602) says where a run is in its plan, but not what the agent is doing in words. The existing "now" line names the newest tool call ("lead · running a gate"), which is accurate but low level. An owner who wants "lead is running the agent gate for the docs milestone" reads the transcript.

## Outcome

While an issue run is executing a milestone, the run page progress card, the TUI detail `PROGRESS` block and `uzi run get` show one plain sentence of at most 120 characters describing the current step. A small model on the run's own credential writes it, and the worker refreshes it as the run moves.

Acceptance examples:
1. A Claude-harness run's lead dispatches a tester lane after a reviewer lane: within 5 minutes the summary changes to one naming the testing step (tested with a fake clock).
2. The owner turns the setting off while a run is executing: the worker makes no further summary call for that run after its next poll, a result already in flight is discarded, and the line disappears from every surface.
3. A summary containing control bytes, ANSI escapes or markup renders as inert plain text on every surface.

## Out of scope

- Time remaining (see #2602 D1).
- Summaries for runs without a non-empty frozen milestone list, for non-issue kinds, or while a run is waiting, parked, queued or stalled.
- Any summary written to the forge, Slack, the PR or the judge's input.

## Modules and seams

### Summary generation (worker)

- **Seam**: every call goes through `runReadOnlyModelPass` (`agent/src/model-pass.ts`), the existing tool-less, repo-isolated advice turn (empty setting sources, deny-all hook, timeout, HOME lifecycle), with the run claim's own credential. On Codex it uses the injected production advice-harness factory; no new Codex construction site is added (`semgrep/codex-fixed-constructor.yml` allows two).
- **Models**: Claude harness `haiku` (the existing delivery-summary default, `agent/src/summary-runner.ts`); Codex harness `gpt-6-luna`. On refusal or error the summary is skipped; there is no fallback to a more expensive model.
- **Triggers**, all worker-local: a milestone transition the worker's milestone observer emits (`agent/src/milestone-progress-observer.ts`), a change of the active lane's role, or 10 minutes since the last summary with at least one new tool frame since then. At most one call per 5 minutes per run; triggers inside that window coalesce into one call at the window's end; a failed or skipped call counts against the window.
- **Gating**: the worker calls only while the run is executing a milestone (not waiting, parked or awaiting a gate) and the effective setting is on. The worker stops scheduling on park, terminal state, claim loss or credential switch, and discards any result that completes after one of those.
- **Input**: the active milestone's title and the newest 20 tool frames, each trimmed to the fields `runactivity` already reads (role, label, tool, detail; never a Bash command), each field capped at 200 runes.
- **Instruction**: describe only what the frames show; never claim that a step passed, failed or finished unless a frame states it.
- **Output**: truncated to 120 characters with control bytes stripped, then posted as a `run_messages` row of the new kind `progress_note` whose payload also carries the active milestone id at generation time.
- **Generation limits**: `runReadOnlyModelPass` and the advice request gain two optional fields, an output-token cap (256) and a reasoning effort, passed only by this caller so existing callers are unchanged. Claude harness: `haiku` with no extended thinking and the cap. Codex harness: `gpt-6-luna` at the lowest reasoning effort the app-server accepts, with the cap where the app-server supports one. Every call has a 30-second timeout and is cancelled when it expires. If a harness cannot enforce the output cap, the PR says so and the timeout is that harness's bound.
- **Usage accounting**: today `runReadOnlyModelPass` returns only text; the Codex advice path drops `AdviceResult.usage` and the Claude advice harness returns `usage: undefined` (`agent/src/claude-advice-harness.ts`). Add an optional usage callback to `runReadOnlyModelPass`, filled from each harness's result (the Claude harness reads it from the SDK result frame), and pass this caller's usage into the run's usage report as one more model entry. Existing callers pass no callback and are unchanged; tests on both harnesses prove the usage is counted once.
- **Credential side effects**: a rate-limit or error result is swallowed: it never drives `limit_wait`, a credential switch, or the run's rate-limit observer.

Resource bounds: at most 12 calls per run-hour, each with a bounded input (20 frames x 4 fields x 200 runes), the output-token cap and a 30-second timeout. The 120-character truncation is a display cap, not a cost bound. A failure never fails, pauses or slows the run.

### Settings and their delivery

- A per-user setting **on by default** (the user's decision on 2026-10-09; the cost is bounded above), shown in Settings next to the summary-model setting and in `uzi settings get`, plus an admin kill switch in instance settings. Both go through the existing settings key mechanism (`api/internal/settings/keys.go`); add a migration only if the per-user store needs one.
- The effective value (user setting AND admin switch) rides on the worker's existing poll or heartbeat response, and the worker checks the newest value before each call, so turning it off reaches running runs.

### api ingest and read

- The api accepts `progress_note` only from the claiming worker, strips terminal-unsafe runes and caps the text at 120 characters on ingest (as for `AgentLabel`), and persists it like every run message.
- `RunDTO.progress.now_note` (`text`, `at`) on the `GetRun` response, attached only when the run is eligible right now: `progress.state = percent` (which already means an executing, healthy issue run with a non-empty frozen list), a non-empty `active_milestone_id` equal to the note's milestone id, and the effective setting on. A waiting, parked, queued or stalled run therefore shows no note, even one written for its still-active milestone. A partial index on `run_messages (run_id, seq) WHERE kind = 'progress_note'`, created concurrently (migration number assigned at merge), keeps the read bounded.
- Mirrors for the new kind: `agent/src/protocol.ts` `MessageKind`; the web transcript (`web/src/components/RunEvent.tsx`, `ActivityFeed.tsx`), the TUI transcript (`api/cmd/uzi/tui_detail_transcript.go`), `uzi run logs` and the stream: hidden from transcripts (the card shows it). Slack and the judge's trace compaction exclude it.

### Surfaces

The web card renders the note under the phase chip; the TUI detail `PROGRESS` block renders a `now` line through `m.renderer.Plain` (added to `d7UntrustedFields`, hostile-value render test extended); `uzi run get` adds it to the `PROGRESS` row. Each marks it as a model summary with its age.

## Testing decisions

- Worker: trigger, coalescing, 5-minute floor, failure-counts-against-window, gating on state and setting, stale-result discard on claim loss and on disable, with a fake model client and fake clock (`agent/test`).
- Worker: a rate-limit error from the summary call leaves the run's rate-limit observer and status untouched.
- api: ingest strip and cap, worker-only acceptance, the newest-note-for-active-milestone selection, hidden when disabled, and hidden in each held state (waiting, parked, queued, stalled) with the same active milestone.
- Worker: the output-token cap, reasoning effort and timeout reach each harness; usage reaches the run's report exactly once on both harnesses; existing `runReadOnlyModelPass` callers are unchanged.
- Renderers: hostile-value render tests on web and TUI.

## Design mock contract

The dotted "Now" line in the web mock (inside the progress card) and TUI mock 4 are the presentation reference for this PRD, under the same contract as #2602: implement the mock; small consistency deviations are expected; larger ones go under `Mock deviations` in the PR with a reason; this text wins where it specifies.

## Milestones

### M1: Claude-harness summaries on the run page, TUI detail and CLI
Blocked by: #2602 merged.
Worker triggers, gating and the call through `runReadOnlyModelPass` for the Claude harness; the `progress_note` kind with its ingest, index and mirrors; the per-user setting, admin kill switch and their delivery to the worker; usage folding; web, TUI and CLI rendering; docs (`task docs:sync`) and a `CHANGELOG.md` line.
Acceptance: examples 1 to 3 on a Claude-harness run; with the setting off the worker makes no call; a Codex-harness run shows no summary.

### M2: Codex-harness summaries
Blocked by: M1.
`gpt-6-luna` added to the Codex contract model set (`agent/src/codex/render.ts`) and to `agent/src/codex/codex-pricing.json` (standard rates verified 2026-10-09 at https://developers.openai.com/api/docs/models/gpt-6-luna: $0.10 per million input tokens, $0.01 cached input, $0.125 cache write, $0.50 output); Codex summaries through the same seam; skip on refusal.
Acceptance: examples 1 and 3 on a Codex-harness run; an account that refuses the model shows no summary and the run is unaffected.

## Decision Log

| # | Decision | Reason | Rejected alternative |
|---|---|---|---|
| D1 | Generate on the worker through `runReadOnlyModelPass` | The worker already holds the run's credential and frames; the seam already isolates advice calls | An api-side model call (new credential use in the api); a new call path |
| D2 | On by default, with a per-user off switch and an admin kill switch | The user's decision (2026-10-09); spend is bounded by at most 12 calls per run-hour, each with an output-token cap and a 30-second timeout | Off by default, as the judge is |
| D3 | `haiku` on Claude, `gpt-6-luna` on Codex, no fallback | The cheapest tier on each harness (Luna $0.10/$0.50, Haiku $1/$5 per million tokens); a fallback would silently cost more | `gpt-6-sol`, the PR-description pin (20x Luna's price) |
| D4 | Worker-local triggers | The worker cannot see the api's phase; a second copy of the role mapping would drift | Triggering on `progress.phase` |
| D5 | Persist as a run message, shown only while its milestone is active | Reuses replay and broadcast; staleness has a clear rule | A field on `runs` (lost history, new write path) |
| D6 | The summary may not infer success | Twenty trimmed frames can show a gate starting, not its result | Free-form summaries that may overclaim |
