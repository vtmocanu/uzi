---
title: Judge menu
order: 101
audience: user
---

# Judge menu

**Factory → Judge** is where you work the judge's backlog. The
[run judge](./judge.md) produces recommendations one run at a time; this page
is the cross-run view of all of them, and the place to actually clear them.

The nav item carries a badge: how many recommendations still need triage,
across every run you own.

## 1. One row per idea, not per run

The same recommendation recurs. "Install `rg` on the worker" can come back in
twenty runs, and triaging it twenty times is twenty decisions about one idea.

So the worklist **dedups by (category, target)**: one row per idea, however
many runs raised it, tagged **seen in N runs**. Recurrence is the ranking
signal — rows are ordered by how many runs raised them, then by how many are
still open. That is deliberately *not* newest-first: a thing twelve runs
complained about matters more than the one that finished last.

Expand a row to see each occurrence — the run, its verdict, and that
occurrence's own triage state.

## 2. Triage a whole group at once

**Mark done** or **Dismiss ▾** (Won't do / Not an issue) on a row applies to
**every open occurrence across every run**, in one call. Tick several rows and
the selection bar does the same across all of them.

Three things worth knowing, because each looks like a bug and is not:

- **The count you get back is coordinates, not recommendations.** One review
  can legitimately carry the same `(category, target)` twice, and both share a
  single triage state — so dismissing a group of 5 can correctly report 4.
- **Only *open* occurrences are touched.** An occurrence you already dismissed,
  marked done, or filed an issue for keeps the state you gave it. Group actions
  clear the backlog; they do not overwrite your decisions.
- **After the action, the row stays.** It re-renders at its new state rather
  than vanishing, so you can see what you just did — and **Undo** in the toast
  reverts exactly the occurrences that action settled.

Filing an issue is still **per recommendation**, from the occurrence expander,
using the same draft-and-review flow as
[the run page](./judge.md#filing-an-issue-from-a-recommendation). There is no
"file the whole group as one issue" — that needs a repo pick and a human draft
per item, and it is not built.

## 3. The tabs, and the one number

**To triage / Filed / Done / Dismissed / All.** A recommendation is in exactly
one, ranked highest-wins: Dismissed > Done > Filed > To triage.

The **To triage** tab, the **Judge** nav badge and the judge notification in
your inbox are the same number, from the same server-side count — not three
tallies of what happens to be on screen. **seen in N runs** is a row's
recurrence, never a competing total.

**If you see a "backlog was truncated" banner, read it as *unknown*, not
*empty*.** The cap applies before rows are grouped, so a group can be missing
entirely, and a group that *is* shown can have understated counts. `?run=` and
the label filter (below) are the two filters applied before the cap; the
bucket tabs filter what survived it.

## 4. Where a review shows up now

- **On the runs list**, each judged run carries `⚖ issues · 2` — the verdict
  first, and the still-to-triage count only when there is one. A run **nobody
  has judged** carries no badge at all: "never judged" and "judged and fine"
  are different claims, and a neutral pill would assert the second.
- **In your inbox**, "Run review ready" now opens **Judge**, anchored to that
  run (`/judge?run=…`), instead of the run page. Notifications of every other
  kind still open their run. The Slack DM's link moved the same way; its
  cadence did not — still one DM per review, no digest.
- **A run of consecutive judge pings collapses** into one "N reviews ready"
  header you can expand. The rows underneath are unchanged — same ids, same
  read state, same **Mark read**.

An anchored link (`/judge?run=…`) opens on the **All** tab, not To triage. That
is on purpose: the recommendation the notification is about may already have
been settled through another run, and landing on an empty To-triage tab would
read as "nothing here". An unanchored **Judge** still opens on To triage.

## 5. Closing a filed issue marks it done

When an issue you filed from a recommendation is **closed** on the forge, uzi
moves that recommendation to **Done** by itself, and labels it so — "done via
#IID", visibly distinct from one you marked by hand.

It fires **once**, on the close, and never overwrites you: a recommendation you
had already dismissed keeps your verdict, and if you **Undo** the automatic
done it stays undone rather than being re-applied on the next poll. Reopening
the issue does not reopen the recommendation.

Two preconditions, both easy to trip:

- **The repo must still be enabled in uzi.** The sync rides the normal issue
  poll, so a disabled repo is not polled and the close is never seen.
- **The issue must still carry the PRD label.** Strip it and the close goes
  unobserved: uzi syncs a label-less issue only while it is **open**, so once
  it closes it is neither re-read nor kept, and the cached row still says open
  until the next reconcile drops it. Filed issues get the label automatically,
  so this only bites if someone removes it.

There is no forge call of its own here and no token spent — it reads the cache
the normal poll already refreshed.

## 6. Filter by label

Above the bucket tabs, a row of chips — one per recommendation label (Enable
a tool or skill, Install a worker tool, Adjust an agent template, Improve an
agent, Add a missing agent, Improve uzi) — narrows the worklist to whichever
you tick. It's the same six labels every group already shows on its badge,
now usable as a filter.

Tick more than one and you see groups in **any** of the selected labels, not
all of them — a recommendation carries exactly one label, so "match all" would
always show nothing. **Clear** drops the filter and returns every label.

The filter lives in the URL as `?category=`, so it's shareable and
reproducible from the link alone, and it stacks with everything else: switch
bucket tabs, follow a `?run=` notification link, or do both, and the label
selection stays. Like `?run=`, it's applied **before** the row cap (see
[the tabs, above](#3-the-tabs-and-the-one-number)), so narrowing to one or two
labels makes the truncation banner *less* likely to show, not more.

Each chip also carries a count: the number of groups in that label across
your **whole backlog** — every bucket, every triage state — not a tally of
what's currently on screen. It's its own server aggregate, the same kind of
canonical count [the tabs and the triage tally](#3-the-tabs-and-the-one-number)
use, so it stays correct even when the truncation banner is showing: a chip
can read `6` while only 4 cards render under the cap. Switching bucket tabs
or marking a group done doesn't move it — a group stays a group, so the
count changes only when the backlog itself does — and it's fetched once when
the page loads, not refetched on every toggle. A chip whose whole-backlog
count is 0 stays in the row, just dimmed, rather than disappearing.

## From the terminal

Everything except filing is available from the
[uzi CLI](./cli.md#reviewing-and-triaging-from-the-cli): `uzi review backlog`
is this page, and `uzi review resolve/dismiss --category … --target …` is the
group action.
