package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// codexSecretRow is one user_secrets row the fake store models in memory.
type codexSecretRow struct {
	id           uuid.UUID
	kind         string
	label        string
	isDefault    bool
	autoEligible bool
	created      pgtype.Timestamptz
	updated      pgtype.Timestamptz
}

// fakeCodexDB is a stateful store.DBTX that models one user's user_secrets +
// codex_credential_state rows entirely in memory, so the codex secret handlers can be
// driven through their real create/patch/delete/list paths without a database. It
// dispatches on the sqlc `-- name:` comment each generated query embeds in its SQL
// string, and it FAILS the request on any query it was not taught — which is also what
// makes "no provider/network call happens" observable: there is no provider client in
// the handler at all, and any surprise store call would surface here as an error.
type fakeCodexDB struct {
	secrets map[uuid.UUID]*codexSecretRow
	order   []uuid.UUID
	states  map[uuid.UUID]string // user_secret_id → codex_credential_state.status

	sealedArgs      [][]byte // every ciphertext handed to the store, for leak checks
	bumpCalls       int      // BumpCodexMaterialRevision invocations
	lastBumpStatus  string
	clearCalls      int // ClearCodexDefaults invocations
	unexpectedQuery string
}

func newFakeCodexDB() *fakeCodexDB {
	return &fakeCodexDB{
		secrets: map[uuid.UUID]*codexSecretRow{},
		states:  map[uuid.UUID]string{},
	}
}

func nowTS() pgtype.Timestamptz { return pgtype.Timestamptz{Time: time.Now(), Valid: true} }

// seed inserts a secret directly (bypassing the handler), for arranging preconditions.
func (f *fakeCodexDB) seed(kind, label string, isDefault bool) uuid.UUID {
	id := uuid.New()
	f.secrets[id] = &codexSecretRow{id: id, kind: kind, label: label, isDefault: isDefault, created: nowTS(), updated: nowTS()}
	f.order = append(f.order, id)
	if kind != store.KindAnthropicToken {
		if kind == store.KindCodexAuth {
			f.states[id] = codexStatusStaging
		} else {
			f.states[id] = codexStatusStatic
		}
	}
	return id
}

func (f *fakeCodexDB) codexDefaultCount() int {
	n := 0
	for _, s := range f.secrets {
		if isCodexKind(s.kind) && s.isDefault {
			n++
		}
	}
	return n
}

func labelCollisionRow() fakeScanRow {
	return fakeScanRow{func(...any) error {
		return &pgconn.PgError{Code: "23505", ConstraintName: "user_secrets_user_kind_label_key"}
	}}
}

func errRow(err error) fakeScanRow { return fakeScanRow{func(...any) error { return err }} }

// scanSecretMeta fills the shared 7-column metadata RETURNING shape (id, kind, label,
// is_default, auto_eligible, created_at, updated_at) used by InsertCodexSecret,
// RotateUserSecret, RenameUserSecret and SetUserSecretDefault.
func scanSecretMeta(s *codexSecretRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = s.id
		*dest[1].(*string) = s.kind
		*dest[2].(*string) = s.label
		*dest[3].(*bool) = s.isDefault
		*dest[4].(*bool) = s.autoEligible
		*dest[5].(*pgtype.Timestamptz) = s.created
		*dest[6].(*pgtype.Timestamptz) = s.updated
		return nil
	}
}

// scanForUpdate fills GetUserSecretForUpdate's 5-column shape.
func scanForUpdate(s *codexSecretRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = s.id
		*dest[1].(*string) = s.kind
		*dest[2].(*string) = s.label
		*dest[3].(*bool) = s.isDefault
		*dest[4].(*bool) = s.autoEligible
		return nil
	}
}

// scanCodexState fills codex_credential_state's 8-column shape; the handler reads only
// Status, so the rest take zero values.
func scanCodexState(secretID uuid.UUID, status string) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = secretID
		*dest[2].(*string) = status
		return nil
	}
}

func (f *fakeCodexDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "name: ClearCodexDefaults"):
		f.clearCalls++
		for _, s := range f.secrets {
			if isCodexKind(s.kind) {
				s.isDefault = false
			}
		}
	case strings.Contains(sql, "name: BumpCodexMaterialRevision"):
		f.bumpCalls++
		status := args[0].(string)
		id := args[1].(uuid.UUID)
		f.lastBumpStatus = status
		if _, ok := f.states[id]; ok {
			f.states[id] = status
		}
	case strings.Contains(sql, "name: DeleteUserSecret"):
		id := args[0].(uuid.UUID)
		delete(f.secrets, id)
		delete(f.states, id)
		for i, o := range f.order {
			if o == id {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
	default:
		f.unexpectedQuery = sql
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeCodexDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	scans := []func(dest ...any) error{}
	switch {
	case strings.Contains(sql, "name: ListUserSecretsAll"):
		for _, id := range f.order {
			scans = append(scans, scanListAllRow(f.secrets[id]))
		}
	case strings.Contains(sql, "name: ListCodexCredentialStatesForUser"):
		for id, status := range f.states {
			scans = append(scans, scanStatePair(id, status))
		}
	default:
		f.unexpectedQuery = sql
	}
	return &fakeCodexRows{scans: scans}, nil
}

// scanListAllRow fills ListUserSecretsAll's 7-column shape.
func scanListAllRow(s *codexSecretRow) func(dest ...any) error { return scanSecretMeta(s) }

// scanStatePair fills ListCodexCredentialStatesForUser's (user_secret_id, status).
func scanStatePair(id uuid.UUID, status string) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = id
		*dest[1].(*string) = status
		return nil
	}
}

func (f *fakeCodexDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "name: CountCodexSecrets"):
		var n int64
		for _, s := range f.secrets {
			if isCodexKind(s.kind) {
				n++
			}
		}
		return fakeScanRow{scanInt64(n)}
	case strings.Contains(sql, "name: InsertCodexSecret"):
		kind := args[1].(string)
		label := args[2].(string)
		wantDefault := args[3].(bool)
		ct := args[4].([]byte)
		for _, s := range f.secrets {
			if s.kind == kind && strings.EqualFold(s.label, label) {
				return labelCollisionRow()
			}
		}
		id := uuid.New()
		row := &codexSecretRow{id: id, kind: kind, label: label, isDefault: wantDefault, created: nowTS(), updated: nowTS()}
		f.secrets[id] = row
		f.order = append(f.order, id)
		f.sealedArgs = append(f.sealedArgs, ct)
		return fakeScanRow{scanSecretMeta(row)}
	case strings.Contains(sql, "name: InsertCodexCredentialState"):
		secretID := args[0].(uuid.UUID)
		status := args[2].(string)
		f.states[secretID] = status
		return fakeScanRow{scanCodexState(secretID, status)}
	case strings.Contains(sql, "name: GetUserSecretForUpdate"):
		id := args[0].(uuid.UUID)
		s, ok := f.secrets[id]
		if !ok {
			return errRow(pgx.ErrNoRows)
		}
		return fakeScanRow{scanForUpdate(s)}
	case strings.Contains(sql, "name: RotateUserSecret"):
		id := args[0].(uuid.UUID)
		ct := args[2].([]byte)
		s := f.secrets[id]
		s.updated = nowTS()
		f.sealedArgs = append(f.sealedArgs, ct)
		return fakeScanRow{scanSecretMeta(s)}
	case strings.Contains(sql, "name: RenameUserSecret"):
		label := args[0].(string)
		id := args[1].(uuid.UUID)
		s := f.secrets[id]
		for _, o := range f.secrets {
			if o.id != id && o.kind == s.kind && strings.EqualFold(o.label, label) {
				return labelCollisionRow()
			}
		}
		s.label = label
		s.updated = nowTS()
		return fakeScanRow{scanSecretMeta(s)}
	case strings.Contains(sql, "name: SetUserSecretDefault"):
		id := args[0].(uuid.UUID)
		s := f.secrets[id]
		s.isDefault = true
		s.updated = nowTS()
		return fakeScanRow{scanSecretMeta(s)}
	case strings.Contains(sql, "name: GetCodexCredentialState"):
		secretID := args[0].(uuid.UUID)
		status, ok := f.states[secretID]
		if !ok {
			return errRow(pgx.ErrNoRows)
		}
		return fakeScanRow{scanCodexState(secretID, status)}
	}
	f.unexpectedQuery = sql
	return errRow(pgx.ErrNoRows)
}

type fakeCodexRows struct {
	scans []func(dest ...any) error
	i     int
}

func (r *fakeCodexRows) Close()                                       {}
func (r *fakeCodexRows) Err() error                                   { return nil }
func (r *fakeCodexRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeCodexRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeCodexRows) Next() bool                                   { r.i++; return r.i <= len(r.scans) }
func (r *fakeCodexRows) Scan(dest ...any) error                       { return r.scans[r.i-1](dest...) }
func (r *fakeCodexRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeCodexRows) RawValues() [][]byte                          { return nil }
func (r *fakeCodexRows) Conn() *pgx.Conn                              { return nil }

// newCodexHandler wires a Handler over a fake store and a real (nil-vault) box, so the
// non-vault seal path is exercised exactly as the anthropic leak test does.
func newCodexHandler(t *testing.T, db store.DBTX) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{q: store.New(db), box: box}
}

func codexReq(t *testing.T, method, path, body string, userID uuid.UUID, secretID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rctx := chi.NewRouteContext()
	if secretID != "" {
		rctx.URLParams.Add("id", secretID)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	return req.WithContext(mw.ContextWithUser(ctx, store.User{ID: userID, IsActive: true}))
}

type codexSecretResp struct {
	Secret struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Label       string `json:"label"`
		IsDefault   bool   `json:"is_default"`
		CodexStatus string `json:"codex_status"`
	} `json:"secret"`
}

func decodeCodexSecret(t *testing.T, rec *httptest.ResponseRecorder) codexSecretResp {
	t.Helper()
	var out codexSecretResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func codexAuthBlob(accessToken string) string {
	b, _ := json.Marshal(map[string]string{"access_token": accessToken, "refresh_token": "r"})
	return string(b)
}

// TestCreateCodexAuthStagingAndForcedDefault: creating the first codex_auth mints a
// 'staging' status and is forced default even though the body did not ask.
func TestCreateCodexAuthStagingAndForcedDefault(t *testing.T) {
	db := newFakeCodexDB()
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": codexAuthBlob("acc-tok"), "label": "sub", "default": false})
	rec := httptest.NewRecorder()
	h.CreateCodexAuth(rec, codexReq(t, http.MethodPost, "/api/me/secrets/codex_auth", string(body), user, ""))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCodexSecret(t, rec)
	if out.Secret.Kind != store.KindCodexAuth {
		t.Errorf("kind = %q, want %q", out.Secret.Kind, store.KindCodexAuth)
	}
	if out.Secret.CodexStatus != codexStatusStaging {
		t.Errorf("codex_status = %q, want %q", out.Secret.CodexStatus, codexStatusStaging)
	}
	if !out.Secret.IsDefault {
		t.Error("first codex credential must be forced default")
	}
	if db.codexDefaultCount() != 1 {
		t.Errorf("codex default count = %d, want 1", db.codexDefaultCount())
	}
}

// TestCreateOpenAIAPIKeyStaticNoProviderCall: creating an openai_api_key mints a
// 'static' status, and no unexpected store/provider call is made (there is no provider
// client in the handler at all — the fake fails the request on any query it was not
// taught).
func TestCreateOpenAIAPIKeyStaticNoProviderCall(t *testing.T) {
	db := newFakeCodexDB()
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": "sk-openai-abc123", "label": "key"})
	rec := httptest.NewRecorder()
	h.CreateOpenAIAPIKey(rec, codexReq(t, http.MethodPost, "/api/me/secrets/openai_api_key", string(body), user, ""))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCodexSecret(t, rec)
	if out.Secret.CodexStatus != codexStatusStatic {
		t.Errorf("codex_status = %q, want %q", out.Secret.CodexStatus, codexStatusStatic)
	}
	if db.unexpectedQuery != "" {
		t.Errorf("an unexpected store query ran (no provider/network call is expected): %s", db.unexpectedQuery)
	}
	// The value handed to the store must be sealed ciphertext, never the plaintext key.
	for _, b := range db.sealedArgs {
		if bytes.Contains(b, []byte("sk-openai-abc123")) {
			t.Fatal("ciphertext arg to the store contains the plaintext key")
		}
	}
}

// TestCreateSecondCodexKindNotForcedDefault is the regression guard for the per-kind
// force bug: a user already holding a codex_auth default who adds their FIRST
// openai_api_key with default:false must NOT get a second default, and must NOT error.
func TestCreateSecondCodexKindNotForcedDefault(t *testing.T) {
	db := newFakeCodexDB()
	db.seed(store.KindCodexAuth, "sub", true) // existing codex default
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": "sk-openai-xyz", "label": "key", "default": false})
	rec := httptest.NewRecorder()
	h.CreateOpenAIAPIKey(rec, codexReq(t, http.MethodPost, "/api/me/secrets/openai_api_key", string(body), user, ""))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCodexSecret(t, rec)
	if out.Secret.IsDefault {
		t.Error("a second codex credential (different kind) with default:false must not become default")
	}
	if db.codexDefaultCount() != 1 {
		t.Errorf("codex default count = %d, want exactly 1 (no second default)", db.codexDefaultCount())
	}
}

// TestSetCodexDefaultAcrossKinds: promoting an openai_api_key to default clears the
// existing codex_auth default — the single codex default moves across kinds.
func TestSetCodexDefaultAcrossKinds(t *testing.T) {
	db := newFakeCodexDB()
	db.seed(store.KindCodexAuth, "sub", true)
	openaiID := db.seed(store.KindOpenAIAPIKey, "key", false)
	h := newCodexHandler(t, db)
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.PatchOpenAIAPIKey(rec, codexReq(t, http.MethodPatch, "/api/me/secrets/openai_api_key/"+openaiID.String(),
		`{"default":true}`, user, openaiID.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !db.secrets[openaiID].isDefault {
		t.Error("the promoted openai_api_key is not default")
	}
	if db.codexDefaultCount() != 1 {
		t.Errorf("codex default count = %d, want exactly 1 after the cross-kind swap", db.codexDefaultCount())
	}
	if db.clearCalls == 0 {
		t.Error("ClearCodexDefaults was not called; the old default was not cleared under the lock")
	}
}

// TestPatchCodexRename renames a codex_auth credential and preserves its status.
func TestPatchCodexRename(t *testing.T) {
	db := newFakeCodexDB()
	id := db.seed(store.KindCodexAuth, "old", true)
	h := newCodexHandler(t, db)
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.PatchCodexAuth(rec, codexReq(t, http.MethodPatch, "/api/me/secrets/codex_auth/"+id.String(),
		`{"label":"new"}`, user, id.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeCodexSecret(t, rec)
	if out.Secret.Label != "new" {
		t.Errorf("label = %q, want %q", out.Secret.Label, "new")
	}
	if out.Secret.CodexStatus != codexStatusStaging {
		t.Errorf("codex_status = %q, want unchanged %q", out.Secret.CodexStatus, codexStatusStaging)
	}
}

// TestPatchCodexReplaceBumpsAndResets: replacing a codex_auth's value bumps the
// material revision and resets its status to 'staging'.
func TestPatchCodexReplaceBumpsAndResets(t *testing.T) {
	db := newFakeCodexDB()
	id := db.seed(store.KindCodexAuth, "sub", true)
	db.states[id] = codexStatusStaging // pretend a later state; replace must reset it
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": codexAuthBlob("new-acc")})
	rec := httptest.NewRecorder()
	h.PatchCodexAuth(rec, codexReq(t, http.MethodPatch, "/api/me/secrets/codex_auth/"+id.String(),
		string(body), user, id.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.bumpCalls != 1 {
		t.Errorf("BumpCodexMaterialRevision called %d times, want 1", db.bumpCalls)
	}
	if db.lastBumpStatus != codexStatusStaging {
		t.Errorf("bump status = %q, want %q", db.lastBumpStatus, codexStatusStaging)
	}
	out := decodeCodexSecret(t, rec)
	if out.Secret.CodexStatus != codexStatusStaging {
		t.Errorf("codex_status = %q, want reset %q", out.Secret.CodexStatus, codexStatusStaging)
	}
}

// TestPatchOpenAIReplaceResetsStatic: replacing an openai_api_key resets its status to
// 'static' (not 'staging').
func TestPatchOpenAIReplaceResetsStatic(t *testing.T) {
	db := newFakeCodexDB()
	id := db.seed(store.KindOpenAIAPIKey, "key", true)
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": "sk-openai-new"})
	rec := httptest.NewRecorder()
	h.PatchOpenAIAPIKey(rec, codexReq(t, http.MethodPatch, "/api/me/secrets/openai_api_key/"+id.String(),
		string(body), user, id.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.lastBumpStatus != codexStatusStatic {
		t.Errorf("bump status = %q, want %q", db.lastBumpStatus, codexStatusStatic)
	}
}

// TestDeleteCodexSecret deletes an openai_api_key by id and returns 204.
func TestDeleteCodexSecret(t *testing.T) {
	db := newFakeCodexDB()
	id := db.seed(store.KindOpenAIAPIKey, "key", true)
	h := newCodexHandler(t, db)
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.DeleteOpenAIAPIKeyByID(rec, codexReq(t, http.MethodDelete, "/api/me/secrets/openai_api_key/"+id.String(), "", user, id.String()))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := db.secrets[id]; ok {
		t.Error("secret was not deleted")
	}
	if _, ok := db.states[id]; ok {
		t.Error("codex_credential_state was not cascaded")
	}
}

// TestPatchCodexWrongKindIs404: a codex_auth id patched through the openai route is a
// 404 — the routes are kind-scoped.
func TestPatchCodexWrongKindIs404(t *testing.T) {
	db := newFakeCodexDB()
	id := db.seed(store.KindCodexAuth, "sub", true)
	h := newCodexHandler(t, db)
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.PatchOpenAIAPIKey(rec, codexReq(t, http.MethodPatch, "/api/me/secrets/openai_api_key/"+id.String(),
		`{"label":"x"}`, user, id.String()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a wrong-kind id; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDeleteCodexForeignIdIs404: an id the user does not own is a 404.
func TestDeleteCodexForeignIdIs404(t *testing.T) {
	db := newFakeCodexDB()
	h := newCodexHandler(t, db)
	user := uuid.New()
	foreign := uuid.New()

	rec := httptest.NewRecorder()
	h.DeleteCodexAuthByID(rec, codexReq(t, http.MethodDelete, "/api/me/secrets/codex_auth/"+foreign.String(), "", user, foreign.String()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListMySecretsMergesCodexStatus: the all-kinds list returns codex rows carrying
// their status and anthropic rows without one.
func TestListMySecretsMergesCodexStatus(t *testing.T) {
	db := newFakeCodexDB()
	db.seed(store.KindAnthropicToken, "anthropic", true)
	db.seed(store.KindCodexAuth, "sub", true)
	db.seed(store.KindOpenAIAPIKey, "key", false)
	h := newCodexHandler(t, db)
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.ListMySecrets(rec, codexReq(t, http.MethodGet, "/api/me/secrets/", "", user, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Secrets []struct {
			Kind        string `json:"kind"`
			CodexStatus string `json:"codex_status"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Secrets) != 3 {
		t.Fatalf("got %d secrets, want 3", len(out.Secrets))
	}
	byKind := map[string]string{}
	for _, s := range out.Secrets {
		byKind[s.Kind] = s.CodexStatus
	}
	if byKind[store.KindAnthropicToken] != "" {
		t.Errorf("anthropic row carries codex_status %q, want empty", byKind[store.KindAnthropicToken])
	}
	if byKind[store.KindCodexAuth] != codexStatusStaging {
		t.Errorf("codex_auth row codex_status = %q, want %q", byKind[store.KindCodexAuth], codexStatusStaging)
	}
	if byKind[store.KindOpenAIAPIKey] != codexStatusStatic {
		t.Errorf("openai_api_key row codex_status = %q, want %q", byKind[store.KindOpenAIAPIKey], codexStatusStatic)
	}
}

// TestCreateCodexVaultLocked: a locked vault surfaces as 409 vault_locked, never a 500,
// and no secret is written.
func TestCreateCodexVaultLocked(t *testing.T) {
	db := newFakeCodexDB()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	// A vault with nothing unlocked → Seal returns vault.ErrLocked for any user.
	h := &Handler{q: store.New(db), box: box, vault: vault.New(nil, nil)}
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": codexAuthBlob("acc"), "label": "sub"})
	rec := httptest.NewRecorder()
	h.CreateCodexAuth(rec, codexReq(t, http.MethodPost, "/api/me/secrets/codex_auth", string(body), user, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vault_locked") {
		t.Errorf("body = %s, want a vault_locked code", rec.Body.String())
	}
	if len(db.secrets) != 0 {
		t.Error("a secret was written despite a locked vault")
	}
}

// TestCreateCodexRejectsInvalidValue: an empty codex_auth blob and a blob without a
// non-empty access_token are 400s; an openai key with whitespace is a 400.
func TestCreateCodexRejectsInvalidValue(t *testing.T) {
	db := newFakeCodexDB()
	h := newCodexHandler(t, db)
	user := uuid.New()

	for _, tc := range []struct {
		name, path, body string
		create           func(http.ResponseWriter, *http.Request)
	}{
		{"empty blob", "/api/me/secrets/codex_auth", `{"token":"","label":"a"}`, h.CreateCodexAuth},
		{"blob no access_token", "/api/me/secrets/codex_auth", `{"token":"{}","label":"a"}`, h.CreateCodexAuth},
		{"blob not json", "/api/me/secrets/codex_auth", `{"token":"not-json","label":"a"}`, h.CreateCodexAuth},
		{"empty key", "/api/me/secrets/openai_api_key", `{"token":"  ","label":"a"}`, h.CreateOpenAIAPIKey},
		{"key with space", "/api/me/secrets/openai_api_key", `{"token":"sk a","label":"a"}`, h.CreateOpenAIAPIKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.create(rec, codexReq(t, http.MethodPost, tc.path, tc.body, user, ""))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	if len(db.secrets) != 0 {
		t.Error("an invalid value was stored")
	}
}

// TestCreateCodexLabelCollision maps a duplicate label to a 409.
func TestCreateCodexLabelCollision(t *testing.T) {
	db := newFakeCodexDB()
	db.seed(store.KindCodexAuth, "dup", true)
	h := newCodexHandler(t, db)
	user := uuid.New()

	body, _ := json.Marshal(map[string]any{"token": codexAuthBlob("acc"), "label": "dup"})
	rec := httptest.NewRecorder()
	h.CreateCodexAuth(rec, codexReq(t, http.MethodPost, "/api/me/secrets/codex_auth", string(body), user, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCodexWritesAreCookieOnly walks the REAL router and asserts each of the six codex
// write routes is mounted under RequireAuth (cookie+CSRF) and NOT under RequireUser —
// a Bearer-reachable mint would let a stolen uzc_ replace a user's credentials (PRD
// #104 D8), the same posture as the anthropic writes they sit beside. Uses the same
// classifyAuthMW instrument as route_auth_boundary_test.go.
func TestCodexWritesAreCookieOnly(t *testing.T) {
	limiters := newProbeLimiters()
	h := &Handler{cfg: config.Config{WorkerHostingEnabled: true}}
	router := h.Routes(limiters[0], limiters[1], limiters[2], limiters[3],
		limiters[4], limiters[5], limiters[6], limiters[7], limiters[8]).(chi.Routes)

	want := map[string]bool{
		"POST /api/me/secrets/codex_auth":            true,
		"PATCH /api/me/secrets/codex_auth/{id}":      true,
		"DELETE /api/me/secrets/codex_auth/{id}":     true,
		"POST /api/me/secrets/openai_api_key":        true,
		"PATCH /api/me/secrets/openai_api_key/{id}":  true,
		"DELETE /api/me/secrets/openai_api_key/{id}": true,
	}
	seen := map[string]bool{}
	if err := chi.Walk(router, func(method, pattern string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := method + " " + pattern
		if !want[key] {
			return nil
		}
		seen[key] = true
		var hasAuth, hasUser bool
		for _, m := range mws {
			switch classifyAuthMW(m) {
			case kindAuth:
				hasAuth = true
			case kindUser:
				hasUser = true
			}
		}
		if !hasAuth {
			t.Errorf("%s is not mounted under RequireAuth (cookie-only)", key)
		}
		if hasUser {
			t.Errorf("%s is mounted under RequireUser — a codex WRITE must be cookie-only", key)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("route %s was not found in the router", key)
		}
	}
}
