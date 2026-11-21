# Go cache admission: environment qualification record (issue 4575, M3)

**Status: prerequisite evidence; NO-QUALIFIED-ENVIRONMENT; producer admission stays
closed; #4203 and #2013 remain open; nothing here is an A/B verdict or production
authorization.**

Each assertion carries a label and its source:

- **observed**: read from the daemon or host by the collector, bound to the collector-run revision.
- **cited**: read from source or docs at the stated revision; it describes code, not a live setting.
- **inferred**: a conclusion drawn from observed or cited facts, with the step stated.
- **UNVERIFIED**: no evidence exists; the missing mechanism or owner decision is named.

See [README.md](README.md) for the verifier (`qualify.py`) and collector (`observe.py`) contracts.

## 1. Scope and bindings

- **Candidate** (observed): one disposable container's worth of environment on this run's own
  DinD endpoint, a loopback TCP daemon (rendered at `controller/internal/kube/render_dind.go`
  `:219` and `:248`, cited at HEAD 58fce7a0d). The collector made GET requests only: no
  container was created, no image pulled, no mount, privilege or configuration changed.
- **Collector-run repository revision**: 6e02e276f24418c2ccd750932931f79a9e5dcdc3. **Scaffold
  revision**: d75ab6f0d5b3d5d94326bb454dc109f4e5d63f92 (the #4214 merge).
- **Evidence files**: [evidence/artifact.json](evidence/artifact.json) and
  [evidence/verdict.json](evidence/verdict.json). Later commits only tightened daemon-field
  allowlists; a unittest (`test_observe.py`) confirms the evidence is reproduced by the current
  collector. Daemon identity appears only as a per-run salted hash.

## 2. Requirements R1-R8

PROVEN = observed with the stated boundary. NEGATIVE = contradicted. UNVERIFIED = missing evidence.

| # | Requirement | Verdict | Evidence and what is missing |
| --- | --- | --- | --- |
| R1 | Preserved decisions | PROVEN (cited) | See R1 below. |
| R2 | Daemon and image binding | Daemon PROVEN (observed); image binding UNVERIFIED | See R2 below. |
| R3 | Live soft threshold | UNVERIFIED | See R3 below. |
| R4 | All-sink, all-mount charge map | UNVERIFIED | See R4 below. |
| R5 | Enforcement | UNVERIFIED | See R5 below. |
| R6 | Lane topology | UNVERIFIED (not applied) | See R6 below. |
| R7 | Frozen envelope | Declared only; ceiling unapproved | See R7 below. |
| R8 | Canary | UNVERIFIED, not run | See R8 below. |

### R1 Preserved decisions (cited)

- The boundary-only HOME trim policy and the Docker-wired owner answer are unchanged
  ([parent Decision Log](../../prds/2013-run-cache-growth-mid-turn.md#decision-log), D1-D20 and the
  carried-forward owner answers).
- Candidate A is session admission. It can overshoot in aggregate; it is not a hard cap.
- Candidate B is conditional on a valid measured A. No valid A exists, so B is not reached.
- There is no D13 or D20 exception.

### R2 Daemon and image binding

- **Daemon** (observed at 6e02e276f): 29.8.0 linux/amd64, runc 1.5.1, overlayfs on the
  containerd snapshotter, cgroup v2 with the cgroupfs driver, json-file default log driver,
  Memory, Swap and Pids limit support all true, not rootless.
- **Images listed** (observed): `postgres` and `hello-world`, by index digest with `id_kind`
  `index`. Under the containerd snapshotter the image Id is the index digest; no platform digest
  was collected, so the binding leg stays `binding-source-unbound`.
- **No Go-bearing image verified** (observed): only image names and metadata were listed; no
  image filesystem was inspected. Neither image declares Go (`go-not-declared`, which is not
  `go-absent-verified`).
- **Host-only Go** (observed, host coordinates, not an image binding): go1.27.3 binary sha256
  159888b397dac32306b4355c52d59ae5bc9b81515d387efa30f04657fc3a4cc7; source inventory sha256
  1aa9f80fc3bb7ba02cfb63ef4e5649e35af4b3d74341cf70e7a45465e8262f0d; the toolchain profile is a
  Nix store path.
- **Coordinates are not proof** (inferred): `/etc/uzi-toolchain-profile`, the devbox lock and a
  flake narHash name a toolchain but do not show what an image contains. The compose persistent
  `/nix` volume can differ from the image it was seeded from (cited, `docker-compose.yml`
  `agentnix:/nix` and its staleness comment).

### R3 Live soft threshold (UNVERIFIED)

Owner threshold inputs were not supplied at the plan gate. Provenance is not impossible; it needs:

1. the API threshold value and its observation time;
2. the worker's `UZI_DISK_SOFT_MARGIN` input, set or unset;
3. a binding to the image, pod and process incarnation that used it;
4. the effective value, from that incarnation's `soft_threshold` log line or another
   owner-authorized observation.

Cited mechanics at HEAD 58fce7a0d: the soft value is the threshold minus the margin, or the
threshold itself when that is not positive (`agent/src/disk-reclaim.ts:948-951`). The
`soft_threshold` field is logged only with admission on and only on a transition
(`:1001-1003`), and on the unknown-sample transition (`:984`). The heartbeat-accepted value
(`agent/src/client.ts:2618-2621`) is held in memory and never logged; a rejected value keeps
the last good one. The margin is parsed by `marginFraction` (`agent/src/config.ts:461-466`,
read at `:659`); `margin-golden.json` is generated from the real `loadConfig`.

### R4 Charge map, all sinks and all mounts (UNVERIFIED; no container mount facts collected)

The verifier's `CHARGE_SINKS` is the preflight sinks plus the container, daemon and export sinks.

| Surface | Rule in `qualify.py` |
| --- | --- |
| Read-only mount | No storage coverage needed. |
| Writable `proc`, `devpts` | Non-storage, listed with a reason. |
| Writable `mqueue`, `sysfs`, `cgroup`, `cgroup2`, network binds, bind or volume mounts, any other fstype | `unaccounted-writable-mount`. |
| Writable `tmpfs` | Needs `size` and `nr_inodes` coverage. Docker default `/dev` and `/dev/shm` lack `nr_inodes`. |
| `daemon_*` sinks and `container_logs` | Outer-domain sinks, charged only through the backing leg. |
| Writable fd | Must map to an accounted writable mount (`mount_id` alone when present, else superblock). |

**Kernel memory** (cited, inferred): tmpfs pages and dentry/inode slab of every mount, including
read-only and non-storage mounts, stay charged to the memory domain. The leg's `kernel_memory`
list holds every enumerated mount; no mount is dropped from kernel-memory accounting because it
is non-storage.

### R5 Enforcement (UNVERIFIED)

Claims are resource- and sink-specific triples `{kind, resource, sinks}`:

| kind | resource | sinks |
| --- | --- | --- |
| tmpfs-size | bytes | that tmpfs superblock |
| tmpfs-nr_inodes | inodes | that tmpfs superblock |
| exclusive-finite-fs | bytes and inodes | collector-observed capacity, exclusive |
| readonly-rootfs | write-deny | sinks resolving to the read-only root mount |
| pids-max | pids | process tree |
| log-driver-none | logs | `container_logs` |
| memcg-max-eq-swap | memory-swap | outer memory domain |

- **DAC claims are UNVERIFIED** (`dac-ownership-mode-unverified`): CHOWN/SETUID-class
  capabilities may remain and pre-existing writable fds are unaccounted. Non-root `--user` is
  defence in depth, not enforcement.
- Storage enforcement relies only on a read-only root mount plus bounded tmpfs. Write-deny
  evidence is EROFS only.
- Reason codes in play: `new-mount-possible` (needs CapBnd 0, NoNewPrivs 1 and the default
  seccomp profile, or `max_user_namespaces` 0), `boundary-not-allocation-enforcement` (refused
  kinds such as emptyDir `sizeLimit`, ephemeral-storage limits, monitoring, PVC requests,
  watchdogs, sampled `du`/`statfs`, json-file max-size), `boundary-removable`,
  `daemon-capability-unsupported` and `limits-not-applied-observed`.
- Support facts are not applied limits: the daemon reporting Memory/Swap/Pids support (observed)
  says nothing about what a container would have applied.

### R6 Lane topology (rendered vs observed)

Nothing below is observed for a container; "rendered" means cited from source.

| Lane | Rendered (cited) | Observed |
| --- | --- | --- |
| Hosted k8s without Docker | Worker data PVC shared by runs. No `limits.ephemeral-storage` by design (`controller/internal/kube/render.go`, comment near line 250); that decision is unchanged. emptyDir `sizeLimit` and ephemeral-storage are eviction-only ([Kubernetes ephemeral storage](https://kubernetes.io/docs/concepts/storage/ephemeral-storage/)). A memory-backed tmpfs needs a new mount: an owner decision. | None. |
| Hosted Docker-wired | `dind-data` daemon volume (`render_dind.go:43`, mounted at `:357`). The `runner-shared` subPath at `/data/runner` is shared by worker and dind (`:73-74`, `:82-83`): excluded as shared and not quota'd. DinD memory default 2Gi (`:121`; the chart default was not observed). `--storage-opt size` is unproven on overlayfs with the containerd snapshotter ([dockerd reference](https://docs.docker.com/reference/cli/dockerd/)). | Only the loopback daemon facts in R2. |
| Compose | `agentdata:/data`, `agentnix:/nix` (persistent, seeded from the image) and `pgdata`; `mem_limit` only (`docker-compose.yml:20`, `:362`, `:421-438`). The dev stack was not touched. | None. |

A fixture proves only that fixture.

### R7 Frozen envelope (declared, not enforced)

- Unchanged: 2 GiB = 768 + 128 + 1152 MiB; memory 1 GiB; clients 2; writers 1; body 64 MiB;
  objects 4096; records 8192; tiny 16 MiB / 30 s. Metadata, rounding and reports are charged in
  the work share.
- **Inode ceiling proposal** 65,536 = retained 8,192 (at least 2x objects) + staging 1,024 +
  work 56,320, pending owner approval (`inode-ceiling-unapproved`).
- The roughly 64 MiB kernel-memory figure (about 1 KiB per inode) is an **unverified estimate**,
  not a worst-case guarantee. Measure it inside the 1 GiB limit before any live allocation.
- Backing check: projected use must stay strictly below soft times capacity for bytes and inodes,
  from a fresh capacity sample (`backing-*` codes). No backing capacity was collected.
- The 269 MiB and 576 MiB heavy race archives stay resource-incompatible with body and staging
  limits. That is not an A failure.

### R8 Canary (UNVERIFIED, not run)

The canary was not run because R2 and R3 are not established. Distinguish the outer envelope
(the 2 GiB aggregate) from a sublimit (a per-container bound); a sublimit does not prove the
envelope. Owner requests are in section 4.

## 3. Expected vs observed verdict (evidence/verdict.json)

| Leg | Status | Reasons |
| --- | --- | --- |
| binding | UNVERIFIED | `binding-source-unbound`, `go-not-declared`, `no-go-bearing-image-verified` |
| threshold | UNVERIFIED | `owner-input-not-supplied` |
| charge_map | UNVERIFIED | `no-container-mount-facts` |
| enforcement | UNVERIFIED | `no-container-mount-facts` |
| aliasing | UNVERIFIED | `no-container-mount-facts` |
| envelope | UNVERIFIED | `inode-ceiling-unapproved` |
| backing | UNVERIFIED | `backing-soft-unknown` |
| canary | UNVERIFIED | `canary-collector-not-implemented` |

Overall: `qualified` false, `NO-QUALIFIED-ENVIRONMENT`, `producer_launched` false,
`production_authorized` false. The expected result for a read-only daemon-only collection.

## 4. Owner decision requests

Each is a request; none is granted.

- **(a) Threshold inputs.** Supply: the API threshold value and observation time; whether
  `UZI_DISK_SOFT_MARGIN` is set or unset on the worker (and its value); the image, pod and
  process incarnation; and the effective `soft_threshold` observation or another
  owner-authorized one. Blast radius: read-only; no change.
- **(b) Digest-pinned Go-toolchain image** already present on the run's daemon, with trusted Go
  output provenance (`signed-output-metadata` or `reproducible-build-attestation`). Acquiring one
  needs separate approval: there is no hosted pull (#225). Blast radius: none until acquired.
- **(c) Inode ceiling 65,536** with the split in R7. Blast radius: changes only the declared
  envelope; nothing is applied.
- **(d) Later canary on the (b) image**, in one disposable container:

  ```text
  --read-only --cap-drop ALL --security-opt no-new-privileges --network none \
  --log-driver none --pids-limit 64 --memory 64m --memory-swap 64m \
  --tmpfs /w:size=4m,nr_inodes=256
  ```

  plus explicit `size` and `nr_inodes` overrides for `/dev/shm` and any other runtime tmpfs, and
  the default seccomp profile. Whether Docker applies the overrides, and whether `/dev/mqueue`
  can be absent or read-only, must be observed; otherwise those mounts stay uncovered and the
  environment cannot qualify. Harness stdout at most 64 KiB; the run stays inside 16 MiB / 30 s.
  Cleanup: remove that one container by its exact owned name (a name outside the `uzi-`
  prefix). Blast radius: the run's own DinD daemon storage and memory, within the sublimit.
  A non-root `--user` is defence in depth only. A `postgres:17` run would be containment-only and
  cannot qualify (offered, not run).

## 5. Parent-gate carry-forward

- Parent M2 stays blocked unless the selected environment can exercise the unchanged
  `measureRunCaches`, the Go-unit helpers and `trimRunCaches` (`agent/src/run-caches.ts`,
  `agent/src/cache-cap.ts`) under the real runner/UID layout; otherwise it needs a separate
  qualification and a justification of any transferred evidence.
- Parent M3 needs an explicit owner resource/scope decision, including narrowing of the
  representative set and limits on conclusions. A production decision is separate.
- #4346, #4349 and cache-cap tuning are independent mitigations, not enforcement evidence.
- #4494: verified rebuildable Go cache entries are performance data, not session, WIP or custody
  state. This record grants no deletion exemption and adds no portability infrastructure.

## 6. Verification record

Reported by the dispatch unless marked otherwise; none of it is an environment result.

- `test:go-cache-preflight`: 18 tests in about 0.06 s at base f99e8a84d; 179 tests in about
  22 s at HEAD 58fce7a0d (5 of 5 runs green). The growth is about 45 subprocess launches of the
  collector, with no sleeps. The CI `gate:repo` impact is about +22 s as measured locally; the
  CI-runner delta is unmeasured.
- `check-docs:web`, `check:token-literals`, `check:changelog-entry` and `git diff --check` exited
  0 at 58fce7a0d. The margin golden table was regenerated from the real `loadConfig` and
  reproduced byte-equal by the reviewer.
- Mutation record:
  - M1: round 1 killed 24 of 26 mutants and every survivor was fixed (headroom `st_dev`
    keying, exclusive-finite-fs pair); round 2 survivors (`mount_id` agreement, enforcement
    evidence list) were fixed; round 3 killed all, including re-anchored `mount_id` aliasing and
    the fresh-API-only threshold.
  - M2: 18 of 20 killed, two equivalent (a redundant control-character check in `parse_ref`, a
    redundant symlink check in reset). Later survivors (out-dir name pin, header-cap checks,
    response close, `+4` terminator) were fixed with tests and confirmed killed.
- Limits: folds were deletion or relaxation only; the dirfd-walk race protection has no
  hermetic test.

## 7. What was not exercised

No container, no mount, fd, capability or seccomp facts, no applied limits, no allocation or
refusal at any ceiling, no backing capacity and no threshold. The full ceilings were never
exercised. Public-output safety: the evidence was reviewed by the reviewer and auditor and holds
no hostnames, worker IDs or daemon name; daemon identity appears only as a per-run salted hash.

## 8. Deferred non-blocking review notes

- The `conn.close` keep-alive path is untested.
- Two test stubs lack an EOF break and could hang on a regression.
- Rootless with a non-allowlisted `CgroupDriver` string in hand-built artifacts is treated as
  supported.
- The image Arch allowlist nulls legitimate 386 and mips64le values.
- Suite time could be trimmed by about 5 s with in-process argument tests.
