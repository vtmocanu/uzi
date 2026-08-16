---
title: Incidental findings
order: 107
audience: user
---

# Incidental findings

While a worker is heads-down on your task, it sometimes notices a bug that's
**outside** that task — a leaked ticker in a sweeper it read on the way past, a
retry that can never succeed. Locally, Claude Code would just ask "want me to
file this?"; a headless uzi run has nobody to ask. Incidental findings are the
headless equivalent: the worker flags the bug and keeps working, and you file
it (or dismiss it) later, on your own schedule. **The worker never writes to
the forge — you gate every filing, exactly like every other forge write in
uzi.**

## Where you'll see it

- **A card in the run's stream**, if you're watching it live: a blue
  "incidental finding" card with **File**, **Edit & file**, and **Dismiss**.
  It's non-blocking by design — a different accent from the amber gate cards
  (plan approval, a clarifying question) that actually park the run.
- **Your inbox**, and a **Slack DM** if you've linked your account (see
  [Slack](./slack.md)) — even if you weren't watching. A run that flags
  several findings sends **one** DM and **one** inbox entry whose count grows,
  not one ping per finding.

Either surface lands you on the same place: the **Findings** backlog.

## The Findings backlog

**Findings**, in the sidebar's **Work** group, carries a badge for how many
findings still need filing. It's a per-repo, deduped list collected across
every run, grouped by repo (a single-repo view drops the redundant repo
header).

Findings dedupe on **where** they are, not **which run** found them: the same
bug seen in five different runs is one row reading **"seen in 5 runs"**, not
five things to triage. Three buckets — **To file**, **Filed**, **Dismissed** —
plus **All**.

## Filing a finding

Click **File** and uzi opens a real issue on **your own forge connection**,
with a marker label your admin controls (`agent-found` by default) plus any
labels you pick. Click **Edit & file** first if you want to change the title,
description, or labels before it's created. Either way, a filed finding links
back to the issue it became.

## Dismissing a finding

Click **Dismiss** and pick a reason — **Won't do** (valid, but not worth
doing) or **Not an issue** (the worker got it wrong). A dismissed finding
**stays dismissed**: if a later run trips over the exact same bug again, it
does **not** re-notify you and does **not** reappear in To file. Only a
**materially different** finding at that same spot re-opens it — so
dismissing something is a real "stop nagging me about this," not a snooze.

An old finding card can lag the backlog (it's a historical record of the
moment it was posted, not a live view) — clicking **File** on a card for a
coordinate someone already filed or dismissed just shows "already filed or
resolved," never an error.

## Untrusted text

A finding's title, description, and location are written by the agent, from
whatever it was reading when it noticed the bug — treat them as data, not as
something to trust. uzi renders them as inert text everywhere they show up
(the stream card, the backlog, notifications), and the issue it files runs
each field through the same sanitizers uzi's other forge writes use before
anything reaches the forge.

## From the terminal

Everything here is also available from the [uzi CLI](./cli.md#incidental-findings-uzi-findings):

```sh
uzi findings list                                       # what still needs filing
uzi findings file <finding-id>                           # file it
uzi findings dismiss <finding-id> --reason wont-do       # or not-an-issue
```

## Good to know

- **Which runs can report a finding.** Any autonomous run — issue, CI-fix,
  scheduled prompt, or self-improvement — can flag one; chat can't, since chat
  already has its own user-directed way to draft an issue.
- **A run can't flood the backlog.** A single run stops flagging new findings
  after 10, so a noisy run can't drown out the backlog — it keeps working on
  its actual task either way.
- **Nothing is spent filing or dismissing.** Both are plain writes against
  your own connection; no Anthropic token is involved.
