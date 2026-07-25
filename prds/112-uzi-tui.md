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

The **only** backend change is M1: `/api/ws` today is cookie-only (the "Cookie-only
tail" group comment sits at `handler.go:735-736`, not `764-767` as an earlier
draft of this line had it — `766-767` is the route mount itself); open it to a
Bearer CLI token so the headless TUI can subscribe. Everything else is
client-side, on APIs that already exist.

## Inspiration

**Added retroactively — this PRD shipped without the inspiration-first check
CLAUDE.md requires, and it's added here now that the check has been done.**
`inspiration/` holds three submodules; a terminal UI for uzi is the first
feature in this repo's history that's actually comparable to one of them.

- **`dot-agent-deck`** is the closest prior art in *purpose* — a terminal
  dashboard for watching agent runs — but it's **Rust + ratatui**, not Bubble
  Tea. There is no Go code, no Elm-architecture `Update`/`View` split, and no
  library dependency to inherit; what's worth taking from it is architecture
  and UX only, not a stack. It treats subagent start/stop as an informational
  log line, with **no per-subagent lane at all** — every agent's activity
  interleaves into one stream. `uzi tui`'s lane rail (one row per
  `agent_instance`, keyed and labelled, D5) is the place this PRD goes
  further, and it's able to precisely because PRD #99 gives every message a
  real invocation id and task label to key a lane on; deck has no equivalent
  data to build one from.
- **`bottega`** and **`multica`** have no terminal UI of any kind — nothing
  in either to match or beat here.

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
unchanged: `GetRunForViewer(user.ID, user.IsAdmin, runID)` (`ws.go:55-60`) runs
the identical call for either credential.

**Corrected during implementation (`middleware/cli_auth.go:85-87`):** this
paragraph originally claimed `RequireUser` and `RequireAuth` resolve
owner-or-admin *identically* for cookie and token identities. That's false —
`RequireUser` clears `user.IsAdmin` on any token whose scope isn't
`admin_ro`, so the same admin's session cookie and default-scope `uzc_` token
reach different branches of `GetRunForViewer`. Same context key, same
`store.User` type, same authz call — but the *reach* is narrowed for a
non-admin-scoped token, never widened, which is exactly the direction that's
safe and needed no code change. The `[a]` admin board (M3) depends on this:
it's why a `uzc_` token is cleanly refused the factory-wide view even when
its owner is an admin in the database (`uzi whoami` over a `uzc_` token
likewise reports `is_admin: false` for the same reason — see `docs/cli.md`).
One hub, one authz path, one unchanged origin rule — narrowed by token
scope, not duplicated by transport.

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
effort, never authoritative.** The run's own `status`/`health` (from the
`state`/`health` frames and `GetRun`) drive the run-level chip; a lane's own dot
is a display heuristic over an append-only message log, not a field the server
guarantees, and must degrade gracefully (unknown → a neutral dot) rather than
block rendering on a frame it cannot classify.

**Corrected during implementation.** This section named the wrong file for the
lane key, described the wrong function for the dot, and undercounted the state
ladder by one. Verified against `origin/main`:

- **The lane key is `web/src/components/ActivityFeed.tsx:77-79`**, not
  `web/src/lib/runStream.ts:78` (that line is `refreshRun: boolean;` —
  `runStream.ts` has no lane logic at all). `laneKeyOf(m) = m.agent_instance ||
  m.agent || LEAD`; the `||` (never `??`) is load-bearing, since an
  empty-string instance must fall through to the role rather than survive as
  a key of its own.
- **The dot and the transcript's one-liner text are two separate pure
  functions, and the paragraph above conflated them.** Its prose ("last frame
  was a tool call still open → running…") describes `agentOneLiner`
  (`ActivityFeed.tsx:279-310`), which produces the one-liner TEXT. The DOT is
  `crewStateFor` (`:209-245`), which takes **no frame kind as input at all** —
  only `run.status`, `run.health`, whether the lane is the active actor, and
  recency. Both got ported; they are not the same function.
- **There are five states, not four: `working | stalled | waiting | idle |
  done`.** The list above omitted `stalled` — the PRD #47 health integration,
  and the only state that means "look at this now." Precedence, worst-first:
  1. `run.status ∈ {completed, failed, cancelled}` → `done`.
  2. `run.status == awaiting_approval` or `run.health == waiting_worker` →
     `waiting`, dominating every lane, not just the active one.
  3. The active actor and `run.health ∈ {stalled, slow, looping}` → `stalled`.
  4. The active actor otherwise → `working`.
  5. A non-active lane younger than 45s → `waiting`; older → `idle`.
  A rolled-up dot (e.g. a per-role summary) takes the group's *worst* state,
  never the newest.

  Three traps the ported source's own comments call out, all load-bearing:
  the **active** lane trusts `run.health`, never the recency timer — the
  server's own stall flag defaults to 300s, so a healthy tool call running
  45s-300s must still read `working`, not flip to `idle`; "active" for lane
  purposes means `run.status ∈ {running, claimed}`, **including `claimed`**,
  not `running` alone (a running-only test exists too, but it drives the tool
  spinner, not the dot — get this wrong and every lane of a claimed run reads
  `idle`); and the rollup is worst-state-wins, not newest-wins.

**Do not invent this from scratch — inherit the web's proven logic**, now
ported (not merely referenced): the lane key above, `crewStateFor`'s ladder,
and the **resumed-subagent edge case** (`ActivityFeed.tsx:61-116`): on SDK
replay a subagent frame can arrive with `agent == "lead"` and no
`subagent_type`, and must **not** collapse into the orchestrator's own lane.
A lane's role is titled from its first frame naming a *non-lead* role, not
its first frame outright — testing "is `agent` non-null" would not catch
this, since the field is already collapsed to the literal string `"lead"`
upstream, never to null. The lane label is a separate, independent field
(first frame carrying one wins; absent renders no placeholder), clamped at
**80 runes** to match the server's storage cap — not the web's 48 UTF-16
code units, whose own comment admits it can split an astral pair. That's a
defect in the source being ported, not a contract, so the Go port clamps on
runes instead of copying it.

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
already has the discipline: `sanitizeTTY` strips terminal control characters
from every untrusted string bound for a TTY (`api/cmd/uzi/run.go:468-478` at
draft time; `478-485` on the tree M3 actually landed on — re-derive the line
before citing it), applied today to health/failure reasons, review text,
table cells. The TUI
renders a **live transcript** of attacker-influenceable content — tool output,
repo file contents, `agent_label` — straight to the terminal, and a raw `ESC[`
sequence in that stream can move the cursor, recolor, or spoof the frame. Every
string the TUI draws (transcript lines, tool output, lane labels, issue text) must
pass through `sanitizeTTY` (or an equivalent) **before** Glamour, not instead of
it. This is a hard requirement of M3/M4, not a nicety.

**Corrected during implementation — three things this section understated.**

1. **`sanitizeTTY` alone is the wrong unit for a fixed-width cell.** It
   deliberately spares `\t`/`\n` — correct for a scrolling transcript, wrong
   for a table row or a lane-rail line, where a raw newline breaks column
   alignment. Fixed-width cells (the board's rows, the lane rail, the review
   overlay's headers) go through a second pair: `compactText` (folds
   `\t`/`\n`, caps length) and `cellText` (`sanitizeTTY` + `compactText`
   composed). `sanitizeTTY` is necessary but not sufficient for that half of
   the UI, and this section named only it.
2. **The render order is a functional requirement, not only a security one —
   and that turned out to be measurable, not just argued.** Glamour *emits*
   the ANSI escapes that make styled markdown work; sanitizing after it
   strips Glamour's own styling along with anything hostile. Measured on the
   shipped renderer (glamour v2.0.1): `sanitizeTTY → Glamour` yields output
   carrying 426 escape sequences with correct styling; reversed, it yields
   **zero** escapes and prints literal `[38;5;39;1m##` garbage on screen.
   Getting the order backwards breaks the screen outright rather than
   opening a silent hole, which is what makes it a regression test
   (`TestTUIRenderOrderIsSanitizeThenGlamour`) rather than a review note.
3. **`sanitizeTTY` grew a second half during M3: Unicode format-character
   (`Cf`) stripping, plus DEL.** The predicate this section described (C0
   below `0x20` except tab/newline, plus the `0x80`-`0x9F` C1 range) let DEL
   (`0x7f`) through and could never catch `Cf` at all — that needs a category
   predicate, not a codepoint range. It now uses `unicode.IsControl`
   (subsumes the old C0/C1 ranges and covers DEL) plus
   `unicode.In(r, unicode.Cf)`, deliberately converged on the same pair
   `workersvc.hasUnsafeChar` already used
   (`api/internal/workersvc/agent_selection.go:236-240`), so the CLI and the
   server settle on one predicate rather than two free to drift apart. The
   `Cf` half matters most for a bidi override (`U+202A`-`U+202E`): unlike a
   plain control byte, it visually *reorders* text, so an agent label or a
   judge's `target` could be made to read as something it isn't — exactly the
   spoof a fixed-width rail invites. **Combining marks are category `Mn`, not
   `Cf`, and stay out of scope here**: "Zalgo" text is a grapheme-width
   problem, not a control-stripping one, and needs a width-aware layout to
   fix, not a stripper.

**D8 — TUI is additive; the plain commands stay.** `uzi tui` is a new subcommand;
`--json` and every scriptable verb are untouched. The TUI degrades to a clear
message (not a crash) when the terminal is not a TTY, when the token lacks admin
scope for the `[a]` toggle, or when `/api/ws` is unreachable (fall back to the
2s REST poll for the transcript, matching `run logs --follow`).

Two degradations the review surfaced that are **not optional**:

- **An admin watching someone else's run is observe-only.** Subscribe is
  owner-or-admin, but the steer surface is owner-only: `ListRunInputs` 404s a
  non-owner incl. `admin_ro` (`handler.go:698-702`) and `SubmitRunInput` is
  owner-scoped (`workersvc/service.go:2018` — **corrected**: `:2018` is
  `createRun`; the write is `SubmitInput` at `:2239`). So when the `[a]` admin board opens a
  run the caller does not own, the run detail must render **read-only** (transcript
  + lanes + review), suppressing the steer bar and the queued/delivered indicator
  rather than showing controls that 404. M3/M4 must gate the steer surface on
  ownership, not on visibility.

  **The "gate on ownership" mechanism above was, as stated, unimplementable —
  discovered during M4, corrected there.** Nothing on the wire carries an
  owner to compare against: `RunDTO` has no user id at all, and
  `RunListItemDTO` carries only `OwnerEmail`, which is `omitempty` and
  populated on the **admin** list only — so a client-side `run.UserID ==
  caller.ID` check, which this section implicitly assumed, cannot be
  computed from anything the TUI has. What shipped asks the server instead,
  using the endpoint that shares the write's own predicate: `ListFollowUpInputs`
  resolves ownership with `s.GetRun(ctx, userID, runID)`
  (`service.go:2128-2132`), and `SubmitInput`'s first statement
  (`service.go:2239`) is the identical call. So probing `RunInputs` and
  reading its 404 is not an approximation of "may this caller steer" — it's
  the same predicate the write will evaluate, evaluated by the same code,
  not a second copy of the rule that's free to drift from it.
- **Chat runs are watch-only in the TUI (or out of scope).** They are excluded
  from the board (kind filter above), and a chat follow-up is a forge-minting,
  chat-limited action that rides the cookie-only `/chats` surface
  (`handler.go:751-759`) — `SubmitRunInput` does **not** gate `kind=chat`, so a raw
  follow-up would inject into a chat outside that guarded path. `uzi tui <chat-run>`
  therefore suppresses the steer bar entirely; see Out of Scope.

## Milestones

**Phase 1 — the live channel (backend, its own MR; prerequisite for realtime detail)**

- [x] **M1 — `/api/ws` accepts a Bearer CLI token (a route move, `AcceptOptions{}`
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

- [x] **M2 — `StreamRun` on `uzicli.Client`.** Add `StreamRun(ctx, runID)` that
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

- [x] **M3 — `uzi tui` board + run-detail with agent lanes (read-only first).**
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

  **Corrected during implementation:** quit shipped gated on confirmation for
  both `[q]` and `ctrl+c` (a second `ctrl+c` quits at once) rather than a bare
  `[q]` — a stray keystroke must not drop a watched run. Lane switching
  shipped as `[tab]`/`h`/`l` (or `←`/`→`); `j`/`k` (and `↑`/`↓`) scroll the
  selected lane's transcript instead — the PRD's `[tab]`/`j`/`k` conflated
  the two. The modules landed as `charm.land/bubbletea/v2 v2.0.8`,
  `charm.land/lipgloss/v2 v2.0.5`, `charm.land/bubbles/v2 v2.1.1`,
  `charm.land/glamour/v2 v2.0.1` (a fork move, not the `github.com/
  charmbracelet/*` v1 import paths named above); measured cost to `api/go.sum`:
  129 → 182 lines. **There is no Go symbol named `boardColumns`** — that
  discipline reference is `web/src/lib/boardColumns.ts` (TypeScript); the Go
  analogue D6 means is `approveSelection` (`api/cmd/uzi/run.go:346`).

- [x] **M4 — Steering + review overlay (the mutations).** Steer bar: `follow_up`
      text input, `[a]pprove`/`[r]eject` at a plan gate, `[x]` cancel, all via
      `SubmitRunInput`; queued/delivered indicator from `RunInputs`; the input
      steer-queue change reflected live off the `input` frame. Review overlay:
      `RunReview` render (verdict/summary/triage/recs) with resolve/dismiss/undo
      via `SetDisposition`/`DeleteDisposition`, reusing `review.go`'s short-id
      resolution and D7 inert-rendering. A destructive-ish action (cancel, reject)
      asks for confirmation before firing. `entry: uzi tui` (board) and `uzi tui
      <run>` (straight into one run's lanes).

  **Corrected during implementation:** approve/reject shipped as `[y]`/`[n]`,
  not `[a]`/`[r]` — `[a]` doubling as the board's admin toggle *and* approve
  would put "approve a plan" one keystroke from `[x]` cancel-a-live-run, and
  `[r]` is refresh everywhere else in the app. `[f]` starts a follow-up and
  `[v]` opens/closes the review overlay; neither was named above. The steer
  bar is additionally gated on **ownership**, not merely on the run loading —
  see the corrected D8 below; an admin watching someone else's run and any
  `kind=chat` run render read-only, with the reason shown in place of the
  bar.

- [x] **M5 — Docs + SKILL + specs/ai.md.** `docs/cli.md` gains a `uzi tui`
      section (views, keybindings, the admin toggle, the WS/Bearer note, the
      run-level-steering scope). The shipped agent skill
      (`api/internal/uzicli/skill/SKILL.md`) gets a `uzi tui` entry — but framed
      honestly for its agent audience: the TUI is a human affordance; agents keep
      using `--json` verbs. `specs/ai.md` gains an append-at-tail AI-decisions
      record for the Bearer-WS origin-gate (D2), the ride-the-hub choice (D1), the
      run-level-steer scope (D4), and the lane status heuristic (D5). Per repo
      convention, the specs/ai.md items may land per milestone rather than all at
      once.

  **Status: complete.** The SKILL.md entry actually landed in M3, not here — see
  the blocking structural note above; `TestSkillMatchesCommandTree` requires every
  runnable command documented in the same milestone that makes it runnable.
  `docs/cli.md` and `CHANGELOG.md` landed with the documenter's pass;
  `specs/ai.md` followed as the spec-keeper's write and the two are committed
  together so M5 is one commit.

  `specs/ai.md` claimed **§368-§372**, not §367-§371: §367 was already taken by
  PRD #108 Phase 2, uncommitted in a sibling worktree when the numbers were
  checked (2026-07-25). The file is append-at-tail and its numbers collide the
  way goose migration numbers do, so the check has to cover every unmerged
  branch's working tree, not just its tip. The five sections are the Bearer-WS
  route move (D1/D2, plus R6 and the supersede of §279's `/api/ws` line), the
  hub's seq-less drop hole and the `Kind`-open/`Status`-closed split (M2), the
  `package main` layout (M3), terminal safety (D7), and the lane/steer
  decisions (D4/D5/D8). §279's `/api/ws` line is superseded by a pointer from
  §368, **not** edited in place — it is the record of a decision that was true
  when it was made.

- [ ] **M6 — Verified on k8s.** The board, a run drill-in with live lanes, a
      follow-up, a plan-gate approve, and a cancel all behave against a real run
      on **dev-cluster** (per CLAUDE.md, k8s is the primary validation target, not
      compose), with a `uzc_` token for own-runs and a `uza_` token for the admin
      board. The compose path (`docker compose` + `./e2e/run-e2e.sh`) still works
      as the laptop dev loop.

  **Deferred out of this branch, explicitly, not silently dropped.** M6
  needs a deployed image and live `uzc_`/`uza_` tokens on dev-cluster, both
  post-merge. It cannot be done from a pre-merge branch and is called out as
  outstanding in the MR rather than checked off or quietly skipped.

## Parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | `api/internal/handler/ws.go`, `handler.go`, `e2e/` |
| 1 | M2 | M1 (needs the Bearer upgrade to exist to test against, though the decode/replay logic can be written against a fake first) | `api/internal/uzicli/client.go`, `fake.go` |
| 2 | M3 | M2 | `api/cmd/uzi/tui/*` (new), `api/go.mod` |
| 2 | M4 | M3 | `api/cmd/uzi/tui/*` |
| 2 | M5 | M3, M4 | `docs/cli.md`, `api/internal/uzicli/skill/SKILL.md`, `specs/ai.md` |
| 2 | M6 | M4 | — (validation) |

**Corrected during implementation — the Files column's `api/cmd/uzi/tui/*`
package was never buildable, and R3's mitigation below inherited the same
error.** All 27 files in `api/cmd/uzi/` are `package main` (Go forbids
importing a main package from a subpackage), and D6/D7 both mandate reusing
roughly two dozen unexported helpers that already live there — `sanitizeTTY`,
`compactText`, `cellText`, `capCell`, `shortInstanceID`, `runTitle`, `relAge`,
the disposition/short-id resolution in `review.go`, the roster selection in
`run.go` — none of which a subpackage could reach. It shipped as
`api/cmd/uzi/tui_*.go`, in `package main`, file-level separation instead of
package-level. That is a tradeoff worth recording honestly rather than
re-describing as the original plan.

**Split the claim in two, because "by construction" is true of only half of it
and a reader who sees the MR say "review property" will otherwise call it a
contradiction.** REACHABILITY holds by construction: the helpers are in-package,
so no future contributor has a justification for writing a second `sanitizeTTY`
or a second short-id resolver — the reason duplicates get written is that the
original is out of reach, and it is not. ENFORCEMENT is a review property:
nothing *makes* a new render path call them, and a raw `sb.WriteString(dto.Field)`
compiles. `tui_d7_guard_test.go` narrows that gap at the syntax level (direct
writes and concatenations, including the internal field names the lane rail uses)
and the frame tests catch the indirection case the guard cannot; neither is a
compile-time guarantee, and the PRD should not claim one.

**Reopen trigger, so the decision is revisitable rather than permanent:** if the
helpers are ever extracted into an importable package, extract *all* of them. The
half-measure was priced and rejected — leaving `renderReview`, `resolveRecID` and
`approveSelection` behind means the subpackage still would not compile, so a
partial extraction buys nothing and costs a refactor.

The cost of the shipped choice is that the TUI has no package boundary of its own
— see R3, also corrected below.

M1 is a small, security-sensitive backend MR that lands on its own so a reviewer
reasons about the origin-gate change in isolation (not buried in a UI diff). M2
is a self-contained client method, testable against a fake before M1 is live. M3
and M4 are one contributor's sequential work in `api/cmd/uzi/tui_*.go`
(not a separate package — see above) and would conflict if split across
agents. The board portion of M3 can be
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

  **Corrected during implementation:** it is not package-isolated — see the
  Files-column correction above. It shipped as `api/cmd/uzi/tui_*.go` in
  `package main`, so the mitigation is **file-level separation, not
  package-level**: reuse of `uzicli.Client` and the plain commands' helpers
  holds (arguably more strongly, since nothing needs re-exporting to be
  reachable), but nothing stops a future change to `api/cmd/uzi`'s other
  files from reaching into `tui_*.go` internals the way a real package
  boundary would. That is the honest tradeoff, not a discipline this PRD can
  enforce by naming a package that doesn't exist.
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

  **Reframed during implementation:** "parity" undersells the change. A
  browser session cookie is short-lived and tied to one open tab; a `uzc_`/
  `uza_` token is *designed* to outlive a session (that's the whole point of
  a CLI credential), and the WS ping (`wsPingInterval`, 30s) keeps the socket
  up indefinitely rather than letting an idle tab time out on its own. So
  this is **parity in mechanism** (the server has never had a way to kick a
  live socket on revocation) but **wider in blast radius**: a leaked, revoked
  `uzc_` can watch a run far longer, in expectation, than a leaked session
  ever could. A hub-level kick-on-revoke is its own future backend PRD, not a
  TODO folded into this one — recorded here so it isn't lost, not because
  this PRD is where it gets built.

## Open Questions

- **Entry name and jump-in.** `uzi tui` (board) + `uzi tui <run>` (one run) — or a
  separate `uzi watch <run>`? Leaning `uzi tui [run]`, single command.
- **Board liveness.** Poll interval for `ListRuns` (2s like the log follow, or
  slower with a manual `[r]efresh`)? A board-level WS is deferred (D3); revisit
  only if the poll proves too laggy in practice.
- **Theme — closed during implementation.** Adaptive, not fixed — but **not**
  via `lipgloss.AdaptiveColor`. In Lip Gloss v2, `AdaptiveColor` survives only
  in the `compat` shim, driven by a package-level `HasDarkBackground` probe
  evaluated at import time against `os.Stdin`/`os.Stdout` — wrong for a Bubble
  Tea program, which owns the terminal and fires a terminal query even
  without a TTY. The shipped palette uses `lipgloss.LightDark(isDark)`,
  fed by `tea.BackgroundColorMsg.IsDark()` — the background Bubble Tea itself
  reports once it has the terminal, not a second, independent detection that
  could disagree with it.

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

## Known Gaps (pre-existing, not introduced by this PRD)

Found during implementation and recorded rather than silently fixed or
silently ignored, since neither is this PRD's to decide:

- **`/api/ws` has no per-user connection cap.** Pre-existing, and M1 doesn't
  make it worse — but M1 does make the exposure easier to reach in practice:
  the credential opening a socket can now be a long-lived, headless `uzc_`/
  `uza_` token instead of only a browser session tied to an open tab (see the
  R6 reframing above). Worth a limiter in its own right; out of scope here.
- **The web has the same `Cf`-stripping gap D7 closes for the TUI.**
  `web/src/pages/RunView.tsx:955` renders judge free text
  (`summary_md`/`rationale_md`) as escaped plain text — safe against
  HTML/script injection, since React never interprets it as markup, but not
  against a Unicode bidi override: the browser's own bidi algorithm still
  reorders a plain text node, so the same visual-spoof class D7 strips for
  the terminal is still reachable in the browser. Filed separately as issue
  #124, deliberately out of scope here — this PRD's D7 covers the TUI's own
  render path, not the web's.
