package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const recoveryCauseWorkerMemoryPressure = "worker_memory_pressure"

var (
	ErrMemoryBinding = errors.New("memory intervention binding conflict")
	ErrMemoryStale   = errors.New("memory intervention claim is no longer active")
)

// MemoryPolicy is frozen for the entire episode, including across new claims.
type MemoryPolicy struct {
	Version          int   `json:"version"`
	MaxInterventions int32 `json:"max_interventions"`
}

func memoryPolicy(raw []byte) (MemoryPolicy, error) {
	var p MemoryPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("%w: invalid memory policy", ErrInvalidState)
	}
	if err := p.validate(); err != nil {
		return p, err
	}
	return p, nil
}

func (p MemoryPolicy) validate() error {
	if p.Version != 1 || p.MaxInterventions <= 0 || p.MaxInterventions > 9999 {
		return fmt.Errorf("%w: invalid memory policy", ErrInvalidState)
	}
	return nil
}

// optionalMemoryPolicy preserves an absent policy on claims until worker opt-in.
func optionalMemoryPolicy(raw []byte) (*MemoryPolicy, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	p, err := memoryPolicy(raw)
	return &p, err
}

type MemoryReservationRequest struct {
	MemoryBinding
	Policy MemoryPolicy `json:"policy"`
}

// MemoryBinding names one worker-generated UUID. It is never rebound by a retry.
type MemoryBinding struct {
	RunID           uuid.UUID `json:"run_id"`
	WorkerID        uuid.UUID `json:"worker_id"`
	RegisterNonce   string    `json:"register_nonce"`
	ClaimGeneration int64     `json:"claim_generation"`
	MemoryEpisode   int64     `json:"memory_episode"`
	InterventionID  uuid.UUID `json:"intervention_id"`
}

type MemoryAllowance struct {
	Limit     int32 `json:"limit"`
	Used      int32 `json:"used"`
	Remaining int32 `json:"remaining"`
}

type MemoryReservation struct {
	MemoryBinding
	Admitted    bool            `json:"admitted"`
	Authorizing bool            `json:"authorizing"`
	Allowance   MemoryAllowance `json:"allowance"`
	Policy      MemoryPolicy    `json:"policy"`
	Outcome     string          `json:"outcome,omitempty"`
}

type MemoryOutcomeRequest struct {
	MemoryBinding
	Outcome string `json:"outcome"`
}

func (b MemoryBinding) validate(w store.Worker) error {
	if b.RunID == uuid.Nil || b.InterventionID == uuid.Nil || b.WorkerID != w.ID ||
		b.RegisterNonce == "" || len(b.RegisterNonce) > 128 ||
		b.ClaimGeneration <= 0 || b.MemoryEpisode < 0 {
		return fmt.Errorf("%w: invalid memory binding", ErrInvalidState)
	}
	return nil
}

func memoryBindingMatches(row store.MemoryIntervention, b MemoryBinding, user uuid.UUID) bool {
	return row.InterventionID == b.InterventionID && row.RunID == b.RunID &&
		row.WorkerID == b.WorkerID && row.UserID == user &&
		row.RegisterNonce == b.RegisterNonce && row.ClaimGeneration == b.ClaimGeneration &&
		row.MemoryEpisode == b.MemoryEpisode
}

func memoryCurrent(w store.Worker, r store.Run, b MemoryBinding) bool {
	return w.ID == b.WorkerID && w.UserID == r.UserID &&
		w.SnapshotRegisterNonce.Valid && w.SnapshotRegisterNonce.String == b.RegisterNonce &&
		r.WorkerID == pgconv.UUID(w.ID) && !r.ClaimReleasedAt.Valid &&
		r.ClaimGeneration == b.ClaimGeneration && r.MemoryEpisode == b.MemoryEpisode &&
		(r.Status == "claimed" || r.Status == "running") &&
		!r.PauseRequestedAt.Valid && !r.CredentialSwitchRequestedAt.Valid
}

// memoryLock always takes the worker lock before applicable parent and run locks.
// Auth middleware's worker snapshot is not authority for nonce or tenant checks.
func (s *Service) memoryLock(ctx context.Context, w store.Worker, b MemoryBinding) (pgx.Tx, *store.Queries, store.Worker, store.Run, error) {
	if s.txBeginner == nil {
		return nil, nil, store.Worker{}, store.Run{}, errors.New("memory transaction unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, nil, store.Worker{}, store.Run{}, err
	}
	q := store.New(tx)
	locked, err := q.GetWorkerForUpdate(ctx, w.ID)
	if err == nil && (locked.ID != w.ID || locked.UserID != w.UserID ||
		!slices.Contains(locked.ProtocolCapabilities, capability.WorkerMemoryPressureV1)) {
		err = ErrMemoryStale
	}
	var run store.Run
	if err == nil {
		run, err = q.GetMemoryRunForUpdate(ctx, store.GetMemoryRunForUpdateParams{ID: b.RunID, UserID: locked.UserID})
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrRunNotOwned
		}
		return nil, nil, locked, run, err
	}
	return tx, q, locked, run, nil
}

func memoryParentCurrent(ctx context.Context, q *store.Queries, run store.Run) (bool, error) {
	if run.Kind != "cross_check" {
		return true, nil
	}
	return q.MemoryParentEligible(ctx, pgconv.UUID(run.ID))
}

func memoryResponse(row store.MemoryIntervention, current bool) (MemoryReservation, error) {
	p, err := memoryPolicy(row.Policy)
	return MemoryReservation{
		MemoryBinding: MemoryBinding{RunID: row.RunID, WorkerID: row.WorkerID,
			RegisterNonce: row.RegisterNonce, ClaimGeneration: row.ClaimGeneration,
			MemoryEpisode: row.MemoryEpisode, InterventionID: row.InterventionID},
		Admitted: row.Admitted, Authorizing: row.Admitted && current && !row.Outcome.Valid,
		Policy: p, Outcome: row.Outcome.String,
		Allowance: MemoryAllowance{Limit: p.MaxInterventions, Used: row.AllowanceUsed,
			Remaining: max(0, p.MaxInterventions-row.AllowanceUsed)},
	}, err
}

// ReserveMemoryIntervention serializes admission under the run lock. A globally
// conflicting ID rolls back; an exact duplicate charges nothing. Denials are also
// immutable ledger entries. The request context bounds lock waits; no retry loop.
func (s *Service) ReserveMemoryIntervention(ctx context.Context, w store.Worker, req MemoryReservationRequest) (MemoryReservation, error) {
	b := req.MemoryBinding
	if err := b.validate(w); err != nil {
		return MemoryReservation{}, err
	}
	tx, q, locked, run, err := s.memoryLock(ctx, w, b)
	if err != nil {
		return MemoryReservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	parent, err := memoryParentCurrent(ctx, q, run)
	if err != nil {
		return MemoryReservation{}, err
	}
	current := memoryCurrent(locked, run, b) && parent
	prior, err := q.GetMemoryIntervention(ctx, b.InterventionID)
	if err == nil {
		if !memoryBindingMatches(prior, b, locked.UserID) {
			return MemoryReservation{}, ErrMemoryBinding
		}
		if err := req.Policy.validate(); err != nil {
			return MemoryReservation{}, err
		}
		persisted, err := memoryPolicy(prior.Policy)
		if err != nil {
			return MemoryReservation{}, err
		}
		if persisted != req.Policy {
			return MemoryReservation{}, ErrMemoryBinding
		}
		return memoryResponse(prior, current)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MemoryReservation{}, err
	}
	if !current {
		return MemoryReservation{}, ErrMemoryStale
	}
	p := req.Policy
	if err := p.validate(); err != nil {
		return MemoryReservation{}, err
	}
	if len(run.MemoryPolicy) != 0 {
		frozen, err := memoryPolicy(run.MemoryPolicy)
		if err != nil {
			return MemoryReservation{}, err
		}
		if p != frozen {
			return MemoryReservation{}, ErrMemoryBinding
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return MemoryReservation{}, err
	}
	admitted := run.MemoryInterventionCount < p.MaxInterventions
	used := run.MemoryInterventionCount
	if admitted {
		used++
	}
	inserted, err := q.InsertMemoryIntervention(ctx, store.InsertMemoryInterventionParams{
		InterventionID: b.InterventionID, RunID: b.RunID, UserID: locked.UserID,
		WorkerID: b.WorkerID, RegisterNonce: b.RegisterNonce, ClaimGeneration: b.ClaimGeneration,
		MemoryEpisode: b.MemoryEpisode, Admitted: admitted, AllowanceUsed: used, Policy: raw,
	})
	if err != nil {
		return MemoryReservation{}, err
	}
	// Another run/worker can race on the globally unique ID. Do not charge until
	// insertion succeeds, and never change the winner's binding.
	if inserted == 0 {
		prior, err = q.GetMemoryIntervention(ctx, b.InterventionID)
		if err != nil {
			return MemoryReservation{}, err
		}
		if !memoryBindingMatches(prior, b, locked.UserID) {
			return MemoryReservation{}, ErrMemoryBinding
		}
		return memoryResponse(prior, current)
	}
	if admitted {
		_, err = q.ChargeMemoryIntervention(ctx, store.ChargeMemoryInterventionParams{ID: b.RunID, Policy: raw})
	} else {
		_, err = q.FreezeMemoryPolicy(ctx, store.FreezeMemoryPolicyParams{ID: b.RunID, Policy: raw})
	}
	if err != nil {
		return MemoryReservation{}, err
	}
	prior, err = q.GetMemoryIntervention(ctx, b.InterventionID)
	if err != nil {
		return MemoryReservation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return MemoryReservation{}, err
	}
	return memoryResponse(prior, current)
}

// RecordMemoryInterventionOutcome records worker evidence only. The first outcome
// is immutable; ambiguous/no_signal outcomes never refund. Even a current outcome
// ACK is non-authorizing, and no outcome settles custody or emits a feed frame.
func (s *Service) RecordMemoryInterventionOutcome(ctx context.Context, w store.Worker, req MemoryOutcomeRequest) (MemoryReservation, error) {
	if err := req.validate(w); err != nil {
		return MemoryReservation{}, err
	}
	if req.Outcome != "no_signal" && req.Outcome != "unknown" && req.Outcome != "confirmed_drained" {
		return MemoryReservation{}, fmt.Errorf("%w: invalid memory outcome", ErrInvalidState)
	}
	tx, q, locked, _, err := s.memoryLock(ctx, w, req.MemoryBinding)
	if err != nil {
		return MemoryReservation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := q.GetMemoryIntervention(ctx, req.InterventionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemoryReservation{}, ErrMemoryStale
	}
	if err != nil {
		return MemoryReservation{}, err
	}
	if !memoryBindingMatches(row, req.MemoryBinding, locked.UserID) {
		return MemoryReservation{}, ErrMemoryBinding
	}
	if !row.Admitted || (row.Outcome.Valid && row.Outcome.String != req.Outcome) {
		return MemoryReservation{}, ErrMemoryBinding
	}
	row, err = q.RecordMemoryOutcome(ctx, store.RecordMemoryOutcomeParams{InterventionID: req.InterventionID, Outcome: req.Outcome})
	if err != nil {
		return MemoryReservation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return MemoryReservation{}, err
	}
	return memoryResponse(row, false)
}

func (s *Service) parkWorkerMemoryPressure(ctx context.Context, w store.Worker, id uuid.UUID, req StateRequest) (store.Run, bool, error) {
	if req.State != "recovery_wait" || req.ClaimGeneration == nil || req.MemoryEpisode == nil || req.RegisterNonce == "" {
		return store.Run{}, false, fmt.Errorf("%w: memory hold requires nonce, generation and episode", ErrInvalidState)
	}
	b := MemoryBinding{RunID: id, WorkerID: w.ID, RegisterNonce: req.RegisterNonce,
		ClaimGeneration: *req.ClaimGeneration, MemoryEpisode: *req.MemoryEpisode}
	tx, q, locked, run, err := s.memoryLock(ctx, w, b)
	if err != nil {
		return run, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if locked.SnapshotRegisterNonce.String == b.RegisterNonce && run.ClaimGeneration == b.ClaimGeneration &&
		run.MemoryEpisode == b.MemoryEpisode && run.WorkerID == pgconv.UUID(w.ID) &&
		run.Status == "recovery_wait" && run.RecoveryWaitCause.String == recoveryCauseWorkerMemoryPressure {
		return run, false, nil
	}
	parent, err := memoryParentCurrent(ctx, q, run)
	if err != nil {
		return run, false, err
	}
	if !memoryCurrent(locked, run, b) || !parent {
		return run, false, ErrStaleClaim
	}
	run, err = q.ParkWorkerMemoryPressure(ctx, store.ParkWorkerMemoryPressureParams{
		ID: id, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: b.ClaimGeneration,
		MemoryEpisode: b.MemoryEpisode, RegisterNonce: pgconv.Text(b.RegisterNonce),
	})
	if err != nil {
		return run, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return run, false, err
	}
	if s.bcast != nil {
		s.bcast.PublishState(id, run.Status)
	}
	return run, true, nil
}
