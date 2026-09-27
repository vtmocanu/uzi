package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PRD #1732 M1 (D4/D11) live-DB coverage beyond the transition test: dependents
// pagination, the shared Codex default slot, create-after-all-disabled, and the
// legacy PUT racing a disable.

// insertEnablementSecret inserts one credential row directly (no vault, no handler).
func insertEnablementSecret(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID, kind, label string, isDefault bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		VALUES ($1, $2, $3, $4, $5, 'x', 'master')`, id, user, kind, label, isDefault)
	return id
}

// secretSlotViolations returns every per-slot breach of the default invariant for a
// user: a disabled default, two defaults, or enabled credentials with no default.
// The Codex kinds share one slot.
func secretSlotViolations(ctx context.Context, pool *pgxpool.Pool, user uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT slot,
		       count(*) FILTER (WHERE is_default),
		       count(*) FILTER (WHERE is_default AND disabled_at IS NOT NULL),
		       count(*) FILTER (WHERE disabled_at IS NULL)
		FROM (SELECT CASE WHEN kind = 'anthropic_token' THEN 'anthropic' ELSE 'codex' END AS slot,
		             is_default, disabled_at
		      FROM user_secrets WHERE user_id = $1) s
		GROUP BY slot`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var slot string
		var defaults, disabledDefaults, enabled int64
		if err := rows.Scan(&slot, &defaults, &disabledDefaults, &enabled); err != nil {
			return nil, err
		}
		if defaults > 1 {
			out = append(out, fmt.Sprintf("%s: %d defaults", slot, defaults))
		}
		if disabledDefaults > 0 {
			out = append(out, fmt.Sprintf("%s: %d disabled defaults", slot, disabledDefaults))
		}
		if enabled > 0 && defaults == 0 {
			out = append(out, fmt.Sprintf("%s: %d enabled credentials but no default", slot, enabled))
		}
	}
	return out, rows.Err()
}

func requireSlotInvariant(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
	t.Helper()
	bad, err := secretSlotViolations(t.Context(), pool, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("slot invariant violated: %v", bad)
	}
}

type enablementDependentPage struct {
	Items []struct {
		ID uuid.UUID `json:"id"`
	} `json:"items"`
	Total      int64  `json:"total"`
	NextCursor string `json:"next_cursor"`
}

func dependentsRequest(t *testing.T, h *Handler, user uuid.UUID, kind string, id uuid.UUID, query url.Values) map[string]enablementDependentPage {
	t.Helper()
	rec := httptest.NewRecorder()
	path := "/api/me/secrets/" + kind + "/" + id.String() + "/dependents?" + query.Encode()
	h.GetSecretDependents(rec, userReq(http.MethodGet, path, "", user, map[string]string{"kind": kind, "id": id.String()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode dependents: %v: %s", err, rec.Body.String())
	}
	out := map[string]enablementDependentPage{}
	for _, name := range []string{"workers", "schedules", "runs", "enabled_siblings"} {
		var page enablementDependentPage
		if err := json.Unmarshal(raw[name], &page); err != nil {
			t.Fatalf("decode %s: %v: %s", name, err, raw[name])
		}
		out[name] = page
	}
	return out
}

// walkDependentSection follows one section's cursor to the end, asserting the
// total on every page, and returns the ids seen plus the number of pages.
func walkDependentSection(t *testing.T, h *Handler, user uuid.UUID, kind string, id uuid.UUID, section string, limit int, wantTotal int64) ([]uuid.UUID, int) {
	t.Helper()
	var seen []uuid.UUID
	cursor := ""
	pages := 0
	for {
		q := url.Values{"limit": {fmt.Sprint(limit)}}
		if cursor != "" {
			q.Set(section+"_cursor", cursor)
		}
		page := dependentsRequest(t, h, user, kind, id, q)[section]
		pages++
		if page.Total != wantTotal {
			t.Fatalf("%s page %d total = %d, want %d", section, pages, page.Total, wantTotal)
		}
		if len(page.Items) > limit {
			t.Fatalf("%s page %d has %d items, limit %d", section, pages, len(page.Items), limit)
		}
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		if page.NextCursor == "" {
			return seen, pages
		}
		if len(page.Items) != limit {
			t.Fatalf("%s page %d: short page (%d) still carries a cursor", section, pages, len(page.Items))
		}
		cursor = page.NextCursor
		if pages > 50 {
			t.Fatalf("%s: cursor never terminated", section)
		}
	}
}

func requireSameIDSet(t *testing.T, section string, got, want []uuid.UUID) {
	t.Helper()
	seen := map[uuid.UUID]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("%s: duplicate id %s across pages", section, id)
		}
		seen[id] = true
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d ids, want %d", section, len(got), len(want))
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("%s: missing id %s", section, id)
		}
	}
}

func TestSecretDependentsPaginationLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	ctx := t.Context()
	user := mkSecretUser(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })

	secret := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "bound", true)
	other := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "other", false)

	connID, repoID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, user, []byte{0x1})
	mustExecT(ctx, t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, 'g/deps', 'https://forge.e2e/g/deps', 'main', true)`, repoID, connID)

	const n = 5 // limit 2 => 3 pages per section
	var workers, schedules, runs []uuid.UUID
	for i := 0; i < n; i++ {
		w := uuid.New()
		mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, anthropic_secret_id, anthropic_bind_mode)
			VALUES ($1, $2, $3, $4, $5, 'pinned')`, w, user, fmt.Sprintf("w-%d", i), w[:], secret)
		workers = append(workers, w)

		s := uuid.New()
		mustExecT(ctx, t, pool, `INSERT INTO run_schedules (id, user_id, repo_id, target, timing, cron_expr, credential_override_mode, credential_override_secret_id)
			VALUES ($1, $2, $3, 'sweep', 'recurring', '0 * * * *', 'pinned', $4)`, s, user, repoID, secret)
		schedules = append(schedules, s)

		r := uuid.New()
		status := []string{"queued", "running", "awaiting_approval", "limit_wait", "paused"}[i]
		if i%2 == 0 {
			mustExecT(ctx, t, pool, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, anthropic_secret_id)
				VALUES ($1, $2, 'chat', 't', 'd', $3, $4)`, r, user, status, secret)
		} else {
			mustExecT(ctx, t, pool, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, credential_override_mode, credential_override_secret_id)
				VALUES ($1, $2, 'chat', 't', 'd', $3, 'pinned', $4)`, r, user, status, secret)
		}
		runs = append(runs, r)
	}
	// Noise the pages must exclude: terminal runs, and dependents of another secret.
	for _, status := range []string{"completed", "failed", "cancelled"} {
		mustExecT(ctx, t, pool, `INSERT INTO runs (user_id, kind, issue_title, issue_description, status, anthropic_secret_id)
			VALUES ($1, 'chat', 't', 'd', $2, $3)`, user, status, secret)
	}
	noise := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, anthropic_secret_id, anthropic_bind_mode)
		VALUES ($1, $2, 'noise', $3, $4, 'pinned')`, noise, user, noise[:], other)
	mustExecT(ctx, t, pool, `INSERT INTO run_schedules (user_id, repo_id, target, timing, cron_expr, credential_override_mode, credential_override_secret_id)
		VALUES ($1, $2, 'sweep', 'recurring', '0 * * * *', 'pinned', $3)`, user, repoID, other)
	mustExecT(ctx, t, pool, `INSERT INTO runs (user_id, kind, issue_title, issue_description, status, anthropic_secret_id)
		VALUES ($1, 'chat', 't', 'd', 'running', $2)`, user, other)

	for section, want := range map[string][]uuid.UUID{"workers": workers, "schedules": schedules, "runs": runs} {
		got, pages := walkDependentSection(t, h, user, "anthropic_token", secret, section, 2, n)
		if pages != 3 {
			t.Errorf("%s: %d pages, want 3", section, pages)
		}
		requireSameIDSet(t, section, got, want)
	}

	// Enabled Codex siblings: aliases of the same provider account.
	codex := insertEnablementSecret(ctx, t, pool, user, "codex_auth", "codex-main", true)
	var account, otherAccount uuid.UUID
	for i, dst := range []*uuid.UUID{&account, &otherAccount} {
		if err := pool.QueryRow(ctx, `INSERT INTO codex_provider_account (user_id, provider_user_id, workspace_account_id, sealed_login, sealed_with)
			VALUES ($1, $2, 'ws', 'x', 'master') RETURNING id`, user, fmt.Sprintf("pu-%d-%s", i, uuid.NewString())).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	link := func(secretID, acct uuid.UUID) {
		mustExecT(ctx, t, pool, `INSERT INTO codex_credential_state (user_secret_id, user_id, status, provider_account_id)
			VALUES ($1, $2, 'linked', $3)`, secretID, user, acct)
	}
	link(codex, account)
	var siblings []uuid.UUID
	for i := 0; i < n; i++ {
		s := insertEnablementSecret(ctx, t, pool, user, "codex_auth", fmt.Sprintf("alias-%d", i), false)
		link(s, account)
		siblings = append(siblings, s)
	}
	disabledSibling := insertEnablementSecret(ctx, t, pool, user, "codex_auth", "alias-disabled", false)
	link(disabledSibling, account)
	mustExecT(ctx, t, pool, `UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, disabledSibling)
	link(insertEnablementSecret(ctx, t, pool, user, "codex_auth", "other-account", false), otherAccount)

	got, pages := walkDependentSection(t, h, user, "codex_auth", codex, "enabled_siblings", 2, n)
	if pages != 3 {
		t.Errorf("enabled_siblings: %d pages, want 3", pages)
	}
	requireSameIDSet(t, "enabled_siblings", got, siblings)

	// Exact-multiple edge: limit 5 over 5 items is one page with no cursor.
	page := dependentsRequest(t, h, user, "anthropic_token", secret, url.Values{"limit": {"5"}})["workers"]
	if len(page.Items) != n || page.NextCursor != "" {
		t.Fatalf("exact-multiple page: %d items, cursor %q", len(page.Items), page.NextCursor)
	}
}

func TestSecretEnablementSharedCodexSlotLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	ctx := t.Context()
	user := mkSecretUser(t, pool)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })

	login := insertEnablementSecret(ctx, t, pool, user, "codex_auth", "login", true)
	key := insertEnablementSecret(ctx, t, pool, user, "openai_api_key", "key", false)
	anthropic := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "anth", true)

	codexDefault := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*), (array_agg(id))[1] FROM user_secrets
			WHERE user_id = $1 AND is_default AND disabled_at IS NULL AND kind IN ('codex_auth', 'openai_api_key')`, user).Scan(&n, &id); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("enabled codex defaults = %d, want 1", n)
		}
		return id
	}

	if code, _ := enablementRequest(t, h, user, "codex_auth", login, `{"enabled":false}`); code != http.StatusConflict {
		t.Fatalf("codex_auth disable without replacement = %d, want 409", code)
	}
	if code, _ := enablementRequest(t, h, user, "codex_auth", login, `{"enabled":false,"new_default_id":"`+anthropic.String()+`"}`); code != http.StatusConflict {
		t.Fatalf("cross-slot replacement = %d, want 409", code)
	}
	if code, body := enablementRequest(t, h, user, "codex_auth", login, `{"enabled":false,"new_default_id":"`+key.String()+`"}`); code != http.StatusOK {
		t.Fatalf("codex_auth -> openai_api_key hand-off = %d: %v", code, body)
	}
	if got := codexDefault(); got != key {
		t.Fatalf("codex default = %s, want openai_api_key %s", got, key)
	}
	requireSlotInvariant(t, pool, user)

	// Re-enable the login: the slot already has a default, so it stays non-default.
	if code, _ := enablementRequest(t, h, user, "codex_auth", login, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("re-enable login = %d", code)
	}
	if got := codexDefault(); got != key {
		t.Fatalf("re-enable displaced default: %s", got)
	}
	// The reverse hand-off: openai_api_key default -> codex_auth.
	if code, body := enablementRequest(t, h, user, "openai_api_key", key, `{"enabled":false,"new_default_id":"`+login.String()+`"}`); code != http.StatusOK {
		t.Fatalf("openai_api_key -> codex_auth hand-off = %d: %v", code, body)
	}
	if got := codexDefault(); got != login {
		t.Fatalf("codex default = %s, want codex_auth %s", got, login)
	}
	requireSlotInvariant(t, pool, user)
	var anthropicDefault bool
	if err := pool.QueryRow(ctx, `SELECT is_default FROM user_secrets WHERE id = $1`, anthropic).Scan(&anthropicDefault); err != nil || !anthropicDefault {
		t.Fatalf("anthropic default disturbed by codex hand-off: %v %v", anthropicDefault, err)
	}
}

func createSecretViaRoute(t *testing.T, h *Handler, user uuid.UUID, kind, label, value string) (int, secretResp) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"token": value, "label": label, "default": false})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := userReq(http.MethodPost, "/api/me/secrets/"+kind, string(body), user, nil)
	switch kind {
	case "anthropic_token":
		h.CreateAnthropicToken(rec, req)
	case "codex_auth":
		h.CreateCodexAuth(rec, req)
	default:
		h.CreateOpenAIAPIKey(rec, req)
	}
	var out secretResp
	if rec.Code == http.StatusCreated {
		out = decodeSecret(t, rec)
	}
	return rec.Code, out
}

func requireNonDefault(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		var def, disabled bool
		if err := pool.QueryRow(t.Context(), `SELECT is_default, disabled_at IS NOT NULL FROM user_secrets WHERE id = $1`, id).Scan(&def, &disabled); err != nil {
			t.Fatal(err)
		}
		if def || !disabled {
			t.Fatalf("%s: is_default=%v disabled=%v, want disabled non-default", id, def, disabled)
		}
	}
}

func TestSecretCreateAfterAllDisabledLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)

	t.Run("anthropic POST", func(t *testing.T) {
		user := mkSecretUser(t, pool)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
		_, first := createSecretViaRoute(t, h, user, "anthropic_token", "first", "tok-one")
		_, second := createSecretViaRoute(t, h, user, "anthropic_token", "second", "tok-two")
		a, b := uuid.MustParse(first.Secret.ID), uuid.MustParse(second.Secret.ID)
		if code, _ := enablementRequest(t, h, user, "anthropic_token", b, `{"enabled":false}`); code != http.StatusOK {
			t.Fatalf("disable non-default = %d", code)
		}
		if code, _ := enablementRequest(t, h, user, "anthropic_token", a, `{"enabled":false}`); code != http.StatusOK {
			t.Fatalf("disable last default = %d", code)
		}
		code, created := createSecretViaRoute(t, h, user, "anthropic_token", "third", "tok-three")
		if code != http.StatusCreated || !created.Secret.IsDefault {
			t.Fatalf("create into empty slot = %d default=%v, want 201 default", code, created.Secret.IsDefault)
		}
		requireNonDefault(t, pool, a, b)
		requireSlotInvariant(t, pool, user)
	})

	t.Run("anthropic legacy PUT", func(t *testing.T) {
		user := mkSecretUser(t, pool)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
		put := func() (int, secretResp) {
			rec := httptest.NewRecorder()
			h.PutAnthropicToken(rec, userReq(http.MethodPut, "/api/me/secrets/anthropic_token", `{"token":"tok-legacy"}`, user, nil))
			var out secretResp
			if rec.Code == http.StatusOK {
				out = decodeSecret(t, rec)
			}
			return rec.Code, out
		}
		_, orig := put() // label 'default'
		_, extra := createSecretViaRoute(t, h, user, "anthropic_token", "extra", "tok-extra")
		a, b := uuid.MustParse(orig.Secret.ID), uuid.MustParse(extra.Secret.ID)
		for _, id := range []uuid.UUID{b, a} {
			if code, _ := enablementRequest(t, h, user, "anthropic_token", id, `{"enabled":false}`); code != http.StatusOK {
				t.Fatalf("disable %s = %d", id, code)
			}
		}
		code, created := put()
		if code != http.StatusOK || !created.Secret.IsDefault {
			t.Fatalf("PUT into empty slot = %d default=%v", code, created.Secret.IsDefault)
		}
		if id := uuid.MustParse(created.Secret.ID); id == a || id == b {
			t.Fatalf("PUT rotated disabled row %s instead of creating one", id)
		}
		requireNonDefault(t, pool, a, b)
		requireSlotInvariant(t, pool, user)
	})

	t.Run("codex shared slot", func(t *testing.T) {
		user := mkSecretUser(t, pool)
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, user) })
		_, login := createSecretViaRoute(t, h, user, "codex_auth", "login", `{"access_token":"at-one"}`)
		_, key := createSecretViaRoute(t, h, user, "openai_api_key", "key", "plain-test-value-1")
		a, b := uuid.MustParse(login.Secret.ID), uuid.MustParse(key.Secret.ID)
		if !login.Secret.IsDefault || key.Secret.IsDefault {
			t.Fatalf("setup defaults: login=%v key=%v", login.Secret.IsDefault, key.Secret.IsDefault)
		}
		if code, _ := enablementRequest(t, h, user, "openai_api_key", b, `{"enabled":false}`); code != http.StatusOK {
			t.Fatalf("disable key = %d", code)
		}
		if code, _ := enablementRequest(t, h, user, "codex_auth", a, `{"enabled":false}`); code != http.StatusOK {
			t.Fatalf("disable last codex = %d", code)
		}
		for i, kind := range []string{"openai_api_key", "codex_auth"} {
			code, created := createSecretViaRoute(t, h, user, kind, fmt.Sprintf("new-%d", i), map[string]string{
				"openai_api_key": "plain-test-value-2", "codex_auth": `{"access_token":"at-two"}`,
			}[kind])
			if code != http.StatusCreated {
				t.Fatalf("create %s = %d", kind, code)
			}
			// The first create fills the empty shared slot; the second must not steal it.
			if created.Secret.IsDefault != (i == 0) {
				t.Fatalf("create %s default=%v, want %v", kind, created.Secret.IsDefault, i == 0)
			}
		}
		requireNonDefault(t, pool, a, b)
		requireSlotInvariant(t, pool, user)
	})
}

// TestConcurrentLegacyPutVsDisableLiveDB races the deprecated PUT (rotate/create the
// default) against a PATCH disabling the current default, with and without an
// enabled replacement. Both paths take the same per-user advisory lock, so every
// interleaving must leave each slot with no disabled default and at most one default.
func TestConcurrentLegacyPutVsDisableLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	const iterations = 25
	for _, withReplacement := range []bool{true, false} {
		t.Run(fmt.Sprintf("replacement=%v", withReplacement), func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				ctx := t.Context()
				user := mkSecretUser(t, pool)
				def := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "default", true)
				body := `{"enabled":false}`
				if withReplacement {
					repl := insertEnablementSecret(ctx, t, pool, user, "anthropic_token", "spare", false)
					body = `{"enabled":false,"new_default_id":"` + repl.String() + `"}`
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				var putCode, patchCode int
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					rec := httptest.NewRecorder()
					h.PutAnthropicToken(rec, userReq(http.MethodPut, "/api/me/secrets/anthropic_token", `{"token":"tok-race"}`, user, nil))
					putCode = rec.Code
				}()
				go func() {
					defer wg.Done()
					<-start
					rec := httptest.NewRecorder()
					h.PatchSecretEnabled(rec, userReq(http.MethodPatch, "/api/me/secrets/anthropic_token/"+def.String()+"/enabled", body, user,
						map[string]string{"kind": "anthropic_token", "id": def.String()}))
					patchCode = rec.Code
				}()
				close(start)
				wg.Wait()
				if putCode != http.StatusOK || patchCode != http.StatusOK {
					t.Fatalf("iteration %d: PUT=%d PATCH=%d, want 200/200", i, putCode, patchCode)
				}
				bad, err := secretSlotViolations(ctx, pool, user)
				if err != nil {
					t.Fatal(err)
				}
				if len(bad) > 0 {
					t.Fatalf("iteration %d: %v", i, bad)
				}
				var disabled bool
				if err := pool.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM user_secrets WHERE id = $1`, def).Scan(&disabled); err != nil || !disabled {
					t.Fatalf("iteration %d: original default not disabled (%v, %v)", i, disabled, err)
				}
				if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
