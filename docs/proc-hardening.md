# Worker `/proc` isolation: the structural residual and its close

This documents the M3/M4 accepted residual around `/proc/<pid>/environ`, what M6
investigated, the partial hardening M6 landed in the compose MVP, and the
concrete pod design that closes it fully on the remote-worker (k8s/VM) phase.

## The residual

The uzi worker and the agent it drives run in the **same container, under the
same uid** (`uzi`). The worker spawns the Claude Agent SDK as a subprocess and
controls it in-process (abort, group-kill, session dir, pid capture). Because
they share a uid, a survivor the agent leaves behind (a `setsid`-escaped
process that outlives the per-turn `abort()` and the done-path group-kill) can
read the worker's own `/proc/<worker-pid>/environ` and any transient git child's
`/proc/<child-pid>/environ`:

- **Join token.** Historically delivered as the `UZI_WORKER_TOKEN` env var, so it
  sat in the worker's `environ` for the whole process lifetime. Reading it lets
  an attacker impersonate the worker against the API (claim runs, receive PATs).
  Redacted from every run message, but present in `environ`.
- **Bot PAT.** Never in the worker's persistent env. It enters a git **child's**
  env only during the push/MR window. M4 audit finding B1 closed the common case
  by group-killing the whole agent process tree *before* the PAT-bearing push, so
  a normal backgrounded survivor is dead first. A `setsid`-escaped survivor in
  its own session is not reached by that group-kill and could still race the
  short push window.

No in-container string guard can prevent a same-uid process from reading `/proc`
(a symlink to `/proc`, `/proc/<pid>/mem`, `ptrace`, etc.). The robust close is
**structural**: the agent must not share a uid with the worker's own process, or
`/proc` must hide other pids, or the agent must run in a stronger sandbox.

## What M6 investigated (and the honest finding)

The compose MVP runs the worker container **non-root** (`USER uzi`) with
`cap_drop: ALL` and `no-new-privileges: true`. Against that posture:

| Structural close | Needs | Feasible in the MVP? |
| --- | --- | --- |
| Agent under a **different uid** than the worker (so `/proc/<worker>/environ`, mode `0400` owned by the worker uid, is unreadable to the agent uid) | `CAP_SETUID`/`CAP_SETGID` (a `su-exec`/`gosu` drop), or start as root | **No** — dropped by `cap_drop: ALL`; the container starts as `uzi` and cannot gain a second uid |
| `/proc` mounted **`hidepid=2`** (other pids invisible) | remount `/proc` → `CAP_SYS_ADMIN` + a private mount namespace; no per-service compose knob exists | **No** — needs privilege the posture forbids |
| **gVisor / `runsc`** runtime (syscall interception, no host `/proc`) | a runtime the Docker daemon is configured with (`--runtime=runsc`) | **No** — daemon-level, not a compose-file property |
| Dedicated **agent sidecar** container with its own uid + shared data volume | an IPC protocol between the worker and the out-of-process agent (the worker currently spawns and controls the SDK in-process) | **Not in M6** — an architectural redesign, tracked for the remote-worker phase |

**Conclusion:** a clean structural close is **not achievable** in the non-root,
`cap_drop: ALL` compose MVP without breaking that very posture (every option above
either re-adds a capability or requires daemon/runtime privilege). Per the M6
brief we did **not** force a privileged hack. Instead:

## What M6 landed (clean, in-posture)

**The join token is now delivered by file, not by environment variable.**

Set `UZI_WORKER_TOKEN_FILE` to a path and the worker reads the token once at
startup, then **unlinks the file** (best-effort — a read-only secret mount can't
be unlinked, and that's fine). The token then lives only in the worker's process
**heap**, which a same-uid descendant **cannot** read: with the container's
`cap_drop: ALL` there is no `CAP_SYS_PTRACE`, and the default `yama`
`ptrace_scope` forbids a child from `ptrace`-attaching to (or reading
`/proc/<parent>/mem` of) its ancestor. So:

- the join token is **absent from every process `environ`** (it was never an env
  var), closing the persistent `/proc/<pid>/environ` leak that the residual named;
- with the file unlinked it is **not on disk** for the agent's Bash to `cat`
  either;
- backward compatible: `UZI_WORKER_TOKEN` (env) still works when the file var is
  unset, so nothing existing breaks.

**Where it is wired (shipping stack vs test overlay):**

- **The shipping stack** (`docker-compose.yml`, `--profile agent up`) delivers the
  token as a **compose file secret**: `UZI_WORKER_TOKEN` (from `.env`) is mounted
  at `/run/secrets/worker_token` and the agent reads it via `UZI_WORKER_TOKEN_FILE`
  — so the token is **absent from the container `environ`**. Compose secret mounts
  are **read-only**, so the worker's post-read unlink *fails and the file persists*
  at `/run/secrets/worker_token`. The **environ win still holds** (the leak the
  residual named is the `/proc/<pid>/environ` read, which is closed); the on-disk
  copy is a lesser, documented residue that the full structural close below also
  covers.
- **The E2E overlay** (`e2e/docker-compose.e2e.yml`) delivers the token via a
  **writable** bind-mounted file, so there the unlink also succeeds and **both**
  the environ and the on-disk vectors are closed. The harness (`e2e/run-e2e.sh`)
  asserts the token appears in **zero** process `environ`s and that the delivery
  file was unlinked.

See `agent/src/config.ts` (`resolveWorkerToken`), the `secrets:` block in
`docker-compose.yml`, and `e2e/docker-compose.e2e.yml`.

**What this does NOT close:** the **PAT** transient-window vector (a
`setsid`-escaped survivor racing the push) is unchanged — the PAT is a git
*child's* env, not the worker's, so file-delivery does not apply. That vector
still needs one of the structural closes below and remains the honest residual
for the remote-worker phase.

## The full close: remote-worker (k8s/pod) design

On the remote-worker phase the worker runs as a pod, where the uid split and
`/proc` hiding the MVP cannot express become first-class:

1. **uid split across two containers in one pod (preferred).** A `worker`
   container (holds the join token + PAT, does all authenticated git/MR) and an
   `agent` container (runs the SDK) in the **same pod**, sharing only the
   worktree via an `emptyDir`:

   ```yaml
   spec:
     shareProcessNamespace: false        # agent cannot see worker pids at all
     containers:
       - name: worker
         securityContext: { runAsUser: 10001, runAsNonRoot: true,
                            allowPrivilegeEscalation: false,
                            capabilities: { drop: ["ALL"] },
                            readOnlyRootFilesystem: true }
       - name: agent
         securityContext: { runAsUser: 10002, runAsNonRoot: true,   # DIFFERENT uid
                            allowPrivilegeEscalation: false,
                            capabilities: { drop: ["ALL"] } }
     volumes: [ { name: worktree, emptyDir: {} } ]
   ```

   With `shareProcessNamespace: false` the agent container has its own pid
   namespace and cannot see, `ptrace`, or read `/proc` of the worker container at
   all — the environ and `/proc/<pid>/mem` vectors (join token **and** PAT) are
   both structurally closed. Requires making the worker↔agent boundary an IPC
   one (the redesign noted above).

2. **`hidepid=2` on `/proc` — only as a COMPLEMENT to the uid split, not alone.**
   Important caveat: `hidepid=2` hides the `/proc` entries of *other users'*
   processes; it does **not** hide same-uid processes from each other. So with a
   same-uid worker+agent it buys nothing — the agent still sees the worker's
   `/proc/<pid>/environ`. It is only useful **once the uids differ** (option 1),
   where it additionally hides the worker's entire `/proc` entry from the agent
   (not just makes `environ` unreadable via the 0400 perm). Do **not** rely on
   `hidepid` as a standalone same-uid mitigation.

3. **gVisor (`runtimeClassName: gvisor`).** Run the whole pod under `runsc`:
   `/proc` is a gVisor-synthesized, sandbox-local view and host-level `/proc`
   reads / `ptrace` do not reach another real process. Defense in depth on top of
   1 or 2.

**Recommendation:** option 1 (pod-level uid split with a non-shared pid
namespace) is the clean close and the one to build when the worker moves to
pods; it closes both the join-token and the PAT vectors at once. Until then the
MVP keeps the join-token file hardening above plus the M4 B1 group-kill, with the
PAT `setsid`-race documented as the remaining, accepted residual.
