---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: three forge labels and a
default theme.

## The three labels

| Setting | Default | Controls |
|---|---|---|
| PRD label | `PRD` | Which GitLab label marks an issue as factory work; only issues carrying it appear on any board. |
| Autopilot label | `autopilot` | Which GitLab label, added alongside the PRD label, triggers an unattended run for an opted-in user. See [Autopilot](./autopilot.md). |
| PRDLESS label | `PRDLESS` | Which GitLab label lets an issue start a run with no `prds/*.md` link, when the toggle below is on. See [PRDLESS label](./prdless.md). |

## The PRDLESS toggle

Unlike the other two, the PRDLESS label also has its own instance-wide on/off
switch, separate from its name — **on** by default. Turning it off requires
every run on this instance to have a real PRD link again; it doesn't touch a
run already in flight. *(Note: this toggle and the label-name field land on
the Settings page in a later milestone of this release; until then the
feature runs at its compiled-in default — on, named `PRDLESS`.)*

## Default theme

Which theme a user with no personal choice sees — new users, and anyone who
hasn't picked one under Settings → Appearance. A user's own pick, once made,
always wins over this setting. Saving restyles the admin's own session live;
every other un-overridden user picks up the change on their next `me`
refresh (in practice, their next login or reload — there's no push). See
[Theming](./theming.md) for how themes work and how to add one.

## Validation

- A label value (PRD, autopilot, or PRDLESS) may not be empty, longer than 64
  characters, or contain a comma (GitLab's own label-list separator).
- PRD and autopilot must differ — equal values would autopilot every PRD
  issue. *(Note: the same later milestone that brings the PRDLESS toggle to
  the Settings page also makes PRDLESS pairwise-distinct from both other
  labels — checked even while its toggle is off, so re-enabling it later is
  always safe — and gives the toggle a strict on/off parse. Until then
  neither check is enforced.)*
- An invalid save is rejected before anything is written. The same rules run
  client-side first for immediate feedback, but the server is the source of
  truth.

## Changing a label never touches GitLab

Renaming a label here doesn't create or rename anything on the forge —
create the label in GitLab yourself (or it simply never matches anything).
uzi only reads label names; the label objects themselves stay entirely
GitLab's. The one exception is applying the PRDLESS label from uzi's own UI,
which does create it on first use — see [PRDLESS label](./prdless.md).

## Resync after a change

Saving a changed PRD or autopilot label triggers a full resync of every
enabled repo, not just the next incremental poll, so the effect isn't
instant: boards drop issues that only carried the old label and pick up the
new set once that repo's resync completes. See "Freshness contract" in
[Configuration](./configuration.md) for how sync cadence otherwise works.
This resync fires on any changed label (the PRD label, the autopilot label,
and for now the two PRDLESS keys), even the autopilot label, which doesn't
affect what boards show — harmless, and simpler than special-casing which key
mattered. A default-theme-only save does **not** trigger it: theming is
presentation-only and never affects what a board shows. *(Note: a later
milestone of this release also excludes the PRDLESS keys from triggering this
resync, since neither changes which issues a board shows, only whether a run
can start without a PRD link.)*

## No secrets here

Instance settings are plain values, readable by any admin — never put a
token, password, or PAT in a settings field. Secrets (Anthropic tokens,
forge PATs) have their own encrypted-at-rest storage; see
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest).
