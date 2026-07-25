---
title: Proc hardening
audience: design
---

# Worker/agent uid split: the local containment and its k8s mapping

This is the **implemented** design (PRD #51), not a sketch. On the bundled
compose stack the worker and the untrusted code-execution surfaces (the Claude
Agent SDK, the self-improvement check runner, the devbox/nix provision hooks, and
the runner clone's own git) run **under two different OS uids in one container**,
so the credential-holding worker's secrets are unreadable by the code the agent
influences. It supersedes the pre-split "accepted same-uid residual" this page
used to describe.

## What was closed, and why it mattered

Before the split, the worker and the agent it drives shared one uid (`uzi`).
Because they shared a uid, a process the agent left behind could read the
worker's `/proc/<pid>/environ` (holding the join token) and any transient git
child's environ (holding the bot PAT during the push window), and any code the
agent could run as that uid could read the join-token **file** and plant a
code-exec key into a git config source the worker later read. No in-container
string guard can stop a same-uid process from reading `/proc` or a
same-uid-readable file — the robust close is **structural**: the untrusted code
must not share the worker's uid.

## The mechanism, as built (mechanism A1)

Two OS uids are baked into the worker image
([`agent/templates/base/Dockerfile`](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/agent/templates/base/Dockerfile)):
**`worker`** (uid 10001, the credential custodian) and **`runner`** (uid 10002,
cap-less, runs the untrusted surfaces). `worker` is a member of group `runner` so
it can reach runner-group trees; `runner` is **not** in group `worker`.

- **Root entrypoint, then a setuid drop.**
  [`agent/templates/entrypoint.sh`](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/agent/templates/entrypoint.sh)
  starts as root (compose grants `cap_add: [SETUID, SETGID, SETPCAP, CHOWN,
  DAC_OVERRIDE]`), runs a minimal startup window (volume-ownership migration,
  token hardening, per-uid tmp + `/data` carve-out), then `setpriv`-drops to
  `worker` keeping **only** `CAP_SETUID`/`CAP_SETGID` as **ambient** caps. The
  worker retains those two for the run lifetime so it can spawn `runner`-uid
  children per run; `no-new-privileges: true` stays on (it blocks a privilege
  *gain* on execve, not a root→lower drop).
- **Per-spawn drop to the cap-less runner.**
  [`agent/src/runner-uid.ts`](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/agent/src/runner-uid.ts)
  wraps every untrusted spawn (SDK CLI, checks + `npm ci`, provision hooks, the
  runner-clone seed clone/checkout) in `setpriv --reuid runner --regid runner
  --init-groups --bounding-set -all --inh-caps -all --ambient-caps -all` (the
  `--bounding-set -all` is a documented no-op — the worker lacks CAP_SETPCAP to
  shrink the child's bounding set — kept for intent). The inheritable+ambient
  cap clear is load-bearing: a plain reuid from a uid holding ambient CAP_SETUID
  would let the child setuid back, so without the clear the split would be
  defeated. The runner child ends `CapEff=CapPrm=CapAmb=0`, and — since the image
  ships no setuid/file-capability binary and `no-new-privileges` blocks any
  execve-time raise — the inert `CapBnd` residue cannot be climbed.
- **Cross-uid signalling.** The worker (a different uid, holding no `CAP_KILL`)
  cannot `kill(2)` a runner process, so the pre-push reap (audit B1) and the
  watchdog signal via a `setpriv`-to-runner `kill` of the process group.

### What each close now rests on

- **Join token.** Delivered as a file, not an env var, so it is absent from every
  process `environ` (the PRD #46 M6 close). The entrypoint forces
  `/run/secrets/worker_token` to `0400 worker:worker`, so the `runner` uid
  **cannot read it** — the file is **persisted, not unlinked** (a read-only
  secret mount cannot be unlinked anyway; the uid boundary, not removal, is the
  close). See [`agent/src/config.ts`](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/agent/src/config.ts)
  `resolveWorkerToken` and the `secrets:` block in `docker-compose.yml`.
- **Bot PAT (push-window race).** The PAT enters a git **child's** env only during
  the worker's push/MR. That git child runs as `worker`, so its
  `/proc/<pid>/environ` is `0400 worker`-owned and a `runner` survivor **cannot
  read it** — the structural close for the vector B1's group-kill could only
  race. B1 (reaping the agent tree before the push, via the `setpriv`-to-runner
  kill) stays as a second layer.
- **Shared-git write→worker-execute.** `git` has config keys whose value is run as
  a command (`diff.external`, `core.fsmonitor`, `core.pager`, `core.sshCommand`,
  `credential.helper`, `core.askpass`, …). The worker pins every fixed-name one
  via inline `GIT_CONFIG_*` (highest precedence), sets `GIT_CONFIG_NOSYSTEM`, and
  defaults `GIT_CONFIG_GLOBAL` to `/dev/null` (PRD #51 M0,
  [`agent/src/git.ts`](https://gitlab.example.com/vtmocanu/uzi/-/blob/main/agent/src/git.ts)
  `gitEnv`). The arbitrary-name class (`filter.<name>.*`, `uploadpack.packObjectsHook`,
  a `commondir`/`gitdir` rewrite) is closed by **config-source ownership**: under
  the `(b)` topology the worker is **bare-only** — it never runs `git` with a
  runner-owned clone as its git dir. The runner gets its **own** clone (working
  tree + object store): a **runner-run** local `git clone --shared` from the worker
  bare seeds it (run as the runner uid via `runGitAsRunner`, which is what makes the
  clone runner-owned), and the agent checks out + commits there. The worker then
  `fetch`es the agent branch **back** over `file://`+pack (never the local-copy path,
  so it never traverses the clone's alternates or a planted hook), and pushes from
  its own bare with the PAT.
- **PATH + scratch isolation.** `/nix` is runner-owned and group-runner-writable,
  so the worker's credentialed-exec PATH is stripped to root-owned image dirs only
  (no `/nix`); the full `/nix`-bearing PATH reaches the runner via `UZI_RUNNER_PATH`.
  Worker and runner each get a private `0700` `TMPDIR` (`/tmp/uzi-worker`,
  `/tmp/uzi-runner`), so neither reads the other's git/npm scratch.

### The honest posture (stated, not hidden)

The containment is asymmetric by design: the **worker** (the PAT/token custodian)
also holds `CAP_SETUID`, so a *compromised worker* could become any uid, including
0. That is accepted — the worker is the trusted side, and the containment we buy is
on the **runner** side (the surface that actually runs agent-influenced code). The
split shrinks the blast radius of an agent/tool compromise, not of a worker
compromise.

## The `#58` single-uid (non-root) start

The **same image** also runs in a restricted-PodSecurity k8s namespace
(`runAsUser: 10001`, no addable capabilities — PRD #58 v1). There the entrypoint
detects a non-root start and runs **single-uid**: no root window, no `runner` uid,
no `setpriv` drop (an unconditional `--reuid` would `EPERM` → CrashLoop). The #51
uid-split containment applies only on the **root-started** (compose / A1) path;
`UZI_UID_SPLIT=1` is exported only there, and its absence makes every
`runner-uid.ts` primitive a passthrough. Single-uid is PRD #58's own accepted
posture; the cross-container split for that world is the k8s form below.

`UZI_RUNNER_PATH` is the one split-shaped var the non-root branch **does** export
(issue #120), and it carries no containment meaning there — it exists so the PATH
handed to runner children is the image's own in **both** modes. The container's CMD
is `npm run start`, and npm's run-script prepends `/app/node_modules/.bin` to the
worker's PATH; leaving `UZI_RUNNER_PATH` unset made `runnerPath()` fall back to that
mutated PATH, where the real `agent-browser` npm CLI shadowed the `/usr/local/bin`
shim that injects `--no-sandbox` (PRD #87) and Chromium aborted on the setuid
sandbox this very hardening makes impossible. Exporting it does not activate the
split (`UZI_UID_SPLIT` stays unset) and widens nothing: single-uid means worker ==
runner, so that PATH was already the worker's.

## Local ↔ k8s mapping (align at the abstraction, not the mechanism)

The durable abstraction is **"the untrusted execution surface runs under a
distinct uid from the credential custodian, and cannot read its secrets."** How
that is realized differs by platform, and the two do **not** map 1:1:

| | Local (compose, A1) — built | k8s remote-worker — design (deferred) |
| --- | --- | --- |
| Boundary | two uids in **one** container | two **containers** in one pod |
| Drop | root entrypoint + in-process `setpriv` per spawn | per-container `runAsUser` (10001 / 10002) set by the kubelet |
| Caps | worker keeps ambient `CAP_SETUID` to spawn the runner | **no `CAP_SETUID`**, no in-process uid spawn (the kubelet assigns uids) |
| `/proc` | same pid namespace; environ unreadable via `0400`-owner | `shareProcessNamespace: false` → the agent container cannot see worker pids at all |
| Worker↔runner | in-process spawn + shared `/data` volume | an **IPC** boundary + a shared `emptyDir` worktree (the `(C)`/sidecar model) |

Do **not** claim mechanism A1 maps onto k8s `runAsUser`: A1 needs `CAP_SETUID` and
an in-process drop, which the k8s form deliberately does not use. The k8s form is
the PRD's **(C)** two-container model, which additionally requires making the
worker↔runner boundary an IPC one (today the worker spawns and controls the SDK
in-process). **Actual k8s manifests are deferred to the remote-worker PRD** — this
page aligns the design only. `shareProcessNamespace: false` (or `hidepid=` /
gVisor) are complements available **once the uids differ across containers**; they
buy nothing for a same-uid pair.

## Regression guards

The uid boundary is asserted end-to-end so it cannot silently regress: unit tests
(`agent/test/git-hardening.test.ts`, `runner-uid.test.ts`, `sdk-env` /
`self-improve` / `provision` env-scrub, `templates-guardrails`) and the live
image-level block in `e2e/run-e2e.sh` (a `setpriv`-to-runner read of the worker's
`/proc/environ` and token is denied; the runner child's caps are all zero and its
`/proc/self/fd` carries no leaked worker fds; the worker/runner TMPDIRs are
distinct `0700` trees; a runner-planted `packObjectsHook` is ignored on the image's
git). See the PRD #51 M6 milestone.
