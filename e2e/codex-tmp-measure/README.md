# codex-tmp-measure — PRD #1598 M5 boundary measurement

Measures the real `/tmp` residue, per-run Codex command cache behavior, and
Go module download traffic on either side of PRD #1598 M5 (the per-run Codex
command cache holder), by driving the **real** `uzi-codex-supervisor` and
`uzi-codex-command-sandbox` binaries, as uid 10003, over a scripted Go-heavy
command sequence against a given git ref.

## What this measures

For a given ref, `run.sh`:

1. builds an image containing that ref's supervisor + sandbox binaries and a
   writable copy of this repo's `api/` module;
2. feature-detects whether that ref's supervisor supports the `--hold-cache`
   standalone mode (PRD #1598 M5); if it does, starts one cache holder for
   the whole run and points `GOMODCACHE`/`GOCACHE`/`npm_config_cache` at it;
3. runs N (default 5) commands through the real supervisor + sandbox, each:
   `--expect-uid 10003 --cleanup-token <uuid> -- uzi-codex-command-sandbox
   --root <workdir> --tmp /tmp/uzi-codex-command-<uuid> --cwd <workdir>
   [--cache <cacheroot>/<runuuid>] --mode best-effort -- /bin/sh -c 'cd api
   && go build ./... && go test -count=1 -run XXX_none ./...'` (the `-run
   XXX_none` pattern matches no test, so this is a full compile+link with
   zero test-execution time — the network/disk cost is isolated to the
   toolchain, not the tests);
4. after each command, and once at the end, records `du -sb /tmp`, the count
   of `uzi-codex-command-*` directories left in `/tmp`, the cache directory's
   size, and the count of `go: downloading` lines (a direct measure of
   modules actually fetched over the network for that command);
5. releases the cache holder (`{"op":"release","drained":true}`) and records
   whether the cache directory is removed or retained.

`RESULTS.md` is the base-vs-branch table this produced for issue #1598.

## The proxy caveat — read this before trusting the numbers as production truth

**This is a boundary-level PROXY, not the hosted Codex-run measurement.** The
hosted measurement (the real k8s worker fleet, the real `agent/templates/*`
image, the real Anthropic/Codex-mediated run) is a **post-deploy maintainer
check**, out of scope here. What this script DOES prove: the real
supervisor/sandbox binaries, at the exact sources of the ref under test,
really do create/lock/adopt/remove the ephemeral command tmp and the per-run
cache the way `agent/codex/supervisor/doc.go` documents, under uid 10003,
against a real (if small) Go module graph with real network downloads.

### Deviation from the brief: a FALLBACK minimal image, not the real worker image

The brief's default is to build `agent/templates/base/Dockerfile` (the real
worker image) and drive the two binaries inside it. That Dockerfile also
provisions a full **nix + devbox** toolchain (see its own comments), which on
this box's disk budget (`/data` had ~450 MB free at measurement time; the
Dockerfile pulls a nix installer, a devbox binary and multiple toolchain
closures far exceeding that) does not fit, and pre-provisioning enough disk
was out of scope for this measurement. **`run.sh` therefore always uses the
fallback**: `Dockerfile.fallback` builds the **exact same supervisor +
cmdsandbox sources** at the ref under test (`agent/codex/supervisor`,
including its vendored `golang.org/x/sys` dependency, so the supervisor's own
build needs no network) on a plain `golang:1.26-alpine` base, with the same
three runtime uids the real image creates (`worker` 10001, `runner` 10002,
`runner-cmd` 10003, PRD #51 A1 / PRD #1171 M1). What is proxied, concretely:

- **No read-only root, no seccomp profile, no worker-level capability drop.**
  The real worker image runs its entrypoint under `--read-only` plus a
  seccomp profile (see `e2e/codex-m3a/run-lifecycle.sh` for the production
  confinement posture); this fallback runs a plain root-started container
  with a hand-picked, narrower-than-default `--cap-drop ALL` set (see below).
  The supervisor's OWN pre-fork profile verification (subreaper,
  nondumpable, capability/no-new-privs checks) is real and unrelaxed either
  way — that part is not proxied.
- **No nix-provisioned Go toolchain version pin.** The real worker's Go
  toolchain (used for a Go-heavy Codex run) comes from a pinned nix closure;
  this fallback uses whatever `go` `golang:1.26-alpine` ships. `GOTOOLCHAIN`
  is forced to `local` for every measured command (see below) specifically
  so this version difference cannot inject an extra, unrelated download into
  the "module bytes downloaded" numbers.
- **The docker daemon here may not share a filesystem with the invoking
  shell.** `DOCKER_HOST` pointed at a TCP endpoint during development, and a
  bind-mounted host path silently resolved to an empty directory on the
  daemon's own filesystem — see "Why everything is baked into the image"
  below. `run.sh` therefore takes NO input via bind mount and reports
  results only over the docker CLI's own stdout/stderr, which is a real
  constraint of the harness this ran in, not of the real worker deployment
  (which never spans a docker socket like this).

If a future maintainer HAS the disk budget for the real image, the
comparison worth doing is: swap `Dockerfile.fallback` for a build of
`agent/templates/base/Dockerfile` (reusing the build invocation style in
`e2e/codex-m3a/run-lifecycle.sh`) and re-point `run.sh`'s single `docker
build` call at it; `measure-inner.sh` is unchanged either way, since it only
depends on the two binaries and a writable API checkout being present.

### Other bypasses, named explicitly

- **`GOTOOLCHAIN=local`.** `api/go.mod` pins `toolchain go1.27.1`; without
  this override, the FIRST `go build` of every command (even on the cached
  branch path) would additionally try to download that toolchain (tens of
  MB), which is a fixed one-time cost unrelated to the M5 boundary this
  script measures (the module/build cache, not the toolchain itself). Left
  unset, this would make cross-run timing noisy without changing the
  qualitative base-vs-branch comparison this script exists to make.
- **The capability set is hand-picked, not the real worker's.** `run.sh`
  passes `--cap-drop ALL --cap-add SETUID --cap-add SETGID --cap-add SETPCAP
  --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER`. SETUID/SETGID
  are what `setpriv --reuid/--regid` need to drop from root to uid 10003;
  **SETPCAP is required for `setpriv --bounding-set -all` to actually clear
  the bounding set** (without it, `setpriv` silently leaves the bounding set
  unchanged — confirmed by inspecting `/proc/self/status` inside the
  container during development — which the supervisor's own pre-fork
  profile check then correctly refuses with `profile:capBnd`, since neither
  a fully-empty nor an exact SETUID|SETGID-residue bounding set was
  achieved). CHOWN/DAC_OVERRIDE/FOWNER are only for root's own setup step
  (copying `api/` to a uid-10003-owned tree, creating the 0700 cache root);
  they are dropped again for every actual supervisor invocation via that
  same `--bounding-set -all` (confirmed empty, `CapBnd: 0000000000000000`).
  None of this maps onto the real worker's actual container capability
  set, which is provisioned by the k8s pod spec, not by this script.
- **Landlock is whatever this kernel/sandbox actually offers**, not forced
  either way. `--mode best-effort` is passed (never `required`), and the
  actual probe result is recorded in `RESULTS.md` rather than assumed.

### Why everything is baked into the image (no bind mounts)

During development, `docker version` showed `DOCKER_HOST=tcp://127.0.0.1:2375`
(a sibling/remote daemon), and a `-v <host-path>:<container-path>` bind mount
of a path that did not already exist on the DAEMON's own filesystem silently
resolved to an empty directory there (Docker's normal "auto-create missing
bind source" behavior) — files written into it from the container never
appeared back in the invoking shell's view of that path, and files placed
there by the invoking shell never appeared in the container. Only the docker
CLI's own attached stdout/stderr are a reliable channel back to the caller in
this harness. `run.sh` therefore:

- bakes the API module under test and the driver script into the image at
  **build time** (`COPY`), never a bind mount;
- has `measure-inner.sh` print exactly one NDJSON object per event to
  **stdout**, and all human-readable progress to **stderr** — the two are
  never mixed, so a caller that wants the machine-readable record just
  redirects stdout to a file (this is how `RESULTS.md`'s numbers were
  captured).

A maintainer running this somewhere with a real shared filesystem (a local
`unix:///var/run/docker.sock`) does not need to change anything; the same
invocation just also happens to keep working if a shared filesystem exists.

### The repo's `.dockerignore` and the synthetic build context

The repo-root `.dockerignore` excludes `agent/`, `api/` and `e2e/` (its own
header explains why: it exists for the WEB image's context, which is the
repo root). BuildKit resolves that ignore file for **any** build whose
context is the repo root, not just the web image's own build — so `run.sh`
does NOT build from the ref's checkout root. It instead assembles a small
synthetic context directory containing only
`agent/codex/supervisor/`, `api/`, and this directory's `measure-inner.sh`,
which has no `.dockerignore` of its own to fight.

## How to run it

```
./run.sh <git-ref>                   # e.g. ./run.sh main, a branch, a sha
UZI_M1598_N=5 ./run.sh <git-ref>     # commands per ref (default 5)
UZI_M1598_KEEP=1 ./run.sh <git-ref>  # keep the throwaway worktree + image
```

Prints progress to stderr and one NDJSON line per measured event to stdout;
redirect stdout to a file to keep the machine-readable record, e.g.:

```
./run.sh f77d31036236289a45abe9aef6858a8c3aaa68e6 > base.ndjson 2> base.log
./run.sh e648d16c                                 > branch.ndjson 2> branch.log
```

Each invocation creates its own detached worktree and image (named after the
resolved commit sha, e.g. `m1598-img-e648d16c-e648d16cd48f`) and removes both
on exit unless `UZI_M1598_KEEP=1`. Nothing outside a throwaway worktree and a
uniquely-named `m1598-*` image/container is touched.

### Requirements

- `docker` reachable (build + run); network egress to `proxy.golang.org` (the
  measurement fetches real Go modules on a cold cache — this is the point).
- Enough disk for two ~350 MB fallback images plus their build layers (well
  within a typical CI/dev box's budget, unlike the real worker image).

## Known development quirks (kept as comments in the scripts too)

- **The cache holder's release channel needs a plain write-only FIFO open,
  opened AFTER the holder is backgrounded**, not the same self-open
  read-write (`exec {FD}<>fifo`) trick the command control/evidence channels
  use. The RW self-open measurably does not deliver EOF to the holder's
  `readRelease` loop even after `exec {FD}>&-` — confirmed to hang
  indefinitely during development. The command control (fd 3) and evidence
  (fd 4) channels never need EOF (dispose is answered inline, not by
  closing), so they keep the simpler RW self-open.
- **`golang:1.26-alpine`'s baked-in container env `GOPATH=/go` defeats
  ephemeral-HOME isolation if not explicitly overridden.** Without pinning
  `GOPATH` under the per-command ephemeral tmp, EVERY command (base and
  branch alike) would silently share one fixed, unmeasured, never-cleaned
  module cache at `/go/pkg/mod`, regardless of `--cache` — invisibly
  defeating the whole comparison this script exists to make. `measure-inner.sh`
  pins `GOPATH=$TMPDIR/go` on every command for exactly this reason.
- The polling loop that waits for a command to finish before sending
  `dispose` must NOT also break early on `kill -0 $SUPPID` failing: that
  raced true in development before the evidence line was actually
  flushed/grepped, sending `dispose` while the real `go build`/`go test`
  was still running (visible as a force-killed dispose and a falsely tiny
  duration). Only a `child_exit` line in the evidence log means the command
  actually finished.
