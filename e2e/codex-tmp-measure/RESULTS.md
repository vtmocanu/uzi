# PRD #1598 boundary measurement — results

**This is a BOUNDARY-LEVEL PROXY, not the hosted Codex-run measurement.** It
drives the real supervisor/sandbox binaries against a scripted Go build/test
sequence on a minimal fallback image; the hosted measurement (real k8s worker
fleet, real `agent/templates/*` image, a real Anthropic/Codex-mediated run)
remains a **post-deploy maintainer check**, out of scope here. See the "proxy
caveat" in README.md before reading any number below as production truth.

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
| branch | `agent/issue-1598`, HEAD at measurement time | `e648d16c` |

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
| 5 | 76.85 s | 875,015,401 (834.48 MiB) | 5 | 0 | 78 |
| **final** (after all 5) | — | **875,005,630 (834.48 MiB)** | **5** | 0 | **390 total** |

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
specific to this exact base commit's era (a pre-#1598 commit with no
supervisor-owned tmp at all): `internal/safetree` (M1, `7f1ba455`) and the
supervisor taking ownership of the command tmp with a permission-restoring
removal (M2, `9e5ef5fe`) both land after this base commit, so the residue
reported here is exactly what M1/M2 fix, not something introduced or
removed later in the #1598 branch; it is reported here as measured, at the
exact base commit given, not as a general claim about every pre-#1598 state.

### branch (`e648d16c`, #1598 branch, M1–M5) — `--hold-cache` supported (feature-detected: `cache_support.supported=1`)

Cache holder ready at `/cache/<run-uuid>` before the first command; released
(`{"op":"release","drained":true}`) after the last.

| cmd | duration | /tmp bytes after | `uzi-codex-command-*` dirs left in /tmp | cache dir bytes | `go: downloading` lines |
|---|---|---|---|---|---|
| 1 | 80.40 s | 9,887 | 0 | 1,004,013,728 (957.50 MiB) | 78 |
| 2 | 9.68 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 3 | 10.09 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 4 | 8.78 s | 5,683 | 0 | 1,004,013,728 | 0 |
| 5 | 8.83 s | 5,683 | 0 | 1,004,013,728 | 0 |
| **final** (after release) | — | **0** | **0** | **peak 1,004,013,728 (957.50 MiB); retained: NO — holder reported `cache_cleanup state=removed`** | **78 total** |

The small non-zero `/tmp` byte counts on this ref (9,887 bytes after command
1, 5,683 bytes after commands 2-5) are **not** residue left by the supervised
commands — every `uzi-codex-command-*` dir count is 0, meaning the
supervisor's own tmp cleanup left nothing behind. Their attribution to **the
harness's own files** (the release FIFO/log and, per command, the
control/evidence FIFOs and their log/output files that `measure-inner.sh`
placed directly under `/tmp`: `m1598-hold-*`, `m1598-ctl-*`, `m1598-ev-*`,
`m1598-evlog-*`, `m1598-cmdout-*`) is **inferred** from reading the version of
`measure-inner.sh` that produced this run, not observed directly (no
per-file `du` breakdown of `/tmp` was captured at measurement time — only the
aggregate `du -sb /tmp` byte counts in the table above). See "Harness files
under /tmp" below; `measure-inner.sh` has since been changed to place these
files under `/work` instead, so a re-run is **expected** to show 0 bytes
here rather than this residue — this has not itself been re-measured.

Every command's ephemeral `/tmp` residue is fully removed by the supervisor's
own `tmpCleanup` (`{"state":"removed","reason":""}` on every dispose) — it
never contains module-cache files at all, since `GOMODCACHE`/`GOCACHE` are
pointed at the separate `--cache` directory instead. Only the FIRST command
pays the module-download cost (78 `go: downloading` lines, ~958 MiB of
module cache built up); commands 2-5 show no `go: downloading` lines and
finish in under 11 seconds, consistent with a warm cache (this is an
inference from duration and the absence of download lines, not a direct
"cache hit" signal — see the exit-code caveat below for why child_exit alone
cannot rule out a fast failure), a ~7.4–9.4x speedup over base's ~75-83 s per
command. At the end of the run the cache holder is released with
`drained:true` and the supervisor's own attestation removes the whole
per-run cache directory — nothing is left behind.

**Exit code was not recorded for these runs.** `measure-inner.sh` did not
emit the command's exit code (`child_exit`'s code, present in the evidence
log) into the NDJSON result row at the time these numbers were captured; the
evidence log is deleted before this field could be back-filled, so it cannot
be re-derived from these recorded runs. `measure-inner.sh` now emits it
(`child_exit_code` in the `command` event) for future runs, but every row in
this results file predates that field. Practically: a fast command 2-5 could
in principle be a fast *failure* rather than a warm-cache hit; the "warm
cache" reading above rests on duration plus the absence of `go: downloading`
lines, not on a confirmed zero exit code.

## Summary: pre-#1598 (`f77d3103`) vs the #1598 branch (`e648d16c`, M1–M5), as measured here

| Metric (N=5 commands) | base (pre-#1598, no cache, no supervisor-owned tmp) | branch (#1598 branch, M1–M5: safetree + supervisor-owned tmp + cache holder) |
|---|---|---|
| Total wall time for 5 commands | ~396 s (74-83 s each) | ~118 s (80 s cold + 4× ~9 s) |
| Total `go: downloading` lines | 390 (78 × 5, every command) | 78 (only command 1) |
| `/tmp` residue after 5 commands | **834.48 MiB, growing without bound** | **0 bytes** |
| `uzi-codex-command-*` dirs left in /tmp | 5 (never cleaned) | 0 (cleaned after every command) |
| Per-run cache dir | none | peak 957.50 MiB, **not retained** after release |

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

## Harness files under /tmp

The tiny branch-ref residues noted above (9,887 and 5,683 bytes) are
**inferred** to be the harness's own release FIFO/log and per-command
control/evidence FIFOs/logs/output files, which `measure-inner.sh` placed
directly under `/tmp` at measurement time — inferred from the script version
that produced this run, not confirmed by a per-file breakdown captured
during that run. They have since been moved to `/work/m1598-logs` so the
`/tmp` measurement reflects only what the supervised commands themselves
leave behind, with no harness-file noise; the underlying finding (0 bytes
retained on the branch ref, once supervisor-owned tmp cleanup ran) is
unchanged, and a re-run is **expected** to show exactly 0 rather than these
small values — this expectation has not itself been re-measured.

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
