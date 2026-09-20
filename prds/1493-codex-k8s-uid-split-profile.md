# PRD #1493: Codex on hosted k8s, an opt-in uid-split worker profile

**Issue:** [#1493](https://github.com/vtmocanu/uzi/issues/1493)
**Status:** Planned (2026-09-20).
**Parent:** #1106 M6 (Codex on hosted k8s). Follows #1492 / #1495 (shared-directory ownership, fixed in 0.84.0-rc.3).
**Execution:** two tracks.
- **Track U (send to uzi, Auto mode, gated plan):** M1 to M5. Controller + agent image entrypoint + agent probe and command sandbox + chart + docs. No migration. **No `.github/workflows/**` touched** in implementation or validation (`.claude/rules/prds.md`).
- **Track M (maintainer, local):** M6 and M7. A real-kubelet validation and live verification. A uzi worker can do neither. No workflow edit is needed anywhere in this PRD.

Use current `main` and a new working branch, never write to `main`.

## Problem

A `--harness codex` run cannot execute on a hosted k8s worker. With the directory-ownership bug fixed, the run now gets past provisioning and dies at launch:

> Codex effect roots require the A1 uid split; refusing to launch

Codex is fail-closed on a **three-identity OS model**: the trusted `worker` (uid 10001, holds the forge PAT and join token), the provider `runner` (uid 10002, holds the Codex credential and runs the app-server) and the command `runner-cmd` (uid 10003, runs model-authorized shell and file effects). On compose the image's root-started entrypoint establishes this and exports `UZI_UID_SPLIT=1`. On k8s the pod starts non-root at uid 10001 with every capability dropped, the entrypoint takes its single-uid branch and **unsets** `UZI_UID_SPLIT`, so both launcher guards throw.

Two further defects sit behind that one:

1. **A worker that cannot run Codex still advertises that it can.** The `codex_harness_v1` capability comes from the image's installer receipt alone. A split-less k8s worker therefore *claims* a Codex run and then fails it, instead of leaving it queued with the api's existing reason `no Codex-capable worker is online`.
2. **Command roots also require Landlock, and many kernels do not provide it.** `uzi-codex-command-sandbox` is fail-closed (`landlock unavailable`, exit 2). Measured 2026-09-20 on the current dev cluster: node kernel 6.1 built with `# CONFIG_SECURITY_LANDLOCK is not set`. Enterprise and vendor kernels commonly omit it or leave it out of the active LSM list. Today that makes Codex unusable on such a node no matter what else is fixed.

## Solution

Activate the mechanism that already exists and is already proven on compose, behind an operator opt-in. Do **not** rebuild the worker/execution boundary as IPC across containers.

1. **`workers.uidSplit.enabled`** (chart value, default `false`). When on, the controller renders the `worker` container to start as root with exactly the capability set compose grants (`SETUID`, `SETGID`, `SETPCAP`, `CHOWN`, `DAC_OVERRIDE`; everything else dropped, `allowPrivilegeEscalation: false`). The image's **existing** root entrypoint branch then migrates ownership, carves the runner trees, exports `UZI_UID_SPLIT=1` and `setpriv`-drops to `worker` keeping only ambient `SETUID`/`SETGID`.
2. **Make the pod and the root entrypoint branch k8s-safe.** Four known couplings, all already flagged in source comments or measured here: the `seed-nix` init container's identity, the docker lane's `TMPDIR`, first enablement over a populated single-uid `/data` volume, and the read-only join-token mount.
3. **`workers.codex.commandSandbox: required | best-effort`** (chart value, default `required`). `required` is today's behaviour, unchanged. `best-effort` applies Landlock whenever the kernel offers it and otherwise runs the command without filesystem confinement, relying on the uid split. It never relaxes the uid split.
4. **Honest capability advertisement.** The agent advertises `codex_harness_v1` only when the installer receipt is intact **and** the uid split is active **and** (Landlock is available **or** the sandbox mode is `best-effort`). The api side already routes and explains (`reasonNoCodexCapableWorker`); nothing changes there.
5. **Admission.** The kube-native worker namespace renders PodSecurity `baseline` instead of `restricted` while the uid-split profile is on. The docker-lane namespace already enforces `privileged` for its DinD sidecar and needs no change.
6. **Docs** for operators: both knobs, the posture each trades, and how to read `no Codex-capable worker is online`.

**The invariant, stated narrowly.** With both knobs at their defaults, the controller-rendered worker pod, the chart's controller Deployment (both new env vars are omitted, not rendered empty) and the command sandbox's enforcement are byte-for-byte today's. What does change on upgrade, by design, is advertisement: a hosted single-uid worker, and a split worker on a kernel without Landlock in `required` mode, stop claiming Codex runs they were going to fail. Those runs now queue with the api's existing reason instead.

## Why not the multi-container design PRD #51 and PRD #58 recorded

PRD #51 Decision 8 and PRD #58 sketched the k8s form as separate containers with per-container `runAsUser` and no `CAP_SETUID`. That sketch predates the Codex harness and no longer fits what was built:

- **It assumed two identities; Codex needs three**, each launched per effect root through a subreaper supervisor that the worker starts, signals, drains and reaps, and whose started-evidence (`uid`, zero live caps, `no_new_privs`, subreaper, bounding set) the launcher verifies. A process in another container cannot be spawned, waited on or reaped by the worker. The container form means three or more containers, a supervisor daemon in each, and an RPC protocol for spawn, stdio, signals, drain and exit status. PRD #51 itself called this "the hardest part of a security PRD".
- **PRD #58 rejected root entry for a reason that has since expired for the docker lane.** Its objection was that it "drops the label that bounds a compromised controller, for the sole pod that would need it". The docker-lane namespace was added later and already enforces `privileged`; every pod there carries a privileged DinD sidecar reachable unauthenticated on loopback. `CAP_SETUID` in the worker container is small next to that.
- **The A1 mechanism is kernel-enforced, shipped, and exercised by the compose e2e.** Reusing it moves no security logic; it only lets k8s reach code that already runs.
- **Docker-lane-only was considered and rejected.** It would avoid touching the `restricted` namespace, at the price of denying Codex to every ordinary non-Docker worker. Conditional `baseline` is the smallest standard admission change: all five capabilities are inside it, while `restricted` admits only `NET_BIND_SERVICE`.
- **Proportion.** uzi's hosted tier is a private install running the operator's own repos. The goal is to keep the containment the product already has, not to add layers.

The container form stays a legitimate future option for operators who must keep `restricted`. It is out of scope here and nothing in this PRD blocks it.

## Resolved facts (read locally, no internet needed)

Verified against `main` at `0ba6a0f2` on 2026-09-20, then independently peer-checked against the code. Re-verify an anchor before relying on its line number.

### The pod spec is controller code, not chart YAML

- `controller/internal/kube/render.go`: `podTemplate` builds the whole pod. `workerUID`/`workerGID` = 10001. Pod-level `SecurityContext` sets `runAsNonRoot: true`, `runAsUser`/`runAsGroup` 10001, `fsGroup` 10001 with `FSGroupChangeOnRootMismatch`, seccomp `RuntimeDefault`. One shared container `SecurityContext` (`allowPrivilegeEscalation: false`, drop `ALL`) is applied to both the `seed-nix` init container and the `worker` container. There is no `Capabilities.Add` anywhere in the controller.
- The worker container sets no `command`/`args`; it runs the image `ENTRYPOINT` (`/usr/local/sbin/uzi-entrypoint`). Overriding it from the pod spec stays rejected: it would bypass the security wrapper.
- `RenderConfig` (same file) is where operator knobs arrive; `controller/internal/config/config.go` binds them from env; `deploy/chart/templates/controller-deployment.yaml` sets that env from chart values. `render_dind.go` is the in-repo precedent for a per-container security posture that differs from the pod default.
- The join token is a Secret volume at `/run/secrets`, `DefaultMode: 0440`. The kubelet presents it as `uid=0, gid=10001, mode=0440`, read-only by construction.
- Any pod-template change moves the spec hash, so the fleet rolls when a knob flips. That is the intended behaviour.
- **Kubernetes has no ambient capabilities.** `capabilities.add` on a container that runs as a non-root uid is inert: the capabilities are not effective after `execve`. Any container that needs a capability here must run as uid 0.

### The image already carries everything the split needs

- `agent/templates/base/Dockerfile` and `agent/templates/jvm/Dockerfile` bake `worker` 10001, `runner` 10002, `runner-cmd` 10003 and group `codex-session` 10004. User `worker` is a member of group `runner`; user `runner-cmd` is a member of group `runner`; **neither runner identity is a member of group `worker`**. util-linux `setpriv` is asserted at build. `kubernetes-helm` and `kubeconform` are baked into the worker image.
- `agent/templates/entrypoint.sh`: the non-root branch unsets `UZI_UID_SPLIT`, `UZI_RUNNER_PATH`, `UZI_RUNNER_TMPDIR` and execs tini single-uid. The root branch runs `migrate_tree` (sentinel-gated, one-time) for `/nix` to `runner:runner` and `/data` to `worker:worker`, carves `/data/{runner,agent-home,provision}` as `worker:runner` 2775, makes per-uid 0700 tmpdirs, forces the token to 0400 `worker:worker` via `chown 0:0` then `chmod` then `chown`, exports the three split variables and `setpriv`-drops.
- The root window has **no `CAP_FOWNER`** by design: root cannot `chmod` a path it does not own. The token block already works around this by reclaiming ownership to root first; `CAP_CHOWN` needs no `FOWNER`.
- Compose grants the root window `SETUID`, `SETGID`, `SETPCAP`, `CHOWN`, `DAC_OVERRIDE` with `no-new-privileges:true` (`docker-compose.yml`). All five are inside PodSecurity `baseline`'s allowed list, as are `FOWNER` and `CHOWN` for the seed container, and `baseline` does not require `runAsNonRoot`.
- `setpriv --init-groups` resets supplementary groups from `/etc/group`, so a runner child does **not** inherit the pod's `fsGroup` 10001. A token at `0440 root:10001` is therefore readable by `worker` (primary gid 10001) and unreadable by `runner` and `runner-cmd`.

### Four couplings the k8s root start trips (the first two are flagged `FUTURE COUPLING` in `render.go`)

1. **`seed-nix` identity.** It runs as the pod uid 10001 and, on a toolchain-marker mismatch, must `chmod`, remove and replace the whole nix store and clear provisioning state on `/data`. After the first split start `/nix` is `runner:runner`, so the next reseed cannot touch it. Its clear is best-effort (`|| true`), so part of the failure is silent.
2. **Docker-lane `TMPDIR`.** The controller sets `TMPDIR` to the DinD-shared run workdir so that a `docker run -v <src>` bind source staged under `$TMPDIR` resolves in the daemon's filesystem. The root branch overrides it with `/tmp/uzi-worker` and exports `/tmp/uzi-runner`, both outside the shared workdir, so bind sources silently resolve to empty directories.
3. **First enablement over a populated single-uid `/data`.** `migrate_tree /data worker:worker` leaves every descendant worker-owned. The carve-out then re-owns only the three **parent** directories, non-recursively, and its `chmod 2775` is guarded on root owning the directory, so on an existing volume it is **skipped**. Legacy content under `provision/` (shared across runs), `agent-home/` and the plain lane's `runner/` stays unwritable by uid 10002.
4. **Read-only join token.** `chown 0:0` on a Secret mount fails with `EROFS`, `set -eu` aborts the entrypoint, and the pod crash-loops.

### Where Codex refuses, what advertises it, and what Landlock protects

- `agent/src/runner-uid.ts`: `uidSplitActive(env)` is exactly `env.UZI_UID_SPLIT === "1"`.
- `agent/src/codex/launcher.ts`: `launchCodexRoot` and `launchCodexEffectRoot` both throw `CodexUnsupportedProfileError` when it is false. Only `launchCodexRoot` has a negative test today.
- `agent/src/codex/codex-runtime-probe.ts`: `probeCodexRuntime` validates the installer receipt only and has a documented no-exec contract; `agent/src/main.ts` reports the result as `codex_capable`.
- `codex_harness_v1` gates **every** Codex-indicating run, including tool-less judge and review advice, which use the uid split but never the command sandbox. Schedules simply queue. Chat is unaffected: Codex Chat is independently forbidden.
- `agent/codex/supervisor/cmdsandbox/main.go` is the **only** place Landlock is applied. Its argv is built by the trusted worker (`commandSandboxArgv` in `agent/src/codex/codex-executor.ts`) and the command's environment is fully replaced (`buildCommandEnv`), so a mode the worker passes in argv cannot be influenced by the model. `confine` probes with `landlock_create_ruleset` and the version flag and returns `landlock unavailable` when the ABI is below 1. The container runtime's default seccomp profile permits all three Landlock syscalls, so a refusal means the kernel, not seccomp.
- What Landlock adds on top of the uid split is **filesystem confinement of a command to its run's worktree and a private tmp**. It is not what protects credentials: the join token, the worker's process state, the 0700 outbox, recovery and quarantine trees, and direct reads of the provider root (runner-owned 0700 plus gid 10004) all stay DAC-denied to uid 10003. Without Landlock, uid 10003 reaches what group `runner` and the world can reach: sibling worktrees, the nix store, the runner-group provisioning and agent-home **ancestors**, world-readable `/app`, the worker's bare repositories read-only, and `/proc` of other same-uid command processes. That is the same *class* of unconfined shell posture the Claude harness has on compose, not an identical one.
- **Ancestor writability is an integrity hole, not just a reach one.** Unlink and rename are governed by the parent directory, not the leaf. `/data/agent-home`, each run HOME and `codex-data` are `worker:runner` group-writable, and uid 10003 is in group `runner`, so without Landlock a command could rename or replace an active provider root it cannot read. The executor already treats a runner-group-writable parent as swappable (the #1496 quarantine logic). The Bash screener's base64/indirection residual explicitly relies on Landlock, so this must be closed at the filesystem, not the screener.
- `confine` collapses **every** initial `landlock_create_ruleset` error into `landlock unavailable`. The kernel documents only `ENOSYS` (not compiled in) and `EOPNOTSUPP` (compiled in but disabled) as the unsupported cases; `EPERM`, `EACCES`, `EINVAL` and an impossible ABI value mean something else is wrong.
- The Codex conformance clauses that cite Landlock (`e2e/codex-m4/clauses-codex.ts`: sibling isolation, `codex-o-command-root-home-denial`, and the base64/variable-indirection residual) are runtime-proven by the maintainer on a Landlock-capable host. They stay defined against `required`.

### Gates: what actually runs what

- `task gate:agent` runs npm checks only. The command sandbox is a **separate Go module** (`agent/codex/supervisor`, `go 1.26.0`, vendored, offline); its tests run under `task test:codex-supervisor` (Linux-only). **No CI job runs that target today.** The `lint-repo` CI job installs Go 1.27 and runs `task gate:repo` on Linux, so a Taskfile edge from `gate:repo` makes it CI-reachable with no workflow edit. The target's comment claims a Go 1.25 floor; `go.mod` says 1.26.0.
- `task gate:repo` does **not** render the chart. Helm `dependency build`, `lint` and `template` live only in `.github/workflows/ci.yml`. The worker egress allowlist admits the chart's OCI subchart pull (`ghcr.io` plus its blob host) precisely so a worker can render in-run.
- The KinD smoke (`deploy/values/kind-smoke.yaml`) runs with `workers.controller.enabled: false` and loads only the api and web images. It cannot exercise a hosted worker pod as it stands.

## Security posture (proportionate, stated once)

- **The uid-split profile adds:** the worker container, and the `seed-nix` init container, start as root with a short fixed capability list. After the entrypoint's startup window the worker runs as `worker` holding ambient `SETUID`/`SETGID`. This is the posture compose has shipped since PRD #51, including its disclosed residual: a compromised worker process can become any uid *inside its container*. `allowPrivilegeEscalation: false` and seccomp `RuntimeDefault` stay on.
- **It buys:** the Codex credential, the forge PAT and the join token become unreadable to model-authorized commands at the OS level. That is strictly stronger than today's hosted posture, where every hosted run (Claude included) executes single-uid.
- **It costs:** the kube-native namespace drops from `restricted` to `baseline` while the knob is on. `baseline` still forbids privileged containers, host namespaces and hostPath, which is what bounds a compromised controller.
- **`best-effort` costs:** on a kernel without Landlock, commands lose worktree confinement (the precise reach is listed above). Credentials stay fenced, and provider-root integrity is kept by sticky ancestors (M2, M3). The operator chooses this knowingly; the default never degrades.
- **This PRD deliberately does not:** relax either launcher guard, run Codex without the uid split under any setting, add a container, add IPC, or change the four `main`-is-never-touched guardrail layers.

## Out of scope

- The multi-container, no-capability form for operators who must keep `restricted`.
- A second protocol capability so that tool-less Codex advice could run on a worker that cannot run commands. With `best-effort` available this has no remaining user.
- Moving Claude runs onto the split on k8s. They gain it as a side effect when the knob is on (the same entrypoint branch serves both harnesses); no Claude behaviour is asserted beyond "still works".
- Any `.github/workflows/**` edit. None is needed: CI coverage of the new Go tests comes through an existing Taskfile edge.
- Per-cluster values for any specific deployment. Those live in the operator's GitOps repo.

## Milestones

Each Track U milestone ends green on its named gates, each run once to a log (`CLAUDE.md`, *Run economy*).

### M1: controller renders the uid-split profile (Track U)

- `RenderConfig` gains `UIDSplit bool`; `controller/internal/config/config.go` binds it from `UZI_WORKER_UID_SPLIT` (default false, strict bool parse like its neighbours).
- With it on, the **`worker` container** gets its own `SecurityContext`: `runAsUser: 0`, `runAsGroup: 0`, `runAsNonRoot: false`, `allowPrivilegeEscalation: false`, capabilities drop `ALL` add exactly the five compose capabilities.
- With it on, the **`seed-nix` init container** runs as uid 0 with drop `ALL` add `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, so a reseed works over both a legacy worker-owned store and a runner-owned one, and its `/data` clear stops failing silently. Not a non-root uid with added capabilities: those are inert on Kubernetes. Update the two `FUTURE COUPLING` comments in `render.go` to describe what is now true.
- Every DinD container keeps its current posture. Pod-level `runAsNonRoot`/`runAsUser` must not contradict the container overrides (move them to the containers that need them, or override per container; the render tests pin the result either way). `fsGroup` stays.
- `RenderConfig` also gains the command-sandbox mode and renders `UZI_CODEX_COMMAND_SANDBOX` on the worker container **only when it is not `required`**.
- With both knobs at their defaults, the rendered pod is **byte-identical** to today's, for both lanes. Pin that against the existing golden or spec-hash expectation.
- Tests in `controller/internal/kube/render_test.go`: on/off for both lanes, exact capability lists for worker and seed, DinD containers unchanged, spec hash differs between on and off, sandbox env present only when non-default.
- Gate: `task gate:controller`.

### M2: root entrypoint branch is k8s-safe (Track U)

- **Token.** When `/run/secrets/worker_token` cannot be re-owned (read-only mount), do not abort. Verify the exact kube posture instead, `uid=0, gid=10001, mode=0440`, plus a positive check that `worker` can read it, and fail closed on anything else. The compose path (writable secret) keeps today's reclaim, chmod 0400, hand-over sequence unchanged.
- **Tmpdirs.** When the pod supplies the DinD-shared run workdir as `TMPDIR`, derive both private tmpdirs **beneath it** (still 0700, still one per uid), so bind sources resolve in the daemon. With no such input the tmpdirs stay under `/tmp` exactly as on compose.
- **Legacy `/data`.** On the first split start over a populated single-uid volume, the three carve-out trees must end up usable by uid 10002: parents `worker:runner` with setgid and group-write (reclaim ownership to root before `chmod`, as the token block does, because the root window has no `CAP_FOWNER`), and legacy descendants either re-owned or, where the state is disposable, purged. One-time and sentinel-gated like `migrate_tree`. **Never a per-boot recursive chown**: it would re-own a requeued run's resume state. State in the Decision Log which trees hold non-disposable state and which choice was made for each.
- **Sticky carve-out parents.** The three carve-out directories get the sticky bit as well (3775, not 2775), on compose and k8s alike, so a `runner`-group member that does not own an entry cannot rename, unlink or replace it. This closes the provider-root swap for the ancestors the entrypoint creates; M3 covers the ones the executor creates. Nothing running as `runner-cmd` legitimately deletes or renames a top-level entry in those directories.
- **PVC root directories.** After migration, leave each mounted volume's **root directory** matching what the kubelet's `OnRootMismatch` check expects (group 10001, setgid, group-rwx) without changing the ownership of the contents, so the kubelet skips its recursive walk of the nix store on every start.
- The non-root branch is untouched: a non-root start still unsets the split variables and runs single-uid.
- Tests: extend `agent/test/templates-guardrails.test.ts` and the entrypoint shell tests beside it for each of the four behaviours and for the unchanged compose path. Build token-shaped fixtures from parts (`.claude/rules/prds.md`).
- Gate: `task gate:agent` plus `task lint:repo` (shellcheck).

### M3: optional Landlock and honest capability advertisement (Track U)

- **Sandbox mode.** `uzi-codex-command-sandbox` accepts a worker-supplied mode in argv, **before** the `--` separator. `required` (the default when the argument is absent) behaves exactly as today. `best-effort`: if Landlock is available, apply it exactly as today including the deny probe, and **still fail closed if applying it fails**; only a probe result of *unavailable* runs the command unconfined. In both modes keep `no_new_privs`, the private 0700 tmp, the `cwd`-inside-`root` check and exit-status passthrough.
- **"Unavailable" is defined by errno.** One typed probe result, shared by advertisement and execution: `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)` returning `ENOSYS` or `EOPNOTSUPP` is *unavailable*; any other errno, an ABI below 1, and every later create, add-rule, restrict or deny-probe failure is an *error* and stays fatal in both modes. `confine`'s current collapse of every initial errno into `landlock unavailable` must not survive.
- **`--probe` mode** on the same binary: that one call, no directory created, no policy applied, distinct exit codes for available, unavailable, and error.
- **Sticky executor-created ancestors.** `ensureCodexSharedDirectory` enforces 3770 (sticky plus setgid) for **every** caller, so the per-run HOME, the `codex-data` tree, and the two disposable advice parents (`codex-advice-data`, `codex-advice-cwd`, whose `<uuid>` children are provider roots) all get it without per-site logic. Uid 10003 then cannot rename, unlink or replace an active provider root through its group-writable parent. Add an advice-parent assertion beside the existing run-HOME one.
- **Worker config.** `UZI_CODEX_COMMAND_SANDBOX` (`required` default, strict parse, unknown value refuses to start) flows into `commandSandboxArgv`. The mode never comes from run, repo or model input.
- **Advertisement.** A thin availability wrapper called from `main.ts` combines the receipt probe, `uidSplitActive()` and the Landlock probe with the mode; `probeCodexRuntime` keeps its no-exec contract. Each failing precondition yields a distinct logged `reason`.
- **Visibility without noise.** No per-command warning in the model-visible stream. The worker logs the degraded mode once at startup and writes one line into each Codex run's feed.
- Both launcher guards stay exactly as they are. Add the missing negative test for `launchCodexEffectRoot` refusing without the split.
- **CI reachability.** Add `test:codex-supervisor` to `gate:repo`'s member list in `Taskfile.yml` (the `lint-repo` job runs it on Linux with Go); keep `platforms: [linux]` so a macOS `task gate:repo` skips it rather than failing; fix the target comment's stale "go 1.25" floor.
- Tests: Go tests beside `cmdsandbox/main_test.go` for mode parsing, `--probe`, and errno classification through an injected syscall seam (the existing deny-probe test already injects `open`): `ENOSYS` and `EOPNOTSUPP` degrade in `best-effort`, `EPERM`/`EINVAL` and an ABI of 0 stay fatal in both modes; `agent/test/codex-runtime-probe.test.ts` and `agent/test/codex-executor.test.ts` for the conjunction and the argv, including a malicious `--mode` after the `--` separator and a fake `UZI_CODEX_COMMAND_SANDBOX` in the tool env, neither of which may change the parsed mode; the opt-in Linux command-root suite gains a check that uid 10003 cannot rename, unlink or replace an active provider HOME.
- Gates: `task gate:agent` **and** `task test:codex-supervisor` (now also reached by `gate:repo`). Both logs go in the run's evidence.

### M4: chart wiring and admission (Track U)

- `deploy/chart/values.yaml`: `workers.uidSplit.enabled: false` and `workers.codex.commandSandbox: required`, each with a two-sentence comment stating the posture trade. An unknown sandbox value fails the render.
- `controller-deployment.yaml` passes `UZI_WORKER_UID_SPLIT` and `UZI_CODEX_COMMAND_SANDBOX` **only when set away from their defaults** (conditional omission, so the default Deployment is unchanged); the knob-off render asserts both are absent. `worker-namespace.yaml` renders `enforce: baseline` (audit and warn stay `restricted`, so the delta remains visible) only while the uid-split knob is on; correct the comment there that says the A1 mechanism "cannot run here". `worker-docker-namespace.yaml` is unchanged.
- `worker-invariants.yaml`: if it asserts a restricted posture, make the assertion follow the knob rather than deleting it.
- Helm `-}}` object-deleting trap: read `.claude/rules/stack.md` before editing templates.
- Validation, run explicitly because no task target does it: `helm dependency build deploy/chart`, `helm lint deploy/chart`, then `helm template` with the CI render values for knob-off and knob-on, asserting the controller env and the native namespace label in each, and `kubeconform` over both outputs.
- Gate: `task gate:repo` plus the helm commands above, all to logs.

### M5: docs and specs (Track U)

- Operator doc (the hosted-workers page under `docs/`): both knobs, the posture each trades, the one-line Landlock check, that a knob flip rolls the fleet, and how `no Codex-capable worker is online` reads for a split-less or Landlock-less fleet, including that Codex advice queues too. Run `task docs:sync` and commit the mirror.
- `ARCHITECTURE.md`: one paragraph under the hosted-workers section; link this PRD rather than restating it.
- `specs/human.md`: terse entry, tagged `(AI-synced 2026-09-20)`.
- `prds/1106-codex-harness-phase1.md` M6: record this prerequisite (the issue notes M6 omits it).
- The Codex conformance README states that sibling isolation, `codex-o-command-root-home-denial` and the base64/variable-indirection residual are `required`-mode claims, not claimed in `best-effort` on a kernel without Landlock.
- Decision Log below filled in with what was actually decided during implementation.
- Gates: `task check-docs:web` and `task gate:api` (the embedded-docs byte-equality test).

### M6: real-kubelet validation (Track M, maintainer, local)

The repo's KinD smoke cannot do this as it stands. Build a bespoke local overlay: build and load the controller and agent images, supply the controller credential, enable hosting. Then, profile on:

- pod admits under `baseline`; worker reaches Ready; entrypoint logs the split, not the single-uid line;
- inside the pod: `worker` reads the token, `runner` and `runner-cmd` cannot; `UZI_UID_SPLIT=1`; capability sets after the drop match compose; as `runner-cmd`, renaming or unlinking an entry under `agent-home` that it does not own fails;
- **forced toolchain-marker mismatch** reseeds a runner-owned store;
- **docker lane, both DinD postures:** a bind source staged under the runner tmpdir is visible inside the container;
- **starting from a populated single-uid PVC:** a run provisions after first enablement; then off, then on again;
- second start of the same pod skips the kubelet's recursive `fsGroup` walk (timed);
- profile off: pod spec identical to the previous release.

Fixes found here go through a local branch, peer review, then a PR with the bot reviewer, never back to a uzi run.

### M7: live Codex verification (Track M, maintainer)

- **M7a, current cluster (no Landlock):** uid split on, sandbox `best-effort`. Let the fleet roll, confirm the worker advertises Codex and logs the degraded mode, then re-run the throwaway Codex smoke issue end to end (plan, approve, implement, MR).
- **M7b, a cluster whose nodes provide Landlock:** same run with sandbox `required`.

Record both on #1493 and in #1106 M6. If a further blocker appears it gets its own issue; this PRD closes on the split being provisioned and proven, not on every later Codex defect.

## Dependency graph and parallelism

| Phase | Milestones | Depends on | Files | Track |
|---|---|---|---|---|
| 1 (parallel) | M1 | none | `controller/**` | U |
| 1 (parallel) | M2 | none | `agent/templates/entrypoint.sh`, `agent/test/**` | U |
| 1 (parallel) | M3 | none | `agent/codex/supervisor/cmdsandbox/**`, `agent/src/codex/**`, `agent/src/main.ts`, `agent/src/config.ts`, `agent/test/**`, `Taskfile.yml` | U |
| 2 | M4 | M1, M3 (env names) | `deploy/chart/**` | U |
| 3 | M5 | M1 to M4 | `docs/**`, `ARCHITECTURE.md`, `specs/human.md`, `prds/**`, `e2e/codex-m4/README.md` | U |
| 4 | M6 | Track U merged | none (validation) | M |
| 5 | M7a | M6, a released image | operator GitOps repo | M |
| 6 | M7b | M7a, a Landlock cluster | operator GitOps repo | M |

M2 and M3 both add tests under `agent/test/` but to different files. One uzi run does M1 to M5 serially; the table is for a human splitting the work.

## Success criteria

1. Both knobs at default: rendered pod byte-identical to the previous release for both lanes; sandbox enforcement unchanged. The only behaviour change is that workers which cannot run Codex stop advertising it.
2. Uid split on: the worker container starts root with exactly five capabilities and ends as `worker` with `UZI_UID_SPLIT=1`; neither runner identity can read the join token; a reseed and a docker bind mount both still work; a populated legacy volume is usable.
3. A worker without the split never advertises `codex_harness_v1`, under any sandbox mode. A worker with the split on a kernel without Landlock advertises it only in `best-effort`. Otherwise a Codex run stays queued with `no Codex-capable worker is online`.
4. Both launcher guards are unchanged and each has a negative test. `best-effort` degrades only on `ENOSYS`/`EOPNOTSUPP` and still fails closed on every other Landlock error. Uid 10003 cannot rename, unlink or replace an active provider root.
5. M6 passes on a real kubelet. M7a shows a Codex run reaching an MR on hosted k8s.

## Risks

| Risk | Mitigation |
|---|---|
| Root branch crash-loops on k8s for a reason unit tests cannot see | M1 and M2 handle the four known couplings; M6 exists to find the rest before release |
| Knob flip rolls the whole fleet and capacity drops to zero for minutes (#1470) | Documented in M5; operator flips it in a quiet window |
| `baseline` is a posture regression some operators will not accept | Default off; the container form stays possible later |
| `best-effort` is mistaken for "Landlock off" | It still applies Landlock wherever the kernel has it, and still fails closed on an apply error; tests pin both |
| Plan drifts into relaxing a launcher guard, or lets the sandbox mode come from run or repo input | Both named in *Security posture*; the plan review rejects them |
| A seccomp or implementation failure is mistaken for "no Landlock" and silently degrades | Unavailable is defined by errno (`ENOSYS`/`EOPNOTSUPP` only); everything else stays fatal, with tests for each class |

## Decision Log

| # | Decision | Rationale |
|---|---|---|
| D1 | Reuse the A1 root-entry mechanism on k8s behind an opt-in, instead of the multi-container form | Codex needs three identities with worker-owned process supervision; the container form is an IPC rebuild. A1 is shipped and kernel-enforced |
| D2 | `baseline` for the kube-native namespace only while the knob is on; audit and warn stay `restricted`. Not docker-lane-only | Smallest standard admission change that admits the pod; docker-lane-only would deny Codex to ordinary workers |
| D3 | Landlock is optional through an explicit operator knob, default `required` (maintainer decision 2026-09-20, reversing an earlier "no fallback" draft) | Kernels without Landlock are common. Credentials are fenced by the uid split, not by Landlock; what degrades is worktree confinement, the same class of unconfined shell posture the Claude harness has on compose |
| D4 | Fix routing in the agent's advertisement, not the api | The api already routes on the capability and explains the queue; only the advertisement was dishonest |
| D5 | Split-enabled `seed-nix` runs as uid 0 with three capabilities | It must rewrite a store owned by either uid, and added capabilities are inert for a non-root container on Kubernetes |
| D6 | Real-kubelet and live verification are maintainer milestones; CI coverage of the Go tests rides an existing Taskfile edge | A uzi worker has no cluster; the `lint-repo` job already has Go and runs `gate:repo`, so no workflow edit is needed |
| D7 | Sticky bit on every runner-group-writable ancestor of a provider root, in both sandbox modes | Unlink and rename are parent-governed; without Landlock uid 10003 could swap an active provider root it cannot read. Sticky is one bit and needs no new group |
| D8 | "Landlock unavailable" means `ENOSYS` or `EOPNOTSUPP`, nothing else | The kernel documents exactly those two as unsupported or disabled; collapsing other errnos would degrade on a seccomp or implementation fault |
