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
token, and only for issues that trace back to you.

## 3. Add the label in GitLab

Add the autopilot label to any issue that already carries the PRD label.
uzi resolves *who added it*: the label adder if mapped and opted in, else
the issue's author, else neither — and it stops there. A run only ever
starts for someone who has both set their forge identity and opted in, with
the repo connected under their own account. Closed issues are never
autopilot candidates, even freshly relabeled — an autopilot-only guard the
manual **Start run** button doesn't have.

## What happens next

- **Eligible**: the issue moves to In Progress, a run starts unattended
  (never shows "awaiting approval"), the plan is recorded, and on success a
  comment lands on the issue with the merge request link.
- **No eligible user**: one comment explaining why, and no run — never
  repeats on later polls, even across a full resync.
- **No PRD link in the issue description**: one comment, no run.
- **Failed run**: one comment with a link to the run, not the failure
  reason itself — the run may contain agent-supplied free text, so the
  comment points at the access-controlled run page for detail rather than
  repeating it in a member-visible GitLab comment.

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
