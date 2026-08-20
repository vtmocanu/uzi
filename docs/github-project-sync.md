---
title: GitHub Projects v2 sync
order: 71
audience: user
---

# GitHub Projects v2 sync

uzi's board is a label-driven kanban: a card's column *is* a forge label (see
[Board](./board.md)). This feature keeps that board in step with a linked
GitHub **Projects v2** board's built-in **Status** single-select field, for
a user who prefers GitHub's native UI. **GitHub only** — GitLab and Forgejo
repos are untouched. The label always stays canonical; Status is a
projection of it, never the other way round. Only column labels sync
(`Planned`, `In Progress`, `Human Review`, `Later`, …); other labels (`PRD`,
`bug`, `autopilot`, …) are never touched.

## Forward vs. reverse — why reverse isn't instant

Moving a card in uzi (a drag, or the run lifecycle) sets the linked item's
Status as part of that move. There's no equivalent trigger the other way:
GitHub fires no repo-level event for a board drag, and uzi has no inbound
webhook endpoint to receive one anyway. So a drag inside GitHub is picked up
on uzi's regular poll cadence instead — eventually consistent, not instant.

## Enabling it

Two admin-only switches: the `github_project_sync_enabled` **instance kill
switch** (see [Admin settings](./admin-settings.md)), off by default and a
strict no-op while off; then, **per repo**, an admin links it to a Projects
v2 board by either **adopting** an existing one or having uzi **provision**
a new one — the link is the per-repo enabled state, no separate flag. Both
are admin-only API actions today: no `uzi` CLI verb (the CLI admin surface
is read-only by design — the write endpoints are cookie-only) and no
dedicated settings-page control yet.

## Adopt vs. provision

- **Adopt** links a project you already built by hand: give uzi its project
  number and uzi matches the Status field's existing options against your
  board's column names. An unmatched option is skipped, and logged, never
  guessed.
- **Provision** is zero-click: uzi creates a new project, adds its own
  single-select field ("uzi Status", not GitHub's built-in Status) seeded
  with one option per board column, links it to the repo, and seeds every
  current issue's Status from its column. Provisioning a repo that already
  has a link is refused — disable it first.

## The `project` PAT scope

The connection's GitHub PAT must carry the **`project` scope** (read+write —
`read:project` is read-only and cannot drive the sync). A classic PAT's
scopes can be edited in place on github.com, no re-paste needed. See
[GitHub bot setup](./github-bot-setup.md) for the base `repo` scope this
adds to.

## What the model does and doesn't cover

A Status field holds exactly one value, matching a card in exactly one
column. An issue with no column label (uzi's implicit "Open") maps to
GitHub's native **No Status**. A closed issue is left to issue state — no
dedicated "Done" option — and uzi stops tracking its card, leaving it on the
project at its last-known Status, never deleted. Disabling sync for a repo
is likewise non-destructive: uzi drops its own link and stops tracking, but
never deletes a GitHub project or any of its cards, whether uzi created the
project or you did.

Status options are built once, at adopt/provision time — a later board
column add/rename/remove is a manual step in v1, not propagated to GitHub.
