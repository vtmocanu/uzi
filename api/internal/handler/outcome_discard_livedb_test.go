package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/outcomeloss"
)

// Issue #2984: the owner-confirmed outcome discard through the REAL mounted routes of h.Routes(),
// so the RequireUser / RequireWorker wiring, the CLI-token no-CSRF path and the cookie CSRF path
// are what is under test, not a hand-built chain. Skipped unless UZI_TEST_DATABASE_URL points at
// a throwaway Postgres (./e2e/run-store-it.sh).

type discardHTTP struct {
	*settleEnv
	mu   sync.Mutex
	now  time.Time
	run  uuid.UUID
	ownr string // uzc_ CLI token of the owning user
}

func (d *discardHTTP) clock() time.Time { d.mu.Lock(); defer d.mu.Unlock(); return d.now }
func (d *discardHTTP) advance(x time.Duration) {
	d.mu.Lock()
	d.now = d.now.Add(x)
	d.mu.Unlock()
}

func newDiscardHTTP(t *testing.T) *discardHTTP {
	t.Helper()
	e := newSettleEnv(t)
	e.wsvc.SetTxBeginner(e.pool)
	d := &discardHTTP{settleEnv: e, now: time.Now().UTC().Truncate(time.Millisecond)}
	e.wsvc.SetClock(d.clock)
	d.run = d.runFor(e.user)
	d.ownr = cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
	return d
}

// runFor seeds a failed run owned by user with no worker attribution.
func (d *discardHTTP) runFor(user uuid.UUID) uuid.UUID {
	d.t.Helper()
	d.iidCount++
	id := uuid.New()
	d.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	        VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'failed')`, id, user, d.repo, d.iidCount)
	return id
}

func discardJournalHex(n byte) string { return strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

func (d *discardHTTP) report(worker, run uuid.UUID, gen int64, reason string, variant byte) apitypes.BlockedOutcomeReport {
	m := &apitypes.OutcomeLossManifest{
		Version: 1, WorkerID: worker.String(), RunID: run.String(), RunDir: run.String(), ClaimGeneration: gen,
		JournalSHA256: discardJournalHex(1), BlockedReason: reason,
		Outcome:  apitypes.OutcomeLossOutcome{Status: "failed", Phase: "run"},
		Siblings: []apitypes.OutcomeLossSibling{},
		Clones: []apitypes.OutcomeLossClone{{
			Path: "/data/runner/x/issue-1", Branch: "agent/issue-1", CommitBasis: outcomeloss.BasisWorkerBare,
			UnpublishedCommits: []string{strings.Repeat("a", 40)},
			Paths:              []apitypes.OutcomeLossPathItem{{Path: "notes.txt", Status: "??", Kind: "file", Size: 3, SHA256: discardJournalHex(variant)}},
		}},
	}
	_, sum, err := outcomeloss.Sum(m)
	if err != nil {
		d.t.Fatal(err)
	}
	return apitypes.BlockedOutcomeReport{RunID: run.String(), ClaimGeneration: gen, JournalSHA256: discardJournalHex(1),
		BlockedReason: reason, LossComplete: true, LossSHA256: sum, Loss: m}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (d *discardHTTP) put(token string, reports ...apitypes.BlockedOutcomeReport) *httptest.ResponseRecorder {
	if reports == nil {
		reports = []apitypes.BlockedOutcomeReport{}
	}
	return rejectionHTTP(d.settleEnv, http.MethodPut, "/api/worker/blocked-outcomes", token, jsonBody(d.t, apitypes.BlockedOutcomesPutRequest{Outcomes: reports}))
}

func (d *discardHTTP) mustPut(token string, reports ...apitypes.BlockedOutcomeReport) {
	d.t.Helper()
	if rec := d.put(token, reports...); rec.Code != http.StatusOK {
		d.t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
}

func discardRequestFor(r apitypes.BlockedOutcomeReport) apitypes.OutcomeDiscardRequest {
	return apitypes.OutcomeDiscardRequest{RunID: r.RunID, ClaimGeneration: r.ClaimGeneration, JournalSHA256: r.JournalSHA256,
		LossSHA256: r.LossSHA256, Evidence: apitypes.OutcomeDiscardEvidenceInformed}
}

func (d *discardHTTP) ownerPath(worker uuid.UUID, rest string) string {
	return "/api/workers/" + worker.String() + rest
}

func (d *discardHTTP) post(token string, worker uuid.UUID, r apitypes.BlockedOutcomeReport) *httptest.ResponseRecorder {
	return bearerReqBody(d.router, http.MethodPost, d.ownerPath(worker, "/blocked-outcomes/discard?confirm=discard"), token, jsonBody(d.t, discardRequestFor(r)))
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder, want int) T {
	t.Helper()
	var v T
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, want, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return v
}

func TestOutcomeDiscardRoutesLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	rep := d.report(d.workerA, d.run, 3, outcomeloss.ReasonRunNotOwned, 1)

	if rec := d.put("", rep); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT without a worker token = %d", rec.Code)
	}
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/register", d.tokenA, "{}"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "outcome_discard_v1") {
		t.Fatalf("register did not advertise outcome_discard_v1: %d %s", rec.Code, rec.Body.String())
	}
	put := decodeInto[apitypes.BlockedOutcomesPutResponse](t, d.put(d.tokenA, rep), 200)
	if put.Stored != 1 {
		t.Fatalf("stored = %d", put.Stored)
	}

	list := decodeInto[apitypes.OwnerBlockedOutcomesResponse](t, bearerReq(d.router, http.MethodGet, d.ownerPath(d.workerA, "/blocked-outcomes"), d.ownr), 200)
	if len(list.Outcomes) != 1 || list.Outcomes[0].LossSHA256 != rep.LossSHA256 || list.Outcomes[0].Loss == nil || list.Outcomes[0].Loss.RunID != d.run.String() {
		t.Fatalf("owner list = %+v", list)
	}

	// confirm=discard is required, before any lookup.
	rec := bearerReqBody(d.router, http.MethodPost, d.ownerPath(d.workerA, "/blocked-outcomes/discard"), d.ownr, jsonBody(t, discardRequestFor(rep)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST without confirm = %d %s", rec.Code, rec.Body.String())
	}
	dir := decodeInto[apitypes.OutcomeDiscard](t, d.post(d.ownr, d.workerA, rep), 200)
	if dir.State != "authorized" || dir.WorkerID != d.workerA.String() || dir.LossSHA256 != rep.LossSHA256 {
		t.Fatalf("directive = %+v", dir)
	}
	status := func(token string) *httptest.ResponseRecorder {
		return bearerReq(d.router, http.MethodGet, d.ownerPath(d.workerA, "/outcome-discards/"+dir.ID), token)
	}
	if got := decodeInto[apitypes.OutcomeDiscard](t, status(d.ownr), 200); got.State != "authorized" {
		t.Fatalf("status = %+v", got)
	}

	disc := decodeInto[apitypes.WorkerOutcomeDiscardsResponse](t, rejectionHTTP(d.settleEnv, http.MethodGet, "/api/worker/outcome-discards", d.tokenA, ""), 200)
	if len(disc.Discards) != 1 || disc.Discards[0].ID != dir.ID {
		t.Fatalf("worker directives = %+v", disc)
	}
	begin := decodeInto[apitypes.OutcomeDiscard](t, rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/begin", d.tokenA, ""), 200)
	if begin.State != "executing" || begin.RemainingMS != 120000 || begin.JournalSHA256 != rep.JournalSHA256 || begin.ClaimGeneration != 3 {
		t.Fatalf("begin = %+v", begin)
	}
	d.advance(5 * time.Second)
	again := decodeInto[apitypes.OutcomeDiscard](t, rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/begin", d.tokenA, ""), 200)
	if again.RemainingMS != 115000 || !again.LeaseUntil.Equal(*begin.LeaseUntil) {
		t.Fatalf("repeated begin = %+v (first %+v)", again, begin)
	}
	// Strict request bodies: an unknown field is a 400, not a silent ignore.
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/complete", d.tokenA, `{"result":"deleted","reason":"","x":1}`); rec.Code != 400 {
		t.Fatalf("complete with an unknown field = %d", rec.Code)
	}
	done := decodeInto[apitypes.OutcomeDiscard](t, rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/complete", d.tokenA, `{"result":"deleted","reason":""}`), 200)
	if done.State != "completed" {
		t.Fatalf("complete = %+v", done)
	}
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/complete", d.tokenA, `{"result":"refused","reason":"x"}`); rec.Code != 409 {
		t.Fatalf("a different result = %d", rec.Code)
	}
	if got := decodeInto[apitypes.OutcomeDiscard](t, status(d.ownr), 200); got.State != "completed" {
		t.Fatalf("owner status after complete = %+v", got)
	}

	// Cancel route: a fresh generation, cancelled by its owner, then not cancellable twice differently.
	rep2 := d.report(d.workerA, d.run, 4, outcomeloss.ReasonRunNotOwned, 2)
	d.mustPut(d.tokenA, rep, rep2)
	dir2 := decodeInto[apitypes.OutcomeDiscard](t, d.post(d.ownr, d.workerA, rep2), 200)
	cancelled := decodeInto[apitypes.OutcomeDiscard](t, bearerReq(d.router, http.MethodPost, d.ownerPath(d.workerA, "/outcome-discards/"+dir2.ID+"/cancel"), d.ownr), 200)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancel = %+v", cancelled)
	}
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir2.ID+"/begin", d.tokenA, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatalf("begin after cancel = %d %s", rec.Code, rec.Body.String())
	}
}

// The principals that must be refused, on every owner route, through the real router.
func TestOutcomeDiscardAuthorityMatrixLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	rep := d.report(d.workerA, d.run, 1, outcomeloss.ReasonRunNotOwned, 1)

	// A second user's run, reported by the same worker, to prove it never reaches e.user.
	other := cliSeedUser(t, d.pool, false)
	otherRun := d.runFor(other)
	foreign := d.report(d.workerA, otherRun, 1, outcomeloss.ReasonRunNotOwned, 1)
	d.mustPut(d.tokenA, rep, foreign)

	list := decodeInto[apitypes.OwnerBlockedOutcomesResponse](t, bearerReq(d.router, http.MethodGet, d.ownerPath(d.workerA, "/blocked-outcomes"), d.ownr), 200)
	if len(list.Outcomes) != 1 || list.Outcomes[0].RunID != d.run.String() {
		t.Fatalf("the worker owner saw a run they do not own: %+v", list)
	}

	dir := decodeInto[apitypes.OutcomeDiscard](t, d.post(d.ownr, d.workerA, rep), 200)

	type principal struct{ name, token string }
	otherTok := cliMintToken(t, d.pool, other, clitoken.ScopeUser)
	admin := cliSeedUser(t, d.pool, true)
	adminDefault := cliMintToken(t, d.pool, admin, clitoken.ScopeUser)
	adminRO := cliMintToken(t, d.pool, admin, clitoken.ScopeAdminRO)
	for _, p := range []principal{
		{"another user", otherTok},
		{"admin (default scope) owning neither", adminDefault},
		{"admin (admin_ro scope) owning neither", adminRO},
	} {
		for _, call := range []struct{ name, method, path, body string }{
			{"list", http.MethodGet, d.ownerPath(d.workerA, "/blocked-outcomes"), ""},
			{"discard", http.MethodPost, d.ownerPath(d.workerA, "/blocked-outcomes/discard?confirm=discard"), jsonBody(t, discardRequestFor(rep))},
			{"status", http.MethodGet, d.ownerPath(d.workerA, "/outcome-discards/"+dir.ID), ""},
			{"cancel", http.MethodPost, d.ownerPath(d.workerA, "/outcome-discards/"+dir.ID+"/cancel"), ""},
		} {
			rec := bearerReqBody(d.router, call.method, call.path, p.token, call.body)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d %s, want 404", p.name, call.name, rec.Code, rec.Body.String())
			}
		}
	}

	// Run owner who does not own the worker: the other user's run, on e.user's worker.
	if rec := bearerReqBody(d.router, http.MethodPost, d.ownerPath(d.workerA, "/blocked-outcomes/discard?confirm=discard"), otherTok, jsonBody(t, discardRequestFor(foreign))); rec.Code != 404 {
		t.Errorf("run owner without the worker = %d %s", rec.Code, rec.Body.String())
	}
	// Worker owner who does not own the run: e.user naming the other user's run.
	if rec := bearerReqBody(d.router, http.MethodPost, d.ownerPath(d.workerA, "/blocked-outcomes/discard?confirm=discard"), d.ownr, jsonBody(t, discardRequestFor(foreign))); rec.Code != 404 {
		t.Errorf("worker owner without the run = %d %s", rec.Code, rec.Body.String())
	}
	// No credential at all.
	if rec := bearerReq(d.router, http.MethodGet, d.ownerPath(d.workerA, "/blocked-outcomes"), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", rec.Code)
	}
	// And nothing above created or changed a directive beyond the one the owner confirmed.
	var n int
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM worker_outcome_discards WHERE worker_id=$1`, d.workerA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("directives = %d, %v", n, err)
	}
}

func TestOutcomeDiscardCookieCSRFLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	rep := d.report(d.workerA, d.run, 1, outcomeloss.ReasonRunNotOwned, 1)
	d.mustPut(d.tokenA, rep)
	jwt := cliMintJWT(t, d.pool, d.user)
	path := d.ownerPath(d.workerA, "/blocked-outcomes/discard?confirm=discard")
	body := jsonBody(t, discardRequestFor(rep))

	// A cookie session with NO CSRF header: refused, and no directive exists afterwards.
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	d.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF = %d %s, want 403", rec.Code, rec.Body.String())
	}
	var n int
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM worker_outcome_discards WHERE worker_id=$1`, d.workerA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a CSRF-less POST created %d directives, %v", n, err)
	}
	// With a valid CSRF header it succeeds.
	dir := decodeInto[apitypes.OutcomeDiscard](t, cookieReq(t, d.router, http.MethodPost, path, jwt, body), 200)
	if dir.State != "authorized" {
		t.Fatalf("cookie+CSRF directive = %+v", dir)
	}
	// A cookie GET needs no CSRF.
	getReq := httptest.NewRequest(http.MethodGet, d.ownerPath(d.workerA, "/outcome-discards/"+dir.ID), nil)
	getReq.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request
	getRec := httptest.NewRecorder()
	d.router.ServeHTTP(getRec, getReq)
	if getRec.Code != 200 {
		t.Fatalf("cookie GET = %d %s", getRec.Code, getRec.Body.String())
	}
}

// Worker B (same owner, different identity) must reach nothing of worker A's.
func TestOutcomeDiscardWorkerIsolationLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	rep := d.report(d.workerA, d.run, 1, outcomeloss.ReasonRunNotOwned, 1)
	d.mustPut(d.tokenA, rep)
	dir := decodeInto[apitypes.OutcomeDiscard](t, d.post(d.ownr, d.workerA, rep), 200)

	list := decodeInto[apitypes.WorkerOutcomeDiscardsResponse](t, rejectionHTTP(d.settleEnv, http.MethodGet, "/api/worker/outcome-discards", d.tokenB, ""), 200)
	if len(list.Discards) != 0 {
		t.Fatalf("worker B listed worker A's directive: %+v", list)
	}
	for _, c := range []struct{ name, path, body string }{
		{"begin", "/begin", ""},
		{"complete", "/complete", `{"result":"deleted","reason":""}`},
		{"reconcile", "/reconcile", `{"observation":"absent"}`},
	} {
		if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+c.path, d.tokenB, c.body); rec.Code != http.StatusNotFound {
			t.Errorf("worker B %s = %d %s, want 404", c.name, rec.Code, rec.Body.String())
		}
	}
	// B cannot report on A's behalf: the manifest scope names worker A.
	if rec := d.put(d.tokenB, rep); rec.Code != http.StatusBadRequest {
		t.Errorf("worker B PUT of a manifest scoped to worker A = %d", rec.Code)
	}
	// B's own (empty) PUT does not disturb A's rows or A's directive.
	d.mustPut(d.tokenB)
	if got := decodeInto[apitypes.OutcomeDiscard](t, bearerReq(d.router, http.MethodGet, d.ownerPath(d.workerA, "/outcome-discards/"+dir.ID), d.ownr), 200); got.State != "authorized" {
		t.Fatalf("directive after worker B traffic = %+v", got)
	}
	var n int
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM worker_blocked_outcomes WHERE worker_id=$1`, d.workerA).Scan(&n); err != nil || n != 1 {
		t.Fatalf("worker A's reported rows = %d, %v", n, err)
	}
	// And A's own routes still work.
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/begin", d.tokenA, ""); rec.Code != 200 {
		t.Fatalf("worker A begin = %d", rec.Code)
	}
}

func TestOutcomeDiscardRefusalBodiesLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	rep := d.report(d.workerA, d.run, 1, outcomeloss.ReasonRunNotOwned, 1)
	code := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		r := decodeInto[apitypes.OutcomeDiscardRefusal](t, rec, http.StatusConflict)
		return r.Code
	}
	if got := code(d.post(d.ownr, d.workerA, rep)); got != "not_blocked" {
		t.Fatalf("nothing reported: %s", got)
	}
	d.mustPut(d.tokenA, d.report(d.workerA, d.run, 1, "reserve_exhausted", 1))
	if got := code(d.post(d.ownr, d.workerA, rep)); got != "reason_not_discardable" {
		t.Fatalf("other reason: %s", got)
	}
	d.mustPut(d.tokenA, rep)
	changed := rep
	changed.LossSHA256 = discardJournalHex(9)
	if got := code(d.post(d.ownr, d.workerA, changed)); got != "loss_changed" {
		t.Fatalf("stale digest: %s", got)
	}
	d.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, d.run, d.workerA)
	if got := code(d.post(d.ownr, d.workerA, rep)); got != "attributed" {
		t.Fatalf("attributed: %s", got)
	}
	d.exec(`UPDATE runs SET worker_id = NULL WHERE id = $1`, d.run)
	hold := d.insertHold(d.run, 1, d.workerA)
	if got := code(d.post(d.ownr, d.workerA, rep)); got != "custody_hold_open" {
		t.Fatalf("open hold: %s", got)
	}
	d.exec(`UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL WHERE id=$1`, hold)
	dir := decodeInto[apitypes.OutcomeDiscard](t, d.post(d.ownr, d.workerA, rep), 200)
	rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/begin", d.tokenA, "")
	if got := code(d.post(d.ownr, d.workerA, rep)); got != "execution_unresolved" {
		t.Fatalf("executing: %s", got)
	}
	// Reconcile settles it without a reported row; then a new confirmation is possible again.
	d.mustPut(d.tokenA)
	rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/reconcile", d.tokenA, `{"observation":"absent"}`)
	if got := decodeInto[apitypes.OutcomeDiscard](t, rec, 200); got.State != "completed_unknown" || got.Reason != "journal_absent_after_begin" {
		t.Fatalf("reconcile = %+v", got)
	}
	if rec := rejectionHTTP(d.settleEnv, http.MethodPost, "/api/worker/outcome-discards/"+dir.ID+"/reconcile", d.tokenA, `{"observation":"nope"}`); rec.Code != 400 {
		t.Fatalf("bad observation = %d", rec.Code)
	}
}

func TestOutcomeDiscardPutLimitsLiveDB(t *testing.T) {
	d := newDiscardHTTP(t)
	one := d.report(d.workerA, d.run, 1, outcomeloss.ReasonRunNotOwned, 1)
	nine := make([]apitypes.BlockedOutcomeReport, 9)
	for i := range nine {
		nine[i] = d.report(d.workerA, d.run, int64(i), outcomeloss.ReasonRunNotOwned, 1)
	}
	if rec := d.put(d.tokenA, nine...); rec.Code != http.StatusBadRequest {
		t.Fatalf("nine outcomes = %d", rec.Code)
	}
	if rec := d.put(d.tokenA, one, one); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate = %d", rec.Code)
	}
	bad := one
	bad.LossSHA256 = discardJournalHex(5)
	if rec := d.put(d.tokenA, bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("digest mismatch = %d", rec.Code)
	}
	huge := rejectionHTTP(d.settleEnv, http.MethodPut, "/api/worker/blocked-outcomes", d.tokenA, `{"outcomes":[],"pad":"`+strings.Repeat("a", outcomeloss.MaxPutBodyBytes)+`"}`)
	if huge.Code != http.StatusRequestEntityTooLarge && huge.Code != http.StatusBadRequest {
		t.Fatalf("oversize body = %d", huge.Code)
	}
	// None of the rejected PUTs stored anything.
	var n int
	if err := d.pool.QueryRow(d.ctx, fmt.Sprintf(`SELECT count(*) FROM worker_blocked_outcomes WHERE worker_id='%s'`, d.workerA)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after rejected PUTs = %d, %v", n, err)
	}
}
