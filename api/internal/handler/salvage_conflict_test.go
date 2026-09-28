package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// PRD #1867 M2: only a 23503 on run_salvage's live pointer maps to the salvage 409; any
// other FK violation (e.g. a custody hold's) or error keeps the handlers' 500.
func TestIsSalvageRestrict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"salvage fk", &pgconn.PgError{Code: "23503", ConstraintName: "run_salvage_live_run_id_fkey"}, true},
		{"wrapped salvage fk", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503", ConstraintName: "run_salvage_live_run_id_fkey"}), true},
		{"custody fk", &pgconn.PgError{Code: "23503", ConstraintName: "recovery_custody_holds_live_run_id_fkey"}, false},
		{"other code same constraint", &pgconn.PgError{Code: "23514", ConstraintName: "run_salvage_live_run_id_fkey"}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isSalvageRestrict(tc.err); got != tc.want {
			t.Errorf("%s: isSalvageRestrict = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWriteSalvageConflict(t *testing.T) {
	decode := func(rec *httptest.ResponseRecorder) (string, []string, int64) {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		var body struct {
			Error string   `json:"error"`
			Refs  []string `json:"salvage_refs"`
			Count int64    `json:"salvage_count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Error, body.Refs, body.Count
	}

	// More refs than named: the text says how many more.
	rec := httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 7, []string{"refs/uzi-salvage/a", "refs/uzi-checkpoints/agent/issue-1"})
	msg, refs, count := decode(rec)
	if count != 7 || len(refs) != 2 {
		t.Fatalf("count=%d refs=%v, want 7 and 2 refs", count, refs)
	}
	for _, want := range []string{"this repo has 7 failed run(s)", "refs/uzi-salvage/a", "refs/uzi-checkpoints/agent/issue-1", "and 5 more", "checkpointed commits", "expire"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}

	// A raced or failed lookup still refuses: count floored at 1, refs an empty array.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "connection", 0, nil)
	msg, refs, count = decode(rec)
	if count != 1 || refs == nil || len(refs) != 0 || !strings.Contains(msg, "this connection has 1 failed run(s)") {
		t.Fatalf("floored conflict: msg=%q refs=%v count=%d", msg, refs, count)
	}
	if strings.Contains(rec.Body.String(), `"salvage_refs":null`) {
		t.Fatalf("salvage_refs must be an array, got %s", rec.Body.String())
	}
}
