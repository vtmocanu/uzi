---
title: Seeding a plan
order: 106
audience: user
---

# Seeding a plan

If you've already planned the work yourself — in Claude Code, against your own
clone, watching the plan take shape — `uzi run create --plan-file` skips the
part where uzi plans it again. Hand over a written plan and the worker goes
straight to implementing it: no planning turn, no approval gate. The human
checkpoint moves from the plan to the merge request.

This is what people mean by **seed it to uzi**: author the plan locally and run
`uzi run create --plan-file <path>`, which bypasses uzi's own planning turn *and*
the approval gate. ("Ship it to uzi" and "send it to uzi" name the broader
hands-off flow that drives an issue all the way to a merged MR; seeding is one
mode of it.)

A run started with no `--plan-file` is completely unaffected: same planning
turn, same gate, same everything.

## Before you write: the plan must stand alone

A seeded run starts **cold**. It has no chat session, no memory of the
conversation you had while writing the plan — `plan_md` is its only
instruction. Anything phrased as "as we discussed" or "the file we looked
at" means nothing to it.

Write the plan as if handing it to someone who has read only that file:
name the files to touch, the change to make in each, and how to tell it's
done. A plan that made sense in the conversation that produced it is not the
same thing as a plan that makes sense on its own.

## 1. Write the plan

Any plain-text plan works — there's no required schema. Save it to a file,
or keep it on stdin for the next step.

## 2. Note your base commit (optional)

If you planned against a specific commit and want to know if `main` moved
before the worker gets there, note that commit's SHA. `--planned-commit`
records it; the worker compares it to the clone's own base once it checks
out and warns in the run feed on a mismatch. Add `--require-base` to make a
mismatch fail the run instead of warning — useful when the plan describes
exact line ranges that a moved base would invalidate.

`--planned-commit` must be a hex commit SHA, 7-64 characters — a shorter or
non-hex value is a usage error (exit 2). The safe answer is always the full
SHA of the commit you planned against: `--planned-commit $(git rev-parse
HEAD)` in your own clone.

## 3. Pick the roster (optional)

By default a seeded run uses the same roster any other run would: the
repo's own agents if the clone ships a [`.claude/agents/`
directory](./repo-agents.md), otherwise your own [agent
templates](./agent-templates.md). Pass `--agent-source own|repo` to pick
explicitly, and `--exclude-agents a,b` to drop specific ones from that
source.

Unlike the plan-approval gate, none of this is checked against the clone's
actual roster at create time — the clone doesn't exist yet, so there's
nothing to check against. Confirm the roster from the filesystem before
naming it (`ls .claude/agents/` in your own clone for `--agent-source
repo`), not from memory: `--agent-source repo` against a clone that turns
out to have no `.claude/agents/` **falls back to your own agent templates**
and records the fallback in the run feed, rather than running with zero
subagents. An `--exclude-agents` name that doesn't match anything in the
chosen source is still silently a no-op rather than an error.

## 4. Create the run

```sh
uzi run create --repo <repo-id> --issue <issue-iid> \
  --plan-file plan.md --agent-source repo \
  --planned-commit $(git rev-parse HEAD)
```

Pass `-` instead of a file path to read the plan from stdin. An empty plan,
or one over the 256 KiB cap, is rejected at create time (exit 1) rather than
stored. A roster or base-commit flag without `--plan-file` is a usage error
(exit 2) — see [Agents: `--json` and exit codes](./cli.md).

The worker then clones, checks out, and implements the plan directly. Watch
it the same way as any other run — `uzi run logs --follow` or `uzi tui
<run-id>` — the run view's usual plan panel just doesn't apply, since there
was never a gate to show it at.

## Budget: a seeded run gets the default, not a scaled one

A seeded run never reaches the plan gate, so it freezes no milestones — and the
milestone-scaled budget only exists on the gated path, where the milestone count
drives it. With no milestones its budget columns stay empty and it runs on the
**global default**: `RUN_MAX_ITERATIONS` iterations and `RUN_TIMEOUT` wall-clock
(out of the box 5 iterations / 2h; both are configurable server settings, not
constants).

So for a large, multi-component change, pick deliberately:

- **Split it into per-component seeded runs**, each small enough to finish inside
  the default budget; or
- **Use the gated `uzi run create`** (no `--plan-file`) so the lead proposes
  milestones and the budget scales to them — at the cost of one approval.

## No PRD file needed

An issue with no `prds/*.md` file works fine, since the plan you're
supplying is what a PRD file would have provided anyway. It needs only the
`uzi` label — the single run-eligibility gate — nothing else.

Adding the `uzi` label to a fresh issue doesn't take effect immediately:
uzi checks against its own cached copy of the issue's labels, which only
catches up once the poller syncs. Going through **Promote** instead writes
the label to the forge first and updates the cache in the same request, so
`uzi run create --plan-file` against a freshly-promoted issue works right
away — label it and immediately create a run, and you'll get a "does not
carry the uzi label" error until one of those two things happens.

Related: [uzi CLI](./cli.md#commands) · [Repo agents](./repo-agents.md) ·
[Admin settings](./admin-settings.md#run-eligibility) · [Run activity pane](./run-activity.md)
