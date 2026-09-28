package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #1423: the judge review and task-review advice POSTs carry claim_generation and map
// the service's claim-fence refusals to 409s. adviceFakeStore authorizes one advice run of
// each kind and records what the fenced upsert received; upsertErr simulates the fence
// (pgx.ErrNoRows = the statement persisted nothing).
type adviceFakeStore struct {
	workersvc.Store
	owner, adviceRunID, targetID uuid.UUID
	upsertErr                    error
	reviewParams                 *store.UpsertRunReviewWithRecommendationsParams
	taskParams                   *store.UpsertTaskReviewWithFindingsParams
}

func (f *adviceFakeStore) GetActiveJudgeRunForWorkerTarget(context.Context, store.GetActiveJudgeRunForWorkerTargetParams) (store.Run, error) {
	return store.Run{ID: f.adviceRunID, UserID: f.owner, Kind: runkind.Judge}, nil
}

func (f *adviceFakeStore) GetActiveTaskReviewRunForWorkerTarget(context.Context, store.GetActiveTaskReviewRunForWorkerTargetParams) (store.Run, error) {
	return store.Run{ID: f.adviceRunID, UserID: f.owner, Kind: runkind.Task}, nil
}

func (f *adviceFakeStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	if id != f.targetID {
		return store.Run{}, pgx.ErrNoRows
	}
	return store.Run{ID: f.targetID, UserID: f.owner, Status: "completed"}, nil
}

func (f *adviceFakeStore) UpsertRunReviewWithRecommendations(_ context.Context, arg store.UpsertRunReviewWithRecommendationsParams) (uuid.UUID, error) {
	f.reviewParams = &arg
	if f.upsertErr != nil {
		return uuid.UUID{}, f.upsertErr
	}
	return uuid.New(), nil
}

func (f *adviceFakeStore) UpsertTaskReviewWithFindings(_ context.Context, arg store.UpsertTaskReviewWithFindingsParams) (uuid.UUID, error) {
	f.taskParams = &arg
	if f.upsertErr != nil {
		return uuid.UUID{}, f.upsertErr
	}
	return uuid.New(), nil
}

func adviceRouter(st *adviceFakeStore, wkr store.Worker) http.Handler {
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), wkr)))
		})
	})
	r.Post("/api/worker/runs/{id}/review", h.WorkerRunReview)
	r.Post("/api/worker/runs/{id}/task-review", h.WorkerTaskReview)
	return r
}

func advicePost(router http.Handler, target uuid.UUID, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+target.String()+"/"+path, strings.NewReader(body)))
	return rec
}

func adviceDisposition(rec *httptest.ResponseRecorder) string {
	var body struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Disposition
}

func newAdviceFakeStore() *adviceFakeStore {
	return &adviceFakeStore{owner: uuid.New(), adviceRunID: uuid.New(), targetID: uuid.New()}
}

var adviceCases = []struct {
	name, path, body string
}{
	{"judge review", "review", `{"verdict":"ok","status":"complete"%s}`},
	{"task review", "task-review", `{"status":"complete"%s}`},
}

func adviceBody(tmpl, extra string) string { return strings.Replace(tmpl, "%s", extra, 1) }

func TestAdvicePostDecodesClaimGeneration(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, `,"claim_generation":11,"advice_run_id":"`+st.adviceRunID.String()+`"`))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
			}
			var gotGen int64
			var gotValid bool
			if st.reviewParams != nil {
				gotGen, gotValid = st.reviewParams.ClaimGeneration.Int64, st.reviewParams.ClaimGeneration.Valid
			} else if st.taskParams != nil {
				gotGen, gotValid = st.taskParams.ClaimGeneration.Int64, st.taskParams.ClaimGeneration.Valid
			} else {
				t.Fatal("no upsert reached the store")
			}
			if !gotValid || gotGen != 11 {
				t.Fatalf("upsert claim_generation = %d (valid=%v), want 11", gotGen, gotValid)
			}
		})
	}
}

func TestAdvicePostStaleClaimIs409Disposition(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAdviceFakeStore()
			st.upsertErr = pgx.ErrNoRows // the fence persisted nothing
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, `,"claim_generation":3,"advice_run_id":"`+st.adviceRunID.String()+`"`))
			if rec.Code != http.StatusConflict || adviceDisposition(rec) != "stale_claim" {
				t.Fatalf("stale post = %d %s, want 409 disposition stale_claim", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdvicePostMissingGenerationIs409(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, ""))
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "claim_generation") {
				t.Fatalf("unstamped capability post = %d %s, want 409 naming claim_generation", rec.Code, rec.Body.String())
			}
			if adviceDisposition(rec) != "" {
				t.Fatalf("missing-generation refusal must not carry a stale_claim disposition: %s", rec.Body.String())
			}
			if st.reviewParams != nil || st.taskParams != nil {
				t.Fatal("an unstamped capability post must write nothing")
			}
		})
	}
}

// A capability worker that stamps claim_generation but omits advice_run_id is refused with the
// byte-identical response a missing generation gets (same 409, same error, no disposition) and
// nothing written: without the run id a stale post of an ended advice run could resolve to the
// active run of the same target at a colliding per-run generation.
func TestAdvicePostMissingAdviceRunIDIsMissingGeneration(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name, func(t *testing.T) {
			wantSt := newAdviceFakeStore()
			wantWkr := store.Worker{ID: uuid.New(), UserID: wantSt.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			want := advicePost(adviceRouter(wantSt, wantWkr), wantSt.targetID, tc.path, adviceBody(tc.body, ""))

			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, `,"claim_generation":1`))
			if rec.Code != http.StatusConflict || rec.Code != want.Code || rec.Body.String() != want.Body.String() {
				t.Fatalf("run-id-less capability post = %d %s, want the missing-generation response %d %s",
					rec.Code, rec.Body.String(), want.Code, want.Body.String())
			}
			if st.reviewParams != nil || st.taskParams != nil {
				t.Fatal("a capability post without advice_run_id must write nothing")
			}
		})
	}
}

func TestAdvicePostLegacyWorkerUnstamped(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("legacy unstamped post = %d %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}

// Issue #1423 advice_run_id: a flight stamps the advice run it holds; a different run's id (a
// stale flight of an earlier judge/review run whose generation collides) is a 409 stale_claim
// with nothing written, a non-uuid is a 400, and the authorized run's id lands.
func TestAdvicePostAdviceRunID(t *testing.T) {
	for _, tc := range adviceCases {
		t.Run(tc.name+"/mismatch is 409 stale_claim", func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			extra := `,"claim_generation":1,"advice_run_id":"` + uuid.NewString() + `"`
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, extra))
			if rec.Code != http.StatusConflict || adviceDisposition(rec) != "stale_claim" {
				t.Fatalf("mismatched advice_run_id = %d %s, want 409 disposition stale_claim", rec.Code, rec.Body.String())
			}
			if st.reviewParams != nil || st.taskParams != nil {
				t.Fatal("a mismatched advice_run_id must write nothing")
			}
		})
		t.Run(tc.name+"/malformed is 400", func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path,
				adviceBody(tc.body, `,"claim_generation":1,"advice_run_id":"not-a-uuid"`))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "advice_run_id") {
				t.Fatalf("malformed advice_run_id = %d %s, want 400 naming advice_run_id", rec.Code, rec.Body.String())
			}
			if st.reviewParams != nil || st.taskParams != nil {
				t.Fatal("a malformed advice_run_id must write nothing")
			}
		})
		t.Run(tc.name+"/matching id lands", func(t *testing.T) {
			st := newAdviceFakeStore()
			wkr := store.Worker{ID: uuid.New(), UserID: st.owner, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
			extra := `,"claim_generation":1,"advice_run_id":"` + st.adviceRunID.String() + `"`
			rec := advicePost(adviceRouter(st, wkr), st.targetID, tc.path, adviceBody(tc.body, extra))
			if rec.Code != http.StatusOK {
				t.Fatalf("matching advice_run_id = %d %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}
