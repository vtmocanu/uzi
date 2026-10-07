package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func rejectionHTTP(e *settleEnv, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestTerminalRejectionRoutesLiveDB(t *testing.T) {
	e := newSettleEnv(t)
	e.wsvc.SetTxBeginner(e.pool)
	post := func(token string, g int64) *httptest.ResponseRecorder {
		return rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", token,
			fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":%d,"reason":"mac_failure"}]}`, e.run, g))
	}
	get := func(g int64) workersvc.TerminalRejectionCustody {
		rec := rejectionHTTP(e, http.MethodGet, fmt.Sprintf("/api/worker/runs/%s/terminal-rejection-custody?generation=%d", e.run, g), e.tokenA, "")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() > 128<<10 {
			t.Fatalf("GET %d %s", rec.Code, rec.Body.String())
		}
		var out workersvc.TerminalRejectionCustody
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.RunID != e.run.String() || out.WorkerID != e.workerA.String() || out.Generation != g {
			t.Fatalf("echo %+v", out)
		}
		return out
	}
	if rec := post("", 1); rec.Code != 401 {
		t.Fatalf("no auth %d", rec.Code)
	}
	if rec := rejectionHTTP(e, http.MethodPost, "/api/worker/register", e.tokenA, "{}"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "terminal_rejection_report") {
		t.Fatalf("register %d %s", rec.Code, rec.Body.String())
	}
	// Register may change assignment/state; pin the retroactive case after registration.
	e.exec("UPDATE runs SET status='failed', fail_origin='worker_lost', failure_reason='old', worker_id=$2, claim_generation=1 WHERE id=$1", e.run, e.workerA)
	duplicate := e.insertHold(e.run, 1, e.workerA)
	closed := e.insertHold(e.run, 1, e.workerA)
	e.exec("UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL WHERE id=$1", closed)
	for i := 0; i < 2; i++ {
		assertRejectionDisposition(t, post(e.tokenA, 1), e.run, 1, "recorded")
	}
	for _, id := range []uuid.UUID{e.pred, duplicate} {
		var annotation, state string
		if err := e.pool.QueryRow(e.ctx, "SELECT terminal_record_rejection,state FROM recovery_custody_holds WHERE id=$1", id).Scan(&annotation, &state); err != nil {
			t.Fatal(err)
		}
		if annotation != "mac_failure" || state != "open" {
			t.Fatalf("hold %s %s", annotation, state)
		}
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE id=ANY($1) AND terminal_record_rejection IS NOT NULL", []uuid.UUID{closed, e.sibGen, e.sibWork}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sibling/closed modified %d %v", n, err)
	}
	var reason, status string
	if err := e.pool.QueryRow(e.ctx, "SELECT failure_reason,status FROM runs WHERE id=$1", e.run).Scan(&reason, &status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody" {
		t.Fatalf("run %s %s", status, reason)
	}
	out := get(1)
	if out.ExactCount != 3 || out.SiblingCount != 1 || out.Outcome != "retained" {
		t.Fatalf("snapshot %+v", out)
	}
	ownerToken := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	rec := bearerReq(e.router, http.MethodGet, "/api/recovery/holds", ownerToken)
	var owner apitypes.RecoveryCustodyHoldsDTO
	if rec.Code != 200 {
		t.Fatalf("owner %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &owner); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range owner.Holds {
		if h.ID == e.pred.String() {
			found = h.TerminalRecordRejection == "mac_failure"
		}
	}
	if !found {
		t.Fatal("owner projection missing annotation")
	}
	// Closed exact rows remain visible, and an open older-generation sibling prevents settlement.
	e.exec("UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL WHERE run_id=$1 AND original_worker_id=$2 AND generation=1", e.run, e.workerA)
	if out := get(1); out.Outcome != "unknown" || out.ExactCount != 3 {
		t.Fatalf("ambiguous %+v", out)
	}
	e.exec("UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL WHERE id=$1", e.sibGen)
	if out := get(1); out.Outcome != "settled" {
		t.Fatalf("settlement %+v", out)
	}
	assertRejectionDisposition(t, post(e.tokenA, 1), e.run, 1, "skipped")
	if out := get(99); out.Outcome != "unknown" || out.ExactCount != 0 {
		t.Fatalf("missing %+v", out)
	}
	// Independent caps, counts before limits, deterministic ID ordering.
	for i := 0; i < 257; i++ {
		e.insertHold(e.run, 3, e.workerA)
		e.insertHold(e.run, 4, e.workerA)
	}
	out = get(3)
	var exact []struct{ ID, State string }
	var siblings []struct {
		ID         string
		Generation int64
	}
	if err := json.Unmarshal(out.ExactHolds, &exact); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.SiblingHolds, &siblings); err != nil {
		t.Fatal(err)
	}
	if out.ExactCount != 257 || out.SiblingCount != 257 || len(exact) != 256 || len(siblings) != 256 || out.Complete || out.Outcome != "retained" {
		t.Fatalf("caps %+v lengths %d %d", out, len(exact), len(siblings))
	}
	for i := 1; i < len(exact); i++ {
		if exact[i-1].ID >= exact[i].ID {
			t.Fatal("exact order")
		}
	}
	for i := 1; i < len(siblings); i++ {
		if siblings[i-1].ID >= siblings[i].ID {
			t.Fatal("sibling order")
		}
	}
	// Original provenance, not live pointers, is the scope.
	e.exec("UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1", e.pred, e.workerB)
	if out := get(1); out.ExactCount != 3 {
		t.Fatalf("lost provenance %+v", out)
	}
	foreign := uuid.New()
	foreignToken := e.insertWorker(cliSeedUser(t, e.pool, false), foreign)
	rec = rejectionHTTP(e, http.MethodGet, fmt.Sprintf("/api/worker/runs/%s/terminal-rejection-custody?generation=1", e.run), foreignToken, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"exact_count":0`) {
		t.Fatalf("foreign %d %s", rec.Code, rec.Body.String())
	}
}

func assertRejectionDisposition(t *testing.T, rec *httptest.ResponseRecorder, run uuid.UUID, generation int64, disposition string) {
	t.Helper()
	if rec.Code != 200 || rec.Body.Len() > 128<<10 {
		t.Fatalf("POST %d %s", rec.Code, rec.Body.String())
	}
	var out workersvc.TerminalRejectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Dispositions) != 1 {
		t.Fatalf("dispositions %+v", out)
	}
	want := workersvc.TerminalRejectionDisposition{RunID: run.String(), ClaimGeneration: generation, Reason: "mac_failure", Disposition: disposition}
	if out.Dispositions[0] != want {
		t.Fatalf("disposition %+v want %+v", out.Dispositions[0], want)
	}
}

const rejectionExplanation = "terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody"

func TestTerminalRejectionWorkerLostWritersLiveDB(t *testing.T) {
	for _, writer := range []string{"stale", "register", "attested", "snapshot"} {
		for _, scope := range []string{"exact", "foreign", "history"} {
			for _, custodyState := range []string{"open", "released", "discarded"} {
				t.Run(writer+"/"+scope+"/"+custodyState, func(t *testing.T) {
					e := newSettleEnv(t)
					e.wsvc.SetTxBeginner(e.pool)
					e.exec("UPDATE runs SET status='running', claim_generation=1, requeue_count=2, claim_released_at=NULL, status_since=now()-interval '1 hour', started_at=now() WHERE id=$1", e.run)
					generation := int64(1)
					token := e.tokenA
					if scope == "foreign" {
						token = e.tokenB
					}
					if scope == "history" {
						generation = 2
					}
					rec := rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", token, fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":%d,"reason":"mac_failure"}]}`, e.run, generation))
					assertRejectionDisposition(t, rec, e.run, generation, "recorded")
					// Settled fixtures model explicit historical source settlement; retain all rows and diagnostics.
					if custodyState != "open" {
						e.exec("UPDATE recovery_custody_holds SET state=$2,live_worker_id=NULL,live_run_id=NULL WHERE run_id=$1 AND user_id=$3", e.run, custodyState, e.user)
					}
					var custodyBefore string
					if err := e.pool.QueryRow(e.ctx, "SELECT jsonb_agg(to_jsonb(h) ORDER BY id)::text FROM recovery_custody_holds h WHERE run_id=$1", e.run).Scan(&custodyBefore); err != nil {
						t.Fatal(err)
					}
					q := store.New(e.pool)
					generic := pgconv.TextOrNull("generic worker lost")
					var err error
					switch writer {
					case "stale":
						e.exec("UPDATE workers SET last_heartbeat_at=now()-interval '2 hours' WHERE id=$1", e.workerA)
						_, err = q.FailRunsOfStaleWorkersOverCap(e.ctx, store.FailRunsOfStaleWorkersOverCapParams{FailureReason: generic, MaxRequeues: 2, FailCutoff: pgconv.Time(time.Now().Add(-time.Hour))})
					case "register":
						_, err = q.FailWorkerRunsOverCap(e.ctx, store.FailWorkerRunsOverCapParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 2})
					case "attested":
						_, err = q.FailAttestedFinalizeRunsOverCap(e.ctx, store.FailAttestedFinalizeRunsOverCapParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 0, RunIds: []uuid.UUID{e.run}, ClaimGenerations: []int64{1}})
					case "snapshot":
						_, err = q.FailRunsMissingFromSnapshot(e.ctx, store.FailRunsMissingFromSnapshotParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 2, MissingCutoff: pgconv.Time(time.Now()), Now: pgconv.Time(time.Now()), GlobalTimeoutSeconds: 7200})
					}
					if err != nil {
						t.Fatal(err)
					}
					var custodyAfter string
					if err := e.pool.QueryRow(e.ctx, "SELECT jsonb_agg(to_jsonb(h) ORDER BY id)::text FROM recovery_custody_holds h WHERE run_id=$1", e.run).Scan(&custodyAfter); err != nil {
						t.Fatal(err)
					}
					if custodyBefore != custodyAfter {
						t.Fatalf("writer changed source custody or diagnostic provenance: before=%s after=%s", custodyBefore, custodyAfter)
					}
					if custodyState == "open" {
						rejectionAssertPark(t, e)
						e.assertOpen(e.pred, e.sibGen, e.sibWork)
						return
					}
					var status, origin, reason string
					if err := e.pool.QueryRow(e.ctx, "SELECT status,fail_origin,failure_reason FROM runs WHERE id=$1", e.run).Scan(&status, &origin, &reason); err != nil {
						t.Fatal(err)
					}
					want := "generic worker lost"
					if scope == "exact" {
						want = rejectionExplanation
					}
					var absent bool
					if err := e.pool.QueryRow(e.ctx, `SELECT finished_at IS NOT NULL
					AND NOT (worker_recovery_evidence->>'custody_uncertain')::boolean
					AND NOT (worker_recovery_evidence->>'unknown')::boolean
					AND checkpoint_tip IS NULL
					AND NOT EXISTS(SELECT 1 FROM recovery_captures WHERE run_id=r.id)
					AND NOT EXISTS(SELECT 1 FROM checkpoint_publish_attempts WHERE run_id=r.id)
					FROM runs r WHERE id=$1`, e.run).Scan(&absent); err != nil || !absent {
						t.Fatalf("fallback absence not established: %t %v", absent, err)
					}
					if status != "failed" || origin != "worker_lost" || reason != want {
						t.Fatalf("run %s %s %q want failed worker_lost %q", status, origin, reason, want)
					}
				})
			}
		}
	}
}

// Wait on the database's lock graph, bounded by 10 seconds and 10000 probes.
// Each subtest finishes its own sessions before the next independent case.
func rejectionWaitBlocked(t *testing.T, e *settleEnv, pid uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	for attempts := 0; attempts < 10000; attempts++ {
		var blocked bool
		if err := e.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
	}
	t.Fatal("no lock waiter within probe bound")
}

func TestTerminalRejectionMutationFirstRacesLiveDB(t *testing.T) {
	for _, operation := range []string{"generation", "completion", "discard", "release", "upload"} {
		t.Run(operation, func(t *testing.T) {
			e := newSettleEnv(t)
			e.wsvc.SetTxBeginner(e.pool)
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := store.New(tx)
			if _, err := q.GetRunByIDForUpdate(ctx, e.run); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", e.tokenA, fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":1,"reason":"mac_failure"}]}`, e.run))
			}()
			rejectionWaitBlocked(t, e, tx.Conn().PgConn().PID())
			switch operation {
			case "generation":
				_, err = tx.Exec(ctx, "UPDATE runs SET claim_generation=3,status='running' WHERE id=$1", e.run)
			case "completion":
				_, err = q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: e.run, WorkerID: pgconv.UUID(e.workerA)})
			case "discard":
				_, err = q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: e.pred, RunID: e.run, UserID: e.user, ReleaseEvidence: pgconv.TextOrNull("owner_discard")})
			case "release":
				_, err = q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: e.run, Generation: 1, WorkerID: e.workerA, ReleaseEvidence: pgconv.TextOrNull("forge_no_output")})
			case "upload":
				_, err = q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{RunID: e.run, UserID: e.user, OriginalWorkerID: pgconv.UUID(e.workerA), OriginalWorkerIdentity: "fixture", SourceSha: settleSource, IdempotencyKey: uuid.NewString(), Generation: 1})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			want := "recorded"
			if operation == "discard" || operation == "release" {
				want = "skipped"
			}
			assertRejectionDisposition(t, rec, e.run, 1, want)
			var annotation *string
			if err := e.pool.QueryRow(ctx, "SELECT terminal_record_rejection FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&annotation); err != nil {
				t.Fatal(err)
			}
			if want == "skipped" && annotation != nil {
				t.Fatalf("closed hold annotated: %v", annotation)
			}
			if want == "recorded" && (annotation == nil || *annotation != "mac_failure") {
				t.Fatalf("open hold missing annotation: %v", annotation)
			}
			var generation int64
			var status string
			if err := e.pool.QueryRow(ctx, "SELECT claim_generation,status FROM runs WHERE id=$1", e.run).Scan(&generation, &status); err != nil {
				t.Fatal(err)
			}
			if operation == "generation" && (generation != 3 || status != "running") {
				t.Fatalf("new assignment changed %d %s", generation, status)
			}
			if operation == "completion" && status != "completed" {
				t.Fatalf("completion changed %s", status)
			}
		})
	}
}

// Exposes the report transaction's backend so the test can prove the competing
// mutation waits on the report's run lock before releasing the hold barrier.
type rejectionObservedBeginner struct {
	e       *settleEnv
	started chan uint32
}

func (b rejectionObservedBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.e.pool.Begin(ctx)
	if err == nil {
		b.started <- tx.Conn().PgConn().PID()
	}
	return tx, err
}

func TestTerminalRejectionReportFirstRacesLiveDB(t *testing.T) {
	for _, operation := range []string{"generation", "completion", "discard", "release", "upload"} {
		t.Run(operation, func(t *testing.T) {
			e := newSettleEnv(t)
			ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
			defer cancel()
			barrier, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = barrier.Rollback(ctx) }()
			if _, err := barrier.Exec(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE", e.pred); err != nil {
				t.Fatal(err)
			}
			started := make(chan uint32, 1)
			e.wsvc.SetTxBeginner(rejectionObservedBeginner{e, started})
			report := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				report <- rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", e.tokenA, fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":1,"reason":"mac_failure"}]}`, e.run))
			}()
			var reportPID uint32
			select {
			case reportPID = <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			rejectionWaitBlocked(t, e, barrier.Conn().PgConn().PID())
			mutation := make(chan error, 1)
			go func() {
				tx, err := e.pool.Begin(ctx)
				if err != nil {
					mutation <- err
					return
				}
				defer func() { _ = tx.Rollback(ctx) }()
				q := store.New(tx)
				if _, err = q.GetRunByIDForUpdate(ctx, e.run); err == nil {
					switch operation {
					case "generation":
						_, err = tx.Exec(ctx, "UPDATE runs SET claim_generation=3,status='running' WHERE id=$1", e.run)
					case "completion":
						_, err = q.SetRunCompleted(ctx, store.SetRunCompletedParams{ID: e.run, WorkerID: pgconv.UUID(e.workerA)})
					case "discard":
						_, err = q.DiscardCustodyHoldForOwner(ctx, store.DiscardCustodyHoldForOwnerParams{HoldID: e.pred, RunID: e.run, UserID: e.user, ReleaseEvidence: pgconv.TextOrNull("owner_discard")})
					case "release":
						_, err = q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{RunID: e.run, Generation: 1, WorkerID: e.workerA, ReleaseEvidence: pgconv.TextOrNull("forge_no_output")})
					case "upload":
						_, err = q.ReserveCaptureExact(ctx, store.ReserveCaptureExactParams{RunID: e.run, UserID: e.user, OriginalWorkerID: pgconv.UUID(e.workerA), OriginalWorkerIdentity: "fixture", SourceSha: settleSource, IdempotencyKey: uuid.NewString(), Generation: 1})
					}
				}
				if err == nil {
					err = tx.Commit(ctx)
				}
				mutation <- err
			}()
			rejectionWaitBlocked(t, e, reportPID)
			if err := barrier.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-report:
				assertRejectionDisposition(t, rec, e.run, 1, "recorded")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var annotation string
			if err := e.pool.QueryRow(ctx, "SELECT terminal_record_rejection FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&annotation); err != nil {
				t.Fatal(err)
			}
			if annotation != "mac_failure" {
				t.Fatalf("lost annotation %s", annotation)
			}
		})
	}
}

func TestTerminalRejectionLiveSettleRacesLiveDB(t *testing.T) {
	for _, generation := range []int64{1, 2} {
		for _, first := range []string{"report", "settle"} {
			t.Run(fmt.Sprintf("generation_%d/%s_first", generation, first), func(t *testing.T) {
				le := newLiveEnv(t)
				e := le.settleEnv
				ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
				defer cancel()
				barrier, err := e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Rollback(ctx) }()
				barrierHold := e.pred
				if first == "report" && generation == 2 {
					barrierHold = e.sibGen
				}
				if _, err := barrier.Exec(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE", barrierHold); err != nil {
					t.Fatal(err)
				}
				started := make(chan uint32, 1)
				e.wsvc.SetTxBeginner(rejectionObservedBeginner{e, started})
				report := make(chan *httptest.ResponseRecorder, 1)
				type settleResult struct {
					code int
					res  apitypes.RecoverySettleResponse
					raw  string
				}
				settled := make(chan settleResult, 1)
				startReport := func() {
					go func() {
						report <- rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", e.tokenA, fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":%d,"reason":"mac_failure"}]}`, e.run, generation))
					}()
				}
				startSettle := func() {
					go func() {
						code, res, raw := le.settleLive(e.tokenA, e.run, e.pred, goodLiveBody(apitypes.RecoverySettleTargetCheckpoint))
						settled <- settleResult{code, res, raw}
					}()
				}
				if first == "report" {
					startReport()
				} else {
					startSettle()
				}
				rejectionWaitBlocked(t, e, barrier.Conn().PgConn().PID())
				if first == "report" {
					startSettle()
				} else {
					startReport()
				}
				var reportPID uint32
				select {
				case reportPID = <-started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				// The second operation must enter a real lock wait before the barrier opens.
				if first == "report" {
					rejectionWaitBlocked(t, e, reportPID)
				} else {
					for attempts := 0; attempts < 10000; attempts++ {
						var blocked bool
						if err := e.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", reportPID).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
						if blocked {
							break
						}
						if attempts == 9999 {
							t.Fatal("report did not wait on settle")
						}
					}
				}
				if err := barrier.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				want := "recorded"
				if first == "settle" && generation == 1 {
					want = "skipped"
				}
				select {
				case rec := <-report:
					assertRejectionDisposition(t, rec, e.run, generation, want)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case out := <-settled:
					le.assertReleased(out.code, out.res, out.raw)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				e.assertOpen(e.sibGen, e.sibWork)
				if le.runStatus() != "running" {
					t.Fatal("diagnostic report changed live run")
				}
			})
		}
	}
}
