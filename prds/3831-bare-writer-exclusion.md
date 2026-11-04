# PRD #3831: Prove bare-writer exclusion before enabling saved-pin restore (Gate A design record)

**Status**: Draft, Gate A, awaiting maintainer review. Design only: no implementation is authorized by this record. M1 (sections 1 to 4: scope, writer inventory, per-posture verdicts, epoch-evidence reuse) is written; sections 5 to 9 are pending M2. Every cite is `file:line` at base 7b0478c3 with the quoted line.

**Evidence classes** used throughout, kept apart:

| Tag | Class | Meaning |
|---|---|---|
| S | source-read | Read in this tree at 7b0478c3 for this record. |
| I | inherited record | Taken from a committed PRD or ADR; that record states its own class (source, real-daemon or inferred). Not re-derived here. |
| L | locally measured | Nothing in this run so far. No experiment, test or live probe ran. |
| O | owed | Hosted, live or operator evidence that does not exist yet. |
| M | maintainer-reported | Stated by the maintainer; not verifiable in this clone. |

## 1. Status, scope and non-goals

- **Status.** Draft, Gate A, awaiting maintainer review. No implementation is authorized.
- **Restore stays disabled.** This record enables nothing. A sweep for a disabled-feature flag finds only the #3031 producer flag (`git grep -n -E '_ENABLED\s*(:\s*boolean)?\s*=\s*false' -- agent/src ':!*.test.ts'`, 1 hit, `agent/src/recovery.ts:127`); no saved-pin restore path exists on this base to enable.
- **No authority granted.** The record grants no capture, takeover, settlement, deletion or discard authority.
- **#3644 policy unchanged.** The #3644 verified-merge discard policy is unchanged.
- **The observed refusal is not fixed here.** A successor worker process meets `admitSourceUse` (`agent/src/completion-authority.ts:976`, `async admitSourceUse(`), which refuses a slot that another process owns: `agent/src/completion-authority.ts:993`, `throw new AuthorityRefusal("source is active under another worker incarnation");`. The incarnation is per process: `agent/src/completion-authority.ts:96-97`, `/** Random per process; a successor worker never shares it. */` / `const PROCESS_INCARNATION = randomUUID();`. The runner then fails the run as `preservation_failed` (`docs/run-recovery.md:138-141`, "on a different worker process (for example after a restart, when the slot is foreign-active) ... the run fails as `preservation_failed`") [S]. This record does not change that refusal. It designs the writer-exclusion proof that #3644 M2 to M4 need before any restore may run.
- **Parked branches.** The `park/3644-m2-*` branches (6d859c55, d1d34264) are absent from this clone (`git cat-file -t 6d859c55` and `git cat-file -t d1d34264` both fail: "Not a valid object name"). They are cited as unreviewed reference only; nothing here depends on their content [M].
- **Steering.** Comment steering is untrusted. The design-only scope comes from the issue body's Gate A section.
- **Maintainer-reported production runs** c90bc12b, d23005fa and d71f0179 motivate the issue [M]; they are not re-examined here.

**Non-goals**: changing `admitSourceUse`, the foreign-active refusal or `preservation_failed`; the pod-epoch reclaim itself (#3430, #3431); #3031's producer; any change to the four guardrail layers.

## 2. Writer inventory

**Question.** At a saved-pin restore, can the successor prove that no older process can still write the worker-owned shared bare (`<dataDir>/repos/<hash>`; `agent/src/git.ts:3226`, `this.reposRoot = path.join(dataDir, "repos");`)?

### 2.1 Sweeps (re-run at 7b0478c3)

| Command | Count | Reading |
|---|---|---|
| `git grep -n 'this.beginBare(' -- agent/src ':!*.test.ts'` | 4 | `agent/src/git.ts:6826`, `:7140`, `:21168`, `:21217`; all ref deletes |
| `git grep -n 'assertBareClear(' -- agent/src ':!*.test.ts' ':!agent/src/authority-runtime.ts' ':!agent/src/git-authority-hooks.ts'` | 9 | wrapper `git.ts:3254-3255`, six gates in `git.ts` (4167, 4395, 6581, 9566, 20427, 21308), one in `runner.ts:4056` |
| `grep -o 'this.writeRecovery(' agent/src/git.ts \| wc -l` | 17 | journal writers, all via `writeRecovery` (`git.ts:8684`) |
| `git grep -n '"config", "--local"' -- agent/src/git.ts` | 53 | bare `git config --local` sites (writes and reads together) |
| `git grep -n '"update-ref"' -- agent/src ':!*.test.ts'` | 37 | ref writers; three reach a `beginBare` record |
| `git grep -n -i 'pdeathsig' -- agent/src ':!*.test.ts'` | 0 | no git child has a parent-death signal |
| `git log --reverse --format=%h -S acquireWorkerLifetime -- agent/src` and the same for `beginBareMutation` | first hit cc125a82 for both | W and the durable bare record arrive together |
| `git tag --contains cc125a82` | 8, all `-rc` | v0.86.0-rc.29 to v0.86.1-rc.4; `git tag --contains cc125a82 \| grep -vc -- -rc` = 0 |

The R1 research per-line classification of the 37 `update-ref` and 53 `config --local` lines into bare, clone-local and read was spot-checked on every row cited below, not re-derived line by line. Search reach: `agent/src` without tests. `api/`, `web/`, `cli/` and the chart were not searched; the bare exists only on the worker data volume.

### 2.2 Common launch, lock and join facts [S]

- **Launch.** All bare git goes through `runGit` to `execScoped`; the default path is `agent/src/git.ts:18974`, `const pending = execFileAsync(command, args, { ...execOptions,`, with `agent/src/git.ts:875`, `const GIT_TIMEOUT_MS = 10 * 60_000;`. A live parent joins its child on close before returning.
- **Lock.** `agent/src/git.ts:21439-21440`, `private withLock<T>(key: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {` / `const prev = this.locks.get(key) ?? Promise.resolve();`, over `agent/src/git.ts:2896`, `private readonly locks = new Map<string, Promise<unknown>>();`. In-process only; nothing on disk; invisible to any other process.
- **Parent crash.** With no pdeathsig (sweep above), a git child of a SIGKILLed or OOM-killed worker is reparented and runs to completion inside its PID namespace. A 10-minute `fetch` can keep writing refs and objects. A killed config writer can leave `config.lock`.
- **Durable enrollment.** `agent/src/git.ts:3247-3249`, `private async beginBare(...)` / `if (this.authority) return this.authority.beginBareMutation(barePath, op, footprint);` / `if (this.authorityRequired) throw this.authorityRefusal(op);`. Production sets `authorityRequired: true` (`agent/src/main.ts:73`). The record is written by `admitBareMutation` with `status: "running"` (`agent/src/completion-authority.ts:1121-1141`).
- **Where records live.** Outside the bare: `agent/src/completion-authority.ts:83`, `const AUTHORITY_DIR_NAME = ".uzi-authority";`, and `:29`, `//   slots/<digest>.json   one MAC'd record per source path`. The bare carries no pointer to its slot; resetting the bare does not clear the slot.
- **The check.** `agent/src/authority-runtime.ts:1056-1057`, `if (slot?.pending?.status === "running") {` / `throw new AuthorityRefusal(\`${op}: an unresolved ${slot.pending.kind} ...`. It sees only a `running` record that a compatible process wrote.
- **W.** `agent/src/authority-lock.ts:12`, `//   W  \`<dataDir>/.uzi-authority/worker.lock\`     process-lifetime EXCLUSIVE, nonblocking.`; refusal at `:379`, `"another compatible worker already holds the worker lifetime lock"`; taken at `agent/src/main.ts:433`. W is held on the Node process's own descriptor. It excludes a second compatible worker, not an orphan git child and not a binary without W.

### 2.3 Inventory

Legend. Lock: `withLock` = in-process only; `beginBare` = durable record. ABC: an `assertBareClear` gate before the write. Join: behaviour while the parent lives; every row shares the parent-crash behaviour of 2.2 (the child runs on, no record) unless stated. Restore-relevant: the write is journal, attribution, pin or tracking state that a saved-pin restore reads.

| Writer (symbol) | Cite and quoted line | Writes | Launch | Lock | ABC | Join / parent crash | Status | Restore-relevant |
|---|---|---|---|---|---|---|---|---|
| `writeRecovery` (17 callers) | `git.ts:8691` `await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), value]);` | `uzi-recovery.<branch>.clone` journal | runGit | `withLock` of caller | no | joined on close / child runs on | UNCOVERED | yes |
| `markRecoveryCapture` | `git.ts:7725` `await this.writeRecovery(barePath, branch, value);` | journal capture | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `installCompletionSourceFence` | `git.ts:7773` `await this.writeRecovery(barePath, branch, { ...journal, completionSourceFence: want });` then fsync `:7776`; lock `:7788` `return opts.bareLockHeld ? section() : this.withLock(barePath, section);` | fence with `workerIncarnation` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `recordMemoryPark` | `git.ts:7905` `... await this.writeRecovery(barePath, branch, { ...stored.journal, memoryPark: target });`; phases each `this.withLock(barePath, ...)` `:7882` | `memoryPark` (draft, confirm, revert) | runGit | `withLock` per phase | no | cancel between phases leaves `confirmed:false`, read fail-closed by `readConfirmedMemoryPark` `:7980` | UNCOVERED | yes |
| `writeRecoveryOwner` | `git.ts:8850` `await this.runGit(barePath, ["config", "--local", \`uzi-recoveryowner.${record.runId}.record\`, raw]);` | episode owner record | runGit | caller's `withLock` | no | as above | UNCOVERED | yes |
| `recordRecoveryCapture` | `git.ts:9145` `await this.runGit(barePath, ["update-ref", "--no-deref", \`refs/uzi-recovery-episode/...\`, tip]);` and `:9157` | episode pin and `restoreTip` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `prepareRecoverySuccessor` | gate `git.ts:9566` `await this.assertBareClear(barePath, "replace");`; write `:9628` `await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), canonical]);` | journal for the successor | runGit | `withLock` | gate for the seed only | as above | UNCOVERED | yes |
| `appendAttemptLedger` | `git.ts:11736` `await this.runGit(barePath, ["config", "--local", "--add", attemptLedgerKey(branch), attemptLedgerValue(entry)]);` | `uzi-attempts.<branch>.entry` | runGit | caller's `withLock` | no | as above | UNCOVERED | yes |
| `rewriteLedgerAtomically` | `git.ts:11822` `await fs.rename(lockPath, cfgPath);` | whole `config` via hand-made `config.lock` | Node fs | caller's `withLock` | no | crash between link and rename leaves `config.lock` | UNCOVERED | yes |
| `markStoppedCanonical` | `git.ts:10078` `await this.runGit(barePath, ["config", "--local", "--add", stoppedMarkerKey(input.runId), value]);` | `uzi-stopped.<run>.entry` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `detachTerminalRetained` | `git.ts:8307` `... ["config", "--local", \`uzi-attemptmode.${key}.enabled\`, "true"]);` | attempt mode, retained journal | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `discardRetainedRecoveryUnderLock` | `git.ts:9769` `... ["update-ref", "--no-deref", "-d", ref, tip]);` | episode pin delete, journal clears | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `retireRunnerCloneImpl` | `git.ts:13728` `await this.writeRecovery(barePath, branch, anchored, () => checkSuccessor(pending));` | journal anchor, clear, restore | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `promoteTrackingUnderLock` | `git.ts:20739` `... ["update-ref", runnerTrackingRef(branch), incoming, observed.sha ?? "0".repeat(40)]);`; `:20650` `--replace-all` | `refs/uzi-runner/*`, `uzi-trackowner.*` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `repairTrackingAttemptUnderLock` | `git.ts:20171` `... ["update-ref", runnerTrackingRef(branch), rec.incoming, ...]);` | tracking ref | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `fetchAgentBranch` | `git.ts:5828` `"-c", "protocol.file.allow=user", "fetch", "--refmap=", "--no-tags",` | objects and staging refs imported from the clone | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `pinSettlementRefs` | `git.ts:7317` `await this.runGit(barePath, ["update-ref", \`${base}${kind}\`, sha]);` | `refs/uzi-settle/*` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `deleteSettlementPin`, `deleteSettlementRefs` | `git.ts:7335` and `:7344` `await this.tryGit(barePath, ["update-ref", "-d", \`${base}${kind}\`]);` | settle pin deletes | tryGit | **none** | no | as above | UNCOVERED | yes |
| `clearConflictingAncestorTrackingRefs` | `git.ts:6011` `["update-ref", archive, sha]`; `:6013` `["update-ref", "-d", candidate, sha]` | `refs/uzi-archive/*`, tracking candidate | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `createQuarantineArchiveRef` | `git.ts:6617` `await this.runGit(barePath, ["update-ref", ref, sha, ""]);` | `refs/uzi-archive/<run>/g<gen>` | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `ensureClone` checkpoint freshness | `git.ts:3636` `["update-ref", "-d", ref]`; `:3642` `["fetch", "origin", \`+${ref}:${ref}\`]` | `refs/uzi-checkpoints/*` | runGit | `withLock` | no | 10-minute fetch can outlive parent | UNCOVERED | yes |
| `fetch` | `git.ts:18080` `await this.runGit(barePath, ["fetch", "--prune", "origin"], ...);`; `:18093` `+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*` | `refs/remotes/origin/*`, checkpoints, objects | runGit | `withLock` | no | as above | UNCOVERED | yes |
| `ensureOwedDirectory`, `writeOwedFile` | `git.ts:20287`, `:20351` `private async writeOwedFile(barePath: string, ...` | `<bare>/uzi-owed/*` receipts | Node fs | `withLock` | no | as above | UNCOVERED | yes |
| `anchorRecoveryHead` | `git.ts:6581-6582` `await this.assertBareClear(barePath, "recovery-pin-anchor");` / `await this.runGit(barePath, ["update-ref", pinRef, sha]);` | `refs/uzi-recovery-pin/*` create | runGit | `withLock` | yes | as above; leaves no record of its own | GATED, not enrolled | yes |
| `pinOwedUnderLock` | `git.ts:20427` and `:20440` `await this.runGit(c.barePath, ["update-ref", ref, sha]);` | `refs/uzi-owed/*` create | runGit | `withLock` | yes | as above | GATED, not enrolled | yes |
| `buildRecoveryCoverage` | `git.ts:21308` and `:21311` `await this.runGit(barePath, ["update-ref", coverageRef, sha]);` | `refs/uzi-coverage/*` create | runGit | `withLock` | yes | as above | GATED, not enrolled | yes |
| `deleteRecoveryPin` | `git.ts:6826` `const settlement = await this.beginBare(barePath, "recovery-pin-delete-ref", [pinRef]);` | pin delete | tryGitExit | `beginBare`, no `withLock` | n/a | unknown exit leaves record `running`; survives a parent crash and fails closed | COVERED | yes |
| `cleanupArchivedRecoveryPin` | `git.ts:7140` `... await this.beginBare(chosen.bare, "recovery-pin-archive-delete-ref", [ref])` | archived pin delete | spawnGit | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `cleanupRecoveryGeneration` | `git.ts:21168` `... await this.beginBare(barePath, "recovery-cleanup-delete-ref", [ref]);` | generation pin delete | `deleteRefSettled` `:19219` | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `reconcileOwedCandidates` | `git.ts:21217` `... await this.beginBare(barePath, "owed-reconcile-delete-ref", [candidate.pinRef]);` | owed pin delete | `deleteRefSettled` | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `cloneBare`, `disableAutoMaintenance` | `git.ts:18047` `["clone", "--bare", ...]`; `:5705-5706` `["config", "maintenance.auto", "false"]` / `["config", "gc.auto", "0"]` | bare creation, maintenance keys | runGit | `withLock` | no | as above | UNCOVERED | no |
| scratch and floor refs, `recovered-source` bundle ref | R1 inventory; not re-quoted | temporary refs | runGit | `withLock` | no | as above | UNCOVERED | no |

Writers outside the worker binary are not in the table and are invisible to every durable check: a human operator running git, and a runner-uid tool process in a `--shared` clone whose origin is the bare (`agent/src/git.ts:4006`, `["clone", "--shared", "--no-checkout", "--", barePath, clonePath]`). The worker issues no `gc`, `repack` or `maintenance` on the bare and disables auto-maintenance; a bare created before that reassert could still spawn a detached `maintenance --auto` (R1, not re-verified).

### 2.4 Key facts

- **Only four sites are durably enrolled**, all ref deletes (`beginBare` sweep: 4). [S]
- **Every journal and attribution writer is in-process only.** All 17 `writeRecovery` callers, `writeRecoveryOwner`, the attempts ledger, stopped and retained markers, tracking promote and repair, settlement pins (two with no lock at all) and owed receipts. [S]
- **Nothing sets pdeathsig** (sweep: 0). [S]
- **W and `beginBareMutation` first shipped after the newest stable tag.** Both first appear in cc125a82 (#3577). `git tag --contains cc125a82` lists only v0.86.0-rc.29 through v0.86.1-rc.4; the newest stable tag in this clone is v0.85.2 (`git tag -l 'v*' | grep -v -- -rc | sort -V | tail -1`). Verified against this clone's 99 tags; a tag absent from the clone is not covered. Every stable-release binary writes the bare with no W and no record. [S]
- **The authority records live outside the bare**, under `<dataDir>/.uzi-authority`. [S]
- **`PROCESS_INCARNATION` is never persisted on its own.** It appears only inside slot owners, the completion fence and `memoryPark.origin.authorityIncarnation` (`agent/src/git.ts:1823`). No per-bare record names the last writer of the bare's config. [S]

**Conclusion.** `assertBareClear` cannot exclude an older unenrolled writer. It proves only that no compatible process left a covered ref delete `running`. A stable-release binary, an orphan git child of a dead worker, an operator's git and a runner-uid tool process all write the bare's journal, attribution keys and refs without any record that the check could see. Any exclusion that restore relies on must therefore come from process-lifetime teardown, not from bare records.

## 3. Per-posture verdicts

### 3.1 What every posture shares

- **Process chain.** `agent/templates/base/Dockerfile:580`, `# dropped worker: uzi-entrypoint -> setpriv (drop) -> tini -> npm run start.`; the entrypoint `exec`s tini (`agent/templates/entrypoint.sh:241`, `exec "$TINI" -- "$@"`; root path `:1141`, `-- "$TINI" -- "$@"`); `agent/package.json:9`, `"start": "tsx src/main.ts",`. So tini (PID 1) runs npm, which runs tsx, which runs node. tini takes no flags. [S]
- **No respawn.** No in-container supervisor or self-relaunch was found; on a fatal error `agent/src/main.ts:1125`, `process.exitCode = 1;`. [S]
- **Teardown in practice.** If node dies, npm and tsx exit, tini exits, and the kernel kills every process left in the container's PID namespace before init is reaped (`prds/1860-pod-epoch-writer-exclusion.md:234`, "When a pid namespace's init dies, the kernel SIGKILLs every process in that namespace and its descendant namespaces, and init is not reaped until they are gone.") [I: source-read in #1860 plus a real-daemon corroboration]. That kills an orphan git child.
- **Weak links.**
  - tsx propagating node's exit to npm is **inferred**; tsx source was not read. [O]
  - A **hung tini** would keep the namespace alive. No successor then starts in that namespace, but nothing proves that bound. [O]
  - Whether memcg `oom.group` is set is **not read**. If the OOM victim is npm or tsx, tini still exits and the namespace is torn down; only a hung tini breaks that. [O]
  - `prds/1860-pod-epoch-writer-exclusion.md:370` says "A worker *process* restart inside a live container keeps run-agent children". This record found no path that restarts the worker process inside a live container, so that statement is not contradicted, and not proven impossible either.
- **Not verifiable at admission.** Teardown happening in practice is not the same as the successor being able to verify it. The worker cannot read any identity today that tells it the predecessor ran in a different PID namespace that is now dead. `PROCESS_INCARNATION` is per process (`agent/src/completion-authority.ts:97`). The pod UID (`controller/internal/kube/render.go:1065`, `{Name: "UZI_POD_UID", ValueFrom: ... FieldPath: "metadata.uid"}`) is the same across container restarts. R2 found no read of a container ID, PID-namespace inode or boot ID in `agent/src`. W proves only that no compatible worker is live now. The issue requires verifiable evidence, so a posture whose protection is teardown alone is **NOT PROVEN** at admission.

### 3.2 Verdicts

E2E column: phases 45 and 52 hard-kill the compose agent container (`e2e/phases/45-bounded-concurrency.sh:203`, `"${COMPOSE[@]}" kill -s KILL agent >/dev/null`, restart `:218`, `"${COMPOSE[@]}" up -d --wait agent >/dev/null`; `e2e/phases/52-api-outage-outbox.sh:929`, `rc=0; "${COMPOSE[@]}" kill -s SIGKILL agent >/dev/null 2>&1 || rc=$?`, restart `:931`, `"${COMPOSE[@]}" start agent`). Today neither phase exercises a saved-pin restore, because none ships; phase 45 asserts recovery by capture and fresh seed (`:215-216`, "#2512 verifies/captures the retained predecessor and seeds a fresh attempt"). The column states what a refusal would mean once restore exists.

| Posture | Teardown in practice | Admission-verifiable | Verdict | Class | Cites (quoted) | Owed evidence | Phases 45/52 if it refused |
|---|---|---|---|---|---|---|---|
| (a0) process-only death, in a container | yes, through tini exit and PID-namespace teardown | no | NOT PROVEN | S + I | chain above; `docker-compose.yml:347`, `# OOM is a SHARED-cgroup event: an over-budget run OOM-kills the whole container,`; `:352`, `mem_limit: ${AGENT_MEM_LIMIT:-4g}` | live OOM kill on the real image showing no pre-kill git PID at successor start; tsx exit propagation; `oom.group` | Same as (a): these phases kill the container, so they are (a0) and (a) at once. |
| (a0) process-only death, non-container `npm start` | no: no tini, no PID namespace of its own | no | **REFUSE** | S | `agent/README.md:33`, `npm start           # tsx src/main.ts  (needs the env vars below)`; `agent/src/config.ts:604`, `const dataDir = env.UZI_DATA_DIR?.trim() \|\| "/data";` | none: no mechanism exists to observe | None; E2E does not run this posture. |
| (a) compose, with and without `dind` | yes: no `restart:`, `init:` or `pid:` key (`grep -nE '^\s*(restart\|init\|pid):' docker-compose.yml`, 0 hits); `kill` then `start` gives a new PID namespace on the same `agentdata` volume; `dind` mounts no `agentdata` | no | NOT PROVEN | S + I | `docker-compose.yml:411`, `- agentdata:/data` (only mount); `:560-562`, "`dind` mounts NONE of agentdata/`/data`, agentnix/`/nix`, or the worker_token secret, so a container it launches can bind-mount nothing of the worker's"; `:537`, `privileged: true` (residual: privileged device-level reach, `adr/3153-runner-shared-pvc-subpath.md:59`) | an E2E assertion that no pre-kill git PID exists at re-registration; an identity the successor can check | Restore would never run after the kill. Both phases would keep today's capture and seed path, or park; any later assertion of a restore after the kill would fail. |
| (b) hosted Docker lane, worker-container restart, same pod UID | yes: per-container PID namespace; no `ShareProcessNamespace` or `HostPID` is rendered (`git grep -n 'ShareProcessNamespace\|HostPID' -- controller ':!*_test.go'`, 1 hit, the evidence check `controller/internal/kube/pod_epoch_session.go:381`, `return !p.Spec.HostPID && (p.Spec.ShareProcessNamespace == nil \|\| !*p.Spec.ShareProcessNamespace)`) | no: same pod UID, and the epoch never reopens (section 4) | NOT PROVEN | S + I | DinD mount inventory: `controller/internal/kube/render_dind.go:80-82`, "It is the ONLY data mount dind may have." / `return corev1.VolumeMount{Name: dataVolume, MountPath: dindWorkdirDir, SubPath: dindWorkdirSubPath}`, with `:72`, `dindWorkdirSubPath = "runner-shared"`; `:62`, "/data/repos, the agent home, /nix and the token/secret mounts stay UNSHARED with"; the bare is at `git.ts:3226`, `path.join(dataDir, "repos")`. So a container started by the inner dockerd cannot bind the bare. Worker mounts: `render.go:1269-1271`, `{Name: "data", MountPath: dataMountPath},` (whole PVC) | a real-node worker-container restart showing no pre-restart git PID, on containerd+runc and on CRI-O+crun (the CRI-O audit is partly INFERRED, `prds/3430-pod-epoch-crio-audit.md:162`, "So an orphaned init is reaped by host PID 1 (INFERRED)."); whether a memcg OOM can leave tini alive | None directly (compose only). |
| (c) hosted Docker lane, new pod epoch | graceful Recreate: yes. Pod delete, eviction or drain: not proven, the replacement can start while the old pod terminates. Force delete or node loss: operator residual | only for the first worker of the new pod, before admission, and only on the DinD lane (section 4) | NOT PROVEN for a later restore | S + I | `render.go:1029-1031`, "// Recreate, never RollingUpdate: the surge pod would Multi-Attach-deadlock" / `Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},`; `render.go:783`, `AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},`; `prds/1860-pod-epoch-writer-exclusion.md:248`, "A second pod on the same node can therefore mount the data or DinD-data PV."; dead-shim residual `prds/1860-pod-epoch-writer-exclusion.md:231`, "On this path no shim reaps init, and if `runc delete --force` failed, init may still be alive." | #3430 M3 shadow acceptance; cross-node RWO detach (inferred in #1860); force delete and node loss stay operator residuals | None directly. |
| (d) kube-native lane (no DinD) | in-pod restart: as (b). Pod replacement: Recreate plus RWO only | no: no epoch evidence is rendered | NOT PROVEN; REFUSE if restore must run without new evidence | S | `render.go:397-398`, `func withDinD(w protocol.DesiredWorker) bool {` / `return w.Docker && !w.Isolated`; the epoch env sits inside `render.go:1316`, `if withDinD(w) {`; `agent/src/main.ts:380`, `const epochEnabled = process.env.UZI_EPOCH_LOCK === "1";` | the same as (b) for restarts; a new evidence source for pod replacement | None directly. |

**Reading.** No posture is PROTECTED at admission today. Four postures are protected in practice by teardown, from source reading, in most cases. One posture, the non-container `npm start`, must refuse. The gap common to every posture is the lack of an identity the successor can verify (3.1).

## 4. Epoch-evidence reuse (#1860, #3430, #3431) and #3031

### 4.1 What the epoch proof certifies

- **Scope.** `prds/3430-pod-epoch-lock-shadow.md:142`, "**What the proof covers.** The immediate predecessor, any pod in between, and any concurrent worker process. It does **not** exclude every historical writer." It was designed for settled attempt trees against DinD, not for the bare. [I]
- **Needs a different, terminal predecessor pod UID.** `prds/3430-pod-epoch-lock-shadow.md:242`, "One pod UID with `status.phase ∈ {Failed, Succeeded}`."; `:245`, "an exit code that is ≥ 0 and not 137 or 255"; `:179`, "S re-reads the pod-entry record (module 2) and requires `current == S` and `previous == X`." [I]
- **DinD only.** `controller/internal/kube/render.go:397`, `func withDinD(w protocol.DesiredWorker) bool {`, gates the env block at `:1316-1318`, `if withDinD(w) {` / `env = append(env, corev1.EnvVar{Name: "UZI_EPOCH_LOCK", Value: "1"},`. `prds/3430-pod-epoch-lock-shadow.md:169`, "Compose and other lanes never lock." [S]
- **Does not cover (b), by design.** `prds/1860-pod-epoch-writer-exclusion.md:43`, "A worker container restart, an interrupted reclaim or any uncertainty never reopens it." [I]
- **Covers (c) only before admission, for the first worker.** `prds/3430-pod-epoch-lock-shadow.md:174`, "Candidate S takes a tentative `LOCK_EX|LOCK_NB` at the top of `main()`". [I]
- **EX ends before admission.** `prds/3430-pod-epoch-lock-shadow.md:179`, "Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission." EX cannot be re-taken later, so it cannot be held through a restore that runs at claim time. SH (`:173`) excludes a reclaimer's EX, not another SH holder or a non-participant. [I] #3431's plan keeps that order (`prds/3431-pod-epoch-reclaim.md:43`, "After all work settles, close EX and acquire SH boundedly with admission still closed"). [I]
- **No coverage of orphan children by the lock.** The epoch and W locks sit on Node's own descriptor; #3431 proposes descriptor inheritance only for reclaim's mutating children (`prds/3431-pod-epoch-reclaim.md:41`, "Every mutating child receives the reclaim lock descriptor until it ends"). [I]

### 4.2 Consumption ledger

- Volume-global ledger: `agent/templates/pod-epoch-init.cjs:84`, `ledger: path.join(root, '.uzi-epoch-consumed'),`. [S]
- Durability and order (R3, from `pod-epoch-init.cjs` `consume`; not re-read line by line): open, `flock` EX on the ledger inode, verify, append one 37-byte UUID record, `fsync`, verify, release. It runs before evaluation and before admission. An unsynced append is settled by the next process as `existing` (consumed for good). A torn ledger stops startup without admission. The ledger is never truncated or reset, and a consumed pod UID is never reusable. The evidence itself is not stored locally; only the consumption is. [I, R3 summary]
- Consequence for restore: one evaluation per pod UID, at startup. Any restore later in the pod's life, or after a container restart, has no fresh proof.

### 4.3 The accepted residual

- `prds/3431-pod-epoch-reclaim.md:11`, "It does not exclude every historical writer: an older pod's process may survive its termination and still write after a later evidenced epoch. The owner accepted that residual on 2026-10-24." [I]
- It was accepted for **deleting settled trees**, where a stray write does bounded damage. Nothing records it as accepted for **attribution and journal writes in a live bare**, which a restore would trust. A reuse of the epoch proof must not inherit the residual silently.
- **Owner question (for Gate A).** Does the 2026-10-24 older-pod-survivor residual extend to restore attribution in a live bare, or must restore close it (for example by refusing whenever more than one epoch separates the writer of the saved pin from the restorer)?

### 4.4 #3031 maintenance exclusion

- Same-namespace only: `adr/1197-transient-recovery-park.md:145-146`, "runs a deny-only process scan (worker-local, never signalling)". It cannot see another PID namespace, which is exactly where an older writer would live. [S]
- Producer disabled: `agent/src/recovery.ts:127`, `export const TERMINAL_ORIGIN_ADMISSION_ENABLED: boolean = false;`. [S]
- It names no journal writers. It proves worker idleness for the producer's boundary, not journal quiescence (`adr/1197-transient-recovery-park.md:159`, "Exclusive-idle provides temporal closure only"). [S]

### 4.5 Dependency and existing statements of the gap

- **Dependency.** The in-flight #3431 run b7abdf19 is reported to be changing the epoch contract [M]. `git grep b7abdf19` finds nothing, and no #3431 branch exists in this clone. Gate B must re-read `prds/3431-pod-epoch-reclaim.md` and `prds/3430-pod-epoch-lock-shadow.md` at its own base before reusing anything in 4.1 to 4.3.
- **Docs that already state the gap.**
  - `adr/1783-run-quiescence-and-attempt-clone-paths.md:1318-1320`, "An unresolved bare mutator (a pending `update-ref -d`, a detached destructive `rm` after owner death) blocks that repository's seeds and the exact footprint indefinitely. No process scan, PID absence or empty lock holder clears it."
  - `adr/1783-run-quiescence-and-attempt-clone-paths.md:1336-1337`, "Docker accepted-but-invisible creates (#1860) and unregistered or sibling writers are unchanged".
  - `specs/human.md:1414`, "Docker accepted-but-invisible creates (#1860), unregistered/sibling writers, Claude terminal proof, per-attempt cgroups (#1862) and reclaim/custody (#1858, #2746) remain open".

**Reuse verdict.** The epoch proof can be reused only for a restore that runs inside the startup EX window of a fresh pod on the DinD lane, before admission. It never covers (a), (b) or (d), and a later restore in (c). Anything else needs new evidence, which M2 designs.

## 5. Candidate exclusion designs

Pending M2.

## 6. Recommendation

Pending M2.

## 7. Owed evidence and acceptance

Pending M2.

## 8. Open questions for the owner

Pending M2.

## 9. Decision Log

Pending M2.
