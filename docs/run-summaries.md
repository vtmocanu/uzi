---
title: Run summaries
order: 39
audience: user
---

# Run summaries

Every run gets two short plain-English summaries, so you can tell what it's
doing without opening the issue or reading the raw plan:

- **Intent summary** — "what this run will implement", compiled from the
  issue title and body (plus a linked PRD, when there is one) shortly after
  the run starts. It shows as a one-line preview on the runs list and as a
  card on the run page.
- **Plan summary + deltas** — "what the proposed plan will do", compiled the
  moment the agent's plan reaches the approval gate. Alongside it, a short
  tagged list of how the plan diverged from the original ask: **added**,
  **changed**, or **dropped**. The card reads **Proposed plan** while the run
  sits at the gate and relabels to **Approved plan** once you approve — the
  same text, not a regenerated one. A revised plan (after **Request
  changes**) regenerates both.

Both summaries are generated on **your own Anthropic token**, on a cheap
model by default, so they cost very little next to the run itself.

## Read the deltas as a heads-up, not a substitute

The deltas are the model's own read of how the plan differs from what was
asked for — a quick way to spot something worth a closer look, not a
guarantee of completeness. The model is summarizing text that came from the
issue, the PRD, and the plan itself, all of which a hostile or careless
issue could shape to hide something (say, a dropped security step). Always
read the actual plan before approving; the deltas are there to draw your eye,
not to replace that read.

## It's advisory — it never blocks your run

Summary generation runs alongside the real work, never in place of it. If it
fails or times out, it's simply skipped: the card falls back to the issue
title and the run proceeds exactly as if summaries didn't exist. A seeded or
pre-approved run (one that skips planning) gets an intent summary only —
there's no plan gate for a plan summary to attach to.

## Collapsing a card

Each card can be collapsed; the choice is remembered **per run, in your
browser**, for up to 7 days. It's expanded by default and isn't synced
across devices.

## Which model it runs on

The instance default is **haiku** — summaries are lightweight and run once
or twice per run, so a fast, cheap model is the right default. From
**Settings → Run defaults → Run summaries**, the **Summary model** picker
lets you override that for your own runs; leave it on **Inherit** to use the
instance default. See [Admin settings](./admin-settings.md#run-summaries) for
the instance-wide setting.

## From the CLI

`uzi run get <id>` prints the same summaries as `INTENT`, `PLAN SUMMARY`, and
one `DELTA` row per change — each only when it exists. See
[the CLI docs](./cli.md#commands).
