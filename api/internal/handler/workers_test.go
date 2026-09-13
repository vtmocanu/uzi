package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// deleteWorkerStore is a minimal workersvc.Store for the DeleteWorker handler tests:
// it answers only the three queries wsvc.DeleteWorker reaches (active-run count, open
// custody count, the delete itself) and panics on anything else (embedded interface),
// so a mapping test cannot pass silently through an unexercised path.
type deleteWorkerStore struct {
	workersvc.Store
	activeRuns int64
	holds      int64
	deleteRows int64
	deleted    bool
}

func (s *deleteWorkerStore) CountWorkerNonTerminalRuns(context.Context, store.CountWorkerNonTerminalRunsParams) (int64, error) {
	return s.activeRuns, nil
}

func (s *deleteWorkerStore) CountOpenCustodyHoldsForWorker(context.Context, store.CountOpenCustodyHoldsForWorkerParams) (int64, error) {
	return s.holds, nil
}

func (s *deleteWorkerStore) DeleteWorkerForUser(context.Context, store.DeleteWorkerForUserParams) (int64, error) {
	s.deleted = true
	return s.deleteRows, nil
}

// deleteWorkerReq builds a DELETE /api/workers/{id} authenticated as user with the chi
// {id} route param the handler reads through httpx.PathUUID.
func deleteWorkerReq(user store.User, id uuid.UUID) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/api/workers/"+id.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

// TestDeleteWorkerCustodyConflict pins the M4b handler mapping (PRD #1296, D3): when the
// service refuses with *WorkerHasCustodyError, DeleteWorker answers a 409 that carries a
// `custody_holds` count and recovery guidance — DISTINCT from the active-runs 409 — and
// never reaches the delete. The count wired to the store proves the handler surfaces the
// service's Holds rather than a hardcoded value.
func TestDeleteWorkerCustodyConflict(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &deleteWorkerStore{holds: 3}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}

	rec := httptest.NewRecorder()
	h.DeleteWorker(rec, deleteWorkerReq(owner, uuid.New()))

	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE custody-held worker = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error        string `json:"error"`
		CustodyHolds int64  `json:"custody_holds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v (%s)", err, rec.Body.String())
	}
	if body.CustodyHolds != 3 {
		t.Errorf("custody_holds = %d, want 3 (the service's Holds count)", body.CustodyHolds)
	}
	// The refusal must NAME the retained work and the recovery path — and read distinctly
	// from the active-runs refusal, which is what makes it actionable rather than generic.
	if !strings.Contains(body.Error, "3") || !strings.Contains(body.Error, "recover") {
		t.Errorf("error = %q, want it to name the count and point to recovery", body.Error)
	}
	if strings.Contains(body.Error, "active runs") {
		t.Errorf("custody refusal must not read as the active-runs case: %q", body.Error)
	}
	if st.deleted {
		t.Error("a custody-held worker must not be deleted")
	}
}

// TestDeleteWorkerNormalSucceeds is the positive control: with no active runs and no
// custody holds the delete reaches the store and answers 204, so the 409 above is
// specifically the custody guard and not a blanket refusal.
func TestDeleteWorkerNormalSucceeds(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &deleteWorkerStore{deleteRows: 1}
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}

	rec := httptest.NewRecorder()
	h.DeleteWorker(rec, deleteWorkerReq(owner, uuid.New()))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE clean worker = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	if !st.deleted {
		t.Error("a clean worker delete must reach DeleteWorkerForUser")
	}
}

// TestWorkerDTODockerMapping pins the worker.docker_enabled → WorkerDTO.Docker
// convention (PRD #83 M3, PRD #76) without a database: the mapping is pure, so a
// bare store.Worker is enough to exercise every branch of boolPtrValue through the
// real DTO builder. A NULL column (an external worker, where DinD is not a concept)
// must map to JSON null; a valid column must carry the stored true/false through
// unchanged so the row badge reflects a hosted worker's actual sidecar state.
func TestWorkerDTODockerMapping(t *testing.T) {
	cases := []struct {
		name    string
		col     pgtype.Bool
		wantNil bool
		want    bool // only meaningful when wantNil is false
	}{
		{"external worker (NULL)", pgtype.Bool{Valid: false}, true, false},
		{"hosted docker-capable", pgtype.Bool{Bool: true, Valid: true}, false, true},
		{"hosted no sidecar", pgtype.Bool{Bool: false, Valid: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto := workerDTOFromWorker(store.Worker{DockerEnabled: tc.col}, 0, false, "", "", "", time.Now(), time.Now())
			if tc.wantNil {
				if dto.Docker != nil {
					t.Fatalf("Docker = %v, want nil for a NULL docker_enabled column", *dto.Docker)
				}
				return
			}
			if dto.Docker == nil {
				t.Fatalf("Docker = nil, want a non-nil *bool for a valid column")
			}
			if *dto.Docker != tc.want {
				t.Fatalf("*Docker = %v, want %v", *dto.Docker, tc.want)
			}
		})
	}
}

// TestWorkerDTOCarriesUpgradeClassification pins the wiring, not the rules: that BOTH
// DTO builders run the classifier and that they agree. The rules themselves are pinned
// in workersvc/upgrade_test.go, which is where a rule change belongs.
//
// Two builders exist because the list path has a credential-label join the bare-row
// path does not, and they have drifted apart before. A field added to one and forgotten
// in the other is invisible from the outside — the list endpoint would carry the badge
// while the register/heartbeat responses silently returned the zero value, so a worker
// would show up classified and then blank itself on its next beat.
func TestWorkerDTOCarriesUpgradeClassification(t *testing.T) {
	const cpVersion = "0.11.7"
	behind := pgtype.Text{String: "0.11.0", Valid: true}

	fromWorker := workerDTOFromWorker(store.Worker{Version: behind}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	fromRow := workerDTOFromRow(store.ListWorkersByUserRow{Version: behind}, cpVersion, "", time.Now(), time.Now())

	for name, dto := range map[string]struct {
		status string
		detail *string
	}{
		"workerDTOFromWorker": {fromWorker.UpgradeStatus, fromWorker.UpgradeDetail},
		"workerDTOFromRow":    {fromRow.UpgradeStatus, fromRow.UpgradeDetail},
	} {
		if dto.status != workersvc.UpgradeStatusOutdated {
			t.Errorf("%s: UpgradeStatus = %q, want %q", name, dto.status, workersvc.UpgradeStatusOutdated)
		}
		if dto.detail == nil {
			t.Errorf("%s: UpgradeDetail = nil, want the sentence behind the badge", name)
			continue
		}
		if *dto.detail != "running 0.11.0, target 0.11.7" {
			t.Errorf("%s: UpgradeDetail = %q, want the running/target sentence", name, *dto.detail)
		}
	}

	// The steady state serializes as JSON null rather than an empty string, so the UI
	// renders no explanatory line at all instead of an empty one.
	current := workerDTOFromWorker(store.Worker{Version: pgtype.Text{String: cpVersion, Valid: true}}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	if current.UpgradeStatus != workersvc.UpgradeStatusUpToDate {
		t.Errorf("UpgradeStatus = %q, want %q", current.UpgradeStatus, workersvc.UpgradeStatusUpToDate)
	}
	if current.UpgradeDetail != nil {
		t.Errorf("UpgradeDetail = %q, want nil for the up_to_date steady state", *current.UpgradeDetail)
	}

	// A NULL version column must not reach the classifier as a comparable value: the
	// pgtype zero value is the empty string, which is the unreported case.
	never := workerDTOFromWorker(store.Worker{Version: pgtype.Text{Valid: false}}, 0, false, "", cpVersion, "", time.Now(), time.Now())
	if never.UpgradeStatus != workersvc.UpgradeStatusUnknown {
		t.Errorf("UpgradeStatus = %q for a worker that never registered a version, want %q",
			never.UpgradeStatus, workersvc.UpgradeStatusUnknown)
	}
}

// TestWorkerDTOCarriesDrainingSince pins the workers.draining_since →
// WorkerDTO.DrainingSince wiring (PRD #496 M1) through BOTH DTO builders, the same
// way TestWorkerDTOCarriesUpgradeClassification does: a field added to one mapper and
// forgotten in the other is invisible from the outside. A valid pgtype.Timestamptz
// must carry its instant through unchanged; a NULL column must map to JSON null (the
// worker that will claim normally).
func TestWorkerDTOCarriesDrainingSince(t *testing.T) {
	ts := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	now := time.Now()

	t.Run("workerDTOFromWorker set", func(t *testing.T) {
		dto := workerDTOFromWorker(
			store.Worker{DrainingSince: pgtype.Timestamptz{Time: ts, Valid: true}},
			0, false, "", "", "", now, now)
		if dto.DrainingSince == nil {
			t.Fatalf("DrainingSince = nil, want a non-nil *time.Time for a valid column")
		}
		if !dto.DrainingSince.Equal(ts) {
			t.Fatalf("DrainingSince = %v, want %v", *dto.DrainingSince, ts)
		}
	})

	t.Run("workerDTOFromWorker null", func(t *testing.T) {
		dto := workerDTOFromWorker(
			store.Worker{DrainingSince: pgtype.Timestamptz{Valid: false}},
			0, false, "", "", "", now, now)
		if dto.DrainingSince != nil {
			t.Fatalf("DrainingSince = %v, want nil for a NULL draining_since column", *dto.DrainingSince)
		}
	})

	t.Run("workerDTOFromRow set", func(t *testing.T) {
		dto := workerDTOFromRow(
			store.ListWorkersByUserRow{DrainingSince: pgtype.Timestamptz{Time: ts, Valid: true}},
			"", "", now, now)
		if dto.DrainingSince == nil {
			t.Fatalf("DrainingSince = nil, want a non-nil *time.Time for a valid column")
		}
		if !dto.DrainingSince.Equal(ts) {
			t.Fatalf("DrainingSince = %v, want %v", *dto.DrainingSince, ts)
		}
	})

	t.Run("workerDTOFromRow null", func(t *testing.T) {
		dto := workerDTOFromRow(
			store.ListWorkersByUserRow{DrainingSince: pgtype.Timestamptz{Valid: false}},
			"", "", now, now)
		if dto.DrainingSince != nil {
			t.Fatalf("DrainingSince = %v, want nil for a NULL draining_since column", *dto.DrainingSince)
		}
	})
}
