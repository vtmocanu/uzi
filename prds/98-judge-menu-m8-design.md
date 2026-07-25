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

## Revision log

**v1 (`4c647f84`)** — first pass, both mechanisms.

**v2 (`e30f97e0`)** — seam 6 revised in place after four follow-ups and four user rulings.
What changed, and why each change was forced rather than chosen:

- **A10 is new.** A real mock↔server divergence exists *today* in `rationalePreview` (runes vs
  UTF-16 code units, plus a differing trim set). The user ruled **fix the mock, in two
  separate changes**. Until that ruling, `expected.json` was unauthorable — with a live
  divergence there is no single expected output. It is authorable now: **the expected output
  is the server's.**
- **A5 gains three mandatory non-ASCII rows.** v1's preview row was ASCII-only, which would
  have produced a fixture whose green is load-bearing on its own ASCII-ness — certifying a
  fidelity the code does not have. That is the "snapshot that rots" failure arriving by a
  route the PRD item did not anticipate, and v1 walked into it.
- **A6's sort row is no longer a caveat.** v1 said `sort.SliceStable → sort.Slice` "may
  produce a false green" and left it there. MEASURED since: it produces a false green for
  *every fixture shape anyone would naturally write*, and diverges only under a specific,
  unnatural one. The row now specifies that shape exactly. A caveat attached to a green is
  the thing this branch keeps deleting; this replaces it with a construction rule.
- **A1 and A10 are AUTHORISED** by the user — the extraction and the mock fix both land.
- **Part B/B8 is marked DEAD.** The no-token-spend e2e assertion cannot fail. Left in place
  with its refutation rather than deleted, per the rule that a removed check with no recorded
  reason gets re-added by the next person who reads the PRD's criteria list.
- **Part B is otherwise unrevised** and awaits pass 3. Do not build from it yet.
  *(Superseded by v4 below — Part B is now buildable. Kept as the v2 record, not as advice.)*

**v3 (`2639af95`)** — **Part C is new**: the printed-instruction backstop, pass 2 of the
approved three-way split. Everything in it is measured by execution against `api/cmd/uzi/`,
not by reading. It supersedes v1's B9/B10, which are marked as such in place. The headline: the
registry conflates **two kinds** of string — runtime emissions and help references — and the
"must have been EXECUTED" bar is right for the first and wrong for the second. Four of the
eight entries' notes misdescribe their own site.

**v4 (this commit)** — **Part B is now v2 and buildable** (pass 3, the last). B8 struck with
its refutation in place; B6 carries the forge-fake mutator design plus **a correction to v1's
own stated mechanism** — the fake ignores `updated_after` entirely, so the `updated_at` bump is
about the fake not lying about the forge, not about making the harness pass. That correction
exposes a limit worth more than the mutator: **the e2e leg structurally cannot exercise the
incremental-sync path at all.** B6a makes the positive-controlled negative window concrete (a
probe edge per window, since an edge is consumed once). B4 is re-priced — Part C's truncation
row gated on it makes it the **last** block to cut, inverting v1's ordering. Also found:
`apidelete` is referenced in the harness's own `:235` comment and **never defined**.

**Correction to v1's reporting, recorded because the error was mine.** I attributed the lead's
stale status read to the persisted-cwd trap in `.claude/agent-team.md`. That was wrong, and the
lead disproved it: its check ran in `prd-98-fid` and reported `ad6c63d9` while `prd-98` was at
`bb4a66ac`, so a wrong-tree read would have shown *those* commits. The real mechanism was
staleness — the check ran minutes before the commit. **A correct rule applied to the wrong
incident is worse than no rule**, because it ships with evidence attached and sends the next
reader to guard a mechanism that was not involved.

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
| 8 | **multibyte cut — REQUIRED, not optional** | some row's `rationale_md` has **rune count ≠ UTF-16 code-unit count** *and* exceeds 280 runes, so the cut lands past the point where the two counts diverge |
| 9 | **multibyte no-cut** | some row is >280 code units but **≤280 runes** — the case that separates a fixed mock from an unfixed one, since both cut identically once past 280 runes |
| 10 | **trim-set boundary** | some row's rune 280 is one of NBSP / U+FEFF / U+2028 — the characters JS `\s` trims and Go's `" \t\r\n"` does not |

Row 4 is the gap the PRD measured: the demo fixture "never has to *choose*". The
`no todo member` clause is the part that is easy to get wrong and would silently make the
whole ladder untested.

**Output-side (proves the golden still describes the discrimination), over `expected.json`:**

- case 2: some expected group has `occurrences.length > run_count`
- case 4: each pair's expected group `bucket` equals the higher rung
- case 5: the tied groups appear in first-seen input order
- case 7: the preview ends in `…` and is 281 runes
- case 8: the preview is **exactly 281 runes** and its rune count **differs** from its UTF-16
  code-unit count — the second clause is what fails if someone "simplifies" the case to ASCII
- case 9: the preview is byte-identical to the input `rationale_md` (no `…`), proving the
  no-cut branch was taken on a string that a code-unit implementation would have cut
- case 10: the preview's final rune (before `…`) **is** the NBSP/U+FEFF/U+2028 — i.e. Go kept
  what JS `\s` would have stripped
- case 11: the 13 groups appear in the fixture's own first-seen order within each `run_count`

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
| Go `sort.SliceStable` → `sort.Slice` (`:259`) | **RED only under the case-11 shape below — see A6a** |
| JS `Array.from(s)` → `s` in `rationalePreview` (post-fix) | case 8/9 RED, Go GREEN |
| JS trim `/[ \t\r\n]+$/` → `/[\s]+$/` (post-fix) | case 10 RED, Go GREEN |

The last one is flagged deliberately. Go's `sort.Slice` is pdqsort and is frequently stable in
practice at small N, so this fold may well produce a **false green**. If it does, the honest
record is *"the fixture does not pin sort stability, measured"* — not a quiet omission. The
PRD's rule is to record both halves of an experiment, and this is the half that will be
tempting to leave out.

The first two folds are the ones that justify the third artifact: each reddens exactly one
runtime, which is the property a Go-vs-JS diff cannot deliver.

### A6a. Sort stability — MEASURED, and it is a construction rule, not a caveat

v1 said `sort.SliceStable → sort.Slice` "may produce a false green" and stopped. That was a
caveat attached to a green, which is the shape this branch keeps deleting. Measured instead
(Go probe, run at `ad6c63d9` then deleted; `sort.Slice` vs `sort.SliceStable` over the real
comparator `run_count DESC, open_count DESC`):

| fixture shape | n | `sort.Slice` diverges from `SliceStable`? |
|---|---|---|
| **all groups tied** on `(run_count, open_count)` | 2…200 | **NEVER** |
| **realistic backlog** — 2 groups at `run_count=3`, 3 at `2`, rest at `1`, in that order | 8, 12, 13, 16, 20 | **NEVER** |
| **interleaved two-key** — `run_count` alternating `1,2,1,2,…` | ≤12 | never |
| **interleaved two-key** | **13** | **YES** — `[9 1 3 5 7 11 2 4 6 8 0 10 12]` |

**THE FINDING, and it is more useful than the fixture spec below: the obvious fixture is the
one that cannot catch this.** "Many tied groups will catch a stability regression" is what
almost anyone would write, and it is precisely wrong — **the mutation produces a false green
for every fixture shape anyone would naturally reach for**, including the two most obvious
ones (all-tied, and a realistic recency-ordered backlog), at any n. Two mechanisms, both in
Go's pdqsort: below n=12 it uses insertion sort, which is stable; and it short-circuits on
input it detects as already-ordered — which is exactly what those two shapes are. A fixture
author optimising for realism optimises directly away from detection here.

**The construction rule, therefore — case 11:** the stability case needs **≥13 groups**, **at
least two distinct `run_count` values**, **ties within each value**, and an input order that
is **NOT already sorted by `run_count`** (interleave them). All four clauses are load-bearing;
drop any one and the mutation goes green.

**Two consequences the coder must carry into the fixture file:**

- The case is **deliberately unrealistic**, and that is the entire point. It needs a
  do-not-tidy note at the site, in the shape `.claude/agent-team.md` already documents for
  the shared-coordinate fixtures: *reordering these groups into recency order silently
  deletes the only thing pinning sort stability.* Someone will otherwise "fix" it to look
  like a real backlog and never know.
- It is the **cheapest** case despite being the largest: 13 groups × 1 row each, all
  `open_count=0`, no occurrence variety needed. ~13 rows in and 13 small groups out.

**What this pins and what it does not.** It pins the **Go** side. The JS side is not at risk
from the same mutation: `Array.prototype.sort` has been **required** to be stable since ES2019,
so JS stability is a language guarantee rather than a call-site choice. The JS ordering risk is
a comparator change, and the case-5 tiebreak-drop fold already covers that. Say this at the
site so nobody later "balances" the design by adding a JS stability mutation that cannot fail.

**If the lead would rather not pay 13 groups:** the honest alternative is to declare sort
stability **unpinned**, with the reason (`sort.Slice` is stable at this scale for every natural
shape, so the property is real but unreachable by a proportionate fixture). An unpinned
property named honestly is fine. What is not fine is the v1 text — a caveat that reads as
partial coverage.

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

### A10. The rune/code-unit divergence — RULED: fix the mock, in TWO changes

This is a **live defect**, not a hypothetical, and it is the reason `expected.json` was
unauthorable until the user ruled. I re-derived both sides myself rather than relaying.

**MEASURED — Go `rationalePreview` (`judge_backlog.go:77-83`), probe run at `ad6c63d9`:**

| input | cut? | out runes | valid UTF-8 |
|---|---|---|---|
| 200× U+1F680 | **false** | 200 | yes |
| `"a"` + 200× U+1F680 | **false** | 201 | yes |
| 300× U+1F680 | true | 281 | yes |
| `"a"` + 300× U+1F680 | true | 281 | yes |

**MEASURED — JS `rationalePreview` (`mockApi.ts:294-296`), same inputs, node:**

| input | cur cut? | cur out runes | cur lone surrogate | fixed cut? | fixed out runes | fixed lone surrogate |
|---|---|---|---|---|---|---|
| 200× U+1F680 | **true** | **141** | no | false | 200 | no |
| `"a"` + 200× | **true** | **142** | **YES** | false | 201 | no |
| 300× U+1F680 | true | **141** | no | true | 281 | no |
| `"a"` + 300× | true | **142** | **YES** | true | 281 | no |

Three distinct defects, not one:

1. **Different answer to "was this cut".** At 200 emoji Go returns the string whole and JS
   truncates. Not rounding — a different boolean.
2. **Different cut LENGTH even when both cut.** At 300 emoji Go yields 281 runes, JS yields 141.
3. **A lone surrogate**, exactly the broken glyph `judge_backlog.go:64-65` says the rune count
   exists to prevent. Confirmed by scanning the **whole** output string. (My first probe checked
   `o.slice(-3)`, which splits a surrogate pair by itself and reported a false positive on the
   *fixed* path — an instrument artifact, caught and discarded before it reached this file. Worth
   recording: the measurement tool manufactured the very defect it was looking for.)

**MEASURED — trim set**, padding rune 280 with each character and asking whether it survives:

| pad | Go trims? | JS current trims? | JS with `/[ \t\r\n]+$/` |
|---|---|---|---|
| SPACE, TAB | yes | yes | yes |
| NBSP U+00A0 | **no** | **yes** | no |
| BOM U+FEFF | **no** | **yes** | no |
| U+2028 | **no** | **yes** | no |

**RULING (user, relayed by the lead): fix the mock. It is TWO changes, and that framing is the
ruling, not a gloss.**

- **Change 1** — `Array.from(s)` / `[...s]` for both the length test and the slice. Fixes
  defects 1, 2 and 3 together (measured above: all three go away).
- **Change 2** — replace `/[\s]+$/` with `/[ \t\r\n]+$/`. **Change 1 does not touch this.** A
  fix that stops at the spread operator leaves the trim divergence live, and a golden fixture
  authored afterwards would go green with a real divergence still in the tree.

**Consequence for `expected.json`: the expected output is the SERVER's.** No exception is
encoded, and the fixture is not written against current mock behaviour.

**The trap this creates, and it is the one to state loudest.** Post-fix, both sides cut at the
same place — so a fixture authored against the OLD behaviour, or authored in ASCII, is
**indistinguishable from a fixture that pins nothing**. That is why A5 rows 8-10 are marked
REQUIRED rather than optional, and why row 9 (>280 code units, ≤280 runes) exists at all: it is
the *only* shape that separates a fixed mock from an unfixed one, because once a string exceeds
280 runes both implementations cut and the outputs converge.

**Carry the clamp offset into any escaping case.** The cut is at 280 **runes** server-side. A
hostile multibyte payload therefore lands at a different offset than a naive code-unit count
predicts, so an escaping fixture that counts characters the wrong way tests a different string
than it thinks and calls the two one string. Any case combining untrusted text with the preview
cap must state which count it is using.

**Scope note.** The fix lands in `mockApi.ts`, which this branch owns, so it can ride the same
wave as the A1 extraction. It is a **behaviour** change to shipped demo code — authorised here,
recorded so nobody re-litigates it at review.

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

Revised for v2 — the fixture grew from 9 cases to 11, three of them multibyte and one of them
13 groups wide.

| | |
|---|---|
| JS extraction (A1) — authorised | 1-2h |
| **Mock `rationalePreview` fix (A10) — TWO changes** | 0.5h |
| Authoring `cases.json` + `expected.json` (11 cases; the expected DTOs are the bulk) | 4-5h |
| — of which case 11 (13 interleaved groups) | ~1h |
| — of which cases 8-10 (multibyte; each needs its rune/code-unit counts computed, not eyeballed) | ~1.5h |
| Two harnesses (A3/A4) | 2h |
| Self-checks, both runtimes (A5) | 2h |
| Mutation measurement (A6/A6a) — now 7 folds | 2h |
| **Seam 6 total** | **~2 days** |
| Truncation fix (A8), after the remaining product decision | 2h |

**Two authoring RULES, not tips. Both of these produced FALSE RESULTS rather than errors,
which is why they are rules — an error stops you, a false result gets published.**

1. **Build exotic characters by code point, never by pasting the glyph.**
   `string(rune(0xFEFF))` / `"\u{FEFF}"`. Measured: writing a literal U+FEFF through a shell
   heredoc silently produced a raw BOM, which Go then refuses to compile **anywhere** in a
   source file — three probe attempts died on it before the cause was visible. This is the
   repo's standing "read a file back after a shell heredoc" rule, earned again in a new place.
2. **Compute expected previews; never count them by eye or by a convenience slice.** Measured:
   a probe reading `o.slice(-3)` to inspect a tail **manufactured the very lone surrogate it
   was testing for** — `slice` is code-unit based, so it split a surrogate pair by itself and
   reported a defect on the *fixed* implementation. An instrument that produces the defect it
   is hunting returns a plausible, wrong, publishable answer. Scan whole strings, not slices,
   and derive the rune index, the code-unit index and the trimmed tail programmatically.

---

## Part B — M8b: the e2e leg in `e2e/run-e2e.sh`

> **PART B IS v2 (pass 3) AND IS READY TO BUILD.** B8 is struck with its refutation in place;
> B6 carries the mutator design, a correction to v1's own mechanism, and the
> positive-controlled negative window (B6a) made concrete.
>
> **Base re-derived before designing, not assumed.** At `2639af95`, `main` is still
> `ad6c63d9`, and the three in-flight branches were checked by diff rather than by trust:
> `feature/prd-98-t2-lim` is **one new test file** (`route_limiter_mounts_test.go`, 699 lines,
> no route changes — the admin CLI-token endpoint the lead mentioned has **not** landed on it
> yet), `feature/prd-98-t2-web` is the N2 tests plus a small component change, and
> `feature/prd-98-t2-seam6` touches **only** `web/src/mocks/mockApi.ts`. **Zero migrations on
> any of them.** So `e2e/run-e2e.sh`, the poller, `forgesvc` and the judge routes are
> byte-identical across all four branches, and Part B's measurements still hold — established
> by running the falsification, since an unchanged path only fails to falsify and the
> environment (the migration set) is the half that moves without appearing in a diff anyone
> would think to run.
>
> **Watch item, not a blocker:** the admin CLI-token inventory endpoint is *being built* on
> the lim branch. Nothing in Part B touches the admin surface, so it does not interact today —
> but if that endpoint lands with new routes, re-run the diff above before writing B1-B8.

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

**B4 — the row cap cuts BEFORE grouping. NOW HAS TWO CONSUMERS.** `JudgeBacklogMaxRows = 2000`
is a compile-time const (MEASURED, `judge_backlog.go:136`) — not env-tunable, so the only way
to reach `truncated: true` is to seed 2001 rows. One `INSERT … SELECT FROM generate_series` —
INFERRED sub-second, verify. Assert `truncated == true`, and that a coordinate seeded only in
the **oldest** review is absent while its group would exist had the cut been post-grouping.

> **Re-price this block: it is no longer only about proving the cap.** Part C's
> truncation-remedy row (`uzi review backlog --run <run-id>`, `review.go:403`) is **gated on
> this seed** — it is the only arrangement in the repo where that printed instruction can be
> executed at all. So B4 buys two things: the cut-before-grouping property, and the *only*
> reachable execution of an instruction whose predecessor at the same site **shipped false**.
> If M8b is ever trimmed for time, B4 is the last block to cut, not the first — which inverts
> the v1 ordering, where it read as the most expensive block for a single property.
>
> Sequencing consequence: **B4's seed must be live when Part C's row runs.** Either the two
> land together, or Part C's row is written to arm the seed itself. Recommend landing them
> together and running B4 last in the phase, so the teardown below stays the single owner of
> the cleanup.

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
helper:

```js
// mirrors the /_e2e/mrs/{iid}/state mutator. Bumps updated_at because a real forge
// does — see the incremental-sync fidelity note below for why that is about honesty
// rather than about making this harness pass.
if (method === "POST" && (m = path.match(/^\/_e2e\/issues\/(\d+)\/state$/))) {
  const issue = state.issues[Number(m[1])];
  if (!issue) return send(res, 404, { message: "404 Not found (no such issue)" });
  const body = await readBody(req);
  issue.state = body.state === "closed" ? "closed" : "opened";
  issue.updated_at = new Date().toISOString();
  persist();
  log("issue", issue.iid, "state ->", issue.state);
  return send(res, 200, issue);
}
```

> **CORRECTION TO v1, and it is mine.** v1 said the bump was needed because *"a close that
> does not bump `updated_at` is invisible until the next `FullSync`"*. **That is wrong about
> this fake.** MEASURED at `2639af95`: `IncrementalSync` passes `UpdatedAfter: &hwm`
> (`forgesvc/service.go:294-298`) and the GitLab driver does send it (`forge/gitlab.go:257`),
> but **`forge-fake.mjs` ignores `updated_after` entirely** — its `GET /issues` returns every
> recorded issue, by deliberate design (*"Keeps a reconcile pass from evicting the cache"*).
> So in the harness a close is picked up **immediately**, bump or no bump. Against real
> GitLab, `updated_after=hwm` would **exclude** an unbumped issue and the close would be
> missed until something else touched it. Bump it because a real forge does — the fake must
> not lie about the forge — not because the harness needs it.

**The fidelity limit this exposes, which is bigger than the mutator and must be stated in the
phase's comment.** Because the fake ignores `updated_after`, **the e2e leg structurally cannot
exercise the incremental-sync path at all.** M6's close-sync rides `IncrementalSync`, so this
block proves the close→Done edge fires *given the cache was updated* — it does **not** prove
the real incremental sync would ever observe the close. That is a genuine hole in what the
harness can say, and it is invisible unless someone reads the fake.

Do **not** close it inside #98 by making the fake honour `updated_after`. That would change
`GET /issues` semantics for **every** phase that relies on "return all recorded issues", and
the comment at that handler says the current behaviour is load-bearing for reconcile-eviction.
It is a real improvement and it needs its own change, its own measurement, and a full harness
run — raise it separately.

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

#### B6a. The positive-controlled negative window — concrete

**The problem, restated so the mechanism is obvious.** Both negative windows (edge-once,
Undo-sticks) assert that *nothing happened during a wait*. A poller that **crashed** after the
first edge produces a **byte-identical green**. A bare `sleep 8` is not evidence — it is the
same false-green family as a live-DB suite that ran nothing, and the harness's existing 2-tick
window (`:2035-2040`) does not carry a control.

**A negative window needs a PROBE: an independent close edge, armed inside the window, whose
landing proves the poller ran.** Each window needs its **own** probe, because an edge is
consumed once and cannot serve two windows.

Fixture: two extra filed coordinates, `probeA` and `probeB`, each on its own issue, each filed
and settled but **not yet closed** when the block starts. (Both go through the existing #68
filing path or a direct `recommendation_filed_issues` seed — the harness already does the
latter for gauge rows.)

```bash
# ---- window 1: edge-once ------------------------------------------------
SET_AT_BEFORE="$(disposition_set_at "$F_REVIEW" install_worker_tool jq)"
close_issue "$PROBE_A_IID"                 # arm the control INSIDE the window
wait_disposition "$PROBE_A_REVIEW" "$PROBE_A_CAT" "$PROBE_A_TGT" done 20 \
  || fail "positive control: probeA's close edge was NOT processed — the poller is not running, so the edge-once assertion below would be vacuous"
[ "$(disposition_set_at "$F_REVIEW" install_worker_tool jq)" = "$SET_AT_BEFORE" ] \
  || fail "edge-once violated: set_at moved, so the sync re-applied on a later tick"
pass "edge-once: set_at unchanged across a window in which the poller demonstrably processed another close edge"
```

and identically for window 2, arming `probeB` after the Undo:

```bash
# ---- window 2: Undo sticks ----------------------------------------------
# NB: `apidelete` DOES NOT EXIST and, per the wave-3 ruling, never will — see the struck
# ruling below. Inline curl. (This sketch belongs to B6a, which the narrowing DROPPED.)
curl -fsS -b "$JAR" -X DELETE "$BASE/api/runs/$J_RUN/review/recommendations/$F_REC/disposition" \
  -H "X-CSRF-Token: $(csrf)" >/dev/null || fail "the human Undo failed"
close_issue "$PROBE_B_IID"
wait_disposition "$PROBE_B_REVIEW" ... done 20 \
  || fail "positive control: probeB's close edge was NOT processed — 'Undo stuck' below would be vacuous"
[ "$(disposition_count "$F_REVIEW" install_worker_tool jq)" = 0 ] \
  || fail "Undo did not stick: the auto-done came back, so close_synced_at is not consuming the edge"
pass "Undo sticks across a window in which the poller demonstrably processed another close edge"
```

**Why this is strictly better than a longer sleep**, and the property to state at the site: the
wait is now bounded **below by observed poller work** rather than by wall-clock optimism. It
cannot pass while the poller is dead, cannot pass while the poller is merely slow, and it gets
*faster* than a fixed sleep because `wait_disposition` returns as soon as the probe lands.

**Two ordering constraints, both load-bearing:**

- The probe must be closed **after** the thing under test is arranged (after the `set_at`
  capture; after the Undo), or its edge may be consumed by a tick that ran before the window
  opened, and the control would prove nothing about *this* window.
- The probe must be a coordinate **nothing else asserts on** — otherwise its auto-done
  perturbs `triage.todo`, which B6's earlier "fell by exactly 1" assertion reads. Use a
  category outside `improve_uzi` (the unscoped-backlog landmine class) and factor the
  `triage.todo` assertion to run **before** any probe is armed.

**Helpers this block needs, and one that does not exist.** MEASURED at `2639af95`: **there is
no `apidelete` function in `run-e2e.sh`.** It is named in the `retry_read` comment at `:235`
(*"the write helpers (apipost/apiput/apipatch/apidelete, fake_post) … are deliberately NOT
wrapped"*) but never defined; the harness's single DELETE is an inline
`curl -fsS -b "$JAR" -X DELETE … -H "X-CSRF-Token: $(csrf)"` against
`/api/me/secrets/anthropic_token/` in the PRD #104 token-binding phase — cited by PHASE because
the `:3843` this line used to carry now points at unrelated text, which is the very defect the
paragraph is about. A dangling reference
to a helper that was never written — the same class as the registry notes in Part C, in the
harness's own documentation.

~~**Ruling: define `apidelete` properly** (matching `apipost`/`apiput`, CSRF header included,
deliberately *not* `retry_read`-wrapped) and use it. That makes the `:235` comment true rather
than aspirational, and it is three lines. Do **not** rewrite `:3843` to use it in this MR —
that is an unrelated phase, and touching it is noise a reviewer has to read.~~

> 🔴 **SUPERSEDED AND STRUCK, 2026-07-25.** The architect's wave-3 ruling (§2.5 of
> `98-judge-menu-m8-w3-rulings.md`) reversed this, and `a01787f2` implemented the reversal:
> **correct the comment, do not define the helper.** The reason the reversal is right is that
> this ruling was made *in service of B6a's Undo window*, and the wave-3 narrowing dropped
> B6a — so defining `apidelete` would have shipped dead code **plus** a comment that had just
> become true about a function nobody calls. That is strictly worse than the dangling
> reference it was meant to fix.
>
> **Left struck rather than deleted, because this section is the one a future B6-matrix author
> would read**, and an instruction that silently vanished teaches nothing about why. What
> landed: `apidelete` removed from the `retry_read` comment's helper list, and the list's
> line-number citations replaced by phase names.

Three more helpers, all following existing patterns:

| helper | shape |
|---|---|
| `close_issue IID` | `fake_post "/_e2e/issues/$1/state" '{"state":"closed"}'` — mirrors `flip_mr` (`:501`) |
| `disposition_set_at REVIEW CAT TGT` | `db_psql "SELECT COALESCE(to_char(set_at,'YYYYMMDDHH24MISSUS'),'') FROM …"` — mirrors `notified_at` (`:3511`), which already uses exactly this to-char trick to make a timestamp shell-comparable |
| `wait_disposition REVIEW CAT TGT WANT [TIMEOUT]` | `wait_eq` (`:422`) over a `db_psql` status read |

**B7 — the notification deep-links to `/judge?run=`.** **Open the DTO before writing this
block.** MEASURED that `notificationLink` is a *web* function (`web/src/lib/notifications.ts`,
unit-covered by `notifications.test.ts`). If the deep link is computed client-side, there is
nothing on the wire to assert and the only honest e2e assertion is that the `judge_review`
notification carries `run_id` — which the #46 phase already makes at `:2709`. **In that case
drop the block and say so**, rather than assert a proxy. Asserting presence where efficacy was
claimed is the exact failure `.claude/agent-team.md` records for the `title`-attribute pass.

**~~B8 — no token spend on any disposition.~~ DEAD. DO NOT BUILD IT.**

**v1 text, kept so the refutation has a subject:** *"Follow the steer phase verbatim
(`:1690-1701`): fake MR count unchanged, no branch pushed, `run.usage` zero on both target
runs, and `SELECT count(*) FROM runs` unchanged across the whole #98 phase."*

**Why it is dead: the assertion cannot fail.** A disposition creates no run, so a
`run.usage`/run-count delta sits at zero whether or not the property holds. The one honest
precedent (`run-e2e.sh:1690-1700`) could attach to `run.usage` only because a run existed to
attach it to. The harness **already records this in its own words** at `:2832-2836`, describing
the structural proof as *"strictly stronger than this harness's before/after run count and
forge-state signature, which could only catch a write that happened to land"*.

**The criterion is met, and by the stronger proof.** `TestDispositionTouchesStoreOnly`
(`handler/review_disposition_test.go:421`) and `TestBulkDispositionTouchesStoreOnly`
(`judge_bulk_disposition_test.go:678`) are positive store-call **allowlists**: they prove the
path calls only the owner-resolve reads plus the single disposition write, never a
run-create/enqueue or any forge method. That is a statement about what the code *can* do; a
before/after count is a statement about what it *did once*.

**Recorded rather than deleted, deliberately.** The PRD's Success Criteria say the no-spend
property is *"proven by the M8 e2e leg"*. A silent removal invites the next reader to re-add it
from that line. **The PRD sentence is the thing that needs amending** — that file is the lead's,
so this is a request, not an edit: the criterion is proven by the two structural tests, not by
e2e.

**Authored by me in v1, and the failure is the one this branch keeps cataloguing** — I shipped
a decorative assertion as a criterion's proof while quoting, two blocks earlier, the very
harness comment that refutes it. Proxy substituted for property: a zero delta proxied for "no
spend can happen".

### B9. The printed-instruction backstop — the EXECUTING half

> **B9 and B10 are SUPERSEDED and belong to pass 2** (the backstop pass the lead approved).
> They were written without knowing that `instructionRE` (`instructions_test.go:130`) excludes
> `%`, `<` and `|`, so a backticked instruction carrying a format verb or a placeholder cannot
> be lifted at all — which means the file's central claim that "a FOURTH instruction cannot
> land silently" does not hold for the shape a real instruction takes. Pass 2 owns the
> static-vs-runtime matcher split, the class widening, the prefix-boundary ruling, the
> 0-of-8 baseline, and the reachability triage. **What survives from below**: the placement
> ruling (e2e, not a build-tagged Go test) and the extraction discipline (assert the count,
> never `head -1`; `eval` guarded by a fixed-shape regex; bind to the emitting command).

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

Revised for pass 3: B8 removed, B6a added, B9/B10 moved to Part C.

| | |
|---|---|
| forge-fake `/_e2e/issues/{iid}/state` mutator + `close_issue` | 1h |
| ~~`apidelete`~~ + `disposition_set_at` + `wait_disposition` helpers (apidelete STRUCK — see §above) | 0.5h |
| B1-B3 | 2h |
| B4 (seed, containment, delete-and-recheck control) | 1.5h |
| B5 | 2h |
| B6 (the edge/undo/dismissed matrix) | 3h |
| **B6a (two probe fixtures + both positive-controlled windows)** | **1.5h** |
| B7 (or its documented drop) | 0.5h |
| ~~B8~~ | **0 — dead** |
| ~~B9/B10~~ → Part C | — |
| iteration against a 30-min harness — budget ≥3 full runs | 2× write time |
| **M8b total** | **~2.5 days** |

Down from v1's ~3 days: B8 is gone and B9/B10 moved to Part C, against a smaller addition for
B6a and the helpers.

---

## Part C — the printed-instruction backstop (pass 2)

Everything in this part is MEASURED by execution against `api/cmd/uzi/` at `e30f97e0`, using
the real regex, the real AST walk and the real prefix matcher, not by reading. Probes were run
in throwaway packages under `api/` and deleted; the tree is clean.

### C0. The finding that reframes the mechanism: there are TWO kinds, and the registry conflates them

I classified every lifted candidate by its **enclosing syntactic position**. Every one resolved
to a definite kind — zero `UNKNOWN`:

| candidate | kind | site |
|---|---|---|
| `uzi login` | **RUNTIME** `Exitf` | `login.go:104`, `:110` |
| `uzi repo list` | **RUNTIME** `Exitf` | `run.go:192` |
| `uzi review backlog --run <run-id>` | **RUNTIME** `Println` | `review.go:403` |
| `uzi review show %s` | **RUNTIME** `Exitf` | `review.go:531` |
| `uzi review undo %s %s` | **RUNTIME** `Printf` | `review.go:447` |
| `uzi review backlog` | **HELP** flag usage | `review.go:212`, `:213` |
| `uzi run inputs` | **HELP** `Long` | `run.go:287` |
| `uzi skill install` | **HELP** `Short` | `skill.go:95` |
| `uzi worker set-token` | **HELP** `Long` | `token.go:25` |

**A runtime emission tells a user what to do next at a decision point. A help reference is
documentation cross-linking a sibling command.** The file's bar — *"adding a line here is a
claim that the instruction has been EXECUTED and its outcome asserted"* (`:38-41`) — is the
right bar for the first kind and **the wrong bar for the second**. Nobody needs to *execute*
`uzi skill install` to validate a `Short` description that mentions it; what that string can be
wrong about is naming a command path that does not exist.

**Consequence: four of the eight registry notes misdescribe their own site.** Measured:

- `uzi run inputs` — note says *"Printed by run.go after submitting a steer"*. It is in a
  `Long` help string (`run.go:287`).
- `uzi skill install` — note says *"Printed by the skill auto-upgrade warning"*. It is a
  `Short` description (`skill.go:95`).
- `uzi login` — note says *"Printed by the auth-required path"*. It is the **device-auth
  polling loop**'s retry hint (`login.go:104`/`:110`), a different path with different
  reachability.
- `uzi review backlog` — note says *"Printed by both truncation warnings"*. The candidate the
  extractor actually lifts comes from **flag usage** (`:212`/`:213`); the truncation remedy
  lifts as the longer, distinct `uzi review backlog --run <run-id>` — and only after the
  widening below, because the current extractor cannot see it at all.

The registry's notes are themselves unverified claims. That is the exact class the registry
exists to catch, applied to the registry.

### C1. Extractor blindness — MEASURED, and the widening ruling

The claim that a fourth instruction "cannot land silently" (`instructions_test.go:32-36`) is
**false for the shape a real instruction takes.** Running the real `instructionRE` against the
real literals:

| literal | current extractor lifts |
|---|---|
| `` `uzi review show %s` `` (`review.go:531`) | **nothing** |
| `` `uzi review backlog --run <run-id>` `` (`review.go:403`) | **nothing** |
| `  uzi review undo %s %s` (`review.go:447`) | `uzi review undo` — truncated at the `%` |

`uzi review show` is registered **only because a human happened to type it**. Had they not,
the registration check would have stayed green.

**RULING — widen the class to `%`, `<`, `>` ONLY. Do not add `|` or `/`.** Measured
package-wide, three variants:

| class | candidates | new vs current |
|---|---|---|
| current | 7 | — |
| **`% < >`** | **9** | the three real instructions above, **zero false positives** |
| `% < > \| / _` | 10 | those three **plus** `uzi review resolve\|dismiss --category/--target` |

The only thing `|` and `/` buy is `review.go:54`'s shorthand — an **alternation that is not
runnable as printed**. Registering it would assert someone executed a string nobody can. And
excluding it needs no special case: **the alternation character is itself the marker that a
span is a reference rather than a command.**

**The auditor's read is confirmed: the cobra-verb filter carries the load, not the character
class.** Measured — the root's own `Long`, *"uzi drives the factory from the terminal"*, is
rejected under **both** the current and the widened class, because `drives` is not a verb in
the tree. So widening costs nothing in prose leakage.

### C2. Matcher asymmetry — RULING: make them symmetric

MEASURED: for `uzi review show`, `strings.Contains` over raw literals returns **true** while
the registration scan sees **nothing**. So `TestRegisteredInstructionsAreStillPrinted` keeps an
entry alive that `TestPrintedInstructionsAreRegistered` is blind to — and **only the blind one
fails the build on a new instruction**. That asymmetry, not the character class alone, is the
defect.

**Fix: compute the lift set ONCE and run both directions over it.**

- registration: every lifted candidate must match some entry;
- staleness: every entry must be matched by some lifted candidate.

This also removes a hazard nobody has hit yet: `strings.Contains` would keep an entry alive on
the strength of the command name appearing in a **comment** or a doc string, since it never
asked whether anything *prints* it.

### C3. Prefix absorption — RULING: take the one-liner, it is measured

MEASURED, the proposed `cmd == k.command || strings.HasPrefix(cmd, k.command+" ")` against the
same corpus:

| candidate | absorbed today | absorbed with the fix |
|---|---|---|
| `uzi review backlog` | yes | yes |
| `uzi review backlog --run x` | yes | yes |
| `uzi review undo a b` | yes | yes |
| `uzi review backlog-export --all` | **yes (hole)** | **no** |
| `uzi review backlogger` | **yes (hole)** | **no** |
| `uzi review showdown` | **yes (hole)** | **no** |

All three holes close; every legitimate match survives, **including the newly-widened longer
forms** (`uzi review show %s` still matches the `uzi review show` entry, and so on). It is a
real hole and it is one line — **close it in this wave.**

### C4. The static/runtime split — the lead's reframing, adopted, with its consequence

The `Exitf` finding is confirmed at `uzicli/output.go:48`: `fmt.Errorf(format, a...)`
substitutes at emit time, so a user sees `uzi review show <a real uuid>` — complete and
runnable, no verb in it.

- **STATIC — "does a row exist?"** Must read source literals, because an unregistered
  instruction has to fail the build *before* anyone runs anything. This is the direction with
  the holes: it needs C1's widening, C2's symmetry and C3's boundary. **It can never verify
  execution and must not imply it.**
- **RUNTIME — "does the printed text work?"** Must read **emitted output**, never source.
  **It needs no regex widening at all**, because formatting has already happened.

**The consequence worth stating: `kind` is DERIVED, not declared.** It comes from the AST
position (C0), so an author cannot label a runtime instruction as "help" to dodge the execution
bar. That answers *"what fails if someone flips an entry to EXECUTED with no row behind it"*
from the stronger side — they cannot choose the kind in the first place.

### C5. The registry shape

```go
type instructionKind int   // DERIVED from the AST position — never hand-declared
const (kindHelp instructionKind = iota; kindRuntime)

type evidenceKind int
const (
    evidenceNotExecuted evidenceKind = iota // LEGAL, GREEN, PERMANENT — requires a reason
    evidenceGoTest
    evidenceE2E
)

type knownInstruction struct {
    command  string
    evidence evidenceKind
    where    string // a Go test NAME, or the e2e phase's `say` label
    reason   string // required when evidence == evidenceNotExecuted
    note     string
}
```

Checks, all static, no stack, all in `go test ./...`:

1. **HELP entries: assert the referenced path RESOLVES in the cobra tree.** New, cheap, and
   *stronger than today* — nothing currently checks that `uzi worker set-token` is a real
   command path, only that `worker` is a real top-level verb. This is the right and complete
   bar for the help kind.
2. `evidenceE2E` → `where` must appear in `e2e/run-e2e.sh`.
3. `evidenceGoTest` → `func <where>(` must exist under `api/`.
4. `evidenceNotExecuted` → `reason` must be non-empty.
5. **Positive control**: fail if `e2e/run-e2e.sh` cannot be **read**. A moved harness must
   redden this, not silently satisfy it. Do **not** assert "≥1 entry is `evidenceE2E`" — that
   fails today, before any row lands.

**`evidenceNotExecuted` stays legal, green and permanent.** A mechanism that fails the build on
an honestly-declared gap gets deleted the first time someone is in a hurry, taking the
arriving-instruction check with it. The `reason` requirement is what stops it becoming a
shrug.

**What this proves and what it does not:** it proves the claim's *address* is live. It does
**not** prove the named phase asserts anything — the same honest boundary the query-inventory
test carries, and it must be written at the site in those words.

### C6. Reachability triage — the truthful subset

The baseline first, and it is worse than "6 of 8": against the file's own bar at `:38-41`,
**0 of 8 qualify today**, and 4 of the 8 are help references that were never the right subject
for that bar.

**RUNTIME kind — the four that need execution rows:**

| instruction | reachable? | cost / mechanism |
|---|---|---|
| `uzi review undo %s %s` | **YES — flagship** | Arrange 2 open members → group dismiss → extract 2 addresses → execute both → assert both rows gone **and** `triage.todo` back up by 2 |
| `uzi review show %s` | **YES — and CHEAP** | Arrange = `uzi review resolve $J_RUN <bogus-id>` against a run that already has a review. **Corrects the standing inference that this is e2e-expensive**: the arrange step is one extra CLI call, not a new fixture |
| `uzi repo list` | **YES — cheap** | Arrange = a run-start call with `--repo` omitted; execute `uzi repo list` (a pure read); assert it lists `$REPO_ID` |
| `uzi review backlog --run <run-id>` | **YES, but gated** | Needs Part B's B4 2001-row seed. This is the only arrangement in the repo where the truncation remedy can be executed at all |
| `uzi login` | **NO — permanent** | `login.go` declares **no flags**; it is a device-authorization flow, and the hint is emitted from inside the polling loop on a terminal/timeout state. Executing it verbatim means driving a browser approval. Honest permanent `evidenceNotExecuted` with *that* reason — not "inherited gap" |

**Two traps in the two Exitf rows.** `uzi review show %s` and `uzi repo list` are emitted
through `Exitf`, so they go to **stderr** and the process exits **non-zero**
(`ExitNotFound`/`ExitUsage`). A row must capture stderr and **tolerate a non-zero exit** —
under `set -euo pipefail` the naive form kills the harness, and a row that "passes" because it
never ran is the false green this whole mechanism exists to prevent.

**HELP kind — the four that need no execution, ever:** `uzi review backlog`, `uzi run inputs`,
`uzi skill install`, `uzi worker set-token`. Covered completely by C5 check 1. Their current
`NOT EXECUTED` notes should be **replaced**, not upgraded — they answer a question that was
never the right one to ask of a `Short` description.

So the honest landing state is **4 runtime rows executed, 1 runtime permanently declared with
a real reason, 4 help entries fully checked by a bar that actually fits them** — against 0 of 8
today.

### C7. Making the hand-written shortcut unavailable — and the honest residual

The requirement is that a row execute the printed text **verbatim**, never a hand-written
argv, because both previously-false instructions **parsed perfectly** and a copy would have
passed on both.

Three mechanisms, in descending strength:

1. **One shared helper, used by every row.** Extraction and execution live in a single
   `run_printed_instruction` function; a row that hand-writes argv has to *visibly bypass* the
   helper, which is reviewable in a way that a subtly-wrong string is not.
2. **A shape-guarded `eval`.** The helper matches the captured span against a fixed shape
   (line-anchored, UUID-shaped where applicable) before executing. `eval` therefore cannot see
   text that did not come out of the command in the expected form — and this is also what makes
   `eval` safe here rather than reckless.
3. **Assert the count before using any match.** Never `head -1`: output that stops at your
   limit is indistinguishable from output that ended (the repo's own truncated-view rule).

**The honest residual, stated because the requirement was "structurally unavailable" and shell
cannot deliver that.** A determined author can still assign a literal to the variable the
helper reads. There is no type system here to forbid it. What the three mechanisms buy is that
the shortcut becomes *visible in review* rather than *invisible in a passing test* — which is a
real improvement and is not the same as impossible. I would rather record that than claim a
guarantee the substrate cannot make.

### C8. Extraction must never be positional

MEASURED at `run.go:478-485`: `sanitizeTTY` strips C0 except `\t` and `\n`, and C1
(`0x80-0x9f`) — but **`0x7f` (DEL) survives**, and the tab/DEL fold lives only in `cellText`
(`run.go:577`), which the backlog path does not use. `renderBacklog` (`review.go:308-314`)
sanitizes `target` and `rationale_preview` and prints `category`/`bucket` raw (sound — there is
a real CHECK constraint at `00059_run_reviews.sql:47`).

**So an embedded tab in a judge `target` shifts the rendered columns.** Any row reading
`renderBacklog`'s table must extract by a **delimiter the sanitizer guarantees absent**, never
by column position. There is a measured precedent in this repo (`run.go:564-566`): a benign
label put a payload at rendered column 58, `a\tb\tc\td\te` at 76, and eight tabs at 107 — while
the *rune* offset stayed pinned at 58 throughout, which is exactly why the existing alignment
test could not see it.

### C9. Isolation — no new env var needed

Confirmed against `run-e2e.sh:103-107`: the allowlist is exactly 19 names. **None of Part C
needs a new one** — the rows run through the existing `uzi_cli` helper (`:1407`), which already
carries `UZI_URL`, `UZI_TOKEN` and `UZI_SKILL_AUTO_UPGRADE=0` under `env -i`. The argv sentinel
(`[ "${1:-}" != "--e2e-sanitized" ]`, `:101`) is untouched. Flagging this explicitly because
widening that allowlist is a blocking-class change and a silent addition is how it would land.

### C10. Effort

| | |
|---|---|
| C1 widening + C2 symmetry + C3 boundary (one file, all static) | 2h |
| C0/C4 kind derivation from the AST position | 2h |
| C5 registry restructure + the five checks | 2.5h |
| C6 four runtime rows in the harness (undo, show, repo list; backlog gated on B4) | 3h |
| C7 the shared helper | 1h |
| Rewriting the four help notes + the five misdescribing notes | 1h |
| Negative controls — one per check, RED then restored | 2h |
| **Part C total** | **~1.5-2 days** |

Note C6's `uzi review backlog --run` row is **gated on Part B's B4**; the other three are
independent of Part B entirely and can land first.

### C11. What Part C cannot catch

- That a named e2e phase **asserts** anything — only that its address is live (C5).
- A **new** instruction printed through a helper the AST walk does not recognise as an emitter.
  C0's classifier knows `Printf`/`Println`/`Print`/`Exitf`/`Errorf`/`Fprintf`/`Fprintln`; a new
  wrapper would classify `UNKNOWN`. **Design requirement: `UNKNOWN` must FAIL, not default to
  help** — defaulting to help would let a new emitter dodge the execution bar silently, which
  is the whole failure this part exists to close.
- An instruction assembled from concatenated fragments at runtime; the walk sees literals.
- Whether the *outcome* a row asserts is the right outcome.

---

## Open questions for the user (product), via the lead

**RESOLVED (user, 2026-07-21):**

- ~~**A1 — the `mockApi.ts` extraction.**~~ **AUTHORISED.**
- ~~**A10 — fix the divergence or encode it as an expected exception.**~~ **FIX THE MOCK, in
  TWO changes** (`Array.from` for the count/slice; the trim set separately). Expected output is
  the server's.
- ~~**B11 — pay 90s for a second real judged run?**~~ **Drive to 100%; M8b is funded.** Do not
  trim it for time.

**STILL OPEN:**

1. **A8 — demo-mode truncation.** Permanently-truncated demo, test-only reachability, or a
   deliberate dev toggle? M3's requirement is that the *mock renders every state*, and only the
   toggle satisfies that for a human. Recommend the toggle. This is the **only** unresolved
   product question in Part A.
2. **A6a — sort stability.** Not a product question, but a cost one the lead may want to
   overrule: 13 interleaved groups to pin it, or declare it unpinned with the measured reason.
   Recommend paying — it is the cheapest case per row despite being the widest.
3. **A request against `prds/98-judge-menu.md` (the lead's file, not mine):** the Success
   Criteria say no-token-spend is *"proven by the M8 e2e leg"*. It is not, and cannot be — see
   the B8 refutation. It is proven by `TestDispositionTouchesStoreOnly` and
   `TestBulkDispositionTouchesStoreOnly`. Left as a request so the line does not silently
   re-summon the dead assertion.

## What this design cannot catch, in one place

Seam 6: the `?run=` anchor, the row cap and `truncated`, the SQL join producing
`disposition_status`/`filed_settled`, the query's `ORDER BY`, `scope=open`, and the
representativeness of `data.ts` itself.
M8b: the self-improve backlog drop (no wire observable), the `LIMIT`/`ORDER BY` interaction at
scale, and — for B10 — whether a registered phase *asserts* anything, as opposed to existing.
