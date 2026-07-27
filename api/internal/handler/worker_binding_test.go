package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// bindStore is the narrow workersvc.Store slice PATCH /api/workers/{id} touches.
// It embeds the interface so any query the handler reaches for beyond these three
// panics loudly rather than returning a zero value that quietly passes a test.
//
// It models the two properties the handler's behavior rests on: label lookup is
// per (user, label), and the by-id secret fetch is OWNER-SCOPED — a secret whose
// owner is not the caller returns pgx.ErrNoRows, exactly as the SQL predicate does.
type bindStore struct {
	workersvc.Store

	// secrets maps secret id → owner, and labels maps (owner, lowercased label) →
	// secret id. Deliberately separate so a test can stage a secret that EXISTS but
	// belongs to someone else, which is the cross-user case D11 is about.
	secrets map[uuid.UUID]uuid.UUID
	labels  map[string]uuid.UUID

	setCalled bool
	setArg    store.SetWorkerAnthropicSecretParams
	setErr    error
}

func (b *bindStore) GetUserSecretIDByLabel(_ context.Context, arg store.GetUserSecretIDByLabelParams) (uuid.UUID, error) {
	id, ok := b.labels[arg.UserID.String()+"|"+arg.Label]
	if !ok {
		return uuid.UUID{}, pgx.ErrNoRows
	}
	return id, nil
}

func (b *bindStore) GetUserSecretCiphertextByID(_ context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	owner, ok := b.secrets[arg.ID]
	// The real query is `WHERE id = $1 AND user_id = $2`, so a foreign secret is
	// indistinguishable from a missing one. Modelling anything softer here would
	// let a handler bug pass.
	if !ok || owner != arg.UserID {
		return store.GetUserSecretCiphertextByIDRow{}, pgx.ErrNoRows
	}
	return store.GetUserSecretCiphertextByIDRow{UserID: owner, Kind: store.KindAnthropicToken}, nil
}

func (b *bindStore) SetWorkerAnthropicSecret(_ context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error) {
	b.setCalled = true
	b.setArg = arg
	if b.setErr != nil {
		return store.Worker{}, b.setErr
	}
	return store.Worker{
		ID: arg.ID, UserID: arg.UserID,
		AnthropicSecretID: arg.AnthropicSecretID,
		AnthropicBindMode: arg.AnthropicBindMode,
	}, nil
}

func newBindHandler(t *testing.T, st *bindStore) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{})}
}

func patchWorkerReq(t *testing.T, user store.User, workerID uuid.UUID, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/workers/x", bytes.NewReader([]byte(body)))
	ctx := mw.ContextWithUser(req.Context(), user)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", workerID.String())
	return req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
}

// TestPatchWorkerBindsByLabel: the happy path. A label the caller owns resolves to
// its id and reaches the UPDATE scoped to the caller.
func TestPatchWorkerBindsByLabel(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	secretID, workerID := uuid.New(), uuid.New()
	st := &bindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: owner.ID},
		labels:  map[string]uuid.UUID{owner.ID.String() + "|console-key": secretID},
	}
	h := newBindHandler(t, st)

	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, `{"anthropic_token":"console-key"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !st.setCalled {
		t.Fatal("the binding was never written")
	}
	if st.setArg.UserID != owner.ID || st.setArg.ID != workerID {
		t.Fatalf("update scoped to (%v,%v), want (%v,%v)", st.setArg.ID, st.setArg.UserID, workerID, owner.ID)
	}
	if !st.setArg.AnthropicSecretID.Valid || uuid.UUID(st.setArg.AnthropicSecretID.Bytes) != secretID {
		t.Fatalf("bound to %+v, want %v", st.setArg.AnthropicSecretID, secretID)
	}
	// The response carries the label, never a credential.
	var env struct {
		Worker struct {
			AnthropicSecretID    *string `json:"anthropic_secret_id"`
			AnthropicSecretLabel *string `json:"anthropic_secret_label"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Worker.AnthropicSecretLabel == nil || *env.Worker.AnthropicSecretLabel != "console-key" {
		t.Fatalf("response label = %v, want console-key", env.Worker.AnthropicSecretLabel)
	}
}

// TestPatchWorkerClearsBinding: an explicit JSON null clears it, and so does an
// empty string (what a shell passes for an omitted value). An OMITTED key is not a
// clear — see TestPatchWorkerOmittedTokenIsRejected.
func TestPatchWorkerClearsBinding(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	workerID := uuid.New()
	for _, body := range []string{`{"anthropic_token":null}`, `{"anthropic_token":""}`} {
		st := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
		h := newBindHandler(t, st)
		rec := httptest.NewRecorder()
		h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code = %d, want 200; body=%s", body, rec.Code, rec.Body.String())
		}
		if !st.setCalled {
			t.Fatalf("%s: the clear was never written", body)
		}
		if st.setArg.AnthropicSecretID.Valid {
			t.Fatalf("%s: wrote %+v, want a NULL binding", body, st.setArg.AnthropicSecretID)
		}
	}
}

// TestPatchWorkerOmittedTokenIsRejected: a body that names nothing changes nothing.
// PATCH means "change what I named", so an omitted key must never be read as a
// clear — the day this body grows a second field, absent-means-clear would wipe a
// user's binding every time they renamed a worker.
func TestPatchWorkerOmittedTokenIsRejected(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	st := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newBindHandler(t, st)
	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, uuid.New(), `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for a body that names nothing", rec.Code)
	}
	if st.setCalled {
		t.Fatal("an omitted anthropic_token must not clear the binding")
	}
}

// TestPatchWorkerRefusesForeignSecret is the handler half of M3's second
// acceptance criterion (D11): a PATCH naming ANOTHER user's secret id must be
// refused here, before the database is asked to write anything.
//
// The schema half — the same binding still refused with this check bypassed — is
// TestWorkerAnthropicBindingLiveDB in internal/store, which drives the UPDATE
// directly against real Postgres and asserts the composite FK rejects it.
func TestPatchWorkerRefusesForeignSecret(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	stranger := uuid.New()
	foreignSecret, workerID := uuid.New(), uuid.New()

	st := &bindStore{
		// The secret EXISTS — it just belongs to someone else. A single-column FK
		// would happily accept it, which is the whole reason D11 uses a composite one.
		secrets: map[uuid.UUID]uuid.UUID{foreignSecret: stranger},
		// And the caller can name it by label only if it were theirs; it is not, so
		// the label map is empty for them. The id path is what a crafted request uses.
		labels: map[string]uuid.UUID{owner.ID.String() + "|theirs": foreignSecret},
	}
	h := newBindHandler(t, st)

	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, `{"anthropic_token":"theirs"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 for another user's secret; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("the handler wrote a cross-user binding — the ownership check did not run before the UPDATE")
	}
	// 404, not 403: a 403 would confirm the id names a real credential someone else
	// owns, which is a membership oracle over other users' secrets.
	if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte("worker not found")) {
		t.Fatalf("body = %s, want the same message an unknown worker gets (no oracle)", body)
	}
}

// TestPatchWorkerUnknownLabel: a label the caller has no token under is a 400 that
// says so, not a 500 and not a silent fallback to the default.
func TestPatchWorkerUnknownLabel(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	st := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newBindHandler(t, st)

	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, uuid.New(), `{"anthropic_token":"nope"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if st.setCalled {
		t.Fatal("an unknown label must not reach the UPDATE")
	}
}

// TestPatchWorkerUnknownWorkerIs404: the UPDATE is owner-scoped, so a foreign or
// unknown worker id affects no rows, which the service surfaces as
// ErrWorkerNotFound.
func TestPatchWorkerUnknownWorkerIs404(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	secretID := uuid.New()
	st := &bindStore{
		secrets: map[uuid.UUID]uuid.UUID{secretID: owner.ID},
		labels:  map[string]uuid.UUID{owner.ID.String() + "|mine": secretID},
		setErr:  pgx.ErrNoRows,
	}
	h := newBindHandler(t, st)

	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, uuid.New(), `{"anthropic_token":"mine"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPatchWorkerRequiresAuth: no user in context is a 401 before any store access.
func TestPatchWorkerRequiresAuth(t *testing.T) {
	st := &bindStore{}
	h := newBindHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/workers/x", bytes.NewReader([]byte(`{}`)))
	h.PatchWorker(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if st.setCalled {
		t.Fatal("an unauthenticated PATCH reached the store")
	}
}

// TestWorkerDTOCarriesBinding pins the DTO mapping: a NULL column is JSON null
// (unbound ⇒ the owner's default), a set column is the id string.
func TestWorkerDTOCarriesBinding(t *testing.T) {
	id := uuid.New()
	bound := workerDTOFromWorker(store.Worker{
		AnthropicBindMode: workersvc.BindModePinned,
		AnthropicSecretID: pgtype.UUID{Bytes: id, Valid: true},
	}, 0, false, "console-key", "", time.Now(), time.Now())
	if bound.AnthropicSecretID == nil || *bound.AnthropicSecretID != id.String() {
		t.Fatalf("bound id = %v, want %s", bound.AnthropicSecretID, id)
	}
	if bound.AnthropicSecretLabel == nil || *bound.AnthropicSecretLabel != "console-key" {
		t.Fatalf("bound label = %v, want console-key", bound.AnthropicSecretLabel)
	}
	unbound := workerDTOFromWorker(store.Worker{}, 0, false, "", "", time.Now(), time.Now())
	if unbound.AnthropicSecretID != nil || unbound.AnthropicSecretLabel != nil {
		t.Fatalf("unbound worker must serialize both fields as null, got %v/%v",
			unbound.AnthropicSecretID, unbound.AnthropicSecretLabel)
	}
}

// --- PRD #111 M3: the bind mode --------------------------------------------

// bindModeOf decodes the effective mode the response reports.
func bindModeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Worker struct {
			AnthropicBindMode string `json:"anthropic_bind_mode"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return env.Worker.AnthropicBindMode
}

// TestPatchWorkerDerivesModeFromTokenField: a body with no mode keeps its
// pre-#111 meaning exactly. That is what lets every existing client — the shipped
// web picker, `uzi worker set-token`, anyone's script — keep working with no
// version negotiation, and it is the same rule 00088's backfill applies to rows.
func TestPatchWorkerDerivesModeFromTokenField(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	secretID, workerID := uuid.New(), uuid.New()
	for _, tc := range []struct{ name, body, wantMode string }{
		{"a label means pinned", `{"anthropic_token":"console-key"}`, workersvc.BindModePinned},
		{"null means default", `{"anthropic_token":null}`, workersvc.BindModeDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bindStore{
				secrets: map[uuid.UUID]uuid.UUID{secretID: owner.ID},
				labels:  map[string]uuid.UUID{owner.ID.String() + "|console-key": secretID},
			}
			h := newBindHandler(t, st)
			rec := httptest.NewRecorder()
			h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, tc.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if st.setArg.AnthropicBindMode != tc.wantMode {
				t.Fatalf("wrote mode %q, want %q", st.setArg.AnthropicBindMode, tc.wantMode)
			}
		})
	}
}

// TestPatchWorkerSetsAutoMode: the third mode, with no token, writes 'auto' AND a
// NULL id. The id half matters: a stale id left beside a non-pinned mode is one
// refactor away from being spent.
func TestPatchWorkerSetsAutoMode(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	workerID := uuid.New()
	st := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newBindHandler(t, st)

	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, `{"anthropic_bind_mode":"auto"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.setArg.AnthropicBindMode != workersvc.BindModeAuto {
		t.Fatalf("wrote mode %q, want auto", st.setArg.AnthropicBindMode)
	}
	if st.setArg.AnthropicSecretID.Valid {
		t.Fatalf("auto wrote a credential id (%+v); a non-pinned mode must carry none", st.setArg.AnthropicSecretID)
	}
	if got := bindModeOf(t, rec); got != workersvc.BindModeAuto {
		t.Fatalf("response mode = %q, want auto", got)
	}
}

// TestPatchWorkerRefusesContradictoryModeAndToken: a mode and a token that
// disagree are REFUSED rather than reconciled. Either choice of winner spends a
// credential the caller did not ask for on some request, and "auto" arriving with
// a leftover label is the realistic shape of a half-updated client — exactly when
// a quiet reconcile does the most damage.
func TestPatchWorkerRefusesContradictoryModeAndToken(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	secretID, workerID := uuid.New(), uuid.New()
	for _, tc := range []struct{ name, body string }{
		{"auto with a label", `{"anthropic_bind_mode":"auto","anthropic_token":"console-key"}`},
		{"default with a label", `{"anthropic_bind_mode":"default","anthropic_token":"console-key"}`},
		{"pinned with no label", `{"anthropic_bind_mode":"pinned","anthropic_token":null}`},
		{"pinned with nothing at all", `{"anthropic_bind_mode":"pinned"}`},
		{"an illegal mode", `{"anthropic_bind_mode":"whatever","anthropic_token":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bindStore{
				secrets: map[uuid.UUID]uuid.UUID{secretID: owner.ID},
				labels:  map[string]uuid.UUID{owner.ID.String() + "|console-key": secretID},
			}
			h := newBindHandler(t, st)
			rec := httptest.NewRecorder()
			h.PatchWorker(rec, patchWorkerReq(t, owner, workerID, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			// And nothing was written. A 400 that had already mutated the row would be
			// the worst of both.
			if st.setCalled {
				t.Fatalf("a refused request still wrote %+v", st.setArg)
			}
		})
	}
}

// TestPatchWorkerModeAloneSatisfiesTheRequiredField: `anthropic_bind_mode` on its
// own is a complete request. Requiring a redundant `"anthropic_token": null`
// beside it would be ceremony — the caller has named what they are changing, which
// is the whole rule the omitted-key 400 exists to enforce.
func TestPatchWorkerModeAloneSatisfiesTheRequiredField(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsActive: true}
	st := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	h := newBindHandler(t, st)
	rec := httptest.NewRecorder()
	h.PatchWorker(rec, patchWorkerReq(t, owner, uuid.New(), `{"anthropic_bind_mode":"default"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// An empty body still is not: it names nothing.
	st2 := &bindStore{secrets: map[uuid.UUID]uuid.UUID{}, labels: map[string]uuid.UUID{}}
	rec2 := httptest.NewRecorder()
	newBindHandler(t, st2).PatchWorker(rec2, patchWorkerReq(t, owner, uuid.New(), `{}`))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("empty body: code = %d, want 400", rec2.Code)
	}
}

// TestEffectiveBindModeHidesAPhantomPin is D9 at the DTO boundary. 00078's FK nulls
// a worker's credential id when the token is deleted and deliberately leaves the
// mode alone, so the database legitimately holds 'pinned' with no id. Resolution
// treats that worker as default; the API must say so, or every client draws a pin
// to a token that does not exist.
func TestEffectiveBindModeHidesAPhantomPin(t *testing.T) {
	id := uuid.New()
	bound := pgtype.UUID{Bytes: id, Valid: true}
	for _, tc := range []struct {
		name, stored, want string
		secret             pgtype.UUID
	}{
		{"pinned with an id stays pinned", workersvc.BindModePinned, workersvc.BindModePinned, bound},
		{"pinned with NO id reports default", workersvc.BindModePinned, workersvc.BindModeDefault, pgtype.UUID{}},
		{"auto is untouched by the id", workersvc.BindModeAuto, workersvc.BindModeAuto, pgtype.UUID{}},
		{"default is untouched", workersvc.BindModeDefault, workersvc.BindModeDefault, pgtype.UUID{}},
		// An auto worker carrying a stale id must still read auto: the id is not
		// read in that mode, so reporting 'pinned' would describe a spend that
		// cannot happen.
		{"auto with a stale id still reports auto", workersvc.BindModeAuto, workersvc.BindModeAuto, bound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveBindMode(tc.stored, tc.secret); got != tc.want {
				t.Fatalf("effectiveBindMode(%q, valid=%v) = %q, want %q", tc.stored, tc.secret.Valid, got, tc.want)
			}
		})
	}
}
