package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// secretsCRUDHandler builds a live-DB Handler for the PRD #104 M2 token CRUD: real
// pool (so the advisory-lock transaction actually runs), real box (nil vault → the
// master-box seal path, which is all these tests need), no poker.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func secretsCRUDHandler(t *testing.T) (*Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	h := &Handler{pool: pool, q: store.New(pool), box: newHandlerTestBox(t)}
	return h, pool
}

func mkSecretUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	u := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		u, fmt.Sprintf("secrets-crud-%s@e2e", u)); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return u
}

func userReq(method, path, body string, user uuid.UUID, urlParams map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	ctx := mw.ContextWithUser(req.Context(), store.User{ID: user, IsActive: true})
	if len(urlParams) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range urlParams {
			rctx.URLParams.Add(k, v)
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

type secretResp struct {
	Secret struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		IsDefault bool   `json:"is_default"`
	} `json:"secret"`
}

func decodeSecret(t *testing.T, rec *httptest.ResponseRecorder) secretResp {
	t.Helper()
	var s secretResp
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return s
}

func listSecrets(t *testing.T, h *Handler, user uuid.UUID) []struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	IsDefault bool   `json:"is_default"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ListMySecrets(rec, userReq(http.MethodGet, "/api/me/secrets", "", user, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Secrets []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			IsDefault bool   `json:"is_default"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return body.Secrets
}

// create posts a token and returns the recorder.
func (h *Handler) createToken(t *testing.T, user uuid.UUID, label, token string, isDefault bool) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"token":%q,"label":%q,"default":%t}`, token, label, isDefault)
	rec := httptest.NewRecorder()
	h.CreateAnthropicToken(rec, userReq(http.MethodPost, "/api/me/secrets/anthropic_token", body, user, nil))
	return rec
}

// TestSecretsCRUDLiveDB walks the full M2 lifecycle against real Postgres: create,
// list, the forced-first-default invariant, rename, set-default, rotate, and the
// D5/D6 delete rules.
func TestSecretsCRUDLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	user := mkSecretUser(t, pool)

	// --- First token is forced default even asking for non-default (M1 hardening) ---
	rec := h.createToken(t, user, "primary", "tok-primary-value", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create first: code %d, body %s", rec.Code, rec.Body.String())
	}
	first := decodeSecret(t, rec)
	if !first.Secret.IsDefault {
		t.Fatal("the FIRST token must be the default whatever the body asked (invisible-token hazard)")
	}

	// --- Second token, non-default ---
	rec = h.createToken(t, user, "console", "tok-console-value", false)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create second: code %d, body %s", rec.Code, rec.Body.String())
	}
	second := decodeSecret(t, rec)
	if second.Secret.IsDefault {
		t.Fatal("a second token created with default=false must not be the default")
	}

	// --- Duplicate label (case-insensitive) is a 409 ---
	rec = h.createToken(t, user, "Console", "tok-dup-value", false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate label: code %d, want 409", rec.Code)
	}

	// --- List: default first, both present, no value fields ---
	secrets := listSecrets(t, h, user)
	if len(secrets) != 2 {
		t.Fatalf("list has %d, want 2", len(secrets))
	}
	if secrets[0].Label != "primary" || !secrets[0].IsDefault {
		t.Fatalf("list[0] = %+v, want the default 'primary' first", secrets[0])
	}

	// --- Rename the second ---
	rec = httptest.NewRecorder()
	h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+second.Secret.ID,
		`{"label":"cheap-console"}`, user, map[string]string{"id": second.Secret.ID}))
	if rec.Code != http.StatusOK || decodeSecret(t, rec).Secret.Label != "cheap-console" {
		t.Fatalf("rename: code %d body %s", rec.Code, rec.Body.String())
	}

	// --- Set the second as default: the swap must leave exactly one default ---
	rec = httptest.NewRecorder()
	h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+second.Secret.ID,
		`{"default":true}`, user, map[string]string{"id": second.Secret.ID}))
	if rec.Code != http.StatusOK || !decodeSecret(t, rec).Secret.IsDefault {
		t.Fatalf("set-default: code %d body %s", rec.Code, rec.Body.String())
	}
	if defaults := countDefaults(t, pool, user); defaults != 1 {
		t.Fatalf("after set-default there are %d defaults, want exactly 1", defaults)
	}
	secrets = listSecrets(t, h, user)
	for _, s := range secrets {
		wantDefault := s.ID == second.Secret.ID
		if s.IsDefault != wantDefault {
			t.Fatalf("token %s is_default=%v, want %v after the swap", s.Label, s.IsDefault, wantDefault)
		}
	}

	// --- default=false is refused (you promote another, never un-default) ---
	rec = httptest.NewRecorder()
	h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+second.Secret.ID,
		`{"default":false}`, user, map[string]string{"id": second.Secret.ID}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("default=false: code %d, want 400", rec.Code)
	}

	// --- Rotate the (now non-default) first token's value; label/default unchanged ---
	rec = httptest.NewRecorder()
	h.PatchAnthropicToken(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+first.Secret.ID,
		`{"token":"tok-primary-rotated"}`, user, map[string]string{"id": first.Secret.ID}))
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: code %d body %s", rec.Code, rec.Body.String())
	}
	if got := decodeSecret(t, rec); got.Secret.Label != "primary" || got.Secret.IsDefault {
		t.Fatalf("rotate changed label/default: %+v", got.Secret)
	}

	// --- D6: deleting the DEFAULT while others exist is a 409 ---
	rec = httptest.NewRecorder()
	h.DeleteAnthropicTokenByID(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+second.Secret.ID,
		"", user, map[string]string{"id": second.Secret.ID}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete-default-while-others-exist: code %d, want 409", rec.Code)
	}

	// --- Deleting a NON-default is fine ---
	rec = httptest.NewRecorder()
	h.DeleteAnthropicTokenByID(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+first.Secret.ID,
		"", user, map[string]string{"id": first.Secret.ID}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete non-default: code %d, want 204", rec.Code)
	}

	// --- Now the default is the last token: deleting it IS allowed (token-less) ---
	rec = httptest.NewRecorder()
	h.DeleteAnthropicTokenByID(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+second.Secret.ID,
		"", user, map[string]string{"id": second.Secret.ID}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete last token: code %d, want 204", rec.Code)
	}
	if len(listSecrets(t, h, user)) != 0 {
		t.Fatal("user should be token-less after deleting the last token")
	}

	// --- A foreign secret id is a 404, never another user's row ---
	other := mkSecretUser(t, pool)
	h.createToken(t, other, "theirs", "tok-theirs", false)
	otherSecrets := listSecrets(t, h, other)
	rec = httptest.NewRecorder()
	h.DeleteAnthropicTokenByID(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+otherSecrets[0].ID,
		"", user, map[string]string{"id": otherSecrets[0].ID}))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleting another user's token: code %d, want 404", rec.Code)
	}
}

// TestDeleteAliasMultiToken409LiveDB: the D14 kind-path DELETE alias 409s for a
// multi-token user (delete by id), and returns 204 for a single-token user
// (returning them to token-less) and for a token-less user (idempotent).
func TestDeleteAliasMultiToken409LiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	user := mkSecretUser(t, pool)

	del := func() int {
		rec := httptest.NewRecorder()
		h.DeleteAnthropicToken(rec, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token", "", user, nil))
		return rec.Code
	}

	// Token-less: idempotent 204.
	if code := del(); code != http.StatusNoContent {
		t.Fatalf("token-less alias delete: code %d, want 204", code)
	}
	// One token: 204, returns to token-less.
	h.createToken(t, user, "only", "tok-only", false)
	if code := del(); code != http.StatusNoContent {
		t.Fatalf("single-token alias delete: code %d, want 204", code)
	}
	if len(listSecrets(t, h, user)) != 0 {
		t.Fatal("single-token alias delete should leave the user token-less")
	}
	// Two tokens: 409, delete by id instead.
	h.createToken(t, user, "a", "tok-a", false)
	h.createToken(t, user, "b", "tok-b", false)
	if code := del(); code != http.StatusConflict {
		t.Fatalf("multi-token alias delete: code %d, want 409 (D14)", code)
	}
	// And nothing was deleted.
	if n := len(listSecrets(t, h, user)); n != 2 {
		t.Fatalf("a refused alias delete removed a token: %d remain, want 2", n)
	}
}

// TestConcurrentFirstTokenCreatesLiveDB is a D12 acceptance criterion: N concurrent
// FIRST-token creates for one user must not both become default, AND none may fail.
//
// Both halves matter, and they are guarded by different things — which is the point
// worth not losing. The partial unique index protects the DATA: it makes two
// defaults impossible whether or not the lock is there, so the countDefaults
// assertion alone would still pass with the lock removed. The advisory lock
// protects the USER-VISIBLE OUTCOME: without it the losing goroutines collide on
// user_secrets_one_default_key, the handler maps that 23505 to a 500, and the
// default arm of the switch below trips.
//
// Measured, not reasoned: neutralising only the pg_advisory_xact_lock call (keeping
// the transaction) fails this test on every run with several `concurrent create
// returned 500`. So this test is NOT redundant with the delete-vs-create one — that
// one proves the lock prevents a corrupt state, this one proves it prevents a
// broken response. Deleting either loses a real guarantee.
func TestConcurrentFirstTokenCreatesLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	user := mkSecretUser(t, pool)

	const n = 8
	var created, conflicts atomic.Int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Each asks for default=true with a DISTINCT label, so the label index does
			// not serialize them — the default invariant is what is under test.
			rec := h.createToken(t, user, fmt.Sprintf("tok-%d", i), fmt.Sprintf("value-%d", i), true)
			switch rec.Code {
			case http.StatusCreated:
				created.Add(1)
			case http.StatusConflict:
				conflicts.Add(1)
			default:
				t.Errorf("concurrent create returned %d: %s", rec.Code, rec.Body.String())
			}
		}(i)
	}
	wg.Wait()

	// Every distinct-label create should succeed (labels do not collide); what must
	// NOT happen is two defaults.
	if defaults := countDefaults(t, pool, user); defaults != 1 {
		t.Fatalf("after %d concurrent creates there are %d defaults, want exactly 1", n, defaults)
	}
	total := countTokens(t, pool, user)
	if int64(total) != created.Load() {
		t.Fatalf("rows=%d but %d creates reported success", total, created.Load())
	}
	if created.Load() == 0 {
		t.Fatal("no create succeeded")
	}
}

// TestConcurrentDeleteDefaultVsCreateLiveDB is the other D12 race: a delete of the
// sole (default) token racing a second-token create must never leave the user with
// tokens and no default. Whichever order they serialize in, at the end either the
// user is token-less, or they have token(s) with exactly one default — never
// token(s) with zero defaults.
func TestConcurrentDeleteDefaultVsCreateLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)

	// Run the race many times to shake out the interleaving.
	for iter := 0; iter < 25; iter++ {
		user := mkSecretUser(t, pool)
		// Seed one token (the default).
		rec := h.createToken(t, user, "seed", "tok-seed", false)
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
		}

		var wg sync.WaitGroup
		wg.Add(2)
		// A: alias delete (removes the sole default, or 409s if a second appeared first).
		go func() {
			defer wg.Done()
			r := httptest.NewRecorder()
			h.DeleteAnthropicToken(r, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token", "", user, nil))
		}()
		// B: create a second token, non-default.
		go func() {
			defer wg.Done()
			h.createToken(t, user, "second", "tok-second", false)
		}()
		wg.Wait()

		// The invariant: if the user has any token, exactly one is default. Never zero.
		if n := countTokens(t, pool, user); n > 0 {
			if d := countDefaults(t, pool, user); d != 1 {
				t.Fatalf("iter %d: user has %d tokens but %d defaults — the delete-vs-create race left a no-default state",
					iter, n, d)
			}
		}
	}
}

func countDefaults(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token' AND is_default`, user).Scan(&n); err != nil {
		t.Fatalf("count defaults: %v", err)
	}
	return n
}

func countTokens(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'`, user).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

// TestAliasVsLockedMutationsInvariantLiveDB covers the one interaction the other
// two concurrency tests miss: the UNLOCKED writer against the locked ones.
//
// PUT /api/me/secrets/anthropic_token (the D14 alias) is the single deliberate
// exception to M2's serialization scheme — it takes no advisory lock, on the
// reasoning that a single INSERT .. ON CONFLICT DO UPDATE is atomic and so cannot
// interleave the invariant into a two-default or no-default state. That is exactly
// the kind of reasoning that stops being true when someone changes the arbiter,
// adds a lock, or adds a fourth writer, and until now it was asserted nowhere:
// TestConcurrentFirstTokenCreates is create-vs-create and
// TestConcurrentDeleteDefaultVsCreate is delete-vs-create. Neither fires the alias.
//
// So: hammer one user with all four mutations at once — locked create-as-default,
// locked set-default, locked delete, and the unlocked alias PUT — and assert the
// invariant after every round. tokens > 0 ⇒ exactly one default, always.
//
// What this test does and does not prove, measured rather than assumed:
//
//   - It DOES bite. Breaking a locked mutation so it clears the default without
//     re-setting it (a half-done set-default swap) fails this within three rounds:
//     `user holds 2 tokens but 0 defaults`.
//   - It does NOT catch an alias rewritten to insert a NON-default row — and that
//     turned out to be informative rather than a gap. The only alias miswrite that
//     could produce a SECOND default is rejected by the partial unique index
//     before it commits, so the alias's blast radius on this invariant is smaller
//     than its unlocked status suggests. The index is doing more of the work here
//     than the atomicity argument is.
func TestAliasVsLockedMutationsInvariantLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)

	for round := 0; round < 15; round++ {
		user := mkSecretUser(t, pool)
		// Seed two tokens so set-default and delete both have something to act on
		// and the alias has a default to rotate.
		if rec := h.createToken(t, user, "alpha", "tok-alpha", false); rec.Code != http.StatusCreated {
			t.Fatalf("round %d seed alpha: %d %s", round, rec.Code, rec.Body.String())
		}
		if rec := h.createToken(t, user, "beta", "tok-beta", false); rec.Code != http.StatusCreated {
			t.Fatalf("round %d seed beta: %d %s", round, rec.Code, rec.Body.String())
		}
		listed := listSecrets(t, h, user)
		if len(listed) != 2 {
			t.Fatalf("round %d: seeded %d tokens, want 2", round, len(listed))
		}
		beta := listed[0].ID
		for _, s := range listed {
			if s.Label == "beta" {
				beta = s.ID
			}
		}

		var wg sync.WaitGroup
		wg.Add(4)
		// 1. Locked: create a THIRD token asking to be the default (clear-then-insert).
		go func() {
			defer wg.Done()
			h.createToken(t, user, "gamma", "tok-gamma", true)
		}()
		// 2. Locked: promote beta (the clear-then-set swap).
		go func() {
			defer wg.Done()
			r := httptest.NewRecorder()
			h.PatchAnthropicToken(r, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+beta,
				`{"default":true}`, user, map[string]string{"id": beta}))
		}()
		// 3. Locked: delete beta (409s when it is the default and others exist).
		go func() {
			defer wg.Done()
			r := httptest.NewRecorder()
			h.DeleteAnthropicTokenByID(r, userReq(http.MethodDelete, "/api/me/secrets/anthropic_token/"+beta,
				"", user, map[string]string{"id": beta}))
		}()
		// 4. UNLOCKED: the D14 alias, rotating-or-creating the default.
		go func() {
			defer wg.Done()
			r := httptest.NewRecorder()
			h.PutAnthropicToken(r, userReq(http.MethodPut, "/api/me/secrets/anthropic_token",
				`{"token":"tok-via-alias"}`, user, nil))
		}()
		wg.Wait()

		if n := countTokens(t, pool, user); n > 0 {
			if d := countDefaults(t, pool, user); d != 1 {
				t.Fatalf("round %d: user holds %d tokens but %d defaults — the unlocked alias interleaved "+
					"with a locked mutation and broke the invariant", round, n, d)
			}
		}
	}
}
