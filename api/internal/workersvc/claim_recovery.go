package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

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
var errClaimRecoveryNoTx = errors.New("final run claim check requires a transaction")

// finishRunClaimAttempts bounds the whole-transaction retry on 55P03 (lock_not_available)
// from the classifier's NOWAIT alias/account locks. A concurrent re-login or refresh holds
// those rows for one short transaction; without a retry a successful claim would strand
// in 'claimed' until SweepClaimedNeverStarted. Past the budget the claim returns the error
// with no payload and no mutation.
const finishRunClaimAttempts = 3

// finishRunClaimRetryDelay is the pause between those attempts. A var so a test can shrink it.
var finishRunClaimRetryDelay = 75 * time.Millisecond

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

// claimFinishQueries is the statement surface of finishRunClaim's exact-claim transaction.
// *store.Queries bound to the pgx transaction satisfies it.
type claimFinishQueries interface {
	GetRunOwnedByWorkerForUpdate(ctx context.Context, arg store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error)
	LockCodexAliasForShareNowait(ctx context.Context, arg store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error)
	LockCodexAccountForShareNowait(ctx context.Context, arg store.LockCodexAccountForShareNowaitParams) (uuid.UUID, error)
	GetRunCodexAuthContext(ctx context.Context, id uuid.UUID) (store.GetRunCodexAuthContextRow, error)
	LockOpenCustodyHoldsForRunWorkerGeneration(ctx context.Context, arg store.LockOpenCustodyHoldsForRunWorkerGenerationParams) ([]uuid.UUID, error)
	ReleaseCustodyHoldExact(ctx context.Context, arg store.ReleaseCustodyHoldExactParams) (int64, error)
	ParkRunCodexAccountUnavailable(ctx context.Context, arg store.ParkRunCodexAccountUnavailableParams) (store.Run, error)
	LockSecretEnablementForShareNowait(ctx context.Context, arg store.LockSecretEnablementForShareNowaitParams) (bool, error)
	ParkCredentialDisabledRun(ctx context.Context, arg store.ParkCredentialDisabledRunParams) (int64, error)
	RequeueClaimAssemblyExact(ctx context.Context, arg store.RequeueClaimAssemblyExactParams) (int64, error)
	FailClaimAssemblyExact(ctx context.Context, arg store.FailClaimAssemblyExactParams) (int64, error)
}

// claimFinishTx is one open exact-claim transaction: its statements plus commit/rollback.
type claimFinishTx interface {
	claimFinishQueries
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// claimFinishBeginner is an optional Store surface (the codexStore idiom): an in-memory
// transactional fake-store implements it so unit tests drive the same fenced outcome logic.
// The production *store.Queries does not; production opens the transaction on txBeginner.
type claimFinishBeginner interface {
	BeginClaimFinish(ctx context.Context) (claimFinishTx, error)
}

// pgxClaimFinishTx binds the generated queries to one pgx transaction.
type pgxClaimFinishTx struct {
	*store.Queries
	tx pgx.Tx
}

func (t pgxClaimFinishTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t pgxClaimFinishTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// beginClaimFinish opens the exact-claim transaction. With neither a pgx transaction source
// nor a transactional store there is no fenced writer, so it refuses rather than falling back.
func (s *Service) beginClaimFinish(ctx context.Context) (claimFinishTx, error) {
	if s.txBeginner != nil {
		tx, err := s.txBeginner.Begin(ctx)
		if err != nil {
			return nil, err
		}
		return pgxClaimFinishTx{Queries: store.New(tx), tx: tx}, nil
	}
	if b, ok := s.q.(claimFinishBeginner); ok {
		return b.BeginClaimFinish(ctx)
	}
	return nil, errClaimRecoveryNoTx
}

// claimTestHooks are the LiveDB race seams Service.claimHooks carries (nil in production).
type claimTestHooks struct {
	// beforeAssembly runs after ClaimRun commits, before assembleClaim.
	beforeAssembly func(ctx context.Context, run store.Run)
	// afterMint runs once codexClaimSecrets' capability mint has persisted.
	afterMint func(ctx context.Context, run store.Run)
	// afterAssembly runs with assembleClaim's result, before finishRunClaim.
	afterAssembly func(ctx context.Context, run store.Run, payload *ClaimPayload, err error)
	// afterChatAssembly runs after a successful assembleChatClaim, before finishChatClaim.
	afterChatAssembly func(ctx context.Context, run store.Run)
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

// assembleAndFinishRunClaim is the run lane's single post-ClaimRun tail, shared by both
// Claim return paths so each outcome goes through the same exact-claim transaction.
func (s *Service) assembleAndFinishRunClaim(ctx context.Context, wkr store.Worker, run store.Run, recoveryCapable bool) (*ClaimPayload, error) {
	if h := s.claimHooks; h != nil && h.beforeAssembly != nil {
		h.beforeAssembly(ctx, run)
	}
	payload, err := s.assembleClaim(ctx, wkr, run)
	if h := s.claimHooks; h != nil && h.afterAssembly != nil {
		h.afterAssembly(ctx, run, payload, err)
	}
	return s.finishRunClaim(ctx, run, payload, err,
		claimRecoveryIdentity{workerID: wkr.ID, recoveryCapable: recoveryCapable})
}

// finishRunClaim checks even successful payloads against the exact mint and current
// authority. A failed assembly and a late authority refusal share one transaction, and
// no run-lane outcome reaches an unfenced writer: without a transaction it refuses.
func (s *Service) finishRunClaim(ctx context.Context, run store.Run, payload *ClaimPayload, assemblyErr error, identity claimRecoveryIdentity) (*ClaimPayload, error) {
	if errors.Is(assemblyErr, errCodexMintAmbiguous) || isTransientClaimDBError(assemblyErr) {
		return nil, assemblyErr // the write may have landed; no mutation is safe
	}
	if errors.Is(assemblyErr, errRunVanished) {
		return nil, nil
	}
	transient := errors.Is(assemblyErr, errVaultLocked) || errors.Is(assemblyErr, errAutoPoolEmpty) ||
		errors.Is(assemblyErr, errCustomModelCapabilityMissing)
	// errCredentialDisabled is its own non-terminal classification (PRD #1732 D14): neither
	// transient nor terminal, so it must pass this early return to reach the fenced park.
	if assemblyErr != nil && !transient && !claimAssemblyTerminal(assemblyErr) &&
		!errors.Is(assemblyErr, errCredentialDisabled) {
		return nil, assemblyErr
	}
	for attempt := 1; ; attempt++ {
		out, settled, err := s.finishRunClaimTx(ctx, run, payload, assemblyErr, transient, identity)
		if err == nil {
			switch settled {
			case "failed":
				s.notify(run.ID, "failed")
			case "paused":
				// PRD #1732 D14: the claimed -> paused/credential_disabled park is a visible
				// transition (clients render the hold reason), published like the promoter's
				// paused -> queued.
				s.publishSwept(run.ID, "paused")
			}
			return out, nil
		}
		if errors.Is(err, errClaimRecoveryNoTx) && assemblyErr != nil {
			return nil, fmt.Errorf("%w: %w", err, assemblyErr)
		}
		if !isLockNotAvailable(err) || attempt >= finishRunClaimAttempts {
			return nil, err
		}
		timer := time.NewTimer(finishRunClaimRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// The caller gave up: report that, not the 55P03 a later attempt might have cleared.
			return nil, fmt.Errorf("finish run claim retry: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// finishRunClaimTx is one attempt of finishRunClaim's transaction. It returns the payload to
// deliver (nil for idle) and the status it committed that the caller must publish ("failed"
// for a terminal failure, "paused" for the credential_disabled park, "" otherwise), or an
// error after which nothing was committed.
func (s *Service) finishRunClaimTx(ctx context.Context, run store.Run, payload *ClaimPayload, assemblyErr error, transient bool, identity claimRecoveryIdentity) (*ClaimPayload, string, error) {
	q, err := s.beginClaimFinish(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = q.Rollback(ctx) }()
	locked, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: run.ID, WorkerID: pgconv.UUID(identity.workerID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if locked.Status != "claimed" || locked.ClaimGeneration != run.ClaimGeneration ||
		locked.WorkerID != pgconv.UUID(identity.workerID) {
		return nil, "", nil // another transition won; idle with no mutation
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
			return nil, "", errors.New("invalid minted claim capability")
		}
		hash = hashCodexCapability(secret)
	}
	if locked.CodexClaimEpoch != epoch || !bytes.Equal(locked.CodexCapHash, hash) {
		return nil, "", nil // superseded mint; never settle a newer claim's custody
	}

	holdClass := false
	var authorityErr error
	if run.Harness == harnessCodex {
		holdClass, authorityErr, err = classifyLockedCodexClaim(ctx, q, locked)
		if err != nil {
			return nil, "", err
		}
		// A no-payload quarantine discovered during assembly still parks when
		// recovery completes before this lock, provided current authority is valid.
		if assemblyErr != nil && payload == nil && authorityErr == nil &&
			(errors.Is(assemblyErr, ErrCodexAccountQuarantined) ||
				errors.Is(assemblyErr, ErrCodexMaterialRevisionStale)) {
			holdClass = true
		}
	}
	// PRD #1732 D14: the credential this claim resolved is disabled. Either assembly said so,
	// or the in-transaction re-check below sees a disable that committed after the credential
	// was opened but before this decision. Checked AFTER the exact-claim and Codex
	// capability-identity checks above, so a stale claim is idle before it is classified.
	credDisabled := errors.Is(assemblyErr, errCredentialDisabled)
	if !credDisabled && assemblyErr == nil && payload != nil {
		if credDisabled, err = claimCredentialDisabled(ctx, q, locked, payload); err != nil {
			return nil, "", err
		}
	}
	origin := claimAssemblyOrigin(assemblyErr)
	// Only credential authority faults may park, and only on a custody-holding kind: a judge
	// stays terminal. Guardrail and provisioning failures remain terminal even if an account
	// changes concurrently.
	holdClass = holdClass && (assemblyErr == nil || origin == "credential_unavailable" || transient) &&
		claimOpenedCustody(run.Kind, true)
	decision := assemblyErr
	if credDisabled {
		// A distinct non-terminal outcome: never a transient requeue, never the account
		// park, and never rewritten into a failure by the authority rewrite below. The owner
		// re-enabling (or reassigning) the credential resumes it through the promoter.
		if decision == nil {
			decision = errCredentialDisabled
		}
		holdClass, transient = false, false
	} else if authorityErr != nil && origin != "provisioning_failed" && origin != "guardrail_blocked" {
		// Lock-time authority is the fresher truth. When it decides the outcome (a park it
		// classified, or a terminal refusal that overrides a success, a transient requeue or
		// an assembly-time hold), the recorded reason is the authority error that decided it.
		decision = fmt.Errorf("%w: %w", errCredentialUnavailable, authorityErr)
		origin = "credential_unavailable"
		if !holdClass {
			transient = false
		}
	}
	if decision == nil {
		if err := q.Commit(ctx); err != nil {
			return nil, "", err
		}
		return payload, "", nil
	}

	expected := 0
	if claimOpenedCustody(run.Kind, identity.recoveryCapable) {
		expected = 1
	}
	holds, err := q.LockOpenCustodyHoldsForRunWorkerGeneration(ctx, store.LockOpenCustodyHoldsForRunWorkerGenerationParams{
		RunID: run.ID, Generation: run.ClaimGeneration, WorkerID: identity.workerID,
	})
	if err != nil {
		return nil, "", err
	}
	if len(holds) != expected {
		slog.Warn("claim custody mismatch", "run", run.ID, "expected", expected, "actual", len(holds))
		return nil, "", errClaimRecoveryCustody
	}
	if expected == 1 {
		n, err := q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
			RunID: run.ID, Generation: run.ClaimGeneration, WorkerID: identity.workerID,
			ReleaseEvidence: pgconv.TextOrNull("no_adopted_source"),
		})
		if err != nil {
			return nil, "", err
		}
		if n != 1 {
			return nil, "", errClaimRecoveryCustody
		}
	}
	var n int64
	switch {
	case credDisabled:
		// Status exactly 'claimed' at this generation and worker with an unreleased claim (the
		// query's fence, which admits any hold_reason: a resumed completion_blocked annotation
		// is superseded, see ParkCredentialDisabledRun): the payload was never delivered, so no
		// running flight is parked (D3). The fence always matches the exact claim locked above,
		// so this custody release commits with the park instead of rolling back with a stale
		// park and leaking the hold.
		n, err = q.ParkCredentialDisabledRun(ctx, store.ParkCredentialDisabledRunParams{
			ID: run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
		})
	case holdClass:
		_, err = q.ParkRunCodexAccountUnavailable(ctx, store.ParkRunCodexAccountUnavailableParams{
			ID: run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			err = errClaimRecoveryStale
		}
		n = 1
	case transient:
		n, err = q.RequeueClaimAssemblyExact(ctx, store.RequeueClaimAssemblyExactParams{
			PoolWait: errors.Is(decision, errAutoPoolEmpty),
			ID:       run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
		})
	default:
		n, err = q.FailClaimAssemblyExact(ctx, store.FailClaimAssemblyExactParams{
			ID: run.ID, WorkerID: pgconv.UUID(identity.workerID), ClaimGeneration: run.ClaimGeneration,
			FailureReason: pgconv.TextOrNull(decision.Error()), FailOrigin: pgconv.TextOrNull(origin),
		})
	}
	if err == nil && n != 1 {
		err = errClaimRecoveryStale
	}
	if err != nil {
		return nil, "", err
	}
	if err := q.Commit(ctx); err != nil {
		return nil, "", err
	}
	switch {
	case credDisabled:
		return nil, "paused", nil
	case !holdClass && !transient:
		return nil, "failed", nil
	}
	return nil, "", nil
}

// claimCredentialDisabled is finishRunClaimTx's in-transaction re-check of the credential a
// successful payload actually resolved (PRD #1732 D14): the frozen alias for a Codex run, the
// Anthropic credential assembly just recorded on the locked row for a Claude run. It runs
// under the exact-claim run lock and after classifyLockedCodexClaim's alias/account locks,
// and takes the credential row FOR SHARE NOWAIT: a concurrent disable either committed first
// (seen here, the claim parks) or waits for this decision (the claim was issued first and
// finishes, D3). A held row is 55P03, which rolls the transaction back for the bounded
// retry instead of blocking under the run lock. A missing row is not this classification.
func claimCredentialDisabled(ctx context.Context, q claimFinishQueries, locked store.Run, payload *ClaimPayload) (bool, error) {
	var secretID pgtype.UUID
	switch {
	case locked.Harness == harnessCodex:
		if payload.Secrets.Codex == nil {
			return false, nil
		}
		secretID = locked.CodexSecretID
	case payload.Secrets.AnthropicOAuthToken != "":
		secretID = locked.AnthropicSecretID
	}
	if !secretID.Valid {
		return false, nil
	}
	disabled, err := q.LockSecretEnablementForShareNowait(ctx, store.LockSecretEnablementForShareNowaitParams{
		ID: uuid.UUID(secretID.Bytes), UserID: locked.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return disabled, err
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

// isLockNotAvailable reports SQLSTATE 55P03, the NOWAIT refusal finishRunClaim retries.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
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
func classifyLockedCodexClaim(ctx context.Context, q claimFinishQueries, run store.Run) (bool, error, error) {
	if !run.CodexSecretID.Valid {
		return false, ErrCodexRunNotBound, nil
	}
	alias, err := q.LockCodexAliasForShareNowait(ctx, store.LockCodexAliasForShareNowaitParams{
		UserSecretID: uuid.UUID(run.CodexSecretID.Bytes), UserID: run.UserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrCodexRunNotBound, nil
	}
	if err != nil {
		return false, nil, err
	}
	if alias.ProviderAccountID.Valid {
		_, err = q.LockCodexAccountForShareNowait(ctx, store.LockCodexAccountForShareNowaitParams{
			ID: uuid.UUID(alias.ProviderAccountID.Bytes), UserID: run.UserID,
		})
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
	hold, predicateErr := classifyCodexClaimAuthority(run, row, alias.Status, alias.MaterialRevision)
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
