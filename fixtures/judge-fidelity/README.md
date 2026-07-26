# judge-fidelity: the mock/server golden fixture (PRD #98, seam 6)

`web/src/mocks/mockApi.ts` is not a stub. It reimplements the Judge backlog's dedup, the
`dismissed > done > filed > todo` rollup, `run_count`, the rationale preview and the
`?bucket=` filter, all of which also exist in Go. Until this fixture existed, the agreement
between the two was verified only by reading -- and when it drifts, two things break at once
and neither announces itself: the demo lies, and ~950 mock-backed vitests pass while
asserting a fiction.

## The three artifacts, and why they live here

```
fixtures/judge-fidelity/cases.json      inputs, hand-authored
fixtures/judge-fidelity/expected.json   outputs, hand-authored
fixtures/judge-fidelity/README.md       this file
```

Repo root, **owned by neither runtime**. Not `api/internal/workersvc/testdata/`, which is
where a `go test -update` flag gets added; not `web/src/mocks/`, which is where
`toMatchSnapshot()` gets added.

Two readers, and **neither generates either file**:

| | |
|---|---|
| Go | `api/internal/workersvc/judge_backlog_fidelity_test.go` (relative path; `go:embed` cannot escape the `api/` module) |
| vitest | `web/src/mocks/judgeBacklogFidelity.test.ts` (plain `fs`, resolved against `import.meta.url`) |

**Each runtime compares its OWN output against `expected.json`.** Never Go against JS
directly. A direct diff can only report *that* they disagree, never *which one drifted* --
and it would make `npm test` depend on a Go toolchain. Against a shared expected file each
suite stands alone and the failure names the side: Go red plus vitest green means Go
drifted, and vice versa. Both test files say so at the failure site.

A missing or unreadable fixture is a **fatal**, never a skip, on both sides. A skip here is
the same false-green shape `CLAUDE.md` records for the live-DB suites, where a suite that
ran nothing prints `ok`.

### 🔴 The two halves are NOT symmetric, and the Go half needs `-count=1`

Go's test cache hashes the files a test opens, **but only those inside the module root** --
cmd/go's own words are *"Do not recheck files outside the module, GOPATH, or GOROOT root"*.
This directory is at the repo root, above `api/`, **on purpose** (see below). So every byte
of `cases.json` and `expected.json` is outside the `api` module and contributes **nothing**
to `internal/workersvc`'s cache key.

MEASURED at `6002d808`, and it produced output indistinguishable from success:

| | |
|---|---|
| delete an entire case from `cases.json`, then `cd api && go test ./internal/workersvc/` | `ok  (cached)` |
| same tree, `cd api && go test -count=1 ./...` | **FAIL** -- *"fixture broken: cases.json has no case ..."* plus the orphaned-golden message |
| same tree, `cd web && npx vitest run src/mocks/judgeBacklogFidelity.test.ts` | **FAIL**, no flag needed -- vitest has no such cache |

**That asymmetry is the finding, because the rule above reads as symmetric.** "Go red plus
vitest green means Go drifted" has a third explanation this file did not name: *Go never
ran.* If the two halves ever disagree, check that the Go half actually executed before
concluding anything about which implementation moved.

Exposure, narrowed rather than left as "the Go half":

- **`./e2e/run-store-it.sh` was never at risk**, for two independent reasons -- it passes
  `-count=1` (line 72), and it sweeps `-run 'LiveDB$'` over `./internal/store/...` and
  `./internal/handler/...` only, so it never reaches this package.
- **CI's `test:controller` was never at risk** -- it already passes `-count=1`, and
  `.gitlab-ci.yml` spells out this exact mechanism for the `api/` goldens *it* reads across
  the same module boundary. That comment predates this fixture and describes it precisely.
- **The exposed gates were `cd api && go test ./...`** (the command `CLAUDE.md` prescribes)
  **and CI's `test:api`**, which ran it bare while `.go_job` persists `.gocache/` across
  pipelines. `test:api` now passes `-count=1` for the reason its controller sibling already
  did.

**Do not "fix" this by moving the fixture under `api/`.** That reintroduces exactly the
regenerator gravity the next section rejects.

## There is no regenerator, and that is deliberate

No `-update` flag on the Go side. No `toMatchSnapshot()` on the vitest side -- vitest
*writes* a missing snapshot on first run and passes, which is precisely the "golden file
rots into a snapshot" mechanism this fixture exists to prevent, so it is disqualified by
construction rather than by discipline.

The fixture is **authored to discriminate, never snapshotted from `web/src/mocks/data.ts`**.
A golden derived from `mockReviews` would lock in the demo's own blind spot (measured
2026-07-21: zero instances of `occurrences > run_count`, zero fully-settled groups with
disagreeing members), agree on everything it covered, and read as full coverage.

**To change the fixture**: work out by hand what the new output must be, from the rules in
`GroupJudgeRecommendations`' doc comment, and write it into `expected.json`. If you find
yourself pasting a test's actual output, stop -- that is the regeneration this file exists
to forbid, and the output-side self-check below is what will catch it.

Then run **both** halves, and the Go one **with `-count=1`** -- a fixture-only edit changes
nothing inside the `api` module, so a bare `go test` reports `ok (cached)` and you will have
verified nothing:

```sh
cd api && go test -count=1 ./internal/workersvc/
cd web && npx vitest run src/mocks/judgeBacklogFidelity.test.ts
```

`-count=1` is load-bearing here, not a habit. See the asymmetry section above for the
measurement.

## Both JSON files are pure ASCII, by construction

Every character outside printable ASCII is written as a `\uXXXX` escape, including the
astral ones as surrogate pairs. Three reasons, and the first is not theoretical:

1. **A pasted glyph corrupts silently.** While this fixture was being authored, a literal
   U+0020 in a probe arrived as U+00A0, and the probe then reported a defect it had itself
   manufactured. A separate incident turned a U+FEFF into a raw byte-order mark that Go
   refuses to compile anywhere in a source file.
2. Both `encoding/json` and `JSON.parse` decode `\uXXXX` (and surrogate pairs) identically,
   so the escape is the one representation neither side can interpret differently.
3. A raw U+2028 or U+FEFF inside a file is invisible in every diff view, and this fixture
   deliberately contains both.

The same rule applies to the two test files: they are ASCII-only and build every exotic
character by code point (`rune(0x00A0)` / `String.fromCodePoint(0x00a0)`).

## The cases

Every case names what it proves in its own `proves` field; several carry a `do_not_tidy`
note. Those notes are load-bearing -- see the last section.

| case | proves |
|---|---|
| `dedup-across-runs` | one coordinate in two runs collapses to one group at `run_count` 2 |
| `occurrences-exceed-run-count` | the same coordinate twice in ONE review: 3 occurrences behind 2 runs (the SQLSTATE 21000 shape) |
| `partial-settle` | one open member and one settled one; the rollup short-circuits to todo; `set_via` present on one occurrence and absent on the other |
| `rollup-precedence-pairs` | all six pairs of the ladder, one coordinate each |
| `sort-tie-first-seen-order` | a three-way tie on `(run_count, open_count)`, plus the `open_count` tiebreak |
| `bucket-filter-all` / `-todo` / `-dismissed` | one row set at three `?bucket=` values, expecting 5, 2 and 1 groups |
| `preview-ascii-cut` | the 280-rune cut, and that the preview comes from the GROUP's first row |
| `preview-multibyte-cut` | 300 astral code points: 281 runes out, versus 141 from a code-unit implementation; the odd-offset row cuts mid surrogate pair |
| `preview-multibyte-no-cut` | 200 astral code points: over the cap in code units, under it in runes |
| `preview-trim-boundary` | NBSP / U+FEFF / U+2028 at rune 280 survive Go's cutset and are stripped by JS `\s`; a U+0020 row is the control |
| `sort-stability-13-groups` | 13 groups with interleaved `run_count`s: the only shape where `sort.Slice` differs from `sort.SliceStable` |

## The self-check, in two halves

Both halves are duplicated in both runtimes and touch neither implementation. Their failure
messages follow the shape already in the tree:
`fixture broken: <what> -- otherwise this test proves nothing about <property>`.

**Input side** (`...CasesDiscriminate`): predicates over `cases.json` proving each case can
actually tell a correct implementation from a wrong one -- that a coordinate really does
recur across runs, that a `(category, target, run_id)` triple really does repeat, that each
of the six precedence pairs is present, that the stability case really is interleaved, and
so on.

**Output side** (`...GoldenStillDiscriminates`): predicates over `expected.json` proving the
golden still *describes* that discrimination. **This is the anti-regeneration mechanism.**
If someone regenerates `expected.json` from a regressed grouper -- say `RunCount++` escapes
the `runsSeen` guard -- the regenerated golden shows `occurrences == run_count` and the
output-side check fatals: *this case no longer exercises occurrences > run_count*. The
predicates depend on neither implementation, so no regeneration can talk them into agreeing.

**Its honest limit**: a regression that happens to preserve every declared property
regenerates cleanly. The declared property list IS the coverage, and it is only as good as
the authoring of `rollup-precedence-pairs`.

One further honest note on the input-side half: it classifies a row's rung with its own
four-branch copy of the ladder rather than calling `BucketOf`. That is what makes it immune
to a mutated ladder, and the cost is that it cannot notice a ladder change -- catching that
is the golden comparison's job, not the self-check's.

## Detection power, MEASURED at landing

Recorded because a fixture's own claim to discriminate is worth nothing unless someone ran
the experiment. Ten folds, all against tree `5429ebe9`, each applied by script, each **proved
applied by diffing the tree** before its result was believed, each restored by copy-aside.
Results are recorded by the assertion MESSAGE that reddened, not by red/green: a fold can go
red at the wrong assertion and certify nothing.

| fold | Go | vitest | what reddened |
|---|---|---|---|
| JS `BUCKET_RANK` done/dismissed swapped | GREEN | RED, 5 cases | `mock grouper disagrees ... for rollup-precedence-pairs` (+ sort-tie, both bucket-filter cases, sort-stability) |
| Go `bucketRank` `case "done": return 3` | RED, 5 cases | GREEN | `case "rollup-precedence-pairs": the GO grouper disagrees ...` |
| Go `g.RunCount++` hoisted out of the `runsSeen` guard | RED, 1 | GREEN | `case "occurrences-exceed-run-count": the GO grouper disagrees ...` |
| JS drop the `open_count` half of the comparator | GREEN | RED, 3 | `... for sort-tie-first-seen-order` (+ rollup-precedence-pairs, bucket-filter-all) |
| Go `sort.SliceStable` -> `sort.Slice` | RED, 1 | GREEN | `case "sort-stability-13-groups": the GO grouper disagrees ...` |
| JS `Array.from` -> code units | GREEN | RED, 2 | `... for preview-multibyte-cut`, `... for preview-multibyte-no-cut` |
| JS trim set -> `/[\s]+$/` | GREEN | RED, 1 | `... for preview-trim-boundary` |
| **regeneration**: the `RunCount` fold PLUS rewriting the golden's `run_count` to match | golden **PASS**, self-check RED | same | `fixture broken: no expected group has more occurrences than run_count -- this case no longer describes the shape it is named for ...` |
| delete a case from `cases.json` | RED x3 | RED x3 | `fixture broken: cases.json has no case "..."` AND `fixture broken: expected.json carries golden output for "..." but cases.json no longer defines it` |
| **tidy** the stability case into recency order | golden **PASS**, self-check RED | same | `fixture broken: the groups are already in run_count DESC order before the sort runs ...` |

The first two are what justify the third artifact: each reddens exactly ONE runtime, which a
direct Go-against-JS diff cannot do. The rune fold and the trim fold redden **disjoint** case
sets, which is why the mock fix was two changes and not one.

**The last three are the ones to read.** In each, `TestJudgeGrouperMatchesFidelityGolden`
printed `--- PASS` while the fixture had been neutered, so the golden comparison alone
certified nothing and only the self-check caught it. The tidy fold left `expected.json`
**byte-identical**.

`sort.Slice` reddened `sort-stability-13-groups` and **nothing else in the fixture** -- the
6-group precedence case and the 5-group tie case both stayed green. That is the measured
confirmation, from the other direction, that no naturally-shaped case catches an unstable
sort.

### A case that shipped green and asserted nothing

`sort-tie-first-seen-order` first landed claiming, in its own `proves` field, that dropping
the `open_count` tiebreak would reorder it. **It did not.** Epsilon (the `open_count 0` group)
was authored LAST in the input rows, where first-seen order and the sorted order coincide, so
the fold went green against the case named for it. The fix was to move one row; `expected.json`
did not change, because the correct OUTPUT was never wrong. Recorded because that is the whole
argument for measuring detection power rather than reasoning about it: a case can be entirely
correct about its output and still pin nothing, and only running the fold says which.

## What this fixture CANNOT catch

Stated here rather than implied, because a fixture that reads as covering more than it does
is worse than a smaller one.

1. **The `?run=` anchor.** Go filters ROWS pre-grouping, in SQL, with a coordinate-level
   `EXISTS`; the mock filters GROUPS post-grouping. Different algorithms at different
   layers, and the Go half is not in the grouper at all.
2. **The row cap and `truncated`.** `JudgeBacklogMaxRows` cuts rows BEFORE grouping, in
   `JudgeRecommendationBacklog`. The grouper never sees it, and there is no exported pure
   helper for a case to call -- so a fixture case here would have to invent a cap value
   neither production path uses, while the `Lim: max + 1` interaction that distinguishes a
   full page from an exactly-full one stayed out of reach. That is a proxy reading as a pin.
   The MOCK side of the cut is pinned instead by `web/src/mocks/judgeBacklogTruncation.test.ts`;
   the cross-implementation pin belongs to M8b/B4.
3. **The SQL join.** `disposition_status`, `set_via` and `filed_settled` are join *outputs*
   in Go and array lookups in the mock. The fixture supplies both as *input*, so the
   filed-issues join's coordinate predicate is invisible here.
4. **The query's `ORDER BY`.** The fixture *declares* row order, so a drift in
   `ORDER BY rv.updated_at DESC` -- which decides which occurrence supplies the preview --
   is not observable.
5. **`scope=open`.** A bulk-disposition property, not a grouper one.
6. **The demo data.** The fixture is authored, so `mockReviews` can still be
   unrepresentative and nothing here would say so.
7. **A filed link with a NULL iid or URL.** `filed_settled` is `(f.filed_at IS NOT NULL)` in
   SQL, so the combination cannot occur through the query. Both implementations fall back to
   the zero value if a fixture hand-writes it; no case does.
8. **An empty `?bucket=`.** Go treats `""` as unfiltered; the mock's `JudgeBacklogBucket` is
   a closed union with no `""` member, so the branch is unreachable from this side.
9. **An unknown `set_via` value.** Go ships `SetVia string`; the TS type narrows to the
   single literal `"issue_close"`. The narrowing is documented at the type.

Items 1 and 2 belong to the e2e leg (M8b), which can execute the SQL.

## Do not tidy

These properties look like untidiness and are the only thing holding several pins up. Each
is also recorded in the relevant case's `do_not_tidy` field, so it travels with the data.

- **`sort-stability-13-groups` must stay unrealistic.** Go's `sort.Slice` (pdqsort)
  insertion-sorts below n=12 and short-circuits on input it detects as already ordered.
  Measured: an all-tied fixture never diverges at any n from 2 to 200, and a realistic
  recency-ordered backlog never diverges at 8, 12, 13, 16 or 20. Divergence needs all four
  of: at least 13 groups, at least two distinct `run_count` values, ties within each, and an
  input order that is NOT already sorted. Reordering these groups into a backlog-shaped list
  deletes the only thing in the repo pinning sort stability, and everything stays green.
- **There is deliberately no mirror-image JS stability case.** `Array.prototype.sort` has
  been required to be stable since ES2019, so a JS stability mutation cannot fail; adding one
  "for symmetry" would be decoration.
- **`preview-multibyte-no-cut` must stay under 280 runes and over 280 code units.** Past 280
  runes both implementations cut, and cut in the same place, so every longer string agrees.
  This case is the only shape that can tell a rune implementation from a code-unit one.
- **The three settled pairs in `rollup-precedence-pairs` must have no todo member.** One
  stray todo pushes `open_count` above zero, the rollup short-circuits, and the rung ranking
  those rows exist to test is never consulted.
- **The U+0020 row in `preview-trim-boundary` is the control, not a fourth variation.**
  Without it, "these three characters survive the trim" is satisfied by never trimming.
- **The two rows sharing a review in `occurrences-exceed-run-count` are the whole case.**
