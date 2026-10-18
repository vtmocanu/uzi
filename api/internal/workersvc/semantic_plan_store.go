package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
	run, err := store.New(tx).GetRunByIDForUpdate(ctx, runID)
	if err != nil {
		return empty, err
	}
	if !run.WorkerID.Valid || run.WorkerID.Bytes != workerID || run.ClaimGeneration != generation {
		return empty, ErrRunNotOwned
	}
	if !run.CompletionContractVersion.Valid || run.CompletionContractVersion.Int32 != 1 {
		return empty, fmt.Errorf("semantic enrollment requires contract schema version 1")
	}
	if terminalStatuses[run.Status] {
		return empty, ErrRunTerminal
	}
	var enrollmentRaw, priorRaw []byte
	var revision int64
	var fingerprint *string
	err = tx.QueryRow(ctx, `SELECT enrollment, revision, fingerprint, candidate FROM semantic_planning WHERE run_id=$1`, runID).Scan(&enrollmentRaw, &revision, &fingerprint, &priorRaw)
	if err != nil {
		return empty, err
	}
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
	if fingerprint != nil && *fingerprint == fp {
		if err = strictSemanticJSON(priorRaw, &candidate); err != nil {
			return empty, err
		}
		if candidate.Revision != revision || candidate.Digest == "" {
			return empty, fmt.Errorf("invalid persisted semantic candidate")
		}
		if _, err = buildSemanticCompletionContract(candidate, 1); err != nil {
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
	blobs := map[string][]byte{}
	for _, capture := range captures {
		blobs[semanticHash(capture.Bytes)] = capture.Bytes
	}
	plan := []byte(structural.PlanMd)
	blobs[semanticHash(plan)] = plan
	var used int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(octet_length(raw)),0)::bigint FROM semantic_snapshots WHERE run_id=$1`, runID).Scan(&used); err != nil {
		return empty, err
	}
	// At most ten distinct blobs per submission. A failure aborts the entire
	// transaction; there is no partial admission or best-effort eviction.
	for hash, raw := range blobs {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM semantic_snapshots WHERE run_id=$1 AND hash=$2)`, runID, hash).Scan(&exists); err != nil {
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
		if _, err = tx.Exec(ctx, `INSERT INTO semantic_snapshots(run_id,hash,raw) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, runID, hash, raw); err != nil {
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
		if _, err = tx.Exec(ctx, `INSERT INTO semantic_source_snapshots(run_id,identity_hash,source_id,kind,hash,provenance,completeness) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, runID, semanticHash(metadata), ref.ID, ref.Kind, ref.Hash, ref.Provenance, ref.Completeness); err != nil {
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
	if _, err = tx.Exec(ctx, `INSERT INTO semantic_candidate_history(run_id,revision,candidate) VALUES($1,$2,$3)`, runID, candidate.Revision, raw); err != nil {
		return empty, err
	}
	tag, err := tx.Exec(ctx, `UPDATE semantic_planning SET revision=$2,fingerprint=$3,candidate=$4,review_state=NULL WHERE run_id=$1`, runID, candidate.Revision, fp, raw)
	if err != nil {
		return empty, err
	}
	if tag.RowsAffected() != 1 {
		return empty, errors.New("semantic planning state disappeared")
	}
	if err = tx.Commit(ctx); err != nil {
		return empty, err
	}
	return candidate, nil
}
