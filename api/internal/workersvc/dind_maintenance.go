package workersvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// DindMeter identifies the meter's completed Unix-second sampling epoch.
// SampledAt must equal that epoch, rather than a heartbeat's send time.
type DindMeter struct {
	RegisterNonce string    `json:"register_nonce"`
	Epoch         int64     `json:"epoch"`
	SampledAt     time.Time `json:"sampled_at"`
}

// DindMaintenance binds one operation to a deployment, PVC and registration.
// Reason is request-only: recycle_disabled is accepted only from the controller.
type DindMaintenance struct {
	ID            string `json:"id"`
	Nonce         string `json:"nonce"`
	Phase         string `json:"phase"`
	DeploymentUID string `json:"deployment_uid"`
	PVCUID        string `json:"pvc_uid"`
	RegisterNonce string `json:"register_nonce"`
	Fenced        bool   `json:"fenced"`
	ReadyACK      bool   `json:"ready_ack"`
	Reason        string `json:"reason,omitempty"`
}

// DindMaintenanceReadyACK proves local quiescence, with measured zero counts.
// Prune results are advisory: failed/unknown resampling does not block recycling.
// Phase cancelled requests worker cancellation using validated below evidence only.
type DindMaintenanceReadyACK struct {
	DindMaintenance
	LocalClaims      *int      `json:"local_claims"`
	LocalExecutions  *int      `json:"local_executions"`
	CustodyClear     bool      `json:"custody_clear"`
	CustodyCheckedAt time.Time `json:"custody_checked_at"`
	Pruned           bool      `json:"pruned"`
	ResampledEpoch   int64     `json:"resampled_epoch"`
}

var ErrDindMaintenanceConflict = errors.New("dind maintenance precondition failed")

func maintenancePending(w store.Worker) bool {
	return w.MaintenancePhase == "requested" || w.MaintenancePhase == "ready" || w.MaintenancePhase == "stopping" || w.MaintenancePhase == "recycling"
}

func DindMaintenanceFromWorker(w store.Worker) *DindMaintenance {
	if !w.MaintenanceID.Valid {
		return nil
	}
	return &DindMaintenance{ID: uuid.UUID(w.MaintenanceID.Bytes).String(), Nonce: w.MaintenanceNonce,
		Phase: w.MaintenancePhase, DeploymentUID: w.MaintenanceDeploymentUid, PVCUID: w.MaintenancePvcUid,
		RegisterNonce: w.MaintenanceRegisterNonce, Fenced: w.MaintenanceFenced, ReadyACK: w.MaintenanceReadyAck}
}

func validDiskPair(used, total *int64) bool {
	return used != nil && total != nil && *total > 0 && *used >= 0 && *used <= *total
}
func diskPairOver(used, total *int64, threshold float64) bool {
	return threshold > 0 && threshold <= 1 && validDiskPair(used, total) && float64(*used)/float64(*total) >= threshold
}

func (s *Service) dindActuable(w store.Worker) bool {
	return w.Kind == "hosted" && !w.Ephemeral && w.DockerEnabled.Valid && w.DockerEnabled.Bool &&
		s.p.DiskPressureThreshold > 0 && s.p.DiskPressureThreshold <= 1 && slices.Contains(w.ProtocolCapabilities, capability.DindMaintenanceV1)
}

func (s *Service) dindFresh(w store.Worker) bool {
	now := s.now()
	return w.DindMeterAt.Valid && !w.DindMeterAt.Time.After(now) && now.Sub(w.DindMeterAt.Time) <= 45*time.Second
}

func (s *Service) dindBelow(w store.Worker) bool {
	return w.DindBelowThreshold && s.dindFresh(w) && w.DindMeterAt.Time.After(w.MaintenanceActivityFloor.Time)
}

func maintenanceMatches(w store.Worker, op DindMaintenance) bool {
	current := DindMaintenanceFromWorker(w)
	return current != nil && current.ID == op.ID && current.Nonce == op.Nonce && current.DeploymentUID == op.DeploymentUID && current.PVCUID == op.PVCUID && current.RegisterNonce == op.RegisterNonce
}

// TransitionDindMaintenance is controller-only. Every request locks the worker
// before checking all nonterminal claims. No internal retries are attempted.
func (s *Service) TransitionDindMaintenance(ctx context.Context, id uuid.UUID, op DindMaintenance) (*DindMaintenance, error) {
	return s.mutateDindMaintenance(ctx, id, op, nil)
}

func (s *Service) AckDindMaintenance(ctx context.Context, id uuid.UUID, ack DindMaintenanceReadyACK) (*DindMaintenance, error) {
	return s.mutateDindMaintenance(ctx, id, ack.DindMaintenance, &ack)
}

func (s *Service) mutateDindMaintenance(ctx context.Context, id uuid.UUID, op DindMaintenance, ack *DindMaintenanceReadyACK) (*DindMaintenance, error) {
	if !validMaintenanceRequest(op) || s.txBeginner == nil {
		return nil, ErrDindMaintenanceConflict
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	w, err := q.GetWorkerForUpdate(ctx, id)
	if err != nil {
		return nil, err
	}
	if w.Kind != "hosted" || w.Ephemeral {
		return nil, ErrDindMaintenanceConflict
	}
	phase := w.MaintenancePhase
	// Retries of a persisted transition return its binding without moving clocks.
	// Worker ACK retries still validate their fresh local evidence below.
	initialRetry := ack == nil && op.Phase == "requested" && maintenancePending(w) && op.ID == "" && op.Nonce == "" &&
		op.DeploymentUID == w.MaintenanceDeploymentUid && op.PVCUID == w.MaintenancePvcUid &&
		w.MaintenanceRegisterNonce == w.SnapshotRegisterNonce.String
	if ack == nil && (initialRetry || maintenanceMatches(w, op) && phase == op.Phase && (phase != "requested" || w.MaintenanceRegisterNonce == w.SnapshotRegisterNonce.String)) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return DindMaintenanceFromWorker(w), nil
	}
	refresh := ack == nil && (op.Phase == "requested" || op.Phase == "ready") && phase == "requested" &&
		w.MaintenanceRegisterNonce != w.SnapshotRegisterNonce.String &&
		w.MaintenanceID.Valid && uuid.UUID(w.MaintenanceID.Bytes).String() == op.ID && w.MaintenanceNonce == op.Nonce &&
		(op.RegisterNonce == w.SnapshotRegisterNonce.String || op.RegisterNonce == w.MaintenanceRegisterNonce)
	if ack != nil && op.Phase == "ready" {
		if !s.dindActuable(w) || !maintenanceMatches(w, op) || phase != "ready" || !w.MaintenanceFenced || w.SnapshotRegisterNonce.String != op.RegisterNonce ||
			ack.LocalClaims == nil || *ack.LocalClaims != 0 || ack.LocalExecutions == nil || *ack.LocalExecutions != 0 || !ack.CustodyClear ||
			!ack.CustodyCheckedAt.After(w.MaintenanceActivityFloor.Time) || ack.CustodyCheckedAt.After(s.now()) || s.now().Sub(ack.CustodyCheckedAt) > 45*time.Second {
			return nil, ErrDindMaintenanceConflict
		}
		if err := maintenanceIdle(ctx, q, w); err != nil {
			return nil, err
		}
		w.MaintenanceReadyAck = true
	} else {
		if ack != nil && (op.Phase != "cancelled" || op.Reason != "" && op.Reason != "below_threshold") {
			return nil, ErrDindMaintenanceConflict
		}
		if op.Phase == "requested" && !maintenancePending(w) || refresh {
			if !refresh && (op.ID != "" || op.Nonce != "") {
				return nil, ErrDindMaintenanceConflict
			}
			if !s.dindActuable(w) || !s.dindFresh(w) || w.DindPressureStreak < 2 || op.DeploymentUID == "" || op.PVCUID == "" {
				return nil, ErrDindMaintenanceConflict
			}
			// Refresh directly into the fence after legacy replacement, so a
			// failed readiness check cannot erase the old binding that schedules it.
			if op.Phase == "ready" {
				if err := maintenanceIdle(ctx, q, w); err != nil {
					return nil, err
				}
				w.MaintenanceFenced = true
			}
			if !refresh {
				w.MaintenanceID = pgconv.UUID(uuid.New())
				w.MaintenanceFenced = false
			}
			w.MaintenanceNonce = uuid.NewString()
			w.MaintenanceDeploymentUid = op.DeploymentUID
			w.MaintenancePvcUid = op.PVCUID
			w.MaintenanceRegisterNonce = w.SnapshotRegisterNonce.String
			w.MaintenanceReadyAck = false
		} else {
			if !maintenanceMatches(w, op) {
				return nil, ErrDindMaintenanceConflict
			}
			switch op.Phase {
			case "ready":
				if phase != "requested" {
					return nil, ErrDindMaintenanceConflict
				}
				if !s.dindActuable(w) || !s.dindFresh(w) || w.DindPressureStreak < 2 || w.SnapshotRegisterNonce.String != op.RegisterNonce {
					return nil, ErrDindMaintenanceConflict
				}
				if err := maintenanceIdle(ctx, q, w); err != nil {
					return nil, err
				}
				w.MaintenanceFenced = true
			case "stopping":
				if phase != "ready" || !w.MaintenanceFenced || !w.MaintenanceReadyAck || !s.dindActuable(w) || s.dindBelow(w) || w.SnapshotRegisterNonce.String != op.RegisterNonce || !w.MaintenanceAckAt.Valid || w.MaintenanceAckAt.Time.After(s.now()) || s.now().Sub(w.MaintenanceAckAt.Time) > 45*time.Second {
					return nil, ErrDindMaintenanceConflict
				}
				if err := maintenanceIdle(ctx, q, w); err != nil {
					return nil, err
				}
			case "recycling":
				if phase != "stopping" {
					return nil, ErrDindMaintenanceConflict
				}
			case "complete":
				// RegisterWorker stamps last_heartbeat_at at dind_register_floor.
				// Require a later, fresh heartbeat from the replacement registration
				// before releasing the fence; keep the operation bound to the old nonce.
				now := s.now()
				if phase != "recycling" || !w.MaintenanceFenced || !s.dindActuable(w) || w.Status != "online" ||
					!w.SnapshotRegisterNonce.Valid || strings.TrimSpace(w.SnapshotRegisterNonce.String) == "" ||
					w.SnapshotRegisterNonce.String == w.MaintenanceRegisterNonce ||
					!w.DindRegisterFloor.Valid || !w.LastHeartbeatAt.Valid ||
					!w.LastHeartbeatAt.Time.After(w.DindRegisterFloor.Time) ||
					w.LastHeartbeatAt.Time.After(now) || now.Sub(w.LastHeartbeatAt.Time) > 45*time.Second {
					return nil, ErrDindMaintenanceConflict
				}
				if err := maintenanceIdle(ctx, q, w); err != nil {
					return nil, err
				}
				w.MaintenanceFenced = false
				w.MaintenanceReadyAck = false
			case "cancelled":
				if phase == "cancelled" && ack != nil {
					if err := tx.Commit(ctx); err != nil {
						return nil, err
					}
					return DindMaintenanceFromWorker(w), nil
				}
				if phase != "requested" && phase != "ready" {
					return nil, ErrDindMaintenanceConflict
				}
				if (ack != nil || op.Reason != "recycle_disabled") && !s.dindBelow(w) {
					return nil, ErrDindMaintenanceConflict
				}
				w.MaintenanceFenced = false
				w.MaintenanceReadyAck = false
			default:
				return nil, ErrDindMaintenanceConflict
			}
		}
		w.MaintenancePhase = op.Phase
	}
	var custodyCheckedAt time.Time
	if ack != nil {
		custodyCheckedAt = ack.CustodyCheckedAt
	}
	updated, err := q.SetDindMaintenance(ctx, store.SetDindMaintenanceParams{
		ID: id, MaintenanceID: w.MaintenanceID, MaintenanceNonce: w.MaintenanceNonce, MaintenancePhase: w.MaintenancePhase,
		MaintenanceDeploymentUid: w.MaintenanceDeploymentUid, MaintenancePvcUid: w.MaintenancePvcUid, MaintenanceRegisterNonce: w.MaintenanceRegisterNonce,
		MaintenanceFenced: w.MaintenanceFenced, MaintenanceReadyAck: w.MaintenanceReadyAck,
		AdvanceActivityFloor: phase != w.MaintenancePhase || refresh,
		ResetPressure:        op.Phase == "complete", RefreshReadyAck: ack != nil && op.Phase == "ready",
		CustodyCheckedAt: pgconv.Time(custodyCheckedAt),
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return DindMaintenanceFromWorker(updated), nil
}

// Bound request-only text before storing an operation. Unknown states or reasons
// cannot become an instruction, even on the controller-authenticated route.
func validMaintenanceRequest(op DindMaintenance) bool {
	switch op.Phase {
	case "requested", "ready", "stopping", "recycling", "complete", "cancelled":
	default:
		return false
	}
	for _, value := range []string{op.ID, op.Nonce, op.DeploymentUID, op.PVCUID, op.RegisterNonce} {
		if len(value) > 128 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t") {
			return false
		}
	}
	if op.DeploymentUID == "" || op.PVCUID == "" {
		return false
	}
	if op.ID != "" {
		if _, err := uuid.Parse(op.ID); err != nil {
			return false
		}
	} else if op.Phase != "requested" {
		return false
	}
	return op.Reason == "" || op.Phase == "cancelled" && (op.Reason == "below_threshold" || op.Reason == "recycle_disabled")
}

func maintenanceIdle(ctx context.Context, q *store.Queries, w store.Worker) error {
	n, err := q.CountWorkerNonTerminalRuns(ctx, store.CountWorkerNonTerminalRunsParams{WorkerID: pgconv.UUID(w.ID), UserID: w.UserID})
	if err != nil {
		return err
	}
	if n != 0 {
		return ErrDindMaintenanceConflict
	}
	held, err := q.DindMaintenanceCustodyHeld(ctx, pgconv.UUID(w.ID))
	if err != nil {
		return err
	}
	if held {
		return ErrDindMaintenanceConflict
	}
	return nil
}
