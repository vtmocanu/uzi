# PRD #3831: Prove bare-writer exclusion before enabling saved-pin restore (Gate A design record)

**Status**: Draft, Gate A, awaiting maintainer review. Design only: no implementation is authorized by this record. M1 (sections 1 to 4: scope, writer inventory, per-posture verdicts, epoch-evidence reuse) and M2 (sections 5 to 9: design comparison, the PROPOSED contract, predecessor-source writers, Gate B obligations, owner questions and Decision Log) are written. Section 6 is a proposal for owner approval, not an approved contract. Every cite is `file:line` at base 7b0478c3 with the quoted line.

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
- **Saved-pin restore, defined.** In this record, "saved-pin restore" means the #3644 M2 to M4 feature: a successor worker process installs its own attribution in the shared bare from a saved restore pin while the predecessor's slot is still foreign-active and its source is retained.
- **Existing restore-point consumers are not that feature.** Two shipped paths already reuse a saved restore point [S]:
  - `agent/src/runner.ts:26130-26132`, in `recoverRetainedClaim`, reads `source.recovery.restoreTip` (`? source.recovery.restoreTip : undefined;`) and calls `verifiedRecoveryRestorePoint` (`agent/src/git.ts:9004`). That check, under `withLock`, re-reads the journal (`checkedRecovery`), requires `refs/uzi-recovery-episode/<run>/<tip>` to equal the tip, and requires the clone's tree to match it with no diff or untracked files (`git.ts:9011-9019`). On success the free capture skips a new capture and only publishes a checkpoint.
  - `agent/src/git.ts:8990`, `verifiedRetainedRecoveryRestorePoint`, called at `runner.ts:25055` and `:25096` in `enterRetainedCredentialSwitch`, runs the same check for every recorded source before a retained credential switch.
  - Both verify an existing pin; neither installs attribution for a successor over a foreign-active slot (read from what they do; not traced to source-use admission). Both trust journal state (`uzi-recovery.<branch>.clone`) and episode pins written by the unenrolled writers in 2.3 (`writeRecovery`, `recordRecoveryCapture`), so the 2.4 finding bears on them too. This record does not change them.
- **Restore stays disabled.** This record enables nothing. A symbol sweep for the feature finds nothing in shipped source: `git grep -n -i -E 'saved[-_ ]?pin|savedPin' -- agent/src ':!*.test.ts'`, 0 hits. A name sweep cannot prove that no equivalent path exists under another name; it shows only that none carries the feature's name. The #3644 M2 entry points are parked off-tree, on the branches below [M]. A sweep for a disabled-feature flag finds only the #3031 producer flag (`git grep -n -E '_ENABLED\s*(:\s*boolean)?\s*=\s*false' -- agent/src ':!*.test.ts'`, 1 hit, `agent/src/recovery.ts:127`).
- **No authority granted.** The record grants no capture, takeover, settlement, deletion or discard authority.
- **#3644 policy unchanged.** The #3644 verified-merge discard policy is unchanged.
- **The observed refusal is not fixed here.** A successor worker process meets `admitSourceUse` (`agent/src/completion-authority.ts:976`, `async admitSourceUse(`), which refuses a slot that another process owns: `agent/src/completion-authority.ts:993`, `throw new AuthorityRefusal("source is active under another worker incarnation");`. The incarnation is per process: `agent/src/completion-authority.ts:96-97`, `/** Random per process; a successor worker never shares it. */` / `const PROCESS_INCARNATION = randomUUID();`. The runner then fails the run as `preservation_failed` (`docs/run-recovery.md:138-141`, "on a different worker process (for example after a restart, when the slot is foreign-active) ... the run fails as `preservation_failed`"). The two `completion-authority.ts` cites are [S]; the `preservation_failed` outcome is doc-sourced from `docs/run-recovery.md`, and the runner path to it was not traced. This record does not change that refusal. It designs the writer-exclusion proof that #3644 M2 to M4 need before any restore may run.
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
| `git grep -n -E 'fs\.(writeFile\|rename\|rm\|unlink\|mkdir\|link\|open\|appendFile\|copyFile\|chmod\|chown)\(' -- agent/src/git.ts` | 75 | non-Git fs writes; cross-filtered on the bare below |
| `git grep -n -E 'process\.execPath\|\bfork\(\|relaunch\|respawn' -- agent/src ':!*.test.ts'` | 24, none `relaunch` or `respawn` | no self-relaunch by name; the 24 `process.execPath` hits were not read one by one |
| `git grep -n -E 'boot_id\|ns/pid\|self/cgroup\|containerId\|container_id\|mountinfo' -- agent/src ':!*.test.ts'` | 0 for `boot_id`, `ns/pid`, `containerId`, `container_id` | the `self/cgroup` and `mountinfo` hits (`stats.ts`, `worker-memory-reader.ts`, `env-probe.ts`, `run-procs.ts`, `run-quiescence.ts`) read memory and proc-mount visibility, not a predecessor identity |
| `git grep -n -E 'disableAutoMaintenance\(' -- agent/src ':!*.test.ts'` | 5 | definition `git.ts:5699`; bare callers `:3606` (warm `ensureClone`) and `:18063` (`cloneBare`) |

**fs-write cross-filter.** The 75 hits were classified by their path argument (read at each line, not traced through callers). 22 lines write, delete or directory-fsync inside a bare: `clearConflictingAncestorTrackingRefs` receipt unlink `git.ts:6019-6020`; `withCodeSnapshotFence` `:6338`, `:6346`, `:6353`; `rewriteLedgerAtomically` `:11805`, `:11811`, `:11818`, `:11819`, `:11822`, `:11824`, `:11828`; `fetchWorkflowTargetTip` `:14489`; `cloneBare` failure cleanup `:18066` (removes a bare that failed to clone); `ensureOwedDirectory`, `writeOwedFile`, `removeOwedFile` `:20290-20291`, `:20354`, `:20363-20364`, `:20369`, `:20374`, `:20379`. Four more open the bare's `config` or directory read-only for fsync (`:7776`, `:7778`, `:7837`, `:7839`). `:3594` creates the `repos/` parent. The other 48 are read-only opens (some inside a bare, for example `git.ts:11493` on the bare's `config` and `:20317` on an owed file) or target clones, runner scratch, the maintenance-import `outDir`, bundle files or `os.tmpdir()`.

The R1 research per-line classification of the 37 `update-ref` and 53 `config --local` lines into bare, clone-local and read was spot-checked on every row cited below, not re-derived line by line. Search reach: `agent/src` without tests, plus `agent/templates/entrypoint.sh` `migrate_tree` (row in 2.3). The rest of `agent/templates` (including `pod-epoch-init.cjs`, whose ledger sits at the data root, `pod-epoch-init.cjs:84`) was not swept for bare writes. `api/`, `web/`, `cli/` and the chart were not searched; the bare exists only on the worker data volume.

### 2.2 Common launch, lock and join facts [S]

- **Launch.** Bare git launches through, among others, `runGit` (`git.ts:19170`), `tryGit` (`:19925`), `tryGitExit` (`:19947`) and `spawnGit` (`:19337`, which spawns through `spawnCheckpointProcess`), plus direct `execScoped` calls (for example `git.ts:3480`, `:9056`). In `execScoped` (`:18945`) the default path is `agent/src/git.ts:18974`, `const pending = execFileAsync(command, args, { ...execOptions,`, and the maintenance branch is `:18968`, `const result = await this.execMeteredNative(command, args, options, operationSignal);`. The default timeout is `agent/src/git.ts:875`, `const GIT_TIMEOUT_MS = 10 * 60_000;`. A live parent joins its child on close before returning.
- **Lock.** `agent/src/git.ts:21439-21440`, `private withLock<T>(key: string, fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {` / `const prev = this.locks.get(key) ?? Promise.resolve();`, over `agent/src/git.ts:2896`, `private readonly locks = new Map<string, Promise<unknown>>();`. In-process only; nothing on disk; invisible to any other process.
- **Parent crash.** No launch path above sets pdeathsig (sweep: 0 across `agent/src`), so the conclusion holds for all of them. That a git child of a SIGKILLed or OOM-killed worker is then reparented and runs to completion inside its PID namespace is **inferred** from kernel behaviour, not measured here. A 10-minute `fetch` can keep writing refs and objects. A killed config writer can leave `config.lock`.
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
| `rewriteLedgerAtomically` tmp prune | `git.ts:11805` `if (n.startsWith(LEDGER_COMPACT_TMP_PREFIX)) await fs.rm(path.join(barePath, n), { force: true });` | deletes leftover compaction temp files in the bare root | Node fs | caller's `withLock` | no | in-process; no child | UNCOVERED | no (temp files git never reads) |
| `fetchWorkflowTargetTip` abort cleanup | `git.ts:14488-14489` `tempRef, \`${tempRef}.lock\`, \`logs/${tempRef}\`, \`logs/${tempRef}.lock\`,` / `].map((refFile) => fs.rm(path.join(barePath, refFile), { force: true })));`; method `:14457` | loose `refs/uzi-workflow-target/<uuid>` ref, its `.lock` and reflog, removed by hand when `update-ref -d` aborts | Node fs (after a runGit fetch and `update-ref -d`) | `withLock` | no | the preceding fetch shares 2.2; the `fs.rm` is in-process | UNCOVERED | no (temp ref) |
| `withCodeSnapshotFence`, `writeCodeSnapshotMetadata` | `git.ts:6335` `const dir = await this.ensureOwedDirectory(barePath, "uzi-code-snapshots");`; fence create `:6338`; metadata `:6402` `await this.writeOwedFile(barePath, this.codeSnapshotName(metadata.leadId), metadata, "uzi-code-snapshots");` | `<bare>/uzi-code-snapshots/*` fence and metadata files | Node fs | fence by `O_EXCL` create (`:6337`, "Exclusive creation is cross-process authority."); metadata writers not re-traced for `withLock` | no | in-process; a crash leaves the fence file, which fails closed (`:6337`) | UNCOVERED | no (cross-check snapshots, not recovery state) |
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
| `deleteRecoveryPin` | `git.ts:6826` `const settlement = await this.beginBare(barePath, "recovery-pin-delete-ref", [pinRef]);` | pin delete | tryGitExit | `beginBare`; takes no lock itself, callers may hold the bare lock (`git.ts:6810`, `// No lock is taken here: callers may already hold this bare's lock, as before.`) | n/a | unknown exit leaves record `running`; survives a parent crash and fails closed | COVERED | yes |
| `cleanupArchivedRecoveryPin` | `git.ts:7140` `... await this.beginBare(chosen.bare, "recovery-pin-archive-delete-ref", [ref])` | archived pin delete | spawnGit | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `cleanupRecoveryGeneration` | `git.ts:21168` `... await this.beginBare(barePath, "recovery-cleanup-delete-ref", [ref]);` | generation pin delete | `deleteRefSettled` `:19219` | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `reconcileOwedCandidates` | `git.ts:21217` `... await this.beginBare(barePath, "owed-reconcile-delete-ref", [candidate.pinRef]);` | owed pin delete | `deleteRefSettled` | `beginBare` + `withLock` | n/a | as above | COVERED | yes |
| `cloneBare`, `disableAutoMaintenance` | `git.ts:18047` `["clone", "--bare", ...]`; `:5705-5706` `["config", "maintenance.auto", "false"]` / `["config", "gc.auto", "0"]` | bare creation, maintenance keys | runGit | `withLock` | no | as above | UNCOVERED | no |
| scratch and floor refs, `recovered-source` bundle ref | `git.ts:3852` `const scratchRef = \`refs/uzi-publication-floor/${branch}\`;`; `:15932` `const scratchRef = \`refs/uzi-secret-scan-floor/${branch}\`;`; `:16280` `await this.runGit(barePath, ["update-ref", RECOVERY_BUNDLE_REF, h]);` (`:1158`, `"refs/heads/recovered-source"`) | temporary refs | runGit | `withLock` at `:15935`; not re-verified for the other two | no | as above | UNCOVERED | no |
| entrypoint `migrate_tree` (outside `agent/src`) | `agent/templates/entrypoint.sh:344` `"$CHOWN" -R "$owner" "$path"` (epoch path `:341`, per child), called `:350` `migrate_tree "$DATA_DIR" "$WORKER_OWNER"` | ownership of all of `/data`, `repos/` included; content unchanged | root shell, before node, root-started (uid-split) path only | sentinel file per owner (`:333-334`); no lock | no | runs in the successor container before W; a no-op once the sentinel exists | UNCOVERED | no (ownership only) |

Writers outside the worker binary are not in the table and are invisible to every durable check:
- a human operator running git;
- a runner-uid tool process in a `--shared` clone whose origin is the bare (`agent/src/git.ts:4006`, `["clone", "--shared", "--no-checkout", "--", barePath, clonePath]`). This applies only when uid-split is inactive (the single-uid start, `agent/templates/entrypoint.sh:190`, "single-uid non-root mode (PRD #58) — no A1 uid-split on this start"), where worker and runner are one uid. Under uid-split, `entrypoint.sh:374-375` keeps the bare worker-only: "WORKER bare cache repos/ stays worker-only (its config/hooks/refs are the B2 code-exec" / "surface — the runner must never write it)", and `:382`, "repos/ is deliberately NOT in this list."

**Detached maintenance.** The worker issues no `gc`, `repack` or `maintenance` on the bare. Its own git pins both keys inline (`git.ts:23221-23222`, `["maintenance.auto", "false"],` / `["gc.auto", "0"],`, in `gitEnv` `:23152`) and reasserts them in the bare's config on the warm path (`:3606`, `await this.disableAutoMaintenance(barePath);`). A detached `maintenance --auto` on the bare could therefore come only from a git outside `gitEnv` on a bare whose config lacks the keys, or from an older release. Older releases were not swept [O].

### 2.4 Key facts

- **Only four sites are durably enrolled**, all ref deletes (`beginBare` sweep: 4). [S]
- **Every journal and attribution writer is in-process only.** All 17 `writeRecovery` callers, `writeRecoveryOwner`, the attempts ledger, stopped and retained markers, tracking promote and repair, settlement pins (two with no lock at all) and owed receipts. [S]
- **Nothing sets pdeathsig** (sweep: 0). [S]
- **W and `beginBareMutation` first shipped after the newest stable tag.** Both first appear in cc125a82 (#3577). `git tag --contains cc125a82` lists only v0.86.0-rc.29 through v0.86.1-rc.4; the newest stable tag in this clone is v0.85.2 (`git tag -l 'v*' | grep -v -- -rc | sort -V | tail -1`). Verified against this clone's 99 tags; a tag absent from the clone is not covered. Every stable-release binary writes the bare with no W and no record. [S]
- **The authority records live outside the bare**, under `<dataDir>/.uzi-authority`. [S]
- **`PROCESS_INCARNATION` is never persisted on its own.** It appears only inside slot owners, the completion fence and `memoryPark.origin.authorityIncarnation` (`agent/src/git.ts:1823`). No per-bare record names the last writer of the bare's config. [S]

**Conclusion.** `assertBareClear` cannot exclude an older unenrolled writer. It proves only that no compatible process left a covered ref delete `running`. A stable-release binary, an orphan git child of a dead worker, an operator's git and, on a single-uid start, a runner-uid tool process all write the bare's journal, attribution keys and refs without any record that the check could see. Any exclusion that restore relies on must therefore come from process-lifetime teardown, not from bare records.

**Scope of this finding against the spec.** `specs/human.md:1414` treats "a per-source fence in the completing bare journal" as part of the durable authority that releases a #3404 completion source, and that fence's writer, `installCompletionSourceFence`, is UNCOVERED in 2.3. This record's finding bears on restore admission only. It does not itself change the #3404 rule, which stays the owner's decision. **Owner question (section 9, Q1):** does an unenrolled writer of the completion fence also weaken the #3404 release rule, and if so, is that tracked under #3404 rather than here?

## 3. Per-posture verdicts

### 3.1 What every posture shares

- **Process chain.** `agent/templates/base/Dockerfile:580`, `# dropped worker: uzi-entrypoint -> setpriv (drop) -> tini -> npm run start.`; the entrypoint `exec`s tini (`agent/templates/entrypoint.sh:241`, `exec "$TINI" -- "$@"`; root path `:1141`, `-- "$TINI" -- "$@"`); `agent/package.json:9`, `"start": "tsx src/main.ts",`. So tini (PID 1) runs npm, npm runs the script through `sh -c`, the shell runs tsx, and tsx runs node. tini takes no flags. The tini, npm and script cites are [S]; the `sh -c` step is npm's documented run-script behaviour, **inferred** (npm source not read).
- **No respawn.** No in-container supervisor or self-relaunch was found by name (2.1 sweep: no `relaunch` or `respawn`; the 24 `process.execPath` hits were not read one by one, so this is a name sweep, not a proof); on a fatal error `agent/src/main.ts:1125`, `process.exitCode = 1;`. [S]
- **Teardown in practice.** If node dies, the shell, npm and tsx exit, tini exits, and the kernel kills every process left in the container's PID namespace before init is reaped (`prds/1860-pod-epoch-writer-exclusion.md:234`, "When a pid namespace's init dies, the kernel SIGKILLs every process in that namespace and its descendant namespaces, and init is not reaped until they are gone.") [I: source-read in #1860 plus a real-daemon corroboration]. That kills an orphan git child.
- **Weak links.**
  - tsx propagating node's exit, and the `sh -c` shell and npm exiting when their child dies, are **inferred**; tsx and npm source were not read. [O]
  - A **hung tini** would keep the namespace alive. No successor then starts in that namespace, but nothing proves that bound. [O]
  - Whether memcg `oom.group` is set is **not read**. If the OOM victim is npm or tsx, tini still exits and the namespace is torn down; only a hung tini breaks that. [O]
  - `prds/1860-pod-epoch-writer-exclusion.md:370` says "A worker *process* restart inside a live container keeps run-agent children". This record found no path that restarts the worker process inside a live container, so that statement is not contradicted, and not proven impossible either.
- **Not verifiable at admission.** Teardown happening in practice is not the same as the successor being able to verify it. The worker cannot read any identity today that tells it the predecessor ran in a different PID namespace that is now dead. `PROCESS_INCARNATION` is per process (`agent/src/completion-authority.ts:97`). The pod UID (`controller/internal/kube/render.go:1065`, `{Name: "UZI_POD_UID", ValueFrom: ... FieldPath: "metadata.uid"}`) is the same across container restarts. The 2.1 sweep (`boot_id`, `ns/pid`, `containerId`, `container_id`: 0 hits in `agent/src` without tests) finds no read of a container ID, PID-namespace inode or boot ID by those names. W proves only that no compatible worker is live now. The issue requires verifiable evidence, so a posture whose protection is teardown alone is **NOT PROVEN** at admission.
- **NOT PROVEN means refuse.** Under the issue's bar, missing or unknown evidence retains the source and stops. So a NOT PROVEN posture means restore refuses (retain and stop) until the owed evidence for that posture exists. No verdict below permits restore today.

### 3.2 Verdicts

E2E column: phases 45 and 52 hard-kill the compose agent container (`e2e/phases/45-bounded-concurrency.sh:203`, `"${COMPOSE[@]}" kill -s KILL agent >/dev/null`, restart `:218`, `"${COMPOSE[@]}" up -d --wait agent >/dev/null`; `e2e/phases/52-api-outage-outbox.sh:929`, `rc=0; "${COMPOSE[@]}" kill -s SIGKILL agent >/dev/null 2>&1 || rc=$?`, restart `:931`, `"${COMPOSE[@]}" start agent`). Today neither phase exercises a saved-pin restore, because none ships; phase 45 asserts recovery by capture and fresh seed (`:215-216`, "#2512 verifies/captures the retained predecessor and seeds a fresh attempt"). The column states what a refusal would mean once restore exists.

| Posture | Teardown in practice | Admission-verifiable | Verdict | Class | Cites (quoted) | Owed evidence | Phases 45/52 if it refused |
|---|---|---|---|---|---|---|---|
| (a0) process-only death, in a container | yes, through tini exit and PID-namespace teardown | no | NOT PROVEN | S + I | chain above; `docker-compose.yml:347`, `# OOM is a SHARED-cgroup event: an over-budget run OOM-kills the whole container,`; `:352`, `mem_limit: ${AGENT_MEM_LIMIT:-4g}` | live OOM kill on the real image showing no pre-kill git PID at successor start; tsx exit propagation; `oom.group` | Same as (a): these phases kill the container, so they are (a0) and (a) at once. |
| (a0) process-only death, non-container `npm start` | no: no tini, no PID namespace of its own | no | **REFUSE** | S | `agent/README.md:33`, `npm start           # tsx src/main.ts  (needs the env vars below)`; `agent/src/config.ts:604`, `const dataDir = env.UZI_DATA_DIR?.trim() \|\| "/data";` | none: no mechanism exists to observe | None; E2E does not run this posture. |
| (a) compose, with and without `dind` | yes: no `restart:`, `init:` or `pid:` key (`grep -nE '^\s*(restart\|init\|pid):' docker-compose.yml`, 0 hits); that `kill` then `start` gives a new PID namespace on the same `agentdata` volume is **inferred** from Docker behaviour; `dind` mounts no `agentdata` | no | NOT PROVEN | S + I | `docker-compose.yml:411`, `- agentdata:/data` (the agent's only `agentdata` mount); `:560-562`, "`dind` mounts NONE of agentdata/`/data`, agentnix/`/nix`, or the worker_token secret, so a container it launches can bind-mount nothing of the worker's"; `:537`, `privileged: true` (residual: privileged device-level reach, `adr/3153-runner-shared-pvc-subpath.md:59`) | an E2E assertion that no pre-kill git PID exists at re-registration; an identity the successor can check | Restore would never run after the kill. Both phases would keep today's capture and seed path, or park; any later assertion of a restore after the kill would fail. |
| (b) hosted Docker lane, worker-container restart, same pod UID | yes: per-container PID namespace; no `ShareProcessNamespace` or `HostPID` is rendered (`git grep -n 'ShareProcessNamespace\|HostPID' -- controller ':!*_test.go'`, 1 hit, the evidence check `controller/internal/kube/pod_epoch_session.go:381`, `return !p.Spec.HostPID && (p.Spec.ShareProcessNamespace == nil \|\| !*p.Spec.ShareProcessNamespace)`) | no: same pod UID, and the epoch never reopens (section 4) | NOT PROVEN | S + I | DinD mount inventory: `controller/internal/kube/render_dind.go:80-82`, "It is the ONLY data mount dind may have." / `return corev1.VolumeMount{Name: dataVolume, MountPath: dindWorkdirDir, SubPath: dindWorkdirSubPath}`, with `:72`, `dindWorkdirSubPath = "runner-shared"`; `:62`, "/data/repos, the agent home, /nix and the token/secret mounts stay UNSHARED with"; the bare is at `git.ts:3226`, `path.join(dataDir, "repos")`. So, as rendered, a container started by the inner dockerd has no mount of the bare. Two residuals remain: in single-uid mode the worker uid can swap the subPath entry so a later dind restart mounts more of the volume (`render_dind.go:63-66`, "Residual (ADR-3153): kubelet re-resolves the subPath on every container start, so in single-uid mode the worker uid, which owns /data, can swap the entry and make a later dind restart mount more of the volume."), and a rootful daemon's device-level reach is not bounded by the mount view (`adr/3153-runner-shared-pvc-subpath.md:59`, "It does not bound a rootful daemon's device-level reach"). Containers started by the inner dockerd are outside the worker container's PID namespace, so a worker-container restart does not kill them (**inferred** from the namespace layout); only their mounts keep them away from the bare. Worker mounts: `render.go:1269-1271`, `{Name: "data", MountPath: dataMountPath},` (whole PVC) | a real-node worker-container restart showing no pre-restart git PID, on containerd+runc and on CRI-O+crun (the CRI-O audit is partly INFERRED, `prds/3430-pod-epoch-crio-audit.md:162`, "So an orphaned init is reaped by host PID 1 (INFERRED)."); whether a memcg OOM can leave tini alive | None directly (compose only). |
| (c) hosted Docker lane, new pod epoch | graceful Recreate: yes. Pod delete, eviction or drain: not proven, the replacement can start while the old pod terminates. Force delete or node loss: operator residual | only for the first worker of the new pod, before admission, only on the DinD lane, and only if the owner extends the 4.3 residual to restore attribution (section 4) | NOT PROVEN for a later restore | S + I | `render.go:1029-1031`, "// Recreate, never RollingUpdate: the surge pod would Multi-Attach-deadlock" / `Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},`; `render.go:783`, `AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},`; `prds/1860-pod-epoch-writer-exclusion.md:248`, "A second pod on the same node can therefore mount the data or DinD-data PV."; dead-shim residual `prds/1860-pod-epoch-writer-exclusion.md:231`, "On this path no shim reaps init, and if `runc delete --force` failed, init may still be alive." | #3430 M3 shadow acceptance; cross-node RWO detach (inferred in #1860); force delete and node loss stay operator residuals | None directly. |
| (d) kube-native lane (no DinD) | in-pod restart: as (b). Pod replacement: Recreate plus RWO only | no: no epoch evidence is rendered | NOT PROVEN | S | `render.go:397-398`, `func withDinD(w protocol.DesiredWorker) bool {` / `return w.Docker && !w.Isolated`; the epoch env sits inside `render.go:1316`, `if withDinD(w) {`; `agent/src/main.ts:380`, `const epochEnabled = process.env.UZI_EPOCH_LOCK === "1";` | the same as (b) for restarts; a new evidence source for pod replacement | None directly. |

**Reading.** Every posture refuses today: none is PROTECTED at admission, and NOT PROVEN means retain and stop (3.1). The rows differ only in what could lift the refusal:
- **Protected in practice by teardown, from source reading plus inferred runtime behaviour:** (a0) in a container, (a) compose, (b) the hosted Docker lane's worker-container restart, (d) in-pod restart (as (b)), and (c) only on a graceful Recreate. Each still owes the evidence in its row before restore could run.
- **Not protected in practice:** (c) on pod delete, eviction or drain, force delete or node loss, and (d) on pod replacement (Recreate plus RWO only).
- **No route to evidence at all:** (a0) non-container `npm start`, which has no tini and no PID namespace of its own.

The gap common to every posture is the lack of an identity the successor can verify (3.1).

## 4. Epoch-evidence reuse (#1860, #3430, #3431) and #3031

### 4.1 What the epoch proof certifies

- **Scope.** `prds/3430-pod-epoch-lock-shadow.md:142`, "**What the proof covers.** The immediate predecessor, any pod in between, and any concurrent worker process. It does **not** exclude every historical writer." It was designed for settled attempt trees against DinD, not for the bare. [I]
- **Needs a different, terminal predecessor pod UID.** `prds/3430-pod-epoch-lock-shadow.md:242`, "One pod UID with `status.phase ∈ {Failed, Succeeded}`."; `:245`, "an exit code that is ≥ 0 and not 137 or 255"; `:179`, "S re-reads the pod-entry record (module 2) and requires `current == S` and `previous == X`." [I]
- **DinD only.** `controller/internal/kube/render.go:397`, `func withDinD(w protocol.DesiredWorker) bool {`, gates the env block at `:1316-1318`, `if withDinD(w) {` / `env = append(env, corev1.EnvVar{Name: "UZI_EPOCH_LOCK", Value: "1"},`. `prds/3430-pod-epoch-lock-shadow.md:169`, "Compose and other lanes never lock." [S]
- **Does not cover (b), by design.** `prds/1860-pod-epoch-writer-exclusion.md:43`, "A worker container restart, an interrupted reclaim or any uncertainty never reopens it." [I]
- **Covers (c) only before admission, for the first worker.** `prds/3430-pod-epoch-lock-shadow.md:174`, "Candidate S takes a tentative `LOCK_EX|LOCK_NB` at the top of `main()`". [I]
- **EX ends before admission.** `prds/3430-pod-epoch-lock-shadow.md:179`, "Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission." [I] The ownership handle offers no way back to EX: `agent/templates/pod-epoch-init.d.cts:15-17`, `/** Admission stays closed until this resolves; closes EX before opening SH. */` / `reacquireShared(): Promise<void>;` / `close(): void;`, with no SH-to-EX member in `PodEpochOwnership` (`:7-18`) [S]. That EX therefore cannot be re-taken later, and so cannot be held through a restore that runs at claim time, is **inferred** from that API. SH (`prds/3430-pod-epoch-lock-shadow.md:173`) excludes a reclaimer's EX, not another SH holder or a non-participant. [I] #3431's plan keeps that order (`prds/3431-pod-epoch-reclaim.md:43`, "After all work settles, close EX and acquire SH boundedly with admission still closed"). [I]
- **No coverage of orphan children by the lock.** The epoch and W locks sit on Node's own descriptor; #3431 proposes descriptor inheritance only for reclaim's mutating children (`prds/3431-pod-epoch-reclaim.md:41`, "Every mutating child receives the reclaim lock descriptor until it ends"). [I]

### 4.2 Consumption ledger

- Volume-global ledger: `agent/templates/pod-epoch-init.cjs:84`, `ledger: path.join(root, '.uzi-epoch-consumed'),`. [S]
- Durability and order (R3, from `pod-epoch-init.cjs` `consume`; not re-read line by line): open, `flock` EX on the ledger inode, verify, append one 37-byte UUID record, `fsync`, verify, release. It runs before evaluation and before admission. An unsynced append is settled by the next process as `existing` (consumed for good). A torn ledger stops startup without admission. The ledger is never truncated or reset, and a consumed pod UID is never reusable. The evidence itself is not stored locally; only the consumption is. [I, R3 summary]
- Consequence for restore: one evaluation per pod UID, at startup. Any restore later in the pod's life, or after a container restart, has no fresh proof.

### 4.3 The accepted residual

- `prds/3431-pod-epoch-reclaim.md:11`, "It does not exclude every historical writer: an older pod's process may survive its termination and still write after a later evidenced epoch. The owner accepted that residual on 2026-10-24." [I]
- It was accepted for **deleting settled trees**, where a stray write does bounded damage. Nothing records it as accepted for **attribution and journal writes in a live bare**, which a restore would trust. Search: `git grep -n '2026-10-24' -- prds adr specs docs ':!prds/3831-bare-writer-exclusion.md'` gives 121 lines; filtering them with `grep -i -E 'survivor|historical writer|older.pod'` leaves five: `prds/3430-pod-epoch-lock-shadow.md:142`, `:450` and `prds/3431-pod-epoch-reclaim.md:11`, all about the epoch proof for reclaim, plus `prds/done/1232-completion-interlock-integration.md:5` and `:175`, where "survivor" means a mutation-test survivor and is unrelated. A prose search can miss a differently worded acceptance. A reuse of the epoch proof must not inherit the residual silently.
- **Owner question (for Gate A; section 9, Q2).** Does the 2026-10-24 older-pod-survivor residual extend to restore attribution in a live bare, or must restore close it? Refusing whenever more than one epoch separates the writer of the saved pin from the restorer does not close it; Q2 (9.1) concludes that the real choice is between accepting the residual and refusing restore on every lane.

### 4.4 #3031 maintenance exclusion

- Same-namespace only: `adr/1197-transient-recovery-park.md:145-146`, "runs a deny-only process scan (worker-local, never signalling)" [I]. That it cannot see another PID namespace, which is exactly where an older writer would live, is **inferred** from "worker-local" and kernel PID-namespace behaviour; the ADR does not say it.
- Producer disabled: `agent/src/recovery.ts:127`, `export const TERMINAL_ORIGIN_ADMISSION_ENABLED: boolean = false;`. [S]
- It names no journal writers. It proves worker idleness for the producer's boundary, not journal quiescence (`adr/1197-transient-recovery-park.md:159`, "Exclusive-idle provides temporal closure only"). [S]

### 4.5 Dependency and existing statements of the gap

- **Dependency.** The in-flight #3431 run b7abdf19 is reported to be changing the epoch contract [M]. `git grep b7abdf19` finds nothing, and no #3431 branch exists in this clone. Gate B must re-read `prds/3431-pod-epoch-reclaim.md` and `prds/3430-pod-epoch-lock-shadow.md` at its own base before reusing anything in 4.1 to 4.3.
- **Docs that already state the gap.**
  - `adr/1783-run-quiescence-and-attempt-clone-paths.md:1318-1320`, "An unresolved bare mutator (a pending `update-ref -d`, a detached destructive `rm` after owner death) blocks that repository's seeds and the exact footprint indefinitely. No process scan, PID absence or empty lock holder clears it."
  - `adr/1783-run-quiescence-and-attempt-clone-paths.md:1336-1337`, "Docker accepted-but-invisible creates (#1860) and unregistered or sibling writers are unchanged".
  - `specs/human.md:1414`, "Docker accepted-but-invisible creates (#1860), unregistered/sibling writers, Claude terminal proof, per-attempt cgroups (#1862) and reclaim/custody (#1858, #2746) remain open".

**Reuse verdict.** The epoch proof can be reused at most for a restore that runs inside the startup EX window of a fresh pod on the DinD lane, before admission, and only if the owner extends the 2026-10-24 older-pod-survivor residual to restore attribution (4.3); otherwise not at all. It never covers (a), (b) or (d), nor, as shipped, a later restore in (c); section 6 adds a persisted record for this. Anything else needs new evidence, which sections 5 and 6 design.

This record's findings bear on restore admission only; they do not change the #3404 completion-source rule in `specs/human.md:1414` (see the owner question at the end of 2.4).

## 5. Smallest-design comparison

**Bar.** Each option is judged against:
- the 6(a) invariant;
- the postures of section 3;
- legacy writers. Every stable-release binary and every build before cc125a82 writes the bare with no W and no record (2.4). A build without this design's new record also leaves no trace of itself.

The options are taken from smallest to largest. A larger option is considered only after each smaller one is shown to fail, or to hold only in part. By size the order is (ii), (i), the boundary-stamp variant of (i), (iii), (iv).

### 5.1 (ii) Reuse #3031's bounded writer exclusion (no new state)

- **Proves.** Inside the worker's own PID namespace, no process outside an admitted set exists while the producer's boundary is held. The scan admits the worker, its ancestors, the subtrees the producer slot recorded, zombies and a bootstrap set, and refuses on every other process and, when Docker is wired, on any running container (`adr/1197-transient-recovery-park.md:145-150`, "That scan admits only the worker, its ancestors, the subtrees" ... "When Docker is wired, any running container refuses."). The worker is idle behind its closed claim gate (4.4). [I]
- **Postures.** None for an older writer. An older writer lives in another PID namespace: an earlier container, possibly surviving, or another pod. A worker-local scan cannot see it (4.4, inferred). In (a0) in a container, the scan can at most observe that no orphan is visible in the successor's own namespace.
- **Invariant.** Neither branch. It offers PID absence from one or a few scans. The issue body's Gate A section names "Repeated empty snapshots, PID absence" as not replacement authority and says "temporary inability at one observation is insufficient" (quoted in full in 6(a)). Its hold ends with the producer's boundary, not with restore's later authoritative use.
- **Persisted state.** None.
- **Legacy.** Blind to every writer outside its namespace, of any build.
- **Cost.** Reuse only. The producer is disabled: `agent/src/recovery.ts:127`, `export const TERMINAL_ORIGIN_ADMISSION_ENABLED: boolean = false;`. [S]
- **Verdict. Reject**, not a component.

### 5.2 (i) Recorded incarnation boundary plus posture predicate, reusing #3430/#3431 as shipped

- **Proves.** At a fresh pod's first worker start, before admission: the immediate predecessor pod X is terminal, with every container exited under the allowlist; no pod lies between; no concurrent participant holds SH (4.1). [I] A terminal pod never runs again, so "X is terminal" is a permanent fact, not a held boundary.
- **Postures.**
  - (c): only the first worker, only before admission (4.1).
  - (b): nothing. The pod UID is unchanged and the epoch never reopens (`prds/1860-pod-epoch-writer-exclusion.md:43`, "A worker container restart, an interrupted reclaim or any uncertainty never reopens it."). [I]
  - (a), (a0), (d): no evidence is rendered (3.2).
- **Invariant.** Branch 1 for X's processes, subject to the older-pod survivor residual (4.3). As shipped it is unusable at claim time:
  - Only the consumption is stored, never the evidence (4.2).
  - The EX under which the fact was established ends before admission (`prds/3430-pod-epoch-lock-shadow.md:179`, "Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission."). [I]
  - The design must not treat that EX hold as a boundary. The approved plan lists an epoch hold that ends before admission as non-evidence (6(a)).
- **Persisted state.** As shipped, only the volume-global consumption ledger (4.2).
- **Legacy.** Build-independent: the evidence is kubelet-reported pod status, so X's processes are covered whatever build X ran. This is **inferred** from where the evidence comes from.
- **Cost.** Reuse only.
- **Verdict. Holds in part. Component.** The pod-terminal fact is reused (section 6) once it is persisted and attributed (5.3). On its own, (i) covers neither (b) nor any restore after admission.

### 5.3 A per-process boundary stamp (variant of (i)), tested

**The hypothesis as given:**
- Each worker process records a durable stamp once at startup, under W, before any bare writer can spawn.
- The stamp binds `PROCESS_INCARNATION` to the process's PID-namespace inode, boot ID and container or pod identity.
- At restore admission, consider every stamp recorded since a stamp-less build last touched the bare. The successor S requires each stamp to be either its own, or to name a namespace that is provably gone. A namespace is gone if it has a different boot ID, or if it differs from S's namespace and its node process is dead (S holds W) in a supported container posture.
- A bare written by a stamp-less build is ineligible. A non-container posture refuses.

**Checks.**

1. **Namespace inode reuse.** The kernel's proc inode allocator hands freed namespace inode numbers out again, so a number is unique only among live namespaces. This is **inferred** from kernel behaviour; the kernel source was not read here.
   - **Equal inode, same boot.** "Same namespace" and "reused after death" cannot be told apart, so S must refuse. The (a0) same-container respawn looks exactly like this: same pod UID, same boot ID, same inode, and P's orphans share S's namespace. Refusal is the right outcome there.
   - **Different inode, same boot.** This proves a different namespace. It does not by itself prove that namespace is dead.
   - **Proposed strengthening (future (b) only, 6(g)).** Also record PID 1's start time as seen inside the namespace (clock ticks since boot). That a PID namespace has one init for its whole life, and that a namespace inode number is reused only after its namespace is gone, are both **inferred** from kernel behaviour and owed (8.2). If they hold, the pair (inode, init start) names one namespace instance within a boot, and an equal inode with a later init start shows the earlier namespace was freed. Two namespaces whose inits start in the same clock tick and share a reused inode cannot be told apart, so an equal pair must refuse. That PID 1's start time is readable inside the container is also **inferred** and owed (8.2). The interim contract (6(f)) does not use these fields.
2. **W proves release, not death.** W is a kernel lock on Node's own open file description: `agent/src/authority-lock.ts:12`; `:351`, "Strong references keep the descriptors (and so the kernel locks) alive until exit."; `:364`, "The returned capability has no release." [S]
   - S holding W proves only that no compatible process on this kernel still holds it. That implies P's Node process exited on this kernel.
   - It says nothing about P's git children (2.2), about a process on another kernel, or about a stamp-less build, which never takes W.
   - The issue lists "a released worker lock" as non-evidence. **This part fails.** The death argument must instead come from namespace teardown: container-instance ordering within one pod, and pod-terminal evidence across pods.
3. **DinD-started containers.** Containers started by the inner dockerd are outside the worker's PID namespace and survive a worker-container restart (3.2 (b), inferred). No stamp names them and the teardown argument does not reach them. As rendered they cannot reach the bare (`controller/internal/kube/render_dind.go:80-82`). The two residuals from 3.2 (b) remain:
   - **The single-uid subPath swap.** It is closed under uid-split (`render_dind.go:65-66`, "That uid already holds all of /data;" / "under uid-split no untrusted identity can write /data."). [S]
   - **A rootful daemon's device-level reach.** This needs owner acceptance (Q7).

   These containers do reach the predecessor's source clones (section 7).
4. **Runner-uid processes in single-uid mode.** P's runner-uid processes live in P's container PID namespace and die with it, so as older writers they are covered by the same teardown.
   - Single-uid adds check 3's subPath swap. Through it, a DinD-started container can reach the bare, but only one started after a later dind restart, because kubelet re-resolves the subPath at container start (`render_dind.go:63-65`, "kubelet re-resolves the subPath on every container start," / "so in single-uid mode the worker uid, which owns /data, can swap the entry and make" / "a later dind restart mount more of the volume."). [S]
   - On the hosted lane the default is single-uid, because uid-split is opt-in: `controller/internal/kube/render.go:573-575`, "UIDSplit opts into the Codex uid-split worker profile (PRD #1493 M1). When true the" / "worker + seed-nix containers start as root ..."; the controller default is off (`controller/internal/config/config.go:198-199`, "WorkerUIDSplit opts the fleet into the Codex uid-split worker profile" / "(UZI_WORKER_UID_SPLIT, default false)."). [S]
   - So requiring uid-split refuses the default hosted fleet (Q6).
5. **Permanently unable, or a held boundary?** Once a PID namespace is gone, its processes are dead and cannot return. That is branch 1, with no held boundary. It holds only where the namespace's end is shown by evidence, not by inference.
   - In-pod restart ordering carries the dead-shim residual: `prds/1860-pod-epoch-writer-exclusion.md:231`, "On this path no shim reaps init, and if `runc delete --force` failed, init may still be alive." [I]
   - Pod evidence closes that residual for pod replacement only, by refusing exit codes 137 and 255.
   - An OOM-killed worker container is expected to report exit 137 too (**inferred** from SIGKILL's exit status; not measured, owed in 8.2), so that rule cannot be reused unchanged for in-pod restarts (Q4).
6. **The legacy rule is not computable.** "Every stamp since the bare was last touched by a stamp-less build" needs the time of the last stamp-less write. A stamp-less build leaves no trace, so a rollback followed by a roll-forward is invisible in the stamp set. **This part fails.**
   - Teardown evidence, however, is build-independent. A legacy process in an earlier container of S's pod, or in the evidenced predecessor pod X, is dead whatever it ran.
   - A legacy process cannot share S's container instance through the image, because one container instance runs one image. A different binary exec'd or copied into the live container (by an operator or a runner-uid process) can, and is the operator residual of Q8.
   - **The rule becomes:** a missing record for P refuses. Every other older writer is covered only by build-independent teardown, never by enumerating stamps.
7. **A different boot ID.**
   - **Within one pod UID,** it means the node rebooted, because a pod stays on one node for its life. This is **inferred** from Kubernetes scheduling semantics. Every process of the earlier boot is then dead.
   - **Across pod UIDs,** it proves nothing: it may be another node, still running. That is the cross-node RWO residual (`prds/1860-pod-epoch-writer-exclusion.md:248`, "Cross-node reuse waits for detach ... *inferred*, not traced to the end."). [I]
   - Across pods, only pod-terminal evidence counts.
8. **Is it smaller than (iv)?** Yes.
   - The stamp design is two write-once records plus one evaluator at a single admission point.
   - (iv) enrolls and joins every one of the 23 restore-relevant UNCOVERED rows and the 3 GATED rows in 2.3 (counted on this file with `grep -c -E '\| UNCOVERED \| yes \|$'` and `grep -c -E '\| GATED, not enrolled \| yes \|$'`, which anchor on the row end so this sentence does not match itself). Each of them leaves a blocking record when its parent crashes (5.5).

**Verdict on the hypothesis: holds in part; adopted with three corrections and one narrowing.**
1. W is dropped as death evidence.
2. The stamp only locates P (its pod UID). Death comes from persisted pod-terminal evidence for the predecessor pod X.
3. The uncomputable "since the last stamp-less write" rule is replaced by two rules: P's record must exist, and every other older writer is covered by build-independent teardown evidence.

**Narrowing.** Container-instance ordering inside one pod is not evidence today: an earlier instance of S's own pod may have died on the dead-shim path (check 5) with a delayed git child, and kubelet keeps only the last termination of each container (6(g)). So the interim contract admits only the worker incarnation that wrote S's pod record (the first worker instance of pod S) restoring over a P that ran in X. The in-pod restart (b) and the boot-ID fallback (b′) are NOT ELIGIBLE; the namespace identity fields (inode, init start, boot ID) move to the future-(b) path in 6(g). Any posture outside a uid-split hosted Docker lane refuses.

### 5.4 (iii) Isolating restore mutations from old writers

- **Proves.** Only that the successor's own new writes land where an older writer cannot reach. Two such places exist:
  - The authority store: 0700, MAC'd, outside the bare, and unknown to builds before cc125a82 (`agent/src/completion-authority.ts:25-29`). [S]
  - A successor-private bare.
- **Postures.** None gained.
- **Invariant.** Not met.
  - Stale `uzi-recovery.<branch>.*` writes still land on the predecessor's journal keys that restore reads: `agent/src/git.ts:8691`, `await this.runGit(barePath, ["config", "--local", recoveryCaptureKey(branch), value]);`.
  - #3644's contracts are reported to bind to state that lives in the bare [M] (the #3644 branches are off-tree, section 1). The shipped state they would read is in the bare [S]: the episode pin (`git.ts:9145`, `await this.runGit(barePath, ["update-ref", "--no-deref", \`refs/uzi-recovery-episode/${journal.runId}/${tip}\`, tip]);`) and `restoreTip` (`git.ts:9157`, `await this.writeRecovery(barePath, branch, { ...journal, restoreTip: tip,`); and the retained source clone, whose objects resolve through the bare (`git.ts:4006`, `["clone", "--shared", "--no-checkout", "--", barePath, clonePath]`). The spec places the per-source fence in the bare journal (`specs/human.md:1414`, a spec statement, not source).
  - Copying that state out at admission is one observation of state that a live writer may be part-way through changing, for example a `memoryPark` draft (2.3) or a hand-made `config.lock` before rename (`git.ts:11822`, `await fs.rename(lockPath, cfgPath);`).
- **Persisted state.** A second store holding copies of the journal, pins and objects.
- **Legacy.** Legacy writers still write the predecessor's keys.
- **Crash window.** A crash mid copy-out leaves a partial private copy that must be discarded, while the predecessor's keys stay writable throughout; the option adds a window and closes none.
- **Cost.** Every #3644 read and any path that reads the bare's recovery state would have to be re-homed. Large.
- **Verdict. Reject**, and not needed as a component. Under the chosen design no older writer is alive, so successor attribution can stay in the bare under #3644's contract.

### 5.5 (iv) Durable pre-launch enrollment of every relevant bare writer (largest)

- **Proves.** Every enrolled write either settled after a joined child, or left a `running` record that `assertBareClear` sees (`agent/src/authority-runtime.ts:1056-1057`). [S]
- **Postures.** All, but only for compatible builds.
- **Invariant.** Neither branch on its own.
  - A dead parent's `running` record cannot be released without proof that its orphan is dead. That proof is exactly what (i′) supplies.
  - Without that proof the record blocks indefinitely: `adr/1783-run-quiescence-and-attempt-clone-paths.md:1318-1320`, "No process scan, PID absence or empty lock holder clears it." [I]
  - A crash or OOM is exactly when restore is wanted, so (iv) alone turns the target case into a permanent block.
- **Persisted state.** A pending record per write. Each slot write already costs a head-shard write, so four barriers in all (`completion-authority.ts:38-41`, "each slot write costs two extra barriers (a" / "file fsync and a directory fsync) on top of the slot's own two."). [S] These records would sit on paths that write the journal many times per run.
- **Legacy.** Recording only new writers leaves every stable-release writer and every writer before cc125a82 unrecorded. A legacy rule is still needed, and that rule is (i′)'s evidence.
- **Crash window.** Every enrolled write gains a window between its `running` record and joined settlement; a parent crash there leaves a record that blocks the bare until orphan death is proved, which (iv) cannot supply.
- **Moving parts.** 23 restore-relevant UNCOVERED rows, 3 GATED rows, joined settlement for unknown exits, and recovery of every crash window.
- **Verdict. Reject**, both standalone and as a component. Once (i′) holds, (iv) adds blocking and no protection.

### 5.6 Selection

Why each alternative falls short:
- (ii) offers only non-evidence.
- (i) as shipped covers one posture, and only at startup.
- The hypothesis as stated fails on W and on the legacy rule.
- (iii) leaves the predecessor's keys writable.
- (iv) cannot release a dead parent's record and does not reach legacy writers.

**Chosen: (i′)**, which is (i) plus the corrected hypothesis. It has three parts:
- a per-process boundary record (incarnation and pod UID);
- a per-pod record that persists the attested predecessor-terminal fact and names the worker incarnation that wrote it;
- a finite posture predicate.

It is the smallest combination that closes the predecessor-pod case (c), for the first worker instance of the new pod. The in-pod restart (b) and the boot-ID fallback (b′) are NOT ELIGIBLE in this interim contract (5.3, narrowing); 6(g) keeps their analysis as the path to future eligibility. Every other posture refuses.

## 6. Chosen contract (PROPOSED, not approved)

This section is a proposed contract for owner approval. No implementation is authorized.

### 6(a) Invariant and branch

The invariant, verbatim from the issue (issue #3831 body, Gate A section): "Before successor mutation starts, every relevant older writer must be permanently unable to commit stale writes, or a continuously held exclusion/isolation boundary must prevent those writes throughout the protected operation and its subsequent authoritative use."

- **Branch taken: the first ("permanently unable"), for one case only.** The successor S is the worker incarnation that wrote S's pod record, that is the first worker instance of pod S that consumed the epoch, and P ran in the evidenced terminal predecessor pod X. Then:
  - P's processes, and those of X's other containers, ran in PID namespaces of X, whose terminal evidence comes from #3430/#3431 (4.1). That evidence certifies the final instance of each X container: `prds/3430-pod-epoch-lock-shadow.md:245`, every status entry "`state.terminated` with reason `Completed` or `Error`, a non-empty `containerID`, and an exit code that is ≥ 0 and not 137 or 255". For an earlier instance inside X it certifies only the kubelet ordering (`prds/1860-pod-epoch-writer-exclusion.md:195`, kubelet "re-reads the runtime and fails if any container still runs" before writing the terminal status) plus the per-container `restartCount` the handoff carries (`prds/3430-pod-epoch-lock-shadow.md:269`, "X's UID, with per-container `containerID`, exit code and `restartCount`;"). An earlier X instance that died on the dead-shim path is not excluded by the allowlist [I]. E11 (6(f)) closes this: X's evidence must report `restartCount` 0 for every X container that can write the data volume, so no earlier instance of those containers ran inside X.
  - No earlier worker instance of pod S reached admission, because consumption precedes admission on every path (`prds/3430-pod-epoch-lock-shadow.md:452`, "Consume the one-shot before admission on every path.") [I] and S is the consuming instance. That an instance of pod S which exited before consuming wrote nothing to the bare is an assumption Gate B verifies (8.1).
  - **Remaining older-writer exposure.** With E11, P is X's only worker instance. Two exposures remain, both already named:
    - X's last instance of each container: covered by the allowlist's refusal of exit 137 and 255 (`prds/3430-pod-epoch-lock-shadow.md:245`), which on the paths #1860 read rejects the dead-shim exit (`prds/1860-pod-epoch-writer-exclusion.md:231`, "on the paths read, the dead shim reports only 137 or 255"); a path not read stays the #1860 dead-shim residual.
    - Pods older than X: the 2026-10-24 older-pod-survivor residual (4.3, Q2).

  The processes of a dead namespace cannot write again. No held boundary is relied on. The in-pod restart (b) and boot-ID fallback (b′) are not taken: nothing today shows that every earlier container instance of S's pod, or of S's boot, is gone (6(g)).
- **W.** W stays held for the process lifetime under its existing #3404 contract:
  - owner: the worker process;
  - binding: `<dataDir>/.uzi-authority/worker.lock`;
  - release: only at process exit.

  This design uses W only so that its own record writes are uncontended. W is not evidence about older writers.
- **Non-evidence.** The issue body's Gate A section states: "Repeated empty snapshots, PID absence, a released worker lock, a timer, or `config.lock` alone are not replacement authority." and "An unresolved writer blocks admission; temporary inability at one observation is insufficient." The approved plan adds an epoch EX hold that ends before admission, which §4.1 shows ends before admission (`prds/3430-pod-epoch-lock-shadow.md:179`, "Once that pass has settled, S closes EX and takes SH (bounded) with admission still closed, and only then opens admission.").
  - The predicate consumes none of them.
  - It reads only two kinds of input: identities that each process recorded about itself, and the pod-terminal fact persisted from attested evidence.
  - It does not read EX ownership. X's terminal status stays true after EX closes.

### 6(b) Admission point and race ordering

**Startup, once per worker process:**
1. Acquire W: `agent/src/main.ts:433`, `installWorkerLifetime(await acquireWorkerLifetime(config.dataDir, { signal: controller.signal }));`. [S]
2. Read the process's identity: `PROCESS_INCARNATION` and `UZI_POD_UID`. If the pod UID cannot be read, write no record (restore stays disabled for this process). The posture inputs of E1 are read from S's own environment at restore admission, not recorded; the namespace identity fields belong to the future-(b) path (6(g)).
3. Write the boundary record durably (6(d)). This happens before `ensureRunnerTmpdir` (`main.ts:439`, `const runnerTmp = ensureRunnerTmpdir({ dataDir: config.dataDir });`), before the GitCache exists, and before any registration or claim. [S] If the write fails, the process continues with restore disabled for itself. Its successors find no record, so they refuse.
4. On the epoch lane only: after an attested (not shadow) fresh epoch verdict, and before admission opens, write the pod record once for this pod UID, naming this process's incarnation as its writer (6(d)). Only the instance that consumed the epoch fresh can do this, so the writer is the first worker instance of pod S. At base no such verdict exists: `agent/src/main.ts:1074-1075`, `if (epochBootstrap && epochOwnership && epochConsumption) {` / `const result = await runPodEpochShadow({`, runs a report-only pass (`agent/src/pod-epoch-reclaim.ts:112`, "Read-only, first-process shadow pass."), whose `authority` result is not consumed as authority before #3431 (`:58-59`, `... Deletion` / `* authority (M1b) requires BOTH verdict "eligible" AND authority "attested". */`). [S] So no pod record, and no E3 pass, exists until #3431 ships attested authority (8.3).
5. Admission opens. Bare writers may now spawn.

**Restore admission**, per claim of a run whose source slot is foreign-active and carries a saved pin (the #3644 entry):
1. Open an authority transaction A.
2. Load the slot and let P = `owner.worker`. If P is S, the existing path applies.
3. Evaluate the 6(f) predicate. Any false, missing, undecodable or unknown input means retain and stop: today's refusal (section 1), and nothing is written.
4. Run `assertBareClear` on the run's bound bare, inside the same A: `agent/src/authority-runtime.ts:1045`, `async #assertBareClear(bare: string, op: string): Promise<void> {`; refusal at `:1056-1057`. [S] A `running` record refuses.
5. Refuse on any other unresolved footprint, or any other foreign-active slot, over the bound bare or source. P's own source slot, the one being taken over, is the only exception.
6. Make the first successor mutation, still inside A: #3644's takeover write of the source slot. It records P, and a digest of P's boundary record, as its basis.
7. Close A. Write the attribution into the bare (#3644), syncing the bare's `config` and then the bare directory (`agent/src/git.ts:7776-7779`).
8. Later authoritative reads: no re-check of older writers is needed (branch 1). The existing checks of S's own ownership continue.

**Race ordering.** Steps 2 to 6 run in one A transaction. W excludes any concurrent compatible worker. The older writers the contract covers (the processes of X, and any earlier pod-S instance, which never reached admission) are dead or never wrote; older pods are the 4.3 residual, and earlier instances inside X are refused by E11. No TOCTOU window remains against the covered writers.

**Crash windows of restore admission.**
- **Before step 6.** Nothing was written; the slot is still P's, and the next eligible successor re-evaluates from scratch.
- **Between the durable slot takeover (step 6) and the bare attribution write (step 7).** The slot is owned by S with P as its basis, while the bare journal still carries P's attribution. S is now a dead incarnation. A later instance in pod S refuses (it is not the pod-record writer, E3). The first worker of the next pod sees a slot owned by S, whose pod is that worker's X, and so may be eligible over S. #3644 must read the slot-versus-journal mismatch fail-closed, or complete the attribution from the slot's recorded basis; Gate B tests this window (8.1).
- **During step 7.** A crash part-way through the bare write can leave `config.lock` or a partial attribution; that is #3644's to read fail-closed (6(c)).

**Effect on the observed refusal: none in Gate A.** `admitSourceUse` and `preservation_failed` are unchanged (section 1).

### 6(c) No force-clear

- Existing unresolved footprints, `running` records and foreign-active slots are never cleared, rewritten or reclassified to make the predicate pass.
- A bare that fails the predicate keeps its source and pins until a guarded operator discard: `specs/human.md:1414`, "There is no TTL, size cap, disk-pressure reclaim or generic force-clear; retained bytes are unbounded." [S]
- The predecessor's partial state, such as a `memoryPark` draft or a leftover `config.lock`, is #3644's to read fail-closed. This design proves only that no older process can still add to it.

### 6(d) Durability and crash recovery

**Persisted properties.**
- **Boundary record.** For every incarnation of this build that could write the bare, a write-once authenticated boundary record is durable on the data volume before that incarnation's first bare write or slot write.
- **Pod record.** For a pod whose first worker obtained an attested fresh epoch proof, a write-once record naming the terminal predecessor X is durable before that pod's admission opens.

**Failure model.**
- **Covered:** process crash at any instruction (SIGKILL, OOM).
- **Covered by design; evidence owed (8.2, #3430 storage audit):** storage or power loss, on supported storage (ext4 or xfs; `agent/src/authority-lock.ts:170`, `export async function requireSupportedStorage(`) whose fsync is honoured. [S for the storage gate; the fsync behaviour is owed]
- **A lost pod record is permanent for the pod's life.** The epoch ledger has already consumed the pod UID (4.2), so no later instance of the pod can write the record again and E3 can never pass in that pod. This fails safe: restore refuses until the next pod.
- **Not covered:** restoring an old copy of the store. This is out of scope, as it is for slots: `agent/src/completion-authority.ts:68`, "or a full reset), is NOT detected and is out of scope." [S]

**Records** (proposed names and contents):

| Record | Path | Content | Write |
|---|---|---|---|
| boundary | `<dataDir>/.uzi-authority/boundaries/<sha256(incarnation)>.json` | version, incarnation, pod UID; MAC'd under the store key | `writeDurable`, `completion-authority.ts:378-401`, plus a caller read-back |
| pod | `<dataDir>/.uzi-authority/pods/<sha256(podUid)>.json` | version, S's pod UID, predecessor X, digest of the attested evidence, X's name and `restartCount` for each container in the E11 set, writer incarnation; MAC'd | same |

Both records are write-once. If the file already exists with different content, the write refuses; an existing record is never rewritten.

**Why only these fields.** The interim predicate needs P's pod UID (to compare with X), S's incarnation and pod UID (to match the pod record's writer and pod), X's per-container `restartCount` for the E11 set (copied from the attested evidence when the pod record is written), and nothing else. Posture flags are read from S's environment at admission. The boot ID, PID-namespace inode and PID 1 start time serve only a future (b) or (b′) and need not land now: a P record without them simply cannot qualify for a future (b), which refuses (fail-safe), at the cost of one more non-restorable restart after that later upgrade (6(g)).

**Write and sync order** (`completion-authority.ts:30`, "Writes are tmp(O_EXCL|O_NOFOLLOW) -> file fsync -> rename -> directory fsync -> read back."). [S]
1. Ensure `boundaries/` or `pods/` exists, then fsync `.uzi-authority` **unconditionally**, whether or not this process created the directory. The precedent, `ensurePrivateDir` (`completion-authority.ts:453-463`), fsyncs the parent only when it created the directory (`:459`, `if (created) {`), so a creator that crashed between `mkdir` and that fsync would leave an entry a later process never makes durable. The proposed ordering therefore does not rely on that helper's conditional fsync.
2. Create the tmp file with `O_EXCL`, write it, fsync it.
3. Rename it into place.
4. fsync the containing directory (`boundaries/` or `pods/`).
5. Read the record back. `writeDurable` (`:378-401`) covers steps 2 to 4 and does no read-back itself; its callers do (`:437-439`, `if (await readRegular(path.join(dir, \`${shard}.json\`), ...) !== content) {`, and `:956-958`, `if (back !== content) throw new AuthorityRefusal("authority slot did not read back");`). The new record writers must do the same. [S]

This is the same file-then-directory order as `git.ts:7776-7779`: `const config = await fs.open(path.join(barePath, "config"), ...`, then `try { await sync(config, "config"); } ...`, then `const dir = await fs.open(barePath, ... O_DIRECTORY ...`, then `try { await sync(dir, "directory"); } ...`. [S] Admission (startup step 5) opens only after the read-back succeeds.

**State recovered at each crash point.** The table is for the boundary record. The pod record behaves the same, with "pod admission" in place of "first write".

| Crash point | Process crash | Storage or power loss | Outcome |
|---|---|---|---|
| before or during directory creation | no record | the directory may be lost | no record; the process had written nothing to the bare |
| after the tmp fsync, before the rename | stale `.<name>.<uuid>.tmp`, no record | same | no record; nothing written; the tmp file is never read |
| after the rename, before the directory fsync | record visible | the rename may be lost | either way, the process had not yet passed the barrier and wrote nothing |
| after the directory fsync and read-back | durable | durable | the first bare or slot write may follow |

With the unconditional fsync of step 1, a slot that names P can exist only after P's record was durable, on storage whose fsync is honoured (owed, 8.2). The guarantee that does not depend on that is weaker and sufficient: if a record is missing for any reason, the result is absence, and absence refuses (fail-safe). No crash point produces a record that certifies more than it should.

**Recovery ordering and rediscovery.**
- Startup does no replay, repair or rewrite.
- The records are inert evidence, read by key at admission: P comes from the slot owner, and the pod record from S's pod UID.
- A startup sweep may remove stale tmp names, limited to this design's directories. This is fail-safe, because a tmp file is never read.

**Verification (Gate B, 8.1).**
- Fault injection through the `AuthorityFsOps` hooks asserts the order file sync, rename, directory sync, and that admission waits for them. The seam has only those three members (`completion-authority.ts:99-103`, `syncFile(handle: fsp.FileHandle): Promise<void>;` / `syncDir(handle: fsp.FileHandle): Promise<void>;` / `rename(from: string, to: string): Promise<void>;`), so it cannot observe the read-back or the unconditional parent fsync's place before the first record write; asserting those needs a test-visible read or a new seam, which Gate B adds.
- A process-crash test at each crash point shows that the successor refuses.
- Neither read-back nor a process-crash test proves survival of storage loss. That rests on ext4 and xfs fsync semantics plus the operator storage audit, which is owed (8.2).

### 6(e) Versions and rollback

- **Older writers** leave no boundary record. They are the stable releases (v0.85.2 and earlier), builds before cc125a82, and W-era builds without this record.
  - As P, they refuse. So the first pod replacement after an upgrade is never restorable (Q10).
  - As other older writers, they are covered only by build-independent evidence: pod X's terminal evidence, and the fact that no earlier instance of pod S reached admission (6(a)). Earlier instances inside X are refused by E11; older pods remain the 4.3 residual.
- **Older readers.** Builds without the gate have no restore.
  - The proposed directories sit beside `enrollment.json`, `heads/` and `slots/`.
  - The current store lists `.uzi-authority` only to remove stale head staging directories, so it ignores the new entries. See `completion-authority.ts:482-483`, `for (const name of await fsp.readdir(init.dir)) {` / `if (STALE_HEADS_TMP.test(name)) ...`, with `:474`, `const STALE_HEADS_TMP = /^\.heads\.[0-9a-f-]{36}\.tmp$/;`. [S]
  - Gate B re-verifies this for every W-era release still deployable.
- **Mixed versions.**
  - W excludes concurrent compatible workers on one data directory.
  - Overlapping pods run one image: `prds/3430-pod-epoch-lock-shadow.md:187`, "Pods that overlap on one volume come from the same ReplicaSet, so they run the same image." [I]
  - An out-of-band pod running another build is an operator residual (Q8).
- **Rollback to a build without the gate.** Safe. That build has no restore, ignores the records and leaves them in place. After a roll-forward, the rollback-era incarnations have no record and refuse as P.
- **Undecodable or new metadata.** A record with an unknown version, a bad MAC (for example after a worker-token change) or unknown fields is preserved, never rewritten or deleted. The reader refuses eligibility.
- **Unsupported rollback is refused.**
  - A build whose predicate version is older than a record's refuses that record.
  - The attribution journal that restore writes must carry a fence that older workers refuse: `specs/human.md:1414`, "Older workers refuse to attach, recover or discard a journal carrying the fence (accepted rollback fence)." That is #3644's contract, and Gate B checks it.
- **No silent downgrade and no force-clear.** A missing or failed record never widens eligibility.

### 6(f) Eligibility predicate (every condition must hold)

E1. **Posture.** All of:
- `UZI_EPOCH_LOCK=1` (`agent/src/main.ts:380`, `const epochEnabled = process.env.UZI_EPOCH_LOCK === "1";`) and `UZI_DATA_DIR=/data`;
- a declared runtime in {containerd-runc, crio-crun} (`agent/src/pod-epoch-bootstrap.ts:1`, `type Runtime = "containerd-runc" | "crio-crun";`, read at `:31`, `const runtime: Runtime | undefined = declared === "containerd-runc" || declared === "crio-crun" ? declared : undefined;`) [S];
- supported storage;
- uid-split active (`agent/src/runner-uid.ts:72-73`, `export function uidSplitActive(env: NodeJS.ProcessEnv = process.env): boolean {` / `return env.UZI_UID_SPLIT === "1";`). [S]

E2. **S's boundary record** exists, its MAC verifies, its version is known, its incarnation equals `PROCESS_INCARNATION`, and its pod UID equals `UZI_POD_UID`.

E3. **S's pod record, written by S.** It exists for S's pod UID and its MAC verifies. It names X from an attested fresh evaluation under EX with `current == S` and `previous == X` (`prds/3430-pod-epoch-lock-shadow.md:179`) [I]. Its writer incarnation equals S's `PROCESS_INCARNATION` and S's boundary record's incarnation, and its pod UID equals S's boundary record's pod UID. So S is the first worker instance of pod S; any later instance of the pod (after a container restart) fails E3. E3 cannot pass at base: the pod record needs an attested, non-shadow epoch verdict, which only #3431 delivers (6(b) startup step 4; 8.3).

E4. **P's record.** P is the owner of the foreign-active source slot being restored, and P ≠ S. P's boundary record exists, its MAC verifies, and its version is known.

E5. **(c), predecessor pod: P.pod = X from E3.** This is the only eligible case.
- **(b), in-pod restart: NOT ELIGIBLE.**
- **(b′), boot-ID fallback (P.pod = S.pod, P.boot ≠ S.boot): NOT ELIGIBLE.**

Both stay so until Q4 is decided AND an evidence source exists that covers every earlier container instance in the pod or boot (6(g)).

E6. **Anything else refuses.** If P.pod is not X (including P.pod = S.pod), refuse.

E7. **`assertBareClear` passes** for the run's bound bare, inside the same A transaction as E1 to E6 and the takeover write.

E8. **No other blocker.** No other foreign-active slot or unresolved footprint touches the bound bare or source (6(c)).

E9. **Accepted residuals.** The owner has accepted Q2 (older-pod survivors), Q7 (DinD device reach) and Q8 (operator-started processes) for restore. Otherwise refuse.

E10. **Scope.** The predicate reads only the run's bound bare and its slots. An unrelated or unknown bare never refuses an ordinary pin or capture (8.1, tripwire).

E11. **No earlier instance inside X of a data-volume writer.** S's pod record carries, from X's attested evidence, the `restartCount` of every X container in the set below, and each is 0. A set member absent from the evidence or the record, an absent, non-integer or nonzero `restartCount`, or a pod record without the counts refuses. With E5 (P.pod = X), this makes P X's only worker instance.
- **The set: X containers that mount the `data` PVC read-write beyond the `runner-shared` subPath.** As rendered for the E1 posture (`controller/internal/kube/render.go`):
  - `worker`: the whole PVC, read-write (`render.go:1271`, `{Name: "data", MountPath: dataMountPath},`);
  - `seed-nix` (regular init): the whole PVC at the seed path (`render.go:1266`, `{Name: "data", MountPath: dataSeedMountPath},`);
  - `runner-init` (regular init, DinD lane): the whole PVC (`render.go:1349`, `VolumeMounts:    []corev1.VolumeMount{{Name: dataVolume, MountPath: dataSeedMountPath}},`).

  The two init containers are included because they hold the whole volume read-write, not because they were shown to write the bare; requiring 0 for them is fail-safe. Search behind the set: `grep -rn 'Name: *dataVolume\|Name: *"data"\|runnerWorkdirMount()' controller/internal --include=*.go | grep -v _test` finds these three mounts, the PVC volume itself (`render.go:1293`), and the `runner-shared` subPath mount (`render_dind.go:82`) used by `dind` (`:301`) and the worker (`render.go:1386`).
- **`dind` is excluded; its restarts do not bear on the bare.** Its only `data` mount is the subPath: `render_dind.go:80-82`, "It is the ONLY data mount dind may have." / `return corev1.VolumeMount{Name: dataVolume, MountPath: dindWorkdirDir, SubPath: dindWorkdirSubPath}`, with `:301`, `mounts := []corev1.VolumeMount{runnerWorkdirMount()}`; its other volume is the separate DinD claim (`:360`, `mounts = append(mounts, corev1.VolumeMount{Name: dindDataVolume, MountPath: dataDir})`). That subPath is `/data/runner`, while the bare lives under `path.join(dataDir, "repos")` (`agent/src/git.ts:3226`). The one way a later `dind` instance could mount more of the volume is the single-uid subPath swap (`render_dind.go:63-66`), and E1 requires uid-split, under which "no untrusted identity can write /data" (`:66`); 3.2 (b) and 5.3 check 3 reach the same verdict. So no `dind` instance, earlier or last, and no container its daemon started, reaches the bare through a mount. What remains is a rootful daemon's device-level reach, which is Q7 under E9. `dind` restarts do matter for the predecessor's source clone under `/data/runner`; that is Q3 (section 7), not this condition.
- **`dind-init` and `dind-meter` are excluded: neither mounts `data`.** `render_dind.go:483-484`, `VolumeMounts: []corev1.VolumeMount{` / `{Name: dindSocketVolume, MountPath: dindSocketDir},`; `:602-604`, `{Name: dindDataVolume, MountPath: dindMeterDataDir, ReadOnly: true},` / `{Name: dindMeterVolume, MountPath: dindMeterDir},`.
- **The evidence already carries the field; no format extension is needed.** Controller: `controller/internal/kube/pod_epoch_evidence.go:24`, ``RestartCount int32  `json:"restartCount"` ``, validated non-negative at `:165`; filled from kubelet status for containers and init containers alike (`pod_epoch_session.go:412`, `out = append(out, PodEpochContainer{Name: c.Name, ContainerID: c.ContainerID, ExitCode: t.ExitCode, RestartCount: c.RestartCount})`, called for `p.Spec.Containers` at `:416` and `p.Spec.InitContainers` at `:420`). Agent: `agent/src/pod-epoch-evidence.ts:4`, `name: string; containerID: string; exitCode: number; restartCount: number;`; the key is required (`:105`, `shape(e, ["name", "containerID", "exitCode", "restartCount"]);`) and must be an integer in [0, 2^31−1] (`:108-109`). [S] Gate B adds only the copy into the pod record (6(d)) and the set check (8.1).
- **Inferred.** That a kubelet `restartCount` of 0 means no earlier instance of that container ran in the pod is **inferred** from the Kubernetes pod-status API, not read in kubelet source; it is owed (8.2).

**Supported posture** (only under E1, and only once the 8.2 evidence for it exists and #3431 ships attested authority):
- (c) restore by the first worker instance of a new pod over the immediately preceding pod on the hosted Docker lane.

**Refused:**
- (b) worker-container restart and (b′) boot-ID fallback, until 6(g) is met;
- (a0), every case;
- (a) compose (Q5);
- (d) kube-native (Q6);
- the isolated lane;
- any single-uid start (Q6);
- an undeclared runtime or unsupported storage;
- non-container `npm start`;
- anything not listed.

### 6(g) Path to future eligibility of (b) and (b′) (NOT ELIGIBLE now)

The earlier draft admitted these two cases; review showed both unsafe. The analysis stays here as the route to eligibility.

**The earlier conditions.**
- (b): P.pod = S.pod and P.boot = S.boot; (P.pidns, P.initStart) ≠ (S.pidns, S.initStart) and P.initStart < S.initStart; termination evidence for P's container instance excludes the dead-shim path.
- (b′): P.pod = S.pod and P.boot ≠ S.boot.

**Why they fail.** Both examine only P, not the instances between P and S, nor S's own earlier instances. Scenario: S0 writes the pod record (X terminal); S0 dies on the dead-shim path (`prds/1860-pod-epoch-writer-exclusion.md:231`, "On this path no shim reaps init, and if `runc delete --force` failed, init may still be alive.") with a delayed git child; kubelet starts S1 in the same pod; S1 restores over P in X. E3 would pass on S0's record and E7 cannot see the orphan. The same holds for (b) with any instance between P and S, and for (b′) with an earlier instance on S's boot.

**What eligibility needs.**
1. Q4 decided.
2. An evidence source that covers every earlier container instance in the pod (for (b)) or on S's boot (for (b′)): termination evidence bound to each instance's container ID and restart count. Pod status alone is not enough, because kubelet keeps only the last termination of each container (`lastState`); that it keeps no longer history is **inferred** from the Kubernetes pod-status API and owed (8.2).
3. The identity fields recorded per process, in a new boundary-record version: boot ID, own PID-namespace device and inode, and PID 1's start time. Their kernel properties are inferred and owed (5.3 check 1): one init per namespace for its life, inode reuse only after the namespace is gone, and the same-tick case, where an equal (inode, init start) pair must refuse. Records of the interim version lack them and so never qualify (fail-safe).
4. For #3644's same-source takeover, Q3's answer on DinD-started containers (section 7).

## 7. Predecessor-source writers (owner question)

Section 6 covers writers of the bare. #3644's parked same-source takeover would also trust the predecessor's source clone under `<dataDir>/runner`, and that clone has a writer the bare does not.

**The extra writer.** Containers started by the inner dockerd mount the runner root:
- `controller/internal/kube/render_dind.go:53-55`, "The path is the agent's runner-clone root: git.ts sets runnerRoot =" / "<UZI_DATA_DIR>/runner and the run's SDK cwd is a clone under it, so the docker bind" / "source is /data/runner/... COUPLED to agent/src/git.ts; if that root moves, this";
- `:72`, `dindWorkdirSubPath = "runner-shared"`. [S]

These containers survive a worker-container restart (3.2 (b), inferred).

**Per posture:**
- **(b).** A container started by P's run can still write P's source clone after S starts, whatever S proves about the bare.
- **(c).** X's DinD sidecar ended with pod X, so its containers are covered by X's terminal evidence (inferred).
- **Both.** Runner-uid processes in P's container die with its namespace.

**Owner question (section 9, Q3).** For #3644's same-source takeover, must the proof also cover writers into the predecessor's source clone? If yes, there are two candidate routes:
- (b) is refused for same-source takeover until the DinD containers started by P's runs are shown to be gone. A DinD-side inventory, or a sidecar restart bound to the worker restart, could supply that proof.
- Same-source takeover is limited to (c).

Under the interim contract only (c) is eligible anyway (6(f)), so for now Q3 bears on (c), where the cover of X's sidecar is inferred, and on any future (b) (6(g)). This record does not decide it.

## 8. Gate B obligations and owed evidence

Every item in this section is **pending, not delivered by Gate A**.

### 8.1 Implementation obligations (pending, not delivered by Gate A)

**Watch-fail regressions on real child and write seams.** Every path refuses today, so a refusal regression cannot fail on the pre-change tree. Instead, each watch-fail case runs the restore admission path with one E-condition mutated to pass (an unsafe variant of the predicate) and observes the stale write or the missing refusal; the real predicate then refuses. Each joins every process it owns. No proof is sleep-only.
- **Second instance in the same pod (the S0/S1 case).** S0 writes the pod record (X terminal) and holds a delayed git child blocked before its `git config` write on the bound bare; S0 is killed while that child stays alive in S0's PID namespace; S1 starts in the same pod and attempts restore over P in X. The real predicate refuses at E3 (S1 is not the pod-record writer). The unsafe variant, with E3's writer check removed, admits S1, and releasing the child shows its stale write landing after S1's attribution.
- **Posture refusal.** While a killed worker's PID namespace stays alive (a child of the killed worker held in it), admission refuses without qualifying evidence; the unsafe variant with E1 or E5 forced true admits.
- **Mutated E-conditions.** For each of E1 to E8, one unsafe variant with that condition forced true, each showing the stale write or the missing refusal it guards.
- **Earlier instance inside X (E11).** X's evidence reports `restartCount` 1 for `worker`: an earlier X worker instance holds a delayed git child blocked before its `git config` write on the bound bare, and P is X's last worker instance. The real predicate refuses at E11. The unsafe variant, with E11 removed, admits S, and releasing the child shows its stale write landing after S's attribution. Controls: `restartCount` 0 for the whole set admits (all other conditions passing); a nonzero count on `seed-nix` or `runner-init` refuses; a nonzero count on `dind`, `dind-init` or `dind-meter` alone does not refuse; a set member missing from the evidence, or a pod record without the counts, refuses.
- **E11 set pinned to the render (differential test).** E11's container-name set is a second statement of the render's mounts, so a controller render test enumerates every container and init container of a rendered E1-posture pod whose `VolumeMounts` name `data` without `ReadOnly` and without the `runner-shared` subPath, and asserts that list equals E11's set. Cases it must discriminate, each asserted exercised: a whole-PVC mount (`worker`), a whole-PVC init mount (`seed-nix`, `runner-init`), the subPath-only mount (`dind`), and a container with no `data` mount (`dind-init`, `dind-meter`).
- **Enrollment or isolation variant**, only if the owner chooses (iii) or (iv) instead:
  1. Delay an attribution writer before its config write.
  2. Kill its worker.
  3. Attempt admission.
  4. Release the writer.

**Posture and evidence check (not a product regression).** Real PID-namespace teardown kills a delayed writer before a successor starts. It checks the kernel behaviour the design relies on, not product code. It runs in a mandatory Linux PID-namespace lane:
1. Block a writer before its `git config` write.
2. Kill the namespace's init.
3. Start the successor.
4. Show that the writer never completes.

**Controls.**
- a normally joined writer;
- an enrolled unresolved footprint, which refuses;
- an unregistered legacy writer as P with no record, which refuses;
- restart and replay: the record is never rewritten;
- failed evidence: a missing pod UID, a bad MAC, an unknown version or a missing pod record refuses;
- the rollback boundary (6(e)).

**Other obligations.**
- **Record crash windows.** Cover every row of the 6(d) table through `AuthorityFsOps` injection (file sync, directory sync, rename only), with a successor refusal at each. The read-back and the unconditional parent fsync need a test-visible read or a new seam (6(d)).
- **Restore crash windows.** Cover the window between the durable slot takeover and the bare attribution write (6(b)), showing #3644's fail-closed read.
- **Pre-consumption writes.** Show that a pod-S instance which exits before epoch consumption has spawned no bare writer (6(a)).
- **Preservation.** The predecessor's bytes, source epoch, fence, pins, journals and custody stay byte-identical across every refusal.
- **Discard policy.** The #3644 verified-merge discard policy stays unchanged.
- **Records.**
  - An ADR-1783 amendment recording the branch, the predicate, the residuals and the rollback rules.
  - Recovery-guidance updates in `docs/run-recovery.md` for the new refusal reasons.
- **Tripwire.** `agent/test/recovery-coordinator.test.ts:507`, `` it(`an unrelated unknown bare does not suppress an ordinary ${boundary} (#3831 tripwire)`, async t => { ``, pins that an unrelated unknown bare does not suppress an ordinary pin or capture. The unknown bare is a symlink to a missing repository, at `:530-531`. [S] Gate B keeps it green by keeping the refusal selective and bound to the run's bare (E10): the predicate runs only at restore admission, for the run's bound bare, and never on the ordinary pin or capture path.
- **E2E sweep** of states, reasons and ordering through `e2e/phases`, `e2e/lib.sh` and `e2e/forge-fake`.
  - A sweep for agent kills and recovery holds matched three phases: `e2e/phases/45-bounded-concurrency.sh`, `e2e/phases/52-api-outage-outbox.sh` and `e2e/phases/73-forge-unreachable-park.sh`. The sweep used `grep -l -E 'preservation_failed|foreign-active|kill -s (KILL|SIGKILL) agent|restoreTip|recovery_wait'`. Only phases 45 and 52 restart the agent. Phase 73 matched on `recovery_wait` alone and does not apply: `e2e/phases/73-forge-unreachable-park.sh:39`, "the agent is never restarted; the api sees the healthy forge for every claim + snapshot."
  - Every affected phase left unrun is named, with its reason.
  - Under 6(f) these compose phases refuse restore (Q5).
  - Nightly phases 45 and 52, with phase 52 cases 7 and 8 (#3833), are the combined #3644 acceptance signal [M].
- **Timing.** Measure the startup cost of the record write (one file fsync and two directory fsyncs) and of admission evaluation. The design adds no wait or poll. Any wait that a later revision adds must state its failure-duration tradeoff.
- **Landing obligation.** `full agent suite: exact-head PR CI`.
- **Workflows.** No workflow edits in an agent-dispatched implementation.

### 8.2 Owed hosted and live evidence (pending, not delivered by Gate A)

| Evidence | Actor | Steps |
|---|---|---|
| Runtime and storage compliance | operator | Following the #3430 storage audit, attest the declared runtime on each hosted lane. Also attest that the data storage honours fsync and either enforces attach exclusivity or is node-local. |
| Earlier instances inside X (E11) | maintainer with operator | Confirm from kubelet source, or live on each declared runtime, that a container's `restartCount` of 0 means no earlier instance of it ran in the pod (including across a kubelet restart). Record how often real replacements report a nonzero count for `worker`, `seed-nix` or `runner-init`, which is how often E11 refuses. |
| Future (b) only (6(g)): per-instance termination evidence | maintainer with operator | Find or design a source that binds termination evidence to each container instance (container ID and restart count), and confirm live that pod status keeps only the last termination. |
| Future (b) only: DinD across an in-pod worker restart | operator with maintainer | On the hosted Docker lane, start a long-running container from a run and restart the worker container. Record whether the sidecar and that container survive, and what each mounts: compare `docker inspect` mounts with `render_dind.go:80-82`. |
| Future (b) only: no surviving git PID after an OOM or in-pod restart | operator | On containerd-runc and on CRI-O-crun, OOM-kill the worker while a delayed git child runs. Show from a host-side PID-namespace listing that the child is gone before the successor starts. Record the reported exit code and reason, for Q4. |
| Future (b) only: identity readability | maintainer | On each runtime, confirm that the worker can read the PID-namespace inode, PID 1's start time and the boot ID. Measure how often a restart reuses the old inode, and confirm the kernel properties 5.3 check 1 infers. |
| Kube-native behaviour | operator | Repeat the restart test on the kube-native lane, to inform Q6. |
| Force delete, node loss, cross-node RWO detach | operator | Record what evidence, if any, the controller delivers. The expected outcome is "ineligible", confirmed live. Cross-node detach is inferred in #1860 and is owed. |
| #3644 partial M2 | maintainer | Disclose 6d859c55 and d1d34264, and have them independently reviewed against 6(b) and 6(e) before any reuse [M]. |

### 8.3 Issue overlap and sequencing

- **#3644**: the consumer. It owns the source, pin, attribution, publication and fence contracts.
- **#3404**: W, slots and the completion fence (Q1).
- **#3031**: rejected as evidence (5.1).
- **#1860, #3430, #3431**: the epoch evidence, reused here as the persisted pod-terminal fact. E3 cannot pass, and Gate B cannot enable (c), until #3431 ships an attested, non-shadow epoch verdict; at base only the report-only `runPodEpochShadow` runs (6(b) startup step 4).
- **#3621**: #3431's live acceptance.
- **#3709, #3710**: the compose and plain-hosted exclusion proofs (Q5, Q6).
- **#3213**: an overlap named by the maintainer. `git grep -n '#3213' -- prds adr docs specs ':!prds/3831-bare-writer-exclusion.md'` finds nothing in this clone [M].
- **#3833**: phase 52 cases 7 and 8.

Gate B re-reads `prds/3430-pod-epoch-lock-shadow.md` and `prds/3431-pod-epoch-reclaim.md` at its own base (4.5). It serializes behind the in-flight runs eb39666b (#3859) and c13948f0 (#3885) [M].

## 9. Open owner questions and Decision Log

### 9.1 Owner questions

1. **Q1 (from 2.4).** Does an unenrolled writer of the completion fence also weaken the #3404 release rule? If so, is that tracked under #3404?
2. **Q2 (from 4.3).** Does the 2026-10-24 older-pod-survivor residual extend to restore attribution in a live bare?
   - Refusing whenever more than one epoch separates P from S does not close this residual: an older pod's survivor could write the bare whoever P is.
   - So the real choice is between accepting the residual and refusing restore on every lane.
3. **Q3 (from section 7).** Must the proof cover writers into the predecessor's source clone for #3644's same-source takeover? If so, how are DinD-started containers handled in a future (b)?
4. **Q4.** The in-pod restart (b) inherits the dead-shim residual, and an OOM-killed worker container is expected to exit 137 too (inferred, owed). (b) and (b′) are NOT ELIGIBLE in the interim contract, and stay so until this is decided AND an evidence source covers every earlier container instance in the pod or boot (6(g)). Choose one:
   - accept the residual for in-pod restarts; or
   - require new controller evidence of each instance's termination status (bound to container ID and restart count; for example reason `OOMKilled`) before (b) is eligible.
5. **Q5.** Compose has no evidence that the containers before the current one are gone. Choose one:
   - accept Docker's stop-before-recreate ordering (inferred) as a residual, and add a verified compose container-instance identity; or
   - refuse compose. Phases 45 and 52 then cannot be the restore acceptance signal.
6. **Q6.** Confirm that the kube-native lane and every single-uid start refuse.
   - Single-uid is the hosted default (`render.go:573-575`; `controller/internal/config/config.go:198-199`, `UZI_WORKER_UID_SPLIT`, default false), so the default hosted fleet would refuse.
   - The alternative is to accept ADR-3153's single-uid subPath residual for restore.
7. **Q7.** Should a rootful DinD daemon's device-level reach (`adr/3153-runner-shared-pvc-subpath.md:59`, "It does not bound a rootful daemon's device-level reach") be accepted as a restore residual, or should rootful DinD postures refuse?
8. **Q8.** Should these be accepted as operator residuals?
   - a worker started by hand inside a live worker container, by exec or in a nested PID namespace;
   - an operator's git;
   - an out-of-band pod running another build.
9. **Q9.** There is one boundary record per worker start. Should records that no slot names be pruned (fail-safe, because absence refuses), or should they stay unbounded?
10. **Q10.** Confirm that the first pod replacement after upgrading to the gated build stays non-restorable, because P has no record.

### 9.2 Decision Log

| Date | Decision (PROPOSED, not approved) | Why | Rejected alternative |
|---|---|---|---|
| 2026-11-04 | Take branch 1 ("permanently unable") via build-independent pod-terminal evidence for X, for the first worker instance of the new pod only; rely on no held boundary. | A dead namespace's processes cannot return. The non-evidence named by the issue body and the approved plan excludes every held signal available today. | A held boundary: EX ends before admission, and W is per-process and named as non-evidence. |
| 2026-11-04 | Adopt (i′): a write-once boundary record per worker process (incarnation, pod UID), a write-once pod record persisting the attested predecessor-terminal fact and naming its writer incarnation, and the E1 to E11 predicate. | The smallest design covering (c): two records and one evaluator. | (ii) offers only non-evidence. (iii) leaves the predecessor's keys writable. (iv) blocks on every crash and misses legacy writers. |
| 2026-11-04 | The boundary-stamp hypothesis (5.3) is adopted in part, with three changes. W is dropped as death evidence. The "since the last stamp-less write" rule is replaced by "P's record must exist" plus teardown evidence. The namespace identity fields (inode, PID 1 start time, boot ID) move to the future-(b) path (6(g)). | W proves release on one kernel only. A stamp-less build leaves no trace. The interim contract does not read the identity fields, and records without them only refuse a future (b). | The hypothesis as stated. |
| 2026-11-04 | Narrowing after review: the in-pod restart (b) and the boot-ID fallback (b′) are NOT ELIGIBLE until Q4 is decided and an evidence source covers every earlier container instance in the pod or boot. | Both examined only P: an earlier instance of S's own pod (S0, dead-shim path, delayed git child) could still write while S1 restores (6(g)). | Admitting (b) and (b′) on P's identity alone. |
| 2026-11-04 | (proposed) Close the former open question Q11 in the contract: E11 requires X's evidence to report `restartCount` 0 for `worker`, `seed-nix` and `runner-init`, the X containers that mount the `data` PVC read-write beyond the `runner-shared` subPath; anything else, including an absent count, refuses. `dind` is excluded. | X's evidence certifies only the last instance of each container (`prds/3430-pod-epoch-lock-shadow.md:245`), so an earlier X worker instance that died on the dead-shim path with a delayed git child was the same hole the S0/S1 narrowing closed for pod S. The #3430 evidence already carries per-container `restartCount` (`pod_epoch_evidence.go:24`, `pod-epoch-evidence.ts:105`). `dind` mounts only `/data/runner`, which under uid-split cannot reach the bare (6(f) E11). | Leaving it as an owner question to accept as a residual alongside Q2: an in-pod survivor of X is not an older-pod survivor, and the evidence to refuse it already exists. |
| 2026-11-04 | Supported posture: the hosted Docker lane with uid-split, where S wrote its pod's record and P ran in X. Everything else refuses until the owed evidence, #3431's attested authority and the owner answers exist. | Only this posture has a defined evidence chain. | Refusing every posture with no route forward. |
| 2026-11-04 | Store both records in `.uzi-authority`, written in the store's order: unconditional parent fsync, tmp, fsync, rename, directory fsync, caller read-back. Absence or any failure refuses. | Reuses an audited durable-write path, and every failure mode refuses. | Records in the bare's config, which the writers in question can change. |
| 2026-11-04 | Gate A changes no behaviour. Restore stays disabled; `admitSourceUse` and `preservation_failed` are unchanged. | Design-only scope. | Shipping any part before owner approval. |
