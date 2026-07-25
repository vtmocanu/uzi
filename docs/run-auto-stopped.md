---
title: Why was my run stopped automatically?
order: 36
audience: user
---

# Why was my run stopped automatically?

Sometimes a run ends with **failed** and the reason *"uzi stopped this run: its updates could not be saved"*, or simply *"run cancelled"* when the agent reported its own stop. Either way the run is marked with the stop kind **`auto_stopped`**, and `uzi run get <id>` shows it.

**This is not a failure of your code, and the agent did not do anything wrong.** It means the server could not write the agent's updates to the database, over and over, for the same reason each time — so uzi stopped the run rather than let it spin.

## What actually happens

The agent streams its work back to uzi as messages. Occasionally a message is something Postgres genuinely cannot store — most often output containing raw NUL bytes (a headless Chromium's error spew is the case that first produced this), a broken Unicode fragment, or a number too large for the database's numeric type. An older worker image treats every rejection as "try again", so it re-sends the same message forever at roughly twice a second: the run reads `running` while producing nothing, spending nothing, and holding a slot until `RUN_TIMEOUT` (two hours by default).

Modern workers (v0.10.1 and later) handle this themselves: they isolate the one bad message, replace it with a marker, and carry on — so you should rarely see an auto-stop at all. **The auto-stop exists mainly for workers on older images.** If you are seeing these, the first thing worth checking is your worker's version.

## What uzi checks before stopping anything

An auto-stop is deliberately hard to trigger. Every one of these must hold:

1. at least 20 consecutive failures for that one run;
2. sustained for at least 60 seconds;
3. no progress in that time — the run's message counter has not moved;
4. the same kind of error every time (an error that keeps *changing* looks like an outage, not a bad message);
5. that error is one **a correct worker could hit through no fault of its own** — output uzi genuinely cannot store, or a batch that grew too large. A malformed request is the worker being broken, and uzi does not stop runs for that (see below);
6. **other runs on the same uzi are successfully saving messages** in that window.

The sixth is the important one. If uzi's database is having a bad day, *every* run's writes fail, and killing runs during an outage would turn a bad hour into lost work — so when nothing else is succeeding, uzi flags and stops there. **A consequence worth knowing: if yours is the only active run, uzi will never auto-stop it.** That is intended, not a gap.

## Flagged but not stopped

A run can carry the `looping` flag forever and never be auto-stopped. That is a real outcome, not a bug, and each cause has its own remedy:

| Why | What to do |
|---|---|
| Nothing else on this uzi is saving messages (guard 6) — often because yours is the only active run | Nothing. The flag is the signal; the run ends at `RUN_TIMEOUT` if it never recovers. |
| The failing requests are **malformed** (guard 5) | **Roll the worker image.** A malformed batch is not something a correct worker produces, so this says the worker *build* is broken — and a build defect hits every run that worker touches. Stopping them one at a time would hide the pattern while the same image kept claiming new work. Operators: the log line carries `failure_class=invalid`. |
| The error keeps changing kind (guard 4) | Usually a transient infrastructure problem resolving itself. If it persists, check the api and database. |

**An absent flag is not proof of an absent wedge.** The streak resets whenever the error changes kind, so a worker whose batch oscillates around the size limit can fail continuously and never accumulate enough of one kind to be flagged at all. That is fail-safe by design, but it means "no flag" means "no *confirmed* loop", not "no problem".

## How you find out

Before any stop, the run is flagged in the UI and the CLI, and Slack notifications DM you on that flag:

```
HEALTH          looping
HEALTH_REASON   the agent's updates can't be saved, so it keeps resending them
```

An auto-stopped run is then styled as **breakage**, not as a stop you asked for — a rose "failed" badge, and it counts toward the browser tab's attention mark. That is on purpose: a run you cancelled is not a problem, and this one is.

## What to do about it

1. **Check the worker's version.** On v0.10.1+ the worker isolates the bad message on its own; upgrading is the real fix.
2. **Look at what the agent was doing just before the run went quiet** — the last message in the run view. The culprit is usually a tool whose output carried binary data: a browser, a hex dump, a compressed artifact, a `cat` of something that was not text.
3. **Re-run it.** Nothing about your repository, branch, or issue was changed by the stop.
4. If it repeats on the same step, have the agent avoid capturing that tool's raw output (redirect it to a file, or filter it) and re-run.

## For operators

Auto-stop can be turned off with `UZI_AUTOSTOP_ENABLED=false` (see [Configuration](configuration.md)). That disables **only the stop** — a wedged run is still flagged, so you keep the visibility and give up the intervention. It is deliberately separate from the [Run health](admin-settings.md#run-health) toggle: turning health off must not silently disable loop protection.

There is no metrics endpoint in uzi today, so the log lines are the surface. Grep the `api` container for:

| Log line | Means |
|---|---|
| `auto-stopping a run whose messages cannot be persisted` | a run was auto-stopped. The `action` field says how: `verdict_enqueued` (the worker was asked to stop), `server_side_failed` (no live worker, so the server ended it), `escalated` (the worker was asked and did not comply within 60s). |
| `auto-stop is holding` | a run is wedged and will **not** be stopped. `decision` says why: `no_comparison_set` (nothing else was succeeding, so there was no evidence the fault was this run's) or `class_not_killable` (the requests are malformed — **roll the worker image**; `failure_class` names the class). |
| `message permanently unstorable` | one batch was permanently rejected, with the run id, the message number and the Postgres error code. |
| `sanitized unstorable bytes out of a worker message` | uzi cleaned something a tool emitted (NUL bytes, broken Unicode) and the run continued. Worth investigating the tool; not worth alarming on. |

The counters behind all of this live in the api process, not in the database. **That makes it one of the reasons the api must run exactly one replica** — with two, neither one sees enough failures to reach the threshold and auto-stop quietly stops working rather than misfiring.

Two costs worth knowing before you read a graph and wonder: a rejected oversize batch now performs one indexed run lookup where it previously touched the database not at all (that is how an oversize loop stays visible at all), and an auto-stopped run's `looping` flag is **erased** when it goes terminal — every terminal path clears run health. The log lines above and this page are the only durable record of why a run carries `stop_kind=auto_stopped`.

Related: [Run health](run-health.md) · [Configuration](configuration.md) · [CLI](cli.md)
