# ADR-4001: command-uid writer exclusion for the retained predecessor FINAL: an opt-in, kube-rendered Docker-free, restricted-posture compound proof for Claude predecessors

**Status**: Proposed contract, incomplete. Nine owner decisions (D1 to D9) are approved and recorded here. Six items (P-1 to P-6) are recommendations or open problems pending owner decision or a missing mechanism, and are NOT approved. The retained predecessor FINAL pass stays disabled in production (`command_uid_unproven`) until P-1 to P-6 are resolved and a separate implementation issue lands and is verified.
**Date**: 2026-11-20
**Issue**: [vtmocanu/uzi#4001](https://github.com/vtmocanu/uzi/issues/4001)
**Refs**: #2969, #3992, #3851, #3831, #3431, #1862, #4007, #4162, #4494, #1934

This is a design record only. No code changed with it. The gate in
`agent/src/runner.ts` `settlePredecessorHolds`,
`if (!this.predecessorFinalForTests) return skip("command_uid_unproven");`,
is unchanged.

## Context

PR #3992 (#2969) shipped the retained predecessor FINAL pass disabled. The
[ADR-1296 #2969 amendment](1296-durable-run-recovery.md#amendment-2969-predecessor-guarded-hold-settlement-is-built-and-disabled-in-production)
records why: nothing proves that no command-uid (`runner-cmd`, uid 10003)
process can still write into an older generation's attempt clone.

- The quiescence scan sees only its target uid:
  `agent/src/run-quiescence.ts` skips `status.uid !== req.targetUid`, and
  `defaultTargetUid()` is the runner uid under the uid split.
  [ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md) records
  "A-foreign-uids: worker, root and runner-cmd processes in scope are ignored,
  never unverified".
- Attempt clones are group-`runner` writable. `agent/src/main.ts` sets
  `process.umask(0o002)` under the split, repo directories are 2775, and
  `agent/src/git.ts` requires clone mode `0o2070`. `runner-cmd` is a member of
  group `runner` (`agent/templates/base/Dockerfile`).
- Command confinement (`agent/codex/supervisor/cmdsandbox`, Landlock) is off by
  default (`deploy/chart/values.yaml` `commandSandbox: "off"`). So with today's
  defaults any run's command can write any clone, and only a process-table
  scan could look for that.
- #2969's run tried three process-table heuristics and all were rejected:
  1. a whole-namespace scan, defeated by a fork behind the cursor after a pid
     wrap;
  2. an `ns_last_pid` bracket, defeated by a full counter cycle;
  3. a fork-total bound, defeated by failed clones and thread pids.

  [ADR-1598](1598-codex-command-storage.md) limits `proveNoUser` to worker
  startup, in a fresh PID namespace before any wrap.

This ADR is the contract a later implementation issue must meet before the
pass may be enabled. One uzi run yields one PR, so the contract lands first.

## Proof boundary

**Target set T.**
- T is every older attempt clone in the verified inventory that a FINAL covers:
  the predecessor source, plus Claude sibling clones.
- The exclusion covers the whole set, not only the held attempt path (owner
  comment, 2026-11-06).
- Sibling-path permission and command-uid exclusion remain separate
  obligations.
- The Codex older-sibling source proof stays its own issue.

**Writer classes.**

| Class | Writers | Disposition |
|---|---|---|
| (a) This run's lower generations | No Claude process runs as `runner-cmd` (`agent/src/run-procs.ts` header). Worker-launched uid-10003 helpers of those generations are in scope, including teardown passes that outlive their generation. | P2 helper tracking and the P6 veto. Codex predecessors are deferred (D3). |
| (b) Other runs' uid-10003 processes | Confined commands, the Codex app-server, checker fileop | Must be excluded (owner decision 2026-11-20). |
| (c) Worker-launched uid-10003 helpers | Every enumerated launch site | Confinement, or the runtime T-overlap veto, plus pinned resolution (H-A, defined in the mechanism section). |
| (w) Worker-uid processes | Hold group `runner` and ambient setuid (`agent/templates/entrypoint.sh` `--ambient-caps -all,+setuid,+setgid`) | Part of the named TCB. Their writes to T are audited: only the pass's own effects and the ordinary paths. |
| (d) Docker daemon and container writers | | Excluded by ineligibility (D4, D5). The ADR-1783/1786 residual is unchanged elsewhere. |
| (e) Runner-uid escaped writers | A runner-uid process with cwd outside every target path and no attempt marker or clone key | The 2026-11-06 owner-accepted residual, unchanged. It never exempts a command-uid writer. |
| (e') Other runs' marked runner-uid agents | Other runs' live Claude agents: they carry their own markers and are ignored by the quiescence scan while their cwd is outside T | NOT covered by the accepted residual, which is limited to markerless processes. Pending under P-1, together with the routes (hardlink alias, rename in and out) by which any runner-uid process can enable a command-uid write. |

No residual's disclosure, and no behaviour of the ordinary terminal FINAL,
substitutes for the command-uid proof. The ordinary terminal FINAL keeps
byte-for-byte base behaviour; its own command-uid gap needs a separate owner
decision.

## Existing mechanisms evaluated

Key: G = this generation, O = older generations of this run, R = other runs,
U = unregistered processes, S = sibling processes.

| Mechanism | Covers | Why it is not sufficient alone |
|---|---|---|
| Registered-root settlement: supervisor subreaper drain to `ECHILD+__WALL`, pidfd signalling, `terminalProcessProof`, the capture admission lane, fileop as a registered root | G registered Codex roots, in memory | Lost at worker death. No coverage of O after a process change, nor of R, U or S (ADR-1783, "C strengthens registered-process exclusion, not daemon or sibling exclusion"). |
| Executor lifetime seal and the durable `sealed` slot (#3404) | G registered provenance, completing Codex flights | Not generation-scoped and published only on completion. No R, U or S coverage ("unregistered or sibling writers are unchanged"). |
| Pod-epoch writer exclusion (PRD 1860, #3430/#3431) | O from an earlier evidenced pod epoch, at startup | No in-life authority. It accepts that an older pod's process may survive. |
| Landlock cmdsandbox | R and O confined commands, path-based opens only | Off by default. A command's own clone is in its domain. No control of metadata (chmod, chown, xattr, utimes), descriptor transfer or network. |
| Container-instance incarnation boundary (#3831, `prds/3831-bare-writer-exclusion.md` §3.0, §6) | O in an ended container instance, under that design's runtime reliance | Does not cover a same-incarnation re-claim. Its reliance does not authorize custody release (D7). Its records are reused for bindings, with no parallel record. |
| Per-attempt cgroups or freeze | All classes | Closed as not planned (#1862); not revisited. |

Conclusion: no existing mechanism, alone or combined, excludes other runs'
and unregistered command-uid writers on the default posture. A new, opt-in
posture is needed.

## Owner decisions (2026-11-20, owner decision via lead and buddy)

D1 to D9 were each answered by the owner through this run's ask_user
gates on 2026-11-20, with the answer recorded as "owner decision via lead
and buddy". The later checker-raised questions (P-1 to P-6) reached no human
and are not decided.

**D1. Opt-in posture approved.**
- The compound proof below is the enablement contract.
- It is valid only on workers running Codex command confinement in `required`
  mode with the tightened policy ("policy v2", defined in the mechanism section).
- Chart and compose defaults stay unchanged. This ADR does not recommend
  changing them.
- Implementation depends on #1934: a fresh packaged proof before confinement
  is re-enabled.

**D2. Descriptor transfer is closed by seccomp, not accepted as a residual.**
- The mechanism is an unprivileged cmdsandbox seccomp filter that denies
  `socket(AF_UNIX)` and allows `socketpair`, for the restricted posture only.
- Landlock ABI 6 alone is insufficient (a design assumption about Landlock
  ABIs, recorded with the owner's decision, that the implementation must
  confirm on the probed kernel). It scopes abstract sockets, but
  withholding socket-file creation does not stop a `connect` to an existing
  pathname socket. Pathname-connect control needs Landlock ABI 9.
- Required, stated here:
  1. **Docker over the unix socket.** The posture is incompatible with
     command-root Docker over the shipped unix socket.
  2. **Inherited descriptors.** An audit of descriptors inherited at
     command-root launch: no writable descriptor into another clone may cross
     the launch boundary.
  3. **Bypass audit.** Each bypass is handled fail-closed: `socketcall`, the
     32-bit and x32 ABIs, io_uring socket operations, and any other path that
     creates an AF_UNIX socket.
  4. **Runtime probe.** Landlock and seccomp availability is probed, with a
     pre-entry skip when either is missing. No CI kernel version is assumed.

**D3. Codex predecessors are ineligible. The pass enables for Claude
predecessors only.**
- Any entry in T owned by uid 10003, or any entry whose ownership is
  unreadable, refuses pre-entry.
- Codex enablement is explicitly deferred because of the metadata-write gap.
  Landlock does not control chmod, chown, xattr or utimes. Files a Codex
  generation writes are owned by uid 10003, so another run's confined command
  could change their tracked mode. A mode digest cannot close the
  check-to-release window.

**D4. Docker-capable workers are ineligible, over any transport.**
- This covers a unix socket and TCP, including hosted DinD
  `tcp://127.0.0.1:2375` (`controller/internal/kube/render_dind.go`), and any
  reachable daemon.
- Opting in requires a positively established Docker-free posture. An unset
  configuration or a failed probe is not enough.
- The ADR-1783/1786 Docker residual stays unchanged everywhere else.

**D5. Kube-rendered only.**
- A worker is eligible only when all of these hold:
  - the controller renders an explicit Docker-free tier;
  - the controller renders a no-co-mount invariant;
  - the local negatives pass: no `DOCKER_HOST` or `UZI_DIND_SOCKET`, no socket
    file at the known paths, no foreign listener in the worker's own network
    namespace.
- Compose workers are ineligible.
- Docker-free eligibility is a bound, controller-backed contract, tied to the
  rendered worker and its PROCESS_INCARNATION. It is not a universal proof
  that no daemon exists.
- Missing, stale or contradictory evidence refuses pre-entry.
- RWO access mode alone does not stop another pod on the same node from
  mounting the volume. The no-co-mount invariant therefore comes from the
  render, not from the access mode.

**D6. The M4 Codex leaf is a refusal leaf.**
- A Codex predecessor skips with `command_uid_unproven`. The leaf must RUN
  and PASS on CI and assert zero predecessor effects.
- The positive Codex leaf is deferred with Codex enablement.
- The positive Claude control stays mandatory. The success case must fail on
  the current gate and pass with the implementation.

**D7. First container only.**
- Eligibility requires positively established first-container history under
  the posture:
  - the posture's first attestation record predates the predecessor
    generation;
  - no attestation record exists for this PVC from an earlier container that
    launched uid-10003 processes.
- A legacy PVC is ineligible, because "no earlier record" cannot be told apart
  from "unrecorded" there.
- The proof never relies on runtime or kubelet teardown. #3831's accepted
  reliance covers that design's own boundary and does not authorize
  predecessor custody release.
- Consequences:
  - A restarted container, any later pod, and a pod replaced after force
    deletion are all ineligible to run their own pass. That ineligibility
    rests on attestation history, not on any teardown claim, and it fences
    only the replacement's own pass. A's in-flight
    effects while a replacement runs are the D9 and P-2 problem, and stay
    gated there.
  - Posture stickiness: a PVC with a genesis record is permanently tied to the
    posture. The controller refuses any non-posture render on it, or such a
    render invalidates the genesis row so every later container is refused.
    Stickiness is enforced by the controller branch only: a worker-side
    history check can enumerate the attestation directory (which first-
    container eligibility and leftover-tmp recovery need), but a non-posture
    incarnation writes no attestation, so its existence is not visible
    there. The P-5 per-incarnation server registration, once built, is the
    listing source for that. (Post-approval correction narrowing an overclaim
    in D7's consequences; not new owner content.)
  - Missing, unreadable or contradictory history refuses pre-entry.

**D8. ReadWriteOncePod.**
- The opted-in posture requires the data claim to use ReadWriteOncePod,
  rendered by the controller.
- The worker has no kube access. Its service account has zero permissions,
  and the token is not mounted (`controller/internal/kube/render.go`). P0
  therefore consumes controller-read evidence that the bound claim's PV:
  - has access mode ReadWriteOncePod;
  - uses a supported CSI posture, meaning a driver advertising
    SINGLE_NODE_SINGLE_WRITER, from a list the implementation defines.
- This is controller trust, not new worker authority. Reading the bound
  PersistentVolume is, however, new controller authority: a PV is
  cluster-scoped, and at base the controller holds only namespaced Roles plus
  the one owner-authorized `storageclasses` get ClusterRole
  (`deploy/chart/templates/worker-rbac.yaml`). That is pending item P-4.
- Existing claims are ReadWriteOnce (`renderPVC`), so a fresh claim is
  required.
- What RWOP covers:
  - It closes foreign-pod co-mounting only.
  - It does not address prior-container death (D7) or force-detached live
    mounts (D9, P-2).
- The in-pod invariant is scoped to "no container concurrent with the worker
  in this pod". The seed-nix and runner-init init containers are
  non-concurrent.

**D9. Server release fence.**
1. **Incarnation-bound release.** A's predecessor FINAL release carries A's
   worker PROCESS_INCARNATION. The API refuses it once a newer incarnation of
   that worker has registered; A then STOPs with custody kept.
2. **Fence before any write.** A new incarnation B must be fenced BEFORE any
   B activity can write into T, startup helpers included. B's registration,
   or an equivalent durable server record, is ordered before B launches
   anything that could write T. Registration after a possible write is too
   late.
3. **Durable and atomic.** The ordering is durable on the server and atomic
   with the release commit. There is no check-then-release window, and a
   stale A must not regain authority by re-registering.
4. **Lost-ACK replay.** A lost ACK for a release the server already committed
   still replays the original ACK. That case is never described as "custody
   kept".

If the ordering in (2) cannot be established for some lane or startup path,
that case stays gated.

Recorded consequences:
- **The worker lifetime lock is per node.** It is a flock
  (`agent/src/authority-lock.ts`, acquired in `agent/src/main.ts` before the
  boundary record). It does not cover the root entrypoint's writes, runner-init
  or epoch acquisition, all of which run earlier, and it does not cover
  another node.
- **Node registration happens late.** It exists today only in `worker.run()`,
  after the first uid-10003 launches.
- **Restart in the same pod: gated.** A container restart inside the same pod
  does not establish (2). A's release RPC can still be waiting on the server's
  row lock, or committing, after A's process died, and that is not a teardown
  question at all. Init containers do not re-run on a container restart, so
  the P-2 fence never fires, and the restarted container's entrypoint and
  startup uid-10003 helpers run before any registration. D7 makes the restarted
  container ineligible for its own pass, but A's outstanding release is not
  fenced. This path therefore stays gated under the sentence above (pending
  item P-5). The candidate mechanism is in P-5.

## Pending items (NOT approved; the pass stays gated until each is resolved)

### P-1. The runner-uid alias route

**Recommendation:** an owner decision in two parts. (i) Accept other runs'
marked runner-uid agents' direct writes to T into the runner-uid residual
(row (e')). (ii) Accept the alias and rename routes below. Part (ii)'s
argument depends on part (i).

**The routes.**
1. A runner-uid process outside T hardlinks a runner-owned T file into a live
   Codex command's writable clone. The process may be the 2026-11-06 escaped
   writer, or another run's live Claude agent: those carry their own markers,
   are ignored by the quiescence scan while their cwd is outside T, and can be
   steered by repository content.
2. The command opens the link for writing.
3. The runner-uid process removes the alias, so later `nlink == 1` checks pass
   while the command keeps a writable descriptor.
4. A variant renames a T entry into a Codex root and back: T's directories are
   runner-owned and Codex roots are group-`runner` writable, `nlink` stays 1,
   and P6 sees no overlap after the move back.

**Why nothing available closes it.**
- Runner-uid processes are unconfined and own T's files.
- `fs.protected_hardlinks` permits a file's owner to link it.
- cmdsandbox grants writes beneath a command's own root.

The issue says the runner-uid residual "never exempts a command-uid writer".

**Recommendation.** Record a command-uid write that is made possible only by
a runner-uid process's act on T (either route above, by the escaped writer or
by another run's marked agent) as part of the runner-uid residual. Such a
process can already write T directly with the same effect. Adopting this
recommendation explicitly overrides the issue's rule that the runner-uid
residual "never exempts a command-uid writer" for these routes, and widens the
residual's described scope to other runs' marked runner-uid agents. It is not
a restatement of the existing residual. For marked agents the "can already
write T directly" argument holds only once part (i) is accepted.

**Alternative.** Keep the gate until runner-uid processes can be confined.

This is an owner decision under requirement 10, because it means releasing
while a relevant command-uid writer remains possible.

### P-2. Cross-node effect fencing

**Status:** insufficient for local effects; kept gated.

**Why replacement pods are hard to order.**
- Replacement pods are created by the ReplicaSet of the rendered Deployment
  (`RenderDeployment`, Recreate), not by the controller.
- PRD 1860 records that a pod deletion lets the ReplicaSet create the
  replacement while the old pod is still terminating.
- So a controller-written pre-pod record cannot order them.

**Candidate design.**
1. **Fence init container.** The opted-in template's first init container is a
   fence. It registers B's pod UID with the API, using the existing worker
   join token, before any other init container or entrypoint write.
2. **Lease-gated admission.** The API admits that registration only after A's
   server-granted PVC lease has expired or been released.
3. **Fencing tokens.** Every server-side predecessor effect carries A's lease
   epoch as a fencing token and commits only under the serialization protocol
   below. These effects are the capture upload, FINAL and release.
4. **Missing fence.** A template without the fence is an operator TCB fault,
   behind a controller version floor. That covers a manual pod or a rollback
   to a pre-fence controller.

**Why it is insufficient.**
- A's local effects cannot be fenced by the server: journal writes,
  bare-repository writes and local capture files.
- A can pass its last check, be descheduled past lease expiry, and resume a
  local effect while B runs. Clock-drift bounds do not bound scheduling pauses.
- Requirement 3's STOP-before-further-effect therefore fails for local
  effects. A post-expiry local effect is never accepted as evidence, but it is
  still performed.

**What would close it.** Either:
- a proof that every local predecessor effect is non-authoritative and
  harmless with B running concurrently, plus an owner decision accepting the
  deviation; or
- a local-write fence, which does not exist today.

Until then the cross-node case stays gated.

**Named costs, if adopted.**
- A bounded clock-rate drift assumption.
- Up to one lease TTL of startup wait for B.

### P-3. Physical-volume genesis provenance

**Recommendation:** an owner decision on CSI provisioner trust, or keep gated.

**The problem.** D7 depends on the absence of earlier attestations. That
absence means something only if the volume has no legacy history, and a fresh
claim does not prove a fresh volume: `renderPVC` creates a claim but proves
nothing about what gets bound to it.

**Specified protocol for the controller genesis record.**
- **Store.** API Postgres, through an extended hosted controller-to-API surface
  (`api/internal/hostedsvc`).
- **Bindings:**
  - worker id;
  - PVC UID;
  - PV name and UID;
  - CSI driver;
  - `volumeHandle`;
  - storage class;
  - posture version.
- **Ordering:**
  1. Create the RWOP claim.
  2. Await binding.
  3. Verify the PV was dynamically provisioned for this claim:
     `pv.kubernetes.io/provisioned-by`, a `claimRef` UID equal to the PVC UID,
     and a PV created after the PVC.
  4. Verify the `volumeHandle` is absent from every genesis record. Whether
     the PV is a Retain reuse cannot be verified (see below).
  5. Commit a genesis row under the authority-row protocol, with a unique
     `volumeHandle`.
  6. Only then render the posture.
- **Use.** The worker's first attestation names the genesis id.
- **Crash and retry.** Retries are keyed by PVC UID. A duplicate
  `volumeHandle` is refused permanently.

**Residual trust.** This trusts CSI provisioning freshness: that a newly
provisioned `volumeHandle` is backed by fresh storage. It also trusts every
principal able to create a PV, because the provenance annotation, `claimRef`
UID and creation time can all be set by whoever creates a static PV, and a
pre-posture volume reused under a new PV object passes the uniqueness check.
Neither the worker nor the controller can verify that, so it is an owner
decision, and the posture stays gated until it is made. An empty-volume check
at first boot is not specified: the entrypoint and runner-init create entries
on every boot, so this ADR defines no such step.

### P-4. Controller read authority over PersistentVolumes

D8 and P-3 make the controller read the bound PersistentVolume: access mode,
CSI driver, `volumeHandle`, the provisioned-by annotation, the `claimRef` UID
and the creation time. A PV is cluster-scoped. At base the controller holds
only namespaced Roles plus one owner-authorized ClusterRole, `storageclasses`
get (`deploy/chart/templates/worker-rbac.yaml`). A `persistentvolumes` get
ClusterRole is therefore new read authority under requirement 4.

**Recommendation:** approve a get-only ClusterRole on `persistentvolumes`.
In practice this is an unscoped, cluster-wide get: dynamically provisioned
PV names exist only after the role does, so `resourceNames` cannot pre-scope
it, and a ClusterRole binds to the controller's service account, not to
opted-in workers. It is
justified against the smaller options: the worker has no kube access, and no
namespaced object carries the PV fields. **Alternative:** keep the gate. This
is an owner decision under requirement 10.

### P-5. Same-pod container restart

A container restart keeps the pod UID and runs no init container, so the P-2
fence never fires, and the restarted container's startup writes and uid-10003
helpers run before node registration. A's release RPC can still be waiting or
committing at the server after A's process died. D9.2 is therefore not
established for this path, and it stays gated under D9's fallback.

**Candidate mechanism (not yet sufficient).**
- Every PROCESS_INCARNATION makes a durable, superseding server registration,
  carried by its attestation, before ANY of its activity that could write T,
  not only before its first uid-10003 launch.
- While A lives, it keeps T active in its registry until a replay settles an
  unknown release outcome, so a client-side RPC timeout never drops T's
  protection. This does nothing once A's process has died, because the
  registry is in-process.
- Activity that runs before that registration needs a disjointness proof or
  an owner decision:
  - the root entrypoint's writes, which run before any node code (TCB writes:
    root, fixed code, but B activity under D9.2);
  - startup passes in `main()` (`agent/src/main.ts`) that run before
    registration, which today happens inside `worker.run()`. A starting
    point, not exhaustive: pod-epoch consume and reclaim, the boundary-record
    write, outbox initialisation, HOME reclaim, the job-workspace reap, the
    startup Codex orphan reaper (a uid-10003 launch) and the retire-scratch
    reclaim.

### P-6. Replication durability of the server fence

The fence's durability claims hold only if no ACKed commit can be lost on
failover. The API cannot verify replication topology or `fsync` by itself
(see Server durability).

**Candidate mechanism (not yet sufficient).**
- Each fence transaction forces `synchronous_commit`.
- After commit and before ANY ACK, the API checks that every standby in the
  vouched promotion set (not merely one synchronous standby) has flushed past
  the commit LSN. Under `ANY 1 (s1,s2)` a commit flushed only on s1 does not
  pass when s2 may be promoted.
- Single-instance or promotion-set evidence is operator-declared, which adds
  no authority. Any controller read of it (an HA manager's state, a Postgres
  operator resource, or a database connection the controller does not have
  today) would be new controller read authority of the same class as P-4,
  and is not approved.
- The vouched promotion set carries a version that the ACK path re-reads
  after the flush check; a changed version withholds the ACK. This detects a
  change before the re-read only (see the open problem on promotion
  admission below).
- `fsync` cannot be forced per transaction and rests only on that trust.

**Open problems.**
- Replay. A commit whose ACK was withheld is still committed. D9.4 (approved)
  says a committed release replays its original ACK, so a retry would hand
  out the withheld ACK. Closing this needs "committed" for replay to mean
  committed and flushed to the promotion set, with every ACK path, replay
  included, re-running the check. That is a reading of D9.4 the owner must
  confirm.
- A withheld ACK leaves A with an unknown release outcome. A STOPs with the
  outcome unknown and retries replay later; it is never described as
  "custody kept" (D9.4). Named availability cost: if a promotion-set member
  stays behind, every replay is withheld and A keeps T locally until an owner
  discard or the standby catches up.
- Server visibility of an unflushed release. Withholding the ACK does not
  keep the server hold open. The release commit flips the hold to `released`
  and clears `live_worker_id` / `live_run_id` in the same update (ADR-1296,
  the two-lifetime foreign-key model), which lifts the `ON DELETE RESTRICT`
  teardown block for every server consumer before the flush check runs. A's
  local retention cannot restore that restriction. Closing P-6 therefore
  needs every custody and teardown consumer to be unable to act on an
  unflushed release (for example a release that stays non-terminal until the
  flush is confirmed), not ACK gating alone.
- Promotion admission after the final re-read. Nothing serializes a topology
  change between the last version re-read and ACK publication, so a standby
  lacking the commit could join the promotion set and later be promoted.
  Closing P-6 needs promotion admission that preserves acknowledged history
  (a standby is promotable only once it has every ACKed fence commit), or an
  equivalent synchronization mechanism.
- A synchronous wait cut short by the transaction's own timeout commits
  locally. The check above treats it like any unflushed commit.

**Recommendation:** adopt the mechanism only once every open problem above
is closed (the replay reading confirmed, server visibility of an unflushed
release prevented, promotion admission preserving acknowledged history), and
accept the topology and `fsync` trust as an owner decision.
**Alternative:** keep the gate.

## Mechanism: the compound proof (requirement 1)

**Invariant.** On an eligible worker, from pre-entry through the release ACK,
no uid-10003 process can change the content, the tracked mode or the
namespace (create, rename, unlink) of any entry of a Claude T. This is subject
to P-1 to P-6. It rests on four facts:
- **(i)** Confined uid-10003 domains have rule inodes that do not overlap T.
  This covers commands, fileop, checker fileop, and the app-server once it is
  confined.
- **(ii)** uid 10003 owns no entry of T (P4), so no uid-10003 process can
  chmod or chown it.
- **(iii)** Every non-directory entry of T has `nlink == 1` (P4). That covers
  aliases persisted on the PVC by earlier incarnations. Landlock checks a
  symlink's resolved path.
- **(iv)** Unconfined uid-10003 helpers run only on targets whose dev:ino does
  not overlap T, and they resolve paths fd-relative, beneath-root and without
  symlinks. This rule is called H-A: a path-walking uid-10003 helper (today
  `purgeChildrenAsAgents` in `agent/src/rmtree.ts` and the provisioning wraps
  in `agent/src/codex/launcher.ts`) makes the posture ineligible until it
  moves to the pinned walk that `rmtree.ts` already uses.

**Policy v2** names the tightened confinement policy (D1) as one unit:
`required` mode; Landlock ABI ≥3 with the existing deny probe; the D2 seccomp
filter with its bypass audit and in-sandbox check; the narrowed `/dev` grant;
and the inherited-descriptor audit. Base cmdsandbox has no policy versioning,
so the version is new and is recorded in every attestation.

xattrs and timestamps on runner-owned entries are outside the git-visible
inventory the archive preserves. The implementation pins that the capture
format keeps neither. A change to either can force only a skip or a STOP,
which is availability, never an integrity loss.

### Proof steps

**P0. Posture and lane probe.**
- It runs at entry and at every P6, and must positively establish all of
  these:
  1. the kube hosted lane, with the controller-rendered Docker-free tier and
     the no-co-mount invariant, bound to the rendered worker and this
     PROCESS_INCARNATION;
  2. the local negatives (D5);
  3. the uid split is active;
  4. `required` confinement with policy v2;
  5. Landlock ABI ≥3, which TRUNCATE requires;
  6. seccomp is available;
  7. a positive, controller-rendered lane value, not a UUID in `UZI_POD_UID`
     alone;
  8. no hostPID and no shareProcessNamespace in the render evidence;
  9. controller-read RWOP and CSI evidence (D8);
  10. controller genesis (P-3) and first-container history (D7);
  11. the mandatory boundary record and attestation, present and MAC-valid;
  12. a current, unsuperseded server generation;
  13. a live lease epoch.
- Compose, or any missing, stale or contradictory evidence, refuses
  pre-entry.

**P1. Startup baseline and history (D7, requirement 7).**
- The ADR-1598 `proveNoUser` result is corroboration only and never in-life
  authority.
- Stated assumption, scoped to the current container's life: tini is PID 1,
  and each container has a single worker process.
- Nothing about custody or eligibility rests on any teardown.
- The current container must be the first uid-10003-launching container on a
  posture-genesis PVC.

**P2. Enrollment gate.**
- It applies only on opted-in workers, so default workers are unchanged.
- **Attestation.**
  - A MAC'd posture attestation, bound to the PROCESS_INCARNATION boundary
    record (#3831 §6(b)), is published per the protocol below.
  - It is published after the posture facts are known and before the first
    uid-10003 launch.
  - The first attestation names the genesis id.
  - The genesis-creating container counts as a uid-10003-launching container.
  - A failed write of the boundary record or the attestation blocks every
    uid-10003 launch for that process. There is no withdraw-and-continue.
- **Launch gating.**
  - Every COMMAND_UID launch is gated at the spawn wrappers: the
    `commandRootCommand` callers plus the direct setpriv kill in
    `agent/src/runner-uid.ts`.
  - The gate is not at `setprivArgsForUid`, which only builds argv.
  - The inventory is keyed on the resolved uid and classified per spawner,
    because `identity: "command"` means the runner uid on the GitCache spawner
    but uid 10003 on the Codex lane.
- **Enrollment.**
  - Each helper launch keeps its handle and target dev:ino until it exits.
  - So a helper that was abandoned but is still live stays enrolled.
- **Named TCB.** tini and the worker process tree: every holder of the ambient
  setuid capability.
- **Inventory pin.** The implementation pins the launch-site inventory by
  symbol and setpriv argument with a static test. It does not use a
  call-syntax grep, which misses uses that pass the function as a value.
- **Advertisement.** The posture capability advertisement is new.

**P3. Confinement.**
- The opted-in posture refuses rather than running any command unconfined.
- The Codex app-server root, today unconfined as uid 10003, must run in a
  domain that:
  - grants write on no directory that an unconfined uid-10003 helper resolves
    paths through;
  - carries the D2 filter;
  - meets ABI ≥3 and passes the deny probe;
  - has its inherited descriptors audited (stdio, control fd 3, evidence
    fd 4).
- Each helper below is listed with its proof obligation: show it confined, or
  show its target is disjoint from T and its resolution is pinned.
  - retained readiness;
  - the provisioning wraps;
  - the rmtree, purge and teardown passes;
  - the reaper modes;
  - the `--hold-cache` mode (closed today: fd-pinned, with a 10003-owned 0700
    root);
  - the kill, `cat` and `accessSync` probes.

  Known uid-10003 launch sites at base 401cb4561. This list is a STARTING
  POINT, not exhaustive: the search skipped oversized files, and value uses of
  `commandRootCommand` escape a call-syntax grep. The implementation's static
  inventory test is the exhaustive pin.

  | Site (under `agent/src/`) | What runs as uid 10003 | Obligation |
  |---|---|---|
  | `codex/launcher.ts` app-server root | Codex app-server, unconfined except for cross-check | Confine (P3) |
  | `codex/launcher.ts` effect roots | commands and fileop under cmdsandbox | Policy v2 |
  | `codex/launcher.ts` reaper roots; `run-quiescence.ts` reaper spawn | reaper modes | Show disjoint and pinned |
  | `codex/launcher.ts` provisioning wraps | stat, `mkdir`, `rm -rf` of command roots | H-A, or ineligible |
  | `codex/launcher.ts` `defaultKillAsCommandUid` | `kill -KILL` | No write |
  | `codex/launcher.ts` `spawnStandaloneMode` | `--hold-cache`, `--remove-cache`, `--measure-cache`, `--reap-orphans` | Hold-cache closed; others show disjoint |
  | `provision.ts` `probePath` | `node -e` `accessSync` probe | Read-only |
  | `runner-group-trust.ts` | `/bin/cat` status probe | No write |
  | `rmtree.ts` `purgeChildrenAsAgents`, `agentWrappers`, removal wrappers | recursive delete, measure, teardown passes | H-A plus the P6 veto |
  | `runner-uid.ts` `killCommandRootPid` | direct setpriv kill | No write |
  | `git.ts` retained-readiness helper | `node -e`, unconfined | Codex-only today; show disjoint |
  | `codex/broker.ts`, `codex/fileop-client.ts`, `codex/cross-check.ts`, `harness.ts` | module-level uses | Classify per spawner |
  | Transitive: `home-reclaim.ts`, `job-workspace.ts`, `run-caches.ts`, `disk-reclaim.ts`, `cross-check-runner.ts`, `code-cross-check-replay.ts` (Claude-run checker fileop), `codex/codex-executor.ts` | callers of the above | Inherit the callee's obligation |

**P4. Ownership and alias walk.**
- A new fixed runner-uid helper does a complete lstat walk of T, including
  `.git` and ignored entries. Claude has no capture lane, so this is a new
  launch site, not a new authority class.
- It refuses on:
  - any uid-10003 owner, or any owner other than runner where the inventory
    does not already allow it;
  - any non-directory with `nlink > 1`;
  - any FIFO, socket or device node;
  - any unreadable or raced entry.
- The bare repository reached through alternates is outside uid-10003 write
  access by DAC.

**P5. Descriptor closure (D2).**
- **Filter installation.**
  - The filter compares only the low 32 bits of the domain argument.
  - It is installed on the sandbox's locked thread, or with TSYNC.
- **Bypass audit:** `socketcall`, an i386 and x32 architecture check,
  io_uring, and user namespaces.
- **In-sandbox check before exec.** `socket(AF_UNIX)` must fail with the
  expected errno, mirroring the existing Landlock deny probe.
- **Stdio.** The worker endpoint of the stdio socketpairs never sends
  SCM_RIGHTS and never receives cmsg.
- **Control fd 3 and evidence fd 4, after launch.** The filter blocks only new
  `socket(AF_UNIX)` calls, so an inherited AF_UNIX descriptor could still
  receive an `SCM_RIGHTS` message after launch. Before the confined process
  runs, each of fd 3 and fd 4 must be closed, shown to be a non-socket type,
  or shown to have a peer that never sends descriptors (the same rule as the
  stdio endpoint). Until that holds, D2 closes only new socket creation, and
  the pass is not enableable.
- **ptrace and dumpability.** Landlock's cross-domain ptrace restriction
  applies. The supervisor and `--hold-cache` are nondumpable.
- **`/dev`.** The `/dev` grant is narrowed, or P0 asserts that no device node
  of T's filesystem is visible.

**P6. Late-invalidation fence.**
- Before every predecessor effect and before the release RPC, it re-runs P0
  and P4.
- It refuses on a dev:ino overlap (equal, ancestor or descendant) between T
  and any of these, taken from the enrollment registry, not a process listing:
  - a rule inode (`--root`, `--tmp`, `--cache`, `/dev`) of a live enrolled
    root;
  - a live enrolled helper's target.
- **Own effects.** The pass's own effects are positively classified: runner-uid
  clone reads, and worker-uid bare and record effects.
- **Registry lock.** It is held from the check through each effect, and
  enrollment refuses any overlap with an active T.
- **Server effects.** Every server effect is gated by the serialization
  protocol, with authorization at a post-lock `clock_timestamp()`.
- **Destroy veto.** Unconfined uid-10003 passes also get a synchronous
  T-overlap veto in the existing `beforeDestroy` hook.
- **On change.** A change after entry is a STOP (`quiescence_failed`).
- **Still open.** The cross-node local-effect window is P-2 and stays gated.

### Interval, worker death and legacy

- **Interval.** The interval covered runs from the pre-entry check, through
  every coordinator source check, to the server's hold release (the FINAL
  ACK). After release, [ADR-2417](2417-guarded-local-retention.md)'s
  `source_protected` and retirement rules govern.
- **Worker death before entry** is a clean skip with zero predecessor effects.
- **Worker death after entry.**
  - The next process is a new container, which D7 makes ineligible on that
    PVC.
  - The hold stays retained or parked under ADR-2417.
  - Replay never completes a predecessor release, except that a release the
    server already committed replays its original ACK (D9.4; under P-6's
    pending reading, only once it is flushed to the promotion set).
  - Named availability cost: one crash after entry disables the pass for that
    PVC.
- **Legacy eligibility.** Any unattested uid-10003 launch makes the
  incarnation ineligible, and so does any pre-posture history.

### Kernel and runtime assumptions

- Linux.
- Landlock and seccomp are present, and probed.
- The command uid holds no capabilities (setpriv, no_new_privs).
- Within the current container, tini is PID 1.
- No cgroup reliance.
- Lanes:
  - kube only, with a positive controller render;
  - compose and Docker-capable lanes are ineligible;
  - a single-uid worker has no command uid, which the implementation must
    establish, else the worker is ineligible.
- Postgres durability and replication topology as in Server durability,
  resting on pending P-6.
- CSI provisioning freshness (P-3, pending).

## Durability, serialization and recovery

### Server serialization protocol (D9.3, P-2, P-3)

**Repository precedents.**
- **Lock before admission.** `api/internal/store/checkpoint_publish_admission.go`
  runs READ COMMITTED, then `GetRunByIDForUpdate` before
  `CheckLiveCheckpointPublishAdmission`.
- **Fresh clock.** `LeaseClockNow` is `SELECT clock_timestamp()`
  (`api/internal/store/hosted_workers.sql.go`), and `TestLeaseClockNowLiveDB`
  asserts it advances past the transaction-start `now()`.

**The authority row.**
- One authority row per worker PVC holds:
  - the current generation;
  - the current pod UID and incarnation;
  - the lease epoch;
  - the lease expiry;
  - the genesis id.
- A child table keyed to that row holds the superseded pod UIDs and
  incarnations.

**Each competing transition** runs as one transaction, in this order. The
competing transitions are registration and supersession, lease grant, renew
and release, each fenced predecessor effect, release, and genesis bind.
1. `SELECT ... FOR UPDATE` on the authority row.
2. **After** the lock is acquired, read a fresh instant `t_auth` with
   `clock_timestamp()`. Never use `now()`, which stays fixed at transaction
   start, and never the worker's clock.
3. Re-check state under the lock at `t_auth`.
4. Write, then commit.

**Linearization point.** The authorization point is `t_auth` under the held
lock.
- An effect or release is valid only if all of these hold:
  - A's pod UID, incarnation, generation and lease epoch equal the row's;
  - A is not superseded;
  - `lease_expires_at > t_auth`.
- Validity carries through commit, because every transition that could
  invalidate it must take the same row lock first.
- An effect that waited for the lock across expiry reads a `t_auth` after
  expiry and is refused.

**Registration and expiry.**
- B's registration reads its own post-lock `t_auth`.
- It proceeds only if `lease_expires_at <= t_auth` or the lease was released.
- It then increments the generation and the epoch, and records A as
  superseded.
- Expiry is never applied by a background writer. It is evaluated at each
  transition's `t_auth`.

**Replay precedence.**
- The release-idempotency lookup runs first, under the lock. The key binds a
  digest of the hold, generation, inventory, incarnation and lease epoch; a
  stored entry with the same key but a different digest is an integrity
  refusal.
- For a PVC bound to an authority row, a predecessor FINAL or release that
  arrives without a valid fencing token is refused. Today the server accepts
  any older-generation FINAL (ADR-1296 #2969 amendment), so a missing token
  must refuse rather than fall through.
- A committed release returns its original ACK (D9.4; see P-6 for the
  pending flushed-commit reading).

**Conflict handling.**
- Each authority-row transaction sets its own `SET LOCAL lock_timeout` and
  `statement_timeout`, as existing recovery paths do; the pool sets none
  globally. A wait that hits either is a refusal.
  A STOPs, and B's fence-init retries.
- A serialization or deadlock error is a refusal. A predecessor effect is
  never silently retried.
- Lock order: fenced predecessor paths already lock the `workers` and `runs`
  rows (`api/internal/recovery`). Every path that takes the authority row
  follows one global order: authority row, then worker, then run, then hold.
  A deadlock is still a refusal, so a violation costs availability only.

**Verification** uses synchronized LiveDB tests, with a hook holding the row
lock.
- **Competing pairs.** Each asserts that exactly one order commits:
  - effect versus registration;
  - effect versus lease expiry;
  - release versus supersession;
  - renew versus registration;
  - replay versus supersession;
  - re-registration of a superseded A;
  - genesis bind versus a duplicate `volumeHandle`.
- **Lock held across expiry.** A lock is held past `lease_expires_at`, and the
  waiting effect and release must be refused.
- **Clock guard.** A clock-guard test mirrors `TestLeaseClockNowLiveDB`.

### Server durability

**Failure model.**
- **API process crash.** Committed transactions survive.
- **Database failover.** The chart supports more than one Postgres instance
  (`deploy/values/ci-render.yaml` sets three), and replication is
  asynchronous unless synchronous replicas are configured. With asynchronous
  replication a failover can lose a committed supersession, release or
  genesis row after its ACK was sent. The posture therefore requires that no
  ACKed fence commit can be lost on failover: a single instance, or
  synchronous replication whose quorum covers every standby that failover may
  promote. The API cannot verify that by itself:
  - a settings `SHOW` cannot tell async replicas from a single instance, and
    `fsync` or `synchronous_standby_names` can change by reload after any
    check;
  - `pg_stat_replication` lists only connected standbys, so a disconnected or
    lagging promotable standby, a cascading standby or storage-level
    replication looks like a single instance;
  - which standbys may be promoted is decided by the HA manager, not by
    Postgres.

  This is pending item P-6, whose candidate mechanism and open problems
  (replay of a withheld ACK, promotion-set coverage, server visibility of an
  unflushed release, promotion admission after the final re-read) are
  stated there. Until
  it closes, the server fence's durability claims are not established.
- **Storage or power loss.**
  - This relies on Postgres `fsync=on` and `synchronous_commit=on`.
  - These are defaults: no override was found in the chart values, the compose
    file or the store package, though that search was not exhaustive.
  - The implementation forces `synchronous_commit` per fence transaction. The
    `fsync` setting and replication topology rest on P-6, and the posture
    refuses where that evidence is missing.

**Recovered state.**
- **Before commit:** nothing changed.
- **After commit, before ACK:** a retry keyed by pod UID, effect key, release
  key or PVC UID returns the stored result (for a release, under pending
  P-6, only once flushed to the promotion set).

**Verification:** crash-between-commit-and-ack injection, the forced
`synchronous_commit`, and the P-6 promotion-set flush check on every ACK path.

**Not claimed:** a process-crash test is not proof of power-loss durability.

### Attestation publication (P2)

**Protocol.** The boundary-record order (`agent/src/restore-boundary.ts`
`writeBoundaryRecord`):
1. fsync the data root;
2. create the private directory and fsync its parent;
3. write a tmp file and fsync it;
4. `link` it into place, refusing EEXIST, then unlink the tmp;
5. fsync the directory;
6. read the record back and compare.

**Failure model:** process crash, and PVC storage or power loss.

**Recovery.**
- A complete record is MAC-verified.
- A leftover tmp, a MAC failure or an unreadable entry counts as an earlier,
  possibly launching container, so the PVC becomes ineligible.

**Ordering:** the record is durable before any uid-10003 launch. Durability
comes from the fsync steps; the read-back only confirms the published bytes.

**Verification:** fault injection at each step, through the existing ops
seam.

## Why the rejected counterexamples cannot defeat this (requirement 2)

The proof takes no process listing, pid count or counter position as
evidence.

1. **Fork behind the cursor after a wrap.** A child forked behind a scan
   cursor after a pid wrap inherits its parent's Landlock domain and seccomp
   filter. It gains no write path to T.
2. **Full `ns_last_pid` cycle.** A full cycle changes nothing measured.
3. **Failed clones and thread pids.** These are irrelevant: confinement is per
   credential and inherited by every thread after exec.

The P6 registry is the worker's own enrollment of what it launched, not a
scan.

## Fail-closed observations (requirement 3)

Each observation below refuses on an unreadable, partial, raced or
unsupported result. Before scope entry the refusal is a clean skip with zero
predecessor effects; once entered, it is a STOP (`quiescence_failed`).
Absence is never inferred.

| Observation | Refuses on |
|---|---|
| Each P0 item: lane, tier, no-co-mount, each local negative, uid split, posture, Landlock ABI, seccomp | Missing, stale, contradictory |
| Attestation and boundary record | Absent, MAC failure, leftover tmp, unreadable |
| Enrollment registry | Unavailable, inconsistent |
| P4 walk | Owner, nlink, entry type, unreadable, raced |
| P5 in-sandbox self-check | Unexpected success or errno |
| P6 overlap | Any overlap |
| RWOP and CSI evidence | Missing, unsupported |
| Controller genesis | Missing, duplicate `volumeHandle`, provenance failure |
| First-container history | Earlier record, legacy, unreadable |
| hostPID / shareProcessNamespace evidence | Present or unknown |
| Registry lock | Not held |
| Lease epoch at post-lock `t_auth` | Expired, mismatched |
| Authority-row lock wait / serialization error | Timeout, error |
| Server generation | Superseded, not current |
| Release fence | Refusal (STOP) |
| Committed release replay | Returns the original ACK, never "custody kept"; under pending P-6, only once flushed to the promotion set |
| Replication durability (P-6) | Missing topology evidence, or the promotion set not flushed past the commit LSN before any ACK, replay included (pending P-6) |
| Unorderable lane or startup path | Gated |

## Authority (requirement 4)

- **No new worker kill, read or identity-switch authority.** Landlock and
  seccomp are unprivileged self-restrictions.
- **New controller read authority (pending P-4).** Reading the bound
  PersistentVolume (D8, P-3) needs a cluster-scoped `persistentvolumes` get
  that the controller does not hold today. The PV read is named here, justified in
  P-4, and not approved.
- **New controller read authority (pending P-6).** Any controller read of
  database topology would also be new read authority. It is named in P-6 and
  not approved.
- **Rendered fields.** New rendered fields travel the existing pod-spec and
  downward-API channel. The worker trusts the controller, and that trust is
  named.
- **Server mechanisms.** The server fence, lease and genesis only add
  refusals and waits.
- **Owner-approved:** D1 to D9. D8 requires a PV read whose authority is
  pending under P-4.
- **Pending:** P-1 to P-6.

## Authority interface (requirement 9)

`PredecessorWriterExclusion`: evidence per (run, generation, source inventory
entry), plus `reverify()` for P6.

**Bindings:**
- worker id;
- pod and render identity;
- PROCESS_INCARNATION;
- container identity (#3831 §3.0);
- clone path and dev:ino;
- head;
- policy version;
- P4 walk digest;
- lease epoch;
- server generation;
- genesis id.

**Consumers:**
- this pass;
- the retained-predecessor case in
  [ADR-4007](4007-terminal-preservation.md), "writer exclusion is #4001's",
  for Claude only until Codex is enabled;
- #3851's deferred incomplete-tree removal (PRD 3431 Decision Log,
  2026-11-10).

**Location-bound bindings for #4494 Direction 1.** PROCESS_INCARNATION, the
container's PID-namespace identity, clone path and dev:ino, the worker-key MAC
on attestations, the PVC and PV identities and the genesis `volumeHandle` are
all bound to one worker pod and its volume. Run state separated from worker
pods would need each to be re-established at its new location. This ADR adds
no portability analysis or infrastructure, and nothing here depends on #4494.

## Coordination (requirement 7)

- **#3851.** #3851 closed through PR #4148 and left incomplete trees
  ineligible. Their removal consumes the interface above.
- **Startup proof.** ADR-1598's startup proof is reused only as P1's baseline,
  where its fresh-namespace, no-wrap assumptions hold. It never becomes in-life
  authority.

## Coverage-based release, evaluated and not adopted (requirement 8)

Coverage-based release means releasing at an authenticated cutoff without
excluding later writers. It is evaluated here as an alternative hypothesis
under today's contracts. #4494 Direction 1 motivates it but supplies nothing.

**Inventory and evidence.**
- **Inventory.** The claim and generation inventory, original roots and heads,
  are in the recovery journal records.
- **Content.**
  - Uncommitted and ignored content is under today's exclusions:
    `.uzi/scratch/` is excluded.
  - A predecessor-only `ignored_residue` gate skips when other ignored files
    exist.
- **Evidence.**
  - Captures are authenticated by the ADR-1296 release evidence classes, with
    saved archive FINAL requests replayed.
  - A saved manifest proves what was captured, not that nothing was written
    later.

**The cutoff.**
- No component today is authorized to establish an authoritative cutoff that
  makes later writers irrelevant.
- Completeness through release cannot be proven without the writer exclusion
  this ADR defines.
- Stable attribution of post-cut bytes to a generation has no mechanism.

**Capture versus release-authorizing evidence.**
- An available capture is not release authority. Only the ADR-1296 release
  evidence classes close a hold. D5's thin archive FINAL refusal was narrowed
  by #3727 C2 to a pinned prerequisite closure (ADR-1296's 2026-11-01
  amendment); thin availability alone supplies no release authority.
- A cutoff would therefore need release-authorizing evidence at the cut, not
  merely a capture, plus complete original root and head attribution for
  every claim generation through release. Today nothing binds post-cut bytes
  to a generation, so that attribution cannot be preserved.

**Closing a hold, retirement and remaining writers.**
- **Closing a hold** permits custody accounting to drop the hold
  ([ADR-2445](2445-custody-admission-accounting.md)). It also clears the
  hold's live custody references, which lifts the server's teardown
  restriction (ADR-1296, the two-lifetime foreign-key model). It is more than
  accounting relief.
- **Consumer protection.** ADR-2417 still retains sibling generations, shared
  owed pins and metadata that has consumers. A cutoff release would have to
  preserve those retention obligations through release, and no mechanism
  today ties them to a cutoff.
- **Retirement still forbidden.** Local retirement and source reuse remain
  forbidden while the run executes: the record is `source_protected`, and a
  pin authorizes no retirement (ADR-2417's #2969 amendment).
- **Writers that still matter.**
  - Any writer able to modify the source can still affect later attempts that
    adopt it, other runs sharing the repo directory, and shared caches.
  - Excluding their post-cut output from preservation is an owner decision
    under requirement 10, and it was not taken.

**Missing authority.**
- a cutoff authority;
- a post-cut attribution mechanism;
- a retirement interlock keyed on the cutoff.

## Decision boundary (requirement 10)

**Alternatives considered:**
- **DAC revocation (chmod).** Defeated by owner chmod of uid-10003-owned
  entries and by already-open descriptors.
- **Per-run uids or groups.** Rejected: needs identity-switch authority.
- **Mount or user namespaces.** Rejected: needs namespace authority.
- **cgroup freeze.** Ruled out by #1862.
- **Coverage cutoff.** See the section above.
- **Landlock ABI 6 or 9 alone.** Insufficient, or unavailable.
- **A mode digest.** Rejected (D3).
- **A Docker residual.** Rejected (D4).
- **A port-wide network deny.** Breaks package downloads.
- **A controller pre-pod record.** Not achievable for ReplicaSet pods.
- **A lease without fencing tokens.** Rejected.
- **Transaction-start `now()` lease checks.** Rejected in favour of post-lock
  `clock_timestamp()`.
- **Chosen:** the opt-in restricted posture with seccomp descriptor closure.

D1 to D9 are owner-approved. P-1 to P-6 are recommendations or open
problems, pending owner decision or a missing mechanism. The production gate
stays.

## Independence (requirement 11)

The contract stands on today's architecture. Nothing in it depends on #4494.

## Controls the implementation issue must carry (requirement 5, real uid split)

**Refusal.** Each of these makes the pass refuse:
- a live uid-10003 writer into T: another run's command, the app-server, or a
  lingering helper;
- a uid-10003-owned entry;
- a pre-planted hardlink from a Codex clone;
- a FIFO or device.

**Success (mandatory, D6).** An eligible Claude predecessor hold is archived
and released with an unrelated Codex run live on the same worker. This
regression must fail on today's `command_uid_unproven` gate and pass with the
implementation, so an always-refusing mechanism fails acceptance.

**Late arrival or invalidation.** After scope entry, either STOPs before any
further predecessor effect.

**Targeted mutants.** On the refusal and late-invalidation guards, since they
may already hold on base.

**Bounded, deterministic counterexample controls.**
- A fork behind the cursor in a private PID namespace.
- `socket(AF_UNIX)` through each route:
  - native;
  - `socketcall`;
  - i386 and x32;
  - io_uring;
  - a high-bit domain argument.
- The inherited-descriptor audit.
- uid-10003 chmod and chown of runner-owned files denied.
- Skips for each of:
  - a Docker-wired worker;
  - a compose worker;
  - contradictory render evidence;
  - a non-RWOP claim;
  - a restarted container;
  - a legacy PVC;
  - hostPID evidence;
  - missing Landlock or seccomp;
  - a claim without controller genesis.
- A symlink-swap racer against each unconfined helper.
- A record-write failure blocking every uid-10003 launch.
- A replacement pod after force deletion is ineligible.
- Supersession and replay:
  - a newer incarnation makes A's release refuse and STOP;
  - a committed release replays its ACK after supersession;
  - a superseded A re-registering is refused.
- B's fence registration just after A's P6 check blocks until A's lease ends.
- A paused after its final check, past expiry:
  - server effects are refused;
  - the local effect is documented per P-2's outcome.
- The synchronized rename-in, rename-out race (P-1), documented per its
  outcome.
- A same-pod container restart while A's release is outstanding (P-5).
- A posture-off render on a genesis PVC refused, or invalidating genesis.
- A non-tokened predecessor FINAL refused; a replay key with a different
  digest refused.
- A fence transaction runs with `synchronous_commit = on` even when the
  session, role or database default is off.
- An ACK is withheld unless every standby in the vouched promotion set has
  flushed past the commit LSN, including a topology change between commit and
  the flush check, and before the final version re-read (P-6). The window
  after that re-read is the promotion-admission open problem.
- A replay after a withheld ACK, with any promotion-set standby still
  behind, is withheld
  again (P-6, subject to the owner's D9.4 reading).
- The synchronized open-then-unlink alias race (P-1), documented per its
  outcome.
- The synchronized row-lock races above, including a lock held across
  `lease_expires_at`.
- Crash-between-commit-and-ack replays for registration, effects, release and
  genesis.
- Reused-volume refusal.
- Genesis crash and retry.
- Attestation fault injection at each publication step.

No global pid-wrap stress. In the docker-run M4 job the kube claim, render and
registration inputs come from fixtures. That is a test-only seam: it proves
input handling, not hosted behaviour.

## M4 (requirement 6, D6)

**New leaves in `e2e/worker-uid`.** Each needs an `inventory.mjs` entry with
literal leaves, `run.sh` invocations, and `lint:shell`.
- Claude sibling sources, including the quote-collision and ignored-residue
  cases.
- The Codex closed-pipeline single source as a refusal leaf. It skips with
  `command_uid_unproven`, RUNs and PASSes, and asserts zero predecessor
  effects.

**Deferred.** The positive Codex leaf, with Codex enablement.

**CI.**
- The leaves probe Landlock and seccomp, and RUN without skips in the existing
  `test-agent-worker-uid` job. Its `worker_uid` path filter covers `agent/**`
  and `e2e/worker-uid/**`.
- `timeout-minutes: 30` is the real limit.
- Raising it is a maintainer-only workflow step, since a uzi run cannot push
  `.github/workflows/**`.

## Dependencies, approvals and implementation obligations (ready to copy into the implementation issue)

**Prerequisite.** Each pending item is closed and recorded as an amendment to
this ADR:
- P-1, P-3 and P-4 by owner decision;
- P-6 by a promotion-set flush check on every ACK path including replay,
  the owner's confirmation of that D9.4 reading, server-side visibility rules
  so no custody or teardown consumer acts on an unflushed release, promotion
  admission that preserves acknowledged history, and an owner decision on the
  topology evidence source and `fsync` trust;
- P-2 by a proof that every local predecessor effect is non-authoritative and
  harmless with B running, plus an owner decision accepting that deviation, or
  by a local-write fence (none exists today);
- P-5 by the per-incarnation superseding registration ordered before any
  activity that could write T, the live-A registry rule, and a disjointness
  proof or owner decision for every pre-registration startup activity (root
  entrypoint, worker-uid startup passes, and the startup uid-10003 Codex
  orphan reaper).
Approval alone does not close P-2, P-5 or P-6.

**Dependencies:**
- **External and records.**
  - #1934;
  - P-4's `persistentvolumes` get ClusterRole, if the owner approves it;
  - the #3831 boundary records.
- **Worker side.**
  - the enrollment registry;
  - the attestation publication protocol and the first-container history
    reader;
  - the seccomp filter and probe;
  - the D5 evidence reader;
  - pinned fd-relative resolution for every unconfined uid-10003 helper;
  - Codex app-server confinement;
  - spawn-wrapper gating keyed on the resolved uid;
  - the fence init container;
  - a durable, superseding server registration by every PROCESS_INCARNATION
    before any activity that could write T (P-5);
  - keeping T active in a live A's registry until replay settles an unknown
    release outcome (P-5).
- **Controller side.**
  - the controller genesis record at claim creation, with provenance checks
    and `volumeHandle` uniqueness;
  - the RWOP render and controller-read bound-claim and CSI evidence;
  - the lane, tier, no-co-mount and no-hostPID/shareProcessNamespace render.
- **Server side.**
  - the authority-row serialization protocol: row lock first, then post-lock
    `clock_timestamp()` authorization via `LeaseClockNow`;
  - the lease, epoch fencing tokens, generations, superseded set, conditional
    release, replay precedence and conflict handling;
  - forced `synchronous_commit`, the promotion-set flush check on every ACK
    path, the versioned promotion set, server visibility rules for an
    unflushed release, promotion admission preserving acknowledged history,
    and operator-declared topology evidence (or controller-read evidence, if
    P-6's authority is approved);
  - synchronized LiveDB race, lock-across-expiry and crash-point tests.

**Controls and M4:** as specified above.

**Implementation obligations:**
- update `docs/run-recovery.md` "Known limits", plus `task docs:sync` for the
  embedded mirror;
- an ADR-2417 amendment for predecessor record cleanup;
- a changelog fragment;
- operator documentation for the opt-in posture and its Docker
  incompatibility.

**Landing obligations:**
- full agent suite: exact-head PR CI;
- the new M4 leaves RUN and PASS without skips on exact-head CI;
- CI timing review;
- controller and API tests for the render, fence, lease and genesis.

**Owner acceptance for that issue:**
- real-cluster RWOP and CSI behaviour;
- force-detach ordering;
- CSI provisioning freshness;
- storage honouring fsync;
- database failover under synchronous replication, and the HA manager's
  promotion set (P-6).

**Deferred:**
- Codex predecessor enablement (D3) and its positive M4 leaf;
- the Codex older-sibling source proof;
- the regenerable ignored-path allowlist.

## Consequences

- The retained predecessor FINAL pass stays disabled in production. Older
  generations' guarded holds stay open and counted toward the per-run bound,
  exactly as before (ADR-2445's 2026-11-11 amendment), until an existing
  release proof or discard policy closes them. This ADR changes no custody
  policy.
- When enabled, the pass applies only to Claude predecessors on an opted-in,
  kube, Docker-free, RWOP, first-container worker. Every other worker keeps
  the gate.
- No statement here is a runtime proof. The kernel, CSI and database behaviour
  described is a set of design assumptions, which the implementation's
  controls and M4 must prove.

## Amendment 2026-11-25, #4628: the fresh resume seed's L2 under the bare lock

Decision record (AI-synced 2026-11-25). The fresh resume destination ([ADR-4628](4628-fresh-resume-durable-evidence.md)) writes into a worker's bare repository outside the usual per-flight flow: its seed (step L2) runs under the bare lock, after the refusal checks and `assertBareClear` and admission. A destination whose bare already holds any local state for the branch abandons the transfer instead of seeding. That seed is a new writer of the kind this ADR classifies, and it is covered by the same lock and the same clear check, not by a coverage-based release: "Coverage-based release, evaluated and not adopted" still stands, which is why automatic fresh resume is deferred. The seed is adapted to the inventory source exclusion (#4643, shipped; AI-synced 2026-11-25): `GitCache.seedFreshAttempt` calls `GitCache.refuseExcludedKey` under the held bare lock after the read-only refusal checks and before the first `assertBareClear` and any L0/L1 write; the destination waits once outside the lock through `awaitInventoryExclusionSettled`, retries once, and otherwise abandons and stops nonterminally. #4655's `handlePermanentFailure` ordering is separate and shipped: the fresh pre-seed branch sits after the `holdOrWallParkConfirmed` early return.
