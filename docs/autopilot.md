---
title: Autopilot
order: 80
audience: user
---

# Autopilot

With autopilot on, adding the **autopilot label** (default `autopilot`, see
[Admin settings](./admin-settings.md)) alongside the **PRD label** to a
GitLab issue starts a run for you, unattended: no plan-approval step,
spending your own Anthropic token. The plan is still recorded in run
history as an audit trail, and the merge request stays your human review
gate. Off by default, and opt-in per user — see below.

## 1. Set your forge identity

uzi only knows your bot account's identity from its PAT, not yours. Open
**Settings → Forge → Your forge identity (for autopilot)** and enter your
own GitLab username for that connection. uzi checks it against the forge on
save; an unresolved username still saves, with a warning, since a forge
blip shouldn't block the save. See [GitLab bot setup](./gitlab-bot-setup.md) or
[Forgejo bot setup](./forgejo-bot-setup.md) for the rest of the Forge page.

Matching is exact and case-sensitive, per forge host: a case-variant of your
username is a different identity for this purpose and isn't deduplicated
against yours. Use the exact casing GitLab shows for your account — a
username value is claimed on a first-saved-wins basis, so entering someone
else's exact username first blocks their own later save with that value.

## 2. Opt in

Open **Settings → Autopilot** and check **Enable autopilot for my
account**. Off by default; attribution only ever spends your own Anthropic
token, and only for issues that trace back to you. An autopilot run is an
ordinary run, so it spends whatever the worker that claims it is bound to —
your default unless you pointed that worker somewhere else (see
[Anthropic tokens](./anthropic-token.md)).

## 3. Add the label in GitLab

Add the autopilot label to any issue that already carries the
run-eligibility (`uzi`) label **or** is assigned to the uzi-bot account —
bot assignment satisfies only the run-eligibility condition, so the
autopilot label is still required for an unattended run. uzi resolves *who
added it*: the label adder if mapped and opted in, else
the issue's author, else neither — and it stops there. A run only ever
starts for someone who has both set their forge identity and opted in, with
the repo connected under their own account. Closed issues are never
autopilot candidates, even freshly relabeled — an autopilot-only guard the
manual **Start run** button doesn't have.

**Bot-assigned issues are candidates too.** An issue is eligible for a run
whenever it carries the run-eligibility label above **or** is assigned to
the uzi-bot account (see [Admin settings → Run
eligibility](./admin-settings.md#run-eligibility)), so the poller also picks
up an autopilot-labelled issue that's bot-assigned but never labelled.
Consent and attribution still key on the **autopilot-label add event**, not
the assignment: an assignment has no adder to resolve, so it never bypasses
the who-added-it rule above — it only widens which issues become autopilot
candidates once the autopilot label lands.

## What happens next

- **Eligible**: the issue moves to In Progress, a run starts unattended
  (never shows "awaiting approval"), the plan is recorded, and on success a
  comment lands on the issue with the merge request link.
- **The agent never stops to ask you something, either.** If it would
  otherwise pause for a clarifying question (see [Answering a
  question](./run-activity.md#answering-a-question)), an autopilot run
  auto-resolves instead — "proceed on your best judgment" — and notes the
  assumption it made in the run feed rather than parking.
- **No eligible user**: one comment explaining why, and no run — never
  repeats on later polls, even across a full resync.
- **Failed run**: one comment with a link to the run, not the failure
  reason itself — the run may contain agent-supplied free text, so the
  comment points at the access-controlled run page for detail rather than
  repeating it in a member-visible GitLab comment.

## PRD updates happen unattended

An issue run is asked to update the issue's linked `prds/*.md` file before it
finishes, and to move it to `prds/done/` if the PRD is now fully complete (see
[Agent skills](./skills.md)). An autopilot run does this with nobody in the
loop, the same way it writes code with nobody in the loop. The edit and the
`git mv` are ordinary commits on the run's branch, so they arrive as reviewable
file changes in the merge request, never as an out-of-band write.

Two things worth knowing before you label an issue:

- **A run using [repo agents](./repo-agents.md) can move a PRD to `prds/done/`
  unattended.** Autopilot defaults to the repo's roster when one is detected,
  and a repo-authored reviewer's sign-off is not uzi's own review. Read the PRD
  diff when you review the merge request, the same as the code.
- **The issue's own PRD link is corrected only after you merge.** If the run
  moved the file, uzi rewrites that link in the issue description once the
  merge request has merged, so your merge decision is what authorizes the edit.
  It only ever repoints a `prds/*.md` link the description already carried, and
  only one whose filename matches the file the run declared it moved, so a link
  to a different PRD is left alone. What that bound does not cover: the run's
  own declaration is what picks which link, and nothing verifies the file really
  moved, so an issue linking several PRDs can end up with the wrong one pointing
  at a path that does not exist. That costs a stale link on that one issue and
  nothing wider.

## Retry

Autopilot only reacts to a label being *added*; fix whatever blocked it
(map your username, opt in, add the PRD link, shorten an oversized
description), then remove and re-add the autopilot label. A failed run is
retried the same way. Toggling the label with nothing else changed is
otherwise a no-op — a run never auto-retries on its own.

## Mid-run label changes

Removing the autopilot label while a run is active changes nothing — the
run keeps going. Re-adding it while a run is already active is swallowed
silently (no second run, no comment): the active run already covers it.

## If two people connect the same project

Each forge connection carries its own consent. If two uzi users both
connect the same GitLab project, adding the label can start a run under one
connection's owner while the other connection independently — and
correctly — reports "no eligible user". Each owner's opt-in only ever gates
their own connection; one person's label add never spends someone else's
tokens.
