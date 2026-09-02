package workersvc

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// stopReasonParam maps an operator's OPTIONAL cancel reason (PRD #503 M3) onto the nullable
// runs.stop_reason column, sanitizing like sanitizeFailureReason (its failure-class
// sibling): strip NUL — a NUL in a text column raises Postgres 22021 and would abort the
// cancel — then trim and cap the length (the same 2048-rune bound as failure_reason). An
// empty / whitespace-only / NUL-only body stores NULL, never an empty string. Shared by
// both cancel paths (server-side + live) and, since PRD #517 M4, by the graceful `stop`
// stopReasonParam converts an optional stop reason to nullable PostgreSQL text after removing NUL characters, trimming whitespace, and limiting it to 2048 runes. Empty results are represented as NULL.
func stopReasonParam(body string) pgtype.Text {
	clean, _ := stripNUL(body)
	clean = truncateRunes(strings.TrimSpace(clean), maxFailureReasonRunes)
	return pgconv.TextOrNull(clean)
}

// sanitizeFailureReason maps the worker's failure reason onto the nullable
// runs.failure_reason column, stripping NUL and capping the length first.
//
// This is the /messages sanitation (M2) applied to its sibling route (PRD #108 A4).
// A NUL in a `text` column raises 22021 exactly as it does in `jsonb`, and this one
// is worse-placed: it rides `failed` — the run's TERMINAL report — so a 22021 there
// 500s, reportState's bounded retries exhaust, and the terminal state never lands,
// leaving the run to the server-side sweeper. The breaker's own permanent-failure
// report travels this exact field, so a poisoned run reporting its own poison could
// sanitizeFailureReason converts a worker failure reason to nullable PostgreSQL text.
// It removes NUL characters, limits the result to 2048 runes, and represents nil or empty input as SQL NULL.
func sanitizeFailureReason(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	clean = truncateRunes(clean, maxFailureReasonRunes)
	return pgconv.TextOrNull(clean)
}

// clampWirePreservedPatch maps the worker's preserved_patch onto the nullable
// runs.preserved_patch column. nil/empty → NULL. Otherwise the untrusted worker text is
// run through termsafe.SanitizeBounded, which strips NUL and every other control/bidi
// rune (Trojan-Source defense) while SPARING \n and \t so the diff's line structure
// survives, then applies the byte bound. A NUL would raise 22021 on this terminal
// `failed` write exactly as it does for failure_reason (see stripNULParam); the byte cap
// clampWirePreservedPatch sanitizes a preserved patch for database storage, enforcing a byte limit and mapping nil or empty values to SQL NULL.
func clampWirePreservedPatch(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean := termsafe.SanitizeBounded(*s, PreservedPatchMaxBytes)
	return pgconv.TextOrNull(clean)
}

// stripNULParam maps a *string with NUL removed onto a nullable text column (nil or an
// empty/NUL-only value → NULL), for the OTHER worker-controlled text
// fields on the /state path that reach a plain `text` column: session_id, plan_md,
// branch, mr_web_url (PRD #108 A4b). A NUL in any of them raises 22021 exactly as it
// does in jsonb, 500s the transition, and — on awaiting_approval/completed/failed —
// the run's new state never lands. M2 sanitized /messages; failure_reason got A4;
// these are the rest of the class on /state.
//
// Deliberately NO length cap, unlike sanitizeFailureReason and run_usage's
// composite-PK keys: plan_md is model prose that is legitimately long, and none of
// these columns is an index key (00020_workers_runs.sql; no index references
// session_id or branch), so a cap would be lossy data loss for no storability gain.
// A NUL-only value strips to "", which pgconv.TextOrNull maps to NULL — for session_id that is
// stripNULParam removes NUL characters from a worker-provided text value and converts empty results to SQL NULL.
func stripNULParam(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	return pgconv.TextOrNull(clean)
}

// textPtr returns a pointer to the text value when valid, or nil for invalid text.
func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// coalesceInt returns the persisted int4 when present and def otherwise (PRD #122
// M2): a NULL budget column means "use the global default", so a 0/1-milestone run
// serves the same caps as a pre-feature run.
func coalesceInt(v pgtype.Int4, def int) int {
	if !v.Valid {
		return def
	}
	return int(v.Int32)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// uniqueViolationOn reports whether err is a Postgres 23505 raised on the named constraint.
func uniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
