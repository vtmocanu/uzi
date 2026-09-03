package handler

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgptr.go holds the pgtype-to-pointer read-side helpers (the inverse of pgconv's
// write-side constructors), shared by the DTO mappers across the handler package.

// textPtrValue returns a pointer to s when valid, else nil — the JSON-null vs
// value convention the DTOs use for nullable text columns.
func textPtrValue(valid bool, s string) *string {
	if !valid {
		return nil
	}
	return &s
}

// uuidPtrValue renders a nullable uuid column as its string form, or nil. Used for
// the worker's Anthropic binding (PRD #104 M3), where NULL is the meaningful
// "unbound, spends the owner's default" state and must serialize as JSON null.
func uuidPtrValue(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// timePtr returns a pointer to t when valid, else nil.
func timePtr(valid bool, t time.Time) *time.Time {
	if !valid {
		return nil
	}
	return &t
}

// intPtrValue returns a pointer to the int value of a nullable int4 column when
// valid, else nil — the JSON-null vs value convention for the worker's advertised
// max_concurrent_runs (PRD #42).
func intPtrValue(i pgtype.Int4) *int {
	if !i.Valid {
		return nil
	}
	v := int(i.Int32)
	return &v
}

// float4PtrValue / int8PtrValue apply the same JSON-null vs value convention to the
// worker's nullable stats columns (PRD #49): a NULL column becomes a JSON null so the
// UI shows nothing rather than a fabricated 0.
func float4PtrValue(f pgtype.Float4) *float64 {
	if !f.Valid {
		return nil
	}
	v := float64(f.Float32)
	return &v
}

func int8PtrValue(i pgtype.Int8) *int64 {
	if !i.Valid {
		return nil
	}
	v := i.Int64
	return &v
}

// boolPtrValue applies the JSON-null vs value convention to a nullable bool column
// (worker.docker_enabled, PRD #83 M3): NULL → JSON null (docker not applicable to an
// external worker), else the stored true/false.
func boolPtrValue(b pgtype.Bool) *bool {
	if !b.Valid {
		return nil
	}
	v := b.Bool
	return &v
}

// int32PtrValue applies the JSON-null vs value convention to a nullable int4 whose zero
// is meaningful — an exit code of 0 and "never terminated" are different facts.
func int32PtrValue(valid bool, v int32) *int32 {
	if !valid {
		return nil
	}
	return &v
}
