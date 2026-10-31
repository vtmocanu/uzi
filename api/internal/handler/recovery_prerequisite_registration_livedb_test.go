package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type prerequisiteRegistrationEnv struct {
	e      *recoveryEnv
	h      *Handler
	router http.Handler
	token  string
}

func newPrerequisiteRegistrationEnv(t *testing.T) *prerequisiteRegistrationEnv {
	t.Helper()
	// Both constructors must share one isolated DSN; their pools close before drop.
	ephemeralPriorityDatabase(t)
	e := newRecoveryEnvGuarded(t, true)
	h, router, pool := cliLiveDB(t)
	assertPriorityDatabase(t, e.ctx, pool, e.pool.Config().ConnConfig.Database)
	h.box = e.box
	h.cfg = recoveryTestCfg()
	h.cfg.PrerequisiteClosureForges = []string{"github"}
	token := uuid.NewString()
	cliMustExec(t, e.pool, "UPDATE workers SET token_hash=$2 WHERE id=$1", e.worker.ID, jointoken.Hash(token))
	cliMustExec(t, e.pool, "UPDATE forge_connections SET base_url='https://github.com' WHERE user_id=$1", e.user)
	// Registration must not orphan-recover the fixture's active run.
	cliMustExec(t, e.pool, "UPDATE runs SET status='failed',claim_generation=1 WHERE id=$1", e.run)
	return &prerequisiteRegistrationEnv{e: e, h: h, router: router, token: token}
}

func (f *prerequisiteRegistrationEnv) register(t *testing.T, enabled bool) {
	t.Helper()
	caps := []string{capability.RecoveryInventoryV1, "unknown_future_protocol"}
	want := []string{capability.RecoveryInventoryV1}
	if enabled {
		caps = append(caps, capability.RecoveryPrerequisiteClosureV1)
		want = append(want, capability.RecoveryPrerequisiteClosureV1)
	}
	body, err := json.Marshal(map[string]any{"protocol_capabilities": caps})
	if err != nil {
		t.Fatal(err)
	}
	rr := bearerReqBody(f.router, http.MethodPost, "/api/worker/register", f.token, string(body))
	if rr.Code != http.StatusOK {
		t.Fatalf("registration: %d %s", rr.Code, rr.Body.String())
	}
	w, err := f.h.q.GetWorkerByID(f.e.ctx, f.e.worker.ID)
	if err != nil || !reflect.DeepEqual(w.ProtocolCapabilities, want) {
		t.Fatalf("stored protocols=%v want=%v err=%v", w.ProtocolCapabilities, want, err)
	}
}

func (f *prerequisiteRegistrationEnv) reserve(t *testing.T) string {
	t.Helper()
	generation := int64(1)
	res, err := f.h.recovery().Reserve(f.e.ctx, f.e.worker, f.e.run, apitypes.RecoveryReserveRequest{Generation: &generation, IdempotencyKey: uuid.NewString(), SourceSha: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	return res.CaptureID
}
func (f *prerequisiteRegistrationEnv) upload(router http.Handler, capture, token string) *httptest.ResponseRecorder {
	body := []byte("synthetic thin archive")
	manifest := manifestFor(body)
	manifest.PrerequisiteShas = []string{strings.Repeat("b", 40)}
	raw, _ := json.Marshal(manifest)
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+f.e.run.String()+"/archives/"+capture+"/upload", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(recoveryManifestHeader, string(raw))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}
func (f *prerequisiteRegistrationEnv) assertRows(t *testing.T, capture string, want int) {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT count(*) FROM recovery_prerequisite_closures WHERE capture_id=$1", capture).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != want {
		t.Fatalf("capture %s closure rows=%d want=%d", capture, n, want)
	}
	if want == 1 {
		row, err := f.h.q.GetPrerequisiteClosure(f.e.ctx, store.GetPrerequisiteClosureParams{UserID: f.e.user, CaptureID: uuid.MustParse(capture)})
		if err != nil || row.State != "pending" || row.LiveRunID.Valid || row.InvokedAt.Valid || row.Attempts != 0 {
			t.Fatalf("S0=%+v err=%v", row, err)
		}
	}
}
func TestRecoveryPrerequisiteRegistrationFixtureIsolationLiveDB(t *testing.T) {
	assertPrerequisiteFixtureIsolation(t, func(t *testing.T) *pgxpool.Pool {
		return newPrerequisiteRegistrationEnv(t).e.pool
	})
}

func TestRecoveryPrerequisiteRegistrationBearerLiveDB(t *testing.T) {
	f := newPrerequisiteRegistrationEnv(t)
	f.register(t, false)
	old := f.reserve(t)
	if rr := f.upload(f.router, old, f.token); rr.Code != http.StatusOK {
		t.Fatalf("old upload: %d %s", rr.Code, rr.Body.String())
	}
	f.assertRows(t, old, 0)
	f.register(t, true)
	// A lost receipt and status retry cannot backfill an already accepted archive.
	if rr := f.upload(f.router, old, f.token); rr.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	rr := bearerReq(f.router, http.MethodGet, "/api/worker/runs/"+f.e.run.String()+"/archives/"+old, f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
	}
	f.assertRows(t, old, 0)
	fresh := f.reserve(t)
	foreign := uuid.NewString()
	foreignID := uuid.New()
	cliMustExec(t, f.e.pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'foreign',$3,'online',$4)", foreignID, f.e.user, jointoken.Hash(foreign), []string{capability.RecoveryPrerequisiteClosureV1})
	if rr := f.upload(f.router, fresh, foreign); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign upload: %d %s", rr.Code, rr.Body.String())
	}
	f.assertRows(t, fresh, 0)
	if rr := f.upload(f.router, fresh, f.token); rr.Code != http.StatusOK {
		t.Fatalf("fresh upload: %d %s", rr.Code, rr.Body.String())
	}
	f.assertRows(t, fresh, 1)
}

func TestRecoveryPrerequisiteAuthenticatedSnapshotLiveDB(t *testing.T) {
	for _, initial := range []bool{false, true} {
		name := "add"
		if initial {
			name = "remove"
		}
		t.Run(name, func(t *testing.T) {
			f := newPrerequisiteRegistrationEnv(t)
			f.register(t, initial)
			capture := f.reserve(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			// This barrier is downstream of real bearer authentication and upstream of Upload.
			r := chi.NewRouter()
			r.Use(mw.RequireWorker(f.h.q))
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					close(entered)
					<-resume
					next.ServeHTTP(w, req)
				})
			})
			r.Post("/api/worker/runs/{id}/archives/{captureID}/upload", f.h.WorkerRecoveryUpload)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.upload(r, capture, f.token) }()
			finished := false
			defer func() {
				select {
				case <-resume:
				default:
					close(resume)
				}
				if !finished {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("upload cleanup did not finish")
					}
				}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("authentication barrier not reached")
			}
			f.register(t, !initial)
			close(resume)
			select {
			case rr := <-done:
				finished = true
				if rr.Code != http.StatusOK {
					t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("upload did not finish")
			}
			want := 0
			if initial {
				want = 1
			}
			f.assertRows(t, capture, want)
		})
	}
}
