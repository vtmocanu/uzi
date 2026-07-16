# PRD #51: Worker/agent uid-split — contain the same-uid token-file read

**GitLab Issue**: [#51](https://gitlab.example.com/vtmocanu/uzi/-/issues/51)
**Status**: Draft (created 2026-07-13 as the containment follow-up to PRD #46; revised after design review + security audit + fact-check the same day; re-reviewed 2026-07-16 by parallel design / security-audit / fact-check agents — the mechanism-feasibility, bare-cache-ownership, and hardened-git findings below are folded in and **must be resolved at the M1 gate**)
**Priority**: Medium
**Depends on**: PRD #46 (surfaced/documented this residual), PRD #18 (worker image / devbox provisioning), `docs/proc-hardening.md` (the remote-worker/uid-split design this implements).

## Problem

The worker process holds the run's credentials: the join token (delivered as a
file at `/run/secrets/worker_token` — a read-only compose secret whose post-read
`unlinkSync` fails and is swallowed, so the file **persists** and is same-uid
readable; `agent/src/config.ts:167-181`), plus the decrypted forge PAT and the
Anthropic OAuth token in memory. The worker then spawns code-execution surfaces
**as the same OS uid** (`uzi`) — no spawn site sets uid/gid:

- the agent SDK subprocess (`agent/src/sdk-spawn.ts`),
- the PRD #46 self-improvement **check runner** (`go test`/`npm test`/`tsc` over code the agent just wrote), and
- provision/nix build hooks (`agent/src/provision.ts:89`).

Because these run as the worker uid, agent-authored or worker-executed code can
read `/run/secrets/worker_token` **directly off the filesystem** → call
`POST /api/worker/runs/claim` → receive the forge PAT + Anthropic token. That
defeats the custody invariant ("the worker holds the PAT; the agent never does").

PRD #46 closed every **env-** and **hook-** level path (M9/M10): the check runner
and all git subprocesses run on scrubbed replacement envs (no join token / API
URL — `buildCheckEnv`, `gitEnv`); `npm ci --ignore-scripts` removed the
lifecycle-script surface; git hooks are neutralized via a root-owned baked hooks
dir. What remains is the **same-uid filesystem read**: the `guardrails.ts`
PreToolUse deny-hook constrains only the **agent's own tools** (Bash + file
reads); it has **no reach over a subprocess the worker spawns**
(`git.ts:313-318`, `self-improve.ts:132-133`). The named structural close is a
**uid-split** (`docs/proc-hardening.md:92,118,123`; `ARCHITECTURE.md:463`, also
`:662`).

Blast radius today: intra-user only (the agent obtains its **own** user's creds
— `ARCHITECTURE.md:427-428`; `main` stays a protected branch and the bot is
Developer-role, never Maintainer/Owner, so it cannot merge — `ARCHITECTURE.md:470-472`).
But it breaks guardrail layer 2 for that user, and
PRD #46's autonomous self-improvement job is exactly the feature that runs
model-authored code through the check runner.

## Solution Overview

Run the credential-holding **worker** and the untrusted **code-execution**
surfaces (agent SDK subprocess + self-improve checks + provision hooks) under
**distinct OS uids**, so a compromised or prompt-injected agent cannot obtain the
worker's credentials — neither by reading the token file, nor by the equivalent
channels below. Delivered in two phases aligned so they don't diverge:

1. **Local (docker-compose)** — the demo/dev path today. The hard case, because
   the agent container is **already hardened**: `cap_drop: ALL`,
   `security_opt: no-new-privileges:true`, Docker's default seccomp, non-root
   `USER uzi` from start, and `/nix` + `/data` both `chown`'d to `uzi`. The
   mechanism must be chosen **against that posture**, not in the abstract.
2. **Kubernetes remote-worker** — the `docs/proc-hardening.md` design (separate
   containers in one pod, distinct `runAsUser` 10001/10002, token projected only
   into the worker). The local design must map onto this.

**Bonus close (claim it), with its precondition stated:** a genuine uid-split
also structurally closes proc-hardening's *other* documented residual — the PAT
setsid/`/proc`-environ race during the push window (`docs/proc-hardening.md:26-31,104-108`).
Precision: the PAT lives in the environ of the credentialed **git child** the
worker spawns during the push (not the worker's *own* environ); both are
worker-uid-owned `0400`, and after the worker's setuid drop the worker process is
non-dumpable, so its `/proc/<pid>/*` are in fact **root-owned `0400`** (stronger
than owner-readable). A runner-uid survivor of a `setsid`-escaped agent therefore
cannot read either. **This bonus close holds only because Decision 1 keeps all
credentialed git on the worker uid** — it breaks the instant any PAT-bearing git
moves to the execution uid. This PRD closes the file read **and** that environ
race, conditioned on Decision 1.

## Design Decisions (DRAFT — mechanism firmed in M1)

1. **Boundary = worker-vs-execution (validated by audit).** The **worker** stays
   the sole custodian of the **worker** credentials and runs everything they
   authenticate: the claim, the worker-side HTTP that uses the join token (judge
   trace-fetch, review-POST), and **credentialed git — clone/fetch and the
   PAT-bearing `push`/MR**. The **execution uid** runs only untrusted code and
   holds **none of the worker credentials** (no join token, no forge PAT): the
   SDK agent subprocess, the self-improve checks, the provision hooks, and
   **local, non-credentialed git only** (the agent's own `edit`/`commit`, which
   carry no PAT). This resolves the earlier Decision 1/4 ambiguity: the push
   **never** moves to the execution uid.
   - *Precision (audit L4):* the execution uid is **not** credential-free in the
     absolute sense — the SDK subprocess necessarily carries the run's own user's
     **Anthropic OAuth token** (`buildSdkEnv`, `sdk-env.ts:59-66`), and a same-uid
     check/provision child can read it from the SDK's `/proc`. Only the *worker*
     credentials are withheld. Tests must not assume a credential-free runner env.
   - *Precision (review nit):* the agent's own `commit` runs through the SDK Bash
     env (`sdk-env.ts` — OAuth token / HOME / PATH only), **not** `gitEnv`, so it
     is **not** `core.hooksPath`-pinned. That is harmless (an agent running its
     own hook as the runner uid is not an escalation; the worker pins
     `core.hooksPath` on *its* own invocations), but do not claim the agent's
     commit is hooks-pinned.

2. **Local mechanism — grounded in the current hardened posture; recommend (A).**
   *Constraints from the current posture:* `cap_drop: ALL` strips
   `CAP_SETUID/SETGID`, `no-new-privileges:true` blocks suid privilege *gain*
   (not a root→lower drop), default seccomp, non-root entry, and `/nix`+`/data`
   are `uzi`-owned. Against that:
   - **(A) root-entry drop (gosu/setpriv) — RECOMMENDED for local, but the
     capability lifetime must be corrected (BLOCKING — audit H1 / review B1).**
     A root entrypoint `setuid(2)`-drops itself to uid `worker` and thereafter
     spawns agent/checks as a distinct uid `runner`. Closes **both** vectors via
     kernel perm checks: the file read (`0400` owned by `worker`) **and** the
     `/proc/<worker>/environ` read (different uid, and root-owned once
     non-dumpable). `no-new-privileges` does **not** block a root→lower drop.
     **The flaw in the original write-up:** the runner-uid children are spawned
     **dynamically, per-run, in-process** by the already-dropped worker — the SDK
     CLI per turn (`sdk-executor.ts:471` `spawnClaudeCodeProcess`), the check
     runner per check (`self-improve.ts:242`), provision hooks
     (`provision.ts:89,170`) — **not** once at container start by the entrypoint.
     To fork a child under a *different* uid the forking process needs
     `CAP_SETUID/SETGID` **at spawn time**, i.e. for its whole lifetime; and
     `no-new-privileges` neutralises any suid `gosu`/`su-exec` helper, so a
     one-shot root→worker drop cannot serve the recurring worker→runner spawns.
     The original Decision 7 ("caps added only to perform the drop and **dropped
     immediately after**", "no residual caps") therefore makes (A) **not
     implementable**. The three internally-consistent forms are:
     - **(A1) worker retains `CAP_SETUID/SETGID` for the run lifetime** (as
       *ambient* caps across the drop — needs a `setpriv --ambient-caps` wrapper
       + `CAP_SETPCAP`; fiddly because runc clears the permitted set on a
       setuid-to-non-root transition). Posture to disclose plainly: **the PAT
       custodian also holds `CAP_SETUID`, so a post-compromise worker can become
       any uid including 0.** Retaining the caps on the *trusted* custodian does
       not weaken the runner-side containment, but "no residual caps" is false and
       must be rewritten. This is the recommended form.
     - **(A2) worker stays root** — worse posture, breaks the k8s `runAsNonRoot`
       analogue; rejected unless A1's ambient-caps path proves unworkable.
     - **(A3) persistent root/`CAP_SETUID` spawn-broker** + IPC — that is
       option (C) in disguise (an extra privileged process for the run's life),
       and forfeits the in-process control the local path is trying to keep.
     `cap_add: [SETUID, SETGID]` (partially reversing `cap_drop: ALL`) is required
     either way; **plus `CAP_CHOWN`/`DAC_OVERRIDE`** if a startup volume-chown is
     needed (Decision 4). **M1 must pick A1/A2/A3 explicitly and rewrite Decision
     7 accordingly — do not assert the lifetime as settled.**
   - **(B) rootless bwrap/user-ns — gated behind a PoC, likely dropped.**
     Unprivileged user-namespaces under `cap_drop: ALL` + `no-new-privileges` +
     seccomp are frequently unavailable and notably fragile on Docker-Desktop /
     macOS (the dev host is darwin); making it work often needs
     `seccomp=unconfined`/`CAP_SYS_ADMIN` (net-negative) or a suid-root bwrap
     (conflicts with `no-new-privileges`). **Not a candidate unless a PoC proves
     unprivileged userns works on the target hosts without loosening seccomp/caps.**
   - **(C) sidecar / second container — the k8s endpoint; heaviest local lift.**
     Two containers with distinct `user:` need **no** cap/seccomp change and are
     the literal k8s design (Decision 8). Cost: the worker↔SDK boundary becomes
     IPC (today the worker controls the SDK in-process — abort, group-kill,
     session-dir, pid capture — `sdk-executor.ts`), plus shared-volume +
     worker-side-push handoff.
   *Recommendation:* **(A1)** for the local image (kernel-enforced, no fragile
   userns dependency, direct k8s analogue); **(C)** as the eventual k8s form;
   **(B)** only if a host PoC clears it.
   *Divergence caveat (review N2):* (A) and (C) map only at the **uid-numbering**
   level, not the control model. (A) keeps the worker↔SDK boundary **in-process**;
   (C) **requires** rebuilding it as IPC (`spawnClaudeCodeProcess` pid capture,
   `killAgentTree`, per-run session dir, in-process abort all become RPC). So the
   (A) spawn wiring in M4 is partly **throwaway** for the eventual k8s (C)
   redesign — the drift the PRD says it wants to avoid. M1 must weigh whether to
   pay for (C) once (even in compose) instead of building (A) as a bridge.

3. **Shared git storage is a cross-uid write→worker-side-execute channel — the
   design MUST close it (audit headline), and it is a LIVE vulnerability TODAY
   independent of the split (review B3).** Moving the token file out of reach is
   *not sufficient*: the bare clone cache (`/data/repos`) and worktrees
   (`/data/worktrees`) share the `agentdata` volume. If a process can write the
   bare repo's on-disk git **config** (a direct `printf >`, which bypasses both
   the `git config` guardrail screen and the guardrail system entirely), it plants
   a code-exec key — `core.fsmonitor`, `core.sshCommand`, `diff.external`,
   `diff.*.textconv`, `core.pager`, `filter.*.process` — that fires in a later
   worker-side git process. PRD #46's `core.hooksPath` pin neutralises hooks only.

   **Two corrections to the earlier framing:**

   - **This is exploitable RIGHT NOW, with no uid split (review B3, audit H2).**
     Today the agent runs as `uzi` and the bare cache under `/data/repos` is
     `uzi`-owned, so agent-authored code can already `printf > <bare>/config` a
     `core.fsmonitor`/`diff.external`/`filter.<x>.process`. And the git that
     fires it need not be credentialed: `changedFiles` runs
     `runGit(worktreePath, ["diff", ...])` **with no PAT** as the worker uid over
     the runner-writable worktree (`git.ts:162-166`); `worktree add`, `diff`,
     `defaultBranchRef` all run worker-side. `gitEnv` pins only `safe.directory`
     + `core.hooksPath` (`git.ts:336-348`) — **not** the code-exec keys above.
     This is the exact sibling of the M10 `hooksPath` vector and should be **pulled
     forward as an immediate, standalone hardening (a fast-follow like M10), not
     gated behind the whole uid split** — see the new milestone M0.
   - **Fire point is `worktree add` / `diff`, not "push" (review B3).** Push over
     HTTPS from a bare repo does not checkout or diff, so smudge-`filter` /
     `diff.external` / `fsmonitor` mostly do **not** fire during the push. They
     fire in the worker's **`worktree add`** (checkout → smudge filters) and
     **`changedFiles` → `git diff`** (diff.external / textconv) — `git.ts:135,139,165`.
     Correct the PRD's "fires inside the worker's credentialed push" wording.

   Close it by **both** controls, which are load-bearing for *different* keys:

   - **(a) Config-source ownership (closes the un-pinnable keys):** **no git
     config source that any worker-side git reads is runner-writable.** Concretely
     the bare repo's **`<bare>/config` and `<bare>/hooks`** are worker-uid-owned
     and non-runner-writable, `GIT_CONFIG_NOSYSTEM` is set, and the worker's
     `GIT_CONFIG_GLOBAL` points at `/dev/null` or a worker-owned file (in prod the
     worker git otherwise reads `$HOME/.gitconfig` — `git.ts:327` only sets
     `GIT_CONFIG_GLOBAL` under the e2e overlay). This is the **only** control that
     covers `filter.<name>.process`, whose driver names are arbitrary and so
     **cannot** be blanket-pinned via `GIT_CONFIG_*` (correcting 3b's "pin
     `filter.*`"). Consider `extensions.worktreeConfig` too.
   - **(b) Hardened worker git (defense-in-depth for the pinnable keys):** pin
     `core.fsmonitor` / `core.sshCommand` / `diff.external` / `diff.*.textconv` /
     `core.pager` **unconditionally** (not gated on the PAT — `changedFiles` and
     every worktree op must be covered) via inline `GIT_CONFIG_*` pairs, the way
     `core.hooksPath` is already pinned. This is a lagging denylist (git keeps
     adding code-exec keys), so it is the *backup*, not the primary guarantee.

   **Crucial reframing (review B3):** because a git worktree **must** share the
   bare `objects/` store and its per-worktree admin dir (see Decision 4), the
   bare cache cannot be wholesale non-writable by the execution uid, so **ownership
   alone cannot wall off the bare — control (a) on the config file, plus (b),
   carry the real weight**, not a blanket "bare not writable." This inverts the
   original 3a-primary / 3b-backup ordering.

4. **Shared-volume ownership model (`/nix` + `/data`) — the real engineering
   work, with the git-worktree constraint made explicit.** Today both are
   `uzi`-owned. Under the split the execution uid must **write** more than the PRD
   originally listed.

   **The git-worktree constraint (BLOCKING — review B2 / audit H3).** A
   `git worktree` **shares** the bare repo's object store `<bare>/objects` and
   keeps each worktree's mutable admin state (`HEAD`, `index`, `logs/`,
   `ORIG_HEAD`) under `<bare>/worktrees/<name>/`. Decision 1 keeps the agent's own
   `git add`/`git commit` on the execution uid (its core job — `ARCHITECTURE.md:457`),
   and a commit **writes objects into `<bare>/objects/` and updates
   `<bare>/worktrees/<name>/`** — both **inside the bare dir**. So a blanket "bare
   cache worker-owned and NOT writable by the execution uid" (the original
   Decision 3a) is **not implementable — it EACCESes the agent's commit.** The
   correct model is per-path:
   - **worker-owned, non-runner-writable:** `<bare>/config`, `<bare>/hooks`
     (the code-exec surface — Decision 3a),
   - **execution-uid-writable (shared gid):** `<bare>/objects/` and the run's
     `<bare>/worktrees/<name>/` admin dir, plus the worktree checkout itself.
   (Alternative worth weighing: give the runner a **separate clone / object
   store** and have the worker `fetch` from it, avoiding shared-write into the
   bare entirely.)

   **Full runner-writable path enumeration (audit M1).** Beyond `/nix`
   (devbox/nix install via provision hooks) and the `/data` worktree checkout, the
   runner-uid SDK also needs write to the per-run SDK HOME `agent-home/<runId>`,
   the provisioning dirs `provisionRoot` (`…/provision`) and the shared
   `provisionHomeDir` / nix-profile HOME (`sdk-executor.ts:153-154`, `run()` mkdir
   `:195-201`) — all created by the worker today. Enumerate every runner-writable
   path (worktrees, `agent-home/<runId>`, `provision/`, shared nix/devbox HOME)
   vs worker-only (`/data/repos` bare `config`/`hooks`, worker HOME/`.gitconfig`).

   **Persisted named-volume migration (BLOCKING — review B4).** `agentnix` /
   `agentdata` are named volumes seeded from the image on first use and then
   **persist with their original ownership** (`docker-compose.yml:239-245,252-257`).
   Re-`chown`ing `/nix` + `/data` in the Dockerfile only affects the **image
   layer**, not an already-populated volume. When the execution uid changes from
   today's `uzi`, an existing install's volumes stay owned by the old uid →
   provisioning writes to `/nix` and worktree writes fail with EACCES until a
   **startup chown of the mounted volumes** runs (needs the root entry +
   `CAP_CHOWN`/`DAC_OVERRIDE`). Add a startup-chown step (or a documented
   `down -v`) and fold `CAP_CHOWN` into the Decision 7 capability set.

   **New intra-uid surface to weigh:** if `/data` worktrees / shared objects
   become group-writable, a compromised executor can tamper with content the
   worker later reads (already partly why Decision 3 hardens the worker git) —
   lower severity than the token read, but note it and keep `<bare>/config` +
   `<bare>/hooks` off the shared-write surface (Decision 3a).

5. **Token delivery across the split — no channel leaks.** The file-vs-env
   `/proc` tension in `docs/proc-hardening.md` resolves under the split: a
   `worker`-owned 0400 token mount closes the file read for the execution uid
   **without** re-opening the environ read (different uid). Keep 0400
   file-delivery (matches k8s) as belt-and-suspenders even though env-delivery
   would also be safe once the environ is cross-uid-unreadable.
   - *Compose caveat (audit L2):* the token is env-sourced
     (`docker-compose.yml:264-265`, `environment: UZI_WORKER_TOKEN`); compose's
     secret `uid`/`gid`/`mode` are reliable for `file:` secrets, not always for
     env-sourced ones. Since (A) starts as root, **enforce `0400`/`worker` on the
     token file in the entrypoint** rather than trusting compose to.

   **Near-zero-cost partial to present first (review N1).** The headline
   *file-read* vector exists today only because the shipping compose mounts the
   token **read-only**, so the worker's post-read `unlinkSync` fails and is
   swallowed (`config.ts:173-178`; `docker-compose.yml:264-265`). The e2e overlay
   already delivers the token via a **writable** file so the unlink *succeeds* and
   the on-disk vector closes (`docs/proc-hardening.md:95-99`). Switching shipping
   delivery to a **writable tmpfs file** closes vector-1 **today, in one line,
   with no uid split** — an immediate risk reduction for operators. The uid split
   is then justified by the vectors unlink does **not** close: the PAT `/proc`
   environ race and the shared-git channel. Present this partial explicitly rather
   than implying the split is required for the token-file read.

5-bis. **Namespace / IPC / tmp hygiene (audit M2, M3, L1).** The uid split does
   **not** isolate the network namespace or `/tmp`; these must be closed
   separately, and the caveats apply to the k8s form too (pod containers share the
   net ns even with `shareProcessNamespace:false`):
   - **No cross-uid-reachable control channel.** If the mechanism ever needs a
     worker↔runner IPC channel (e.g. the A3 spawn-broker), it uses an
     fd-inherited `socketpair` or a `0600` pathname socket — **never** an abstract
     unix socket or a loopback port (the runner shares the net ns and can connect).
     Assert **no** `node --inspect`/debug port on the worker (= RCE in the
     credential holder).
   - **Distinct `TMPDIR` per uid.** `git`/`npm`/`node` write temp files; a shared
     sticky `/tmp` invites symlink races and exposes any worker temp write. Give
     worker and runner distinct `TMPDIR` on their own `0700` trees; audit
     worker-side temp writes.
   - **fd/CLOEXEC hygiene at the new spawn helper.** The worker holds the PAT /
     Anthropic token in memory and live API sockets; the new spawn path is exactly
     where an fd could leak to the runner child. Node sets CLOEXEC by default —
     assert it (M6 test: the runner child's `/proc/self/fd` is only `{0,1,2}` +
     known).

6. **Worker PATH hygiene (invariant).** The token-custodian worker must never
   resolve any executable (git/sh/tool) from an execution-uid-writable path
   (`/nix`, the worktree), or the execution uid plants a trojan the worker runs
   **with the PAT**. PRD #46's `gitEnv` already uses the base-image PATH (not
   `toolEnv`) — carry that forward and state it as an invariant for every
   worker-side exec.

7. **Capability hygiene for (A) — corrected per H1/B1.** Under the recommended
   **(A1)**, the **worker retains `CAP_SETUID/SETGID` for the run lifetime** (as
   ambient caps) because it spawns runner-uid children per-run; the **runner
   children hold no caps** and a distinct uid. So the success criterion is
   **"the worker holds `CAP_SETUID/SETGID` (and `CAP_CHOWN` if a startup
   volume-chown is used); the runner processes hold none"** — **not** "no residual
   caps," which is impossible for (A). `no-new-privileges` stays on post-drop.
   Disclose the posture: the PAT custodian holding `CAP_SETUID` can, post-compromise,
   become any uid including 0 — accepted because it is the *trusted* side and the
   containment we buy is on the *runner* side.
   - **Root startup-window hygiene (audit M5).** (A) reintroduces a root
     entrypoint. The root window must exec only image-baked, root-owned static
     binaries, with **no runner-writable dir on `PATH`** and no shell
     interpolation of env/args, and drop before anything runner-influenced runs
     (`/nix` is empty at entry — state it). The `gosu`/`setpriv` wrapper is not
     itself a suid-escalation surface.

8. **k8s alignment (docs-only here).** Map the local split onto
   `docs/proc-hardening.md`'s remote-worker design (worker pod holds the token
   via a projected secret; a separate execution container with distinct
   `runAsUser` 10001/10002, no token projection, and the bare-cache-ownership +
   hardened-git invariants preserved). **Alignment is at the distinct-uid
   abstraction, not the mechanism (audit L3):** the k8s design uses **separate
   containers** with per-container `runAsUser` and needs **no `CAP_SETUID` and no
   in-process uid spawn** — that is the PRD's own (C)/sidecar model, not (A). Do
   not claim (A) "maps 1:1 to `runAsUser`." Actual k8s manifests defer to the
   remote-worker deployment PRD; this PRD only aligns the design so they don't
   drift.

## Technical Design (PROVISIONAL until the M1 gate resolves B1/B2/B4)

> This section sketches the (A1) shape but is **provisional**: the capability
> lifetime (B1), the per-path bare-cache ownership (B2), and the persisted-volume
> migration (B4) are open at the M1 design gate. Do not treat `/data/repos`
> worker-owned as settled — the per-path split (Decision 4) supersedes the coarse
> "bare cache worker-owned" phrasing below.

- **agent/ image (`base` + `jvm`)**: second uid; root entrypoint + `gosu`/`setpriv`
  drop wrapper (mechanism A1, worker **retains** `CAP_SETUID/SETGID` ambient);
  `/nix`, the worktree checkout, `<bare>/objects` + per-worktree admin dir, and
  the runner data paths (Decision 4) writable by the execution uid, while
  `<bare>/config` + `<bare>/hooks` stay worker-owned.
- **docker-compose.yml**: enforce token `0400`/`worker` in the entrypoint (not via
  the env-sourced secret); `cap_add: [SETUID, SETGID]` (+ `CHOWN` if startup
  volume-chown); root entry; startup chown of persisted `agentnix`/`agentdata`.
- **agent/src**: a spawn helper launching the SDK subprocess + check runner +
  provision hooks under the execution uid (extends `sdk-spawn.ts` /
  `sdk-executor.ts` / `self-improve.ts` / `provision.ts`); the worker keeps the
  token in memory, does the credentialed git/HTTP, and never passes worker creds
  across the boundary; harden **every worker-side git** (credentialed and not —
  `changedFiles`, `worktree add`) per Decision 3.
- **docs**: `docs/proc-hardening.md` becomes the implemented design (not a
  sketch); `ARCHITECTURE.md` layer-2 update; close the PRD #46 residual notes
  (`self-improve.ts` header, `docs/self-improvement.md`).

## Milestones (DRAFT — resequenced 2026-07-16 per review N3)

- [ ] **M0 — Standalone shared-git hardening (fast-follow, NO uid split; review
      B3).** Land Decision 3 independently of the split, because the shared-git
      write→worker-execute channel is exploitable **today**: pin the code-exec git
      keys (`fsmonitor`/`sshCommand`/`diff.external`/`diff.*.textconv`/`core.pager`)
      **unconditionally** in every worker-side git (not PAT-gated — covers
      `changedFiles`/`worktree add`), set `GIT_CONFIG_NOSYSTEM` + a `/dev/null`-or-
      worker-owned `GIT_CONFIG_GLOBAL`, and make `<bare>/config`+`<bare>/hooks`
      non-writable by the agent uid. Gate: an agent-uid `printf > <bare>/config`
      of a `diff.external`/`fsmonitor`/`filter` **cannot** achieve code-exec in the
      worker's later `diff`/`worktree add` (PoC). Ships as its own MR, like PRD #46
      M10.
- [ ] **M1 — Threat model + mechanism decision (design gate).** Confirm the
      same-uid read on the running image (PoC: uid-`uzi` reads the token). **Pick
      A1/A2/A3 explicitly** (Decision 2) — resolve the capability-lifetime flaw
      (B1: the worker must **retain** `CAP_SETUID/SETGID` to spawn runner children,
      so "drop immediately / no residual caps" is out). Settle the **per-path**
      bare-cache ownership (B2) and the **persisted-volume migration** (B4). Run a
      bwrap-userns PoC iff (B) stays a candidate and **drop (B)** if it needs
      seccomp/cap loosening. Weigh building (A) as throwaway vs going straight to
      (C) (N2). Settle the k8s mapping. Gate: a written, reviewed design incl.
      Decisions 3+4 with B1/B2/B4 resolved.
- [ ] **M2 — Image + uid boundary + token perms.** Second uid; the (A1) drop
      wrapper in `base` + `jvm`; token forced `0400`/`worker` **in the entrypoint**
      (env-sourced secret mode is unreliable — L2); startup chown of persisted
      volumes (B4). Note: this gate cannot be *independently* PoC'd — a real
      execution-uid process only exists after M4 (N3), so M2's read-denied check
      rides the M4 spawn path (or a manual `su` PoC).
- [ ] **M3 — Shared-volume ownership model.** The full `/nix` + `/data`
      runner-writable path enumeration (Decision 4: worktrees, `agent-home/<runId>`,
      `provision/`, shared nix HOME, `<bare>/objects` + per-worktree admin) vs
      worker-only (`<bare>/config`+`hooks`, worker HOME/`.gitconfig`); distinct
      `TMPDIR` per uid (5-bis). (Decision 3's git hardening already landed in M0.)
      Gate: agent commit works (writes `<bare>/objects` + worktree admin); worker
      still owns `<bare>/config`.
- [ ] **M4 — Spawn surfaces under the execution uid.** SDK agent, check runner,
      provision hooks launch under the execution uid; the worker retains
      `CAP_SETUID` and does the credentialed git/HTTP; PATH hygiene (Decision 6),
      capability hygiene (Decision 7), root-startup-window hygiene (M5-audit), and
      no cross-uid IPC channel (5-bis) verified.
- [ ] **M5 — Preserve PRD #46/#18 behavior + e2e retooling.** devbox/nix
      provisioning (`toolEnv`), real check evidence (`go test`/`npm test`/`tsc`),
      worktree git, and the agent's own sandbox (`settingSources:[]` + deny-hook)
      all work across the boundary. **Retool the e2e (N4):** under the split the
      token is worker-owned `0400` and **not** unlinked, so `run-e2e.sh`'s
      writable-mount delivery and its "token unlinked" assertion must change to
      uid-boundary reads. Gate: `run-e2e.sh` (judge + self-improve + existing) green.
- [ ] **M6 — Tests.** uid-boundary tests: execution uid can't read the token file,
      can't read the worker's `/proc` environ, and can't code-exec via a shared
      git-config write; **plus (L1/L5):** the runner child's `/proc/self/fd` is only
      `{0,1,2}`+known (fd leak); a runner survivor cannot read the worker's **push
      git-child** `/proc/environ` during the window (the actual PAT-race close); and
      the runner child's **own** environ is still scrubbed (no join token / PAT /
      API URL) under the new spawn path. Worker can still push. No regression.
- [ ] **M7 — Docs (incl. k8s alignment, docs-only).** `docs/proc-hardening.md`
      becomes the implemented design + the local↔k8s mapping (align at the
      distinct-uid abstraction, not the mechanism — L3); `ARCHITECTURE.md` layer-2;
      close the PRD #46 residual notes. Actual k8s manifests deferred to the
      remote-worker PRD.

## Out of Scope

- The full k8s remote-worker deployment (only aligning the design here).
- Cross-user isolation (already holds — the residual is intra-user).
- Re-litigating PRD #46's env-scrub / hooks-dir closes (those stand; this PRD
  closes the remaining same-uid file read, the environ race, and the shared-git
  channel).

## Success Criteria

- A process running as the execution uid (agent code, a self-improve check, a
  provision hook) **cannot**: read `/run/secrets/worker_token`, read the worker's
  `/proc` environ (nor the worker's push git-child's environ during the window),
  **or** achieve code execution in any worker-side git (credentialed **or** the
  non-credentialed `changedFiles`/`worktree add`) via a shared-git-config write —
  all PoC-confirmed on the built image — while the worker authenticates and pushes
  normally.
- Capability posture is stated honestly: **the worker holds `CAP_SETUID/SETGID`
  (and `CAP_CHOWN` if a startup volume-chown is used) for the run lifetime; the
  runner processes hold no caps** (the earlier "no residual caps" criterion was
  impossible for mechanism (A) and is retired — B1).
- The agent's own `git add`/`git commit` still succeeds under the per-path
  bare-cache ownership (it writes `<bare>/objects` + its worktree admin dir — B2).
- The PRD #46 self-improvement job still produces real (or honest-skipped) test
  evidence and opens/extends its MR, with the check code under the isolated uid.
- `run-e2e.sh` (judge + self-improve + existing) stays green.
- The documented same-uid residual (`self-improve.ts` / `docs/self-improvement.md`
  / `docs/proc-hardening.md`) updates from "accepted residual, uid-split is the
  structural close" to "closed for the local path" (with the k8s form mapped in
  docs and deferred to the remote-worker PRD).
