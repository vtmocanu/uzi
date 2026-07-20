package workersvc

import "gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"

// TriageRow is one recommendation's two triage facts — its coordinate's disposition
// (Status/Reason, both "" when undisposed) and whether it has a SETTLED filed link
// (PRD #94 Decisions 2/8). Both the per-review DTO and the global stats handler build a
// slice of these and count them through BucketTriage, so the two counters bucket by the
// identical ladder and cannot drift.
type TriageRow struct {
	Status       string
	Reason       string
	FiledSettled bool
}

// BucketOf is the ONE precedence ladder (PRD #94 Decision 2), consumed by both the
// per-review DTO and the global stats aggregate: dismissed > done > filed(settled) > todo.
// "filed" means a SETTLED link only — an in-flight/unsettled claim is not filed, matching
// the per-review path. There is deliberately no SQL CASE mirroring this; a Go helper cannot
// back a SQL CASE, so bucketing lives here alone.
func BucketOf(dispositionStatus string, filedSettled bool) string {
	switch dispositionStatus {
	case "dismissed":
		return "dismissed"
	case "done":
		return "done"
	}
	if filedSettled {
		return "filed"
	}
	return "todo"
}

// BucketTriage tallies a per-recommendation slice into the TriageDTO (PRD #94), bucketing
// each row via BucketOf so the count matches the on-screen ladder. The denominator is
// recommendation rows (Total++ per row). FalsePositives is the not_an_issue sub-count of
// Dismissed — the "false positives" number surfaced in both counters (Decision 4).
func BucketTriage(rows []TriageRow) apitypes.TriageDTO {
	var out apitypes.TriageDTO
	for _, r := range rows {
		out.Total++
		switch BucketOf(r.Status, r.FiledSettled) {
		case "dismissed":
			out.Dismissed++
			if r.Reason == "not_an_issue" {
				out.FalsePositives++
			}
		case "done":
			out.Done++
		case "filed":
			out.Filed++
		default:
			out.Todo++
		}
	}
	return out
}
