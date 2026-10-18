package workersvc

// boundClaimControlService is a synchronous facade over the caller's locked
// queries, not another runtime service. Claim mutation, owner control and auto-stop
// wrappers use it while holding their transaction's run lock.
//
// Child call inventory:
//
//   - requestCompletionPermitLocked, recordCompletionAttemptLocked,
//     setRunCompletionHoldLocked and consumeInputsLocked use q.
//
//   - ReportWallPark, setCheckpointDurability, setLimitWaitLocked and
//     setRecoveryWait use q, p and now (ownerJudgeBindMode also uses q).
//
//   - appendMessagesLocked/foldRunUsage use q; foldUsageTailStamps must wait
//     for the parent's post-commit call, so txBeginner stays nil.
//
//   - SetState's pause_failed/credential_switch/credential_switch_failed arms
//     use q; their parent publishes/notifies after commit.
//
//   - evaluateAutoStopLocked uses q, p, now and the guarded persistFail tracker;
//     its parent owns the transaction and post-commit terminal effects.
//
//   - forgeForRetention uses q, box and forgeBaseURLAllowed; capabilityGate
//     uses q and capabilitySettings; rosterFor and submitScopeCeiling use q.
//
//   - submitInputForRun approval uses q, p and capabilitySettings for admission,
//     roster validation, milestone/contract/budget freeze, enqueue and override.
//     Its caller owns the transaction and checks pending revisions after locking.
//
// Trackers retain their own guarded identity. Job generation, promoter queues,
// sweeper cursors, atomics and locks are fresh zero values, never copied.
// No transaction opener, connection acquirer, background runner, broadcaster or
// lifecycle callback is installed: callers own connections and post-commit effects.
// This facade must not be used for other Service operations.
func (s *Service) boundClaimControlService(q Store) *Service {
	return &Service{
		q:                   q,
		box:                 s.box,
		p:                   s.p,
		now:                 s.now,
		capabilitySettings:  s.capabilitySettings,
		forgeBaseURLAllowed: s.forgeBaseURLAllowed,
		persistFail:         s.persistFail,
		outbox:              s.outbox,
		quarantine:          s.quarantine,
	}
}
