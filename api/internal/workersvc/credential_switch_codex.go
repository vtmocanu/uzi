package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CodexSwitchPreflightRequest names an explicit target; it never invokes the
// Anthropic/default/auto selection ladder.
type CodexSwitchPreflightRequest struct {
	CredentialSwitchApplyRequest
	TargetSecretID uuid.UUID `json:"target_secret_id"`
}

// CodexSwitchPreflightResult contains decisions only, never auth material or
// provider identity. Ready is a point-in-time observation, not apply authority.
type CodexSwitchPreflightResult struct {
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
	ClaimGeneration int64  `json:"claim_generation"`
	RegisterNonce   string `json:"register_nonce"`
	OpID            string `json:"op_id"`
}

var errCodexSwitchRead = errors.New("codex credential switch preflight unavailable")

// PreflightCodexCredentialSwitch takes the worker/run and alias/account fences
// used by refresh admission. It always rolls back, calls no provider and returns
// no credential. Apply must repeat these checks in its own transaction.
func (s *Service) PreflightCodexCredentialSwitch(ctx context.Context, wkr store.Worker, runID uuid.UUID, req CodexSwitchPreflightRequest) (result CodexSwitchPreflightResult, retErr error) {
	op, _, err := ValidateCredentialSwitchApplyRequest(req.CredentialSwitchApplyRequest)
	if err != nil {
		return CodexSwitchPreflightResult{}, err
	}
	if req.TargetSecretID == uuid.Nil {
		return CodexSwitchPreflightResult{}, ErrCredentialOverridePinnedNeedsSecret
	}
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if s.txBeginner == nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	gen := req.ClaimGeneration
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	q := store.New(tx)
	w, err := q.GetWorkerForUpdate(ctx, wkr.ID)
	if err != nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: runID, WorkerID: pgtype.UUID{Bytes: wkr.ID, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeFenced}, nil
	}
	if err != nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	defer func() {
		if retErr == nil {
			result.ClaimGeneration = run.ClaimGeneration
			result.RegisterNonce = w.SnapshotRegisterNonce.String
			result.OpID = uuid.UUID(run.CredentialSwitchOpID.Bytes).String()
			if !run.CredentialSwitchOpID.Valid {
				result.OpID = ""
			}
		}
	}()
	if !codexSwitchFence(w, run, req.CredentialSwitchApplyRequest) {
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeFenced}, nil
	}
	refuse := func(reason string) (CodexSwitchPreflightResult, error) {
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeNotApplied, Reason: reason}, nil
	}
	prior, err := q.GetRunCredentialSwitchEpochByOp(ctx, store.GetRunCredentialSwitchEpochByOpParams{RunID: runID, OpID: op})
	if err == nil {
		if prior.ClaimGeneration != gen || !prior.WorkerID.Valid || uuid.UUID(prior.WorkerID.Bytes) != w.ID {
			return CodexSwitchPreflightResult{Outcome: ApplyOutcomeFenced}, nil
		}
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeRefusedCommitted, Reason: applyReasonCredentialChanged}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	if !run.CredentialSwitchOpID.Valid || uuid.UUID(run.CredentialSwitchOpID.Bytes) != op {
		if run.CredentialSwitchRequestedAt.Valid && run.CredentialSwitchGeneration.Valid && run.CredentialSwitchGeneration.Int64 == gen {
			return CodexSwitchPreflightResult{Outcome: ApplyOutcomeSuperseded}, nil
		}
		return refuse(applyReasonNoPendingSwitch)
	}
	if !run.CredentialSwitchRequestedAt.Valid {
		return refuse(applyReasonNoPendingSwitch)
	}
	if !run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 != gen {
		return refuse(applyReasonGenerationMismatch)
	}
	if codexSwitchPolicyBlocked(run) {
		return refuse("policy_refusal")
	}
	if !run.CodexSwitchTargetSecretID.Valid || uuid.UUID(run.CodexSwitchTargetSecretID.Bytes) != req.TargetSecretID ||
		!run.CodexSwitchTargetGeneration.Valid || run.CodexSwitchTargetGeneration.Int64 != gen ||
		run.CodexSwitchTargetOpID != run.CredentialSwitchOpID {
		return refuse("target_mismatch")
	}
	base, err := q.ListRunCredentialEpochs(ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: run.UserID})
	if err != nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	children, err := q.ListRunCredentialSwitchEpochs(ctx, store.ListRunCredentialSwitchEpochsParams{RunID: runID, UserID: run.UserID})
	if err != nil {
		return CodexSwitchPreflightResult{}, errCodexSwitchRead
	}
	hasBaseline := false
	for _, b := range base {
		if b.ClaimGeneration == gen {
			hasBaseline = true
			break
		}
	}
	if !hasBaseline {
		return refuse("baseline_missing")
	}
	if !codexSwitchBaselineMatches(run, base, children) {
		return refuse("baseline_mismatch")
	}
	// Lock both aliases before either account. NOWAIT avoids the reverse order
	// used by account re-login writers. Each independent id is visited once;
	// a failure stops this preflight and the deferred rollback releases all locks.
	ids := []uuid.UUID{uuid.UUID(run.CodexSecretID.Bytes), req.TargetSecretID}
	accounts := make([]uuid.UUID, 0, 2)
	var oldAccount uuid.UUID
	var targetAccount uuid.UUID
	var targetKind string
	for i, id := range ids {
		if i == 1 && id == ids[0] {
			continue
		}
		disabled, err := q.LockSecretEnablementForShareNowait(ctx, store.LockSecretEnablementForShareNowaitParams{ID: id, UserID: run.UserID})
		if err != nil {
			return codexSwitchReadFailure(err)
		}
		// A disabled old alias may be replaced, but the target must be enabled.
		if id == req.TargetSecretID && disabled {
			return refuse(applyReasonCredentialDisabled)
		}
		st, err := q.LockCodexAliasForShareNowait(ctx, store.LockCodexAliasForShareNowaitParams{UserSecretID: id, UserID: run.UserID})
		if err != nil {
			return codexSwitchReadFailure(err)
		}
		meta, err := q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{ID: id, UserID: run.UserID})
		if err != nil {
			return codexSwitchReadFailure(err)
		}
		if id == req.TargetSecretID && !codexSwitchTargetUsable(meta.Kind, st.Status, st.ProviderAccountID.Valid) {
			return refuse(applyReasonCredentialUnavail)
		}
		if id == req.TargetSecretID {
			targetKind = meta.Kind
			if st.ProviderAccountID.Valid {
				targetAccount = uuid.UUID(st.ProviderAccountID.Bytes)
			}
		}
		if id == ids[0] {
			if st.MaterialRevision != run.CodexMaterialRevision.Int64 {
				return refuse(applyReasonCredentialChanged)
			}
			if err := codexCheckKindMode(run.CodexAuthMode.String, meta.Kind); err != nil {
				return refuse(applyReasonCredentialChanged)
			}
			if run.CodexAuthMode.String == codexAuthModeSubscription && !st.ProviderAccountID.Valid {
				return refuse(applyReasonCredentialGone)
			}
		}
		if id == ids[0] && st.ProviderAccountID.Valid {
			oldAccount = uuid.UUID(st.ProviderAccountID.Bytes)
		}
		if st.ProviderAccountID.Valid {
			id := uuid.UUID(st.ProviderAccountID.Bytes)
			if !slices.Contains(accounts, id) {
				accounts = append(accounts, id)
			}
		}
	}
	for _, id := range accounts {
		if _, err := q.LockCodexAccountForUpdateNowait(ctx, store.LockCodexAccountForUpdateNowaitParams{ID: id, UserID: run.UserID}); err != nil {
			return codexSwitchReadFailure(err)
		}
	}
	for _, id := range accounts {
		acct, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{ID: id, UserID: run.UserID})
		if err != nil {
			return codexSwitchReadFailure(err)
		}
		if id == oldAccount {
			key, err := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID)
			if err != nil || !run.CodexAccountKey.Valid || key != run.CodexAccountKey.String ||
				!run.CodexAccountRevision.Valid || acct.CredentialRevision != run.CodexAccountRevision.Int64 {
				return refuse(applyReasonCredentialChanged)
			}
		}
		intents, err := q.LockUnresolvedCodexSwitchIntents(ctx, store.LockUnresolvedCodexSwitchIntentsParams{UserID: run.UserID, ProviderAccountID: id})
		if err != nil {
			return codexSwitchReadFailure(err)
		}
		if id == targetAccount {
			if _, err := codexAccountKey(acct.ProviderUserID, acct.WorkspaceAccountID); err != nil {
				return refuse(applyReasonCredentialUnavail)
			}
			snapshot, err := readCodexAccountSnapshot(ctx, q, store.GetCodexProviderAccountSnapshotParams{ID: id, UserID: run.UserID})
			if err != nil {
				return codexSwitchReadFailure(err)
			}
			// Use the transaction reader even for the bounded vault-conversion retry.
			opener := *s
			opener.q = q
			if _, err := opener.openCodexAccountLogin(ctx, run.UserID, snapshot); err != nil {
				return refuse(codexSwitchMaterialReason(err))
			}
		}
		if !codexSwitchAccountIdle(acct, len(intents)) {
			return refuse(applyReasonCredentialBusy)
		}
	}
	if targetKind == store.KindOpenAIAPIKey {
		plain, err := secretopen.OpenByIDOfKind(ctx, q, s.vlt, s.box, run.UserID, req.TargetSecretID, store.KindOpenAIAPIKey)
		if err != nil {
			return refuse(codexSwitchMaterialReason(err))
		}
		if strings.TrimSpace(string(plain)) == "" {
			return refuse(applyReasonCredentialUnavail)
		}
	}
	return CodexSwitchPreflightResult{Outcome: "ready"}, nil
}

func codexSwitchMaterialReason(err error) string {
	if errors.Is(err, errVaultLocked) || errors.Is(err, secretopen.ErrVaultLocked) {
		return applyReasonVaultLocked
	}
	return applyReasonCredentialUnavail
}

func codexSwitchReadFailure(err error) (CodexSwitchPreflightResult, error) {
	if isLockNotAvailable(err) {
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialBusy}, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return CodexSwitchPreflightResult{Outcome: ApplyOutcomeNotApplied, Reason: applyReasonCredentialGone}, nil
	}
	return CodexSwitchPreflightResult{}, errCodexSwitchRead
}

func codexSwitchFence(w store.Worker, r store.Run, req CredentialSwitchApplyRequest) bool {
	return r.Harness == harnessCodex && w.UserID == r.UserID &&
		codexSwitchNegotiated(w) &&
		w.SnapshotRegisterNonce.Valid && w.SnapshotRegisterNonce.String == req.RegisterNonce &&
		r.WorkerID.Valid && uuid.UUID(r.WorkerID.Bytes) == w.ID &&
		r.ClaimGeneration == req.ClaimGeneration && !r.ClaimReleasedAt.Valid &&
		applyLiveStatus(r.Status) && r.Kind != "job" && r.Kind != "cross_check"
}

func codexSwitchTargetUsable(kind, status string, linked bool) bool {
	return kind == store.KindOpenAIAPIKey && status == "static" && !linked ||
		kind == store.KindCodexAuth && status == "linked" && linked
}

func codexSwitchAccountIdle(a store.CodexProviderAccount, unresolved int) bool {
	return a.CoordState == "idle" && !a.CoordOperationID.Valid &&
		!a.LeaseDeadline.Valid && unresolved == 0 && !a.ReauthRequired &&
		len(a.RecoverySealed) == 0
}

// codexSwitchBaselineMatches requires ordinal zero even when children exist.
// The latest active row must match all binding snapshots it recorded.
func codexSwitchBaselineMatches(r store.Run, bases []store.RunCredentialEpoch, children []store.RunCredentialSwitchEpoch) bool {
	var active *store.RunCredentialEpoch
	for i := range bases {
		if bases[i].ClaimGeneration == r.ClaimGeneration {
			active = &bases[i]
			break
		}
	}
	if active == nil {
		return false
	}
	current := *active
	var ordinal int32
	for _, c := range children {
		if c.ClaimGeneration == r.ClaimGeneration && c.Ordinal > ordinal {
			ordinal = c.Ordinal
			current.SecretID, current.CodexAuthMode = c.SecretID, c.CodexAuthMode
			current.CodexMaterialRevision, current.CodexAccountKey = c.CodexMaterialRevision, c.CodexAccountKey
			current.CodexClaimEpoch, current.CodexAccountID = c.CodexClaimEpoch, c.CodexAccountID
		}
	}
	if current.CodexAccountID.Valid {
		var tuple []string
		if !r.CodexAccountKey.Valid || json.Unmarshal([]byte(r.CodexAccountKey.String), &tuple) != nil ||
			len(tuple) != 2 || tuple[1] != current.CodexAccountID.String {
			return false
		}
	}
	return current.SecretID.Valid && current.SecretID == r.CodexSecretID &&
		(!current.CodexAuthMode.Valid || current.CodexAuthMode == r.CodexAuthMode) &&
		(!current.CodexMaterialRevision.Valid || current.CodexMaterialRevision == r.CodexMaterialRevision) &&
		(!current.CodexAccountKey.Valid || current.CodexAccountKey == r.CodexAccountKey) &&
		(!current.CodexClaimEpoch.Valid || current.CodexClaimEpoch.Int64 == r.CodexClaimEpoch)
}

// Both owner requests and worker preflight require the Codex protocol explicitly.
func codexSwitchNegotiated(w store.Worker) bool {
	return slices.Contains(w.ProtocolCapabilities, capability.CodexCredentialSwitchV1) &&
		w.SnapshotRegisterNonce.Valid && w.SnapshotRegisterNonce.String != ""
}

func codexSwitchPolicyBlocked(r store.Run) bool {
	return UnresolvedPolicyEpisode(r)
}
