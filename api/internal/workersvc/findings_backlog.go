package workersvc

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Findings backlog buckets (PRD #333 M4, D7): the ?bucket= filter for GET /api/findings.
// BucketToFile is the finding-specific rung (the "still needs filing" default); the other
// three reuse the judge backlog's wire values (BucketFiled/BucketDismissed/BucketAll) rather
// than redeclaring identical string constants in the same package. The service maps each to a
// disposition status; `all` maps to a NULL status (the unfiltered view).
const BucketToFile = "to_file"

// findingBuckets is the closed ?bucket= enum, reached only through ValidFindingBucket.
// Unexported for the same reason judgeBacklogBuckets is: an exported package-level map is
// mutable from anywhere and a validator you cannot see from the handler is not a validator.
var findingBuckets = map[string]bool{
	BucketToFile: true, BucketFiled: true, BucketDismissed: true, BucketAll: true,
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
		RepoID: nullableUUID(repoFilter), // uuid.Nil → SQL NULL → the repo predicate is a no-op
		RunID:  nullableUUID(runFilter),  // uuid.Nil → SQL NULL → the run semi-join is a no-op
	})
	if err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, err
	}
	openCount, err := s.q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{
		UserID: ownerUserID,
		RepoID: nullableUUID(repoFilter),
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
	for _, r := range rows {
		out.Findings = append(out.Findings, mapIncidentalFindingRow(r))
	}
	return out, nil
}

// mapIncidentalFindingRow projects one store backlog row onto the wire DTO, mapping the
// nullable pgtype columns to omitempty pointers (D12): finding_id is nil when the coordinate's
// evidence was cascaded away, and filed_issue_iid/resolved_at are nil unless the coordinate is
// filed/resolved.
func mapIncidentalFindingRow(r store.ListFindingsBacklogRow) apitypes.IncidentalFindingDTO {
	dto := apitypes.IncidentalFindingDTO{
		Location:      r.Location,
		RepoID:        r.RepoID.String(),
		RepoPath:      r.RepoPath,
		Status:        r.Status,
		LastTitle:     r.LastTitle,
		SeenInRuns:    int(r.SeenInRuns),
		FiledIssueURL: r.FiledIssueUrl,
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
