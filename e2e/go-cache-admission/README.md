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
  `tcp://localhost:...` or any other address is refused. There is no peer-identity
  check on the loopback transport: any local process listening on that port could
  answer, so the evidence binds only to what the endpoint reports (the daemon
  identity, kept as a per-invocation salted hash), not to a verified peer. Only GET /version,
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
python3 -B e2e/go-cache-admission/qualify.py --artifact <abs path under .uzi/scratch> \
    [--head-revision <40 lowercase hex>] [--experimental]
```

Exit 2 unless qualified (never, in this issue); `--experimental` exits 0 and never sets
`qualified`. `--head-revision` is the repo revision the artifact's `repo_revision` must
equal (`binding-source-unbound` otherwise). Without it the CLI does not detect a stale
`repo_revision`; it only checks that the field is a 40-hex string. The artifact is read by `preflight.read_json` (64 KiB, duplicate keys
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
daemon, backing, charge_map and claims (`claims` is a list, the rest are dicts) and
`observer.daemon_id()` a string; collector sections replace the file's. A leg is PROVEN
only when the collector supplied every section it reads: charge_map reads mountinfo and
charge_map; enforcement reads mountinfo, runtime, daemon, charge_map and claims;
aliasing reads mountinfo and charge_map; backing reads backing, threshold and charge_map.
A charge map or claims list that came from the file therefore caps those legs at
FIXTURE-CONSISTENT. The envelope leg is declared-only and cannot exceed
FIXTURE-CONSISTENT. Defects are reported whatever the tier. `evaluate()` grades file
evidence by tier rather than refusing it, so `claim-evidence-not-collector` is raised only
by a direct `evaluate_claims(..., require_collector=True)` call (the default), never on the
CLI or `evaluate()` path.

### Legs and reason codes

- binding: `binding-source-unbound`, `binding-image-mismatch`, `go-host-only`,
  `go-not-declared` (distinct from `go-absent-verified`), `provenance-coordinate-only`
  (tag, label, profile path, devbox lock, narhash), `toolchain-mount-writable`,
  `no-go-bearing-image-verified`. Go must be observed with `observed_in: "image-fs"`
  and a provenance kind in `signed-output-metadata` or `reproducible-build-attestation`.
  The index digest, platform digest and image Id are well-formed sha256 values and the
  platform digest is required. `image_id_kind` is `index` (the Id equals the index digest, as
  under the containerd snapshotter) or `config` (the Id differs from both other digests); the
  platform digest differs from the index digest unless `single_platform` is true. Any other
  shape is `binding-source-unbound`.
- threshold: `owner-input-not-supplied`, `effective-value-unestablished` (an API value
  alone), `threshold-stale` (over 300 s old or in the future), `threshold-contradictory`,
  `margin-js-parse-unsupported`. The effective value must be a `soft-threshold-log` or
  `owner-authorized` observation bound to the same incarnation, equal to the API
  threshold minus `config_margin` (or the threshold itself when that is not positive).
  `config_margin` emulates agent/src/config.ts `marginFraction` (JS trim and Number);
  input outside the emulated grammar raises instead of guessing.
- charge_map: `sink-missing` (CHARGE_SINKS is the preflight SINKS plus the container,
  daemon and export sinks), `no-container-mount-facts`, `unaccounted-writable-mount`,
  `unaccounted-writable-fd`, `sink-malformed` (an incremental sink needs at least one
  inode), `sink-parent-cycle`, `sink-pseudo-forbidden` (an artifact sink may not set
  `pseudo`; only the evaluator injects process_tree and memory). Read-only mounts need
  no storage coverage. Writable proc
  and devpts are listed with a reason as non-storage. Writable mqueue, sysfs, cgroup,
  cgroup2, network binds (/etc/hosts, /etc/hostname, /etc/resolv.conf), bind or volume
  mounts and any other fstype are unaccounted. Writable tmpfs needs `size` and
  `nr_inodes` coverage. A writable fd must map to an accounted writable mount: with a
  `mount_id` it matches on that id alone (an unknown id is unaccounted), otherwise on
  `target_superblock`; a writable fd on a read-only mount is unaccounted too.
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
  backend-support evidence is present; even then it covers bytes of `container_writable_layer`
  only, needs the same removability checks as mount claims, and needs a collector-observed
  applied `writable_layer_quota_bytes` above zero, else `quota-not-applied-observed`; any
  other sink is `boundary-sink-out-of-scope`), `outer-sink-unbounded` (each outer sink needs a
  runtime fact `outer_ops_excluded` naming it as having no incrementing operation in the
  permitted workload; a declaration or backing headroom does not cover it, and a file-sourced
  fact is FIXTURE-CONSISTENT at most), `new-mount-possible` (needs CapBnd 0, NoNewPrivs 1
  and seccomp mode 2 default profile or max_user_namespaces 0),
  `daemon-capability-unsupported` (MemoryLimit, SwapLimit or PidsLimit false, or
  rootless with the cgroupfs driver; rootless with a null CgroupDriver is
  `daemon-info-missing`, not unsupported; judged from the daemon section alone, so it also
  turns the enforcement leg NEGATIVE when no container mount facts exist),
  `limits-not-applied-observed`,
  `claim-evidence-not-collector`, and per-sink coverage codes `sink-bytes-uncovered`,
  `sink-inodes-uncovered`, `sink-pids-uncovered`, `sink-logs-uncovered`,
  `sink-memory-swap-uncovered`. Every incremental-allocation sink and writable storage
  mount needs bytes and inodes coverage; daemon-side sinks are outside container
  enforcement and are only charged through the aliasing and backing legs.
- DAC rule: a `dac-deny-nonroot` kind (or any claim using fields like `non_root_user`,
  `owner`, `mode`, `acl`) is UNVERIFIED `dac-ownership-mode-unverified` and covers
  nothing: ownership checks cannot establish zero allocation (CHOWN/SETUID-class
  capabilities may remain and pre-existing writable fds are unaccounted).
- aliasing: the domain key is the superblock, and a sink's declared superblock is not
  trusted. It must appear in mountinfo, agree with the mount its `mount_id` names, and
  agree with the mount every one of its listed paths resolves to by longest mountpoint
  prefix (the same resolution readonly-rootfs uses). A listed path with no recorded
  realpath cannot be checked, and a `mount_id` never substitutes for a contradicting
  realpath. A container sink with neither a `mount_id` nor resolvable paths, an unresolved
  path, or any disagreement is `sink-mapping-mismatch`. Daemon-side sinks
  (`daemon_image_store`, `daemon_build_cache`, `daemon_metadata_snapshots`) and
  `container_logs` live in an outer domain: they carry an explicit `outer_domain: true`
  and no superblock or `mount_id` (a made-up one is `sink-malformed`) and are charged only
  through the backing leg. An outer sink whose paths resolve into a container mount, a
  non-outer sink whose parent chain reaches an outer sink (a child or a grandchild), and
  `outer_domain` on any other sink name, a child sink included, are `sink-malformed`.
  Collectors must not record realpaths for an outer sink's paths: a recorded realpath resolves
  under `/` (always a container mount) and makes the outer sink `sink-malformed`.
  `domain-overcommitted`
  when sink ceilings on one superblock exceed its limit, `domain-alias-split` when sinks
  on one superblock claim different backing domains; a child charged inside its parent
  counts once.
- envelope: `envelope-drift` unless limits equal preflight.LIMITS exactly;
  `inode-ceiling-unapproved` unless `owner_approved` is true;
  `enforced-capacity-exceeds-envelope` (NEGATIVE) when observed enforced byte capacities
  summed over all enforcing superblocks exceed the aggregate, enforced inode capacities exceed
  the approved ceiling, or the applied memory max exceeds the memory limit or differs from the
  swap max.
- backing: `backing_check` reuses `preflight.project_domains`. Codes
  `backing-link-cited-only`, `backing-capacity-unknown`, `backing-sample-stale` (over
  60 s), `backing-concurrency-unknown`, `backing-mapping-unknown`,
  `backing-aggregate-exceeds-soft` (projected use must stay strictly below soft times
  capacity for bytes and inodes), `backing-soft-unknown`. Each enforcing tmpfs or exclusive
  superblock projects the larger of its declared sink charges and its observed enforced
  capacity; the leg needs mount facts.
- canary: always UNVERIFIED `canary-collector-not-implemented` in M1, whatever any caller
  attaches (`CollectorEvidence` has no canary attribute). `classify_canary` is tested
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

## observe.py: bounded, sanitized, read-only collector (issue 4575, M2)

`observe.py` is the first collector behind `qualify.collect()`. It reads a Docker daemon's own
reports and writes a sanitized artifact and verdict. It creates no container, pulls no image,
inspects no image filesystem and runs no Go. It collects no mountinfo, fd or runtime fact, so the
verdict is always NO-QUALIFIED-ENVIRONMENT; nothing here qualifies an environment.

```sh
python3 -B -I e2e/go-cache-admission/observe.py --endpoint tcp://127.0.0.1:PORT >/dev/null 2>&1
python3 -B -I e2e/go-cache-admission/observe.py --endpoint /abs/docker.sock \
    [--image sha256:<id>] [--go-root <abs>] [--go-binary <abs>] [--toolchain-profile <abs file>] \
    [--repo-revision <40hex>] [--scaffold-revision <40hex>] [--head-revision <40hex>] \
    [--out-dir .uzi/scratch/go-cache-env] >/dev/null 2>&1
```

- `--endpoint` is required and explicit: a Unix socket path or `tcp://127.0.0.1:<port>` /
  `tcp://[::1]:<port>` (see `preflight.docker_get`). No environment variable is read, so
  `DOCKER_HOST` and Docker contexts are ignored. `-I` keeps `PYTHON*` variables and the user site
  out of the interpreter.
- Requests are GET only, on `/version`, `/info`, `/images/json` and
  `/images/sha256:<id>/json` for IDs from that listing. Never `/containers/*`, never a POST.
  More than 64 listed images is a refusal; `--image` may only narrow to an ID in the listing.
  The daemon collection has a 90 second budget on top of the per-request caps: 64 KiB of body,
  16 KiB of status line and headers, and one absolute 5 s deadline covering connect, request,
  headers and body (a trickling header phase is refused). The optional host-side hashing and
  inventory (`--go-root`, `--go-binary`) runs outside that 90 second budget and has its own bounds
  (binary at most 512 MiB and 60 s, inventory at most 256 MiB, 20000 entries and 10 s).
- Daemon fields kept: ServerVersion, Os, Arch, KernelVersion, Driver, DriverType (the
  `driver-type` DriverStatus value), CgroupVersion, CgroupDriver, MemoryLimit, SwapLimit,
  PidsLimit, LoggingDriver, DefaultRuntime, RuncVersion (Runtimes annotations, when present),
  `rootless` and `seccomp` (booleans from SecurityOptions). Each string must match a short fixed
  pattern or allowlist, or is dropped to null. Os, Arch, Driver, DriverType, CgroupVersion,
  CgroupDriver, LoggingDriver and DefaultRuntime must equal an entry of an exact allowlist
  (for example CgroupDriver is `cgroupfs` or `systemd`, DriverType is
  `io.containerd.snapshotter.v1`), so a hostname-shaped value never survives. ServerVersion and RuncVersion must be exactly `N.N.N`
  (else null); KernelVersion keeps its numeric prefix `N.N.N[-N]` plus an optional `.elN` and
  `.x86_64`/`.aarch64` suffix and nothing after it; any daemon string containing `uzi-` or
  `.internal` is null. Each allowlisted daemon field (3 version fields plus 8 enum fields) whose
  value is reduced or dropped is counted in `dropped_daemon_strings`; Name, ID,
  DockerRootDir and SecurityOptions overflow are dropped uncounted. A per-image Architecture or Os
  is held to the same Arch and Os allowlists (null and counted in `dropped_daemon_strings` otherwise). MemoryLimit, SwapLimit and PidsLimit are true or false as reported
  and null when the daemon did not report a boolean; null is missing evidence
  (`daemon-info-missing`, UNVERIFIED), never a negative `daemon-capability-unsupported`. Name, ID, DockerRootDir, hostnames, labels, proxy settings and
  paths are never copied. The daemon ID feeds only the salted hash in `collector.daemon_id_hash`
  (a fresh 16-byte salt per run, so two runs against one daemon differ).
- Per image: Id, `id_kind` (`index` when the daemon DriverType is
  `io.containerd.snapshotter.v1`, else `config`), Architecture, Os, `digests` (`sha256:<64 hex>` split from RepoDigests),
  `repos` and `go_declared` (Env, PATH, Cmd or Entrypoint mention `/usr/local/go`, `/go/bin`,
  GOROOT or GOPATH; metadata only). Repository names are kept only for the official
  `postgres` and `hello-world` (optionally prefixed `docker.io/library/`); any other name becomes
  a per-run salted pseudonym `repo-<12 hex>`. A ref that fails the OCI name grammar, carries a
  control character or exceeds 256 characters is dropped and counted in `dropped_repo_refs`.
  Tags and environment values are never emitted. `go_absent` is never claimed: an image whose
  metadata names no Go is `go-not-declared`, and no image is Go-bearing verified
  (`no-go-bearing-image-verified`: only image names and metadata listed; no image filesystem
  inspected). Under the containerd snapshotter the image Id is the index digest and no platform
  digest is collected, so the binding leg stays `binding-source-unbound`.
- Optional host-only Go coordinates, each labelled `host_only` and never `PROVEN`: the first line of
  `--toolchain-profile` when it is a `/nix/store/<32 chars>-<name>` path (else dropped), the
  sha256 and size of `--go-binary` (regular file, at most 512 MiB, path not recorded) and
  the `--go-root` VERSION and source inventory via `preflight.inventory`. An input that was given
  and cannot be read is a refusal (exit 3).
- The artifact (`uzi-go-cache-env/1`) holds the observed daemon and binding sections plus a
  declared proposal: a candidate charge map and claims for a read-only rootfs, a `/w` tmpfs
  (`size=4m`, `nr_inodes=256`), `/dev` and `/dev/shm` tmpfs without `nr_inodes`, a writable
  `/dev/mqueue`, pids-max, log-driver none and memcg max equal to swap max, and the inode
  ceiling proposal (total 65536 = retained 8192 + staging 1024 + work 56320, `owner_approved`
  false). Those parts are file tier and describe a proposal, not an observation. The threshold
  section is absent (`owner-input-not-supplied`). Kernel memory (tmpfs pages, dentry and inode
  slab) stays charged for every mount, non-storage and read-only ones included.

Output lifecycle (the order is part of the contract):

1. The `--out-dir` (default `.uzi/scratch/go-cache-env`) is pinned: its parent must be exactly
   `.uzi/scratch` and its name `go-cache-env` or `go-cache-env-<1-32 of [a-z0-9-]>`, without
   symlink components or `..`; any other name is refused. `.uzi/scratch` must already exist
   (the collector never creates it; a missing one is an `out-dir-invalid` refusal). It is validated, then deleted and
   recreated before anything else, through descriptors walked from the filesystem root with
   `O_NOFOLLOW|O_DIRECTORY`. An existing out-dir is deleted only when it holds nothing but
   regular files named `run.log`, `artifact.json` and `verdict.json`; any other entry (a
   directory, a symlink, a foreign name) is an `out-dir-invalid` refusal that deletes nothing.
   Directories with those names under `.uzi/scratch` are the only directories the tool deletes.
2. `run.log` is then created (`O_CREAT|O_EXCL|O_NOFOLLOW`) and duplicated over fds 1 and 2. run.log
   is the charged output sink: stdout is capped at 256 bytes (one summary line:
   verdict, exit, reason, `statvfs` f_frsize/f_bsize and apparent/allocated bytes of the three
   files, dropped stderr bytes) and stderr at 4 KiB (fixed reason codes and, on an unexpected
   error, the exception type and innermost frames, never the message). Excess is dropped and
   counted. Output before this step (a usage error) goes to the original stdio.
3. The caller redirects the original stdio to `/dev/null` (`>/dev/null 2>&1`), so nothing
   but run.log carries output.
4. `artifact.json` and `verdict.json` are written with `preflight.encode` (at most 64 KiB each
   including the newline) and `O_CREAT|O_EXCL|O_NOFOLLOW`; a fourth directory entry or an
   oversize document is refused. The verdict also records the out-dir `statvfs` block sizes and
   the artifact's apparent and allocated bytes.
5. Before exit, fds 1 and 2 must still be the (linked) `run.log` of the out-dir (same device and
   inode, `st_nlink` at least 1), else exit 4.

Exit codes: 2 NO-QUALIFIED-ENVIRONMENT (the normal result; 0 would mean qualified and does not
occur in this issue), 3 refusal or failure (usage, `out-dir-invalid`, `docker-unavailable`,
`too-many-images`, `image-not-in-listing`, `daemon-id-missing`, `host-input-invalid`,
`report-limit`, `internal-error`), 4 `run-log-mismatch`.

Evidence copy step (done by the lead after a live run, not by the tool): copy exactly
`artifact.json` and `verdict.json` from the out-dir to `e2e/go-cache-admission/evidence/`, then remove
the scratch out-dir. `test_observe.py` pins that directory: it must exist and hold exactly those
two files, each at most 64 KiB, with the sanitized key sets, a NO-QUALIFIED-ENVIRONMENT verdict,
none of the forbidden strings and official-or-`repo-<12 hex>` repository names; the artifact is
also re-evaluated through `qualify.evaluate` (no collector) and must stay unqualified with no
PROVEN leg.

`test_observe.py` runs the collector in subprocesses (it redirects fds 1 and 2) against owned
loopback TCP and AF_UNIX stub daemons; no real Docker endpoint, container, image pull or Go run.
Its stubs prove sanitization, routes, lifecycle and caps, not any environment.

The requirement-by-requirement record of the one candidate evaluated with this collector, its
verdict and the owner decision requests are in [environment.md](environment.md).
