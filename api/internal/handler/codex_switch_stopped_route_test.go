package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// stoppedRouteTx supplies a SQL verdict, NOT an implementation of the SQL
// predicate. LiveDB CI owns eligibility/replay/storage semantics; this fixture
// exercises the real service transaction and HTTP acknowledgement wiring.
type stoppedRouteTx struct {
	pgx.Tx
	t            *testing.T
	worker       store.Worker
	run          store.Run
	accepted     bool
	commitErr    error
	commits      int
	committed    bool
	settlements  int
	currentReads int
}

func (tx *stoppedRouteTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *stoppedRouteTx) Rollback(context.Context) error        { return nil }
func (tx *stoppedRouteTx) Commit(context.Context) error {
	tx.commits++
	tx.committed = tx.commitErr == nil
	return tx.commitErr
}

func (tx *stoppedRouteTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if sql != "SET TRANSACTION ISOLATION LEVEL READ COMMITTED" {
		tx.t.Fatalf("unexpected Exec: %s", sql)
	}
	return pgconn.CommandTag{}, nil
}

func (tx *stoppedRouteTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.HasPrefix(sql, "-- name: GetWorkerForUpdate "):
		if !reflect.DeepEqual(args, []any{tx.worker.ID}) {
			tx.t.Fatalf("worker lock args: %v", args)
		}
		return codeStatusRow{value: tx.worker}
	case strings.HasPrefix(sql, "-- name: GetRunOwnedByWorkerForUpdate "),
		strings.HasPrefix(sql, "-- name: GetRunOwnedByWorker "):
		want := []any{tx.run.ID, tx.run.WorkerID}
		if !reflect.DeepEqual(args, want) {
			tx.t.Fatalf("owned run args: %v, want %v", args, want)
		}
		if strings.HasPrefix(sql, "-- name: GetRunOwnedByWorker ") {
			tx.currentReads++
		}
		return codeStatusRow{value: tx.run}
	case sql == "SELECT EXISTS (SELECT 1 FROM run_milestone_reconciliations WHERE run_id=$1)":
		if !reflect.DeepEqual(args, []any{tx.run.ID}) {
			tx.t.Fatalf("reconciliation args: %v", args)
		}
		return codeStatusRow{value: struct{ Exists bool }{false}}
	case strings.HasPrefix(sql, "-- name: SetCodexSwitchStopped "):
		want := []any{tx.run.ID, tx.worker.ID, tx.worker.UserID, int64(7), "nonce", int64(41), int32(3600)}
		if !reflect.DeepEqual(args, want) {
			tx.t.Fatalf("settlement args: %v, want %v", args, want)
		}
		tx.settlements++
		if tx.accepted {
			tx.run.Status = "paused"
		}
		return codeStatusRow{value: struct{ Accepted bool }{tx.accepted}}
	default:
		tx.t.Fatalf("unexpected QueryRow: %s", sql)
		return codeStatusRow{err: errors.New("unexpected stopped route query")}
	}
}

// Observe both explicit and implicit response writes: success must not become
// visible until Commit has returned successfully.
type stoppedRouteRecorder struct {
	*httptest.ResponseRecorder
	t  *testing.T
	tx *stoppedRouteTx
}

func (w *stoppedRouteRecorder) WriteHeader(status int) {
	if status == http.StatusOK && !w.tx.committed {
		w.t.Fatal("200 acknowledgement written before successful commit")
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *stoppedRouteRecorder) Write(body []byte) (int, error) {
	if w.Code == http.StatusOK && !w.tx.committed {
		w.t.Fatal("success body written before successful commit")
	}
	return w.ResponseRecorder.Write(body)
}

func TestWorkerRunStateCodexSwitchStoppedRoute(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		accepted   bool
		commitErr  error
		wantStatus int
		planBytes  int
	}{
		{"fresh_accepted", "running", true, nil, http.StatusOK, 0},
		{"fully_accepted_paused_replay", "paused", true, nil, http.StatusOK, 0},
		{"fresh_large_plan", "running", true, nil, http.StatusOK, 64 << 10},
		{"large_plan_paused_replay", "paused", true, nil, http.StatusOK, 64 << 10},
		{"refused_paused_reply", "paused", false, nil, http.StatusConflict, 0},
		{"commit_error", "running", true, errors.New("commit outcome unknown"), http.StatusInternalServerError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exactly the stopped-report fields: no ordinary pause inputs or op_id.
			runID := uuid.New()
			req := workerReq(http.MethodPost, `{"status":"codex_switch_stopped","claim_generation":7,"register_nonce":"nonce","expected_epoch":41}`, runID)
			worker, ok := mw.WorkerFromContext(req.Context())
			if !ok {
				t.Fatal("missing authenticated worker")
			}
			worker.SnapshotRegisterNonce = pgtype.Text{String: "nonce", Valid: true}
			req = req.WithContext(mw.ContextWithWorker(req.Context(), worker))
			tx := &stoppedRouteTx{
				t: t, worker: worker, accepted: tc.accepted, commitErr: tc.commitErr,
				run: store.Run{
					ID: runID, UserID: worker.UserID,
					WorkerID: pgtype.UUID{Bytes: worker.ID, Valid: true},
					Kind:     "issue", Status: tc.status, Harness: "codex",
					ClaimGeneration: 7, CodexClaimEpoch: 41,
					PlanMd:        pgtype.Text{String: strings.Repeat("p", tc.planBytes), Valid: tc.planBytes != 0},
					CodexAuthMode: pgtype.Text{String: "api_key", Valid: true},
					CodexSecretID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
					CodexCapHash:  []byte("fixture-capability-hash"),
				},
			}
			svc := workersvc.New(&protocolStore{}, nil, workersvc.Params{
				RunTimeout: time.Hour, CustodyHoldLimit: defaultCustodyLimit(),
			})
			svc.SetTxBeginner(tx)
			h := &Handler{wsvc: svc}
			rec := &stoppedRouteRecorder{ResponseRecorder: httptest.NewRecorder(), t: t, tx: tx}
			h.WorkerRunState(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK && rec.Body.Len() > 16*1024 {
				t.Fatalf("stopped acknowledgement exceeds worker response bound: %d bytes", rec.Body.Len())
			}
			if tx.run.PlanMd.String != strings.Repeat("p", tc.planBytes) || tx.run.PlanMd.Valid != (tc.planBytes != 0) {
				t.Fatal("stopped acknowledgement changed the stored plan")
			}
			var got struct {
				Applied     *bool   `json:"applied"`
				Disposition *string `json:"disposition"`
				Run         struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == http.StatusOK {
				if got.Applied == nil || !*got.Applied || got.Disposition == nil || *got.Disposition != "codex_switch_stopped" || got.Run.Status != "paused" {
					t.Fatalf("missing explicit stopped acknowledgement: %s", rec.Body.String())
				}
			} else {
				if got.Applied != nil || got.Disposition != nil {
					t.Fatalf("refusal/error acknowledged settlement: %s", rec.Body.String())
				}
				if tc.wantStatus == http.StatusConflict && got.Run.Status != "paused" {
					t.Fatalf("refusal lost actual paused status: %s", rec.Body.String())
				}
			}
			wantCommits, wantReads := 0, 0
			if tc.accepted {
				wantCommits, wantReads = 1, 1
			}
			if tx.settlements != 1 || tx.commits != wantCommits || tx.currentReads != wantReads {
				t.Fatalf("settlements=%d commits=%d current reads=%d; want 1/%d/%d", tx.settlements, tx.commits, tx.currentReads, wantCommits, wantReads)
			}
		})
	}
}
