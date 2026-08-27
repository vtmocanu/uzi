# PRD #119 — Show pending-judge state on the run page (stop the redundant "Run judge" click)

**Issue**: [#119](https://github.com/vtmocanu/uzi/-/issues/119) · **Label**: PRD · **Priority**: Medium
**Area**: `api/internal/{store,workersvc,handler}` (read-side) + `web/` run-page **Run review** panel + `api/cmd/uzi` review output. PRD #46 lineage (the run judge).

## Problem

When a run finishes, uzi automatically enqueues a judge run for it
(`maybeEnqueueJudge`, at the committed terminal transition). The judge is a
separate `kind='judge'` run that rides the normal queue: it sits `queued` until a
worker claims it, then runs on the owner's Anthropic token, and only when it
finishes does the review appear. That gap is **seconds to minutes** wide.

During that gap the run-page **Run review** panel shows nothing but:

> This run hasn't been judged yet. Running the judge reviews the run on your Anthropic token.

…next to an enabled **Run judge** button. The panel cannot tell the difference
between the two states it lumps together, because
`GET /runs/:id/review` returns `{"review": null}` for **both**:

1. **Never judged** — no judge run exists, the button is the only way to start one.
2. **Judge in flight** — an automatic (or a prior manual) judge is already
   `queued`/`claimed`/`running`; a review is coming.

So the user, seeing "hasn't been judged yet" and a live button, clicks **Run
judge** — even though one will run anyway. The click is a redundant no-op: the
`uq_runs_one_active_judge_per_target` partial unique index (00058) already
guarantees one active judge per target, so `RerunJudge` trips `23505` →
`ErrJudgeAlreadyActive` → **409 "a judge run is already in progress for this
run"**. The user gets an error toast for doing the intuitive thing, learns
nothing about the judge that *was* already coming, and is left unsure whether
their click is what finally kicked it off.

The missing piece is purely **visibility**: the server knows an active judge run
exists (the index is built on exactly that fact), but that knowledge never
reaches the panel.

## Solution

Surface the pending-judge state on the run page **from server truth**, and stop
offering a button that would only 409.

1. **API** — the run-review read additionally reports the **active judge run**
   for the target (if any) as a sibling field `pending_judge`. The response
   becomes `{"review": null|{…}, "pending_judge": null|{…}}`; the `review` shape
   is unchanged. `pending_judge` carries a normalized `state`
   (`scheduled` = `queued`, `running` = `claimed`/`running`) and `enqueued_at`.
2. **Web** — when `pending_judge` is set the panel replaces the empty-state copy
   with **"Judge scheduled — the verdict will appear here when it finishes."**
   (scheduled) or **"Judge in progress…"** (running), and **disables +
   relabels** the button (`Judge scheduled` / `Judge running…`) so the redundant
   click is gone in the common case. The 409 (`ErrJudgeAlreadyActive`) handler
   **stays** as the backstop for the narrow TOCTOU window (an auto-judge that
   enqueues between the panel's fetch and a click): on that 409 the panel
   re-fetches the review so it converges to the pending state instead of showing a
   bare error toast. A bounded background poll (generalized from the existing
   post-re-run poll) swaps the panel to the verdict when the review lands.
3. **Re-judge in flight survives a reload.** Today the "Judge re-queued…" banner
   and the disabled button come from a **local** `queued` state that only exists
   in the session that clicked; a page reload loses it and re-offers the button.
   Driving the same disable/banner from server-truth `pending_judge` fixes that
   too — the in-flight state is visible to anyone viewing the run, on any load.
4. **CLI + mock parity** — `uzi review show` / `uzi run show` print "judge
   scheduled" / "judge in progress" instead of a bare "not judged"; mock mode
   grows a pending-judge run so the UI and its tests can exercise the state.

### The load-bearing invariant

The new "is a judge active?" read **must use the exact predicate of the unique
index** — `kind='judge' AND target_run_id=$1 AND status NOT IN ('completed',
'failed', 'cancelled')`. That equivalence is the whole point: the UI must show
"pending" in **precisely** the set of states where a manual click would 23505,
and hide it in precisely the set where the click is legitimately the way to start
a judge. Copy the predicate; never paraphrase it. (This mirrors the existing
`GetActiveJudgeRunForWorkerTarget`, which is the same predicate plus a
`worker_id` scope — the new query is that one minus the worker scope.)

### Approach chosen (and rejected)

- **Chosen — extend the existing review endpoint with a sibling `pending_judge`
  field, sourced from the active judge run.** One round-trip (the panel already
  calls `getRunReview`), one new read query on existing columns, **no migration**.
  The `review` payload is untouched, so every other consumer is unaffected.
- **Rejected — a separate `GET /runs/:id/judge-status` endpoint.** A second
  round-trip and a second route for a signal the panel already fetches alongside;
  no upside.
- **Rejected — reuse the board's judge badge (PRD #98 M4) / the run-list DTO.**
  The badge answers "was this run judged" for the board; the run page needs the
  live scheduled/in-progress distinction, which the badge does not carry. Reusing
  it would couple two surfaces and still leave the panel guessing.
- **Rejected — infer "pending" purely client-side from the target run's own
  status.** The target run is already terminal when the panel shows; its status
  says nothing about whether a *judge* run was enqueued (auto-judge is gated on
  the owner's opt-in + token, so "terminal" does not imply "judge coming"). Only
  the judge run's real state is authoritative.
- **Rejected — WebSocket push of judge completion to the run page.** The judge is
  a distinct run the page does not subscribe to; wiring a cross-run push is far
  more surface than the bounded poll the panel already runs after a re-run. Poll,
  consistent with today's code.

## User journey

- A user opens a run that just finished. Auto-judge has been enqueued but not yet
  claimed. The panel reads **"Judge scheduled — the verdict will appear here when
  it finishes."**; the button is disabled and labelled **Judge scheduled**. No
  redundant click is possible, and the user understands a review is coming.
- Moments later a worker claims the judge: the panel updates to **"Judge in
  progress…"** (button **Judge running…**), then, when the judge posts its
  verdict, the poll swaps the panel to the full review — no manual refresh.
- A user reloads the page mid-judge (whether it was auto-enqueued or someone
  clicked **Re-run judge**): the in-progress state is still shown, because it now
  comes from the server, not from the tab that started it.
- A genuinely unjudged run (owner never opted into auto-judge, or it is off): the
  panel reads "This run hasn't been judged yet." with an **enabled** **Run
  judge** button, exactly as today. The redundant-click problem only existed when
  a judge was in flight, and that is the only case the copy/button now changes.

## Technical scope

No schema change — this is a new **read** over the existing `runs` rows + the
existing partial index. `sqlc generate` after the query is added.

### API (read-side)

- **`api/internal/store/queries/judge.sql`** — new `GetActiveJudgeRunForTarget`:
  `SELECT id, status, created_at FROM runs WHERE kind='judge' AND target_run_id=$1
  AND status NOT IN ('completed','failed','cancelled') LIMIT 1`. Predicate is a
  verbatim copy of the `uq_runs_one_active_judge_per_target` index (see invariant
  above); the index makes it at most one row (LIMIT 1 is belt-and-suspenders).
  Return only the three columns the caller needs, not `RETURNING *` — this is a
  UI signal, not the judge machinery. `sqlc generate` regenerates `judge.sql.go`.
- **`api/internal/workersvc/judge_read.go`** — extend the run-review read. Keep
  `GetReviewForTarget`'s `(nil, nil)` = "not judged" contract, but have the read
  path also load the active judge run and return it. Cleanest shape: a small
  `PendingJudge` struct (`{Status string; EnqueuedAt time.Time}`, `Status` the
  raw run status) returned alongside the review, e.g. widen the method's result
  or add a sibling `GetPendingJudgeForTarget` the handler calls after the same
  `GetRunForViewer` visibility gate. **Visibility is owner-or-admin via the
  existing `GetRunForViewer`** — do **not** re-scope the query by user; a run the
  caller cannot see is already `ErrRunNotFound` before this read, exactly as the
  review read is gated. A pending judge with no review yet must still return
  (review nil, pending set); a pending judge *with* a prior review (a re-judge in
  flight) returns (review set, pending set).
- **`api/internal/apitypes/review.go`** — new `PendingJudgeDTO { State string
  \`json:"state"\`; EnqueuedAt time.Time \`json:"enqueued_at"\` }`. `State` is the
  normalized display value from a **total** mapper next to `reviewToDTO`: `queued
  → "scheduled"`, and **everything else in the index's active set →
  "running"** (`claimed`, `running`, and — see below — `awaiting_approval`). The
  mapper must be total over the active-status domain, never a `queued`/`claimed`/
  `running`-only switch that falls through to `""`: `runs.status` legally admits
  `awaiting_approval` (migration 00020), which is **inside** the index predicate's
  `NOT IN ('completed','failed','cancelled')` set, so the query can return it. A
  judge run never parks at the approval gate today (the judge runner has no
  approval flow; `auto_approve` is autopilot-only), but the schema permits the
  status, and an unmapped one would emit `state:""` and break the web union
  `"scheduled" | "running"`. Default the non-`queued` arm to `"running"` so any
  future/edge active status renders as in-progress rather than blank. Do **not**
  widen `ReviewDTO` — `pending_judge`
  is orthogonal to a review and is present even when `review` is null, so it is a
  **sibling key on the response object**, not a field of `ReviewDTO`.
- **`api/internal/handler/judge.go` — `GetRunReview`** — emit
  `{"review": …, "pending_judge": …}`. Both keys are always present; either may be
  null. The `res == nil` branch (visible-but-unjudged) now still fetches + emits
  `pending_judge` instead of returning early with review-only.

### Web (run page)

- **`web/src/lib/api.ts`** — `getRunReview` return type becomes
  `{ review: RunReview | null; pending_judge: PendingJudge | null }`; add
  `export interface PendingJudge { state: "scheduled" | "running"; enqueued_at:
  string }`.
- **`web/src/pages/RunView.tsx`** (the `RunReview` panel, ~806–946) —
  - Store `pendingJudge` from the fetch. Treat the button as disabled when
    `rerunning || queued || pendingJudge != null`; relabel to
    `Judge scheduled` / `Judge running…` per `pendingJudge.state` (falling back to
    the existing `Re-run judge`/`Run judge`/`Re-queuing…` labels otherwise).
  - Empty-state (`!review`): if `pendingJudge` show the scheduled/in-progress copy;
    else the unchanged "hasn't been judged yet." line.
  - Present-review + `pendingJudge` (re-judge in flight): show the existing
    "Judge re-queued…" info line, now armed from server truth rather than only the
    local `queued` flag — so it survives a reload.
  - **Poll**: generalize the bounded post-re-run poll (871–893) to run whenever a
    judge is pending (server `pendingJudge` **or** the local optimistic `queued`),
    re-fetching `getRunReview` until the review's `updated_at` changes **or**
    `pending_judge` clears. Raise the cap (judges can take minutes, not the ~1 min
    the current 15×4s allows) and stop cleanly when pending clears.
- **`web/src/mocks/mockApi.ts` + `web/src/mocks/data.ts`** — a mock terminal run
  whose `getRunReview` returns `{ review: null, pending_judge: { state:
  "running", enqueued_at } }` (and ideally a second, `scheduled`), so mock mode +
  the panel tests exercise both empty-state variants and the disabled button.

### CLI (second API consumer — CLAUDE.md convention)

- **`api/cmd/uzi/review.go` / `run.go`** — the review read now carries
  `pending_judge`. Where the CLI prints "not judged" for `review:null`
  (`run.go:719`, and the `review show` path via the shared `renderReview`), print
  **"judge scheduled"** / **"judge in progress"** when `pending_judge` is set,
  keeping "not judged" only for the genuinely-null case. Same faithful distinction
  the web panel makes; no new command. **Two client-side changes, not one**: (a)
  the `uzicli` client type gains the `pending_judge` field, **and** (b) the
  `--json` path must not drop it — `runReviewShow`'s JSON mode re-serializes the
  parsed DTO as `p.JSON(map[string]any{"review": rv})`, so it emits its **own**
  envelope rather than passing the server body through; that map must also carry
  `pending_judge`, or `--json` consumers silently never see the field even after
  the type gains it.

## Milestones

Dependency: **M1 (API) blocks M2 (web) and M4 (CLI)** — both consume the new
`pending_judge` field. M2 and M4 are independent of each other.

| Phase | Milestone | Depends on | Files (repo: uzi) |
|---|---|---|---|
| 1 | M1 — API pending-judge read + DTO | — | `store/queries/judge.sql`, `judge.sql.go`, `workersvc/judge_read.go`, `apitypes/review.go`, `handler/judge.go` |
| 2 | M2 — Web run-panel pending state | M1 | `web/src/lib/api.ts`, `web/src/pages/RunView.tsx`, `web/src/mocks/*` |
| 2 | M4 — CLI parity | M1 | `api/cmd/uzi/{review,run}.go`, `uzicli` client |
| 3 | M5 — In-app + docs verification | M2, M4 | run-page verify (k8s-first), `docs/` if any |

- [x] **M1 — API: active-judge read + `pending_judge` DTO.** Add
  `GetActiveJudgeRunForTarget` (predicate = index verbatim); `sqlc generate`.
  Extend the run-review read path to load + return the active judge run behind the
  existing `GetRunForViewer` visibility gate. Add `PendingJudgeDTO` + a **total**
  status mapper (`queued→scheduled`; every other active status, incl.
  `awaiting_approval`, `→running` — never a fall-through to `""`). `GetRunReview`
  emits
  `{review, pending_judge}` (both keys always present, either nullable).
  **Tests**: a store LiveDB test that an active judge for a target is found and a
  terminal one is not (pinning the predicate ↔ index equivalence); a handler test
  that an unjudged target with an active judge returns `review:null` +
  `pending_judge:{state:"scheduled"|"running"}`, and a target with no judge
  returns both null. `go build ./...` + `go test ./...` green; LiveDB sweep green
  via `./e2e/run-store-it.sh` (positive control: the named test `--- PASS`, no
  `--- SKIP`).
  - Landed as described, with two corrections found in review. The predicate is
    the index's partial `WHERE` **with its indexed column spelled out** — not a
    literal copy of the index text, as the first comments claimed; the
    equivalence they assert does hold. The mapper's "incl. `awaiting_approval`"
    was short: the live `runs_status_check` (00092, nine values) makes the
    active set **six** — `queued`, `claimed`, `running`, `awaiting_approval`,
    `awaiting_input`, `limit_wait` — and `awaiting_input` is the one a judge run
    can genuinely reach, since `SetRunAwaitingInput` carries no kind guard.
    Sweep: `RUN=240 PASS=240 FAIL=0 SKIP=0`,
    `--- PASS: TestJudgeQueriesLiveDB/active_judge_for_target`. The predicate is
    mutation-proven: folding the `NOT IN` reddens it, and so does folding
    `kind = 'judge'` (pinned by a decoy non-judge run carrying `target_run_id`).
- [x] **M2 — Web: pending state + disabled button + poll.** api-client type;
  panel empty-state copy + disabled/relabeled button when pending; re-judge-in-
  flight driven by server truth (survives reload); generalized bounded poll;
  keep the 409 (`ErrJudgeAlreadyActive`) handler and make it re-fetch the review
  so a TOCTOU click converges to the pending state instead of a bare error;
  mock states. **Tests** (`web/src/pages/RunView.test.tsx`): (a) `review:null` +
  `pending_judge` renders the scheduled/in-progress copy and a **disabled**
  button (no `RerunJudge` POST fires on click); (b) an unjudged run with
  `pending_judge:null` still offers the enabled **Run judge** button (the existing
  test, unchanged); (c) a present review + `pending_judge` shows the re-queued
  line with the button disabled. `npm test` + `npm run typecheck` green.
  - All three tests exist and pass (115 files / 1593 tests). Two deltas from the
    text above. The re-judge-in-flight line is no longer the "re-queued" one
    when it comes from the server: armed from server truth it cannot assert
    *this viewer* re-queued anything, so it reads "A judge is
    scheduled/running for this run — …"; "Judge re-queued" is kept only on the
    local optimistic arm, where it is accurate. And the poll's stop condition is
    **one-sided**, not two: a landed verdict swaps the panel but does not stop
    the poll, because `PostReview` authorizes against the still-ACTIVE judge
    run, so the review row is written before the judge run goes terminal —
    stopping on `landed` froze a disabled "Judge running…" button over the
    verdict that had just arrived. Only a cleared `pending_judge` (or the cap)
    stops it. Both bugs were invisible to the first suite; the three
    fake-timer tests that now cover them were shown to fail before the fix.
- [x] **M4 — CLI parity.** `uzi review show` / `uzi run show` print "judge
  scheduled" / "judge in progress" when `pending_judge` is set, "not judged"
  only when genuinely null. `uzicli` client type carries the field. **Tests**:
  extend the existing `commands_test.go` "not judged" case with a pending variant.
  `go test ./api/cmd/uzi/...` green.
  - Note the `uzicli.Client.RunReview` signature had to change (it returns the
    pending DTO alongside the review), which rippled to `FakeClient` and the TUI
    overlay — `uzi run review` is a hidden alias onto the same `runReviewShow`,
    so there is no second render path. `--json` carries `pending_judge` in both
    the populated and the null case, pinned by a test that asserts the literal
    output: the CLI re-mints that envelope client-side rather than proxying the
    server body, so a server key not added there is silently invisible.
- [ ] **M5 — Verify in-app + docs.** On the primary runtime (k8s-first per
  CLAUDE.md conventions; compose acceptable for this UI-only change), finish a
  real run with auto-judge on and confirm the panel shows scheduled → in progress
  → verdict without a manual click, and that a reload mid-judge keeps the state.
  Confirm the enabled-button path is unchanged for a run with no judge. Update
  any `docs/` page that describes the run-review panel; `npm run build` green
  (runs `check-docs`).
  - **Left unchecked deliberately — the docs half landed, the runtime half did
    not.** Done: `docs/judge.md` now covers both pending states on the run page
    and in `uzi review show` (the page never mentioned the automatic post-run
    enqueue at all, which is the root of the confusion this PRD fixes);
    `npm run build` green including `check-docs`; and all four panel states were
    driven in headless Chromium against `VITE_UZI_MOCK=1`, which is what found
    that #119 had consumed the only terminal unjudged fixture and left the
    unchanged never-judged state undemoable (a fifth fixture, `run-unjudged`,
    restores it). **Not done: a real run on the primary runtime.** No cluster is
    reachable from this worktree and CLAUDE.md forbids standing a stack up
    casually, so nobody has watched scheduled → in progress → verdict against a
    real judge, or reloaded mid-judge outside jsdom. That matters more than
    usual here: the poll bug above was exactly the kind of defect a fake-timer
    suite hid and a real browser session would have shown in seconds.

## Risks & mitigations

- **Predicate drift from the index** (the one real correctness risk). If the read
  query and the `uq_runs_one_active_judge_per_target` index ever disagree, the UI
  shows "pending" where a click would succeed, or offers the button where it would
  409 — the exact confusion this PRD removes, re-introduced. Mitigation: copy the
  index predicate verbatim, and the M1 LiveDB test asserts the equivalence
  directly (active found, terminal not found).
- **Poll never terminating.** The generalized poll must stop when the review
  lands **or** `pending_judge` clears (a judge that failed leaves no review but
  also no active judge). Mitigation: stop condition keys off *either* a changed
  `updated_at` *or* a now-null `pending_judge`, plus a generous absolute cap;
  clear the interval on unmount (as today).
- **Response shape change for existing consumers.** `getRunReview` gains a sibling
  key; `review`'s own shape is untouched. The CLI is updated in the same PRD (M4);
  the web client in M2. No other consumer reads this route (grep before landing).
- **A stale pending after a judge dies without a review.** A judge that goes
  `failed`/`cancelled` leaves the active set, so the next fetch/poll returns
  `pending_judge:null` and the panel falls back to the correct empty state
  (enabled button) or the last review. No sticky "in progress".
- **TOCTOU: a click can still 409.** The panel's "is a judge pending?" answer is
  a point-in-time read; an auto-judge can enqueue in the gap between that fetch
  and a click, so the button (enabled at fetch time) still 409s on click. The
  window shrinks but does not close. Mitigation: keep the `ErrJudgeAlreadyActive`
  handler and make it **re-fetch** so the panel converges to the pending state —
  the click is absorbed, not surfaced as an error. Do **not** remove the 409
  handling on the theory the disabled button made it unreachable.
- **Non-total state mapper.** `runs.status` admits `awaiting_approval`, which is
  inside the index's active set; a `queued`/`claimed`/`running`-only mapper would
  emit `state:""` and break the web union. Mitigation: the mapper defaults every
  non-`queued` active status to `"running"` (see Technical scope); a judge run
  cannot reach the gate today, but the mapper is total regardless.
- **Regression surface is read-only.** No migration, no write path, no change to
  enqueue/gate logic. The button already 409-guards via the index; this hides the
  affordance that produces the 409 and converges the residual-race click.

## Success criteria

- While an automatic or manual judge is active (`queued`/`claimed`/`running`) for
  a run, the run page shows "Judge scheduled" / "Judge in progress…" and the
  Run-judge/Re-run-judge button is disabled — the redundant click is gone in the
  common case, and a residual-race click is absorbed (409 → re-fetch → pending),
  never a bare error toast. The `state` mapper is total over the active-status
  set (no `""` slips through).
- The pending state is visible on a fresh page load (server-truth), not only in
  the session that triggered the judge; it clears to the verdict (or to the
  enabled empty state) on its own via the poll.
- A run with no active judge is unchanged: enabled **Run judge** button, "hasn't
  been judged yet." copy.
- The pending read uses the index predicate verbatim, with a test pinning the
  equivalence. `uzi review show` mirrors the web distinction. All Go + web tests,
  typecheck, and build green; LiveDB sweep proven to have run.
