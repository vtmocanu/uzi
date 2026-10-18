package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ObserveExactHold reads one snapshot, independently of the held-publication write switch.
// Missing or unverified scope is unknown; no current-run ownership is required.
func (s *Service) ObserveExactHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) (*apitypes.RecoveryExactHold, error) {
	if generation <= 0 || !slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryHeldPublicationV2) {
		return nil, nil
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, exactHoldObservationSQL, runID, generation, wkr.ID, wkr.UserID, wkr.IsolatedLane).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out apitypes.RecoveryExactHold
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil
	}
	if out.RunID != runID.String() || out.Generation != generation || out.OriginalWorkerID != wkr.ID.String() ||
		!exactUUID(out.HoldID) || !exactUUID(out.RepoID) || (out.RunWorkerID != "" && !exactUUID(out.RunWorkerID)) || !out.InventoryGuarded ||
		strings.TrimSpace(out.OriginalWorkerIdentity) == "" || out.CurrentClaimGeneration < generation ||
		!slices.Contains([]string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "pool_wait", "paused", "recovery_wait", "completed", "failed", "cancelled"}, out.RunStatus) {
		return nil, nil
	}
	if out.State == "open" {
		if out.LiveWorkerID != out.OriginalWorkerID || out.LiveRunID != out.RunID || out.ReleaseEvidence != "" || out.FinalReceipt != nil || out.CompletedPublicationReceipt != nil {
			return nil, nil
		}
	} else if out.LiveWorkerID != "" || out.LiveRunID != "" || !validExactClosedObservation(out, wkr.UserID) {
		return nil, nil
	}
	return &out, nil
}

func exactUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validExactClosedObservation(out apitypes.RecoveryExactHold, owner uuid.UUID) bool {
	f := out.FinalReceipt
	if out.State == "discarded" {
		// The policy binding is verified by the query and the immutable database guard.
		// Prior FINAL evidence stays an observation, never a replacement release ACK.
		if out.ReleaseEvidence == "merged_predecessor_discard" {
			return true
		}
		return out.ReleaseEvidence == "owner_discard" && f == nil && out.CompletedPublicationReceipt == nil
	}
	if out.State != "released" || f == nil {
		return false
	}
	if f.Kind != "completed_publication" && out.CompletedPublicationReceipt != nil {
		return false
	}
	switch f.Kind {
	case "held_publication", "archive":
		sha, err := validateSha(f.SourceSha, false)
		if err != nil || sha != f.SourceSha || !validCoverageDigest(f.CoverageDigest) || out.ReleaseEvidence != f.Kind {
			return false
		}
		if f.Kind == "held_publication" {
			return (len(sha) == 40 || len(sha) == 64) && exactUUID(f.PublicationID) && f.CaptureID == ""
		}
		return exactUUID(f.CaptureID) && f.PublicationID == ""
	case "settled":
		return slices.Contains([]string{"publication", "forge_no_output"}, out.ReleaseEvidence) && f.CaptureID == "" && f.PublicationID == "" && f.SourceSha == "" && f.CoverageDigest == emptyInventoryDigest
	case "no_adopted_source":
		return out.ReleaseEvidence == f.Kind && f.CaptureID == "" && f.PublicationID == "" && f.SourceSha == "" && f.CoverageDigest == ""
	case "completed_publication":
		r := out.CompletedPublicationReceipt
		if r == nil || out.ReleaseEvidence != f.Kind || f.CaptureID != "" || f.PublicationID != "" || f.SourceSha != "" || f.CoverageDigest != "" ||
			r.HoldID != out.HoldID || r.RunID != out.RunID || r.RepoID != out.RepoID || r.OwnerID != owner.String() || r.WorkerID != out.OriginalWorkerID || r.Generation != out.Generation ||
			!exactUUID(r.ConnectionID) || r.ProjectID <= 0 || r.MRIID == nil || *r.MRIID <= 0 || r.Branch == "" ||
			!slices.Contains([]string{"github", "gitlab", "forgejo"}, r.ForgeType) {
			return false
		}
		for _, sha := range []string{r.FinalHead, r.ObservedBranchHead} {
			if _, err := validateSha(sha, false); err != nil || (len(sha) != 40 && len(sha) != 64) {
				return false
			}
		}
		u, err := url.Parse(r.BaseURL)
		return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
	default:
		return false
	}
}

const exactHoldObservationSQL = `
SELECT jsonb_strip_nulls(jsonb_build_object(
 'run_id', h.run_id, 'hold_id', h.id, 'generation', h.generation, 'repo_id', h.repo_id,
 'original_worker_id', h.original_worker_id, 'original_worker_identity', h.original_worker_identity,
 'live_worker_id', h.live_worker_id, 'live_run_id', h.live_run_id,
 'inventory_guarded', h.inventory_guarded, 'state', h.state, 'run_status', r.status,
 'current_claim_generation', r.claim_generation, 'claim_released', r.claim_released_at IS NOT NULL,
 'run_worker_id', r.worker_id, 'recovery_wait_cause', r.recovery_wait_cause,
 'release_evidence', h.release_evidence,
 'final_receipt', CASE WHEN h.final_disposition IS NOT NULL THEN jsonb_build_object(
   'kind', h.final_disposition, 'capture_id', h.final_capture_id,
   'publication_id', h.final_publication_id, 'source_sha', h.final_source_sha,
   'coverage_digest', h.final_coverage_digest) END,
 'completed_publication_receipt', h.completed_publication_receipt))
FROM recovery_custody_holds h
JOIN runs r ON r.id=h.run_id AND r.user_id=h.user_id AND r.repo_id=h.repo_id
JOIN workers w ON w.id=h.original_worker_id AND w.user_id=h.user_id
WHERE h.run_id=$1 AND h.generation=$2 AND h.original_worker_id=$3 AND h.user_id=$4
 AND h.inventory_guarded AND h.generation > 0 AND r.claim_generation >= h.generation
 AND (r.egress_profile_id IS NOT NULL)=$5 AND w.isolated_lane=$5
 AND NOT (r.kind='cross_check' AND r.claim_generation=0)
 AND h.original_worker_identity <> ''
 AND ((h.state='open' AND h.live_worker_id=h.original_worker_id AND h.live_run_id=h.run_id
       AND h.release_evidence IS NULL AND h.final_disposition IS NULL)
   OR (h.state IN ('released','discarded') AND h.live_worker_id IS NULL AND h.live_run_id IS NULL
       AND (
        (h.state='discarded' AND h.release_evidence='merged_predecessor_discard'
          AND h.merged_predecessor_discard_binding IS NOT NULL)
        OR (h.released_at IS NOT NULL AND (
        (h.state='discarded' AND h.release_evidence='owner_discard' AND h.final_disposition IS NULL)
        OR (h.state='released' AND (
         (h.final_disposition='settled' AND h.release_evidence IN ('publication','forge_no_output')
          AND h.final_capture_id IS NULL AND h.final_publication_id IS NULL AND h.final_source_sha IS NULL
          AND h.final_coverage_digest='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
         OR (h.final_disposition='no_adopted_source' AND h.release_evidence='no_adopted_source'
          AND h.final_capture_id IS NULL AND h.final_publication_id IS NULL
          AND h.final_source_sha IS NULL AND h.final_coverage_digest IS NULL)
         OR (h.final_disposition='archive' AND h.release_evidence='archive'
          AND h.final_publication_id IS NULL AND EXISTS (
           SELECT 1 FROM recovery_captures c WHERE c.id=h.final_capture_id AND c.hold_id=h.id
            AND c.run_id=h.run_id AND c.user_id=h.user_id AND c.original_worker_id=h.original_worker_id
            AND c.source_sha=h.final_source_sha AND c.coverage_digest=h.final_coverage_digest))
         OR (h.final_disposition='held_publication' AND h.release_evidence='held_publication'
          AND h.final_capture_id IS NULL AND EXISTS (
           SELECT 1 FROM run_held_publications p WHERE p.id=h.final_publication_id AND p.hold_id=h.id
            AND p.run_id=h.run_id AND p.user_id=h.user_id AND p.repo_id=h.repo_id
            AND p.worker_id=h.original_worker_id AND p.generation=h.generation
            AND p.tip=h.final_source_sha AND p.coverage_digest=h.final_coverage_digest))
         OR (h.final_disposition='completed_publication' AND h.release_evidence='completed_publication'
          AND h.completed_publication_receipt->>'hold_id'=h.id::text
          AND h.completed_publication_receipt->>'run_id'=h.run_id::text
          AND h.completed_publication_receipt->>'repo_id'=h.repo_id::text
          AND h.completed_publication_receipt->>'owner_id'=h.user_id::text
          AND h.completed_publication_receipt->>'worker_id'=h.original_worker_id::text
          AND h.completed_publication_receipt->>'generation'=h.generation::text)
        ))
       )))))
`
