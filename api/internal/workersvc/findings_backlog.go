package workersvc

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Findings backlog buckets (PRD #333 M4, D7): the ?bucket= filter for GET /api/findings.
// BucketToFile is the finding-specific rung (the "still needs filing" default); the other
// three reuse the judge backlog's wire values (BucketFiled/BucketDismissed/BucketAll) rather
// than redeclaring identical string constants in the same package. The service maps each to a
// disposition status; `all` maps to a NULL status (the unfiltered view).
const BucketToFile = "to_file"

// maxFindingOccurrences caps the per-coordinate occurrence list on the wire (PRD #1183 M3): a
// bug seen in hundreds of runs ships at most this many, newest-first, so the DTO stays bounded.
// The cap is applied in Go (not SQL), mirroring the delegation.
const maxFindingOccurrences = 20

// findingBuckets is the closed ?bucket= enum, reached only through ValidFindingBucket.
// Unexported for the same reason judgeBacklogBuckets is: an exported package-level map is
// mutable from anywhere and a validator you cannot see from the handler is not a validator.
// BucketDone joins the set (PRD #1183 M3): a finding reaches `done` only via the issue-close
// sync, and the Findings page's fifth tab reads it.
var findingBuckets = map[string]bool{
	BucketToFile: true, BucketFiled: true, BucketDone: true, BucketDismissed: true, BucketAll: true,
}

// ValidFindingBucket reports whether s is an accepted GET /api/findings ?bucket= value. The
// handler rejects anything else with a 400 rather than silently ignoring the filter, exactly
// as ValidJudgeBacklogBucket does for the judge backlog.
func ValidFindingBucket(s string) bool { return findingBuckets[s] }

// findingBucketStatus maps a validated UX bucket to the finding_dispositions.status the
// disposition-driven read filters on (D7): to_file→'open', filed→'filed',
// dismissed→'dismissed', all→NULL (the unfiltered view). A NULL pgtype.Text is the query's
// "all statuses" sentinel.
func findingBucketStatus(bucket string) pgtype.Text {
	switch bucket {
	case BucketToFile:
		return pgtype.Text{String: "open", Valid: true}
	case BucketFiled:
		return pgtype.Text{String: "filed", Valid: true}
	case BucketDone:
		return pgtype.Text{String: "done", Valid: true}
	case BucketDismissed:
		return pgtype.Text{String: "dismissed", Valid: true}
	default: // BucketAll
		return pgtype.Text{}
	}
}

// FindingsBacklog is the Findings backlog read (PRD #333 M4, D7/D8): the caller's
// owner-scoped, coordinate-deduped backlog, disposition-driven so a filed/dismissed
// coordinate survives even after its evidence rows are cascaded away with a deleted run.
//
// Owner-scoped by the query's user_id filter (like JudgeRecommendationBacklog), so there is no
// ownership oracle to leak and a foreign repoFilter/runFilter simply returns an empty list.
// repoFilter/runFilter are uuid.Nil when the corresponding ?repo=/?run= is absent, mapped to a
// SQL NULL (no-op predicate) by nullableUUID. The D8 open-findings count rides on the response
// meta rather than a standalone route, scoped by the SAME ?repo= filter as the list so the
// nav badge and the filtered view agree.
func (s *Service) FindingsBacklog(ctx context.Context, ownerUserID uuid.UUID, bucket string, repoFilter, runFilter uuid.UUID) (apitypes.IncidentalFindingBacklogDTO, error) {
	rows, err := s.q.ListFindingsBacklog(ctx, store.ListFindingsBacklogParams{
		UserID: ownerUserID,
		Status: findingBucketStatus(bucket),
		RepoID: pgconv.UUIDOrNull(repoFilter),
		RunID:  pgconv.UUIDOrNull(runFilter),
	})
	if err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, err
	}
	openCount, err := s.q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{
		UserID: ownerUserID,
		RepoID: pgconv.UUIDOrNull(repoFilter),
	})
	if err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, err
	}

	out := apitypes.IncidentalFindingBacklogDTO{
		Bucket:    bucket,
		OpenCount: int(openCount),
		Findings:  make([]apitypes.IncidentalFindingDTO, 0, len(rows)),
	}
	if repoFilter != uuid.Nil {
		out.Repo = repoFilter.String()
	}
	if runFilter != uuid.Nil {
		out.Run = runFilter.String()
	}
	dispositionIDs := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out.Findings = append(out.Findings, mapIncidentalFindingRow(r))
		dispositionIDs = append(dispositionIDs, r.DispositionID)
	}

	// One batched evidence read for the whole page (never N+1): the newest evidence row per
	// coordinate is the evidence_preview, the rows in order are the occurrence list (capped at 20,
	// newest-first). A coordinate whose evidence was cascaded away returns no rows and keeps its
	// nil preview/occurrences (last_title still keeps it legible).
	if len(dispositionIDs) > 0 {
		if err := s.attachFindingEvidence(ctx, out.Findings, dispositionIDs); err != nil {
			return apitypes.IncidentalFindingBacklogDTO{}, err
		}
	}
	return out, nil
}

// attachFindingEvidence loads the per-run evidence behind a page of coordinates in ONE query and
// sets each DTO's EvidencePreview (newest description_md, capped) and Occurrences (newest-first,
// capped at maxFindingOccurrences). dispositionIDs is index-aligned with findings, so the
// disposition id at findings[i] is dispositionIDs[i].
func (s *Service) attachFindingEvidence(ctx context.Context, findings []apitypes.IncidentalFindingDTO, dispositionIDs []uuid.UUID) error {
	rows, err := s.q.ListFindingEvidenceForDispositions(ctx, dispositionIDs)
	if err != nil {
		return err
	}
	previews := make(map[uuid.UUID]string, len(dispositionIDs))
	occurrences := make(map[uuid.UUID][]apitypes.FindingOccurrenceDTO, len(dispositionIDs))
	for _, e := range rows {
		// Rows arrive newest-first WITHIN each disposition, so the first row seen sets the preview.
		if _, seen := previews[e.DispositionID]; !seen {
			previews[e.DispositionID] = rationalePreview(e.DescriptionMd)
		}
		if len(occurrences[e.DispositionID]) < maxFindingOccurrences {
			occurrences[e.DispositionID] = append(occurrences[e.DispositionID], apitypes.FindingOccurrenceDTO{
				RunID:      e.RunID.String(),
				RunTitle:   e.RunTitle,
				ReportedAt: e.ReportedAt.Time,
				Confidence: e.Confidence,
			})
		}
	}
	for i := range findings {
		id := dispositionIDs[i]
		if p, ok := previews[id]; ok {
			findings[i].EvidencePreview = p
		}
		if occ, ok := occurrences[id]; ok {
			findings[i].Occurrences = occ
		}
	}
	return nil
}

// FindingsStats is the Findings per-status tally (PRD #1183 M3, GET /api/findings/stats): the
// finding twin of JudgeTriageStats, reusing apitypes.TriageDTO. Owner-scoped, with an OPTIONAL
// repo narrow (repoFilter uuid.Nil = all repos, mapped to a SQL NULL). It IGNORES any run anchor
// by construction — the query carries no run param — so the badge, the tabs and the summary strip
// read one number per repo scope even when the list is anchored to a run. todo = open,
// false_positives = the not_an_issue sub-count of dismissed; total includes a transient filing row.
func (s *Service) FindingsStats(ctx context.Context, ownerUserID, repoFilter uuid.UUID) (apitypes.TriageDTO, error) {
	row, err := s.q.CountFindingsByStatusForUser(ctx, store.CountFindingsByStatusForUserParams{
		UserID: ownerUserID,
		RepoID: pgconv.UUIDOrNull(repoFilter),
	})
	if err != nil {
		return apitypes.TriageDTO{}, err
	}
	return apitypes.TriageDTO{
		Total:          int(row.Total),
		Todo:           int(row.Todo),
		Filed:          int(row.Filed),
		Done:           int(row.Done),
		Dismissed:      int(row.Dismissed),
		FalsePositives: int(row.FalsePositives),
	}, nil
}

// mapIncidentalFindingRow projects one store backlog row onto the wire DTO, mapping the
// nullable pgtype columns to omitempty pointers (D12): finding_id is nil when the coordinate's
// evidence was cascaded away, and filed_issue_iid/resolved_at are nil unless the coordinate is
// filed/resolved.
func mapIncidentalFindingRow(r store.ListFindingsBacklogRow) apitypes.IncidentalFindingDTO {
	dto := apitypes.IncidentalFindingDTO{
		DispositionID: r.DispositionID.String(),
		Location:      r.Location,
		RepoID:        r.RepoID.String(),
		RepoPath:      r.RepoPath,
		Status:        r.Status,
		LastTitle:     r.LastTitle,
		SeenInRuns:    int(r.SeenInRuns),
		FiledIssueURL: r.FiledIssueUrl,
	}
	if r.DismissReason.Valid {
		dto.DismissReason = r.DismissReason.String
	}
	if r.SetVia.Valid {
		dto.SetVia = r.SetVia.String
	}
	if r.LatestFindingID.Valid {
		id := uuid.UUID(r.LatestFindingID.Bytes).String()
		dto.FindingID = &id
	}
	if r.FiledIssueIid.Valid {
		iid := r.FiledIssueIid.Int64
		dto.FiledIssueIID = &iid
	}
	if r.ResolvedAt.Valid {
		ts := r.ResolvedAt.Time
		dto.ResolvedAt = &ts
	}
	return dto
}
