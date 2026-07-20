# PRD #97: e2e suite hardening & speedup

**GitLab Issue**: [#97](https://gitlab.example.com/vtmocanu/uzi/-/issues/97)
**Status**: **COMPLETE — ready for MR.** All shipped milestones validated: M1/M2/M3/M5/M8 closed earlier; **M4+M9** gate-passed (173 PASS / 0 FAIL) with reviewer APPROVE + auditor PASS and both owed mutation tests proven RED; **M9b** re-gated (173/0 unchanged, 113 instrumented waits, up from 92) and APPROVED; **M9c** closed a live authz coverage gap the review wave surfaced, with six parameter-construction mutations going from 10-of-11 surviving to all killed. M6 deferred to [#100](https://gitlab.example.com/vtmocanu/uzi/-/issues/100); M7 optional, never started. **Out-of-scope follow-ups needing their own issue are listed under "Follow-up found by M9c".** See RESUME HERE below.
**Priority**: Medium
**Origin**: Three-agent read-only review of the e2e suite (2026-07-20) — coverage (add/drop), speed, and harness-structure passes. Two independent agents flagged the git Basic-auth default as the single highest-value gap.
**Review**: fable adversarial pass folded in (2026-07-20). Load-bearing corrections: M1's "flip the happy-path leg to smart-HTTP" is **not** viable (forge-fake routes every repo path to one bare, breaking the PRD #42 two-repo phase) — only the second-push-pass option survives, and the existing `E2E_GIT_SMART_HTTP=1` full run is likely already broken at #42; dropping #46 Phase B (M4) **breaks the downstream #68 phase** that reads the planted `jq` rec; the #16 collapse must **keep** the non-owner repo-PATCH→404 leg (no handler test covers it); M5's healthcheck saving is ~30-40s (not 55-80s — `start_interval` helps only the two `--wait` boots, not the `--force-recreate` api recreates) and its `assert_no_run_for_issue` default change is a no-op (all call sites pass explicit args). The #94/#53/#33/#40/#46-fallback drops were independently re-verified safe.
**Scope note**: All work is on the test harness and its compose overlay, with two small exceptions that touch product code (a `SWEEP_INTERVAL` config knob in M6, called out explicitly). No product behaviour changes; no e2e assertion is weakened — every "drop" is verified redundant against a cheaper layer, every "faster" preserves the assertion.
> ⚠️ **Corrected 2026-07-20 — this claim was briefly false and is now true again.** One of M4's 11 dropped legs (`#16` private-skill) turned out to have **two** unjoined halves rather than one. The compensating test closed the `ErrNoRows → 404` half; the **authorization** half had no coverage anywhere in the tree, and mutating it into a full bypass on `GET /api/skills/{id}` left `go test ./...` green. M9c (`13508b36`) closes it. Stated here rather than only in the milestone, because this sentence is the PRD's headline promise and it should not be readable as having been true throughout. It was restored, not maintained — and only because a validator went past the question it was asked.

## ▶ RESUME HERE (updated 2026-07-20, after the M4+M9 review wave)

**State: M1/M2/M3/M5/M8 fully closed. M4+M9 gate-passed AND validated — reviewer APPROVE, auditor PASS, both owed mutation tests proven RED. One follow-up (M9b) is in flight. M6 deferred to #100; M7 optional, never started.**

| milestone | state |
|---|---|
| **M1, M2, M3, M5, M8** | ✅ DONE — committed, reviewer APPROVE + auditor PASS, full-suite green |
| **M4** (drops) | ✅ **DONE** — `aad3c201`; 173/0 gate, reviewer APPROVE, auditor PASS |
| **M9** (timing hardening) | ✅ **DONE** — `859a8066`; same gate, reviewer APPROVE, auditor PASS |
| **M9b** (instrumentation completion) | 🔄 **IN FLIGHT** — see below |
| **M6** (speedups) | ⏸ DEFERRED → [issue #100](https://gitlab.example.com/vtmocanu/uzi/-/issues/100) |
| **M7** (refactor) | ⏸ optional, never started |

**The review wave's verdict (2026-07-20).** Both validators cleared the code, and both re-derived the
delta independently: **11 removed − 2 added = −9**, every vanished leg named and mapped to the
commit's accounting table, which was correct in every cell.

- **Reviewer: APPROVE.** The keep-list held and its `⛔ DO NOT DROP` guard comments genuinely ship in
  the file (read in the committed blob, not the diff). `#68`'s rework is dependency-free **by
  mechanism** — it seeds the rec, then re-reads it through `GET /runs/{id}/review`, so the phase only
  proceeds if the DTO actually surfaces the row — not by luck. M9 clears all five of its agreed
  weakening criteria and is strictly stronger than what it replaced: a single read of a ~500ms window
  became a stable-state re-read plus two new preconditions that fail *at the cause*.
  `shellcheck -S style` clean.
- **Auditor: PASS.** Both owed mutation tests go **RED in two independent directions each**, and —
  the decisive part — **each was the only test in the entire `api` suite** that caught its mutation
  (`go test ./...`, not just the one package). That is positive proof the coverage gap was real and
  is now closed, rather than an assertion that it was. `TestJudgeStatsAggregateLadder` stayed green
  under the triage mutation, confirming this PRD's own CITATION CORRECTION by experiment rather than
  by reading. Tree verified byte-identical after every restore.

**⚠️ THREE RECORD ERRORS, CORRECTED HERE. Each was asserted by someone competent and never
re-derived — the exact failure class this PRD exists to kill.**

1. **`wait_card_column 10 → 20s` NEVER HAPPENED.** `859a8066`'s commit message claims it and an
   earlier version of this block repeated it verbatim. **The code deliberately does the opposite, and
   the code is right**: the call site still reads `wait_card_column "$IID" "Later" 10`, under a
   comment citing M9's own "instrument, don't guess" rule; all six call-site ceilings are
   byte-identical across the commit. **M9 shipped three changes, not four**: the `#95` vault lock,
   `sleep 6→8`, and the central instrumentation. The commit message is immutable, so this note is the
   correction. Note the irony — the false claim was a *timeout widen that the milestone explicitly
   forbade*.
2. **"Every wait records actual-vs-ceiling" was an overclaim, and it scopes the gate's headline
   number.** `record_margin` is called from `wait_eq` and `wait_status` only. **Eight bespoke loops
   (~19 call sites) were never instrumented** — `wait_http`, `wait_run_for_issue`,
   `wait_autopilot_done`, `wait_notes`, `wait_review`, `wait_msg_kind`, `wait_msg_text`,
   `wait_tool_result`. So "tightest headroom 10s across 92 instrumented waits" means *tightest among
   instrumented waits*. The three 20s chat-phase helpers are the tightest ceilings in the suite and
   were unmeasured. **M9b closes this** — it is a real gap in M9's deliverable, not a wording fix.
3. **Two ground-state facts in this block were wrong, and had already propagated into a teammate
   brief before the reviewer caught them.** (a) `859a8066`'s `run-e2e.sh` blob is **`a142c5dc`**, not
   `b0424822` — that object does not exist in this repo (`git cat-file -t` → "Not a valid object
   name"). The substantive claim survives by another route: the worktree file at `bf536f76` hashes to
   `a142c5dc`, identical to `859a8066`'s blob, so `run-e2e.sh` is untouched by the four doc-only
   commits. (b) The static pass-leg base is **206**, not 196 — same grep, same SHA.

**Caveat that must travel with any pass-leg count: static pass-legs are NOT runtime PASSes, in both
directions.** Over-count: the forgejo lane (~20 legs) and the `--profile agent-docker` block are
conditional and do not run by default — that is the 206-vs-182 gap. Under-count: the static grep
`^[[:space:]]*pass "` misses one leg that is not at line start (the `#58` XFF leg inside a `case`
arm), so total `pass "` occurrences is 198, not 197. **The static list is valid for the DELTA only**;
quoting 206/197 against 182/173 is apples-to-oranges. The delta holds because both conditional blocks
and the `case` arm are untouched by M4.

**M9b — in flight.** Three items, one coder:
1. Extend `record_margin` to the eight bespoke wait loops (correction 2 above). Diagnostics only:
   success path only, cannot change control flow, cannot fail a run, ceiling bound from a hoisted
   local so the reported ceiling cannot drift from the enforced one.
2. `#68`'s seeded rec: assert the read-back yields **exactly one** id. Found independently by both
   validators, which is why it is worth doing. `review_recommendations` has **no UNIQUE on
   `(review_id, category, target)`**, so a duplicate would make `F_REC` two newline-joined UUIDs; the
   existing non-empty guard passes on that, and the failure would surface far downstream as a
   malformed URL. Latent, not live (`fallbackReview` only emits `install_worker_tool` from
   `signal.missing_tools`, and Phase A plants nothing).
3. Close the `#16` GetSkill seam (~15 lines): the dropped leg's property now lives in two halves that
   no single test joins — the store IT proves `GetSkillForViewer` returns `ErrNoRows` for a non-owner
   against live PG, the route maps `ErrNoRows → 404`, but there is **no `GetSkill` handler test
   anywhere**. Closed with a mutation-proven handler test.

**A full `./e2e/run-e2e.sh` re-run is part of M9b and is not optional**: items 1-2 change
`run-e2e.sh`, which invalidates the validated-tree claim behind the 173/0 gate. Expected 173/0
unchanged (item 1 is diagnostics, item 2 is a guard inside an existing leg) — if the count moves,
that is a stop-and-report, not a reconcile. The re-run also buys the first-ever margin data on those
~19 previously-blind waits.

**`#22` — reconciled (an earlier version of this block contradicted itself, calling it both "passing"
and "RED and UNEXPLAINED").** It **passed** in the gate run, in **0s of its 20s ceiling**. It has
intermittently been RED across the ~10 runs of this PRD, and the mechanism is still **unexplained**.
What is settled is that **no timeout value can matter**: `SetIssuePrdless` (`board.go:661`, forge call
at `:713`) is strictly forge-first and synchronous — on forge failure it returns 502 with the cache
untouched, and the returned card is built from the post-write issue — so a 200 whose card lacks
PRDLESS proves the forge write already returned success. Re-derived from source by the auditor, not
inherited. Three candidates remain (see M9's `#22` entry). **Do not widen the window**; catch a
recurrence with `KEEP_STACK=1` and interrogate forge-fake.

**⚠️ The margin report is STRUCTURALLY INCAPABLE of clearing `#22`, and green runs must never be
read as though it were.** `record_margin` fires on the **success path only** — never on a `fail` or
early-exit path. That is correct by design (a headroom row printed next to a genuine defect would be
actively misleading) and it was verified by execution, not by reading: a helper driven down its
`fail` path recorded **zero** rows. The consequence is that **when `#22` fails, it records nothing**,
so every margin row `#22` can ever produce is drawn exclusively from the branch where the race did
not fire. "0s of 20s, twice running" is therefore **not weak evidence of a fix — it is no evidence,
by construction**, and no number of green runs will change that. The report cannot distinguish
"defect gone" from "race did not fire".
**Status to carry forward: `#22` is INTERMITTENT — it passes in 0s when it passes, the mechanism is
narrowed to three candidates, and it is NOT FIXED.** Do not let a future update record it as
"passing".

**M9b and M9c are both closed and validated.** Final stack on this branch:
`67ec761c` (instrument bespoke waits + `#68` single-rec guard) → `ef286669` (GetSkill 404 seam) →
`9100e7f8` (name uninstrumented loops) → `13508b36` (pin caller identity — the authz fix) →
`5bbeff21` (content anchors + `set -u` note) → `e0a9bd41` (why both hardenings are load-bearing).
Reviewer APPROVE on every commit; auditor PASS with mutation evidence on every test commit. No
amends at any point in the chain.

**Next: MR.** Nothing is owed on this PRD. The only open items are the out-of-scope follow-ups under
"Follow-up found by M9c", which need their own issue and must NOT be folded into this branch.

**Harness image leak — real, and now scoped (candidate M10).** The harness **leaks 4 docker images
per run**: `down -v` reclaims containers and volumes but never images, and the PID-derived project
name guarantees the next run cannot reuse them. Result: 646 of 768 images on the dev machine were
`uzi-e2e-*` orphans (~125GB). A partial prune ran (~523 removed, ~123 left).
**Fix: add `--rmi local` to the teardown `down`.** Verified what that touches: it removes exactly
the four per-run built images (`api`/`web`/`agent`/`forge-fake`, which Compose names
`<project>-<service>` with no user `image:` tag — precisely what `local` means) and **preserves the
pinned externals** (`postgres:17@sha256:…`, `alpine:3.22@sha256:…`, `docker:28-dind-rootless@sha256:…`),
which matters since re-pulling those each run would be a real cost. `KEEP_STACK=1` is **already
exempt by construction** (that branch returns before the `down`) — worth an inline comment saying so
deliberately rather than by accident. Wall-clock cost is near zero: `--rmi local` removes the tagged
image, not the build cache, so the next `compose build` hits cached layers and re-tags. Corollary:
**it stops the ~4-image/run growth but does NOT reclaim the ~71GB build cache** — that is a separate
decision, and the cache is what makes rebuilds cheap, so keep it. Belongs as its own small item
(M10) or a standalone issue; **must not gate M4/M9**.

**⚠️ CORRECTION — the `No such container` daemon error was NOT a daemon fault.** An earlier note here
attributed it to Docker misbehaving under ~145GB of dead state. That is **not supported**. It was
self-inflicted: a run was SIGKILLed mid `up -d --wait`, then `docker compose … down -v
--remove-orphans` was run manually, so the in-flight `up` looked up a container the teardown had just
deleted. The decisive evidence is that the very next run came up cleanly on the same daemon minutes
later. **The leak is real; the daemon fault is not, and nothing so far demonstrates the leak has
caused a failure.**

**Regenerating the auditor's leg-by-leg baseline** (the artifact it called decisive; it lived in a
session scratchpad and does not survive):
```sh
git show 27f72255:e2e/run-e2e.sh | grep -nE '^[[:space:]]*pass "' > /tmp/before.txt
git show aad3c201:e2e/run-e2e.sh | grep -nE '^[[:space:]]*pass "' > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt
```
Every vanished leg must map to a named intended drop, and the restored `#16` leg 3 must **appear**.
**Caveat that must travel with it: static pass-legs (196 at base) are NOT runtime PASSes (182)** —
the forgejo lane and the `--profile agent-docker` block are conditional and don't run by default.
Valid for the DELTA only; quoting the static count against 182 is an apples-to-oranges error.

**~~Small~~ loose end — CORRECTED 2026-07-20, it is not small and not one line.** An earlier version
of this block said `api/internal/handler/runs_test.go` carries pre-existing gofmt drift "worth a
separate one-line fix". Measured: **`gofmt -l ./api` reports 26 files**, and a detached worktree at
`origin/main` reports the **identical 26**. So the drift is repo-wide and entirely pre-existing — not
introduced by this branch, not confined to one file, and not a one-line fix. It stays **out of scope
for this PRD** (a 26-file reformat would swamp a line-by-line-audited diff); it wants its own issue.

**Split integrity, verified not assumed:** `aad3c201` (M4) contains **zero M9 markers** and a diff of
exactly **11 removed / 2 added**, so M4's accounting is reproduced by the split rather than asserted
— independently re-derived by both the reviewer and the auditor. `859a8066` (M9)'s `run-e2e.sh` blob
is **`a142c5dc`** (see correction 3 above; the `b0424822` this block used to cite does not exist in
the repo), and the worktree file at `bf536f76` hashes to the same, so `run-e2e.sh` is untouched by
the four doc-only commits. Caveat for the series: run6 validated the tree **with M9 on top**, so M4's
tree alone was lint/unit-verified but never executed standalone — normal for a split series, stated
rather than implied.

**M9's "net +1 assertion" is a wording error (auditor).** Comparing pass-legs `aad3c201` vs
`859a8066` gives a **zero** delta (197 → 197; the sole difference is a reworded `pass` string). M9's
five additions are `|| fail` **guards inside an existing leg**, not new `pass` legs. Understatement,
not overclaim — but it means "+1 assertion" cannot be reconciled against the 173 count, and a future
reader will try. Read it as **+5 `|| fail` guards, +0 pass legs**.

**Two residuals from M4's drops, named so they are not rediscovered as surprises:**
- **`POST /api/runs/{id}/rejudge` has no end-to-end exercise left.** Phase B was the only place that
  drove the route → judge run enqueued → worker claims → completes. After the drop, `RerunJudge` is
  covered at the service layer (`TestRerunJudgeGates`, `TestRerunJudgeHappyPath`) and the route is
  covered for **auth only** (`cli_auth_livedb_test.go:341`). The handler glue and the enqueue are
  uncovered. Inside the accepted Decision-3 residual — but the commit's `#46` replacement list does
  not mention `rejudge` at all, and "we decided this was acceptable" and "we did not notice" are
  indistinguishable a year from now.
- **The `#46` citation list is incomplete in the commit message.** The `run_messages → payloads`
  link is carried by `store/judge_integration_test.go:83-86` (live PG). Without it the SQL would
  have been faked at every layer, since `TestJudgeClaimCarriesModelAndSignal` uses a `fakeStore`
  with `toolResultPayloads` preset. The link exists and runs in CI; only the citation was missing.

**M9b follow-up items (recorded, not done — none blocks landing):**
- **`wait_status` records no run id — LOW priority** (downgraded by the reviewer, who first called
  it "the biggest blind spot" and then self-corrected after deriving what the proportion actually
  implies). `record_margin "run status -> $want"` collapses **66 call sites** into a handful of
  description strings, so a tight row cannot be traced back to a phase. But the ceiling distribution
  across those 66 sites is 26×90s (stub complete-timeout), 26×90s (implicit), 7×60s, 4×30s, 2×120s,
  1×90s explicit — at which ceilings those rows essentially **cannot** enter a tightest-20 list, and
  in this run **none did** (the list bottomed out at ~20s headroom, so every `wait_status` wait had
  ≥20s to spare). It only bites if one of the 4 sites at 30s or 7 at 60s ever runs hot, and even
  then `-> $want` plus log adjacency narrows it. M9b's own new records *do* carry identifiers, so
  the file is internally inconsistent about it; worth a small follow-up, not a priority.
  *(Recorded as much for the correction as for the finding: an asserted proportion is not a derived
  consequence, and the reviewer caught itself making exactly the error it had just flagged in the
  coder's commit message.)*
- **`67ec761c`'s "roughly doubled the number of instrumented sites" is arithmetically wrong.**
  Measured: 92 → 113 records (+~22%). What multiplied 5× was the number of *helpers* instrumented
  (2 → 10), not sites and not records. The `head -12` → `head -20` decision it justifies is
  correct and worth keeping; only the stated reason is a derived-sounding number nobody derived —
  the same failure mode as M9's coverage overclaim, in the commit written to fix it. The reviewer
  predicted 110-115 **before** the gate printed 113, which is how it got caught.
- **`wait_status`'s rewrite narrows a window, it does not close one — and two validators asked
  different questions about it.** The reviewer asked whether `timeout` could diverge from `deadline`:
  it could not (both evaluated the same `$3` with the same default), so **that half is pure
  de-duplication, not a bug fix**. The coder asked whether `start` could diverge from `deadline`'s
  anchor: it could, because the two `local` statements read `$SECONDS` at different moments, making
  a reported elapsed up to 1s short. **Both answers are correct — different pairs, different
  questions.** What is wrong is only the coder's strength of claim: the new form
  `local start=$SECONDS deadline=$((SECONDS + timeout))` still contains **two** `$SECONDS`
  expansions, so `deadline - start == timeout` is **not** true "by construction" — the window is
  narrowed from two-statements-apart to two-expansions-apart, not eliminated. Residual is ±1s,
  inside `record_margin`'s documented whole-second floor, so **no action**. Recorded because the
  overstatement, not the code, is the thing worth catching.
  *A genuinely by-construction form exists* — derive the deadline from `start`
  (`local start=$SECONDS` then `local deadline=$((start + timeout))`) rather than re-reading the
  clock. **Deliberately NOT done, as a decision rather than an oversight**: it would re-open a
  ten-helper diff after a green gate to buy precision below the instrument's own whole-second floor,
  and a diagnostic does not need to be more precise than the thing it measures. The place it would
  actually pay is PRD #95's per-site sub-second work; carry it there or to #100, not here.
- **The in-file `local` comment undersells its own trap.** It says same-line
  `local a=… b=$((a+1))` "does not sequence"; under the suite's `set -euo pipefail` it is an
  **unbound-variable abort on the first wait**, not a wrong value (verified in bash 5.3). A reader
  who takes the current wording as a style note and re-collapses the lines takes the whole suite
  down. Being strengthened in a comment-only follow-up.

**Four lead errors corrected in-document, kept for the next reader:** the `#68` option-(b)
recommendation (would have raced `UNIQUE (run_id, seq)`); the `#22` "too-tight window" diagnosis
(`wait_eq`'s 2nd arg is SECONDS, not tries); relaying the `b0424822` blob hash and the "196
static base" into a teammate brief as verified ground state when both came from this document
unchecked (correction 3 above); and relaying `67ec761c`'s claim that it **fixed a latent bug** in
`wait_status` — that its reported ceiling could drift from the enforced one. It could not: both
expressions evaluated the same `$3` with the same default, so they were **always equal**. Nothing
was ever observably wrong. It is a duplicated source of truth removed, not a bug fixed, and the
lead handed the overstatement to the reviewer as ground state after reading the commit message
instead of the code. All four were caught by teammates, not by me — which is the argument for
dispatching validators who re-derive rather than confirm. **The pattern across all four is one
thing: treating a careful-sounding claim from a competent source as though it were derived.** Two
of the four were the lead re-transmitting someone else's unverified assertion, which is the
failure mode `.claude/agent-team.md` names as the lead's specific share — relay findings as claims
to check, not facts to apply.

## Problem

The e2e harness (`e2e/run-e2e.sh`, 2984 lines, one serial script driving the full
compose stack with the stub executor + `forge-fake` sidecar) is the local
pre-merge gate. A three-pass review found it is unusually disciplined about
false-greens, but has real gaps in three directions:

1. **A shipped-bug-shaped coverage hole.** The default run rewrites the worker's
   push URL to a local bare repo via `insteadOf` (`run-e2e.sh:559-566`), so git
   ignores all `http.*` config and the worker's `Authorization: Basic` header is
   **never sent**. `e2e/README.md:130-137` says this in plain text: this is
   exactly why the harness "(like every prior test) would not have caught the
   `PRIVATE-TOKEN`-vs-Basic auth bug; the live run did." The fix is already wired
   (`E2E_GIT_SMART_HTTP=1` → forge-fake's smart-HTTP endpoint that 401s without
   valid Basic, asserts at `:1007-1016`) but is **opt-in and off** in the standard
   gate.

2. **Missing full-wire coverage that only the stack can prove.** The top directive
   "`main` is never touched" has **no e2e backstop** — the fake remote would accept
   a push to `main` and the stub run never loads the SDK deny-hook (`guardrails.ts`).
   `/api/ws` (the primary real-time transport) has **zero references** in the
   harness — every stream assertion uses REST `?after=<seq>` replay. The `uzi` CLI,
   a co-equal API consumer that silently rots on DTO/route drift, is **never
   invoked** against the live stack.

3. **A few vacuous negatives, some now-redundant phases, and removable wall-clock.**
   Two secret-hygiene scans pass vacuously if log/`/data` retrieval returns empty
   (no positive control, unlike the scrupulous `/proc` and Decision-3 checks).
   Several phases now duplicate assertions covered more cheaply against real
   Postgres or with handler fakes. And ~55–150s of the ~20-min run is removable
   without dropping a single assertion.

## Solution Overview

One PRD, sequenced so the safe mechanical wins land first and the structural ones
carry explicit author sign-off:

- **Add the full-wire-only coverage** the lower layers cannot reach: git Basic-auth
  push ON by default, a `main`-push-rejection backstop, a live `/api/ws` frame
  assertion, and a thin `uzi` CLI smoke against the running api.
- **Harden the false-greens**: give the two secret-hygiene scans a positive control
  (prove the corpus is non-empty first, mirroring the CI `test:api-store-it`
  gate-on-the-gate), and wrap point-in-time reads so one transient blip doesn't
  abort a 20-min run.
- **Drop / downgrade the verified-redundant phases** — each confirmed (test opened,
  not filename-guessed) to be covered against real Postgres or with handler fakes.
- **Land the speedups** — mechanical (healthcheck `start_interval`, negative-window
  sleeps) then structural (parallelize the health phase, a sweeper interval knob),
  never touching a `wait_*` timeout ceiling (those return on success; lowering only
  adds flake).
- **Optional M7**: extract `e2e/lib.sh` + split phases into `e2e/phases/NN-*.sh`,
  making the phase registry and the implicit inter-phase contract explicit. Keeps
  the single-stack serial model. This de-risks the reorder/parallelize work but is
  not required for the rest.

## Design Decisions

1. **Git Basic-auth: run by default via a second push pass, don't just document the
   flag.** The `insteadOf` local-path rewrite is what makes the default run fast and
   hermetic, so keep it as one pass and add a **second push pass** against the
   smart-HTTP remote so `Authorization: Basic` is actually exercised on every run. A
   flag nobody sets is not coverage. This is the one gap both the coverage and
   structure passes flagged independently. **Do not "flip the happy-path leg to
   smart-HTTP" as an equivalent** (fable review): forge-fake routes *every* repo path
   onto one shared bare (`forge-fake.mjs:381-387`, `PATH_INFO: /repo.git${rest}`),
   so under smart-HTTP the PRD #42 two-repo phase's independent-bare assertions
   (`run-e2e.sh:2665-2670`, run B's branch must be on `repo2.git` and absent from
   `repo.git`) collapse. The second pass must therefore be scoped to a single-repo
   phase (or forge-fake taught real multi-repo git routing, out of scope). Corollary:
   the existing opt-in `E2E_GIT_SMART_HTTP=1` full run is almost certainly **already
   broken at #42** today — M1 must confirm and fix, not just add a new pass.

2. **`main`-push backstop asserts rejection, not just agent-branch success.** Today
   the harness only asserts the *agent* branch was pushed + MR opened
   (`:992-1001`); it never attempts a `main` push and asserts it is refused. The
   fake remote is a bare repo with `http.receivepack true` and no protected-branch /
   pre-receive hook (`:513-518`; forge-fake.mjs `GIT_HTTP_EXPORT_ALL=1`,
   `:383-396`), so this needs a real ref-filter on the fake (a pre-receive hook that
   rejects `refs/heads/main`) for the assertion to mean anything. The invariant
   currently rests entirely on unit tests.

3. **Drops are downgrades to the cheapest layer that still proves the property, not
   deletions of coverage.** Each dropped/downgraded phase was verified against the
   named lower-layer test. The residual "the HTTP route is wired to the store" glue
   is accepted as covered by handler router tests. See M4 for the table and the
   explicit **do-NOT-drop** guard list.

4. **Timeout ceilings are off-limits; only real wall-clock is touched.** `wait_*`
   helpers return the instant state is reached (`run-e2e.sh:302,347`), so
   `UZI_E2E_COMPLETE_TIMEOUT` and the 30/40/60/120 ceilings cost ~0 on the happy
   path. Speedups target only: blind `sleep N`, negative-assertion windows,
   healthcheck detection latency, and structural serialization. Lowering a ceiling
   buys nothing and adds flake under host contention — explicitly out of scope.

5. **Negative-window sleeps floor at 2 poll ticks — except reconcile-driven ones.**
   With `FORGE_POLL_INTERVAL=2s`, a "prove X did not happen" window needs one tick to
   act + one to confirm-stable = 4s. 6→4s is safe there; below 4s is not. **But the
   FullSync-eviction negative (`:1676`) waits on a *reconcile* tick, which fires only
   every 2nd poll tick** (`FORGE_RECONCILE_EVERY=2` → 4s period; the poll interval
   also doubles as the whole-tick deadline, `:1420-1422`). Floor reconcile-driven
   negatives at **2 reconcile periods**, not 2 poll ticks — do not take `:1676` to 4s
   (fable review).

6. **`SWEEP_INTERVAL` is a real (if tiny) product change.** `sweeper.New()` already
   accepts an interval (`api/internal/sweeper/sweeper.go:51`) but `main.go:357`
   passes `0`→15s default (`sweeper.go:19`). Wiring a config var is one line of
   product code and lets the overlay shrink stall-detection latency + the nudge-once
   `sleep 18` (`:2781`). Flagged as product-touching; goes through the normal MR
   flow, not the PRD-doc-to-main path.

7. **Structural speedups carry author sign-off gates.** Parallelizing the health
   phase (M6) and dropping the heartbeat-stale recreate (`:2685`) depend on
   assumptions about per-run state isolation and the 45s stale default that only the
   author can confirm. They are separated from the mechanical wins so a "no" on one
   doesn't block the rest.

## Touchpoints

- `e2e/run-e2e.sh` — the harness (all milestones).
- `e2e/docker-compose.e2e.yml` — overlay: healthcheck `start_interval` (M5), chat
  idle / sweep / stale env (M6).
- `e2e/forge-fake/forge-fake.mjs` — pre-receive `main`-reject hook (M2), and (if
  the GitLab-lane single-pipeline gap is addressed) a newest-first `/pipelines`
  list.
- `e2e/README.md` — document the new default git-auth pass, the WS/CLI legs, and any
  new knobs.
- `api/internal/sweeper/sweeper.go` + `api/cmd/server/main.go` — the `SWEEP_INTERVAL`
  knob (M6, product code, normal MR flow).
- (M7 only) new `e2e/lib.sh` + `e2e/phases/NN-*.sh` + a thin driver.

## Milestones

- [x] **M1 — git Basic-auth default + `main`-push backstop** (highest value)
      — DONE `1e4d88cf`; reviewer APPROVE + auditor PASS; both full harness lanes
      green (default 171/0, `E2E_GIT_SMART_HTTP=1` 172/0, previously red at #42).
      Recommend a human re-run of both lanes on the target host before merge (neither
      validator re-ran the ~20-min stack). Impl notes: single-repo M1 phase drops
      insteadOf mode-aware (restored byte-identically), worker push traverses
      forge-fake's Basic gate, non-vacuous receive-pack counter positive control;
      pre-receive `refs/heads/main`-reject hook in both bares (explicit `chmod 0755`,
      installed after the seed push, force/delete/alt-refspec bypasses all closed):
      make the worker's git-over-HTTPS Basic-auth push run on **every** default run
      via a **second push pass** against the smart-HTTP remote, scoped to a
      single-repo phase (Decision 1 — a happy-path flip is NOT viable; forge-fake's
      one-bare routing breaks the #42 two-repo assertions). First **confirm and fix
      the already-broken `E2E_GIT_SMART_HTTP=1` full run at #42**, then wire the pass
      so `Authorization: Basic` is exercised and a credential-injection regression
      turns the git-layer assertions red. Add a pre-receive `refs/heads/main`-reject
      hook to `forge-fake` and a phase that attempts a `main` push and **asserts it is
      refused** (Decision 2); the hook must be **portable to both images** — it fires
      inside the *agent* container for local-path pushes and inside *forge-fake* for
      smart-HTTP (the bares are one shared host dir, overlay `:35`/`:177`). Self-test
      the hook (push main → refused; push agent branch → accepted). Update
      `README.md` to drop the "would not catch the Basic-auth bug" caveat.
- [x] **M2 — live `/api/ws` + `uzi` CLI smoke** — DONE `5d21ea3c`; reviewer APPROVE +
      auditor PASS; full e2e green (178 PASS / 0 FAIL, run5). WS leg: `ws://api:8080/api/ws?run=`
      with the `uzi_auth` cookie read from the HttpOnly jar line by name, subscribe-then-approve
      ordering, asserts a real hub `type:"message" seq>0` frame, plus a no-cookie-rejected
      negative control. CLI leg: `uzc_` minted via `POST /api/me/cli-tokens`, run hermetically
      under `env -i`, `run list --json` id-set must equal `GET /api/runs`, `run approve` must
      advance a parked run. Placed BEFORE the M1 git-auth leg (keeps M2's completing runs away
      from PRD #95's timing-sensitive steer-queue read; verified via a baseline run). Known
      residual: nginx's `location = /api/ws` proxy stays uncovered (probe is agent→api direct,
      documented in the leg); LOW audit note — the JWT rides the probe's argv, prefer
      `exec -e` next time (matches M1's existing pattern, not a regression). New host dep:
      `go` on PATH for the CLI build (documented; `-buildvcs=false` fallback is worktree-local,
      never committed). Original scope: a WS client subscribes to a live run
      over `/api/ws` and asserts it receives live frames during a real run (not just
      the REST `?after=` replay). **No new tooling needed** (fable review): the agent
      container's Node 22 has a global `WebSocket`, so a `compose exec agent node -e`
      probe suffices (mirroring the smart-HTTP probe at `:1009`) — but `/api/ws` auth
      is **cookie-based**, so the session cookie must be plumbed into the probe. A thin
      CLI leg logs in and drives `runs list --json` → approve a plan against the
      running api, catching DTO/route drift the web-driven phases miss; it runs
      headless via `UZI_TOKEN` (docs/cli.md:61), so **spell out the CLI-token mint from
      the harness admin session**. Both are separate HTTP clients on the real wire —
      no lower layer exercises them.
- [x] **M3 — false-green hardening** — DONE `f4517dba`; reviewer APPROVE + auditor
      PASS; default e2e green (positive controls + `retry_read` banked). The `<16` grep
      change is **forgejo-lane-only** (not in the default gate), static-verified against
      `forgejo.go:179/182` by both validators — bank it with
      `UZI_E2E_FORGE=forgejo ./e2e/run-e2e.sh` before merge. Original scope:
      give the container-log scan (`:1176-1179`) and
      the `/data` scan (`:1182-1186`) a **positive control** — prove the corpus is
      non-empty before asserting the secret's absence (mirror the M6 `/proc` control
      at `:1312-1313` and the Decision-3 control at `:2943-2944`, and the CI
      gate-on-the-gate in `test:api-store-it`). Wrap point-in-time **reads** in a light
      retry so one transient curl/exec blip does not abort a ~20-min run (the
      `fake_has_label` hardening at `:388-392` did this for one call site only). **The
      retry must be a NEW read-only wrapper, not a change to the shared helpers**
      (fable review): `db_psql` (`:1924`) is also used for WRITES (`:2087`, `:2191`,
      `:2843`), and a blanket retry could double-execute a write after an ambiguous
      failure. Tighten the weak Forgejo `<16` secondary grep (`:689`, the `version`
      alternative matches almost anything).
- [x] **M4 — drop / downgrade verified-redundant phases** — DONE `aad3c201`; reviewer APPROVE +
      auditor PASS; full-suite green (173 PASS / 0 FAIL, matching the predicted `182 → 173` exactly).
      Delta independently re-derived by both validators: **11 removed − 2 added = −9**, every
      vanished leg named. The two new tests that are the *sole* justification for a dropped
      assertion — `TestGetRunReviewPerReviewTriage`, `TestRunToDTOStopKind` — were each mutated in
      two directions and each was **the only test in the whole `api` suite** to catch its mutation.
      Two named residuals (the `rejudge` end-to-end path, the `#16` GetSkill seam) are recorded in
      the RESUME block.
      ⚠️ **M4's "every drop is verified redundant against a cheaper layer" was later FALSIFIED for
      one leg — see M9c.** The `#16` private-skill drop had **two** unjoined halves, not one. M9b
      closed the `ErrNoRows → 404` half; the authz half (the handler passing the caller's *real*
      `IsAdmin`/`ViewerID` into the visibility query) had **no coverage anywhere in the tree**, and
      mutating it to a full authorization bypass left `go test ./...` **green**. The dropped e2e leg
      was the only thing that had ever caught it. Closed by M9c (`13508b36`). This does not retract
      M4's other drops — each was re-derived and stands — but it does mean the milestone's blanket
      redundancy claim held for 10 of 11 removed legs, not 11 of 11, and it was found only because a
      validator went looking past the question it was asked. Original scope:
      > ⚠️ **The `run-e2e.sh` line numbers below are as-of-drafting and are now STALE** —
      > the file grew ~400 lines across M1/M2/M3/M5/M8 and will shift again as M4 drops
      > phases. **Navigate by CONTENT** (the phase's `say "…"` header), never by these
      > refs. Concretely dangerous example: `:2182` was cited for #94 triage, but at the
      > post-M8 tip `:2182` is the PRD #6 cross-kind **409 race** line — an
      > `apipost_code` site that must NOT be touched. Verified stale as of 2026-07-20:
      > #16 `:870`→`:941-966` (leg 4 = `:966`), #46 Phase B `:2085`→`:2445`,
      > #53 `:2832`→~`:3284-3331`, #33 `:1392` and #40 `:1023` likewise moved.

      > ⚠️ **CITATION CORRECTIONS (2026-07-20) — several drop justifications below are
      > WRONG or INCOMPLETE. Verify against the code, not against this list.** Found by
      > applying a reverse audit (enumerate what each phase *actually* asserts, then check
      > each leg has a home) rather than only checking that the cited tests cover the
      > listed properties. The latter passes even when the listed properties don't
      > *exhaust* the phase — which is exactly how a coverage-removing change leaks.
      > - **#94**: the cited tests do NOT cover the per-review `.review.triage.false_positives`
      >   counter (`reviewToDTO` builds its own triage rows; `TestJudgeStatsAggregateLadder`
      >   covers only the GLOBAL strip). Closed by a new `TestGetRunReviewPerReviewTriage`.
      > - **#16 leg 3** (non-admin `PUT /api/agent-templates/{id}/skills`→403): `TestAllocatableRules`
      >   tests *which skills may be allocated*, NOT the admin gate (`skill_allocations.go:105-107`).
      >   `SetTemplateSkills` has **no test at all**. This e2e leg is RESTORED, not dropped —
      >   so #16 keeps **two** legs (repos-PATCH + template-skills-403).
      > - **#33**: the cited IT covers the SQL stamping but NOT that `runToDTO`
      >   (`workers.go:166`) surfaces `stop_kind` on `GET /api/runs/{id}`. Closed by a new
      >   `TestRunToDTOStopKind`.
      > - **#16 leg 1** (other user's private skill→404): covered, but NOT by the cited
      >   helper — `GetSkill` never calls `authorizeSkillWrite`; the real home is
      >   `TestSkillsVisibilityLiveDB` (SQL visibility filter → ErrNoRows → 404).
      >
      > Net effect: the drop is **182 → 173** (−9), not the −10 first scoped.

      With each redundancy
      confirmed against the named lower-layer test —
      - PRD #94 triage (`:2182`) → **drop** (covered by
        `store/recommendation_dispositions_integration_test.go` +
        `handler/review_disposition_test.go`, incl. the store-double-panic proof of
        no-spend/no-forge).
      - PRD #53 rate-limits (`:2832`) → **one-liner** (union covered by
        `handler/ratelimits_test.go`).
      - PRD #46 Phase B re-judge (`:2085`) → **drop, keep Phase A** (fallback covered
        by `agent/test/judge-runner.test.ts` + `workersvc/judge_m3_test.go`; Phase A
        committed-terminal→judge→notification is genuinely full-wire). **BLOCKER
        (fable review): dropping Phase B breaks the downstream PRD #68 phase.** #68 at
        `:2104` reads the `install_worker_tool/jq` rec (`F_REC`) that exists ONLY
        because Phase B planted `bash: jq: command not found` (`:2087`) and re-judged;
        remove Phase B and `:2105` fails, taking #68 with it. The drop must first
        rework #68's setup — seed the rec row directly (the way #94 does at `:2191`),
        or plant the signal before Phase A's judge. Do not drop Phase B in isolation.
      - PRD #33 (`:1392`) → keep verbatim-reason-through-worker; **drop the stop_kind
        SQL half** (`store/stop_kind_integration_test.go`).
      - PRD #40 (`:1023`) → keep "worker frame parsed & folded"; **drop the rollup
        math** (`store/run_usage_integration_test.go`).
      - PRD #16 skills authz (`:870`) → **collapse to router glue BUT keep the
        non-owner repo-PATCH leg** (fable review): `handler/skills_test.go` covers the
        403-vs-404 skills/template matrix, but the phase's 4th leg — non-owner
        `PATCH /api/repos/{id}` → 404 (`:889-892`) — is a *repos*-handler property
        with **no handler test anywhere** (no `repos_test.go`). Keep that leg, or add
        the handler test before collapsing.

      **Do NOT drop** (look redundant, are not — different topology / live kernel,
      full-wire-only): #58 XFF (`:1263`, empty-`TRUSTED_PROXIES` compose vs unit's
      k8s CIDR), #51 uid boundary (`:1299`), secret-hygiene (`:1175`), #83 Decision-3
      (`:2879`). This guard list ships in the phase comments so a future reader does
      not "finish the job."
- [x] **M5 — mechanical speedups (~30–40s, low/zero risk)** — DONE `14b1615b`;
      reviewer APPROVE + auditor PASS; default e2e green (overlay `start_interval` merge
      preserves base healthchecks; tightened negative windows; reconcile-driven `:1901`
      left; no `wait_*` ceiling touched). Original scope: add `start_interval: 1s`
      to the db + api + forge-fake healthchecks in the **overlay only** (base
      `docker-compose.yml` defaults untouched). **Corrected mechanism (fable review):**
      this helps ONLY the two full-stack `up -d --wait db api web forge-fake` boots
      (`:592`, `:956`, ~8–12s each via the db→api chain) plus marginally the
      `--wait agent/dind` calls — NOT the four api recreates (`:753,:1430,:2002,:2685`),
      which use `up -d --no-deps --force-recreate` **without** `--wait` and then a
      0.3s `wait_http` curl-poll, so healthcheck cadence is irrelevant to them.
      Realistic ≈ 30–40s total, not the ~24s-from-every-recreate the first draft
      claimed. (`start_interval` needs Docker Engine ≥25 — note it in `README.md`;
      the pinned Docker 29 / Compose 5.1 support it.) Tighten negative-window sleeps
      (Decision 5): `:1441,:1473` 6→4s, `:1690` 5→4s, vault `:1962` 3→1.5s. **NOT
      `:1676`** (reconcile-driven — floor at 2 reconcile periods, Decision 5). **The
      `assert_no_run_for_issue` "default `:434` 6→4s" is a no-op** (fable review): the
      `:434` default is never used — every call site passes an explicit arg (`:1663`=6,
      `:1700`=6, `:1678`=0); change the explicit `6`s at `:1663`/`:1700`, leave `:1678`.
- [~] **M6 — DEFERRED to [issue #100](https://gitlab.example.com/vtmocanu/uzi/-/issues/100)**
      (2026-07-20, user's direction, so PRD #97 can land). Not dropped — it is the single
      largest remaining wall-clock win, but it is also the riskiest milestone (it
      parallelises a phase and moves three timing knobs in a suite this PRD has just
      proven timing-sensitive), and it is **unmeasurable until M9 ships**: its entire
      value is a timing claim and the suite had no instrument. M9's central `wait_*`
      margin instrumentation also answers open question 3 outright and informs 1.
      Corrected estimate carried to #100: **~105s net**, not ~120s — the worker sits at
      cap 2 entering #47 (asserted `cap==2`), so cap≥3 costs an extra agent recreate
      + register wait (~15s). Original scope retained below for reference: parallelize
      the PRD #47 health legs a/b/c on a `WORKER_MAX_CONCURRENT_RUNS≥3` worker
      (`:2735`; per-run `health`/`health_notified_at`, same assertions run
      concurrently → wall clock ~max instead of ~sum, ~120s) — **gated on** the
      author confirming the three sentinels don't interfere and the nudge-once window
      still isolates each run. **Unstated cost (fable review): the worker is at cap 2
      entering #47** (set `:2590`, never reverted; `:2648` asserts cap==2), so cap≥3
      needs an **extra agent recreate + register wait (~15s)** between #42 and #47 —
      net saving ≈ ~105s, not the full ~120s. Wire the `SWEEP_INTERVAL` knob
      (Decision 6; ~2s in the overlay → shrinks stall/loop latency and the `sleep 18`
      at `:2781`). Move `E2E_WORKER_HEARTBEAT_STALE=15s` into the initial env-file and
      **delete the dedicated api recreate** at `:2685` (~20s) — **gated on** no earlier
      phase leaning on the 45s default; scrutinize the mid-suite worker restart the
      gapless-seq assertion spans (`:1018-1021`), where >15s worker downtime would
      trip a sweeper requeue the 45s default currently absorbs. Halve the chat idle
      window (~10s) — **gated on** no gap in the red-team→propose→confirm→dismiss
      sequence (`:2329-2422`) exceeding 10s; **change `WORKER_CHAT_IDLE_TIMEOUT` in
      BOTH overlay places** (api `:104`, agent `:173`) and keep the api-side
      `CHAT_IDLE_TIMEOUT: 90s` ordering (`:97-105`).
- [ ] **M7 — (optional) harness structural refactor**: extract the helper library
      (`:190-467` + the `/_e2e` helpers) into `e2e/lib.sh`, and split the ~30 phases
      into `e2e/phases/NN-<name>.sh` sourced **in order** by a thin driver that owns
      the one shared stack + globals. Makes the phase registry explicit and pulls the
      implicit inter-phase contract (poller sped to 2s at `:1429`, judge toggled off
      at `:2171`, cap bumped, `IID`/`RUN`/`MR_IID` reused 400 lines later) into one
      documented place. Mechanical move, no logic change. Keep the single-stack,
      serial, same-process model — do **not** parallelize (one worker/DB/forge makes
      that impossible). Deferrable; de-risks M4/M6.
- [x] **M8 — harden the `apipost` board-reconcile race** — DONE `7d11e6e9`; reviewer
      APPROVE + auditor PASS. **16** run-create sites routed through `create_run`
      (more complete than the ~13 originally scoped — it also caught the `hrun` helper,
      unnamed here, covering 7 further call sites; same class, not scope creep).
      `create_run`'s body is **byte-identical** across the commit (auditor extracted and
      diffed it at `7d11e6e9^` vs `7d11e6e9`: 17 lines, unchanged), so the retried-condition
      set provably did not widen. Skips preserved: `:2182` (409 cross-kind) and `:2230`
      (422 prdless) stay on `apipost_code`. Original scope below:
      M2 validation on a fast host surfaced pre-existing latent fragility. **M2 is NOT
      blocked on this** — M2's own 8 assertions passed every run (5/5), the M2-stashed
      baseline was green, and run5 (relocated M2) was FULLY green (178 PASS / 0 FAIL),
      so M2 shipped green. The intermittent downstream aborts across earlier runs were
      pre-existing **environmental transients of two DIFFERENT classes** (coder's
      Assessment 1): (a) a genuine board-reconcile `404` on run-create when the 2s
      poller lags (PRD #16, run2) — the exact case `create_run` (`:246`) already retries;
      (b) an api-routing transient `404` on a *fixed* route under host contention
      (PRD #32 vault, run4 — `VaultLock` cannot 404 by design). M8 hardens class (a)
      ONLY: route the ~13 plain-`apipost` run-create sites (~10 exposed after the
      poller-speedup, plus the `skill_run` helper) through `create_run`'s retry (or an
      equivalent read-only-safe wrapper — NOT `db_psql`, and NOT the intentional
      non-200 gates like the PRDLESS 422). Mostly a mechanical sweep (~1-2h + one
      validation run); low blast radius (`create_run` retries only the one documented
      404, fails hard otherwise). It does **not** fix class (b) (a write-path retry is
      deliberately avoided per M3/Decision 3 — double-execute risk); a fuller
      infra-transient fix is a separate, harder question. In the spirit of M3's
      false-green work.

      **Gate (corrected — the original wording overclaimed).** The claim M8 makes is
      **structural, not statistical**: when the race fires it is either *absorbed* (exact-body
      404, ≤6 attempts) or *surfaced* with a real HTTP status + response body + the site
      name. The information-free `curl: (22)` is gone from every run-create path — that is
      a diagnostics upgrade, so net coverage goes UP. The full green run (182 PASS / 0 FAIL,
      incl. PRD #41) proves **no regression from the sweep** (behaviour-preserving across
      182 assertions); it does **not** prove the flake is gone, since one green run is
      equally consistent with a low-rate race simply not firing that time. Do not read
      "0 curl-404" as "fixed".

      **M8 cannot eliminate the flake, by design.** It covers class (a) — the run-create
      board-reconcile 404 — only. Class (b), the api-routing transient 404 on a *fixed*
      route (PRD #32 vault; `VaultLock` cannot 404 by design), is deliberately untouched:
      it is not a run-create, and a write-path retry there would reintroduce exactly the
      double-execute risk M3/Decision 3 rejected. **A future abort remains possible and
      may well be class (b).** Stated explicitly so a future reader does not re-derive it.

- [x] **M9 — timing-fragility hardening (added 2026-07-20; BLOCKS M4's gate and M6)** — DONE
      `859a8066`; reviewer APPROVE + auditor PASS; same 173/0 gate. **Shipped THREE changes, not the
      four the commit message claims** — the `#95` vault lock, `sleep 6→8`, and the central
      instrumentation. `wait_card_column` was deliberately left at 10s (see RESUME correction 1).
      All five agreed weakening criteria cleared. Instrumentation covered `wait_eq` + `wait_status`
      only; the eight bespoke loops are completed in **M9b**. Original scope:
      Running this PRD ~10 times exposed that the suite carries several undisclosed
      coin flips. Observed across our runs: **6 timing failures across 4 distinct
      phases, only ONE of which was a real bug** (M2's WS cookie defect). The rest:
      `#16`/`#39` run-create reconcile 404s (M8 fixed that class), `#95` follow_up
      read-after-write race (×2), `#22` PRDLESS-remove propagation timeout, `#32`
      api-routing transient. **Every "182 PASS / 0 FAIL" run was a suite containing
      multiple races that happened not to fire.** That is why M9 comes before M4's
      gate and M6: neither is measurable on a suite whose green is this weak a signal.
      Scope:
      - **`#95` read-after-write race** → the agreed **vault-lock** design: PRD #32
        already proves a locked owner's run stays queued/never-claimed, so lock →
        create → submit → assert Queued (now a **stable** state) → unlock → assert
        Queued→Delivered as today. Touches only the one racing read (`:1455-1459`);
        everything downstream already polls-until-reached. Ensure the vault ends
        unlocked and the window doesn't collide with the dedicated vault phase.
        **Blocking weakening criteria** (agreed before the diff existed): no accepting
        `consumed_at != null` in any form; no dropping Queued for Delivered-only; no
        retry that tolerates either outcome (waiting for a state to *stabilise* is
        fine, accepting the opposite is not); no substituting the write-response for
        `GET /inputs` (distinct surfaces — assert both or neither); and no merely
        making the read earlier, which shortens the window without closing it.
      - **`#22` PRDLESS-remove → DIAGNOSE. Do NOT widen the window.**
        ⚠️ **Correction (2026-07-20):** an earlier draft of this milestone called `#22`
        a too-tight window — "`wait_eq no 20` ≈ 6s, ~1.5 reconcile periods". **That was
        wrong.** `wait_eq`'s second argument is **SECONDS, not tries**
        (`local deadline=$((SECONDS + timeout))`), so it waited a **full 20s across ~66
        polls** = **5 reconcile periods**, comfortably ABOVE the Decision-5 floor.
        The label was still on the fake forge after all 66 reads, on a **forge-first**
        write whose response card had already come back *without* PRDLESS. So this is
        **a state that never converged, not a window that closed early** — and it fits
        neither M8 class (a) (run-create reconcile 404) nor (b) (api-routing transient).
        **Raising the timeout here would convert a real unexplained defect into a slower
        green** — precisely the false-green pattern this PRD exists to kill. M9's action
        is therefore to **diagnose** (run with `KEEP_STACK=1` so a recurrence can be
        interrogated against forge-fake + api logs instead of being torn down), and to
        fix only once the mechanism is known. If it proves environmental, say so with
        evidence; do not reclassify it as a flake to make the suite green.
        **MECHANISM NARROWED (2026-07-20) — and it kills the timeout theory outright.**
        `SetIssuePrdless` (`board.go:711-717`) is **strictly forge-first and
        synchronous**: on forge failure it returns **502 and leaves the cache
        untouched**, and the card it returns is built from the post-write issue. So a
        **200 whose card lacks PRDLESS proves the forge write already returned
        success**. `wait_eq no 20` is therefore **not waiting on a poll at all** — it
        waits for something that by construction happened *before the HTTP response
        returned*. **The timeout value is irrelevant: 20s, 60s or 600s fail
        identically.** Not a too-tight window, not the fable class, not any M8 class.
        Three remaining candidates to chase with `KEEP_STACK=1`: (1) forge-fake
        accepted the removal but didn't persist it; (2) something re-added the label;
        (3) `fake_has_label`/`fake_state` read stale or wrong-issue state. Note the
        *apply* leg passed on the **same issue** seconds earlier, so forge-fake's
        add path works — which argues for (1) or (2).
      - **FullSync-eviction `sleep 6`** → **8s**, the 2-reconcile-period floor the fable
        review identified. M5 correctly declined to *lower* it; M9 raises it to the floor.
      **FULL-SUITE INVENTORY (2026-07-20) — the fragility is far narrower than feared,
      and it re-scopes M9.** Cadence baseline: the overlay's poller is **24h (effectively
      OFF)** and is only sped to 2s by the api recreate at `:1860`, so **nothing before
      `:1860` can be racing the forge poller at all**. After it: poll 2s, reconcile
      period 4s, worker claim/steer poll 500ms. Floors: poll-driven negative 4s,
      reconcile-driven negative 8s. Against that, the whole suite contains:
      - **Exactly ONE genuine point-in-time race**: `:1487` (#95 `consumed_at == null`).
        `:984` *looks* identical but is **safe by construction** — it runs before any
        worker exists (join token minted at `:993`); add an inline note so a future
        hardening pass doesn't "fix" a non-race. `:2394` is the same shape but asserts a
        **stable** state — and it is **already the vault-lock pattern, working today** in
        the PRD #32 phase. So the #95 fix is not a new mechanism; it is a pattern the
        suite already relies on.
      - **Exactly ONE sub-floor window**: `:2108 sleep 6` (FullSync-eviction) needs 8s.
      - **At floor, verified not assumed** (no action): `:1873`, `:1905`, `:2122`
        `sleep 4` = 2 poll ticks; `:2095`, `:2132` `assert_no_run_for_issue 4`;
        `:2394 sleep 1.5` = 3 worker cycles. `:2110`'s zero-width negative sits directly
        behind `:2108`, so raising that to 8s covers it.
      - **One marginal timeout**: `:1901 wait_card_column … 10` is reconcile-driven at
        2.5 periods — above the 8s floor but the tightest in the suite (every other
        `wait_*` is ≥20s ≈ ≥5 periods). **Do NOT pre-emptively raise it. Instrument it
        and decide from the measurement** — guessing at timeouts is what produced the
        #22 error.
      - **Margin diagnostics — instrument the `wait_*` HELPERS CENTRALLY, not per site.**
        There are ~40 `wait_*` calls; per-site diagnostics don't scale. The helpers
        already loop until the condition holds, so recording elapsed-vs-timeout and
        emitting it costs nothing and converts **every** timing-sensitive assertion in
        the suite into a measurement in one change. This is **M9's core deliverable**;
        per-site diagnostics (like #95's) are the exception. Had this existed, it would
        have surfaced `:1901`'s 2.5-period margin long ago instead of us finding it by
        reading. Timing-sensitive assertions must emit
        their margin (e.g. `#95`'s submit→consume delta), so a run yields a
        **measurement** rather than a pass/fail coin flip. A bare PASS hides a near-miss.
      **Gate (deliberately NOT "one green run")**: a full `./e2e/run-e2e.sh` green **AND**
      the margin diagnostics showing comfortable headroom on the previously-fragile
      assertions. This is the M8 gate lesson applied up front: one green run is equally
      consistent with a race not firing, so the gate must measure the margin, not observe
      an outcome.

- [x] **M9b — complete the margin instrumentation + two validator findings** — DONE `67ec761c`
      (harness) + `ef286669` (GetSkill test) + `9100e7f8` (comment-only); reviewer APPROVE;
      full-suite green at **173 PASS / 0 FAIL, unchanged**, with **113 instrumented waits** (was 92).
      `9100e7f8` landed **after** the gate and is provably inert, so the gate still describes this
      tree's executable content:
      ```sh
      diff <(git show 9100e7f8~1:e2e/run-e2e.sh | shfmt -mn) \
           <(git show 9100e7f8:e2e/run-e2e.sh   | shfmt -mn)   # empty
      ```
      **Use `shfmt -mn` (minify), NOT a grep-strip of `^\s*#` lines.** The lead first "proved" this
      with the grep-strip form and recorded it as sound; the reviewer showed by measurement that it
      is not, *for this file specifically*: `run-e2e.sh` has **7 heredocs, two of which contain `#`
      lines that are program text written to disk** — `:611` (`#!/bin/sh`, fake-devbox) and
      `:677-678` (`#!/bin/sh` plus a comment in the **pre-receive hook**, which git then executes);
      **`:731` is an unquoted `<<EOF`** whose body parameter-expands at write time, so a comment
      containing `$…`/backticks/`\` would be expanded into the env file (not abstract here — this
      change's subject text is `local … deadline=$((SECONDS + timeout))`); and **250 lines end in
      `\`**, after which a `#` line is an *argument to the previous command*, not a comment. A
      classifier keyed on a leading `#` misreads all three classes. On this file today the
      grep-strip happens to return the right answer — luck, not proof.
      `shfmt -mn` strips comments and normalises formatting while preserving heredoc bodies and
      string literals verbatim, so an empty diff means the **executable token stream is identical**.
      Validated in both directions rather than trusted: it stays silent on a genuine comment edit,
      and it fires on a heredoc-`#` edit, a `#` after a continuation, a whitespace change inside a
      `pass` string, and a `wait_card_column 10 → 20` ceiling change. A positive control confirms it
      is not vacuous (`859a8066` → `67ec761c` differs).
      **Neither proof discharges whether the comment is TRUE or attached to the RIGHT helper** — a
      comment moved onto the wrong function is inert to bash and invisible to any diff. That still
      requires reading the text.
      **`5bbeff21` (comment-only) corrects `9100e7f8`, whose own deliverable failed its own check.**
      `9100e7f8` set out to make the uninstrumented-loop claim "checkable rather than asserted" and
      shipped **six line references that were all wrong** — it added 11 lines above everything it
      cited, invalidating its references in the act of writing them (+11 on five, +18 on the sixth).
      One was worse than drift: the `assert_no_run_for_issue` citation resolved to
      `wait_run_for_issue()`, an **instrumented** helper, inside a bullet claiming that line was
      uninstrumented — a reader who checked it would have concluded the exact opposite of the truth.
      `5bbeff21` replaces every line number with a **content anchor** (`rv_deadline`, `cap_deadline`,
      `det_end`, `if_end`, `assert_no_run_for_issue()`), which cannot drift, and sharpens the `local`
      note from "does not sequence" to the actual consequence: **`bash: timeout: unbound variable`,
      exit 1, on the very first wait**. Inertness proven with `shfmt -mn` plus a positive control.
      **This is the third time in this PRD that stale `run-e2e.sh` line numbers cost real time** (the
      PRD's own M4 refs went stale by ~400 lines; `:2182` was cited for a phase that had become an
      unrelated 409-race line). **Do not cite line numbers into this file. Ever. Use content
      anchors.**
      ⚠️ **And when you VERIFY anchors, strip the citing block first.** Grepping an anchor against the
      whole file finds it **inside the comment block that cites it**, so every anchor "resolves"
      trivially and the check passes while proving nothing. The reviewer stripped the block and
      counted against code only: all six anchors unique-in-code, each resolving to the loop its
      bullet describes, every claimed ceiling (20/60/40/45/90s) matching. This is a
      **self-verifying-artifact trap** — citation and cited living in the same file — and it is
      precisely the shape that would let a broken version pass a lazy re-check. It also checked the
      loops **behaviourally**, not just by identity: the four named ones genuinely `break` on success
      with the verdict in a later assertion, and `if_end` genuinely has **no** `break`, running its
      full 90s with the `pass` after the loop. Identity plus behaviour is the difference between "the
      anchor points somewhere" and "the bullet is true".
      Non-blocking sharpening (recorded rather than committed): without `set -u` the collapsed form
      yields `SECONDS + 0` — an **already-expired deadline**, so the loop body never runs and the
      wait falls straight through to its own `fail`. "Instant spurious timeout" is sharper than
      "garbage ceiling".
      **`#68`'s single-rec assertion is a GUARD, not a covered path** (volunteered by the coder,
      unprompted). The duplicate cannot occur under any state the suite can reach, so the `>1` arm
      never executes. It is a tripwire. Recording it as "tested" would be exactly the false-green
      this PRD exists to kill; forcing it would mean mutating the phase's own fixture mid-run, which
      would make the fixture lie about the system's real shape.
      **The measurement is the deliverable, and it came back negative — which is a result, not a
      non-result.** The chat-phase waits at the 20s ceiling, named up front as the likeliest-tight
      class, measured **19-20s headroom** — under 1s of actual wait against a 20s ceiling, roughly
      20× the observed need. **Nothing newly instrumented is tight.** Correction to the framing
      everyone (including the reviewer who raised it) used: it is **five** sites at 20s, not three —
      `wait_msg_kind` at `:3139`/`:3140` takes the *implicit* 20s default and sat outside the "three
      chat waits" description. All five are clean. **Nobody should re-raise this class without new
      evidence.**
      `card #2 column → Later` waited **0s of its 10s ceiling**, which resolves — in the negative —
      the explicit falsifiable condition M9's own in-file comment set ("if the data shows it running
      near the wire, raise it then, with evidence"). The refusal to raise it is now evidenced, not
      merely defensible.
      ⚠️ **Do not read the top of the margin table as a fragility ranking.** `report_margins` sorts
      by **absolute** headroom, so the wait with the **smallest ceiling** permanently leads the list
      even when it is the fastest-settling wait in the file — which is exactly what happened here:
      `card #2 column` waited 0s and still tops the table. Reading "top of the list" as "most
      fragile" is how someone ends up raising a timeout on the suite's *best-behaved* wait. By a
      `waited/ceiling` ratio it is joint-best, not worst. A ratio column would rank real fragility;
      worth a future pass. Also note `0s` means "under 1s", not "instant" — `record_margin` is
      whole-second by design.
      **Coverage claim verified this time**: 10 of 10 helper loops instrumented, 5 of 5 inline
      non-helper loops accounted for. The reviewer enumerated them rather than trusting the list.
      **The `:3463` exclusion is the one that matters and is exactly right**: that loop is inverted —
      it deliberately runs its full 90s and fails if `health` ever reads `stalled` — so instrumenting
      it would emit `waited 90s of 90s (headroom 0s)` on **every green run**, permanently the
      tightest row in the report and entirely spurious. A mechanical "instrument everything" pass
      would have got that wrong. The other four exclusions are a *choice* (they `break` on success
      and could take a record), not an impossibility.
      Original scope: M9's core deliverable was to convert **every** timing-sensitive
      assertion into a measurement; it reached `wait_eq` (7 wrappers) + `wait_status` and stopped.
      Eight bespoke loops (~19 call sites) stayed blind — `wait_http`, `wait_run_for_issue`,
      `wait_autopilot_done`, `wait_notes`, `wait_review`, `wait_msg_kind`, `wait_msg_text`,
      `wait_tool_result` — including the three **20s chat-phase** waits, the tightest ceilings in
      the suite. Scope: (1) extend `record_margin` to those eight, same one-line pattern, success
      path only, ceiling bound from a hoisted local so the reported ceiling cannot drift from the
      enforced one; (2) assert `#68`'s seeded-rec read-back yields **exactly one** id (no UNIQUE
      backs it; both validators found this independently); (3) close the `#16` GetSkill seam with a
      mutation-proven handler test joining `ErrNoRows → 404`.
      **Gate**: full `./e2e/run-e2e.sh` — mandatory, since (1) and (2) change `run-e2e.sh` and
      invalidate the validated-tree claim behind the 173/0 run. Expect **173/0 unchanged**; a moved
      count is a stop-and-report. **Do not raise any ceiling**: if the new margin data argues for
      one, report it — M9's standing rule is instrument-then-decide, and guessing at timeouts
      produced M9's own worst error.

- [x] **M9c — close the AUTHZ half of the `#16` seam** — DONE `13508b36`; reviewer APPROVE +
      auditor PASS. Six parameter-construction mutations go from **10-of-11 surviving** to all six
      authz probes KILLED, each by the semantically correct subtest, verified under
      `go test ./...` across the whole module. The load-bearing proofs that the fix does not merely
      *relocate* the blindness: **A4/A5 pass valid, correctly-typed, WRONG-VALUE uuids** and **A6
      swaps `ID`/`ViewerID`** (both args present, both typed, both non-nil) — all still RED, so the
      assertions fail *because the wrong value was passed*, not because something was recorded. A3
      (`uuid.Nil`) alone would NOT have proved this: a bare `.Valid` check kills Nil while passing
      A4. The original `ErrNoRows → 404` proof was re-run against the reworked fake and survives, so
      the refactor did not defang the test it was built for.
      **This is a correction to M4, not an enhancement.** The `#16` drop's compensating
      test (`ef286669`) closed one half of the seam; the auditor's independent pass found there were
      **two**, and that the second was a live authorization bypass with **zero coverage anywhere in
      the tree**:
      - `skills.go:195` `IsAdmin: actor.IsAdmin` → `true` ⇒ every caller reads as admin, so any
        authenticated non-admin could read **any other user's private skill**. `go test ./...`:
        **green, zero failures.**
      - `skills.go:196` `ViewerID: pgUUID(actor.ID)` → `pgUUID(uuid.Nil)` ⇒ same, **green**.
      **Why both existing tests were blind, which is the generalisable part**: `fakeSkillDB.QueryRow`
      **discarded its `...any`**, branching only on whether `f.skill` was nil — structurally
      incapable of observing which params the handler built. And `TestSkillsVisibilityLiveDB` passes
      `IsAdmin`/`ViewerID` as **explicit literals**, proving the SQL honours params it is *handed*
      while saying nothing about which params `GetSkill` *hands it*. Two real tests, both blind to
      the same half. **The dropped e2e leg was the only thing that had ever joined it** — with a live
      DB and a real non-admin session, `IsAdmin: true` returns 200 instead of 404 and goes red.
      **Consequence for the record: M4's "verified redundant against a cheaper layer" was FALSE for
      this leg**, and the PRD's scope note ("no e2e assertion is weakened") would have shipped false.
      That is the exact failure this PRD exists to prevent, which is why it closed here rather than
      as a follow-up issue.
      Fix: `fakeSkillDB` captures raw positional args; `TestGetSkillPassesCallerIdentity` asserts the
      query received the caller's **real** `IsAdmin` and `ViewerID`; callers set `IsAdmin`
      deliberately via `nonAdminCaller()`/`adminCaller()` rather than relying on a zero value (an
      authz assertion resting on a zero value is one struct-literal edit from passing for the wrong
      reason).
      **The two hardenings are COMPLEMENTARY, and neither alone covers the non-admin side** — proven
      by counterfactual, not argued. The auditor kept a type mismatch in place and rewrote
      `skillQueryArgs` back to the rejected comma-ok form: `non-admin_caller` went **PASS —
      vacuously**, observing nothing, because the failed assertion yields zero-value `false` which
      satisfies "is_admin must be false". `admin_caller` was the only subtest that still failed,
      because it demands `is_admin == true`, which a zero value can never satisfy. So the explicit
      `t.Fatalf` guard is what makes the non-admin subtest non-vacuous, and the **mirror** assertion
      is the sole survivor if that guard is ever weakened. **A future reader who simplifies either
      one silently creates a vacuous test.** Recorded in the test file itself (`e0a9bd41`, sited on
      `skillQueryArgs` — what a simplifier would actually be editing), not only here, and
      independently reproduced by the coder on a second construction before being written down.
      **Why it is invisible rather than merely weak, in the coder's words: a failed comma-ok yields
      `false`, and `false` is *the value the non-admin assertion is looking for*.** The subtest
      passes **because** the capture failed — assertion and failure mode agree on the same value.
      Sharper than "vacuous", and it names what to look for elsewhere: **any assertion whose expected
      value coincides with the default its own failure mode produces.**
      **The "weaken both" half was MEASURED, not just asserted** (reviewer, at `e0a9bd41`): with the
      comma-ok helper restored *and* `admin_caller` removed, a real `IsAdmin: true` bypass mutation
      ships **GREEN — undetected**. As shipped it is RED, caught by `non-admin_caller`. So the
      complementarity claim holds end to end, not merely at the level of the helper.
      **Known asymmetry, recorded rather than fixed** (reviewer's call, and the lead did not override
      it): the strongest reason to keep `admin_caller` is documented on **`skillQueryArgs`**, not on
      `admin_caller` itself. Someone deleting that subtest as redundant would be reading
      `TestGetSkillPassesCallerIdentity`, whose own comment gives the *mirror-mutation* rationale but
      does not say "this subtest is the backstop that keeps the whole authz assertion honest if the
      helper is ever simplified". **The wiring is one-directional: simplify the helper and you hit
      the warning; delete the subtest and you do not.** A hardening, not a hole — the existing comment
      still gives an independent reason not to delete it. One sentence on the test if anyone touches
      the file again.
      **What this does and does not argue for** (the coder's read, adopted): `admin_caller` was added
      to kill the mirror mutation; its double duty as the sole survivor of a weakened helper is an
      **unplanned** second payoff. That is not an argument for adding assertions speculatively — it
      is an argument for asserting **pass-through rather than a constant**, which happens to be
      robust against a whole class of degradations rather than only the one it was aimed at.
      **Sharper rule (auditor), which PREDICTS the C3 split instead of describing it — use this one:
      an assertion is vacuous-able exactly when its expected value equals its type's zero value.**
      `non-admin_caller` expects `is_admin == false`, and `false` **is** the zero `bool`, so a capture
      that failed and a capture that succeeded produce the identical observation. `admin_caller`
      expects `true`, which no zero value can produce, so it cannot be fooled. The viewer assertions
      expect `actor.ID` — non-zero — and are robust one-sided for the same reason.
      This matters because "always add the mirror" would become a ritual: the two-sided test is **the
      remedy for one specific shape**, not a general practice. The fragile shapes are booleans
      expecting `false`, ints expecting `0`, strings expecting `""`, pointers expecting `nil`; each
      needs either a mirror case or a loud capture guard. **An assertion whose expectation is
      non-zero already has the property for free.**
      *Scope, labelled as the auditor asked: the boolean case was **measured**. The extension to other
      zero values is **reasoning from Go's semantics, not a run.** Recorded as such rather than as a
      measured result.*
      **Two hardenings neither the auditor nor the lead proposed, both from the coder:**
      (1) the **mirror** mutation — hardcoding `IsAdmin: false` — is not a bypass, so it does not
      surface when hunting for one, but it silently strips admins of the cross-scope read the flag
      exists to grant; it would have been *a new uncovered path created by the fix for the old one*.
      Both directions are now asserted, so what is pinned is **pass-through of the caller's
      identity, not either constant**. (2) `skillQueryArgs` type-asserts with explicit `t.Fatalf`
      rather than comma-ok, because `id, _ := args[0].(uuid.UUID)` yields a zero value on mismatch
      and a zero `bool` is `false` — which would satisfy an "is_admin must be false" assertion
      **against a param that was never captured**. A test that passes because nothing was recorded
      is the same failure class the commit exists to fix.

### ⚠️ Follow-up found by M9c — OUT OF SCOPE for this PRD → tracked in [issue #101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101)

**All four follow-ups below are filed as [#101](https://gitlab.example.com/vtmocanu/uzi/-/issues/101)**
(viewer-identity call sites, the remaining `GetSkill` properties, the repo-wide gofmt drift, and
homing the AST comment-inertness checker), together with the methodology rules. They must **not** be
folded into this branch. Detail retained here because it is the evidence, and #101 is the tracker.

Chasing the `#16` seam surfaced a **class**, not a one-off. Recorded here with precise wording,
because the difference matters and is easy to garble:

**These are MISSING REGRESSION GUARDS, not live vulnerabilities.** The shipped code passes
`actor.IsAdmin` and the caller's id correctly at every site below. What is absent is anything that
would *catch a regression* — mutate the pass-through into a bypass and the suite stays green.
**The evidence for that distinction, rather than the assertion of it:** at `GetSkill` the bypass
mutant goes red **only because the shipped code passes `actor.IsAdmin` correctly** — the mutation is
what introduces the bypass, and the pristine tree never had it. Same structure at the other sites.
A reader who skims a table of "authz bypass" rows and misses this would escalate something that is
not on fire.

**Seven handler call sites pass viewer identity into a store query; `IsAdmin → true` left the tests
green at all seven.** ⚠️ **But that "seven" is ONE measurement and six UPPER BOUNDS — do not record
it as a count.** Six of the seven were swept **package-scoped** (`./internal/handler/`) because the
full run timed out. **A package-scoped mutation run errs in the same flattering direction as the
build-failure harness below**: a test in any *other* package that would have caught the mutant never
runs, so a covered mutant is reported as surviving. It manufactures gaps and cannot hide one — so
the true number of unguarded sites is **six or fewer, never more**. Only `ListSkills` was measured
full-scope. Re-run the other six on `./...` before acting on them.
Only the first is now guarded:

| site | state |
|---|---|
| `skills.go:195` `GetSkill` | ✅ **guarded** by `13508b36` |
| `skills.go:165` `ListSkills` | ❌ unguarded — **priority** |
| `agent_templates.go:185`, `:457` | ❌ unguarded |
| `template_allocations.go:224`, `:240` | ❌ unguarded |
| `skill_allocations.go:188` | ❌ unguarded |

**`ListSkills` is the priority and the lead verified it personally** rather than relaying: mutated
`IsAdmin: actor.IsAdmin → true` at `13508b36` in a throwaway detached worktree, confirmed it
**builds**, then ran the **full `go test ./...`** — green, nothing catches it. Restored, tree clean,
worktree removed. It matters more than `GetSkill` did because it is a *list* endpoint: an
`IsAdmin: true` regression there would return **every user's private skills in one request** rather
than one at a time. (The reviewer's sweep of the other six was scoped to `./internal/handler/`
because the full run timed out — a scoped approximation, not an equivalent claim. Re-run on `./...`
before acting on those.)

**Four further `GetSkill` properties remain unguarded** (auditor's end-to-end enumeration; the seam
was never "two halves" — `GetSkill` emits exactly five statuses (401, 400, 404, 500, 200) plus param
construction as the non-status property, so **six**; the fix closes param-construction *completely*,
and these are separate properties):
- the **200 DTO is pinned on only 3 of its 9 fields** — dropping `body`, which *is* the payload for
  a skills API, goes undetected. It looks like it pins the DTO and does not. **Close this one first.**
- **a generic DB error must STAY 500 and must not collapse into 404.** (Corrected 2026-07-20: the
  lead first recorded this as "generic DB error → 404", which describes the mutation rather than the
  property and reads as though 404 were the current behaviour. Verified against the code: the
  `ErrNoRows` branch returns 404, everything else falls through to
  `httpx.Error(w, http.StatusInternalServerError, …)`.) The sharper framing is the reviewer's: a 404
  there is an **existence-oracle lie in the opposite direction** from the one we have been guarding —
  it tells the caller "this does not exist" when the truth is "the database fell over". It is the
  exact mirror of the `404 → 500` mutation, and **neither direction is currently pinned.** Rank just
  under the DTO.
- wrong status on **401 / 400** — both defence-in-depth behind `RequireAuth` and chi routing. Low.

**The generalisable lesson, in the reviewer's own words after it went back and diagnosed its miss:
"I checked *does this test bite*, not *does it bite on the thing we deleted*."** It had approved the
compensating test after three mutations — `404→500`, `200→202`, and a DTO field — **all on the
response side**, every one of them visible to a fake that discards its arguments. The mutations were
true and irrelevant. The dropped e2e leg's property was "**a non-owner non-admin** gets 404", and the
non-owner-ness lives in the *params*, which no response-side mutation can reach. It had
`fakeSkillDB.QueryRow(context.Context, string, ...any)` open — parameters unnamed, arguments
dropped — and did not ask what that made invisible.
**Three independent checks cleared the same seam and all three checked the wrong side of it**: the
lead (who set the standard for the dispatch), the reviewer, and the auditor's own first pass. It was
caught only because one validator re-opened a question it had already answered. That is the argument
for adversarial re-derivation over confirmation, stated by the case rather than asserted.

**And the reproducible part, in the reviewer's words: the widening was *downstream of admitting the
error*, not independent of it.** The six further unguarded sites were found in the same pass that
owned the miss, because owning it forced the question "what could my mutations not see?" — and that
question generalised. A validator that quietly corrects itself produces one fix; one that asks what
the error implies about its method produces a class.

**A second-order instance of the same failure, worth recording because it is subtler:** the "two
halves" framing was itself inherited. The reviewer took it from `ef286669`'s own commit message and
repeated it without decomposing the handler, then diagnosed why it felt complete — *a two-part
framing has no room left over once you fill both parts*. Accepting someone else's **partition of a
problem** is the same error as accepting their claim, one level up, and it is harder to see because
the partition never states itself as a claim.

**The coder's own audit of its four self-inflicted errors this milestone points the same way**: the
nonexistent test name, the six drifted line numbers, the understated `set -u` note — **three of four
were references and characterisations, not logic.** Every one was a claim *about* the code rather
than the code itself, and every one passed its author's reading before a re-test caught it. Mutation
discipline catches the logic; references need the same reflex — **resolve it, or do not write it.**

**There IS a mechanical Go equivalent of `shfmt -mn`, and it wants a home in the repo.** The lead
asserted there was none (so a Go comment-only claim had to be proven by re-mutation instead); the
auditor built one in ~20 lines and disproved it: parse with `go/parser` **without**
`parser.ParseComments`, so comments never enter the AST, then reprint with `go/printer`.
Byte-identical output across two revisions ⇒ the diff touched only comments and formatting. It is
**not** fooled by the case that motivated the doubt: a commented-out assertion is *not* comment-only,
because the statement leaves the AST and the output changes. Proven with both controls rather than
trusted — inserting genuine comments prints IDENTICAL (no false alarm), and commenting out the
`if gotIsAdmin { t.Error(…) }` block prints DIFFERS and names the vanished statement. `13508b36` vs
`e0a9bd41`: byte-identical, 10099 bytes both sides.
**Follow-up: give it a home** (`api/tools/` or a `scripts/` one-liner) as the standing Go counterpart
to the build gate. It lived in a session scratchpad and does not survive. Not landed here — it is a
new change and does not belong on a branch going to MR.
**Two independent methods agreeing is the real proof**: the AST check proves no assertion *text*
changed; the mutations prove the assertions still *bite*. A commit could pass the first and fail the
second, so the pair is what closes it.

**Methodology note worth carrying: a mutation that does not COMPILE is not a surviving mutant.** The
auditor's first probe harness inferred survival from the *absence* of `--- FAIL` lines, so a probe
that failed to build read as "nothing catches this" — which **overstates** coverage gaps, the
flattering direction for an auditor. It caught this itself, withdrew the number, rebuilt the harness
to `go build ./...` first and treat a build failure as INVALID, and re-ran both commits from
scratch. Any future mutation sweep in this repo must gate on the build.

**Dependency graph**: **M1, M3, M5 are independent** (git-auth leg / assertion
hardening / mechanical timing — separate parts of the file and the overlay) and can
run as parallel agents. **M2** (new WS + CLI legs) is independent of those but larger.
**M4** (drops) is safest **after** M7 if M7 is done (phase boundaries become explicit),
else standalone; note M4's #46-Phase-B drop is coupled to reworking #68 first
(Decision 3 / M4). **M6** is independent but gated on author sign-off per leg. **M7**,
if taken, should land **first** as it moves every phase; otherwise skip it and treat
M1–M6 as the deliverable. Suggested: Phase 1 = {M1, M3, M5} parallel; Phase 2 = {M2,
M4}; Phase 3 = M6 (sign-off gated); M7 optional bookend/opener. **Caveat (fable
review): "fully parallel" is optimistic** — M1/M3/M5 all edit the same 3000-line
file; regions are distinct but M3 (helper wrappers) and M5 (scattered sleeps) will
brush shared areas, so expect minor merge friction. Sequential landing, or M7-first
to make the boundaries explicit, is smoother.

## Success Criteria

- Every default `./e2e/run-e2e.sh` run exercises the worker's git `Authorization:
  Basic` push, and a phase attempts a `main` push and asserts it is refused — a
  credential-injection or protected-branch regression turns the run red (M1).
- A live `/api/ws` subscription receives frames during a real run, and a `uzi` CLI
  leg drives the running api — DTO/route drift in either consumer fails the run (M2).
- The two secret-hygiene scans fail if their corpus is empty (positive control), and
  a single transient read no longer aborts the run (M3).
- The dropped phases are gone/collapsed with **no** net loss of asserted behaviour —
  each property still proven by its cited lower-layer test; the do-NOT-drop guard
  list is in the phase comments (M4).
- Wall-clock drops by ~30–40s from the mechanical changes alone with **zero** assertion
  weakened, and by a further ~110–135s net if the sign-off-gated structural changes land
  (M5, M6 — the health-phase win is offset by a ~15s cap≥3 recreate). No `wait_*`
  timeout ceiling was lowered.
- (If M7) the harness is a helper lib + an ordered phase registry driven by a thin
  driver; the inter-phase contract is documented in one place; the run is behaviour-
  identical to before.

## Risks

- **Structural speedups can introduce flake if the isolation assumptions are wrong.**
  Parallelizing the health legs and removing the heartbeat recreate rest on per-run
  state isolation and the 45s stale default. Mitigation: each is behind an explicit
  author sign-off gate (Decision 7); land the mechanical wins first and treat the
  structural ones as a separate, revertable step.
- **Dropping phases risks a future reader "finishing the job" and cutting real
  coverage.** The do-NOT-drop guard list (#58 / #51 / secret-hygiene / #83) ships in
  the phase comments precisely because those four look redundant but read the live
  kernel/container and are full-wire-only.
- **The `main`-push backstop is only as real as the fake's ref filter.** A pre-receive
  hook on `forge-fake` that rejects `refs/heads/main` is load-bearing; without it the
  new assertion is vacuous (the current fake would accept the push). Test the hook
  itself (push main → refused; push agent branch → accepted) as part of M1.
- **CI posture is deliberately unchanged.** `run-e2e.sh` stays local-only (needs
  docker-compose on the runner); this PRD does not promote it to CI. The
  silent-rot gap (nothing signals it still passes on `main`) is acknowledged and left
  as-is — a protected-ref-only periodic run is a possible follow-up, out of scope here.

## Out of Scope

- Promoting `run-e2e.sh` (or any subset) into CI. The local-only split is correct;
  `test:api-store-it` and `e2e:kind-smoke` already cover what can run on the runner.
- The GitLab-lane single-pipeline-per-ref fake limitation (`forge-fake.mjs:555`,
  D10a already deferred) and REST auth/scope enforcement on the fake — noted by the
  review, but larger fidelity work than this hardening pass.
- Any product behaviour change beyond the one-line `SWEEP_INTERVAL` knob (M6).

## Open Questions (for author sign-off before M6)

- Do the three PRD #47 health sentinels interfere when run concurrently on a cap-3
  worker, and does the nudge-once window still isolate each run?
- Does any phase between `:574` and `:2685` rely on the 45s heartbeat-stale default
  (i.e. is it safe to set 15s from boot)?
- Does any gap in the chat sequence (`:2329-2422`) exceed 10s (i.e. is halving the
  idle window safe)?
