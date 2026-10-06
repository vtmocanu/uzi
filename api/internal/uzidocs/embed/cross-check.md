---
title: Cross-check
order: 86
audience: user
---

# Cross-check

Cross-check asks the other model family for a second opinion on a run.
**Plan cross-check** is a required gate on opted-in auto-approved plans.
**Code cross-check** is a planned, separate advisory stage before publication;
it is not available in this release.

## 1. Enable Plan cross-check

In **Settings → Run defaults → Cross-check**, enable **Plan cross-check**.
It is off by default and requires usable Claude and Codex credentials. The
setting applies to new eligible auto-approved runs, not existing runs or
human-gated plans. It covers issue, prompt, self-improvement, CI fix and merge
request rework plans that reach the auto-approval gate. Gateless tasks and
plans supplied at creation are outside this gate. Turning it off affects
future runs only.

## 2. Wait for the checked plan

Required runs wait for a worker advertising `cross_check_v1`. A **Claude
lead's plan is checked on Codex** in a separate, read-only checker run.
An APPROVE of the exact candidate permits implementation after the server
acknowledges the matching plan. Changes requested, blocked, timeout or a
failed check normally force a human plan gate. An unavailable checker,
interrupted attempt, refused candidate/planning diff or submit failure also
requires a human decision. An opted-in **Codex lead** parks with
`plan cross-check: not yet supported for a Codex lead`.

Checker runs use ordinary worker slots; the lead holds its slot while
waiting. The verdict deadline includes queue time. Waiting for the check is
excluded from the lead's wall budget. See [Configuration](./configuration.md#cross-check-settings)
for timeout defaults and bounds.

## 3. Read the evidence and decide

Open the run to see the current gate reason, checked-candidate outcome,
findings, checker-run link, recorded model/effort and reported tokens/cost.
Unknown or inconsistent outcomes show **Outcome unavailable**. Findings use
hardened Markdown with a display cap of 16,384 source characters and 20 items;
omitted text is disclosed. Missing cost shows unavailable; subscription usage
is labelled separately from metered spend. After a human revision,
**Earlier-plan evidence** does not certify the current plan. Approve, request
changes or reject the plan shown at the current gate; the original check stays
as history, without an automatic new checker round.

[CLI](./cli.md#plan-cross-check-evidence) shows the reason and findings;
[Slack](./slack.md#using-it) shows the reason without findings.

## Terminal delivery failures

Two delivery-loss exceptions fail the run instead of parking:
`plan cross-check: preparation receipts irrecoverably lost` and
`plan cross-check: human-presentation ACK unrecoverable`. After three
preparation attempts and an acknowledged forced human gate, unresolved ACKs
also fail with `plan cross-check: preparation ACKs unrecoverable`. These paths
do not retry indefinitely, enter `recovery_wait`, invent receipts or approve.
See the [architecture](../ARCHITECTURE.md#plan-cross-check) for the boundary.
