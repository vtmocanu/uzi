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
	setArg    store.SetUserJudgeAnthropicSecretParams
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

func (j *judgeBindStore) SetUserJudgeAnthropicSecret(_ context.Context, arg store.SetUserJudgeAnthropicSecretParams) (store.User, error) {
	j.setCalled = true
	j.setArg = arg
	return store.User{ID: arg.ID, JudgeAnthropicSecretID: arg.JudgeAnthropicSecretID}, nil
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
