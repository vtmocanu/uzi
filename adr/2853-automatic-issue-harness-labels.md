# ADR-2853: Automatic issue harness labels

**Status**: Accepted
**Date**: 2026-10-13
**Issue**: [#2853](https://github.com/vtmocanu/uzi/issues/2853)

## Context

Automatic issue dispatch needs an explicit harness choice for work belonging
to Claude or Codex, independent of an owner's default. Existing schedules
can pin a harness or an Anthropic token; credential disable already has
one-time hold and partial-sweep stop semantics. Label selection must respect
those stored choices without making one candidate's credentials a
schedule-wide choice.

## Decision

Use exact forge labels `harness::codex` and `harness::claude` on automatic
issue starts: every issue schedule selector (bug, Planned, on-deck, assigned
and custom), recurring/once fires, schedule run-now, and the autopilot poller.
Selection reads the fresh `issueinput.Capture` for the target, not the
cached candidate. Manual web/CLI run creation and chat `start_run` keep
their existing selection. Prompt and self-improve schedules do not consume
these labels.

Within an issue fire, validate the `harness::*` namespace before comparing
the schedule pin:

- No harness label preserves the schedule pin, otherwise existing D11
  automatic resolution.
- One recognized value without a pin explicitly selects that harness,
  with no credential fallback to the other. Repeated copies of that same
  value are allowed.
- A matching pin is allowed; a differing pin refuses with
  `harness_label_conflict`.
- Both recognized values, or an unknown `harness::*` value, refuse with
  `harness_label_invalid`, even with a pin.

Selection is ephemeral, not a stored pin. Label-selected Codex with a stored
Anthropic override refuses with `codex_override_conflict`: do not simulate
a pin to gain the actual-pin exemption. Separate harness model defaults and
the D6 stored-model compatibility fallback are unchanged.

## Credential-disable boundaries

The existing once-schedule
`ScheduleCredentialDisabled(stored override, scheduleHarness)` preflight
runs before forge selection. It checks stored choices, not a label-selected
candidate; it can hold before labels are fetched. The authoritative stored
override guard preserves disabled precedence when creation reaches it.

For a label-selected **unpinned sweep**, disabled harness credentials give
a per-candidate `credential_disabled` skip; missing/unusable selected
credentials give `no_usable_credential`. Continue within the existing
scan/start caps, without cross-harness fallback. Disabled actual schedule
pins or stored overrides retain the old once hold before any start and
stop-and-advance after a partial start.

Resolution may fail before the stored-override insert check when the sole
Claude token is also the override and is disabled after preflight. After
that refusal the scheduler calls the strict stored-token-only
`CheckStoredCredentialOverrideEnabled` check. A disabled token restores the
stored-token hold/stop classification. An enabled or deleted token preserves the
original per-candidate label refusal. An unreadable token lookup returns a
wrapped transient error: a once sweep with no starts stays due without a hold
or history write; after a partial start it records `fetch_failed`, stops scanning,
and advances. Recurring sweeps record `fetch_failed` and continue within the
existing caps. Run-now follows the same zero/partial outcomes without persisting
cadence or fire history. The tolerant once preflight remains unchanged.

A once **single issue** whose label selects a disabled harness holds like a
pin and resumes on re-enable. Missing/unusable credentials gain no new
disable hold: explicit `no_credential_for_harness` remains a hard refusal
retried each tick, implicit `no_usable_credential` remains a benign
advancing skip. Recurring credential semantics remain unchanged.
Invalid/conflicting label skips advance tick-driven once schedules, and
sweeps continue. Run-now reports its outcome without persisting cadence,
last-fire or recent-fire changes.

## Poller consent and retry boundary

Owner opt-in and declared forge-identity matching precede fresh capture.
With no harness label, the legacy `HasAnthropicToken` presence gate remains;
actual selection later follows D11. With a valid label,
`ResolveHarnessForUser` requires a usable credential for the explicit
harness, without fallback. A consenting Codex-only owner can start a
Codex-labelled issue without Anthropic.

Invalid-label and selected-credential refusals log the static reason on the
existing `poller: autopilot create run` surface and leave the event
unrecorded for later polling retries. They introduce no issue comment;
the legacy no-eligible-user comment path is unchanged.

## Enforcement boundaries

The label parser and pin comparison live in
[workersvc/issue_harness.go](../api/internal/workersvc/issue_harness.go).
[schedsvc/scheduler.go](../api/internal/schedsvc/scheduler.go) feeds fresh
captures through `createIssueRun` for both issue and sweep targets and
keeps `RunNow` outside persisted advancement.
[credential_override.go](../api/internal/workersvc/credential_override.go)
owns the stored-choice precheck; the issue create transaction's guard in
[workersvc/service.go](../api/internal/workersvc/service.go) checks disabled
stored overrides before harness incompatibility when reached.
[poller/autopilot.go](../api/internal/poller/autopilot.go) owns consent,
fresh capture, the labelled credential check and unrecorded refusal retries.
[harness_resolver.go](../api/internal/workersvc/harness_resolver.go) enforces
explicit selection without cross-harness fallback.

## Surfaces and landing

The scheduler's Go and web TypeScript closed skip-reason sets include
`harness_label_invalid` and `harness_label_conflict`. Web human labels
and warning badges and CLI run-now/get/recent-fire labels and repair hints
surface them. The web's generic `no_usable_credential` label is
"no usable credential", covering a selected harness too.

Newly filed issues whose change belongs to one harness carry its matching
label, never both. Runtime does not infer a harness from file paths.

**Maintainer landing step:** create `harness::codex` and
`harness::claude` labels on this repository's forge. Record this step in
the landing PR; the agent does not create external labels. No schema,
settings or migration change is required, and the run lifecycle is unchanged.

See [Scheduling](../docs/scheduling.md#which-harness-a-schedule-runs-on),
[Autopilot](../docs/autopilot.md#which-harness-autopilot-uses) and
[human requirements](../specs/human.md).
