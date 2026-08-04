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

## 3. Pick the roster (optional)

By default a seeded run uses the same roster any other run would: the
repo's own agents if the clone ships a [`.claude/agents/`
directory](./repo-agents.md), otherwise your own [agent
templates](./agent-templates.md). Pass `--agent-source own|repo` to pick
explicitly, and `--exclude-agents a,b` to drop specific ones from that
source. If you're naming agents, read them out of the clone's
`.claude/agents/` directory rather than guessing — an excluded or misspelled
name that doesn't exist in the chosen source is rejected.

## 4. Create the run

```sh
uzi run create --repo <repo-id> --issue <issue-iid> \
  --plan-file plan.md --agent-source repo
```

Pass `-` instead of a file path to read the plan from stdin. An empty plan,
or one over the 256 KiB cap, is rejected at create time (exit 1) rather than
stored. A roster or base-commit flag without `--plan-file` is a usage error
(exit 2) — see [Agents: `--json` and exit codes](./cli.md#agents-json-and-exit-codes).

The worker then clones, checks out, and implements the plan directly. Watch
it the same way as any other run — `uzi run logs --follow` or `uzi tui
<run-id>` — the run view's usual plan panel just doesn't apply, since there
was never a gate to show it at.

## No PRD file? PRDLESS composes with this

An issue with no `prds/*.md` file still works, since the plan you're
supplying is what a PRD file would have provided anyway. It needs **both**
the `PRD` label and the [`PRDLESS` label](./prdless.md) — PRDLESS is the
escape hatch for a PRD issue with no file yet, not a way to opt out of the
PRD label entirely.

Adding the `PRD` label to a fresh issue doesn't take effect immediately:
uzi checks against its own cached copy of the issue's labels, which only
catches up once the poller syncs. Going through **Promote** instead writes
the label to the forge first and updates the cache in the same request, so
`uzi run create --plan-file` against a freshly-promoted issue works right
away — label it and immediately create a run, and you'll get a "not a PRD
issue" error until one of those two things happens.

Related: [uzi CLI](./cli.md#commands) · [Repo agents](./repo-agents.md) ·
[PRDLESS label](./prdless.md) · [Run activity pane](./run-activity.md)
