// issue #4628: the reader table for the two fresh-resume attempt-ledger states.
//
// `parseAttemptLedgerEntry` (git.ts) used to fail closed on any state outside live|abandoned|retired|reclaimed, and every caller
// treated an unparsed value as unknown evidence for the WHOLE bare. It now accepts `fresh_pending` (written before the fresh seed)
// and `fresh_unbound` (a seed that never reached its receipt commit). Every function that calls the parser, and every consumer of a
// ledger state that decides what to do with it, has one row here. test/fresh-resume-ledger-table.test.ts scans the sources and
// fails when a parser caller has no row, or a row names a function that is gone, so a new reader is a deliberate decision.
//
// The rule behind every disposition: a fresh entry is NEVER adopted, reused, freed or compacted by an ordinary claim. It is kept,
// and only the guarded fresh discard (unit m3-B) retires it.

export type LedgerDisposition =
  /** The caller matches an entry by attempt id, run or path, never by its state: a fresh entry keeps its identity there, so a
   *  conflicting or tainting value is still caught. */
  | "identity_only"
  /** The caller treats any non-retired entry as occupying its key or path: a fresh entry occupies it and is never freed. */
  | "occupies"
  /** The caller is an adoption, seed or reuse route: it never adopts a fresh entry and leaves it untouched. */
  | "set_aside"
  /** The caller refuses (or reports a conflict) when a fresh entry is present. */
  | "blocks"
  /** Retention or compaction: a fresh entry is never dropped, compacted or counted as abandoned. */
  | "kept";

export interface LedgerReaderRow {
  /** `src/<file>.ts#<enclosing function>` of a call to `parseAttemptLedgerEntry`, or the consumer's own symbol. */
  caller: string;
  disposition: LedgerDisposition;
  /** The `parser` and `reader` rows are enforced against the sources (a call of the parser, or of readAttemptLedger or
   *  readAllAttemptLedgers); `consumer` rows decide on a state one of those returned. */
  role: "parser" | "reader" | "consumer";
  note: string;
}

export const LEDGER_READER_TABLE: readonly LedgerReaderRow[] = [
  // ---- parser callers -------------------------------------------------------------------------------------------------------
  { caller: "src/git.ts#freshLocalState", role: "parser", disposition: "blocks",
    note: "the fresh seed's own preemption: any ledger entry of this run on another branch is local state, so the seed is refused" },
  { caller: "src/git.ts#retireLedgerLast", role: "parser", disposition: "identity_only",
    note: "reads one attempt's last canonical value by identity; a fresh value is a canonical value like any other" },
  { caller: "src/git.ts#retireCrossBareConflict", role: "parser", disposition: "identity_only", note: "attempt-id identity across bares" },
  { caller: "src/git.ts#retireIdentifiedAttempt", role: "parser", disposition: "identity_only", note: "extracts the attempt id of a value" },
  { caller: "src/git.ts#retireParsedNamesAttempt", role: "parser", disposition: "identity_only", note: "does a value name an attempt id" },
  { caller: "src/git.ts#scanRunBareConfig", role: "parser", disposition: "identity_only",
    note: "the run-retention scan keeps one identity per attempt id; the fresh entry then reaches judgeRetainedEvidence as active" },
  { caller: "src/git.ts#diagnoseRetainedArtifacts", role: "parser", disposition: "kept",
    note: "an orphaned skills dir of a fresh attempt is kept; a fresh attempt is never counted abandoned" },
  { caller: "src/git.ts#withMaintenanceFamilyLock", role: "parser", disposition: "identity_only", note: "source identity pairing for the maintenance family" },
  { caller: "src/git.ts#attributedRecovery", role: "parser", disposition: "blocks",
    note: "a recovery episode refuses a source or successor id that any ledger entry (fresh included) names with another identity" },
  { caller: "src/git.ts#gatherRetainedUnderLockUncounted", role: "parser", disposition: "identity_only",
    note: "builds each part's ledgerState; evaluateRetained decides what a fresh state means" },
  { caller: "src/git.ts#exactRetiredEmptyJournal", role: "parser", disposition: "identity_only", note: "exact retired shape only; a fresh value is not retired" },
  { caller: "src/git.ts#epochRetirementAttribution", role: "parser", disposition: "identity_only", note: "retirement attribution by attempt id and identity" },
  { caller: "src/git.ts#settleEpochMetadata", role: "parser", disposition: "identity_only", note: "epoch metadata settlement by attempt id and identity" },
  { caller: "src/git.ts#readInventoryCloneHeadsUnderLock", role: "parser", disposition: "identity_only", note: "ledger identity only" },
  { caller: "src/git.ts#readTerminalAttribution", role: "parser", disposition: "identity_only",
    note: "reports each attempt's state verbatim in the terminal attribution; a fresh state is reported, never rewritten" },
  { caller: "src/git.ts#readAcknowledgedReportSourcesUnderLock", role: "parser", disposition: "identity_only", note: "run id and identity checks only" },
  { caller: "src/git.ts#compactAttemptLedger", role: "parser", disposition: "kept", note: "a fresh entry is never dropped, even when its path is gone" },
  { caller: "src/git.ts#readAllAttemptLedgerValues", role: "parser", disposition: "identity_only",
    note: "the shared ledger read behind readAttemptLedger and readAllAttemptLedgers; the callers of those two are the reader rows below" },
  { caller: "src/restore-boundary.ts#enumerateAttributedPaths", role: "parser", disposition: "identity_only",
    note: "adds the entry's path to the attributed set; runAttributionCensus is unchanged and the fresh seed never runs it" },
  { caller: "src/restore-route.ts#classifyCanonicalShape", role: "parser", disposition: "blocks",
    note: "an own-run fresh entry makes the predecessor not plain: the restore route refuses" },
  { caller: "src/restore-route.ts#classifyRestoreRelationships", role: "parser", disposition: "blocks",
    note: "an own-run fresh entry is a conflicting attribution: the restore route refuses" },
  // ---- callers of readAttemptLedger / readAllAttemptLedgers -------------------------------------------------------------------
  { caller: "src/git.ts#attemptCloneForBranch", role: "reader", disposition: "occupies",
    note: "hands the ledger to nonLiveKeyPaths for the seed-time process scan; a fresh entry occupies its path and is never adopted or freed" },
  { caller: "src/git.ts#prepareRecoverySuccessor", role: "reader", disposition: "occupies",
    note: "any entry, fresh included, makes a successor attempt id used (identity_used)" },
  { caller: "src/git.ts#readRemediationUnderLock", role: "reader", disposition: "identity_only",
    note: "matches a grant's origin source by attempt id and identity (sameRecoverySource); the state is not consulted, a fresh entry keeps its identity there" },
  { caller: "src/git.ts#remediationSourceUnderLock", role: "reader", disposition: "blocks",
    note: "the remediation source must be a live entry of the run at its path (state !== live refuses), so a fresh entry refuses" },
  { caller: "src/git.ts#resolveBranchJournalUnderLock", role: "reader", disposition: "blocks",
    note: "a restore stage is valid only with a live or retired ledger entry; a fresh entry throws a restore composite error" },
  { caller: "src/git.ts#seedIndependentAttempt", role: "reader", disposition: "blocks",
    note: "the predecessor must be live (state !== live refuses) and any newer non-retired entry, fresh included, refuses" },
  { caller: "src/git.ts#completionSourceAlreadyRetired", role: "reader", disposition: "blocks",
    note: "answers true only for a retired entry (state !== retired is false), so a fresh entry is never read as retired" },
  { caller: "src/git.ts#clearRetiredCompletionJournal", role: "reader", disposition: "blocks",
    note: "clears only a retired entry, or a live one while retiring; any other state, fresh included, returns false and clears nothing" },
  { caller: "src/git.ts#retireRunnerCloneImpl", role: "reader", disposition: "identity_only",
    note: "matches the owner by run id, path and the flight's own attempt id; one site refuses unless the entry is live, the completion site " +
      "retires the matching entry. A fresh entry is never a live flight's attempt id before its receipt commit" },
  { caller: "src/git.ts#seedFreshAttempt", role: "reader", disposition: "blocks",
    note: "the fresh seed's own re-check: after L0 and L1 the ledger must hold exactly A as fresh_pending, or it refuses" },
  { caller: "src/git.ts#readFreshAttemptEntry", role: "reader", disposition: "identity_only", note: "returns A's own last entry by attempt id for restart discovery" },
  { caller: "src/git.ts#markFreshUnbound", role: "reader", disposition: "identity_only",
    note: "moves only a fresh_pending entry of this run to fresh_unbound; any other state is left as it is" },
  { caller: "src/git.ts#commitFreshReceipt", role: "reader", disposition: "identity_only",
    note: "the receipt commit's own precondition: A's last value is this run's fresh_pending at this path (or already committed)" },
  { caller: "src/git.ts#clearExclusion", role: "reader", disposition: "occupies",
    note: "issue #4632's same-key exclusion clear reads the ledger only to list the probe's target paths (nonLiveKeyPaths / " +
      "restoreSeedSweepPaths): a fresh entry's present path is probed like any other, and nothing is adopted, freed or compacted" },
  // ---- consumers of a ledger state ------------------------------------------------------------------------------------------
  { caller: "src/git.ts#nonLiveKeyPaths", role: "consumer", disposition: "occupies",
    note: "the seed-time sweep names the path of a non-live entry for a process scan only; nothing is deleted or adopted" },
  { caller: "src/git.ts#retainStoppedAttempt", role: "consumer", disposition: "set_aside",
    note: "only a live or reclaimed entry is retained as a stopped source; a fresh entry answers unknown and is kept" },
  { caller: "src/git.ts#classifyOwnerClonePathFromSnapshot", role: "consumer", disposition: "set_aside",
    note: "identity only (run id and path); the orphan reclaim that follows still refuses a path a journal names" },
  { caller: "src/git.ts#evaluateRetained", role: "consumer", disposition: "kept",
    note: "a fresh_pending attempt adds the ledger_fresh blocker unless its id is named in freshTransferNegative (the guarded fresh " +
      "discard); fresh_unbound is already proven negative and adds none, so it is the one fresh state an ordinary discard may retire" },
  { caller: "src/git.ts#discardRetainedSource", role: "consumer", disposition: "kept",
    note: "retires any non-retired entry; reachable for a fresh entry only once evaluateRetained has no blocker, i.e. through the guarded fresh discard" },
  { caller: "src/terminal-inventory.ts#readTerminalSourceUniverse", role: "consumer", disposition: "kept",
    note: "a state other than retired or abandoned whose clone vanished refuses the universe (fail closed), so a fresh attempt is never silently dropped" },
  { caller: "src/runner.ts#freshResumeDiscovery", role: "consumer", disposition: "set_aside",
    note: "a ready journal is decided locally and sets the ledger aside; a seeding journal reads A's entry: fresh_unbound is set aside, " +
      "fresh_pending is reconciled with the server's answer, and a missing entry is cleared only on a proven negative" },
  { caller: "src/ops/retained-source-server.ts#readFreshNegative", role: "consumer", disposition: "kept",
    note: "asks the server about a fresh_pending attempt's own journal transfer only; any other state or failure leaves the attempt blocked" },
  { caller: "src/pod-epoch-run-retention.ts#judgeRetainedEvidence", role: "consumer", disposition: "kept",
    note: "a fresh state keeps the run's retention active (ledger_fresh_pending, ledger_fresh_unbound)" },
  { caller: "src/pod-epoch-reclaim.ts#journalShape", role: "consumer", disposition: "kept",
    note: "the epoch reclaim admits only live and retired shapes, so a fresh entry is counted as an unrecoverable shape and kept" },
];
