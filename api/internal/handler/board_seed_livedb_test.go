package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/board"
)

// TestSeedBoardSeedsDefaultColumnsLiveDB drives the real seedBoard code path
// (board.go: GetBoard → seedBoard when the board has no columns) against a live
// DB and a fake forge, and pins the two silent contracts seedBoard carries:
//
//  1. ORDER — DefaultColumns are persisted as board_columns rows in flow order
//     (Planned, In Progress, Human Review, Later) with matching positions, and
//     the same order crosses the wire to the forge as label-creates.
//  2. COLOUR — the colour on each DefaultColumns entry reaches the forge; the
//     Planned lane inherits Upcoming's #6699cc.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.
func TestSeedBoardSeedsDefaultColumnsLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &boardWriterStub{}
	f := newBoardWriterFixture(ctx, t, stub)

	rr := httptest.NewRecorder()
	f.h.GetBoard(rr, boardWriterReq(f.user, f.repoID, "", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("GetBoard status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	// Contract 1 (order): the persisted board_columns are exactly the default
	// columns in flow order, each at its DefaultColumns index.
	cols, err := f.h.q.ListBoardColumns(ctx, f.repoID)
	if err != nil {
		t.Fatalf("ListBoardColumns: %v", err)
	}
	wantNames := []string{"Planned", board.ColumnInProgress, board.ColumnHumanReview, "Later"}
	if len(cols) != len(wantNames) {
		t.Fatalf("ORDER contract: seeded %d board columns, want %d %v; got %v", len(cols), len(wantNames), wantNames, cols)
	}
	for i, want := range wantNames {
		if cols[i].LabelName != want {
			t.Errorf("ORDER contract: board column[%d] = %q, want %q (full: %v)", i, cols[i].LabelName, want, cols)
		}
		if cols[i].Position != int32(i) {
			t.Errorf("ORDER contract: board column %q Position = %d, want %d", cols[i].LabelName, cols[i].Position, i)
		}
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()

	// Contract 1 (order, on the wire): Planned is created before In Progress, and
	// both actually crossed the wire.
	plannedIdx, inProgressIdx := -1, -1
	for i, n := range stub.labelCreates {
		switch n {
		case "Planned":
			plannedIdx = i
		case board.ColumnInProgress:
			inProgressIdx = i
		}
	}
	if plannedIdx < 0 || inProgressIdx < 0 {
		t.Fatalf("ORDER contract: labelCreates %v missing Planned and/or %q", stub.labelCreates, board.ColumnInProgress)
	}
	if plannedIdx >= inProgressIdx {
		t.Errorf("ORDER contract: Planned created at index %d, %q at %d; Planned must lead (full: %v)", plannedIdx, board.ColumnInProgress, inProgressIdx, stub.labelCreates)
	}

	// Contract 2 (colour): Planned inherits Upcoming's colour across the wire.
	if got := stub.labelColors["Planned"]; got != "#6699cc" {
		t.Errorf("COLOUR contract: Planned label colour = %q, want %q (the colour Upcoming shipped with)", got, "#6699cc")
	}
}
