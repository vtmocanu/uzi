---
title: Run activity pane
order: 37
audience: user
---

# Run activity pane

The run view's activity pane shows your crew at work: a roster strip for
"who's alive," collapsed-by-default logs so a live run doesn't drag your
scroll position around, and a steer queue that tells you whether a
follow-up you sent actually reached the worker.

## Crew roster

One chip per agent, with a state dot:

| State | Meaning |
|---|---|
| working (pulsing) | The active speaker, and the run is healthy — see [Run health](./run-health.md). Stays `working` through a long tool call (a build, a test suite); it does not time out on its own. |
| stalled (amber) | The active speaker, but the run's health has flagged it (`stalled`, `slow`, or `looping`) — a looping agent never reads as healthy green. |
| waiting | Either every chip, while the run is blocked on a plan approval or has no worker claimed yet; or a non-active agent that spoke recently. |
| idle | A non-active agent that hasn't spoken in a while. |
| done | Every chip, once the run has finished (completed, failed, or cancelled). |
| *(empty state)* | No agent has spoken yet — a single muted "waiting for the first agent…" placeholder, not zero chips. |

Click a chip to jump to that agent's log. The `waiting`/`idle` split for a
non-active agent is a recency heuristic (no precise handoff signal exists
yet), so it can lag up to 30 seconds — cosmetic only, it never affects the
`working`/`stalled`/`done` states above.

## Logs: collapsed by default, opt-in Follow

Each agent's log is an accordion, **closed by default**, with a live
one-liner in its header ("running `go test ./...`") that updates in place
and a `+N` pill for messages you haven't seen while collapsed. A finished
run, or a run with only one agent, auto-expands so you're not stuck
clicking through every accordion to read a result; **Expand all** /
**Collapse all** are always one click away.

**Follow live**, off by default, tails only the *expanded* agent's own log
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
