package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3454 (Visibility): run-create responses carry an additive notice while maintenance is
// active. The pause is instance-global, so the test lifts it on cleanup.
func TestMaintenanceNoticeLiveDB(t *testing.T) {
	e := newV1JobsEnv(t, 0)
	_, uzc := e.user()
	setReasonPause := func(active bool, reason string) {
		t.Helper()
		ctx := context.Background()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SetInstanceIntakePause(ctx, store.New(tx), active, reason, pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setReasonPause(false, "") // explicit baseline: an absent singleton row reads as paused
	t.Cleanup(func() { setReasonPause(false, "") })

	create := func() apitypes.V1JobDTO {
		t.Helper()
		return e.create(uzc, v1MinimalJob)
	}

	if j := create(); j.Notice != nil {
		t.Fatalf("job created while maintenance is off carries notice %q", *j.Notice)
	}

	setReasonPause(true, "node drain")
	j := create()
	if j.Notice == nil {
		t.Fatal("job created during maintenance has no notice")
	}
	for _, want := range []string{"maintenance mode", "queued", "Reason: node drain"} {
		if !strings.Contains(*j.Notice, want) {
			t.Errorf("notice %q lacks %q", *j.Notice, want)
		}
	}

	// The notice is create-only: a later GET of the same job does not repeat it.
	r := e.call(http.MethodGet, "/api/v1/jobs/"+j.ID, uzc, "")
	if r.status != http.StatusOK {
		t.Fatalf("GET job = %d %s", r.status, r.body)
	}
	var got apitypes.V1JobDTO
	r.decode(t, &got)
	if got.Notice != nil {
		t.Errorf("GET job carries create-only notice %q", *got.Notice)
	}

	// The shared builder used by the run-create sites adds the same text beside the run.
	body := e.h.createdRunBody(context.Background(), map[string]string{"id": "r"})
	if n, _ := body["notice"].(string); n != *j.Notice {
		t.Errorf("createdRunBody notice = %q, want %q", n, *j.Notice)
	}

	setReasonPause(true, "") // no reason: no "Reason:" suffix
	if n := e.h.maintenanceNotice(context.Background()); n == "" || strings.Contains(n, "Reason:") {
		t.Errorf("reasonless notice = %q", n)
	}

	setReasonPause(false, "")
	if body := e.h.createdRunBody(context.Background(), 1); body["notice"] != nil {
		t.Errorf("createdRunBody after lift carries notice %v", body["notice"])
	}
}
