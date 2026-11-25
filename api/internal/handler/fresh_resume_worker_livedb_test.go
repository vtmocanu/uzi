package handler

// Issue #4628 live-DB proofs for the fresh resume destination routes (/api/worker/runs/{id}/fresh-resume/...)
// and the pre-seed refusal of an excluded park writer, through the real h.Routes() router with a worker
// Bearer. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	freshWorkerGen      int64 = 3
	freshWorkerAttempt        = "20260101T000000Z-g3-0123456789abcdef"
	freshWorkerAttempt2       = "20260101T000001Z-g3-fedcba9876543210"
)

type freshWorkerFix struct {
	*freshHandlerFix
	w2Token string
	frid    uuid.UUID
}

// newFreshWorkerFix starts a transfer through the owner route (pending) and gives the destination worker a
// usable token. The capture's one chunk is sealed so the bundle route can stream it.
func newFreshWorkerFix(t *testing.T) *freshWorkerFix {
	t.Helper()
	f := &freshWorkerFix{freshHandlerFix: newFreshHandlerFix(t, true)}
	tok, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	f.w2Token = tok
	cliMustExec(t, f.pool, `UPDATE workers SET token_hash=$2 WHERE id=$1`, f.w2, hash)
	sealed, err := f.h.box.SealWithAAD([]byte("abc"), []byte("recovery_chunk|"+f.cap.String()+"|0|3"))
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, f.pool, `UPDATE recovery_capture_chunks SET sealed=$2 WHERE capture_id=$1`, f.cap, sealed)
	cliMustExec(t, f.pool, `UPDATE recovery_captures SET chunk_count=1 WHERE id=$1`, f.cap)

	dto := f.preview()
	rec := f.post(f.token, freshStartBody(dto.SelectionToken, uuid.New()))
	var started apitypes.FreshResumeStartDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &started) != nil {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	f.frid = uuid.MustParse(started.ID)
	return f
}

// claim puts the transfer in the state the fresh claim leaves it: the saved tuple taken, the run claimed
// by the destination at gen and the row claimed (the same statements the claim transaction runs).
func (f *freshWorkerFix) claim(gen int64) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.OptInFreshResume(ctx, f.frid.String()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := q.PrepareFreshResumeClaim(ctx, store.PrepareFreshResumeClaimParams{RunID: f.run, ID: f.frid, WorkerID: pgconv.UUID(f.w2)}); err != nil {
		f.t.Fatalf("prepare: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status='claimed', claim_generation=$2, claimed_at=now(), status_since=now(), claim_released_at=NULL WHERE id=$1`, f.run, gen); err != nil {
		f.t.Fatal(err)
	}
	if _, err := q.MarkFreshResumeClaimed(ctx, store.MarkFreshResumeClaimedParams{
		ID: f.frid, WorkerID: pgconv.UUID(f.w2), ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, TransferSeconds: 7200,
	}); err != nil {
		f.t.Fatalf("mark claimed: %v", err)
	}
	if err := q.ClearFreshResumeOptIn(ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *freshWorkerFix) route(method, tail, token, body string) *httptest.ResponseRecorder {
	return bearerReqBody(f.router, method, fmt.Sprintf("/api/worker/runs/%s/fresh-resume/%s%s", f.run, f.frid, tail), token, body)
}

func freshSeededBody(claimGen int64, attempt string, live int64) string {
	b, _ := json.Marshal(apitypes.FreshResumeSeededRequest{ClaimGeneration: claimGen, AttemptID: attempt, LiveGeneration: live})
	return string(b)
}

func freshGenBody(claimGen int64) string {
	b, _ := json.Marshal(apitypes.FreshResumeAbandonRequest{ClaimGeneration: claimGen})
	return string(b)
}

func (f *freshWorkerFix) state(token string, live int64) (int, apitypes.FreshResumeStateDTO) {
	f.t.Helper()
	rec := f.route(http.MethodGet, fmt.Sprintf("/state?live_generation=%d", live), token, "")
	var dto apitypes.FreshResumeStateDTO
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			f.t.Fatalf("decode state: %v", err)
		}
	}
	return rec.Code, dto
}

func (f *freshWorkerFix) wantReason(rec *httptest.ResponseRecorder, code int, reason string) {
	f.t.Helper()
	if rec.Code != code || freshReasonOf(f.t, rec) != reason {
		f.t.Fatalf("got %d %s, want %d reason %q", rec.Code, rec.Body.String(), code, reason)
	}
}

func (f *freshWorkerFix) runRow() (status string, worker *uuid.UUID, gen int64) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT status, worker_id, claim_generation FROM runs WHERE id=$1`, f.run).Scan(&status, &worker, &gen); err != nil {
		f.t.Fatal(err)
	}
	return status, worker, gen
}

func TestFreshResumeWorkerRoutesLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	other := uuid.New()
	tok, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, f.pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, other, f.owner, "other-"+other.String(), hash)
	otherToken := tok

	// Pending: nothing is claimed, so every fenced route is 409 not_claimed and the state route has no row
	// for the caller (404).
	f.wantReason(f.route(http.MethodGet, "/bundle?claim_generation=3", f.w2Token, ""), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/abandon", f.w2Token, freshGenBody(3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	if code, _ := f.state(f.w2Token, 3); code != http.StatusNotFound {
		t.Fatalf("pending state = %d, want 404", code)
	}

	f.claim(freshWorkerGen)

	// state: any caller but the destination, and an unknown transfer, is 404; the destination reads
	// claimed with no seeded fields.
	if code, _ := f.state(otherToken, 3); code != http.StatusNotFound {
		t.Fatalf("foreign worker state = %d, want 404", code)
	}
	unknown := bearerReq(f.router, http.MethodGet, fmt.Sprintf("/api/worker/runs/%s/fresh-resume/%s/state?live_generation=3", f.run, uuid.New()), f.w2Token)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown transfer state = %d, want 404", unknown.Code)
	}
	code, dto := f.state(f.w2Token, 3)
	if code != http.StatusOK || dto.State != "claimed" || dto.SeededAt != nil || dto.DestinationAttemptID != nil || dto.SeededGeneration != nil || !dto.CurrentAuthority {
		t.Fatalf("claimed state = %d %+v", code, dto)
	}
	if _, dto := f.state(f.w2Token, 4); dto.CurrentAuthority {
		t.Fatal("current_authority true against a live generation the run is not at")
	}

	// bundle: a stale generation and a foreign worker are fenced out; the destination streams the capture.
	f.wantReason(f.route(http.MethodGet, "/bundle?claim_generation=2", f.w2Token, ""), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodGet, "/bundle?claim_generation=3", otherToken, ""), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	if rec := f.route(http.MethodGet, "/bundle", f.w2Token, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bundle without a generation = %d, want 400", rec.Code)
	}
	rec := f.route(http.MethodGet, "/bundle?claim_generation=3", f.w2Token, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "abc" {
		t.Fatalf("bundle = %d %q, want 200 abc", rec.Code, rec.Body.String())
	}

	// The pre-seed refusal: a park report of the destination while its transfer is claimed and unseeded is
	// the typed fresh_resume_pre_seed, never the ordinary no-op conflict.
	wallPark := func() *httptest.ResponseRecorder {
		return bearerReqBody(f.router, http.MethodPost, fmt.Sprintf("/api/worker/runs/%s/wall-park", f.run), f.w2Token,
			fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen))
	}
	rec = wallPark()
	var pre map[string]any
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &pre) != nil || pre["reason"] != apitypes.FreshResumeReasonPreSeed || pre["run"] == nil {
		t.Fatalf("pre-seed wall park = %d %s", rec.Code, rec.Body.String())
	}

	// seeded: a stale claim generation is fenced out; an attempt that does not carry G' on a claimed row is
	// not-claimed and leaves the row claimed; the real receipt marks it seeded.
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(2, freshWorkerAttempt, 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/seeded", otherToken, freshSeededBody(3, freshWorkerAttempt, 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, "20260101T000000Z-g2-0123456789abcdef", 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, "20260101T000000Z-gx-0123456789abcdef", 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	if got := f.transferStates(); len(got) != 1 || got[0] != "claimed" {
		t.Fatalf("states after refused receipts = %v, want [claimed]", got)
	}
	rec = f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3))
	var seeded apitypes.FreshResumeSeededDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &seeded) != nil || seeded.State != "seeded" || seeded.SeededAt == nil ||
		seeded.DestinationAttemptID == nil || *seeded.DestinationAttemptID != freshWorkerAttempt || seeded.SeededGeneration == nil || *seeded.SeededGeneration != 3 || !seeded.CurrentAuthority {
		t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
	}
	// Replay of the same attempt and generation is a success; a different attempt or generation is a mismatch.
	rec = f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3))
	var replay apitypes.FreshResumeSeededDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &replay) != nil || replay.State != "seeded" || *replay.SeededAt != *seeded.SeededAt {
		t.Fatalf("receipt replay = %d %s", rec.Code, rec.Body.String())
	}
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt2, 3)), http.StatusConflict, apitypes.FreshResumeReasonAttemptMismatch)
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(4, freshWorkerAttempt, 4)), http.StatusConflict, apitypes.FreshResumeReasonAttemptMismatch)
	// A seeded transfer is no longer abandonable or streamable: the claim fence is gone.
	f.wantReason(f.route(http.MethodPost, "/abandon", f.w2Token, freshGenBody(3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodGet, "/bundle?claim_generation=3", f.w2Token, ""), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	// After seeding the park writers are no longer excluded: the same park is the ordinary conflict.
	rec = wallPark()
	pre = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &pre); err == nil && pre["reason"] == apitypes.FreshResumeReasonPreSeed {
		t.Fatalf("a seeded transfer still answers fresh_resume_pre_seed: %s", rec.Body.String())
	}

	// current_authority: true at the live generation, false once a cancel is deliverable, false after a
	// reclaim at G'+1 judged against the old generation.
	if code, dto := f.state(f.w2Token, 3); code != http.StatusOK || dto.State != "seeded" || !dto.CurrentAuthority {
		t.Fatalf("seeded state = %d %+v", code, dto)
	}
	cliMustExec(t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'cancel', 'stop')`, f.run)
	if _, dto := f.state(f.w2Token, 3); dto.CurrentAuthority {
		t.Fatal("current_authority true with a deliverable cancel")
	}
	rec = f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3))
	if err := json.Unmarshal(rec.Body.Bytes(), &replay); rec.Code != http.StatusOK || err != nil || replay.CurrentAuthority {
		t.Fatalf("receipt replay under a cancel = %d %s", rec.Code, rec.Body.String())
	}
	cliMustExec(t, f.pool, `DELETE FROM run_user_inputs WHERE run_id=$1`, f.run)
	cliMustExec(t, f.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'stop', 'stop')`, f.run)
	if _, dto := f.state(f.w2Token, 3); dto.CurrentAuthority {
		t.Fatal("current_authority true with a deliverable stop")
	}
	cliMustExec(t, f.pool, `DELETE FROM run_user_inputs WHERE run_id=$1`, f.run)
	if _, dto := f.state(f.w2Token, 3); !dto.CurrentAuthority {
		t.Fatal("current_authority false again with the stop input gone")
	}
	cliMustExec(t, f.pool, `UPDATE runs SET claim_generation=4 WHERE id=$1`, f.run)
	if code, dto := f.state(f.w2Token, 3); code != http.StatusOK || dto.CurrentAuthority {
		t.Fatalf("state after a reclaim at G'+1 against G' = %d %+v", code, dto)
	}
	if _, dto := f.state(f.w2Token, 4); !dto.CurrentAuthority {
		t.Fatal("current_authority false at the reclaimed generation")
	}
}

func TestFreshResumeWorkerAbandonLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	f.claim(freshWorkerGen)
	// The routes ignore the gate: with the instance gate off the destination can still abandon.
	off := workersvc.New(f.h.q, f.h.box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), FreshResume: false, WorkerHeartbeatStale: 45 * time.Second, RunTimeout: 2 * time.Hour})
	off.SetVault(f.h.vault)
	off.SetTxBeginner(f.pool)
	f.h.wsvc = off

	f.wantReason(f.route(http.MethodPost, "/abandon", f.w2Token, freshGenBody(2)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	if status, _, _ := f.runRow(); status != "claimed" {
		t.Fatalf("run after a stale abandon = %q, want claimed", status)
	}
	rec := f.route(http.MethodPost, "/abandon", f.w2Token, freshGenBody(freshWorkerGen))
	var out apitypes.FreshResumeAbandonDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.State != "refused" || out.ID != f.frid.String() {
		t.Fatalf("abandon = %d %s", rec.Code, rec.Body.String())
	}
	// The run is back at its held recovery_wait on its predecessor.
	status, worker, _ := f.runRow()
	if status != "recovery_wait" || worker == nil || *worker != f.w1 {
		t.Fatalf("run after abandon = %q worker %v, want recovery_wait on %s", status, worker, f.w1)
	}
	var reason string
	if err := f.pool.QueryRow(context.Background(), `SELECT refusal_reason FROM run_fresh_resumes WHERE id=$1`, f.frid).Scan(&reason); err != nil || reason != workersvc.FreshAbandonedReason {
		t.Fatalf("refusal reason = %q (%v)", reason, err)
	}
	// F1: a transfer refused before seeding answers state=refused with seeded_at null and no authority.
	code, dto := f.state(f.w2Token, freshWorkerGen)
	if code != http.StatusOK || dto.State != "refused" || dto.SeededAt != nil || dto.DestinationAttemptID != nil || dto.CurrentAuthority {
		t.Fatalf("refused state = %d %+v", code, dto)
	}
	// Closed: every fenced route is now 409, including a second abandon.
	f.wantReason(f.route(http.MethodPost, "/abandon", f.w2Token, freshGenBody(freshWorkerGen)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	f.wantReason(f.route(http.MethodGet, "/bundle?claim_generation=3", f.w2Token, ""), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
}

// freshRoutes are the four destination routes with a body that passes validation, so a refusal is the
// route's fence and never a 400.
func (f *freshWorkerFix) freshRoutes(runID uuid.UUID, frid uuid.UUID) []func(token string) *httptest.ResponseRecorder {
	base := fmt.Sprintf("/api/worker/runs/%s/fresh-resume/%s", runID, frid)
	return []func(token string) *httptest.ResponseRecorder{
		func(tok string) *httptest.ResponseRecorder {
			return bearerReqBody(f.router, http.MethodGet, base+"/bundle?claim_generation=3", tok, "")
		},
		func(tok string) *httptest.ResponseRecorder {
			return bearerReqBody(f.router, http.MethodPost, base+"/abandon", tok, freshGenBody(freshWorkerGen))
		},
		func(tok string) *httptest.ResponseRecorder {
			return bearerReqBody(f.router, http.MethodPost, base+"/seeded", tok, freshSeededBody(3, freshWorkerAttempt, 3))
		},
		func(tok string) *httptest.ResponseRecorder {
			return bearerReqBody(f.router, http.MethodGet, base+"/state?live_generation=3", tok, "")
		},
	}
}

func TestFreshResumeWorkerForeignCallersLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	f.claim(freshWorkerGen)
	names := []string{"bundle", "abandon", "seeded", "state"}

	// A worker of a different owner never reaches the transfer: 404 or 409, never 200, and the transfer
	// stays claimed.
	foreignOwner := cliSeedUser(t, f.pool, false)
	tok, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	fw := uuid.New()
	cliMustExec(t, f.pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, fw, foreignOwner, "foreign-"+fw.String(), hash)
	for i, call := range f.freshRoutes(f.run, f.frid) {
		if rec := call(tok); rec.Code != http.StatusNotFound && rec.Code != http.StatusConflict {
			t.Errorf("foreign owner worker on %s = %d %s, want 404 or 409", names[i], rec.Code, rec.Body.String())
		}
	}
	if got := f.transferStates(); len(got) != 1 || got[0] != "claimed" {
		t.Fatalf("states after foreign calls = %v, want [claimed]", got)
	}

	// A transfer id of run X under run Y's path is not found: 404 on state, never 200 on any route.
	var repo uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT repo_id FROM runs WHERE id=$1`, f.run).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	other := rmSeedRun(t, f.pool, f.owner, repo, "completed")
	for i, call := range f.freshRoutes(other, f.frid) {
		rec := call(f.w2Token)
		want := http.StatusConflict
		if names[i] == "state" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Errorf("run Y path with run X's transfer on %s = %d %s, want %d", names[i], rec.Code, rec.Body.String(), want)
		}
	}
	if got := f.transferStates(); len(got) != 1 || got[0] != "claimed" {
		t.Fatalf("states after cross-run calls = %v, want [claimed]", got)
	}
}

func TestFreshResumeWorkerLaneWorkerForbiddenLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	f.claim(freshWorkerGen)
	tok, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	cliMustExec(t, f.pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, template_declared,
	    hosted_size, docker_enabled, ephemeral, ephemeral_run_id, isolated_lane, protocol_capabilities)
	    VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'base', 'm', false, true, $5, true, '{isolated_fetch_v1}')`,
		id, f.owner, "lane-"+id.String(), hash, f.run)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM workers WHERE id=$1`, id) })
	for i, call := range f.freshRoutes(f.run, f.frid) {
		if rec := call(tok); rec.Code != http.StatusForbidden {
			t.Errorf("isolated-lane worker on route %d = %d %s, want 403", i, rec.Code, rec.Body.String())
		}
	}
}

func TestFreshResumeWorkerReceiptReplayLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	f.claim(freshWorkerGen)
	if rec := f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3)); rec.Code != http.StatusOK {
		t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
	}
	// Only a seeded row with the same attempt and generation is a replay success: a superseded or finished
	// row answers not_claimed.
	for _, state := range []string{"superseded", "finished"} {
		cliMustExec(t, f.pool, `UPDATE run_fresh_resumes SET state=$2 WHERE id=$1`, f.frid, state)
		f.wantReason(f.route(http.MethodPost, "/seeded", f.w2Token, freshSeededBody(3, freshWorkerAttempt, 3)), http.StatusConflict, apitypes.FreshResumeReasonNotClaimed)
	}
}

// preSeedBody asserts the typed fresh_resume_pre_seed refusal with the run in the body.
func preSeedBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]any
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["reason"] != apitypes.FreshResumeReasonPreSeed || body["run"] == nil {
		t.Fatalf("got %d %s, want 409 fresh_resume_pre_seed with the run", rec.Code, rec.Body.String())
	}
}

func TestFreshResumePreSeedHooksLiveDB(t *testing.T) {
	f := newFreshWorkerFix(t)
	f.claim(freshWorkerGen)
	post := func(tail, body string) *httptest.ResponseRecorder {
		return bearerReqBody(f.router, http.MethodPost, fmt.Sprintf("/api/worker/runs/%s%s", f.run, tail), f.w2Token, body)
	}
	t.Run("run state fail report", func(t *testing.T) {
		preSeedBody(t, post("/state", fmt.Sprintf(`{"status":"failed","claim_generation":%d}`, freshWorkerGen)))
	})
	t.Run("base attestation hold", func(t *testing.T) {
		preSeedBody(t, post("/base-attestation-hold", fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen)))
	})
	// A claimed run matches no hold writer's status: the writers return zero rows, the handlers' !applied
	// branches answer, and the typed refusal replaces the ordinary 409.
	t.Run("claimed phase switch hold", func(t *testing.T) {
		preSeedBody(t, post("/phase-switch-hold", fmt.Sprintf(`{"claim_generation":%d,"phase":"plan"}`, freshWorkerGen)))
	})
	t.Run("claimed completion hold", func(t *testing.T) {
		preSeedBody(t, post("/completion/hold", fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen)))
	})

	// The destination's run is `running` here (W2, claimed transfer): the hold writers match that status,
	// set paused, and the fresh resume guard raises. The reply is the typed 409, never a 500, and neither
	// the run nor the transfer changes.
	cliMustExec(t, f.pool, `UPDATE runs SET status='running', harness='codex', session_id='fresh-session', require_base_match=true, planned_base_commit=repeat('a',40) WHERE id=$1`, f.run)
	cliMustExec(t, f.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.w2,
		[]string{capability.ProviderPolicyHoldV1, capability.CodexHarnessV1, capability.CodexRuntimeV2})
	snapshot := func() string {
		var s string
		if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT to_jsonb(r) FROM runs r WHERE r.id=$1)::text || (SELECT to_jsonb(x) FROM run_fresh_resumes x WHERE x.id=$2)::text`, f.run, f.frid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	runningCase := func(name, tail, body string, setup ...string) {
		t.Run(name, func(t *testing.T) {
			for _, s := range setup {
				cliMustExec(t, f.pool, s, f.run)
			}
			before := snapshot()
			preSeedBody(t, post(tail, body))
			if after := snapshot(); after != before {
				t.Fatalf("run or transfer changed:\n before %s\n after  %s", before, after)
			}
		})
	}
	runningCase("running base attestation hold", "/base-attestation-hold", fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen))
	runningCase("running policy hold", "/policy-hold", fmt.Sprintf(
		`{"outcome_id":%q,"source_generation":%d,"claim_generation":%d,"detail":"refused","cause":"refused","continuation_mode":"same_conversation","tag":"cyberPolicy","phase":"implementation","correlation_id":"pr-00000000000000000000000000000000-1","published":false}`,
		uuid.NewString(), freshWorkerGen, freshWorkerGen))
	// The iteration park evidence passes Validate, and the run is eligible (issue lane, zero completion
	// attempts, frozen allowance equal to the evidence), so SetRunIterationPark itself runs and raises.
	runningCase("running iteration park", "/iteration-park",
		fmt.Sprintf(`{"claim_generation":%d,"iteration_count":5,"max_iterations":5,"head":"","published":false}`, freshWorkerGen),
		`UPDATE runs SET budget_max_iterations=5, completion_attempts=0 WHERE id=$1`)
	runningCase("running repo agent source hold", "/repo-agent-source-hold", fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen))
	runningCase("running phase switch hold", "/phase-switch-hold", fmt.Sprintf(`{"claim_generation":%d,"phase":"plan"}`, freshWorkerGen))
	runningCase("running completion hold", "/completion/hold", fmt.Sprintf(`{"claim_generation":%d}`, freshWorkerGen),
		`UPDATE runs SET completion_contract_version=1, completion_attempts=1 WHERE id=$1`)
}
