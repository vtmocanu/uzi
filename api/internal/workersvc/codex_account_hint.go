package workersvc

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrCodexAccountUnavailable is an advisory hold, never credential authority.
var ErrCodexAccountUnavailable = errors.New("codex account unavailable")

type codexAccountHoldError struct {
	cause error
	hold  bool
	typed bool
}

func (e *codexAccountHoldError) Error() string { return e.cause.Error() }
func (e *codexAccountHoldError) Unwrap() error { return e.cause }
func (e *codexAccountHoldError) Is(target error) bool {
	return e.typed && target == ErrCodexAccountUnavailable
}

// CodexAccountHoldVerdict returns true, false, or absent independently of the wire opt-in.
func CodexAccountHoldVerdict(err error) (bool, bool) {
	var hint *codexAccountHoldError
	if errors.As(err, &hint) {
		return hint.hold, true
	}
	return false, false
}

func codexAccountHintEligible(err error) bool {
	// Authority, vault and retry refusals dominate even errors joined to state refusals.
	for _, excluded := range []error{
		ErrRunNotOwned, ErrCodexRunNotBound, ErrCodexWorkerMismatch,
		ErrCodexCapabilityMismatch, ErrCodexCapabilityEpoch, ErrCodexScopeNotApplicable,
		ErrCodexKindModeMismatch, ErrCodexRunNotActivelyClaimed,
		ErrCodexAccountKeyUnfrozen, ErrCodexAccountTupleMismatch, ErrCodexAccountRevisionStale,
		ErrCodexBindingConflict, ErrCodexVaultLocked, errVaultLocked,
		ErrCodexRefreshContended, ErrCodexRefreshNoToken, ErrCodexRefreshNoClient,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, excluded) {
			return false
		}
	}
	return errors.Is(err, ErrCodexAccountQuarantined) ||
		errors.Is(err, ErrCodexRefreshQuarantined) ||
		errors.Is(err, ErrCodexRefreshUnrecoverable) ||
		errors.Is(err, ErrCodexMaterialRevisionStale)
}

func (s *Service) decorateCodexAccountHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, cap string, err error) error {
	if !codexAccountHintEligible(err) {
		return err
	}
	hold, verified := s.codexAccountHoldHint(ctx, wkr, runID, cap)
	if !verified {
		return err
	}
	return &codexAccountHoldError{cause: err, hold: hold,
		typed: hold && slices.Contains(wkr.ProtocolCapabilities, capability.CodexAccountParkV1)}
}

// codexAccountHoldHint shares only the authorization prelude. Reads are owner-scoped;
// no account resolution, locks, vault access or provider exchange is needed.
func (s *Service) codexAccountHoldHint(ctx context.Context, wkr store.Worker, runID uuid.UUID, cap string) (bool, bool) {
	row, err := s.codexCredentialPrelude(ctx, wkr, runID, cap, ScopeStartRefresh)
	if err != nil {
		return false, false
	}
	q, _ := s.codexStore()
	alias, err := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
		UserSecretID: uuid.UUID(row.CodexSecretID.Bytes), UserID: wkr.UserID,
	})
	if err != nil {
		return false, false
	}
	run, err := s.q.GetRunByID(ctx, runID)
	if err != nil || run.UserID != wkr.UserID {
		return false, false
	}
	hold, _ := classifyCodexClaimAuthority(run, row, alias.Status, alias.MaterialRevision)
	return hold && run.Harness == harnessCodex &&
		run.CodexAuthMode.Valid && run.CodexAuthMode.String == codexAuthModeSubscription &&
		runClaimOpenedCustody(run, true), true
}
