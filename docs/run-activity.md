---
title: Run activity pane
order: 37
audience: user
---

# Run activity pane

The run view's activity pane shows your crew at work: one lane per actor
with its own status dot, collapsed-by-default logs so a live run doesn't
drag your scroll position around, and a steer queue that tells you whether
a follow-up you sent actually reached the worker.

## Plan approval gate

Before implementation starts, a run parks at `awaiting_approval` with the
lead's plan on screen and three actions:

| Action | What happens |
|---|---|
| **Approve plan** | Locks in the agent selection and starts implementation. |
| **Request changes** | Send feedback in a text box; it goes to the *same* planning session (same run, same branch, full context retained), the agent revises, and the gate re-opens with an updated plan (`v2`, `v3`, ...) for you to review. |
| **Reject** | Fails the run with your free-text reason — unchanged. |

Revision rounds are bounded (the header shows "revision N of 3" by
default) and share the run's single approval-timeout budget across every
round — three rounds do not buy three separate 24h windows, see [Worker
setup](./worker-setup.md#concurrent-runs). An approve or reject sent while
a revision is in flight is discarded rather than silently applied to a
plan you never saw: only a decision made against the plan actually on
screen ever takes effect. Superseded plan versions collapse into a
history accordion below the current one, and your feedback for each round
is stored in the run feed alongside the plan it produced — visible to
admins the same way a reject reason or a follow-up already is.

Every verdict is bound to the exact gate it was sent against, not just
"the current one." Each time a plan is shown, the run's `gate_revision`
counts up by one (readable with `uzi run get <id> --field gate_revision`);
a client that names the revision it displayed — `expected_gate_revision`
on the API, or `--expected-gate-revision` on the CLI (see [the CLI
docs](./cli.md#commands)) — gets a 409 (`gate_revision_mismatch`, naming
the current `current_gate_revision`) instead of applying to a plan you
never saw, whenever the run has since moved to a different revision or
left the gate entirely. An **approve** sent while no gate is visible at
all (nothing displayed yet, or the run has already moved on) is silently
ignored, with a note in the run feed — it can never land against a later
plan by accident. A **reject** or **request changes** sent the same way is
not ignored: it still applies at the run's first gate, the same
fail-closed handling these two actions have always had.

For ordinary plan gates, if a worker cannot re-present a gate it previously
showed — for example after a restart or credential switch that lost track of what was
on screen — the run parks at `recovery_wait` (see [Recovery
wait](./run-recovery-wait.md)) rather than showing a gate it can't stand
behind, and a fresh claim retries. This is bounded: past
`RUN_GATE_REFUSAL_MAX` such refusals for one run (see
[Configuration](./configuration.md)), the next one fails the run instead
of parking it again, with reason `gate_presentation_refused`. Cross-check's
irrecoverable receipt/presentation ACK losses instead fail terminally; see
[Terminal delivery failures](./cross-check.md#terminal-delivery-failures).

If the planning turn itself wrote anything to the worktree — a file the
agent created or modified while it was still just planning, which would
otherwise be swept invisibly into the first implementation commit — the
gate also shows a **Files changed during planning** list. It's
surface-only: uzi doesn't block the write or the approval, it just makes
sure you're approving with the full picture. A plan turn that touched
nothing shows no such list. The same list is available from the terminal
via `uzi run get` (see [the CLI docs](./cli.md#commands)).

Autopilot normally skips the human gate. With [Plan cross-check](./cross-check.md),
a Claude or Codex lead proceeds after the latest exact-plan pass by the other
model family. Eligible changes requested receive bounded automatic revisions;
exhaustion, blockers and check failures normally force this gate. A Codex lead
on a worker without `cross_check_codex_lead_v1` parks for a human.
See [Autopilot](./autopilot.md).
A full revision round also works end to end from
[Slack](./slack.md#using-it), without opening the web UI.

### Plan cross-check evidence

The plan panel and run detail show **Plan cross-check** evidence separately
from the current human gate: checked-candidate outcome, findings, checker-run
link, recorded model/effort and reported tokens/cost. The activity feed
includes distinct plan cross-check rounds even when there is no agent lane.
Automatic revision events carry their checker-round identity separately from
human revision counters. The summary describes the latest checked candidate;
earlier rounds remain feed history. Pending,
passed, changes requested, blocked, verdict deadline, checker model timeout,
malformed/model/confinement failures, interruption and refusal have distinct
labels; unknown or inconsistent verdict/reason pairs show unavailable.

Findings use hardened Markdown, capped at 16,384 source characters across
20 items, with a notice when truncated. Metadata is bounded and sanitized;
missing model/effort is unreported, missing or unreported cost unavailable,
and legacy `subscription` usage shows **No estimate**. Priced Codex checks
in either auth mode show API-equivalent cost, not a bill; see
[run cost](./run-cost.md#metered-subscription-and-unreported-cost).
New plan cross-check events
refresh detail; a failed refresh waits for a later new event rather than
retrying indefinitely.

After a human revision, the original candidate is **Earlier-plan evidence**
and does not certify the current plan. The current gate reason can clear
while those findings remain visible. Decisions still act on the established
presentation and revision; a status read cannot create or replace that gate.
No automatic checker revision or fresh check is added by a human revision.

### Advisory draft captures

Before validation and after each revision, the lead can explicitly call
`save_draft_plan` with its draft Markdown. The exact response is
"Draft capture requested; this is not plan submission or approval."
The activity log renders the latest valid capture by sequence number in both
Timeline and By agent, labelled **draft, unapproved, possibly incomplete**.
Captures are capped at 32 KiB after redaction; a truncated card shows a notice.
Earlier captures remain stored and available in raw CLI JSON, even though their
cards are suppressed in the web presentation. The existing activity display cap
still applies. Subsequent validator reports remain ordinary messages in Timeline
sequence order.

Capture uses normal message delivery and best-effort close: a circuit breaker or
deadline may leave an unacknowledged capture undelivered. The response does not
promise it was saved. A capture offers no approval controls and never becomes the
Plan tab's submitted plan. It cannot recover a plan from prose or a later prompt,
and is never automatically adopted; a retry requires fresh review and ordinary
plan submission and approval.

## Answering a question

Beyond the plan gate and a follow-up you send yourself, an agent can stop
mid-task and ask **you** something — the run's third human-in-the-loop
channel. When it does, the run parks at `awaiting_input`, its badge reads
**needs your answer**, and an **Answer required** panel appears with the
question (rendered as markdown), any suggested options as toggleable chips,
and a free-text box per question — answering in your own words is always
available, whether or not options are offered. If the agent batched several
questions into one stop, they all appear together and **Send answer**
submits them as one round; if it asked more than once in the run, the panel
carries a small `q2`/`q3`/... marker so you know which round you're on. The
run is still cancellable while parked, the same escape hatch a revising plan
gate offers.

**Answer from wherever you're already watching**: the web composer above,
a reply in the run's Slack thread if you've linked your account (see
[Slack notifications](./slack.md#using-it)), or `uzi run answer <id>` from
the terminal (see [the CLI docs](./cli.md#commands)) — all three read the
same open question off the run's feed, so no surface invents a question the
others don't have.

A few things worth knowing:

- **Never paste a credential, token, or password into an answer.** The agent
  is instructed to never ask for one; a question that does is itself a red
  flag, wherever it reaches you. Question text is written by the agent from
  repository and issue content, so a hostile file in a repo can steer what
  you're asked. Your answer is scrubbed of known credential patterns and
  length-bounded regardless of which surface you answer from, but that's a
  backstop, not a reason to test it.
- **Nobody answers ⇒ the run fails**, "clarification timed out", once the
  answer deadline passes (`QUESTION_TIMEOUT_SECONDS`, 24h by default) — there
  is no configurable default action. The timer is held by the worker, so if
  it dies and the run is picked up again the clock restarts. With no other
  fresh executor execution, worker-death retries contribute
  `QUESTION_TIMEOUT_SECONDS × (RUN_MAX_REQUEUES + 1)` — 96h on defaults.
  The question cap (`QUESTION_MAX`, default 5 per attempt) resets the same
  way: `QUESTION_MAX × (RUN_MAX_REQUEUES + 1)` — 20 questions on defaults
  under that condition. Only the initial episode can use the once-per-run
  finalize-resume allowance (#1742) with a positive cap, making these
  multipliers `RUN_MAX_REQUEUES + 2`. Ordinary transient/limit/credential
  redispatch can create fresh `execute()` within the same episode, resetting
  worker-memory question budgets without changing its number or charged
  `requeue_count`. `RUN_MAX_REQUEUES` bounds charged worker-death retries,
  not total attempts or questions per episode or lifetime.
- **Autopilot runs never park on a question.** With nobody in the loop, the
  agent auto-resolves with "proceed on your best judgment," notes the
  assumption it made in the run feed, and keeps going — see
  [Autopilot](./autopilot.md).
- **A run resumed onto a worker from before this feature shipped won't
  surface a new question at all** (it guesses instead, mid-run, or fails
  pre-run) — a narrow window during a rolling upgrade, not a steady-state
  concern. If a run you answered doesn't move, open it in uzi and check
  whether it's actually still waiting.

### When planning ends without a plan

Sometimes a plan (or plan-revision) turn ends with the lead just writing
prose: no plan submitted, no question asked. uzi resumes the same session
once with a fixed corrective nudge, so most of the time you never see this.
If the nudged turn is *still* prose only, a status card appears in the feed
showing the lead's last message (bounded, secret-scrubbed, and rendered as
plain text, never markdown), and, on an attended (non-autopilot) run, the run parks at
`awaiting_input` on a uzi-authored question headed **Plan missing**. Answering
with guidance resumes planning in the same session, but that answer is never
plan approval: a plan still has to be submitted and go through the approval
gate. You can also cancel the run instead. The usual answer deadline applies.
There's no "revise the PRD and retry" option here: to change the PRD or issue,
cancel and re-dispatch. If planning is still prose-only after your guidance,
or the run is autopilot (or otherwise has no one to ask), the run fails
instead of parking again.

## Lanes: one per actor, not one per turn

**By agent** (the default) gives every actor a single lane holding its whole
contribution, however many times it spoke. A lead that delegates, waits, and
delegates again is one lead lane, not four near-empty bars.

Crucially, an actor is an *invocation*, not a role. When the lead runs two
`coder` subagents in parallel they get **two separate lanes**, each titled by
its own task:

```
coder · API wiring      ● working
coder · web gate UX     ● waiting
```

Their messages interleave in real time and still land in the right lane,
live and after a reconnect. Without this they would merge into one garbled
`coder` block — which is what a naive "group by agent name" would do.

**Timeline** is the other half of the toggle in the pane header: the raw
chronological stream, grouped the way it was before lanes existed. Reach for
it when you need to see the exact cross-agent ordering. The choice sticks
across runs and reloads.

Two fallbacks, both deliberate:

- **The lead, and any run from before lanes shipped**, carries no invocation
  id, so those messages fall back to **one lane per role**. Old runs
  therefore re-render as coalesced role lanes under By agent; `Timeline`
  reproduces exactly what you used to see. Nothing was migrated.
- **A subagent with no task label** shows the bare role name, with no `·`
  suffix and no placeholder.

Labels are model-authored, so they render as plain single-line text,
truncated with an ellipsis when long — never as markdown.

> **Subagent lanes are mostly tool activity.** By default the agent SDK
> forwards only a subagent's tool calls and results upstream, not its prose,
> so a subagent lane shows what it *did* and little of what it *said*. That
> is expected, not a bug or a dropped message; a lane that looks thin is a
> lane doing tool work. Turning the prose on is a separate, deliberate change
> — it multiplies message volume and token cost, so it is not bundled here.

## Crew roster

Every lane header carries its own dot, so a small By-agent crew needs **no
separate roster strip** — the collapsed lanes *are* the roster. A strip
appears only when it can tell you something the lanes cannot: when a role is
**doubled** (two or more invocations) or there are more lanes than fit a
glance. Then you get a **role rollup** — one chip per role with a count and
a single dot:

```
coder ×2  ● working      tester ×2  ● stalled
```

**A rollup dot shows the role's _worst_ state, not its most active one**, so
a stalled tester surfaces past a healthy sibling. That means a chip can read
`waiting` while one of its own lanes is visibly pulsing `working` — the chip
is a summary of what needs attention, the lane is what is happening now.
Click a chip to expand and jump to that role's lanes.

In **Timeline** view the roster stays the per-role jump strip it has always
been, because a scattered chronological stream still needs a navigation aid.

The state dots themselves, in both places:

| State | Meaning |
|---|---|
| working (pulsing) | The newest speaker, and the run is healthy — see [Run health](./run-health.md). Stays `working` through a long tool call (a build, a test suite); it does not time out on its own. **Exactly one lane** pulses, even when one role has two live invocations. |
| stalled (amber) | The newest speaker, but the run's health has flagged it `stalled` or `looping` — a looping agent never reads as healthy green. A **near timeout** flag is a budget fact about the run, not evidence the speaker is unhealthy, so it does **not** turn the lane amber. |
| waiting | Either everything, while the run is blocked on a plan approval or has no worker claimed yet; or a lane that spoke recently but isn't the newest. |
| idle | A lane that hasn't spoken in a while. |
| done | Everything, once the run has finished (completed, failed, or cancelled). |
| *(empty state)* | No agent has spoken yet — a single muted "waiting for the first agent…" placeholder, not zero lanes. |

**There is no colour legend on screen, deliberately** — the state word sits
next to every dot ("● working", "● idle"), so a key would just repeat it. The
dot also carries the same text as a tooltip.

The `waiting`/`idle` split is a recency heuristic (no precise handoff signal
exists yet), so it can lag up to 30 seconds — cosmetic only, it never affects
the `working`/`stalled`/`done` states above.

## Lead context meter

A molten "steel channel" meter shows how full the **lead's** live context
window is right now — the fill that predicts the SDK's autocompaction, not
token spend (see [why a hosted run costs less](./run-cost.md) for the spend
side). It lives on the **lead lane header** in By-agent view (`sm` screens
and up, next to the timestamp), and as a small micro-meter + `%` on the
**lead's crew chip** whenever the crew rollup is showing (a doubled role or
more lanes than fit a glance, see Crew roster above) — on a small run with no
rollup, the lane meter is the one that's always there.

Three states track how close the lead is to compaction:

| State | Fill | Meaning |
|---|---|---|
| cool | `< 70%` | Plenty of room — a quiet steel bar, no glow. |
| molten | `70 to <95%` | Filling toward the line — an orange glow. |
| near-compaction | `≥ 95%` | Into the danger wash — a pulsing rose glow (steady, not pulsing, under reduced motion). |

**100% is the compaction line**, not a 90% warning tick: the SDK's
`rawMaxTokens` already *is* the autocompact window, so the channel spans the
whole budget and the top of it is where the SDK summarizes older turns. The
bar clamps at 100% width; the label keeps showing the true number if the
reading comes back over 100.

**Subagent lanes carry no meter.** The SDK only exposes the main-loop
window — the lead's — so a Task subagent's own context fill isn't
observable at all, not just hidden. If a future SDK release exposes it,
the same lane treatment would extend to those lanes.

## Logs: collapsed by default, opt-in Follow

Each lane's log is an accordion, **closed by default**, with a live
one-liner in its header ("running `go test ./...`") that updates in place
and a `+N` pill for messages you haven't seen while collapsed. Both sit on
the lane itself, so an actor that spoke five times still has one header
telling you what it is doing now. A finished run, or a run with only one
actor, auto-expands so you're not stuck clicking through every accordion to
read a result; **Expand all** / **Collapse all** are always one click away.

**Follow live**, off by default, tails only the *expanded* lane's own log
as new messages arrive. This replaces the old whole-pane auto-scroll: a
burst of tool activity now updates the crew strip and unseen-count pills in
place, without yanking your scroll position around.

## Steer queue

Every follow-up you send shows up in the steer queue immediately, and moves
through these delivery states:

| State | Meaning |
|---|---|
| Queued | Not yet picked up by the worker — including while the run is sitting at a plan-approval gate. |
| Received | The worker has fetched it. It is not in a prompt yet. |
| Routed | The run's steering has acted on it: it is lined up for the next turn. |
| Received / Routed — waits for approval | Fetched while the run sits at a plan-approval gate. It reaches a prompt in the first implementation prompt after you approve. |
| Received / Routed — awaits your answer | Fetched while the run waits on a clarification question. It rides the next ordinary turn after you answer. |
| Received / Routed — resumes the run | Fetched while an interactive run waits for its next follow-up; it becomes the next turn and resumes the run. |
| Included in a prompt | The turn carrying it reached the model: it is in a prompt the agent was given. |
| Not delivered — run finished | The run went terminal before the worker ever fetched it. |
| Not confirmed — run finished | The worker fetched it, but no prompt carrying it was confirmed before the run finished. The receipt may have been lost at shutdown, so this is not proof it was never carried. |
| Received / Routed — no inclusion report | The worker predates inclusion reporting, so uzi cannot say whether a prompt carried it. |

**When a follow-up is included.** Claude and Codex runs behave identically.
Each owner follow-up is included in the next ordinary implementation prompt,
one per turn, oldest first, so with several queued a later one stays Received
or Routed until its turn starts. A follow-up sent at the plan gate is included
in the first implementation prompt after you approve. Completion-rework,
clarification and secret-gate turns carry only their own text; a follow-up
that is waiting rides the next ordinary turn. "Included" is recorded once the
turn carrying it reaches the model (the agent's first model activity in that
turn). The follow-up is fenced as untrusted input in the prompt. After a
resume, a follow-up that a worker received, and that the worker reports
inclusion for, but that was never included is sent to the lead again at every
re-claim; rows received by an older worker that does not report inclusion are
not re-sent. Delivery is therefore at-least-once: a follow-up can be included
again after a resume if the worker could not report the first inclusion. Chat messages are included when
their chat turn starts. Diff-review, judge, job and isolated research runs
never include follow-ups.

**"Included" means a prompt carried it, not that the agent acted on it.**
Whether it changed what the agent did next is visible in its following
messages, not in the chip. A follow-up can be Included and still have no
effect: the run finishes, pauses, or hits its scope ceiling before the agent
gets to it, or the agent weighs it against the plan and does something else.
A follow-up buffered at a plan gate is never included if you **reject** the
plan instead of approving it. A crash is not silent for long: a stalled
agent trips the [`stalled` health flag](./run-health.md). When a follow-up
did not land, send it again: on a run that has not finished (a paused one
included), from the steer queue if you can steer the run; on a finished run,
in a new run.

The queue stays visible, read-only, after the run finishes — so a
"Not delivered — run finished" or "Not confirmed — run finished" input doesn't just vanish.

### Scope directives

A **scope directive** — `uzi run stop` or `uzi run scope --through N` on a
milestone-structured issue run (see [Stopping or narrowing a
run](#stopping-or-narrowing-a-run) below) — shows up in the same queue, but
it never goes through the delivery states above. A scope row is never
"consumed"; its state **is** its disposition, so the chip tells you outright
whether the directive changed anything:

| State | Meaning |
|---|---|
| Active — scope ceiling set | The ceiling is set; the run finalizes its committed slice once it reaches it. |
| Applied — finalized at the ceiling | The worker reached the ceiling and finalized there. |
| Superseded — a later directive replaced it | A later `run scope`/`run stop` overwrote this one before it took effect. |
| Declined — not acted on | The run completed normally despite the directive — it finished its milestones (or the ceiling was never reached), so the directive changed nothing. |

This closes the gap the note above names for a plain follow-up: where
"Received" only tells you the worker *has* a follow-up, a scope directive's
chip tells you whether it *fired*.

## Milestones and the now line

On a milestone-structured issue run (see [Stopping or narrowing a
run](#stopping-or-narrowing-a-run) below), the web run view marks each
approved milestone `✓` reported complete, `◐` reported in progress, or
`○` not started; `uzi run get` shows the same three states as text
(`done` / `in progress` / `left`). **"Reported complete" is exactly what
it says** — uzi shows what the worker reported and hasn't itself verified
the work, so the wording never says "done" or "verified".

The in-progress milestone is also where the **now line** attaches: the
active lane's role, its task label, its last tool, and how long ago it
last spoke — e.g. `● coder  Decouple ci_fix detector from branch naming ·
Edit api/internal/poller/ci_autofix.go  40s ago`. Those words come from the
run's newest `tool_use` frame, never from raw tool input: a file path for
`Read`/`Edit`/`Write`/`MultiEdit`, the dispatch description for `Agent` or
`Bash` — a `Bash` command itself is never shown. When one or more
in-progress milestones have an agent attributed — the exact
`subagent_type` the lead dispatched, plus an optional label — **every**
attributed in-progress milestone shows the agent working it: its declared
role and label; an in-progress milestone the lead did **not** attribute
keeps its plain in-progress mark, with no now line. The live tool and the
"how long ago it last spoke" piece still attach to at most one milestone:
the one whose declared agent uniquely matches the run's current activity.
Two lanes sharing a role can't be told apart without the invocation id the
lead doesn't have, so when a role repeats, live tool/age is withheld from
every attributed milestone rather than guessed onto the wrong lane — role
and label alone still show on each. When **no** in-progress milestone is
attributed — nothing declared, or every declaration dropped or stale — the
now line attaches to the first in-progress milestone by the approved
order; the rest still show their plain in-progress mark. A run with
activity but nothing declared in progress shows an unattached now line
under the milestone header instead.

**On the TUI**, the in-progress milestone shows up two ways. In the crew
rail's milestone checklist it's a `◕` that blinks `◕`/`○` on a half-second
tick in the **faint grey** colour — the same colour as a not-started `○`, so
the row reads as one grey circle pulsing between mostly-filled and empty; it's
told apart from a not-started row by the `◕` shape, its motion, and a brighter
title, never by colour. The board's per-run micro-bar and the crew rail's
eyebrow micro-bar instead blink `▰`/`▱` in the **tungsten** colour (the same
warm accent as the done fill), one cell per milestone in progress. A static
frame renders when the terminal isn't interactive (a piped or offline render)
or when blink is turned off — the checklist row's static frame is the filled
`◕` (never a bare `○`, which would be indistinguishable from a not-started
sibling) and the micro-bar's is `▱`.
Set `UZI_TUI_NO_BLINK=1` to pin the static frame yourself, a reduced-motion
opt-out. The crew rail
carries its own now line under the in-progress row (`↳ <role> · <age>`,
with the task label beneath it), and the board's selected row gains a
second line with the same information. `uzi run get` shows the same now
line as a `NOW` row — see [the CLI docs](./cli.md#commands).

## Progress estimate

Live issue runs show a progress percent on the web dashboard and runs list,
on the TUI board and in `uzi run get`, so you can tell at a glance how far
along a run is without opening it. The server derives it from the run's
frozen milestone list: `11% + 89% × completed / total`, capped at 99% until
the run finishes. The 11% is planning, the measured median share of a run's
time spent before the first milestone starts. A run with three milestones
and two reported complete shows `70%`. As with the [milestone
marks](#milestones-and-the-now-line), "completed" is what the worker
reported, not something uzi has verified.

Only issue runs with a frozen milestone list get a percent; other run kinds
show no percent. A flag replaces the number whenever a number would
mislead:

| Flag | When |
|---|---|
| Waits on you | The run is awaiting approval, input or a follow-up. The web row and the CLI name which: `plan gate`, `question` or `follow-up` (the web row adds `since HH:MM`); the TUI board shows only `waits on you` (`on you` on a narrow terminal) |
| Parked | The run is in a usage-limit, pool or recovery wait, or paused. The CLI and TUI board say which: `limit wait`, `pool wait`, `recovery wait` or `paused` (the board abbreviates to `⏸ limit`, `⏸ pool`, `⏸ recov`, `⏸ paused`). On the web the row's status pill already says it, so the progress cell adds only screen-reader text |
| Queued | The run has not started yet (on the web, the status pill says it and the cell adds only screen-reader text) |
| Stalled | The run's health is stalled or looping. The web row (`◼ stalled` over a `since HH:MM` line, replacing the separate health pill on that row) and the CLI (`stalled · since HH:MM`, UTC) show since when; the TUI board shows only `stalled` |
| Planning | The run is planning and has no frozen milestone list yet (on the web, the status pill says it and the cell adds only screen-reader text) |

When there is no estimate at all, the web cell shows `—`, the TUI board cell
is blank and `uzi run get` omits the row.

**No time remaining or finish time is shown.** The estimate is too
unreliable to display: in a backtest of completed issue runs, only 59% of
the estimates fell within 2x of the actual time remaining.

Where it appears:

- **Web** (dashboard and runs list rows): a Progress cell, `70%` with a thin
  bar, or `● waits on you` / `◼ stalled` as above. On the Dashboard the cell
  is hidden at phone widths; the runs list keeps it.
- **TUI board**: a `PROG` column after `MILES`, e.g. `70% ▰▰▰▰▰▰▱▱`. The bar
  drops before the percent on narrow terminals, leaving the percent or a
  short flag. Every flag is text, so `NO_COLOR` keeps all of them. The board
  shows no "since" time.
- **CLI**: `uzi run get` prints a `PROGRESS` row, e.g. `≈70% · milestone 3
  of 3`, `stalled · since 14:05` or `waits on you · plan gate`; a run
  waiting on a question may add ` · may be blocked by <id>` (see [May be
  blocked by](#may-be-blocked-by)). The row is omitted when there is no estimate and once the run is terminal. See [the CLI docs](./cli.md#commands).

### Run page and TUI detail

The run page and the TUI run detail show the estimate in more depth. The two
differ in what they carry; the run page has more.

- **Run page**: a progress card under the budget facts. It shows the percent
  large (`≈70%`), then `milestone k of M · <active milestone title>` (or
  `N of M milestones done` when no milestone is active), and a segmented plan
  track: one segment for planning plus one per milestone, each marked done, in
  progress or pending. While the run is progressing with a known role, it also
  shows the **phase** steps `review`, `validate` and `implement`, with the
  current one marked `▸`. The phase is mapped from the active role:
  `reviewer`, `auditor`, `fact-checker`, `architect`, `web-ux`, `tui-ux` and
  `dba` are `review`, `tester` is `validate`, and any other role is
  `implement`. It is the phase right now, not a history. An **Active role**
  line shows only while the run is progressing or stalled. The flags from the
  table above replace the number when it would mislead: `◼ stalled · since
  HH:MM`, `● waits on you · plan gate|question|follow-up since HH:MM`, the park
  word (`limit wait`, `paused`, ...), `queued`, and `planning · no
  milestones frozen yet`. A parked card adds `resumes HH:MM` only for a
  usage-limit or recovery wait whose resume time is known and in the future.
  There is no card when the run has no estimate.
- **TUI run detail**: a compact `PROGRESS` block above `MILESTONES` in the
  rail: a `PROGRESS ≈70% · 3/4` line, plus `phase ▸ implement` while the run is
  progressing. The flags are shown as `◼ stalled` with `since HH:MM`,
  `● waits on you` with `plan gate|question|follow-up since HH:MM`,
  `⏸ <park word>` (no resume time), `queued`, and `planning` with
  `no milestones frozen yet`. It does not show the active-role line or the
  resume time; use the run page for those.

### Now summary

While an issue run is working a milestone, a small model can write one plain
sentence (at most 120 characters) saying what the active lane is doing, and
the progress card, the TUI detail and `uzi run get` show it next to the
estimate. It is **on by default**; see [Turning it off](#turning-the-now-summary-off).

- **Run page**: a `Now: <text> · model summary · <age>` line under the phase
  chip.
- **TUI run detail**: a `now <text> · model summary · 2m ago` line in the
  `PROGRESS` block. The text wraps under the `now` label; the
  `model summary · <age>` marking moves to its own row when it does not fit.
- **CLI**: `uzi run get` appends ` · now: <text> (model summary, 2m ago)` to
  the `PROGRESS` row.

The text is the model's reading of the run's newest tool calls, not a status
uzi has verified. The instruction forbids saying a step passed, failed or
finished unless a tool call states it, but a model can still be wrong or
vague; read the transcript when it matters. Every surface renders it as
inert plain text: control and format characters are stripped when the API
ingests it, and it is capped at 120 characters.

**When it shows.** Only on the single-run read (the run page, `uzi run get`,
the TUI detail), never on the dashboard, runs list or TUI board. It needs the
progress state to be a percent (an executing, healthy issue run with a frozen
milestone list), an active milestone, and the Now summary enabled both by the admin switch and
in the run owner's settings.
It shows the newest note written for the *current* milestone, so a note from
an earlier milestone never lingers. A waiting, parked, queued or stalled run
shows no note, even one written for its still-active milestone. The note
messages are hidden from the activity transcript on the web, in the TUI and
in `uzi run logs` (`--json` still carries them), and the run judge never sees
them. A note does not count as run activity, so it never revives a run that
is stalled or hides its stall.

**When the worker writes one.** The worker calls the model when the active
milestone changes, when the active role changes, or when 10 minutes have
passed since the last call and at least one new tool call has appeared. It
makes at most one call per 5 minutes (triggers inside that window coalesce
into one call at its end), and a call that fails or whose result is discarded
still counts against the window. Each call times out after 30 seconds. It makes no call while the run
is held (a plan gate, a question, a pause or park, a credential switch), when
the setting is off, or when no milestone is active; after a credential-switch
attempt gives up, summaries resume at the next trigger. A result that arrives
after one of those is discarded.

**What the model sees.** The active milestone's title and the newest 20 tool
calls, each cut down to role, label, tool and detail (the same fields the
[`now` line](#milestones-and-the-now-line) reads; never a Bash command), with
claim secrets redacted.

| Harness | Model | Limits |
|---|---|---|
| Claude | `haiku`, extended thinking off | 256 output tokens |
| Codex | `gpt-6-luna` at reasoning effort `low` | The Codex advice path cannot enforce an output-token cap, so the 30-second timeout is the only bound. `low` is the lowest effort uzi's Codex contract allows |

There is no fallback to a more expensive model: if the account refuses the
model, or the call fails or times out, that summary is skipped and the run is
unaffected. A rate-limit error from the summary call never touches the run's
own rate-limit state, never parks it and never triggers a credential switch.
The Codex call is also denied credential refresh, so it can never move the
run's credential generation; a summary that lands on an expired Codex token
just fails.

**Spend counts.** The summary calls are paid on the run owner's credential
and their tokens and cost are part of the run's usage. They are stored under
a separate per-note usage key (`progress_note:<model>`), so they never merge
into the run's own rows for the same model, and the web usage panel shows
them as a **Now summaries** row that adds up to the **Run total**. A Claude
call with no provider cost is priced from the standard Anthropic price table;
a call the table cannot price shows cost unreported, never `$0`, with its
tokens kept. The Codex `gpt-6-luna` price row is in the pinned table
(verified 2026-10-09). If a call is aborted (a park, the run ending, a
cancel, a credential switch), the usage it had already received is still
recorded once; spend is unavailable only when no usage arrived before the
abort. If the worker loses its claim, usage frames from the stale claim are
rejected as for every result frame, so that call's spend can be missing.
With one call per 5 minutes at most, a run makes at most 12 calls per hour.
The [Codex price coverage](admin-health.md#codex-price-coverage) health check
counts summary usage under the plain model name.

#### Turning the Now summary off

Two switches, both on by default; the summary runs only while both are on:

- **You**: **Settings → Run defaults → Run summaries → Model-written Now
  line** (`now_summary_enabled` on `PUT /api/me/settings`; unset inherits
  on). `uzi settings get` prints `Now summary: on (default)`, `on` or `off`.
- **Your admin**: the instance kill switch in [Admin
  settings](admin-settings.md#run-summaries). Off there stops it for every
  user, whatever their own setting.

The worker reads the effective value on its regular input poll, so turning it
off reaches a running run: no further call is made, a call still in flight is
discarded, and the line disappears from the run page. A worker that does not
receive the setting treats it as off. See [Run
summaries](run-summaries.md#the-now-summary) for how this differs from the
intent and plan summaries.

### May be blocked by

While a run is waiting on a question (`awaiting_input`), the run page, the TUI
detail and `uzi run get` can add `may be blocked by <short run id>`, linking to
that run on the run page. It appears when the open, not-yet-answered question
mentions `#N` and another live run of the **same run owner** on the **same
repo** is working issue N: an issue run for N, or an MR rework or CI fix run
on branch `agent/issue-N`.

It is a hint, not a recorded dependency: a mention in the question text is all
it goes on. It never matches another user's run, another repo or the run
itself, and at most 5 `#N` references in the question are read. Once the
question is answered the hint disappears. It is shown on the detail views
only, not on the board or runs list.

## Stopping or narrowing a run

On a milestone-structured issue run, two operator actions bound how far the
run is allowed to go, and both leave the frozen milestone list untouched —
they cap execution over it, they don't rewrite it:

- **`uzi run stop <id>`** caps the run at whatever it has already completed:
  the worker finishes its current turn, starts no further milestone, and
  finalizes — pushes the branch, opens the MR if one was requested.
- **`uzi run scope <id> --through N`** caps the run at milestone `N`
  instead, letting it complete through a later milestone before finalizing.

Either way, the resulting MR is annotated as a **partial delivery** (it
notes how many of the approved milestones shipped and how many the operator
deferred) and does **not** close the issue — a human still decides whether to open a
follow-up run for the rest. A later directive **supersedes** an earlier
one: `scope --through 4` then `scope --through 5` finalizes at 5, not 4, and
`stop` after either finalizes immediately at the completed count. See [the
CLI docs](./cli.md#commands) for the exact commands, and
[ADR-634](../adr/0634-run-scope-steering.md) for the design rationale.

## When the worker cannot prove a run stopped

Before it pushes a run's branch, captures its work, or reseeds its clone,
the worker checks that everything the run started has actually stopped: the
processes its tools spawned, and, best-effort, the Docker activity it
began. An unresolved Docker container never blocks anything on its own; only
an unproven **process** is treated as a run still running.

What happens next depends on where in the run this check comes up, because
most of the places that check are not the end of the run:

- At a wall-clock, usage-limit, or completion-hold park, the check being
  unproven means the worker skips the capture for that park entirely — no
  checkpoint publish, so the latest local work stays on this worker only
  for now (a checkpoint publish targets a separate checkpoint ref, never
  the run's branch directly) — but the park itself still stands: a
  same-worker resume recovers the kept clone's local work through a capture
  of that clone, which itself needs the clone proven stopped (on a
  Docker-wired worker, that later capture can still fail the run with
  worker residue blocked), while a resume on another worker recovers only
  from whatever checkpoint was last durably published.
- At a pause the owner requested, it means the pause itself fails: the run
  reports **pause failed** and keeps running rather than stopping on
  unproven ground.
- At a milestone checkpoint, it means that one checkpoint's publish is
  skipped and the run continues to its next milestone.
- During a credential switch, the worker retries capturing a verified
  restore point a bounded number of times. If it never succeeds, it reports
  **credential switch failed**; when the server confirms the failure stamp
  cleared, the run normally continues on the old credential in place (its
  clone and session are kept); otherwise it is stopped and left to be
  requeued rather than continuing on uncertain ground.
- A recovery capture that cannot prove the clone stopped is retried, up to a
  bounded number of times; if the clone still can't be proven stopped after
  that, the run fails the same way finalize does (see below), with the clone
  kept.
- On graceful shutdown, an unproven clone means nothing is published to the
  run's checkpoint; the pending requeue stands, and a later resume recovers
  from whatever was last durably saved.

The run actually **fails**, with `fail_origin = worker_residue_blocked`
(shown as **worker residue blocked**), at points where there is no safe
way to continue without the proof: the finalize gate that pushes the run's
branch (and its re-proofs after any git operation that could have started
something new), capturing a predecessor attempt's or a reclaimed orphan's
work (both on a Docker-wired worker), a canonical clone reseed that cannot
free the path it needs, and a recovery capture whose proof stays blocked
past its bounded number of retries (above). Seeding a fresh attempt clone
for a new execution attempt is the one site that is let through when the
only problem is a process it cannot positively attribute to anyone — the
new path is untouched by that process either way, so nothing is moved or
freed by proceeding. It still fails, the same as everywhere else, on any
other unproven verdict, including a process it *can* place in scope (another
live attempt's process, or an unmarked in-scope process left in a non-live
path). A blocked check during cleanup **after** a
run has already reached its own outcome — retiring a finished run's clone,
for instance — does not itself fail the run: the clone is simply kept in
place instead of being removed, and the run's own status and failure
reason (if any) stand unchanged.

When the check is unproven because a specific process's environment or
working directory could not be read, so the worker cannot positively
account for it, the failure reason names that process's process ID and
program name, so an operator knows exactly what to look for on the worker.
That process does not have to belong to the run that failed — any such
unaccountable process, running as the same worker user, blocks a Claude or
stub run's own checks at those sites, and blocks the canonical clone reseed
and, on a Docker-wired worker, every capture of another attempt's clone,
regardless of which harness the run belongs to (seeding a fresh attempt clone is the one
exception described above), by design:
the worker would rather refuse to proceed than guess. **A Codex run's own
checks are a disclosed exception**: they don't scan the process table at
all, relying instead on Codex's own proof that its processes have drained,
so an unaccountable process elsewhere on the worker does not block a Codex
run's own park, finalize, shutdown, retire, or recovery and credential-switch
capture the way it blocks a Claude or stub run's. The exception is a run
capturing a predecessor attempt's clone: its recovery capture always scans.
See [ADR-1783](../adr/1783-run-quiescence-and-attempt-clone-paths.md)
for exactly which of those checks still has no process proof at all. A
process the worker *can* positively tie, by ancestry, to another live run's
own recorded root, or to a long-lived process the worker itself launched
(not the agent), is not this case: it is attributed and does not block.
When the check is unproven
instead because an in-scope process was seen but could not be confirmed
stopped, the failure reason reports only how many such processes survived
the reap, not their pid or program name. Either way, to clear a block on
an unaccountable process, stop the named process on the worker (or wait
for it to exit on its own), then start a new run.

This is a worker infrastructure problem, not something the agent did wrong,
so a run that fails this way is never sent to the judge. Nothing is
published from the unproven state; commits the run had already pushed
before this point stay on its branch. Start a new run once the worker is
healthy again.

## When the skills plugin fails to load

The worker loads the skills selected for a run into the agent's session as
a local plugin. The selected skills are the ones that fit within the
per-run skill size and count limits. If the Claude SDK reports that the
plugin failed to load when the session starts, and the run has selected
skills, the run **fails** with `fail_origin = skills_plugin_load_failed`
(shown as **skills plugin load failed**) rather than carrying on without
the skills you chose.

The failure reason names the plugin, the error type, the path it tried to
load when the SDK reports one, and a trimmed error message, for up to the
first three errors: the length bound can cut the second and third. If there
are more than three errors, the reason ends with a count of the rest.
Secrets are redacted, control characters are removed, and the reason's
length is bounded. An error report the worker cannot parse still counts as
a load failure.

The run is not parked or retried automatically, and because this is a
worker environment problem rather than something the agent did wrong, it is
never sent to the judge. Fix the skill or plugin the reason names, then
start a new run.

If the run has no selected skills, a plugin load error does not fail it:
a status line in the activity feed warns about the error and the run
continues. The worker rebuilds the plugin each time the run starts or
resumes, so the warning is posted once per start or resume, not once per
turn. Codex runs are unaffected.

## When Codex reports a provider safety-policy refusal

A failed Codex turn classified as exactly `cyberPolicy` or
`misalignmentPolicyViolation` has harness category `policy_refusal`.
When that root failure determines the run's outcome, the run fails with
`fail_origin = provider_policy_refusal` (shown as **provider safety-policy
refusal**). Its failure reason is one of two fixed literals:

- `Codex provider safety-policy refusal (cyberPolicy)`
- `Codex provider safety-policy refusal (misalignmentPolicyViolation)`

The reason and refusal metadata do not reflect provider messages, details,
content, or prompts. This records the provider's classification, not an
independent attestation that the task violated a policy. Unknown and
non-policy errors keep their existing mapping.

The refused execution is terminal: it does not automatically retry,
requeue, park, defer for disk recovery, or switch harnesses.
[Judge eligibility](./judge.md#which-runs-are-judged) is unchanged from
`agent_failure`; a retrospective may review substantial earlier work,
but that review does not retry execution.

A dedicated durable worker status event carries
`event: provider_policy_refusal`, `provider: codex`,
`category: policy_refusal`, `policy_tag`, `origin: root|child`,
`phase: planning|implementation`, and `correlation_id`.
The worker allocates this separate opaque ID at the originating root turn's
start or child admission; it is bounded to 52 characters. A child also carries
its admitted, sanitized `role` (1–64 characters from letters, digits,
`_`, `.`, and `-`) and `parent_correlation_id` pointing to that root.
These values are captured at admission. Provider call, thread, and turn IDs
and display projection IDs are excluded; existing display pairing is unchanged.

A refused child returns callback code `child_policy_refused`, preserving
`policyRefusal` metadata. A root refusal shares that metadata across its
error, result, and worker diagnostic. A refused child does not force its
parent to fail: a successful parent stays successful, an unrelated later
parent failure keeps its own origin, and a later root policy refusal carries
the root's provenance. The observation is separate from any winning cancel,
pause, wall-clock, or quiescence outcome.

Failure totals and the failure-origin bucket count run rows once; child
observations are not run outcomes. Analysis of refusal occurrences should
deduplicate by run ID, so a child observation and eventual run failure count
as one run. This adds no public metric or denominator.

The API allowlist and database migration must ship before workers' new origin
can be accepted. Historical refusals cannot be reconstructed if they were not
observed or durably recorded. Provenance travels with run-log retrieval and
is deleted with the run's history.

## From the CLI

`uzi run inputs <run-id>` shows the same queue from the terminal — see
[the CLI docs](./cli.md#commands).
