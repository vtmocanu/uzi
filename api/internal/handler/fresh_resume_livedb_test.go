package handler

// Issue #4628 live-DB proofs for the fresh resume owner routes (GET and POST /api/runs/{id}/fresh-resume),
// through the REAL h.Routes() router: the auth mount, the owner scope, the gate, the DTO seam later units
// build on, the owner Resume integration and the 409 mapping of the fresh resume guard. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type freshHandlerFix struct {
	h      *Handler
	t      *testing.T
	router http.Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	token  string
	jwt    string
	run    uuid.UUID
	w1     uuid.UUID
	w2     uuid.UUID
	cap    uuid.UUID
}

var freshHandlerProject int64 = 4700

const freshHandlerSha = "abababababababababababababababababababab"

// newFreshHandlerFix seeds an owner with a held run (worker_requeue_exhausted, generation 2, one open
// custody hold and one available capture under it) and an eligible destination worker, and wires the
// handler's workersvc with the fresh resume gate set to gate.
func newFreshHandlerFix(t *testing.T, gate bool) *freshHandlerFix {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	svc := workersvc.New(h.q, h.box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), FreshResume: gate, WorkerHeartbeatStale: 45 * time.Second, RunTimeout: 2 * time.Hour,
	})
	svc.SetVault(h.vault)
	svc.SetTxBeginner(pool)
	h.wsvc = svc
	// The recovery service the worker bundle route streams through reads its limits from the config.
	rc := recoveryTestCfg()
	rc.JWTSecret, rc.AuthTokenTTL = h.cfg.JWTSecret, h.cfg.AuthTokenTTL
	h.cfg = rc

	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	freshHandlerProject++
	repo := rmSeedRepo(t, pool, conn, freshHandlerProject, true)
	run := rmSeedRun(t, pool, owner, repo, "recovery_wait")
	w1 := custodySeedWorker(t, pool, owner)
	cliMustExec(t, pool, `UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted', claim_generation=2, worker_id=$2,
	    claim_released_at=now(), released_worker_id=$2 WHERE id=$1`, run, w1)
	hold := custodySeedHoldOnRun(t, pool, owner, run, w1, 2)
	capID := uuid.New()
	cliMustExec(t, pool, `INSERT INTO recovery_captures
	    (id, hold_id, run_id, user_id, original_worker_id, original_worker_identity, source_sha, idempotency_key, state, manifest_bound, byte_size, checksum, prerequisite_shas, expires_at)
	    VALUES ($1,$2,$3,$4,$5,'cw-ident',$6,$7,'available',true,3,'deadbeef','{}',now()+interval '2 hours')`,
		capID, hold, run, owner, w1, freshHandlerSha, "fresh-"+capID.String())
	cliMustExec(t, pool, `INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed) VALUES ($1, 0, 3, $2)`, capID, []byte("abc"))
	w2 := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, w2, owner, "dest-"+w2.String(), w2[:])
	cliMustExec(t, pool, `UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`,
		w2, []string{capability.FreshResumeV1, capability.RecoveryInventoryV1})
	// run_fresh_resumes.capture_id is ON DELETE RESTRICT, so a row left behind makes a later test's
	// unscoped DELETE FROM recovery_captures fail with 23503. Cleanup runs in the context the test ended.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM run_fresh_resumes WHERE run_id=$1`, run); err != nil {
			t.Errorf("cleanup run_fresh_resumes: %v", err)
		}
	})
	return &freshHandlerFix{h: h, t: t, router: router, pool: pool, owner: owner, token: cliMintToken(t, pool, owner, clitoken.ScopeUser),
		jwt: cliMintJWT(t, pool, owner), run: run, w1: w1, w2: w2, cap: capID}
}

func (f *freshHandlerFix) path() string { return fmt.Sprintf("/api/runs/%s/fresh-resume", f.run) }

func (f *freshHandlerFix) preview() apitypes.FreshResumePreviewDTO {
	f.t.Helper()
	rec := bearerReq(f.router, http.MethodGet, f.path(), f.token)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET fresh-resume = %d\n%s", rec.Code, rec.Body.String())
	}
	var dto apitypes.FreshResumePreviewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		f.t.Fatalf("decode preview: %v", err)
	}
	return dto
}

func (f *freshHandlerFix) post(token string, body string) *httptest.ResponseRecorder {
	return bearerReqBody(f.router, http.MethodPost, f.path(), token, body)
}

func freshStartBody(selection string, request uuid.UUID) string {
	b, _ := json.Marshal(apitypes.FreshResumeStartRequest{SelectionToken: selection, RequestID: request.String()})
	return string(b)
}

func freshReasonOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body["reason"]
}

func (f *freshHandlerFix) runStatus() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.t.Context(), `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *freshHandlerFix) transferStates() []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.t.Context(), `SELECT state FROM run_fresh_resumes WHERE run_id=$1 ORDER BY created_at`, f.run)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestFreshResumeRoutesLiveDB(t *testing.T) {
	f := newFreshHandlerFix(t, true)

	// Auth mount: a Bearer and a cookie reach both routes; no credential is 401; a foreign owner and a
	// read-only admin get 404.
	if rec := bearerReq(f.router, http.MethodGet, f.path(), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-cred GET = %d, want 401", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, f.path(), f.jwt, ""); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET = %d, want 200", rec.Code)
	}
	foreign := cliMintToken(t, f.pool, cliSeedUser(t, f.pool, false), clitoken.ScopeUser)
	admin := cliMintToken(t, f.pool, cliSeedUser(t, f.pool, true), clitoken.ScopeAdminRO)
	for _, tok := range []string{foreign, admin} {
		if rec := bearerReq(f.router, http.MethodGet, f.path(), tok); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign GET = %d, want 404", rec.Code)
		}
		if rec := f.post(tok, freshStartBody("x", uuid.New())); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign POST = %d, want 404", rec.Code)
		}
	}

	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusAvailable || dto.Kind != apitypes.FreshResumeKindCapture || dto.CaptureID != f.cap.String() ||
		dto.Sha != freshHandlerSha || dto.EvidenceGeneration != 2 || dto.ClaimGeneration != 2 || dto.SelectionToken == "" ||
		dto.UncoveredGenerations == nil || len(dto.Notes) < 5 {
		t.Fatalf("preview = %+v", dto)
	}
	// The wire shape: the JSON keys later units (CLI, web) read.
	rec := bearerReq(f.router, http.MethodGet, f.path(), f.token)
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"status", "selection_token", "evidence_generation", "claim_generation", "kind", "capture_id", "sha", "uncovered_generations", "notes"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("preview JSON lacks %q: %s", key, rec.Body.String())
		}
	}

	// Body validation is a 400 before anything else.
	for _, body := range []string{``, `{}`, `{"selection_token":"x"}`, `{"selection_token":"x","request_id":"not-a-uuid"}`,
		`{"selection_token":"x","request_id":"` + uuid.New().String() + `","extra":1}`} {
		if rec := f.post(f.token, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %q = %d, want 400", body, rec.Code)
		}
	}

	// A stale token is 409 stale_selection and writes nothing.
	rec = f.post(f.token, freshStartBody("not-the-token", uuid.New()))
	if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != apitypes.FreshResumeReasonStaleSelection {
		t.Fatalf("stale POST = %d %s", rec.Code, rec.Body.String())
	}
	if n := len(f.transferStates()); n != 0 {
		t.Fatalf("rows after a stale start = %d", n)
	}

	// A start: 201 with the pending transfer; the same request replays as 200; another request is 409.
	req := uuid.New()
	rec = f.post(f.token, freshStartBody(dto.SelectionToken, req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d\n%s", rec.Code, rec.Body.String())
	}
	var started apitypes.FreshResumeStartDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.State != "pending" || started.Replay || started.RunID != f.run.String() || started.RequestID != req.String() ||
		started.Kind != apitypes.FreshResumeKindCapture || started.CaptureID != f.cap.String() || started.EvidenceGeneration != 2 || started.AdmissionDeadline == "" {
		t.Fatalf("start DTO = %+v", started)
	}
	rec = f.post(f.token, freshStartBody(dto.SelectionToken, req))
	var replayed apitypes.FreshResumeStartDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &replayed) != nil || !replayed.Replay || replayed.ID != started.ID {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.post(f.token, freshStartBody(dto.SelectionToken, uuid.New()))
	if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != apitypes.FreshResumeStatusInProgress {
		t.Fatalf("second request = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.preview().Status; got != apitypes.FreshResumeStatusInProgress {
		t.Fatalf("preview with a pending row = %q", got)
	}
	if got := f.transferStates(); len(got) != 1 {
		t.Fatalf("transfer rows = %v, want one", got)
	}
}

func TestFreshResumeRoutesGateOffLiveDB(t *testing.T) {
	f := newFreshHandlerFix(t, false)
	dto := f.preview()
	if dto.Status != apitypes.FreshResumeStatusDisabled || dto.SelectionToken != "" {
		t.Fatalf("preview = %+v, want disabled with no token", dto)
	}
	rec := f.post(f.token, freshStartBody("anything", uuid.New()))
	if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != apitypes.FreshResumeStatusDisabled {
		t.Fatalf("POST = %d %s, want 409 disabled", rec.Code, rec.Body.String())
	}
	if n := len(f.transferStates()); n != 0 {
		t.Fatalf("rows = %d, want none", n)
	}
	// With the gate off and no transfer rows, an owner Resume of the same hold behaves as before.
	rec = bearerReq(f.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/resume-now", f.run), f.token)
	if rec.Code != http.StatusOK || f.runStatus() != "queued" {
		t.Fatalf("resume-now = %d %s status=%s, want 200 and queued", rec.Code, rec.Body.String(), f.runStatus())
	}
}

func TestFreshResumeOwnerResumeRoutesLiveDB(t *testing.T) {
	t.Run("a pending row is refused first and the resume proceeds", func(t *testing.T) {
		f := newFreshHandlerFix(t, true)
		dto := f.preview()
		if rec := f.post(f.token, freshStartBody(dto.SelectionToken, uuid.New())); rec.Code != http.StatusCreated {
			t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
		}
		rec := bearerReq(f.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/resume-now", f.run), f.token)
		if rec.Code != http.StatusOK || f.runStatus() != "queued" {
			t.Fatalf("resume-now = %d %s status=%s", rec.Code, rec.Body.String(), f.runStatus())
		}
		if got := f.transferStates(); len(got) != 1 || got[0] != "refused" {
			t.Fatalf("transfer states = %v, want [refused]", got)
		}
	})
	t.Run("a claimed row is 409 fresh_resume_in_progress", func(t *testing.T) {
		f := newFreshHandlerFix(t, true)
		cliMustExec(t, f.pool, `INSERT INTO run_fresh_resumes (run_id,user_id,request_id,evidence_generation,evidence_kind,checkpoint_ref,sha,candidate_token,state,destination_worker_id,claim_generation)
		    VALUES ($1,$2,$3,2,'checkpoint_ref','refs/uzi-checkpoints/x',$4,'tok','claimed',$5,3)`, f.run, f.owner, uuid.New(), freshHandlerSha, f.w2)
		rec := bearerReq(f.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/resume-now", f.run), f.token)
		if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != "fresh_resume_in_progress" {
			t.Fatalf("resume-now = %d %s", rec.Code, rec.Body.String())
		}
		if f.runStatus() != "recovery_wait" {
			t.Fatalf("run status = %s, want untouched", f.runStatus())
		}
	})
	t.Run("an autocommit owner write the guard refuses is 409, never 500", func(t *testing.T) {
		f := newFreshHandlerFix(t, true)
		cliMustExec(t, f.pool, `UPDATE runs SET status='pool_wait', recovery_wait_cause=NULL WHERE id=$1`, f.run)
		cliMustExec(t, f.pool, `INSERT INTO run_fresh_resumes (run_id,user_id,request_id,evidence_generation,evidence_kind,checkpoint_ref,sha,candidate_token)
		    VALUES ($1,$2,$3,2,'checkpoint_ref','refs/uzi-checkpoints/x',$4,'tok')`, f.run, f.owner, uuid.New(), freshHandlerSha)
		rec := bearerReq(f.router, http.MethodPost, fmt.Sprintf("/api/runs/%s/resume-now", f.run), f.token)
		if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != "fresh_resume_in_progress" {
			t.Fatalf("resume-now of a pool_wait run with a transfer = %d %s", rec.Code, rec.Body.String())
		}
		if f.runStatus() != "pool_wait" {
			t.Fatalf("run status = %s, want untouched", f.runStatus())
		}
	})
}

// Owner writers whose SQL changes none of status, worker_id or claim_generation (pause_mode, extend,
// the token switch) never reach the guard trigger, so each handler reads the transfer itself: a pending
// row is 409 fresh_resume_in_progress and nothing is written, and the same request goes through once
// the row is closed.
func TestFreshResumeOwnerWritersRefuseLiveDB(t *testing.T) {
	cases := []struct {
		name, path, body string
	}{
		{"extend", "/inputs", `{"kind":"extend","body":"7200"}`},
		{"pause", "/inputs", `{"kind":"pause","body":"now"}`},
		{"pause_cancel", "/inputs", `{"kind":"pause_cancel"}`},
		{"scope", "/inputs", `{"kind":"scope","body":"1"}`},
		{"stop", "/inputs", `{"kind":"stop"}`},
		{"set-token", "/credential", `{"mode":"default"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFreshHandlerFix(t, true)
			cliMustExec(t, f.pool, `INSERT INTO run_fresh_resumes (run_id,user_id,request_id,evidence_generation,evidence_kind,checkpoint_ref,sha,candidate_token)
			    VALUES ($1,$2,$3,2,'checkpoint_ref','refs/uzi-checkpoints/x',$4,'tok')`, f.run, f.owner, uuid.New(), freshHandlerSha)
			var before time.Time
			if err := f.pool.QueryRow(t.Context(), `SELECT updated_at FROM runs WHERE id=$1`, f.run).Scan(&before); err != nil {
				t.Fatal(err)
			}
			url := fmt.Sprintf("/api/runs/%s%s", f.run, c.path)
			rec := bearerReqBody(f.router, http.MethodPost, url, f.token, c.body)
			if rec.Code != http.StatusConflict || freshReasonOf(t, rec) != "fresh_resume_in_progress" {
				t.Fatalf("%s with a pending transfer = %d %s, want 409 fresh_resume_in_progress", c.name, rec.Code, rec.Body.String())
			}
			var after time.Time
			if err := f.pool.QueryRow(t.Context(), `SELECT updated_at FROM runs WHERE id=$1`, f.run).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !after.Equal(before) {
				t.Fatalf("%s wrote the run despite the transfer", c.name)
			}
			cliMustExec(t, f.pool, `UPDATE run_fresh_resumes SET state='refused', refusal_reason='test', closed_at=now() WHERE run_id=$1`, f.run)
			rec = bearerReqBody(f.router, http.MethodPost, url, f.token, c.body)
			if rec.Code == http.StatusConflict && strings.Contains(rec.Body.String(), "fresh_resume_in_progress") {
				t.Fatalf("%s after the transfer closed is still refused: %s", c.name, rec.Body.String())
			}
		})
	}
}

// TestWriteFreshResumeInProgressMapping pins the one helper every owner path uses: the guard's two
// UZ416 rules and the service's in-progress refusal are 409 with a reason, wrapped or not; any other
// error is not claimed.
func TestWriteFreshResumeInProgressMapping(t *testing.T) {
	inProgress := &pgconn.PgError{Code: "UZ416", Message: "fresh resume transfer in progress"}
	excluded := &pgconn.PgError{Code: "UZ416", Message: "fresh resume predecessor excluded"}
	for _, c := range []struct {
		name   string
		err    error
		reason string
	}{
		{"guard rule 1", inProgress, "fresh_resume_in_progress"},
		{"wrapped guard rule 1", fmt.Errorf("resume: %w", inProgress), "fresh_resume_in_progress"},
		{"service in-progress", &workersvc.FreshResumeRefused{Reason: apitypes.FreshResumeStatusInProgress}, "fresh_resume_in_progress"},
		{"guard rule 2", excluded, "fresh_resume_predecessor_excluded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if !writeFreshResumeInProgress(rec, c.err) || rec.Code != http.StatusConflict || freshReasonOf(t, rec) != c.reason {
				t.Fatalf("got %d %s, want 409 %s", rec.Code, rec.Body.String(), c.reason)
			}
		})
	}
	for _, err := range []error{errors.New("boom"), &pgconn.PgError{Code: "23505"}, &pgconn.PgError{Code: "UZ415", Message: "fresh resume transfer in progress"},
		&workersvc.FreshResumeRefused{Reason: apitypes.FreshResumeStatusDisabled}} {
		rec := httptest.NewRecorder()
		if writeFreshResumeInProgress(rec, err) || rec.Body.Len() != 0 {
			t.Fatalf("%v was claimed: %d %s", err, rec.Code, rec.Body.String())
		}
	}
	if !strings.Contains(freshResumeReasonMessages[apitypes.FreshResumeStatusDisabled], "not enabled") {
		t.Fatal("the disabled refusal has no owner-facing sentence")
	}
}

// The owner discard of a capture a pending transfer references is 409 fresh_resume_in_progress (the
// recovery service refuses it; the handler maps it), the bytes stay, and the transfer row stays.
func TestFreshResumeDiscardArchive409LiveDB(t *testing.T) {
	f := newFreshHandlerFix(t, true)
	dto := f.preview()
	if rec := f.post(f.token, freshStartBody(dto.SelectionToken, uuid.New())); rec.Code != http.StatusCreated {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	del := fmt.Sprintf("/api/runs/%s/archives/%s", f.run, f.cap)
	rec := bearerReq(f.router, http.MethodDelete, del, f.token)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "fresh_resume_in_progress") {
		t.Fatalf("discard of a referenced capture = %d %s, want 409 fresh_resume_in_progress", rec.Code, rec.Body.String())
	}
	if st := custodyCaptureStateByID(t, f.pool, f.cap); st != "available" {
		t.Fatalf("capture state = %q, want available", st)
	}
	if n := custodyChunkCountByID(t, f.pool, f.cap); n != 1 {
		t.Fatalf("capture chunks = %d, want the bytes kept", n)
	}
	// Once the transfer is closed the same discard goes through.
	cliMustExec(t, f.pool, `UPDATE run_fresh_resumes SET state='refused', refusal_reason='test', closed_at=now() WHERE run_id=$1`, f.run)
	if rec := bearerReq(f.router, http.MethodDelete, del, f.token); rec.Code != http.StatusOK {
		t.Fatalf("discard after the transfer closed = %d %s", rec.Code, rec.Body.String())
	}
}
