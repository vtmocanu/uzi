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
  "incidental finding" card with **File issue**, **Mark done**, and
  **Dismiss ▾**. It's non-blocking by design — a different accent from the
  amber gate cards (plan approval, a clarifying question) that actually park
  the run.
- **A Slack DM** if you've linked your account (see [Slack](./slack.md)) —
  even if you weren't watching. A run that flags several findings sends
  **one** DM, best-effort, not one ping per finding.

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

### Filing several findings as one issue

When several findings share a root cause, tick their rows and click **File as
one issue** in the selection bar. It is enabled for 2 to 50 selected open
findings that have evidence and belong to the same repo. uzi opens the
editable issue draft with the repo fixed, listing every selected finding's
location and title; change the title, description, or labels as you would for a
single finding, then file. Once the filing settles, every selected finding shows as **Filed**
against the same issue, and any warning uzi returns is shown with the result.

Filing is all or nothing: uzi claims every selected finding before it talks to
the forge, so a finding that is already filed, dismissed, or mid-filing blocks
the whole group instead of leaving it half filed. The issue carries a hidden
marker that lets uzi recognize it later.

If uzi cannot confirm whether the forge created the issue, the page shows the
operation id and tells you to check the forge. uzi's repo sync settles the group
by itself once it finds the marked issue. If you find no issue on the forge
after the operation's deadline, release the findings with
[`uzi findings release`](./cli.md#incidental-findings-uzi-findings) and file
again. Release is a CLI action; until it succeeds the findings stay held and
show `pending group <operation-id>` in `uzi findings list --bucket all`.
While an operation is pending, the server logs a warning, `finding group
reconciliation pending`, with the pending count and the age of the oldest one.

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

## Marking a finding done yourself

Findings gets the same **Mark done** a person can already put on a judge
recommendation. Click **Mark done** from an open row (To triage), from a
filed row, or from a dismissed row — a done from Dismissed replaces the
dismissal. The chip reads **"✓ Done"** for a done you set yourself, distinct
from **"Done via #N"** below. On the Findings page, the web doesn't offer a
second Mark done once a row is already done, and it doesn't offer the button
on a mid-filing row either. The run-stream finding card is looser, because it
only knows its own local state: it keeps offering Mark done while it still
shows an open row, so clicking it on a coordinate that's already done
elsewhere just succeeds, and clicking it on a coordinate mid-filing gets a
conflict, shown as the card's own "already filed or resolved" note. From the
CLI, `uzi findings resolve` comes back as a conflict (exit 5) on a
mid-filing coordinate the same way.

**Undo** clears a human done and exposes whatever is underneath: **Filed**
if the finding has a filed issue, otherwise **To triage**. On the Findings
page it's in the **toast** that follows a Mark done, single or bulk; on the
run page it's in place of the card's actions — Mark done unmounts and Undo
appears on the card, after its Mark done. Once the toast is gone, `uzi
findings undo <disposition-id>` is the way back. It does not bring back a
previous dismissal; a dismissal that a done replaced is gone for good.

## Closing a filed issue marks it done

When the issue you filed from a finding is **closed** on the forge, uzi moves
that coordinate to **Done** by itself, labelled **"Done via #N"** so it reads
differently from a finding you never filed at all. It fires **once**, on the
close, and never overwrites you — a coordinate you already dismissed keeps
your verdict — and reopening the *filed issue* on the forge does not undo it.

If you'd already marked that same coordinate done yourself, the close is
recorded without changing your verdict: it still reads "✓ Done" for the
human done, not "Done via #N", and if you later Undo back to Filed, that
same close won't re-mark it done behind you — that holds even if the issue
is later reopened and closed again, since this edge fires once per filed
issue. What matters is when uzi *observes* the close, not just when it
happens on the forge: a close it first observes after your Undo marks it
Done via #N, same as any other filed finding — including one that closed
while you had it done and you Undid before uzi's next poll caught up.

If the same bug **reappears** in a later run — a materially different report
at that same spot — the coordinate goes back to **To triage**, even one
already marked Done, so a fix that didn't actually stick gets your attention
again.

There's no forge call of its own here and no token spent: it rides the normal
issue poll, reading the cache that same tick just refreshed. The repo has to
still be **enabled** in uzi for that poll to run at all — a disabled repo's
closes are never seen.

Closing a **group** issue on the forge marks every finding filed into it Done
via #N, the same close behaviour as above. **Undo** is still per finding: undoing
one does not touch the others.

## Untrusted text

A finding's title, description, and location are written by the agent, from
whatever it was reading when it noticed the bug — treat them as data, not as
something to trust. uzi renders them as inert text everywhere they show up
(the stream card, the backlog, the Slack DM), and the issue it files runs
each field through the same sanitizers uzi's other forge writes use before
anything reaches the forge.

## From the terminal

Everything here is also available from the [uzi CLI](./cli.md#incidental-findings-uzi-findings):

```sh
uzi findings list                                       # what still needs triage
uzi findings file <finding-id>                           # file it
uzi findings file <finding-id> <finding-id> ...          # file several as one issue
uzi findings release <operation-id> --confirm-no-issue   # free a group filing that never confirmed
uzi findings dismiss <finding-id> --reason wont-do       # or not-an-issue
uzi findings resolve <finding-id>                        # mark it done yourself
uzi findings undo <disposition-id>                       # undo a done or a dismissal
uzi findings stats                                       # your triage totals, across all repos
```

`--bucket done` lists the coordinates a done settled, yours and the
issue-close sync's alike. `resolve` takes the `finding-id`, same as `file`
and `dismiss`; `undo` takes the coordinate's `disposition_id` instead, not
the `finding_id` the human `list` view prints — read it off `--json`.

Give `file` several ids of one repo to file one issue for all of them (see
[Filing several findings as one issue](#filing-several-findings-as-one-issue)).
Older evidence ids resolve to their finding and duplicates count once, up to 50
distinct findings. `--json` returns the issue and the linked disposition ids.

## Good to know

- **Which runs can report a finding.** Any autonomous run — issue, CI-fix,
  scheduled prompt, or self-improvement — can flag one; chat can't, since chat
  already has its own user-directed way to draft an issue.
- **A run can't flood the backlog.** A single run stops flagging new findings
  after 10, so a noisy run can't drown out the backlog — it keeps working on
  its actual task either way.
- **Nothing is spent filing, dismissing, or marking done.** All three are
  plain writes against your own connection; no Anthropic token is involved.
