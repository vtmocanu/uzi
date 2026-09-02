// Package pgconv is the shared, honestly-named set of pgtype param constructors
// used to build values for sqlc-generated query params. It replaces ~50 helpers
// that were duplicated across the api module's packages and, worse, DISAGREED
// about what the empty string means — the same name (pgText) mapped "" to SQL
// NULL in one package and to a valid empty string in another.
//
// The cure is honest names, never an ambiguous pgText. The whole reason the
// pgText name was retired is that the Text/TextOrNull and TextPtr/TextPtrOrNull
// pairs differ ONLY at the empty case:
//
//   - Text("")           → valid, String=""   (NOT NULL)
//   - TextOrNull("")     → NULL
//   - TextPtr(&"")       → valid, String=""    (nil → NULL)
//   - TextPtrOrNull(&"") → NULL                 (nil → NULL)
//
// Pick the constructor whose name states the "" semantics the call site needs;
// migrating by measured behavior rather than by the old helper's name is the
// point of the consolidation.
//
// pgconv is a LEAF package: it imports only stdlib time, google/uuid, and
// pgtype, and imports nothing from internal/, so any package (including store)
// may depend on it without a cycle.
package pgconv

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// Text builds an ALWAYS-valid pgtype.Text, including for "" (which stores a
// Text creates a valid PostgreSQL text value, including when s is empty.
func Text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

// TextOrNull maps "" to SQL NULL (Valid:false); any other string is valid.
func TextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// TextPtr maps a nil pointer to SQL NULL; a non-nil pointer is always valid,
// TextPtr converts a string pointer to a PostgreSQL text value, using NULL for nil
// pointers and a valid value for non-nil pointers, including pointers to empty strings.
func TextPtr(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}

// TextPtrOrNull maps both a nil pointer AND a pointer to "" to SQL NULL; any
// TextPtrOrNull converts nil pointers and pointers to empty strings to NULL; other
// pointed-to strings are returned as valid text.
func TextPtrOrNull(p *string) pgtype.Text {
	if p == nil || *p == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}

// UUID builds an ALWAYS-valid pgtype.UUID, including for uuid.Nil (which writes
// UUID constructs a valid PostgreSQL UUID value, including for the all-zero UUID.
func UUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

// UUIDOrNull maps uuid.Nil to SQL NULL; any other UUID is valid.
func UUIDOrNull(u uuid.UUID) pgtype.UUID {
	if u == uuid.Nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: u, Valid: true}
}

// UUIDPtr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// UUIDPtr converts a UUID pointer to a PostgreSQL UUID value, preserving uuid.Nil as a valid value.
// A nil pointer produces a NULL value.
func UUIDPtr(p *uuid.UUID) pgtype.UUID {
	if p == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *p, Valid: true}
}

// Time builds an ALWAYS-valid pgtype.Timestamptz, including the zero time
// Time creates a valid PostgreSQL timestamptz value, including for the zero time.
func Time(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// TimePtr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// TimePtr converts a time pointer to a PostgreSQL timestamptz value.
// A nil pointer produces a NULL value; otherwise, the pointed-to time is valid.
func TimePtr(p *time.Time) pgtype.Timestamptz {
	if p == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *p, Valid: true}
}

// BoolPtr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// BoolPtr converts a boolean pointer to a PostgreSQL boolean value, using NULL for nil pointers.
func BoolPtr(p *bool) pgtype.Bool {
	if p == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *p, Valid: true}
}

// Int4Ptr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// Int4Ptr converts an optional int value to a PostgreSQL int4 value.
// A nil pointer produces a NULL value; otherwise, the value is narrowed to int32.
// The returned value is valid when p is non-nil.
func Int4Ptr(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	//nolint:gosec // G115: caller-scoped small ints (worker capacity), never near int32 range
	return pgtype.Int4{Int32: int32(*p), Valid: true}
}

// Int4Ptr32 maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// Int4Ptr32 converts a pointer to an int32 value into a PostgreSQL int4 value.
// A nil pointer produces a NULL value.
func Int4Ptr32(p *int32) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *p, Valid: true}
}

// Int8Ptr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// Int8Ptr converts an int64 pointer to a PostgreSQL int8 value.
// A nil pointer produces a NULL value; otherwise, the pointee is returned as a valid value.
func Int8Ptr(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

// Float4Ptr maps a nil pointer to SQL NULL; a non-nil pointer is valid with its
// A nil pointer produces a NULL value.
func Float4Ptr(p *float64) pgtype.Float4 {
	if p == nil {
		return pgtype.Float4{}
	}
	return pgtype.Float4{Float32: float32(*p), Valid: true}
}
