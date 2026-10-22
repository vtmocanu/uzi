# Production provisioning and uxlab rendering fixtures

## M3 matrix (lead-owned expensive proof)

The implementation has focused local checks only. Docker, the four real-image
cells and the separate runner Devbox leg have **not** been run for M3.
**All M3 live proof remains UNPERFORMED:** matrix cells, timing comparison,
offline renders, Nix controls, local Devbox build and PNG visual inspection.
No production runtime, environment builder, permission or sandbox code changes.

Prepare regular full repository exports under this worktree's scratch directory.
The base is frozen at `3fb1fdbf`; the candidate below is M2 at `cb2cc627`.
Use the actual final candidate ref when it changes. Exporting only agent/src is
insufficient for M3: real Go generation needs the full repository.

```bash
set -euo pipefail
mkdir -p .uzi/scratch
base_export="$(mktemp -d .uzi/scratch/uxlab-base.XXXXXX)"
candidate_export="$(mktemp -d .uzi/scratch/uxlab-candidate.XXXXXX)"
git archive 3fb1fdbf | tar -x -C "$base_export"
git archive cb2cc627 | tar -x -C "$candidate_export"
export UXLAB_RENDER_BASE_DIR="$PWD/$base_export"
export UXLAB_RENDER_CANDIDATE_DIR="$PWD/$candidate_export"
# Optional: a real compatible Go distribution, mounted read-only at the
# production /opt/uzi-toolchain handle. No command PATH override is applied.
export UXLAB_RENDER_GO_DIR="$PWD/.uzi/scratch/go-prereq/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64"
bash e2e/uxlab-render/run.sh matrix
bash e2e/uxlab-render/run.sh local
```

The lead must first supply an image/toolchain with Go >= 1.27.3, matching
`api/go.mod`'s patched toolchain. Every setup command uses
`GOTOOLCHAIN=local`, prints its actual Go path/version and fails if too old.
The current worker Go 1.26.8 cannot provide this proof (#3305).
The optional full distribution mount covers the existing /opt/uzi-toolchain
directory. Both boundary and HOME probes use standalone inline JavaScript via
absolute /usr/local/bin/node, which remains outside that mount; no Python is
needed by these probes. Diagnose compiler/font/image availability before running.
The fixture never appends resolved Nix store binaries to command PATH.
All four cells use one resolved image ID, one mounted production source/helper
set and the same optional toolchain policy.

`matrix` runs base/off, base/required, candidate/off, candidate/required
sequentially. Each cell has a fresh named /nix volume with normal image copy-up
and fresh /data tmpfs, real image entrypoint and worker/runner/command uid split.
Full exported sources and fixture/helper inputs are read-only mounts.
A bounded copy creates the disposable checkout as runner uid 10002, with a
120-second timeout and a separate copy-duration receipt. Only the
candidate `build.sh` is added to the base checkout; frozen base root and nested
manifest hashes are checked, and both manifests must stay byte-identical.
There are no writable host binds.

Each root provisions with empty tier 1, repository opt-in and the default real
installer through `provisionRunTools`. Exact provisioning events and absence
of warnings reject fallback. Provision duration is measured around that call
alone; copy, container creation and Go setup durations are separate receipts.
The before/after comparison is the base and candidate provision receipt for
each mode; timings remain unmeasured until the lead runs them.

Commands use `buildCommandEnv`, `withCommandGitTrust` and
`makeDefaultSpawnCommand`, with a real `HeldRunCommandCache` created by
`startCommandCacheHolder`. Each command checks its fresh empty 0700
HOME/TMPDIR and uid 10003. Setup downloads Go modules and compiles the real
CLI test package through that same command seam while networking is available.
The host then disconnects this exact owned container from its Docker network,
verifies zero attached networks and releases the offline handoff. Render
commands additionally disable Go proxy/sumdb/toolchain downloads. Use a
disconnectable bridge/custom Docker network; host/none/container modes are
rejected. `UXLAB_RENDER_TIMEOUT` defaults to 3600 seconds per matrix cell;
each command has a 900-second wall limit. Setup failure or missing Docker/
image/uid split is a harness failure, never a successful base-red result.

Base cells seed frame/PNG bytes, run the full candidate build entrypoint and
require its named missing-freeze error before generation. Frame inventory,
SHA256 bytes and nanosecond mtimes must remain identical. Candidate cells
require freeze 0.2.2, delete outputs and execute the real `./build.sh`:
full Go generation plus real freeze rendering. Every generated ANSI needs its
light/dark peer and exactly one PNG with PNG signature, IHDR dimensions and a
strictly newer nanosecond mtime. A second render runs without network and
compares bounded HOME snapshots before/after freeze; production cache paths
are outside HOME and are not followed. HOME enumeration uses opendir iteration,
counts before collecting names, and caps at 8192 entries, depth 64, 4 MiB per
file and 16 MiB total (plus one overflow-detection byte). Snapshots retain uid/gid/mode, inode/device,
size, nanosecond mtime, file hashes and symlink text without following links.
No synthetic ANSI success substitutes for real generation.

Both modes require command-side `devbox run build` to fail specifically on
the Nix big-lock permission denial. Bounded standalone Node probes report
uid/gid/mode and try only O_RDWR opens on the existing database and big-lock,
close descriptors and fail if either
open succeeds. They do not create, truncate, lock or write real Nix files.
Runner creates only its own disposable /nix sentinel, mode 0666; an off-mode
command first calibrates an actual one-byte write using the exact same sentinel
path/O_RDWR flags. Required mode must deny that open, preventing writes.
Unexpected real-file or required-mode sentinel opens are immediately closed and
fail without writing. Only the disposable sentinel receives a calibration write.
Runner removes only that sentinel; existing Nix ownership/grants stay unchanged.

`local` is a separate fresh runner-owned store leg: after the same command
Go prewarm it runs actual nested-manifest `devbox run build` as runner,
requiring the same Go version and verifying the full PNG set. It retains network
for the real nested Devbox install; it is not the offline command proof.

Verified candidate PNGs are copied inside the container into a proof directory
after full build and again after offline rendering. Before exact-name cleanup,
the host uses `docker cp` from its own container into this worktree's
`.uzi/scratch/uxlab-png-*` and prints `PNG_EXPORT=`. A later command assertion
failure still exports an already verified full-build set. Inspect these PNGs
before recording visual acceptance. Local PNGs are exported the same way.
Logs print per-file hashes, dimensions, mtimes, command output, identities,
cache settlement and verified cleanup. All cells must finish and all receipts
must be reviewed by the lead before claiming M3 live proof.

Copy/artifact traversal is serial and stops at the first failure: at most
32768 entries, depth 64, 16 MiB per file and 256 MiB total. Internal relative
symlinks are copied without following them; absolute/escaping links and links
into omitted .git/.uzi/node_modules are rejected. These directories are omitted
at every level. Artifact frames additionally cap at 2048 with strict flat
light/dark names. Special files are rejected by the regular-file reader.
The source exports must be stable for the invocation.

Focused local checks (no Docker, dependency installation or repository gate):

```sh
bash -n e2e/uxlab-render/run.sh e2e/uxlab-render/matrix-run.sh
for file in e2e/uxlab-render/*.ts; do node --check "$file"; done
shellcheck -S warning e2e/uxlab-render/run.sh e2e/uxlab-render/matrix-run.sh
cd agent
node --import tsx --test --test-timeout=120000 ../e2e/uxlab-render/inputs.test.ts ../e2e/uxlab-render/tree.test.ts ../e2e/uxlab-render/probes.test.ts
```

Focused receipt (2026-10-22): 17 tests passed, including all eight original M1
input tests, six copy/inventory/PNG tests and three standalone Node probe tests.
Node 24.20.0 syntax, Bash syntax and ShellCheck 0.11.0 at warning severity passed.
These checks do not establish M3 live proof.

## M1 production provisioning canary

Run from the worktree root:

```sh
bash e2e/uxlab-render/run.sh canary
```

This opt-in fixture uses the real root entrypoint, worker 10001, runner 10002,
and production command identity 10003. It provisions a checkout-root
`devbox.json` copied byte-for-byte from the selected root manifest (current
candidate: Ruby 4.0.6 and freeze 0.2.2), with repository opt-in enabled and
empty tier 1. There are no install, shellenv, or PATH-probe stubs. Warnings or
fallback to empty tier 1 fail. Two commands in each of `off` and `required`
assert the provisioned Ruby path/version, freeze version when declared, uid, and fresh empty private
HOME/TMPDIR with mode 0700. Required Landlock absence fails.

Docker needs network access to the real devbox/Nix download endpoints. The
default network is bridge; set `UXLAB_RENDER_NETWORK=host` if that is the
permitted egress route on your test host. A fresh owned named Nix volume uses
normal image copy-up, and /data is disposable tmpfs. No host Nix store is
mounted and no PATH entries are injected to make Ruby reachable.

Configuration:

- `UXLAB_RENDER_IMAGE`: existing compatible base image (default
  `ghcr.io/vtmocanu/uzi/agent-base:0.86.0-rc.18`). The resolved image ID is pinned
  for the invocation.
- `UXLAB_RENDER_MANIFEST`: absolute base root manifest file; defaults to this
  checkout's `devbox.json`. Its content hash is recorded.
- `UXLAB_RENDER_SRC_DIR`: absolute exported `agent/src` directory; defaults to
  this checkout's source. An unpacked source tar/export works without Git
  metadata. The source must be compatible with the image's installed JS deps.
- `UXLAB_RENDER_BIN_DIR`: optional absolute directory with executable
  `uzi-codex-supervisor`, `uzi-codex-fileop`, and
  `uzi-codex-command-sandbox` helpers compatible with that source. All source,
  fixture, and helper mounts are validated read-only binds.
- `UXLAB_RENDER_CREATE_TIMEOUT`: image-store copy-up/container creation seconds
  (default 600), separate from fixture execution.
- `UXLAB_RENDER_TIMEOUT`: whole-canary seconds (default 1800); command roots
  have a 30-second wall deadline and 5-second reap deadline.
- `bash e2e/uxlab-render/run.sh build`: optional image build using the existing,
  unchanged base Dockerfile. `UXLAB_RENDER_BUILD_TIMEOUT` defaults to 1800.

Fixture input reads accept regular files only (no symlinks, FIFOs, or devices).
The root manifest is limited to 1 MiB, each source file to 4 MiB, all source
content to 64 MiB, and each helper binary to 64 MiB. Sizes are checked before
allocation; reads use chunks of at most 64 KiB and at most one extra byte to
detect growth past a cap. Source enumeration counts at most 8192 file/directory
entries across the tree before collecting names for sorting, with at most 64
directory levels below the root. Exceeding any limit fails the fixture.
Successful inputs retain the same byte hashes and source path/content delimiters.

Focused input regressions (installed agent dependencies required):

```sh
cd agent
node --import tsx --test --test-timeout=120000 ../e2e/uxlab-render/inputs.test.ts
```

Tests create and remove only their own directories under this worktree's
`.uzi/scratch/`.

Logs live only in this worktree's `.uzi/scratch/`; the runner prints the path.
They include image ID/labels, source content SHA256, helper binary SHA256s,
image/Node/devbox/Nix versions, provisioning events, command output, elapsed
times, filesystem free-space observations, cleanup verification, and final exit status. Combined container output
is capped at 8 MiB; reaching the cap fails. Cleanup attempts each owned object
once, with bounded commands, and verifies exact-name absence. Docker/image or
uid-split absence exits 77 (unperformed, never PASS); cleanup failure turns a
pass/skip into exit 3. Other failures are nonzero. M1 remains separate from the M3 matrix above; neither changes production runtime code.
To reproduce the historical Ruby-only canary, first create the full base export
above and run:

```sh
UXLAB_RENDER_MANIFEST="$UXLAB_RENDER_BASE_DIR/devbox.json" bash e2e/uxlab-render/run.sh canary
```

The manifest option is still accepted; the canary derives its exact expected
package events from the selected manifest rather than assuming one package.

## M1 validation receipt — 2026-10-22

Historical receipt for the fixture before the bounded-input repair. Fresh
post-repair proof is recorded below.

Executed `bash e2e/uxlab-render/run.sh canary` against base root manifest
SHA256 `d3b4242b28ce03f6fafa548aff5edc8c67ce28d5c0b708f259676f78a578e33b`.
The image was
`sha256:6b788ff9e9feb73e1777f98654b290a561420f26b2f9e8df5264539eed856c41`
(version 0.86.0-rc.18); mounted agent source SHA256 was
`31d4cb2ca5e5ce1bb049573a52401f076f910412ca4ff4c5be16a21372066b57`.

Real runner provisioning completed in 26,501 ms, without a degraded-package
event or provisioning logger warning. Both `off` and enforced `required`
passed twice: uid 10003, Ruby 4.0.6 through this run's
`/data/provision/<uuid>/.devbox/nix/profile/default/bin/ruby`, and four
distinct initially empty HOME/TMPDIR directories with mode 0700.
The Landlock probe returned 0; fixture and wrapper returned 0, and exact-name
container/volume cleanup was verified. Total wrapper duration was 62 seconds.

Earlier attempts failed before the canary: a short container-creation timeout,
then provisioning ENOSPC. The timed-out creation's cleanup handling was repaired;
both failed attempts' exact containers/volumes were subsequently removed and
absence checked. The successful retry used a new volume and included filesystem
capacity diagnostics. These failed attempts are not canary passes and do not
establish a provisioning-time comparison. Full render, Nix-denial controls,
matching-Go proof and local Devbox build remain M3 obligations.

Focused checks passed: Bash syntax, ShellCheck 0.11.0 at warning severity,
and Node 24.20.0 TypeScript syntax.

### Post-repair verification

The lead reran `bash e2e/uxlab-render/run.sh canary` at commit
`076a2a011508f2ca1e6ed99012edcb644d3146be`, using a new fixture-owned Nix
volume. Image, agent source, base manifest and helper hashes matched the
earlier receipt. Real provisioning completed in 27,684 ms. Both `off` and
`required` again passed twice with uid 10003, the provisioned Ruby 4.0.6
profile path, and four distinct empty mode-0700 HOME/TMPDIR directories.
The fixture and wrapper exited 0, with verified container/volume cleanup.
Total wrapper time was 110 seconds; this is a fresh canary receipt, not the
M3 before/after provisioning comparison.

All eight bounded-input regressions passed. Reviewer and auditor independently
reviewed the repair at the same commit and found no remaining findings; the
auditor confirmed resolution of the original unbounded-read finding.
The unchanged runner also passed `task lint:shell` at the earlier fixture
commit. Full rendering and the other M3 checks remain unperformed.
