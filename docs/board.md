---
title: Board
order: 30
audience: user
---

# Board

Each enabled repo gets a board in the sidebar: a kanban view of its GitLab
issues, kept in sync with the forge in both directions.

## Columns

- **Open** (implicit): issues carrying none of the configured column labels.
- One column per configured label (seeded on first open: `In Progress`,
  `Upcoming`, `Later`) — reconfigure any time from the board's column
  settings.
- **Closed** (implicit): the issue's GitLab state, not a label; cards here
  aren't draggable.

## Move a card

Drag a card to another column. uzi writes the label change to GitLab first
(`add`/`remove` on that issue) and only updates the board once the forge
confirms it — a failed write snaps the card back rather than showing a move
that didn't really happen. The same change is visible in GitLab's own issue
and board views.

![Dragging a card between board columns, relabeling the underlying GitLab issue](img/board-move-card.png)

## Why only some issues show up

The board lists only issues carrying the **`PRD`** label — uzi works PRDs,
not arbitrary tickets. Each card also needs its issue description to link a
`prds/*.md` file; a card missing that link shows a warning badge and is
excluded from agent pickup until the link is added. A card carrying more
than one column label (edited outside uzi) shows a conflict badge and
displays in its highest-positioned column until the next move normalizes it.

## Staying in sync

Moves and edits made in GitLab appear in uzi within one poll interval;
de-labeling, closing, or deleting an issue appears within one (less
frequent) reconcile pass. **Refresh** on the board triggers an immediate
full sync if you don't want to wait.
