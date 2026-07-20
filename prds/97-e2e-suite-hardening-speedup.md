# PRD #97: e2e suite hardening & speedup

**GitLab Issue**: [#97](https://gitlab.example.com/vtmocanu/uzi/-/issues/97)
**Status**: In progress — 7 of 9 milestones done (M1/M2/M3/M5/M8 fully closed; M4+M9 committed and gate-passed, review/audit owed). M6 deferred to #100; M7 optional. See RESUME HERE below.
**Priority**: Medium
**Origin**: Three-agent read-only review of the e2e suite (2026-07-20) — coverage (add/drop), speed, and harness-structure passes. Two independent agents flagged the git Basic-auth default as the single highest-value gap.
**Review**: fable adversarial pass folded in (2026-07-20). Load-bearing corrections: M1's "flip the happy-path leg to smart-HTTP" is **not** viable (forge-fake routes every repo path to one bare, breaking the PRD #42 two-repo phase) — only the second-push-pass option survives, and the existing `E2E_GIT_SMART_HTTP=1` full run is likely already broken at #42; dropping #46 Phase B (M4) **breaks the downstream #68 phase** that reads the planted `jq` rec; the #16 collapse must **keep** the non-owner repo-PATCH→404 leg (no handler test covers it); M5's healthcheck saving is ~30-40s (not 55-80s — `start_interval` helps only the two `--wait` boots, not the `--force-recreate` api recreates) and its `assert_no_run_for_issue` default change is a no-op (all call sites pass explicit args). The #94/#53/#33/#40/#46-fallback drops were independently re-verified safe.
**Scope note**: All work is on the test harness and its compose overlay, with two small exceptions that touch product code (a `SWEEP_INTERVAL` config knob in M6, called out explicitly). No product behaviour changes; no e2e assertion is weakened — every "drop" is verified redundant against a cheaper layer, every "faster" preserves the assertion.

## ▶ RESUME HERE (session paused 2026-07-20)

**State: M1/M2/M3/M5/M8 fully closed. M4+M9 committed and GATE PASSED (173/0, comfortable margins) — their review/audit wave is the only thing owed. M6 deferred to #100; M7 optional, never started.**

| milestone | state |
|---|---|
| **M1, M2, M3, M5, M8** | ✅ DONE — committed, reviewer APPROVE + auditor PASS, full-suite green |
| **M4** (drops) | 🟢 **GATE PASSED** — 173 PASS / 0 FAIL; review/audit wave still owed |
| **M9** (timing hardening) | 🟢 **GATE PASSED** — same run; review/audit wave still owed |
| **M6** (speedups) | ⏸ DEFERRED → [issue #100](https://gitlab.example.com/vtmocanu/uzi/-/issues/100) |
| **M7** (refactor) | ⏸ optional, never started |

**Next actions, in order:**
1. ✅ **Both landed and the tree is clean**: **M4 = `aad3c201`** (11 pass legs dropped, 2 new tests,
   `specs/ai.md` corrected) and **M9 = `859a8066`** (vault lock, `:2108` 6→8s, `wait_card_column`
   10→20s, central margin instrumentation). Nothing further to secure.
2. ✅ **GATE ALREADY PASSED — do not re-run to "confirm".** The final run was **fully green:
   `All E2E checks passed`, 173 PASS / 0 FAIL** — matching the predicted `182 → 173` exactly,
   with `#22`, `#41`, `#68` and `#95` all passing. **And the margins are comfortable**, which is
   the half of the gate that matters: M9's report showed the **tightest headroom in the whole
   suite at 10s** (`card #2 column → Later`, waited 0s of a 10s ceiling), across **92 instrumented
   waits**. No thin margins ⇒ a genuine green, not a lucky one. Notably `#22`
   (`remove: PRDLESS gone…`) completed in **0s of its 20s ceiling** — instantaneous when it works,
   which corroborates the never-converged diagnosis: no timeout value was ever going to help it.
3. **Dispatch the review/audit wave** on the M4 and M9 SHAs. Two items are pre-agreed and owed:
   - The auditor has **two mutation tests queued** — `TestGetRunReviewPerReviewTriage` and
     `TestRunToDTOStopKind`. Both are the *sole* justification for a dropped assertion, so if
     either cannot be made to go RED, that drop is unjustified and blocking.
   - The reviewer must decompose the delta as **11 removed − 2 added = −9** against its
     line-numbered baseline, every vanished leg named.
4. **`#22` is RED and UNEXPLAINED — do not "fix" it by widening a timeout.** Mechanism narrowed:
   `SetIssuePrdless` is forge-first and synchronous, so a 200 whose card lacks PRDLESS proves the
   forge write already succeeded ⇒ **no timeout value can matter**. Three candidates remain (see
   M9's `#22` entry). Catch it with `KEEP_STACK=1` and interrogate forge-fake before concluding.
5. Then `/prd-done` up to PR creation.

**Known environmental issue (not a code defect):** the harness **leaks 4 docker images per run**
and never reclaims them — 646 of 768 images on the dev machine were `uzi-e2e-*` orphans (~125GB).
A partial prune ran (~523 removed, ~123 left). One run died on a `No such container` daemon error,
plausibly but *unprovenly* related. Candidate fix: `down -v --rmi local` in teardown, suppressed
under `KEEP_STACK=1`. Not yet scoped to a milestone.

**Two lead errors corrected in-document, kept for the next reader:** the `#68` option-(b)
recommendation (would have raced `UNIQUE (run_id, seq)`) and the `#22` "too-tight window"
diagnosis (`wait_eq`'s 2nd arg is SECONDS, not tries). Both were caught by teammates, not by me.

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
- [ ] **M4 — drop / downgrade verified-redundant phases**:
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

- [ ] **M9 — timing-fragility hardening (added 2026-07-20; BLOCKS M4's gate and M6)**:
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
