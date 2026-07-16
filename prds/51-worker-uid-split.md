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

## M1 gate resolution — design firmed (2026-07-16)

This resolves the M1 design gate. The mechanism (Decision 2), the capability
lifetime (B1 / Decision 7), the bare-cache ownership (B2 / Decision 4 —
separate-runner-clone),
the persisted-volume migration (B4 / Decision 4), and the k8s mapping (Decision
8) are now **decided, not draft**. The detailed analysis is unchanged in the
Decisions below; this section is the authoritative summary M2–M7 build to.

**Confirmed on the built image (PoC).** The same-uid token read was reproduced
against `uzi-agent:latest` (base template): the image's default user is
`uid=100(uzi)`, and a process running as `uzi` reads `/run/secrets/worker_token`
directly. Two findings:
- The worker and the SDK agent subprocess share uid `uzi` — no spawn site setuids
  (`sdk-spawn.ts`, `self-improve.ts:242`, `provision.ts`) — so the agent can
  `cat` the token → `claim` → forge PAT + Anthropic token. This is the vector the
  split closes.
- The compose secret mounts **`0444 root:root` (world-readable)**, not the
  `0400`/worker the design assumed. This corroborates audit L2: an env-sourced
  compose secret's `uid`/`gid`/`mode` are unreliable, so **M2 must force
  `0400`/`worker` on the token file in the entrypoint**, not trust compose to.

**Mechanism = (A1)** (Decision 2). A root entrypoint `setuid`-drops to uid
`worker`; the worker **retains only `CAP_SETUID`/`CAP_SETGID` as ambient caps for
the run lifetime** (it spawns runner-uid children per-run, in-process) and spawns
the SDK agent / self-improve checks / provision hooks as a distinct uid `runner`
holding no caps. `no-new-privileges:true` stays on (it does not block a root→lower
drop). **Drop tool (corrected — reviewer PoC):** the image's `/bin/setpriv` is the
**busybox** applet (busybox 1.37.0), which supports only
`--dump`/`--nnp`/`--inh-caps`/`--ambient-caps` — **NOT `--reuid`/`--regid`/
`--init-groups`** — so it **cannot** perform the uid drop. (An earlier note that "no
extra package is needed at the flag level" was wrong: it checked the ambient-caps
flags but not `--reuid`.) A1 therefore **requires `apk add setpriv`** (real
util-linux `setpriv`, same `/bin/setpriv` path, doing
`--reuid --regid --init-groups --ambient-caps +setuid,+setgid PROG` in one call) in
**both** the base **and** jvm Dockerfiles; `capsh` (`apk add libcap`) is the
alternative. None of util-linux `setpriv` / `gosu` / `su-exec` / `capsh` is on the
image today. M2 must still verify the ambient set survives runc clearing the
permitted set on the setuid-to-non-root transition (the B1 caveat). The container's
bounding set is `cap_add: [SETUID, SETGID, SETPCAP, CHOWN, DAC_OVERRIDE]`, but the
worker process keeps only `SETUID`/`SETGID` for the run lifetime and drops
`SETPCAP`/`CHOWN`/`DAC_OVERRIDE` after the root startup window — see the tightened
Decision 7 below.
- **(B) rootless bwrap/userns — DROPPED.** The dev host is darwin/Docker-Desktop;
  making unprivileged userns work under `cap_drop: ALL` + `no-new-privileges` +
  seccomp would need `seccomp=unconfined`/`CAP_SYS_ADMIN` or a suid `bwrap` (which
  `no-new-privileges` neutralises). No host PoC cleared it, so it is not a
  candidate.
- **(C) sidecar / second container — the eventual k8s form, not built locally.**
  See the k8s mapping and the N2 divergence note below.

**Decision 7 rewritten (B1 — old criterion RETIRED, cap set TIGHTENED).** The
success criterion is now: **for the run lifetime the worker holds ONLY
`CAP_SETUID`/`CAP_SETGID` (ambient, for the per-run runner spawns); the
startup-only caps `CAP_SETPCAP` (to establish the ambient set) and `CAP_CHOWN` +
`CAP_DAC_OVERRIDE` (the B4 volume-chown) are dropped from the worker's permitted
set immediately after the root startup window; the runner processes hold none.**
The original "caps added only to perform the drop and dropped immediately after /
no residual caps" is **impossible for (A)** — the already-dropped worker forks
runner-uid children per-run and therefore needs `CAP_SETUID` at each spawn — and is
struck. **Why the tightening matters (reviewer):** under `no-new-privileges` a
compromised worker that `setuid`s to 0 keeps **exactly** the caps it still holds;
leaving `CAP_CHOWN` + `CAP_DAC_OVERRIDE` in the permitted set would let that uid-0
process read/write **any** file in the container (bypassing every permission bit) —
a strictly larger blast radius than `CAP_SETUID` alone buys. Putting the caps in the
container **bounding set** (`cap_add`) is fine; the control is the **worker process
dropping** `SETPCAP`/`CHOWN`/`DAC_OVERRIDE` from its permitted set after startup.
**Posture disclosed plainly:** the PAT custodian still holds `CAP_SETUID`, so a
post-compromise worker could become any uid including 0. Accepted, because it is the
*trusted* side; the containment we buy is entirely on the *runner* side (the
untrusted code-execution surface holds no worker creds and cannot reach the token,
the worker's `/proc` environ, or worker-side git code-exec).

**B2 — bare-cache ownership: (b) SEPARATE RUNNER CLONE, worker BARE-ONLY (SETTLED
per reviewer + auditor).** The old "shared bare; worker owns `<bare>/config`+`hooks`;
runner writes `<bare>/objects`+`worktrees/<name>`" model is **insufficient** — a
linked worktree shares far more of the common dir than `objects/` + the per-worktree
admin, leaking two channels:
- **Shared-ref-write EACCES.** The agent's `git commit` writes the **shared** branch
  ref `<bare>/refs/heads/<branch>`, `<bare>/packed-refs`, and
  `<bare>/logs/refs/heads/<branch>` in the **common** dir (only `HEAD`, `index`,
  `logs/HEAD`, `ORIG_HEAD` are per-worktree) — a "runner writes objects +
  `worktrees/<name>` only" rule EACCESes the commit.
- **commondir/gitdir config-redirect (auditor PoC).** The per-worktree admin dir
  holds `commondir`/`gitdir` structural pointers; a runner rewriting `commondir`
  makes a later worker-side `git -C <worktree>` resolve a **runner-controlled**
  common dir — reading its `config` (arbitrary-name `filter.<x>.smudge=/evil`,
  code-exec) and `info/attributes`. Inline `-c`/`GIT_CONFIG_*` pins can't stop it
  (git resolves `commondir` from disk before applying inline config).

**Decision — (b) separate-runner-clone (auditor ruling, 12-case PoC matrix on git
2.55).** Fork (a) shared-bare-with-ownership-matrix is **dropped**: its "never pack
agent refs" invariant (an auto-gc / `pack-refs` landmine) was too fragile. Fork (b)
was PoC-confirmed safe — a runner-planted `uploadpack.packObjectsHook` does **not**
execute in the worker's fetch (git ignores that hook from the *fetched* repo's config
across local-path / `file://` / `file://--filter`), so (b)'s one worry is closed and
it kills the commondir/gitdir channel **and** the shared-ref-write EACCES *by
construction*.

**Architecture — the worker is BARE-ONLY; the working tree lives ONLY in the runner
clone; the worker NEVER runs `worktree add`/checkout.** Flow:
1. Worker `git clone --bare` (PAT, from the forge) → the worker's warm bare.
2. Runner clones/fetches from the **worker bare** to get its own working copy
   (runner-owned clone + working tree).
3. Agent checkout **and** commit happen in the **runner** clone (runner-owned).
4. Worker `git fetch` the agent branch **back from the runner clone** (local, inert).
5. Worker `git push` (PAT) from its bare.

The one worker-side op that ever fired smudge filters — `worktree add` checkout — is
**gone by construction**, not walled by ownership. `changedFiles` becomes a
tree-to-tree diff in the worker bare: `git -C <worker-bare> diff --name-only
origin/main...<fetched-ref>` — no working tree, `--name-only` fires no drivers, reads
only worker-owned config; `defaultBranchRef`/`branchExists`/`refExists` are already
bare ref queries. **All** worker-side git now runs on worker-owned config. Both local
fetches are safe: (i) the runner-as-fetcher reads the worker bare's **worker-owned**
config (even a hypothetical hook runs as the *runner* — no escalation, no PAT); (ii)
the worker fetches **from** the runner clone (inert direction). Worker creds (PAT)
touch only the forge clone/fetch + final push, on the worker bare.

**Six invariants (worker-side surface hardening for the relocated boundary — the
worker now reads a runner-controlled store via exactly one fetch):**
1. Runner has its own object store / clone; agent checkout+commit happen there
   (runner-owned); the runner never writes the worker bare.
2. The worker fetches **only** the specific agent branch refspec (e.g.
   `refs/heads/agent/issue-<iid>` → a worker-side tracking ref), **never**
   `refs/heads/*` — do not admit the runner's whole ref namespace into the worker bare.
3. Force the **pack-protocol** transport for that fetch (`file://` or explicit
   `--no-local`) so git's local-copy optimization does **not** traverse a
   runner-planted `objects/info/alternates` (CVE-2022-39253 class); pin
   `protocol.file.allow` deliberately.
4. Keep the worker's fetch on `gitEnv` (`GIT_CONFIG_NOSYSTEM=1` +
   `GIT_CONFIG_GLOBAL=/dev/null` + the M0 pins) so the worker's **own** config governs
   the fetching process; no runner-side config file is consulted for it.
5. **All** worker-side inspection (`changedFiles` / diff / `defaultBranchRef`) runs
   against the **worker bare** after the fetch, on `gitEnv` — never in the runner's
   worktree/clone — so no runner-owned config is read for any diff/checkout.
6. **M3/M6 evidence:** re-run the `packObjectsHook` / upload-pack-hook PoC on the
   **image's** git (`node:22-alpine` `apk` git, whatever version it ships) — the
   mitigation is version-dependent, so confirm on the built image, not just a host git.

**Two structural wins (auditor's full ruling):**
- **Closes the commondir/gitdir + shared-ref channels** (above).
- **Objects integrity.** The worker fetches the agent branch into its **own**
  worker-owned object store, so post-fetch a compromised runner **cannot
  delete/corrupt** the objects the worker's push depends on. Fork (a) would have left
  `<bare>/objects` shared + runner-writable (an intra-user availability/integrity DoS
  on the push); (b) removes that surface too.

**Namespaces.** The agent branch namespaces are `agent/*`, `ci-fix/*`, **and**
`uzi/*` (self-improve) — name all three wherever the design says "the agent branch."
Under (b) the runner clone is fully runner-owned, so there is **no** per-namespace
ownership matrix, but M3/M4 need the full list. (The worker's `git clone --bare` uses
the **files** ref backend by default, so the reftable edge the auditor flagged for
(a) is moot here.)

**Ownership under (b):**
- **worker-only:** the worker's warm bare `repos/<bare>.git` (its `config`, `hooks`,
  `refs`, `objects`) + the worker `.gitconfig` / token — the runner touches none of it.
- **runner-owned:** the runner's clone / object store + its working tree + the
  SDK/provision dirs.

**Shapes M3/M4/M5** (RESEQUENCED 2026-07-16 — coder proposal, lead-approved; the
milestone list below is authoritative): **M3** gives the runner its own clone **and
does the git-flow relocation** (was split into M5 in the earlier draft) — it moves
`worktreeForBranch`/checkout from the worker to the runner (now `runnerCloneForBranch`),
adds the worker `fetchAgentBranch` back-fetch, reshapes `changedFiles` into the
worker-bare tree-diff, and lands the safe-half `/data` ownership carve-out + worker
`TMPDIR` — all **single-uid** (no stray worker-side checkout), so it is provable now.
**M4** (spawn) runs the seed/checkout/commit under uid `runner` **and lands the
`/nix` group-write + worker-PATH-strip atomically** (they cannot precede the spawn —
see M4). **M5** is the PRD #46/#18 behavior-preservation + e2e residual. **Commit-identity
edge (M4 verify):** the agent's `git config user.email/name` writes the **runner clone's**
own config (runner-owned) — fine under (b); confirm commit identity resolves from the
runner config or `GIT_AUTHOR_*`/`GIT_COMMITTER_*` env, never a worker-bare config write.

*Delta vs today's `git.ts`* (my report to the lead): **moderate.** Today `GitCache`
keeps one worker-owned bare + `git worktree add` linked worktrees sharing its
objects+refs; under (b) the worker stays **bare-only**, a **runner-owned clone** holds
the working tree (new), `pushBranch` gains a preceding worker-side `git fetch
<runner-clone> <agent-ref>` with the invariant 2/3 refspec+transport pins, and
`changedFiles` moves from a worktree `git diff` to the worker-bare tree-diff. The
worktree lifecycle (`createOrAttachWorktree`/`worktreeForBranch`) relocates to the
runner. Bounded, but it reshapes the worktree/push/diff surface of `git.ts` (the M5
git-flow work).

This structurally closes the **arbitrary-name** class `filter.<name>.*`
(smudge/clean/process) / `diff.<name>.*` (command/textconv) / `merge.<name>.driver`
that M0's inline pins cannot cover (M0 audit LOW broadened the class) — no worker-side
git reads a runner config source at all. M0's fixed-name pins remain the belt for
`fsmonitor`/`diff.external`/`pager`/`sshCommand` regardless.

**B4 — persisted named-volume migration (Decision 4).** `agentnix`/`agentdata`
are seeded from the image on first use and thereafter **persist their original
ownership**; a Dockerfile chown touches only the image layer, not a populated
volume. When the execution uid changes from today's `uzi`, an existing install's
`/nix` + `/data` stay owned by the old uid → provisioning + worktree writes
EACCES. Decided: a **root-entry startup chown** of the mounted runner-owned
subpaths (`/nix`, and the runner clone/store + worktree checkouts under `/data`)
before the drop, needing `CAP_CHOWN` (+ `CAP_DAC_OVERRIDE`) — which the worker then
**drops from its permitted set after the startup window** (Decision 7). A clean
install (`down -v`) is the documented alternative. **Root-startup-window hygiene
(corrected — the "/nix empty at entry" justification was FALSE):** `agentnix` is a
named volume seeded from the image's **populated** `/nix` and persists prior-run
content, so `/nix` is **populated at entry** (and prior-run-influenced on reuse),
never empty. The correct guarantee is structural, not "safe because empty": the root
window execs **only image-baked, root-owned binaries by ABSOLUTE PATH**, with `PATH`
**excluding `/nix` and `/data`**, and **drops before anything resolves from a volume
— regardless of volume contents**.

**Full runner-writable path enumeration (Decision 4 / audit M1).** Cross-checked
against the worker's actual path construction. Everything under `$UZI_DATA_DIR`
(`/data`) unless noted:

| Path | Access under the split ((b) separate-runner-clone, worker BARE-ONLY) | Source |
| --- | --- | --- |
| `repos/<bare>.git/` — the worker's WARM bare cache (`config`, `hooks`, `refs`, `objects`) | **worker-only** — the worker is bare-only (no worktree/checkout); no runner-side git writes it, no worker-side git reads a runner config source | `git.ts:100` |
| the RUNNER clone / object store + **its working tree** (the ONLY working tree) | **runner-owned** — agent checks out + commits here; worker `fetch`es the branch back from it, then pushes | Decision 4 (M3 seeds it) |
| `agent-home/<runId>/` (per-run SDK HOME) | runner-owned | `main.ts:55,71`, `runner.ts:126` |
| `agent-home/` (shared provisioning HOME — nix profile / devbox warm-start metadata) | runner-owned | `main.ts:81`, `sdk-executor.ts:153` |
| `provision/<runId>/` (`provisionRoot` — synthesized devbox.json, outside any clone) | runner-owned | `sdk-executor.ts:154`, `provision-run.ts:69` |
| `/nix` (nix store, `agentnix` volume) | runner-owned — devbox/nix realize packages as the runner | Dockerfile, `docker-compose.yml:245` |
| judge SDK HOME (`homeRoot` = `/data/agent-home` in prod; `mkdtemp` under it) | runner-owned execution-surface HOME (its trace-fetch / review-POST HTTP stays worker-side — Decision 1) | `judge-runner.ts:191`, `main.ts:124` |
| worker HOME + `.gitconfig`; `/run/secrets/worker_token` | **worker-only** | Decision 5 |

The dividing line under (b): the worker is **bare-only** and owns its warm bare +
`config` + token; everything the untrusted execution surface writes (the runner
clone/store + **the only working tree**, SDK HOME, provision dirs, `/nix`) is a
**runner-owned tree that the worker never reads as a git config source**. Ownership is
enforced by the B4 startup chown plus the per-run spawn under uid `runner` (M4); the
dirs the worker `mkdir`s today (`sdk-executor.ts:195-201`, `provision.ts`, `git.ts`)
become runner-owned at creation, and the worktree lifecycle relocates to the runner
(M5).

**A1 net-ns / TMPDIR / fd invariants (5-bis — LOCAL, not k8s footnotes).** Under
A1 the worker and runner share one container, hence **one network namespace and one
`/tmp`**, so these are load-bearing invariants for the local build now (they also
carry to the k8s form, where pod containers still share the net ns even with
`shareProcessNamespace: false`):
- **No debug port on the worker node process.** No `--inspect`/`--inspect-brk` on
  its argv **and** no `NODE_OPTIONS=--inspect…` in its env — a loopback debug port
  is reachable by the same-net-ns runner and is RCE in the PAT holder. None exists
  today; assert it stays absent.
- **The in-process runner spawn helper sets explicit stdio + closes all
  non-`{0,1,2}` fds.** The worker holds the PAT + Anthropic token in memory and live
  API sockets; the new spawn path is exactly where an fd could leak to the runner
  child. Node sets CLOEXEC by default — assert it; the **M6 fd-leak test** (the
  runner child's `/proc/self/fd` is only `{0,1,2}` + known) is **load-bearing under
  A1**, not a nicety.
- **Distinct `TMPDIR` per uid on `0700` trees.** *Not* about the SDK HOMEs — in prod
  the judge SDK HOME and the check-runner HOME are **overridden onto
  `/data/agent-home`** (`main.ts:124` passes `homeRoot: sdkHomeRoot` to
  `JudgeRunner`; `runner.ts:126→270` runs the check with `runHome =
  agent-home/<runId>`); the `?? os.tmpdir()` in `judge-runner.ts:107` /
  `runner.ts:270` is only the stub/test default. The real exposure is the
  **scratch/`TMPDIR` temp writes** `git`/`npm`/`node` make on a shared sticky `/tmp`
  (symlink races + exposure of any worker temp write). Give worker and runner
  distinct `TMPDIR`s on their own `0700` trees; audit worker-side temp writes.
- **No cross-uid-reachable control channel.** If a worker↔runner IPC channel is ever
  needed (e.g. an A3 spawn-broker — not the chosen A1), it uses an fd-inherited
  `socketpair` or a `0600` pathname socket, **never** an abstract unix socket or a
  loopback port (the runner shares the net ns and could connect).

**k8s mapping (Decision 8 — docs-only here).** Align at the **distinct-uid
abstraction, not the mechanism.** The k8s form is two containers in one pod with
per-container `runAsUser` (worker 10001 / runner 10002), `shareProcessNamespace:
false`, and the token projected only into the worker — that is the PRD's
**(C)/sidecar**, which needs **no `CAP_SETUID` and no in-process uid spawn**. Do
**not** claim (A1) "maps 1:1 to `runAsUser`." Actual manifests defer to the
remote-worker deployment PRD; this PRD only keeps the design aligned so they don't
drift.

**N2 divergence — build (A1) as the local bridge, pay for (C) at the pod move.**
(A1) keeps the worker↔SDK boundary in-process (pid capture, `killAgentTree`,
per-run session dir, in-process abort — `sdk-executor.ts`); (C) rebuilds it as
IPC. So the M4 spawn wiring is partly throwaway for the eventual k8s (C) redesign.
Decision: still build (A1) locally — it is kernel-enforced, has no fragile userns
dependency, and ships on the compose MVP we deploy today; going straight to (C) in
compose would force the IPC rebuild now for a form we do not yet run, buying the
**same** containment. The throwaway is the control-plane wiring, not the security
model.

## Design Decisions (analysis — firmed at the M1 gate above, 2026-07-16)

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
   a code-exec key — the FIXED-name `core.fsmonitor`, `core.sshCommand`,
   `diff.external`, `core.pager`, plus (M0 audit) `credential.helper`,
   `core.askpass`, `core.alternateRefsCommand`, AND the ARBITRARY-name class
   `filter.<name>.*` (smudge/clean/process) / `diff.<name>.*` (command/textconv) /
   `merge.<name>.driver` — that fires in a later worker-side git process. PRD #46's
   `core.hooksPath` pin neutralises hooks only. (The authoritative final pin /
   exclusion set lives in `agent/src/git.ts` `GIT_CODE_EXEC_KEY_PINS`.)

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
     covers the ARBITRARY-name class `filter.<name>.*` (smudge/clean/process) /
     `diff.<name>.*` (command/textconv) / `merge.<name>.driver`, whose driver names
     are attacker-chosen and so **cannot** be blanket-pinned via `GIT_CONFIG_*`
     (correcting 3b's "pin `filter.*`"). Reachability precision: on `worktree add`
     (checkout) it is the **filter** keys that fire (the attacker controls the
     worktree's `.gitattributes` + plants `[filter "x"]` in config); the `diff.*` /
     `textconv` keys need a **content** diff, which the worker's `--name-only`
     `changedFiles` never runs, so they are lower-reachability today. Inline
     `extensions.worktreeConfig=false` does **not** help (verified in M0: git
     decides which config files to read before inline overrides apply), so the
     runner-writable per-worktree `config.worktree` is closed only by
     `<bare>/config` ownership (the attacker cannot then enable `worktreeConfig`).
   - **(b) Hardened worker git (defense-in-depth for the pinnable keys):** pin
     `core.fsmonitor` / `core.sshCommand` / `diff.external` / `core.pager`
     (command-valued) plus `credential.helper` / `core.askpass` /
     `core.alternateRefsCommand` (empty-valued reset/disable, M0 audit)
     **unconditionally** (not gated on the PAT — `changedFiles` and every worktree
     op must be covered) via inline `GIT_CONFIG_*` pairs, the way `core.hooksPath`
     is already pinned. `core.gitProxy` is deliberately **excluded**: it is a
     multivar whose planted entry an inline append does **not** override (verified),
     and only `git://` consults it — a transport the worker never uses. This is a
     lagging denylist (git keeps adding code-exec keys), so it is the *backup*, not
     the primary guarantee.

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
   **(A1)**, the **worker retains only `CAP_SETUID/SETGID` for the run lifetime**
   (as ambient caps) because it spawns runner-uid children per-run; the **runner
   children hold no caps** and a distinct uid. The success criterion is **tightened
   at the M1 gate** (see "Decision 7 rewritten" above): **run-lifetime =
   `SETUID`/`SETGID` ambient only; the startup-only `SETPCAP`/`CHOWN`/`DAC_OVERRIDE`
   are dropped from the worker's permitted set after the root startup window** — a
   uid-0-capable worker must not also keep `CHOWN`+`DAC_OVERRIDE` (any-file
   read/write). **Not** "no residual caps," which is impossible for (A).
   `no-new-privileges` stays on post-drop. Disclose the posture: the PAT custodian
   holding `CAP_SETUID` can, post-compromise, become any uid including 0 — accepted
   because it is the *trusted* side and the containment we buy is on the *runner*
   side.
   - **Root startup-window hygiene (audit M5 — corrected).** (A) reintroduces a root
     entrypoint. The root window must exec **only image-baked, root-owned binaries
     by ABSOLUTE PATH**, with `PATH` **excluding `/nix` and `/data`**, no shell
     interpolation of env/args, and drop **before anything resolves from a volume,
     regardless of volume contents**. (The earlier "`/nix` is empty at entry"
     justification is FALSE: `agentnix` is seeded from the image's populated `/nix`
     and persists prior-run content — the guarantee is the absolute-path + PATH
     exclusion, not emptiness.) The `setpriv` wrapper is not itself a
     suid-escalation surface.

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

## Technical Design (firmed at the M1 gate; B1/B2/B4 resolved above)

> This section sketches the (A1) shape. B1 (capability lifetime), B2 (per-path
> bare-cache ownership), and B4 (persisted-volume migration) are **resolved** at
> the M1 gate — see "M1 gate resolution" above; the per-path split (Decision 4)
> supersedes any coarse "bare cache worker-owned" phrasing below.

- **agent/ image (`base` + `jvm`)**: second uid; root entrypoint + **util-linux
  `setpriv`** (`apk add setpriv`; the busybox applet can't `--reuid`) drop wrapper
  (mechanism A1, worker **retains** only `CAP_SETUID/SETGID` ambient); `/nix` and the
  **runner clone/store + its working tree (the only checkout)** + runner data paths
  (Decision 4, (b) separate-runner-clone) owned by the execution uid, while the
  worker's warm bare `repos/<bare>.git` (config/hooks/refs/objects) stays worker-owned
  and the worker runs bare-only (no worktree/checkout).
- **docker-compose.yml**: enforce token `0400`/`worker` in the entrypoint (not via
  the env-sourced secret); bounding set `cap_add: [SETUID, SETGID, SETPCAP, CHOWN,
  DAC_OVERRIDE]` with the worker dropping all but `SETUID`/`SETGID` after startup
  (Decision 7); root entry; startup chown of persisted `agentnix`/`agentdata`.
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

- [x] **M0 — Standalone shared-git hardening (fast-follow, NO uid split; review
      B3). DONE — landed on this branch (commit 4e4c44e + the M0-audit follow-ups),
      not a separate MR (single-PR flow, per the team brief).** M0 delivers only the
      part achievable BEFORE the uid split: pin the **fixed-name** code-exec git keys
      **unconditionally** in every worker-side git (not PAT-gated — covers the no-PAT
      `changedFiles`/`worktree add`) — `core.fsmonitor` / `diff.external` /
      `core.pager` / `core.sshCommand`, plus (M0 audit) `credential.helper` /
      `core.askpass` / `core.alternateRefsCommand`; and set `GIT_CONFIG_NOSYSTEM` + a
      `/dev/null` `GIT_CONFIG_GLOBAL` default (the e2e insteadOf passthrough
      preserved). Gate met: an agent-uid `printf > <bare>/config` of a
      `diff.external`/`fsmonitor`/`credential.helper`/`core.askpass` **cannot**
      code-exec in the worker's later `diff`/`status`/`worktree add`/credential fill
      (functional PoC in `agent/test/git-hardening.test.ts`).
      **Explicitly DEFERRED to M3 (NOT achievable in M0, so NOT claimed here):**
      (a) the "`<bare>/config` + `<bare>/hooks` non-writable by the agent uid"
      ownership wall — meaningless pre-split (the agent runs as the same uid that
      owns the file, and `chmod` is reversible by the owner, so a chmod here would be
      theater); and (b) closing the **arbitrary-name** class `filter.<name>.*` /
      `diff.<name>.*` / `merge.<name>.driver` (driver names are attacker-chosen, not
      blanket-pinnable — closed only by config-source ownership under the split).
      `core.gitProxy` and `core.editor` are excluded, documented in `git.ts` (not
      inline-pinnable / no worker fire point respectively). See Decision 3.
- [x] **M1 — Threat model + mechanism decision (design gate). DONE 2026-07-16 —
      see "M1 gate resolution" above (A1 chosen, B1/B2/B4 settled, same-uid read
      PoC confirmed on `uzi-agent:latest`, k8s mapping aligned).** Confirm the
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
      **Gate additionally (PRD #58 consumer):** the entrypoint tolerates a
      **non-root start** — a hosted-k8s consumer runs this SAME image in a
      restricted-PodSecurity namespace (`runAsUser: 10001`, no `cap_add`), where an
      unconditional `setpriv --reuid` would EPERM → CrashLoop. The root window
      (B4 migration + token chmod + the A1 drop) is conditional on a root start
      (`id -u == 0`); a non-root start runs **single-uid** (no root window, no
      EPERM), which is #58 v1's accepted posture. The #51 uid-split containment
      applies only on the root-started (compose/A1) path; the k8s split lands later
      via (C)/two-containers (Decision 8). Verified: root start still drops to
      `worker` with setuid/setgid-only caps + 0400 token; `--user 10001 --cap-drop
      ALL` start runs single-uid, rc=0.
- [x] **M3 — Store topology + git-flow relocation + safe-half ownership ((b)
      SEPARATE-RUNNER-CLONE, worker BARE-ONLY — B2 SETTLED). DONE 2026-07-16 —
      single-uid, fully testable now.** RESEQUENCED (coder proposal, lead-approved
      2026-07-16): the git-flow relocation the earlier draft parked in M5 lands HERE,
      because under (b) "give the runner its own clone" is inseparable from the
      `git.ts` reshape (a clone nobody seeds/fetches-back is dead code) and M3's
      topology gate is only meetable once the flow is relocated — and the whole
      relocated flow works **single-uid** (no dependency on the M4 spawn), so it is
      provable now against the real-git harness. Delivered: the runner gets its **own**
      clone/object store (working tree lives ONLY there; the worker stays bare-only);
      `git.ts` `runnerCloneForBranch`/`fetchAgentBranch`/`changedFiles` reshape + the
      `runner.ts` rewiring; the agent checks out + commits in the runner clone; the
      worker `fetch`es the agent branch back from it (the six B2 invariants:
      single-branch refspec → worker tracking ref `refs/uzi-runner/<branch>`,
      `file://`+pack transport + pinned `protocol.file.allow`, worker fetch on `gitEnv`,
      `--no-tags`), then pushes with the PAT from the tracking ref. **Seed transport
      (security-relevant, stated for the review):** the runner clone is a local
      `git clone --shared` from the worker bare — the runner references the bare's
      objects **read-only via `objects/info/alternates`** (it CANNOT corrupt worker-bare
      objects through it), and the runner's own new commit objects land in the clone's
      own store; the worker's fetch-BACK is `file://`+pack (never the local-copy
      optimization), so it NEVER traverses the runner clone's alternates or a
      runner-planted `objects/info/alternates` (CVE-2022-39253, invariant 3). Safe-half
      **ownership**: the `/data` runner-owned subtree carve-out (`runner/` clone store +
      `agent-home/` + `provision/` → `worker:runner 2775` setgid, worker bare `repos/`
      worker-only) + the worker's distinct `0700` `TMPDIR` (5-bis) land in the
      entrypoint, INERT until M4 (no runner process writes them pre-split). Namespaces
      `agent/*` + `ci-fix/*` + `uzi/*`. (Decision 3's git hardening already landed in
      M0.) **/nix group-write + the worker-PATH-strip moved to M4** (see M4 — they are
      unsafe to land here; the atomicity WHY is there). Gate met (single-uid): agent
      commit lands in the runner store; worker `fetch`+push works; `changedFiles` is a
      worker-bare `--name-only` tree-diff; a `<bare>/config` plant is unreadable by the
      runner clone's git (config-source isolation test) and a runner `commondir`/config
      rewrite cannot reach worker-side git by construction (worker reads no runner config
      source); the `packObjectsHook`/upload-pack PoC re-confirmed on the **image's** git
      (node:22-alpine, git 2.54.0) + host git 2.55.0. The **uid-boundary** half of the
      gate (the `runner` uid specifically does the seed+commit while `worker` owns the
      bare) is proven in M4.
- [ ] **M4 — Spawn surfaces under the execution uid ((b) ownership goes live +
      `/nix` group-write + worker-PATH-strip, ATOMIC).** SDK agent, check runner,
      provision hooks launch under the execution uid; the worker retains `CAP_SETUID`
      and does the credentialed git/HTTP; capability hygiene (Decision 7),
      root-startup-window hygiene (M5-audit), and no cross-uid IPC channel (5-bis)
      verified. **These THREE land as ONE unit (resequenced from M3, lead-approved):**
      (a) `/nix` becomes group-runner-writable (handling the `migrate_tree` group guard,
      which keys on owner only), (b) the worker's credentialed-exec PATH drops `/nix` →
      root-owned image dirs ONLY (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:
      /sbin:/bin`) with `/nix` carried ONLY on the runner/SDK/provision PATH
      (`buildSdkEnv`/`buildProvisionEnv`/`toolEnv`), and (c) provisioning + the SDK +
      checks spawn as `runner`. **Why atomic (Decision 6):** they CANNOT split across
      milestones — in M3 provisioning still runs as the worker, so stripping `/nix` from
      the worker PATH would break it, while enabling `/nix` group-write with `/nix` still
      on the worker PATH is exactly the cross-uid-code-exec-into-the-PAT-holder window;
      only when provisioning moves off the worker (the spawn) is the trio coherent. The
      M3 `/data` carve-out + runner `TMPDIR` go live here (per-run leaf-dir group-write +
      the runner env's `TMPDIR=/tmp/uzi-runner`); PATH hygiene (Decision 6).
- [ ] **M5 — Preserve PRD #46/#18 behavior + e2e retooling.** devbox/nix
      provisioning (`toolEnv`), real check evidence (`go test`/`npm test`/`tsc`),
      runner-clone git, and the agent's own sandbox (`settingSources:[]` + deny-hook)
      all work across the boundary. (The git-flow relocation moved to M3; M5 is the
      behavior-preservation + e2e residual.) **Retool the e2e (N4):** under the split the
      token is worker-owned `0400` and **not** unlinked, so `run-e2e.sh`'s
      writable-mount delivery and its "token unlinked" assertion must change to
      uid-boundary reads. Gate: `run-e2e.sh` (judge + self-improve + existing) green.
- [ ] **M6 — Tests.** uid-boundary tests: execution uid can't read the token file,
      can't read the worker's `/proc` environ, and can't code-exec via a shared
      git-config write **nor via a `commondir`/`gitdir` rewrite** (moot-by-construction
      under (b) — the worker is bare-only and never reads a runner config source — but
      keep an explicit regression test) **nor via a runner-planted
      `uploadpack.packObjectsHook`/upload-pack hook exercised on the IMAGE's git**
      (B2 invariant 6 — the mitigation is git-version-dependent, so this runs against
      `node:22-alpine`'s git, not just a host git); **plus (A1 net-ns, L1/L5):** the
      runner child's `/proc/self/fd` is only
      `{0,1,2}`+known (fd leak — **load-bearing under A1**); the worker node process
      has **no `--inspect`/`NODE_OPTIONS=--inspect` debug port**; worker and runner
      have **distinct `TMPDIR`s**; a runner survivor cannot read the worker's **push
      git-child** `/proc/environ` during the window (the actual PAT-race close); and
      the runner child's **own** environ is still scrubbed (no join token / PAT / API
      URL) under the new spawn path. Worker can still push. No regression.
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
  non-credentialed `changedFiles` bare tree-diff and the local fetch from the runner
  clone — the worker is bare-only, so there is no longer a worker-side `worktree add`
  checkout) via a shared-git-config write, a `commondir`/`gitdir` redirect, or a
  runner-planted upload-pack hook — all PoC-confirmed on the built image — while the
  worker authenticates and pushes
  normally.
- Capability posture is stated honestly and **minimised**: **for the run lifetime
  the worker holds ONLY `CAP_SETUID/SETGID` (ambient); `CAP_SETPCAP`/`CAP_CHOWN`/
  `CAP_DAC_OVERRIDE` are startup-window-only and dropped from the worker's permitted
  set after the root startup window; the runner processes hold no caps** (the
  earlier "no residual caps" criterion was impossible for mechanism (A) and is
  retired; leaving `CHOWN`+`DAC_OVERRIDE` on a uid-0-capable worker was too broad —
  B1).
- The agent's own `git add`/`git commit` still succeeds in the **runner's own
  clone/store** — the only working tree ((b) separate-runner-clone, worker bare-only
  — B2); the worker `fetch`es the agent branch back from that store (single-branch
  refspec, pack transport) and pushes with the PAT from its bare, and runs no
  worktree/checkout of its own.
- The PRD #46 self-improvement job still produces real (or honest-skipped) test
  evidence and opens/extends its MR, with the check code under the isolated uid.
- `run-e2e.sh` (judge + self-improve + existing) stays green.
- The documented same-uid residual (`self-improve.ts` / `docs/self-improvement.md`
  / `docs/proc-hardening.md`) updates from "accepted residual, uid-split is the
  structural close" to "closed for the local path" (with the k8s form mapped in
  docs and deferred to the remote-worker PRD).
