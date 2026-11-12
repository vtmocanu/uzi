# M1 read-only inventory — issue #4203

Baseline: 18da9c670f4403f4fcab3891f8bb8c59757243ef.
This is prerequisite evidence, not a producing experiment or an A/B verdict.
No Go builds/tests, cache producers, image pulls, containers, mounts, quotas or
runtime configuration changes were performed for this inventory.

## Commands observed in this run

All five commands exited 0:

| Command | Observation |
| --- | --- |
| env GOENV=off GOTOOLCHAIN=local go version | go version go1.27.3 linux/amd64 |
| env GOENV=off GOTOOLCHAIN=local go env GOROOT | canonical Nix Go 1.27.3 share/go tree |
| command -v go | /opt/uzi-toolchain/bin/go |
| readlink -f "$(command -v go)" | executable inside that canonical Go tree |
| timeout 8s docker version --format '{{.Client.Version}} {{if .Server}}{{.Server.Version}}{{end}}' | client 29.9.2; server 29.8.0 |

GOENV=off avoids persisted Go environment configuration for these metadata-only
commands; GOTOOLCHAIN=local forbids automatic toolchain download. They do not
compile or test code. The observed Nix output matches the x86_64-linux output in
agent/devbox-global/devbox.lock:131 and its version at line 125.
This is version/path agreement, not image provenance. No current image digest was
provided, and none was inferred from the host toolchain.

SHA-256 (sha256sum exited 0), installed executable:
159888b397dac32306b4355c52d59ae5bc9b81515d387efa30f04657fc3a4cc7.

Installed source, relative to GOROOT/src/cmd/go/internal:

| Source | SHA-256 |
| --- | --- |
| cache/cache.go | 22da4896758772a443d90dd148719866d20e811bfefd3668597eae083b6f39b2 |
| cache/prog.go | a7daa1491a65421200f2106ff702aa274d03caab86464f205ec2cb7b774c0fc4 |
| cache/default.go | 3b4697e22509682f6339813ab7fdd9ed0f5628bdcbed261ee2eff3003b7f5d54 |
| cache/hash.go | e60b5396f38d90f8801fbcad2a47764ded0f6a6dba749c44991895ebdde12845 |
| work/buildid.go | 6de8324513d2a5f9f629e8ae7ede623d1e87ef7b826fd1441265fc845a256555 |

## Pinned caller inventory (starting points, not an all-writes proof)

- cache/default.go:44–58, initDefaultCache: MkdirAll, README and Open(dir)
  precede startCacheProg(cfg.GOCACHEPROG, diskCache).
- cache/cache.go:103–105, Open: creates 256 shard directories.
- cache/cache.go:566–587 and cache/prog.go:313–338: DiskCache.put and
  ProgCache.Put hash bodies with sha256.New and io.Copy; ProgCache sends IDs,
  body and size to the child.
- cache/cache.go:265–278, GetFile: Get, OutputFile and file-size check.
  cache/prog.go:289–295 requires DiskPath; OutputFile at 305–308 returns the
  remembered local path.
- cache/prog.go:371–374, FuzzDir: delegates to the disk-based default cache.
- work/buildid.go:756: PutExecutable requires c.(*cache.DiskCache).
  Archive caching instead uses c.Put at 742 and OutputFile for Export at 751.
  Archive hits must not be presented as executable-cache parity.

Writes outside the cache-program protocol:
ordinary cache initialization above; test/test.go:1599 plus internal/fuzz/fuzz.go:
1054–1057 (fuzz MkdirAll/WriteFile); modfetch/cache.go:654–712 and
modfetch/fetch.go:268–271,461 (metadata, archives, extraction and partial markers);
toolchain/select.go:45,364,506 (downloaded toolchain and destination writes);
work/action.go:298 (MkdirTemp under GOTMPDIR); work/exec.go:932 (_pkg_.a),
1657 (link a.Target),2035/2083 (move/copy final outputs). These are source
observations, not measurements of peak bytes, actual reuse or child write sinks.

## Gate outcomes

| Leg | Outcome | Exact limit |
| --- | --- | --- |
| Installed toolchain/source inspection | FIXTURE-PROVEN | Host Go version/path and listed source fingerprints only |
| Docker endpoint | FIXTURE-PROVEN | Version request succeeded; no topology, container permission or allocation proof |
| Current worker image/source binding | UNVERIFIED | No known image identity; host path/version is insufficient |
| Live soft-pressure threshold | UNVERIFIED | No current nonsecret configuration observation with provenance |
| Aggregate storage/memory enforcement | UNVERIFIED | No independently verified all-sink allocation contract |
| Child environment effects | UNVERIFIED | Pure builder tests only; actual Go/vet/test/race child effects not executed |
| Protocol/admission/cleanup and A comparison | UNVERIFIED, not reached | Producing prerequisites missing |
| Heavy handler/workersvc legs | UNVERIFIED/resource-incompatible | Owner-reported 269/576 MiB archives exceed 64 MiB body and 128 MiB staging; latter exceeds 2T=512 MiB |
| B | Not reached | No valid representative A failure; no authorization to infer one from fixture limits |

The unchanged worker computes the soft threshold at
agent/src/disk-reclaim.ts:947–950, using a 0.9 fallback (:867).
agent/src/client.ts:2174–2176 accepts a valid live heartbeat threshold;
agent/src/config.ts:649 supplies the configured soft margin. Source defaults do
not prove current settings. The harness intentionally does not default an
unknown live threshold.

The existing calibration pattern has bounded tmpfs mounts and memory/swap
arguments, but also a writable calibration bind (e2e/worker-uid/calibration.py:
293–299). Endpoint reachability and source flags establish no complete allocation
envelope here. Nothing was provisioned to fill the gap.

No producing leg, actual cache hit rate, overshoot, recompile time, peak storage,
free-space delta, Linux race build or alias/isolation proof is claimed.
Overall: **not viable yet**, due to unverified prerequisites, not a
mechanism-level negative for A. All frozen limits remain unchanged; heavy
workloads require a reviewed resource/scope decision before launch.
