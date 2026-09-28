package handler

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestCreateRunInputGateRevisionMismatchLiveDB drives POST /api/runs/{id}/inputs through the
// REAL h.Routes() router for PRD #1795 M2 (D5): an approve/reject/revise carrying an
// expected_gate_revision the run no longer shows is answered 409 with the typed body
// {"error", "reason": "gate_revision_mismatch", "current_gate_revision"} and writes nothing; a
// matching one is accepted and stamped bound; a finished run answers the same typed 409 (not the
// untyped terminal one); an expected revision on a non-verdict kind is 400.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestCreateRunInputGateRevisionMismatchLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{PlanMaxRevisions: 3, WorkerHeartbeatStale: time.Minute})
	wsvc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: wsvc, cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim)

	owner, connID, repoID, wkrID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	cliMustExec(t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner, fmt.Sprintf("gaterev-in-%s@e2e", owner))
	cliMustExec(t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	cliMustExec(t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, uniq(repoID), fmt.Sprintf("g/gaterev-in-%s", repoID))
	// A live poller, so a reject is enqueued rather than applied server-side.
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at) VALUES ($1, $2, $3, $4, 'online', now())`,
		wkrID, owner, "gaterev-in-"+wkrID.String()[:8], wkrID[:])
	newGate := func(revision int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		cliMustExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		        claim_generation, auto_approve, plan_source, plan_md, gate_revision)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'awaiting_approval', $5, 1, false, 'agent', 'plan', $6)`,
			id, owner, repoID, uniq(id), wkrID, revision)
		return id
	}
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	post := func(run uuid.UUID, body string) (int, map[string]any) {
		t.Helper()
		rec := bearerReqBody(router, http.MethodPost, "/api/runs/"+run.String()+"/inputs", uzc, body)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		return rec.Code, out
	}
	inputRows := func(run uuid.UUID) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT kind || '=' || COALESCE(gate_binding, 'legacy') || COALESCE('(' || gate_revision || ')', '')
		      FROM run_user_inputs WHERE run_id = $1 ORDER BY id`, run)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}

	for _, kind := range []string{"approve_plan", "reject_plan", "revise_plan"} {
		t.Run(kind+": a stale expected revision is 409 gate_revision_mismatch and writes nothing", func(t *testing.T) {
			run := newGate(2)
			code, body := post(run, fmt.Sprintf(`{"kind":%q,"body":"why","expected_gate_revision":1}`, kind))
			if code != http.StatusConflict {
				t.Fatalf("status %d, want 409: %v", code, body)
			}
			if body["reason"] != "gate_revision_mismatch" || body["current_gate_revision"] != float64(2) {
				t.Fatalf("409 body = %v, want reason gate_revision_mismatch and current_gate_revision 2", body)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Fatalf("409 body carries no error message: %v", body)
			}
			if len(body) != 3 {
				t.Fatalf("409 body keys = %v, want exactly error, reason, current_gate_revision", body)
			}
			if rows := inputRows(run); len(rows) != 0 {
				t.Fatalf("a refused verdict wrote rows: %v", rows)
			}
			var status string
			var stopKind *string
			var reviseCount int32
			if err := pool.QueryRow(ctx, `SELECT status, stop_kind, revise_count FROM runs WHERE id = $1`, run).Scan(&status, &stopKind, &reviseCount); err != nil {
				t.Fatal(err)
			}
			if status != "awaiting_approval" || stopKind != nil || reviseCount != 0 {
				t.Fatalf("a refused verdict changed the run: status %s stop_kind %v revise_count %d", status, stopKind, reviseCount)
			}
		})
		t.Run(kind+": a matching expected revision is accepted and stamped bound", func(t *testing.T) {
			run := newGate(2)
			if code, body := post(run, fmt.Sprintf(`{"kind":%q,"body":"why","expected_gate_revision":2}`, kind)); code != http.StatusAccepted {
				t.Fatalf("status %d, want 202: %v", code, body)
			}
			if rows := inputRows(run); len(rows) != 1 || rows[0] != kind+"=bound(2)" {
				t.Fatalf("rows = %v, want [%s=bound(2)]", rows, kind)
			}
		})
	}

	// Plan decision 10: with expected_gate_revision present the typed mismatch is answered
	// whenever the run is not awaiting_approval, a finished run included, so a client acting on a
	// stale gate learns the current revision instead of the untyped "run has already finished".
	for _, status := range []string{"failed", "completed"} {
		for _, kind := range []string{"approve_plan", "reject_plan", "revise_plan"} {
			t.Run(status+" run, "+kind+": an expected revision is 409 gate_revision_mismatch with current_gate_revision", func(t *testing.T) {
				run := newGate(2)
				cliMustExec(t, pool, `UPDATE runs SET status = $2, finished_at = now() WHERE id = $1`, run, status)
				code, body := post(run, fmt.Sprintf(`{"kind":%q,"body":"why","expected_gate_revision":2}`, kind))
				if code != http.StatusConflict || body["reason"] != "gate_revision_mismatch" || body["current_gate_revision"] != float64(2) {
					t.Fatalf("status %d body %v, want 409 reason gate_revision_mismatch current_gate_revision 2", code, body)
				}
				if rows := inputRows(run); len(rows) != 0 {
					t.Fatalf("a refused verdict wrote rows: %v", rows)
				}
				// Control: without the field the terminal run keeps its untyped 409.
				code, body = post(run, fmt.Sprintf(`{"kind":%q,"body":"why"}`, kind))
				if code != http.StatusConflict || body["reason"] != nil {
					t.Fatalf("without expected_gate_revision: status %d body %v, want the untyped terminal 409", code, body)
				}
			})
		}
	}

	t.Run("an expected revision on a non-verdict kind is 400", func(t *testing.T) {
		run := newGate(2)
		if code, body := post(run, `{"kind":"follow_up","body":"x","expected_gate_revision":2}`); code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %v", code, body)
		}
	})
}
