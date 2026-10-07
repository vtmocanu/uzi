package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The same real state drives the display-only count and all existing safety reads.
// No safety predicate depends on how many holds await an owner decision.
func TestWorkerCustodyDecisionMatrixLiveDB(t *testing.T) {
	cases := []struct {
		name, status, state string
		captures            []string
		attention           string
		decisions           int
	}{
		{"active running", "running", "open", nil, "active", 0},
		{"active queued", "queued", "open", nil, "active", 0},
		{"active paused", "paused", "open", nil, "active", 0},
		{"needs action live", "running", "open", []string{"needs_action"}, "needs_action", 1},
		{"needs action terminal", "failed", "open", []string{"needs_action"}, "needs_action", 1},
		{"source terminal", "failed", "open", nil, "source_only", 1},
		{"source missing", "missing", "open", nil, "source_only", 1},
		{"archive ready", "failed", "open", []string{"available"}, "archive_ready", 0},
		{"available beats latest failure", "running", "open", []string{"available", "needs_action"}, "archive_ready", 0},
		{"preparing terminal", "failed", "open", []string{"preparing"}, "capturing", 0},
		{"uploading live", "running", "open", []string{"uploading"}, "capturing", 0},
		{"created time beats id", "failed", "open", []string{"uploading", "needs_action"}, "needs_action", 1},
		{"newest capture wins", "failed", "open", []string{"needs_action", "uploading"}, "capturing", 0},
		{"released", "failed", "released", nil, "released", 0},
		{"discarded", "failed", "discarded", nil, "discarded", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRecoveryEnv(t)
			q := store.New(e.pool)
			mustExecT(e.ctx, t, e.pool, `UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1`, e.worker.ID)
			if tc.status == "missing" {
				mustExecT(e.ctx, t, e.pool, `UPDATE recovery_custody_holds SET run_id=$2 WHERE id=$1`, e.holdID, uuid.New())
			} else {
				mustExecT(e.ctx, t, e.pool, `UPDATE runs SET status=$2 WHERE id=$1`, e.run, tc.status)
			}
			mustExecT(e.ctx, t, e.pool, `UPDATE recovery_custody_holds SET state=$2 WHERE id=$1`, e.holdID, tc.state)
			for i, state := range tc.captures {
				// Equal timestamps exercise the deterministic id DESC tie-break as well as EXISTS.
				// Each subtest owns its hold; retain its random prefix for unique capture UUIDs.
				id := e.holdID
				if i == 0 {
					id[15] = 1
				} else {
					id[15] = 2
				}
				created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				if tc.name == "created time beats id" {
					if i == 0 {
						id[15] = 2
					} else {
						id[15] = 1
					}
					created = created.Add(time.Duration(i) * time.Hour)
				}
				mustExecT(e.ctx, t, e.pool, `INSERT INTO recovery_captures (id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state,created_at)
    VALUES ($1,$2,$3,$4,'worker','sha',$5,$6,$7)`, id, e.holdID, e.run, e.user, id.String(), state, created)
			}
			svc := e.service(recoveryTestLimits())
			counts, err := svc.CustodyDecisionsByWorker(e.ctx, []uuid.UUID{e.worker.ID})
			if err != nil || counts[e.worker.ID] != tc.decisions {
				t.Fatalf("real batch counts=%v error=%v want=%d", counts, err, tc.decisions)
			}
			owner, err := svc.ListHoldsForOwner(e.ctx, e.user, false)
			if err != nil || len(owner.Holds) != 1 {
				t.Fatalf("owner list=%+v error=%v", owner, err)
			}
			if owner.Holds[0].Attention != tc.attention || owner.Aggregate.DecisionNeeded != tc.decisions {
				t.Fatalf("owner parity=%+v want=%s/%d", owner, tc.attention, tc.decisions)
			}
			batch, err := q.ListOpenCustodyHoldsForWorkers(e.ctx, []uuid.UUID{e.worker.ID})
			wantOpen := tc.state == "open"
			if err != nil || (len(batch) == 1) != wantOpen {
				t.Fatalf("batch=%+v err=%v open=%v", batch, err, wantOpen)
			}
			if wantOpen {
				rows, err := q.ListCustodyHoldsForOwner(e.ctx, store.ListCustodyHoldsForOwnerParams{UserID: e.user})
				if err != nil {
					t.Fatal(err)
				}
				a, b := rows[0], batch[0]
				if a.State != b.State || a.HasAvailableCapture != b.HasAvailableCapture || a.CaptureState != b.CaptureState || a.RunStatus != b.RunStatus {
					t.Fatalf("real owner/batch input mismatch: %+v / %+v", a, b)
				}
			}
			holds, err := q.CountOpenCustodyHoldsForWorker(e.ctx, store.CountOpenCustodyHoldsForWorkerParams{UserID: e.user, WorkerID: e.worker.ID})
			if err != nil || (holds == 1) != wantOpen {
				t.Fatalf("safety count=%d err=%v wantOpen=%v", holds, err, wantOpen)
			}
			hosted, err := q.ListHostedWorkersForController(e.ctx, store.ListHostedWorkersForControllerParams{DiskPressureMinStreak: 2, HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, w := range hosted {
				if w.ID == e.worker.ID {
					found = true
					if w.CustodyHeld != wantOpen {
						t.Fatalf("hosted custody=%v want=%v", w.CustodyHeld, wantOpen)
					}
				}
			}
			if !found {
				t.Fatal("hosted worker absent")
			}
			h := e.handler()
			for _, admin := range []bool{false, true} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(e.ctx, store.User{ID: e.user}))
				if admin {
					h.AdminListWorkers(rec, req)
				} else {
					h.ListWorkers(rec, req)
				}
				rows := custodyListJSON(t, rec)
				found = false
				for _, row := range rows {
					if row["id"] == e.worker.ID.String() {
						found = true
						if row["custody_decisions_needed"] != float64(tc.decisions) {
							t.Fatalf("list count=%v", row)
						}
					}
				}
				if !found {
					t.Fatal("listed worker absent")
				}
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(`{}`)).WithContext(mw.ContextWithWorker(e.ctx, e.worker))
			h.WorkerHeartbeat(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("heartbeat HTTP %d: %s", rec.Code, rec.Body.String())
			}
			var heartbeat struct {
				Worker map[string]any `json:"worker"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &heartbeat); err != nil {
				t.Fatal(err)
			}
			if heartbeat.Worker["retaining_unpublished_work"] != wantOpen {
				t.Fatalf("heartbeat safety=%v want=%v", heartbeat.Worker, wantOpen)
			}
			if _, ok := heartbeat.Worker["custody_decisions_needed"]; ok {
				t.Fatal("heartbeat emitted decision count")
			}
		})
	}
}

func TestWorkerCustodyDecisionIdentityLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	other := newRecoveryEnv(t)
	q := store.New(e.pool)
	live, zero := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{live, zero} {
		mustExecT(e.ctx, t, e.pool, `INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,'live',$3,'online')`, id, e.user, id[:])
	}
	mustExecT(e.ctx, t, e.pool, `UPDATE runs SET status='failed' WHERE id=$1`, e.run)
	// A hold moves to a same-owner live worker; the owner DTO still records original provenance.
	mustExecT(e.ctx, t, e.pool, `UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1`, e.holdID, live)
	mustExecT(e.ctx, t, e.pool, `INSERT INTO recovery_custody_holds (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
  VALUES ($1,$2,$3,2,'open',$4,'original',$5,$3)`, e.user, e.repo, e.run, e.worker.ID, live)
	// Mismatched hold owner must not be counted even though the live FK points at this worker.
	mustExecT(e.ctx, t, e.pool, `UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1`, other.holdID, live)
	counts, err := e.service(recoveryTestLimits()).CustodyDecisionsByWorker(e.ctx, []uuid.UUID{e.worker.ID, live, zero, other.worker.ID})
	if err != nil || len(counts) != 4 || counts[live] != 2 || counts[e.worker.ID] != 0 || counts[zero] != 0 || counts[other.worker.ID] != 0 {
		t.Fatalf("identity counts=%v err=%v", counts, err)
	}
	// Owner-matching run LEFT JOIN: a foreign running run must read as missing, not active.
	mustExecT(e.ctx, t, e.pool, `UPDATE recovery_custody_holds SET run_id=$2 WHERE id=$1`, e.holdID, other.run)
	rows, err := q.ListOpenCustodyHoldsForWorkers(e.ctx, []uuid.UUID{live})
	if err != nil || len(rows) != 2 {
		t.Fatalf("batch rows=%v err=%v", rows, err)
	}
	statuses := map[string]int{}
	for _, row := range rows {
		statuses[row.RunStatus]++
	}
	if statuses[""] != 1 || statuses["failed"] != 1 {
		t.Fatalf("owner matched run statuses=%v", statuses)
	}
	h := e.handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workers", nil).WithContext(mw.ContextWithUser(e.ctx, store.User{ID: e.user}))
	h.ListWorkers(rec, req)
	listed := custodyListJSON(t, rec)
	if len(listed) != 3 {
		t.Fatalf("owner list leaked worker: %v", listed)
	}
	for _, row := range listed {
		want := float64(0)
		if row["id"] == live.String() {
			want = 2
		}
		if row["custody_decisions_needed"] != want {
			t.Fatalf("owner count=%v want=%v", row, want)
		}
	}
	rec = httptest.NewRecorder()
	h.AdminListWorkers(rec, req)
	listed = custodyListJSON(t, rec)
	seen := map[string]bool{}
	for _, row := range listed {
		id := row["id"].(string)
		want, requested := counts[uuid.MustParse(id)]
		if requested {
			seen[id] = true
			if row["custody_decisions_needed"] != float64(want) {
				t.Fatalf("admin count=%v want=%v", row, want)
			}
		}
	}
	if len(seen) != 4 {
		t.Fatalf("admin omitted requested workers: %v", seen)
	}
}
