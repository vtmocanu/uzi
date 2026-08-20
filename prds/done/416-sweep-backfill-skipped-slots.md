# PRD #416: Sweep backfill for skipped slots

**Issue**: #416
**Priority**: Medium
**Scope**: `api/internal/schedsvc` (the shared sweep path) + a small `web/` label change + tests + docs. No SQL/sqlc change, no wire-contract change, no new migration.

## Problem

A sweep schedule fires on the oldest N issues carrying its label (N = `max_issues`) and starts a run for each. When a matched candidate cannot start, it is recorded as a typed skip and the slot is simply lost: the fire starts fewer than `max_issues`.

Measured on the live `bug` sweep (schedule `848d49b2`, fired 2026-08-19 23:00:34Z, cap 3):

- matched 3: `#189`, `#190`, `#198`
- started 2: `#189` (run `1f22b72e`), `#198` (run `a929de3c`)
- skipped 1: `#190`, reason `no_prd_link`

The third slot did nothing. Because candidates are taken oldest-first and eligibility is only known after fetching each issue, a run of ineligible issues at the head of the backlog (for example an old bug with no PRD link and no `PRDLESS` waiver) can under-fill every fire indefinitely, while newer eligible issues wait.

This affects every sweep. Today two exist (`bug`, enabled; `Night-Shift`, currently disabled) and more may be added; the fix must be in the shared sweep path, not per-schedule.

Note `api/internal/uzicli/skill/SKILL.md:396` already documents the cap as "how many issues one `--sweep` fire **starts**", which is not what the code does today (it caps candidates *considered*). This PRD makes the code match that documented intent.

## Solution

Count the cap against runs **started**, not candidates matched. Keep the flag unchanged: a skipped candidate is still recorded as a typed skip (that record is the flag, surfaced in the schedules UI and persisted in `last_fire`). Then walk oldest-first past the skip and start the next eligible candidate, until `max_issues` runs have started or a bounded scan window is exhausted.

Key property: the change preserves the existing invariant `Matched == len(Started) + len(Skips)` by defining `Matched` as the number of candidates the fire actually **examined** (attempted). A slot refilled by walking one issue further simply means one more examined candidate that landed in `Started`.

## Background: current behaviour (code facts, for an offline implementer)

All file paths are in-repo; no external lookup is needed.

**The sweep fire** — `api/internal/schedsvc/scheduler.go`, `fireSweep` (around lines 280-354):

1. Resolves repo + forge and the label selector (`resolveSweepLabels`; an empty/NULL selector defaults to the `PRD` label, never an empty jsonb array).
2. `candidates, _ := store.ListSweepCandidateIssues({RepoID, Labels, MaxIssues: sched.MaxIssues})`. The query (`api/internal/store/queries/schedules.sql:132`, generated into `schedules.sql.go`) is:
   ```sql
   SELECT forge_issue_iid, author FROM issues
   WHERE repo_id = $1 AND state = 'opened' AND labels @> $2::jsonb
   ORDER BY forge_issue_iid ASC
   LIMIT $3
   ```
   `LIMIT` is `sched.MaxIssues` (a `pgtype.Int4`); sqlc renders a NULL/invalid narg as an unlimited `LIMIT`, so a NULL cap fetches every matching open issue. The query does **not** filter on PRD-link eligibility — only the label and open state. Eligibility is only determined later, per candidate.
3. `capped` is computed only when `MaxIssues.Valid`: `capped = CountSweepCandidateIssues(...) > len(candidates)`.
4. `out := FireOutcome{Matched: len(candidates), Capped: capped}`, then for each candidate, oldest-first:
   - `HasActiveRunForIssue` pre-check; if true, a `SkipAlreadyRunning`.
   - `f.GetIssue(...)` (one forge call per candidate) to get title/description/labels; on error a `SkipFetchFailed`.
   - `createIssueRun(...)` returns a single-issue `FireOutcome` (one `Started` or one `Skip`); its buckets are folded into `out`.
   - `e.sleep(sweepPacing)` (50ms) between candidates.

So today the number of forge `GetIssue` calls per fire is exactly `len(candidates)` (= `max_issues` when capped), and a skip consumes one of those `max_issues` slots.

**The outcome type** — `api/internal/schedsvc/outcome.go`:
- `FireOutcome{ Matched int; Capped bool; Started []Started; Skips []Skip }`.
- Documented invariant: `Matched == len(Started) + len(Skips)`.

**The skip reasons** — `api/internal/schedsvc/skip_reason.go`: closed set `no_prd_link`, `not_eligible`, `already_running`, `description_too_large`, `fetch_failed`. A cross-language contract test parses this file to keep the TS union in sync. This PRD adds **no** new skip reason, so that contract is untouched.

**Persistence + wire** — `api/internal/schedsvc/last_fire.go` marshals a `FireOutcome` to the `run_schedules.last_fire` jsonb as `{fired_at, matched, capped, started[], skips[]}`. `apitypes.LastFire` (handler), the web `LastFire` type, and the CLI all read these tags. This PRD keeps every tag, so no wire/contract change.

**UI** — `web/src/pages/Schedules.tsx`, `LastFireDetail`:
- A tally row renders `matched` / `started` / `skipped` / `max issues`.
- Started runs render as rows with a run link; skips render as rows with a tone-coded reason badge (`web/src/lib/scheduleSkipReasons.ts`). Both already render every element of `started[]` and `skips[]`, so a fire that starts 3 and flags 1 already displays correctly with no structural change.
- A hint block renders only when `capped && skips.length > 0 && started.length === 0` ("Nothing newer was reached; raise the cap or add `PRDLESS` / a PRD link"). Still valid as the degenerate all-skipped case after this change.

## Design

Change `fireSweep` only (the per-issue `fireIssue`/`firePrompt` paths are unchanged; `Matched=1` there stays correct):

1. **Widen the fetch to a bounded scan window.** When `sched.MaxIssues.Valid`, fetch `max_issues + backfillHeadroom` candidates instead of `max_issues` (compute a new `pgtype.Int4` limit in Go and pass it as the existing `MaxIssues` query param; the SQL is unchanged). When the cap is NULL/invalid, fetch unlimited exactly as today. `backfillHeadroom` is a package constant (proposed default 10) documented beside the code: it bounds the extra forge `GetIssue` calls a single fire will spend walking past ineligible candidates.
2. **Start until filled, then stop.** Iterate the fetched window oldest-first, recording each non-start as a flagged skip exactly as today. After folding each candidate, if `sched.MaxIssues.Valid` and `len(out.Started) >= max_issues`, break. This keeps forge calls minimal when eligibility is dense (best case: examine exactly `max_issues`), and caps them at the scan window in the worst case.
3. **Set `Matched` at the end**, to `len(out.Started) + len(out.Skips)` (the candidates actually examined), so the invariant holds by construction. `Matched` may now exceed `max_issues` (it counts attempts, not the cap).
4. **`Capped`** is computed as today (`capped = CountSweepCandidateIssues(...) > len(fetchedWindow)`), but because the fetch widens from `max_issues` to `max_issues + backfillHeadroom`, its meaning shifts from "more matching issues than `max_issues`" to "more matching issues than the scan window". This is a deliberate, documented shift: `capped` is persisted in `last_fire` and drives the started-nothing hint in both the web (`Schedules.tsx:427`) and the CLI (`schedule.go:794`). In the all-skip degenerate case, for `max_issues < total <= window` the "raise the cap" hint that shows today no longer shows, which is arguably more correct (nothing eligible exists within reach), but it IS a behaviour change, not a no-op.

Consequences, all intended:
- The `bug` sweep example above would start 3: `#189`, `#198`, and `#199` (the next eligible bug, PRD-linked), with `#190` still flagged `no_prd_link`. Tally: examined 4, started 3, skipped 1.
- A wall of more than `backfillHeadroom` ineligible issues at the head under-fills the fire (starts fewer than `max_issues`) and spends at most `max_issues + backfillHeadroom` forge calls; the started-nothing hint covers the fully-degenerate `started == 0` case. This is strictly no worse than today per fire in outcome and bounded in cost.
- **New partial-under-fill mode, acknowledged as a known limitation.** Backfill introduces a case that does not exist today: `started > 0` but `< max_issues` because the scan window was exhausted by ineligible candidates while an eligible issue sits just past the window (for example head = 1 eligible + `backfillHeadroom` ineligible, with an eligible issue one past the window). The started-nothing hint fires only when `started == 0`, so this partial mode is currently invisible. It is left as a limitation for v1 (the skipped issues are still each flagged); a `scan_exhausted` fire-level signal is the natural follow-up that would surface it, tracked as a possible future signal rather than shipped here.
- Oldest-eligible-first ordering is preserved: a stale ineligible issue no longer blocks the run budget, but the oldest issue that *can* run still goes first.

## Milestones

### M1 - Backfill logic in `fireSweep` [no deps]
- [x] Widen the candidate fetch to a bounded scan window (`max_issues + backfillHeadroom`) when the cap is set; unlimited when NULL.
- [x] Iterate oldest-first, starting runs until `max_issues` have started (early break); each non-start recorded as the same typed flagged skip as today.
- [x] Set `Matched = len(Started) + len(Skips)` so the invariant holds; leave the NULL-cap path behaviourally identical to today.
- [x] `backfillHeadroom` is a documented package constant with its cost rationale beside it.

### M2 - Tests [deps: M1]
- [x] Backfill fills the slot past a skip: window `[eligible, no_prd_link, eligible, eligible]`, `max_issues=3` starts 3, flags 1, `Matched=4`, oldest-eligible first.
- [x] Scan bound: a head of `> backfillHeadroom` ineligible issues starts fewer than `max_issues`, examines at most the window, and does not walk the whole backlog. Note two test-fake caveats to design around: the fake `fakeForge.GetIssue` (`scheduler_test.go:201`) takes unnamed args and counts nothing today, so it needs a call counter to assert the examined count; and the fake `ListSweepCandidateIssues` (`scheduler_test.go:60`) runs no SQL and does not apply the LIMIT, so a unit test can only assert the *threaded limit param* (`max_issues + headroom`), not that truncation happens.
- [x] Add one live-DB (`*LiveDB`) test that exercises the real `LIMIT max_issues + headroom` arithmetic against Postgres, since the unit fake cannot (run via `./e2e/run-store-it.sh`).
- [x] `already_running` and `no_prd_link` are both backfilled past.
- [x] NULL cap unchanged (examines all, starts all it can). `TestTickSweepThreadsMaxIssues` (`scheduler_test.go:387`) is not just a number bump: its stated purpose is that `fireSweep` passes `max_issues` *straight into* the query param, which this change inverts (it now threads `max_issues + headroom`); update the test AND its rationale comment.
- [x] The invariant `Matched == started + skipped` holds in every case (the existing `scheduler_test.go:588` `assertBalances` assertion stays green).
- [x] Deterministic only: no timing/wall-clock/sleep assertions.

### M3 - UI + CLI display clarity [deps: M1]
Redefining `Matched` as "examined" (may exceed `max_issues`) leaks into **four** count-display sites, all of which must be relabeled consistently (the wire field name stays `matched`; these are display strings only):
- [x] Web `LastFireDetail` tally: `Schedules.tsx:458` `label="matched"` to "examined".
- [x] Web `LastRunOutcome` collapsed "Last run" cell (always visible, no expand needed): `Schedules.tsx:369` `· matched {fire.matched}` to "examined". The two `matched 0` empty-state badges (`:412`, `:451`) describe the zero-candidate outcome and can keep their wording.
- [x] CLI `uzi schedule get`: `api/cmd/uzi/schedule.go:786` `fired %s · matched %d · started %d · skipped %d` to "examined".
- [x] CLI `uzi schedule run-now`: the `matched/skipped` tally printer around `schedule.go:825` (`Matched %d candidate(s), skipped %d:`) to "examined". The `--max-issues` flag help (`schedule.go:87`) already says "started per fire" and is correct as-is.
- [x] No backfill pill or per-row marker: a refilled slot is just another started run (its position past the skip is enough).
- [x] Update `web/src/pages/Schedules.test.tsx` for the relabels and add a case with started > 0 alongside a flagged skip; confirm the started/skip rows and the started-nothing hint still render.

### M4 - Docs [deps: M1-M3]
- [x] Update the sweep-mechanics docs to state backfill and that it applies to all sweeps: `api/internal/uzicli/skill/SKILL.md` (the `--max-issues` / oldest-first / "Sweep gotcha" section around lines 380-438), `ARCHITECTURE.md`'s sweep description, and the schedule guidance text if it describes cap behaviour.
- [x] Note that `Matched` now means "examined" (may exceed `max_issues`) and that `Capped` now means "more than the scan window".
- [x] `specs/ai.md` design-decision entry for the cap-counts-started change.

### M5 - Gate [deps: M1-M4]
- [x] Run the repo gate for touched components (`task gate:api`, `task gate:web`) and the live-DB store sweep for the new `*LiveDB` test.

## Success criteria

1. A capped sweep whose oldest candidate(s) cannot start still starts up to `max_issues` runs by walking to the next eligible candidate, bounded by the scan window.
2. The skipped candidate is still flagged exactly as before (typed skip, shown in the UI, persisted in `last_fire`); nothing about the flag changes.
3. The change lives in the shared `fireSweep` path and applies to `bug`, `Night-Shift`, and any future sweep with no per-schedule code.
4. `Matched == len(Started) + len(Skips)` holds in every fire; no new skip reason, no wire/contract change, no SQL/sqlc change, no migration.
5. Per-fire forge `GetIssue` calls are bounded by `max_issues + backfillHeadroom`.

## Decision Log

- **Cap counts started, not matched.** Aligns behaviour with the documented intent (`SKILL.md:396`) and removes the slot-waste. Alternative (cap counts candidates, as today) is what we are fixing.
- **Bounded scan window (additive headroom), not unbounded walk.** "Keep going until filled" is unbounded and could scan the entire backlog (one forge call each) on a fire whose head is all ineligible. An additive constant (`max_issues + backfillHeadroom`) bounds per-fire cost predictably; a multiplier was considered and rejected as less predictable for small caps.
- **No new wire field or skip reason.** `Matched` is redefined as "examined" (still `started + skipped`), so the invariant, the `last_fire` json tags, `apitypes.LastFire.Matched`, the web `LastFire` type, and the skip-reason contract test are all untouched at the wire level. The redefinition does, however, change what four count-*display* sites should say (two web, two CLI; see M3) — the value can now exceed `max_issues`, so "matched N" reads wrong and becomes "examined N". This is a relabel, not a data/contract change.
- **`Capped` redefined to "more than the scan window".** Because the fetch widens, `capped`'s meaning shifts (Design point 4). Kept rather than dropped because the started-nothing hint still needs it; the shift is documented in M4 and specs.
- **No SQL change.** The scan window is a computed `LIMIT` value passed to the existing `ListSweepCandidateIssues`; the query string is unchanged, so no `sqlc generate`. (A live-DB test still covers the real LIMIT arithmetic, since the unit fake does not apply it.)
- **Eligibility is not pre-filtered in SQL.** The PRD-link check needs the issue body, not just cached labels, so it stays per-candidate after `GetIssue`. A SQL pre-filter for `PRDLESS`/label-only eligibility is a possible future optimization to cut `GetIssue` on ineligible candidates; out of scope here.
- **`backfillHeadroom` default 10.** A round, generous headroom for realistic backlogs; a package constant so it is trivially tunable and needs no schema/API surface. Not user-configurable in v1.

## Risks and mitigations

- **More forge AND DB calls per fire.** Each examined candidate costs one `HasActiveRunForIssue` DB call (`scheduler.go:317`) and one `f.GetIssue` forge call (`:331`), now bounded by `max_issues + backfillHeadroom` rather than `max_issues`. At the default cap (`max_issues` defaults to 10, SKILL.md:396) with `backfillHeadroom=10` the window is ~20, doubling the per-fire budget; the enabled `bug`/`Night-Shift` sweeps use `max_issues=3`, so their window is 13. The number of runs *started* is still capped at `max_issues`, so there is no new load on the run limiter or worker fleet. Mitigated by the early break (dense eligibility examines exactly `max_issues`), the existing 50ms `sweepPacing`, and the tunable headroom.
- **Tick serialization latency.** Schedules are processed sequentially within a tick (`scheduler.go:172-174`), so a longer sweep delays later schedules in the same tick. Acceptable at nightly cadence and bounded window sizes; worth watching if many sweeps are added.
- **New silent partial-under-fill mode** (`started > 0` but `< max_issues`, window exhausted). Acknowledged as a v1 limitation (Design consequences); each skipped issue is still individually flagged. A `scan_exhausted` signal is the follow-up that would surface it.
- **`Matched`/`Capped` semantic shifts confuse a reader.** Mitigated by the "examined" relabel across all four display sites (M3) and the specs note (M4).
- **A permanently ineligible head still under-fills.** By design: those issues are flagged for a human to fix (add a PRD link or `PRDLESS`). The started-nothing hint points the owner at the fix in the fully-degenerate case. Backfill improves the common case (a *few* ineligible at the head) without hiding a genuinely stuck backlog.
- **Existing tests assert `Matched == len(candidates)` and the threaded param.** Updated in M2 (including `TestTickSweepThreadsMaxIssues`, whose rationale inverts); the invariant assertion (`Matched == started + skipped`) is unaffected and stays as the durable check.

## Out of scope (explicitly)

- Any new skip reason, `last_fire` field, or migration. A `scan_exhausted` fire-level flag to surface the partial-under-fill mode is acknowledged as a follow-up (Risks) but not shipped in v1.
- SQL-side eligibility pre-filtering (possible later optimization to cut `GetIssue` on ineligible candidates).
- Making `backfillHeadroom` user-configurable (per-schedule or global setting).
- Any change to `fireIssue` / `firePrompt`, or to how skips are flagged.
- A backfill pill / per-row "backfilled" marker in the UI (explicitly declined: position past the skip is sufficient).
