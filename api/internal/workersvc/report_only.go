package workersvc

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// clampWireReportOnly gates the worker's report_only declaration to issue runs
// (defense-in-depth: the worker already schema-gates the tool param to issue runs).
// runs.kind is NOT NULL. A nil/false declaration or a non-issue run yields false.
//
// Like clampWirePRDDonePath this DROPS AND WARNS; it never returns an error. It runs
// on the terminal `completed` report, the one call a worker must not be able to fail
// on a technicality (see clampWirePRDDonePath's doc comment for the full rationale).
func clampWireReportOnly(run store.Run, p *bool) bool {
	if p == nil || !*p {
		return false
	}
	if run.Kind != runkind.Issue {
		slog.Warn("dropping report_only: not an issue run", "run_id", run.ID, "kind", run.Kind)
		return false
	}
	return true
}

// clampWireReportMd narrows the worker's report_md (the lead's findings summary) to
// what may be stored. Control-char/format stripped, length-bounded, then secret-
// scrubbed — the agent may have seen repo secrets during the run and this text is
// untrusted. DROPS-AND-WARNS, never errors (terminal report).
//
// report_md is the deliverable of a report-only completion, so it is stored ONLY when
// `reportOnly` is true (the already-kind-gated value from clampWireReportOnly). That
// keeps the column's invariant — non-NULL only on a report_only run — true even against
// an untrusted worker that sends report_md without report_only; `reportOnly==true` also
// implies an issue run, so no separate kind check is needed here.
func clampWireReportMd(run store.Run, p *string, reportOnly bool) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	if !reportOnly {
		slog.Warn("dropping report_md: report_only not set", "run_id", run.ID, "kind", run.Kind)
		return pgtype.Text{}
	}
	// Order matters: sanitize the WHOLE field, secret-scrub it, and ONLY THEN apply the
	// byte cap (see scrubThenBound). secretscrub.Scrub matches a credential only when it
	// sees the whole token, so capping first can leave a sub-match-length prefix that Scrub
	// misses — a near-cap credential would leak into the filed report_md. This is the
	// report-only sibling of clampWireProposal's F4 fix (#1122).
	s := scrubThenBound(*p, ReviewSummaryMaxBytes)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
