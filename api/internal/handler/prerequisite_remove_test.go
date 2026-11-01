package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPrerequisiteRestrict(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New(prerequisiteRestrictConstraint), false},
		{"exact", &pgconn.PgError{Code: "23503", ConstraintName: prerequisiteRestrictConstraint}, true},
		{"wrapped", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503", ConstraintName: prerequisiteRestrictConstraint}), true},
		{"other-fk", &pgconn.PgError{Code: "23503", ConstraintName: heldRestrictConstraint}, false},
		{"other-state", &pgconn.PgError{Code: "23514", ConstraintName: prerequisiteRestrictConstraint}, false},
		{"missing-name", &pgconn.PgError{Code: "23503"}, false},
		{"wrapped-other-fk", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503", ConstraintName: salvageRestrictConstraint}), false},
		{"wrapped-other-state", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23514", ConstraintName: prerequisiteRestrictConstraint}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPrerequisiteRestrict(tc.err); got != tc.want {
				t.Fatalf("classifier = %v, want %v", got, tc.want)
			}
		})
	}
}

func assertPrerequisiteRemovalConflict(t *testing.T, rec *httptest.ResponseRecorder, count int64) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 {
		t.Fatalf("409 must contain only error and prerequisite_closure_count: %s", rec.Body.String())
	}
	var got int64
	if err := json.Unmarshal(body["prerequisite_closure_count"], &got); err != nil || got != count {
		t.Fatalf("count = %d, want %d, err = %v", got, count, err)
	}
	var message string
	if err := json.Unmarshal(body["error"], &message); err != nil {
		t.Fatal(err)
	}
	const want = "prerequisite pins protect recovery archives; wait for their lifecycle to settle before removing this resource"
	if message != want {
		t.Fatalf("message = %q, want static explanation %q", message, want)
	}
}

func TestPrerequisiteRemovalResponse(t *testing.T) {
	for _, tc := range []struct {
		count int64
		want  int64
	}{
		{math.MinInt64, 1}, {-1, 1}, {0, 1}, {1, 1}, {7, 7}, {math.MaxInt64, math.MaxInt64},
	} {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePrerequisiteConflict(rec, tc.count)
			assertPrerequisiteRemovalConflict(t, rec, tc.want)
		})
	}
}
