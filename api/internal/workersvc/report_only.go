package workersvc

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
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
	if run.Kind != "issue" {
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
	// Order matters: sanitize (strip + cap) THEN scrub — mirroring judge_worker.go:
	// ScrubSecrets(termsafe.SanitizeBounded(...)), so the scrubber sees whole runes and the
	// cap is applied to structural text before redaction rewrites it.
	s := secretscrub.Scrub(termsafe.SanitizeBounded(*p, ReviewSummaryMaxBytes))
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
