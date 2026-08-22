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

Autopilot runs skip this gate entirely — see [Autopilot](./autopilot.md).
A full revision round also works end to end from
[Slack](./slack.md#using-it), without opening the web UI.

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
  it dies and the run is picked up again the clock restarts: the honest
  worst case is the timeout **times one more than the requeue limit**
  (`RUN_MAX_REQUEUES`, default 1) — 48h, not 24, on the defaults. The
  question cap (`QUESTION_MAX`, default 5 per attempt) resets the same way,
  for the same reason, so its honest lifetime bound is likewise **× (requeue
  limit + 1)** — 10 questions, not 5.
- **Autopilot runs never park on a question.** With nobody in the loop, the
  agent auto-resolves with "proceed on your best judgment," notes the
  assumption it made in the run feed, and keeps going — see
  [Autopilot](./autopilot.md).
- **A run resumed onto a worker from before this feature shipped won't
  surface a new question at all** (it guesses instead, mid-run, or fails
  pre-run) — a narrow window during a rolling upgrade, not a steady-state
  concern. If a run you answered doesn't move, open it in uzi and check
  whether it's actually still waiting.

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
| stalled (amber) | The newest speaker, but the run's health has flagged it (`stalled`, `slow`, or `looping`) — a looping agent never reads as healthy green. |
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
| molten | `70–95%` | Filling toward the line — an orange glow. |
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
through one of these delivery states:

| State | Meaning |
|---|---|
| Queued | Not yet picked up by the worker — including while the run is sitting at a plan-approval gate. |
| Delivered | The worker has fetched it for its next turn. |
| Delivered — applies after approval | Fetched while the run was sitting at a plan-approval gate; it's buffered and takes effect once you approve. |
| Not delivered — run finished | The run went terminal before the worker ever fetched it. |

**"Delivered" means handed to the worker, not necessarily acted on.**
Whether it actually changed what the agent did next is visible in its
following messages, not in the chip. Two cases show Delivered with nothing
happening: the worker crashes right after fetching it, or a follow-up
buffered at a plan gate is never applied because you **reject** the plan
instead of approving it. Neither is silent for long — a stalled agent trips
the [`stalled` health flag](./run-health.md), and the fix in both cases is
the same: send it again.

The queue stays visible, read-only, after the run finishes — so a
"Not delivered — run finished" input doesn't just vanish.

## From the CLI

`uzi run inputs <run-id>` shows the same queue from the terminal — see
[the CLI docs](./cli.md#commands).
