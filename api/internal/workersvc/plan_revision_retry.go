package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrStaleRevisionSource = errors.New("plan revision source is no longer current")

const maxSafeRevisionID int64 = 1<<53 - 1

// revisionRetryReceipt lives beside the immutable gate payload. It is excluded from
// the presentation digest and records the exact settlement set for lost-ACK replay.
type revisionRetryReceipt struct {
	SourceRevision  int64   `json:"source_revision"`
	RevisionInputID int64   `json:"revision_input_id"`
	SupersededIDs   []int64 `json:"superseded_revision_input_ids,omitempty"`
}

func validateRevisionRetry(req StateRequest) error {
	present := req.ExpectedGateRevision != nil || req.RevisionInputID != nil || len(req.SupersededRevisionInputIDs) > 0
	if !present {
		return nil
	}
	if req.State != "awaiting_approval" && req.State != "failed" {
		return ErrInvalidState
	}
	if req.ExpectedGateRevision == nil || *req.ExpectedGateRevision <= 0 || *req.ExpectedGateRevision > maxSafeRevisionID {
		return ErrInvalidState
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return ErrClaimGenerationRequired
	}
	if req.State == "failed" && (req.RevisionInputID != nil || len(req.SupersededRevisionInputIDs) > 0) {
		return ErrInvalidState
	}
	if req.RevisionInputID == nil {
		if len(req.SupersededRevisionInputIDs) > 0 {
			return ErrInvalidState
		}
		return nil
	}
	if req.PlanMd == nil {
		return ErrInvalidState
	}
	plan, _ := stripNUL(*req.PlanMd)
	if strings.TrimSpace(plan) == "" {
		return ErrInvalidState
	}
	if req.PresentationID == nil || *req.RevisionInputID <= 0 || *req.RevisionInputID > maxSafeRevisionID || len(req.SupersededRevisionInputIDs) > 1000 {
		return ErrInvalidState
	}
	prev := int64(0)
	for _, id := range req.SupersededRevisionInputIDs {
		if id <= prev || id >= *req.RevisionInputID {
			return ErrInvalidState
		}
		prev = id
	}
	return nil
}

func requestedRevisionReceipt(req StateRequest) revisionRetryReceipt {
	return revisionRetryReceipt{SourceRevision: *req.ExpectedGateRevision, RevisionInputID: *req.RevisionInputID, SupersededIDs: req.SupersededRevisionInputIDs}
}

// validateRevisionTargets runs under the same run lock as the publication.
// Every target is read once; a failure aborts the whole transaction.
func validateRevisionTargets(ctx context.Context, q *store.Queries, run store.Run, worker uuid.UUID, req StateRequest) error {
	ids := append(slices.Clone(req.SupersededRevisionInputIDs), *req.RevisionInputID)
	rows, err := q.ListInputReceiptRows(ctx, store.ListInputReceiptRowsParams{RunID: run.ID, Ids: ids})
	if err != nil {
		return err
	}
	if len(rows) != len(ids) {
		return ErrStaleRevisionSource
	}
	for _, row := range rows {
		if row.Kind != "revise_plan" || row.AppliedAt.Valid || !row.ConsumedAt.Valid ||
			!row.ConsumedWorkerID.Valid || uuid.UUID(row.ConsumedWorkerID.Bytes) != worker ||
			!row.ConsumedClaimGeneration.Valid || row.ConsumedClaimGeneration.Int64 != run.ClaimGeneration ||
			row.GateBinding.String != "bound" || !row.GateRevision.Valid || row.GateRevision.Int64 != *req.ExpectedGateRevision {
			return ErrStaleRevisionSource
		}
	}
	return nil
}

// requireSemanticMutation runs only after exact replay has been ruled out.
// The pending receipt grants compilation/review, never execution or approval.
func requireSemanticMutation(ctx context.Context, q *store.Queries, run store.Run, worker uuid.UUID) error {
	if run.Status == "running" {
		return nil
	}
	if run.Status != "awaiting_approval" {
		return ErrInvalidState
	}
	pending, err := q.HasPendingSemanticRevision(ctx, store.HasPendingSemanticRevisionParams{
		RunID: run.ID, WorkerID: pgconv.UUID(worker), ClaimGeneration: pgtype.Int8{Int64: run.ClaimGeneration, Valid: true},
		GateRevision: pgtype.Int8{Int64: run.GateRevision, Valid: true},
	})
	if err != nil {
		return err
	}
	if !pending {
		return ErrStaleRevisionSource
	}
	return nil
}

func revisionReceiptReplay(run store.Run, req StateRequest) bool {
	var metadata struct {
		Receipt *revisionRetryReceipt `json:"revision_retry_receipt"`
	}
	if json.Unmarshal(run.GatePresentedPayload, &metadata) != nil || metadata.Receipt == nil {
		return false
	}
	want := requestedRevisionReceipt(req)
	got := metadata.Receipt
	return got.SourceRevision == want.SourceRevision && got.RevisionInputID == want.RevisionInputID && slices.Equal(got.SupersededIDs, want.SupersededIDs)
}

func settleRevisionTargets(ctx context.Context, q *store.Queries, run store.Run, worker uuid.UUID, req StateRequest) error {
	receipt, err := json.Marshal(requestedRevisionReceipt(req))
	if err != nil {
		return err
	}
	n, err := q.SettlePlanRevisionRetry(ctx, run.ID, worker, run.ClaimGeneration, *req.RevisionInputID, req.SupersededRevisionInputIDs, receipt)
	if err != nil {
		return err
	}
	if n != int64(len(req.SupersededRevisionInputIDs)+1) {
		return fmt.Errorf("revision settlement lost locked targets: %w", ErrStaleRevisionSource)
	}
	return nil
}
