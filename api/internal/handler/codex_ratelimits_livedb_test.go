package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// codexAccountJSON decodes one account element of a /me or /admin codex-rate-limits
// response, carrying the DTO fields the assertions read.
type codexAccountJSON struct {
	AccountID     string   `json:"account_id"`
	Aliases       []string `json:"aliases"`
	IsDefault     bool     `json:"is_default"`
	Status        string   `json:"status"`
	LastSuccessAt string   `json:"last_success_at"`
	Stale         *bool    `json:"stale"`
	Buckets       []struct {
		ID      string `json:"id"`
		Primary *struct {
			UsedPercent *float64 `json:"used_percent"`
		} `json:"primary"`
	} `json:"buckets"`
}

func codexRLReq(userID uuid.UUID, admin bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, IsAdmin: admin, IsActive: true}))
}

// byFirstAlias keys accounts by their first alias label, which is unique per seeded
// account, so an assertion need not depend on the query's account-id ORDER BY.
func byFirstAlias(accounts []codexAccountJSON) map[string]codexAccountJSON {
	m := map[string]codexAccountJSON{}
	for _, a := range accounts {
		if len(a.Aliases) > 0 {
			m[a.Aliases[0]] = a
		}
	}
	return m
}

// TestCodexRateLimitsReadSurfaceLiveDB exercises GET /me/codex-rate-limits and
// /admin/codex-rate-limits end to end against real Postgres (PRD #1209 M3): every closed-set
// status, duplicate-alias collapse, owner isolation, admin grouping, and the empty-accounts
// shape. Asserts actual DTO field values, not just HTTP status.
func TestCodexRateLimitsReadSurfaceLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)

	sealed, err := box.Seal([]byte(`{"access_token":"a","refresh_token":"r"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	mkUser := func(t *testing.T) (uuid.UUID, string) {
		t.Helper()
		id := uuid.New()
		email := "codexrl-" + uuid.NewString() + "@example.test"
		mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, email)
		return id, email
	}

	// seedAccount inserts a canonical provider account for userID plus one linked codex_auth
	// alias per label (two labels ⇒ one account with two aliases, the dedup case). At most one
	// account per user may pass defaultFirst=true (user_secrets allows one codex default).
	seedAccount := func(t *testing.T, userID uuid.UUID, labels []string, defaultFirst bool) uuid.UUID {
		t.Helper()
		account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: userID, ProviderUserID: "prov-" + uuid.NewString(), WorkspaceAccountID: "ws-" + uuid.NewString(),
			SealedLogin: sealed, SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert provider account: %v", err)
		}
		for i, label := range labels {
			secret, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
				UserID: userID, Kind: store.KindCodexAuth, Label: label, WantDefault: defaultFirst && i == 0,
				Ciphertext: sealed, SealedWith: store.SealedWithMaster,
			})
			if err != nil {
				t.Fatalf("insert codex secret %q: %v", label, err)
			}
			if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
				UserSecretID: secret.ID, UserID: userID, Status: "staging",
			}); err != nil {
				t.Fatalf("insert credential state: %v", err)
			}
			linked, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
				ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: userID, MaterialRevision: 0,
			})
			if err != nil || linked != 1 {
				t.Fatalf("link credential state = (%d, %v), want (1, nil)", linked, err)
			}
		}
		return account.ID
	}

	// seedRL writes a rate-limit snapshot row directly, so last_success_at can be set to a
	// controlled past time (the fenced upsert uses now()). Pass nil for a NULL timestamp/buckets.
	seedRL := func(t *testing.T, userID, accountID uuid.UUID, buckets any, lastSuccess, lastAttempt any) {
		t.Helper()
		mustExecT(ctx, t, pool, `INSERT INTO codex_account_rate_limits
			(user_id, provider_account_id, buckets, observed_generation, observed_credential_revision,
			 last_success_at, last_attempt_at, attempt_status, attempt_error)
			VALUES ($1, $2, $3::jsonb, 0, 0, $4, $5, 'ok', NULL)`,
			userID, accountID, buckets, lastSuccess, lastAttempt)
	}

	buckets := func(pct int) string {
		return fmt.Sprintf(`[{"id":"primary","display_name":"Primary","allowed":true,"limit_reached":false,`+
			`"primary":{"used_percent":%d,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1784000000},`+
			`"secondary":null}]`, pct)
	}

	now := time.Now()
	old := now.Add(-1 * time.Hour) // > 3× the 5m interval ⇒ stale

	userA, emailA := mkUser(t)
	userB, emailB := mkUser(t)
	userEmpty, _ := mkUser(t) // has no codex account at all

	// userA's six accounts, one per status case.
	acctFresh := seedAccount(t, userA, []string{"fresh-sub"}, true)
	seedRL(t, userA, acctFresh, buckets(42), now, now)

	acctStale := seedAccount(t, userA, []string{"stale-sub"}, false)
	seedRL(t, userA, acctStale, buckets(77), old, old)

	acctNoReading := seedAccount(t, userA, []string{"noreading-sub"}, false)
	seedRL(t, userA, acctNoReading, nil, nil, now) // attempted, never succeeded

	seedAccount(t, userA, []string{"pending-sub"}, false) // no snapshot row at all

	acctReauth := seedAccount(t, userA, []string{"reauth-sub"}, false)
	seedRL(t, userA, acctReauth, buckets(10), now, now) // a fresh reading reauth must OVERRIDE
	// reauth_required=true requires the observed (generation, credential_revision) set too
	// (codex_provider_account_reauth_coherence, 00239).
	mustExecT(ctx, t, pool, `UPDATE codex_provider_account
		SET reauth_required = true, reauth_generation = 0, reauth_credential_revision = 0 WHERE id = $1`, acctReauth)

	acctDup := seedAccount(t, userA, []string{"dup-a", "dup-b"}, false) // two aliases ⇒ one account
	seedRL(t, userA, acctDup, buckets(33), now, now)

	// userB: one fresh account, for isolation + admin grouping.
	acctB := seedAccount(t, userB, []string{"userb-sub"}, true)
	seedRL(t, userB, acctB, buckets(5), now, now)

	h := &Handler{pool: pool, q: q, box: box, cfg: config.Config{CodexUsagePollInterval: 5 * time.Minute}}

	decodeAccounts := func(t *testing.T, rec *httptest.ResponseRecorder) []codexAccountJSON {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Accounts []codexAccountJSON `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		return body.Accounts
	}

	t.Run("self", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.SelfCodexRateLimits(rec, codexRLReq(userA, false))
		accounts := decodeAccounts(t, rec)

		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
		}
		if len(accounts) != 6 {
			t.Fatalf("got %d accounts, want 6 (dup collapses to one)", len(accounts))
		}
		byAlias := byFirstAlias(accounts)

		// Owner isolation: userB's account never leaks into userA's read.
		if _, leaked := byAlias["userb-sub"]; leaked {
			t.Fatal("owner read leaked another user's account")
		}

		fresh := byAlias["fresh-sub"]
		if fresh.Status != codexRateLimitStatusFresh {
			t.Errorf("fresh-sub status = %q, want fresh", fresh.Status)
		}
		if !fresh.IsDefault {
			t.Error("fresh-sub should be the default account")
		}
		if fresh.Stale != nil {
			t.Errorf("fresh-sub stale = %v, want nil (omitted)", *fresh.Stale)
		}
		if fresh.LastSuccessAt == "" {
			t.Error("fresh-sub should carry last_success_at")
		}
		if len(fresh.Buckets) != 1 || fresh.Buckets[0].Primary == nil || fresh.Buckets[0].Primary.UsedPercent == nil || *fresh.Buckets[0].Primary.UsedPercent != 42 {
			t.Errorf("fresh-sub buckets = %+v, want one bucket with primary.used_percent 42", fresh.Buckets)
		}

		stale := byAlias["stale-sub"]
		if stale.Status != codexRateLimitStatusStale {
			t.Errorf("stale-sub status = %q, want stale", stale.Status)
		}
		if stale.Stale == nil || !*stale.Stale {
			t.Error("stale-sub should carry stale=true")
		}
		if len(stale.Buckets) != 1 || stale.Buckets[0].Primary.UsedPercent == nil || *stale.Buckets[0].Primary.UsedPercent != 77 {
			t.Errorf("stale-sub buckets = %+v, want primary.used_percent 77", stale.Buckets)
		}

		noReading := byAlias["noreading-sub"]
		if noReading.Status != codexRateLimitStatusNoReading {
			t.Errorf("noreading-sub status = %q, want no_reading", noReading.Status)
		}
		if len(noReading.Buckets) != 0 {
			t.Errorf("noreading-sub buckets = %+v, want empty (never null)", noReading.Buckets)
		}
		if noReading.Buckets == nil {
			t.Error("noreading-sub buckets must be an empty slice, not null")
		}
		if noReading.LastSuccessAt != "" {
			t.Errorf("noreading-sub last_success_at = %q, want empty", noReading.LastSuccessAt)
		}

		pending := byAlias["pending-sub"]
		if pending.Status != codexRateLimitStatusPending {
			t.Errorf("pending-sub status = %q, want pending", pending.Status)
		}
		if len(pending.Buckets) != 0 {
			t.Errorf("pending-sub buckets = %+v, want empty", pending.Buckets)
		}

		reauth := byAlias["reauth-sub"]
		if reauth.Status != codexRateLimitStatusCredentialActionRequired {
			t.Errorf("reauth-sub status = %q, want credential_action_required (reauth overrides a fresh reading)", reauth.Status)
		}

		dup := byAlias["dup-a"]
		if len(dup.Aliases) != 2 || dup.Aliases[0] != "dup-a" || dup.Aliases[1] != "dup-b" {
			t.Errorf("dup account aliases = %v, want [dup-a dup-b] on ONE account (no double budget)", dup.Aliases)
		}
		if dup.Status != codexRateLimitStatusFresh {
			t.Errorf("dup account status = %q, want fresh", dup.Status)
		}
	})

	t.Run("empty accounts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.SelfCodexRateLimits(rec, codexRLReq(userEmpty, false))
		accounts := decodeAccounts(t, rec)
		if len(accounts) != 0 {
			t.Fatalf("got %d accounts, want 0", len(accounts))
		}
		if !strings.Contains(rec.Body.String(), `"accounts":[]`) {
			t.Errorf("empty read body = %s, want an empty (non-null) accounts array", rec.Body.String())
		}
	})

	t.Run("admin grouping", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.AdminCodexRateLimits(rec, codexRLReq(uuid.New(), true))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("Cache-Control = %q, want %q", got, "private, no-store")
		}
		var body struct {
			Users []struct {
				Email       string             `json:"email"`
				Name        string             `json:"name"`
				VaultLocked bool               `json:"vault_locked"`
				Accounts    []codexAccountJSON `json:"accounts"`
			} `json:"users"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		// Other live-DB tests may seed their own users; assert only on ours.
		byEmail := map[string]int{}
		locked := map[string]bool{}
		for _, u := range body.Users {
			byEmail[u.Email] = len(u.Accounts)
			locked[u.Email] = u.VaultLocked
		}
		if byEmail[emailA] != 6 {
			t.Fatalf("admin: %s has %d accounts, want 6 (dup collapsed)", emailA, byEmail[emailA])
		}
		if byEmail[emailB] != 1 {
			t.Fatalf("admin: %s has %d accounts, want 1", emailB, byEmail[emailB])
		}
		if locked[emailA] {
			t.Errorf("%s vault_locked = true with no vault wired", emailA)
		}
	})

	t.Run("vault locked overrides all", func(t *testing.T) {
		hLocked := &Handler{pool: pool, q: q, box: box, cfg: config.Config{CodexUsagePollInterval: 5 * time.Minute}}
		hLocked.vault = vault.New(box, nil) // empty cache ⇒ every user reads locked
		rec := httptest.NewRecorder()
		hLocked.SelfCodexRateLimits(rec, codexRLReq(userA, false))
		accounts := decodeAccounts(t, rec)
		if len(accounts) != 6 {
			t.Fatalf("got %d accounts, want 6", len(accounts))
		}
		for _, a := range accounts {
			if a.Status != codexRateLimitStatusVaultLocked {
				t.Errorf("account %v status = %q, want vault_locked (a locked vault overrides even reauth/fresh)", a.Aliases, a.Status)
			}
		}
	})

	t.Run("polling disabled", func(t *testing.T) {
		hOff := &Handler{pool: pool, q: q, box: box, cfg: config.Config{CodexUsagePollInterval: 0}}
		rec := httptest.NewRecorder()
		hOff.SelfCodexRateLimits(rec, codexRLReq(userA, false))
		accounts := decodeAccounts(t, rec)
		if len(accounts) != 6 {
			t.Fatalf("got %d accounts, want 6", len(accounts))
		}
		for _, a := range accounts {
			if a.Status != codexRateLimitStatusPollingDisabled {
				t.Errorf("account %v status = %q, want polling_disabled", a.Aliases, a.Status)
			}
			if a.Stale == nil || !*a.Stale {
				t.Errorf("account %v stale = %v, want true when the poller is disabled", a.Aliases, a.Stale)
			}
		}
	})
}

// TestCodexVaultUnlockPokesPollerLiveDB proves a successful vault unlock pokes the Codex
// account rate-limit poller (PRD #1209), so a user's account meters refresh within seconds
// of unlocking rather than up to a poll interval later.
func TestCodexVaultUnlockPokesPollerLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	userID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, "codex-unlock-"+uuid.NewString()+"@example.test")

	vlt := vault.New(box, q)
	h := &Handler{pool: pool, q: q, box: box, vault: vlt, cfg: config.Config{CodexUsagePollInterval: 5 * time.Minute}}
	poker := &fakeCodexPoker{}
	h.SetCodexUsagePoker(poker)

	const password = "unlock-password-123"
	// First-ever unlock creates + unlocks the vault; then lock it so the handler exercises
	// the real UnlockExisting success path.
	if err := vlt.Unlock(ctx, userID, password); err != nil {
		t.Fatalf("seed vault: %v", err)
	}
	vlt.Lock(userID)

	req := httptest.NewRequest(http.MethodPost, "/api/vault/unlock", strings.NewReader(`{"password":"`+password+`"}`))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: userID, IsActive: true}))
	rec := httptest.NewRecorder()
	h.VaultUnlock(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("unlock = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if got := poker.count(userID); got != 1 {
		t.Fatalf("vault unlock poked %d times, want exactly 1", got)
	}
}
