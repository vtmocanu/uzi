package workersvc

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// stopReasonParam maps an operator's OPTIONAL cancel reason (PRD #503 M3) onto the nullable
// runs.stop_reason column, sanitizing like sanitizeFailureReason (its failure-class
// sibling): strip NUL — a NUL in a text column raises Postgres 22021 and would abort the
// cancel — then trim and cap the length (the same 2048-rune bound as failure_reason). An
// empty / whitespace-only / NUL-only body stores NULL, never an empty string. Shared by
// both cancel paths (server-side + live) and, since PRD #517 M4, by the graceful `stop`
// path, which carries the operator's OPTIONAL stop reason the same way a cancel does.
func stopReasonParam(body string) pgtype.Text {
	clean, _ := stripNUL(body)
	clean = truncateRunes(strings.TrimSpace(clean), maxFailureReasonRunes)
	return pgText(clean)
}

func textParam(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgText(*s)
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
// fail to record that it failed.
func sanitizeFailureReason(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	clean = truncateRunes(clean, maxFailureReasonRunes)
	return pgText(clean)
}

// clampWirePreservedPatch maps the worker's preserved_patch onto the nullable
// runs.preserved_patch column. nil/empty → NULL. Otherwise the untrusted worker text is
// run through termsafe.SanitizeBounded, which strips NUL and every other control/bidi
// rune (Trojan-Source defense) while SPARING \n and \t so the diff's line structure
// survives, then applies the byte bound. A NUL would raise 22021 on this terminal
// `failed` write exactly as it does for failure_reason (see stripNULParam); the byte cap
// bounds a hostile or buggy worker. A value that sanitizes to empty maps to NULL.
func clampWirePreservedPatch(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean := termsafe.SanitizeBounded(*s, PreservedPatchMaxBytes)
	return pgText(clean)
}

// stripNULParam is textParam with NUL removed, for the OTHER worker-controlled text
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
// A NUL-only value strips to "", which pgText maps to NULL — for session_id that is
// its documented "no change" sentinel, which is the right outcome for garbage input.
func stripNULParam(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	return pgText(clean)
}

func int8Param(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// pgIntPtr maps an optional int (a nil pointer = "not reported") onto a nullable
// int4 column: nil → NULL, else the value. Used for the worker's advertised
// max_concurrent_runs (PRD #42).
func pgIntPtr(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true} //nolint:gosec // G115: max_concurrent_runs is a small operator-set worker capacity, never near int32 range
}

// pgFloat4Ptr maps an optional float (nil = "not reported") onto a nullable real
// column: nil → NULL, else the value. Used for the worker's stats_cpu_pct (PRD #49),
// which the worker omits on its first tick.
func pgFloat4Ptr(v *float64) pgtype.Float4 {
	if v == nil {
		return pgtype.Float4{}
	}
	return pgtype.Float4{Float32: float32(*v), Valid: true}
}

// pgUUID wraps a uuid you KNOW IS PRESENT as a valid pgtype.UUID. It sets Valid=true
// unconditionally — including for uuid.Nil, which it turns into the REAL all-zero uuid,
// not SQL NULL.
//
// That is correct for its contract and must not be "fixed" to auto-NULL uuid.Nil: at the
// ~45 call sites that legitimately assume presence, auto-NULLing would convert a loud FK
// violation into a silent NULL write.
//
// The trap is passing uuid.Nil as an "absent" sentinel to a sqlc.narg parameter. The
// query's `IS NULL` escape hatch then never fires and the filter matches nothing, so the
// endpoint SILENTLY RETURNS NOTHING rather than erroring (PRD #98 M1 hit exactly this; it
// took a live-DB test to surface, since a fake store cannot show it). For a genuinely
// optional id use the house idiom instead: *uuid.UUID with an explicit nil guard
// (ListRunsForUser, above), or leave the zero pgtype.UUID (Valid:false → NULL) and call
// this only on the present branch. workersvc.nullableUUID does the latter for the judge
// backlog's ?run= anchor.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

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
