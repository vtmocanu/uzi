# PRD #1116 — Slack DM when a usage-limit-paused run resumes

**Issue**: #1116
**Priority**: Medium
**Status**: Draft
**Base at authoring**: `4c17d4dd34c6f8008ee6354b6982a64e51eb4340` (line anchors below are as of this commit; re-derive symbol-first, lines drift)

## Problem

When a run parks on an Anthropic usage limit (`limit_wait`, PRD #35), the Slack notifier already tells the owner: the run's root DM is edited to `⏸️ Paused · usage limit` and a threaded event is posted under it with the window, the reader-local resume time and, from the second park on, the pause count (`renderThreadBlocks`, `api/internal/slacksvc/notifier_state.go:559-572`; detail in `limitWaitDetail`, `:601`).

When the run **resumes**, nothing is posted. That is not an oversight: it is a ruling written into the function's doc comment (`notifier_state.go:470-495`, property 2: *"The RESUME posts nothing. `queued` falls to the default arm (ok=false) … Without this half, a run that parks five times would produce ten posts and the feature would read as a notification stream"*), pinned by `TestParkThreadsAnEventWhileResumeDoesNot` (`api/internal/slacksvc/notifier_test.go:889`). The root line is edited back to `▶️ Running`, and a Slack **edit raises no notification**, so the owner learns the run is working again only by happening to look.

The owner wants the opposite: **a Slack message when a paused run resumes.** The resume is the moment that matters, because:

- the reset time is not reliably predictable (PRDs #1020/#1114 exist precisely because the 7-day window reopens early), so "paused, resumes at 03:00" cannot stand in for "resumed";
- resuming is when spend restarts and the plan gate, a clarification, or the MR come back into play. Wait-on-limit is **on by default** (`docs/run-limit-wait.md`), and every scheduled job on the maintainer's instance runs `wait_on_limit=true`, so parked-overnight runs are the normal case, not the edge.

## Solution

Post **one `▶️ Resumed` reply into the run's existing DM thread** the first time the run is genuinely running again after a park, carrying how long it waited and the pause count, and dedupe it through the per-run Slack anchor row (`slack_run_messages`) exactly the way the plan gate (`gate_generation`, compare-and-swap in `SetSlackRunGateIf`) and the milestone line (`milestones_notified_completed`) already dedupe theirs.

The old ruling's *bound* survives, its *conclusion* is reversed: a run can post at most `2 × RUN_LIMIT_MAX_WAITS` park/resume lines over its life (default 5 parks, `api/internal/config/config.go:890`), each pair describing one pause episode. The thread reads pause → resume → (maybe) pause → resume, never a stream.

### What the owner sees

In the run's DM thread, under the existing `⏸️ Paused · usage limit` event:

```
▶️ Resumed · usage limit cleared
waited 2h 13m (pause 2) · working Implement the notifier seam · 🔗 <run link>
```

- `waited …` is omitted when the pause start is unknown or implausible (never invented; see D6 and the lost-event risk).
- `(pause N)` appears only from the second park, mirroring `limitWaitDetail`.
- `· working <title>` reuses `inProgressTitle` (`notifier_state.go:445`), escaped like every untrusted field; omitted when no milestone is in progress.
- The root line is edited to `▶️ Running` as it is today (unchanged).

### Mechanism

**Which transitions the notifier sees on a resume.** The sweeper promotes `limit_wait → queued` (`PromoteLimitWaitRuns`, `api/internal/workersvc/sweep.go:149-159`, fanned out through `publishSwept(r.ID, r.Status)`); the run is then claimed (affinity to its old worker) and the worker's first state report lands `running`, broadcast from `SetState` (`api/internal/workersvc/service.go:2163`, `PublishState(runID, run.Status)`). Two things make "this event is a resume" unreadable from the event itself: the notifier's `stateEvent` carries only `{runID, status}` (`notifier.go:199-206`), no previous status; and re-broadcasts of the *current* status are routine (`publishRunState`, `api/internal/workersvc/summaries.go:154`, re-emits `run.Status` whenever a summary persists, and every worker progress report re-emits `running`). So the resume must be read from durable state: the anchor row.

**One new anchor column** on `slack_run_messages` (`api/internal/store/migrations/00044_slack.sql:29`, extended by 00074/00093/00101/00105): `limit_paused_at timestamptz` — when the pending park began, NULL when no park is awaiting its resume line. The value is the run's own `status_since` (migration 00163; stamped `now()` by `SetRunLimitWait`, `api/internal/store/queries/runtime.sql:1379-1384`), which `GetSlackRunContext` (`api/internal/store/queries/slack.sql:109`) must start selecting. It is both the dedupe marker and the pause start.

**Set** on the park: when `handle` (`notifier_state.go:27`) processes a `limit_wait` event, after the thread event posts in the existing-anchor branch (`:104-107`) or after the root posts in the no-anchor branch (the first Slack event for this run *is* the park, e.g. Slack linked mid-run), write `limit_paused_at = rc.StatusSince` (fallback `now()` if NULL: a row from before 00163's backfill cannot occur for a live park, but never leave the marker unset on a posted park). A re-park always follows a consumed resume (a park's SQL source guard is `status = 'running'`, `runtime.sql:1389-1390`, and the resumed worker reports `running` first), so overwriting is correct.

**Consume** on the next transition, in a new `handleLimitResume(ctx, rc, anchor, base)` called from the existing-anchor branch **before** `handleMilestone` (so the thread reads resume-then-progress). With `anchor.LimitPausedAt` set:

| incoming status | action |
|---|---|
| `running` | post the `▶️ Resumed` event; then clear the marker with a compare-and-swap (`WHERE limit_paused_at = @at`, precedent `SetSlackRunGateIf`, `slack.sql:231`) |
| `awaiting_approval`, `awaiting_input`, `awaiting_followup`, `completed`, `failed`, `cancelled` | clear the marker silently (same CAS): the gate / question / terminal post the notifier makes on that same event already carries the news, and a second line would be noise (D4) |
| `queued`, `claimed`, `limit_wait`, `pool_wait` | leave it: the run is eligible again, or re-parked, but not working (D3) |

With the marker NULL, `handleLimitResume` is a no-op, so a redelivered `running` (heartbeat, milestone progress, summary persist) can never re-post. Post-then-clear gives at-least-once: a crash between the two duplicates one line on the next event rather than losing the resume (D5).

**Waited duration**: `now − anchor.LimitPausedAt`, rendered by a small `humanWait(d time.Duration) string` in slacksvc: `"2h 13m"`, `"45m"`, `"<1m"`; `"3d 2h"` above a day. A value above `RUN_LIMIT_MAX_PARK` (default 8 days, `config.go:891`) or negative is implausible for a real park and omits the `waited` fragment (the lost-consume-event case below) instead of rendering it.

### Not changed

- `renderThreadBlocks` itself: `running`/`queued` still fall to `ok=false` (`TestRenderThreadBlocksSkipsRunningAndQueued`, `notifier_test.go:1033`, stays green). The resume is a **separate renderer** driven by the anchor, not a new status arm: a status arm would fire on every `running` broadcast.
- The pause event, the root edit, `limitWaitDetail`, the rate-limit meter, the park/promotion state machine, `RUN_LIMIT_*` config, `runs` schema.
- Web activity feed, CLI (`uzi run get` already renders `LIMIT_WAITS N (resumed)`, `api/cmd/uzi/run_render.go:269`), inbox notifications (D7), chat runs (D8).

## Scope / non-goals

- **Slack only.** No inbox row, no email. Run-state DMs have never written inbox rows (the inbox is `notifysvc`'s seam, a different surface); the park line does not either.
- **No new toggle.** Rides the existing per-user `slack_notify` kill switch like every other run-state DM (D7).
- **No minimum-pause threshold, no cross-run batching** (D7).
- **Chat runs** never park (`runtime.sql:1343`: *"a chat run never reaches SetState's run lane at all"*), and the judge / self-improve suppression (`notifier_state.go:47-52`) is untouched.
- **No `.github/workflows/**` changes** in implementation or validation (worker PAT lacks `workflow` scope; `.claude/rules/prds.md`). This feature touches none.

## Success criteria

1. A run that parks (`limit_wait`) and later reports `running` gets exactly **one** `▶️ Resumed` reply in its DM thread, after the `⏸️ Paused` reply, with the waited duration and (from the second park) the pause count.
2. A second, third … `running` report after that resume posts **nothing** more (marker consumed).
3. A run whose first post-park transition is the plan gate, a question, or a terminal state gets that surface's existing post and **no** resume line; a *later* park + resume on the same run posts again (marker cleared, not stuck).
4. `queued` after a park posts nothing (the resume waits for `running`).
5. A run whose park is its first-ever Slack event (root created on the park) still gets a resume line, timed from the park's `status_since`.
6. The waited duration comes from the park's `status_since` captured on the anchor; an implausible value (negative, or above `RUN_LIMIT_MAX_PARK`) omits the fragment rather than rendering it.
7. The compare-and-swap clear refuses a stale value: proven against a live database in `api/internal/store`, not through the recording fake (M1).
8. Every new test is mutation-checked per `.claude/rules/go.md` (fails on the unfixed code, passes with the fix; see M1/M3 for the specific mutations). `task gate:api` green; `task gate:repo`'s `check:migration-numbering` green.

## Milestones

- [x] **M1 — Anchor column, queries, live-DB proof (api/store).** New goose migration (draft number `00191_slack_limit_pause_anchor.sql`, renumbered at merge above the live head, currently `00190`): `ALTER TABLE slack_run_messages ADD COLUMN limit_paused_at timestamptz;` with a Down. In `api/internal/store/queries/slack.sql`: add `runs.status_since` to `GetSlackRunContext` (`:109`); `SetSlackRunLimitPause :one` (`UPDATE … SET limit_paused_at = @at, updated_at = now() WHERE run_id = @run_id RETURNING *`) and `ClearSlackRunLimitPause :one` as a compare-and-swap (`… SET limit_paused_at = NULL, updated_at = now() WHERE run_id = @run_id AND limit_paused_at = @at RETURNING *`; no row = refused), each with a comment in the style of `SetSlackRunGateIf` (`slack.sql:231-241`) saying why it is its own column and why the clear is guarded. `sqlc generate`; the new methods join the notifier's store interface (`notifier.go:54` neighbourhood). **Live-DB test** `TestClearSlackRunLimitPauseLiveDB` in `api/internal/store` on the `UZI_TEST_DATABASE_URL` pattern of `TestSetSlackRunQuestionLiveDB` (`api/internal/store/slack_question_integration_test.go:105`): a stale `@at` returns `pgx.ErrNoRows` and leaves the column, the matching `@at` clears it. Mutation: drop the `AND limit_paused_at = @at` guard → the stale-value case reddens. This is the only place a `WHERE` clause executes; the slacksvc fake never runs SQL. Gate: `task gate:api`, plus `task check:migration-numbering` (or `gate:repo`).
- [x] **M2 — Notifier: record the park, post the resume (api/slacksvc).** In `notifier_state.go`: (a) on a `limit_wait` event, write `limit_paused_at` from `rc.StatusSince` after the park's thread post (existing-anchor branch) or after the root post (no-anchor branch); (b) add `handleLimitResume` per the table above, called from the existing-anchor branch before `handleMilestone`; (c) add `resumeThreadBlocks(rc, waited time.Duration, hasWait bool, base string)` (section `▶️ *Resumed · usage limit cleared*` + one context block: waited / pause count / working-title / deep link, fallback `Resumed · <repo>#<iid>`, every untrusted field through `EscapeMrkdwn` + `ScrubSecrets`, emoji-presentation glyph like the rest) and `humanWait`, with the plausibility bound (negative or `> RUN_LIMIT_MAX_PARK` → `hasWait=false`; the notifier reads the configured value, or a named constant equal to the config default if the config is not reachable from slacksvc, stated in a comment); (d) **rewrite the ruling** in `renderThreadBlocks`'s doc comment (`:470-495`): property 2 now reads that the resume posts exactly once per park via the anchor, the bound is `2 × RUN_LIMIT_MAX_WAITS`, and `renderThreadBlocks` still returns `ok=false` for `running`/`queued` on purpose because the resume is anchor-driven. Leave `statusGlyph` alone. Gate: `task gate:api`.
- [x] **M3 — Tests (api/slacksvc).** Make `fakeNotifStore` (`notifier_test.go:22`) **stateful for the new column**: `SetSlackRunLimitPause` writes `msg.LimitPausedAt`, `ClearSlackRunLimitPause` nils it only on a matching value and returns `pgx.ErrNoRows` otherwise (the fake's other setters return a static row, which cannot drive criteria 2 and 3 through `handle`). **Rename, do not drop,** `TestParkThreadsAnEventWhileResumeDoesNot` (`:889-900`): its assertions are about `renderThreadBlocks` and stay true; re-comment it as the "resume is anchor-driven, not a status arm" pin. Add cases for success criteria 1–6: resume posts once on the first `running`; a redelivered `running` posts nothing; `queued` after a park posts nothing; park → `awaiting_approval` clears the marker with no resume line, then a later park → `running` posts again with `(pause 2)`; park as the first Slack event (no anchor) still resumes; `humanWait` table (`<1m`, `45m`, `2h 13m`, `3d 2h`); an implausible waited value omits the fragment. Keep `TestRenderThreadBlocksSkipsRunningAndQueued` and the `limitWaitDetail` tests green. Mutation checks (`.claude/rules/go.md`, watch both directions): (i) delete the `running` arm of `handleLimitResume` → criterion 1 reddens; (ii) delete the CAS clear after the post → criterion 2 reddens; (iii) delete the silent-clear arm → criterion 3's "no resume line after the gate" reddens; (iv) the unguarded-clear mutation is M1's live-DB test, not a slacksvc test. Gate: `task gate:api` (`-race`, `-count=1` as `Taskfile.yml` carries them).
- [x] **M4 — Docs, specs, changelog.** `docs/slack.md`: add `▶️ Resumed · usage limit cleared` to the glyph legend (`:133-139`) and one sentence under the DM description that a park posts a pause line and the resume posts a resume line into the same thread. `docs/run-limit-wait.md` "What you'll see" (`:24-26`): add a **Slack** bullet (pause + resume replies, waited duration, pause count). Then `task docs:sync` (`Taskfile.yml:1988`) and commit the `api/internal/uzidocs/embed/` mirror (root `CLAUDE.md`: a docs-only commit without it reddens `gate:api`). `CHANGELOG.md` `[Unreleased]` entry in the file's one-line-title/one-line-description format. `specs/human.md` Feature #35 (`:590`): one terse user line, `[user 2026-09-05, PRD #1116]`, "when a run parked on a usage limit resumes, the owner gets a Slack message in the run's thread" (user-stated in the originating conversation). `specs/ai.md`: new section **616** (last is 615) with D1–D9 below. Gate: `task gate:api` (docs mirror test) + one `task validate:web` (or `gate:web`) since `web/scripts/check-docs.mjs` reads `docs/*.md`.
- [ ] **M5 — Maintainer validation on a real park/resume (post-release, not the worker).** After a release carrying M1–M4 rolls to the hosted instance, observe one real `limit_wait` → `running` cycle on a run the maintainer owns and confirm: the `▶️ Resumed` reply lands under the `⏸️ Paused` reply, its `waited` figure agrees with `uzi run get <id>` timing within a minute, and no second resume line appears over the rest of the run. Left **unchecked** by the implementing run (needs a deploy and a real limit; mirrors #1114 M5). The PRD stays out of `prds/done/` until a maintainer ticks it.

### Parallel milestone analysis

| Phase | Milestone | Depends on | Files | Parallel? |
|---|---|---|---|---|
| 1 | M1 | – | `store/migrations/00191_*.sql`, `store/queries/slack.sql`, generated `store/*.go`, `store/*_integration_test.go`, `slacksvc/notifier.go` (interface) | – |
| 1 | M4 (docs half) | – | `docs/slack.md`, `docs/run-limit-wait.md`, `uzidocs/embed/`, `CHANGELOG.md`, `specs/*.md` | yes, with M1 |
| 2 | M2 | M1 | `slacksvc/notifier_state.go` | – |
| 3 | M3 | M2 | `slacksvc/notifier_test.go` | – |
| post-release | M5 | release | none | maintainer |

M1 → M2 → M3 are sequential (same package, each builds on the previous). M4 touches only docs/specs and can run alongside M1. One worker doing them in order is fine; the PRD is small.

## Decision Log

- **D1 — Reverse the #268 M3 ruling: the resume posts.** The ruling's reasoning (an edit raises no notification; a park lasting hours must be communicated) is exactly the argument for the resume line too; only its conclusion ("ten posts would read as a stream") is overturned, by the owner, who wants the pair. The bound the ruling cared about survives: ≤ `2 × RUN_LIMIT_MAX_WAITS` lines per run. (User, 2026-09-05.)
- **D2 — A thread reply, not a root edit.** Same reason the pause line is a reply: a Slack edit raises no notification, and the whole point is to be told.
- **D3 — Trigger on the first `running` after the park, not on the sweeper's `queued` promotion.** Promotion means "eligible to be claimed again", not "working": a worker may be busy or draining, the run may fall into `pool_wait`, or the re-claim may re-park. `running` is the honest moment, and it is broadcast from `SetState` on the worker's first report. `claimed` is not a usable trigger: no site broadcasts it by name, and where it does arrive (a summary persist re-emitting `run.Status`) it says nothing about work having restarted; the table leaves the marker on it.
- **D4 — A resume that lands straight in a gate, a question, or a terminal state posts no resume line.** That event's own post (gate card, question card, ✅/❌/🚫) already tells the owner the run is back and what it needs; a `▶️ Resumed` a second before it is noise. The marker is still cleared so the *next* park/resume cycle posts normally.
- **D5 — Dedupe through the anchor row, post-then-clear with a compare-and-swap.** Mirrors `gate_generation`/`SetSlackRunGateIf` and `milestones_notified_completed`: the anchor is the notifier's only durable memory, the event carries no previous status, and a redelivered `running` is routine. Post-then-clear is at-least-once (a crash between post and CAS duplicates one line) and is preferred over clear-then-post's silent loss; the CAS keeps a stale clear from wiping a newer park's marker.
- **D6 — The pause start is the run's own `status_since`, captured onto the anchor at the park.** `SetRunLimitWait` stamps `status_since = now()` (`runtime.sql:1379-1384`), so the exact park time exists on the row when the notifier handles the park; but promotion re-stamps it (`runtime.sql:1394-1445`), so by the `running` event it is the *resume* time. Copying it to the anchor at the park is what preserves it, and it makes the marker and the duration one value. An earlier draft derived the duration from the pause post's Slack `ts`; rejected once `status_since` was found: a DB timestamp needs no parsing, no malformed branch, and no Slack round-trip semantics.
- **D7 — No threshold, no batching, no toggle, no inbox row.** A park is bounded below by `retry_not_before` (the reset window plus backoff), so sub-minute flapping is impossible and a minimum-duration filter would guard against nothing. One thread per run is the existing DM model, and a cross-run "3 runs resumed" digest would need a new per-user surface for a rare case. The per-user `slack_notify` switch already silences every run-state DM. Run-state DMs write no inbox rows today; the pause line does not, so the resume line does not.
- **D8 — Chat runs never park; the judge/self-improve suppression is untouched.** `SetRunLimitWait`'s own comment (`runtime.sql:1343`) rules a chat run out of the run lane, and neither `agent/src/chat-runner.ts` nor `notifier_chat.go` knows `limit_wait`. Nothing to do on the chat path.
- **D9 — No CLI or web change.** Checked per the root `CLAUDE.md` convention: `uzi run get` already renders `LIMIT_WAITS N (resumed)`, the run view and feed already show the pause and resume, and this PRD adds no route or DTO.

## Risks & mitigations

- **A lost consume event.** The marker is durable but its consume is event-driven, and the notifier's queue drops when full (`notifier.go:199-206`, warn-logged) and loses in-flight events on a restart. If the `awaiting_approval` (or terminal) event after a resume is dropped, the marker survives, and the next `running` (the worker's own report after approval; plan approval itself broadcasts nothing, `submitApproval`, `api/internal/workersvc/submit.go:378`) posts a late `▶️ Resumed` whose `waited` would span the gate. Mitigation: the plausibility bound in M2 omits a `waited` above `RUN_LIMIT_MAX_PARK`, and the line still reads as a resume, which is true. Same failure class every run-state DM already has; not new.
- **A duplicate resume line after a crash between post and clear** (D5): accepted, one line at most, and the pause count in the text makes it obviously a repeat.
- **A run that re-parks seconds after resuming** (the resuming claim already skips the credential that parked it, so this needs a *second* exhausted credential): two honest lines, `(pause 2)` on the second pause. Not suppressed.
- **The marker sticks** if a consuming arm were missed: the table covers all eleven statuses (the live `runs_status_check` in `00170_run_pool_wait.sql`); `pool_wait` is deliberately "leave", and it can only exit to `queued` → `running`, which consumes. M3's criterion 3 test guards the silent-clear arm.
- **Slack outage at the resume**: the post fails, the marker stays set, the next `running` report retries (the same best-effort contract every other run-state post has).

## Internet-independence (offline worker)

Every anchor above is a file:symbol in this clone, re-derivable offline. The transition facts (what is broadcast on promotion and on the worker's first report) are read from `sweep.go` / `service.go` / `summaries.go`, not from external docs. Durations come from database timestamps (`status_since`), so no Slack API semantics are needed. No milestone the worker executes (M1–M4) needs the open internet or a `.github/workflows/**` edit.
