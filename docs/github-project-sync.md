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

Two switches, held by two different people. An admin flips the
`github_project_sync_enabled` **instance kill switch** in **Admin →
Instance settings** (see [Admin settings](./admin-settings.md)), off by
default and a strict no-op while off — nothing below works until this is
on. Then, **per repo**, the repo's connection owner (or an admin) links it
to a Projects v2 board from the **Boards** page: the repo row's "Project
sync" cell opens a Manage panel with **Adopt** (link an existing board) or
**Provision** (have uzi create one) — the link itself is the per-repo
enabled state, no separate flag.

That's a two-tier authorization model: the admin holds the one instance-
wide lever, while each repo's own sync is managed by whoever owns its
forge connection, or by an admin. A user who is neither never sees the
Manage control and can't reach the routes behind it either — the API
existence-hides them (404) rather than exposing a 403 that would confirm
the repo exists. GitHub only; GitLab and Forgejo repos never show the
cell.

There's still no `uzi` CLI verb for any of this. The per-repo and
instance writes require a cookie session plus a CSRF token, while the CLI
authenticates with a bearer token and never carries either — so it can't
reach these routes structurally, not merely by policy.

## Adopt vs. provision

- **Adopt** links a project you already built by hand: give uzi its project
  number and uzi matches your board's column names against the Status field's
  existing options by exact name. A column with no matching option is skipped,
  and logged, never guessed — so if you later drag such a card in uzi its
  Status is left unchanged until you add the matching option.
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

## Board access — visibility and sharing

A board uzi **provisions** is created by the connection's PAT, so it's owned
by that account — often a separate bot account, kept deliberately out of your
own GitHub login — and, per GitHub's default, **private**. GitHub gates a
private project on project-level read access, so your repo access alone
doesn't let you see the board uzi just made; you'd otherwise have to leave
uzi and fix that in GitHub's own project settings. **Board access** puts
those same controls inside uzi instead.

You'll find it in the linked board's Manage panel on the **Boards** page,
right below the sync status readout — same reach as the rest of the panel:
the repo's connection owner (or an admin), GitHub only, and only once an
admin has turned on the `github_project_sync_enabled` instance switch above.

- **Visibility toggle.** Reads the board's current public/private state and
  lets you flip it — it round-trips GitHub's `ProjectV2.public` flag, so the
  toggle always reflects true state. When the board is public, the panel
  warns that it's visible to anyone on the internet.
- **Share with a GitHub user (Reader).** Type a GitHub username and uzi
  grants that user **Reader** access to the board; a Revoke control removes
  it. Reader is the only role this version grants.
- **Write-only, by GitHub's own limit.** GitHub's Projects v2 API exposes no
  readable collaborator list, so uzi can grant and revoke access by username
  but cannot show you who currently has it. The panel lists only the users
  you granted **in the current session**, as a convenience for revoking —
  not as an authoritative list of who has access right now. To remove
  someone's access later, revoke them by username.
- **Bad usernames are reported, not swallowed.** An unknown GitHub login
  comes back as a clear inline error, distinct from a transient or
  permission failure — so a typo never looks like a successful grant.

Like the rest of the panel, these are web-only actions; there's still no
`uzi` CLI verb for them.

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
