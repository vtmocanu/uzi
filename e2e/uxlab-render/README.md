# M1 production provisioning canary

Run from the worktree root:

```sh
bash e2e/uxlab-render/run.sh canary
```

This opt-in fixture uses the real root entrypoint, worker 10001, runner 10002,
and production command identity 10003. It provisions a checkout-root
`devbox.json` copied byte-for-byte from the base root manifest (currently only
`ruby@4.0.6`), with repository opt-in enabled and
empty tier 1. There are no install, shellenv, or PATH-probe stubs. Warnings or
fallback to empty tier 1 fail. Two commands in each of `off` and `required`
assert the provisioned Ruby path and version, uid, and fresh empty private
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
pass/skip into exit 3. Other failures are nonzero. No full render matrix or
runtime modifications are included in M1.

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
