# PRD #1809: Worker disk safety for long runs

**Issue**: #1809
**Status**: In progress (M2, M4, M5, M6 landed 2026-09-28; M1 and M3 await the real uid-split fixture run; M7 awaits the maintainer's hosted-k8s acceptance)
**Priority**: High
**Created**: 2026-09-27

## Problem

A Claude run on a hosted worker gets a private HOME at `/data/agent-home/<run>` on the worker's data volume (the data PVC, 25 GiB on the `l` preset). `GOCACHE`, `GOMODCACHE` and the npm cache are not set for Claude runs, so Go and npm write their defaults under that HOME. Nothing bounds them while the run lives, and when the run parks the whole HOME is kept for the resume (`preserveResumeArtifacts`, `agent/src/runner.ts` ~2405-2431). A uzi-on-uzi run builds and tests the Go modules many times (`-race`, cover, several packages), so its cache grows by tens of GB. Go's own five-day trim does not constrain this growth.

Measured on 2026-09-27 on two hosted Docker-lane workers:

| run | worker | HOME | `.cache/go-build` | `go/pkg/mod` |
|---|---|---|---|---|
| issue #1798 run | worker A (volume at 100%) | 24G | 21G | 1.9G |
| issue #1795 run | worker B (volume at 93%) | 17G | 15G | 1.9G |
| issue #1783 run | worker B | 4.9G | 2.8G | 1.9G |

Clearing the idle Go build caches by hand took worker B from 93% to 19%.

What happened to the #1798 run (worker log, UTC):

- 11:06: time-based checkpoint published to origin.
- 11:15 onward: `fetch-back on interruption failed … No space left on device` on every tick.
- 11:18: usage-limit park. `recovery: failed to pin source head … ENOSPC`; the checkpoint overlay could not fetch `main` and shipped `realTip`. The park completed and reported `park checkpoint published to origin`. The HOME was kept for the resume.
- 13:02: resume. `git fetch --prune origin` → `fatal: unable to write loose object file: No space left on device` → the run **failed**. The outbox could not replenish its reserve (ENOSPC).
- After the failure the terminal cleanup removed the HOME; the volume showed 127M used three minutes later.

(The api then also deleted the run's checkpoint ref from origin; that is PRD #1810.)

Nothing else acted in time:

- The worker's startup sweep (`agent/src/home-reclaim.ts`) runs once per boot and only removes terminal runs' HOMEs.
- The PRD #837 recycle (api threshold `UZI_DISK_PRESSURE_THRESHOLD`, default 0.90, two heartbeats) is deferred while the worker is busy, parked runs included, and skipped for a custody-held worker (`controller/internal/kube/materializer.go` ~616, ~639, ~770). It deletes whole volumes and is a last resort, not a reclaim step.
- `/data/provision/<run>` is removed with a plain `fs.rm(...).catch(() => undefined)` (`agent/src/sdk-executor.ts` ~882, `agent/src/executor.ts` ~1174, `agent/src/provision-run.ts` ~111 and ~131), not the uid-aware `rmHomeTree`; leftovers for completed, failed and cancelled runs were present on both workers.

## Goal

With the controls this PRD adds enabled (the default), a long or parked run cannot fill its worker's data volume with rebuildable caches. If the volume does fill, the run waits and retries within a bound instead of failing, with the cause stated.

## Scope

In:

- Claude runs on hosted and self-hosted workers (Docker lane and plain), their per-run HOME caches, and the data volume.
- ENOSPC and inode exhaustion on the data volume during a run, a park and a resume.
- A non-destructive worker reclaim step, and the provision-dir leak.
- Temporary directories that uzi's own agent test suites leave in the runner scratch dir.

Out:

- A shared, writable, worker-wide Go/npm cache (D1).
- Codex runs' caches: already per-run on the `codex-cmd-cache` emptyDir and reaped at run end (`agent/src/codex/codex-executor.ts` ~3777, `controller/internal/kube/render.go` ~255); #1598 tracks their measurement. The cache drop in D2 applies to Claude runs only.
- Checkpoint-ref retention (PRD #1810), #1790, Docker image and `dind-data` pressure (#225, #1759, #1760), `/nix` GC (#79).

## Decision log

**D1. Keep caches per run; bound them rather than share them.** Runs on one worker can execute different repositories' untrusted code, and a shared writable build cache would let one run plant entries another run's build consumes. `GOFLAGS=-modcacherw` fixes directory permissions, not that boundary. Each run keeps its own caches under its HOME, and uzi bounds them (D2, D3). A shared cache is a possible later optimisation that needs its own trust decision.

**D2. A park that ends the run's process drops a named list of rebuildable caches.** The deletion list is explicit: `.cache/go-build`, `go/pkg/mod`, `.npm/_cacache` under the run's HOME (the plan may add a cache only with the code evidence that it is rebuildable). Everything else in the HOME is kept, including unknown files: the session transcript `.claude/projects/*/<sid>.jsonl` and its `<sid>/` subagent dir (`agent/src/sdk-session.ts`), `.claude.json`, `.claude/todos`, `.claude/shell-snapshots`, the memory dir (`agent/src/memory-tools.ts`), and `go/bin`. The sibling skills plugin dir keeps today's preservation rule; it is rebuilt from the claim on every claim including resume (`agent/src/skills-plugin.ts` ~45), so it is not resume-critical.

The drop runs only on park paths where the run's executor has returned and no process of the run remains (usage-limit, owner pause, recovery parks). A run parked at a gate (`awaiting_approval`, `awaiting_input`, `awaiting_followup`) keeps a live executor (`runner.ts` ~2295) and is **not** eligible. It applies to Claude runs only.

**D3. A uid-aware subtree purge.** `rmHomeTree` refuses a root the worker does not own (`agent/src/rmtree.ts` `openRootToRunnerGroup`), and under the uid split the cache dirs are owned by `runner`/`runner-cmd`, some `0700` or `0555`. The plan adds a helper that removes a named subtree inside a HOME by running the existing agent-uid purge script as the owning uid, never following symlinks, and leaving the HOME root and its siblings intact. D6 measures cache sizes through the same agent-uid path, since the worker cannot `du` runner-private dirs.

**D4. Bounded growth while running.** Two layers, because one long build inside a single turn can fill the volume before any turn boundary.

- **Soft layer, the cap.** Each running run has a cache cap: `UZI_RUN_CACHE_CAP_FRACTION` of the data volume divided by the worker's max concurrent runs (default 0.5, so 6.25G per run on a 25G volume with two runs). When a run's caches exceed the cap, the runner trims them at a proven quiet point only: between turns, with no background process of the run alive (checked against the runner's own process tracking, not file mtimes). If the run stays over the cap and no quiet point comes, the runner parks it at its next turn boundary through a process-ending park (D2 then drops its caches) and requeues it with affinity.
- **Hard layer, mid-turn pressure stop.** The worker samples the data volume on every stats tick (the heartbeat cadence) independently of turn boundaries. At or above a hard threshold (the api recycle threshold minus `UZI_DISK_HARD_MARGIN`, default 0.03, so 87% by default) it stops the run with the largest cache bytes through the existing quiesce-and-park path an owner `uzi run pause --now` uses: interrupt the turn, reap the run's process tree, checkpoint what is committed, park. D2 then drops its caches and the D7 reclaim runs. The park carries the `data_volume_full` cause (D6), so it counts toward the same lifetime cap. This is what makes the Goal hold within a turn; a stop loses at most the in-flight step, as a pause does.

Both layers can be disabled; the Goal's guarantee then does not hold, and D6's bounded disk-full handling is what remains.

**D5. Admission and reclaim thresholds come from the api.** The api sends the worker its recycle threshold (`UZI_DISK_PRESSURE_THRESHOLD`) on the claim or heartbeat response. The worker's soft threshold is that value minus a margin (default 0.10, so 80% by default). Above the soft threshold the worker runs the D7 reclaim and claims no new run until it is back under.

**D6. A full data volume is a recoverable, bounded condition.**

- **Attribution.** A write failure counts as data-volume disk-full only when the failing operation's destination is on the data volume (the runner knows which path each operation writes: the bare, the outbox, the HOME) and a `statfs` of that volume shows bytes or inodes below a floor. The heartbeat's `statfs` gains inode counts (`files`, `ffree`; `agent/src/stats.ts` ~163 reads only blocks today). A `statfs` failure, or a filesystem without inode accounting, is "unknown" and keeps today's handling. Node `ENOSPC`, git's `No space left on device` / `unable to write` diagnostics and `EDQUOT` are the recognised signals.
- **Handling.** Run the D7 reclaim, retry the operation once, and if still full park in `recovery_wait` with a new typed cause `data_volume_full`. Follow the newest precedent: migration `00257_recovery_wait_vault_locked.sql` and ADR-1766 for the cause, and migration 00232's `forge_park_count` for a lifetime counter (a new `disk_park_count`), because `recovery_wait_count` only shapes the backoff and has no cap. Past `UZI_RUN_DISK_PARK_MAX` (default 3) the run fails with a new `fail_origin` value naming the cause.
- **Mirrors.** The cause needs every mirror: the DB check constraint, `recoveryWaitCauses` in `api/internal/workersvc/forgepark.go` and its vocab test, the worker-protocol feature gate (an older api 400s an unknown cause; see `recovery_cause_vault_locked` in `api/internal/workersvc/worker_protocol.go` ~399 and `agent/src/protocol.ts` ~2161), `agent/src/client.ts`, `api/internal/apitypes/run.go`, `failorigin.go` with `runs_fail_origin_check` and its vocab test, `api/cmd/uzi/run_render.go`, the `tui_*` renderers, and web `RunView.tsx`, `failOriginLabel.ts`, `apiTypes.ts`. It is a recovery-wait cause, not a run kind.
- **Preflight.** A claim and a resume check the data volume first and apply the same handling. Free-space checks cannot remove races, so the typed handling is the guarantee and the preflight an optimisation.
- **Outbox cannot write.** The park is reported through the claim-fenced park call and reconciled from the api's acknowledgement; the worker keeps local custody until the api confirms the park. A heartbeat alone is not assumed to carry the transition.
- **Recycle interaction.** A `data_volume_full` park still holds custody and counts as busy, so the PRD #837 recycle stays deferred; the D7 reclaim is the only relief, and past the cap the run fails with its cause stated rather than waiting forever.

**D7. Non-destructive reclaim before the recycle.** The worker runs a reclaim pass periodically and whenever its data volume crosses the D5 soft threshold. It removes only what it can prove safe, coordinating with the runner's registry of active runs and holding a per-run lock so a resume cannot start while it deletes:

- terminal runs' HOMEs and provision dirs (status confirmed with the api, as the startup sweep does);
- D2 caches of runs parked with no live executor;
- model-pass dirs (`uzi-summary-*` and siblings) whose owning process is proven stopped, never by name prefix alone.

The PRD #837 recycle stays unchanged as the last resort. On the Docker lane the runner scratch dir is an emptyDir separate from the data volume; on a plain worker it is on the data volume. The reclaim covers each where it lives; cleaning the emptyDir relieves node ephemeral pressure, not the data volume.

**D8. Truthful durability reporting and per-run size.** "Checkpoint published" keeps meaning exactly that a checkpoint ref was published. The park report also says whether that checkpoint contains the run's latest committed work (it does not when the recovery pin or fetch-back failed), and custody is not released while the only copy of the latest work is on the worker (as today). The worker reports each live or parked run's HOME and cache bytes in its heartbeat; the api persists them beside the existing worker disk stats (migration, following `00191_worker_disk_stats.sql`), and `uzi worker list`, the admin health view and the run page show them.

### Implementation notes (2026-09-28)

Where the branch diverges from, or pins down, the decisions above. The lasting parts are in `adr/1809-per-run-cache-bounds.md`.

- **D4.** The soft-cap park is UNCOUNTED: it sends `disk_park_preventive: true` and does not bump `disk_park_count` (human plan-review decision); the hard stop and disk-full parks are counted. "Stays over" is 3 consecutive over-cap turn boundaries, or 1.25x the cap while a live process blocks the trim. The in-run trim is LRU down to `UZI_RUN_CACHE_LOW_WATER` (default 0.6) of the cap, not a wipe (human decision); `go/pkg/mod` goes whole only while still over the cap. The quiet point is not "the runner's own process tracking": it is no live CLI process group AND no process attributed to the run by environ `HOME` or cwd (a bounded agent-uid scan of the process table; an unreadable process counts only when stat-linked to the run). Residual: a process that changes its HOME and leaves the run's dirs is invisible. The hard stop only picks runs inside their implement loop (never clone, plan, plan gate or finalize). Switches: `UZI_RUN_CACHE_CAP_ENABLED`, `UZI_DISK_HARD_STOP_ENABLED`.
- **D5.** The admission stop has its own switch (`UZI_DISK_ADMISSION`) and is bounded by `UZI_DISK_ADMISSION_MAX_WAIT` (default 15m) once a reclaim has run and could not relieve the volume, because the run lane cannot restrict a claim to this worker's own parked runs. Soft margin `UZI_DISK_SOFT_MARGIN` (0.10); the threshold arrives on the heartbeat response.
- **D6.** A pre-clone `data_volume_full` park releases the claim's custody hold (only on an exact-release proof), so it no longer defers the #837 recycle; mid-run (M4) disk parks keep custody, as D6 said. A filesystem without inode accounting is "unknown". The worker-protocol feature gate lives in `api/internal/handler/worker_protocol.go`, not `api/internal/workersvc/`.
- **D7.** Legacy runner-owned provision dirs (re-owned by the entrypoint's pre-split migration) are out of the reclaim's reach: it acts only on worker-owned dirs, through the descriptor-pinned removal. Reclaim directory listings are streamed, capped, and rotate across passes. The reclaim is also switchable (`UZI_DISK_RECLAIM`, cadence `UZI_DISK_RECLAIM_INTERVAL`, default 10m).
- **D8.** `checkpoint_contains_latest` tracks the fetch-back (the bare tracking ref equals the clone HEAD). A recovery-pin-only failure still reports true: the pin does not change what the published checkpoint holds. The usage-limit park now publishes its checkpoint before the `limit_wait` report (it used to publish after), so the report can carry the field.

## Milestones

| Phase | Milestone | Depends on | Main files |
|---|---|---|---|
| 1 | M1 subtree purge and park cache drop | none | `agent/src/rmtree.ts`, `agent/src/runner.ts` |
| 1 | M2 agent test temp hygiene | none | `agent/test/` helpers named below, `Taskfile.yml` |
| 2 | M3 worker reclaim, admission, provision cleanup | M1 | `agent/src/home-reclaim.ts`, `agent/src/main.ts`, provision rm sites, `api/internal/workersvc/` (threshold delivery) |
| 3 | M5 disk-full classification and recovery park | M3 | `agent/src/`, `agent/src/stats.ts`, migration, `api/internal/workersvc/`, the D6 mirror set |
| 4 | M4 in-run cache cap and mid-turn pressure stop | M1, M5 (the `data_volume_full` cause) | `agent/src/runner.ts`, `agent/src/sdk-executor.ts`, `agent/src/stats.ts` |
| 4 | M6 durability reporting and per-run size | M5 | `agent/src/protocol.ts`, migration, `api/internal/healthsvc/`, `api/cmd/uzi/`, `web/src/` |
| 5 | M7 docs, spec, ADR, acceptance | all | `docs/`, `specs/human.md`, new ADR |

M4 and M6 are both phase 4 and both touch the runner and `stats.ts`; they run in parallel only if M4 owns the per-turn and per-tick hooks and M6 owns the protocol and heartbeat fields, otherwise sequentially.

Every milestone passes its component gates (`task gate:agent`, `task gate:api`, `task gate:web` as touched) and `task check-docs:web` when it touches docs or PRDs. Migrations are numbered at merge time (`task migration:renumber`).

**Constraints for the uzi run:**

- Neither implementation nor validation creates or modifies a file under `.github/workflows/`. Before finalize, `git diff --name-only <base>..HEAD` shows no entry there.
- While working on this repo, keep your own build cache small: run `go clean -cache` at each milestone boundary and before each full `task gate:api`, and check `df -h /data` before a gate. The worker you run on does not have this PRD's fix yet.
- Tests that need a full disk simulate it through a seam (an injected `ENOSPC`/`EDQUOT` error or a fake `statfs`), never by filling a real volume.

- [ ] **M1: Subtree purge and park cache drop (D2, D3).** The uid-aware subtree purge helper, and the cache drop on process-ending park paths for Claude runs. Regression test (fails on current main): after a usage-limit park the preserved HOME has no `.cache/go-build`, `go/pkg/mod` or `.npm/_cacache`, with the exact uid-split ownership (`runner`-owned `0555` module cache under a worker-owned HOME). Controls: a resumed run continues its actual session (the transcript is read back, not merely present); unknown files and `go/bin` survive; a gate-parked run and a Codex run are untouched; a terminal run is unchanged.
  - Partial (2026-09-28): `rmHomeSubtree` (descriptor-pinned, agent-uid passes) and the park cache drop landed, with tests (`agent/test/runner-park-cache-drop.test.ts`, `rmtree.test.ts`, `rmtree-pinned.test.ts`); the uid-split ownership is simulated in unit tests only. The opt-in `task test:home-uid-split` fixture (`e2e/home-uid-split/fixture.test.ts`) was extended but not executed; tick after it passes.
- [x] **M2: Agent test temp hygiene.** These helpers leave directories in the runner scratch dir: `agent/test/runner-completion-1220-regression.test.ts` (~66, `uzi-1220-wt-*`), `agent/test/runner-completion-attempt.test.ts` (~24, `uzi-completion-wt-*`), `agent/test/checkpoint-followup-drain.test.ts` (~30, `uzi-ckpt-followup-wt-*`), `agent/test/runner-harness.ts` (~54, `uzi-runnerhome-*`), `agent/test/codex-reap-fixture.ts` (~161, `uzi-m2-recov-*`), `agent/test/js-deps.test.ts` (~24, ~406, ~537, `js-deps-*`). The `*-wt-*` helpers return a worktree path, and the runner creates the sibling skills plugin dir `.uzi-skills-<name>` that the test does not remove. Confirm each attribution first, then fix each test. Standing guard: a `task gate:agent` step runs the agent suite with a fresh `TMPDIR` and fails if anything remains in it; show it red on current main before the fixes.
- [ ] **M3: Worker reclaim, admission, provision cleanup (D5, D7).** Threshold delivery from the api, the periodic and soft-threshold reclaim with the per-run lock, admission stop above the soft threshold, provision dirs removed with the uid-aware path. Regression test (fails on current main): a permission-denied fixture shows a terminal run's provision dir removed under the uid split. Tests: over the soft threshold with a process-ended parked run present, the pass frees its caches and terminal leftovers and leaves its resume artifacts; a live run and a gate-parked run are never touched; a resume waits for an in-progress reclaim; no new claim above the threshold; the PRD #837 recycle still fires when reclaim cannot relieve the volume.
  - Partial (2026-09-28): threshold delivery, the periodic and soft-threshold reclaim (`agent/src/disk-reclaim.ts`, `run-disk-locks.ts`), the bounded admission stop and pinned provision-dir removal landed, with tests (`worker-disk-admission.test.ts`, `provision-run.test.ts`, `api/internal/handler/worker_heartbeat_disk_threshold_test.go`). The permission-denied regression under the real uid split is simulated in unit tests only; the extended `task test:home-uid-split` fixture was not executed. Tick after it passes.
- [x] **M4: In-run cache cap and mid-turn pressure stop (D4).** Cap computation, trim at a proven quiet point, park at the next turn boundary when the run stays over the cap; the per-tick hard-threshold stop through the pause-now quiesce path. Tests: a cache over the cap is trimmed between turns with no live process; a live background process blocks the trim; a run that stays over the cap parks and its caches are dropped; a simulated volume crossing the hard threshold mid-turn stops the largest run, reaps its process tree and parks it with the `data_volume_full` cause; both layers can be disabled.
- [x] **M5: Disk-full classification and recovery park (D6).** Attribution by destination mount with inode data, reclaim-retry, the `data_volume_full` park with its lifetime counter and cap, the new `fail_origin`, the full mirror set, preflight at claim and resume, the outbox-cannot-write path. Regression test (fails on current main): a resume whose first fetch hits a simulated ENOSPC with a confirmed full data volume parks instead of failing. Tests: ENOSPC on another mount keeps today's handling; a `statfs` failure is unknown; inode exhaustion classifies like bytes; the cap produces the failure with its `fail_origin`; an older api's 400 on the new cause is handled by the feature gate; live-DB tests for the counter and constraint.
  - Landed (2026-09-28) with one divergence: the outbox-cannot-write case only tags its reserve warnings with `cause: data_volume_full`; no park is needed there because park and state reports go straight to the claim-fenced `POST /runs/{id}/state`, never through the outbox (see the D6 implementation note). Live-DB tests ran against a throwaway Postgres.
- [x] **M6: Durability reporting and per-run size (D8).** The park report states whether the published checkpoint contains the latest work; per-run HOME and cache bytes flow through the heartbeat, are persisted and are shown. Tests: a park with a failed recovery pin reports the checkpoint as not containing the latest work and keeps its custody hold; protocol round-trip; live-DB test for the persisted bytes; a health check fires above a threshold; CLI and web render tests.
  - Landed (2026-09-28) with one divergence: `checkpoint_contains_latest` tracks the fetch-back, so a recovery-pin-only failure reports `true` (the pin does not change what the checkpoint holds); the #1798 shape, where the fetch-back failed, reports `false` and keeps custody. Live-DB tests ran against a throwaway Postgres.
- [ ] **M7: Docs, spec, ADR, acceptance.** Update the worker-disk operator docs, `docs/run-recovery-wait.md` (the new cause) and `docs/cli.md` (the new cause and the `uzi worker list` column), then `task docs:sync`; a terse `specs/human.md` requirement with its `(AI-synced YYYY-MM-DD)` tag; an ADR for D1-D3 (per-run cache isolation, the named cache list, drops only on process-ended parks). Write the ADR path un-backticked in PRDs until it exists (it now exists). Maintainer-owned live acceptance on hosted k8s: a Go-heavy run that parks on a usage limit resumes with its caches dropped and the volume well below the threshold; a simulated full volume parks and resumes.
  - Partial (2026-09-28): `specs/human.md` Feature #1809 and `adr/1809-per-run-cache-bounds.md` landed; the operator docs landed too (`docs/worker-setup.md`, `docs/run-recovery-wait.md`, `docs/cli.md`, `docs/configuration.md`, `docs/admin-health.md`, synced with `task docs:sync`). The maintainer-owned live acceptance on hosted k8s is not done.

## Success criteria

- With the controls enabled, no run fails because rebuildable caches filled the data volume.
- A process-ended parked run's HOME holds no rebuildable caches.
- A disk-full condition produces a bounded wait with a stated cause, never a silent failure.
- The largest per-run HOME on each worker is visible before it fills a volume.

## Risks

- **Deleting state a resume needs.** Mitigation: D2 deletes only a named list of cache trees and keeps everything else; M1 tests a real session continuation.
- **Deleting a cache under a live process.** Mitigation: drops only on process-ended parks and at proven quiet points; gate-parked runs are excluded; the reclaim holds a per-run lock against a resume.
- **Misclassifying a failure as disk-full.** Mitigation: destination-mount attribution plus `statfs` confirmation; unknown keeps today's handling; the park is capped.
- **Cold builds after each park or trim.** Accepted cost: minutes of rebuild, against a failed run.

## Out of scope

- A shared worker-wide cache (D1).
- Codex cache handling, Docker image pressure, `/nix` GC, checkpoint-ref retention (PRD #1810).
- Raising the data volume size (an operator choice, not a fix: a run's cache grows until it fills any size).
