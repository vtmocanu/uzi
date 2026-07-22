# PRD #112: `uzi tui` — a modern terminal UI to watch runs live, see per-agent activity, and steer

**GitLab Issue**: [#112](https://gitlab.example.com/vtmocanu/uzi/-/issues/112)
**Status**: Draft (created 2026-07-22)
**Priority**: Medium
**Related**: [#64](https://gitlab.example.com/vtmocanu/uzi/-/issues/64) (the `uzi` CLI this extends), [#99](https://gitlab.example.com/vtmocanu/uzi/-/issues/99) (per-agent invocation id/label — the data the lanes render), [#95](https://gitlab.example.com/vtmocanu/uzi/-/issues/95) (steer queue), [#47](https://gitlab.example.com/vtmocanu/uzi/-/issues/47) (run health), [#94](https://gitlab.example.com/vtmocanu/uzi/-/issues/94)/[#46](https://gitlab.example.com/vtmocanu/uzi/-/issues/46) (judge review + triage), [#102](https://gitlab.example.com/vtmocanu/uzi/-/issues/102) (the web board this is the terminal analogue of)
**Review**: adversarial review by a Fable agent, 2026-07-22, verified against `main` — verdict **SOUND-WITH-FIXES** (no reversal; the hub/auth/replay premises all check out). Its corrections are folded into this draft: the D2/M1 origin reasoning (the CLI passes the default check by sending no Origin, `accept.go:228-232` — no `InsecureSkipVerify`, no auth-type gate), the board's kind filter + admin-view asymmetry + dropped agent-count column, D5's web-parity lane logic, D7's `sanitizeTTY` requirement, D8's admin-observe-only + chat watch-only degradations, and M2's reconnect-replay contract.

A single full-screen `uzi tui` that gives the terminal what the browser board
gives a tab: a live view of every active run, a drill-in showing what each run's
lead and subagents are doing right now, and in-place steering — approve a plan
gate, send a follow-up, cancel — without leaving the keyboard. It is built on
the API the CLI already speaks, and on the WebSocket hub the web already uses;
the only backend change is opening that hub to a Bearer CLI token.

## Problem

The `uzi` CLI (PRD #64) is complete for scripting but poor for *watching*. Every
live affordance today is a one-run, one-shot, poll-based command:

- **No factory-wide live view.** `uzi run list` prints a static table; to see
  whether anything changed you re-run it. There is no board that updates itself.
- **The log follow is a 2s REST poll, one run at a time.** `uzi run logs
  <id> --follow` polls `/api/runs/{id}/messages?after=<seq>` every 2s
  (`api/cmd/uzi/run.go:35-39`) and dumps a flat message stream to stdout. You
  cannot watch two runs at once, and you cannot see the *structure* of a run.
- **The per-agent structure is invisible at the terminal.** PRD #99 gave every
  message an `agent_instance` (the subagent invocation id) and `agent_label`
  (its task) — the browser lanes live activity off these (`api/internal/hub/hub.go:38-45`,
  `api/internal/apitypes/run.go:130-145`). The CLI throws that structure away and
  prints a flat log, so "what is the `coder` subagent doing vs the `tester`" is
  unanswerable headless.
- **Steering means leaving what you were looking at.** Approving a plan gate,
  sending a follow-up, or cancelling is a separate `uzi run approve|follow-up|cancel`
  invocation against a run id you have to have copied from somewhere else. There
  is no "watch this run and act on it in place" surface.

The result: an operator who wants to babysit the factory from a terminal (over
SSH, in a tmux pane, on a headless box) has no equivalent to the web board — they
poll static commands and read flat logs.

## Solution Overview

Ship `uzi tui`: one full-screen, keyboard-driven terminal app in the existing
`api/cmd/uzi` binary, built on the **Bubble Tea** ecosystem
(Bubble Tea event loop + Lip Gloss styling + Bubbles components + Glamour for
markdown). It reuses the existing `uzicli.Client` for every read/write and adds
**one** live-data capability: a Bearer-authenticated subscription to the
`/api/ws` hub the web board already rides.

Three views, one app:

1. **Board** (home). A live list of runs — own runs by default (`ListRuns`), an
   `[a]` toggle to the factory-wide admin view (`AdminListRuns`) when an admin
   (`uza_`) token is present. Columns: run id, kind, issue, status chip, health
   (`⚠ stuck 6m`, PRD #47), branch, MR state. **Kind is `issue | ci_fix |
   self_improve`** — chat and judge runs are excluded from both list queries
   (`store/queries/runtime.sql:246-249`, `kind NOT IN ('chat','judge')`), so the
   board never shows them (see the chat-run note under D8). **No "active-agent
   count" column**: `RunListItemDTO` (`apitypes/run.go:105-125`) carries no
   agent-count field and D3 forbids the per-run message fetch that would compute
   one; the active-agent view lives in the run detail, not the board. **The `[a]`
   admin view is not symmetric with own-runs**: `AdminListRuns`/`ListActiveRunsAll`
   returns **non-terminal runs only**, no judge-verdict/usage columns, capped at
   500 (`runtime.sql:246`), whereas the own-runs list includes finished runs with a
   judge badge — the board must label the admin view as "active runs" and not
   promise completed rows there. Refreshed on a light `ListRuns` poll (the board is
   list-level; a WS per run is overkill). `[enter]` opens a run; `[c]` creates one;
   `[/]` filters.

2. **Run detail** (the point of the feature). A split view fed by the run's
   `/api/ws` subscription, with a REST replay (`GetRun` + `RunLogs(after)`) for
   history and gap recovery — exactly the browser's replay-then-subscribe
   contract (`ws.go:40-43`):
   - **Left rail — agent lanes.** The lead plus each live subagent instance,
     keyed by `agent_instance`, labelled by `agent_label`, each with a status dot
     (thinking / running a tool / idle / done) and its current tool. This is the
     "see what each agent is doing" surface, driven directly by the `message`
     frames' `agent`/`agent_instance`/`agent_label` (`hub.go:38-45`) — no
     per-frame REST re-read, same as the browser lanes.
   - **Main pane.** The selected lane's transcript in a scrollback viewport
     (autoscroll, tool calls collapsible), text rendered through Glamour.
   - **Steer bar.** A text input that sends a run-level `follow_up`; at a plan
     gate, `[a]pprove` / `[r]eject`; `[x]` cancel; a queued/delivered indicator
     from `RunInputs` (PRD #95). `[v]` opens the review overlay.

3. **Review overlay.** The judge's verdict, summary, triage line, and
   recommendations with disposition keys (resolve / dismiss / undo), reusing the
   `api/cmd/uzi/review.go` logic and `SetDisposition`/`DeleteDisposition`.

The **only** backend change is M1: `/api/ws` today is cookie-only
(`handler.go:764-767`, "Cookie-only tail"); open it to a Bearer CLI token so the
headless TUI can subscribe. Everything else is client-side, on APIs that already
exist.

## Design Decisions

**D1 — Ride the existing `/api/ws` hub; do not build a new SSE endpoint.** The
realtime channel already exists and is battle-tested by the web: a per-run hub
(`internal/hub/hub.go`) fans `message` / `state` / `health` / `input` frames to
subscribers (`hub.go:27-46`), delivered over `GET /api/ws?run=<id>`
(`ws.go:44`). It is explicitly a *live channel only* — every frame is already
persisted, and the client recovers a dropped/missed frame via the REST replay
(`ws.go:40-43`). That is precisely what a TUI needs, and building a parallel
SSE endpoint would fork the fan-out into two channels to keep in sync. Reuse the
one hub. (Rejected: a new `text/event-stream` endpoint — simpler to *consume*,
but a second server-side channel to maintain, and the WS client is already a dep,
`coder/websocket`.)

**D2 — Open `/api/ws` to Bearer by moving it to `RequireUser`; NO Origin-check
change is needed (a route move only).** Today `/ws` sits in the cookie-only
`RequireAuth` group (`handler.go:766-767`) and leans on coder/websocket's default
same-origin `Accept` to defend the *cookie* against cross-site WebSocket
hijacking (`ws.go:30-34`). Mount `/ws` under `RequireUser` — session **or**
user-scoped CLI token, the same guard the run read routes use
(`handler.go:691-693`) — and **leave `AcceptOptions{}` untouched**. This works,
and stays safe, for two independent reasons the Fable review verified against the
dependency source:

- **The CLI already passes the default check.** A browser-less client sends **no
  `Origin` header**, and coder/websocket's `authenticateOrigin` returns nil for an
  empty Origin (module `accept.go:228-232`: `if origin == "" { return nil }`). A
  Bearer upgrade therefore passes the default `Accept` as-is. An earlier draft of
  this PRD claimed we must set `InsecureSkipVerify` on the Bearer path to let the
  CLI in — **that is false**; nothing needs to be skipped, and adding a skip would
  only weaken the defense.
- **The cookie CSWSH defense is preserved for free.** A cross-site browser page
  carries the ambient cookie but **cannot attach an `Authorization` header** (the
  browser WebSocket API forbids custom headers), so it stays on the cookie path,
  sends a foreign `Origin`, and is still rejected same-origin. `RequireUser`
  dispatches on credential *presence* at parse time
  (`middleware/cli_auth.go:39-103`), populates the identical context `user`, and a
  GET passes CSRF exactly as under `RequireAuth` (`auth/cookie.go:94-96`), so
  browser WS behavior is byte-identical after the move.

Because of this there is **no auth-type detection in `ServeWS`** (which would
duplicate `RequireUser`'s dispatch predicate and risk drift — a real hole the
review flagged) and **no `InsecureSkipVerify` anywhere**. Per-run authz is
unchanged: `GetRunForViewer(user.ID, user.IsAdmin, runID)` (`ws.go:55-60`)
resolves owner-or-admin identically for cookie and token identities, because
`RequireUser` and `RequireAuth` populate the same `user`. One hub, one authz path,
one unchanged origin rule.

**D3 — Board updates on a `ListRuns` poll; only the drilled-in run opens a WS.**
`/api/ws` is per-run (`?run=<id>`). Opening N sockets to keep a board's
"3 running" counter live is disproportionate; a periodic `ListRuns` (the same
call `uzi run list` makes) is cheap and list-level. The WS is reserved for the
run-detail view, where sub-second latency on the transcript and lanes is the
whole point. (This also means the board works before M1 lands — see Milestones.)

**D4 — Steering is run-level in this PRD; per-agent steer is explicitly out of
scope.** The steer verbs the server accepts are run-level: `follow_up`,
`approve_plan`, `reject_plan`, `cancel`, `revise_plan`
(`api/internal/handler/workers.go:663`). A follow-up enters the run's steer queue
and the lead drains it on its next turn (PRD #95); `AgentSelection` only chooses
the *roster* at approval, not a live target. There is no wire to address one live
subagent, and building one (a steer kind targeting an `agent_instance`, plus
worker-side routing into that subagent's context) is its own backend PRD. The TUI
therefore *observes* per-agent and *steers* the run/lead, which then directs its
agents. This is called out so nobody reads "steer agents" into the run-detail
view and expects to whisper to `coder` alone.

**D5 — Agent-lane status is inferred client-side from the frame stream, best
effort, never authoritative.** A lane's dot is derived from the frames seen for
that `agent_instance`: last frame was a tool call still open → *running <tool>*;
a text/thinking frame → *thinking*; a subagent-result/terminal frame → *done*; no
frame within an idle window → *idle*. The run's own `status`/`health` (from the
`state`/`health` frames and `GetRun`) drive the run-level chip. The lane status is
a display heuristic over an append-only message log, not a field the server
guarantees; it must degrade gracefully (unknown → a neutral dot) and never block
rendering on a frame it cannot classify.

**Do not invent this from scratch — inherit the web's proven logic.** The lane key
is `agent_instance || agent || "lead"` (mirror `web/src/lib/runStream.ts:78`), and
the status classification should port `ActivityFeed`'s (`agentOneLiner`
~`web/src/components/ActivityFeed.tsx:280`; lane-state kinds at `:591-608`) rather
than a parallel table. In particular it must carry the **resumed-subagent edge
case** the web already handles (`ActivityFeed.tsx:61-116`): on SDK replay a
subagent frame can arrive with `agent == "lead"` and no `subagent_type`, and must
**not** be mislabeled as the orchestrator's own lane. Settle the final table in M3
against real captured frames and this parity source, not in the abstract.

**D6 — Reuse `uzicli.Client` and the existing command logic; add exactly one
client method.** Board/detail/steer/review are all existing calls:
`ListRuns`/`AdminListRuns`, `GetRun`, `RunLogs`, `RunInputs`, `RunReview`,
`SubmitRunInput`, `SetDisposition`/`DeleteDisposition`
(`api/internal/uzicli/client.go`). The one addition is `StreamRun(ctx, runID)
(<-chan hub.Event-equivalent, error)` dialing `/api/ws` with the Bearer header.
Reuse (not re-implement) the disposition/short-id resolution in `review.go` and
the roster-selection in `run.go` so the TUI and the plain commands cannot drift.

**D7 — Render every judge free-text field as inert data (Risk 13).** The review
overlay shows `target`, `rationale_md`, `summary_md` — untrusted, LLM-derived,
attacker-influenceable free text (per the CLI's own Risk 13 note). Branch only on
the closed enums (`verdict`, `category`, `confidence`); render the free text
through Glamour as inert markdown, never interpret it as a key binding, command,
or action. Same rule for issue titles/descriptions and `agent_label` shown in the
lanes.

**And strip terminal control bytes — Glamour does not do this for you.** The CLI
already has the discipline: `sanitizeTTY` strips C0/C1 control and raw ESC
sequences from every untrusted string bound for a TTY (`api/cmd/uzi/run.go:468-478`,
applied today to health/failure reasons, review text, table cells). The TUI
renders a **live transcript** of attacker-influenceable content — tool output,
repo file contents, `agent_label` — straight to the terminal, and a raw `ESC[`
sequence in that stream can move the cursor, recolor, or spoof the frame. Every
string the TUI draws (transcript lines, tool output, lane labels, issue text) must
pass through `sanitizeTTY` (or an equivalent) **before** Glamour, not instead of
it. This is a hard requirement of M3/M4, not a nicety.

**D8 — TUI is additive; the plain commands stay.** `uzi tui` is a new subcommand;
`--json` and every scriptable verb are untouched. The TUI degrades to a clear
message (not a crash) when the terminal is not a TTY, when the token lacks admin
scope for the `[a]` toggle, or when `/api/ws` is unreachable (fall back to the
2s REST poll for the transcript, matching `run logs --follow`).

Two degradations the review surfaced that are **not optional**:

- **An admin watching someone else's run is observe-only.** Subscribe is
  owner-or-admin, but the steer surface is owner-only: `ListRunInputs` 404s a
  non-owner incl. `admin_ro` (`handler.go:698-702`) and `SubmitRunInput` is
  owner-scoped (`workersvc/service.go:2018`). So when the `[a]` admin board opens a
  run the caller does not own, the run detail must render **read-only** (transcript
  + lanes + review), suppressing the steer bar and the queued/delivered indicator
  rather than showing controls that 404. M3/M4 must gate the steer surface on
  ownership, not on visibility.
- **Chat runs are watch-only in the TUI (or out of scope).** They are excluded
  from the board (kind filter above), and a chat follow-up is a forge-minting,
  chat-limited action that rides the cookie-only `/chats` surface
  (`handler.go:751-759`) — `SubmitRunInput` does **not** gate `kind=chat`, so a raw
  follow-up would inject into a chat outside that guarded path. `uzi tui <chat-run>`
  therefore suppresses the steer bar entirely; see Out of Scope.

## Milestones

**Phase 1 — the live channel (backend, its own MR; prerequisite for realtime detail)**

- [ ] **M1 — `/api/ws` accepts a Bearer CLI token (a route move, `AcceptOptions{}`
      untouched).** Move the `/ws` route out of the cookie-only tail into a
      `RequireUser` mount (session OR `uzc_`/`uza_` token). **Do not** change
      `websocket.Accept`'s options — the CLI sends no Origin and already passes the
      default check, and the cookie same-origin defense is preserved by the browser
      not being able to send a Bearer header (D2). Per-run authz via
      `GetRunForViewer` is unchanged. Sites: `api/internal/handler/handler.go:766-767`
      (route move) and the group comment at `handler.go:735-736` ("Cookie-only
      tail: … and the WS follow channel") — falsified by the move, rewrite it;
      `api/internal/handler/ws.go:25-42` (the docstring asserts "runs inside the
      session-authenticated group" and "relies on … same-origin … cookie-authenticated
      socket" — rewrite to state the dual cookie/Bearer model and why the origin
      rule still holds). Both comment fixes land in this MR per CLAUDE.md.
      Tests: **a no-Origin Bearer upgrade succeeds** and receives a published frame
      (this is the actual enabler — pin it); a cookie upgrade from a foreign Origin
      is still rejected; a Bearer token for a run the caller cannot see gets the
      pre-upgrade 404 (owner-or-admin parity with the REST reads); a `uza_` admin
      token can subscribe to another user's run (read parity with `AdminListRuns`).
      `go build ./... && go test ./...` green. **e2e** (`./e2e/run-e2e.sh`, the
      local pre-merge gate per CLAUDE.md): the existing cookie WS leg
      (`e2e/run-e2e.sh:1300-1316`) gains a Bearer WS subscribe assertion so the new
      auth path is exercised end to end.

- [ ] **M2 — `StreamRun` on `uzicli.Client`.** Add `StreamRun(ctx, runID)` that
      dials `/api/ws?run=<id>` with the `Authorization: Bearer` header via
      `coder/websocket`, decodes each frame into the `hub.Event` shape
      (`type ∈ message|state|health|input`, plus `seq/kind/agent/agent_instance/
      agent_label/payload/created_at/status`, `hub.go:32-46`), and emits them on a
      channel. **Only `message` frames carry a `seq`** — `state`/`health`/`input`
      do not — so the recovery contract is: on *any* reconnect (not just a detected
      seq gap) replay via `RunLogs(after)` **and** re-read `GetRun` for
      authoritative run state, exactly as the web does
      (`web/src/lib/runStream.ts:107-116`); the WS is never the source of truth
      (`ws.go:40-43`). Unit tests against a fake WS server (extend
      `internal/uzicli/fake.go`): frame decode, reconnect→replay recovery, clean
      shutdown on ctx cancel. This is the seam the TUI consumes; it is independently
      testable without any UI.

**Phase 2 — the TUI (depends on M2 for realtime; board can start on the poll)**

- [ ] **M3 — `uzi tui` board + run-detail with agent lanes (read-only first).**
      The Bubble Tea app: board (`ListRuns` poll, `[a]` admin toggle gated on
      `uza_`, `[/]` filter, `[enter]` open, `[q]` quit) and run detail (replay via
      `GetRun`+`RunLogs`, live via `StreamRun`, left-rail agent lanes keyed on
      `agent_instance` with the D5 status heuristic, main-pane Glamour transcript,
      `[tab]`/`j`/`k` to switch lanes). No mutations yet. Add the deps
      (`charmbracelet/bubbletea`, `lipgloss`, `bubbles`, `glamour`) to
      `api/go.mod` — noting they land in the **shared** server module, growing its
      dep graph and the kaniko build inputs for a CLI-only feature; acceptable in a
      single-binary repo but stated so it is a deliberate choice. Non-TTY /
      unreachable-WS degrade cleanly (D8). The status
      classification table (D5) is settled here against captured frames and unit
      tested as a pure function (the `review.go`/`boardColumns` discipline: logic
      in a testable helper, not tangled in the update loop).

- [ ] **M4 — Steering + review overlay (the mutations).** Steer bar: `follow_up`
      text input, `[a]pprove`/`[r]eject` at a plan gate, `[x]` cancel, all via
      `SubmitRunInput`; queued/delivered indicator from `RunInputs`; the input
      steer-queue change reflected live off the `input` frame. Review overlay:
      `RunReview` render (verdict/summary/triage/recs) with resolve/dismiss/undo
      via `SetDisposition`/`DeleteDisposition`, reusing `review.go`'s short-id
      resolution and D7 inert-rendering. A destructive-ish action (cancel, reject)
      asks for confirmation before firing. `entry: uzi tui` (board) and `uzi tui
      <run>` (straight into one run's lanes).

- [ ] **M5 — Docs + SKILL + specs/ai.md.** `docs/cli.md` gains a `uzi tui`
      section (views, keybindings, the admin toggle, the WS/Bearer note, the
      run-level-steering scope). The shipped agent skill
      (`api/internal/uzicli/skill/SKILL.md`) gets a `uzi tui` entry — but framed
      honestly for its agent audience: the TUI is a human affordance; agents keep
      using `--json` verbs. `specs/ai.md` gains an append-at-tail AI-decisions
      record for the Bearer-WS origin-gate (D2), the ride-the-hub choice (D1), the
      run-level-steer scope (D4), and the lane status heuristic (D5). Per repo
      convention, the specs/ai.md items may land per milestone rather than all at
      once.

- [ ] **M6 — Verified on k8s.** The board, a run drill-in with live lanes, a
      follow-up, a plan-gate approve, and a cancel all behave against a real run
      on **dev-cluster** (per CLAUDE.md, k8s is the primary validation target, not
      compose), with a `uzc_` token for own-runs and a `uza_` token for the admin
      board. The compose path (`docker compose` + `./e2e/run-e2e.sh`) still works
      as the laptop dev loop.

## Parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | `api/internal/handler/ws.go`, `handler.go`, `e2e/` |
| 1 | M2 | M1 (needs the Bearer upgrade to exist to test against, though the decode/replay logic can be written against a fake first) | `api/internal/uzicli/client.go`, `fake.go` |
| 2 | M3 | M2 | `api/cmd/uzi/tui/*` (new), `api/go.mod` |
| 2 | M4 | M3 | `api/cmd/uzi/tui/*` |
| 2 | M5 | M3, M4 | `docs/cli.md`, `api/internal/uzicli/skill/SKILL.md`, `specs/ai.md` |
| 2 | M6 | M4 | — (validation) |

M1 is a small, security-sensitive backend MR that lands on its own so a reviewer
reasons about the origin-gate change in isolation (not buried in a UI diff). M2
is a self-contained client method, testable against a fake before M1 is live. M3
and M4 are one contributor's sequential work in a new `api/cmd/uzi/tui/` package
and would conflict if split across agents. The board portion of M3 can be
prototyped on the 2s REST poll before M1/M2 land and swapped to `StreamRun` when
ready (D3/D8).

## Success Criteria

- `uzi tui` opens a full-screen board that updates without re-invocation; `[enter]`
  drills into a run and shows a left rail of the lead + each live subagent with a
  live status and current tool, and a live transcript, within ~1s of the event
  (WS, not the 2s poll).
- From the run-detail view, a user approves a plan gate, sends a follow-up, and
  cancels a run — no separate command, no copied run id.
- The admin `[a]` toggle shows the factory-wide board with a `uza_` token and is
  cleanly refused (not a crash) with a `uzc_` token.
- `/api/ws` accepts a Bearer token for a run the caller may see and refuses one
  they may not, while a cross-origin *cookie* upgrade is still rejected.
- No regression to any existing `uzi` command or to the web board's use of the
  same hub.

## Risks

- **R1 — Widening `/api/ws` auth weakens the CSWSH defense.** Mitigation: the
  origin check is **not touched** (D2) — the route move alone lets the CLI in
  because it sends no Origin (which coder/websocket passes, `accept.go:228-232`),
  while a cross-site browser page cannot send a Bearer header and so stays on the
  cookie path and is still rejected same-origin. The cookie path is byte-for-byte
  unchanged. M1's tests assert both halves (foreign-origin cookie still rejected;
  no-Origin Bearer allowed). This is the one change a reviewer must scrutinize —
  hence M1 as its own MR.
- **R2 — Lane status is a heuristic and can mislabel.** Mitigation: D5 keeps it
  best-effort, degrades unknown frames to a neutral dot, and never blocks
  rendering; the run-level chip (authoritative `status`/`health`) is always
  correct even when a lane dot is stale.
- **R3 — A TUI is a large surface to maintain in a CLI binary.** Mitigation: it is
  strictly additive (D8), isolated in its own `api/cmd/uzi/tui/` package, and
  reuses `uzicli.Client` + existing command logic so business rules live in one
  place. Charm libraries are the mainstream, well-maintained Go TUI stack.
- **R4 — Prompt-injection via judge/issue/agent free text (Risk 13).** Mitigation:
  D7 — branch only on closed enums, render all free text as inert markdown.
- **R5 — WS/terminal edge cases** (non-TTY, resize, disconnect, huge transcripts).
  Mitigation: D8 degradation, viewport windowing, and the replay-on-reconnect
  contract from M2.
- **R6 — A live socket outlives token revocation.** A revoked/expired
  `uzc_`/`uza_` token's subscription persists until the socket drops. This is
  parity with today's cookie sessions (session revocation does not kill a live
  socket either), not a new regression — recorded in the specs note (M5) so it is a
  known, accepted property rather than a surprise.

## Open Questions

- **Entry name and jump-in.** `uzi tui` (board) + `uzi tui <run>` (one run) — or a
  separate `uzi watch <run>`? Leaning `uzi tui [run]`, single command.
- **Board liveness.** Poll interval for `ListRuns` (2s like the log follow, or
  slower with a manual `[r]efresh`)? A board-level WS is deferred (D3); revisit
  only if the poll proves too laggy in practice.
- **Theme.** Adaptive light/dark via Lip Gloss `AdaptiveColor`, or a fixed dark
  palette? Adaptive is the best-practice default.

## Out of Scope

- **Per-agent steering** (whisper to one live subagent) — deferred to its own
  backend PRD (D4).
- **Chat-run steering** — `uzi tui <chat-run>` is watch-only (D8); the cookie-only,
  chat-limited `/chats` follow-up path (`handler.go:751-759`) is not
  reimplemented in the TUI. Chat runs also never appear on the board.
- **A board-level live WS** — deferred (D3); the board polls.
- **Mouse interaction** — keyboard-first; mouse support can come later.
- **Any new server capability beyond M1's Bearer-on-`/api/ws`** — the TUI is a new
  consumer of existing APIs, not a reason to add endpoints.
- **Replacing the web board** — this is the terminal analogue, not a migration.
