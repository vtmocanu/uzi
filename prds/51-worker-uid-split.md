# PRD #51: Worker/agent uid-split — contain the same-uid token-file read

**GitLab Issue**: [#51](https://gitlab.example.com/vtmocanu/uzi/-/issues/51)
**Status**: Draft (created 2026-07-13 as the containment follow-up to PRD #46; revised after design review + security audit + fact-check the same day)
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
(`git.ts:313-316`, `self-improve.ts:135-136`). The named structural close is a
**uid-split** (`docs/proc-hardening.md:92,118,123`; `ARCHITECTURE.md:443-445`).

Blast radius today: intra-user only (the agent obtains its **own** user's creds;
`main` stays a protected branch and the bot is Developer-role, so it cannot merge
— `ARCHITECTURE.md:452-459`). But it breaks guardrail layer 2 for that user, and
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

**Bonus close (claim it):** a genuine uid-split also structurally closes
proc-hardening's *other* documented residual — the PAT setsid/`/proc`-environ
race during the push window (`docs/proc-hardening.md`) — because a different uid
cannot read the worker's `/proc/<pid>/environ` (0400, uid-owned). This PRD closes
the file read **and** that environ race.

## Design Decisions (DRAFT — mechanism firmed in M1)

1. **Boundary = worker-vs-execution (validated by audit).** The **worker** stays
   the sole credential custodian and runs everything token/PAT-authenticated:
   the claim, the worker-side HTTP that uses the join token (judge trace-fetch,
   review-POST), and **credentialed git — clone/fetch and the PAT-bearing
   `push`/MR**. The **execution uid** runs only untrusted code with **no token
   and no credential**: the SDK agent subprocess, the self-improve checks, the
   provision hooks, and **local, non-credentialed git only** (the agent's own
   `edit`/`commit`, which carry no token and run with `core.hooksPath` pinned).
   This resolves the earlier Decision 1/4 ambiguity: the push **never** moves to
   the execution uid.

2. **Local mechanism — grounded in the current hardened posture; recommend (A).**
   *Constraints from the current posture:* `cap_drop: ALL` strips
   `CAP_SETUID/SETGID`, `no-new-privileges:true` blocks suid privilege *gain*
   (not a root→lower drop), default seccomp, non-root entry, and `/nix`+`/data`
   are `uzi`-owned. Against that:
   - **(A) root-entry drop (gosu/setpriv) — RECOMMENDED for local.** A root
     entrypoint `setuid(2)`-drops the worker to uid `worker` and spawns
     agent/checks as a distinct uid `runner`. `no-new-privileges` does **not**
     block a root→lower drop; the token secret gets `uid`/`mode 0400` set on the
     compose `worker_token` secret (currently unset). Closes **both** vectors via
     kernel perm checks: the file read (0400 owned by `worker`) **and** the
     `/proc/<worker>/environ` read (different uid). Maps 1:1 to k8s `runAsUser`.
     **Explicit tradeoff to disclose:** requires the image to start as root and
     `cap_add: [SETUID, SETGID]` (partially reversing `cap_drop: ALL`), with the
     caps **dropped immediately after** the priv-drop and `no-new-privileges`
     preserved. The uid boundary is a stronger guarantee than non-root+cap-drop,
     but this posture change must be stated plainly, not buried.
   - **(B) rootless bwrap/user-ns — gated behind a PoC, likely dropped.**
     Unprivileged user-namespaces under `cap_drop: ALL` + `no-new-privileges` +
     seccomp are frequently unavailable and notably fragile on Docker-Desktop /
     macOS (the dev host is darwin); making it work often needs
     `seccomp=unconfined`/`CAP_SYS_ADMIN` (net-negative) or a suid-root bwrap
     (conflicts with `no-new-privileges`). **Not a candidate unless a PoC proves
     unprivileged userns works on the target hosts without loosening seccomp/caps.**
   - **(C) sidecar / second container — the k8s endpoint; heaviest local lift.**
     Two containers with distinct `user:` need **no** cap/seccomp change and are
     the literal k8s design (Decision 5). Cost: the worker↔SDK boundary becomes
     IPC (today the worker controls the SDK in-process — abort, group-kill,
     session-dir, pid capture), plus shared-volume + worker-side-push handoff.
   *Recommendation:* **(A)** for the local image (kernel-enforced, no fragile
   userns dependency, direct k8s analogue); **(C)** as the eventual k8s form
   (same drop mechanism aligns them); **(B)** only if a host PoC clears it.

3. **Shared git/nix/data storage is a cross-uid write→credentialed-execute
   channel — the design MUST close it (audit headline).** Moving the token file
   out of reach is *not sufficient*: the bare clone cache (`/data/repos`) and
   worktrees (`/data/worktrees`) share the `agentdata` volume, and the worker
   runs the PAT-bearing `push` with `cwd=barePath`. If the execution uid can
   write the bare repo's on-disk git **config** (a direct `printf >`, which
   bypasses both the `git config` guardrail screen and the guardrail system
   entirely), it plants a code-exec key — `core.fsmonitor`, `core.sshCommand`,
   `diff.external`, `filter.*.process` — that fires inside the worker's
   credentialed git process. PRD #46's `core.hooksPath` pin neutralizes hooks
   only. Close it by **both**:
   - **(a) Ownership:** the bare clone cache is **worker-uid-owned and NOT
     writable by the execution uid**; the execution uid gets only the worktree
     checkout it must edit. (Requires a `/data` ownership model — see Decision 4.)
   - **(b) Hardened worker git:** the worker's credentialed git runs with
     `GIT_CONFIG_NOSYSTEM`, ignores/overrides the on-disk repo config, and
     **pins every code-exec key** (`fsmonitor`/`sshCommand`/`diff.external`/
     `filter.*`) the way `core.hooksPath` is already pinned via inline
     `GIT_CONFIG_*` pairs.
   Belt-and-suspenders: do both, so neither an ownership miss nor a missed key
   alone re-opens the channel.

4. **Shared-volume ownership model (`/nix` + `/data`) — the real engineering
   work, named explicitly.** Today both are `uzi`-owned. Under the split the
   execution uid must **write** `/nix` (devbox/nix install via provision hooks)
   and the `/data` worktree (agent edits, node_modules, build artifacts), while
   the worker **owns** the bare cache + does the credentialed clone/push. Define
   a concrete model (shared gid with careful perms, or re-owning `/nix` +
   `/data/worktrees` to the execution uid while the worker keeps `/data/repos`),
   preserving: devbox/nix provisioning (the PRD #46 `toolEnv`), real check
   evidence, and the worker's clone/push. **New intra-uid surface to weigh:** if
   `/data` worktrees become group-writable, a compromised executor can tamper
   with content the worker later reads — lower severity than the token read, but
   note it and keep the bare cache off that shared-write surface (Decision 3a).

5. **Token delivery across the split — no channel leaks.** The file-vs-env
   `/proc` tension in `docs/proc-hardening.md` resolves under the split: a
   `worker`-owned 0400 token mount closes the file read for the execution uid
   **without** re-opening the environ read (different uid). Keep 0400
   file-delivery (matches k8s) as belt-and-suspenders even though env-delivery
   would also be safe once the environ is cross-uid-unreadable.

6. **Worker PATH hygiene (invariant).** The token-custodian worker must never
   resolve any executable (git/sh/tool) from an execution-uid-writable path
   (`/nix`, the worktree), or the execution uid plants a trojan the worker runs
   **with the PAT**. PRD #46's `gitEnv` already uses the base-image PATH (not
   `toolEnv`) — carry that forward and state it as an invariant for every
   worker-side exec.

7. **Capability hygiene for (A).** `CAP_SETUID/SETGID` are added only to perform
   the drop and **dropped immediately after**; `no-new-privileges` stays on
   post-drop; the `gosu`/`setpriv` wrapper is not itself a suid-escalation
   surface. Verify the running worker + execution processes hold no residual
   caps.

8. **k8s alignment (docs-only here).** Map the local (A) split onto
   `docs/proc-hardening.md`'s remote-worker design (worker pod holds the token
   via a projected secret; a separate execution container with distinct
   `runAsUser`, no token projection, and the bare-cache-ownership + hardened-git
   invariants preserved). Actual k8s manifests defer to the remote-worker
   deployment PRD; this PRD only aligns the design so they don't drift.

## Technical Design (firmed post-M1)

- **agent/ image (`base` + `jvm`)**: second uid; root entrypoint + `gosu`/`setpriv`
  drop wrapper (mechanism A); `/nix` + `/data/worktrees` owned by the execution
  uid, `/data/repos` (bare cache) owned by the worker uid.
- **docker-compose.yml**: `worker_token` secret gains `uid`/`mode: 0400` (worker);
  `cap_add: [SETUID, SETGID]` scoped + dropped after the drop; root entry.
- **agent/src**: a spawn helper launching the SDK subprocess + check runner +
  provision hooks under the execution uid (extends `sdk-spawn.ts` /
  `self-improve.ts` / `provision.ts`); the worker keeps the token in memory,
  does the credentialed git/HTTP, and never passes creds across the boundary;
  harden the worker git config per Decision 3b.
- **docs**: `docs/proc-hardening.md` becomes the implemented design (not a
  sketch); `ARCHITECTURE.md` layer-2 update; close the PRD #46 residual notes
  (`self-improve.ts` header, `docs/self-improvement.md`).

## Milestones (DRAFT)

- [ ] **M1 — Threat model + mechanism decision (design gate).** Confirm the
      same-uid read on the running image (PoC: uid-`uzi` reads the token). Pick
      the local mechanism **against the real posture** (`cap_drop`/`no-new-priv`/
      seccomp/root-entry) — recommend (A) with the disclosed cap/root tradeoff;
      run a bwrap-userns PoC iff (B) stays a candidate and **drop (B)** if it
      needs seccomp/cap loosening. Settle the k8s mapping. Gate: a written,
      reviewed design incl. Decisions 3 (shared-git close) + 4 (volume ownership).
- [ ] **M2 — Image + uid boundary + token perms.** Second uid; `worker_token`
      owned `worker` mode 0400; the (A) drop wrapper in `base` + `jvm`. Gate:
      the execution uid provably **cannot** read the token file **or** the
      worker's `/proc` environ (PoC → "Permission denied"); the worker still can.
- [ ] **M3 — Shared-volume ownership + hardened worker git.** The `/nix` +
      `/data` ownership model (Decision 4); the bare cache worker-owned +
      execution-non-writable (Decision 3a); the worker git config neutralizes
      `fsmonitor`/`sshCommand`/`diff.external`/`filter.*` + `GIT_CONFIG_NOSYSTEM`
      (Decision 3b). Gate: an execution-uid write to the bare git config cannot
      achieve code-exec in the worker's push (PoC).
- [ ] **M4 — Spawn surfaces under the execution uid.** SDK agent, check runner,
      provision hooks launch under the execution uid; the worker holds the token
      and does the credentialed git/HTTP; PATH hygiene (Decision 6) + capability
      hygiene (Decision 7) verified.
- [ ] **M5 — Preserve PRD #46/#18 behavior.** devbox/nix provisioning (`toolEnv`),
      real check evidence (`go test`/`npm test`/`tsc`), worktree git, and the
      agent's own sandbox (`settingSources:[]` + deny-hook) all work across the
      boundary. Gate: `run-e2e.sh` (judge + self-improve + existing) green.
- [ ] **M6 — Tests.** uid-boundary tests (execution uid can't read token/environ
      or code-exec via the shared git config; worker can push); no regression in
      existing suites.
- [ ] **M7 — Docs (incl. k8s alignment, docs-only).** `docs/proc-hardening.md`
      becomes the implemented design + the local↔k8s mapping; `ARCHITECTURE.md`
      layer-2; close the PRD #46 residual notes. Actual k8s manifests deferred to
      the remote-worker PRD.

## Out of Scope

- The full k8s remote-worker deployment (only aligning the design here).
- Cross-user isolation (already holds — the residual is intra-user).
- Re-litigating PRD #46's env-scrub / hooks-dir closes (those stand; this PRD
  closes the remaining same-uid file read, the environ race, and the shared-git
  channel).

## Success Criteria

- A process running as the execution uid (agent code, a self-improve check, a
  provision hook) **cannot**: read `/run/secrets/worker_token`, read the worker's
  `/proc` environ, **or** achieve code execution in the worker's PAT-bearing git
  via a shared-git-config write — all PoC-confirmed on the built image — while the
  worker authenticates and pushes normally.
- The PRD #46 self-improvement job still produces real (or honest-skipped) test
  evidence and opens/extends its MR, with the check code under the isolated uid.
- `run-e2e.sh` (judge + self-improve + existing) stays green.
- The documented same-uid residual (`self-improve.ts` / `docs/self-improvement.md`
  / `docs/proc-hardening.md`) updates from "accepted residual, uid-split is the
  structural close" to "closed for the local path" (with the k8s form mapped in
  docs and deferred to the remote-worker PRD).
