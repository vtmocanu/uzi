# PRD #49: Worker resource stats — live CPU/memory per worker, portable across compose and k8s

**GitLab Issue**: [#49](https://gitlab.example.com/vtmocanu/uzi/-/issues/49)
**Status**: Draft — reviewed 2026-07-12 by 3 agents (design, security, fact-check);
all blocking/major findings folded in below (marked ↳review where the design
changed). Fact-check: 24/26 verified; the two misses (#40 file overlap, #47
migration-number wording) fixed.
**Priority**: Medium
**Created**: 2026-07-12
**Depends on**: PRD #4 (workers/heartbeat protocol) — done

## Problem

There is zero visibility into what a worker container is doing to its host. A worker
runs the Claude SDK CLI, git operations, and optional devbox provisioning — all
memory-hungry — on a laptop today and in a k8s pod later. When the SDK subprocess
balloons or the OOM killer takes it, the only symptom is a dead/requeued run;
nothing in the UI says "this worker was at 97% of its memory limit". Users sizing
`mem_limit`/`cpus` (compose) or requests/limits (k8s) are guessing, and with
per-worker concurrency arriving (PRD #42, `WORKER_MAX_CONCURRENT_RUNS`), a
fat-fingered cap is exactly the kind of mistake live gauges would catch before the
OOM killer does (#42 explicitly warn-logs above a soft ceiling for this reason).

User confirmed per-worker granularity is sufficient — no per-run attribution.

## Solution Overview

The worker **self-reports container-level CPU and memory on the existing 15s
heartbeat** (`WORKER_HEARTBEAT_INTERVAL`, `agent/src/config.ts:179`), read from its
own **cgroup v2 files** — the one mechanism that works identically under
docker-compose and kubelet, needs no docker socket, no metrics-server, and
preserves the worker's outbound-only trust posture. The API stores the latest
sample as columns on `workers`, exposes it in the worker DTOs, and the web renders
live gauges (CPU %, memory used / limit) on Settings → Workers and the Dashboard
worker cards.

- Sample = `{cpu_pct, mem_bytes, mem_limit_bytes | null, source}` computed per tick.
- CPU % from the `usage_usec` delta between consecutive heartbeats over the
  **measured monotonic elapsed time**, normalized by the cgroup quota (`cpu.max`)
  when set, else by host core count — so 100% always means "all the CPU this
  container may use".
- Latest-sample only; no time series in Postgres (history is Prometheus territory,
  out of scope).
- "Real time" = heartbeat cadence (15s) × UI poll (10s, the Dashboard's existing
  `usePollWhileVisible` rhythm, `web/src/pages/Dashboard.tsx:132`).

## Design Decisions

1. **Self-report from inside the container; never pull from outside.** The worker
   is outbound-only by hard invariant (schema comment,
   `migrations/00020_workers_runs.sql:3`; no inbound port, no cookies). Every
   pull-based alternative was rejected: mounting the docker socket into anything
   (compose-only, and a socket in a container that also runs a prompt-injected
   agent's Bash tool is a root-equivalent hole — see the compose hardening comments
   at `docker-compose.yml:131`); k8s metrics-server/cAdvisor (k8s-only, and the API
   has no network path or credentials to reach it); a metrics endpoint on the
   worker (breaks outbound-only). Reading `/sys/fs/cgroup` from inside works under
   both runtimes because both put every container in its own cgroup, and the
   container view of cgroupfs is namespaced to exactly that cgroup.

2. **cgroup v2 reader with a process-level fallback, and the sample says which.**
   New `agent/src/stats.ts` reads `memory.current`, `memory.stat`, `memory.max`,
   `cpu.stat` (`usage_usec`), `cpu.max` under `/sys/fs/cgroup`. All are
   world-readable; the non-root `uzi` user
   (`agent/templates/base/Dockerfile:23,132`) and the distinct-uid agent
   subprocess both live in the same container cgroup, so one read covers the
   worker *and* every SDK/git/devbox child — per-worker is exactly per-container.
   When the v2 files are absent (cgroup v1 host, or running un-containerized in
   dev), fall back to `process.memoryUsage().rss` + `process.cpuUsage()` deltas
   and mark `source: "process"` — honest but children-blind, and the UI labels it
   ("worker process only"). No cgroup v1 parser: Docker Desktop and current k8s
   default to v2; v1 gets the fallback.
   - **Reported memory = `memory.current − memory.stat:inactive_file`**
     (↳review, design major): `memory.current` counts reclaimable page cache,
     which git/build workloads pin near the limit while the kernel would evict it
     long before OOM — the raw number cries wolf. Subtracting `inactive_file` is
     exactly what `docker stats` shows, so the gauge matches operator intuition
     and is a real OOM-proximity signal.
   - **CPU % = Δ`usage_usec` ÷ measured monotonic elapsed µs ÷ allowed CPUs**
     (↳review, design major). Elapsed comes from `process.hrtime.bigint()`
     between samples — never the nominal 15s interval, which drifts with request
     latency and retry backoff (`agent/src/worker.ts:58-62`). Allowed CPUs =
     `quota/period` from `cpu.max` (format `"$MAX $PERIOD"`, e.g.
     `50000 100000` → 0.5 CPUs); `"max"` (no quota) → `os.cpus().length` (inside
     a container this is the host count — which *is* the ceiling when unquoted).
   - `memory.max` = `"max"` (no limit) → `mem_limit_bytes: null`; UI shows
     absolute usage without a percentage bar.
   - First tick after start has no CPU delta → omit `cpu_pct` on that tick. The
     previous `usage_usec`/hrtime pair lives in collector memory only; a worker
     restart just re-runs the first-tick omission.
   - **Namespace-root sanity check** (↳review, audit low; ↳impl correction
     2026-07-14, verified empirically on Docker 29.4.0 — private-ns and
     `cgroupns=host` both tested): with a private cgroup namespace (the modern
     Docker + kubelet default), `/proc/self/cgroup` reads `0::/` because the
     container's own cgroup IS the namespace root, and `/sys/fs/cgroup` is mounted
     at exactly that cgroup — so its files reflect the container. Use the cgroup
     source in that case. Under `cgroupns=host` (older runtimes / explicit config)
     the process sits at a *non-root* path (`0::/docker/<id>`, `0::/kubepods/…`)
     and `/sys/fs/cgroup` is an ancestor (the host root) whose numbers masquerade
     as the container's (`memory.max` reads `"max"` though the container is
     capped) — so a non-root `/proc/self/cgroup` path means "treat cgroup data as
     unavailable, use the `process` fallback" rather than report the host as the
     worker. (The originally-drafted wording had this inverted — it named `0::/`
     as the masquerade — which would have forced the process fallback for every
     normally-containerized worker and defeated Success Criteria 1–2.)

3. **Transport: optional `stats` field on the existing heartbeat — and the
   decode contract is spelled out, because the naive implementation bricks the
   fleet** (↳review — the design review's blocking finding). `HeartbeatRequest`
   (`agent/src/protocol.ts:84`) gains
   `stats?: {cpu_pct?, mem_bytes, mem_limit_bytes?, source}` — same
   absent-optional convention as `template` on register (`protocol.ts:74`). The
   server ignores the heartbeat body today ("No body",
   `api/internal/handler/worker_protocol.go:101-115`) — but **every current
   worker already sends `{"version": ...}`** (`agent/src/client.ts:76-79`), and
   `httpx.DecodeJSON` is strict (`DisallowUnknownFields`,
   `api/internal/httpx/respond.go:35`). A `struct{ Stats }`-only decode would
   therefore 400 every heartbeat from every worker, old and new; the loop only
   warns and retries (`worker.ts:59-61`), so within `WORKER_HEARTBEAT_STALE` the
   sweeper marks the whole fleet offline and requeues its runs. The decode
   contract is register's, exactly
   (`worker_protocol.go:54-68`):
   - the heartbeat struct **declares `version`** (ignored, like register's
     unread fields — see the test comment at `worker_protocol_test.go:88`);
   - empty body tolerated via the `io.EOF` check, same as register;
   - **`stats` is decoded defensively, separately from the outer decode**
     (↳review, audit major): declared as `json.RawMessage` and parsed in a
     second step whose failure *drops the stats and nothing else* — a literal
     `float64` field would abort the whole decode on `1e999`/int64-overflow
     *before* any validation runs, turning one malformed number into a
     self-DoS (worker marked stale, runs requeued). Liveness must never hinge
     on telemetry hygiene.
   Back-compat matrix: **new worker + old server** — extra body bytes ignored,
   harmless; **old worker + new server** — `{"version"}` decodes, stats absent,
   columns overwritten to NULL (Decision 4). No new endpoint, no new auth
   surface, no cadence change.

4. **Storage: nullable columns on `workers`, overwritten every heartbeat —
   including to NULL.** Migration (draft `00090` — clear of drafts held by open
   PRDs #41:00070, #42:00075, #46:00080–83, #45:00053 — the last, like #47's
   draft 00054, actually collides with the already-landed head
   (`00054_proposal_confirming.sql`) and those PRDs renumber at their own merge
   (↳review, fact-check); this PRD renumbers to the live head at merge per
   CLAUDE.md convention):
   `stats_cpu_pct real`, `stats_mem_bytes bigint`, `stats_mem_limit_bytes bigint`,
   `stats_source text` — all nullable, no new table. `HeartbeatWorker`
   (`queries/runtime.sql:48`) writes whatever the tick carried, nulls when it
   carried nothing — so a stats-capable worker that stops sending (downgrade,
   collector error) self-clears instead of pinning a stale gauge, and freshness is
   simply `last_heartbeat_at`, which the UI already has. Accepted consequence
   (↳review): one bad collector tick blanks the gauge for ~15s; the UI may hold
   the last value for one poll cycle to avoid flicker — implementer's choice.
   Alternative rejected: a samples table — nothing queries history;
   latest-sample columns are free.

5. **Stats are display-only and clamped at the door — never a scheduling input.**
   The worker is the least-trusted component; a hostile one can report anything.
   The server validates (post-decode, per Decision 3's two-step parse): `cpu_pct`
   finite, clamped to `[0, 100 × 64]`; `mem_bytes`/`mem_limit_bytes` non-negative
   int64; `source` ∈ `{cgroup, process}`; violations drop the whole `stats`
   object and the heartbeat still 200s. **On drop, log `worker_id` + a static
   reason only — never the raw values or `source` string** (attacker-controlled
   text until enum-validated; mirror `sanitizeSelfReported`,
   `worker_protocol.go:79-86`) (↳review, audit minor). Claim logic, run
   assignment, and the sweeper never read these columns — and that contract is
   **enforced, not prose** (↳review, audit major): the migration carries a SQL
   comment marking `stats_*` display-only, and an M2 regression test asserts no
   query in `runtime.sql` outside `HeartbeatWorker`/the list queries references
   `stats_`. Worker-side invariant, mirror-image: `stats.ts` is called only from
   the worker's own `heartbeatLoop` and never incorporates any agent-supplied
   value (the agent subprocess holds no join token and cannot forge a heartbeat;
   cgroup accounting files are kernel-maintained). Nothing here can leak secrets
   or repo content: the payload is four numbers and an enum.

6. **Display: gauges where workers already render; polling where it already
   exists.** `workerDTO` (`api/internal/handler/workers.go:53`) gains the four
   fields (nullable, `AdminWorker` inherits for the admin page free). Web:
   - `Settings → Workers` (`web/src/pages/WorkersSettings.tsx`) — currently
     mount-only load; gains the same 10s `usePollWhileVisible` the Dashboard uses,
     plus per-worker CPU bar and "used / limit" memory bar (percentage only when a
     limit is known; warn tone ≥ 80%, danger ≥ 95%). **Bar widths clamp at 100%**
     regardless of stored value (↳review — the server accepts up to 6400% CPU;
     the DOM must not render it).
   - Dashboard: a new compact "Worker load" fleet card (↳impl correction
     2026-07-14 — no per-worker tiles existed; `Dashboard.tsx` had only the
     aggregate "Workers online N/M" StatTile, and the mount fetch + existing 10s
     poll already fetch the fleet). One "name · cpu 34% · mem 2.1/4 GiB" line per
     worker that has reported a sample, dimmed when offline, hidden until any
     worker reports — the faithful "factory floor at a glance" realization.
   - `status: offline` (sweeper-marked) renders stats dimmed with the stale
     heartbeat age — last-known, clearly not live. `source: "process"` gets a
     tooltip: measures the worker process only.
   - Web mocks (`web/src/mocks/data.ts`, `mockApi.ts`) grow representative
     samples (limited, unlimited, offline, process-source).

7. **k8s needs zero changes by design — that's the point of Decision 1.** In a
   pod, `memory.max`/`cpu.max` reflect the container's limits, so the same gauges
   automatically show "% of pod limit". One stated conditional (↳review): the
   promise assumes a private cgroup namespace (the modern Docker/containerd and
   k8s default) — `cgroupns=host` setups hit Decision 2's root-cgroup check and
   degrade to `source: "process"`, documented. kubelet/cAdvisor Prometheus
   metrics remain available and complementary for cluster operators; in-app
   stats are the product-level view for users who don't have Grafana. Docs get
   sizing examples for both: compose `mem_limit`/`cpus` (none set today on the
   `docker-compose.yml` agent service — though PRD #42 plans to add them, see
   coordination below) and k8s `resources:` blocks, noting that setting a limit
   is what makes the percentage bar appear.

## Milestones

- [x] **M1 — Agent collector + payload**: `agent/src/stats.ts` (cgroup v2 reader,
  `inactive_file` subtraction, hrtime CPU delta math with `cpu.max` quota/period,
  root-cgroup check, process fallback, first-tick omission), wired into
  `heartbeatLoop` (`agent/src/worker.ts:55`) via `client.heartbeat()`
  (`agent/src/client.ts:76`); `protocol.ts` types; unit tests on fixture cgroup
  trees (limited, `max`, quota'd cpu incl. period parse, missing files →
  fallback, malformed → fallback, root-cgroup → fallback). A collector failure
  must never fail the heartbeat.
- [x] **M2 — API storage + protocol**: migration draft `00090` (with the
  display-only SQL comment) + sqlc regen, Decision 3 decode (declared `version`,
  EOF tolerance, two-step `json.RawMessage` stats parse) + Decision 5
  validation/clamping + static-reason drop logging, `HeartbeatWorker` query
  update (write-or-null), DTO fields; Go tests: **current-worker `{"version"}`
  body → 200** (↳review blocking — an empty-body test alone would mask the
  DisallowUnknownFields break; mirror `TestWorkerRegisterAcceptsNameField`,
  `worker_protocol_test.go:93`), empty body → 200, `1e999`/int64-overflow/NaN
  in stats → stats dropped but 200, garbage `source` dropped, null round-trip,
  admin DTO, no-`stats_`-in-scheduling-queries regression grep.
- [x] **M3 — Web gauges**: WorkersSettings poll + bars, Dashboard tile line,
  offline dimming, process-source tooltip, mock data; vitest coverage incl.
  no-limit (no bar) and offline (dimmed) rendering.
- [ ] **M4 — Docs + specs**: `docs/worker-setup` sizing section (compose + k8s
  limits, what the gauges mean, 15s/10s freshness caveat), `specs/ai.md` design
  record; `specs/human.md` addition proposed to user (per-worker live stats was a
  user-stated requirement).
- [ ] **M5 — E2E verification**: `./e2e/run-e2e.sh` worker (real worker loop, stub
  executor, Linux container → real cgroup v2) asserts the workers API returns
  populated stats after one heartbeat interval; smoke-check the UI renders them.

### Parallel execution plan

| Phase | Milestones | Depends on | Files touched | Notes |
|---|---|---|---|---|
| 1 (parallel) | M1 (agent) ∥ M2 (api) | wire contract frozen first (the `stats` JSON shape above) | `agent/src/{stats,worker,client,protocol}.ts`, `agent/test/` ∥ migration, `runtime.sql`, `worker_protocol.go`, `workers.go`, api tests | disjoint trees |
| 2 | M3 (web) | M2 DTO | `WorkersSettings.tsx`, `Dashboard.tsx`, `api.ts`, mocks | |
| 3 | M4, M5 | M1–M3 | docs, specs, e2e | M4 ∥ M5 |

Cross-PRD coordination (↳review — heavier than first drafted): **#42** overlaps
on `WorkersSettings.tsx`, `worker_protocol.go` (register handler),
`workers.go`/`api.ts` (workerDTO + Worker type), the `workers` list queries in
`runtime.sql`, a `workers` migration (draft 00075), *and* it plans to add
`cpus`/`mem_limit` to the compose agent service (#42 Decision 9) — exactly the
limits that make this PRD's percentage bars appear; whichever lands second
rebases and renumbers. **#40** (token usage) shares `Dashboard.tsx` and
`web/src/lib/api.ts` (both Phase-2 web files here — same land-second-rebases
rule), nothing server-side.

## Success Criteria

- A worker under load shows CPU/memory in Settings → Workers within one heartbeat
  (15s) + one poll (10s), on both docker-compose and a k8s pod, with **no
  environment-specific code paths** beyond the cgroup/process fallback.
- With `mem_limit` (compose) or a k8s memory limit set, the gauge shows a
  percentage of that limit; with no limit, absolute usage and no bar.
- Memory reported covers SDK subprocess spikes (container-level, not
  worker-process-level) when `source: "cgroup"`, and tracks `docker stats`
  (cache-excluded), not raw `memory.current`.
- A worker sending a malformed stats object (overflow number, NaN, junk `source`)
  keeps heartbeating with 200s — telemetry hygiene never costs liveness.
- An old worker against the new API heartbeats exactly as today (nulls, no gauge);
  a new worker against an old API heartbeats exactly as today (stats ignored).
- Garbage stats from a hostile worker are dropped without failing liveness, and
  nothing outside the DTO layer ever reads the columns.
- Offline workers show dimmed last-known stats with heartbeat age, never a
  live-looking gauge.

## Out of Scope

- **Per-run attribution** (user confirmed per-worker is enough; one cgroup can't
  split concurrent runs anyway, see PRD #42).
- **Time series / graphs / Prometheus exposition** — latest sample only; a future
  PRD can add an exporter or scrape kubelet metrics in k8s.
- **Alerting/thresholds acting on stats** (no auto-pause, no claim throttling —
  Decision 5 makes them display-only).
- **cgroup v1 parsing** — v1 hosts get the process fallback, labeled as such.
- **Host-level stats** (the API/web/db containers) — workers only.

## Decision Log

- 2026-07-12 — PRD created. Per-worker granularity confirmed by user. Core choice:
  self-reported cgroup v2 on the existing heartbeat, over docker-socket/
  metrics-server pulls (portability + outbound-only invariant, Decision 1).
- 2026-07-12 — Review round (3 agents). Design review's blocker (strict
  `DecodeJSON` + the already-sent `{"version"}` body would 400 every worker's
  heartbeat → fleet marked stale) became Decision 3's decode contract + the M2
  `{"version"}`-body test; its majors fixed the CPU% math (monotonic elapsed,
  `cpu.max` quota *and* period) and the memory number (`inactive_file`
  subtraction, docker-stats semantics). Security audit's majors: two-step
  `json.RawMessage` stats parse so overflow/NaN can't abort the outer decode
  (self-DoS), and making display-only enforceable (migration SQL comment +
  no-`stats_`-in-scheduling regression test); minors added drop-log hygiene and
  the root-cgroup/`cgroupns=host` fallback. Fact-check (24/26): #40 does share
  `Dashboard.tsx`/`api.ts` (coordination note fixed); #45/#47 draft numbers
  collide with the landed head and renumber at their own merges (wording fixed);
  minor line-ref precision fixes.
- 2026-07-14 — Impl correction (M1): Decision 2's root-cgroup sanity check was
  inverted. Verified empirically on Docker 29.4.0 that a private cgroup namespace
  (the Docker/kubelet default) reads `/proc/self/cgroup` = `0::/` **with a real
  `memory.max`** (the good, use-cgroup case), while `cgroupns=host` reads a non-root
  path (`0::/docker/<id>`) with `memory.max` = `"max"` (the host masquerade). The
  collector therefore uses the cgroup source at `0::/` and the process fallback on a
  non-root path — the inverse of the original bullet, which would have forced the
  process fallback for every normal worker. Bullet text above corrected; check +
  test isolated in `stats.ts` `cgroupIsNamespaceRoot` (endorsed by the lead).
- 2026-07-14 — Impl correction (M3): Decision 6's "Dashboard worker tiles / worker
  cards" did not exist — `Dashboard.tsx` rendered only the aggregate "Workers online
  N/M" StatTile (no per-worker tiles), so there was nothing to hang the compact stats
  line on. Lead ruled option (a): added a new compact "Worker load" fleet card
  (one `WorkerStatLine` per worker with a sample, offline-dimmed, hidden until any
  worker reports), the faithful realization of the "factory floor at a glance" intent.
  Decision 6 Dashboard bullet corrected above. Flicker-hold (Decision 4, implementer's
  choice): NOT implemented — a bad tick blanks the gauge for one poll cycle, the PRD's
  documented accepted consequence; chosen for simplicity and to avoid a stale value
  looking live.
