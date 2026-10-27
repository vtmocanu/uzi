package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func assertAdmissionSpreadBound(t *testing.T, eligible pgtype.Timestamptz, expiry time.Time, widthMs int32) {
	t.Helper()
	if !eligible.Valid || eligible.Time.Before(expiry) {
		t.Fatalf("spread eligibility=%v before expiry=%v", eligible, expiry)
	}
	if widthMs == 0 {
		if !eligible.Time.Equal(expiry) {
			t.Fatalf("zero-width eligibility=%v, want %v", eligible, expiry)
		}
	} else if !eligible.Time.Before(expiry.Add(time.Duration(widthMs) * time.Millisecond)) {
		t.Fatalf("spread eligibility=%v outside persisted width=%dms from %v", eligible, widthMs, expiry)
	}
}

// Real Bearer -> route -> handler -> service -> SQL, before any M2 dependents.
// This uses the worker_codex_livedb_test.go fixture pattern without minting a
// refresh capability: the admission endpoint must not require one.
func TestWorkerCodexAdmissionPersistedLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
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
	svc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	svc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: svc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
	owner, connection, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, "admission-"+owner.String()+"@example.test")
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
        VALUES($1,$2,'gitlab','https://forge.example','bot',1,$3)`, connection, owner, []byte("fixture"))
	mustExecT(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
        VALUES($1,$2,1,$3,'https://forge.example/repo','main',true)`, repo, connection, "admission/"+repo.String())
	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, worker, owner, worker.String(), hash)
	secret, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: owner, Kind: store.KindCodexAuth, Label: "admission", WantDefault: true, Ciphertext: []byte("fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: owner, ProviderUserID: "principal-" + uuid.NewString(), WorkspaceAccountID: "workspace-" + uuid.NewString(),
		SealedLogin: []byte("fixture"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret.ID, UserID: owner, Status: "staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: owner, MaterialRevision: 0}); err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
        VALUES($1,$2,$3,'issue',1,'admission','fixture','claimed',$4,1)`, run, owner, repo, worker)
	if err = svc.FreezeCodexBinding(ctx, owner, run, secret.ID, "subscription"); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"claim_generation":1,"attempt_id":%q,"class":"rate_limit","provider_thread_id":"thread","provider_turn_id":"turn"}`, uuid.NewString())
	path := "/api/worker/runs/" + run.String() + "/codex/admission-signal"
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	t.Run("malformed frozen binding is refused without mutation", func(t *testing.T) {
		var validKey string
		if err := pool.QueryRow(ctx, "SELECT codex_account_key FROM runs WHERE id=$1", run).Scan(&validKey); err != nil {
			t.Fatal(err)
		}
		snapshot := func(t *testing.T) string {
			t.Helper()
			var state string
			// Include the full run (status, claim generation and release included) and
			// every owner throttle row, including receipts; absence must stay absence.
			if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
				'run', (SELECT to_jsonb(r) FROM runs r WHERE r.id=$1),
				'throttles', (SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY s.provider_account_id), '[]'::jsonb)
					FROM codex_account_admission_throttle s WHERE s.user_id=$2))::text`,
				run, owner).Scan(&state); err != nil {
				t.Fatal(err)
			}
			return state
		}
		for _, tc := range []struct {
			name string
			key  string
		}{
			{"empty", ""},
			{"invalid token", "not-json"},
			{"truncated array", `["principal"`},
			{"JSON null", "null"},
			{"JSON object", "{}"},
			{"JSON scalar", `"principal"`},
			{"nonmatching tuple", `["other-principal","other-workspace"]`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mustExecT(ctx, t, pool, "UPDATE runs SET codex_account_key=$2 WHERE id=$1", run, tc.key)
				t.Cleanup(func() {
					mustExecT(ctx, t, pool, "UPDATE runs SET codex_account_key=$2 WHERE id=$1", run, validKey)
				})
				before := snapshot(t)
				rec := post()
				if rec.Code != http.StatusNotFound {
					t.Errorf("malformed binding POST = %d %s, want 404", rec.Code, rec.Body.String())
				}
				if after := snapshot(t); after != before {
					t.Fatalf("refused admission mutated run or throttle state: before=%s after=%s", before, after)
				}
			})
		}
	})
	// Reuse the exact Bearer, run, generation and attempt after restoring the
	// valid frozen tuple: refusals must not consume this observation's identity.
	rec := post()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	state, err := q.GetCodexAdmissionState(ctx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	if state.RateStep != 0 || !state.RateExpiry.Valid || !state.EpisodeID.Valid {
		t.Fatalf("missing persisted transition: %+v", state)
	}
	duration := state.RateExpiry.Time.Sub(state.RateLastQualifying.Time)
	if duration < 30*time.Second || duration > 36*time.Second {
		t.Fatalf("server jitter duration=%s", duration)
	}
	before := string(state.Receipts)
	if rec = post(); rec.Code != http.StatusNoContent {
		t.Fatalf("duplicate = %d", rec.Code)
	}
	state, err = q.GetCodexAdmissionState(ctx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
	if err != nil || string(state.Receipts) != before {
		t.Fatalf("duplicate altered receipts: %v", err)
	}
	eligible, err := q.GetCodexAdmissionRunEligibility(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	assertAdmissionSpreadBound(t, eligible, state.EffectiveExpiry.Time, state.SpreadWidthMs)
	t.Run("service reconstruction preserves admission and duplicate receipt", func(t *testing.T) {
		// Ordinary service reconstruction, with its own pool and query context; the original
		// fixture stays open for the deterministic-spread and post-park checks below.
		restartedCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		restartedPool, err := store.OpenPool(restartedCtx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer restartedPool.Close()
		restartedQ := store.New(restartedPool)
		restartedSvc := workersvc.New(restartedQ, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
		restartedSvc.SetTxBeginner(restartedPool)
		restartedHandler := &Handler{q: restartedQ, wsvc: restartedSvc}
		restartedRouter := restartedHandler.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))
		assertPersisted := func() {
			t.Helper()
			got, err := restartedQ.GetCodexAdmissionState(restartedCtx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
			if err != nil {
				t.Fatal(err)
			}
			// Compare the entire persisted row: steps, expiries, episode, width, clocks,
			// votes and receipts must survive, and the duplicate must change none of them.
			if !reflect.DeepEqual(got, state) {
				t.Fatalf("service reconstruction/duplicate changed persisted admission: got %+v want %+v", got, state)
			}
			gotEligibility, err := restartedQ.GetCodexAdmissionRunEligibility(restartedCtx, run)
			if err != nil {
				t.Fatal(err)
			}
			if !gotEligibility.Valid || !gotEligibility.Time.Equal(eligible.Time) {
				t.Fatalf("reconstructed eligibility=%v, want %v", gotEligibility, eligible)
			}
		}
		assertPersisted()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		restartedRec := httptest.NewRecorder()
		restartedRouter.ServeHTTP(restartedRec, req)
		if restartedRec.Code != http.StatusNoContent {
			t.Fatalf("reconstructed duplicate = %d %s", restartedRec.Code, restartedRec.Body.String())
		}
		assertPersisted()
	})
	t.Run("deterministic spread vectors", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		// Fixed UUIDs exist only until this transaction rolls back. Copy the
		// fixture's owner-scoped binding; distinct issues avoid the active-run index.
		run9 := uuid.MustParse("00000000-0000-0000-0000-000000000009")
		run10 := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
		for i, id := range []uuid.UUID{run9, run10} {
			tag, err := tx.Exec(ctx, `INSERT INTO runs
				(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,
				 worker_id,claim_generation,harness,codex_secret_id,codex_auth_mode,
				 codex_secret_label,codex_material_revision,codex_account_key,codex_account_revision)
				SELECT $1,user_id,repo_id,kind,$2,issue_title,issue_description,status,
				 worker_id,claim_generation,harness,codex_secret_id,codex_auth_mode,
				 codex_secret_label,codex_material_revision,codex_account_key,codex_account_revision
				FROM runs WHERE id=$3`, id, 3530009+i, run)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 1 {
				t.Fatal("missing baseline run")
			}
		}
		txq := store.New(tx)
		for _, tc := range []struct {
			name    string
			run     uuid.UUID
			episode string
			width   int32
			offset  time.Duration
		}{
			{"above 30 seconds", run9, "00000000-0000-0000-0000-000000000001", 35000, 32607 * time.Millisecond},
			{"different run", run10, "00000000-0000-0000-0000-000000000001", 35000, 3491 * time.Millisecond},
			{"different episode", run9, "00000000-0000-0000-0000-000000000002", 35000, 21139 * time.Millisecond},
			{"zero width", run9, "00000000-0000-0000-0000-000000000001", 0, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := tx.Exec(ctx, `UPDATE codex_account_admission_throttle
					SET episode_id=$3,spread_width_ms=$4 WHERE user_id=$1 AND provider_account_id=$2`,
					owner, account.ID, uuid.MustParse(tc.episode), tc.width); err != nil {
					t.Fatal(err)
				}
				persisted, err := txq.GetCodexAdmissionState(ctx, store.GetCodexAdmissionStateParams{UserID: owner, ProviderAccountID: account.ID})
				if err != nil {
					t.Fatal(err)
				}
				// Query the actual codex_admission_run_eligible_at twice for repeatability.
				for repeat := 0; repeat < 2; repeat++ {
					got, err := txq.GetCodexAdmissionRunEligibility(ctx, tc.run)
					if err != nil {
						t.Fatal(err)
					}
					assertAdmissionSpreadBound(t, got, persisted.EffectiveExpiry.Time, persisted.SpreadWidthMs)
					if want := persisted.EffectiveExpiry.Time.Add(tc.offset); !got.Time.Equal(want) {
						t.Fatalf("eligibility=%v, want %v (offset %s)", got, want, tc.offset)
					}
				}
			})
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	})
	mustExecT(ctx, t, pool, "UPDATE runs SET status='recovery_wait',claim_released_at=now() WHERE id=$1", run)
	if rec = post(); rec.Code != http.StatusNotFound {
		t.Fatalf("post-park = %d %s", rec.Code, rec.Body.String())
	}
}
