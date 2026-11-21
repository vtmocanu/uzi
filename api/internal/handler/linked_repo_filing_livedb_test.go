package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// linkedDrivers hands out the BaseFake-backed driver; a test that uses it never drives an
// issue write through it.
type linkedDrivers struct{ built *atomic.Int32 }

func (d linkedDrivers) DriverFor(_ context.Context, _, _ uuid.UUID, _ issuetracker.Binding, _ issuetracker.StateMapping) (issuetracker.Driver, error) {
	d.built.Add(1)
	return &itDriver{}, nil
}

// Reconfiguring the columns on a linked repo writes only board_columns rows. Converted from
// TestLinkedRepoFilingRoutesAreRefusedAndReleaseTheirClaimLiveDB: the four filing routes it
// refused with a 409 are accepted since PRD #4173 M5 (see
// linked_repo_create_livedb_test.go), and this was its remaining subtest.
func TestLinkedRepoConfigureColumnsTouchesNoIssueTrackerLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	var built atomic.Int32
	f.h.svc.SetIssueTrackerDrivers(linkedDrivers{built: &built})
	itConn := uuid.New()
	mustExecT(ctx, t, f.pool, `INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
	      VALUES ($1, $2, 'youtrack', $3, '1-1', 'bot', $4)`, itConn, f.admin.ID, "https://"+uuid.NewString()+".yt.test", []byte{1})
	// Link the fixture's repo (the marker only ever goes true to false).
	mustExecT(ctx, t, f.pool, `UPDATE repos SET enabled = true, awaiting_first_enable = false,
	        issue_tracker_connection_id = $2, issue_tracker_project_id = '0-1', issue_tracker_project_short = 'UZI',
	        eligibility_tag_id = '5-1', eligibility_tag_name = 'uzi', issue_tracker_linked_at = now() WHERE id = $1`, f.repoID, itConn)

	before := built.Load()
	rr := httptest.NewRecorder()
	f.h.ConfigureColumns(rr, f.request(http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": "a"}}},
		map[string]string{"id": f.repoID.String()}))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	if n := f.hits.Load(); n != 0 {
		t.Errorf("the forge saw %d request(s), want 0", n)
	}
	if n := built.Load() - before; n != 0 {
		t.Errorf("ConfigureColumns resolved %d issue tracker driver(s), want 0", n)
	}
	var cols int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM board_columns WHERE repo_id = $1 AND label_name = 'a'`, f.repoID).Scan(&cols); err != nil || cols != 1 {
		t.Errorf("board column rows = %d (%v), want 1", cols, err)
	}
}
