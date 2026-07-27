---
title: Board
order: 30
audience: user
---

# Board

Each enabled repo gets a board in the sidebar: a kanban view of its GitLab
issues, kept in sync with the forge in both directions. Cards also carry
their latest agent run, so the board doubles as a run tracker: it moves
issues automatically as a run progresses and refreshes itself, without a
manual reload.

## Columns

- **Backlog** (implicit): issues carrying none of the configured column
  labels. There is no `Backlog` label behind it, so nothing is written to
  GitLab when a card sits here; GitLab's own board lists these issues in its
  built-in **Open** list instead, and the two names cannot be reconciled
  (GitLab's built-in list can't be renamed).
- One column per configured label, seeded on first open in this order:
  `Planned`, `In Progress`, `Human Review`, `Later`. Reconfigure any time
  from the board's column settings.
- **Closed** (implicit): the issue's GitLab state, not a label; cards here
  aren't draggable.

A fresh board therefore reads **Backlog · Planned · In Progress · Human
Review · Later · Closed**, left to right in the order work moves: nobody has
decided yet, someone picked it, an agent has it, its merge request is waiting
on you, or it was looked at and deliberately deferred.

`In Progress` and `Human Review` are also the two columns the run automation
below moves cards through; `Planned`/`Later` are plain backlog buckets it
never touches except to restore a card to one.

**Existing boards keep the columns they already have.** A board seeded before
`Planned` existed still shows `Upcoming`, in its old position: uzi will not
rename a real GitLab label out from under you, and there's no undo for that
from inside uzi. To adopt the new name by hand, see
[Configuration](./configuration.md).

**Hide empty columns.** The board toolbar has a **Hide empty** tick box: turn
it on and any column with no cards drops out, with a **`N hidden`** count next
to the box. The choice is remembered per board. Hiding is recomputed on every
poll, so a column reappears on its own the moment a card lands in it (a run
auto-move, a change made in GitLab); while you drag a card, the hidden lanes
reappear dimmed so you can still drop into them.

## More room for the board

The board uses the full width of the window. To reclaim the sidebar's 240px,
collapse it to an icon rail with the toggle at the sidebar's bottom edge; every
destination stays one click away, and it stays collapsed (on this browser)
until you expand it again.

## Move a card

Drag a card to another column. uzi writes the label change to GitLab first
(`add`/`remove` on that issue) and only updates the board once the forge
confirms it: a failed write snaps the card back rather than showing a move
that didn't really happen. The same change is visible in GitLab's own issue
and board views.

![Dragging a card between board columns, relabeling the underlying GitLab issue](img/board-move-card.png)

## Automatic moves

Starting a run moves its issue for you: **Start run** puts the card in **In
Progress**; when the run completes, it moves to **Human Review** (with or
without a merge request); a failed or cancelled run moves it back to
wherever it started (Backlog,
Planned, Later) rather than a hardcoded column, so a backlog placement is
never lost. A manual drag always wins — move a card by hand after a run has
started and automation leaves it alone from then on. Moves are best-effort
against the forge: one that fails (e.g. GitLab briefly unreachable) is
retried in the background for up to 30 minutes, without blocking the run.

Closing a completed run's merge request without merging it is treated as
"rework needed": on the next poll its card moves from **Human Review** back
to **In Progress**. Reopening that MR moves the card back to **Human
Review**. Merging is untouched — the MR's `Closes #N` link closes the
issue, and the regular issue sync moves the card to Closed. A manual drag
always wins here too. A card stuck in Human Review from before this
behavior existed (an MR close uzi never observed) needs one manual drag to
unstick; automation keeps it in sync from then on.

## Run badges

Each card shows its latest run at a glance: **queued**/**claimed** while
waiting for a worker, **running Nm** (pulsing, worker name below), amber
**awaiting approval** (the loudest treatment — see Attention strip), rose
**failed** with the reason on hover, and neutral **stopped** for a
deliberate cancel or plan rejection — even one carrying your own free-text
reason — never styled as a failure. The stopped/failed call is made
server-side, not guessed from the reason text, so there's no wording that
can slip a deliberate stop into the rose "failed" treatment.

A completed run with a merge request becomes an **`!N`** chip linking to
it, colored to match the MR's own state: an open MR renders exactly as
before, a merged one gets an ok-toned **`!N merged`**, and one closed
unmerged gets a muted, struck-through **`!N closed`**. Without an MR a
completed run is still a plain **completed** badge (never invisible). More
than one run on an issue adds a small **×N** count next to the badge — full
history is one click away (see Opening an issue).

The merged/closed chip is a best-effort hint, not a live poll, and its
freshness guarantee applies only to a card's **latest** run — that one
chip is always accurate as of the board's last sync. Older runs, wherever
you see their chips (an issue's run history, the run view), are frozen at
whatever MR state uzi last observed for that specific run: if a later
rework run supersedes it, its chip stops updating and can go stale — hover
any chip for "as of last sync". A stale **closed** is the case to watch for
(that MR may have since been reopened); a stale **merged** is rare in
practice, since merging an MR closes its issue and the card leaves the
board before the state would ever go stale.

## Card labels

Each card also shows its other GitLab labels as small chips, so `bug` or
`security` is visible without opening the issue. Workflow markers are left
off: the `PRD` label, the `PRDLESS` escape hatch and the `autopilot` label
(whatever your admin has configured those to be), plus the card's own column
label, since a card sitting in `Planned` doesn't need a `Planned` chip too.
A card carrying many labels shows the first few and a **`+N`** count with the
rest on hover, so one heavily-labelled issue can't stretch its column.

## Attention strip

When any of your runs on this board is **awaiting approval**, or the
[run-health](./run-health.md) detector has flagged one as looking stuck, a
banner appears above the columns naming both counts ("1 run needs approval ·
2 runs look stuck") and linking straight to them — a human is the likely
blocker, so this is designed to be impossible to miss.

## Opening an issue

Click a card's title to open it in-app: full description, its column and
labels, and its complete run history (start time, duration, worker, MR
link). **Start run** is available there too. GitLab stays one click away via
the small icon next to the card's issue number.

## Staying in sync

Moves and edits made in GitLab appear in uzi within one poll interval;
de-labeling, closing, or deleting an issue appears within one (less
frequent) reconcile pass. The board also polls on its own (about every 10
seconds while the tab is visible), so run-driven moves and badges show up
without a manual **Refresh** — a brief toast ("#42 → Human Review")
announces each one. **Refresh** still triggers an immediate full sync if you
don't want to wait.

## Why only some issues show up

The board lists only issues carrying the **`PRD`** label (uzi works PRDs,
not arbitrary tickets). Each card also needs its issue description to link a
`prds/*.md` file; a card missing that link shows a warning badge and is
excluded from agent pickup until the link is added. A card carrying more
than one column label (edited outside uzi) shows a conflict badge and
displays in its highest-positioned column until the next move normalizes it.

A run that finishes a PRD is asked to move the file to `prds/done/` in its own
merge request (see [Agent skills](./skills.md)). Once that merge request
merges, uzi rewrites that PRD's link in the issue description so it still
resolves. It matches on the moved file's name, so a link to a *different* PRD
in the same description is left alone.
