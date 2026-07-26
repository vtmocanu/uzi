# PRD #98 — wave-3 design rulings: instruction attribution, and Part B re-derived

**Status**: design only. Nothing here is built. Written 2026-07-25 by the architect, on
`feature/prd-98-t3` @ `d4740e1c` (origin/main `6be9f542` + `t2-fid` + `t2-web` merged;
`t2-lim`, `tier2`, `t2-seam6`, `t2-cli` still pending at that moment).

**This file does not edit `prds/98-judge-menu-m8-design.md`.** That file is landing through
task #1's merge of `feature/prd-98-t2-fid`, and two writers on one file is how this PRD lost
work before. Where a ruling below supersedes a section of the design note, it says so by
section id; read them together, this one on top.

**Everything below was re-derived on 2026-07-25 against the tree, not inherited.** The design
note was written 2026-07-21 against `ad6c63d9`. Claims are tagged MEASURED (I ran it, or read
the file at a named SHA) or INFERRED (reasoned, not executed). Attack the INFERRED ones first.

---

## Ruling 1 — most-specific-wins attribution: **CONFIRMED, keep it as landed**

### 1.1 The question

`api/cmd/uzi/instructions_test.go` at `4b94f714` (`feature/prd-98-t2-cli`) matches a lifted
candidate against a registry entry on a word boundary:

```go
func matchesCommand(cmd, entry string) bool {
    return cmd == entry || strings.HasPrefix(cmd, entry+" ")
}
```

Two entries exist for the same verb — `uzi review backlog` (a flag-usage reference, HELP) and
`uzi review backlog --run <run-id>` (the truncation remedy's `Println`, RUNTIME). The candidate
lifted from the remedy matches **both**. The coder resolved it with `attribute()` returning the
**longest** matching entry, and documented the rule on the `knownInstruction` type. The
question the lead raised: confirm or overrule.

### 1.2 It is forced, not chosen — MEASURED

I ran a throwaway probe in a detached worktree at `79fada44` (removed afterwards; two probe
files, deleted), calling the package's own unexported `liftAll`, `attribute` and
`matchesCommand`. The whole lift set:

| candidate | kind(s) | entries it matches |
|---|---|---|
| `uzi login` | RUNTIME ×2 | `uzi login` |
| `uzi repo list` | RUNTIME | `uzi repo list` |
| `uzi review backlog` | HELP ×2 | `uzi review backlog` |
| **`uzi review backlog --run <run-id>`** | **RUNTIME** | **`uzi review backlog`, `uzi review backlog --run <run-id>`** |
| `uzi review show %s` | RUNTIME | `uzi review show` |
| `uzi review undo %s %s` | RUNTIME | `uzi review undo` |
| `uzi run inputs` | HELP | `uzi run inputs` |
| `uzi skill install` | HELP | `uzi skill install` |
| `uzi worker set-token` | HELP | `uzi worker set-token` |

**9 candidates, 0 UNKNOWN — the baseline the brief cited, independently re-measured, not
inherited.** Exactly **one** candidate in the whole package matches more than one entry, so the
tie-break has exactly one live consumer today.

Then I simulated the two alternatives over the same lift set:

```
FIRST-MATCH would make "uzi review backlog" ambiguous: map[HELP:[review.go:212 review.go:213] RUNTIME:[review.go:403]]
ALL-MATCH   would make "uzi review backlog" ambiguous: map[HELP:[review.go:212 review.go:213] RUNTIME:[review.go:403]]
```

Both land the RUNTIME candidate on the HELP entry, which then derives two kinds and trips
`TestInstructionEvidenceIsWellFormed`'s `len(byKind) > 1` arm — *"is printed from BOTH help and
runtime positions. Split it into two entries."* The entries **are** already split. The only way
to green either alternative is to delete one of the two entries, which is to undo C0's finding
that the registry conflates two kinds — the finding the whole of Part C is built on.

So the alternatives are not merely worse. Against a registry that C0 requires, they are
**unsatisfiable**. Confirmed.

### 1.3 "Longest" is never ambiguous — INFERRED, with one exception that is real

If entries A and B both match candidate C on a word boundary, both are word-boundary prefixes
of the same string C, so one is a prefix of the other and their byte lengths are strictly
ordered. `len(k.command)` is therefore a faithful specificity measure, not a heuristic.

The single exception is **two entries with the identical `command` string**. MEASURED (probe:
appended a duplicate `uzi repo list` entry): `attribute`'s strict `>` keeps the **first**, and
the duplicate surfaces only through `TestRegisteredInstructionsAreStillPrinted` as *"no lifted
candidate matches any more — remove the entry rather than leaving a claim about code that is
gone"*, which is the wrong diagnosis: the code is not gone, a duplicate ate it.

**REQUIRED (small, same commit as anything else touching this file):** a registry
well-formedness check. In `TestInstructionEvidenceIsWellFormed`, before the per-entry loop:

- no two entries share a `command`;
- `strings.TrimSpace(k.command) == k.command` — a trailing space breaks `entry+" "` matching
  outright and would silently un-match every candidate for that entry.

Neither fires today (MEASURED: no duplicates, no whitespace). Both are ~6 lines and they remove
the only place the tie-break can be silent.

### 1.4 What the rule costs the guarantee — MEASURED, and it is not the tie-break's fault

The guarantee is *"a FOURTH instruction cannot land silently."* The residual is a property of
**prefix matching**, not of the tie-break: every alternative absorbs identically. Measured
against the real registry at `79fada44`:

```
"uzi repo list --json"                     -> ABSORBED by "uzi repo list"                (evidenceE2E)
"uzi review undo --all"                    -> ABSORBED by "uzi review undo"              (evidenceE2E)
"uzi review show %s --json"                -> ABSORBED by "uzi review show"              (evidenceE2E)
"uzi review backlog --run <run-id> --json" -> ABSORBED by "uzi review backlog --run …"   (evidenceNotExecuted)
"uzi login --device"                       -> ABSORBED by "uzi login"                    (evidenceNotExecuted)
"uzi review backlog-export --all"          -> UNREGISTERED (fails the build)
"uzi review backlogger"                    -> UNREGISTERED (fails the build)
"uzi review showdown"                      -> UNREGISTERED (fails the build)
```

The bottom three are C3's fix working. The top three are the residual, and it is **in class**:
a new hint printing `uzi repo list --json` from a new site inherits an `evidenceE2E` claim that
someone **executed** it. A false EXECUTED claim is precisely what the registry exists to
prevent — two instructions shipped false, both parsed perfectly.

Three mitigations already in the tree, named so nobody double-pays for them:

- a new site of a **different kind** is caught (`len(byKind) > 1`);
- a new command **path** rather than an extension is caught — that is how `uzi worker
  set-token` arrived from PRD #104 and reddened the build;
- the e2e rows lift from the emitting command's **own output**, so a changed text at the **same**
  site is still executed verbatim.

The residual is exactly: **same kind, new site, extension text.**

### 1.5 Rejected: exact-match entries

Requiring `command` to equal the lifted candidate closes the absorption completely. It also keys
the registry on **format strings** — `uzi review undo %s %s` — so a `%s`→`%v` edit, a reordered
Printf, or any new flag becomes a registry edit, and the entry text stops being a command anyone
can read as a command. That churn is what gets a mechanism deleted, which is the same reasoning
that put `evidenceNotExecuted` in the design as a legal, green, permanent state. **Do not take
it.**

### 1.6 Recommended follow-up (its own commit, NOT a rework of `4b94f714`): bind the entry to its site

The cheapest thing that closes 1.4's residual without 1.5's churn is to make the entry declare
**where** it is emitted from, by enclosing function name — never by line, per the repo's own
rule that a line number is meaningless without a SHA.

`inspectWithStack` already carries the ancestor chain, so the enclosing `*ast.FuncDecl` is in
hand at classification time. Add `sites []string` to `knownInstruction`, collect the enclosing
function name alongside `file:line` in `candidate`, and assert that the set of functions
producing an entry's candidates **equals** the declared set — an undeclared function is a new
emitter the author must either claim or split into its own entry.

MEASURED — every one of the nine sites has an enclosing `FuncDecl`, so no package-scope fallback
is needed today:

| candidate | enclosing function |
|---|---|
| `uzi review undo %s %s`, `uzi review backlog --run <run-id>` | `runGroupDisposition` |
| `uzi review show %s` | `resolveRecID` |
| `uzi review backlog` | `addCoordFlags` |
| `uzi repo list`, `uzi run inputs` | `newRunCmd` |
| `uzi login` | `pollUntilDone` |
| `uzi skill install` | `newSkillCmd` |
| `uzi worker set-token` | `newTokenCmd` |

**Its honest limit, stated because the partial version is what will actually ship:**
`newRunCmd` is a 200-line command constructor holding both a HELP field and a RUNTIME `Exitf`.
A second emitter added *inside* `newRunCmd` would still be absorbed. What the check closes is
the **cross-function** case — a new hint in a new file or a new handler — which is the shape a
new feature takes. It is a partial closure, and it is strictly better than nothing.

The second thing it buys is worth as much: it converts each entry's `note` from prose into a
checked claim. C0 measured **four of eight** notes as misdescribing their own site. Nothing in
the current mechanism would have caught that, and nothing would catch it recurring.

~30 lines, no new dependency. **Recommend taking it — as a separate commit.** `4b94f714` is
coherent and green as it stands (MEASURED: all three backstop tests `--- PASS` at `79fada44`
with `GOFLAGS=-buildvcs=false`), and re-opening a reviewed file to add a feature is how this PRD
lost work before.

### 1.7 The zero-UNKNOWN baseline confirms the choice rather than complicating it

Because every current candidate classifies, most-specific-wins never arbitrates an UNKNOWN
today, and a later `kindUnknown` is by construction a new emitter rather than an original gap.
When one appears, attribution routes it to the **single most precise** entry, so the failure
names one entry. Under all-matching the same new emitter would redden `uzi review backlog`
*and* `uzi review backlog --run <run-id>` — two error lines for one cause. Same detection,
better diagnosis.

### 1.8 One existing claim I checked and it holds

`TestRegisteredInstructionsAreStillPrinted`'s message says *"Matching is on a word boundary and
most-specific-wins, so a longer entry shadowing this one would also show up here."* True: if the
`addCoordFlags` usage text ever went away, `uzi review backlog` would be reported unmatched
while the longer entry stayed green. The message is honest about its own remedy being wrong in
that case. Leave it.

---

## Ruling 2 — Part B (M8b): buildable, but **narrow it to two blocks**

### 2.0 The headline measurement: Part B's surface has ZERO drift

The brief's premise is that the design is 122 commits behind. The commit count is right and the
drift is **not in Part B**. MEASURED, `git diff --stat ad6c63d9 origin/main` scoped to Part B's
entire dependency set:

| path | delta `ad6c63d9` → `6be9f542` |
|---|---|
| `e2e/run-e2e.sh`, `e2e/forge-fake/forge-fake.mjs`, `e2e/*` | **unchanged** |
| `api/internal/workersvc/judge_backlog.go` | **unchanged** |
| `api/internal/store/queries/**` | **unchanged** |
| `api/internal/store/migrations/**` | **unchanged — ZERO new migrations** |
| `api/internal/apitypes/**` | **unchanged** |
| `api/internal/forgesvc/**`, `api/internal/poller/**` | **unchanged** |
| `api/cmd/uzi/**`, `api/internal/uzicli/**` | **unchanged** |
| `web/src/mocks/mockApi.ts` | **unchanged** |

The only files in that neighbourhood that moved are `workersvc/sanitize.go` (new, PRD #108
message sanitation), `workersvc/service.go` (+302, same PRD — MEASURED, its diff contains no
occurrence of judge/backlog/recommendation/disposition), and `web/src/mocks/data.ts` (16 lines
of PRD #115 rate-limit demo numbers). None of them is Part B's.

Every line number the design note cites into `judge_backlog.go` still lands: `:49` `bucketRank`,
`:77` `rationalePreview`, `:136` `const JudgeBacklogMaxRows = 2000`, `:162` `Lim: …+1`,
`:167-169` the cut, `:198` `GroupJudgeRecommendations`, `:303` `filterGroups`. **The prescription
does not fail against main.** What has changed is the *reason to build it*, and that is the rest
of this ruling.

### 2.1 What actually moved: the sibling branches, not main

MEASURED, per-branch file lists vs `ad6c63d9`:

- **`feature/prd-98-t2-cli` @ `79fada44` is the ONLY branch touching `e2e/run-e2e.sh`**, and it
  has already opened a `PRD #98 M8c` phase (+188 lines) **at exactly the insertion point B0
  prescribes** — after `pass "cleaned up the filed-issue run …"`, before the judge-disable
  restore. B0's ruling is now satisfied by a phase that exists. M8b's author **extends that
  block**; a second `#98` phase in the same file is the thing to avoid.
- **No branch touches `e2e/forge-fake/forge-fake.mjs`.** B6's mutator is genuinely unbuilt.
- **`feature/prd-98-t2-seam6` @ `f4f78eaa` has already built the whole of Part A**, including
  A8's truncation as option (c), the dev toggle (`MOCK_BACKLOG_MAX_ROWS`, `?mock=truncated-backlog`).
  That product question is closed by the tree.
- **`feature/prd-98-tier2` @ `3e8a25ef` added `api/internal/store/judge_recommendations_integration_test.go`** —
  and this is the finding that reprices Part B. See 2.3.
- `feature/prd-98-t2-lim` @ `8ce7ba50` **did** land new routes (`handler/handler.go`,
  `handler/cli_tokens.go`), which is the design's own "watch item" firing. Re-derived: it
  touches no judge route, no e2e file, no forge-fake, no poller. **No interaction with Part B.**

### 2.2 Re-deriving the two claims the brief named

**Claim 1 — the `?run=` anchor and the row cap live in SQL, were not executed by the seam-6
harness, and "read as equivalent" needs a live DB, which is what M8b is for.**

- *Anchor lives in SQL*: **CONFIRMED.** `api/internal/store/queries/judge_recommendations.sql`
  carries an `EXISTS` semi-join on `sqlc.narg('run_anchor')`, pinned to `@user_id` directly
  rather than correlated. `judge_backlog.go` passes it down; the grouper never sees it.
- *Row cap lives above the grouper*: **CONFIRMED.** `judge_backlog.go:162,167,169` reads
  `MaxRows+1`, computes `truncated`, slices, **then** calls `GroupJudgeRecommendations`.
- *Not executed by the seam-6 harness*: **CONFIRMED**, and now against the built article rather
  than a plan — `judge_backlog_fidelity_test.go` at `f4f78eaa` calls exactly
  `filterGroups(GroupJudgeRecommendations(c.Rows), c.Bucket)` and nothing else. No anchor, no cap.
- *"and that is exactly what M8b is for"*: **HALF OF THIS IS NO LONGER TRUE.** The **anchor**
  half has since been executed against a real database by `TestJudgeBacklogRunAnchorLiveDB`
  (`tier2` @ `3e8a25ef`), whose own header states the three properties B3 asserts — owner
  scoping, the semi-join keeping other-run occurrences, and an owner-scoped anchor — and whose
  body carries the assertion message *"(the anchor selects coordinates, it does not trim
  occurrences)"*. That **is** B3(c), the assertion the design called *"the assertion the
  Go-grouper harness structurally could not make"*. It is made, more cheaply and more precisely,
  one layer down.
  The **cap** half is still unexecuted anywhere live: the largest `Lim` any live-DB test passes
  is 1000, and the only cap assertion in the tree (`handler/judge_recommendations_test.go:359-373`)
  feeds a **fake store** 2001 rows, which proves the service's slice and says nothing about the
  query's `LIMIT`.

**Claim 2 — B4 gates Part C's truncation-remedy row; they must land in the same commit; extend
the existing e2e block.**

- *Gating*: **CONFIRMED.** `JudgeBacklogMaxRows = 2000` is a compile-time const with no env
  override, and the remedy is printed on the post-write re-read's `truncated` flag
  (`review.go:403`, inside `runGroupDisposition`). 2001 rows is the only arrangement that
  reaches it. The registry entry on `t2-cli` already declares `evidenceNotExecuted` with exactly
  this reason.
- *"same commit"*: **right conclusion, and the design gave the wrong reason.** The seed and the
  row are coupled by **ordering within one harness phase**, which a commit boundary does not
  enforce. What genuinely is same-commit-coupled is the **registry flip**: turning that entry
  from `evidenceNotExecuted` into `evidenceE2E` requires a `where` label that
  `TestInstructionEvidenceIsWellFormed` looks for by reading `../../../e2e/run-e2e.sh`. Flip it
  without the harness row and `go test ./...` goes red; land the row without the flip and the
  registry keeps a stale not-executed claim. **So: harness row + registry flip in one commit;
  seed and row in one phase, seed first.**
- *"extend the existing block"*: **more true than when written, for a new reason** — the block
  now exists (2.1). Restated as an instruction: append to the `PRD #98 M8c` phase from
  `4b94f714`, do not open a second `#98` phase, and rebase onto the merged `t2-cli` before
  writing a line.

### 2.3 Why Part B as specified is now mostly duplicate — MEASURED

Part B's stated value is *"entirely in assertions fakes structurally cannot make."* That premise
was true on 2026-07-21. It is no longer the right test, because the alternative is not a fake —
it is a **live-DB store test running the real SQL against a real Postgres**, and `tier2` plus
the already-merged `#94`/`#98` work now carries a lot of them.

| block | design's justification | what the tree says on 2026-07-25 | verdict |
|---|---|---|---|
| **B1** dedup grouping on the wire | fakes can't | grouper pinned in *both* runtimes by the seam-6 fixture (`f4f78eaa`) and by `handler/judge_recommendations_test.go`; the SQL join spine pinned by `TestJudgeBacklogProjectsEveryColumnLiveDB` | **DROP** |
| **B2** `occurrences > run_count` | wire-only | grouper half: seam-6 fixture case 2, both runtimes. Bulk half: `TestBulkDispositionHandlesDuplicateCoordinateLiveDB`. I did **not** find a live-DB read-path test seeding one coordinate **twice on one review** | **DROP the block; fold the gap into B4′ (2.4) as one extra seeded row** |
| **B3** the `?run=` anchor semi-join | *"the assertion the Go-grouper harness structurally could not make"* | `TestJudgeBacklogRunAnchorLiveDB` makes all three, incl. (c) | **DROP** |
| **B4** row cap cuts before grouping | 2001-row seed | **unpinned anywhere live**; max live `Lim` is 1000; the one cap test uses a fake store. And it gates Part C | **KEEP — see B4′** |
| **B5** group Dismiss fans out, `scope=open` | wire | `TestBulkDispositionScopeLiveDB`, `…FansOutAcrossRunsLiveDB`, `…FiledMemberIsNotOpenLiveDB`, `…SettledAddressesAreDistinctPerRunLiveDB` | **DROP** |
| **B6** close→Done, edge-once, Undo sticks, dismissed not overwritten | needs the fake mutator | `TestFiledIssueCloseAutoDonesOnceLiveDB`, `…UndoSticksLiveDB`, `…DoesNotOverwriteDismissedLiveDB`, `…ReopenDoesNotReopenLiveDB`, `…IsRepoScopedLiveDB`, `…SkipsUnsettledAndOrphanedLiveDB` — the entire matrix | **REDUCE to the wiring leg — see B6′** |
| **B6a** positive-controlled negative windows | makes B6's windows non-vacuous | exists only to serve windows that are now dropped | **DROP with them** |
| **B7** notification deep-link | drop if client-side | `notificationLink` is `web/src/lib/notifications.ts`, unit-covered; `run_id` on the `judge_review` notification is already asserted in the #46 phase | **DROP — as the design already said** |
| **B8** no token spend | already struck | still dead | **DROP** |

**The residual that is genuinely e2e-only, and it is the strongest thing left in Part B.** Every
one of those six close-sync live-DB tests calls `svc.SyncFiledIssueCloses(ctx, repoID)`
**directly**. The poller's call site (`poller/poller.go:251`) is covered only by
`forgesvc/judge_issue_close_test.go`'s `TestSyncFiledIssueClosesWiring`, which runs against a
**fake store**. MEASURED: nothing in the repo runs the poller. So the chain

> a real forge issue is closed → the issue cache reflects it → the poller's tick calls
> `SyncFiledIssueCloses` → a disposition with the right provenance appears

is **unpinned end to end**. That is the assertion neither a fake nor a live-DB store test can
make, and it is what M6 claims to do.

### 2.4 The narrowed Part B, as a build spec

Two blocks. Both extend the existing `PRD #98 M8c` phase in `e2e/run-e2e.sh`.

**B4′ — the row cap, and Part C's remedy executed against it.** Supersedes B4 and folds in B2's
one uncovered gap.

1. Seed a dedicated review; `INSERT … SELECT FROM generate_series` for 2001 recommendation rows
   on it. Include **two rows sharing one `(category, target)` on that one review** — that is B2's
   uncovered shape and it costs nothing here (MEASURED at `ad6c63d9`, restated by the harness's
   own comment: there is no UNIQUE on `(review_id, category, target)`).
2. Assert `truncated == true` on `GET /api/me/judge/recommendations?bucket=all`, and that a
   coordinate seeded only in the **oldest** review is **absent** — the cut-before-grouping
   property. A `truncated` boolean alone is satisfiable by a flag flip over complete data.
3. Run the group disposition against the truncated backlog, lift the remedy through the existing
   `run_printed_instructions` helper (`4b94f714`), execute it verbatim, assert the re-read is
   **not** truncated.
4. Flip `uzi review backlog --run <run-id>` from `evidenceNotExecuted` to `evidenceE2E` with a
   `where` label matching the row's `say`/label string — **in the same commit** (2.2).
5. `DELETE` the seeded review and **re-assert `truncated == false`**. This is the positive
   control: without it a `truncated:true` from an unrelated cause is indistinguishable.
6. Run it **last** in the phase, so the teardown is the single owner of the cleanup.

INFERRED, verify at implementation: the seed is sub-second. If it is not, say so rather than
raising the cap.

**B6′ — the close→Done wiring leg.** Supersedes B6 and B6a. **Only the wiring**; the matrix is
live-DB covered and re-asserting it here buys nothing but harness minutes.

1. Add `POST /_e2e/issues/{iid}/state {state}` to `e2e/forge-fake/forge-fake.mjs`, mirroring the
   MR analogue. MEASURED and still true at `6be9f542`: issues are created `state: "opened"` and
   **no route mutates it**; the GitLab `PUT /issues/{iid}` handler applies only
   `add_labels`/`remove_labels`. Bump `updated_at` — because a real forge does, not because the
   harness needs it (see 3 below).
2. Helpers: `close_issue IID` (mirrors `flip_mr`) and `wait_disposition REVIEW CAT TGT WANT
   [TIMEOUT]` (over `wait_eq` + `db_psql`). **Do NOT add `apidelete`** — see 2.5.
3. On `$F_IID` (already filed against `$F_REC` by the #68 phase): precondition that the
   coordinate buckets `filed` with `close_synced_at IS NULL` and no disposition row; close it;
   `wait_disposition … done`; assert the **provenance triple** `status='done'`,
   `set_via='issue_close'`, `set_by_user_id IS NULL`.
4. **State the fidelity limit in the phase's comment, in these words or better.** MEASURED and
   re-verified at `6be9f542`: `forge-fake.mjs` contains **zero** occurrences of `updated_after`
   or any equivalent — GitLab `GET /issues` returns `Object.values(state.issues)` wholesale, by
   deliberate design (*"Keeps a reconcile pass from evicting the cache"*), and the Forgejo lane
   does the same. `IncrementalSync` really does send `UpdatedAfter` (`forgesvc/service.go:297`,
   `forge/gitlab.go:257-258`). **So this block proves the poller wires the edge up given the
   cache reflects the close; it does NOT prove the real incremental sync would ever observe the
   close.** Do not close that hole inside #98 — it changes `GET /issues` semantics for every
   phase that depends on "return all recorded issues". Raise it separately.
5. **Lane awareness, which the design note never addressed.** The harness has had two lanes since
   before the note was written: `UZI_E2E_FORGE=gitlab|forgejo`. The `#98` phases sit in the
   gitlab lane. The mutator must either be lane-neutral (it operates on shared `state.issues`,
   so it is) or the block must be gated to the gitlab lane explicitly. Read the lane guard before
   writing, and say which you chose at the site.

### 2.5 `apidelete`: fix the comment, not the code — supersedes the design's B6a ruling

MEASURED at `6be9f542`: `apidelete` appears **exactly once** in `e2e/run-e2e.sh`, in
`retry_read`'s own comment listing write helpers that are *"deliberately NOT wrapped"*. It is
never defined. The harness's single DELETE is still an inline `curl … -X DELETE` against
`/api/me/secrets/anthropic_token/…`. The dangling reference is real.

The design ruled *"define `apidelete` properly"* — but that was in service of B6a's Undo window,
which 2.3 drops. **Defining a helper nothing calls is worse than the dangling reference**: it is
dead code plus a comment that is now true about a function nobody uses. **Ruling: correct the
comment to name only the helpers that exist.** One line, it makes the doc true, and it is in
scope because M8b touches that file anyway.

### 2.6 Two ride-alongs the same commit should carry

- **The harness's final summary line** enumerates every phase it ran and does **not** name PRD
  #98; `4b94f714` added a phase without adding to it. Add `+ PRD #98 judge menu`.
- **`CLAUDE.md:103` is stale and I am not fixing it here because it is outside this ruling's
  scope.** It says *"`api/internal/forge` … `gitlab.go` is the only driver."* MEASURED at
  `6be9f542`: `api/internal/forge/` contains `forgejo.go` and `forgejo_pipelines.go`, `adr/`
  contains `0065-forgejo-driver.md`, and `run-e2e.sh` has a whole `UZI_E2E_FORGE=forgejo` lane.
  **Proposed correction, for the lead to authorise:** *"`gitlab.go` and `forgejo.go` are the
  drivers (ADR 0065)."* Flagging rather than editing, because `CLAUDE.md` is shared and this
  wave has four writers.

### 2.7 Effort, revised

| | design (v4) | narrowed |
|---|---|---|
| forge-fake mutator + `close_issue` | 1h | 1h |
| helpers (`wait_disposition`; **no** `apidelete`) | 0.5h | 0.3h |
| B1-B3 | 2h | **0 — dropped** |
| B4 → **B4′** (seed, cap, remedy row, registry flip, control) | 1.5h | 2h |
| B5 | 2h | **0 — dropped** |
| B6 matrix → **B6′** wiring leg | 3h | 1.5h |
| B6a | 1.5h | **0 — dropped** |
| B7 | 0.5h | **0 — dropped, documented** |
| comment/summary ride-alongs | — | 0.3h |
| iteration against a ~30-min harness (budget ≥3 runs) | 2× write time | 2× write time |
| **total** | **~2.5 days** | **~1 to 1.5 days** |

Added harness runtime: the design's `+3 to +4 min` drops to roughly **+1 min** — the 2001-row
seed plus one close-edge wait. The second judged run the user funded is no longer needed: B4′
direct-seeds (the harness's own precedent, which `4b94f714` already uses), and B6′ rides the
`$F_IID` the #68 phase already filed.

---

## For the lead — one decision, and it is not mine to take

**The narrowing contradicts a standing user ruling.** On 2026-07-21 the user ruled *"Drive to
100%; M8b is funded. Do not trim it for time."* What I am recommending is **not** a time trim —
it is duplicate removal, and the duplication did not exist when that ruling was made:
`tier2`'s `judge_recommendations_integration_test.go` and the six `TestFiledIssueClose*LiveDB`
tests are what changed. But it **is** a change to a funded scope, so it goes to you, not to the
coder as a guess.

**My recommendation, and the honest way to honour "100%":** build B4′ and B6′, and record in
`prds/98-judge-menu.md` — by test function name — where B1, B2, B3, B5 and B6's matrix are
actually pinned. That is 100% of the properties covered, with the coverage *named* rather than
*repeated*, at roughly 40% of the cost and about a quarter of the added harness minutes. A
property asserted twice is not covered twice; it is covered once and paid for twice, and the
second copy is the one that rots when the first is edited.

**If you would rather keep Part B whole**, it is buildable exactly as the design note specifies —
2.0 is the evidence that nothing in its prescription has gone stale. You would be paying ~1.5
extra days and ~3 extra harness minutes per run for wire-level restatements of live-DB
assertions. Say the word and the spec above becomes an addition rather than a replacement.

**One request against `prds/98-judge-menu.md`, carried forward from the design note because it
is still unactioned:** the Success Criteria say the no-token-spend property is *"proven by the
M8 e2e leg"*. It is not and cannot be — see the B8 refutation. It is proven by
`TestDispositionTouchesStoreOnly` and `TestBulkDispositionTouchesStoreOnly`. Left as a request;
that file is yours.

## What these rulings cannot catch, in one place

Ruling 1: a second emitter added **inside an already-declared function** (1.6's stated limit);
an instruction assembled from concatenated fragments at runtime; whether the outcome an e2e row
asserts is the *right* outcome.
Ruling 2: the incremental-sync path, structurally, for as long as the fake ignores
`updated_after` (2.4/4); the `LIMIT`/`ORDER BY` interaction at scale, which B4′ pins the flag and
the cut ordering of but not the behaviour of; and whether a registered e2e phase **asserts**
anything, as opposed to existing.
