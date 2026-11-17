package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnthropicTokenSavePokesDefaultOnlyLiveDB (#4406): a token save (the legacy PUT
// and the create-by-label POST) keeps the single-token poke, one Poke(owner) per save,
// and never asks for the vault-ready user-wide poll or a named-token poll.
func TestAnthropicTokenSavePokesDefaultOnlyLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	poker := &recordingUsagePoker{}
	h.SetUsagePoker(poker)
	user := mkSecretUser(t, pool)

	expect := func(step string, wantPokes int) {
		t.Helper()
		users, secrets := poker.snapshot()
		if len(users) != wantPokes || len(secrets) != 0 || len(poker.snapshotAll()) != 0 {
			t.Fatalf("%s: Poke=%d PokeSecret=%d PokeUserAll=%d, want %d/0/0", step, len(users), len(secrets), len(poker.snapshotAll()), wantPokes)
		}
		for _, owner := range users {
			if owner != user {
				t.Fatalf("%s: poked %s, want %s", step, owner, user)
			}
		}
	}

	rec := httptest.NewRecorder()
	h.PutAnthropicToken(rec, userReq(http.MethodPut, "/api/me/secrets/anthropic_token", `{"token":"tok-put-value"}`, user, nil))
	if rec.Code >= 300 {
		t.Fatalf("put: code %d, body %s", rec.Code, rec.Body.String())
	}
	expect("put", 1)

	if rec := h.createToken(t, user, "second", "tok-second-value", false); rec.Code != http.StatusCreated {
		t.Fatalf("create: code %d, body %s", rec.Code, rec.Body.String())
	}
	expect("create", 2)
}
