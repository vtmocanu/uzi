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
	a, p, c, cp := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	// A promoted row names its salvage ref; a pending row is named by run id (and, once its
	// ref was created, that ref too), decided by state, not by an empty ref. More runs than
	// listed: the text says how many more. The branch-scoped checkpoint ref (#1810's) is
	// never named. Reddening mutation: decide pending-ness by an empty SalvageRef again (the
	// created-pending run cp drops out of salvage_pending_runs).
	rec := httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 7, []salvageConflictRow{
		{RunID: a, State: "promoted", SalvageRef: "refs/uzi-salvage/" + a.String()},
		{RunID: p, State: "pending"},
		{RunID: cp, State: "pending", SalvageRef: "refs/uzi-salvage/" + cp.String()},
	})
	b := decode(rec)
	if b.Count != 7 || strings.Join(b.Refs, ",") != "refs/uzi-salvage/"+a.String()+",refs/uzi-salvage/"+cp.String() ||
		strings.Join(b.Pending, ",") != p.String()+","+cp.String() {
		t.Fatalf("body = %+v, want count 7, two salvage refs and two pending runs", b)
	}
	for _, want := range []string{
		"this repo has live salvage copies, made or still being made, of the last published checkpoints of 7 failed run(s) (",
		"(refs/uzi-salvage/" + a.String() + "; ",
		"run " + p.String() + ", copy pending; ",
		"run " + cp.String() + ", copy pending at refs/uzi-salvage/" + cp.String(),
		"; and 4 more run(s))",
		"the block lifts when each copy expires, or when a pending copy ends without being kept, so remove the repo after that",
	} {
		if !strings.Contains(b.Error, want) {
			t.Errorf("message %q lacks %q", b.Error, want)
		}
	}
	for _, bad := range []string{"uzi-checkpoints", "recover"} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Fatalf("the 409 must never contain %q: %s", bad, rec.Body.String())
		}
	}

	// A live row with no created ref and an unexpected state has no ref to name: it is
	// reported as pending by run id rather than silently dropped.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 1, []salvageConflictRow{{RunID: c, State: "promoted"}})
	b = decode(rec)
	if len(b.Refs) != 0 || strings.Join(b.Pending, ",") != c.String() {
		t.Fatalf("ref-less live row: %+v, want it listed as pending", b)
	}

	// A raced or failed lookup still refuses: count floored at 1, both lists empty arrays.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "connection", 0, nil)
	b = decode(rec)
	if b.Count != 1 || len(b.Refs) != 0 || len(b.Pending) != 0 || !strings.Contains(b.Error, "this connection has live salvage copies, made or still being made, of the last published checkpoints of 1 failed run(s);") {
		t.Fatalf("floored conflict: %+v", b)
	}
	if strings.Contains(b.Error, " (") {
		t.Fatalf("a conflict naming nothing must carry no parenthetical: %q", b.Error)
	}

	// Every listed run named: no "more" suffix; a stale count below the listed runs is
	// raised to it.
	rec = httptest.NewRecorder()
	writeSalvageConflict(rec, "repo", 1, []salvageConflictRow{{RunID: a, State: "promoted", SalvageRef: "refs/uzi-salvage/a"}, {RunID: c, State: "promoted", SalvageRef: "refs/uzi-salvage/b"}})
	b = decode(rec)
	if b.Count != 2 || strings.Contains(b.Error, "more") {
		t.Fatalf("fully listed conflict: %+v, want count 2 and no more-suffix", b)
	}
}
