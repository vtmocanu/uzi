package workersvc

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_action.go is PRD #1590 D6: the owner-action reason of a run held in
// recovery_wait on cause codex_account_unavailable. It is derived at read time, with plain
// reads and no locks, from the run's frozen binding plus its alias and account rows, and is
// never persisted. The derivation reuses the promoter's own decision
// (classifyCodexAccountHold over codexReleaseInputs) so the surface cannot claim a run will
// resume when the next promoter tick would not resume it.

// The codex_account_action vocabulary (apitypes.RunDTO.CodexAccountAction).
const (
	CodexAccountActionReconciling     = "reconciling"
	CodexAccountActionReloginRequired = "relogin_required"
	CodexAccountActionVerifyingLogin  = "verifying_login"
	CodexAccountActionResuming        = "resuming"
)

// IsCodexAccountHold reports whether a run is held on its Codex subscription account, the
// only runs a codex_account_action is derived for. Read handlers use it to skip the
// derivation query entirely for every other run.
func IsCodexAccountHold(status string, recoveryWaitCause *string) bool {
	return status == "recovery_wait" && recoveryWaitCause != nil &&
		*recoveryWaitCause == recoveryCauseCodexAccountUnavailable
}

// codexAccountActionStore is the derivation's one batched read. *store.Queries satisfies it.
type codexAccountActionStore interface {
	ListCodexAccountActionInputs(ctx context.Context, runIds []uuid.UUID) ([]store.ListCodexAccountActionInputsRow, error)
}

// CodexAccountHold is one held run's D6 surface: the derived action, plus the run's OWN
// snapshotted alias label (runs.codex_secret_label, frozen at claim). Label is never read from
// the alias row, so it can never name a different account; nil when the snapshot is NULL or
// empty.
type CodexAccountHold struct {
	Action string
	Label  *string
}

// CodexAccountActionsForRuns returns the D6 codex_account_action (and the run's own snapshotted
// alias label) of each held run in runIDs, keyed by run id, from ONE batched read. A run that is
// not (or no longer) held on codex_account_unavailable is absent from the map (null on the
// wire). Callers pass only the runs IsCodexAccountHold selects, so a page with no held run costs
// no query.
func (s *Service) CodexAccountActionsForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]CodexAccountHold, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]CodexAccountHold{}, nil
	}
	q, ok := s.q.(codexAccountActionStore)
	if !ok {
		return nil, errCodexStoreUnavailable
	}
	rows, err := q.ListCodexAccountActionInputs(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make(map[uuid.UUID]CodexAccountHold, len(rows))
	for _, row := range rows {
		hold := CodexAccountHold{Action: deriveCodexAccountAction(codexAccountActionInputsFromRow(row, now))}
		if row.CodexSecretLabel.Valid && row.CodexSecretLabel.String != "" {
			label := row.CodexSecretLabel.String
			hold.Label = &label
		}
		out[row.ID] = hold
	}
	return out, nil
}

// codexAccountActionInputs is the pure snapshot deriveCodexAccountAction decides over.
type codexAccountActionInputs struct {
	aliasDeleted  bool   // runs.codex_secret_id is NULL: the alias was deleted after the park
	aliasPresent  bool   // the alias's codex_credential_state row was found
	aliasStatus   string // codex_credential_state.status
	accountLinked bool   // the alias names an account and its row was found

	// release is the promoter's classification input over the same rows.
	release codexReleaseInputs

	recoveryAtGeneration bool // recovery material at the account's current generation
	leaseLive            bool // the account's lease_deadline is still in the future
	leaseExpired         bool // the account's lease_deadline is set and has passed
	reauthRequired       bool // codex_provider_account.reauth_required
}

// codexAccountActionInputsFromRow projects one ListCodexAccountActionInputs row. release is
// built the way codexReleaseInputsFromAuthRow builds it from GetRunCodexAuthContext, over the
// same columns.
func codexAccountActionInputsFromRow(row store.ListCodexAccountActionInputsRow, now time.Time) codexAccountActionInputs {
	return codexAccountActionInputs{
		aliasDeleted:  !row.CodexSecretID.Valid,
		aliasPresent:  row.AliasStatus.Valid,
		aliasStatus:   row.AliasStatus.String,
		accountLinked: row.AliasProviderAccountID.Valid && row.AccountID.Valid,
		release: codexReleaseInputs{
			authMode:  row.CodexAuthMode.String,
			boundKind: row.BoundKind.String,
			status:    row.Status,

			frozenMaterialRev:       row.CodexMaterialRevision.Int64,
			frozenMaterialRevValid:  row.CodexMaterialRevision.Valid,
			currentMaterialRev:      row.CurrentMaterialRevision.Int64,
			currentMaterialRevValid: row.CurrentMaterialRevision.Valid,

			frozenAccountKey:      row.CodexAccountKey.String,
			frozenAccountKeyValid: row.CodexAccountKey.Valid,

			currentProviderUserID:          row.ProviderUserID.String,
			currentProviderUserIDValid:     row.ProviderUserID.Valid,
			currentWorkspaceAccountID:      row.WorkspaceAccountID.String,
			currentWorkspaceAccountIDValid: row.WorkspaceAccountID.Valid,

			frozenAccountRev:          row.CodexAccountRevision.Int64,
			frozenAccountRevValid:     row.CodexAccountRevision.Valid,
			currentCredentialRev:      row.CurrentCredentialRevision.Int64,
			currentCredentialRevValid: row.CurrentCredentialRevision.Valid,

			coordState:      row.CurrentCoordState.String,
			coordStateValid: row.CurrentCoordState.Valid,
		},
		recoveryAtGeneration: row.RecoveryAtGeneration,
		leaseLive:            row.LeaseDeadline.Valid && row.LeaseDeadline.Time.After(now),
		leaseExpired:         row.LeaseDeadline.Valid && !row.LeaseDeadline.Time.After(now),
		reauthRequired:       row.ReauthRequired.Valid && row.ReauthRequired.Bool,
	}
}

// deriveCodexAccountAction maps a held run's inputs to its D6 action. It follows the
// promoter's order (codex_account_promote.go):
//
//   - alias deleted (codex_secret_id NULL): relogin_required. Transient: the next promoter
//     tick fails the run (D5);
//   - alias state row missing: relogin_required (unreachable; the promoter keeps it held);
//   - alias staging: verifying_login;
//   - alias failed, or any status other than linked: relogin_required;
//   - linked alias with no account: relogin_required (the promoter fails it as incoherent);
//   - then classifyCodexAccountHold, after simulating D5 re-admission when the alias material
//     is ahead of the run's and the account is idle or committed (ReadmitRunCodexBinding's
//     coord_state fence; its identity and credential-revision fences are classify's own
//     terminal checks). Promote is resuming (transient until the next tick);
//   - stay on a quarantined account with recovery material at its current generation:
//     reconciling, EVEN WHEN the reauth flag is set. The survivor pass
//     (reconcileUnresolvedCodexRefresh) promotes exactly that material, and PromoteCodexRecovery
//     clears reauth_required in the same statement, so the account recovers with no owner
//     action; telling the owner to re-log in would ask for work the next tick makes moot;
//   - stay on a quarantined account otherwise: relogin_required when the reauth flag is set,
//     reconciling when a lease is live, otherwise relogin_required;
//   - stay on an in_progress account with a live lease (a refresh in flight, the promoter waits
//     for it to finish before re-admitting): reconciling. D6's table names the live lease only
//     under quarantine; an owner re-login would not help a run whose account is mid-refresh,
//     so this follows that row's intent;
//   - stay on an in_progress account whose lease has expired: reconciling. The survivor pass
//     lists it (ListUnresolvedCodexRefreshAccounts) and reaps it into quarantine on its next
//     tick (QuarantineExpiredCodexLease), after which this derivation re-reads the quarantined
//     account and reports whatever that state warrants.
//
// FALLBACK: every other state is relogin_required. That covers a terminal classification the
// next promoter tick commits (a changed identity, credential revision or auth mode, an
// unfrozen key), and any stay this table does not name. relogin_required is the one action
// that is never wrong to show a held owner: a fresh verified login either resumes the run
// (D5) or confirms the binding change that ends it.
func deriveCodexAccountAction(in codexAccountActionInputs) string {
	switch {
	case in.aliasDeleted, !in.aliasPresent:
		return CodexAccountActionReloginRequired
	case in.aliasStatus == "staging":
		return CodexAccountActionVerifyingLogin
	case in.aliasStatus != "linked", !in.accountLinked:
		return CodexAccountActionReloginRequired
	}
	rel := in.release
	if rel.frozenMaterialRevValid && rel.currentMaterialRevValid && rel.currentMaterialRev > rel.frozenMaterialRev &&
		rel.coordStateValid && (rel.coordState == codexCoordIdle || rel.coordState == codexCoordCommitted) {
		rel.frozenMaterialRev = rel.currentMaterialRev // D5 re-admission, as the promoter would apply it
	}
	outcome, _ := classifyCodexAccountHold(rel)
	switch outcome {
	case codexHoldPromote:
		return CodexAccountActionResuming
	case codexHoldFail:
		return CodexAccountActionReloginRequired
	}
	switch {
	case rel.coordState == codexCoordQuarantined && in.recoveryAtGeneration:
		return CodexAccountActionReconciling // before the reauth flag: the promotion clears it
	case rel.coordState == codexCoordQuarantined && in.reauthRequired:
		return CodexAccountActionReloginRequired
	case rel.coordState == codexCoordQuarantined && in.leaseLive:
		return CodexAccountActionReconciling
	case rel.coordState == codexCoordInProgress && (in.leaseLive || in.leaseExpired):
		return CodexAccountActionReconciling
	default:
		return CodexAccountActionReloginRequired
	}
}
