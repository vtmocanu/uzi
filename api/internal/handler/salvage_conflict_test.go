package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	type body struct {
		Error   string   `json:"error"`
		Refs    []string `json:"salvage_refs"`
		Pending []string `json:"salvage_pending_runs"`
		Count   int64    `json:"salvage_count"`
	}
	decode := func(rec *httptest.ResponseRecorder) body {
		t.Helper()
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		var b body
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if b.Refs == nil || b.Pending == nil {
			t.Fatalf("salvage_refs and salvage_pending_runs must be arrays, got %s", rec.Body.String())
		}
		return b
	}
	a, p, c := uuid.New(), uuid.New(), uuid.New()

	// A created row names its salvage ref; a row whose copy is still being made names its
	// run. More runs than listed: the text says how many more. The branch-scoped checkpoint
	// ref (#1810's) is never named.
	rec := httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 7, []salvageConflictRow{
		{RunID: a, SalvageRef: "refs/uzi-salvage/" + a.String()},
		{RunID: p},
		{RunID: c, SalvageRef: "refs/uzi-salvage/" + c.String()},
	})
	b := decode(rec)
	if b.Count != 7 || strings.Join(b.Refs, ",") != "refs/uzi-salvage/"+a.String()+",refs/uzi-salvage/"+c.String() ||
		strings.Join(b.Pending, ",") != p.String() {
		t.Fatalf("body = %+v, want count 7, two salvage refs and one pending run", b)
	}
	for _, want := range []string{
		"this repo has 7 failed run(s)", "refs/uzi-salvage/" + a.String(), "refs/uzi-salvage/" + c.String(),
		"a salvage copy is being made for run " + p.String(), "and 4 more run(s)",
		"checkpointed commits are kept on the forge for recovery until they expire", "remove the repo after that",
	} {
		if !strings.Contains(b.Error, want) {
			t.Errorf("message %q lacks %q", b.Error, want)
		}
	}
	if strings.Contains(rec.Body.String(), "uzi-checkpoints") {
		t.Fatalf("the 409 must never name a branch checkpoint ref: %s", rec.Body.String())
	}

	// A raced or failed lookup still refuses: count floored at 1, both lists empty arrays.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "connection", 0, nil)
	b = decode(rec)
	if b.Count != 1 || len(b.Refs) != 0 || len(b.Pending) != 0 || !strings.Contains(b.Error, "this connection has 1 failed run(s)") {
		t.Fatalf("floored conflict: %+v", b)
	}
	if strings.Contains(b.Error, " (") {
		t.Fatalf("a conflict naming nothing must carry no parenthetical: %q", b.Error)
	}

	// Every listed run named: no "more" suffix; a stale count below the listed runs is
	// raised to it.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 1, []salvageConflictRow{{RunID: a, SalvageRef: "refs/uzi-salvage/a"}, {RunID: c, SalvageRef: "refs/uzi-salvage/b"}})
	b = decode(rec)
	if b.Count != 2 || strings.Contains(b.Error, "more") {
		t.Fatalf("fully listed conflict: %+v, want count 2 and no more-suffix", b)
	}
}
