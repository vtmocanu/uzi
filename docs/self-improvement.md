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
instance token. If that admin holds several named tokens, the job follows the
**judge-lane** token (**Settings → Run judge → Token the judge spends**), not
the binding of whichever worker picks the run up: self-improvement is uzi
reviewing and improving itself, so it bills alongside the retrospectives
rather than alongside your product work. Left unset, it is your default token.
See [Anthropic tokens](./anthropic-token.md).

This is a **standing consent**, not a one-time spend: while
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
- For a self-improvement run specifically, the picker is also shown a list of
  work already in flight on the connected repo — every other active run,
  keyed on run **status** rather than on a branch, so a run that hasn't pushed
  a branch or opened an MR yet still shows up — and is directed to avoid
  picking an improvement whose fix overlaps with it. This is **advisory, not
  enforced**: the picker is an LLM weighing a rendered list, and while it
  reduces duplicate picks, it does not guarantee against one. The in-flight
  list is assembled fresh at claim time, so the check reaches every worker
  immediately; the prompt code that *renders* the list ships with the worker
  image, so it only takes effect for newly provisioned workers until the
  fleet rolls — an older worker simply ignores the extra data, no breakage,
  just no benefit yet.
- Changes land on a **fixed branch** (`uzi/self-improve`). An already-open
  self-improvement MR is extended rather than replaced, so everything from
  every cycle is tested together in one MR.
- The worker installs dependencies (best-effort) and runs the repo's own test
  suites, including their evidence in the MR description; the MR also gets uzi's
  real CI pipeline (validate/test/build), so a reviewer sees both the worker's
  in-MR proof and CI's independent verdict. Evidence is **best-effort and
  conditional**:
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
to remove the easiest code-execution path in. It also runs under a distinct,
cap-less OS user (`runner`) from the credential-holding worker (PRD #51), so the
join-token file — owned `0400` by the worker uid — is unreadable to it: a hostile
self-improvement change's test code cannot read the worker's credentials at all.
(A restricted-PodSecurity single-uid start has no such split; that is PRD #58's
own accepted posture, with its cross-container close mapped in
[proc-hardening.md](proc-hardening.md).) Even so, review a self-improvement MR
the way you would any change from an autonomous author before merging.

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
