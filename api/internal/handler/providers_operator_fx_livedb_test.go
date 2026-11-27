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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Shared fixtures of the organization-key (PRD #4540 M1) admin route tests. Every test here drives the
// REAL h.Routes() router. They are skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (./e2e/run-store-it.sh). Tests that capture the audit log swap the global slog default, so
// none of them runs in parallel.

// opFx is a shared fixture with one organization-funded definition (keyless) at the fake gateway.
type opFx struct {
	*sharedFx
	op apitypes.AdminSharedProviderDTO
}

func newOpFx(t *testing.T) *opFx {
	t.Helper()
	s := newSharedFx(t, false)
	o := &opFx{sharedFx: s}
	o.op = s.createOperator(t, "org-"+uuid.NewString()[:8])
	return o
}

// orgKeyFixture is a runtime-built gateway key with no vendor prefix: nothing token-shaped is in source.
func orgKeyFixture() string {
	return strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
}

const operatorFields = `"credential_mode":"operator_key","acknowledge_recipient_trust":true,`

func (s *sharedFx) operatorBody(name, auth string) string {
	return strings.Replace(s.sharedBody(name, auth), `"name"`, operatorFields+`"name"`, 1)
}

// createOperator posts an acknowledged organization-key create through the admin route.
func (s *sharedFx) createOperator(t *testing.T, name string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	rec := cookieReq(t, s.f.router, http.MethodPost, adminProvidersPath, s.admin.jwt, s.operatorBody(name, "bearer"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin operator create = %d %s", rec.Code, rec.Body.String())
	}
	dto := decodeAdminProvider(t, rec)
	if dto.CredentialMode != "operator_key" || dto.OperatorKey == nil {
		t.Fatalf("created operator definition = %+v", dto)
	}
	s.cleanupShared(t, dto.ID)
	return dto
}

func decodeAdminProvider(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AdminSharedProviderDTO {
	t.Helper()
	var env struct {
		Provider apitypes.AdminSharedProviderDTO `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return env.Provider
}

func opPath(id string, parts ...string) string {
	return adminProvidersPath + "/" + id + strings.Join(append([]string{""}, parts...), "/")
}

// admin sends one request as the admin session (cookie + CSRF).
func (s *sharedFx) adminDo(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return cookieReq(t, s.f.router, method, path, s.admin.jwt, body)
}

// mustPutOrgKey sets or replaces the organization key and requires 200.
func (s *sharedFx) mustPutOrgKey(t *testing.T, id, key string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	rec := s.adminDo(t, http.MethodPut, opPath(id, "key"), `{"key":"`+key+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put organization key = %d %s", rec.Code, rec.Body.String())
	}
	return decodeAdminProvider(t, rec)
}

// custody reads the custody row's identity directly: the oracle for key_rev and the row id.
func (s *sharedFx) custody(t *testing.T, id string) (keyID uuid.UUID, keyRev int64, disabled bool, ok bool) {
	t.Helper()
	var dis *time.Time
	err := s.f.pool.QueryRow(context.Background(),
		`SELECT id, key_rev, disabled_at FROM model_provider_operator_keys WHERE provider_id = $1`, id).Scan(&keyID, &keyRev, &dis)
	if err != nil {
		return uuid.Nil, 0, false, false
	}
	return keyID, keyRev, dis != nil, true
}

func (s *sharedFx) evidenceRows(t *testing.T, id string) int {
	t.Helper()
	return countRows(t, s, `SELECT count(*) FROM model_provider_operator_key_test_results WHERE provider_id = $1`, id)
}

// csrfCookieReq is a cookie request whose CSRF header is chosen by the caller: csrf nil sends none.
func csrfCookieReq(t *testing.T, router http.Handler, method, path, jwt, body string, csrf *string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	if csrf != nil {
		req.Header.Set(auth.CSRFHeaderName, *csrf)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// auditEvents returns the provider_audit lines of one action.
func auditEvents(logs *providerAuditLog, action string) []string {
	var out []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "provider_audit") && strings.Contains(l, "action="+action+" ") {
			out = append(out, l)
		}
	}
	return out
}

// auditField reads key=value from a slog text line ("" when absent).
func auditField(line, key string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

// secondHandler builds a second handler instance over the same pool, collaborators and config, with its
// own, independent probe limiter: two admins on two API replicas.
func secondHandler(t *testing.T, h1 *Handler) (*Handler, http.Handler) {
	t.Helper()
	h2 := &Handler{
		pool: h1.pool, q: h1.q, box: h1.box, vault: h1.vault, cfg: h1.cfg, wsvc: h1.wsvc,
		settings: h1.settings, hub: h1.hub, providerProbeLimiter: providers.NewProbeLimiter(),
	}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	return h2, h2.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
}

// probeHooks is the test seam of the admin Test: store reads can be paused, failed or followed by a change.
type probeHooks struct {
	beforeKeyRead func() error // runs before the sink read; a non-nil error replaces its result
	afterKeyRead  func()       // runs after the sink read returned
	getProvider   func(call int) error
	afterKeyLock  func() // runs after the evidence key lock returned, while step 7 holds its locks
	upsert        error  // replaces the evidence upsert's result
	gets          int
}

type wrappedProbeStore struct {
	operatorProbeStore
	hk *probeHooks
}

func (w *wrappedProbeStore) GetOperatorProviderForTest(ctx context.Context, id uuid.UUID) (store.GetOperatorProviderForTestRow, error) {
	w.hk.gets++
	if w.hk.getProvider != nil {
		if err := w.hk.getProvider(w.hk.gets); err != nil {
			return store.GetOperatorProviderForTestRow{}, err
		}
	}
	return w.operatorProbeStore.GetOperatorProviderForTest(ctx, id)
}

func (w *wrappedProbeStore) GetOperatorProviderKeyForTest(ctx context.Context, arg store.GetOperatorProviderKeyForTestParams) (store.GetOperatorProviderKeyForTestRow, error) {
	if w.hk.beforeKeyRead != nil {
		if err := w.hk.beforeKeyRead(); err != nil {
			return store.GetOperatorProviderKeyForTestRow{}, err
		}
	}
	row, err := w.operatorProbeStore.GetOperatorProviderKeyForTest(ctx, arg)
	if w.hk.afterKeyRead != nil {
		w.hk.afterKeyRead()
	}
	return row, err
}

func (w *wrappedProbeStore) LockOperatorProviderKeyForEvidence(ctx context.Context, arg store.LockOperatorProviderKeyForEvidenceParams) (store.LockOperatorProviderKeyForEvidenceRow, error) {
	row, err := w.operatorProbeStore.LockOperatorProviderKeyForEvidence(ctx, arg)
	if err == nil && w.hk.afterKeyLock != nil {
		w.hk.afterKeyLock()
	}
	return row, err
}

func (w *wrappedProbeStore) UpsertOperatorProviderKeyTestResult(ctx context.Context, arg store.UpsertOperatorProviderKeyTestResultParams) error {
	if w.hk.upsert != nil {
		return w.hk.upsert
	}
	return w.operatorProbeStore.UpsertOperatorProviderKeyTestResult(ctx, arg)
}

// wireProbe installs hooks on a handler and returns them.
func wireOrgProbe(h *Handler) *probeHooks {
	hk := &probeHooks{}
	h.adminProbeWrap = func(s operatorProbeStore) operatorProbeStore {
		return &wrappedProbeStore{operatorProbeStore: s, hk: hk}
	}
	return hk
}

func countRowsPool(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedOperatorDefinition inserts an organization-funded definition with revision 1 at base and a sealed
// key, straight through the store, and registers its cleanup (selections first: a selected definition
// cannot be deleted).
func seedOperatorDefinition(t *testing.T, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, base string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	p, err := q.InsertSharedOperatorModelProvider(ctx, "operator-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("insert operator definition: %v", err)
	}
	t.Cleanup(func() {
		for _, sql := range []string{
			`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE claude_provider_id = $1`,
			`DELETE FROM model_providers WHERE id = $1`,
		} {
			if _, err := pool.Exec(context.Background(), sql, p.ID); err != nil {
				t.Errorf("cleanup %q: %v", sql, err)
			}
		}
	})
	if err := q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: base, AuthStyle: "bearer",
		ModelMap: []byte(`{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`),
	}); err != nil {
		t.Fatalf("insert operator revision: %v", err)
	}
	sealed, err := operatorkey.Seal(box, p.ID, []byte(orgKeyFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertOperatorProviderKey(ctx, store.InsertOperatorProviderKeyParams{ProviderID: p.ID, Ciphertext: sealed, SetBy: uuid.New()}); err != nil {
		t.Fatalf("insert operator custody row: %v", err)
	}
	return p.ID
}

// forceOperatorSource makes op the user's Claude source by direct SQL: no route can select it.
func forceOperatorSource(t *testing.T, pool *pgxpool.Pool, user, op uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = '`+sharedNilScope+`' WHERE id = $1`, user, op); err != nil {
		t.Fatalf("force the operator source: %v", err)
	}
}

func strictUnmarshal(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
