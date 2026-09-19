package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1432 M2: the member "request an override" handler (RequestGuardrailOverride) and
// the enrichment it drives. These reuse the enable-gate live-DB harness from
// repo_enable_guardrail_livedb_test.go (fakeGuardForge, enableGuardFixture, seedRepo):
// the request handler re-runs the SAME live guard SetRepoEnabled runs, so its verdicts
// are derived from real branch-protection JSON, not a fake store. The reason-validation
// cases need no DB (validation precedes any DB/forge read) and run in a plain go test.

// requestOverride drives RequestGuardrailOverride as user against repoID with the given
// reason and returns the recorder. Mirrors the fixture's setEnabled shape.
func (f enableGuardFixture) requestOverride(t *testing.T, user store.User, repoID uuid.UUID, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"reason": reason})
	r := httptest.NewRequest(http.MethodPost, "/repos/x/override-request", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.RequestGuardrailOverride(w, r)
	return w
}

// overrideRequestRow reads the single persisted request for a repo (reason/status +
// decoded findings) plus the row count, so a test can assert what was stored and that a
// re-request updated the same row rather than piling up a duplicate.
func (f enableGuardFixture) overrideRequestRow(ctx context.Context, t *testing.T, repoID uuid.UUID) (count int, reason, status string, findings []apitypes.GuardrailFindingDTO) {
	t.Helper()
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM guardrail_override_requests WHERE repo_id = $1`, repoID).Scan(&count); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if count == 0 {
		return 0, "", "", nil
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT reason, status, findings FROM guardrail_override_requests WHERE repo_id = $1`, repoID,
	).Scan(&reason, &status, &raw); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if err := json.Unmarshal(raw, &findings); err != nil {
		t.Fatalf("decode stored findings %s: %v", raw, err)
	}
	return count, reason, status, findings
}

func hasFindingCode(findings []apitypes.GuardrailFindingDTO, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// A waivable live refusal (unprotected default branch) → 200, a pending row persisted
// with the reason + the coded block findings; a second request (a re-request) UPDATES
// the same row rather than piling up a duplicate (the one-pending-per-repo upsert).
func TestRequestGuardrailOverrideWaivablePersistsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 201, false, protUnprotected)

	const reason = "our platform team manages branch protection centrally; please allow this repo"
	w := f.requestOverride(t, f.owner, repoID, reason)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		OverrideRequest apitypes.OverrideRequestStateDTO `json:"override_request"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 200 body: %v (body %s)", err, w.Body.String())
	}
	if resp.OverrideRequest.Status != "pending" {
		t.Errorf("status = %q, want pending", resp.OverrideRequest.Status)
	}
	if resp.OverrideRequest.Reason != reason {
		t.Errorf("reason = %q, want %q", resp.OverrideRequest.Reason, reason)
	}
	if resp.OverrideRequest.DecidedAt != nil || resp.OverrideRequest.DecisionNote != nil {
		t.Errorf("a pending request must have null decided_at/decision_note, got %+v", resp.OverrideRequest)
	}

	count, gotReason, gotStatus, findings := f.overrideRequestRow(ctx, t, repoID)
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
	if gotStatus != "pending" || gotReason != reason {
		t.Errorf("persisted status/reason = %q/%q, want pending/%q", gotStatus, gotReason, reason)
	}
	if !hasFindingCode(findings, "default_branch_unprotected") {
		t.Errorf("persisted findings = %+v, want a default_branch_unprotected code", findings)
	}

	// Re-request with a new reason: the open pending row is updated in place.
	const reason2 = "updated: protection is enforced via our CI ruleset, not branch protection"
	w2 := f.requestOverride(t, f.owner, repoID, reason2)
	if w2.Code != http.StatusOK {
		t.Fatalf("re-request status = %d, want 200 (body %s)", w2.Code, w2.Body.String())
	}
	count2, gotReason2, _, _ := f.overrideRequestRow(ctx, t, repoID)
	if count2 != 1 {
		t.Errorf("row count after re-request = %d, want 1 (idempotent per repo)", count2)
	}
	if gotReason2 != reason2 {
		t.Errorf("reason after re-request = %q, want %q", gotReason2, reason2)
	}
}

// A repo owned by another user is a 404 for the requester (owner-scoped GetRepoForUser),
// resolved before any forge read; no row is persisted.
func TestRequestGuardrailOverrideNonOwnerIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 202, false, protUnprotected)

	stranger := store.User{ID: uuid.New()}
	w := f.requestOverride(t, stranger, repoID, "let me in")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-owned repo (body %s)", w.Code, w.Body.String())
	}
	if count, _, _, _ := f.overrideRequestRow(ctx, t, repoID); count != 0 {
		t.Errorf("a 404 request must persist no row, got %d", count)
	}
}

// The shared reason validator rejects an empty, control-character, or over-long reason
// BEFORE any DB/forge read, so these need no live DB.
func TestRequestGuardrailOverrideReasonValidation(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   int
	}{
		{"empty", "", http.StatusBadRequest},
		{"whitespace only", "   \t ", http.StatusBadRequest},
		{"newline (forged audit line)", "looks fine\nActor: someone-else approved", http.StatusBadRequest},
		{"too long", strings.Repeat("x", maxGuardrailOverrideReasonBytes+1), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{} // nil q/pcheck: validation precedes any DB/forge access
			body, _ := json.Marshal(map[string]string{"reason": tc.reason})
			r := httptest.NewRequest(http.MethodPost, "/repos/x/override-request", bytes.NewReader(body))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", uuid.New().String())
			r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), store.User{ID: uuid.New()}), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			h.RequestGuardrailOverride(w, r)
			if w.Code != tc.want {
				t.Fatalf("reason %q → status %d, want %d (body %s)", tc.reason, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// A repo the live guard does NOT block → 409 (nothing to request; enable it directly);
// no row is persisted.
func TestRequestGuardrailOverrideNotBlockedIs409LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 203, false, protClean)

	w := f.requestOverride(t, f.owner, repoID, "please allow this repo")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an unblocked repo (body %s)", w.Code, w.Body.String())
	}
	if count, _, _, _ := f.overrideRequestRow(ctx, t, repoID); count != 0 {
		t.Errorf("a 409 (not blocked) must persist no row, got %d", count)
	}
}

// A non-waivable refusal (protection_unreadable — the fail-closed case an admin override
// can never waive) → 422 with "waivable": false and NO row persisted.
func TestRequestGuardrailOverrideNonWaivableIs422LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	repoID := f.seedRepo(ctx, t, 204, false, protError)

	w := f.requestOverride(t, f.owner, repoID, "please allow this repo")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a non-waivable refusal (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error      string   `json:"error"`
		Violations []string `json:"violations"`
		Waivable   bool     `json:"waivable"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 422 body: %v (body %s)", err, w.Body.String())
	}
	if resp.Waivable {
		t.Errorf("waivable = true, want false for protection_unreadable")
	}
	if len(resp.Violations) == 0 {
		t.Errorf("violations = %v, want a non-empty block-finding list", resp.Violations)
	}
	if count, _, _, _ := f.overrideRequestRow(ctx, t, repoID); count != 0 {
		t.Errorf("a non-waivable 422 must persist no row, got %d", count)
	}
}

// The SetRepoEnabled 422 now carries the machine-readable waivability the member UI
// drives its affordance off (PRD #1432): waivable:true + coded findings for a waivable
// block, waivable:false for protection_unreadable.
func TestSetRepoEnable422CarriesWaivabilityLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)

	type body struct {
		Waivable bool                           `json:"waivable"`
		Findings []apitypes.GuardrailFindingDTO `json:"findings"`
	}

	// Waivable block: unprotected default branch.
	waivableRepo := f.seedRepo(ctx, t, 205, false, protUnprotected)
	w := f.setEnabled(t, waivableRepo, true)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}
	var wb body
	if err := json.Unmarshal(w.Body.Bytes(), &wb); err != nil {
		t.Fatalf("decode 422 body: %v (body %s)", err, w.Body.String())
	}
	if !wb.Waivable {
		t.Errorf("waivable = false, want true for an unprotected-branch block")
	}
	if !hasFindingCode(wb.Findings, "default_branch_unprotected") {
		t.Errorf("findings = %+v, want a default_branch_unprotected code", wb.Findings)
	}

	// Non-waivable: protection read error → protection_unreadable.
	unreadableRepo := f.seedRepo(ctx, t, 206, false, protError)
	w2 := f.setEnabled(t, unreadableRepo, true)
	if w2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreadable status = %d, want 422 (body %s)", w2.Code, w2.Body.String())
	}
	var ub body
	if err := json.Unmarshal(w2.Body.Bytes(), &ub); err != nil {
		t.Fatalf("decode unreadable 422 body: %v (body %s)", err, w2.Body.String())
	}
	if ub.Waivable {
		t.Errorf("waivable = true, want false for protection_unreadable")
	}
	if !hasFindingCode(ub.Findings, "protection_unreadable") {
		t.Errorf("findings = %+v, want a protection_unreadable code", ub.Findings)
	}
}

// PRD #1432 M3: the admin approve/reject of a member override request. These reuse the
// enable-gate live-DB harness (a real GitLab httptest server behind the real driver + a
// real privcheck.Service), so the CROSS-USER regression exercises the real live guard:
// approving a request sets the EXISTING #66 override, and the member's retry-Enable then
// downgrades the waivable finding exactly as the M8 override path does. A real notifier
// is wired over the live-DB store (notifysvc.New(f.h.q, …)) so the decided-kind inbox row
// is actually inserted and asserted.

// decideRequest POSTs approve (or reject) for reqID as actor, with an OPTIONAL decision
// note (empty ⇒ no body ⇒ the tolerant "no note" path). Returns the recorder.
func (f enableGuardFixture) decideRequest(t *testing.T, actor store.User, reqID uuid.UUID, approve bool, note string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/admin/override-requests/x/reject"
	if approve {
		path = "/admin/override-requests/x/approve"
	}
	var r *http.Request
	if note == "" {
		r = httptest.NewRequest(http.MethodPost, path, nil)
	} else {
		body, _ := json.Marshal(map[string]string{"decision_note": note})
		r = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", reqID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), actor), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	if approve {
		f.h.ApproveGuardrailOverrideRequest(w, r)
	} else {
		f.h.RejectGuardrailOverrideRequest(w, r)
	}
	return w
}

// overrideBy reads guardrail_override_by as text (NULL → ""), for asserting the approving
// admin is recorded as the live override actor.
func (f enableGuardFixture) overrideBy(ctx context.Context, t *testing.T, repoID uuid.UUID) string {
	t.Helper()
	var by *string
	if err := f.pool.QueryRow(ctx, `SELECT guardrail_override_by::text FROM repos WHERE id = $1`, repoID).Scan(&by); err != nil {
		t.Fatalf("read override by: %v", err)
	}
	if by == nil {
		return ""
	}
	return *by
}

// requestDecision reads the settled request's status + decided_by (as text, NULL → "").
func (f enableGuardFixture) requestDecision(ctx context.Context, t *testing.T, reqID uuid.UUID) (status, decidedBy string) {
	t.Helper()
	var decidedByN *string
	if err := f.pool.QueryRow(ctx,
		`SELECT status, decided_by::text FROM guardrail_override_requests WHERE id = $1`, reqID,
	).Scan(&status, &decidedByN); err != nil {
		t.Fatalf("read request decision: %v", err)
	}
	if decidedByN != nil {
		decidedBy = *decidedByN
	}
	return status, decidedBy
}

// notificationCount counts inbox rows of a kind for a user, so the decided-kind notify is
// observable through the real notifysvc write.
func (f enableGuardFixture) notificationCount(ctx context.Context, t *testing.T, userID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = $2`, userID, kind,
	).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// Approve: → 200; (i) the repo carries the request reason + admin actor as the override;
// (ii) the request is approved, decided_by the admin; (iii) the repo is STILL disabled
// (approval must not enable); (iv) a decided-kind notification exists for the requester.
func TestApproveGuardrailOverrideRequestLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	f.h.SetNotifier(notifysvc.New(f.h.q, nil, 0, nil))
	admin := f.mkAdmin(ctx, t)

	repoID := f.seedRepo(ctx, t, 301, false, protUnprotected)
	const reason = "protection is enforced by our CI ruleset, not branch protection; please allow this repo"
	reqID := f.seedPendingRequest(ctx, t, repoID, f.owner.ID, reason, []apitypes.GuardrailFindingDTO{
		{Code: "default_branch_unprotected", Severity: "block", Message: "the default branch is not protected"},
	})

	w := f.decideRequest(t, admin, reqID, true, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Request apitypes.OverrideRequestStateDTO `json:"request"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 200 body: %v (body %s)", err, w.Body.String())
	}
	if resp.Request.Status != "approved" {
		t.Errorf("response status = %q, want approved", resp.Request.Status)
	}

	// (i) the repo now carries the override with the request reason + admin actor.
	gotReason, set := f.overrideReason(ctx, t, repoID)
	if !set {
		t.Fatalf("approve must set the repo override")
	}
	if gotReason != reason {
		t.Errorf("override reason = %q, want the request reason %q", gotReason, reason)
	}
	if by := f.overrideBy(ctx, t, repoID); by != admin.ID.String() {
		t.Errorf("override by = %q, want the admin id %q", by, admin.ID.String())
	}
	// (ii) the request row is approved, decided_by the admin.
	status, decidedBy := f.requestDecision(ctx, t, reqID)
	if status != "approved" {
		t.Errorf("request status = %q, want approved", status)
	}
	if decidedBy != admin.ID.String() {
		t.Errorf("request decided_by = %q, want the admin id %q", decidedBy, admin.ID.String())
	}
	// (iii) approval must NOT enable the repo.
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("approval must not enable the repo — it stays disabled until the member retries Enable")
	}
	// (iv) a decided-kind notification exists for the requester.
	if n := f.notificationCount(ctx, t, f.owner.ID, guardrailOverrideDecidedKind); n != 1 {
		t.Errorf("requester notifications of kind %q = %d, want 1", guardrailOverrideDecidedKind, n)
	}
}

// Reject: → 200; the repo has NO override set; the request is rejected, decided_by the
// admin; a decided-kind notification exists. The decision note is optional and screened.
func TestRejectGuardrailOverrideRequestLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	f.h.SetNotifier(notifysvc.New(f.h.q, nil, 0, nil))
	admin := f.mkAdmin(ctx, t)

	repoID := f.seedRepo(ctx, t, 302, false, protUnprotected)
	reqID := f.seedPendingRequest(ctx, t, repoID, f.owner.ID, "please allow this repo", []apitypes.GuardrailFindingDTO{
		{Code: "default_branch_unprotected", Severity: "block", Message: "the default branch is not protected"},
	})

	w := f.decideRequest(t, admin, reqID, false, "fix branch protection on the forge first, then retry Enable")
	if w.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	// No override set by a rejection.
	if _, set := f.overrideReason(ctx, t, repoID); set {
		t.Errorf("reject must NOT set the repo override")
	}
	// The request is rejected, decided_by the admin.
	status, decidedBy := f.requestDecision(ctx, t, reqID)
	if status != "rejected" {
		t.Errorf("request status = %q, want rejected", status)
	}
	if decidedBy != admin.ID.String() {
		t.Errorf("request decided_by = %q, want the admin id %q", decidedBy, admin.ID.String())
	}
	// A notification exists for the requester.
	if n := f.notificationCount(ctx, t, f.owner.ID, guardrailOverrideDecidedKind); n != 1 {
		t.Errorf("requester notifications of kind %q = %d, want 1", guardrailOverrideDecidedKind, n)
	}
}

// Already-decided → 409, and an unknown request id → 404. The single-shot guard (`AND
// status = 'pending'`) is what makes the second decide a conflict, not a silent re-write.
func TestDecideGuardrailOverrideRequestConflictAndNotFoundLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	f.h.SetNotifier(notifysvc.New(f.h.q, nil, 0, nil))
	admin := f.mkAdmin(ctx, t)

	repoID := f.seedRepo(ctx, t, 303, false, protUnprotected)
	reqID := f.seedPendingRequest(ctx, t, repoID, f.owner.ID, "please allow this repo", []apitypes.GuardrailFindingDTO{
		{Code: "default_branch_unprotected", Severity: "block", Message: "the default branch is not protected"},
	})

	if w := f.decideRequest(t, admin, reqID, true, ""); w.Code != http.StatusOK {
		t.Fatalf("first approve status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	// Second decide on the now-approved request → 409.
	if w := f.decideRequest(t, admin, reqID, true, ""); w.Code != http.StatusConflict {
		t.Fatalf("second approve status = %d, want 409 (already decided) (body %s)", w.Code, w.Body.String())
	}
	// An unknown request id → 404.
	if w := f.decideRequest(t, admin, uuid.New(), true, ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown-id status = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}

// THE KEY AC — the full cross-user regression. A member owns a repo whose live guard
// returns a WAIVABLE block; the member requests an override, the admin queue surfaces it,
// the admin approves (override set, repo still disabled), and the member's retry-Enable
// now succeeds because the live guard downgrades the waivable finding under the active
// override. Every step's status is asserted, ending in enabled == true.
func TestGuardrailOverrideRequestCrossUserFlowLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	f.h.SetNotifier(notifysvc.New(f.h.q, nil, 0, nil))
	admin := f.mkAdmin(ctx, t)

	// A waivable-block repo (unprotected default branch), owned by the member, disabled.
	repoID := f.seedRepo(ctx, t, 304, false, protUnprotected)

	// The enable is refused before any override exists.
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pre-request enable status = %d, want 422 (body %s)", w.Code, w.Body.String())
	}

	// 1. Member requests an override → 200 pending.
	const reason = "our platform team manages branch protection centrally; please allow this repo"
	if w := f.requestOverride(t, f.owner, repoID, reason); w.Code != http.StatusOK {
		t.Fatalf("request status = %d, want 200 pending (body %s)", w.Code, w.Body.String())
	}

	// 2. The admin cross-user queue surfaces the request; take its id from the queue.
	rq := httptest.NewRequest(http.MethodGet, "/admin/blocked-repos", nil)
	rq = rq.WithContext(mw.ContextWithUser(rq.Context(), admin))
	wq := httptest.NewRecorder()
	f.h.AdminListBlockedRepos(wq, rq)
	if wq.Code != http.StatusOK {
		t.Fatalf("admin queue status = %d, want 200 (body %s)", wq.Code, wq.Body.String())
	}
	var q apitypes.AdminBlockedReposDTO
	if err := json.Unmarshal(wq.Body.Bytes(), &q); err != nil {
		t.Fatalf("decode queue: %v (body %s)", err, wq.Body.String())
	}
	var reqID string
	for _, req := range q.Requests {
		if req.RepoID == repoID.String() {
			reqID = req.ID
			if req.Reason != reason {
				t.Errorf("queue request reason = %q, want %q", req.Reason, reason)
			}
			break
		}
	}
	if reqID == "" {
		t.Fatalf("the pending request for repo %s must appear in the admin queue; got %+v", repoID, q.Requests)
	}
	reqUUID, err := uuid.Parse(reqID)
	if err != nil {
		t.Fatalf("queue request id %q is not a uuid: %v", reqID, err)
	}

	// 3. Admin approves → 200: the override is set, the repo is STILL disabled.
	if w := f.decideRequest(t, admin, reqUUID, true, ""); w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if _, set := f.overrideReason(ctx, t, repoID); !set {
		t.Fatalf("approve must set the repo override")
	}
	if f.repoEnabled(ctx, t, repoID) {
		t.Errorf("approval must NOT enable the repo")
	}

	// 4. Member retries Enable → 200: the live guard downgrades the waivable finding
	// under the active override, so the enable that was refused in step 0 now succeeds.
	if w := f.setEnabled(t, repoID, true); w.Code != http.StatusOK {
		t.Fatalf("enable-after-approve status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !f.repoEnabled(ctx, t, repoID) {
		t.Errorf("the repo must be enabled after the member retries Enable with an active override")
	}
}
