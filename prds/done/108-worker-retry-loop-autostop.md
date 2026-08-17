# PRD #108: Poison-pill message batches — classify permanent failures, auto-stop confirmed retry loops, and stop leaking run HOMEs

**GitLab Issue**: [#108](https://github.com/vtmocanu/uzi/-/issues/108)
**Status**: **COMPLETE — both phases merged.** Phase 1 merged to `main` via MR !95 (`0ae7420a`) on 2026-07-22, released as v0.10.1. **Phase 2 (M4/M5/M7/M9b) implemented on `feature/prd-108-phase2` 2026-07-25** — 11 commits, reviewed and audited at every SHA, plus a tester, a fact-checker and a spec-keeper pass. **Read the "Phase 2 progress" section below before trusting any claim in the Phase 2 milestones above**: implementing it falsified several of them, and the corrections are applied in place with the originals visible. **One coverage boundary is load-bearing and stated here so it cannot be missed: `./e2e/run-e2e.sh` CANNOT reach the auto-stop kill** — not "did not" — so its 182 PASS is regression proof, not kill-path coverage (see Phase 2 progress). Created 2026-07-21; **revised the same day after a fable adversarial review that verified every citation against the code** — three findings changed the design rather than the wording, and four claims in the first draft were measured FALSE; each is corrected in place with the correction left visible. Implementation then falsified **six more** of this document's own claims; those corrections are applied in place below (M9a, 2026-07-22).
**Priority**: High. A run wedges with no product-surface symptom: it reads `running`, spends nothing, produces nothing, holds a run slot, and pins a worker in a ~2 Hz loop against the API. **Bounded, not unbounded** — `RUN_TIMEOUT` (default 2h, `config.go:537`) would have failed it around 20:25. Observed cost on 2026-07-21: 27 minutes and 239 lost messages; worst case without intervention is 2 hours of a held slot and a hot loop. This PRD targets ~2 minutes.
**Depends on**: **one migration** — `runs.stop_kind` is `CHECK (stop_kind IN ('cancelled','plan_rejected'))` (`00050_run_stop_kind.sql:25`), so a distinguishable auto-stop outcome cannot be expressed today. *(The first draft said "nothing new"; that was wrong and is why this line is explicit.)* Touches PRD #4's worker message path, PRD #47's RunHealth detector, PRD #42/#58's per-run `$HOME`.
**Related**: PRD #87 (the run that surfaced this — its Chromium spike produced the first NUL bytes any uzi worker has emitted). PRD #99 (rewrote `workersvc` message handling in the same release; see the honesty note in M0).

## M0 — Root cause, confirmed from a live wedge (2026-07-21)

Run `4d4762cf` (issue #87) was approved at 18:24; its last persisted message is **seq 273 at 18:35:12**. The worker kept working — the filesystem shows `/tmp/m2spike/dbtest`, `dbtest2` created at 18:36 and 18:37 — and none of it reached the API. From 18:37 to 19:05 the pair looped:

```
worker: POST /api/worker/runs/4d4762cf/messages returned 500  (~2 Hz, count climbing 206 → 239)
api:    ERROR "worker run messages": unsupported Unicode escape sequence (SQLSTATE 22P05)
```

Cancelling produced `message batcher closed with undelivered messages … dropped: 239`.

**The payload.** A headless Chromium's stderr was captured into a `tool_result`. That file is 25,418 bytes and holds **84 NUL bytes** (`wc -c` 25418 vs 25334 after `tr -d '\0'`) — HarfBuzz error spew embeds raw NULs. `run_messages.payload` is **`jsonb`** (`00020_workers_runs.sql:76`); Postgres cannot represent `\u0000` there and rejects with 22P05. **Postgres behaviour, not a uzi regression** — the column has been `jsonb` since `00020`, so v0.9.0 rejects the same payload. What is new is that a worker finally emitted a NUL, which took a browser.

*(Two corrections to the first write-up, kept visible because both were confident and wrong. It blamed the 0.10.0 deploy that landed an hour earlier — asserted with no evidence. And it called the loop "unbounded in principle" and said it "would have cost the whole night", while `RUN_TIMEOUT` bounds it at 2h and `health.go`'s own header names that cap as one of "the only liveness backstops". PRD #99 did rewrite `workersvc` message handling in this release (+414 lines), so version-attribution cannot be settled by inspection — M1 reproduces against **both** v0.9.0 and v0.10.0 rather than assuming.)*

**Four independent defects compose into the wedge.** Fixing only the first leaves the class wide open:

1. **No NUL sanitation anywhere.** No `\u0000` stripping exists in `agent/src` or `api/internal` outside `vault.go`'s unrelated AAD construction. Two sanitizers for untrusted worker text already exist and neither is on this path: `sanitizeSelfReported` (`handler/worker_protocol.go:38-44`) and PRD #90's `sanitizeMemoryField`.
2. **The status code lies, and the status code is the retry contract.** `WorkerRunMessages` (`handler/worker_protocol.go:320`) already returns **400** for `ErrInvalidMessage`. A store rejection falls to `default:` → **500** (`:345`). The batcher treats any throw as retryable and re-buffers the identical batch (`batcher.ts:105-118`). **Even with perfect sanitation, any future permanently-unstorable payload wedges a run identically** — the loop is the class; NUL was this month's trigger.
3. **The batcher has no give-up in its steady state.** `close()` bounds retries at 3 (`batcher.ts:130-140`); `doFlush` re-buffers and calls `scheduleFlush()` forever at a fixed sub-second cadence, no backoff.
4. **The retry batch GROWS, and the API caps bodies at 1 MiB** (`httpx/respond.go:11-12`, enforced via `io.LimitReader` at `:34`). `doFlush` posts the entire buffer (`const batch = this.buffer`), and on failure concatenates it back ahead of new messages — the incident's own `206 → 239` is that growth. So a long-enough wedge crosses 1 MiB and the failure **changes class from 500 to a 400 decode failure**. Found by review, not by the incident, and it has three consequences: an oversized single `tool_result` is a *second* poison-pill trigger untouched by sanitation; a chatty run that merely rides out a transient outage can cross the cap and take a permanent 400; and a rotating error class disarms the "same error each time" guard in §4.

## Why RunHealth did not catch it — the part worth reading

PRD #47 already ships the vocabulary this needed. `workersvc/health.go` `runningTarget` classifies a running run **`looping` > `stalled` > `slow`**, and `looping`'s reason text is literally *"the agent keeps repeating the same action"*. It reported **`slow`**. Three structural reasons, verified in review:

- **`looping` reads its evidence from persisted `run_messages`** (`toolWindow` → `ListRunToolWindow`, `health.go:231-241`). The wedge *is* a failure to persist messages, so the detector's only evidence source is the thing that broke. Blind **by construction**, not by threshold.
- **`stalled` is suppressed while a tool call is in flight** (`!stats.inFlight`, `health.go:205`; Decision 9: "a long build/test-suite emits one tool_use then nothing until its result — that is working, not stalled"). The last persisted message, seq 273, is a `tool_use` with no result. The wedge presents as **exactly** the benign case the detector excludes on purpose. That exclusion is correct and must not be weakened.
- Falling through both, it hit **`slow`** at the 45-minute wall-clock default (`DefaultHealthSlowSeconds = "2700"`) — a duration heuristic that says the right vague thing for the wrong reason and equally flags a healthy long run.

**The conclusion driving the design: a detector that infers health from the message stream cannot detect a broken message stream.** The new signal must be one the wedge cannot suppress — the API's own count of write failures for that run, produced server-side on the failing path itself, needing no cooperation from the worker.

## Solution

### 1. Sanitize at the authoritative choke point (and at the source)

Strip in **`workersvc.AppendMessages`**, server-side, before the insert. The worker is untrusted input on this route by the same reasoning that produced `sanitizeSelfReported` in that very file; a worker-only fix protects only workers running the new image. Also strip worker-side as defense in depth, explicitly not the mechanism.

**Scope, widened after review — the first draft's "`\u0000` is the one codepoint `jsonb` cannot represent" was FALSE:**

- **`\u0000`** — the incident's trigger.
- **Unpaired surrogates U+D800–U+DFFF**, which `jsonb` rejects just as hard, and which are a realistic Node-side trigger: well-formed `JSON.stringify` emits a lone `\udXXX` escape whenever a JS string was sliced mid-surrogate-pair, so **any worker-side truncation of tool output can produce one.** Replace with U+FFFD.
- **Raw invalid UTF-8**, which reaches Postgres as **22021**: `service.go:1068` validates with `json.Valid`, and Go's scanner does not validate UTF-8.
- **The sibling TEXT columns, not just the payload.** `kind`, `agent`, `agent_instance`, `agent_label` are worker-controlled and inserted verbatim (`service.go:1083-1085`; `truncateRunes` truncates but strips nothing) — *corrected on two counts, post-implementation: `kind` is a FOURTH such column the first draft never listed, and `agent` was not merely "truncated but not stripped" like its siblings, it had NO cap at all until this phase*. Postgres `text` cannot hold NUL any more than `jsonb` can, so a NUL in `agent_label` wedges a run identically and entirely outside a payload-only fix.

Still **do not** broaden to the whole control class: `\n`, `\t` and ANSI escapes are legal in `jsonb` and load-bearing in tool output — stripping `\n` would mangle every log the UI renders. `sanitizeMemoryField` strips the wider class because its sink is a **terminal table printer**; this payload lands in a React renderer that escapes it. Different sink, different rule.

**Implementation caution (review finding, easy to get backwards): the NUL arrives as the six-byte escape `\u0000` inside otherwise-valid JSON** — a raw `0x00` byte is already invalid JSON and 400s today. The strip must therefore be **JSON-aware**: a naive byte-substring replace corrupts any payload containing the literal text `\\u0000`.

### 2. Make the status code an honest retry contract

Return **400** (joining `ErrInvalidMessage`) for a store rejection that can never succeed, via a new `workersvc.ErrUnstorableMessage`. Map the **enumerated** SQLSTATEs — **22P05** (unsupported escape), **22P02** (invalid text representation), **22021** (invalid byte sequence), **22003** (numeric overflow) — not a vague "and its neighbours". A 500 must mean "try again"; anything else is a lie the client is obliged to believe. *(Corrected post-implementation: this is FOUR SQLSTATEs, not three. `22003` was added because `{"n":1e1000000}` is valid JSON, survives sanitation untouched, and `jsonb` cannot store it — permanent by construction, exactly like the other three, and absent from the first draft only because nobody had tried it yet.)*

**This is the load-bearing fix.** With it, incomplete sanitation degrades to "that batch is rejected and the run fails with a clear reason" instead of "the run wedges silently".

### 3. Worker-side circuit breaker — and a bounded batch

In `batcher.ts`: bounded exponential backoff instead of a fixed cadence; a breaker that trips immediately on the permanent-failure classes (401/403/404, a rejected tombstone, an exhausted bisect budget) and, on an ordinary transient failure, after it has been unbroken for **~10 minutes** — *corrected post-implementation from "N consecutive failures of the same batch": once §2 makes a genuine 5xx mean "retry", a tight consecutive-count trip fails healthy runs through an ordinary API restart, which is this decision's own mirror-image bug*; on trip, stop retrying and fail the run loudly.

**"Never retry a 4xx" is NOT safe on its own** and must ship with the batch bound (review's blocking finding): because the batch grows, a healthy run riding out a transient outage can cross the 1 MiB cap and take a permanent 400 it did not earn. So M3 must also:

- cap batch **bytes** and **split** oversized batches rather than posting one giant body;
- on a permanent rejection, **bisect** to isolate the individual poisoned message and **tombstone** it (re-post under its own seq as a worker-minted marker) — *corrected post-implementation from "drop only that one": a true drop breaks `web/src/lib/runStream.ts`'s seq-contiguity requirement and freezes the live run view at the gap permanently, trading a server-side wedge for a client-side one* — and let the rest land, preserving the run instead of failing it;
- treat "400 because the body was too large" as a **split-and-retry** signal, never as a poison verdict.

**Two ordering traps to design around.** The breaker's explanatory message must **not** travel through the batcher — order-preserving `concat` (`batcher.ts:114`) queues it behind the poison, where it never lands; route it through `reportState`'s separate endpoint (`client.ts:142-158`), which already has bounded retries and 4xx-fatal semantics. And everything buffered behind the poison is **dropped** — a permanent truncation of run history that this PRD owns as an explicit decision rather than leaving implicit in the incident's `dropped: 239`. Bisection (above) is what keeps that loss to one message instead of hundreds.

### 4. Server-side auto-stop for a CONFIRMED loop — guards, and an honest scope

The worker fix does not protect a fleet on an older image, so the authoritative stop is server-side. The API counts consecutive `AppendMessages` failures per run (in-process, mirroring `usagepoller`'s backoff map). A run is auto-failed **only** when every one of these holds — *(corrected post-implementation: the reason is NOT `message_persist_permanent`. `SetRunFailed` overwrites `failure_reason` unconditionally with the worker's own "run cancelled" on the live-poller half, so the two halves cannot carry the same string; and every other `failure_reason` in the codebase is human prose rendered verbatim by the CLI and the web. `runs.stop_kind = 'auto_stopped'` is the machine-readable contract instead, and `failure_reason` is prose that must never be parsed — see `autostop.go`)*:

| guard | why |
|---|---|
| ≥ **N consecutive** failures for this run (start at 20; ~10s at 2 Hz) | one failure is noise |
| sustained ≥ **60 seconds** | rides out a pool exhaustion or an eviction |
| the run's `max(seq)` has **not advanced** in the window | proves no progress, not merely errors |
| **other runs are succeeding** on the same API instance in the window | the outage-vs-poison discriminator |
| the error is the **same class** each time | a rotating error is an outage signature |

**The global-versus-per-run discriminator is what makes auto-stop safe.** If the API is broken, *every* run's writes fail and killing them turns an outage into data loss. If one run fails while neighbours succeed, the fault is that run's payload and nothing else can clear it.

**Honest scope, stated because the first draft oversold this milestone: M5 would NOT have fired on the incident that motivated it.** There was one active run, so there was no comparison set — and a neighbour mid-long-build appends nothing, so "active neighbour" is not "succeeding neighbour". On a single-active-run instance the rule degrades permanently to **flag + notify, never kill**, which is the correct behaviour on insufficient evidence and must be tested as such. **M5's tested value on this deployment class is the flag; the kill is for genuinely multi-active-run instances.** The fixes that actually close the incident are M2 and M3.

**The stop mechanism, corrected — the first draft described a path that does not exist.** It claimed auto-fail writes terminal state which "the worker's next state poll sees". **There is no worker state poll.** A user cancel on a run with a live poller does not write terminal state at all: it enqueues a stop verdict (`CreateStopVerdictInput`, `service.go:2067`) which the worker's **steering** poll consumes (`steering.ts:225-227`) and aborts the SDK — that is the 3-second abort observed in the incident. `CancelRunServerSide` (`service.go:2040`) runs only in the **no-live-poller** branch. So M5 must implement both halves explicitly: **enqueue a synthetic stop input** for a live worker, and take the server-side terminal transition as the no-poller fallback. And because `stop_kind` is `CHECK (stop_kind IN ('cancelled','plan_rejected'))`, a distinguishable outcome needs **a migration plus old-worker compatibility analysis for an unknown input kind**.

**Health first, kill second.** Before the window elapses the run is flagged `looping` with a reason naming the persistence failure — the enum already exists end to end (workersvc → `health_reason` → slacksvc wording → web badge), so M4 adds a detector, not a vocabulary.

### 5. Stop leaking run HOMEs

`runner.ts:455-458` does `fs.rm(runHome, { recursive: true, force: true })`. **`force: true` suppresses ENOENT, not EACCES.** The Go module cache writes directories mode `0555`, so unlinking anything inside fails and the tree survives:

```
run HOME cleanup failed … EACCES: permission denied,
unlink '/data/agent-home/4d4762cf-…/go/pkg/mod/gopkg.in/inf.v0@v0.9.1/benchmark_test.go'
```

Measured leftover: **167.3 MB for one run** (`/data` at 219 MB of 19.5 GB — not urgent, and unbounded). Every Go-touching run leaks, which is most of them. Fix: restore write permission on directories during a walk, then remove. Cleanup stays best-effort and must never fail a run. Ship a one-off reclaim for HOMEs already stranded on fleet PVCs.

## Design Decisions

1. **Server-side sanitation is the mechanism; worker-side is an optimization.** The worker is untrusted input here, exactly as `handler/worker_protocol.go` already assumes for self-reported strings.
2. **Strip `\u0000` and unpaired surrogates, in the payload *and* the sibling text columns; do not broaden to the control class.** Sink-driven: this renders in React, not a terminal table. The first draft's narrower scope rested on a false premise about `jsonb` and is corrected above.
3. **Permanent failures return 400, from an enumerated SQLSTATE set — four codes, not three (corrected post-implementation; see Solution §2).** The status code is the retry contract; a permanent failure returned as 500 is what converts one bad payload into an unbounded loop. This closes the *class*, which is why the PRD is not titled "strip NUL bytes".
4. **Bound the batch before making 4xx fatal.** Shipping "never retry a 4xx" without the byte cap and bisection would convert transient outages into failed healthy runs — a strictly worse bug than the one being fixed.
5. **Auto-stop requires a comparison set; with none, flag and notify.** A rule that cannot distinguish "this run is poisoned" from "the database is down" must not kill runs.
6. **Auto-stop reuses the stop-verdict input path** (proven in the incident) with the server-side transition as fallback — and accepts the migration that a distinguishable `stop_kind` requires.
7. **The api hard singleton is a load-bearing assumption of M4/M5.** `deploy/chart/values.yaml:48-52` documents three reasons the api cannot run >1 replica. The in-process streak counters and the "other runs succeeding on this instance" comparison set are a **fourth**: split traffic never reaches `N` and the comparison set becomes per-pod noise. M5 adds that fourth reason to the values.yaml comment in the same MR, so a future HA effort cannot silently invalidate the guard.
8. **Auto-stop is NOT subordinate to the health toggle.** `detectRunHealth` no-ops when `HealthEnabled` is false (`health.go:95-106`). The `looping` flag may ride that toggle; the availability fix must not, or an admin disabling health silently disables loop protection.
9. **Do not weaken `stalled`'s in-flight suppression.** It is correct — a long build genuinely looks like silence. The wedge is caught by a new, independent signal instead.
10. **No metrics endpoint exists** (no `promhttp` or `/metrics` anywhere in the api). Alerting is out of scope; M7 records the gap and the log lines to key on rather than pretending a dashboard is one milestone away.

## Milestones

**Two phases, two MRs, and the split is load-bearing rather than cosmetic.** Phase 1 closes the incident outright: after it, a NUL cannot wedge a run, a permanent failure cannot be retried forever, a batch cannot grow into the 1 MiB cap, and HOMEs stop leaking. It is small diffs in small files (`batcher.ts` is 142 lines; `worker_protocol.go` 515) at low risk. Phase 2 adds *detection and killing*, which is where both the cost and the danger are — a new signal source in the health detector, a migration for the `stop_kind` vocabulary, and a test suite whose centre of gravity is **negative** cases.

**Phase 1 is shippable alone and Phase 2 is genuinely optional**, which the honest scope note in §4 already implies: on a single-active-run instance M5 degrades to flag-and-notify permanently, so on this deployment class Phase 1 *is* the fix and Phase 2 buys observability plus protection for multi-run instances and for workers on older images. Do not let Phase 2's difficulty delay Phase 1.

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| **1 — close the incident (MR 1)** | M1 repro · M2 sanitize + 400 · M3 batcher · M6 HOME · M9a docs | nothing | `workersvc`, `handler/worker_protocol.go`, `agent/src/batcher.ts`, `agent/src/runner.ts`, `ARCHITECTURE.md`, `specs/ai.md` |
| **2 — detect and stop (MR 2)** | M4 detector · M5 auto-stop (+ migration) · M7 observability · M9b CLI + docs | Phase 1 | `workersvc/health.go`, a migration, `deploy/chart/values.yaml`, `api/cmd/uzi/`, `docs/` |

*(M8 was **"audit the other unbounded retry loops"**. It is now issue [#109](https://github.com/vtmocanu/uzi/-/issues/109) — an audit has no natural end, it does not belong inside a fix, and leaving it here made this PRD look ~30% larger than the actual work. The number is deliberately not reused: a gap that records where something went beats a renumbering that hides it.)*

### Phase 1 — close the incident

- [x] **M1 — Reproduce, RED first.** Integration test POSTing a payload carrying the `\u0000` escape (assert today's 500); a lone-surrogate case; an oversized-batch case crossing the 1 MiB cap; a runner test with a `0555` directory under a fake HOME. **Run the message test against v0.9.0 and v0.10.0** and record which — the incident write-up guessed and was wrong once.
- [x] **M2 — Sanitation + honest status code (api).** Strip `\u0000` and unpaired surrogates, JSON-aware, in the payload and NUL-strip `kind`/`agent`/`agent_instance`/`agent_label` — *corrected post-implementation: FOUR columns, not three; `kind` is a fourth worker-controlled text column the first draft never listed, and `agent` was uncapped ENTIRELY (not merely unstripped) before this milestone*; add `ErrUnstorableMessage` mapped to 400 for SQLSTATEs 22P05/22P02/22021/22003 (four, not three — see Solution §2). Count and log every strip so a NUL-emitting tool stays visible rather than silently laundered.
- [x] **M3 — Bounded batch, backoff, breaker (agent).** Byte cap + splitting; bisection to isolate one poisoned message, tombstoned rather than dropped; exponential backoff; 4xx not retried **except** the oversize case, which splits; breaker on the permanent classes immediately and on ~10 minutes of unbroken transient failure — *corrected post-implementation from "N identical-batch failures", see Solution §3*; the failure report routed through `reportState`, never the batcher.
- [x] **M6 — HOME cleanup that survives read-only directories (agent).** Permission-restoring removal, still best-effort, tested against a `0555` directory. Plus a one-off reclaim that **skips any `agent-home/<runId>` whose run is non-terminal** — a sweep racing a live run is worse than the leak. **Fully independent of everything else here** — it shares only the incident that surfaced it, so it can land first or in parallel.
- [x] **M9a — Phase-1 docs.** `ARCHITECTURE.md` gains the message-path contract (what is sanitized, what returns 400/413/500, how a batch is bounded and bisected); `specs/ai.md` records Decisions 1-4 *and* the `/state` silent-strip decision — *corrected: "Decisions 1-4" undersold the set, since Phase 1 also ships M6 (HOME reclaim), which needed its own `docs/configuration.md` + `.env.example` entries (`UZI_HOME_RECLAIM`), not a specs/ai.md decision*. No CLI change in this phase: Phase 1 alters no DTO and adds no reason string the CLI renders.

### Phase 1 progress — 2026-07-21

**5 of 5 milestones implemented, gated, validated end-to-end, and merged to `main` via MR !95 on 2026-07-22 (M9a landed the same day, docs-only). The checkpoint gaps below are now closed.**

Branch `feature/prd-108-phase1`, 16 commits from `893fe7eb`. Two tracks (`api/**` and `agent/**`) built in parallel worktrees and merged twice with no conflicts.

| Milestone | Commits |
|---|---|
| M1 | `0d99731a` (api, 7 RED cases) · `5b5e5caf` (agent characterization) |
| M2 | `4bfb70d2` · `c88cfea8` · `0e62e8cc` · `e1935d78` · `f2ddb5ce` |
| M3 | `cdfd91d4` (cap/split/backoff) · `05d138a3` (bisect/tombstone/breaker) |
| M6 | `c7d9cce6` · `00ad07ba` (audit) · `b173ce4c` (review) |

Gates on the merged tip: api `go vet` + `go test ./...` clean (var unset), live sweep RUN=154 PASS=154 SKIP=0, fuzz clean; agent 792 tests / 791 pass / 0 fail / 1 pre-existing skip; web 948 passing, typecheck and `check-docs` clean.

**M1's version question is answered: both v0.9.0 (`89a76017`) and v0.10.0 (`1a3c5494`) reproduce identically** — status 500, SQLSTATE 22P05, 0 rows persisted, measured in separate detached worktrees against separate databases. M0's "Postgres behaviour, not a uzi regression" is confirmed; PRD #99's rewrite did not introduce it.

**Checkpoint gaps — all now closed (updated 2026-07-22):**

- **M9a is DONE (2026-07-22).** ARCHITECTURE.md, `docs/configuration.md`, `.env.example`, `specs/ai.md`, and `CHANGELOG.md` are written; the PRD corrections queued below are applied in place in this same pass.
- **Review is COMPLETE: 13 of 13 commits**, across three passes (M6, Track A, Track B), plus the merge commit itself — verified rather than assumed, by diffing the merged tree against both branch tips (empty both ways) and confirming the two change lists are disjoint. **Audit is now COMPLETE for both tracks**: the auditor has since finished Track B's previously-unaudited halves (`worker_protocol.go`, `sanitize.go`, and its 107 lines of new tests), run its gates, and exercised the N3 log-leak security test green — see `f2ddb5ce`'s status below.
- **The real runtime path is now exercised (2026-07-22).** `./e2e/run-e2e.sh` passed 182/0 and the live-DB sweep 161/161/0/0; criteria 1 & 2 were proven **end-to-end over the network** (nginx → api → real Postgres): a NUL payload → 204, persisted sanitized, the run continues; `{"n":1e1000000}` → 400, zero rows — each with a positive control on the same database. Residual gap, unit-covered rather than driven through the stub e2e: the client-side bisect/tombstone/breaker and the Go-`0555` HOME-leak trigger (a stub executor runs no `go` and emits no real poison).
- **Merged.** MR !95 → `main` (`0ae7420a`) on 2026-07-22, CI green — after fixing a CI-only `runner.test.ts` hang (an un-`unref()`'d FakeApi test-server handle lingering on musl/alpine; test-only, not a product bug). Released as v0.10.1; ArgoCD deploy to dev-cluster underway.

**Three milestones shipped beyond their written scope**, so the checkbox text above no longer describes what landed:

- **M2** additionally returns **413** for an oversize body (`DecodeJSONLimited`, so `DecodeJSON`'s 57 call sites are untouched), enumerates **four** SQLSTATEs rather than three (22003 added — `{"n":1e1000000}` is valid JSON, survives sanitation and is permanently unstorable), sanitizes **`kind`** as a fourth worker-controlled text column the PRD never listed, and caps `agent` at `agenttmpl.MaxNameLen` where it previously reached an unbounded `text` column on every frame.
- **M3** **tombstones instead of dropping** — a dropped seq permanently freezes the web client's live run view, because `runStream.ts` buffers any `seq > lastSeq+1` and never advances — and its breaker trips on a **duration** (~10 min transient) plus the permanent class, not on "N identical-batch failures"; a tight N would fail healthy runs through an ordinary API restart.
- **M6** gained a startup bail (the awaited sweep could gate worker registration for hours against a hanging API), a `UZI_HOME_RECLAIM` operator kill switch, and a terminality oracle that deletes only on a positively-observed terminal status.

**FIXED (A1) — a second poison pill on this route, closed this session.** `run_usage`'s primary key is `(run_id, session_id, model)` (`00062_run_usage.sql:35`), and both `session_id` and `model` are worker-controlled `text`. A btree index entry is capped at 2704 bytes; exceeding it raised **SQLSTATE 54000**, which is not in the enumerated permanent set, so it fell through to 500 and the batcher retried forever — a payload that survives everything else Phase 1 does, since it is valid UTF-8 with no escapes and the sanitizer's fast path returns it untouched. Measured on `postgres:17` against the real schema, via two independent vectors (oversized `model`, and oversized `session_id` with a normal model): a positive control landed the index entry at 3040/3048 bytes — over the 2704 limit — on incompressible data.

**The fix is a cap, not a code** (adding 54000 to the enumerated set would not have worked: the error is raised by `foldRunUsage`, not by `InsertRunMessage`, and the classifier is deliberately statement-level). `foldRunUsage` now caps both `session_id` and `model` at 200 runes (`maxUsageSessionRunes`/`maxUsageModelRunes`, `workersvc/service.go`) before the upsert — the same `truncateRunes` treatment `c88cfea8` gave `agent` — and the positive control above now returns **204**, not 500.

*Reproduction note worth keeping: `repeat('m',N)` does **not** reproduce it, because pglz compresses the index entry — it needs incompressible data. Anyone testing this with a repeated character concludes there is no bug.*

`e1935d78`'s tripwire comment — whose column enumeration was complete but whose *argument* reasoned only about character validity and never about length, and whose `session_id` claim ("comes from the runs row, so Postgres already accepted it") did not transfer to a composite primary key — is corrected by **A2**: the comment now names the length/composite-PK reasoning directly (see `service.go`'s `maxUsageSessionRunes` comment).

**Smaller open items, all closed this session (or recorded as intentional) — mapped to their commit:**

- **FIXED (A3) — `kind` is now capped, not just sanitized.** `f2ddb5ce` NUL-stripped `m.Kind` and added a post-strip empty check but no rune cap; `A3` adds `maxKindRunes = 64` (`truncateRunes(m.Kind, maxKindRunes)`, applied before the two log lines that echo it), closing the same class `c88cfea8` closed for `agent`.
- **Recorded, not a defect: a legal-but-unstorable JSON number costs a message, by design.** `{"n":1e1000000}` survives sanitation untouched — correctly, since silently rewriting a number would be worse corruption than dropping the message — takes a 400, and under M3 is bisected out. That is the right trade; it is a **new class of one-message data loss** the original Success Criteria did not mention, now recorded in `specs/ai.md` (the Decision 3 entry) rather than left implicit.
- **FIXED (B1) — a 413 arriving *during* bisection no longer tombstones a clean message.** `bisect` now treats an `oversize` verdict the same way it already treats `transient`: abandon the search and `return batch.slice(lo)`, letting the outer oversize arm split instead of narrowing the poison window on a size signal that was never evidence about the payload.
- **FIXED (B2/B3) — the reclaim's bail no longer counts a 404 as an outage.** `home-reclaim.ts` now distinguishes "the api answered not-found" (skips, resets the streak, never bails) from "the call could not be made" (skips, counts toward the bail) — see the `RunStatusLookup` contract. This closes the sibling finding below (`stoppedEarly`'s label) in the same commits.
- **FIXED (B4) — the tombstone now carries `agent_instance` and `agent_label`**, so a tombstoned subagent frame stays in its own lane instead of rendering in the top-level stream.
- **FIXED (B5) — a tombstone can no longer be re-tombstoned.** `tombstone()` early-returns unchanged when `item.tombstoned` is already set, so a re-buffered marker cannot lose its original kind/size to a second pass.
- **FIXED (A6) — the "57 non-test `DecodeJSON` call sites" tally is corrected to cite the shape, not a count.** The comment (`httpx/respond.go`) now deliberately keeps no tally ("a tally drifts exactly like a line number") rather than naming a number that had already drifted once.
- **FIXED (A5) — `sanitizePayloadJSON`'s read-only contract is now explicit in its doc comment** (`workersvc/sanitize.go`), rather than being an invisible property of the call site.
- **FIXED (B6) — SECURITY: `agent` and `kind` are now redacted worker-side, symmetric with `agent_instance`/`agent_label`.** `batcher.ts`'s `emit()` now runs both through `redactText`, closing the gap where a secret placed in either field reached Postgres, the WS frame, the browser, and `uzi run logs` unscrubbed. Pinned by a direct kind-secret-redaction test (test-only commit `5426cedf`). This was distinct from the missing cap (A3): a capped `kind` was still an unredacted one, and now neither is true.
- **FIXED (A4 + A4b) — `failure_reason` and its four siblings on `/state` are now sanitized, closing the class on this sibling route.** `A4` added `sanitizeFailureReason` (strip + 2048-rune cap); `A4b` extended the strip-only treatment to `session_id`/`plan_md`/`branch`/`mr_web_url`. This closes the NUL/22021 class on **both** `/messages` (M2 + `f2ddb5ce`'s `kind`) and `/state` — new work beyond the original M2/checkbox scope, recorded here rather than silently absorbed into it.
- **FIXED (same commits as above) — `stoppedEarly: "api_unreachable"` now names a cause the check actually measures.** The reviewer's and auditor's independent findings — three reclaimable HOMEs skipped and stuck at zero across boots, vs. a misleading diagnostic pointing an operator at a healthy api — are both closed by the same fix: only a genuine could-not-ask counts toward the bail.
- **`f2ddb5ce` is now FULLY AUDITED = CLEAN**, not partly. The auditor has since completed the previously-unaudited halves (`worker_protocol.go`, `sanitize.go`, and its 107 lines of new tests), run its gates, and exercised the N3 log-leak security test green.
- **FYI, not a Phase-1 defect: `SubmitInput` (`service.go` ~2239) writes user-supplied `body` via `pgText` with no sanitation, on a user-driven route (it takes a `userID`, not a worker).** This is user steering input (approve/reject/request-changes/follow-up), not the worker `/state` or `/messages` path — a different actor and a different failure mode (a one-off 500 on bad user input; no batcher rides this route, so no retry wedge). Recorded as a known adjacent-route gap, in the spirit of M8→#109, and explicitly out of Phase 1's scope.

**Six of this document's claims were falsified by implementing it.** Corrections have been applied in place across eight edit sites (M9a, 2026-07-22): the three-SQLSTATE enumeration (Decision 3, Solution §2 — now four); "drop only that one" (Solution §3 — now tombstone-not-drop); "Decisions 1-4" (M9a — Phase 1 also ships M6); the breaker's N-consecutive-same-batch rule (Solution §3, M3 — now a ~10-minute duration); Solution §1's account of the text columns, wrong twice in one bullet (`agent` was uncapped entirely, not merely unstripped; and `kind` is a fourth column) — the latter also in M2's checkbox.

*(Two things deliberately **not** corrected: M0's account of the pre-fix failure is history and stays as written — fixing what a passage describes does not falsify the passage; and Risk §2's "~8 extra round-trips" is correct for the one-sided bisection that shipped, and only looked wrong against a two-sided variant costing 16.)*

### Phase 2 — detect and stop

- [x] **M4 — Server-side loop detection → `looping` health.** Per-run consecutive-failure counter with **eviction on terminal state**; flag `looping` with a reason naming the persistence failure. **Ships independently of M5 and is the cheap two-thirds of this phase's value** — the flag alone turns the incident from silent into obvious.
- [x] **M5 — Auto-stop, guarded.** Stop-verdict input + no-poller fallback + the `stop_kind` migration + the values.yaml singleton note. **The tests are mostly negative**: an API-wide outage must auto-fail nothing; a single failing run beside healthy neighbours must; **no comparison set must flag and not kill**; one success resets the streak; a wedged **chat** run (`chat-runner.ts:128` builds the same batcher against the same endpoint) is covered by the same rule. *(Corrected post-implementation: **this fixture was never needed.** The architect's design made the guards a pure function of in-process state plus one `GetRunByID`, with `Service.now` injectable and `Store` already an interface — so the "multi-run" part is two map entries in a fake-store suite, not two rows in Postgres. Live DB is needed for exactly two things, both narrow: `00082`'s widened CHECK and `FailRunAutoStop`'s SQL. That was the single largest scope reduction in the phase, and this line predicted the opposite.)*
- [x] **M7 — Observability + the honest gap.** Structured fields on the auto-stop decision (run id, streak, window, comparison-set size, decision) so an operator can reconstruct why a run died. Record that no metrics surface exists and name the log lines; leave a metrics endpoint to its own PRD.
- [x] **M9b — CLI + Phase-2 docs.** Per CLAUDE.md's second-consumer rule, `api/cmd/uzi/` must render the new health reason — *corrected post-implementation: `run.go` already rendered `HEALTH_REASON`/`FAILURE_REASON` generically, switching on no vocabulary, so the reasons owed NO CLI change. What was owed was a `STOP_KIND` row: `stop_kind` was rendered nowhere, and without it an auto-stopped run printed `STATUS failed` / `FAILURE_REASON run cancelled`, byte-identical to a user cancel.* `ARCHITECTURE.md` gains the auto-stop rule and its guards; `docs/` gains the operator-facing "why was my run auto-failed"; `specs/ai.md` records Decisions 5-10.

### Phase 2 progress — 2026-07-25

**4 of 4 milestones implemented on `feature/prd-108-phase2`, 11 commits from `6be9f542`.** Reviewed and audited at every SHA, plus a tester (e2e + mutation), a fact-checker (every claim-bearing artifact) and a spec-keeper (`specs/ai.md` §367) pass.

Gates on the tip: api `go vet` clean and `go test -race ./...` green with **zero** DATA RACE (`UZI_TEST_DATABASE_URL` unset); live sweep `RUN=162 PASS=162 FAIL=0 SKIP=0` with a positive control; web 959 + typecheck + check-docs; agent 814/813/0/1 pre-existing skip; `./e2e/run-e2e.sh` **182 PASS / 0 FAIL**.

**🔴 THE COVERAGE BOUNDARY, stated first because it is the one thing a reader must not get wrong.** `./e2e/run-e2e.sh` **cannot** reach M5's kill — not "did not". Three independent structural reasons: the harness carries zero Phase-2 assertions; the stub executor's sentinel vocabulary cannot emit a NUL, a lone surrogate, an out-of-range JSON number or a >1 MiB body; and the harness builds a **0.10.1+** agent, which sanitizes, caps, splits and bisects, while auto-stop exists to protect **pre**-0.10.1 workers. **Its 182 PASS is exactly the Phase-1 baseline, and that identity IS the finding: Phase 2 added zero e2e assertions.** Regression proof, not kill-path coverage. Closing it needs a direct-to-API probe (POST `{"n":1e1000000}` ~20× over 60s beside a succeeding run, ~90s, no product change) — deliberately **not** built here and split out as issue [#125](https://github.com/vtmocanu/uzi/-/issues/125), the same disposition M8 got as #109. Recorded rather than silently skipped, because a coverage boundary nobody wrote down reads as coverage.

**Five defects were found by validators that every green gate had passed.** Each is listed with the instrument that caught it, because the instrument is the transferable part:

1. **The killable-class guard the design called "the most important" was never implemented.** `autoStopKillableKinds` did not exist; all four failure classes could kill, including `store`, which the code's own comment calls "transient BY CONTRACT". A run whose messages persisted perfectly could be killed at ~75s over a failing `run_usage` upsert — while Phase 1's client breaker waits ~10 minutes on the same class. Found by two validators independently. **The only trace was that three artifacts carried three different guard counts, all consistent with a guard being absent.**
2. **A requeued run was killed on the dead attempt's streak.** uzi grants a fresh attempt, then auto-stop destroys it before the new worker persists a byte. Sharpest shape, from the auditor: *an operator upgrades the worker image to fix the wedge, and the fix is what the kill lands on.* The sweep beats a cold `git clone` by default, not occasionally. Fixed at the **recorder** under one rule — *a streak is evidence about one running attempt* — retiring the class rather than patching each requeue path.
3. **The fix for (2) was applied to one of two recording hooks.** `NoteOversizeBatch` stayed on the old terminal-only rule, so an `oversize` streak accumulated at the approval gate and killed the run on approval. The commit claimed to retire a class and retired the path it measured.
4. **A test certified a guard it never reached.** `...WillNotKillOverAStableUsageFoldFailure` staged its neighbour 61s against a 60s window, so G4 blocked and G5 was never consulted — it passed on a different guard than the one it exists for. Fixed with an explicit `peersSucceeding > 0` **precondition** rather than corrected staging, so a future change to the window announces itself instead of silently returning the test to certifying the wrong guard.
5. **The outage test silently stopped testing G4** the moment the class gate landed: staged on `store`, it was refused upstream and never reached the comparison set. Split in two, verdicting for different reasons.

**The transferable lesson, and it is narrower than "re-read your comments".** Four comments were true when written and falsified by a guard added **upstream** of what they described — they stayed true about their own function and became false about the path. And every blocking item in the final wave was **a claim verified against one instance and asserted about the class**. The check is not re-reading; it is **enumerate the siblings** — every recording hook, every artifact carrying the number, every consumer of the numbering. The instrument that caught all five: **fold the code and check that the intended assertion is what reddens.** A fold that stops reddening is the signal.

**Corollary, learned by breaking it four times: duplicate the CLAIM, never the COUNT.** A duplicated claim is a cross-check that fires when reality moves — three disagreeing tallies are the only reason defect (1) was found. A duplicated count is four things to drift: this branch shipped "five guards" and "six guards" simultaneously across four artifacts, **three of them written after the rule prohibiting it, one in the same commit as the rule**, and the true conjunction is nine conditions. All four are now deleted in favour of citing the mechanism.

## Success Criteria

**Phase 1 — after this, the incident cannot recur:**

- A tool result containing NUL bytes or a lone surrogate is **persisted sanitized** and the run continues; nothing 500s.
- A genuinely unstorable payload returns **400**, is **not** retried, and costs **one message** (bisected out), not the run.
- A chatty run that rides out a transient outage **never** takes a permanent 400 from batch growth.
- A Go-touching run leaves **no HOME behind**; stranded HOMEs are reclaimed without touching live runs.

**Phase 2 — after this, a loop of any cause is visible and bounded:**

- A run whose writes are failing reads `looping` with a truthful reason, not `slow`.
- A confirmed per-run loop is auto-stopped within **~2 minutes on the compliant path** (measured 60-78s), against 27 minutes observed and a 2-hour `RUN_TIMEOUT` worst case. *(Corrected post-implementation: a worker that IGNORES the stop verdict takes the escalation path, which is **120-150s** — over the target. There is no acknowledgement channel for a steering input, so the alternative to escalating was riding to `RUN_TIMEOUT` at 2h. Both figures are recorded rather than the flattering one.)*
- An **API-wide outage auto-stops nothing**, and a **run with no comparison set is flagged, not killed** — both proven by tests, because these are the failure modes worse than the bug.

## Risks

- **Auto-stop killing healthy runs.** The whole design risk, and it lives entirely in Phase 2. Mitigated by the guard conjunction, the comparison-set requirement, and making the negative tests the milestone's centre of gravity. **The phasing is itself the mitigation**: Phase 1 closes the incident without any kill path, so Phase 2 can be reviewed on its merits rather than under pressure to fix an open defect. If confidence is still low at that review, **ship M4 (flag only) and hold M5** — the flag alone turns this incident from silent into obvious.
- **Bisection amplifying load.** Isolating one poisoned message in a 239-message batch costs ~8 extra round-trips. Bounded, one-off per poison, and cheaper than the 2 Hz loop it replaces.
- **Sanitation hiding a real bug.** Silently dropping bytes means a future NUL-emitting tool is never investigated — hence the count-and-log requirement in M2.
- **In-process counters and restarts.** A restart does not "survive" the window — nothing is counted while the process is down, so the window restarts. That **delays** a kill (fail-safe) rather than causing a false positive; stated because the first draft's guard table implied the opposite.
- **The one-off HOME reclaim racing a live run.** Skip any non-terminal run's directory. A sweep that deletes a running job's HOME is a worse bug than the leak it fixes.
