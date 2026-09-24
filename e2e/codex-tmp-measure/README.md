# codex-tmp-measure — PRD #1598 boundary measurement

**Boundary-level proxy, not the hosted Codex-run measurement.** Measures the
real `/tmp` residue, per-run Codex command cache behavior, and Go module
download traffic comparing pre-#1598 (`f77d3103`) against the #1598 branch
(`e648d16c`, milestones M1-M5: `internal/safetree` and supervisor-owned
command tmp from M1/M2, the per-run cache holder from M5), by driving the
**real** `uzi-codex-supervisor` and `uzi-codex-command-sandbox` binaries, as
uid 10003, over a scripted Go-heavy command sequence against a given git ref.
The hosted measurement (the real k8s worker fleet, the real
`agent/templates/*` image, a real Anthropic/Codex-mediated run) remains a
**post-deploy maintainer check**, out of scope for this script.

## What this measures

For a given ref, `run.sh`:

1. builds an image containing that ref's supervisor + sandbox binaries and a
   writable copy of this repo's `api/` module;
2. feature-detects whether that ref's supervisor supports the `--hold-cache`
   standalone mode (added at PRD #1598 M5); if it does, starts one cache
   holder for the whole run and points
   `GOMODCACHE`/`GOCACHE`/`npm_config_cache` at it;
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
   modules actually fetched over the network for that command); per command
   it also records the child's own exit code (`child_exit_code`, from the
   evidence log's `child_exit` event, `null` if that event was never
   observed), the supervisor process's own exit status (`sup_rc`), and the
   command's wall-clock duration (`dur_ms`) — so a fast command can never be
   silently misread as a fast, warm-cache success when it was in fact a fast
   failure or a timed-out dispose;
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

- **No read-only root, no seccomp profile, and a capability set that is
  hand-picked for this harness rather than the worker's own.** The real
  worker image runs its entrypoint under `--read-only` plus a seccomp
  profile (see `e2e/codex-m3a/run-lifecycle.sh` for the production
  confinement posture); this fallback runs a plain root-started container
  that DOES apply `--cap-drop ALL` plus a small, hand-picked `--cap-add`
  set (see below) — a real drop, just not the one the k8s pod spec applies
  in production. The supervisor's OWN pre-fork profile verification
  (subreaper, nondumpable, capability/no-new-privs checks) is real and
  unrelaxed either way — that part is not proxied.
- **No nix-provisioned Go toolchain version pin.** The real worker's Go
  toolchain (used for a Go-heavy Codex run) comes from a pinned nix closure;
  this fallback uses whatever `go` `golang:1.26-alpine` ships. `GOTOOLCHAIN`
  is forced to `local` for every measured command (see below) specifically
  so this version difference cannot inject an extra, unrelated download into
  the numbers this script actually records: `du -sb` of the per-run cache
  directory (peak and after release) and the count of `go: downloading`
  lines in each command's output (see "What this measures" above — there is
  no separate byte-accounted module-download total).
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

- **The environment is inherited, not replaced.** The real launcher
  (`agent/src/codex/launcher.ts`) spawns the supervisor with a fully
  **REPLACED** environment: a fixed `PATH`, `LANG=C`, and a private
  `HOME`/`TMPDIR`/XDG tree per command, plus the cache variables — never a
  merge of the worker process's own environment. This harness's
  `measure-inner.sh` instead runs the supervisor as a normal child of the
  golang-image shell, so it **inherits** that image's whole environment
  (`PATH`, locale, etc.) and only explicitly overrides the handful of
  variables it cares about (`HOME`, `TMPDIR`, `GOPATH`, `GOTOOLCHAIN`, and,
  on the branch, `GOMODCACHE`/`GOCACHE`/`npm_config_cache`). This does not
  affect the tmp/cache boundary being measured (the supervisor and sandbox
  binaries only look at the variables they define), but it means the
  harness's env is not a faithful proxy of production's locked-down env.
- **The cache root path differs from production.** This harness uses
  `/cache` as the cache root (`CACHE_ROOT=/cache` in `measure-inner.sh`);
  production uses `/var/cache/uzi-codex-cmd` (see
  `agent/codex/supervisor/cmdsandbox/cache_test.go`). Only the path differs;
  the cache-holder/`--cache` mechanics exercised are the same regardless of
  root.
- **The `GOPATH` pin matches production's default, it does not bypass it.**
  `measure-inner.sh` pins `GOPATH=$HOME/go` under the ephemeral per-command
  `HOME` on every command. Production never sets `GOPATH` at all, so Go
  falls back to its own default of `$HOME/go` under production's private,
  per-command `HOME` tree — the same effective path. The pin here exists
  only to defeat the alpine base image's own baked-in `GOPATH=/go`, which
  would otherwise leak a free, shared, unmeasured module cache across every
  command regardless of ref; it is not a deviation from what production
  does.
- **`GOTOOLCHAIN=local`, and its rationale understates the branch's real
  benefit.** `api/go.mod` pins `toolchain go1.27.1`; without this override,
  the FIRST `go build` of every command (even on the cached branch path)
  would additionally try to download that toolchain (tens of MB). That
  download is NOT purely a one-time cost orthogonal to what M5 measures:
  Go stores a downloaded toolchain under `GOMODCACHE` (a `golang.org/toolchain@...`
  module), so on the branch ref, once the per-run cache holder is warm, a
  downloaded toolchain would persist in the SAME per-run cache directory
  the branch already keeps warm across commands, but base has no persistent
  cache at all, so it would re-download the toolchain on EVERY command. That
  means command 1 (cold, on both refs) would be unchanged either way — the
  toolchain download is a one-time cold cost paid on the first command
  regardless of `GOTOOLCHAIN` — while the gap between refs would instead
  GROW across commands 2-5 (branch: paid once, then cached; base: paid
  again every command), not stay "bigger on a cold run and identical
  thereafter" as an earlier draft of this note claimed. Forcing
  `GOTOOLCHAIN=local` removes that source entirely so the numbers in
  `RESULTS.md` isolate the module/build cache boundary alone. Whether the
  real boundary this script measures is therefore understated relative to
  production is conditional, not automatic: only if a production run's
  nix-pinned toolchain does NOT already satisfy `api/go.mod`'s `toolchain
  go1.27.1` pin would production also pay (and, on the branch, cache) a
  toolchain download this fallback's override suppresses; if production's
  pinned toolchain already matches, no such download happens there either,
  and this override changes nothing relative to production.
- **The capability set differs from production's residue, not just from a
  hand-picked default.** `run.sh` passes `--cap-drop ALL --cap-add SETUID
  --cap-add SETGID --cap-add SETPCAP --cap-add CHOWN --cap-add DAC_OVERRIDE
  --cap-add FOWNER`. SETUID/SETGID are what `setpriv --reuid/--regid` need
  to drop from root to uid 10003; **SETPCAP is required for `setpriv
  --bounding-set -all` to actually clear the bounding set** (without it,
  `setpriv` silently leaves the bounding set unchanged — confirmed by
  inspecting `/proc/self/status` inside the container during development —
  which the supervisor's own pre-fork profile check then correctly refuses
  with `profile:capBnd`, since neither a fully-empty nor an exact
  SETUID|SETGID-residue bounding set was achieved). CHOWN/DAC_OVERRIDE/FOWNER
  are only for root's own setup step (copying `api/` to a uid-10003-owned
  tree, creating the 0700 cache root); they are dropped again for every
  actual supervisor invocation via that same `--bounding-set -all`
  (confirmed empty in this harness, `CapBnd: 0000000000000000`).
  **Production's own pre-fork profile check instead expects, and accepts, a
  residual SETUID|SETGID bounding set (`0xc0`)** left by its
  controller-only setuid/setgid entrypoint (see
  `agent/codex/supervisor/profile.go`'s `capBndSetuidSetgidResidue`
  constant and `doc.go`'s note on the entrypoint's controller-only
  SETUID/SETGID). This harness's fully-empty bounding set is therefore
  STRICTER than production's actual bounding set, not merely "not the real
  worker's" — the supervisor's profile check treats both as passing, but
  they are not the same posture.
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
resolved commit sha AND a per-invocation id from its throwaway worktree's own
`mktemp` suffix, lowercased because Docker image names must be lowercase, e.g. `m1598-img-e648d16c-e648d16cd48f-a1b2c3`, never just the
sha, so two invocations at the same ref never share an image/container name
and one's `UZI_M1598_KEEP=1` image can never be retagged/removed by the
other) and removes both on exit unless `UZI_M1598_KEEP=1`. **This does not
mean nothing else is touched.** Two things persist outside the throwaway
worktree and the uniquely-named `m1598-*` image/container, and neither is
cleaned up by
`run.sh`: the `golang:1.26-alpine` base image, pulled (and cached) by the
docker daemon the first time any ref is measured, and BuildKit's own build
cache/layer cache for the intermediate build stages, both of which persist
in the docker daemon's local storage across invocations and across repo
checkouts. Reclaim them with the ordinary docker commands
(`docker image rm golang:1.26-alpine`, `docker builder prune`) if disk needs
to be recovered.

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
  actually finished — but `child_exit` alone only proves the command
  finished, not that it succeeded: `measure-inner.sh` also pulls the
  `code` field off that same evidence line (before the log is deleted) into
  each result row's `child_exit_code`, so a fast *failure* can never be
  misread as a fast, warm-cache success from duration alone.
- **This harness's own scratch files (FIFOs, evidence/output/holder logs)
  live under `/work/m1598-logs`, never `/tmp`.** `/tmp` is the exact
  directory this script measures residue in; a harness bookkeeping file
  placed there would count as if the supervised commands themselves left
  it behind. Only the sandboxed command's own `--tmp` directory
  (`/tmp/uzi-codex-command-<token>`) is deliberately under `/tmp` — that
  one IS what is being measured.
- **The cache holder's stderr is captured to its own file, never merged
  into the log this script parses.** Production's own launcher only ever
  reads the holder's stdout NDJSON; merging stderr in as this harness
  originally did risked a stray diagnostic line being read back as a real
  evidence line. `measure-inner.sh` also runs every log line spliced into
  its own NDJSON output through a `safe_embed_json` check (must look like a
  single-line `{...}` JSON object, else emitted as the literal `null`), so
  a malformed or partial line from either log can never corrupt the
  NDJSON row this script itself emits.
