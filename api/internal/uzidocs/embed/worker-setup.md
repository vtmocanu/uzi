---
title: Worker setup
order: 60
audience: user
---

# Worker setup

A **worker** is the `uzi-agent` container: it connects to your uzi server, claims your queued runs, and drives them with the Claude Agent SDK. One worker per user is normal; it runs anywhere that can reach the server outbound (laptop, VM, CI runner), since it never needs an inbound port.

On a k8s deployment where an admin has turned hosting on, you can skip this whole page: provision a worker from **Settings → Workers** instead, on the **Add a worker** tab, and the cluster runs the container for you. See [Hosted workers](./hosted-workers.md).

## 1. Generate a join token

In uzi, open **Settings → Workers**, then the **Add a worker** tab, and use the **Register your own worker** card to register a worker (give it a name, e.g. `laptop`). The join token is shown **once**: copy it now, since only its hash is stored server-side (register a new worker if you lose it).

![Settings → Workers, showing a newly generated join token](img/worker-setup-join-token.png)

## 2. Anthropic credential

The worker runs agents against your own Anthropic credential, which must already be saved in uzi: see [Anthropic tokens](./anthropic-token.md). It's decrypted server-side and handed to the worker only inside a run's claim response, never stored on the worker beyond that run.

**Which credential, if you have several.** By default a worker spends your default token. To point it at a different one, use the picker on its row in **Settings → Workers**, or `uzi worker set-token <worker-id> <label>`. Because the token rides each claim rather than the worker, a rebind takes effect on that worker's **next claim** — no restart, no re-issued join token, nothing to change in your `.env`. Two caveats worth knowing before you rely on it:

- A bound worker's **chat** runs still spend your *default* token; the binding covers the run lane (issue, autopilot, and CI-fix runs).
- Deleting the token a worker is bound to does not break the worker: it silently falls back to your default from the next claim.

## 3. Run the worker

**Bundled compose profile** (the common case): set `UZI_WORKER_TOKEN=<join token>` in your `.env` (see [configuration.md](./configuration.md)), then:

```sh
docker compose --profile agent up
```

This starts the `agent` service pointed at the compose network's `api`, with its data on the named volume `agentdata`. Once it registers, **Settings → Workers** shows it as **online**.

**Standalone**, for a different host or a remote server (note the `-f` selecting the template Dockerfile, see [Worker templates](#worker-templates) below):

```sh
docker build -t uzi-agent -f agent/templates/base/Dockerfile agent
docker run -d -e UZI_API_URL=https://uzi.example.com -e UZI_WORKER_TOKEN=<the join token> \
  -v uzi-agent-data:/data --cap-drop ALL --security-opt no-new-privileges:true uzi-agent
```

Put a TLS-terminating proxy in front of a worker reached over an untrusted network: `api` itself listens plain HTTP.

## Worker templates

A worker image is built from a **template**: a curated, code-reviewed Dockerfile under `agent/templates/<name>/`. Templates exist for heavy or system-level dependencies a per-repo tool provisioner can't supply well (a JDK, system libraries); everyday CLI tools belong to the repo, not the image. Two ship today:

| Template | What it adds | Use it when |
|---|---|---|
| `base` (default) | Node 24 + git + bash + make + the `docker` CLI + go + python3 — the minimal worker | Most repos |
| `jvm` | `base` plus a JDK (`java`/`javac`) | Repos that build or test Java |

Every worker also ships the `docker` CLI and a default go/python3/pip toolchain, baked at build time onto both templates' `PATH` — no per-repo provisioning needed for either. The `docker` CLI alone can't run anything until a daemon is wired up: see [Docker inside a worker](./worker-docker.md). go/python3/pip share the nix store's first-run-only warm cache described under [Tool provisioning](#tool-provisioning) below: they refresh only when `/nix` is deleted and the worker reprovisions, not on every worker image upgrade. The baked set also covers a handful of everyday utilities (`ripgrep`, `opentofu`/`tofu`, and others) beyond go/python3/pip — the shared manifest at `agent/devbox-global/devbox.json` is the canonical list, not this page.

Pick a template at build time with the `WORKER_TEMPLATE` variable, which selects `agent/templates/<name>/Dockerfile`:

```sh
WORKER_TEMPLATE=jvm docker compose --profile agent build agent
WORKER_TEMPLATE=jvm docker compose --profile agent up
```

With `WORKER_TEMPLATE` unset, compose builds `base`. Set it to a **bare template name** only (one of the names above): it is interpolated into the Dockerfile path, so a value with `/`, `..`, or an absolute path is unsupported and would resolve outside `agent/templates/`. Standalone, point `docker build -f` at the template's Dockerfile (e.g. `-f agent/templates/jvm/Dockerfile agent`).

Each template's Dockerfile bakes its own name into the image as `UZI_WORKER_TEMPLATE` (a fixed literal, independent of the `WORKER_TEMPLATE` build variable), and the worker **reports** that at register, so **Settings → Workers** shows each worker's template. Because the reported value is the image's own baked-in identity, it flags a genuine mismatch when you build with one `WORKER_TEMPLATE` but declared another at issuance. This is observability only: the join token is still the sole trust anchor, so a worker's reported template is never used to accept or reject it.

## Docker sidecar

A worker's `docker` CLI (above) is inert without a daemon: bring up a **docker-capable worker** to actually run containers.

```sh
UZI_DIND_SOCKET=/run/dind/docker.sock docker compose --profile agent --profile agent-docker up
```

This adds a rootless Docker-in-Docker sidecar alongside the ordinary `agent` service; leave `UZI_DIND_SOCKET` unset (plain `--profile agent`) and nothing changes from today. See [Docker inside a worker](./worker-docker.md) for the trust model, the hosted (k8s) path, and sizing.

## Tool provisioning

Beyond the image's baked-in tools, a run can install **per-repo CLI tools** (kubectl, opentofu, jq, and so on) on demand with [devbox](https://www.jetify.com/devbox) (nix under the hood). Users set a repo's tool profile, opt into a repo's own `devbox.json`, and admins manage the allowlist — all covered in [Per-repo tools](./worker-tools.md). The operator points to know:

- **New outbound egress.** Installing tools reaches the **devbox resolver** (`search.devbox.sh`, hit by `devbox install` to turn `name@version` into a nix ref), the **GitHub API** (`api.github.com`, hit by `devbox install`'s generated dev-env flake to resolve the nixpkgs revision — forge-independent, nixpkgs lives on GitHub regardless of your forge), and then **nix substituters** (`https://cache.nixos.org` plus any you configure) to fetch the package — the *new* egress this feature adds. A worker also reaches the forge directly for git (clone/fetch/push), so its full outbound set is `api` + the forge + `*.anthropic.com` (the Claude API) + `api.openai.com`/`chatgpt.com`/`auth.openai.com` (the Codex API, ChatGPT-subscription backend, and OpenAI auth host — reachable regardless of which harness you actually run, since the restricted tier's allowlist is namespace-wide, not per-harness) + the container-registry pair `ghcr.io`/`pkg-containers.githubusercontent.com` + the resolver + `api.github.com` + the substituters. Allow the resolver, `api.github.com`, and the substituters through an egress firewall if you run one; on a hosted kube-native worker the resolver, `api.github.com`, and the default substituter (`cache.nixos.org`) are already on the shipped FQDN allow-list — but not any additional substituter you configure yourself, which you'll need to allow separately.
- **Provisioning is secret-scrubbed.** The install runs in a subprocess stripped of the forge token, the Anthropic token, and the join token, so a package's build hook cannot read your credentials. Only an explicit allowlist of tool environment variables (`PATH` and nix's TLS/locale vars) is passed back to the agent.
- **Provisioning failure fails the run** with a clear message rather than silently continuing without the tool.
- **The admin allowlist is gated to what the image bakes.** This is a server-side rule, not an egress one: allowlisting an unbaked package or saving it to a tool profile is rejected with a 400 naming it, and a run that reaches claim with one still fails the claim — enforced regardless of what a worker can reach. (A hosted kube-native worker can now resolve an arbitrary package name via the devbox resolver above — that egress was widened, not tightened, to unblock provisioning — so the resolver being reachable is not what enforces the baked-only rule.) So an admin can only allowlist a package the worker image already bakes into its shared devbox toolchain (`agent/devbox-global/devbox.json`). Allowlisting a new tool means baking it into that toolchain and rolling the worker image; the gate rejects the alternative with a clear message instead of letting a run hang. `kubectl` and `nodejs` are the two documented exceptions that stay allowlisted without being baked — see [Per-repo tools](./worker-tools.md#the-allowlist-admins).

The worker image installs a **pinned** devbox binary and nix at build time (no floating installer, no first-run download). Storage: the nix store is the `agentnix` volume at `/nix`; devbox/nix per-user metadata lands HOME-derived under `/data` (the `agentdata` volume). Both persist across `docker compose down`/`up`, so only a fresh `down -v` re-downloads packages.

## Resource stats and sizing

Once a worker is running, **Settings → Workers** and the Dashboard's "Worker load"
card show live CPU, memory, and disk gauges, self-reported by the worker from its
own cgroup and filesystem on every heartbeat. A worker under real load (running
the e2e suite) reported `cpu 1%` / `mem 0.1/4 GiB` / `disk 7.1/20 GiB` — a small,
honest number, since the SDK subprocess and git were mostly idle between tool
calls.

**Setting a memory limit is what makes the percentage bar appear.** With no limit,
the gauge shows absolute memory used and no bar. CPU shows a percentage whenever
the collector has a prior sample to diff against — it reads "—" on the very first
tick or right after a cgroup/process source flip.

**Compose** already sizes the `agent` service by default (`docker-compose.yml`):
`cpus: ${AGENT_CPUS:-2}` and `mem_limit: ${AGENT_MEM_LIMIT:-4g}` — 2 CPUs, 4 GiB
out of the box, so the memory bar appears without any extra configuration. Tune it
via `AGENT_CPUS`/`AGENT_MEM_LIMIT` in `.env` (see [Concurrent runs](#concurrent-runs)
for how to size these against `WORKER_MAX_CONCURRENT_RUNS`), or edit the service
directly for a one-off value.

**Kubernetes**, on the worker pod's container:

```yaml
resources:
  requests:
    memory: "1Gi"
    cpu: "500m"
  limits:
    memory: "4Gi"
    cpu: "2"
```

**A [docker sidecar](#docker-sidecar) costs more** on top of either number above: budget roughly an extra **1-2 GiB memory / 1 CPU** for the `dind` daemon itself, plus its own storage for pulled images and build cache — a compose `dinddata` volume, or its k8s equivalent. Under compose, `dinddata` only grows: reclaim it the same way as `agentnix` (`docker compose down -v`, or `docker volume rm <project>_dinddata`) once you don't need what's cached. On a hosted k8s worker the equivalent is a persistent per-worker PVC (`dind-data`), and a `dind-meter` sidecar samples its fill level and lets the worker prune its own build cache automatically once it's under pressure — see [Docker inside a worker](./worker-docker.md#dind-data-metering-and-automatic-pruning) for what that covers and what it doesn't. Expect a real workload — uzi's own e2e suite pulls `postgres:17` and builds `api`/`web`/`agent` through the sidecar — to land in the **5-20 GiB** range.

**On a hosted k8s worker, a per-pod Codex command cache draws on the pod's ephemeral storage, separately from the dind budget above**: it exists only under the uid split. A worker-only emptyDir (`codex-cmd-cache`) holds one directory per run with that run's Codex command build cache (`GOMODCACHE`/`GOCACHE`/npm). It is removed at the end of any run whose command roots have all drained, and via `--remove-cache` after a cache-holder loss; otherwise it is kept on disk until the startup orphan reaper sweeps it. It counts toward `workers.ephemeralRequest`/`workers.docker.ephemeralRequest`, the docker tier's 5Gi default adds provisional headroom for it (issue #1757), unmeasured; the plain tier's 512Mi does not yet, pending a hosted measurement of a Go-heavy Codex run's peak usage.

What the gauges mean:

- **CPU %** is the share of *allowed* CPUs used, not host CPUs: with the compose
  default (`AGENT_CPUS=2`) or a `2` CPU k8s limit, 100% means both are saturated.
  With no CPU limit, it's normalized by the host's core count instead.
- **Memory** matches `docker stats`, not the raw cgroup number: reclaimable page
  cache is excluded, so a git-heavy workload pinning cache near the limit doesn't
  cry wolf.
- **Freshness**: the worker samples every 15s (heartbeat) and the UI polls every
  10s, so a gauge can lag reality by up to ~25s — not a live feed.
- **Disk** shows used/total bytes per reported volume — `/nix` (the tools
  cache) and `/data`, each a separate bar when both are reported, one bar when
  only one is. A volume that fails to report (no `/nix` mount in dev/compose,
  for instance) shows no bar for that volume rather than a misleading zero. A
  docker-capable hosted worker additionally shows a **Disk dind** bar for the
  `dind-data` volume — used/total bytes, plus an inode percentage shown
  alongside only when the (rounded) inode fill exceeds the (rounded) byte
  fill. It's display-only, sampled by the `dind-meter` sidecar and reported by
  the worker on its own heartbeat; see
  [Docker inside a worker](./worker-docker.md#dind-data-metering-and-automatic-pruning).
- A dropped or malformed sample self-clears the gauge for that one tick (by
  design: stale-but-plausible is worse than briefly blank) rather than holding a
  stale-looking value.
- **`source: process`** (hover the gauge for the tooltip) means the reading covers
  the worker process only, blind to the SDK/git/devbox child processes it spawns.
  This happens on a cgroup v1 host, when running un-containerized in dev, or under
  `cgroupns=host` (an older/explicit runtime setting) — the common containerized
  case (private cgroup namespace, the Docker/kubelet default) reports the full
  container instead and needs no configuration.
- An **offline** worker shows its last-known stats dimmed, never live-looking.

## Worker disk safety (PRD #1809)

A run's build caches can grow far faster than the worker's data volume
shrinks on its own, so uzi bounds them on a worker instead of relying only
on the last-resort [disk self-heal recycle](hosted-workers.md#disk-self-heal).
The bounded subtrees are the Go build cache (`.cache/go-build`), the Go
module cache (`go/pkg/mod`), and the npm cache (`.npm/_cacache`); other
caches, including `node_modules` and pip's, are not individually tracked or
trimmed by any of the mechanisms below. Four mechanisms, from routine to
last resort:

1. **Cache drop on park.** A Claude run parked with its process ended
   (`limit_wait`, `recovery_wait`, `paused`) has its rebuildable caches
   dropped immediately — everything else in its HOME (the transcript,
   session state) stays for the resume, so nothing needed to pick the run
   back up is lost. This immediate drop is Claude-run-only (a Codex run's
   caches sit on its own per-run volume); the periodic reclaim pass below
   drops the caches of any process-ended parked worker-owned HOME, no
   harness filter.
2. **In-run cache cap, two layers** (D4). A *soft* layer trims a running
   run's own caches (least-recently-used entries first) at a proven quiet
   point between turns — no live process, never judged from file mtimes —
   whenever they exceed the run's share of the volume; a run that stays over
   the cap parks preventively (uncounted, so this cannot by itself fail a
   run). A *hard* layer watches the data volume itself on every stats tick,
   independent of turn boundaries, and — only once the volume nears the
   api's disk-pressure threshold — stops the running Claude run with the
   largest caches and parks it with a **counted** `data_volume_full` park.
   See [`UZI_RUN_CACHE_CAP_ENABLED`/`UZI_DISK_HARD_STOP_ENABLED` and the
   rest of this group](configuration.md#worker-disk-safety-prd-1809) for the
   exact thresholds and how to tune or disable either layer.
3. **Periodic reclaim and a bounded admission stop** (D7, D5). Separately
   from the in-run cap, the worker periodically sweeps for space it can
   prove safe to remove — a terminal run's leftover HOME, a parked run's
   rebuildable caches, an aged model-pass HOME — and runs the same sweep
   out of cycle whenever the volume crosses a soft threshold below the disk-
   pressure line. The worker stops claiming from the moment the volume
   crosses that soft threshold — including a resume of one of its own
   parked runs; the run-lane claim cannot be narrowed to this worker's own
   work — not only after a reclaim pass finds it still over. This admission
   stop is itself time-bounded: once a reclaim pass that started after the
   crossing has finished and the volume is still over the soft threshold
   `UZI_DISK_ADMISSION_MAX_WAIT` after the crossing, claims reopen (with a
   warning) so a worker with nothing left to reclaim does not idle forever;
   they close again only on the next fresh crossing.
4. **Disk-full classification and a bounded park** (D6). This reclaim-retry-
   park treatment covers only two writes: the claim/resume preflight (before
   anything is cloned) and the clone/fetch itself. If either fails
   disk-full — a recognised `ENOSPC`/`EDQUOT` signal or git's own
   diagnostics, confirmed against the volume's actual free space and inodes
   — the worker runs one more reclaim pass, retries once, and parks the run
   as `recovery_wait`/`data_volume_full` instead of failing it outright. A
   disk-full write *after* the clone exists (e.g. a build failing mid-turn)
   takes the normal failure path instead — it is not retried or parked here.
   Mid-run disk parks come only from mechanism 2 above: the soft cache-cap
   park (uncounted) and the hard pressure stop (counted). The claim/resume
   and clone/fetch park has the same lifetime cap
   (`UZI_RUN_DISK_PARK_MAX`, shared with the hard-stop park); past it, the
   run fails with `fail_origin=data_volume_full`. See [Worker data volume
   full](run-recovery-wait.md#worker-data-volume-full) for exactly what an
   owner sees and how counted parks differ from preventive ones.

**The [disk self-heal recycle](hosted-workers.md#disk-self-heal) (PRD #837)
is still the last resort**, unchanged by any of the above: a hosted worker's
volume that fills up anyway (sustained at or above the disk-pressure
threshold) still gets drained and recycled, losing everything on `/data`.
Everything in this section exists to make that outcome rare, not to replace
it — none of these mechanisms touch `/nix` or trigger a recycle themselves.
See [configuration.md](configuration.md#worker-disk-safety-prd-1809) for
every tunable in this section and its default.

## Online, offline, busy

- **online**: a recent heartbeat arrived within the server's staleness window.
- **offline**: no heartbeat in time; the server re-queues any run the worker was holding.
- **busy**: the worker holds one or more non-terminal runs — by default just one at a
  time; see [Concurrent runs](#concurrent-runs) to raise that.

## Message outbox

An api outage — of any length — no longer costs a run its message feed. The worker's message batcher keeps flushing to `api` as usual; once flushes have been failing transiently for `WORKER_TRANSIENT_TRIP_MS` (default 10m), it stops trying the network and starts spilling run messages to a durable, authenticated on-disk outbox under `<dataDir>/outbox` instead of tripping the run. A per-worker drainer replays the backlog, in order, once the api comes back — however long the outage lasted, as long as the retained data stays within the quotas below; past them, some message frames are dropped and replay as contiguous per-seq gap markers rather than the original messages. The outbox can't grow without bound: a per-run quota, a worker-total quota, an in-memory buffer cap while spilled, and a retention window on fully-drained runs (`WORKER_OUTBOX_RUN_MAX_BYTES`, `WORKER_OUTBOX_MAX_BYTES`, `WORKER_OUTBOX_SPILL_BUFFER_BYTES`, `WORKER_OUTBOX_RETENTION`) all keep how much disk it ever holds bounded — see [configuration.md](./configuration.md#worker-container-agent) for every knob and its default.

**Same-uid caveat on the hosted runtime.** A [hosted worker](./hosted-workers.md) runs the `#58` single-uid posture (see [proc-hardening.md](proc-hardening.md)): the model process shares the worker's own uid, so it can read the outbox's signing key and forge, truncate, or delete any run's outbox files. There, the outbox protects against an api outage, not against a hostile model — integrity and durability against the model itself are a documented residual, closed by a later container-split follow-up (the model running in its own container behind an IPC boundary). On a self-run worker's [uid-split](proc-hardening.md#the-mechanism-as-built-mechanism-a1) start, the outbox tree is unwritable by the `runner` uid the model runs as.

## Surviving an api restart mid-run

The message outbox above keeps a run's message feed intact across an api
outage; this is about the run's own status. On every heartbeat and claim, a
worker reports which run-lane attempts it's actually executing and in which
phase (running, or parked at a plan-approval or clarifying-question gate).
Once the api is back (after a short boot grace where it holds off declaring
workers stale, so a worker that only lost the api and not its own health
isn't wrongly re-queued), it uses that report to restore a run the outage
flipped to `queued` back to its exact phase within one heartbeat — no
re-queue budget spent, no new custody hold. A run that was genuinely
re-queued during the outage gets that charge refunded once it's back. See
[configuration.md](./configuration.md#server-api) for `SWEEPER_BOOT_GRACE`
and the related knobs.

The same write-ahead journal covers a run-lane attempt's terminal outcome (an issue run, a judge, or a review; chat is excluded — it never carries a claim generation to fence on). Before the worker ever sends a `completed`/`failed` report, it journals the outcome to the same durable outbox tree the message outbox above uses, keyed by the run and its claim generation. Once the api is back, replay sends the run's own messages first and the outcome only after they've caught up, so a run that finished during the outage still gets its MR and its final state exactly once — a journaled outcome is never re-attempted, and a duplicate claim on the same run is refused rather than executed. If the worker container restarts mid-outage, it resolves every pending journal from disk before its claim loops start, so the outcome lands even though the executor that produced it is gone. An outcome the api permanently refuses (most often because the run's messages can't be reconciled, or a completion permit no longer matches) is never discarded on a timer: it's surfaced on the run as a held outcome and cleared only by the owner explicitly cancelling with the discard bit set (`uzi run cancel --discard-pending-outcome`, or the same confirmation from the run page). Same-uid caveat as above: the outcome journal lives in the same worker-owned tree as the message outbox, so on the hosted single-uid runtime it protects against the outage, not against a hostile model.

## Run artifacts and the sandbox

The worker provisions `.uzi/scratch/` inside each runner checkout before the
agent starts. [ADR-1719](../adr/1719-run-scratch-dir.md) records the path policy.
Use it for gate logs, screenshots and plain exported review snapshots.
For a gate log, create a unique file with
`mktemp .uzi/scratch/gate-log.XXXXXX`. A snapshot exported with `git archive`
has no git metadata or installed dependencies; run git-dependent gates in the
real checkout. Scratch persists only while the identical runner clone is
retained through a park and resume. A reseed, even on the same worker, and a
cross-worker recovery create an empty scratch directory. Do not rely on it for
durable recovery.

The worker locally excludes the scratch directory from ordinary staging.
Checkpoint and final publication refuse a scratch path in any commit being
sent or an index/WIP capture. The check covers the branch's full history, so a
branch that ever committed a scratch path stays refused even after a later
commit deletes it. An ignore rule does not prevent forced staging;
publication refusal guards that case. A repository collision at `.uzi/scratch/`
fails provisioning instead of replacing repository content.

Direct file-tool paths are limited to the run worktree on Claude and Codex.
This is a tool policy, not a promise that every shell command is filesystem
confined: Claude's Bash guardrail screens commands but does not jail paths;
Codex also screens the shell working directory and uses Landlock where
available. OS permissions and the worker/runner uid split still apply. The
existing credential and `.git` restrictions also apply inside scratch.

### Environment facts in the lead's prompt

Before the lead's first turn (the plan turn, or the first implement turn on a
run with no plan turn, e.g. a pre-approved resume), the worker runs a fixed
probe it owns (a constant `node -e` script, never a repo script). It measures
three facts: whether `/proc` can be enumerated (a `/proc` mounted with
`hidepid` or `subset=` lists entries but hides other processes, so it counts
as limited), whether the command's actual `$HOME` is writable, and whether its
actual `$TMPDIR` is writable. A probe
that times out or fails is reported as "not verified", never as access being
available.

The timeout and the point in the run differ by harness. On Claude, the probe
runs as the runner uid with the agent's SDK env — a copy with `NODE_OPTIONS`
and `NODE_PATH` removed, so nothing outside the fixed script can change what
it runs — with a 2-second cap, right after tool provisioning finishes and
before the JS dependency install starts. On Codex, the probe runs through the
same registered command sandbox the agent's shell commands use, right after
the first provider epoch starts and before any turn, with a 10-second cap: it
starts a sandboxed command root, not a bare process, so it needs longer. On
Codex the result is cached for the run rather than re-measured when the
provider epoch is recreated, and each Codex command gets its own private
`$HOME` and `$TMPDIR`, so "writable" there says nothing about what persists
between commands. Either way, if the worker cannot confirm the probe's own
process was cleaned up — a check that can add a short bounded wait after a
timeout — the run fails before that first turn rather than reporting a fact
it can't stand behind.

The worker also passes through, unprobed, the harness and whether Docker is
wired — worker configuration, not a measurement. It reports no egress tier:
the worker receives no egress-tier configuration to pass through.

The facts feed the prompt on a schedule that differs by harness: on Claude,
the plan prompt and the first implement prompt only; on Codex, the plan
prompt and every implement prompt. The block lists only the limits and "not
verified" facts it found, plus a Docker-not-wired line when Docker isn't
wired on this worker — so on a non-Docker worker it appears every run, even
with every probed fact `ok` — plus a fixed rule: when a gate is blocked by a
verified limit, the lead records it as not run or blocked, runs the checks
that remain valid, and names the CI or other test lane that must complete
validation. A plan needs an **Environment limits** section only when a
measured fact actually affects its planned validation — a missing `/proc`
alone does not prove a gate is blocked.

When the block has anything to report, the worker also posts one status
line to the run's activity summarizing the facts, e.g. `environment facts
(codex): /proc limited; $HOME ok; $TMPDIR not verified; docker not wired`.
This is what makes the [uzi-watcher](../.agents/skills/uzi-watcher/SKILL.md)
plan-trap check for a gate the environment facts affect actionable without
reading the full transcript.

## Concurrent runs

By default a worker executes one run at a time. Set `WORKER_MAX_CONCURRENT_RUNS`
above 1 (see [configuration.md](./configuration.md)) to let it run several runs
concurrently, each in its own slot. A slot is roughly one SDK CLI process, its git
operations, and any devbox tool provisioning it triggers — size the cap to what the
host can actually run at once; the worker still honors a value above the soft
ceiling of 8, but warns at boot that it probably shouldn't. The cap is worker-side
only: it's reported at registration so **Settings → Workers** can show `active/cap`,
but the server never enforces it.

A run parked at the plan-approval gate holds its slot for the whole wait, up to
`WORKER_PLAN_APPROVAL_TIMEOUT` (default 24h) — approve your plans, since an
unapproved one pins a slot until it times out. At the default cap of 1 that's
already today's behavior. A run parked on a clarifying question holds its slot
the same way, up to `QUESTION_TIMEOUT_SECONDS` (default 24h) — see [Answering a
question](./run-activity.md#answering-a-question).

Raising the cap is an informed trade-off, not a free speedup:

- **Bash isn't jailed to its own worktree.** The guardrail denies push and
  credential-reading commands but not writes outside a run's own worktree, so a
  prompt-injected run could shell-write into a sibling's worktree or the shared
  bare-repo cache.
- **One container, one memory budget.** A runaway run can OOM the whole container,
  requeuing every in-flight run together; raise `RUN_MAX_REQUEUES` (default 1)
  alongside any cap above 1 so an innocent sibling isn't failed outright by another
  run's crash.
- **One Anthropic token, N runs.** Every slot on a worker shares that worker's
  credential, so a higher cap multiplies 429 pressure on it — the SDK's own
  retry/backoff is the only mitigation today. Binding two *workers* to two
  different tokens does split the pressure between them; binding cannot split
  it *within* one worker, and uzi never fails a throttled run over to another
  credential on its own.
- **Same-repo runs still serialize.** Two concurrent runs against the same repo
  queue behind each other at the git layer — correct, just not actually parallel.

These are single-container, shared-resource specifics (a shared filesystem, memory
budget, and Anthropic token); the cross-run credential read is closed by the
worker/runner uid split (PRD #51, [proc-hardening.md](proc-hardening.md)) on the
root-started compose stack (a #58 single-uid start does not split). The design behind this feature
(`adr/0042-worker-run-concurrency.md`) has the full research and the
container-per-run model that eventually closes the rest. How a queued run
picks *which* worker to land on, across a multi-worker fleet, is a separate
decision — see [ADR-216](../adr/0216-fleet-aware-claim.md) and
[Multiple workers, removing a worker](#multiple-workers-removing-a-worker)
below.

**Hosted workers don't set `WORKER_MAX_CONCURRENT_RUNS` directly.** For a
controller-managed k8s worker (see [Hosted workers](./hosted-workers.md)) the cap
comes from the chart value `workers.maxConcurrentRuns` (default 1), which the
controller renders into the pod's `WORKER_MAX_CONCURRENT_RUNS` env for you. Raising
it is an operator action — it needs a new controller/chart release to take effect,
since hosted workers only roll on release — and the operator must size the preset
to hold that many concurrent runs. It's the same knob described above, and raising
it opts into the same intra-user residuals just covered. An ephemeral (run-bound)
hosted worker is always recorded with a cap of 1, whatever it advertises, since it
only ever runs the one run it was created for. Its `/data` volume also has its own
operator-set size, 20Gi by default, rather than its size preset's (see
[Type and size](./hosted-workers.md#type-and-size)).

## Multiple workers, removing a worker

Register more than one worker (e.g. `laptop` and `ci-runner-1`); each claims independently from your queue. Since PRD #216 the server itself spreads queued runs across your idle workers as part of the claim: a worker already holding a run defers a fresh queued run to a less-loaded, eligible peer instead of taking a second run while that peer is idle, so two runs queued together against two idle workers land one per worker — without lowering anyone's `WORKER_MAX_CONCURRENT_RUNS`. A resumed run still returns to its prior worker first, within the affinity grace described above. **Settings → Workers → Delete** removes a registration (refused while it holds a non-terminal run); it doesn't stop the container itself.

## Seeing raw run events

The run view shows a terse, readable feed, not raw JSON. To watch the complete raw events a run emits (every tool call, tool result, and status frame), start the worker with `UZI_LOG_LEVEL=debug` and follow its logs:

```sh
UZI_LOG_LEVEL=debug docker compose --profile agent up -d
docker logs -f uzi-agent-1
```

Each run event is logged as a `run event` line with its `kind` and payload. Secrets (your worker token, forge credential, and Anthropic token) are redacted before anything is written. Leave the level at `info` for normal use, where these per-event lines are absent.

See [configuration.md](./configuration.md#worker-container-agent) for every worker environment variable, and [ARCHITECTURE.md](../ARCHITECTURE.md#run-lifecycle) for claim and requeue semantics.
