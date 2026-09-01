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
(`Planned`, `In Progress`, `Human Review`, `Later`, …); other labels (`bug`,
`autopilot`, …) are never touched.

## Forward vs. reverse — why reverse isn't instant

Moving a card in uzi (a drag, or the run lifecycle) sets the linked item's
Status as part of that move. There's no equivalent trigger the other way:
GitHub fires no repo-level event for a board drag, and uzi has no inbound
webhook endpoint to receive one anyway. So a drag inside GitHub is picked up
on uzi's regular poll cadence instead — eventually consistent, not instant.

**A cap bounds how much damage one reverse-sync tick can do.** Reverse
sync writes each item's live GitHub Status back onto the real issue as a
board-column label, and one genuine drag should only ever move one issue.
If something on the GitHub side goes wrong all at once instead — a Status
field's options edited or cleared out from under uzi — uzi caps how many
issue-label changes a single tick will make, so a bulk change upstream can
never cascade into mass-relabeling your real issues. Once a tick's
intended changes cross that cap, uzi aborts the whole tick, changes
nothing, and records the failure as the sync's last error instead.

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

There's still no `uzi` CLI verb for Adopting, Provisioning, or the board-
access controls below. The per-repo and instance writes require a cookie
session plus a CSRF token, while the CLI authenticates with a bearer token
and never carries either — so it can't reach those routes structurally,
not merely by policy. A small `uzi project-sync status`/`resync` CLI
does exist for checking, and re-running the sync of, an already-linked
repo (it never creates or removes a link) — see the
[CLI reference](./cli.md).

## Sync health at a glance

The repo list on the **Boards** page carries a "Sync" pill next to each
repo, and it reflects real state instead of sitting there as a static
label: **green** when the repo is linked and its last sync ran clean, a
**warning/error** tone when the last sync recorded an error, and
**neutral** when the repo isn't linked at all. Use it as the first signal
that something needs attention before opening the Manage panel — a
freshly-adopted large board can take a short while to finish seeding (see
[Adopting returns immediately](#the-adopt-flow-step-by-step) below), and
the badge is how you'd notice that seeding is still in flight or failed.

## Adopt vs. provision

The sync panel leads with **Adopt** as the recommended default, with a
terse explainer up front: *"Adopt a Project you already created
(recommended); Provision only if uzi should create one for you (org
repos)."* Provision remains available and is the safe, zero-risk path for
an **org-owned** repo. For a **user-owned** repo, Provision is shown
**disabled** with a reason instead of failing after the fact: GitHub
requires a linked project's owner to match the repo's owner, and a bot
account can't own a Project under someone else's personal account — so
Adopt is the only path there. (If uzi can't determine the repo's owner
type, it falls back to showing both options rather than guessing.)

- **Adopt** links a project you already built by hand: give uzi its project
  number and uzi matches your board's column names against the Status field's
  existing options by exact name. A column with no matching option is skipped
  — see [Skipped columns and fixing them](#skipped-columns-and-fixing-them)
  below — never guessed, so if you later drag such a card in uzi its Status
  is left unchanged until the column has a matching option. See [The Adopt
  flow, step by step](#the-adopt-flow-step-by-step) for the full walkthrough.
- **Provision** is zero-click: uzi creates a new project, adds its own
  single-select field ("uzi Status", not GitHub's built-in Status) seeded
  with one option per board column, links it to the repo, and seeds every
  current issue's Status from its column. Provisioning a repo that already
  has a link is refused — disable it first. Because this uses "uzi Status"
  rather than the built-in Status, set the board's **Column by** to it as
  described in [Skipped columns and fixing them](#skipped-columns-and-fixing-them).

## The Adopt flow, step by step

1. **Create a Project** (Projects v2 — the current GitHub Projects, not the
   old per-repo Projects) under your own GitHub account, or an org you
   belong to.
2. **Invite the uzi bot as a Write collaborator on the Project itself.**
   This is separate from adding the bot to the *repo*: a Project has its
   own membership, and the bot needs write access there too before it can
   read or move the Status field. See [GitHub bot setup → Project sync:
   invite the bot onto the
   Project](./github-bot-setup.md#project-sync-invite-the-bot-onto-the-project-github-projects-v2-only)
   for the exact steps.
3. **Name the Project's Status options to match uzi's board column labels**
   (`Planned`, `In Progress`, `Human Review`, `Later`, …) — Adopt matches by
   exact name. You can skip this and Adopt anyway; an unmatched column just
   comes back skipped (see [Skipped columns and fixing
   them](#skipped-columns-and-fixing-them)) until you add the option or use
   auto-create.
4. **Adopt.** On the **Boards** page, open the repo's Manage panel, pick
   Adopt, and enter the Project's number (the `N` in the project's URL).
   Adopt persists the link and returns right away; seeding every current
   issue's Status runs in the background, so a large board doesn't leave
   the request hanging — watch the [sync health badge](#sync-health-at-a-glance)
   for progress, and give a freshly-adopted large board a little time to
   finish populating.

## Skipped columns and fixing them

If a board column has no matching Status option when you Adopt (or
Resync), the Manage panel lists it by name under the sync status readout:
*"These board columns have no matching Status option and won't sync:
…"*. Until it's fixed, cards in that column simply don't move on GitHub's
side — nothing is guessed or silently dropped elsewhere.

Two ways to fix it, both offered from the same panel:

- **Add the matching Status option in GitHub, then click Resync.** Resync
  re-reads the *same* field the link already points at — by its stored
  field id, never by re-resolving the name — and re-maps your board
  columns against that field's current options, so it picks up anything
  you've since added there. It never switches fields: on a board synced
  via uzi's own "uzi Status" field, Resync keeps using "uzi Status" and
  will not fall back to the built-in Status. It's idempotent, so it's
  safe to run any time.
- **Auto-create the missing columns.** uzi creates its own fresh "uzi
  Status" field on the Project, containing every one of your board
  columns as an option, and points sync at that field instead. This never
  modifies or deletes the existing Status field or any of its options — it
  only creates a brand-new one — which is what makes it safe to click
  without risking anything already on the board. The tradeoff: your
  Project ends up carrying **two** status-like fields (the original
  Status, and uzi's own "uzi Status"), since the old one is left alone
  rather than edited in place.

  If a board's link was already mis-pointed back to the built-in Status
  field by an older uzi version — the symptom is most columns showing as
  unmatched/"won't sync" after a Resync, with sync writing to the wrong
  field — re-running **auto-create** re-establishes the link to "uzi
  Status" on the *same* board and is the recommended way back. (On an org
  repo, **Provision** also lands you on a "uzi Status" field, but it spins
  up a *fresh* board rather than re-pointing the current one.) A
  secondary, manual option: temporarily rename the built-in
  Status field in GitHub, click Resync once, then rename it back — only
  relevant on an uzi version old enough to still resolve by name.

**After auto-create (or Provision), point the board view at "uzi Status".**
Because "uzi Status" is a *separate* field, your GitHub board keeps grouping
by whichever field it grouped by before (usually the built-in **Status**,
whose default options are `Todo` / `In Progress` / `Done` / `No Status`) — so
the board still shows *those* columns even though uzi is now writing your
board's columns to "uzi Status". This looks like "sync did nothing", but the
values are there on the other field. Switch the board over in GitHub: open the
board, click **View** (top right), set **Column by** to **uzi Status**, then
click **Save view** — this last step is required. An unsaved grouping change is
local and temporary: it reverts on reload and other viewers still see the old
columns, so it looks like the switch didn't take. Once saved, the board shows
your uzi columns with every item already populated. (In Table layout the same
control is labeled **Group by**; the original Status field is harmless and can
be ignored or deleted.)

## Closed issues and the Done status

Closing an issue now projects to a dedicated **Done** Status option on the
linked board, and uzi keeps the card — it no longer stops tracking it, the
way it used to. This runs on uzi's periodic sync tick, the same poll
cadence described in [Forward vs. reverse](#forward-vs-reverse--why-reverse-isnt-instant),
so it lands on the next tick rather than the instant you close the issue.
Reopening the issue restores the card to its current column (or clears it
to **No Status** if the issue has no column label) and resumes normal
tracking.

Where the `Done` option comes from depends on how the field was built:

- **Provision, or auto-create** (see [Skipped columns and fixing
  them](#skipped-columns-and-fixing-them)): uzi appends a `Done` option
  whenever it creates its own field, so a uzi-owned "uzi Status" field
  carries `Done` automatically — no user action needed.
- **Adopt, on GitHub's built-in Status field**: the built-in Status ships
  `Todo` / `In Progress` / `Done` / `No Status` by default, so an adopted
  built-in Status field already has `Done` and picks it up automatically
  too.
- **A `uzi Status` field or custom field with no `Done` option**: uzi never
  adds an option to an existing field — there's no safe API for that, the
  same constraint behind [Skipped columns and fixing
  them](#skipped-columns-and-fixing-them). Add a `Done` option to the field
  in GitHub yourself, then click **Resync** — or re-provision. Until then,
  the sync panel and `uzi project-sync status` (see the [CLI
  reference](./cli.md)) carry an advisory: *"Closed issues won't show a
  Done status. Add a `Done` option to the synced field and Resync, or
  re-provision."*

**One-time note for boards linked before this feature shipped.** Whether a
link shows the advisory depends on whether it has a *stored* Done-option
id, and linking (or last resyncing) an older board never captured one — so
even a board that adopted the built-in Status field, which already has
`Done`, can show the advisory until its next Resync. A single Resync
re-captures the existing `Done` option and clears the advisory; treat this
as a one-time step after upgrading, not a bug.

Reverse never reacts to Done: dragging a card to Done in GitHub, or a Done
item showing up on the board any other way, never reopens or closes the
issue. uzi only ever changes issue open/closed state because you closed or
reopened the issue itself.

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
GitHub's native **No Status**. A closed issue projects to a dedicated
**Done** option and keeps its card tracked, and reopening it restores the
card to its column — see [Closed issues and the Done
status](#closed-issues-and-the-done-status) above for the full behavior,
including the fields that don't have a `Done` option to project to yet.
Disabling sync for a repo is likewise non-destructive: uzi drops its own
link and stops tracking, but never deletes a GitHub project or any of its
cards, whether uzi created the project or you did.

Status options are built at adopt/provision time. A board column added
afterward that has no matching option is handled by [Resync or auto-create](#skipped-columns-and-fixing-them)
— but a later column **rename or remove** is still a manual step in v1:
uzi never edits or deletes an existing Status option, so renaming or
removing a board column doesn't propagate to GitHub on its own.
