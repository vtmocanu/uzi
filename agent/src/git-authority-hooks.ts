/**
 * issue #3404 M2 U2 — the seam through which GitCache asks the completion-authority store before it
 * changes anything the slot epoch state does not already cover.
 *
 * Contract for every implementer (the runner, over the authority store):
 *  - Every method is called from INSIDE an outer authority transaction the runner established. The
 *    hooks borrow that transaction; GitCache never acquires the authority lock itself.
 *  - A rejected promise is a refusal. GitCache aborts BEFORE the effect it was about to perform.
 *  - Settlement (`settle`) is called only once the effect's child process or in-process await has
 *    been positively joined. When joining is unknown (a spawn error after a possible start, a
 *    timeout the child has not yet closed after) GitCache does NOT settle, so the authority
 *    record stays unresolved and the footprint stays fenced.
 *
 * Wiring: `new GitCache(dataDir, log, retry, { authority, authorityRequired })`. With no hooks and
 * `authorityRequired` false (every test default) behaviour is exactly the legacy one. With
 * `authorityRequired` true and no hooks, every call site below refuses before its effect.
 *
 * Dispatch inventory (file git.ts; symbol; mutable footprint; existing owner/join; why the slot epoch
 * state does not already cover it):
 *
 * admitSourceUse(clonePath, op) — source bytes under a runner clone path.
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
 *    delete. Footprint: every path of the retained record.
 *  Not hooked: retainStoppedAttempt and markStoppedCanonical change bare ledger/marker/journal
 *  metadata only and never touch clone bytes; retireRunnerClone is fenced by the journal's
 *  completion-source fence (expectedFence), not by this hook.
 *
 * beginBareMutation(barePath, op, footprint) -> settlement — a git child that mutates the SHARED bare
 *  and whose outcome the authority must be able to reconcile.
 *  - "recovery-cleanup-delete-ref": GitCache.cleanupRecoveryGeneration's deleteRef
 *    (`update-ref -d`). Footprint: the ref. Join: runGit resolving (exit 0) then a read-back of the
 *    ref; a rejected runGit does not distinguish exit-nonzero from timeout or spawn failure, so it is
 *    left unsettled. The slot epoch covers clone sources, not refs of the shared bare that other
 *    runs' cleanup and pinning also touch.
 *  - "owed-reconcile-delete-ref": GitCache.reconcileOwedCandidates (`update-ref -d` of the owed pin).
 *    Same footprint, join and reasoning.
 *
 * assertBareClear(barePath, op) — refuses while a beginBareMutation record on that bare is unresolved,
 *  so a pin or coverage ref cannot be re-created over a pending (ABA) deletion.
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
 *    ok; any other joined exit settles with the target as residue); an `error` event or an unsettled
 *    timeout leaves the record open, and a late `close` of an unsettled child settles it as residue.
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
