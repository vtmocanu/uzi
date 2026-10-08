package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func recoveryResumeRequest(h *Handler, id, userID uuid.UUID) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/resume-now", nil)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(mw.ContextWithUser(ctx, store.User{ID: userID}), chi.RouteCtxKey, route))
	rec := httptest.NewRecorder()
	h.ResumeRunNow(rec, req)
	return rec
}

// The actual owner endpoint must feed the ordinary claim lane, without resetting
// lifetime loss history. Each finite episode attempts exactly limit automatic
// requeues; a refusal blocks the next claim and requires another owner decision.
func TestWorkerRecoveryResumeClaimEpisodesLiveDB(t *testing.T) {
	for _, limit := range []int32{0, 1, 3} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			h := &Handler{q: fx.q}
			h.cfg.RunMaxRequeues = int(limit)
			oldWorker := fx.onlineWorker("old incarnation", true)
			worker := fx.onlineWorker("new incarnation", true)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=ANY($1::uuid[])", []uuid.UUID{oldWorker, worker}, []string{capability.RecoveryArchiveV1})
			id := fx.queuedRun([]string{})
			tip := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			oldHold := uuid.New()
			exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=2,requeue_count=$3,started_at=now()-interval '1 hour',budget_wall_seconds=86400,checkpoint_tip=$4,checkpoint_tip_at=now(),iteration_count=4 WHERE id=$1", id, oldWorker, limit, tip)
			exec(`INSERT INTO recovery_custody_holds
                (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
                VALUES ($1,$2,$3,$4,2,'open',$5,'old incarnation',$5,$4)`, oldHold, fx.userID, fx.repoID, id, oldWorker)
			park := func(w uuid.UUID) {
				t.Helper()
				rows, err := fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{
					WorkerID: pgtype.UUID{Bytes: w, Valid: true}, MaxRequeues: limit,
					FailureReason: pgtype.Text{String: "worker requeue budget exhausted", Valid: true},
				})
				if err != nil || len(rows) != 1 {
					t.Fatalf("park rows=%+v err=%v", rows, err)
				}
				row, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil || row.Status != "recovery_wait" || row.RecoveryWaitCause.String != "worker_requeue_exhausted" || !row.ClaimReleasedAt.Valid {
					t.Fatalf("park row=%+v err=%v", row, err)
				}
			}
			claim := func(claimWorker uuid.UUID, expectBlocked bool) store.Run {
				t.Helper()
				tx, err := fx.pool.Begin(fx.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(fx.ctx) }()
				q := fx.q.WithTx(tx)
				claimant, err := q.GetWorkerForUpdate(fx.ctx, claimWorker)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				row, err := q.ClaimRun(fx.ctx, store.ClaimRunParams{
					WorkerID: pgtype.UUID{Bytes: claimWorker, Valid: true}, UserID: fx.userID,
					AffinityCutoff:  pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
					SpreadCutoff:    pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
					HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
					IsDockerWorker:  true, DockerRepoAllowlist: []uuid.UUID{fx.repoID},
					RecoveryCapable: true, WorkerIdentity: "new incarnation",
					WorkerProtocolCaps: claimant.ProtocolCapabilities,
				})
				if expectBlocked {
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("released incarnation claimed run: id=%s err=%v", row.ID, err)
					}
					if err := tx.Commit(fx.ctx); err != nil {
						t.Fatal(err)
					}
					return store.Run{}
				}
				if err != nil || row.ID != id {
					t.Fatalf("normal ClaimRun id=%s err=%v", row.ID, err)
				}
				if err := tx.Commit(fx.ctx); err != nil {
					t.Fatal(err)
				}
				claimed, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil {
					t.Fatal(err)
				}
				return claimed
			}
			fence := func(w uuid.UUID, generation int64, want bool) {
				t.Helper()
				live, err := fx.q.RunUsageFenceLiveLocked(fx.ctx, store.RunUsageFenceLiveLockedParams{
					RunID: id, WorkerID: pgtype.UUID{Bytes: w, Valid: true}, ClaimGeneration: pgtype.Int8{Int64: generation, Valid: true},
				})
				if err != nil || live != want {
					t.Fatalf("claim fence worker=%s generation=%d live=%v err=%v want=%v", w, generation, live, err, want)
				}
			}
			park(oldWorker)
			original, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
			if err != nil {
				t.Fatal(err)
			}
			generation := int64(2)
			lifetime := limit
			// Three owner episodes, each bounded to limit automatic requeues.
			for episode := int64(1); episode <= 3; episode++ {
				foreign := recoveryResumeRequest(h, id, uuid.New())
				if foreign.Code != http.StatusNotFound {
					t.Fatalf("foreign code=%d", foreign.Code)
				}
				unchanged, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil || unchanged.WorkerRecoveryEpisode != episode-1 {
					t.Fatalf("foreign changed episode: %+v err=%v", unchanged, err)
				}
				exec("UPDATE runs SET status_since=now()-interval '2 minutes' WHERE id=$1", id)
				heldBeforeResume, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil {
					t.Fatal(err)
				}
				rec := recoveryResumeRequest(h, id, fx.userID)
				if rec.Code != http.StatusOK {
					t.Fatalf("resume code=%d body=%s", rec.Code, rec.Body.String())
				}
				var body struct {
					Run apitypes.RunDTO `json:"run"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				wr := body.Run.WorkerRecovery
				if wr == nil || wr.Episode != episode || wr.AutomaticRequeueLimit != int(limit) || wr.EpisodeUsed != 0 || wr.EpisodeRemaining != int(limit) || body.Run.RequeueCount != lifetime {
					t.Fatalf("resume DTO=%+v lifetime=%d", wr, body.Run.RequeueCount)
				}
				resumed, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil || resumed.Status != "queued" || resumed.RequeueEpisodeBaseline != lifetime || resumed.RecoveryWaitCause.Valid ||
					len(resumed.WorkerRecoveryEvidence) != 0 || resumed.StaleRequeueGeneration.Valid || !resumed.ClaimReleasedAt.Valid ||
					resumed.ClaimGeneration != generation || resumed.StartedAt != original.StartedAt || resumed.BudgetWallSeconds != original.BudgetWallSeconds ||
					resumed.CheckpointTip.String != tip || resumed.IterationCount != 4 || (resumed.BudgetPausedSeconds-heldBeforeResume.BudgetPausedSeconds < 120 || resumed.BudgetPausedSeconds-heldBeforeResume.BudgetPausedSeconds > 125) {
					t.Fatalf("resume invariants row=%+v err=%v", resumed, err)
				}
				var holdState, holdIdentity string
				var holdWorker uuid.UUID
				var holds int
				if err := fx.pool.QueryRow(fx.ctx, "SELECT state,original_worker_id,original_worker_identity FROM recovery_custody_holds WHERE id=$1", oldHold).Scan(&holdState, &holdWorker, &holdIdentity); err != nil {
					t.Fatal(err)
				}
				if holdState != "open" || holdWorker != oldWorker || holdIdentity != "old incarnation" {
					t.Fatalf("original custody changed: %s/%s/%s", holdState, holdWorker, holdIdentity)
				}
				if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", id).Scan(&holds); err != nil {
					t.Fatal(err)
				}
				if holds != 1+int(episode-1)*int(limit+1) {
					t.Fatalf("resume reset custody history: holds=%d episode=%d", holds, episode)
				}
				fence(oldWorker, 2, false)
				if episode == 1 {
					claim(oldWorker, true)
				} else {
					// Resume does not waive the released-incarnation fence. Model a
					// new registration only after proving the old incarnation is barred.
					claim(worker, true)
					exec("UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", worker, fmt.Sprintf("episode-%d", episode))
				}
				claimed := claim(worker, false)
				generation++
				if claimed.ClaimGeneration != generation || claimed.ClaimReleasedAt.Valid {
					t.Fatalf("claim generation/release=%d/%v", claimed.ClaimGeneration, claimed.ClaimReleasedAt.Valid)
				}
				fence(oldWorker, generation, false)
				fence(worker, generation-1, false)
				fence(worker, generation, true)
				for used := int32(0); used < limit; used++ {
					// Model execution loss after the real normal claim, then use the real automatic writer.
					exec("UPDATE runs SET status='running' WHERE id=$1", id)
					rows, err := fx.q.RequeueWorkerRuns(fx.ctx, store.RequeueWorkerRunsParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: limit})
					if err != nil || len(rows) != 1 || rows[0] != id {
						t.Fatalf("automatic requeue used=%d rows=%v err=%v", used, rows, err)
					}
					lifetime++
					claimed = claim(worker, false)
					generation++
					if claimed.RequeueCount != lifetime || claimed.ClaimGeneration != generation {
						t.Fatalf("automatic history=%d/%d generation=%d/%d", claimed.RequeueCount, lifetime, claimed.ClaimGeneration, generation)
					}
				}
				exec("UPDATE runs SET status='running' WHERE id=$1", id)
				rows, err := fx.q.RequeueWorkerRuns(fx.ctx, store.RequeueWorkerRunsParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: limit})
				if err != nil || len(rows) != 0 {
					t.Fatalf("episode exceeded allowance rows=%v err=%v", rows, err)
				}
				park(worker)
				held, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				if err != nil || held.RequeueCount != lifetime || held.WorkerRecoveryEpisode != episode || held.IterationCount != 4 || held.CheckpointTip.String != tip {
					t.Fatalf("held history=%+v err=%v", held, err)
				}
				var audits int
				if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", id).Scan(&audits); err != nil {
					t.Fatal(err)
				}
				if audits != int(episode) {
					t.Fatalf("audit history=%d want=%d", audits, episode)
				}
			}
		})
	}
}

func TestWorkerRecoveryResumeGuardsLiveDB(t *testing.T) {
	for _, budget := range []int{1, 86400} {
		t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
			fx := newEphemeralFixture(t, false)
			h := &Handler{q: fx.q}
			worker := fx.onlineWorker("guard", true)
			id := fx.queuedRun([]string{})
			if _, err := fx.pool.Exec(fx.ctx, "UPDATE runs SET status='running',worker_id=$2,requeue_count=1,checkpoint_tip=$3,checkpoint_tip_at=now(),started_at=now()-interval '1 hour',budget_wall_seconds=$4 WHERE id=$1", id, worker, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", budget); err != nil {
				t.Fatal(err)
			}
			if rows, err := fx.q.FailWorkerRunsOverCap(fx.ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, MaxRequeues: 1}); err != nil || len(rows) != 1 {
				t.Fatalf("park rows=%v err=%v", rows, err)
			}
			before, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
			if err != nil {
				t.Fatal(err)
			}
			if budget == 1 {
				rec := recoveryResumeRequest(h, id, fx.userID)
				after, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if rec.Code != http.StatusConflict || err != nil || string(a) != string(b) {
					t.Fatalf("budget refusal code=%d err=%v changed=%v", rec.Code, err, string(a) != string(b))
				}
				return
			}
			start := make(chan struct{})
			results := make(chan int, 2)
			var wg sync.WaitGroup
			// Exactly two contenders, both joined before inspecting the one-winner result.
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); <-start; results <- recoveryResumeRequest(h, id, fx.userID).Code }()
			}
			close(start)
			wg.Wait()
			close(results)
			codes := map[int]int{}
			for code := range results {
				codes[code]++
			}
			if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 1 {
				t.Fatalf("concurrent codes=%v", codes)
			}
			after, err := fx.q.GetRunByIDForUser(fx.ctx, store.GetRunByIDForUserParams{ID: id, UserID: fx.userID})
			if err != nil || after.WorkerRecoveryEpisode != 1 || after.RequeueEpisodeBaseline != before.RequeueCount {
				t.Fatalf("concurrent row=%+v err=%v", after, err)
			}
			var audits int
			if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", id).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if audits != 1 {
				t.Fatalf("concurrent audits=%d", audits)
			}
		})
	}
}
