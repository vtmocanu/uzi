---
title: Automatic CI fixes
order: 81
audience: user
---

# Automatic CI fixes

With automatic CI fixes on, uzi reacts to a failed pipeline on one of your
own agent merge-request branches by itself: no clicking **Fix CI**, spending
your own Anthropic token. It's the unattended sibling of the manual **Fix
CI** button described in [Configuration](./configuration.md#ci-status-integration-prd-6)
— same `ci_fix` run type, same verification, just started for you. Off by
default, and opt-in per user.

## 1. Opt in

Open **Settings → Automatic CI fixes** and check **Automatically fix my
failed CI pipelines**. Off by default; it only ever spends your own
Anthropic token, and only on branches that trace back to your own agent
runs. Admins can also force-enable or force-disable it for any individual
user from **Admin → Users**, the same pattern as the [run judge](./judge.md).

## What triggers it

On the same poll tick that refreshes a repo's pipeline status, uzi checks
every watched **agent-owned MR branch** (`agent/issue-N`, the branch one of
your issue runs pushed to). When that branch's latest pipeline is failed and
you have the toggle on and an Anthropic token configured, uzi queues the
same `ci_fix` run the **Fix CI** button would — auto-approved, so it skips
the plan-approval step and starts working right away.

`main`, the repo's default branch, and any non-MR ref are never auto-touched
— only a branch that is itself the product of one of your agent runs. And
none of this fires at all unless the pipeline watch itself is on
(`CI_WATCH_MAX_REFS` > 0 — see [Configuration](./configuration.md#ci-status-integration-prd-6)); with the watch off, there are no CI badges and no automatic fixes.

## The loop guard

A persistently red pipeline can't loop forever:

- **A cap.** At most `CI_AUTOFIX_MAX_ATTEMPTS` (2 by default) automatic
  attempts per branch. Reaching it halts.
- **A no-progress check.** If a fix attempt's pipeline fails again with the
  *same* failure signature as the attempt before it, uzi halts early rather
  than spending a second identical attempt.

Either way, uzi posts one comment on the backing issue (worded differently
for "hit the attempt limit" than for "no progress") and lands an in-app
notification, then stops trying automatically — **it does not retry on its
own**. The manual **Fix CI** button is still there as your escape hatch any
time. The attempt counter only resets once the branch's pipeline actually
goes green.

## Code fixes push automatically; CI-config fixes wait for you

A fix that only touches code or tests pushes on its own, like any
auto-approved run. A fix that would edit the CI/pipeline configuration
itself — `.gitlab-ci.yml`, anything under `.gitlab/`, or the project's own
configured CI config path — is different: uzi parks that plan for your
approval instead of pushing it unattended, the same gate a fix you started
manually would hit if you didn't approve it.

As a fail-closed backstop, the worker also refuses to push an
auto-approved fix whose diff touches one of those protected paths (or whose
diff it couldn't compute) — the run fails rather than risk an unattended
change to the pipeline that judges it. A fix you (or an admin) approved by
hand — the manual button, or an approved CI-config plan — pushes normally.

## Notifications

You get an in-app notification when an automatic fix starts, when it halts,
and when a fix lands (its pipeline goes green and the run is verified). The
start and halt notifications also post a comment on the backing
`agent/issue-N` issue. These are in-app and issue-comment only — automatic
CI-fix events don't currently go out as Slack DMs, unlike some other run
notifications.

## Forge support

Validated on GitLab. On Forgejo and GitHub the toggle and loop guard work
the same way, but the extra step of reading the project's own configured CI
config path (used to widen the protected-path guard above) isn't
implemented there yet — those forges have no equivalent per-project setting
today.

## Good to know

- **Never your default branch.** Only a branch an agent run of yours
  created is ever eligible — automatic fixes never touch `main`.
- **It doesn't fight the manual button.** If you click **Fix CI** yourself
  while the automatic path would also have fired, uzi treats the run that's
  already in flight as covering it and doesn't start a second one.
- **Cost.** Every automatic attempt is an ordinary run billed to your own
  Anthropic token, exactly like a run you started by hand.
