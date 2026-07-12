---
title: Self-improvement
order: 75
audience: user
---

# Self-improvement

Once an admin enables it, uzi periodically reviews its own codebase (plus any
accumulated "improve uzi" recommendations from the [run judge](./judge.md))
and autonomously opens or extends **one** merge request on the connected uzi
repo, picking a single top improvement each cycle — a bug, a feature, or a
whole refactor. It never touches `main`, and a human always merges. Off by
default, admin-only.

## 1. Enable it

Open **Admin → Instance settings → Self-improvement**:

1. Check **Enable the self-improvement job (uses your token)**.
2. Pick the connected repo the job runs against (uzi's own repo).
3. Set the **Interval** as a Go duration (default `48h`, every two days) — how
   often a cycle becomes due.
4. Save.

## Whose token, and what "standing" means

The job runs on **the enabling admin's own Anthropic token** — not a shared
instance token. This is a **standing consent**, not a one-time spend: while
that admin stays logged in (so their vault stays unlocked), the job runs
unattended on its schedule and produces autonomous code changes, cycle after
cycle, until disabled. If the admin's vault is locked at tick time, that cycle
is skipped (with an inbox note) rather than spending nothing silently.

## What happens each cycle

- The engine files (or reuses) a tracking issue on the repo, then starts an
  auto-approved run — no plan-approval gate, but the plan is still recorded
  and viewable on the run page like any run.
- The run's planning prompt gets the accumulated unaddressed "improve uzi"
  recommendations plus the repo itself, and is instructed to pick **one** top
  thing rather than a list.
- Changes land on a **fixed branch** (`uzi/self-improve`). An already-open
  self-improvement MR is extended rather than replaced, so everything from
  every cycle is tested together in one MR.
- The worker installs dependencies (best-effort) and runs the repo's own test
  suites, including their evidence in the MR description — this repo has no CI,
  so the MR carries its own proof. Evidence is **best-effort and conditional**:
  a suite runs (and produces real pass/fail) only when its toolchain is present
  in the worker, which for the compiled toolchains means **the connected uzi
  repo's tool profile must provision them**. To get real evidence for every
  suite, add **`go`** and **`nodejs`** to that repo's tool profile (Repos →
  the uzi repo → tool profile); a suite whose toolchain is not provisioned, or
  whose dependencies did not install, is reported **skipped**, and the MR states
  plainly that **skipped is not passed** so a reviewer never mistakes an unrun
  suite for a green one.
- If the change touches a guard-critical path (guardrails, auth, secret/vault
  code, worker token handling, compose secret wiring), the MR description
  flags it loudly for extra-careful review.

## The bot never merges

All four guardrail layers that protect every other run — the bot's
Developer-only GitLab role, the protected `main` branch, the SDK's own
deny-hook, and the disabled `.claude/` settings load — apply here exactly as
they do to any other run. A self-improvement MR is never auto-merged: a
human always reviews and merges it, same as any other run's MR.

## A note on the test evidence and trust

To gather the MR's test evidence, the worker runs the self-improvement change's
**own** test code — the test files the agent just wrote, `package.json`
scripts, `vite`/`tsc`/`go test`. That code runs in a scrubbed, credential-free
environment: it cannot read the worker's forge token, its API URL, or the join
token from its process environment, and `npm ci` runs with `--ignore-scripts`
to remove the easiest code-execution path in. One residual remains for the MVP:
the checks run under the same OS user as the worker, and the join-token file is
readable at a fixed path by that user, so a hostile self-improvement change
could in principle read it. The blast radius is bounded — that token grants the
bot's Developer-role GitLab access (which cannot merge protected `main`) plus
the enabling admin's own Anthropic token (which the run already uses). The
structural fix — running the agent under a distinct OS user from the worker —
is planned for the remote-worker phase. Until then, review a self-improvement
MR the way you would any change from an autonomous author before merging.

## Inspecting a cycle

Open the run from **Notifications** (a "Self-improvement run started" entry
lands there) or from the tracking issue's board card. The run page shows the
plan, the full trace, and — once it finishes — the MR link.

## Recommendations feed it, but aren't required

Rows the [run judge](./judge.md) tags "improve uzi" accumulate as backlog for
this job; once a cycle picks one up, it's marked addressed. With zero
recommendations, the job still runs — it reviews the codebase directly.
Recommendation text is judge/worker output over untrusted run traces, so the
planning prompt treats it as data to weigh, never as instructions.

## Break-glass

To stop a run cycle from happening again, uncheck **Enable the
self-improvement job** and save — the next tick sees the feature off and does
nothing. To stop an in-flight or already-opened MR, close it directly in
GitLab (the bot has no merge rights, so closing is the same one-person action
as rejecting any other change).

## Good to know

- Only one self-improvement cycle runs at a time; if one is still active, the
  next tick skips (with a notification) and retries later.
- A tick also skips, with a notification, if the target repo becomes
  disconnected or is no longer owned by the enabling admin.
- Hostile or low-value recommendations can't become instructions (they're
  validated, scrubbed, and framed as untrusted data), but a bad one can still
  waste a cycle's pick — admins should skim the backlog occasionally.
