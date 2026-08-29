---
title: "Handoff: renting a remote worktree"
order: 108
audience: user
---

# Handoff: renting a remote worktree

`uzi handoff` (alias `uzi task`) is a second, lighter way to get work done in
uzi. Where the normal flow is durable and forge-tracked — an issue, a plan
gate, a merge request someone reviews — a handoff is a **rented remote
worktree**: you push it some work, watch it, pull the result, and throw the
branch away. No issue to file, no PRD to write, no MR to review, nothing to
clean up on the forge afterward (unless you ask for one).

For the full flag reference and exact behavior, see [uzi
CLI](./cli.md#uzi-handoff-ephemeral-branch-scoped-task-runs).

## Two modes, not one flow with an escape hatch

uzi splits into two coherent modes, and handoff is not a shortcut through the
first one — it's a genuinely different shape of run:

| | PRD flow (`uzi run create`) | Handoff flow (`uzi handoff`) |
|---|---|---|
| Trigger | A forge issue, usually PRD-labelled | A local checkout + inline context |
| Gate | Plan approval before implementation | None — the worker starts immediately |
| Deliverable | A merge request you review | Commits on a branch you pull |
| Record | The issue + MR are the durable history | The run transcript in uzi; no forge artifact by default |
| Cleanup | The MR merges or closes | `uzi handoff rm <run-id>` |

Product-grade, reviewable work belongs in the PRD flow. The dev-loop task you
would otherwise hand-orchestrate yourself — "take this, do it, I'll pull the
result" — is what handoff productizes.

## What actually happens

1. You run `uzi handoff -m "<context>"` inside a checkout with an `origin`
   remote. The CLI creates a `task` run and gets back a server-named branch,
   `uzi/task/<run-id>` — the destination is never yours to name, which is
   part of what keeps it safe (see [Lifecycle and guardrails](#lifecycle-and-guardrails)
   below).
2. The CLI pushes your local HEAD to that branch, with **your own** git
   credentials — the same push you'd type by hand.
3. The CLI dispatches the run. Only now can a worker claim it.
4. The worker clones `uzi/task/<run-id>`, works your inline context, commits,
   and pushes its commits back to the same branch — no forge issue, no merge
   request, unless you passed `--mr`.
5. You `git fetch origin uzi/task/<run-id> && git switch uzi/task/<run-id>`
   to pick up the result. Send more context with the existing `uzi run
   follow-up <run-id>`; watch with `uzi run get`/`uzi run logs --follow` or
   [`uzi tui`](./cli.md#watching-runs-live-uzi-tui) — a handoff is a run like
   any other on every one of those surfaces.

Two writers touch the branch, but never at the same time: your push seeds
it once, before the worker starts, and after that the worker is the sole
writer. If you push more local commits to a *live* task branch mid-run,
that push is rejected non-fast-forward rather than clobbering the worker's
history — a mid-run user push is out of scope for v1. Use `uzi run
follow-up` to steer a running task instead of pushing over it.

## The review / then-fix loop

A handoff can end with a second opinion instead of ending with your pull:

- **`--review`** runs a fresh diff-review once the task completes — clones
  `uzi/task/<run-id>`, diffs it against its base, and produces structured
  findings (file, symbol, line, severity, summary, rationale). Fetch them
  with `uzi handoff review <run-id>` (`--json` for the machine-readable
  form). The findings are never committed to the branch — they're something
  you read, not a diff the worker writes onto your work.
- **`--then-fix`** (which turns `--review` on for you) chains a follow-on fix
  run once the review lands: it consumes the findings and pushes fixes for
  them to the same `uzi/task/<run-id>` branch, so the whole loop — task,
  review, fix — runs without a manual step in between.

## Interactive mode

A one-shot task is done when the agent calls `signal_done`: it finalizes
(pushes the branch, opens an MR if you asked) and goes terminal. Pass
`--interactive` and it doesn't finalize on a clean `signal_done` — it
**parks** instead, so you can keep iterating with the *same* agent session
instead of paying for a fresh task's cold-start context every time.

```sh
uzi handoff -m "add input validation to the signup form" --interactive
```

- **Park, not finish.** On `signal_done`, an interactive task
  checkpoint-pushes `uzi/task/<run-id>` and enters a new non-terminal status,
  `awaiting_followup`, holding its SDK session, clone and branch alive rather
  than tearing them down.
- **`uzi run follow-up <run-id>` wakes it.** The *same* agent session resumes
  with full context (a session-id resume, not a fresh session with the
  history replayed), takes turns against your follow-up until the next
  `signal_done`, checkpoint-pushes, and parks again — repeat as many times as
  you like.
- **`uzi run stop <run-id>` winds it down on purpose.** Unlike `uzi run
  cancel`, which aborts mid-turn, `stop` finishes the current turn and
  finalizes gracefully — push, open the MR iff `--mr` was set — then lands
  `completed` with a distinct stop disposition. See [uzi
  CLI](./cli.md) for its exit codes.
- **An idle timeout backstops a forgotten park.** If nobody sends a
  follow-up or a stop, the worker's own clock (`WORKER_TASK_IDLE_TIMEOUT`,
  default 30 minutes) finalizes the task the same way `run stop` does — push,
  then `completed` — so a parked task never pins a worker slot indefinitely.
  Work is checkpoint-pushed at *every* park, not only at wind-down, so a
  worker dying mid-session never strands unpushed commits.
- **A plain (non-interactive) handoff gets its own wall-clock budget.** It runs
  under a dedicated default of 4h (`HANDOFF_RUN_TIMEOUT`), separate from the
  global `RUN_TIMEOUT`, so a longer handoff does not require moving every run's
  cap.
- **`--review`/`--mr` compose at wind-down, not at every park.** The
  diff-review fires once — when the run finally reports `completed`, whether
  via `run stop` or the idle timeout — never on an intermediate park.
- **`--interactive --then-fix` is rejected.** `--then-fix` auto-terminates a
  task into a chained review-and-fix run, which conflicts with keeping the
  task alive to iterate; the CLI rejects the combination as a usage error
  (exit 2) rather than guessing which behavior you meant.

Interactive mode is a `uzi handoff` opt-in only — a task run created any
other way (there is no `uzi run create --interactive`) is always one-shot.

## Lifecycle and guardrails

- **The branch is always `uzi/task/<run-id>`, server-named.** You never
  choose the destination in v1. That's what makes it safe by construction: a
  server-minted name in the `uzi/task/*` namespace can never collide with a
  protected or default branch, so there's no way to accidentally hand off
  onto `main`.
- **The worker's push back is non-forced**, the same property that makes a
  stray mid-run user push fail closed instead of silently rewriting history
  (above).
- **`uzi handoff rm <run-id>`** deletes the remote `uzi/task/<run-id>` branch
  with your own git credentials, and it only ever deletes inside the
  `uzi/task/*` namespace — it refuses anything else, including a run that
  isn't a `task` run at all.
- **`--mr` exempts a branch from `rm`.** If the worker opened a merge
  request for the branch, that MR needs its source branch to stay alive, so
  `rm` refuses it — delete it via the merge request instead. This is also
  the escalation path: a throwaway task that turns out to be keeper work
  gets an MR without redoing anything, just by having asked for `--mr` up
  front (or by opening one yourself from the branch you pulled).
- **There's no server-side auto-prune yet.** A finished no-MR task's branch
  sits on the forge until you delete it with `rm` — that's the v1 cleanup
  story, not an automatic sweep. Run `rm` once you've pulled what you need.
- **A raw handoff has no forge record.** With no issue and no MR, there's
  nothing durable on the forge naming what happened — the run transcript and
  your inline context are still persisted in uzi (`uzi run get`/`uzi run
  logs`), but there's no issue thread or MR description to point someone
  else at later. That's the accepted tradeoff of the ephemeral mode: pass
  `--mr` when you want a durable artifact instead.

## See also

- [uzi CLI](./cli.md#uzi-handoff-ephemeral-branch-scoped-task-runs) — the
  full flag reference.
- [Run activity pane](./run-activity.md) — the same transcript/steering
  surfaces a handoff run uses.
- [`uzi tui`](./cli.md#watching-runs-live-uzi-tui) — watch a handoff live
  alongside every other run.
