# PRD #98 — M8 design note: seam 6 (mock↔server fidelity) and M8b (the e2e leg)

**Status**: design only. Nothing here is built. Written 2026-07-21 against `ad6c63d9`
(branch `feature/prd-98-t2-fid`, based on `origin/main`).
**Owner of the parent PRD**: the team lead — this file does not edit `prds/98-judge-menu.md`.

**Why this file and not `adr/`.** `adr/` holds two entries and both are `Status: Accepted
(PRD #N merged)` — durable design records of shipped work. An ADR for unbuilt work would
read as a decision the tree already reflects, which is the "reads as an audit" failure this
PRD's own history keeps producing. This sits next to the PRD it serves and archives with it.

**How to read the evidence markers.** Every factual claim below is tagged MEASURED (I ran it
or opened the file at `ad6c63d9`) or INFERRED (reasoned, not executed). The INFERRED ones are
the ones to attack first.

---

## Part A — Seam 6: the mock↔server fidelity golden fixture

### A0. The actual open problem: the two sides are not at the same layer

MEASURED, both files opened at `ad6c63d9`:

- Go: `GroupJudgeRecommendations(rows []store.ListJudgeRecommendationRowsForUserRow)
  []apitypes.JudgeRecommendationGroupDTO` (`api/internal/workersvc/judge_backlog.go:198`) is a
  pure function over **flat join rows**. Its inputs `disposition_status` and `filed_settled`
  arrive already computed by SQL. The `?run=` anchor and the row cap are **not** in it — they
  are in the query and in `JudgeRecommendationBacklog` (`:156-185`).
- JS: `computeBacklog(bucket, runAnchor)` (`web/src/mocks/mockApi.ts:309`) takes **no rows**.
  It reads the module-level `reviews` array and does *three* jobs in one function: the SQL's
  (order by `updated_at` DESC, resolve the disposition and filed-issue joins per coordinate
  via `bucketOfRec` at `:264`), the grouper's (dedup, rollup, sort), and the anchor filter
  (`:371-373`, a **post-grouping** `groups.filter(...occurrences.some(...))`).

So there is no shared input shape today, and that — not the file format — is the design
question. The PRD's measured harness papered over it by generating rows from `mockReviews`
inside a throwaway script.

Two ways to close it, and only one survives:

- **Fixture in review shape** (the mock's input). Go would have to synthesise flat rows from
  nested reviews — i.e. reimplement the SQL join in Go test code. That is a **third**
  implementation of the thing under test, and it would be the one nothing checks. Rejected.
- **Fixture in flat-row shape** (the Go grouper's input). The JS side needs its grouping half
  extracted into a function that takes rows. That extraction is behaviour-preserving and it
  puts both runtimes at the layer the wire contract already defines. **Chosen.**

### A1. The JS-side extraction (prerequisite, ~1-2h)

From `mockApi.ts`, extract three exported functions that mirror the Go trio one-to-one:

```ts
// mirrors store.ListJudgeRecommendationRowsForUserRow — keys BYTE-IDENTICAL to its json tags
export type JudgeBacklogRow = {
  review_id: string; run_id: string; verdict: string; run_title: string;
  rec_id: string; category: string; target: string; rationale_md: string;
  confidence: string;
  disposition_status: string | null; dismiss_reason?: string | null;
  set_via: string | null; filed_settled: boolean;
  filed_issue_iid: number | null; filed_issue_url: string | null; filed_at: string | null;
};

export function bucketOf(dispositionStatus: string | null, filedSettled: boolean): JudgeBacklogBucket;
//   ^ mirrors workersvc.BucketOf (api/internal/workersvc/triage.go:21)
export function groupJudgeRecommendations(rows: JudgeBacklogRow[]): JudgeRecommendationGroup[];
//   ^ mirrors workersvc.GroupJudgeRecommendations
export function filterGroups(groups: JudgeRecommendationGroup[], bucket: JudgeBacklogBucket): JudgeRecommendationGroup[];
//   ^ mirrors workersvc.filterGroups (judge_backlog.go:303)
```

`computeBacklog` then becomes: build `JudgeBacklogRow[]` from `reviews` in `updated_at` DESC
order → `groupJudgeRecommendations` → anchor filter → `filterGroups` → assemble the DTO with
`computeTriage()`. The row-building step is today's flatten loop with the `occ` construction
moved out; `bucketOfRec(review, cat, tgt)` becomes a lookup that yields
`(disposition_status, filed_settled)` and defers to `bucketOf`.

**The extraction is not plumbing — it is what makes the ladder comparable at all.** Today the
mock's ladder is `bucketOfRec`'s if-chain over *review objects* while Go's is
`BucketOf(string, bool)`. They cannot be fed the same input. After the extraction they can,
and the `dismissed > done > filed > todo` precedence — the PRD's "single most-duplicated logic
across the two implementations" — lands inside the golden fixture's reach.

> **Confirm with the lead before the coder starts.** This edits shipped demo-mode code, and
> `CLAUDE.md`'s "always confirm when changing existing functionality" applies even to a
> behaviour-preserving extraction. My recommendation is to take it: without it, seam 6 can
> only ever compare hand-built adapters.

### A2. The Go side needs no mapper at all — MEASURED

I ran a throwaway probe (a temp package under `api/`, run then deleted; the worktree is clean):

```
dispStatus="done" valid=true setVia="issue_close" filedIID=42 filedAt=2026-07-21 10:00:00 +0000 UTC
unknown-field err = json: unknown field "nope"
remarshal → {"review_id":…,"disposition_status":"done","dismiss_reason":null,"filed_settled":true,…}
```

So `json.NewDecoder(...).DisallowUnknownFields()` decodes the fixture **straight into
`store.ListJudgeRecommendationRowsForUserRow`** — `pgtype.Text`/`Int8`/`Timestamptz` and
`google/uuid` all round-trip, `null` yields `Valid:false`, and an unknown key errors by name.
The generated struct's json tags already carry the SQL column names
(`judge_recommendations.sql.go:90-107`).

Two consequences worth stating precisely, because the loose version overclaims:

- **No hand-written Go mapper exists to drift.** The fixture is typed by the generated struct.
- **This catches a RENAMED or REMOVED column, not an ADDED one.** `DisallowUnknownFields`
  rejects extra keys in the *JSON*; a new struct field the fixture omits decodes to the zero
  value silently. Do not sell it as a schema tripwire in both directions.

`dismiss_reason` may be omitted from fixture rows — the grouper never reads it.

### A3. File layout and ownership

```
fixtures/judge-fidelity/cases.json      # inputs, hand-authored
fixtures/judge-fidelity/expected.json   # outputs, hand-authored
fixtures/judge-fidelity/README.md       # the rules; the no-regenerator decision and why
```

Root-level, **owned by neither runtime**. Not `api/internal/workersvc/testdata/` (that is
where `go test -update` gets added) and not `web/src/mocks/` (that is where
`toMatchSnapshot()` gets added). `go:embed` cannot escape the `api/` module, so the Go test
reads `../../../../fixtures/judge-fidelity/cases.json` by relative path — deterministic,
since `go test` runs with cwd set to the package source dir.

Readers:

- Go: `api/internal/workersvc/judge_backlog_fidelity_test.go`
- vitest: `web/src/mocks/judgeBacklogFidelity.test.ts` (node env, plain `fs.readFileSync`;
  no vite/bundler involvement at test time)

**A missing or unreadable fixture must `t.Fatal` / `throw`, never skip.** A skip here is the
exact false-green shape `CLAUDE.md` records for the live-DB suites.

### A4. The three-artifact structure — the answer to "how does one fixture feed two runtimes"

`cases.json`:

```json
{ "cases": [
  { "name": "occurrences-exceed-run-count",
    "proves": "the same coordinate twice in ONE review — the SQLSTATE 21000 shape; run_count counts DISTINCT runs, occurrences do not",
    "bucket": "all",
    "rows": [ /* ListJudgeRecommendationRowsForUserRow JSON, in query order */ ] }
] }
```

`expected.json`: `{ "<case name>": [ <JudgeRecommendationGroupDTO JSON>, … ] }`, in output
order.

Each runtime, independently: run its own `filterGroups(group(rows), bucket)` over
`case.rows`, serialise, deep-compare against `expected[case.name]`.

**Neither side generates either file.** No `-update` flag on the Go side. No
`toMatchSnapshot()` on the vitest side — vitest *writes* a missing snapshot on first run and
passes, which is precisely the "golden file rots into a snapshot" mechanism the PRD names, so
it is disqualified by construction rather than by discipline.

**Why a third artifact rather than diffing Go against JS directly** (the harness's one-off
shape): a direct diff can only report *that* they disagree, never *which one drifted* — and it
forces one runtime to invoke the other, making `npm test` depend on a Go toolchain. Against a
shared expected file, each suite stands alone and **the failure names the side**: Go red +
vitest green means Go drifted, and vice versa. That asymmetry is the whole value.

**Output-shape discipline.** The comparison is on marshalled JSON, so a Go json-tag change or
a TS key change *is* a failure — desirable. One real hazard, MEASURED at
`api/internal/apitypes/review.go:94-120`: `SetVia` is `json:"set_via,omitempty"` and
`FiledIssue` is `json:"filed_issue,omitempty"`, and the mock uses conditional spreads
(`...(disp?.set_via ? {set_via} : {})`, `mockApi.ts:331-334`) — so both sides **omit** rather
than emit null, and a deep-equal over parsed JSON lines up. The coder must re-verify this
per-field at implementation time; if any field ever emits `null` on one side and is absent on
the other, normalise in the comparison helper and say so at the site.

### A5. The self-check — one case per reimplemented behaviour, and proof each case bites

Two layers, both duplicated in both runtimes (~40 lines each; pure predicates over the parsed
JSON, touching neither implementation). Failure messages follow the shape already in the tree
(`judge_backlog_test.go:326`, `Judge.test.tsx:519`, `judge_bulk_disposition_livedb_test.go:361`):
`fixture broken: <what> — otherwise this test proves nothing about <property>`.

**Input-side (proves the case discriminates), over `cases.json`:**

| # | Case | The predicate that proves it bites |
|---|---|---|
| 1 | dedup across runs | some case has ≥2 rows sharing `(category,target)` with ≥2 distinct `run_id` |
| 2 | `occurrences > run_count` | some case has ≥2 rows sharing `(category,target,run_id)` |
| 3 | partial settle | some coordinate has ≥1 member bucketing `todo` **and** ≥1 not |
| 4 | rollup precedence, **6 pairs** | for each of (dismissed,done) (dismissed,filed) (dismissed,todo) (done,filed) (done,todo) (filed,todo): a coordinate whose member buckets include both, **and — for the pairs not involving `todo` — no `todo` member at all**, else `OpenCount>0` short-circuits and `topRung` is never consulted |
| 5 | sort tie / stability | ≥2 groups tying on `(run_count, open_count)` |
| 6 | bucket filter | ≥2 cases sharing a row set but differing in `bucket`, with different expected group counts |
| 7 | rationale preview | some row's `rationale_md` exceeds 280 runes, and some group's preview is drawn from a **non-first** input row's coordinate |

Row 4 is the gap the PRD measured: the demo fixture "never has to *choose*". The
`no todo member` clause is the part that is easy to get wrong and would silently make the
whole ladder untested.

**Output-side (proves the golden still describes the discrimination), over `expected.json`:**

- case 2: some expected group has `occurrences.length > run_count`
- case 4: each pair's expected group `bucket` equals the higher rung
- case 5: the tied groups appear in first-seen input order
- case 7: the preview ends in `…` and is 281 runes

**Why the output-side half is the anti-regeneration mechanism, and its honest limit.** If
someone regenerates `expected.json` from a *regressed* Go grouper — say `RunCount++` escapes
the `runsSeen` guard — the regenerated golden would show `occurrences.length == run_count` and
the output-side check fatals: *this case no longer exercises occurrences>run_count*. The check
depends on neither implementation, so no regeneration can talk it into agreeing. **It is not
airtight**: a regression that happens to preserve every declared property regenerates cleanly.
The declared property list is the coverage, and it is only as good as row 4's authoring.

### A6. Detection power must be MEASURED at landing, not asserted

Five folds, each **compiled before it is believed** (`CLAUDE.md`'s rule; `npm run typecheck` /
`go vet` settle it in under a minute), each **asserted to have applied**, each restored:

| Fold | Expect |
|---|---|
| JS `BUCKET_RANK` done↔dismissed swapped (`mockApi.ts:299`) | vitest RED, **Go GREEN** |
| Go `bucketRank` `case "done": return 3` (`judge_backlog.go:52`) | Go RED, **vitest GREEN** |
| Go `g.RunCount++` hoisted out of the `runsSeen` guard (`:238-241`) | case 2 RED |
| JS drop the `\|\| (b.open_count - a.open_count)` tiebreak (`:374`) | case 5 RED |
| Go `sort.SliceStable` → `sort.Slice` (`:259`) | **unknown — record what it does** |

The last one is flagged deliberately. Go's `sort.Slice` is pdqsort and is frequently stable in
practice at small N, so this fold may well produce a **false green**. If it does, the honest
record is *"the fixture does not pin sort stability, measured"* — not a quiet omission. The
PRD's rule is to record both halves of an experiment, and this is the half that will be
tempting to leave out.

The first two folds are the ones that justify the third artifact: each reddens exactly one
runtime, which is the property a Go-vs-JS diff cannot deliver.

### A7. What seam 6 CANNOT catch — stated, not implied

1. **The `?run=` anchor.** Go filters **rows pre-grouping** in SQL (a coordinate-level
   `EXISTS`); the mock filters **groups post-grouping** (`mockApi.ts:371-373`). Different
   algorithms at different layers, and the Go half is not in the grouper. → M8b/B3.
2. **The row cap and `truncated`.** Go: `Lim: JudgeBacklogMaxRows + 1` then a slice in
   `JudgeRecommendationBacklog` (`judge_backlog.go:162-170`). Cutting **before** grouping is a
   SQL/service property; the grouper never sees it. → M8b/B4.
3. **The SQL join itself.** `disposition_status` and `filed_settled` are join *outputs* in Go
   and computed lookups in JS. The fixture supplies both as *input*, so the filed-issues
   coordinate-half predicate (still open in the PRD) is invisible here.
4. **The query's `ORDER BY`.** The fixture *declares* row order. A drift in
   `ORDER BY rv.updated_at DESC` — which decides which occurrence supplies
   `rationale_preview` — is not observable.
5. **`scope=open`** is a *bulk-disposition* property (`BulkSetDispositions` /
   `bulkSetJudgeDisposition`), not a grouper one. It is on the lead's seam-6 list; it does not
   belong at this seam. → M8b/B5. Flagging this rather than quietly satisfying it with a
   look-alike case.
6. **The demo data.** The fixture is authored, so `mockReviews` can still be unrepresentative
   and nothing here would say so.

### A8. Second finding — truncation is unreachable in demo mode

MEASURED at `ad6c63d9`: the hardcodes are `mockApi.ts:381` (`computeBacklog`) and
**`mockApi.ts:1982`** (`bulkSetJudgeDisposition`). **The PRD's `:1812` is stale** — correcting
it here rather than copying it forward.

The fix is not a cosmetic banner. The server's cap cuts **rows before grouping**, so the
honest demo state (per the PRD's Risks section) is a *surviving group under-reporting
`run_count`, possibly rolling up to the wrong bucket and vanishing from `?bucket=todo`*. A
mock that flips a boolean without cutting rows would demo a lie about a state whose whole
subtlety is that it understates surviving groups.

Shape: a `MOCK_BACKLOG_MAX_ROWS` applied at the **row** level inside `computeBacklog`, before
grouping, mirroring the server; `truncated` derived from it; the same value plumbed to the
bulk-disposition re-read at `:1982`.

> **PRODUCT DECISION for the user — I am not choosing this.** What value makes the state
> *demoable*? (a) a cap below `data.ts`'s row count, which permanently truncates the demo;
> (b) a cap only tests can lower, which satisfies "every state has a test" but leaves a human
> clicking the demo unable to see it — and M3's requirement is that the **mock renders every
> state**; (c) a dev toggle (a query param or a demo-state switch) that turns it on
> deliberately. My recommendation is **(c)**: it is the only one that makes the state visible
> to a person without making the demo permanently wrong.

### A9. Effort

| | |
|---|---|
| JS extraction (A1) | 1-2h |
| Authoring `cases.json` + `expected.json` (9 cases; the expected DTOs are the bulk) | 3-4h |
| Two harnesses (A3/A4) | 2h |
| Self-checks, both runtimes (A5) | 1.5h |
| Mutation measurement (A6) | 1.5h |
| **Seam 6 total** | **~1.5 days** |
| Truncation fix (A8), after the product decision | 2h |

---

## Part B — M8b: the e2e leg in `e2e/run-e2e.sh`

Conventions matched from the file at `ad6c63d9`, not invented: `say`/`pass`/`fail` (`:192-194`),
`apiget`/`apipost`/`apipost_code` (`:256-308`), `retry_read` (reads only, `:239`), `create_run`
(`:271`), `db_psql` (`:2530`), `fake_post`/`fake_state`/`flip_mr` (`:497-506`), `wait_eq`
(`:422`), `record_margin` (`:402`), `uzi_cli` (`:1407`). Shebang is
`#!/usr/bin/env bash` (MEASURED) — bash-isms are legal *in the harness*; the zsh trap in
`.claude/agent-team.md` is about the agent's own shell.

### B0. Placement — reuse ~5 minutes of already-paid setup

MEASURED: by `:2023` the poller is already at `E2E_FORGE_POLL_INTERVAL=2s` +
`FORGE_RECONCILE_EVERY=2` (a ~4s reconcile period), and the PRD #46/#68 phase at `:2663-2807`
already leaves in place:

- `$J_RUN` — a completed, judged run; `$F_REVIEW` — its review
- `$F_REC` — a seeded `install_worker_tool/jq` recommendation
- `$F_IID` — a **real** filed forge issue, PRD+PRDLESS-labelled, with the #68 link persisted

Insert the #98 phase **after `pass "cleaned up the filed-issue run …"` and before the
judge-disable restore** (`:2803`). MEASURED that the close-sync is not gated on
`judge_enabled`: `poller.go:251` and `forgesvc/judge_issue_close.go:44` take only a `repoID`.
Placing it before the restore is still preferable — it keeps a second judged run cheap.

### B1-B8. The assertion blocks

Scoped, per the lead, to *assertions fakes structurally cannot make*. A happy-path walkthrough
is explicitly not wanted.

**B1 — dedup grouping.** `apiget "/api/me/judge/recommendations?bucket=all"` (MEASURED route,
`apitypes/review.go:147` — **not** `/backlog`). Assert exactly one group for a coordinate
shared by two reviews, `run_count == 2`, `occurrences|length == 2`, two distinct `run_id`s.
*Precondition or vacuous*: `db_psql` count of that coordinate across the two reviews **is 2** —
otherwise "exactly one group" is satisfied by there being only one row.

**B2 — `occurrences > run_count` on the wire.** Seed the same coordinate **twice on one
review** (MEASURED: no UNIQUE on `(review_id, category, target)` — the harness documents this
itself at `:2738`). Assert `run_count == 2` while `occurrences|length == 3`. This is the shape
that crashed the endpoint with SQLSTATE 21000, and the wire is the only place the whole path
(join fan-out → grouper → DTO) meets it.

**B3 — the `?run=` anchor's coordinate-level `EXISTS`** — the SQL half seam 6 cannot reach.
`apiget "/api/me/judge/recommendations?bucket=all&run=$J_RUN"`, then **three** assertions,
because only the third separates SQL's row-level filter from a group-level one:

- (a) the shared coordinate is **present**;
- (b) a coordinate existing **only** on the other run is **absent** — proves filtering happened.
  *Precondition*: assert that coordinate **is** present in the unanchored read first, or (b) is
  vacuous;
- (c) the kept group still carries its **other-run** occurrence (`occurrences|length == 2`) —
  rows are filtered pre-grouping by a coordinate-level `EXISTS`, not by dropping other-run rows.

(c) is the assertion the Go-grouper harness structurally could not make.

**B4 — the row cap cuts BEFORE grouping.** `JudgeBacklogMaxRows = 2000` is a compile-time
const (MEASURED, `judge_backlog.go:136`) — not env-tunable, so the only way to reach
`truncated: true` is to seed 2001 rows. One `INSERT … SELECT FROM generate_series` — INFERRED
sub-second, verify. Assert `truncated == true`, and that a coordinate seeded only in the
**oldest** review is absent while its group would exist had the cut been post-grouping.

Containment, because 2001 rows would otherwise poison every later backlog read: seed them on a
**dedicated review**, run this block **last** in the #98 phase, then `DELETE` the review and
re-assert `truncated == false`. **The delete-and-recheck is the positive control** that the
2001 rows were what caused the flag — without it, a `truncated:true` from some unrelated cause
is indistinguishable.
Honest limit: this pins the flag and the cut ordering, not the `LIMIT`/`ORDER BY` interaction
at scale.

**B5 — group Dismiss fans out across runs.** POST the bulk disposition at the group coordinate
with `scope=open`. Assert `updated == 2`; `settled` carries two distinct `(run_id, rec_id)`
pairs; and a member **pre-settled before the call** is **not** in `settled` (the `scope=open`
property). *Precondition*: assert that member is settled first.

> **The "drops an `improve_uzi` rec from the self-improve backlog" half has no wire
> observable.** MEASURED: `ListOpenImproveUziRecommendations` is consumed only by
> `selfimprove/engine.go:225`, which folds it into a composed run description; the only
> `/selfimprove` routes are the admin config GET/PUT (`handler.go:566,579`). There is no
> trigger endpoint. **Do not manufacture a `db_psql` copy of the query** — asserting a
> hand-written duplicate of the SQL under test proves nothing. The property is already pinned
> at the live-DB layer (`TestFiledIssueCloseAutoDonesOnceLiveDB`, the PRD item closed at
> `0da9186a`). State the boundary in the phase's comment; do not claim it.

**B6 — issue-close → Done, edge-once, Undo sticks, dismissed not overwritten.**

*This block needs a new forge-fake mutator.* MEASURED at `ad6c63d9`: `forge-fake.mjs` sets
`state: "opened"` at creation (`:195`) and **no route ever mutates it** — the GitLab
`PUT /issues/{iid}` handler (`:954-968`) applies only `add_labels`/`remove_labels`. The MR
analogue exists (`/_e2e/mrs/{iid}/state` → `flip_mr`, `:501`); the issue analogue does not.

Add `POST /_e2e/issues/{iid}/state {state}` mirroring the MR mutator, plus a `close_issue`
helper. **It must bump `issue.updated_at`**: `IncrementalSync` is high-water-marked on
`updated_at`, so a close that does not bump it is invisible until the next `FullSync`. With
`FORGE_RECONCILE_EVERY=2` the reconcile would mask that within ~4s — which is exactly why the
bug would be invisible *in the harness* while being real against a live forge. Bump it, and
record that reason in the mutator's comment.

Then, on `$F_IID` (already filed against `$F_REC`):

- *precondition*: the coordinate buckets `filed`, `close_synced_at IS NULL`, no disposition row;
- `close_issue $F_IID`; poll `db_psql` in `wait_eq` style for the disposition to appear;
- assert the provenance triple: `status='done'`, `set_via='issue_close'`,
  `set_by_user_id IS NULL`;
- assert it left To triage: the group's bucket is `done` **and** `triage.todo` fell by exactly 1;
- **edge-once**: capture `set_at`, wait ≥2 reconcile periods, assert `set_at` **unchanged** and
  `close_synced_at` stamped;
- **Undo sticks**: delete the disposition, wait ≥2 reconcile periods, assert **no** row returns;
- **dismissed not overwritten**: a *second* filed coordinate, hand-dismissed **before** its
  issue closes; close it; assert the disposition is still `dismissed` **and**
  `close_synced_at` **is** stamped — the `ON CONFLICT DO NOTHING` half, where the edge is
  consumed without writing.

> **The strongest design point in B6, and the easiest to omit.** Both negative windows
> (edge-once, Undo-sticks) assert that *nothing happened during a wait*. A poller that
> **crashed** after the first edge produces a byte-identical green. A bare `sleep 8` is
> therefore not evidence — it is the same false-green family as a live-DB suite that ran
> nothing. **Positive-control the window**: arrange a *second, independent* close edge that
> must be processed inside the same wait, and assert it landed. Only then does "the disposition
> did not come back" mean the poller ran and declined, rather than the poller being gone.
> The harness's existing 2-tick negative window (`:2035-2040`) does not carry this, and it is
> the pattern this block must improve on rather than copy.

**B7 — the notification deep-links to `/judge?run=`.** **Open the DTO before writing this
block.** MEASURED that `notificationLink` is a *web* function (`web/src/lib/notifications.ts`,
unit-covered by `notifications.test.ts`). If the deep link is computed client-side, there is
nothing on the wire to assert and the only honest e2e assertion is that the `judge_review`
notification carries `run_id` — which the #46 phase already makes at `:2709`. **In that case
drop the block and say so**, rather than assert a proxy. Asserting presence where efficacy was
claimed is the exact failure `.claude/agent-team.md` records for the `title`-attribute pass.

**B8 — no token spend on any disposition.** Follow the steer phase verbatim (`:1690-1701`):
fake MR count unchanged, no branch pushed, `run.usage` zero on both target runs, and
`SELECT count(*) FROM runs` unchanged across the whole #98 phase. Cite
`TestDispositionTouchesStoreOnly` alongside it — a positive store-call allowlist is
*structurally stronger* than a before/after count, and the e2e block must not be credited with
more than it proves.

### B9. The printed-instruction backstop — the EXECUTING half

**Where it lives: `e2e/run-e2e.sh`. Not a build-tagged Go test.** Reasons from the code, not
preference:

- The instructions are emitted *against a live API*. `runGroupDisposition` prints one undo
  address per member of the server's `settled` array (`api/cmd/uzi/review.go:447`); the
  truncation remedy fires on the server's `truncated` flag (`:403`). A build-tagged Go test
  would need the same booted stack — at which point it is the e2e harness with more machinery.
- The harness already has everything: `uzi_cli` (hermetic `env -i`, `$UZI_TOKEN_VAL`,
  `:1407`), a built binary (`:1391`), and `db_psql` for outcome assertions.
- A third long-running suite would join `run-e2e.sh` and `run-store-it.sh` with no runner —
  and the second already carries an unexplained intermittent failure nobody has diagnosed.

**Row shape (arrange → produce → extract → assert):**

```bash
# arrange — and assert the precondition, or the row is vacuous
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE …")" = 0 ] || fail "…"
[ "$(db_psql "SELECT count(*) FROM review_recommendations WHERE …")" = 2 ] || fail "…"

# produce — capture the stdout of the command that EMITS the instruction, nothing else
OUT="$(uzi_cli review dismiss --category "$CAT" --target "$TGT" --reason wont-do)"

# extract — lift EVERY match, assert the COUNT before using any of them
mapfile -t INSTR < <(printf '%s\n' "$OUT" | sed -n 's/^ *\(uzi review undo [0-9a-f-]\{36\} [0-9a-f-]\{36\}\)$/\1/p')
[ "${#INSTR[@]}" = 2 ] || fail "expected 2 printed undo addresses, got ${#INSTR[@]}: $OUT"

# execute the printed text VERBATIM — never a hand-written copy
for cmd in "${INSTR[@]}"; do eval "uzi_cli ${cmd#uzi }" >/dev/null || fail "printed instruction failed: $cmd"; done

# assert the OUTCOME, not the exit code
[ "$(db_psql "SELECT count(*) FROM recommendation_dispositions WHERE …")" = 0 ] || fail "…"
```

Three constraints that are not stylistic:

- **Never `head -1` on the extraction.** `.claude/agent-team.md`'s "a truncated view is not the
  output" — output that stops at your limit is indistinguishable from output that ended.
  Assert the count.
- **`eval` is how you stay verbatim**, and the regex above (fixed-shape UUIDs, line-anchored)
  is what makes it safe: `eval` can never see text that did not match. Both previously-false
  instructions *parsed perfectly*, so any hand-splitting reintroduces the copy the mechanism
  exists to forbid.
- **Bind each row to the command that EMITS it.** The capture is one command's stdout, never a
  global scan — running an instruction against the wrong command manufactures a false finding
  exactly as reading it manufactures a false pass.

**Which instructions M8b can actually close** (re-derived at `ad6c63d9`):

| Printed at | Text | Closable? |
|---|---|---|
| `review.go:447` | `  uzi review undo <run> <rec>`, per settled member | **Yes, fully.** Arrange 2 open members → dismiss → extract 2 → execute both → assert both rows gone **and** `triage.todo` back up by 2 |
| `review.go:403` | backticked `uzi review backlog --run <run-id>` truncation remedy | **Yes, but only with B4's 2001-row seed in place.** Run the group disposition against the truncated backlog, extract the remedy, execute it verbatim, assert the re-read is **not** truncated. This is what makes B4 worth its cost — it is the only arrangement in the repo where this remedy can be executed at all |
| `review.go:286` | `warning: backlog truncated at the server's row cap …` | Prose, no command — not a registry row |
| `uzi login` / `repo list` / `run inputs` / `skill install` | the four inherited `NOT EXECUTED` entries | Out of scope; leave the honest notes |

### B10. How the registry learns a row exists

It does **not** learn automatically, and it must not: an entry is a human's claim that they
executed the thing, and `instructions_test.go:38-41` says so. What *can* be mechanised is
**binding the claim to a harness phase that still exists**. Add to `knownInstruction`:

```go
type knownInstruction struct {
    command  string
    evidence evidenceKind // evidenceNone | evidenceGoTest | evidenceE2E
    where    string       // a Go test NAME, or the e2e phase's `say` label
    note     string
}
```

plus a third test in the same file, `TestInstructionEvidenceAddressesStillExist`:

- `evidenceE2E` → read `../../../e2e/run-e2e.sh`; fail unless `where` appears in it;
- `evidenceGoTest` → fail unless `func <where>(` exists under `api/` (the file already walks
  ASTs; a grep is fine);
- `evidenceNone` → require a non-empty `note` — the honest-gap rule made checkable;
- **positive control**: fail if `run-e2e.sh` cannot be *read at all*. A moved or renamed harness
  must redden this test, not silently satisfy it. (Do **not** assert "≥1 entry is
  `evidenceE2E`" — that would fail today, before M8b lands.)

**What this proves and what it does not.** It proves the claim's *address* is live. It does
**not** prove the phase asserts anything — same honest boundary the query-inventory test
carries. It closes the reverse-rot direction for the evidence side that
`TestRegisteredInstructionsAreStillPrinted` closes for the printing side.

Ride-along cleanup the same commit should carry: the existing notes cite
`worker_test.go:56-82` by **file and line**. `.claude/agent-team.md` rule 2 — a line number is
meaningless without a SHA; cite the assertion by name. Converting those to `where` test names
is what makes them checkable at all.

### B11. Runtime cost

The existing harness is ~30 min. Added by the #98 phase:

| | |
|---|---|
| one extra judged run (create issue → complete → funnel → judge claim → review lands) | 90-120s |
| 2001-row seed + read + delete | <5s (INFERRED — verify) |
| three close-edge waits @ ~2 reconcile periods | 20-25s |
| Undo-sticks negative window **with its positive control** | ~10s |
| CLI instruction rows | ~5s |
| remaining API round-trips | ~10s |
| **total** | **+3 to +4 min → ~34 min** |

The dominant term is the second judged run. It can be dropped to **+1.5 min** by direct-seeding
a second `run_reviews` row via `db_psql` (the harness's own precedent, `:2732`). **I recommend
paying the 90s**: the dedup property is about two genuinely judged runs, and the funnel is
precisely what a fake cannot fake.

### B12. Effort

| | |
|---|---|
| forge-fake `/_e2e/issues/{iid}/state` mutator + `close_issue` helper | 1h |
| B1-B3 | 2h |
| B4 (seed, containment, delete-and-recheck control) | 1.5h |
| B5 | 2h |
| B6 (the edge/undo/dismissed matrix with real positive controls) | 4h |
| B7 (or its documented drop) | 0.5h |
| B8 | 1h |
| B9 instruction rows | 2h |
| B10 registry change + third test + citation cleanup | 1.5h |
| iteration against a 30-min harness — budget ≥3 full runs | 2× write time |
| **M8b total** | **~3 days** |

---

## Open questions for the user (product), via the lead

1. **A8 — demo-mode truncation.** Permanently-truncated demo, test-only reachability, or a
   deliberate dev toggle? M3's requirement is that the *mock renders every state*, and only the
   toggle satisfies that for a human. Recommend the toggle.
2. **A1 — the `mockApi.ts` extraction** is behaviour-preserving but edits shipped demo code;
   `CLAUDE.md` wants a confirmation. Recommend taking it — seam 6 has no other honest shape.
3. **B11** — pay 90s of e2e for a second real judged run, or direct-seed it? Recommend paying.

## What this design cannot catch, in one place

Seam 6: the `?run=` anchor, the row cap and `truncated`, the SQL join producing
`disposition_status`/`filed_settled`, the query's `ORDER BY`, `scope=open`, and the
representativeness of `data.ts` itself.
M8b: the self-improve backlog drop (no wire observable), the `LIMIT`/`ORDER BY` interaction at
scale, and — for B10 — whether a registered phase *asserts* anything, as opposed to existing.
