# PRD #527 — Record "merged" MR state for cleanly-merged PRs (and backfill older ones)

- **Issue**: [#527](https://github.com/vtmocanu/uzi/issues/527)
- **Priority**: Low (display-correctness; no functional impact on merges or guardrails)
- **Status**: Draft — ready for implementation

> **Self-contained for an offline worker.** This is an api-only change: one SQL query, `sqlc` regen, tests, and doc comments. No milestone needs the open web, and the change never touches `.github/workflows/**` (safe for a uzi worker's `repo`-scoped PAT). Every fact below is resolved against the repo clone; all file:line references resolve there. **Offline caveats** (verified during review): `sqlc generate`'s `go run …sqlc@v1.30.0` fetches from the Go module proxy, which is inside the worker's package-cache egress allowlist, so it is reachable; and the one `*LiveDB` test edited here is a **compile-checked assertion rewrite** whose *run* is CI's job (`test:api-store-it`) — it self-skips in `task gate:api` (no `UZI_TEST_DATABASE_URL`), so the worker can still gate green without Postgres. The exact expected assertions are pinned in M2 so the worker does not have to run the test to get them right.

---

## Problem

A run's **"merged"** badge almost never appears on the runs list (`/runs`) or run view, even for PRs that clearly merged. Most completed runs show a plain green `PR #NNN` with no "merged" suffix; only the occasional one (e.g. the most recent) shows `PR #NNN merged`.

The badge renders "merged" only when the per-run frozen hint `runs.mr_state === "merged"` (`web/src/components/MrChip.tsx:49`, `web/src/lib/runBadge.ts:50`). That column has exactly **one writer**, `SetRunMRState`, called only by the MR-close watcher (`api/internal/forgesvc/mr_watch.go:106,185`; enforced by `TestMRStateIsWatcherOwned` in `api/internal/store/mr_state_test.go`).

The watcher only inspects runs returned by `ListMRWatchCandidates` (`api/internal/store/queries/forge.sql:451-483`), whose WHERE requires **`i.state = 'opened'`**:

```sql
WHERE l.status = 'completed'
  AND l.mr_iid IS NOT NULL
  AND i.state = 'opened'                                        -- <- the blocker
  AND (jsonb_exists(i.labels, 'Human Review') OR l.mr_state = 'closed');
```

**The race that makes "merged" unreachable.** Merging a PR closes its issue (via `Closes #N` in the MR body), and each poller tick runs the issue sync (`FullSync`/`IncrementalSync`) **before** the MR watcher (`api/internal/poller/poller.go:328,338,352`). So on the very tick a merge first becomes observable, the issue-sync marks the issue `state='closed'` first; `ListMRWatchCandidates` then drops that run (`i.state='opened'` fails); and the watcher never records `mr_state='merged'`. The hint freezes at whatever it was (`NULL` for a run whose MR was never polled while its card sat in Human Review, or `'opened'`), so the chip shows `PR #NNN` with no suffix — even though the PR really merged.

A run shows "merged" today **only** if a poll tick happened to land in the narrow window after the forge marked the PR merged but before the issue-close sync closed the issue (and the card was still in Human Review). That is timing luck, not correctness — which is why one run in a page shows "merged" and the rest do not.

This is purely a **display / metadata-freshness** defect. Merges, guardrails, board moves, and run lifecycle are all unaffected; the only wrong thing is the frozen `mr_state` hint the badge reads.

## Solution overview

Add a **closed-issue terminal-recording lane** to the MR-watch candidate set: when a completed run's issue is now **closed** and its `mr_state` is not yet terminal, keep observing the MR (until `merged`/`closed`) and record that state **without moving any board card**. This is a pure `SetRunMRState` observation, so it preserves the watcher-owned invariant and adds no new writer.

The change is almost entirely in the candidate **query**; the Go watcher (`syncOneMRState`) already does the right thing for these observations and needs no behavioral change (see Background). It also **backfills** already-merged historical PRs: on subsequent ticks each such run is polled once, its terminal state is recorded, and it then drops out of the candidate set.

**Scoped deliberately to closed issues.** The new lane is gated `i.state = 'closed'`, so the existing open-issue board-move lane (Lane A) is left **byte-identical** and its candidate set is unchanged — the new lane targets exactly the merge-closed-the-issue case the bug is about, and nothing else. A modest **hardcoded per-tick `LIMIT`** bounds the one-time backfill burst of forge reads (no sqlc param, so zero Go signature change), and an open-issues-first ordering guarantees Lane A candidates are never deferred behind backfill rows.

---

## Background — why the Go watcher needs no behavioral change (resolved; do not re-investigate)

`syncOneMRState` (`api/internal/forgesvc/mr_watch.go:52-108`) already handles every observation a closed-issue candidate can produce **move-free**, and the existing tests prove it:

1. **Bootstrap (first observation, `mr_state IS NULL`)** records the observed state and returns **without moving** — `mr_watch.go:73-80`, Decision 9. Covered by `TestMRBootstrapRecordsWithoutMoving` (`mr_watch_test.go:98`). A historical merged run has `mr_state=NULL`, so its first poll records `'merged'` directly via this path.
2. **`merged` / `locked` transitions** hit the `default` switch arm — "record, never move" (`mr_watch.go:101-107`). Covered by `TestMRMergedIsRecordedNeverMoves` (`mr_watch_test.go:288`) and `TestMRLockedIsRecordedNeverMoves` (`:303`).
3. **The close-edge / reopen-edge board moves** already guard against a closed issue inside `guardedMRMove`: `issue.State == "closed"` returns `moveSkipped` (record, no move) — `mr_watch.go:147-149`. Covered by `TestMRCloseEdgeClosedIssueConsumesEdge` (`mr_watch_test.go:194`).
4. **Unknown / empty states** are ignored before any write (`mr_watch.go:68-71`), so a transient glitch cannot poison the baseline.

So admitting closed-issue candidates cannot cause a spurious board move: every path they can take is either an explicit record-without-move (bootstrap, merged, locked) or an explicit closed-issue skip (the edge paths). **The only reason these runs are not already recorded is that the query never selects them.**

**The cached issue row survives the merge, so the candidate JOIN holds for backfill.** `FullSync` fetches the PRD-labelled set with `state=all` (`forgesvc/service.go:430` passes only `Labels`, no `State`; `forge.go:337` `StateAll = ""` is the zero value, pinned by `issue_state_param_test.go:81-94`), so a merged (closed) but still-PRD-labelled issue stays in the `issues` cache and is not evicted. `ListMRWatchCandidates` JOINs `issues` to read `i.state`; that JOIN still matches for a closed-but-cached issue. (If an issue is both closed *and* de-labelled it can be evicted, in which case its run simply never backfills — an accepted edge, Risks R3.)

---

## Technical scope

### The query change — `api/internal/store/queries/forge.sql` (`ListMRWatchCandidates`)

Replace the single `i.state = 'opened' AND (...)` clause with a **union of two lanes**. Lane A is byte-identical to today; Lane B is the new, closed-issue-only recording lane:

```sql
WHERE l.status = 'completed'
  AND l.mr_iid IS NOT NULL
  AND (
    -- Lane A (UNCHANGED): open-issue board-move watch — card in Human Review
    -- (close-edge) or run already recorded closed (reopen-edge, Decision 10).
    (i.state = 'opened' AND (jsonb_exists(i.labels, 'Human Review') OR l.mr_state = 'closed'))
    OR
    -- Lane B (NEW): closed-issue terminal-state recording. A merge CLOSES the
    -- issue (Closes #N) before SyncMRStates runs, so the merged state is only
    -- observable after i.state='closed'. Keep polling until mr_state is terminal
    -- (merged/closed). Move-free by construction (see PRD Background): bootstrap
    -- and the merged/locked transition never move a card, and the edge paths skip
    -- a closed issue. `locked` is a transient mid-merge state, kept polling so it
    -- settles to merged (Decision D5).
    (i.state = 'closed' AND (l.mr_state IS NULL OR l.mr_state IN ('opened', 'locked')))
  )
-- Open-issue (Lane A) candidates first so the board-move watch is never deferred
-- behind a closed-issue backfill burst; then newest runs first so recent merges
-- record before old ones. Qualify l.created_at (not bare) — `issues` has none.
ORDER BY (i.state = 'opened') DESC, l.created_at DESC
LIMIT 100;   -- hardcoded burst bound; see Decisions D4. Must exceed the steady-state
             -- open-issue (Lane A) candidate count so closed-issue backfill still
             -- gets slots — it comfortably does (Lane A is HR/reopen cards only).
```

Notes for the implementer:
- **Lane B is `i.state = 'closed'` only.** It does **not** admit open, non-Human-Review runs — so the open-issue candidate set is exactly Lane A's, unchanged. This is why only one live-DB fixture flips (M2).
- **`LIMIT 100` is a literal constant, not a sqlc param — deliberately.** A named param would change the generated signature and ripple through the `Querier` interface (`forgesvc/service.go:97`), the `fakeStore` mock (`forgesvc/sync_test.go:305`), and both callers. A constant touches zero Go. 100 comfortably exceeds any realistic Lane-A count, so closed-issue backfill always gets the remaining slots and completes over a few ticks.
- Add `r.created_at` to the `latest` CTE's projection (`forge.sql:470-476`) so the outer `ORDER BY` can reference `l.created_at`. **Do not** change the `DISTINCT ON (r.issue_iid) ... ORDER BY r.issue_iid, r.created_at DESC` selection — it must still pick the latest run per issue (Decision 4: never fall back to an older run's MR). Because the outer SELECT still projects the same 4 columns, the generated `ListMRWatchCandidatesRow` struct and the `ListMRWatchCandidates(ctx, repoID)` signature **do not change** (verified in review).
- Update the query's doc comment (`forge.sql:451-469`) to describe both lanes and the move-free guarantee.

**Then regenerate sqlc** (the generated const in `api/internal/store/forge.sql.go` is what executes — a `.sql`-only edit is inert):
```sh
cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```
CI's `validate:api` asserts this regenerate is a no-op, so the regenerated `forge.sql.go` must be committed.

### The Go watcher — `api/internal/forgesvc/mr_watch.go`

**No behavioral change** (see Background). Add a comment at `syncOneMRState` noting that closed-issue candidates are now expected and are handled move-free by the existing bootstrap/`default`/closed-skip paths. No signature change (the query stays `ListMRWatchCandidates(ctx, repoID)` because `LIMIT` is a literal).

### Update the two stale cross-references (they assert the premise this PRD removes)

The open-only requirement is referenced from another query's rationale, which becomes misleading:
- `forge.sql:537-543` (doc comment of `ListPRDLinkPatchCandidates`) says *"ListMRWatchCandidates requires i.state = 'opened' and would therefore miss this deterministically."* After Lane B that is no longer true. Reword to: `ListMRWatchCandidates` now has a closed-issue lane, **but it only records `mr_state` — it performs no description write and no PRD-link patch**, so `ListPRDLinkPatchCandidates` remains a separate query (it does a forge description WRITE with its own superseded-run and edge scoping; see Decision 10). Keep Decision 10's intent explicit rather than leaving the two rationales in silent tension.
- `api/internal/store/prd_link_patch_query_test.go:17-19` carries the same prose in a comment. Update it to match. (Its test `TestPRDLinkPatchCandidatesReadsNoIssueState` scans only the *other* query's body, so it does not break — this is a comment-accuracy fix, per the repo's "correct a doc the moment you find it wrong" rule.)

### No schema / migration / DTO / web change

`runs.mr_state` already exists (written only by `SetRunMRState`). No new column, **no goose migration**, no DTO change. `TestMRStateIsWatcherOwned` stays green (`SetRunMRState` remains the sole writer). The web already renders `mr_state === "merged"` (`MrChip.tsx`, `runBadge.ts`); once the backend records `merged`, the runs list (live-updating since PRD #522) and run view show it with **no frontend edit** (confirm during validation).

### Out of scope
- **Real-time "merged the instant it merges."** The badge updates on the poller's cadence, same as every watcher-driven field. No webhook/push path.
- **Open, non-Human-Review runs.** Lane B does not admit them; their MR is still open and is Lane A's concern while in Human Review. When such a PR eventually merges and closes the issue, Lane B catches it then.
- **Evicted (closed *and* de-labelled) issues** — never backfill (Risks R3). **`mr_web_url` / other MR metadata** — only `mr_state`.

---

## Milestones

Small, api-only, sequential (M2 depends on M1's query shape).

- [ ] **M1 — Add the closed-issue terminal-recording lane.** In `ListMRWatchCandidates`: rewrite the WHERE as Lane A (unchanged) OR Lane B (`i.state='closed' AND mr_state IS NULL OR IN ('opened','locked')`); add `r.created_at` to the `latest` CTE; add `ORDER BY (i.state='opened') DESC, l.created_at DESC` and `LIMIT 100`; update the query doc comment. Update the two stale cross-references (`forge.sql:537-543`, `prd_link_patch_query_test.go:17-19`). Run `sqlc generate` and commit the regenerated `forge.sql.go`. Add the clarifying comment in `mr_watch.go`. Verify `TestMRStateIsWatcherOwned` and `TestPRDLinkPatchCandidatesReadsNoIssueState` still pass (no new writer, no signature change).
- [ ] **M2 — Tests: candidate selection (incl. the existing LiveDB test) + move-free recording.**
  1. **Rewrite `TestListMRWatchCandidatesLiveDB`** (`api/internal/store/mr_watch_integration_test.go`) — its negative assertions are inverted by Lane B and it **will fail to compile-pass** otherwise. With Lane B scoped to closed issues, **exactly one fixture flips**: fixture **104** (`:111-113`, closed issue, completed run, MR 204, `mr_state=NULL`) becomes a candidate (bootstrap will record its terminal state). So: move `104` out of the `absent` map (`:143-149`), give it a present-assertion with an updated comment ("closed issue → terminal-recording candidate; records merged/closed, no board move"), and change the count assertion (`:155`) from `2` to **3** with expected set `{101, 104, 106}`. Fixtures 102, 103, 105, 107 **stay absent** (105 stays absent because Lane B is closed-only — assert this explicitly, it is the guard that Lane A is unchanged). Add a new closed-issue fixture whose run already has `mr_state='merged'` and assert it is **NOT** re-selected (terminal → Lane B decay bound).
  2. **Watcher unit tests** (`api/internal/forgesvc/mr_watch_test.go`, extend existing fixtures): a completed run on a **closed** issue whose MR reads `'merged'` records `mr_state='merged'` and **moves no card** (`assertNoMove`); same for `'closed'` (records closed, no move); a `NULL`→`merged` bootstrap on a closed issue records `merged`, no move.
  3. **Multi-tick backfill/decay** (offline, fake forge/store): over successive `SyncMRStates` ticks on historical closed-issue runs (`mr_state=NULL`, merged MRs), each records `merged` then drops out (idempotent, decays, no repeated forge read once terminal).
  4. Prove each new assertion non-vacuous via a mutation, per the repo's discipline — **for the sqlc query fold the generated const in `forge.sql.go`, not the `.sql`**; restore from a `cp` backup, never `git checkout --`.
- [ ] **M3 — Gate green + spec note.** `task gate:api` passes (fmt-check, vet, build, lint, deadcode, test with `-race -count=1`). Add a `specs/ai.md` entry (and the PRD Decision Log below) describing the closed-issue terminal-recording lane, the move-free guarantee, and the `LIMIT` rationale. If a maintainer runs the store live-DB suite (`./e2e/run-store-it.sh`, needs Postgres — not part of the worker's gate), confirm the rewritten `TestListMRWatchCandidatesLiveDB` passes with a positive control (named test `--- PASS`, `RUN > 0`, zero `SKIP`); otherwise report it as CI-verified (`test:api-store-it`).

---

## Success criteria

1. **Discriminating fixture (offline, gate-adjacent):** in `TestListMRWatchCandidatesLiveDB`, a completed run on a **closed** issue with `mr_state=NULL` and MR present is now a candidate (set `{101,104,106}`, count 3); the watcher unit test shows one `SyncMRStates` tick records `mr_state='merged'` for a closed-issue merged MR with **no board-card move**. Falsifiable: on `main` today fixture 104 is asserted absent and the set is `{101,106}`.
2. **No regression to board moves / open-issue selection:** Lane A is byte-identical; fixtures 101/105/106 keep their current membership (105 stays absent — Lane B is closed-only); every existing `mr_watch_test.go` case behaves as before.
3. **Backfill + decay:** historical merged runs record `merged` over subsequent ticks and then stop being candidates; the `LIMIT 100` bounds forge reads and never defers Lane A candidates.
4. **Invariant preserved:** `SetRunMRState` remains the sole writer of `runs.mr_state` (`TestMRStateIsWatcherOwned` green); no schema/migration/DTO/web change and no `ListMRWatchCandidates` signature change.
5. **UI reflects it:** with a merged run's `mr_state='merged'` recorded, the runs list and run view show `PR #NNN merged` (confirmed during validation; no frontend edit).
6. `task gate:api` is green.

## Risks & mitigations

- **R1 — Backfill burst of forge reads.** Lane B admits every completed run on a closed issue whose `mr_state` is non-terminal, so the first ticks after deploy may poll many historical MRs (bounded by closed-PRD-issue count, one per issue via DISTINCT-ON). Mitigation: `LIMIT 100`, ordered open-issues-first, spreads backfill across ticks and never defers Lane A; each polled run reaches a terminal state and drops out, so the set decays. Repo scale (hundreds of issues) is within forge rate limits (GitHub 5000/hr) even unbounded, so the LIMIT is defense-in-depth.
- **R2 — A spurious board move on a closed-issue candidate.** None can occur — bootstrap/merged/locked never move, and the edge paths skip a closed issue (Background; proven by existing tests). M2 adds explicit `assertNoMove` coverage.
- **R3 — Evicted issue never backfills.** A closed *and* de-labelled issue can be evicted from the cache, so its run's JOIN fails and it never records `merged`. Accepted (rare); re-labelling restores candidacy. Documented, not fixed here.
- **R4 — `LIMIT` starves closed-issue backfill if Lane A is large.** The open-issues-first ordering means closed backfill only uses slots left after Lane A. Mitigation: `LIMIT 100` far exceeds any realistic Lane-A count (Human-Review/reopen cards only), so backfill always progresses. If a deployment ever had >100 open Lane-A candidates, raise the constant.
- **R5 — `sqlc` regen forgotten.** A `.sql`-only edit is inert. Mitigation: M1 runs `sqlc generate` and commits `forge.sql.go`; CI's `validate:api` asserts the regenerate is a no-op.
- **R6 — LiveDB assertion rewritten wrong (worker cannot run it).** The offline worker edits `TestListMRWatchCandidatesLiveDB` without a Postgres to run it against. Mitigation: M2 pins the exact expected outcome (only fixture 104 flips; set `{101,104,106}`, count 3), and CI's `test:api-store-it` runs it for real; the worker verifies compilation via `task gate:api` (which self-skips the LiveDB test).

## Dependencies

- None. Self-contained api change on `runs.mr_state` (already present) and `ListMRWatchCandidates` (already present). No new package, migration, or external dependency.

## Decision Log

- **D1 — Fix in the candidate query, not a new writer.** `runs.mr_state` is watcher-owned (`TestMRStateIsWatcherOwned`); recording `merged` from a run-status/completion path would break that invariant and risk clobbering the watcher's edge tracking. The watcher already records merged/closed move-free — the only defect is that its query never selects a run once the merge closes the issue. So the fix lives entirely in `ListMRWatchCandidates`.
- **D2 — Lane B scoped to `i.state = 'closed'`, not "any non-terminal run."** The bug is specifically the merge-closed-the-issue race, so the new lane targets closed issues only. This keeps Lane A (open-issue board-move) byte-identical and its candidate set unchanged, flips exactly one live-DB fixture, and avoids polling open non-Human-Review runs every tick. Backfill of all history still works because merged issues are closed.
- **D3 — Bound Lane B on non-terminal `mr_state`, not on a recency window.** "`mr_state` NULL / `opened` / `locked`" makes the set decay to near-empty as MRs settle and backfills **all** history (not just recent). A recency window was rejected: it would permanently exclude older PRs — the exact thing this PRD is asked to fix.
- **D4 — `LIMIT` as a literal constant (100), not a sqlc param.** A param would ripple through the `Querier` interface, the `fakeStore` mock, and both callers; a literal touches zero Go and keeps the `ListMRWatchCandidates(ctx, repoID)` signature and generated row struct unchanged. Ordered open-issues-first so Lane A is never deferred; 100 far exceeds any realistic Lane-A count so closed backfill still progresses.
- **D5 — `locked` kept as a polling candidate.** `locked` is a transient mid-merge state; Lane B keeps polling it so the run settles to `merged` rather than freezing at `locked`. Recording `locked` already never moves a card (`TestMRLockedIsRecordedNeverMoves`).
- **D6 — `ListPRDLinkPatchCandidates` stays separate.** Decision 10 created it because `ListMRWatchCandidates` was open-only and could not do the post-merge PRD-link description patch. #527 only lets the watcher **record `mr_state`** on closed issues; it does **not** fold in the description write (different edge, superseded-run scoping, tenancy-critical write). The two queries remain distinct; the stale cross-reference comments are corrected to say so.
