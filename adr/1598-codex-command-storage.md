# ADR-1598: Codex command tmp and cache no longer accumulate in the writable layer, cleanup moves to the supervisor, and per-run caching gets its own worker-only emptyDir

**Status**: Accepted (implemented, issue #1598)
**Date**: 2026-09-24
**Deciders**: architect (design), coder (implementation), reviewer.
**Supersedes (in part)**: `specs/ai.md` §480's "`run-workdir` is the only remaining
emptyDir and must NEVER get a `sizeLimit`" (issue #224 M-a). That sentence is
frozen text and is not edited here; this ADR records that a worker pod now has a
worker-only emptyDir, `codex-cmd-cache` (in addition to any the docker lane
adds), and states explicitly that the same never-a-`sizeLimit` rule binds it
too, for the same reason: a `sizeLimit` is a kubelet eviction path of its own
(issue #224 / `specs/ai.md` §480; issue #225 is unrelated node image
accumulation, not this eviction rule).
**Related**: issue #1598, issue #224, issue #1597, issue #1582.

## Context: the evidence

Every Codex model-authorized command ran with `HOME=TMPDIR=/tmp/uzi-codex-command-<uuid>`,
a fresh directory per command. Any `go` invocation in that tree re-downloaded its
module graph into `GOMODCACHE`/`GOCACHE` under that per-command tmp, because
nothing persisted a cache across commands, let alone across runs. Worse, cleanup
used `os.RemoveAll`, which cannot delete a Go module cache's read-only (mode
0555) directories as the non-root owner that wrote them. `os.RemoveAll` returns
that failure as its own error; the call sites discarded it (`defer
os.RemoveAll(tmp)` in cmdsandbox, `_ = os.RemoveAll(...)` in the supervisor, at
the fork point f77d3103), so the tree was silently retained regardless.
Measured impact: two worker pods were evicted (about 26 GB of writable layer
each), and a live worker measured afterwards held 22 GB in `/tmp`. Fixing the leak, and giving Codex commands a real cache
instead of re-downloading every time, are the same issue because both live at
the same boundary: what a command's `HOME`/`TMPDIR`/`GOMODCACHE` point at, and
who is responsible for removing it.

## Decision, mechanism by mechanism

### `internal/safetree`: an fd-relative, no-follow tree removal primitive

A dedicated removal primitive, independent of `os.RemoveAll`, built to actually
delete what this issue produces:

- It opens the root relative to a caller-supplied parent fd and name, checked
  against the pin (never a path re-resolved from scratch, so a rename/symlink
  race after the caller pinned the directory cannot redirect the removal),
  pins the root's device/inode, and checks the **owner** of every entry it
  descends into. A mismatched device/inode, a foreign owner, an unexpected
  name, or an I/O error each stop at the first failure and report why; what
  was already removed stays removed and the rest is retained. Only the
  deadline case (and a transient I/O error) converges on a later retry: a
  foreign-owned entry, a root that no longer matches the pin, or an invalid
  name fails the same way every time and stays for inspection.
- It streams entries per `getdents` chunk instead of loading a whole directory
  listing into memory, and hoists deep subdirectories up to be processed from
  the root, so removal is not bounded by depth or by how large any one
  directory is.
- On the already-verified, owner-checked inode it may add the missing owner
  rwx bits (e.g. 0555 becomes 0755) so it can traverse and unlink a Go module
  cache's read-only directories as their owner: the chmod goes through the
  inode's own `/proc/self/fd/N` magic link (`addOwnerBits`), never the
  entry's own pathname, and the inode is re-checked against the same
  dev/ino/mode afterwards. This is what `os.RemoveAll` cannot do as the owner of a
  directory it made read-only.
- Removal is bounded by a caller-supplied deadline; hitting it stops the walk
  at the first failure and keeps whatever has not yet been removed (what the
  walk already removed stays removed, nothing is rolled back), reporting
  "deadline" so a retry converges rather than blocking forever: the tree may
  be left partly removed; a later RemoveBy resumes and converges.

### Cleanup belongs to the supervisor, not cmdsandbox

The `uzi-codex-supervisor` binary, already the trusted, statically linked,
root-owned 0555 process-supervision anchor for the Codex launcher (see
`agent/codex/supervisor/doc.go`), now also owns the per-command tmp's whole
lifecycle:

- It creates `/tmp/uzi-codex-command-<token>` itself, before fork, through a
  no-follow fd on `/tmp`, and pins the directory's device/inode.
- It holds `LOCK_EX` on that directory's fd (close-on-exec, so no descendant
  inherits it) for its own lifetime: this is the liveness signal the startup
  orphan reaper reads (below).
- It removes the tmp only **after** a confirmed ECHILD+`__WALL` drain (the
  same process-tree-empty proof the supervisor already established for its
  core reap responsibility) and only within the drain's own deadline, via
  `internal/safetree` against the pinned fd. The outcome (`removed` or
  `retained`, with a fixed-word reason) is reported in the dispose evidence as
  `tmpCleanup`.

**Why the supervisor and not cmdsandbox.** cmdsandbox is the thing that
*executes inside* the tmp; it is scoped to one command's lifetime, has no
independent knowledge of when the command's whole process tree (including any
detached grandchildren) is actually empty, and is not the process holding the
liveness lock a startup reaper can check. The supervisor already computes the
ECHILD+`__WALL` drain proof for its unrelated core responsibility (confirming
the tracked tree is empty before reporting `dispose`), so ownership of "is it
safe to delete this tmp yet" falls out of a fact the supervisor already knows
and cmdsandbox does not. cmdsandbox therefore only **adopts** the tmp the
supervisor already created; it never creates or removes it. Any uid-10003
process can already delete the tmp's contents (and, without Landlock, the
directory itself); the supervisor instead holds removal RESPONSIBILITY, not
exclusive authority: it is the only process that knows the pin and holds the
liveness lock, and it is the process that proves (ECHILD+`__WALL`) that no
descendant remains before removing, so it, rather than a peer of the command
process it supervises, decides when removal is safe. When that proof fails
(an unconfirmed drain, or a killed supervisor) descendants can outlive it and
the tmp is left for the startup reaper. ("Root-owned 0555" describes the supervisor binary itself, not the
directory it removes.)

Stray file descriptors inherited into the supervisor are marked
close-on-exec (`close_range`, falling back to enumerating `/proc/self/fd`)
before anything else runs; if neither mechanism succeeds, the supervisor
refuses to fork at all rather than risk leaking an fd into an untrusted
command.

### Startup orphan reaper (`--reap-orphans`): a liveness principle that fails closed

A retained tmp or cache (a `retained` dispose, an unconfirmed drain, or the
supervisor/holder being killed outright) needs a second sweep, run once at
worker startup before any run is launched:

- **A held flock always protects a directory from removal.** An *unlocked*
  directory is only a **candidate**: the lock follows the supervisor or
  holder process, not the command descendants that can outlive it (none of
  them inherits the close-on-exec lock fd), so an unlocked name is not proof
  of anything by itself.
- **Deleting a candidate needs a kernel process-table proof**, not just an
  unlocked name: no process in the PID namespace, other than the reaper
  itself, may have the command uid (10003) as its real, effective, saved, or
  filesystem uid. The proof fails closed on every ambiguity it cannot
  resolve: a `hidepid`/`subset` proc mount option that could hide a matching
  process, a parse error reading `/proc/<pid>/status`, a scan that would
  exceed its pid bound, and, critically, any listed pid vanishing between
  being listed and being read (`maxProofScans = 5`: up to 5 scans in total,
  i.e. at most 4 retakes, before giving up). Every
  one of those outcomes means "retain", never "assume dead and remove".
- **The reaper's own exemption is narrow and explicit.** Only the reaper's own
  pid is excluded from the "is anyone still running as this uid" check: not
  its ancestors, because `setpriv` execs into the reaper as the same process
  rather than forking a child, so there genuinely is no ancestor running as
  the command uid to exempt.
- **Stated assumption: the PID namespace's counter has not wrapped since the
  namespace was created.** The kernel allocates pids cyclically from the last
  allocated pid, so a wrap makes a freshly-created child's pid indistinguishable
  from a stale, already-scanned one. This holds for the reaper's one intended
  call site (once, at worker startup, in a fresh container PID namespace,
  before any run has launched anything) and would NOT hold if the reaper were
  ever invoked mid-lifetime after arbitrarily many processes had already been
  forked and reaped. This is why the worker invokes the reaper only at
  startup, never on a timer or between runs.

### Cache placement: `/var/cache/uzi-codex-cmd`, a worker-only emptyDir, not the PVC, the checkout, or the dind workdir

The per-run cache root is a **worker-only emptyDir** on the worker pod (in
addition to any the docker lane adds), `codex-cmd-cache` (mounted only into
the `worker` container, never into seed-nix, dind-init or dind), prepared
0700 owned by uid 10003 by the entrypoint, only under the uid split.

- **Not the `/data` PVC.** The PVC holds durable, trusted state (the bare
  repo, resume state, credentials-adjacent material) across pod restarts and
  reprovisions. A Go module/build cache produced by model-authorized commands
  has none of those properties: it is untrusted, per-run, and disposable, so
  persisting it durably would be paying a durability cost for data that is
  neither trusted nor meant to survive.
- **Not the checkout.** The checkout is the run's own working tree; mixing a
  command cache into it risks the cache leaking into what the run commits, or
  a `docker run -v`/compose bind resolving into cache contents it was never
  meant to see.
- **Not the dind-shared run workdir** (`dindWorkdirDir`, `/data/runner`). That
  volume exists specifically so bind-mount sources under the run's checkout
  resolve inside the dind daemon; the command cache has no reason to be
  reachable from a docker-in-docker daemon, and putting it there would make it
  visible to any container a run starts.
- An emptyDir is the right shape precisely because it is per-pod, worker-only,
  and disappears with the pod: no explicit provisioning step, no separate
  reclaim path, and no growth surviving a pod roll the way the docker tier's
  `dindDataSize` PVC deliberately does for its own, differently-motivated
  reasons (issue #224 M-a).
- No `sizeLimit` on this emptyDir, matching `run-workdir`'s existing rule and
  for the identical reason: a `sizeLimit` is enforced by kubelet **evicting
  the pod**, which is itself an eviction trigger and defeats the point of a
  cache that exists to reduce ephemeral churn (issue #224 / `specs/ai.md`
  §480; not issue #225, which is node image accumulation, unrelated to this
  rule). No container declares a matching `limits.ephemeral-storage` either,
  for the same reason the worker's other ephemeral request carries none.

### The lifecycle: holder process, adoption, exact-directory grant

A uid-10003 `--hold-cache` process runs for the whole run, creating
`<root>/<uuid>/{gomod,gocache,npm}` and holding `LOCK_EX` on that directory for
its lifetime. Commands get `GOMODCACHE`, `GOCACHE`, and `npm_config_cache`
pointed into that per-run directory, while `HOME` and `TMPDIR` stay
per-command as before (unrelated concerns: the tmp is per-command scratch, the
cache is per-run reuse). Commands also get cmdsandbox `--cache` with an
exact-directory Landlock grant to the adopted fd, so a command can use the
cache without a broader filesystem grant than that one directory.

The cache is released at the terminal registry teardown, gated on a **drained
attestation** from the worker: the holder removes its directory only when told
`{"op":"release","drained":true}` with no `drained:false` at all before EOF;
removal happens at EOF (sticky: once a `false` arrives, a later `true` cannot
undo it). If the holder dies
mid-run, later commands in that run simply get no cache (never a crash: the
absence just means no cache reuse for the rest of that run), and
`--remove-cache`, which trusts the worker's own attestation that every
command root using the cache has drained, taking no process-table proof of its
own, since a held lock still means live, runs only once every root has
drained.

### Trust statement

**This is a storage and performance boundary, not a trust boundary.** Every
command root for a given run shares uid 10003; the cache does not add
isolation between commands that did not already exist (or not exist) between
them. In `required` mode, or in `best-effort` mode on a kernel that offers Landlock,
the exact-directory grant isolates one run's cache from another's; only when
Landlock is unavailable (the best-effort degrade) is there no such isolation,
and the cache must be treated as **untrusted**: no secret is ever written there, its
contents are never reused across runs (each run gets a fresh random uuid
directory and it is torn down at that run's end), and nothing that comes out
of it is treated as trusted output: it is disposable build-tool cache
content, nothing more.

### Requests deliberately unchanged

`workers.ephemeralRequest` (512Mi, plain) and `workers.docker.ephemeralRequest`
(4Gi, docker) are NOT raised for this change. The cache is a real new
consumer of a worker pod's ephemeral-storage budget, but issue #1598 requires
measuring a Go-heavy Codex run's actual peak and retention on hosted workers
before that number can be picked correctly, and that hosted measurement is
still pending. `e2e/codex-tmp-measure/` has landed, but it is a boundary-level
proxy against the same primitives run outside a hosted worker; it cannot
measure a real hosted-worker Codex run, so the hosted measurement stays a
pending post-deploy maintainer check. The proxy did measure a ~957 MiB
per-run cache peak for one `go build`/`test` of `api/` on a cold cache, above
the unchanged 512Mi plain-tier request; the request is left unchanged pending
the hosted measurement. Leaving the requests as they are means a Go-heavy
Codex run may exceed the current budget; because a request only **ranks** for
eviction and never **limits** (this repo's existing conservative posture from
issue #224, and no container here declares an ephemeral limit), the practical
effect of running over budget is a worse eviction rank under node pressure,
never a new failure mode.

### Rollout

Both the plain and docker worker pod specs now render the `codex-cmd-cache`
volume and mount unconditionally (the same rendered pod shape for every
worker, split or not, so enabling the uid split later never forks the spec on
this axis); this changes every hosted worker's pod spec hash, so every hosted
worker pod rolls exactly once on this change, gated the same way every other
worker roll is: an idle worker rolls straight away, while a busy worker is
cordoned and drained before `Recreate`, up to the configurable
`workers.drainDeadline` (24h by default) or a force-roll, either of which
requeues its runs (see ADR-422). Compose needs no
configuration change: with the uid split its per-run cache lives in the
container layer, removed when the run's command roots all drained;
otherwise it is kept until the next container start's reaper, so there is
no analogous emptyDir concept to add.

## Consequences and residuals

- A retained cache (a `drained:false`, a bare EOF, a stdin error on the
  holder, or a crashed run) stays on disk **until the next container start**,
  when the startup orphan reaper's process-table proof allows it to be
  removed. It is never garbage-collected mid-lifetime.
- Tmp removal that hits the drain deadline is reported "retained: deadline"
  and keeps its partial progress; the tree is picked up by the same startup
  reaper pass on the next container start.
- The reaper's correctness depends on the PID-namespace pid-wrap assumption
  holding: true at its one call site (worker startup, before any run), false
  if it were ever invoked later in the container's lifetime.
- Landlock isolation between two runs' caches is **best-effort**, not
  guaranteed: best-effort mode applies Landlock whenever the kernel offers it,
  and only an unavailable kernel runs unconfined; only `required` mode then
  refuses to run at all. This is why the cache is explicitly a
  non-trust-boundary (above), not an incidental gap.
- **A command process in one run can kill another run's cache holder**: all
  command roots share uid 10003, and nothing prevents one uid-10003 process
  from signalling another. This is an availability cost only (the victim
  run loses caching for its remainder, next commands simply re-populate a
  fresh cache or fall back to no cache), never a confidentiality or integrity
  break, because the cache carries no secrets and its retained/removed state
  is idempotent and disposable either way.
- The hosted-worker measurement that would justify raising the ephemeral
  requests is explicitly **pending**, not delivered by this issue.
