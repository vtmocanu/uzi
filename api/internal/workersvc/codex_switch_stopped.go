package workersvc

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const stateCodexSwitchStopped = "codex_switch_stopped"

// CodexSwitchStoppedFeature advertises claim-scoped stopped settlement.
const CodexSwitchStoppedFeature = "codex_switch_stopped_v1"

// stopCodexSwitch never releases custody or consumes pause inputs. Advertisement
// gates admission only; a previously admitted stopped claim can settle after downgrade.
func (s *Service) stopCodexSwitch(ctx context.Context, w store.Worker, id uuid.UUID, req StateRequest) (store.Run, bool, error) {
	if req.ClaimGeneration == nil || (*req.ClaimGeneration <= 0 || *req.ClaimGeneration > 9007199254740991) ||
		req.ExpectedEpoch == nil || (*req.ExpectedEpoch <= 0 || *req.ExpectedEpoch > 9007199254740991) ||
		!validHoldRegisterNonce(req.RegisterNonce) || !utf8.ValidString(req.RegisterNonce) || strings.ContainsRune(req.RegisterNonce, 0) || req.OpID != nil {
		return store.Run{}, false, fmt.Errorf("%w: invalid codex stopped settlement", ErrInvalidState)
	}
	if s.txBeginner == nil {
		return store.Run{}, false, ErrSemanticCompletionAuthority
	}
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	locked, run, tx, err := s.beginClaimMutation(ctx, w, id, req.ClaimGeneration)
	if err != nil {
		return run, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if !codexSwitchLaneEligible(run) || UnresolvedPolicyEpisode(run) {
		return run, false, nil
	}
	accepted, err := store.New(tx).SetCodexSwitchStopped(ctx, store.SetCodexSwitchStoppedParams{
		RunID: id, WorkerID: w.ID, UserID: w.UserID, ClaimGeneration: *req.ClaimGeneration,
		RegisterNonce: req.RegisterNonce, ExpectedEpoch: *req.ExpectedEpoch, TimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	})
	if err != nil || !accepted {
		return run, false, err
	}
	current, err := locked.runOwnedByWorker(ctx, id, w)
	if err != nil {
		return run, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return run, false, err
	}
	if run.Status != "paused" && s.bcast != nil {
		s.bcast.PublishState(id, current.Status)
	}
	return current, true, nil
}
