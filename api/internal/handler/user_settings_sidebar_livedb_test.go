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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPutMySettingsSidebarTokensLiveDB proves the sidebar_token_ids WRITE PATH
// end to end over real SQL: HTTP PUT (ownership filter against real user_secrets
// rows) -> SetUserSidebarTokens -> HTTP GET read-back. The store-level LiveDB
// test exercises the queries in isolation; this one is the review-wave gap it
// left — until it existed, the handler's write was only ever proven against the
// fake store.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestPutMySettingsSidebarTokensLiveDB(t *testing.T) {
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
	h := &Handler{pool: pool, q: store.New(pool)}

	user := mkSecretUser(t, pool)
	other := mkSecretUser(t, pool)
	mkTok := func(owner uuid.UUID, label string, isDefault bool) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			 VALUES ($1, $2, 'anthropic_token', $3, $4, 'ct', 'master')`,
			id, owner, label, isDefault); err != nil {
			t.Fatalf("insert secret %s: %v", label, err)
		}
		return id
	}
	deflt := mkTok(user, "default", true)
	extra := mkTok(user, "console-key", false)
	foreign := mkTok(other, "default", true)

	getIds := func() []string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", user, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: code %d, body %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Settings struct {
				SidebarTokenIds []string `json:"sidebar_token_ids"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode GET: %v (body=%s)", err, rec.Body.String())
		}
		return resp.Settings.SidebarTokenIds
	}

	// A fresh user reads the empty (default-only) choice.
	if got := getIds(); len(got) != 0 {
		t.Fatalf("pristine sidebar_token_ids = %v, want empty", got)
	}

	// PUT the extra + a FOREIGN user's id + the caller's default: the real SQL
	// listing must let only the owned non-default id through.
	body := fmt.Sprintf(`{"sidebar_token_ids":[%q,%q,%q]}`, extra, foreign, deflt)
	rec := httptest.NewRecorder()
	h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, user, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: code %d, body %s", rec.Code, rec.Body.String())
	}
	if got := getIds(); len(got) != 1 || got[0] != extra.String() {
		t.Fatalf("read-back sidebar_token_ids = %v, want exactly [%s]", got, extra)
	}

	// Clearing with [] survives the round trip too.
	rec = httptest.NewRecorder()
	h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", `{"sidebar_token_ids":[]}`, user, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT clear: code %d, body %s", rec.Code, rec.Body.String())
	}
	if got := getIds(); len(got) != 0 {
		t.Fatalf("read-back after clear = %v, want empty", got)
	}
}
