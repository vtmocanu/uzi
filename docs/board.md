---
title: Board
order: 30
audience: user
---

# Board

Each enabled repo gets a board in the sidebar: a kanban view of its GitLab
issues, kept in sync with the forge in both directions. A board's cards are
its **membership set**: the repo's `PRD`-labeled issues plus a configurable
set of *extra* labels (`bug` by default) — see
[Which issues show up](#which-issues-show-up). The toolbar's **Issues**
control tunes the extras, per repo, per account. Cards also carry their
latest agent run, so the board doubles as a run tracker: it moves issues
automatically as a run progresses and refreshes itself, without a manual
reload.

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

## Ordering and sorting

The **Sort** control in the board toolbar chooses how cards are ordered inside
every column:

- **Manual** (the default): your own order, the one you set by moving cards
  around. On a board nobody has ever reordered this is exactly issue-number
  order, so the board looks the same as it always did until you decide
  otherwise.
- **Issue number**: plain numeric order. This is the way back once you have
  reordered something and want to ignore your own order for a moment.
- **Recent run activity**: cards whose latest agent run was touched most
  recently come first. Cards that have never run go last.
- **Last updated**: most recently changed on GitLab first.
- **Title**: alphabetical.

The sort choice is remembered per board, on this browser. The order itself is
different: it is stored with your account, so it follows you to any browser or
device you sign in from. It is yours, not the team's; another person looking at
the same GitLab project sees their own board and their own order.

### Two ways to reorder a card

Both do exactly the same thing, so use whichever suits you.

**With the keyboard.** Focus a card, or hover it, and use the small **up** and
**down** buttons next to the issue number. Each press moves the card one place
within its column. The **up** button is disabled on the top card and **down** on
the bottom one; a card that is alone in its column shows no buttons at all,
since there is nowhere for it to go. They are reachable by tabbing, and each one
is announced with the card and the direction ("Move issue #42 down in Planned"),
so this works without a mouse.

**By dragging.** Pick a card up and drop it where you want it. A line appears on
the edge of the card you are hovering to show where it will land. Dropping onto
another column moves it there and changes its label on GitLab, exactly as
before; dropping within a column only changes the order and writes nothing to
GitLab.

### What a reorder records

Moving one card records the order of the **whole board**, exactly as it looks to
you at that moment, and switches the board to **Manual**. That is deliberate:
if only the moved card were recorded, every other card would fall back to
issue-number order the moment the board switched to Manual, and a single drag
would look like it had scrambled everything.

Two consequences worth knowing:

- Reordering one card while sorted by, say, **Last updated** freezes that
  Last-updated arrangement as your manual order. It is the arrangement you were
  looking at, which is usually what you meant, but it does replace whatever
  order you had recorded before.
- Issues that arrive after you last reordered appear at the **bottom** of their
  column rather than jumping to the top, in issue-number order among
  themselves.
- Closed issues drop out of the recorded order the next time you reorder
  anything, so an issue that reopens after that comes back at the bottom of its
  column. Reopened before then, it returns to the place it used to hold.

## Search

The toolbar's search field filters cards across every column at once, by
title, `#iid`, or label — case-insensitive, with the matched text
highlighted. Press **`/`** anywhere on the board to jump into it; **`Esc`**
clears the query.

While a query is active: a column with no matches drops out entirely, a
capped column showing only some of its matches gets an `N/M` count (see
[Per-lane paging](#per-lane-paging) below), and a board-level **`N
results`** line appears above the columns. Clear the query and everything
comes back.

**Search only finds board members.** It filters the same set the board
already renders — the `primary ∪ extras` membership described in
[Which issues show up](#which-issues-show-up) — so an issue that exists on
GitLab but isn't a member of *this* board won't turn up, even searched by
its exact `#iid`. That's deliberate: search narrows what's already on the
board, it doesn't widen membership. Use the **Issues** control to bring an
issue onto the board first if you need it to be findable.

## Per-lane paging

A column with more cards than its cap renders only the cap; the rest sit
behind a **`Show N more · N left`** button at the bottom of the column.
Each click reveals one more page (up to 50) of cards; the column grows to
fit and the whole page scrolls, rather than the column scrolling inside a
fixed box. **Collapse** puts it back to the cap. The cap applies to every column, including **Closed**.
Past a couple of pages of hidden remainder, a nudge suggests searching
instead of paging through everything (the **Show more** button stays, so
you can still page if you prefer).

A card you drag or move (with the keyboard) past the cap always reveals
itself — a card can never disappear behind a cap you just moved it into.

The **Per lane** control in the toolbar sets the cap (5 / 10 / 20, default
**10**). Like **Sort** and **Hide empty**, it's remembered per board, on
this browser, and re-baselines every column when you change it. While a
search is active the cap lifts: a matching column opens at a full page and
pages from there instead of starting at the cap.

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
waiting for a worker, an indigo **planning** badge (pulsing) while the agent
drafts its plan and nothing is committed yet, the cyan **running Nm** badge
(pulsing, worker name below) once the plan is approved and it starts
implementing — both are the run's `running` state under the hood, and
planning is a display-only distinction that tells "still proposing work"
apart from "actively writing code" — amber **awaiting approval** or amber
**needs your answer** (a run parked on a
clarifying question — see [Answering a question](./run-activity.md#answering-a-question);
both get the loudest treatment — see Attention strip), rose
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
(whatever your admin has configured those to be), plus every configured
column label, since a column already has its own lane. A card carrying many
labels shows the first few and a **`+N`** count, with the rest available by
hovering or by tabbing to the count, so one heavily-labelled issue can't
stretch its column.

## Attention strip

When any of your runs on this board is **awaiting approval**, **needs your
answer**, or the [run-health](./run-health.md) detector has flagged one as
looking stuck, a banner appears above the columns naming each count
separately ("1 run needs approval · 2 runs need an answer · 3 runs look
stuck") and linking straight to them — a human is the likely blocker, so
this is designed to be impossible to miss. The counts stay separate rather
than merging into one, so the banner always names the actual action owed.

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

One exception, and it looks like a bug if you do not know about it. Issues
without the `PRD` label are synced **open-only**, so closing one on the forge
is not something the frequent poll can see. The card keeps looking open until
the next reconcile pass (about 10 minutes at the shipped defaults), and then
it disappears rather than sliding into **Closed**. `PRD` cards are unaffected:
they animate into **Closed** as they always have.

## Which issues show up

A board's membership is **the primary label ∪ your extras**. The **primary**
label (`PRD` by default — see [Admin settings](./admin-settings.md)) is the
label uzi *writes* to mark an issue as its own work (Promote, a judge-filed
issue, board issue creation) and the one it fetches boards with; every board
shows it, always. **Extras** are the
labels layered on top — `bug` by default — tuned per repo, per account, from
the **Issues** control below.

Membership and run-*eligibility* are different questions, and being one
doesn't imply the other:

- **Membership** decides what you see. It's `primary ∪ extras`, and the
  Issues control (below) is the only thing that changes it.
- **Eligibility** decides what can start a run. An admin configures the
  run-eligible label set (`PRD` and `bug` by default) instance-wide,
  separately from any board's extras — see
  [Admin settings](./admin-settings.md).

A card that's both a member and eligible offers **Start run** directly: a
default-configuration `bug` card, for instance, is shown and runnable with no
extra step. A card that's a member but *not* eligible — a label your admin
put in "also show on boards" but left out of the run-eligible set — is drawn
with a **dashed border** and offers **Promote to PRD** instead: one click
adds the `PRD` label on the forge and the card becomes an ordinary, runnable
board citizen. There is no un-promote in uzi; remove the label in GitLab if
you change your mind. The reverse combination also exists: a label can be
made run-eligible without ever being added to a board's extras, in which
case an issue carrying it is runnable the moment its card is on screen for
some other reason (it carries a shown label too, or **Show all other
issues** is on) but never earns a default slot on the board just for being
eligible — visibility and eligibility are independently configured.

A member card that's also eligible still needs its issue description to link
a `prds/*.md` file to start a run, unless a non-primary eligible label waives
that — see [PRDLESS label](./prdless.md#the-prd-link-waiver). A card missing
a required link shows a warning badge and is excluded from agent pickup
until the link is added. A card carrying more than one column label (edited
outside uzi) shows a conflict badge and displays in its highest-positioned
column until the next move normalizes it.

Closed `PRD` issues keep appearing in the **Closed** column, since the
primary's fetch keeps the closed backlog cached. A closed *extra*-label
card never will: the fetch behind an extra label is open-issues-only, so
that card simply drops off the board when the issue closes rather than
sliding into Closed. Accepted, not a bug — see "Staying in sync" below for
the general open-only behavior extras share.

### The Issues popover

The toolbar's **Issues** control replaces the old "Show other issues"
checkbox. It lists the labels present on the repo's open issues, with a
count of how many additional cards each one would add, and lets you tune
your own extras:

- The primary label is a pinned row, always checked and un-removable — it's
  on every board.
- One row per label actually present on the board's issues, with a count of
  how many more cards ticking it would add.
- A configured admin default that has no matching issues on this repo still
  gets a row — greyed, showing `0` — so you can see that it's inert rather
  than wondering why it isn't there.
- **Show all other issues** is the old escape hatch, still last in the list,
  for "I don't know what label it has": it brings in everything else,
  member or not.
- **Reset to default** clears your override and re-adopts your admin's
  configured extras.

Your choice is **per repo, per account** — stored server-side, so it follows
you to any browser or device you sign in from, unlike **Hide empty** above,
which stays per-browser. It changes only what you see: nobody else's board
moves, and no label is written. (A pre-existing per-browser "show other
issues" preference from before this control existed is migrated into your
account automatically, once, the first time you load a board after
upgrading.)

Issues carrying only extra labels are only ever synced while they are
**open**, so they never reach the **Closed** column: one that closes on the
forge keeps looking open until the reconcile pass removes it, as described
above. uzi's own self-improvement tracking issue is always hidden.

A run that finishes a PRD is asked to move the file to `prds/done/` in its own
merge request (see [Agent skills](./skills.md)). Once that merge request
merges, uzi rewrites that PRD's link in the issue description so it still
resolves. It matches on the moved file's name, so a link to a *different* PRD
in the same description is left alone.
