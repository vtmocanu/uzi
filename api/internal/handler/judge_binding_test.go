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
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// judgeBindStore is the workersvc.Store slice PUT /api/me/judge's token half needs.
// It embeds the interface so any query beyond these two panics rather than returning
// a zero value that quietly passes. Ownership is modelled exactly as the SQL does:
// a secret whose owner is not the caller is indistinguishable from a missing one.
type judgeBindStore struct {
	workersvc.Store

	secrets map[uuid.UUID]uuid.UUID // secret id → owner
	labels  map[string]uuid.UUID    // "owner|label" → secret id

	setCalled bool
	setArg    store.SetUserJudgeAnthropicBindingParams

	// retUser, when non-nil, is the store.User SetUserJudgeAnthropicBinding hands back
	// INSTEAD of echoing arg. It lets a test stage the exact stored row toDTO must
	// render — in particular the FK-nulled 'pinned'+NULL state a coupling CHECK cannot
	// forbid (00088), which no valid request can otherwise produce.
	retUser *store.User
}

func (j *judgeBindStore) GetUserSecretIDByLabel(_ context.Context, arg store.GetUserSecretIDByLabelParams) (uuid.UUID, error) {
	id, ok := j.labels[arg.UserID.String()+"|"+arg.Label]
	if !ok {
		return uuid.UUID{}, pgx.ErrNoRows
	}
	return id, nil
}

func (j *judgeBindStore) GetUserSecretCiphertextByID(_ context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	owner, ok := j.secrets[arg.ID]
	if !ok || owner != arg.UserID {
		return store.GetUserSecretCiphertextByIDRow{}, pgx.ErrNoRows
	}
	return store.GetUserSecretCiphertextByIDRow{UserID: owner, Kind: store.KindAnthropicToken}, nil
}

func (j *judgeBindStore) SetUserJudgeAnthropicBinding(_ context.Context, arg store.SetUserJudgeAnthropicBindingParams) (store.User, error) {
	j.setCalled = true
	j.setArg = arg
	if j.retUser != nil {
		return *j.retUser, nil
	}
	return store.User{
		ID:                     arg.ID,
		JudgeAnthropicBindMode: arg.JudgeAnthropicBindMode,
		JudgeAnthropicSecretID: arg.JudgeAnthropicSecretID,
	}, nil
}

func newJudgeBindHandler(t *testing.T, db *fakeUserDB, st *judgeBindStore) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{q: store.New(db), wsvc: workersvc.New(st, box, workersvc.Params{})}
}

func judgeReq(user uuid.UUID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/me/judge", strings.NewReader(body))
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: user, IsActive: true}))
}

// TestSetJudgeBindsByLabel: the happy path — a label the caller owns resolves and is
// written against the SESSION user.
func TestSetJudgeBindsByLabel(t *testing.T) {
	owner, secretID := uuid.New(), uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: owner},
		labels:  map[string]uuid.UUID{owner.String() + "|cheap-console": secretID},
	}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":"cheap-console"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.setCalled {
		t.Fatal("the judge binding was never written")
	}
	if st.setArg.ID != owner {
		t.Fatalf("binding written for %v, want the SESSION user %v", st.setArg.ID, owner)
	}
	if !st.setArg.JudgeAnthropicSecretID.Valid || uuid.UUID(st.setArg.JudgeAnthropicSecretID.Bytes) != secretID {
		t.Fatalf("bound to %+v, want %v", st.setArg.JudgeAnthropicSecretID, secretID)
	}
	var env struct {
		User struct {
			JudgeAnthropicSecretLabel *string `json:"judge_anthropic_secret_label"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.User.JudgeAnthropicSecretLabel == nil || *env.User.JudgeAnthropicSecretLabel != "cheap-console" {
		t.Fatalf("response label = %v, want cheap-console", env.User.JudgeAnthropicSecretLabel)
	}
}

// TestSetJudgeAbsentTokenLeavesBindingAlone is the compatibility guarantee: every
// existing client PUTs {"enabled":…} with no token field, and that must NOT unbind
// the user's judge credential. Absent and explicit-null are different requests.
func TestSetJudgeAbsentTokenLeavesBindingAlone(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("an absent anthropic_token must leave the binding untouched — " +
			"every pre-M4 client sends exactly this body, and would silently unbind itself")
	}
}

// TestSetJudgeNullTokenClears: explicit null is a CLEAR and must reach the store as
// a NULL binding — distinct from omitting the key, which the test above pins as a
// no-op. Telling those two apart is the entire reason the field is a
// json.RawMessage; with a *string this test and the one above could not both pass.
func TestSetJudgeNullTokenClears(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":null}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.setCalled {
		t.Fatal("explicit null must clear the binding")
	}
	if st.setArg.JudgeAnthropicSecretID.Valid {
		t.Fatalf("wrote %+v, want a NULL binding", st.setArg.JudgeAnthropicSecretID)
	}
	// A clear must write mode `default`, NOT `auto` — `auto` also carries a NULL
	// pointer, so without this a pre-M2 client clearing its judge token would
	// silently land on the pool (PRD #1140 success criterion 5).
	if st.setArg.JudgeAnthropicBindMode != workersvc.BindModeDefault {
		t.Fatalf("clear wrote mode %q, want default", st.setArg.JudgeAnthropicBindMode)
	}
}

// TestSetJudgeMalformedTokenIs400: a number or an object is a client bug, and must
// be named rather than silently treated as "absent".
func TestSetJudgeMalformedTokenIs400(t *testing.T) {
	owner := uuid.New()
	for _, body := range []string{`{"enabled":true,"anthropic_token":7}`, `{"enabled":true,"anthropic_token":{"a":1}}`} {
		st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
		h := newJudgeBindHandler(t, &fakeUserDB{}, st)
		rec := httptest.NewRecorder()
		h.SetJudgeEnabled(rec, judgeReq(owner, body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400", body, rec.Code)
		}
		if st.setCalled {
			t.Fatalf("%s: a malformed value must not reach the UPDATE", body)
		}
	}
}

// TestSetJudgeEmptyTokenClears: an empty string is the explicit clear a shell or a
// form sends for "none".
func TestSetJudgeEmptyTokenClears(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":""}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.setCalled {
		t.Fatal("an empty label must clear the binding")
	}
	if st.setArg.JudgeAnthropicSecretID.Valid {
		t.Fatalf("wrote %+v, want a NULL binding", st.setArg.JudgeAnthropicSecretID)
	}
	// A clear writes mode `default`, not `auto` (see TestSetJudgeNullTokenClears).
	if st.setArg.JudgeAnthropicBindMode != workersvc.BindModeDefault {
		t.Fatalf("clear wrote mode %q, want default", st.setArg.JudgeAnthropicBindMode)
	}
}

// TestSetJudgeRefusesForeignSecret is the handler half of D11 for the judge lane:
// a label resolving to ANOTHER user's secret is refused before any write. The schema
// half — the same binding refused with this check bypassed — is
// TestUserJudgeBindingLiveDB.
func TestSetJudgeRefusesForeignSecret(t *testing.T) {
	owner, stranger := uuid.New(), uuid.New()
	foreignSecret := uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{foreignSecret: stranger},
		labels:  map[string]uuid.UUID{owner.String() + "|theirs": foreignSecret},
	}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":"theirs"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 for another user's secret; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("the handler wrote a cross-user judge binding — the ownership check did not run before the UPDATE")
	}
}

// TestSetJudgeUnknownLabelIs400: a label the caller has no token under is a 400,
// not a silent fallback to the default.
func TestSetJudgeUnknownLabelIs400(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"anthropic_token":"nope"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("an unknown label must not reach the UPDATE")
	}
}

// TestSetJudgeAutoModeWritesAutoWithNullPointer (PRD #1140 M2): judge_bind_mode:"auto"
// with no token writes mode 'auto' and a NULL pointer — the pool, not the default.
func TestSetJudgeAutoModeWritesAutoWithNullPointer(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"auto"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.setCalled {
		t.Fatal("a judge_bind_mode with no token must still write the binding")
	}
	if st.setArg.JudgeAnthropicBindMode != workersvc.BindModeAuto {
		t.Fatalf("mode = %q, want auto", st.setArg.JudgeAnthropicBindMode)
	}
	if st.setArg.JudgeAnthropicSecretID.Valid {
		t.Fatalf("auto mode wrote a pointer %+v, want NULL", st.setArg.JudgeAnthropicSecretID)
	}
}

// decodeJudgeBindMode reads the RESPONSE envelope of PUT /api/me/judge and returns the
// judge_anthropic_bind_mode field toDTO emitted — the effective mode a client sees, not
// the raw stored column.
func decodeJudgeBindMode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		User struct {
			JudgeAnthropicBindMode string `json:"judge_anthropic_bind_mode"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode response envelope: %v; body=%s", err, rec.Body.String())
	}
	return env.User.JudgeAnthropicBindMode
}

// TestSetJudgeResponseReportsEffectiveModeForNulledPin (PRD #1140 M2, D6) is the
// response-layer guard on toDTO's effectiveBindMode correction. It stages the row a
// coupling CHECK cannot forbid (00088): a 'pinned' binding whose secret pointer was
// nulled by the FK's ON DELETE SET NULL. The stored column still reads "pinned", but the
// pointer is gone, so the effective mode is "default" — and the RESPONSE must report
// "default", never the stale "pinned". Dropping the effectiveBindMode call in toDTO
// (rendering u.JudgeAnthropicBindMode raw) leaks "pinned" here and turns this red.
func TestSetJudgeResponseReportsEffectiveModeForNulledPin(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{},
		labels:  map[string]uuid.UUID{},
		// The FK-nulled state: mode still 'pinned', pointer NULL. No valid PUT can write
		// this (pinned-without-a-label is a 400), so it is staged as the stored row the
		// write returns; the request below only needs to reach the binding path.
		retUser: &store.User{
			ID:                     owner,
			JudgeAnthropicBindMode: workersvc.BindModePinned,
			JudgeAnthropicSecretID: pgtype.UUID{Valid: false},
		},
	}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"auto"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeBindMode(t, rec); got != workersvc.BindModeDefault {
		t.Fatalf("response judge_anthropic_bind_mode = %q, want %q — a 'pinned' row with a "+
			"NULL pointer must render as the effective mode, not the stale stored value",
			got, workersvc.BindModeDefault)
	}
}

// TestSetJudgeResponseReportsAutoMode is the pass-through direction: a row whose mode is
// 'auto' (pointer NULL) renders unchanged as "auto" in the response — effectiveBindMode
// only corrects the pinned+NULL contradiction, everything else is reported verbatim.
func TestSetJudgeResponseReportsAutoMode(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"auto"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeBindMode(t, rec); got != workersvc.BindModeAuto {
		t.Fatalf("response judge_anthropic_bind_mode = %q, want %q", got, workersvc.BindModeAuto)
	}
}

// TestSetJudgeResponseReportsPinnedMode: a genuinely pinned row (mode 'pinned' WITH a
// live pointer) renders as "pinned" — the effective mode and the stored mode agree, so
// the correction is a no-op and the response reports the real binding.
func TestSetJudgeResponseReportsPinnedMode(t *testing.T) {
	owner, secretID := uuid.New(), uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: owner},
		labels:  map[string]uuid.UUID{owner.String() + "|cheap-console": secretID},
	}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"pinned","anthropic_token":"cheap-console"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeJudgeBindMode(t, rec); got != workersvc.BindModePinned {
		t.Fatalf("response judge_anthropic_bind_mode = %q, want %q", got, workersvc.BindModePinned)
	}
}

// TestSetJudgePinnedModeNeedsLabel: judge_bind_mode:"pinned" with no token label is a
// 400 (mirrors PatchWorker), before any write.
func TestSetJudgePinnedModeNeedsLabel(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"pinned","anthropic_token":null}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("pinned-without-a-label must not reach the UPDATE")
	}
}

// TestSetJudgeAutoModeRejectsLabel: judge_bind_mode:"auto" (or default) WITH a token
// label is a 400 — the two contradict.
func TestSetJudgeAutoModeRejectsLabel(t *testing.T) {
	owner, secretID := uuid.New(), uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: owner},
		labels:  map[string]uuid.UUID{owner.String() + "|cheap-console": secretID},
	}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"auto","anthropic_token":"cheap-console"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("auto-with-a-label must not reach the UPDATE")
	}
}

// TestSetJudgeInvalidModeIs400: a mode outside the closed set is a named 400, not a 500
// from the CHECK.
func TestSetJudgeInvalidModeIs400(t *testing.T) {
	owner := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newJudgeBindHandler(t, &fakeUserDB{}, st)

	rec := httptest.NewRecorder()
	h.SetJudgeEnabled(rec, judgeReq(owner, `{"enabled":true,"judge_bind_mode":"sometimes"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("an invalid mode must not reach the UPDATE")
	}
}

// TestAdminJudgeToggleIgnoresMode is the admin-route half of D-audit-H3 for the mode:
// an admin body carrying judge_bind_mode must leave the target's binding untouched, the
// same way it ignores anthropic_token.
func TestAdminJudgeToggleIgnoresMode(t *testing.T) {
	target := uuid.New()
	st := &judgeBindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	db := &fakeUserDB{}
	h := newJudgeBindHandler(t, db, st)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/x/judge",
		strings.NewReader(`{"enabled":true,"judge_bind_mode":"auto"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", target.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.SetUserJudgeEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("the admin toggle should still flip judge_enabled")
	}
	if st.setCalled {
		t.Fatal("the admin toggle honored judge_bind_mode — an admin must not switch a user onto the pool")
	}
}

// TestAdminJudgeToggleIgnoresToken: the admin per-user toggle shares the request
// type but must never redirect another user's spend. An admin choosing WHICH of a
// user's credentials burns is a narrower version of the thing PRD #46's audit H3
// already refuses.
func TestAdminJudgeToggleIgnoresToken(t *testing.T) {
	target, secretID := uuid.New(), uuid.New()
	st := &judgeBindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: target},
		labels:  map[string]uuid.UUID{target.String() + "|theirs": secretID},
	}
	db := &fakeUserDB{}
	h := newJudgeBindHandler(t, db, st)

	req := httptest.NewRequest(http.MethodPut, "/api/admin/users/x/judge",
		strings.NewReader(`{"enabled":true,"anthropic_token":"theirs"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", target.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.SetUserJudgeEnabled(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.called {
		t.Fatal("the admin toggle should still flip judge_enabled")
	}
	if st.setCalled {
		t.Fatal("the admin toggle honored anthropic_token — an admin must not choose which of a user's " +
			"credentials burns, which is the same property audit H3 protects")
	}
}
