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
findings still need triage. It's a per-repo, deduped list collected across
every run, grouped by repo (a single-repo view drops the redundant repo
header).

Findings dedupe on **where** they are, not **which run** found them: the same
bug seen in five different runs is one row reading **"seen in 5 runs"**, not
five things to triage. Five tabs carry the count — **To triage**, **Filed**,
**Done**, **Dismissed**, **All** — straight from the server, scoped to
whichever repo you have selected, the same tally the nav badge reads (never a
count of what's rendered on screen). Expand a row (the chevron) to see its
**evidence**, a preview of the newest report, and **the runs it was seen in**,
each a link straight to that run.

## Filing a finding

Click **File** and uzi opens a real issue on **your own forge connection**,
with a marker label your admin controls (`agent-found` by default) plus any
labels you pick. Click **Edit & file** first if you want to change the title,
description, or labels before it's created. Either way, a filed finding links
back to the issue it became.

## Dismissing a finding

Click **Dismiss ▾** and pick a reason — **Won't do** (valid, but not worth
doing) or **Not an issue** (the worker got it wrong). Tick several rows first
and the same menu, from the selection bar, dismisses all of them at once with
one shared reason; **Undo** in the toast that follows reverses exactly the
ones that action settled.

A dismissed finding **stays dismissed**: if a later run trips over the exact
same bug again, it does **not** re-notify you and does **not** reappear in To
triage. Only a **materially different** finding at that same spot re-opens
it — so dismissing something is a real "stop nagging me about this," not a
snooze.

An old finding card can lag the backlog (it's a historical record of the
moment it was posted, not a live view) — clicking **File** on a card for a
coordinate someone already filed or dismissed just shows "already filed or
resolved," never an error.

## Closing a filed issue marks it done

When the issue you filed from a finding is **closed** on the forge, uzi moves
that coordinate to **Done** by itself, labelled **"Done via #N"** so it reads
differently from a finding you never filed at all. It fires **once**, on the
close, and never overwrites you — a coordinate you already dismissed keeps
your verdict — and reopening the *filed issue* on the forge does not undo it.

If the same bug **reappears** in a later run — a materially different report
at that same spot — the coordinate goes back to **To triage**, even one
already marked Done, so a fix that didn't actually stick gets your attention
again.

There's no forge call of its own here and no token spent: it rides the normal
issue poll, reading the cache that same tick just refreshed. The repo has to
still be **enabled** in uzi for that poll to run at all — a disabled repo's
closes are never seen.

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
uzi findings list                                       # what still needs triage
uzi findings file <finding-id>                           # file it
uzi findings dismiss <finding-id> --reason wont-do       # or not-an-issue
uzi findings undo <disposition-id>                       # undo a dismissal
uzi findings stats                                       # your triage totals, across all repos
```

`--bucket done` lists the coordinates the issue-close sync settled. `undo`
takes the coordinate's `disposition_id`, not the `finding_id` the human `list`
view prints — read it off `--json`.

## Good to know

- **Which runs can report a finding.** Any autonomous run — issue, CI-fix,
  scheduled prompt, or self-improvement — can flag one; chat can't, since chat
  already has its own user-directed way to draft an issue.
- **A run can't flood the backlog.** A single run stops flagging new findings
  after 10, so a noisy run can't drown out the backlog — it keeps working on
  its actual task either way.
- **Nothing is spent filing or dismissing.** Both are plain writes against
  your own connection; no Anthropic token is involved.
