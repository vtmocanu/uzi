package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CodexOperationSource identifies the consumed human revision receipt, not an approval.
type CodexOperationSource struct {
	GateRevision    int64  `json:"gate_revision"`
	PresentationID  string `json:"presentation_id"`
	RevisionInputID int64  `json:"revision_input_id"`
}

type CodexOperationIdentity struct {
	OperationID string                `json:"operation_id"`
	Kind        string                `json:"kind"`
	Source      *CodexOperationSource `json:"source,omitempty"`
}

type CodexOperationRequest struct {
	ClaimGeneration int64 `json:"claim_generation"`
	CodexOperationIdentity
}

func canonicalOperationUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func (operation CodexOperationIdentity) Validate() error {
	if !canonicalOperationUUID(operation.OperationID) {
		return ErrCodexAdmissionInvalid
	}
	switch operation.Kind {
	case "root", "child", "advice", "checker":
	default:
		return ErrCodexAdmissionInvalid
	}
	if source := operation.Source; source != nil {
		if source.GateRevision <= 0 || source.GateRevision > maxSafeRevisionID ||
			source.RevisionInputID <= 0 || source.RevisionInputID > maxSafeRevisionID ||
			!canonicalOperationUUID(source.PresentationID) {
			return ErrCodexAdmissionInvalid
		}
	}
	return nil
}

func (req CodexOperationRequest) Validate() error {
	if req.ClaimGeneration <= 0 || req.ClaimGeneration > maxSafeRevisionID {
		return ErrCodexAdmissionInvalid
	}
	return req.CodexOperationIdentity.Validate()
}

// CodexOperationDecision is a closed admit/wait union on the wire. Wait-only
// fields are omitted on admit; no account or credential identity is disclosed.
type CodexOperationDecision struct {
	Decision        string     `json:"decision"`
	OperationID     string     `json:"operation_id"`
	ClaimGeneration int64      `json:"claim_generation"`
	Class           string     `json:"class,omitempty"`
	EpisodeID       string     `json:"episode_id,omitempty"`
	ServerNow       *time.Time `json:"server_now,omitempty"`
	EligibleAt      *time.Time `json:"eligible_at,omitempty"`
}

// CheckCodexOperationAdmission reads policy without changing admission, lifecycle,
// or custody. The run lock serializes release, generation and source settlement.
func (s *Service) CheckCodexOperationAdmission(ctx context.Context, worker store.Worker, runID uuid.UUID, req CodexOperationRequest) (CodexOperationDecision, error) {
	if err := req.Validate(); err != nil {
		return CodexOperationDecision{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	if s.txBeginner == nil {
		return CodexOperationDecision{}, errors.New("admission database unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return CodexOperationDecision{}, err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
		defer done()
		_ = tx.Rollback(cleanup)
	}()
	// Keep source and binding reads in the same transaction snapshot.
	for _, statement := range []string{
		"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET LOCAL lock_timeout = '500ms'",
		"SET LOCAL statement_timeout = '2000ms'",
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return CodexOperationDecision{}, err
		}
	}
	q := store.New(tx)
	if _, err = q.LockCodexAdmissionRun(ctx, store.LockCodexAdmissionRunParams{
		Column1: runID, Column2: worker.UserID, Column3: worker.ID, Column4: req.ClaimGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrCodexAdmissionRefused
		}
		return CodexOperationDecision{}, err
	}
	decision, err := readCodexOperationAdmission(ctx, q, worker, runID, req)
	if err != nil {
		return CodexOperationDecision{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CodexOperationDecision{}, err
	}
	return decision, nil
}

func readCodexOperationAdmission(ctx context.Context, q *store.Queries, worker store.Worker, runID uuid.UUID, req CodexOperationRequest) (CodexOperationDecision, error) {
	out := CodexOperationDecision{Decision: "admit", OperationID: req.OperationID, ClaimGeneration: req.ClaimGeneration}
	auth, err := q.GetCodexOperationAuthority(ctx, store.GetCodexOperationAuthorityParams{
		RunID: runID, UserID: worker.UserID, WorkerID: worker.ID, ClaimGeneration: req.ClaimGeneration,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrCodexAdmissionRefused
		}
		return out, err
	}
	if req.Source == nil {
		if auth.RevisionActive {
			return out, ErrStaleRevisionSource
		}
	} else {
		source := req.Source
		presentation, _ := uuid.Parse(source.PresentationID)
		if source.GateRevision != auth.GateRevision || !auth.GatePresentationID.Valid ||
			presentation != uuid.UUID(auth.GatePresentationID.Bytes) {
			return out, ErrStaleRevisionSource
		}
		valid, err := q.ValidCodexOperationSource(ctx, store.ValidCodexOperationSourceParams{
			RunID: runID, WorkerID: worker.ID, ClaimGeneration: req.ClaimGeneration,
			GateRevision: source.GateRevision, RevisionInputID: source.RevisionInputID,
		})
		if err != nil {
			return out, err
		}
		if !valid {
			return out, ErrStaleRevisionSource
		}
	}
	cooldown := auth
	if cooldown.EligibleAt.Valid && cooldown.EligibleAt.Time.After(cooldown.ServerNow.Time) {
		if !cooldown.EpisodeID.Valid {
			return out, errors.New("cooldown episode unavailable")
		}
		out.Decision, out.Class = "wait", cooldown.Class
		out.EpisodeID = uuid.UUID(cooldown.EpisodeID.Bytes).String()
		out.ServerNow, out.EligibleAt = &cooldown.ServerNow.Time, &cooldown.EligibleAt.Time
	}
	return out, nil
}

// Optional malformed metadata is discarded independently of custody validation.
func parseActiveCooldown(raw json.RawMessage) *CodexOperationIdentity {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var operation CodexOperationIdentity
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&operation) != nil || operation.Validate() != nil {
		return nil
	}
	return &operation
}

func applyActiveCooldown(ctx context.Context, q *store.Queries, worker store.Worker, e validatedEntry) error {
	params := store.SetWorkerActiveCooldownParams{WorkerID: worker.ID, RunID: e.runID}
	if e.cooldown != nil && e.claimGeneration > 0 && !e.terminalPending {
		req := CodexOperationRequest{ClaimGeneration: e.claimGeneration, CodexOperationIdentity: *e.cooldown}
		decision, err := readCodexOperationAdmission(ctx, q, worker, e.runID, req)
		if err != nil && !errors.Is(err, ErrCodexAdmissionRefused) && !errors.Is(err, ErrStaleRevisionSource) {
			return err
		}
		if err == nil && decision.Decision == "wait" {
			raw, err := json.Marshal(e.cooldown)
			if err != nil {
				return err
			}
			episode, _ := uuid.Parse(decision.EpisodeID)
			params.Operation = raw
			params.Class = pgtype.Text{String: decision.Class, Valid: true}
			params.EpisodeID = pgtype.UUID{Bytes: episode, Valid: true}
			params.EligibleAt = pgtype.Timestamptz{Time: *decision.EligibleAt, Valid: true}
			params.ObservedAt = pgtype.Timestamptz{Time: *decision.ServerNow, Valid: true}
		}
	}
	return q.SetWorkerActiveCooldown(ctx, params)
}
