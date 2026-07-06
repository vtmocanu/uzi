---
title: PRDLESS label
order: 85
audience: user
---

# PRDLESS label

Every run normally needs the issue description to link a `prds/*.md` file —
the PRD is the agent's spec. The **PRDLESS label** (default name `PRDLESS`,
see [Admin settings](./admin-settings.md)) is an escape hatch: an issue
carrying it can start a run with no PRD link at all, for work small enough
that a throwaway PRD file would just restate the issue.

This is a quality-gate bypass, not a security change. Applying the label
never weakens any of uzi's `main`-protection guardrails, and the run
proceeds exactly like a PRD-linked one: same state machine, same planning
turn, same human approval gate. A thin issue description just means a
thinner plan for you to review before clicking Start.

## Turning it on or off

An admin enables or disables the feature instance-wide, and sets the label's
name, from **Admin → Instance settings** — see
[Admin settings](./admin-settings.md). Turning it off only blocks *new* runs
— one already queued, claimed, running, or awaiting approval when the label
is removed or the feature is disabled keeps going.

## Applying or removing the label

Anyone with a uzi session on a connected repo can toggle the label — the
same population, and the same forge-first trust model, that can already
move a card between board columns. Two places:

1. **Issue view**: open the issue and use the PRDLESS toggle.
2. **Board card**: the same toggle appears on the card itself.

The toggle only appears when the feature is enabled instance-wide. Applying
or removing writes the label to GitLab first; the button only reflects the
new state once the forge confirms it, so a failed write never shows a change
that didn't really happen.

Once the label is applied, the card and issue view swap the "no PRD link"
warning badge for a `PRDLESS` badge, so at a glance you can tell a run may
start without a PRD link.

The first time the label is applied from uzi, it's auto-created on the
GitLab project (amber) — no manual setup needed. Applying it from GitLab's
own UI instead still requires creating the label there yourself first.

## Combining with autopilot

An issue can carry the PRD label, the autopilot label, and the PRDLESS label
all at once. That combination starts a fully unattended run with no plan
approval and no PRD link, because all three are separate, deliberate
opt-ins — see [Autopilot](./autopilot.md). Worth a second look before
adding PRDLESS to an issue that's already autopilot-eligible.
