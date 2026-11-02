package handler

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func exactHoldRoute(h *Handler, worker store.Worker, run uuid.UUID, query string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Get("/runs/{id}/recovery-holds", func(w http.ResponseWriter, req *http.Request) {
		h.WorkerListRecoveryHolds(w, req.WithContext(mw.ContextWithWorker(req.Context(), worker)))
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/runs/"+run.String()+"/recovery-holds"+query, nil))
	return rec
}

// Invalid generation avoids the database while testing the negotiation wire contract.
func TestExactRecoveryObservationGuaranteeRequiresRegistration(t *testing.T) {
	for _, tc := range []struct{ capability, guarantee string }{
		{"recovery_bindingless_closed_v1", "recovery_bindingless_closed_v1"},
		{"recovery_held_publication_v2", ""},
	} {
		worker := store.Worker{ProtocolCapabilities: []string{tc.capability}}
		rec := exactHoldRoute(&Handler{}, worker, uuid.New(), "?generation=0")
		if rec.Code != 200 || rec.Header().Get("Uzi-Recovery-Observation") != tc.guarantee {
			t.Fatalf("capability=%s status=%d guarantee=%q", tc.capability, rec.Code, rec.Header().Get("Uzi-Recovery-Observation"))
		}
	}
}

func TestExactRecoveryHoldRouteLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	worker := e.worker
	worker.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1, "recovery_held_publication_v2", capability.RecoveryInventoryV1}
	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE runs SET worker_id=$2, claim_generation=1 WHERE id=$1", e.run, worker.ID)
	observe := func(w store.Worker, query string) map[string]any {
		t.Helper()
		rec := exactHoldRoute(h, w, e.run, query)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := observe(worker, ""); body["exact_hold"] != nil {
		t.Fatalf("default widened: %v", body)
	}
	body := observe(worker, "?generation=1")
	exact, ok := body["exact_hold"].(map[string]any)
	if !ok || exact["hold_id"] != e.holdID.String() || exact["repo_id"] != e.repo.String() || exact["state"] != "open" || exact["current_claim_generation"] != float64(1) {
		t.Fatalf("exact guarded open observation missing: %v", body)
	}
	foreign := worker
	foreign.ID = uuid.New()
	foreignUser := worker
	foreignUser.UserID = uuid.New()
	lane := worker
	lane.IsolatedLane = true
	for _, w := range []store.Worker{foreign, foreignUser, lane} {
		if body := observe(w, "?generation=1"); body["exact_hold"] != nil {
			t.Fatalf("foreign scope disclosed: %v", body)
		}
	}
	for _, query := range []string{"?generation=0", "?generation=-1", "?generation=x", "?generation=2", "?generation=1&generation=2"} {
		if body := observe(worker, query); body["exact_hold"] != nil {
			t.Fatalf("invalid/missing generation: %v", body)
		}
	}
	v1 := worker
	v1.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
	if body := observe(v1, "?generation=1"); body["exact_hold"] != nil || len(body["holds"].([]any)) != 1 {
		t.Fatalf("V1 default changed: %v", body)
	}
	exec("UPDATE runs SET worker_id=NULL, claim_generation=2, status='failed' WHERE id=$1", e.run)
	if body := observe(worker, "?generation=1"); body["exact_hold"] == nil {
		t.Fatalf("original custody lost: %v", body)
	}
	exec("UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL, release_evidence='owner_discard', released_at=now() WHERE id=$1", e.holdID)
	body = observe(worker, "?generation=1")
	exact, ok = body["exact_hold"].(map[string]any)
	if !ok || exact["state"] != "discarded" || exact["release_evidence"] != "owner_discard" || exact["final_receipt"] != nil {
		t.Fatalf("closed observation fabricated FINAL: %v", body)
	}
	if body := observe(worker, ""); len(body["holds"].([]any)) != 0 {
		t.Fatalf("default closed list = %v", body)
	}
}

// Saved FINAL is observation only, even with the publication write feature switched off.
func TestExactRecoveryHoldFinalReceiptRouteLiveDB(t *testing.T) {
	e := newRecoveryEnvGuarded(t, true)
	worker := e.worker
	worker.ProtocolCapabilities = []string{"recovery_held_publication_v2"}
	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	if _, err := e.pool.Exec(e.ctx, "UPDATE runs SET worker_id=$2, claim_generation=1, status='failed' WHERE id=$1", e.run, worker.ID); err != nil {
		t.Fatal(err)
	}
	q := store.New(e.pool)
	pub, err := q.UpsertHeldPublication(e.ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: 1, HoldID: e.holdID, UserID: e.user, RepoID: e.repo, WorkerID: worker.ID,
		Ref: "refs/uzi-held/" + e.run.String() + "/1", Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.MarkHeldCreateInvoked(e.ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = q.RecordHeldCreateOutcome(e.ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: pub.ID}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ReleaseHeldPublicationHold(e.ctx, store.ReleaseHeldPublicationHoldParams{
		ID: e.holdID, RunID: e.run, UserID: e.user, Generation: 1, WorkerID: worker.ID, PublicationID: pub.ID, Tip: heldRouteTip, CoverageDigest: heldRouteDigest,
	}); err != nil || n != 1 {
		t.Fatalf("release=%d %v", n, err)
	}
	rec := exactHoldRoute(h, worker, e.run, "?generation=1")
	var body struct {
		ExactHold *struct {
			State           string `json:"state"`
			ReleaseEvidence string `json:"release_evidence"`
			FinalReceipt    *struct {
				PublicationID string `json:"publication_id"`
			} `json:"final_receipt"`
		} `json:"exact_hold"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || body.ExactHold == nil || body.ExactHold.State != "released" || body.ExactHold.ReleaseEvidence != "held_publication" || body.ExactHold.FinalReceipt == nil || body.ExactHold.FinalReceipt.PublicationID != pub.ID.String() {
		t.Fatalf("saved FINAL missing: %d %s", rec.Code, rec.Body)
	}
	var publicationState string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM run_held_publications WHERE id=$1", pub.ID).Scan(&publicationState); err != nil {
		t.Fatal(err)
	}
	if publicationState != "created" {
		t.Fatalf("observation acknowledged publication: %s", publicationState)
	}
}

// exactFixtureMutation models corrupt legacy rows without changing production admission.
// A transaction restores all user triggers even when a fixture assertion fails.
func exactFixtureMutation(t *testing.T, e *recoveryEnv, sql, check string, args ...any) {
	t.Helper()
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err = tx.Exec(e.ctx, "ALTER TABLE recovery_custody_holds DISABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(e.ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("fixture mutation rows=%d err=%v", tag.RowsAffected(), err)
	}
	var valid bool
	if err = tx.QueryRow(e.ctx, check, args...).Scan(&valid); err != nil || !valid {
		t.Fatalf("fixture readback=%t err=%v", valid, err)
	}
	if _, err = tx.Exec(e.ctx, "ALTER TABLE recovery_custody_holds ENABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExactRecoveryRawCandidatesLiveDB(t *testing.T) {
	// Corrupt legacy rows must never escape into the shared store-it database.
	ephemeralPriorityDatabase(t)
	cases := []struct {
		name, set, check string
		sibling, known   bool
	}{
		{"one closed", "", "", false, true},
		{"one open", "state='open',live_worker_id=$5,live_run_id=$2,release_evidence=NULL,released_at=NULL", "state='open'", false, true},
		{"missing run history", "", "", false, false},
		{"zero", "run_id=gen_random_uuid()", "run_id<>$2", false, false},
		{"wrong owner", "user_id=gen_random_uuid()", "user_id<>$3", false, false},
		{"wrong repo", "repo_id=gen_random_uuid()", "repo_id<>$4", false, false},
		{"missing repo", "repo_id=NULL", "repo_id IS NULL", false, false},
		{"wrong original worker", "original_worker_id=gen_random_uuid()", "original_worker_id<>$5", false, false},
		{"missing worker history", "original_worker_id=gen_random_uuid()", "original_worker_id<>$5", false, false},
		{"blank identity", "original_worker_identity='   '", "original_worker_identity='   '", false, false},
		{"unguarded", "inventory_guarded=false", "NOT inventory_guarded", false, false},
		{"wrong generation", "generation=2", "generation=2", false, false},
		{"unsupported historical ancestry", "state='released',release_evidence='ancestry',release_pushed_sha=repeat('a',40),release_source_sha=repeat('b',40),release_adopted_sha=repeat('c',40),release_final_head_sha=repeat('d',40),release_successor_generation=2,release_branch='work'", "release_evidence='ancestry' AND release_successor_generation=2", false, false},
		{"mismatched disposition", "state='released',final_disposition='no_adopted_source'", "final_disposition='no_adopted_source'", false, false},
		{"forged archive", "state='released',release_evidence='archive',final_disposition='archive',final_capture_id=gen_random_uuid(),final_source_sha='aaaa1111',final_coverage_digest=repeat('b',64)", "final_disposition='archive'", false, false},
		{"two closed", "", "", true, false},
		{"closed and open", "state='open',live_worker_id=$5,live_run_id=$2,release_evidence=NULL,released_at=NULL", "state='open'", true, false},
		{"closed and invalid owner", "user_id=gen_random_uuid()", "user_id<>$3", true, false},
		{"closed and invalid worker", "original_worker_id=gen_random_uuid()", "original_worker_id<>$5", true, false},
		{"closed and invalid repo", "repo_id=gen_random_uuid()", "repo_id<>$4", true, false},
		{"closed and unguarded", "inventory_guarded=false", "NOT inventory_guarded", true, false},
		{"closed and blank identity", "original_worker_identity=' '", "original_worker_identity=' '", true, false},
		{"unrelated generation", "generation=2", "generation=2", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRecoveryEnvGuarded(t, true)
			h := e.handler()
			h.recoverySvc = e.service(recoveryTestLimits())
			w := e.worker
			w.ProtocolCapabilities = []string{capability.RecoveryBindinglessClosedV1}
			tag, err := e.pool.Exec(e.ctx, "UPDATE runs SET claim_generation=3,worker_id=NULL,status='failed' WHERE id=$1", e.run)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("advance run: %v %v", tag, err)
			}
			exactFixtureMutation(t, e, "UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard',released_at=now() WHERE id=$1",
				"SELECT state='discarded' AND release_evidence='owner_discard' FROM recovery_custody_holds WHERE id=$1", e.holdID)
			id := e.holdID
			if tc.sibling {
				id = uuid.New()
				exactFixtureMutation(t, e, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded,release_evidence,released_at)
    SELECT $1,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,inventory_guarded,release_evidence,released_at FROM recovery_custody_holds WHERE id=$2`,
					"SELECT count(*)=2 AND bool_or(id=$1) FROM recovery_custody_holds WHERE run_id=(SELECT run_id FROM recovery_custody_holds WHERE id=$2)", id, e.holdID)
			}
			if tc.set != "" {
				// Reference every parameter in a typed CTE so PostgreSQL can infer all fixture binds.
				prefix := "WITH binds AS (SELECT $1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid) "
				exactFixtureMutation(t, e, prefix+"UPDATE recovery_custody_holds SET "+tc.set+" WHERE id=$1",
					prefix+"SELECT "+tc.check+" FROM recovery_custody_holds WHERE id=$1", id, e.run, e.user, e.repo, w.ID)
			}
			if tc.name == "missing worker history" {
				if err := e.pool.QueryRow(e.ctx, "SELECT original_worker_id FROM recovery_custody_holds WHERE id=$1", e.holdID).Scan(&w.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "missing run history" {
				tag, err := e.pool.Exec(e.ctx, "DELETE FROM runs WHERE id=$1", e.run)
				if err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("remove run history: %v %v", tag, err)
				}
			}
			var before, after string
			snapshot := func(dest *string) {
				t.Helper()
				if err := e.pool.QueryRow(e.ctx, "SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY id)::text,'[]') FROM recovery_custody_holds h WHERE run_id=$1", e.run).Scan(dest); err != nil {
					t.Fatal(err)
				}
			}
			snapshot(&before)
			rec := exactHoldRoute(h, w, e.run, "?generation=1")
			var body struct {
				ExactHold json.RawMessage `json:"exact_hold"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			known := len(body.ExactHold) > 0 && string(body.ExactHold) != "null"
			if rec.Code != 200 || known != tc.known {
				t.Fatalf("known=%t want=%t status=%d body=%s", known, tc.known, rec.Code, rec.Body)
			}
			snapshot(&after)
			if before != after {
				t.Fatal("observation mutated custody")
			}
		})
	}
}

func TestExactRecoveryProductionBearerRoutesLiveDB(t *testing.T) {
	ephemeralPriorityDatabase(t)
	e := newRecoveryEnvGuarded(t, true)
	h := e.handler()
	h.recoverySvc = e.service(recoveryTestLimits())
	successor := uuid.New()
	successorToken := "successor-fixture-" + uuid.NewString()
	successorHash := sha256.Sum256([]byte(successorToken))
	tag, err := e.pool.Exec(e.ctx, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,'successor',$3,'online',ARRAY['recovery_bindingless_closed_v1'])", successor, e.user, successorHash[:])
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("successor setup: %v %v", tag, err)
	}
	token := "exact-fixture-" + uuid.NewString()
	hash := sha256.Sum256([]byte(token))
	tag, err = e.pool.Exec(e.ctx, "UPDATE workers SET token_hash=$2,protocol_capabilities=$3,name='renamed worker' WHERE id=$1", e.worker.ID, hash[:], []string{capability.RecoveryBindinglessClosedV1})
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("worker setup: %v %v", tag, err)
	}
	tag, err = e.pool.Exec(e.ctx, "UPDATE runs SET claim_generation=7,worker_id=$2,status='failed' WHERE id=$1", e.run, successor)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("run setup: %v %v", tag, err)
	}
	exactFixtureMutation(t, e, "UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,release_evidence='owner_discard',released_at=now() WHERE id=$1",
		"SELECT state='discarded' AND release_evidence='owner_discard' FROM recovery_custody_holds WHERE id=$1", e.holdID)
	foreign := newRecoveryEnvGuarded(t, true)
	foreignToken := "foreign-fixture-" + uuid.NewString()
	foreignHash := sha256.Sum256([]byte(foreignToken))
	tag, err = e.pool.Exec(e.ctx, "UPDATE workers SET token_hash=$2,protocol_capabilities=$3 WHERE id=$1", foreign.worker.ID, foreignHash[:], []string{capability.RecoveryBindinglessClosedV1})
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("foreign setup: %v %v", tag, err)
	}
	lim := mw.NewLimiter(10000, time.Minute, nil)
	routers := map[string]http.Handler{"Routes": h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim), "WorkerRoutes": h.WorkerRoutes(lim)}
	for name, router := range routers {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				token  string
				status int
				known  bool
			}{{token, 200, true}, {"", 401, false}, {"invalid", 401, false}, {foreignToken, 200, false}, {successorToken, 200, false}} {
				req := httptest.NewRequest("GET", fmt.Sprintf("/api/worker/runs/%s/recovery-holds?generation=1", e.run), nil)
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body)
				}
				if rec.Code == 200 {
					if rec.Header().Get("Uzi-Recovery-Observation") != "recovery_bindingless_closed_v1" {
						t.Fatal("registered read lacks the explicit per-response guarantee")
					}
					var body struct {
						ExactHold *struct {
							Identity string `json:"original_worker_identity"`
							State    string `json:"state"`
						} `json:"exact_hold"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if (body.ExactHold != nil) != tc.known {
						t.Fatalf("credential scope: %s", rec.Body)
					}
					if body.ExactHold != nil && (body.ExactHold.Identity != e.worker.Name || body.ExactHold.State != "discarded") {
						t.Fatalf("saved provenance lost: %s", rec.Body)
					}
				}
			}
		})
	}
	tag, err = e.pool.Exec(e.ctx, "UPDATE workers SET isolated_lane=true,kind='hosted',ephemeral=true,hosted_size='small',template_declared='base' WHERE id=$1", e.worker.ID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lane setup: %v %v", tag, err)
	}
	for name, router := range routers {
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/worker/runs/%s/recovery-holds?generation=1", e.run), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("%s lane guard=%d body=%s", name, rec.Code, rec.Body)
		}
	}
}
