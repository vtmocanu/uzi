# ADR-1597: A mid-turn tick publishes a scanned, pinned checkpoint without waiting for a milestone

**Status**: Accepted (M1/M2 implemented; M3a is a measurement-only investigation with no
code change)
**Date**: 2026-09-24
**Deciders**: architect (design), team lead, Vlad (maintainer).
**Issue**: GitHub issue [vtmocanu/uzi#1597](https://github.com/vtmocanu/uzi/issues/1597) —
there is no PRD file; the issue carries the reproduction and the milestone breakdown. This
ADR carries the decision, its invariants, and the alternatives most likely to look
attractive to a future reader who has not walked the same failure.
**Numbering**: `0122` is a PRD number (PRD #122); `1036` and `1597` are **issue** numbers
(issue #1036's PRD is #1062). ADRs are numbered by their tracking item, whichever kind it is.
**Related**: extends [ADR-122](0122-checkpoint-push-broker.md) (the credential-free,
api-brokered checkpoint push this ADR's tick reuses without changing the broker) and
[ADR-1036](1036-checkpoint-workflow-overlay.md) (the workflow overlay this tick's
overlay-less publish path deliberately does not build — see below).

## Decision (summary)

A long implementation turn on a hosted worker previously reached no durable checkpoint at
all: the two existing triggers (milestone-cooperative, PRD #122 M8; time-gated iteration
boundary, PRD #267) both fire only at a **turn boundary**, so a worker evicted mid-turn
during a single very long turn lost everything committed since the last boundary — the
whole turn, in the worst case. This closes that gap **inside** the turn:

1. **A repeating mid-turn tick**, `CHECKPOINT_TICK_INTERVAL` (default `5m`; `0` disables),
   fetches the runner clone's committed work back into the worker's bare repository at
   most once per tick, when the branch tip has moved and the clone is not busy, and, once the
   branch tip differs from the last *confirmed* published tip and `CHECKPOINT_INTERVAL`
   (default `20m`, unchanged from PRD #267) has elapsed since the last publish *attempt* —
   not the last success, so a persistent broker failure is retried at most once per
   interval — publishes it to origin through the existing ADR-122 broker — credential-free,
   worker-side, unchanged wire contract.
2. **A per-flight sink gate** serializes the tick against every other path that moves the
   run's durable state — the checkpoint closure, the pause/wall/completion-hold parks, and
   the credential switch — so a tick can never interleave with one of those and fetch or
   publish a marker a park path is about to undo, or race the checkpoint floor.
3. **A metadata-only git-busy probe** (one non-blocking open of `.git`, then `lstat` of
   each marker; no git child), rather than racing git, skips the tick whenever one of a
   fixed set of git lock files or merge/rebase/cherry-pick/revert markers is present in the
   runner clone, or its gitdir is unreadable, regardless of age.
4. **Every checkpoint publish that goes through the checkpoint body without an overlay
   context is secret-scanned** — the mid-turn tick, the time-gated iteration-boundary tier,
   and milestone publishes on non-GitHub forges — over the pinned commit range being
   packed, minus content already public, before the pack is built; a finding or an
   untrusted scan skips the remote publish and falls back to the local fetch-back only. On
   a Codex run such an overlay-less (non-GitHub) milestone publish is instead deferred
   (`scan_deferred`) rather than scanned inside the boundary permit; a GitHub milestone —
   Codex or otherwise — always carries an overlay context and publishes unscanned.
5. **The tick runs in a cancellable process scope**: a quiescing run, shutdown, or a
   preempting sink waits for every tick child to exit, every lock wait to resolve, and any
   in-flight publish to settle before the clone or bare is touched again.
6. **The graceful-shutdown checkpoint feed line names one of a fixed set of reason
   classes** when the checkpoint is not published, instead of ever putting a raw error
   message or remote text on the feed.

Entry points: `GitCache.withBoundaryProcessSpawner` is in `agent/src/git.ts`;
`agent/src/tick-spawner.ts` provides the `TickSpawner` handed to it (own process group per
child, lock custody on cancellation); `agent/src/sink-gate.ts` is the per-flight mutex; the
tick body and the shutdown reason-class union live in `agent/src/runner.ts`; the pinned
scan-then-pack range and the size caps live in `agent/src/git.ts`; the pre-push (finalize)
secret scan runs via `agent/src/git.ts`'s `secretScanRange`, with `agent/src/secret-scan-guard.ts`
holding its pure, I/O-free helpers; `CHECKPOINT_TICK_INTERVAL` parsing is in
`agent/src/config.ts`.

## Context

### The gap: a single long turn reaches no checkpoint

Both existing checkpoint triggers are threaded from the executor's turn-boundary
`checkpoint?.()` call — the model-cooperative call at a signalled milestone
(`{ reap: true, ... }`, e.g. `agent/src/sdk-executor.ts:2410` and `agent/src/codex/codex-executor.ts:1654`)
and the iteration-boundary fallback after a turn ends (`{ reap: false, ... }`, e.g.
`agent/src/sdk-executor.ts:2713` and `agent/src/codex/codex-executor.ts:1666`). Neither
fires while a single turn is still running. A run whose lead spends a very long turn on one
milestone — the case this issue was filed against — commits real work along the way that
is invisible to both triggers until the turn ends; a worker eviction inside that window
loses the whole turn, not "up to `CHECKPOINT_INTERVAL`" as PRD #267's own worst case
assumed.

### The tick: interval, worst-case loss, and its claim boundary

`CHECKPOINT_TICK_INTERVAL` defaults to `5m` (`0` disables it entirely, same convention as
`CHECKPOINT_INTERVAL`). Each tick fetches back at most once, and only when the branch tip
has moved and the clone is not busy; it publishes to origin once `CHECKPOINT_INTERVAL`
(still `20m`) has elapsed since the last publish **attempt** — not the last success, so a
persistent broker failure is retried at most once per interval rather than hammered. A
milestone publish deferred out of a Codex permit (`pendingPublish`) is owed and bypasses
this gate once; after that attempt (published, blocked, or failed) the normal
`CHECKPOINT_INTERVAL` gate governs retries again. Worst-case data loss from a worker-disk
loss, while at least one publish sink (origin, through the broker) is reachable, is
therefore bounded at roughly `CHECKPOINT_INTERVAL` plus one tick interval — the tick that
was about to publish, plus the interval it waited to notice work had moved. That bound is a
best-effort opportunity, not a guarantee: sink reachability alone does not provide it. It
holds only while the tick actually runs (the clone is not git-busy, the sink gate is free,
and no retained bare lock remains), the scan of the pinned range is trusted and finding-free, and the broker accepts the
publish. A git-busy marker defers the tick for as long as it is present, whatever its age; a
finding or an untrusted scan keeps that tick's work on worker-local storage (a later
unscanned park/shutdown/pause/capture or GitHub milestone publish may still ship it, so this
is not a containment guarantee); and the tick retries a failed publish at most once per
`CHECKPOINT_INTERVAL`. In each of those cases the work is at risk from a worker-disk loss,
and is never reported as durable.

**There is no durability claim while every sink is down.** The tick's fetch-back into the
worker's own bare is local disk, not a different failure domain from the worker itself; the
tick's improvement is real only insofar as the *origin* publish succeeds. A fetch-back
alone — with the remote publish skipped or failing — is never reported as durable on the
shutdown feed (see the reason classes below): it is exactly the pre-existing PRD #267
local-only fallback, unchanged.

### The claim boundary that does not move: park/shutdown/pause/capture and every GitHub milestone stay unscanned

**This is unchanged by design, not an oversight.** Scanning covers every checkpoint publish
that goes through the checkpoint body **without** an overlay context: the mid-turn tick, the
time-gated iteration-boundary tier, and milestone publishes on non-GitHub forges. On a Codex
run such a milestone publish is deferred (`scan_deferred`) instead of being scanned inside
the boundary permit — see the scan hardening section below. On GitHub, every milestone
(`reap:true`) publish carries an overlay context and is therefore unscanned, whether or not
an overlay commit is actually built (the overlay context is built for every GitHub milestone;
the pack carries the agent's own commits either way). Explicitly, the unscanned set is: park,
shutdown, pause, capture, and GitHub milestone publishes — all pushed unscanned to
`refs/uzi-checkpoints/<branch>` by the api (`Service.Publish` in
`api/internal/workersvc/service.go`). The rationale: those paths are narrow, already-audited
windows (a park/shutdown/pause/capture publish happens once, at a boundary the agent does not
control mid-flight; the overlay is a synthetic transport wrapper, not agent-authored content),
while the mid-turn tick fires repeatedly, unattended, deep inside a live agent turn — the
highest-frequency, highest-exposure publish path uzi has. Concretely: **a secret committed
and then removed within the same single turn is caught by any scanned publish whose range
contains the adding commit**, because gitleaks git mode scans each commit's patch. The
unscanned publishers (park/shutdown/pause/capture, and GitHub milestone publishes) would ship
it. **uzi's push-protection handling runs only at finalize** — it is not relied on as a
backstop for this path (`agent/src/secret-scan-guard.ts`'s pre-push scan plus the `GH013`
remote parse), long after a checkpoint publish would already have shipped the range to the
`refs/uzi-checkpoints/<branch>` mirror. Once a finding is in the unpublished range, it blocks
every *scanned* publish until it leaves that range (fail closed); the unscanned
park/shutdown/pause/capture and GitHub-milestone publishers would still ship it, unaffected by
the block. gitleaks git mode also skips files with a NUL byte (binary content) — a known
instrument limit this scan shares with the finalize pre-push scan, not a regression.
Another known limit shared with the finalize scan: a multi-line secret (e.g. a PEM key
body swapped between existing BEGIN/END lines) is not detected when only its middle lines
are added — the same as gitleaks git mode for ordinary commits, and now for merges too
(only the lines a merge adds are fed to gitleaks).

### The scan hardening

- **gitleaks v8.30.1 in `git` mode does not honour `--timeout`** — it exits `0` with a
  partial, silently-clean-looking report instead of erroring or timing out. The worker
  therefore enforces its **own** hard deadline on every gitleaks child process and classes
  an overrun as **untrusted** (`deadline`); it never passes `--timeout` to a `git`-mode
  invocation, because that flag cannot be trusted to do anything there.
- **The checkpoint secret scan never runs inside a Codex boundary permit** (the finalize
  pre-push scan, unchanged by this issue, still runs under the finalize permit). An
  overlay-less milestone on a Codex run instead fetches back and **defers** the remote
  publish (`scan_deferred`): a tick is kicked to run about 1s later; if that tick finds the
  sink gate held (`gate_busy`) it is kicked once more about 1s later, and a second miss
  leaves the owed publish to the next overlay-less checkpoint (the next regular tick or
  iteration boundary), which publishes it regardless of the time gate. With
  `CHECKPOINT_TICK_INTERVAL=0` the deferred scan+publish runs right after the permit ends,
  before the milestone checkpoint returns. This avoids either starving the permit's own
  deadline for scan time or running the scan un-timed inside a window the permit's
  supervisor cannot individually kill a child in.
- **The scan range is floored at content already public.** Three floors, each excluded
  from the scan (`tipSha ^excludeSha ^<default tip> ^<confirmed tip>`), so nothing
  reachable from any of them is re-scanned: `excludeSha` (`origin/<branch>` when
  it exists, else the default branch), the last **confirmed** published checkpoint tip
  (only when it is an ancestor of the pinned tip), and the origin default-branch tip. The
  pack range stays exactly `tipSha ^excludeSha` — unchanged from the pre-tick pinned-range
  contract, but the SCANNED range is over the pinned range being packed, minus commits
  already reachable from the default branch or the last confirmed checkpoint (content
  already public). Invariant: every packed commit is either scanned right now, already
  reachable from a confirmed checkpoint publish (which may itself have been an unscanned
  park/pause/shutdown publish — already public), or already on the default branch — no
  packed commit is both unscanned and not already public.
- **Size caps charge what gitleaks actually reads**, including the OLD-side blobs of
  modified or deleted paths (a diff touches both sides): 8 MiB per blob; 128 MiB total
  across distinct old- and new-side blob bytes; 100k distinct blob oids (both sides); at
  most 32 merges (`too_many_merges`) and a 32 MiB cap per merge diff
  (`merge_diff_too_large`). Exceeding any cap is treated as **untrusted**, same as a scan
  failure — the publish is skipped, not force-published unscanned.
- **Merge commits.** gitleaks git mode neither diffs nor counts merge commits; each merge's
  own contribution is scanned via `git show --remerge-diff --text` for a two-parent merge on
  git ≥2.36, or `git diff --text M^1 M` (a superset) for an octopus merge or older git,
  feeding gitleaks stdin only the ADDED lines (what git mode itself scans), with a
  byte-count liveness check. gitleaks git mode also skips files with a NUL byte (a known
  instrument limit shared with finalize) in an ordinary, non-merge commit; it is not a
  regression introduced here.
- **Commits with no textual hunk in a non-deleted file** (pure rename, empty, mode-only,
  binary-only, empty new file, delete-only) are excluded from the expected count so the
  liveness check (expected == gitleaks' "N commits scanned") does not class a complete scan
  untrusted.

### Per-flight sink gate and preemption

The gate (`agent/src/sink-gate.ts`) is an async mutex, re-entrant for the owner via
`AsyncLocalStorage`, held by exactly one of: the checkpoint closure, a pause park, the wall
park, the completion-hold park, a credential switch, or the tick. A gated path always wins:
if the tick holds the gate when a gated path needs it, the gated path **preempts** the tick
(aborts its process scope) and then waits until the tick has fully settled — every child
exited, any lock wait resolved, any in-flight publish rejected or completed — before
proceeding. The tick, by contrast, only ever *tries* to acquire the gate
(`tryAcquire`, non-blocking); a miss is a normal tick outcome (`gate_busy`), never an error.

### Git-busy probe

Before touching the runner clone, the tick opens `.git` exactly **once**, with
`O_RDONLY | O_NOFOLLOW | O_NONBLOCK` (so a FIFO left in place opens immediately instead of
blocking on a writer, and a symlink is refused with `ELOOP`), then classifies it by
`fstat` on that same file handle — never a separate stat-then-open, which would leave a
TOCTOU window. "Busy" is one of a fixed set of lock and in-progress markers (`index.lock`,
`HEAD.lock`, `packed-refs.lock`, `refs/heads/<branch>.lock`, `MERGE_HEAD`,
`CHERRY_PICK_HEAD`, `REVERT_HEAD`, `rebase-merge/`, `rebase-apply/`), or a `.git` that is
unreadable or neither a regular gitfile nor a directory. Any marker present makes the tick skip, **whatever its
age**: an old, abandoned lock is treated exactly like a fresh one, because the probe cannot
distinguish "still being written" from "leaked" without racing the writer. Two residuals
follow directly from this design, not from a bug: (1) the TOCTOU window between the probe's
open and the tick's actual git invocation is real but narrow, and errors the tick's own
attempt rather than corrupting state; and (2) a **planted** lock file (or marker) left by
some other process can make the tick defer indefinitely — this is surfaced to an operator
once it becomes visible on the run: after 3 consecutive git-busy ticks (any marker), one
deduped feed line, distinguishing the tick's outcome from an ordinary transient busy state.

### Pinned-SHA scan-then-pack

The publish resolves the range to be scanned and packed to **immutable commit SHAs** first
(`tipSha`/`excludeSha`), then scans exactly that pinned range, then packs exactly that
pinned range — no ref is re-resolved between the scan and the pack, so a tracking or origin
ref that moves mid-tick cannot desync what was scanned from what is shipped.

### Cancellable tick scope and quiescence

Every tick child is spawned in its own process group (`detached: true`); cancelling the
tick's scope signals the whole group with `SIGTERM`, then `SIGKILL` after a 2s grace.
Teardown — a quiescing run, shutdown, or a preempting sink — always awaits the tick's full
settlement (every child exited, every lock wait resolved, any publish rejected or
completed) before finalize or shutdown proceeds; nothing is touched concurrently with a
tick that has not yet finished unwinding.

### Lock ownership proof

git removes its own `*.lock` files on a clean `SIGTERM`, but a child that survives
`SIGTERM` and is then `SIGKILL`ed can leave one behind, and a stray lock in the worker's
bare would fail every later fetch-back, publish, or finalize. A lock is deleted **only** on
proven ownership: absent from a pre-spawn snapshot of candidate lock paths (presence +
dev/ino), **and** held open by the cancelled child at `SIGKILL` time (read from
`/proc/<pid>/fd/*` for every live member of the child's process group), **and** still the
same dev/ino at that path afterward. Every other case — non-Linux, a `/proc` read failure,
an inode mismatch, a lock a foreign process opened, a lock that pre-existed the spawn — is
**retained**, reported with its evidence, and blocks the sink for as long as the file
exists (never by age). **Retention, not removal, is expected to be the common outcome**:
real git usually closes a ref-lock's file descriptor before the final rename, so a git
process killed in the narrow window after that close but before the rename leaves a lock no
live process holds open — which this module cannot prove was its own child's, and therefore
correctly keeps rather than risks deleting something it cannot verify.

### Shutdown reason classes

The graceful-shutdown feed line now says `published`, or names one bounded reason class
when it is not: `timeout`, `boundary_blocked`, `publish_rejected`, `publish_skipped`,
`publish_error`, `no_local_tip`, or `bare_lock_retained` — every one of these classes is
new in this issue (the last was added in M2 — a cancelled tick's retained lock, still
present, is what is blocking the fetch-back or publish). **A publish that landed wins over
a late Codex boundary error**: if the checkpoint body's publish was ACKed before the
boundary itself later throws (e.g. a deadline or cleanup failure after the ACK), the feed
reports `published`, not the boundary error — a checkpoint that is real on origin must
never be reported as failed because of an unrelated failure downstream of it. On the
**generic publish-failure feed** an aborted publish is silent (run log only), because a
deadline/permit abort is an expected bounded stop rather than a publish fault; it can land
before or during the upload, and an abort after the pack tip is known is recorded as an
ambiguous attempted tip. On the **shutdown line specifically**, an aborted publish is
instead named: `timeout` when the permit/deadline signal is what aborted it, else
`publish_error` for any other abort. No raw error message, remote text, or credential is
ever placed on the feed; only the class. A checkpoint later confirmed to have landed, after
an earlier report of failure, emits one recovery line. A deferred Codex milestone's
post-permit publish (the `CHECKPOINT_TICK_INTERVAL=0` inline case above) is itself skipped,
not retried, when the run is already being cancelled or shut down by the time it would run.

### The #1416 steer's feed timing

The worker-authoritative safety steer that PRD #1416 arms on a detected history rewrite
(`maybeSteerOnDivergence`) is drained by the executor only at its **turn-boundary** loop
top (`pullSafetySteer`, ahead of the follow-up drain), on both executors. Because the
mid-turn tick's fetch-back can now detect and arm that divergence **mid-turn** — earlier
than either pre-existing trigger could — the steer's feed line can appear on the feed up to
one turn **before** the agent itself sees and acts on it: the tick's fetch-back arms the
steer as soon as it observes the rewrite, but the agent does not consume it until the next
turn boundary. This is not a regression introduced here; it follows directly from
`pullSafetySteer` being a turn-boundary-only drain point that this issue does not change.

## The ephemeral-storage investigation (M3a)

A separate line of work under this issue investigated a historical report of a docker-lane
worker being evicted for exceeding its ephemeral-storage request, attributed at the time to
"25.9 GB" of container growth. `scripts/measure-worker-ephemeral.sh` reproduces the measurement
from scratch, against the real worker image built from `agent/templates/base/Dockerfile`,
under a workload of: an offline clone of this repo, `npm ci` in `agent/`, a `go build` + `go
test` of `api/internal/config` under the sparse agent environment, and a worker-default-env
phase (`docker ps -s` reported `55B` virtual `9.61GB` for the resulting container).

kubelet's own `"Container worker was using …"` eviction figure is `Rootfs.UsedBytes +
Logs.UsedBytes` (traced to upstream `pkg/kubelet/eviction/helpers.go`'s `evictionMessage`;
the cluster's actual kubelet version was not checked against that source read, so this is the
upstream contract, not a confirmed match to the exact deployed kubelet). The `run-workdir`
`emptyDir` mount is counted **separately** from that figure by kubelet, not folded into it.

Table, condensed from `scripts/measure-worker-ephemeral.sh` output (real worker image):

```
writable layer (SizeRw)                      55  55.0B     COUNTED (Rootfs.UsedBytes)
container log bytes                         318  318.0B    COUNTED (Logs.UsedBytes); startup only
  home (/home/worker) writable               55  55.0B     writable-layer delta (docker diff)
  /tmp, /root, /var/tmp, /usr/local writable  0  0.0B      writable-layer delta (docker diff)
run-workdir (/data/runner)            583368704  556.3MB   SEPARATE (emptyDir)
data PVC (excluding run-workdir)      425385984  405.7MB   SEPARATE (PVC)
  agent-home (/data/agent-home)       425377792  405.7MB     (subset of data PVC)
nix PVC (/nix)                       3621756928  3.4GB     SEPARATE (PVC)
```

**Conclusion: no writable-layer or log-growth source was demonstrated by this workload.**
The historically observed 25.9 GB figure is not reproduced here, and its historical path
attribution remains **unknown**. Explicitly **not modelled** by this measurement: the
agent's PID1 stdout accumulating over a multi-hour real run, cross-run accumulation on one
long-lived pod (this measurement is a single fresh container), tools that ignore `TMPDIR`
(e.g. a headless browser writing under a fixed path regardless of the env var), and the
dind sidecar's own storage.

Because no resource-request or cleanup change was demonstrated as warranted by this
investigation, **none was made**: `render.go`'s ephemeral-storage request stays deliberately
"err low" and `workers.docker.ephemeralRequest` in the chart stays at `4Gi`, matching the
existing "conservative, chart-tunable" posture recorded in `specs/human.md` Feature #224.
The next step this investigation leaves open is measuring a live pod's actual `LogPath` and
writable-layer growth over a genuinely long real run, which this offline reproduction cannot
substitute for.

## Alternatives considered

- **Lower `CHECKPOINT_INTERVAL` instead of adding a mid-turn tick.** Rejected: the existing
  time-gated path only ever fires at a turn boundary, so no value of `CHECKPOINT_INTERVAL`
  closes the single-long-turn gap — it bounds the wait *between* boundaries, not the
  duration of one that never ends.
- **Run the mid-turn fetch-back without ever publishing to origin.** Rejected: a
  worker-local fetch-back is not a different failure domain from the worker whose eviction
  this issue is about; only an origin publish (through the credential-free broker) improves
  on the pre-existing PRD #267 behaviour, so the tick must also gate a real publish.
- **Scan every checkpoint publisher (park/shutdown/pause/capture/overlay), not just the mid-turn
  tick.** Deferred, not rejected outright: those paths are narrow, already-audited windows
  that fire once at a boundary the agent does not control, unlike the tick's repeated,
  unattended, mid-flight firing — folding them in is a larger, separable piece of work with
  a different risk/cost profile, tracked as a residual rather than taken here.
- **Trust gitleaks' own `--timeout` in `git` mode.** Rejected once measured: v8.30.1 exits
  `0` with a partial, clean-looking report on an internal timeout in `git` mode instead of
  failing loudly, so relying on it would silently under-scan a range and still publish it
  as if fully scanned. The worker's own hard deadline plus an untrusted classification on
  overrun replaces it.
- **Run the scan inside the Codex boundary permit with a fixed sub-deadline fraction** (the
  pre-round-3 shape). Superseded by deferring the publish to the next, non-permit-bound
  tick: sharing the permit's own bounded deadline with an unpredictable-duration scan risks
  starving either the scan or the permit's other cleanup work, whereas deferral costs at
  most one extra tick interval and runs the scan un-shared.

## Consequences and residuals

- A long single turn on a hosted worker now reaches a worker-independent checkpoint within
  roughly `CHECKPOINT_INTERVAL` plus one tick interval, without waiting for a milestone —
  closing the gap this issue was filed against, while the api-brokered push contract
  (ADR-122) and the workflow overlay (ADR-1036) are both reused byte-unchanged.
- **No durability claim is made while every publish sink is unreachable**; a fetch-back
  with no successful remote publish is reported exactly as before (never `published`).
- **The claim boundary intentionally does not widen**: park/shutdown/pause/capture and every
  GitHub milestone publish remain unscanned, by design, not oversight — see the dedicated
  section above. A future decision to scan them is a separate, larger piece of work.
- **Neither `CHECKPOINT_TICK_INTERVAL` nor `CHECKPOINT_INTERVAL` is exposed via the chart or
  the controller**, so hosted workers run the defaults (`5m`/`20m`); overriding either needs
  a worker-env change outside the chart's current surface.
- **A planted lock or marker can defer the tick indefinitely**; visible to an operator after
  3 consecutive git-busy ticks (any marker), but not auto-resolved — the busy probe
  deliberately never races a writer to decide otherwise.
- **The #1416 safety steer can now be armed up to one turn before the agent consumes it**,
  a direct, expected consequence of `pullSafetySteer` staying a turn-boundary-only drain
  point while the tick's own detection moved earlier.
- **The ephemeral-storage report's historical 25.9 GB figure remains unexplained.** This
  ADR records that as an open question, not a closed one; the next step (a live pod's
  `LogPath` and writable-layer growth over a genuinely long run) is left for future work.
