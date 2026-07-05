---
title: Admin settings
order: 70
audience: user
---

# Admin settings

uzi keeps a small set of instance-wide settings in the database, editable by
an admin from **Admin → Instance settings**. Today: two forge labels.

## The two labels

| Setting | Default | Controls |
|---|---|---|
| PRD label | `PRD` | Which GitLab label marks an issue as factory work; only issues carrying it appear on any board. |
| Autopilot label | `autopilot` | Which GitLab label, added alongside the PRD label, triggers an unattended run for an opted-in user. See [Autopilot](./autopilot.md). |

## Validation

- Neither may be empty, longer than 64 characters, or contain a comma
  (GitLab's own label-list separator).
- The two labels must differ — equal values would autopilot every PRD issue.
- An invalid save is rejected before anything is written. The same rules run
  client-side first for immediate feedback, but the server is the source of
  truth.

## Changing a label never touches GitLab

Renaming a label here doesn't create or rename anything on the forge —
create the label in GitLab yourself (or it simply never matches anything).
uzi only reads label names; the label objects themselves stay entirely
GitLab's.

## Resync after a change

Saving a changed label triggers a full resync of every enabled repo, not
just the next incremental poll, so the effect isn't instant: boards drop
issues that only carried the old label and pick up the new set once that
repo's resync completes. See "Freshness contract" in
[Configuration](./configuration.md) for how sync cadence otherwise works.
This resync fires on either label changing, even the one (autopilot label)
that doesn't yet affect what boards show — harmless, and simpler than
special-casing which key mattered.

## No secrets here

Instance settings are plain values, readable by any admin — never put a
token, password, or PAT in a settings field. Secrets (Anthropic tokens,
forge PATs) have their own encrypted-at-rest storage; see
[ARCHITECTURE.md](../ARCHITECTURE.md#secrets-per-user-credentials-at-rest).
