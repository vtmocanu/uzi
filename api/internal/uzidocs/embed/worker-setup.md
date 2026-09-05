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

- **New outbound egress.** Installing tools reaches the **devbox resolver** (`search.devbox.sh`, hit by `devbox install` to turn `name@version` into a nix ref), the **GitHub API** (`api.github.com`, hit by `devbox install`'s generated dev-env flake to resolve the nixpkgs revision — forge-independent, nixpkgs lives on GitHub regardless of your forge), and then **nix substituters** (`https://cache.nixos.org` plus any you configure) to fetch the package — the *new* egress this feature adds. A worker also reaches the forge directly for git (clone/fetch/push), so its full outbound set is `api` + the forge + `*.anthropic.com` (the Claude API) + the container-registry pair `ghcr.io`/`pkg-containers.githubusercontent.com` + the resolver + `api.github.com` + the substituters. Allow the resolver, `api.github.com`, and the substituters through an egress firewall if you run one; on a hosted kube-native worker all three are already on the shipped FQDN allow-list.
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

**A [docker sidecar](#docker-sidecar) costs more** on top of either number above: budget roughly an extra **1-2 GiB memory / 1 CPU** for the `dind` daemon itself, plus its own storage for pulled images and build cache — a compose `dinddata` volume, or its k8s equivalent. `dinddata` only grows; reclaim it the same way as `agentnix` (`docker compose down -v`, or `docker volume rm <project>_dinddata`) once you don't need what's cached. On a hosted k8s worker the equivalent is a persistent per-worker PVC rather than an emptyDir, and it is just as unbounded: reclaim it with `docker system prune` inside the worker, or by deleting and reprovisioning the worker, which recreates the PVC empty. Expect a real workload — uzi's own e2e suite pulls `postgres:17` and builds `api`/`web`/`agent` through the sidecar — to land in the **5-20 GiB** range.

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
  for instance) shows no bar for that volume rather than a misleading zero.
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

## Online, offline, busy

- **online**: a recent heartbeat arrived within the server's staleness window.
- **offline**: no heartbeat in time; the server re-queues any run the worker was holding.
- **busy**: the worker holds one or more non-terminal runs — by default just one at a
  time; see [Concurrent runs](#concurrent-runs) to raise that.

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
it opts into the same intra-user residuals just covered.

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
