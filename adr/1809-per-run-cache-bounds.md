# ADR-1809: a run's rebuildable caches stay per run and are bounded, trimmed and dropped through descriptor-pinned agent-uid removal

**Status**: Accepted (issue #1809; implemented in this branch, with the M1/M3 real uid-split fixtures and the hosted-k8s acceptance pending, see the PRD)
**Date**: 2026-09-28 (amended 2026-10-03 by approved #1829 review)
**Deciders**: agent team (issue #1809 milestones and review waves); the maintainer at plan review (the uncounted soft-cap park, trim-not-wipe).
**Related**: PRD #1809 (`prds/1809-worker-disk-safety.md`, the decision log D1-D8 this ADR condenses the lasting parts of); [ADR-837](0837-worker-disk-lifecycle.md) (the pressure recycle this design sits in front of, unchanged); [ADR-1766](1766-codex-vault-lock-park.md) (the typed `recovery_wait` cause precedent `data_volume_full` follows); [ADR-1598](1598-codex-command-storage.md) (Codex's own per-run cache, out of scope here). Serves `specs/human.md` Feature #1809.

## Decision (summary)

A Claude run's Go and npm caches live under its private HOME (`/data/agent-home/<run>`) and stay **per run**. uzi bounds them instead of sharing them: a process-ended park drops a **named list** of cache subtrees; a running run is **trimmed least-recently-used first** at a **proven quiet point** when it exceeds its cap; and a run that stays over the cap is parked. At the hard volume threshold the periodic layer selects a registered run with measured cache bytes; it parks that run only with a server-confirmed running executor, and reclaims caches in place in other phases. The named cache removals and the four covered teardown trees use a **descriptor-pinned walk run as the owning agent uid**; that walk is a seam other code must use, not re-implement. Other path-based cleanup sites remain as the residuals listed below.

## Context

Measured on 2026-09-27: one uzi-on-uzi run's HOME reached 24G (21G of it `.cache/go-build`) on a 25 GiB data volume, and the run failed on resume with ENOSPC. Nothing bounded the caches while the run lived, the park kept the whole HOME, and the PRD #837 recycle is deferred while a worker is busy (parked runs included). Under the uid split the cache dirs are owned by `runner`/`runner-cmd`, some `0555` or `0700`, so the worker cannot even list them, and a run HOME is `worker:runner 0775`, not sticky: any live `runner` process, the run's own or another run's on the same worker, can rename a component and plant a symlink at any moment.

## The decisions

### D1: per-run caches, not a shared cache (trust boundary)

Runs on one worker may execute different repositories' untrusted code. A shared writable build or module cache would let one run plant an entry another run's build consumes; `GOFLAGS=-modcacherw` fixes permissions, not that boundary. Each run keeps its own caches and uzi bounds them. A shared cache is a possible later optimisation that needs its own trust decision.

### D2: the named cache list

Only `.cache/go-build`, `go/pkg/mod` and `.npm/_cacache` (relative to the run HOME; `RUN_CACHE_SUBTREES` in `agent/src/rmtree.ts`) are ever deleted. The removal helper refuses any other name. Everything else stays, including unknown files, the session transcript and its subagent dir, `.claude.json`, the memory dir and `go/bin`. Adding an entry needs code evidence that its tool refills it on demand.

### D3: drops only on process-ended parks, Claude runs only

The drop runs in the claim's `finally`, only for a Claude run that parked with its executor returned and its process tree reaped (usage limit, owner pause, recovery and disk parks). A gate park (`awaiting_approval`, `awaiting_input`, `awaiting_followup`) keeps a live executor and never reaches it. Codex runs keep their caches on their own per-run volume (ADR-1598). Separately, the worker's periodic reclaim drops the same named caches from any worker-owned HOME of a run the api reports parked with its process ended, whatever its runtime. The drop is best-effort under one deadline and never throws: a failed drop leaves the HOME as it was before this change.

## What outlives this work

### Descriptor-pinned agent-uid removal is the only way to delete inside a run tree while runs are live

`rmHomeSubtree` (a listed cache subtree under a HOME) and `rmTreePinned` (a whole run tree under a parent, used by the running disk reclaim) walk by **pinned directory descriptors**, never by path: every lookup goes through `/proc/self/fd/<dirfd>/<name>`, each directory is pinned with `O_PATH | O_DIRECTORY | O_NOFOLLOW`, the helper refuses to start unless its own pin is the inode the worker pinned, and the final `rmdir` re-checks the inode through the pinned parent. Under the uid split the walk runs as `runner`, then `runner-cmd`, then `runner` (then the worker for worker-owned leftovers), with no path-based worker removal at all; single-uid it runs once as the worker. A symlinked, non-directory or not-worker-owned root is refused. Without Linux `/proc/self/fd` both refuse outright; there is no path fallback. Listings are streamed and every pass checks entry and cooperative time budgets. The teardown deadline bounds worker waiting across all ordered passes, not guaranteed helper lifetime: under the uid split the worker cannot kill a foreign-uid helper blocked in a syscall. A refusal or exhausted budget warns and retains whatever remains without changing the run outcome; non-Linux production retains the tree with a warning and never falls back to path removal. Model-pass query settlement/grace precedes removal, and its live-HOME registry entry remains until the cleanup attempt settles.

Consequence for the covered cache and teardown paths: deletion or measurement while other runs' processes may be live must go through these helpers (or the same pinned prelude), never `fs.rm`/`rmHomeTree` by path. Path-based `rmHomeTree` stays only in boot sweeps and the startup job reaper before claims begin. Terminal run HOMEs, provision dirs, model-pass HOMEs and job workspaces use `rmTeardownTree`, which validates the absolute path and calls `rmTreePinned` with its dirname, basename and one shared deadline of now + 120000 ms. Reaping one run does not stop sibling runs' writers. The worker-owned adapter refuses roots the worker does not own: legacy runner-owned provision dirs stay skipped, not chmodded open.

[Issue #2324](https://github.com/vtmocanu/uzi/issues/2324) extends the worker-owned teardown adapter to leftover job replacement, terminal skills-plugin cleanup, Codex advice cwd and session-seed cleanup. Fresh job creation uses an exclusive mkdir first; an existing root must be removed successfully before a second exclusive mkdir, so refused content is never reused. Session-seed removal before adoption is required and fails closed. An exclusive non-recursive mkdir then creates a fresh worker-owned seed root with mode 0750; the worker-owned sticky codex-data parent and lack of group write prevent a foreign runner from replacing or populating it. An entry planted between removal and creation aborts preparation without adoption. Terminal disposal remains best-effort with a warning. The worker-owned adapter opts into the fixed `.uzi-skills-` prefix with a nonempty suffix that is one component, has no leading dot/dash, separator or control character, and permits branch punctuation and Unicode; the default disk-reclaim name contract remains unchanged.

Unit 2 routes `GitCache.removeRunnerClone` and the executor's fallback disposal of Codex advice `ownedDataRoot` through `rmRunnerTeardownTree`. Its private ownership policy requires the fixed runner uid under the split, and the actual current worker uid otherwise (unknown identity refuses). The worker pins the parent and leaf with the same owner, inode, no-follow and budget checks, but never group-widens a runner-owned root. Existing runner, runner-cmd, runner and worker passes empty accessible content; the worker then removes the identity-checked leaf through its pinned parent. Owner-only restoration handles private/read-only runner directories. Ordinary mixed-uid content under shared ancestors is removable. A private runner ancestor containing nonempty private runner-cmd content refuses and retains the remainder without widening access: provisioned clone scratch is shared 2770, and runner-cmd cannot create beneath an already-private runner ancestor. Closing an ancestor after foreign content was created, or moving that content into a private ancestor, can manufacture this retained layout. Clone disposal propagates refusals to its existing warning caller and preserves terminal custody checks; the advice-data fallback runs only after a positively confirmed `clean: true` disposal outcome. A `clean: false`, unknown or thrown outcome retains the tree for forensic recovery and warns, because a supervised writer may still be alive. Advice cwd cleanup remains unconditional. Normal clean-path data removal is the launcher's runner-uid rm; the pinned executor fallback handles its remainder. Clone names alone opt into the same one-component predicate as skills suffixes, preserving punctuation and Unicode without changing `TREE_NAME_RE`.

Known residuals still use path-based removal while sibling runs may be live: skills-plugin rebuilding in `materializeSkillsPlugin`, holding/scratch disposal in `retireRunnerClone`, and the Codex launcher's separate `defaultRemoveRunnerTree` cleanup (which precedes the executor's advice disposal). The last site invokes cap-less BusyBox `/bin/rm -rf` as the tree's own uid. BusyBox walks by path, but a redirect can delete only what that unconfined uid already has permission to delete, the same posture as `freeCanonicalClonePath`. This argument must be revisited if a Landlock-confined runner-cmd can reach a tree that an unconfined same-uid rm deletes; securing the executor's later fallback does not establish that confinement boundary. The advice executor racer isolates that later site by making only the launcher's synchronous removal preflight observe an absent root; the executor's actual removal sees the raced tree.

### The LRU trim order

A trim (never a wipe) evicts until the caches are at or under the low-water mark (`UZI_RUN_CACHE_LOW_WATER`, default 0.6 of the cap), in this order:

1. `.cache/go-build`, oldest mtime first, one unit per `*-a`/`*-d` file or executable-cache directory (Go refreshes an entry's mtime on use); `README`, `trim.txt` and `testexpire.txt` are never touched.
2. Only while still over the low-water mark: `.npm/_cacache` by index entry, oldest `index-v5` bucket first, then only `content-v2` files no remaining bucket references (npm errors on an index entry whose content is gone). Skipped when the index could not be read completely.
3. Only while still over the **cap**: `go/pkg/mod` whole (Go cannot evict single modules from its read-only tree).

An entry whose mtime or type changed between listing and eviction was used meanwhile and is kept.

### The quiet-point proof: HOME/cwd process attribution

The CLI spawns each Bash tool command detached (new session and process group), so "the CLI's process groups are empty" does not prove the run is idle. A process is the run's when its environ holds exactly `HOME=<run HOME>` or its cwd is inside the run's HOME or worktree, found by a bounded scan of the `runner` uid's processes (single-uid: the worker's). A non-dumpable process reads EACCES even for its own uid, so an unreadable process counts only when **stat-linked**: its session or process group is a readable-attributed process's (never the worker's own), or its bounded parent chain reaches one or a run CLI pid. A proc mount with `hidepid`/`subset=`, a truncated scan, or any unknown answer is "not quiet". The same attribution drives the reap before every cache drop.

Residual: a process that changes its HOME and leaves the run's directories is invisible to the scan.

### Counted vs uncounted disk parks

Disk parks use `recovery_wait` with cause `data_volume_full`. The original four paths are: a disk-full failure of the clone/fetch, the claim/resume preflight, the soft cache cap and the mid-turn hard stop. Parks that mean the volume actually filled or hit the hard threshold (the first two, and the hard stop) are **counted** toward `UZI_RUN_DISK_PARK_MAX` (default 3 timed counted parks; the next counted park holds for owner Resume/Cancel under #3969). The soft-cap park is preventive and carries `disk_park_preventive: true`, so it is **uncounted**: a run that merely grew past its cap is never failed by it. The api accepts the flag only with that cause.

Strict classify-reclaim-retry remains limited to clone/fetch and claim/resume preflight. It does not classify opaque executor errors. The separate approved #1829 policy below can defer an otherwise-terminal executor rejection when the current data volume is confirmed full.

The retained-successor admission rule (#3865) is narrowly different from the claim/resume preflight and is not a fifth park path. Before a retained recovery seeds a fresh attempt, after the existing floor check, the worker samples the successor destination's volume and refuses the seed when current usage already reaches the governor's effective hard threshold, or when projected usage would. The projection is the predecessor tree's measured footprint times 1.25 plus a 1% allowance; when the measurement is incomplete it uses the larger of the partial reading and a fallback of max(1 GiB, 2%) bytes and max(100,000, 2%) inodes; the higher of the byte and inode projections decides. One bounded reclaim pass runs before a refusal. A refusal is not a park: it creates no successor, reports no `data_volume_full` and is charged to the recovery iteration the claim already reserved (at most 3 per five-minute window). Exhaustion ends as the existing `budget_exhausted` blocker and `retained_recovery_exhausted` park, which counts toward the retained-park cap and never toward `disk_park_count`. The rule applies only when the api advertises both `recovery_cause_data_volume_full` and `recovery_cause_retained_recovery_exhausted`; otherwise behaviour is unchanged. The projection is an estimate, not a quota or reservation, and the hard governor remains the backstop. No predecessor source is deleted and no recovery hold is discarded; only the existing D7 reclaim runs. Accepted costs (owner-confirmed): the check has no off switch and applies after any park cause; at the default 0.87 hard threshold on a 49 GiB volume a 1.2 GiB predecessor tree is refused from about 83% byte usage (about 83.5% on the fallback), and inode pressure can refuse earlier. Codex retained seeds are included as sibling protection, since the hard governor stops only Claude runs; a Codex run under pressure therefore spends its own retained-recovery allowance. Each claimed generation still takes its normal server-side custody hold, so holds are bounded by the retained-park cap plus one, and once that cap is spent automatic retries stop for owner Resume.

### Amendment (#3969): owner hold after the counted disk allowance

2026-11-06. **Supersedes the original D6 decision to fail past the cap.** After
three timed counted parks at the default `UZI_RUN_DISK_PARK_MAX`, the next counted
`data_volume_full` park stays in `recovery_wait` with NULL
`recovery_retry_not_before`, without terminal failure or a judge run. The hold
preserves the consumed `disk_park_count`, custody, worker/session affinity and
unrelated budgets; `recovery_wait_count` still increments. `0` remains unlimited
timed counted parks. Preventive parks stay uncounted and timed. Stored
`fail_origin=data_volume_full` is retained for historical read compatibility only.

Authenticated owner Resume, via the existing bodyless
`POST /api/runs/{id}/resume-now`, queues one ordinary attempt. It resets neither
counts nor recovery episodes or other grants; admission can refuse the attempt,
and a subsequent counted park reholds. Resume banks held time after execution
has started and retains `started_at` and the remaining wall allowance; an
exhausted wall may require extension. A stamped cancel wins. Timer, credentials
and generic promotion cannot wake a NULL hold on compatible serving APIs.
Mixed old API versions retain generic promotion behavior: finish the serving-API
rollout before relying on holds. This is application policy, not a database
invariant.

Resume idempotence covers the current hold only. A bodyless request has no
durable identity binding to the observed hold; a delayed duplicate crossing a
later hold is not durably fenced. A later hold requires another claim and counted park;
that park can occur during preflight, before model execution starts. Clients issue one POST per owner action without automatic
retry; no concrete single-action replay has been found. This does not establish
a durable replay fence. The enforcing boundaries are the
[locked park transaction](../api/internal/workersvc/diskpark.go), the
[owner-scoped bodyless Resume handler](../api/internal/handler/resume.go) and
`HoldRunDataVolumeFull` / `ResumeDataVolumeFullHold` in the
[status-scoped queries](../api/internal/store/queries/runtime.sql).

The existing Go DTO has `RecoveryRetryNotBeforePresent`: explicit JSON NULL
identifies a hold, an omitted key means unknown timing, and a finite timestamp
means timed even when due. Malformed non-null JSON timestamps are rejected;
there is no new wire field. Default CLI wait/follow stops at owner holds,
explicit `--until` preserves its targets, and CLI/TUI/web show owner Resume/Cancel
without a countdown. See the [disk recovery contract](../docs/run-recovery-wait.md#worker-data-volume-full).

Existing park stop/quiesce and safe rebuildable-cache drops bound resource
activity; a held run runs no executor capacity-polling loop. There is no new
capacity-based auto-resume, cleanup or reclaim authority, nor a power-loss
durability promise. Open source custody remains protected with no
`forge_no_output` release exception; archives remain exportable and expire
independently. Unpublished worker-local work is not guaranteed across worker or
storage loss. The distinct #3865/#3882 successor-refusal mechanism, #3330 recovery
budgets and #3329 genuine-idle reclaim/no busy-pressure policy remain unchanged.

### Full-volume terminal-failure deferral (#1829)

Approved #1829 review supersedes the standing decision that a later disk-full execution error must fail immediately. This is intentional failure deferral based on current occupancy, separate from D6's strict write classification and safe worker-owned retry. It neither attributes the failed write nor requires ENOSPC, a diagnostic path, or write instrumentation.

Admission and the periodic hard layer use the higher valid byte or inode used fraction (#1829). Missing or invalid accounting, including missing or zero inode totals, falls back to the valid pair; neither valid means unknown. Existing thresholds, protocol and configuration switches are unchanged.

The issue lane has one final generic arm after executor writer, ticker and report settlement. Only the direct, otherwise-untyped executor rejection qualifies; setup, finalize, bookkeeping and settlement failures do not. Typed recovery outcomes and recognizable typed, wrapped or trusted security/guardrail failures remain excluded, including admission, launcher and plan-wiring refusals. Preserved trusted types and `cause`/`interruption` wrappers remain excluded. Complete legacy reasons are recognized directly and through leading `<context>: <reason>` envelopes with a literal colon and space. Contexts may contain apostrophes; double-quoted or multiline contexts, alternate separators, quoted reason diagnostics and producer-domain near-misses are not legacy refusal envelopes. Ordinary opaque failures keep their existing handling. The exclusion fixtures in `agent/test/runner-terminal-disk-deferral.test.ts` pin this boundary. Cancellation, pause, shutdown, stale or terminal ownership and terminal journal evidence keep precedence; a qualifying counted park past the disk cap now takes the #3969 owner hold. Eligibility requires the `recovery_cause_data_volume_full` feature, claim-generation stamping, ownership explicitly `running` with the exact current generation present, and direct device equality of the actual worktree and HOME with `dataDir`. A fresh sample must have valid byte accounting AND raw `files` and `ffree` present and finite, with positive `files`. Available bytes below max(256 MiB, 1% of volume size) OR free inodes below max(1000, 1% of inode total) qualify. Missing/unknown accounting, device mismatch, an old API or an `awaiting_approval` revision keeps existing handling.

Once eligible, the worker waits for one bounded existing reclaim pass, then takes one counted, nonpreventive `data_volume_full` park even if room returns. It does not replay the command, provider or executor. The clone, HOME, session, journal and custody are retained for recovery.

This bound now applies per park invocation to every mid-run `data_volume_full` park, not this policy alone (see [Amendment (#3916)](#amendment-3916-mid-run-disk-park-capture-bound)). Capture permits at most three nonblocked unverified attempts; five consecutive blocked proofs fail `worker_residue_blocked`. Alternating blocks allow at most 15 capture calls, followed by at most five final proof attempts, using existing waits and deadlines. After three nonblocked unverified captures, degraded parking requires affirmative quiescent Linux Claude/stub process proof, or a successful existing Codex supervisor boundary plus the applicable clone proof. Skipped, absent or unverified proof and surviving processes cannot authorize degraded parking. ACK transport reconciliation is separate from these bounds, does not reset the capture budget, and is not a fixed wall-time promise.

A degraded park retains the original clone and journal without claiming a verified predecessor or a latest published checkpoint. Same-worker resume captures dirty work before admitting a fresh executor. Verified capture keeps its existing truthful checkpoint reporting. Terminal, cancellation and stale-claim dispositions remain; #3969 cap exhaustion is a nonterminal hold and grants no cleanup authority. Archive expiry is independent, so this is not perpetual archive retention.

Accepted residuals: an unrelated opaque execution error can coincide with fullness and be deferred. For actual trusted refusals, only completely erased-origin opaque failures remain eligible under this accepted coincidence residual. The cap bounds automatic counted retries, not misclassification; a configured unlimited cap remains unlimited. Periodic hard-stop selection depends on registered runs with measured cache bytes and a server-confirmed running executor, so it cannot guarantee this failing run parks before failure. Approval revisions are unchanged, including existing in-place cache relief; preserving approval through a new park mechanism is out of scope. Degraded recovery is worker-local and can lose newer work on cross-worker recovery.

## Consequences

- A resumed or trimmed run rebuilds cold (minutes), accepted against a failed run.
- The hard layer considers every watched Claude run in every phase (#1830). It stops (counted park) only a run whose executor is running and for which the server has acknowledged `running` for a status report newer than its last non-`running` report and newer than any declined or unreadable reply (reports are ordered by when they were sent; a `running` report still awaiting its reply changes nothing), because the api accepts a disk park only from `running` and a park would swallow a pending approval. Any other run (cloning, at the plan gate, in a revision turn, waiting on a question or follow-up, between approval and its first progress report, finalizing) is not stopped: its rebuildable caches are dropped in place, `.npm/_cacache` kept while a JS-deps install may be running, and it keeps its gate. A stop pending when a run is about to report a wait parks it from `running` instead.
  - Accepted hazards (#1830): a run in a revision planning turn, or in finalize (for example self-improve's dependency install and check suites), is not stopped: the api accepts a disk park only from `running`, and a finalizing run has left its executor, so the hard layer never asks it to stop and drops its caches in place instead. Its rebuildable Go caches can disappear under a running tool command at the hard threshold, and the tool rebuilds them; `.npm/_cacache` is kept while an npm install may be running. A clarification or completion question already posted to the run's feed stays visible when a pending stop parks the run instead of entering that wait.
- The PRD #837 data-PVC recycle remains the last resort on plain hosted workers. Amendment 2026-11-03 (#3852): after the fixed controller deploys, hosted Docker workers preserve `/data`; only explicit nix pressure permits nix-only recycle. Docker data pressure relies on park-and-trim and supported manual cleanup until #3431 M1b/#3329.

## Amendment (#3870): park-only Go scratch

2026-11-03. Go's temporary build work directory (`$WORK`, the `go-build*` directories) was created under the general `TMPDIR`, outside any per-run location the worker could find and remove. A Claude run on Linux now launches with `GOTMPDIR` pointing at `<run HOME>/.uzi-go-work/g<claimGeneration>` (`agent/src/go-work.ts`). `GOTMPDIR` moves only `$WORK`; an external link's `go-link-*` directory still goes to `TMPDIR`. This amends D2, D3 and D4 as below; everything else in this ADR stands.

- **Disposable scratch is distinct from cache eviction.** The leaf is not a cache: nothing refills it on demand and nothing in it is needed to resume. `RUN_CACHE_SUBTREES` in `agent/src/rmtree.ts` is unchanged. `rmHomeSubtree` additionally accepts exactly `.uzi-go-work/g<N>` (N a positive decimal generation); the `.uzi-go-work` parent is never removed by it, and any other name is still refused. This amends D2's "Only `.cache/go-build`, `go/pkg/mod` and `.npm/_cacache` … are ever deleted": those remain the only caches deleted, and the scratch leaf is the one other named subtree the removal helper accepts.
- **When it is removed (amends D3).** Only at the points D3 already gates, after the same complete process reap: a process-ended park (the claim's `finally`) and a disk park's drop before its capture remove all leaves, and the worker's periodic reclaim (D7) removes them from a parked run's HOME after the no-attributed-process proof, under the per-run lock. One addition: at a re-claim, once the #1828 HOME reap succeeds, leaves of strictly older generations are removed (the current generation's leaf is kept). Terminal teardown is unchanged and removes the Go-work root with the HOME. The in-place drop at the hard threshold (#1830) and the turn-boundary trim never touch the scratch.
- **Attribution (amends D4).** A process also counts as the run's when its environ holds a `GOTMPDIR` naming a managed leaf `g<N>` of this run's HOME (or a path below it), in addition to the exact `HOME` entry and the cwd test. This catches a process that reset `HOME` but still holds the scratch.
- **Not cross-run isolation.** The parent is created non-recursively through pinned directory descriptors, mode 2770 and group `runner` under the uid split, 0700 single-uid, and is never re-owned. A 2770 non-sticky directory in a group-writable HOME does not stop another same-uid process, including another run's on the same worker, from renaming or replacing it. That is the existing shared-UID posture and is accepted here; the descriptors only keep a swapped ancestor path from redirecting a later create outside the pinned directory.
- **Accounting.** Scratch counts in the run HOME's size. It does not count in cache bytes, the cap or the #1830 selection, and it is not capped while the run is live.
- **Failure behavior.** A missing or non-positive generation, or an ordinary root-creation failure, launches without `GOTMPDIR` and logs one bounded warning; the run is not failed. Removal is best-effort and bounded: at most 64 leaves are attempted per call, the rest are left for a later pass. A refused parent, a truncated enumeration or any failed or skipped leaf makes the result not clean. A scratch failure at a park does not set `sourceJoinFailed`. In the D7 pass a not-clean scratch is retried on every pass (bounded per pass) and the run is memoized only when its caches and scratch are both clean; the summary reports `go_scratch_removed` and `go_scratch_retained`.
- **Unchanged.** General `TMPDIR`, `DOCKER_HOST` and the Codex environment. Shared `go-build*` residue left in `TMPDIR` by earlier runs is not collected by this change.

## Amendment (#3916): mid-run disk park capture bound

2026-11-04. On hosted Docker workers with the source-authority runtime, a mid-run disk park's capture failed `run paused (now)` on every retry: the disk stop aborts the run lifecycle with its own pause and never rearms it, and the capture waited on that signal. The run stayed `running` and never reached `recovery_wait`. This amends the #1829 section above as described below; everything else stands.

- **The #1829 bound applies per park invocation to every mid-run `data_volume_full` park** (counted hard stop and preventive cache-cap; not credential-deferred), replacing "for this policy only". Three nonblocked unverified captures lead to the final quiescence proof (at most five attempts): proven gives the existing degraded park (clone, HOME, session, journal and custody retained, no verified or published checkpoint claimed, same status text); unproven fails `worker_residue_blocked` with custody kept. An incomplete capture settlement counts as a blocked attempt; five consecutive fail `worker_residue_blocked`, custody kept, with no capture run. Cancellation, shutdown and claim fence keep precedence; a pending owner or wall pause does not interrupt the proof and stays pending for resume.
- **Soft-cap parks remain uncounted** (`disk_park_preventive` kept), including when they take the degraded park. Later preventive parks of the same run can recur under existing policy; each is bounded per park and there is no cross-flight cap on uncounted parks.
- **The cap bounds automatic counted retries only when finite.** `UZI_RUN_DISK_PARK_MAX` limits timed counted parks before the #3969 owner hold; `0` remains unlimited.
- **Root fix: the park's capture waits only on terminal lifecycle and shutdown for source-authority acquisition** (`diskParkCaptureWait` in `agent/src/runner.ts`), not on the run's own pause. As a wait it bounds acquisition only; the fetch-back transport runs without that lock. The capture also uses the same signal for its remediation bootstrap and checkpoint publish, so those can be aborted. A real cancel, claim fence, receipt failure and worker shutdown still abort it.
- **Residual.** Owed-head reconciliation after a disk-park publish still waits on the paused signal and is skipped until the next claim. It does not block the park.
