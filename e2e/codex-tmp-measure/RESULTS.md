# PRD #1598 M5 boundary measurement — results

**Date:** 2026-09-24
**Host kernel:** `Linux 6.1.83-4.ph5 #1-photon SMP PREEMPT_DYNAMIC` (x86_64)
**Image mode:** **fallback** (`Dockerfile.fallback`: `golang:1.26-alpine` +
the real `uzi-codex-supervisor`/`uzi-codex-command-sandbox` sources at each
ref, vendored, no nix/devbox). See `README.md` for why the real
`agent/templates/base/Dockerfile` was not used (disk budget) and exactly
what that substitution does and does not proxy.
**Landlock mode actually in effect:** `--mode best-effort` was passed on
every command (never `required`); the sandbox's own `--probe` returned
**rc=10 (unavailable — ENOSYS/EOPNOTSUPP)** on this kernel/container setup,
so every command ran **unconfined** (no filesystem-confinement layer; uid
isolation and the ephemeral-tmp/cache adoption checks still applied in
full). Identical on both refs (same host, same fallback base image).

**Refs measured:**

| Label | Ref | Resolved sha |
|---|---|---|
| base | `f77d31036236289a45abe9aef6858a8c3aaa68e6` | `f77d3103` |
| branch | `agent/issue-1598` @ HEAD | `e648d16c` |

**Command sequence (N=5 per ref):** for each command, the real supervisor +
sandbox ran, as uid 10003: `--expect-uid 10003 --cleanup-token <uuid> --
uzi-codex-command-sandbox --root <workdir> --tmp
/tmp/uzi-codex-command-<uuid> --cwd <workdir> [--cache <cacheroot>/<runuuid>]
--mode best-effort -- /bin/sh -c 'cd api && go build ./... && go test
-count=1 -run XXX_none ./...'` against this repo's `api/` module (compiles +
links everything; the `-run XXX_none` pattern matches no test, so zero
test-execution time). `GOTOOLCHAIN=local` was forced on every command (see
README.md); `GOPATH` was pinned under the ephemeral per-command tmp on every
command (also see README.md — the alpine base image's own baked-in
`GOPATH=/go` would otherwise silently share one cache across everything).

Exact commands run to produce this file:

```
./run.sh f77d31036236289a45abe9aef6858a8c3aaa68e6 > base.ndjson   2> base.log
./run.sh e648d16c                                 > branch.ndjson 2> branch.log
```

(N defaulted to 5.)

## Per-command results

### base (`f77d3103`) — no `--hold-cache` support at this commit (feature-detected: `cache_support.supported=0`)

| cmd | duration | /tmp bytes (`du -sb /tmp`) after | `uzi-codex-command-*` dirs left in /tmp | cache dir bytes | `go: downloading` lines |
|---|---|---|---|---|---|
| 1 | 74.28 s | 175,010,891 (166.9 MiB) | 1 | 0 (no cache support) | 78 |
| 2 | 80.83 s | 350,012,023 (333.8 MiB) | 2 | 0 | 78 |
| 3 | 81.17 s | 525,013,149 (500.7 MiB) | 3 | 0 | 78 |
| 4 | 82.87 s | 700,014,275 (667.6 MiB) | 4 | 0 | 78 |
| 5 | 76.85 s | 875,015,401 (834.6 MiB) | 5 | 0 | 78 |
| **final** (after all 5) | — | **875,005,630 (834.5 MiB)** | **5** | 0 | **390 total** |

Every single command re-downloaded all 78 modules (no cache exists at this
commit), and — the more striking result — **every command's ephemeral tmp
directory was RETAINED, never removed**, so `/tmp` residue grew linearly by
~167 MiB per command with no upper bound over a long-running worker's
lifetime. Root cause, confirmed by reading the code at this commit: at
`f77d3103` the supervisor (`agent/codex/supervisor`) has no
`commandtmp.go`/`internal/safetree` at all — the sandbox process itself
(`cmdsandbox/main.go`) both creates (`os.Mkdir`) and cleans up
(`defer os.RemoveAll(tmp)`) its own `--tmp` directory, with no permission
restoration first. `go`'s module cache extracts modules with **read-only**
directory permissions (a deliberate Go behavior, the same reason
`go clean -modcache` exists instead of a plain `rm -rf`) — and this
commit's `HOME`/`GOPATH` point INSIDE that same ephemeral `--tmp`, so
`os.RemoveAll` silently fails on the read-only module-cache subtree it just
populated, leaving the directory (and everything under it) behind. This is
specific to this exact base commit's era (a supervisor with
`commandtmp.go`+`internal/safetree` — confirmed present starting one of the
intermediate M4 commits, well before M5 — uses a permission-restoring
removal and would very likely not exhibit this particular residue, even
without M5's cache relocation); it is reported here as measured, at the
exact base commit given, not as a general claim about every pre-M5 state.

### branch (`e648d16c`, #1598 M5) — `--hold-cache` supported (feature-detected: `cache_support.supported=1`)

Cache holder ready at `/cache/<run-uuid>` before the first command; released
(`{"op":"release","drained":true}`) after the last.

| cmd | duration | /tmp bytes after | `uzi-codex-command-*` dirs left in /tmp | cache dir bytes | `go: downloading` lines |
|---|---|---|---|---|---|
| 1 | 80.40 s | 9,887 | 0 | 1,004,013,728 (957.7 MiB) | 78 |
| 2 | 9.68 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 3 | 10.09 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 4 | 8.78 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 5 | 8.83 s | 5,683 | 0 | 1,004,013,728 | 0 |
| **final** (after release) | — | **0** | **0** | **peak 1,004,013,728 (957.7 MiB); retained: NO — holder reported `cache_cleanup state=removed`** | **78 total** |

Every command's ephemeral `/tmp` residue is fully removed by the supervisor's
own `tmpCleanup` (`{"state":"removed","reason":""}` on every dispose) — it
never contains module-cache files at all, since `GOMODCACHE`/`GOCACHE` are
pointed at the separate `--cache` directory instead. Only the FIRST command
pays the module-download cost (78 `go: downloading` lines, ~958 MiB of
module cache built up); commands 2-5 hit the warm cache and each finish in
under 11 seconds, a ~8-9x speedup over base's ~75-83 s per command. At the
end of the run the cache holder is released with `drained:true` and the
supervisor's own attestation removes the whole per-run cache directory —
nothing is left behind.

## Summary: what M5 changes, as measured here

| Metric (N=5 commands) | base (no cache) | branch (M5 cache holder) |
|---|---|---|
| Total wall time for 5 commands | ~396 s (74-83 s each) | ~118 s (80 s cold + 4× ~9 s) |
| Total `go: downloading` lines | 390 (78 × 5, every command) | 78 (only command 1) |
| `/tmp` residue after 5 commands | **875 MB, growing without bound** | **0 bytes** |
| `uzi-codex-command-*` dirs left in /tmp | 5 (never cleaned) | 0 (cleaned after every command) |
| Per-run cache dir | none | peak 958 MiB, **not retained** after release |

## What this run could NOT measure (and where that IS covered)

- **The real hosted worker fleet's behavior** (the real `agent/templates/base`
  image, real k8s pod capability/seccomp posture, a real Codex-mediated run
  rather than a scripted `go build`/`go test`). That is explicitly a
  **post-deploy maintainer check**, out of scope for this boundary-level
  script (see README.md).
- **Landlock filesystem confinement itself** (this kernel/container setup
  reports it unavailable — probe rc=10 — so every command ran unconfined;
  the uid-isolation and tmp/cache-adoption checks are unaffected and were
  fully exercised).
- **A production-scale command sequence** (many more than 5 commands, other
  toolchains e.g. `npm`, concurrent runs sharing one worker). The mechanism
  measured here (persistent per-run `GOMODCACHE`/`GOCACHE`, cleaned
  ephemeral tmp) is expected to scale the same way; only the exact byte/time
  numbers would differ.

## Bugs/observations noticed while building this harness (not part of the measurement)

- The base commit's (`f77d3103`) unbounded `/tmp` growth documented above is
  a genuine, reproducible behavior at that exact commit, not a harness
  artifact — confirmed by reading `cmdsandbox/main.go` at that commit (naive
  `defer os.RemoveAll(tmp)`, no permission restoration, and `HOME`/`GOPATH`
  both pointed inside the same ephemeral tmp by the harness, matching how a
  real Go-heavy Codex command would be run). It is not a **branch** bug —
  the branch's `internal/safetree`-based removal (present independently of
  M5) already restores permissions before removing, and M5 additionally
  moves the module/build cache out of the ephemeral tmp entirely.
