package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PRD #1034 / PR #1051 finding A: ConfigureColumns must reject a reserved "closed"
// column name (case-insensitive) with 400 BEFORE any forge or store call. MoveIssue
// canonicalizes a case-insensitive "closed" drop target to the close sentinel and
// routes it to CloseIssue rather than a column move, so a user column literally named
// "closed" could never receive a card — a drop onto it would close the issue. The
// handler rejects it at configuration time instead of creating a silently unreachable
// column.
//
// This reuses writer_primary_label_livedb_test.go's boardWriterStub + fixture: the stub
// records every EnsureLabels create so the "no forge call on reject" half is asserted on
// the wire, and the fixture seeds NO board_columns so a post-reject count of 0 proves no
// store write happened.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// boardColumnCount reads how many board_columns rows the repo has, so a rejected
// configure can be proven to have written none.
func boardColumnCount(ctx context.Context, t *testing.T, f boardWriterFixture) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_columns WHERE repo_id=$1`, f.repoID,
	).Scan(&n); err != nil {
		t.Fatalf("count board_columns: %v", err)
	}
	return n
}

func TestConfigureColumnsRejectsReservedClosedLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &boardWriterStub{}
	f := newBoardWriterFixture(ctx, t, stub)

	// Both "closed" and "CLOSED" must be refused at 400, before any forge
	// (EnsureLabels) or store (InsertBoardColumn) write — the reject is EqualFold, so it
	// is case-insensitive, matching MoveIssue's case-insensitive canonicalization.
	for _, name := range []string{"closed", "CLOSED"} {
		t.Run("reject "+name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			body := fmt.Sprintf(`{"columns":[{"label_name":%q}]}`, name)
			f.h.ConfigureColumns(rr, boardWriterReq(f.user, f.repoID, "", body))

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("ConfigureColumns(%q) status = %d, want 400; body=%s", name, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "reserved column name") {
				t.Fatalf("ConfigureColumns(%q) body = %s, want a reserved-column-name error", name, rr.Body.String())
			}
			// The reject must precede any store write.
			if n := boardColumnCount(ctx, t, f); n != 0 {
				t.Fatalf("board_columns rows after rejected %q = %d, want 0 — reject must precede the store write", name, n)
			}
			// ...and any forge EnsureLabels call.
			stub.mu.Lock()
			creates := append([]string(nil), stub.labelCreates...)
			stub.mu.Unlock()
			if len(creates) != 0 {
				t.Fatalf("EnsureLabels created %v after rejected %q, want none — reject must precede the forge call", creates, name)
			}
		})
	}

	// Positive control: a normal column name still succeeds — 200, the row is written,
	// and the label is ensured on the forge. This proves the 400 is specific to "closed"
	// and the handler is otherwise live (so the rejects above are not passing vacuously).
	t.Run("normal name succeeds", func(t *testing.T) {
		rr := httptest.NewRecorder()
		f.h.ConfigureColumns(rr, boardWriterReq(f.user, f.repoID, "", `{"columns":[{"label_name":"Backlog"}]}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("ConfigureColumns(Backlog) status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if n := boardColumnCount(ctx, t, f); n != 1 {
			t.Fatalf("board_columns rows = %d, want 1 after a valid configure", n)
		}
		stub.mu.Lock()
		creates := append([]string(nil), stub.labelCreates...)
		stub.mu.Unlock()
		if !contains(creates, "Backlog") {
			t.Fatalf("EnsureLabels created %v, want to contain Backlog", creates)
		}
	})
}
