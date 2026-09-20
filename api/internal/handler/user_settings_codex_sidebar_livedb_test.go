package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// mkLinkedCodexAccountH inserts a provider account owned by `owner` plus a codex_auth alias
// linked to it (is_default = deflt), returning the account id and alias id. The handler
// package's sibling of the store-test helper — used to build the real linked-account state
// the settings validation reads.
func mkLinkedCodexAccountH(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q *store.Queries, owner uuid.UUID, deflt bool) (accountID, secretID uuid.UUID) {
	t.Helper()
	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: owner, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	secretID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		 VALUES ($1, $2, 'codex_auth', $3, $4, 'ct', 'master')`,
		secretID, owner, "codex-"+uuid.NewString(), deflt); err != nil {
		t.Fatalf("insert codex_auth secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretID, UserID: owner, Status: "staging",
	}); err != nil {
		t.Fatalf("insert credential state: %v", err)
	}
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: secretID, UserID: owner,
		ProviderAccountID: pgtype.UUID{Bytes: acc.ID, Valid: true}, MaterialRevision: 0,
	}); err != nil || n != 1 {
		t.Fatalf("link credential state = (%d, %v), want (1, nil)", n, err)
	}
	return acc.ID, secretID
}

// TestPutMySettingsSidebarCodexAccountsLiveDB proves the sidebar_codex_account_ids write
// path end to end over real SQL (PRD #1209 M1): a linked account stores; the implicit
// default account is EXCLUDED (not 400); a well-formed non-member (unknown or another
// user's) is a 400 with an IDENTICAL message; a malformed id is a 400; and the GET path
// atomically prunes an id whose alias was since unlinked.
func TestPutMySettingsSidebarCodexAccountsLiveDB(t *testing.T) {
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
	q := store.New(pool)
	h := &Handler{pool: pool, q: q}

	user := mkSecretUser(t, pool)
	other := mkSecretUser(t, pool)

	// A default linked account (implicit, always shown), a non-default linked account
	// (storable), and a foreign user's linked account.
	defAcct, defSecret := mkLinkedCodexAccountH(ctx, t, pool, q, user, true)
	extra, _ := mkLinkedCodexAccountH(ctx, t, pool, q, user, false)
	foreign, _ := mkLinkedCodexAccountH(ctx, t, pool, q, other, true)

	getIds := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", user, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: code %d, body %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Settings struct {
				SidebarCodexAccountIds []string `json:"sidebar_codex_account_ids"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode GET: %v (body=%s)", err, rec.Body.String())
		}
		return resp.Settings.SidebarCodexAccountIds
	}
	put := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, user, nil))
		return rec
	}

	// Fresh user reads the empty (default-only) choice.
	if got := getIds(); len(got) != 0 {
		t.Fatalf("pristine sidebar_codex_account_ids = %v, want empty", got)
	}

	// Store the extra + the default account: the default is excluded, not rejected.
	if rec := put(fmt.Sprintf(`{"sidebar_codex_account_ids":[%q,%q]}`, extra, defAcct)); rec.Code != http.StatusOK {
		t.Fatalf("PUT extra+default: code %d, body %s", rec.Code, rec.Body.String())
	}
	if got := getIds(); len(got) != 1 || got[0] != extra.String() {
		t.Fatalf("read-back = %v, want exactly [%s] (default excluded, not stored)", got, extra)
	}

	// A foreign account and an unknown id both 400 with the SAME body — no existence oracle.
	unknown := uuid.New()
	recForeign := put(fmt.Sprintf(`{"sidebar_codex_account_ids":[%q]}`, foreign))
	recUnknown := put(fmt.Sprintf(`{"sidebar_codex_account_ids":[%q]}`, unknown))
	if recForeign.Code != http.StatusBadRequest || recUnknown.Code != http.StatusBadRequest {
		t.Fatalf("non-member ids: foreign=%d unknown=%d, want 400 both", recForeign.Code, recUnknown.Code)
	}
	if recForeign.Body.String() != recUnknown.Body.String() {
		t.Fatalf("400 bodies differ (existence oracle): foreign=%q unknown=%q", recForeign.Body.String(), recUnknown.Body.String())
	}
	// The rejected PUTs wrote nothing — the prior [extra] survives.
	if got := getIds(); len(got) != 1 || got[0] != extra.String() {
		t.Fatalf("after rejected PUTs, read-back = %v, want [%s] unchanged", got, extra)
	}

	// A malformed uuid is a 400.
	if rec := put(`{"sidebar_codex_account_ids":["not-a-uuid"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: code %d, want 400", rec.Code)
	}

	// GET-path prune: unlink the extra account's alias, then GET drops the now-stale id.
	extraSecret := extraAliasSecret(ctx, t, pool, user, extra)
	if _, err := pool.Exec(ctx, `DELETE FROM user_secrets WHERE id=$1`, extraSecret); err != nil {
		t.Fatalf("delete extra alias: %v", err)
	}
	if got := getIds(); len(got) != 0 {
		t.Fatalf("after unlinking the extra account, GET = %v, want [] (prune dropped the stale id)", got)
	}

	_ = defSecret // referenced for clarity; the default alias stays linked throughout
}

// extraAliasSecret resolves the linked codex_auth alias id for `account` owned by `user`,
// so the prune test can delete it.
func extraAliasSecret(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, account uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT user_secret_id FROM codex_credential_state WHERE user_id=$1 AND provider_account_id=$2 AND status='linked' LIMIT 1`,
		user, account).Scan(&id); err != nil {
		t.Fatalf("resolve extra alias: %v", err)
	}
	return id
}
