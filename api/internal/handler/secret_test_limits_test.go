package handler

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestSecretConnectionPerIDLimitLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	id := seedTestConnection(t, h, pool, owner, "openai_api_key", "fixture-per-id")
	var calls atomic.Int32
	installTestTransport(h, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelsResponse(http.StatusOK, liveModels)
	})
	path := testConnectionPath("openai_api_key", id)
	for i := 0; i < 3; i++ {
		requireTestResult(t, cookieReq(t, router, http.MethodPost, path, session, ""), http.StatusOK, "ok", "")
	}
	if rec := cookieReq(t, router, http.MethodPost, path, session, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth probe = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("provider calls = %d, want 3", got)
	}
}

func TestSecretConnectionOwnerLimitAcrossIDsLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	session := cliMintJWT(t, pool, owner)
	var calls atomic.Int32
	installTestTransport(h, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return modelsResponse(http.StatusOK, liveModels)
	})
	ids := make([]uuid.UUID, 5)
	for i := range ids {
		ids[i] = seedTestConnection(t, h, pool, owner, "openai_api_key", "fixture-owner-limit")
	}
	for i := 0; i < 12; i++ {
		id := ids[i/3]
		requireTestResult(t, cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", id), session, ""),
			http.StatusOK, "ok", "")
	}
	rec := cookieReq(t, router, http.MethodPost, testConnectionPath("openai_api_key", ids[4]), session, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("thirteenth probe on fresh ID = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 12 {
		t.Fatalf("provider calls = %d, want 12", got)
	}
}
