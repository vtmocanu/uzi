# PRD #108: Poison-pill message batches — classify permanent failures, auto-stop confirmed retry loops, and stop leaking run HOMEs

**GitLab Issue**: [#108](https://gitlab.example.com/vtmocanu/uzi/-/issues/108)
**Status**: Draft (created 2026-07-21; **revised the same day after a fable adversarial review that verified every citation against the code** — three findings changed the design rather than the wording, and four claims in the first draft were measured FALSE; each is corrected in place with the correction left visible).
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
- **The sibling TEXT columns, not just the payload.** `agent`, `agent_instance`, `agent_label` are worker-controlled and inserted verbatim (`service.go:1083-1085`; `truncateRunes` truncates but strips nothing). Postgres `text` cannot hold NUL any more than `jsonb` can, so a NUL in `agent_label` wedges a run identically and entirely outside a payload-only fix.

Still **do not** broaden to the whole control class: `\n`, `\t` and ANSI escapes are legal in `jsonb` and load-bearing in tool output — stripping `\n` would mangle every log the UI renders. `sanitizeMemoryField` strips the wider class because its sink is a **terminal table printer**; this payload lands in a React renderer that escapes it. Different sink, different rule.

**Implementation caution (review finding, easy to get backwards): the NUL arrives as the six-byte escape `\u0000` inside otherwise-valid JSON** — a raw `0x00` byte is already invalid JSON and 400s today. The strip must therefore be **JSON-aware**: a naive byte-substring replace corrupts any payload containing the literal text `\\u0000`.

### 2. Make the status code an honest retry contract

Return **400** (joining `ErrInvalidMessage`) for a store rejection that can never succeed, via a new `workersvc.ErrUnstorableMessage`. Map the **enumerated** SQLSTATEs — **22P05** (unsupported escape), **22P02** (invalid text representation), **22021** (invalid byte sequence) — not a vague "and its neighbours". A 500 must mean "try again"; anything else is a lie the client is obliged to believe.

**This is the load-bearing fix.** With it, incomplete sanitation degrades to "that batch is rejected and the run fails with a clear reason" instead of "the run wedges silently".

### 3. Worker-side circuit breaker — and a bounded batch

In `batcher.ts`: bounded exponential backoff instead of a fixed cadence; a breaker that trips after `N` consecutive failures of the *same* batch; on trip, stop retrying and fail the run loudly.

**"Never retry a 4xx" is NOT safe on its own** and must ship with the batch bound (review's blocking finding): because the batch grows, a healthy run riding out a transient outage can cross the 1 MiB cap and take a permanent 400 it did not earn. So M3 must also:

- cap batch **bytes** and **split** oversized batches rather than posting one giant body;
- on a permanent rejection, **bisect** to isolate the individual poisoned message, drop only that one, and let the rest land — preserving the run instead of failing it;
- treat "400 because the body was too large" as a **split-and-retry** signal, never as a poison verdict.

**Two ordering traps to design around.** The breaker's explanatory message must **not** travel through the batcher — order-preserving `concat` (`batcher.ts:114`) queues it behind the poison, where it never lands; route it through `reportState`'s separate endpoint (`client.ts:142-158`), which already has bounded retries and 4xx-fatal semantics. And everything buffered behind the poison is **dropped** — a permanent truncation of run history that this PRD owns as an explicit decision rather than leaving implicit in the incident's `dropped: 239`. Bisection (above) is what keeps that loss to one message instead of hundreds.

### 4. Server-side auto-stop for a CONFIRMED loop — guards, and an honest scope

The worker fix does not protect a fleet on an older image, so the authoritative stop is server-side. The API counts consecutive `AppendMessages` failures per run (in-process, mirroring `usagepoller`'s backoff map). A run is auto-failed with reason `message_persist_permanent` **only** when every one of these holds:

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
3. **Permanent failures return 400, from an enumerated SQLSTATE set.** The status code is the retry contract; a permanent failure returned as 500 is what converts one bad payload into an unbounded loop. This closes the *class*, which is why the PRD is not titled "strip NUL bytes".
4. **Bound the batch before making 4xx fatal.** Shipping "never retry a 4xx" without the byte cap and bisection would convert transient outages into failed healthy runs — a strictly worse bug than the one being fixed.
5. **Auto-stop requires a comparison set; with none, flag and notify.** A rule that cannot distinguish "this run is poisoned" from "the database is down" must not kill runs.
6. **Auto-stop reuses the stop-verdict input path** (proven in the incident) with the server-side transition as fallback — and accepts the migration that a distinguishable `stop_kind` requires.
7. **The api hard singleton is a load-bearing assumption of M4/M5.** `deploy/chart/values.yaml:48-52` documents three reasons the api cannot run >1 replica. The in-process streak counters and the "other runs succeeding on this instance" comparison set are a **fourth**: split traffic never reaches `N` and the comparison set becomes per-pod noise. M5 adds that fourth reason to the values.yaml comment in the same MR, so a future HA effort cannot silently invalidate the guard.
8. **Auto-stop is NOT subordinate to the health toggle.** `detectRunHealth` no-ops when `HealthEnabled` is false (`health.go:95-106`). The `looping` flag may ride that toggle; the availability fix must not, or an admin disabling health silently disables loop protection.
9. **Do not weaken `stalled`'s in-flight suppression.** It is correct — a long build genuinely looks like silence. The wedge is caught by a new, independent signal instead.
10. **No metrics endpoint exists** (no `promhttp` or `/metrics` anywhere in the api). Alerting is out of scope; M7 records the gap and the log lines to key on rather than pretending a dashboard is one milestone away.

## Milestones

- [ ] **M1 — Reproduce, RED first.** Integration test POSTing a payload carrying the `\u0000` escape (assert today's 500); a lone-surrogate case; an oversized-batch case crossing the 1 MiB cap; a runner test with a `0555` directory under a fake HOME. **Run the message test against v0.9.0 and v0.10.0** and record which — the incident write-up guessed and was wrong once.
- [ ] **M2 — Sanitation + honest status code (api).** Strip `\u0000` and unpaired surrogates, JSON-aware, in the payload and in `agent`/`agent_instance`/`agent_label`; add `ErrUnstorableMessage` mapped to 400 for SQLSTATEs 22P05/22P02/22021. Count and log every strip so a NUL-emitting tool stays visible rather than silently laundered.
- [ ] **M3 — Bounded batch, backoff, breaker (agent).** Byte cap + splitting; bisection to isolate one poisoned message; exponential backoff; 4xx not retried **except** the oversize case, which splits; breaker after N identical-batch failures; the failure report routed through `reportState`, never the batcher.
- [ ] **M4 — Server-side loop detection → `looping` health.** Per-run consecutive-failure counter with **eviction on terminal state**; flag `looping` with a reason naming the persistence failure.
- [ ] **M5 — Auto-stop, guarded.** Stop-verdict input + no-poller fallback + the `stop_kind` migration + the values.yaml singleton note. **The tests are mostly negative**: an API-wide outage must auto-fail nothing; a single failing run beside healthy neighbours must; **no comparison set must flag and not kill**; one success resets the streak; a wedged **chat** run (`chat-runner.ts:128` builds the same batcher against the same endpoint) is covered by the same rule.
- [ ] **M6 — HOME cleanup that survives read-only directories (agent).** Permission-restoring removal, still best-effort, tested against a `0555` directory. Plus a one-off reclaim that **skips any `agent-home/<runId>` whose run is non-terminal** — a sweep racing a live run is worse than the leak.
- [ ] **M7 — Observability + the honest gap.** Structured fields on the auto-stop decision (run id, streak, window, comparison-set size, decision) so an operator can reconstruct why a run died. Record that no metrics surface exists and name the log lines; leave a metrics endpoint to its own PRD.
- [ ] **M8 — Audit the other unbounded retry loops.** Not unique to this batcher: sweep for retry loops with no bound, no backoff, or no permanent-vs-transient classification (forge sync, judge dispatch, notification delivery, `uzicli` polling). **One file at a time with the call sites open** — a repo-wide table written in one sitting is an audit-shaped artifact nobody verified.
- [ ] **M9 — CLI + docs.** Per CLAUDE.md's second-consumer rule, `api/cmd/uzi/` must render the new health reason and the `message_persist_permanent` failure reason. `ARCHITECTURE.md` gains the auto-stop rule and its guards; `docs/` gains the operator-facing "why was my run auto-failed"; `specs/ai.md` records the decisions.

## Success Criteria

- A tool result containing NUL bytes or a lone surrogate is **persisted sanitized** and the run continues; nothing 500s.
- A genuinely unstorable payload returns **400**, is **not** retried, and costs **one message** (bisected out), not the run.
- A chatty run that rides out a transient outage **never** takes a permanent 400 from batch growth.
- A confirmed per-run loop is auto-stopped within **~2 minutes**, against 27 minutes observed and a 2-hour `RUN_TIMEOUT` worst case.
- An **API-wide outage auto-stops nothing**, and a **run with no comparison set is flagged, not killed** — both proven by tests, because these are the failure modes worse than the bug.
- A run whose writes are failing reads `looping` with a truthful reason, not `slow`.
- A Go-touching run leaves **no HOME behind**; stranded HOMEs are reclaimed without touching live runs.

## Risks

- **Auto-stop killing healthy runs.** The whole design risk. Mitigated by the five-guard conjunction, the comparison-set requirement, and making the negative tests the milestone's centre of gravity. If confidence is low at review, **ship M4 (flag only) and hold M5** — the flag alone turns this incident from silent into obvious, and M2+M3 already close the incident.
- **Bisection amplifying load.** Isolating one poisoned message in a 239-message batch costs ~8 extra round-trips. Bounded, one-off per poison, and cheaper than the 2 Hz loop it replaces.
- **Sanitation hiding a real bug.** Silently dropping bytes means a future NUL-emitting tool is never investigated — hence the count-and-log requirement in M2.
- **In-process counters and restarts.** A restart does not "survive" the window — nothing is counted while the process is down, so the window restarts. That **delays** a kill (fail-safe) rather than causing a false positive; stated because the first draft's guard table implied the opposite.
- **The one-off HOME reclaim racing a live run.** Skip any non-terminal run's directory. A sweep that deletes a running job's HOME is a worse bug than the leak it fixes.
