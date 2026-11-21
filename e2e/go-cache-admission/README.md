# Go cache admission: M1 only (issue 4203)

This Python stdlib preflight is nonproducing. It never executes Go, starts a
subprocess, inspects processes, creates containers, pulls images, installs tools,
changes native caches, creates mounts, or raises limits. There is no producer
implementation or enabling flag. It is not completed cache protocol, A/B,
quota, admission, or performance proof.

Run from the current worktree:

```sh
python3 -B e2e/go-cache-admission/preflight.py
python3 -B e2e/go-cache-admission/preflight.py --experimental
python3 -B -m unittest discover -s e2e/go-cache-admission -v
```

Default exit is 2 (prerequisites not established). Experimental exit is 0 for
preflight inspection only, including unavailable prerequisites; consult every
leg's status. Neither exit authorizes production. JSON schema 2 is fixture-only,
ASCII escaped, at most 64 KiB, with PROVEN/FIXTURE-PROVEN/NEGATIVE/UNVERIFIED per leg. Raw probe
responses, errors, environment values, and supplied input strings are not echoed.
ANSI, controls and bidi characters are escaped by the JSON encoder. No inherited
environment or proxy/config is passed to probes: Docker uses direct GETs (Unix
socket or literal loopback TCP, see below), not a CLI. No environment enumeration occurs.

## Explicit inputs

All inputs are optional so unavailable prerequisites can be inspected:
- `--endpoint`: existing absolute Unix Docker socket (default
  /var/run/docker.sock), or a loopback TCP endpoint written exactly
  `tcp://127.0.0.1:<port>` or `tcp://[::1]:<port>` (port 1-65535). The address is a
  literal: no hostname resolution, no `DOCKER_HOST` or other environment read, and
  `tcp://localhost:...` or any other address is refused. Only GET /version,
  GET /info, GET /images/json and GET /images/sha256:<64 hex>/json; any non-200
  status, redirects included, is refused.
- `--docker-version`: exact expected daemon version.
- `--image`: installed immutable sha256 image ID; `--repo-digest`: exact
  repository@sha256 digest in that local image's RepoDigests.
- `--go-root`: existing absolute installed Go tree without symlink ancestors.
  Reads VERSION and src; never executes its tools. `--source-sha256` must match
  the deterministic source inventory digest.
- `--toolchain-path`: explicit bounded absolute directory list, with no default or
  inherited PATH; for example /opt/uzi-toolchain/bin:/usr/bin:/bin when those
  directories satisfy the constructor checks. Missing input leaves environment UNVERIFIED.
- `--manifest`: absolute existing regular JSON file beneath this worktree's
  .uzi/scratch, without symlink components or parent traversal. Maximum 64 KiB;
  integer tokens at most 20 characters, duplicate keys rejected. Malformed or
  excessively nested JSON yields UNVERIFIED rather than aborting the report.

The nonsecret threshold manifest has exactly these keys:

```json
{"schema":2,"provenance":"live-worker-config","observed_at":0,"threshold":0.8,"margin_state":"set","margin":"0.1"}
```

Replace observed_at with the actual Unix observation time of current live
configuration; the example is deliberately stale. Freshness is at most 300 seconds,
future times are rejected. Threshold must be numeric finite (0,1], never missing
or defaulted to 0.9. Margin requires explicit margin_state "set" or "unset". Unset requires JSON null
margin and alone selects default 0.1. Set requires a nonblank string with finite
[0,1) value, using decimal/exponent or unsigned 0x/0b/0o numeric syntax with
ASCII surrounding whitespace only; underscores
are rejected. This is intentionally stricter than agent config.ts's JS Number
conversion and invalid-value fallback to 0.1; it does not claim full parser parity.
Schema 1, missing state, implicit null/blank, bad states, and invalid numbers fail closed. Positive threshold-minus-margin is soft;
otherwise soft is threshold. Provenance is an asserted source label, not
authenticated proof of live worker configuration. A wellformed manifest emits
UNVERIFIED with shape_valid and calculated_soft facts; the label never proves
live configuration. Never put secrets in inputs.

The source digest starts with VERSION bytes, then sorted depth-first src file
relative path, NUL, contents, NUL. Facts include file count, content bytes,
aggregate SHA256 and VERSION SHA256. Inventory caps: 20,000 directory/file
entries checked during streaming directory enumeration before retaining names,
256 MiB content, 1 KiB VERSION, 10 seconds checked between entries and reads.
Only small regular fixtures are unit tested. Concurrent replacement or special
filesystem behavior is not an attested snapshot; no hostile-filesystem guarantee.
Installed host source is not proof of the Go version/source inside an image.

Docker body reads are capped at 64 KiB with two-second socket waits and a
five-second body deadline; HTTP parsing also uses Python's finite header limits.
Daemon version matching proves only endpoint identity observations.
Image matching proves only local metadata, not runtime use or quotas.
No arbitrary response fields are emitted. A bounded local AF_UNIX HTTP stub tests both fixed routes and Host: localhost;
this is a new local stub test, not a Docker or Go experiment. The connection
endpoint is separate from the fixed HTTP host.

## Gates intentionally still closed

All-sink enforceable allocation is UNVERIFIED unconditionally in M1. There is
no quota flag, user-manifest assertion, or Docker-availability bypass. A later
implementation needs independently verifiable enforcement covering home,
GOPATH, modules, cache, Go temp, temp, work, output, logs, retained and staging,
including bytes and inodes. `headroom` is a pure admission calculation seam:
it requires all named sink charges and positive separately declared inode charges.
Retained and staging must cover their frozen shares, remaining sinks together
must cover the work share, and total bytes must equal exactly the approved 2 GiB.
Metadata must fit within those shares; it cannot increase the ceiling.
There is no known hard inode cap: inode calculations remain UNVERIFIED as live
admission evidence. The helper re-samples statvfs on every call, sums
projections per device and requires both projected usage fractions strictly
below soft. It is not allocation enforcement. Later potential producing
launches must call it immediately before every launch; M1 has none.

The child environment builder creates nothing. It starts from an empty mapping,
requires an explicit PATH (at most 4096 characters and 32 directories), checks
that each PATH entry is absolute and checks its final resolved directory for
group/world write access (ancestor directories are not checked), and deterministically finds the first executable regular go and cc files on that
PATH (also rejecting group/world writable resolved files). /bin symlinks are allowed.
These checks do not attest executable trust, image identity, or actual children.
It emits only FIXTURE-PROVEN construction evidence when successful, and
supplies only PATH, LANG=C.UTF-8, TZ=UTC, private scratch HOME/GOPATH/GOMODCACHE/
GOCACHE/GOTMPDIR/TMPDIR, GOTOOLCHAIN=local, GOMAXPROCS=2, GOENV=off,
GOFLAGS="", and GIT_CONFIG_GLOBAL/SYSTEM=/dev/null. DiskCache omits GOCACHEPROG;
CacheProg explicitly requires a scratch path without whitespace or shell quoting. Private directory provisioning,
executable trust, permissions and actual launch confinement remain later gates.

Frozen constants/schema are in preflight.py: aggregate 2 GiB = retained 768 MiB +
staging 128 MiB + work/modules/output/logs 1152 MiB; memory 1 GiB, clients 2,
writers 1, objects 4096, records 8192, frames 2, body 64 MiB, header 16 KiB,
chunk 64 KiB. Tiny 16 MiB/30s, workload 180s, series 30min. These are declared
budgets, not enforced quotas. No automatic increases.

Frozen baseline: 18da9c670f4403f4fcab3891f8bb8c59757243ef.
Representative tiny example.com/cacheproof leaf/middle/app and runkind small
remain unlaunched pending proven resource fit. Owner-observed heavy race archives
api/internal/handler 269 MiB and workersvc 576 MiB exceed body 64 MiB and staging
128 MiB; workersvc also exceeds 2T=512 MiB. Both are
UNVERIFIED/resource-incompatible, never launches or B triggers.
T=256 MiB, 2T=512 MiB, overshoot=256 MiB, slowdown=1.25, warm-hit=0.90,
near-threshold=5% remain frozen.

Tests use only bounded fixtures in owned temporary directories beneath
.uzi/scratch, with cleanup limited to their own directory. They exercise public
functions with injected statfs/Docker observations, without launching any child.
No live Docker endpoint, Go workload, protocol producer, A/B experiment, full-stack or
repo-wide gate has been validated by these tests.

The local socket stub uses two accepts, bounded request bytes, socket timeouts
and a bounded join on a non-daemon thread, with socket/directory closure. Fixture
setup creates root scratch before temporary directories, including in a fresh
checkout. Tests never execute the fake go/cc files. All allocation/quota authority,
actual child confinement and hard inode enforcement remain UNVERIFIED.

## qualify.py: prerequisite verifier (issue 4575, M1)

`qualify.py` is a pure, nonproducing verifier. It starts no Docker, Go, container or
subprocess, and it qualifies no environment: the canary leg has no collector, so
`qualified` is always false and the verdict is NO-QUALIFIED-ENVIRONMENT. No live proof
is claimed; every test fixture is synthetic and labelled fixture-only.

```sh
python3 -B e2e/go-cache-admission/qualify.py --artifact <abs path under .uzi/scratch> [--experimental]
```

Exit 2 unless qualified (never, in this issue); `--experimental` exits 0 and never sets
`qualified`. The artifact is read by `preflight.read_json` (64 KiB, duplicate keys
rejected, scratch-only path, schema `uzi-go-cache-env/1`); an unreadable or wrong-schema
artifact reports every leg UNVERIFIED `schema-unsupported`. Output is the `encode()`d
JSON: `qualified`, `verdict`, `producer_launched=false`, `production_authorized=false`
and eight legs (binding, threshold, charge_map, enforcement, aliasing, envelope,
backing, canary), each with `status`, `reason` and the full `reasons` list.

Statuses: PROVEN, NEGATIVE (a contradiction), UNVERIFIED (missing or unproven evidence),
FIXTURE-CONSISTENT (consistent, but only file-sourced evidence).

### Evidence tiers

Whatever a file claims about its own provenance is ignored: file values reach at most
FIXTURE-CONSISTENT. PROVEN needs a `CollectorEvidence`, which only `collect(observer)`
can construct (a module-private token; it also stores the daemon identity as a hash
salted per invocation). The CLI never builds one. A later collector calls
`evaluate(artifact, collector=collect(observer), now=...)`, where
`observer.observe()` returns the sections binding, threshold, mountinfo, runtime,
daemon and backing and `observer.daemon_id()` a string; collector sections replace the
file's. The envelope leg is declared-only and cannot exceed FIXTURE-CONSISTENT.
Defects are reported whatever the tier.

### Legs and reason codes

- binding: `binding-source-unbound`, `binding-image-mismatch`, `go-host-only`,
  `go-not-declared` (distinct from `go-absent-verified`), `provenance-coordinate-only`
  (tag, label, profile path, devbox lock, narhash), `toolchain-mount-writable`,
  `no-go-bearing-image-verified`. Go must be observed with `observed_in: "image-fs"`
  and a provenance kind in `signed-output-metadata` or `reproducible-build-attestation`.
- threshold: `owner-input-not-supplied`, `effective-value-unestablished` (an API value
  alone), `threshold-stale` (over 300 s old or in the future), `threshold-contradictory`,
  `margin-js-parse-unsupported`. The effective value must be a `soft-threshold-log` or
  `owner-authorized` observation bound to the same incarnation, equal to the API
  threshold minus `config_margin` (or the threshold itself when that is not positive).
  `config_margin` emulates agent/src/config.ts `marginFraction` (JS trim and Number);
  input outside the emulated grammar raises instead of guessing.
- charge_map: `sink-missing` (CHARGE_SINKS is the preflight SINKS plus the container,
  daemon and export sinks), `no-container-mount-facts`, `unaccounted-writable-mount`,
  `unaccounted-writable-fd`. Read-only mounts need no storage coverage. Writable proc
  and devpts are listed with a reason as non-storage. Writable mqueue, sysfs, cgroup,
  cgroup2, network binds (/etc/hosts, /etc/hostname, /etc/resolv.conf), bind or volume
  mounts and any other fstype are unaccounted. Writable tmpfs needs `size` and
  `nr_inodes` coverage. A writable fd must map to an accounted mount.
- Kernel memory: every enumerated mount, read-only and non-storage ones included, stays
  in the leg's `kernel_memory` list charged to the memory domain (tmpfs pages, dentry
  and inode slab). It is never dropped because a mount is not a storage sink.
- enforcement: claims are triples `{kind, resource, sinks}`.

| kind | resource | scope and evidence |
| --- | --- | --- |
| tmpfs-size | bytes | sinks on that tmpfs superblock; `size` in its options |
| tmpfs-nr_inodes | inodes | same; `nr_inodes` in its options |
| exclusive-finite-fs | bytes and inodes (two triples, both needed) | collector capacity, inode count, `exclusive: true` |
| readonly-rootfs | write-deny | sink realpath resolves by longest prefix to the read-only root mount |
| pids-max | pids | process tree only; limit observed applied |
| log-driver-none | logs | container_logs only; driver observed `none` |
| memcg-max-eq-swap | memory-swap | outer memory domain only; memory max equals swap max |

  Codes: `boundary-resource-missing`, `-unknown`, `-mismatch`, `boundary-sink-out-of-scope`,
  `boundary-removable` (privileged, CAP_SYS_ADMIN for mount claims, writable cgroupfs or
  no cgroupns for pids/memory claims), `boundary-not-allocation-enforcement` (emptyDir
  sizeLimit, ephemeral-storage limit, quota monitoring, PVC request, fs capacity,
  watchdog, sampled du/statfs, broker counters, eviction, cooperative writer caps,
  json-file max-size, rlimit fsize, cacheprog object count, and storage-opt size unless
  backend-support evidence is present), `new-mount-possible` (needs CapBnd 0, NoNewPrivs 1
  and seccomp mode 2 default profile or max_user_namespaces 0),
  `daemon-capability-unsupported` (MemoryLimit, SwapLimit or PidsLimit false, or
  rootless with the cgroupfs driver), `limits-not-applied-observed`,
  `claim-evidence-not-collector`, and per-sink coverage codes `sink-bytes-uncovered`,
  `sink-inodes-uncovered`, `sink-pids-uncovered`, `sink-logs-uncovered`,
  `sink-memory-swap-uncovered`. Every incremental-allocation sink and writable storage
  mount needs bytes and inodes coverage; daemon-side sinks are outside container
  enforcement and are only charged through the aliasing and backing legs.
- DAC rule: a `dac-deny-nonroot` kind (or any claim using fields like `non_root_user`,
  `owner`, `mode`, `acl`) is UNVERIFIED `dac-ownership-mode-unverified` and covers
  nothing: ownership checks cannot establish zero allocation (CHOWN/SETUID-class
  capabilities may remain and pre-existing writable fds are unaccounted).
- aliasing: the domain key is the superblock; `mount_id` only maps paths. `domain-overcommitted`
  when sink ceilings on one superblock exceed its limit, `domain-alias-split` when sinks
  on one superblock claim different backing domains; a child charged inside its parent
  counts once.
- envelope: `envelope-drift` unless limits equal preflight.LIMITS exactly;
  `inode-ceiling-unapproved` unless `owner_approved` is true.
- backing: `backing_check` reuses `preflight.project_domains`. Codes
  `backing-link-cited-only`, `backing-capacity-unknown`, `backing-sample-stale` (over
  60 s), `backing-concurrency-unknown`, `backing-mapping-unknown`,
  `backing-aggregate-exceeds-soft` (projected use must stay strictly below soft times
  capacity for bytes and inodes), `backing-soft-unknown`.
- canary: evaluated only when the other legs are PROVEN and a canary-collector result
  exists; none does, so `canary-collector-not-implemented`. `classify_canary` is tested
  with fixtures: readonly-rootfs needs EROFS and the read-only flag
  (`write-deny-not-erofs`), tmpfs needs ENOSPC, EDQUOT only for an accepted quota kind
  (`canary-errno-unexpected`), a kill without an errno is `canary-kill-only`.
- Other codes: `schema-unsupported`, `malformed-section`, `container-facts-missing`,
  `daemon-info-missing`, `sink-mapping-mismatch`.

### Margin golden table

`margin-golden.json` records the output of the real `loadConfig(...).diskSoftMargin` from
agent/src/config.ts at revision f99e8a84d9111dea0f0431485295d2411ec5e971, with the
sha256 of that file. Tests check every row and that the file still hashes to the recorded
value, so a config.ts change turns them (and the threshold leg) red until the table is
regenerated. Regenerate the rows with this command (it passes only the required
variables plus `UZI_DISK_SOFT_MARGIN`), then update `config_ts_sha256` and `revision`:

```sh
cd agent && node --import tsx -e 'import("./src/config.ts").then(({ loadConfig }) => { const rows = [null, "", "   ", " ", "\u3000 0.2 ", "0.1", "0.25", "1e-1", ".5", "5.", "0x0", "0b0", "-0.1", "1", "1.5", "abc", "NaN", "Infinity", "1_0", "0.5abc", "+0.3", " 0.3\n"]; console.log(JSON.stringify(rows.map((input) => { const explicit = { UZI_API_URL: "http://x", UZI_WORKER_TOKEN: "t" }; if (input !== null) explicit.UZI_DISK_SOFT_MARGIN = input; return { input, expected: loadConfig(explicit).diskSoftMargin }; }))); })'
```

The new tests are `test_qualify.py`: synthetic fixtures only, including an owned
loopback TCP stub for `docker_get`. They validate code paths, not an environment.
