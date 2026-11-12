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
environment or proxy/config is passed to probes: Docker uses direct Unix-socket
GETs, not a CLI. No environment enumeration occurs.

## Explicit inputs

All inputs are optional so unavailable prerequisites can be inspected:
- `--endpoint`: existing absolute Unix Docker socket (default
  /var/run/docker.sock); only GET /version and GET /images/sha256:…/json.
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
