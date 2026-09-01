---
title: AI attribution in commits
order: 85
audience: user
---

# AI attribution in commits

Every commit uzi's worker makes carries the Agent SDK's standard
`Co-Authored-By: Claude` attribution trailer. On by default, so it matches
today's behavior for everyone who never touches the setting. Turn it off if
your organization's policy requires no AI attribution in git history.

## Opt out

Open **Settings → Run defaults**, find the **AI attribution in commits**
card, and uncheck **Include the Co-Authored-By: Claude trailer in my worker
commits**. It's an account default, and it's owner-only — you can only
change it for yourself, and there's no admin force-toggle for it (unlike
[Automatic CI fixes](./ci-autofix.md#1-opt-in) and the [run judge](./judge.md),
which admins can enable or disable for any individual user).

## When it takes effect

The value is read fresh from your account each time a worker **claims** a run
— not when the run is created — so no worker restart is needed. It applies to
every run claimed after you change it, including one that is still queued: a
queued run hasn't been claimed yet, so it picks up the current setting when a
worker takes it. A run that is already running keeps the setting it was
claimed with, until it next resumes (a resume re-reads the current value).

## Scope: the commit trailer only

This affects the `Co-Authored-By` trailer on the worker's own commits and
nothing else. Merge-request and pull-request descriptions are unaffected
either way: uzi builds those itself, and they already carry no AI
attribution regardless of this setting.
