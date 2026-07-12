---
title: Run judge
order: 100
audience: user
---

# Run judge

With the run judge on, every one of your **finished** runs gets a
retrospective: an LLM reads the run's trace (agents, tools, plan, review
cycles, delivery) and produces a verdict plus structured recommendations —
never code changes, only advice. It runs on **your own Anthropic token**, so
it's opt-in and off by default; your instance admin also has to enable it
globally first.

## 1. Enable it

Your admin enables the feature globally under **Admin → Instance settings →
Run judge**. Once that's on, open **Settings → Run judge** and check
**Judge my finished runs**. Admins can also force-enable or force-disable the
judge for any individual user from **Admin → Users**.

## What you get

When a judged run finishes (completed or failed — a cancelled run is never
judged), a review lands in three places:

- **The run page**: a verdict chip (Ideal / OK / Issues found) plus a list of
  recommendations, each with a category, a target (the tool/agent/repo it's
  about), a rationale, and a confidence level.
- **Your [inbox](#the-inbox)**: a "Run review ready" notification.
- **Slack** (if you've linked your account): the same summary as a DM.

Recommendations use a fixed taxonomy: enable an existing tool or skill,
install a missing worker tool, adjust an agent template or prompt, improve an
existing agent (including a repo agent living in git), propose a missing
agent for the repo, or improve uzi itself. That last category feeds the
[self-improvement job](./self-improvement.md), if your admin has it enabled.

## The deterministic fallback

Separately from the LLM call, uzi scans the run's own tool output for
`command not found` / missing-executable errors and always turns any hit into
an "install a worker tool" recommendation naming the tool — even if the judge
model call itself fails. When that happens the run page shows a "judge
incomplete" badge next to the verdict, but the deterministic findings still
land.

## Re-running the judge

On any judged run's page, click **Run judge** (or **Re-run judge**, once a
review already exists) to get a fresh retrospective. This is owner-only —
clicking it is your consent to spend your own token, no separate opt-in
required — and rate-limited per user so it can't be hammered. A re-run
replaces the previous verdict rather than appending a second one.

## Which runs are judged

Only finished **issue** and **CI-fix** runs are eligible. Chat runs, judge
runs, and self-improvement runs are never judged — there's no recursive
judging, and no self-feeding loop.

## The inbox

The bell icon in the sidebar (**Notifications**) opens your inbox: every
review, and any other notification uzi produces, in one place. You see your
own; an admin can switch to **All users** to see everyone's (each row shows
its owner). Unread rows are highlighted; **Mark read** clears them from your
unread count. Marking read is scoped to your own rows even in the admin
all-view.

## Good to know

- **Cost**: a judge run is one model round-trip over a compacted version of
  the run's trace, billed to the run owner exactly like any other run on
  their token. With the feature off (globally, or for you), nothing fires and
  nothing is spent.
- **What's shared**: the inbox row and the Slack DM carry the verdict, a
  short (secret-scrubbed) summary, and the recommendation count and
  categories. The full recommendation detail (target, rationale) stays on the
  run page.
- **Untrusted text, rendered safely**: every free-text field the judge
  produces is validated, length-capped, and secret-scrubbed before it's
  stored, and the run page renders it as plain escaped text, never as
  markdown or HTML.
