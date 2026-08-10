package workersvc

import (
	"log/slog"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretscrub"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
// untrusted. DROPS-AND-WARNS, never errors (terminal report). Kind-gated to issue runs.
func clampWireReportMd(run store.Run, p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	if run.Kind != "issue" {
		slog.Warn("dropping report_md: not an issue run", "run_id", run.ID, "kind", run.Kind)
		return pgtype.Text{}
	}
	// Order matters: sanitize (strip + cap) THEN scrub — mirroring judge_worker.go's
	// ScrubSecrets(sanitizeReviewText(...)), so the scrubber sees whole runes and the
	// cap is applied to structural text before redaction rewrites it.
	s := secretscrub.Scrub(sanitizeReportText(*p, ReviewSummaryMaxBytes))
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// sanitizeReportText mirrors handler.sanitizeReviewText (judge_worker.go:495) — it
// cannot be reused across packages (handler imports workersvc, not the reverse), so it
// is duplicated here with the same semantics: trim, drop control + Unicode Cf format
// chars (except \n and \t so multi-line markdown survives), byte-bound after each whole
// rune (never splitting a multi-byte rune), then trim again. Keep the two bodies
// identical — if you change one, change both.
func sanitizeReportText(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
