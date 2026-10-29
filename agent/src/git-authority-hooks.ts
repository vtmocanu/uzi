/**
 * issue #3404 M2 U2 — the seam through which GitCache asks the completion-authority store before it
 * changes anything the slot epoch state does not already cover.
 *
 * Contract for every implementer (the runner, over the authority store):
 *  - admitSourceUse, beginBareMutation and beginDestructive are called from INSIDE an outer authority
 *    transaction the runner established. The hooks borrow that transaction; GitCache never acquires the
 *    authority lock itself. assertBareClear is the exception: it is a read-only load of a durable record
 *    that needs no transaction (see its entry below), so a call site whose only hook is assertBareClear
 *    runs without A.
 *  - A rejected promise is a refusal. GitCache aborts BEFORE the effect it was about to perform.
 *  - Settlement (`settle`) is called only once the effect's child process or in-process await has
 *    been positively joined. When joining is unknown (a spawn error after a possible start, a
 *    timeout the child has not yet closed after) GitCache does NOT settle, so the authority
 *    record stays unresolved and the footprint stays fenced.
 *
 * Implementation: `AuthorityRuntime.hooks` (authority-runtime.ts), built in main.ts over the worker lifetime
 * and passed by buildWorkerGitCache. admitSourceUse, beginBareMutation and beginDestructive REQUIRE the
 * transaction the outermost entry point opened in the current async context and refuse (AuthorityRefusal)
 * when there is none; assertBareClear does not (see its entry below). No hook acquires A beneath B.
 * Every `settle` call is awaited inside the caller's own B section and borrows its transaction, except
 * two that can run with none active: the runner-uid delete's late `close` (a child that outlived its
 * caller) and the continuation GitCache.cleanupArchivedRecoveryPin abandons when its caller's deadline
 * passes (it still holds B). `settle` NEVER acquires A: with no active transaction the settlement is
 * queued in-process and applied by the idle drain `settle` itself schedules (a microtask that acquires
 * A with no lock held and is not awaited), or at the start of the next outermost A acquisition. A failed
 * apply is re-queued a bounded number of times, then logged and left `running`. Until applied the durable
 * record stays `running` (fail-closed); if the process dies first, a successor sees it unresolved.
 *
 * Wiring: `new GitCache(dataDir, log, retry, { authority, authorityRequired })`. With no hooks and
 * `authorityRequired` false (every test default) behaviour is exactly the legacy one. With
 * `authorityRequired` true and no hooks, every call site below refuses before its effect.
 *
 * Dispatch inventory (file git.ts; symbol; mutable footprint; existing owner/join; why the slot epoch
 * state does not already cover it):
 *
 * admitSourceUse(clonePath, op) — source bytes under a runner clone path. The three seeding ops
 *  ("seed-canonical", "seed-attempt", "replace") first call assertBareClear(barePath, op) on the
 *  repository's bare, so a fresh successor is admitted only when its bare footprint has no unresolved
 *  mutation; a refusal there aborts the seed before any effect.
 *  - "seed-canonical": GitCache.runnerCloneForBranch, before freeCanonicalClonePath. Footprint: the
 *    canonical clone dir (deleted then reseeded). Owner: the bare lock. The slot epoch binds a
 *    completion source's identity; a canonical reseed is a new use of the same path.
 *  - "seed-attempt": GitCache.attemptCloneForBranch, before the ledger `live` append and clone.
 *    Footprint: the fresh attempt dir. Owner: the bare lock.
 *  - "normalize": GitCache.normalizeRetainedSourceObjects, after attribution and before the
 *    supervised helper writes object permissions into the clone's objects and the bare's.
 *  - "replace": GitCache.prepareRecoverySuccessor, before the successor ledger/journal writes and
 *    clone. Footprint: the successor path and the journal that names the retained predecessors.
 *  - "discard": GitCache.discardRetainedSource, after the disposition check and before the first
 *    delete. Footprint: every path of the retained record. The guarded operator discard has already
 *    authorized exactly the clone and its paired skills dir, so this is the ONE operator-discard takeover
 *    (AuthorityStore.admitOperatorDiscard): a fresh epoch in state `retiring` whose manifest is that footprint,
 *    whatever state the slot held (a crashed worker's `active` epoch, `ineligible` with settled_failed residue
 *    inside the footprint); only a `running` pending record refuses. The destructive records of the deletes are
 *    residue retries inside that manifest, and ending the source use (AuthorityRuntime.endSourceUse) tombstones
 *    the slot once the footprint is confirmed gone, or returns it to `ineligible` when it is not.
 *  A published source (sealed/claude_ready) is never re-admitted by accident: the runtime passes the
 *  store's explicit consume-the-publication purpose only for "seed-canonical" and "seed-attempt" (they run
 *  after git.ts's journal guards found no journal naming the path). "replace", "normalize" and every
 *  destructive operation never consume a publication; restart retirement (beginRetirement) and the operator
 *  discard (admitOperatorDiscard) are the only other ways a published slot changes.
 *  Not hooked: retainStoppedAttempt and markStoppedCanonical change bare ledger/marker/journal
 *  metadata only and never touch clone bytes; retireRunnerClone is fenced by the journal's
 *  completion-source fence (expectedFence), not by this hook.
 *
 * beginBareMutation(barePath, op, footprint) -> settlement — a git child that mutates the SHARED bare
 *  and whose outcome the authority must be able to reconcile.
 *  - "recovery-cleanup-delete-ref": GitCache.cleanupRecoveryGeneration's deleteRef
 *    (`update-ref -d`, via GitCache.deleteRefSettled). Footprint: the ref. Join: a known exit (0, or a
 *    numeric code with no kill and no signal) then a read-back, settling ok when the ref is gone and
 *    residue when it remains or cannot be read; a spawn failure where no child started (string code,
 *    "spawn" syscall, not killed) settles as residue; anything else (timeout or kill, signal, null code,
 *    non-spawn string code) is left unsettled. That last list is the execFile path: inside a
 *    boundary-process scope the tick spawner reports a joined signal death or its own kill as a numeric
 *    code only after the process group is gone, so it is a known exit. The slot epoch covers clone sources, not refs of the
 *    shared bare that other runs' cleanup and pinning also touch.
 *  - "owed-reconcile-delete-ref": GitCache.reconcileOwedCandidates (`update-ref -d` of the owed pin,
 *    via GitCache.deleteRefSettled). Same footprint, join and reasoning.
 *  - "recovery-pin-delete-ref": GitCache.deleteRecoveryPin (`update-ref -d` of refs/uzi-recovery-pin/
 *    <run>/<gen>, the ref anchorRecoveryHead re-creates). Join: tryGitExit returning a real exit status
 *    (0 or not), then a read-back; null leaves it unsettled (timeout, signal) unless Node's own spawn
 *    failure proves no child ran, which settles as residue.
 *  - "recovery-pin-archive-delete-ref": GitCache.cleanupArchivedRecoveryPin's prepared
 *    `update-ref --stdin` child (same pin ref). Registered after the last eligibility check and before
 *    the child is spawned (a refusal retains the pin). Join: the child's exit status is >= 0 (-1 is
 *    unknown), then a read-back: a committed delete must have removed the ref, an uncommitted abort
 *    must have left no `<ref>.lock`; anything else settles as residue. A spawnGit rejection (no child
 *    object ever existed) settles as residue; a rejected settlement surfaces as AuthorityRefusedError.
 *  Other `update-ref -d` sites swept in git.ts, none in a namespace assertBareClear guards, so none is
 *  hooked: refs/uzi-checkpoints/<key>, refs/uzi-publication-floor/, refs/uzi-incoming/ (the fetch
 *  stage), the tracking-ancestor `candidate` in the archive step (refs/uzi-runner/..., after its
 *  refs/uzi-archive/ copy exists), refs/uzi-cross-check/<run>, refs/uzi-settle/, refs/uzi-recovery-
 *  episode/ (the discard path), refs/uzi-workflow-target/, refs/uzi-secret-scan-floor/, the
 *  RECOVERY_BUNDLE_REF, and clone-local refs (runGitAsRunner / the clone `git` helper). The guarded
 *  namespaces refs/uzi-owed/, refs/uzi-coverage/ and refs/uzi-recovery-pin/ are deleted only by the
 *  four sites above.
 *
 * assertBareClear(barePath, op) — refuses while a beginBareMutation record on that bare is unresolved,
 *  so a pin or coverage ref cannot be re-created over a pending (ABA) deletion, and a seed cannot start
 *  on a repository whose bare has an unresolved mutation. It needs NO transaction: with none active it
 *  reads the bare's slot with a read-only load that never writes (a record written by an owner that is
 *  dead, since W excludes another compatible worker, or by this process's own transactions; the bare lock
 *  B the caller holds does not serialize those, a queued late settlement only relaxes the record), and
 *  refuses fail-closed on a slot one ahead of its head shard instead of completing that crash window.
 *  With a transaction active (the seed and replace sites all hold A) it reads through the transactional
 *  load, which completes that window, so one failed head write cannot block a repository's seeds
 *  permanently. Runner sites that therefore lost their A wrapper (their only hook is this one):
 *  fetchAgentBranch, updateTrackingRef, retainCurrentOwedCandidate (two sites) and anchorRecoveryHead;
 *  in recovery.ts freezeInventory (buildRecoveryCoverage) and the inventory source boundary around it.
 *  - "seed-canonical", "seed-attempt", "replace": GitCache.runnerCloneForBranch, attemptCloneForBranch
 *    and prepareRecoverySuccessor, immediately before their admitSourceUse.
 *  - "owed-pin": GitCache.pinOwedUnderLock (re-creates refs/uzi-owed/<run>/<sha>, which the cleanup
 *    deletes).
 *  - "recovery-coverage-ref": GitCache.buildRecoveryCoverage (re-creates refs/uzi-coverage/...).
 *  - "recovery-pin-anchor": GitCache.anchorRecoveryHead (re-creates refs/uzi-recovery-pin/<run>/<gen>).
 *  Not hooked: the episode pin written by the capture step lives under refs/uzi-recovery-episode/,
 *  which cleanupRecoveryGeneration never deletes.
 *
 * beginDestructive(paths, op) -> settlement — the recursive delete of runner-writable trees.
 *  - "runner-uid-delete": GitCache.runRunnerUidDelete (called by deleteRetainedArtifact and
 *    freeCanonicalClonePath). Under the uid split this is a detached, setpriv-wrapped `rm -rf` that
 *    can outlive its caller (RunnerDeleteUnsettledError); in the single-uid layout it is an
 *    in-process fs.rm. Footprint: the target tree. Join: the child's `close` event (exit 0 settles
 *    ok; any other joined exit settles with the target as residue). A spawn that failed before any
 *    process existed (a synchronous throw, or an `error` event with no pid) settles with the target as
 *    residue. An `error` event after a real start, or an unsettled timeout, leaves the record open, and
 *    a late `close` of an unsettled child settles it as residue. A rejected ok-settlement of a
 *    delete (in-process, or the spawned rm that exited 0) surfaces as an authority error, never as a failed delete.
 */
export interface AuthoritySettlement {
  settle(outcome: { ok: true } | { ok: false; residue: string[] }): Promise<void>;
}

export interface GitAuthorityHooks {
  admitSourceUse(clonePath: string, op: string): Promise<void>;
  beginBareMutation(barePath: string, op: string, footprint: readonly string[]): Promise<AuthoritySettlement>;
  assertBareClear(barePath: string, op: string): Promise<void>;
  beginDestructive(paths: readonly string[], op: string): Promise<AuthoritySettlement>;
}
