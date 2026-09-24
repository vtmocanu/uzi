package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var errCodexMintAmbiguous = errors.New("codex capability mint outcome ambiguous")
var errClaimRecoveryStale = errors.New("claim assembly recovery lost exact claim")
var errClaimRecoveryCustody = errors.New("claim assembly custody count mismatch")

type codexMintedClaimError struct {
	cause error
	epoch int64
	hash  []byte
}

func (e *codexMintedClaimError) Error() string { return e.cause.Error() }
func (e *codexMintedClaimError) Unwrap() error { return e.cause }

type claimRecoveryIdentity struct {
	workerID        uuid.UUID
	recoveryCapable bool
}

// This matches ClaimRun's hold CTE, using the boolean passed to that claim.
func claimOpenedCustody(kind string, recoveryCapable bool) bool {
	if !recoveryCapable {
		return false
	}
	switch kind {
	case runkind.Issue, runkind.CIFix, runkind.SelfImprove, runkind.Prompt, runkind.Task, runkind.MRRework:
		return true
	default:
		return false
	}
}

// finishRunClaim checks even successful payloads against the exact mint and current
// authority. A failed assembly and a late authority refusal share one transaction.
func (s *Service) finishRunClaim(ctx context.Context, run store.Run, payload *ClaimPayload, assemblyErr error, identity claimRecoveryIdentity) (*ClaimPayload, error) {
	if errors.Is(assemblyErr, errCodexMintAmbiguous) || isTransientClaimDBError(assemblyErr) {
		return nil, assemblyErr // the write may have landed; no mutation is safe
	}
	if errors.Is(assemblyErr, errRunVanished) {
		return nil, nil
	}
	transient := errors.Is(assemblyErr, errVaultLocked) || errors.Is(assemblyErr, errAutoPoolEmpty) ||
		errors.Is(assemblyErr, errCustomModelCapabilityMissing)
	if assemblyErr != nil && !transient && !claimAssemblyTerminal(assemblyErr) {
		return nil, assemblyErr
	}
	if s.txBeginner == nil {
		if assemblyErr != nil {
			return nil, fmt.Errorf("exact claim recovery requires a transaction: %w", assemblyErr)
		}
		return nil, errors.New("final run claim check requires a transaction")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	locked, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: run.ID, WorkerID: pgconv.UUID(identity.workerID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if locked.Status != "claimed" || locked.ClaimGeneration != run.ClaimGeneration ||
		locked.WorkerID != pgconv.UUID(identity.workerID) {
		return nil, nil // another transition won; idle with no mutation
	}

	// The claim snapshot is the no-mint identity. A successful mint, including
	// one followed by an assembly error, must carry its actual persisted identity.
	epoch, hash := run.CodexClaimEpoch, run.CodexCapHash
	var minted *codexMintedClaimError
	if errors.As(assemblyErr, &minted) {
		epoch, hash = minted.epoch, minted.hash
	} else if assemblyErr == nil && payload != nil && payload.Secrets.Codex != nil {
		var secret string
		var ok bool
		epoch, secret, ok = parseCodexCapability(payload.Secrets.Codex.Capability)
		if !ok {
			return nil, errors.New("invalid minted claim capability")
		}
		hash = hashCodexCapability(secret)
	}
	if locked.CodexClaimEpoch != epoch || !bytes.Equal(locked.CodexCapHash, hash) {
		return nil, nil // superseded mint; never settle a newer claim's custody
	}

	holdClass := false
	if run.Harness == harnessCodex {
		var authorityErr error
		holdClass, authorityErr, err = classifyLockedCodexClaim(ctx, tx, q, locked)
		if err != nil {
			return nil, err
		}
		// A no-payload quarantine discovered during assembly still parks when
		// recovery completes before this lock, provided current authority is valid.
		if assemblyErr != nil && payload == nil && authorityErr == nil &&
			(errors.Is(assemblyErr, ErrCodexAccountQuarantined) ||
				errors.Is(assemblyErr, ErrCodexMaterialRevisionStale)) {
			holdClass = true
		}
		if assemblyErr == nil && authorityErr != nil {
			assemblyErr = fmt.Errorf("%w: %w", errCredentialUnavailable, authorityErr)
		}
	}
	if assemblyErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return payload, nil
	}
	origin := claimAssemblyOrigin(assemblyErr)
	if origin == "" && !transient {
		return nil, assemblyErr
	}
	// Only credential authority faults may park. Guardrail and provisioning
	// failures remain terminal even if an account changes concurrently.
	holdClass = holdClass && (origin == "credential_unavailable" || transient) && claimOpenedCustody(run.Kind, true)

	expected := 0
	if claimOpenedCustody(run.Kind, identity.recoveryCapable) {
		expected = 1
	}
	holds, err := q.LockOpenCustodyHoldsForRunWorkerGeneration(ctx, store.LockOpenCustodyHoldsForRunWorkerGenerationParams{
		RunID: run.ID, Generation: run.ClaimGeneration, WorkerID: identity.workerID,
	})
	if err != nil {
		return nil, err
	}
	if len(holds) != expected {
		slog.Warn("codex claim custody mismatch", "run", run.ID, "expected", expected, "actual", len(holds))
		return nil, errClaimRecoveryCustody
	}
	if expected == 1 {
		n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
			RunID: run.ID, Generation: run.ClaimGeneration, WorkerID: identity.workerID,
			ReleaseEvidence: pgconv.TextOrNull("no_adopted_source"),
		})
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, errClaimRecoveryCustody
		}
	}
	if holdClass {
		_, err = q.ParkRunCodexAccountUnavailable(ctx, store.ParkRunCodexAccountUnavailableParams{
			ID: run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
		})
	} else if transient {
		status := "queued"
		if errors.Is(assemblyErr, errAutoPoolEmpty) {
			status = "pool_wait"
		}
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, "UPDATE runs SET status = $1, status_since = now(), "+
			"started_at = CASE WHEN $1 = 'pool_wait' THEN NULL ELSE started_at END, "+
			"budget_paused_seconds = CASE WHEN $1 = 'pool_wait' THEN 0 ELSE budget_paused_seconds END, "+
			"health = 'ok', health_reason = NULL, health_since = NULL, "+
			"codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1, updated_at = now() "+
			"WHERE id = $2 AND worker_id = $3 AND claim_generation = $4 AND status = 'claimed'",
			status, run.ID, identity.workerID, run.ClaimGeneration)
		if err == nil && tag.RowsAffected() != 1 {
			err = errClaimRecoveryStale
		}
	} else {
		var n int64
		n, err = q.FailClaimAssemblyExact(ctx, store.FailClaimAssemblyExactParams{
			ID: run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
			FailureReason: pgconv.TextOrNull(assemblyErr.Error()), FailOrigin: pgconv.TextOrNull(origin),
		})
		if err == nil && n != 1 {
			err = errClaimRecoveryStale
		}
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if !holdClass && !transient {
		s.notify(run.ID, "failed")
	}
	return nil, nil
}

func claimAssemblyTerminal(err error) bool { return claimAssemblyOrigin(err) != "" }

func isTransientClaimDBError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "08" || pgErr.Code[:2] == "40") ||
			pgErr.Code == "53300" || pgErr.Code == "57P03"
	}
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

func claimAssemblyOrigin(err error) string {
	switch {
	case errors.Is(err, errCredentialUnavailable):
		return "credential_unavailable"
	case errors.Is(err, errToolPackagesRejected):
		return "provisioning_failed"
	case errors.Is(err, errGuardrailBlockedClaim):
		return "guardrail_blocked"
	default:
		return ""
	}
}

// Lock the alias before its account, then reread the common release predicate.
// The run row is already locked by the caller. A removed alias is terminal.
func classifyLockedCodexClaim(ctx context.Context, tx pgx.Tx, q *store.Queries, run store.Run) (bool, error, error) {
	if !run.CodexSecretID.Valid {
		return false, ErrCodexRunNotBound, nil
	}
	var stateStatus string
	var material int64
	var accountID pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT status, material_revision, provider_account_id
		FROM codex_credential_state WHERE user_secret_id = $1 AND user_id = $2 FOR SHARE NOWAIT`,
		uuid.UUID(run.CodexSecretID.Bytes), run.UserID).Scan(&stateStatus, &material, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrCodexRunNotBound, nil
	}
	if err != nil {
		return false, nil, err
	}
	if accountID.Valid {
		var id uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM codex_provider_account
			WHERE id = $1 AND user_id = $2 FOR SHARE NOWAIT`, uuid.UUID(accountID.Bytes), run.UserID).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, nil, err
		}
	}
	row, err := q.GetRunCodexAuthContext(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrCodexRunNotBound, nil
	}
	if err != nil {
		return false, nil, err
	}
	hold, predicateErr := classifyCodexClaimAuthority(run, row, stateStatus, material)
	return hold, predicateErr, nil
}

func classifyCodexClaimAuthority(run store.Run, row store.GetRunCodexAuthContextRow, stateStatus string, material int64) (bool, error) {
	predicateErr := evalCodexReleasePredicate(codexReleaseInputsFromAuthRow(row))
	// A staging replacement has no linked identity yet. Once linked, require the
	// frozen identity and credential revision before treating newer material as a hold.
	relogin := run.CodexAuthMode.Valid && run.CodexAuthMode.String == codexAuthModeSubscription &&
		run.CodexAccountKey.Valid && run.CodexMaterialRevision.Valid &&
		material > run.CodexMaterialRevision.Int64 &&
		(stateStatus == "staging" || stateStatus == "failed" || stateStatus == "linked") &&
		errors.Is(predicateErr, ErrCodexMaterialRevisionStale)
	if relogin && stateStatus == "linked" {
		inputs := codexReleaseInputsFromAuthRow(row)
		relogin = codexCheckAccountTuple(inputs) == nil &&
			inputs.frozenAccountRevValid && inputs.currentCredentialRevValid &&
			inputs.frozenAccountRev == inputs.currentCredentialRev
	}
	return errors.Is(predicateErr, ErrCodexAccountQuarantined) || relogin, predicateErr
}
