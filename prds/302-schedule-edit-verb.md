# PRD #302: `uzi schedule edit` verb (CLI patch for schedules)

**GitLab Issue**: [#302](https://gitlab.example.com/vtmocanu/uzi/-/issues/302)
**Status**: M1/M2/M4/M5 landed 2026-08-10; M3 (`--model`) deferred until PRD #300 adds `run_schedules.model` (Decision 4)
**Priority**: Medium
**Related**: PRD #241 (run schedules — the `PATCH /api/schedules/{id}` endpoint this verb finally exposes in full). PRD #274 (`MaxIssues`/`Guidance` — the nullable config fields whose replace-semantics dictate this verb's fetch-merge-send design). PRD #300 (per-schedule model — supplies the `model` field this verb's `--model` flag edits; the two are otherwise independent, see Decision 4). This verb is the general form of the CLI-parity gap #300's fable review surfaced.

## Problem

The uzi API already exposes a full schedule patch: `PATCH /api/schedules/{id}`
(`api/internal/handler/schedules.go:232` `PatchSchedule`, merging via
`mergeSchedule` at `:452`). The web edit modal drives it across every mutable
field. **The CLI reaches only the enabled-only slice of that same endpoint** —
`uzi schedule pause`/`resume` PATCH just `enabled` (the `onlyEnabled`
short-circuit, `schedules.go:441`). There is no way from the CLI to change a
schedule's cron, timezone, prompt, labels, guidance, max-issues, or (once PRD
#300 lands) model.

The only workaround is **delete + recreate**, which has real costs, all hit
live in this feature's own design session:

- **Id churn.** Every recreate mints a new schedule id, breaking anything that
  references the old one (a third id inside one short session, in the observed
  case).
- **The enabled-on-recreate footgun.** `schedule create` defaults
  `enabled:true`, so there is a live window between recreate and a follow-up
  `pause`. With an imminent next-fire, that window fires a real run.
- **Lost continuity** — `created_at`, `last_fired_at`, run-history linkage.

This is a textbook CLI-parity gap: the server capability exists, the web uses
it, the CLI cannot.

## Solution Overview

Add `uzi schedule edit <schedule-id>` — a verb that patches a schedule's mutable
fields through the existing endpoint, with **no API or DB change**.

The one design constraint that shapes everything: the config PATCH uses
**replace** semantics. `mergeSchedule` (`schedules.go:452`) rewrites the whole
config row from the request; a sparse body (e.g. only `cron_expr`) sends the
other config fields as their zero/null and **clears** them. The server does this
deliberately — Go's `encoding/json` cannot distinguish an absent `*string` from
an explicit null, so seed-and-keep is impossible and the only sparse PATCH
(`enabled`-only) is short-circuited before the merge (`schedules.go:441`,
`514-529`).

Therefore `edit` **fetches the current schedule, overlays the flags the caller
set, and sends the complete config** — so an unset flag keeps its stored value
and only an explicit clear affordance zeros a nullable field. This is a
client-side merge that compensates for the server's intentional replace.

## Milestones

- [x] **M1 — The `edit` verb (fetch-merge-send).** `uzi schedule edit <id>`
  reads the current schedule (`GET /api/schedules/{id}`), overlays the flags the
  caller passed, and PATCHes the full config. Flags mirror `create`'s mutable set:
  `--cron` / `--at`, `--tz`, `--prompt`, `--label` (repeatable), `--auto-approve[=false]`,
  `--wait-on-limit[=false]`, `--guidance`, `--max-issues`. An unpassed flag keeps
  the stored value. `--json` returns the updated `ScheduleDTO`.

- [x] **M2 — Clear affordances for the nullable fields.** Explicit clears
  distinct from "omit = keep", mirroring the API's tri-state: `--clear-guidance`,
  `--clear-max-issues` (and `--clear-model`, gated with M3). Passing both the set
  and clear form of one field is a usage error (exit 2).

- [ ] **M3 — `--model` (gated on PRD #300).** Once `run_schedules.model` exists
  (PRD #300 M1), `edit` gains `--model` / `--clear-model`. Until then the flag is
  absent; the verb ships its general form independently of #300. (Both PRDs carry
  the `night` label; #300 is the older issue, so the nightly sweep picks it up
  first — but M3 must not assume that ordering and should degrade cleanly if the
  field is not yet present.)

- [x] **M4 — Tests.** Unit: flag → request mapping; the survive-untouched
  property (edit one field, assert the other config fields are unchanged in the
  sent body — the core guarantee); keep-vs-clear for each nullable field; the
  set+clear conflict. Integration: a create → edit → get round-trip that proves a
  single-field edit does not clear the others against a live server.

- [x] **M5 — Docs.** The embedded CLI skill source
  (`api/internal/uzicli/skill/SKILL.md`) documents the `edit` verb, its
  fetch-merge-send behavior, and the clear affordances (edit the embedded source,
  not the installed copy — it is overwritten on every `uzi` command). Command
  help text added.

## Decision Log

1. **Client-side fetch-merge-send, not a new server sparse-merge.** The verb
   compensates for the server's replace semantics by reading the current config
   and re-sending it with overlays. Teaching the server to sparse-merge config
   fields is rejected: it collides with the documented reason the config PATCH
   replaces (`schedules.go:514-529` — JSON absent/null indistinguishability) and
   would change semantics for the web client too. The compensation belongs in the
   one new caller.

2. **Explicit clear affordances for nullable fields.** `--clear-guidance` /
   `--clear-max-issues` / `--clear-model` zero a field; omitting the flag keeps
   it. This is the CLI expression of the API's "present, even to clear" tri-state,
   and it is unavoidable given fetch-merge-send (without an explicit clear there
   is no way to distinguish "leave guidance" from "remove guidance").

3. **No target-kind change.** `edit` stays within a schedule's target
   (prompt/issue/sweep); switching kinds intersects timing/label validation, is
   rare, and stays a delete + recreate. Keeps the verb's validation surface small.

4. **`--model` is gated on PRD #300, the verb is not.** The general edit verb
   (cron/tz/prompt/labels/guidance/max-issues) depends on nothing new and ships
   independently; `--model` is added when #300's `run_schedules.model` field
   exists. The two PRDs are otherwise decoupled.

5. **No API or DB change.** This is purely `api/cmd/uzi/schedule.go` plus the
   embedded SKILL.md. The endpoint, validation, and merge already exist and are
   exercised by the web client.

## Risks & Mitigations

- **Replace-semantics data loss** — the entire reason for the fetch-merge-send
  design. A naive `edit --cron X` that sent only `cron_expr` would clear
  guidance/max-issues/model. Mitigated by M1's read-then-send and M4's
  survive-untouched test, which is the load-bearing assertion of this PRD.

- **CLI/server version skew drops fields on re-send.** Fetch-merge-send round-trips
  the fetched config; a CLI older than the server does not know a newer config
  field, so re-serializing a subset would **omit** it and — under replace
  semantics — clear it. Mitigation: preserve the raw fetched object and overlay
  onto it (patch the decoded map, not a re-typed struct), and surface the existing
  CLI-behind-server warning loudly on `edit`. Call this out in M1.

- **Concurrent edits are last-write-wins.** Two edits racing the same schedule:
  the second read-merge-send overwrites the first. Acceptable for a
  single-owner control surface; note it rather than adding optimistic locking.

## Success Criteria

1. `uzi schedule edit <id> --cron '0 4 * * 2'` changes only the cron; the
   schedule's prompt, labels, guidance, max-issues, tz, and enabled state are
   unchanged (verified against the sent body and a follow-up `get`).
2. `--clear-guidance` removes stored guidance; omitting it keeps guidance intact.
3. The schedule id, `created_at`, and run history are preserved across an edit
   (no churn, unlike delete + recreate).
4. Once PRD #300 lands, `uzi schedule edit <id> --model fable` sets the model and
   `--clear-model` returns it to inherit; before #300 the flag is absent.
5. The embedded `SKILL.md` documents the verb and its fetch-merge-send behavior.
