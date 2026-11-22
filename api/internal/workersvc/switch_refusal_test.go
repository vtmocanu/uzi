package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The staged Codex predicate must hand the verb exactly the errors, in the order, its inline checks
// did: the observed-row call knows only claimed / live status / lane, the in-transaction call adds the
// observed-row comparison, lane, policy episode and worker capability.
func TestCodexSwitchRefusalStagesKeepTheVerbOrder(t *testing.T) {
	cutoff := autoNow.Add(-45 * time.Second)
	worker := store.Worker{
		ID: ncWorker, UserID: ncOwner, Name: "w",
		ProtocolCapabilities:       []string{capability.CodexCredentialSwitchV1, capability.CodexSwitchStoppedV1},
		SnapshotRegisterNonce:      pgtype.Text{String: "n", Valid: true},
		CredentialSwitchApplyNonce: pgtype.Text{String: "n", Valid: true},
		LastHeartbeatAt:            pgconv.Time(autoNow),
	}
	live := cxRun()
	obs := switchFacts{Stage: switchStageObserved}

	for name, tc := range map[string]struct {
		run  store.Run
		want error
	}{
		"claimed":   {func() store.Run { r := live; r.Status = "claimed"; return r }(), ErrCredentialSwitchClaimAssembling},
		"parked":    {func() store.Run { r := live; r.Status = "limit_wait"; return r }(), ErrCodexSwitchIneligible},
		"chat lane": {func() store.Run { r := live; r.Kind = "chat"; return r }(), ErrCredentialOverrideLaneNotSwitchable},
		"live":      {live, nil},
	} {
		if got := codexSwitchRefusal(tc.run, store.Worker{}, obs); !errors.Is(got, tc.want) && got != tc.want {
			t.Errorf("observed/%s: got %v, want %v", name, got, tc.want)
		}
	}

	locked := func(mut func(*store.Run, *store.Worker)) error {
		observed := live
		r, w := live, worker
		mut(&r, &w)
		return codexSwitchRefusal(r, w, switchFacts{Stage: switchStageLocked, Observed: &observed, StaleCutoff: cutoff})
	}
	if err := locked(func(*store.Run, *store.Worker) {}); err != nil {
		t.Errorf("locked/clean: %v", err)
	}
	if err := locked(func(r *store.Run, _ *store.Worker) { r.ClaimGeneration++ }); !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Errorf("locked/generation moved: %v, want raced", err)
	}
	if err := locked(func(r *store.Run, _ *store.Worker) { r.ClaimReleasedAt = pgconv.Time(autoNow) }); !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Errorf("locked/claim released: %v, want raced", err)
	}
	if err := locked(func(r *store.Run, _ *store.Worker) { r.Kind = "cross_check" }); !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Errorf("locked/cross_check: %v, want raced before the lane check", err)
	}
	if err := locked(func(_ *store.Run, w *store.Worker) { w.UserID = uuid.New() }); !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Errorf("locked/foreign worker: %v, want raced", err)
	}
	if err := locked(func(r *store.Run, _ *store.Worker) { r.Kind = "chat" }); !errors.Is(err, ErrCredentialOverrideLaneNotSwitchable) {
		t.Errorf("locked/chat: %v, want lane not switchable", err)
	}
	var held *RunOwnerHeldError
	if err := locked(func(r *store.Run, _ *store.Worker) {
		r.PolicyOutcomeID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		r.PolicyResolvedAt = pgtype.Timestamptz{}
	}); !errors.As(err, &held) || held.Label != "policy hold" {
		t.Errorf("locked/policy episode: %v, want the policy hold", err)
	}
	var unsupported *CredentialSwitchWorkerUnsupportedError
	for name, mut := range map[string]func(*store.Run, *store.Worker){
		"no stop capability": func(_ *store.Run, w *store.Worker) {
			w.ProtocolCapabilities = []string{capability.CodexCredentialSwitchV1}
		},
		"stale heartbeat": func(_ *store.Run, w *store.Worker) { w.LastHeartbeatAt = pgconv.Time(autoNow.Add(-time.Hour)) },
		"no heartbeat":    func(_ *store.Run, w *store.Worker) { w.LastHeartbeatAt = pgtype.Timestamptz{} },
		"nonce mismatch": func(_ *store.Run, w *store.Worker) {
			w.CredentialSwitchApplyNonce = pgtype.Text{String: "other", Valid: true}
		},
	} {
		if err := locked(mut); !errors.As(err, &unsupported) {
			t.Errorf("locked/%s: %v, want worker unsupported", name, err)
		}
	}
	// The full (preview) stage adds the no-worker refusal between the observed and locked checks.
	noWorker := live
	noWorker.WorkerID = pgtype.UUID{}
	if err := codexSwitchRefusal(noWorker, worker, switchFacts{StaleCutoff: cutoff}); !errors.Is(err, ErrCredentialSwitchNoLiveWorker) {
		t.Errorf("full/no worker: %v", err)
	}
}

// lockedParkRefusal keeps the verb's order: the three per-cause sentinels, then the observed-row race,
// then the owner hold.
func TestLockedParkRefusalOrder(t *testing.T) {
	observed := store.Run{ID: uuid.New(), Status: "recovery_wait", ClaimGeneration: 2, StatusSince: pgconv.Time(autoNow)}
	row := lockedRowOf(observed)
	row.RecoveryWaitCause = pgtype.Text{String: recoveryCauseDataVolumeFull, Valid: true}
	if err := lockedParkRefusal(row, observed); !errors.Is(err, ErrDataVolumeFullHeld) {
		t.Errorf("data volume: %v", err)
	}
	// A moved run beats an owner hold: it is a race to retry, not a specific refusal.
	row.RecoveryWaitCause = pgtype.Text{String: "worker_requeue_exhausted", Valid: true}
	moved := row
	moved.ClaimGeneration = 3
	if err := lockedParkRefusal(moved, observed); !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Errorf("moved + owner hold: %v, want raced", err)
	}
	if err := lockedParkRefusal(row, observed); !errors.Is(err, ErrRunOwnerHeld) {
		t.Errorf("owner hold: %v", err)
	}
}
