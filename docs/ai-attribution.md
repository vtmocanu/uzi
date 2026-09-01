---
title: AI attribution in commits
order: 85
audience: user
---

# AI attribution in commits

Every commit uzi's worker makes carries a `Co-Authored-By: Claude
<noreply@anthropic.com>` trailer, added by the Agent SDK itself. On by
default, so it matches today's behavior for everyone who never touches the
setting. Turn it off if your organization's policy requires no AI
attribution in git history.

## Opt out

Open **Settings → Run defaults**, find the **AI attribution in commits**
card, and uncheck **Include the Co-Authored-By: Claude trailer in my worker
commits**. It's an account default, and it's owner-only — you can only
change it for yourself, and there's no admin force-toggle for it (unlike
[Automatic CI fixes](./ci-autofix.md#1-opt-in) and the [run judge](./judge.md),
which admins can enable or disable for any individual user).

## When it takes effect

The value is read fresh from your account on each run claim, so it applies
starting with **the next run you start** — no worker restart needed. A run
that's already running or queued when you flip the toggle keeps whatever the
setting was when it claimed the work.

## Scope: the commit trailer only

This affects the `Co-Authored-By` trailer on the worker's own commits and
nothing else. Merge-request and pull-request descriptions are unaffected
either way: uzi builds those itself, and they already carry no AI
attribution regardless of this setting.
