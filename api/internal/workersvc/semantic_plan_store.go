package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// semanticSnapshotBlobs assembles the exact bytea inputs persisted by submitSemanticCandidate.
func semanticSnapshotBlobs(planMd string, captures []apitypes.SemanticSourceCapture) map[string][]byte {
	blobs := map[string][]byte{}
	for _, capture := range captures {
		raw := capture.Bytes
		if raw == nil {
			raw = []byte{}
		}
		blobs[semanticHash(raw)] = raw
	}
	plan := []byte(planMd)
	blobs[semanticHash(plan)] = plan
	return blobs
}

// submitSemanticCandidate has no production caller or route. The source capture
// argument is worker-owned, distinct from the strictly decoded lead proposal.
// Every mutation requires a transaction and the run lock serializes all capacity,
// revision, ownership and exact-current retry checks. No history is evicted.
func (s *Service) submitSemanticCandidate(ctx context.Context, workerID, runID uuid.UUID, generation int64, structural PlanCrossCheckCandidate, proposal []byte, captures []apitypes.SemanticSourceCapture) (apitypes.SemanticCandidate, error) {
	var empty apitypes.SemanticCandidate
	if s.txBeginner == nil {
		return empty, fmt.Errorf("semantic candidate requires transaction")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	run, err := q.GetRunByIDForUpdate(ctx, runID)
	if err != nil {
		return empty, err
	}
	if !run.WorkerID.Valid || run.WorkerID.Bytes != workerID {
		return empty, ErrRunNotOwned
	}
	if run.ClaimGeneration != generation || run.ClaimReleasedAt.Valid {
		return empty, ErrStaleClaim
	}
	if terminalStatuses[run.Status] {
		return empty, ErrRunTerminal
	}
	if run.Status != "running" {
		return empty, ErrInvalidState
	}
	if !run.CompletionContractVersion.Valid || run.CompletionContractVersion.Int32 != 1 {
		return empty, fmt.Errorf("semantic enrollment requires contract schema version 1")
	}
	planning, err := q.GetSemanticPlanning(ctx, runID)
	if err != nil {
		return empty, err
	}
	enrollmentRaw, priorRaw := planning.Enrollment, planning.Candidate
	revision, fingerprint := planning.Revision, planning.Fingerprint
	var enrollment apitypes.SemanticEnrollment
	if err = strictSemanticJSON(enrollmentRaw, &enrollment); err != nil {
		return empty, err
	}
	if enrollment.Profile != "semantic" || enrollment.Policy.Version != 1 {
		return empty, fmt.Errorf("semantic enrollment required")
	}
	candidate, fp, err := compileSemanticProposal(structural, proposal, captures)
	if err != nil {
		return empty, err
	}
	if fingerprint.Valid && fingerprint.String == fp {
		if err = strictSemanticJSON(priorRaw, &candidate); err != nil {
			return empty, err
		}
		if candidate.Revision != revision || candidate.Digest == "" {
			return empty, fmt.Errorf("invalid persisted semantic candidate")
		}
		if _, err = buildSemanticCompletionContract(candidate); err != nil {
			return empty, err
		}
		// A lost response is a read of the current persisted candidate; no UUIDs or
		// bytes are charged and no historical approval is restored.
		if err = tx.Commit(ctx); err != nil {
			return empty, err
		}
		return candidate, nil
	}
	if revision >= 128 {
		return empty, fmt.Errorf("semantic candidate capacity exceeded")
	}
	blobs := semanticSnapshotBlobs(structural.PlanMd, captures)
	used, err := q.GetSemanticSnapshotBytes(ctx, runID)
	if err != nil {
		return empty, err
	}
	// At most ten distinct blobs per submission. A failure aborts the entire
	// transaction; there is no partial admission or best-effort eviction.
	for hash, raw := range blobs {
		exists, err := q.HasSemanticSnapshot(ctx, store.HasSemanticSnapshotParams{RunID: runID, Hash: hash})
		if err != nil {
			return empty, err
		}
		if !exists {
			used += int64(len(raw))
		}
	}
	if used > 16*1024*1024 {
		return empty, fmt.Errorf("semantic snapshot capacity exceeded")
	}
	for hash, raw := range blobs {
		if err = q.InsertSemanticSnapshot(ctx, store.InsertSemanticSnapshotParams{RunID: runID, Hash: hash, Raw: raw}); err != nil {
			return empty, err
		}
	}
	refs := append([]apitypes.SemanticSourceRef(nil), candidate.Sources...)
	refs = append(refs, apitypes.SemanticSourceRef{ID: "plan", Kind: "plan", Hash: candidate.PlanHash, Provenance: "worker:proposed-plan", Completeness: "complete"})
	// At most nine captured sources plus the plan. Each failure rolls back
	// sibling inserts and leaves the previous candidate and all history intact.
	for _, ref := range refs {
		metadata, marshalErr := json.Marshal(ref)
		if marshalErr != nil {
			return empty, marshalErr
		}
		if err = q.InsertSemanticSourceSnapshot(ctx, store.InsertSemanticSourceSnapshotParams{RunID: runID, IdentityHash: semanticHash(metadata), SourceID: ref.ID, Kind: ref.Kind, Hash: ref.Hash, Provenance: ref.Provenance, Completeness: ref.Completeness}); err != nil {
			return empty, err
		}
	}
	candidate.Revision = revision + 1
	for i := range candidate.Criteria {
		candidate.Criteria[i].ID = uuid.NewString()
	}
	// Revision participates in exact candidate identity: A->B->A cannot restore
	// any prior approval, even if a criterion's text returns to its original form.
	identity, err := json.Marshal(struct {
		Fingerprint string
		Candidate   apitypes.SemanticCandidate
	}{fp, candidate})
	if err != nil {
		return empty, err
	}
	candidate.Digest = semanticHash(append([]byte("uzi:semantic_v1:candidate\x00"), identity...))
	raw, err := json.Marshal(candidate)
	if err != nil {
		return empty, err
	}
	if err = q.InsertSemanticCandidateHistory(ctx, store.InsertSemanticCandidateHistoryParams{RunID: runID, Revision: candidate.Revision, Candidate: raw}); err != nil {
		return empty, err
	}
	affected, err := q.UpdateSemanticPlanning(ctx, store.UpdateSemanticPlanningParams{RunID: runID, Revision: candidate.Revision, Fingerprint: pgtype.Text{String: fp, Valid: true}, Candidate: raw})
	if err != nil {
		return empty, err
	}
	if affected != 1 {
		return empty, errors.New("semantic planning state disappeared")
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return candidate, nil
}
