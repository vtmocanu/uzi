# PRD #308 — Schedule fire outcomes: surface why a fire started nothing (UI + CLI)

**Issue**: [#308](https://gitlab.example.com/vtmocanu/uzi/-/issues/308)
**Mock**: [`prds/mockups/308-schedule-fire-outcome-mock.html`](mockups/308-schedule-fire-outcome-mock.html)
**Priority**: Medium
**Status**: Done

## Problem

A schedule can fire on time and still start **zero** runs. Every candidate is skipped for a benign reason the run-creation gate returns — `ErrNoPRDLink`, `ErrNotPRDIssue`, an active-run dedup, `ErrDescriptionTooLarge` — and the scheduler treats each as a benign skip: it logs `scheduler: issue fire skipped` (`api/internal/schedsvc/scheduler.go`, `createIssueRun` ~line 393) and advances the schedule anyway. The reason exists **only** in the api pod's slog. Nothing is persisted, and no client can read it.

So the Schedules row advances `Last run` to a fresh timestamp and looks healthy, while the schedule has been inert for days. The owner has no way to tell a working sweep from a silently-broken one without kube access to the pod logs.

**The live case that prompted this.** The `bug` label sweep (`max_issues: 1`, auto-approve, fires nightly) has started nothing since it was created. `max_issues: 1` means each fire attempts only the single oldest open `bug` issue — **#96**, labeled `bug` only. `bug` is a run-eligible label, so a human can start #96 from the board (the PRD #196 link waiver), but a timer-fired sweep is explicitly denied that waiver (`workersvc/service.go` ~3764-3774) and #96 has neither a `prds/*.md` link nor the `PRDLESS` label. Result: `ErrNoPRDLink` → benign skip → the fire starts nothing and never reaches the newer `PRDLESS`-labelled bug issues behind it. The schedule looks fine the whole time.

## Goals

1. Persist a **per-fire outcome** on each schedule: how many candidates matched, which runs started (issue↔run, with the issue title), and which candidates were skipped and **why** (typed reasons, not free text).
2. Surface it in the **Schedules page**: an enriched `Last run` cell that distinguishes "started work" from "started nothing", plus an expandable **Last fire** panel with the per-issue breakdown and an actionable hint.
3. Surface it in the **CLI**: a `last_fire` block on `uzi schedule get`, and a per-candidate summary from `uzi schedule run-now`.
4. Keep it a byproduct of firing — the scheduler already reaches every reason; it stops discarding them.

## Non-goals

- **Full fire history.** Only the **last** fire is persisted per schedule. A `schedule_fires` history table is a plausible future extension, explicitly out of scope here.
- **Changing gate behaviour.** No eligibility, PRDLESS, or link-waiver rule changes — this PRD only makes the existing outcome observable. (The `bug`-sweep fix is operational: raise `max_issues` or add `PRDLESS`; this PRD is what would have made that diagnosable in seconds.)
- **New notifications.** Parked schedules already notify (`park` → notifysvc). A "fired but started nothing" alert is a possible follow-up, not part of this PRD.

## User journey

1. An owner opens **Schedules**. The `bug` sweep row shows `0 started · 1 skipped` in amber instead of a bare timestamp — it reads as a problem at a glance.
2. They expand **Last fire**: `matched 1 · started 0 · skipped 1`, then `#96 — no PRD link`, and (because the cap truncated the candidate set) the hint that `max issues` is 1 so newer eligible issues were not reached, with what to do about it.
3. A healthy sweep row shows `3 started` in green; expanding it lists the three runs it started, each row pairing the issue (and its title) with the run it produced.
4. An agent driving uzi headless reads the same breakdown from `uzi schedule get --json` (`.last_fire`) or triggers a fire with `uzi schedule run-now` and gets the per-candidate result — no pod access.

## Technical scope

### Skip-reason taxonomy

A closed enum, declared **once** in Go (`schedsvc`) as the authoritative artifact (see the drift guard below). Four reasons are derived from the run-creation gate; a fifth accounts for a per-candidate **transient failure** so the tally always balances:

| Reason | Where it arises |
|---|---|
| `no_prd_link` | `workersvc.ErrNoPRDLink` from the seam |
| `not_eligible` | `workersvc.ErrNotPRDIssue` from the seam |
| `already_running` | **synthesized at the active-run pre-check** (`HasActiveRunForIssue`/`HasActiveRunForSchedule` true), and also from the seam's `ErrActiveRunExists`/`ErrActivePromptExists` race |
| `description_too_large` | `workersvc.ErrDescriptionTooLarge` from the seam |
| `fetch_failed` | a per-candidate **transient** error inside a sweep fan-out that today is logged-and-`continue`d (`HasActiveRunForIssue` DB error, forge `GetIssue` error, an unexpected mid-sweep `ErrRepoNotFound`) — recorded so the candidate is not silently dropped |

Note two framings the first draft got wrong, corrected here: `already_running` is **not** a returned sentinel on its dominant path — it is a pre-check bool, so it must be *recorded at the pre-check site*, not "stop-swallowing a sentinel"; and the sweep's per-candidate error branches are genuine errors, not benign sentinels, hence `fetch_failed`. `matched == started + skipped` is an **invariant** every fire must satisfy (see Decisions).

### The outcome shape

`fireOne` and the `fire*`/`createIssueRun` helpers today return `([]uuid.UUID, error)` and discard per-candidate detail. They return a structured outcome rich enough to render the mock (issue title, and the issue↔run pairing), and shaped to fit **all three** targets including issue-less `prompt`:

```
FireOutcome {
  Matched  int              // candidates considered THIS fire (see per-target definition)
  Capped   bool             // sweep only: the max_issues cap truncated the candidate set
  Started  []Started        // { IssueIID *int64; RunID uuid.UUID; Title string }
  Skips    []Skip           // { IssueIID *int64; Title string; Reason string }
}
```

- `IssueIID` is a **pointer** so a `prompt` schedule (no issue) is representable; its `Started`/`Skip` carry a nil iid and the prompt title.
- **`Matched` per target**: sweep = the capped candidate count (`len(candidates)`); issue = 1 if the pinned issue was considered, else 0; prompt = 1 if a fire was attempted, else 0.
- **`Capped`** is the only way the "newer issues not reached" hint (Goal 2) can be *factual* rather than a guess: `Matched` is already the capped set, so it cannot reveal how many sit behind it. The sweep candidate query gains a cheap truncation probe (`LIMIT max_issues + 1`, or a sibling `COUNT`) to set `Capped`; the web hint then keys on `Capped && skipped > 0 && started == 0`.

### Persistence

A new migration adds `run_schedules.last_fire jsonb` (nullable; NULL = never fired). `advance()` writes the serialized `FireOutcome` into it in the same `AdvanceSchedule` UPDATE that already sets `last_fired_at`/`next_fire_at`/`status` (`store/queries/schedules.sql`), so it is written exactly on the scheduled-fire path. A single JSONB column (not a history table) is the minimal fit for last-fire-only.

**`last_fire` is written only on the success/benign advance path**, by design and stated so an owner is not surprised: a **transient** fire error skips the write to retry next tick (no advance), and a **park** (`ErrRepoNotFound`/`ErrBadConfig`) routes through `SetRunScheduleStatus`, not `AdvanceSchedule` — so a parked schedule shows its **prior** `last_fire` (or none), never the fire that parked it. Related asymmetry worth one line in the docs: the same forge/DB error **retries** on an issue target (transient, no record) but is bucketed as `fetch_failed` on a sweep target.

### `RunNow` does not persist

Per its contract `RunNow` must not disturb the cadence; it calls `fireOne` directly and never reaches `advance()`, so a manual fire returns its `FireOutcome` in the HTTP/CLI response but does **not** overwrite `last_fire` (which reflects the last *scheduled* fire). Verified: the handler fires `RunNow` "WITHOUT advancing" and `last_fire` lives only in `advance()`.

### API, Web, CLI, drift guard

- **API (`apitypes`)** — `ScheduleDTO` gains `LastFire *LastFire`; `RunNowResponse` gains the `matched`/`capped`/`started`/`skips` fields alongside its existing `created`/`run_ids` (cleanly additive to `{Created, RunIDs}`).
- **Web (`web/`)** — `Schedules.tsx` enriches the `Last run` cell (outcome badge + disclosure) and adds the expandable Last-fire panel; `lib/api.ts` gains the `LastFire`/reason types; **`web/src/mocks/mockApi.ts` gains a `last_fire` fixture** (mockApi already models schedules) so `VITE_UZI_MOCK=1` demos the new surface instead of rendering it empty.
- **CLI (`api/cmd/uzi/schedule.go`)** — `get` prints the `last_fire` summary (human + `--json`); `run-now` prints the per-candidate breakdown instead of only a count. `api/internal/uzicli/skill/SKILL.md` documents both (per the CLAUDE.md "new functionality ⇒ CLI check" convention).
- **Reason-enum drift guard** — a TS type union alone is hollow: it cannot catch a reason added on the Go side (the exact one-directional failure `web/src/lib/runCredential.test.ts` avoids by parsing the migration CHECK). Since these reasons are a JSONB free string with no CHECK to parse, the enum is declared in one parseable Go artifact and pinned by a **cross-language contract test** that reddens when Go gains a reason with no TS counterpart — mirroring the run-usage contract fixture precedent (a repo-root fixture read across the module boundary; `-count=1` already mandated for exactly this).

## Milestones

- [x] **M1 — Scheduler produces the outcome.** `fireOne`/`fire*`/`createIssueRun` return the `FireOutcome` above; the reason enum is declared as the authoritative Go artifact. `already_running` is recorded at **both** the pre-check and the seam race; sweep per-candidate transient errors become `fetch_failed`; `Matched` is defined per target (sweep/issue/prompt) and `Capped` is set from the truncation probe. Unit + live-DB tests assert each reason maps from its source, each target populates the shape, and the `matched == started + skipped` invariant holds (including the `fetch_failed` path).
- [x] **M2 — Persist last-fire summary.** Migration adds `run_schedules.last_fire jsonb`; `AdvanceSchedule` writes it; `advance()` threads the outcome through only on the success/benign path. sqlc regenerated (no-op in CI). Live-DB tests: a scheduled fire persists the expected `last_fire`; a never-fired schedule reads NULL; a parked/transient fire does **not** overwrite it.
- [x] **M3 — API surface + drift guard.** `ScheduleDTO.last_fire`, the widened `RunNowResponse`, and the Go↔TS reason contract test. Handler tests for both DTOs and the `RunNow`-does-not-persist invariant. *(Landed: `apitypes.LastFire`/`RunNowResponse` widened; `scheduleDTO` unmarshals the `last_fire` jsonb; pure `runNowResponse` helper; handler tests `TestScheduleDTOLastFire`/`TestRunNowResponse`; Go enum test `TestSkipReasonEnumIsHonest`; cross-language guard `web/src/lib/scheduleSkipReasons.test.ts`.)*
- [x] **M4 — Web UI** (depends on M3). Enriched `Last run` cell + expandable Last-fire panel per the mock, including the issue↔run pairing and the `Capped`-derived actionable hint. `api.ts` types; `mockApi.ts` `last_fire` fixture. Component tests for every state: `started` / `started-nothing` / `empty-label (matched 0)` / `never-fired` / `parked (prior or no last_fire)`, plus an explicit test of the hint's `Capped && skipped > 0` derivation. *(Landed: `Schedules.tsx` outcome badge + expandable `LastFireDetail` panel; exhaustive `scheduleSkipReasons.ts` label map; three demo `mockApi.ts` fixtures; component + hint tests; web-ux live-DOM reviewed.)*
- [x] **M5 — CLI** (depends on M3). `uzi schedule get` last-fire block and `uzi schedule run-now` per-candidate summary; `SKILL.md`; CLI tests. *(Landed: `renderLastFire`/`renderRunNow` in `cmd/uzi/schedule.go` with a reason→label map + capped hint; `SKILL.md` + `docs/cli.md` updated; `--json` unchanged; CLI tests incl. the started-nothing flagship case.)*
- [x] **M6 — Docs, specs, gates.** `docs/scheduling.md` "Fire outcomes" section; `specs/ai.md` Decisions 1-5 recorded (specs contract); the mock referenced from the PRD. All gates green: `task gate:api`, `gate:web`, `gate:repo`, and `agent`/`controller` untouched. *(Landed: `docs/scheduling.md` "Fire outcomes"; `specs/ai.md` §523-527.)*

## Success criteria

1. A schedule that fires and starts nothing is visibly distinguishable — in the web list, in `uzi schedule get`, and in `uzi schedule run-now` — from one that started runs, without reading pod logs.
2. Each skip carries a typed reason drawn from the authoritative Go enum; no free-text reasons on the wire, and a Go-side reason with no TS counterpart reddens a test.
3. `matched == started + skipped` for every persisted and returned outcome (transient per-candidate failures are `fetch_failed`, not silently dropped).
4. The persisted `last_fire` reflects the last **scheduled** fire; a `run-now` fire reports its outcome without mutating `last_fire`; a parked/transient fire never overwrites it.
5. No change to which candidates fire — behaviour parity with today, verified by the existing scheduler suite staying green.

## Risks & mitigations

- **Scheduler signature churn** touches every `fire*` path and their tests. Mitigation: introduce `FireOutcome` additively (it still yields the run ids callers read), land M1 behind the existing tests before any persistence.
- **The outcome shape must serialize before M2/M3/M4 consume it** — the title/issue↔run/pointer-iid decisions above are fixed in M1 precisely so the later milestones serialize a settled shape.
- **Migration number drift** — the draft `last_fire` migration is renumbered to the next free head above the live migrations dir (currently `00119`) at landing; goose is strict (see CLAUDE.md Conventions).
- **Hollow drift guard** — a TS-only union does not catch Go-side additions; the cross-language contract test (M3) is the real guard, not the union.
- **`RunNow` persistence temptation** — writing `last_fire` from a manual fire would break the "does not disturb cadence" contract; held as Decision 3 and covered by an M3 test.

## Dependencies

- Builds directly on PRD #241 (schedules) and #274 (sweep cap / guidance). No new services, no forge-driver changes, no new trust boundary.

## Decisions

1. **Last fire only, JSONB column** — not a history table. Minimal surface for the question being asked; history is a clean future extension.
2. **Typed reasons from one Go enum** — closed set mapped from existing sentinels (plus `fetch_failed`), pinned cross-language so UI and CLI branch on them and the untrusted-free-text rule does not apply.
3. **`run-now` returns but does not persist** — preserves the PRD #241 `RunNow` contract that a manual fire never disturbs the cadence.
4. **`matched == started + skipped` is an invariant** — every candidate lands in exactly one bucket; per-candidate transient sweep failures are `fetch_failed` rather than dropped, so the tally in the UI always adds up. `matched: 0` (empty label set) is a legitimate, observable outcome, not an error.
5. **`last_fire` records the scheduled fire only** — transient-fail (retry) and park paths do not write it, so a parked row shows the prior fire or none.

## Parallelization

| Phase | Milestones | Depends on | Files (rough) |
|---|---|---|---|
| 1 (sequential, Go) | M1 → M2 → M3 | — | `schedsvc/scheduler.go`, `store/queries/schedules.sql` + migration, `apitypes/schedule.go`, `handler/schedules*.go`, reason contract fixture |
| 2 (parallel) | M4 (web), M5 (CLI), M6 (docs/specs) | M3 | `web/src/pages/Schedules.tsx` + `lib/api.ts` + `mocks/mockApi.ts`; `cmd/uzi/schedule.go` + `SKILL.md`; `docs/scheduling.md` + `specs/ai.md` |

M4/M5/M6 touch disjoint trees and can run as parallel agents once M3's DTO shape is fixed.
