---
title: Issue trackers (YouTrack)
order: 76
audience: user
---

# Issue trackers (YouTrack)

A repo can take its issues from a YouTrack project while its code, merge
requests and CI stay on its forge. YouTrack is the only issue tracker
supported. A repo with no link behaves as before.

## Before you start (operator)

The operator must set `ISSUE_TRACKER_ALLOWED_BASE_URLS` (Helm
`issueTracker.allowedBaseURLs`). It is empty by default, so no connection can
be made until it is set. Entries are a bare `https://host[:port]`: https scheme
and host only. A YouTrack served under a context path (such as `/youtrack`) is
not supported. See [Configuration](./configuration.md).

## Connect

1. In YouTrack, open Profile, Account Security, New token. Give it the scope
   YouTrack, for the account uzi acts as.
2. In uzi, open Settings, Issue trackers, and add a connection: pick the base
   URL from the operator's allowed list and paste the token. (A kind picker
   shows only once more than one kind is supported; today YouTrack is implicit.)

The token is write-only: it is stored sealed, never shown again, and never sent
to the worker. Reconnecting with the same base URL replaces the token; if a repo
is linked, the new token must belong to the same YouTrack account. A connection
with a linked repo cannot be deleted.

## Link a repo

A repo that was never enabled and is not yet linked offers "Link to an issue
tracker" on the Repos page, but only when the operator has set the allowed base
URLs or you have an issue tracker connection. With neither, the Repos page looks
as it did before. Choose the connection, the project and the eligibility tag, and tick
"They can edit issues in this project". The choice cannot be changed afterwards
(no unlink or relink). A repo that was ever enabled cannot be linked: that
includes every repo that existed when issue tracker links were added and a repo
that was enabled, then removed and listed again. Link in the web before
anything enables the repo, including `/server setup`.

Issues carrying the eligibility tag come into uzi. The tag is chosen by id: a
same-named tag of another user does not count, and renaming it keeps working.

- The connection's account must be able to apply the tag, and so must the people
  who start work by tagging.
- A named group never establishes the connection account's own access. It counts
  for others only when YouTrack reports at least two members for it (a missing
  count does not count). So a tag shared only with a named group can be refused
  at link time: share the eligibility tag with named users or with All Users.
- Tag sharing is checked at link time. Whether those people can edit issues in
  the project is not checked, and the admin confirms it.

## A plain State field is required

uzi needs the project's State field to be a plain one it can read. A project
whose State field is run by a state-machine workflow rule in YouTrack is not
supported, and neither is one whose State field the connection's account cannot
read as a plain State field.

At link time uzi reads the first page (up to 100) of the project's issues, with
the same permission it uses to take issues in, and refuses the project if any
issue's State field is run by a state machine. The link is refused with:

> project ABC uses a state-machine workflow on its State field; uzi needs a
> plain State field. Link a project with a plain State field, or remove the
> state-machine rule from the State field in YouTrack.

The link is also refused if an issue's response lists no State field, or lists
one that is not a plain State field, for example because the connection's
account is not allowed to see the field, or the project has no field by that
name. It is refused with:

> uzi could not read project ABC's State field as a plain State field
> (issuetracker: the State field could not be read as a plain State field: issue
> "ABC-12"). The connection's account may not be allowed to see the State field,
> or the project has no field by that name. Let that account see the State field
> in YouTrack, or link a project whose State field it can read.

The same refusal is shown on the "Map States" panel (reading and saving the
mapping). Nothing is linked or saved. If issues in the first page show both
problems, the state-machine reason is shown, because it is the more specific one.

If the check cannot be made (YouTrack answers 403 or 404, is unreachable, or
returns issues none of which is in that project), the link fails with `could not check project
ABC's State field` and the reason. Nothing is linked.

A project with no issues yet links, because there is nothing to check. It is
checked on its first State write, and that write fails closed if the State field
turns out to be run by a state machine or cannot be read as a plain State field.
Before every State write uzi checks the issue's own field again.

On a repo that is already linked to such a project (state machine or unreadable
State field), nothing is written:

- The "Map States" panel shows the reason.
- Moving, closing or reopening a card answers 409 with the reason, and the card
  stays where it was.
- A run retries its State move for up to 30 minutes without writing, then
  gives up; the merge request watch skips the move.

To fix it, link another project (a new link needs a repo that was never enabled)
or remove the state-machine rule from the State field in YouTrack. For an
unreadable State field, let the connection's account see the State field in
YouTrack (field visibility and permissions), check that the project has a State
field, or link a project whose State field it can read.

## Map board columns to States

On a linked repo each board column maps to one State, plus the open State and
the closed State. The closed State must mean done (for example Fixed or Done),
never Won't fix; uzi refuses it. Moving a card into an unmapped column is
refused until it is mapped. Use "Map States" on the repo row or in the board's
column settings.

The web shows an issue's key (for example `ABC-12`, from the project short name
stored at link time) on the board (cards, search, screen-reader labels), the
issue page (header, breadcrumb), the runs list and run page heading, created-issue
lists, the findings group-file notice, the board's needs-attention strip and
the run options name. It still shows `#number` in the
overview run row, the needs-decision card, triage, a schedule's last fire,
proposal cards, the command menu and the capacity rail.

## Not offered on a linked repo

Autopilot, GitHub Projects v2 sync, creating repo labels, eligibility by
assigning the issue to the uzi bot, and changing the issue source after the
first enable.

There is no author mapping setting: only the connection's own YouTrack account
counts as a uzi user. Comments by anyone else are withheld from runs, and a
scheduled run auto-approves its plan only for an issue that account reported
and whose title and body only it edited.

## An issue moved to another YouTrack project

It gets a new number there, so uzi treats it as gone from the linked project.
Later issue writes for a live run are skipped and logged, the run's code side
(branch, MR, CI) continues, and the next full sync (every
`FORGE_RECONCILE_EVERY`-th poll, default 10) removes it from the board.

## From the CLI

`uzi issue-tracker list` and `uzi repo issue-tracker-states <repo>` are
read-only. Connecting and linking are in the web only, because they take a token
or change the repo's issue source. See [CLI](./cli.md).
