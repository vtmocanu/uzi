package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// provider_key_scope_livedb_test.go pins PRD #4320 D2 against real Postgres: a provider key
// (anthropic_compatible_key) is never bindable or openable as an Anthropic token through the
// by-id paths. Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

func seedProviderKey(t *testing.T, fx *cdFix, plaintext string) uuid.UUID {
	t.Helper()
	provider, key := uuid.New(), uuid.New()
	fx.env.exec(`INSERT INTO model_providers (id, owner_user_id, scope_owner_id, name, engine, current_revision) VALUES ($1, $2, $2, $3, 'claude', 1)`,
		provider, fx.userID, "gw-"+provider.String())
	fx.env.exec(`INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
	    VALUES ($1, 1, 'anthropic_messages', 'https://gw.example.com/anthropic', 'bearer', '{}')`, provider)
	sealed, err := fx.env.box.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal provider key: %v", err)
	}
	fx.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
	    VALUES ($1, $2, 'anthropic_compatible_key', $3, false, false, $4, 'master', $5, $2)`,
		key, fx.userID, provider.String(), sealed, provider)
	return key
}

func TestProviderKeyIsNeverAnAnthropicTokenLiveDB(t *testing.T) {
	fx := newCDFix(t)
	const plain = "dummy-gateway-key-0001"
	key := seedProviderKey(t, fx, plain)

	if _, err := fx.svc.SetWorkerAnthropicToken(fx.env.ctx, fx.userID, fx.workerID, BindModePinned, &key); !errors.Is(err, ErrSecretNotOwned) {
		t.Errorf("pinning a provider key to a worker = %v, want ErrSecretNotOwned", err)
	}
	var pinned uuid.UUID
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT anthropic_secret_id FROM workers WHERE id = $1`, fx.workerID).Scan(&pinned); err != nil || pinned != fx.pinTok {
		t.Errorf("worker pin = %v, %v; want it unchanged (%v)", pinned, err, fx.pinTok)
	}
	if _, err := fx.svc.SetUserJudgeBinding(fx.env.ctx, fx.userID, BindModePinned, &key, nil); !errors.Is(err, ErrSecretNotOwned) {
		t.Errorf("pinning a provider key to the judge = %v, want ErrSecretNotOwned", err)
	}

	cred, err := fx.svc.openAnthropic(fx.env.ctx, fx.userID, &key)
	if !errors.Is(err, errNoModelCredential) {
		t.Errorf("openAnthropic(provider key) err = %v, want errNoModelCredential", err)
	}
	if cred.Token != nil || len(cred.Token) != 0 {
		t.Errorf("openAnthropic returned %d token bytes for a provider key", len(cred.Token))
	}

	// An Anthropic token still binds and opens.
	if _, err := fx.svc.SetWorkerAnthropicToken(fx.env.ctx, fx.userID, fx.workerID, BindModePinned, &fx.otherTok); err != nil {
		t.Errorf("pinning an anthropic token: %v", err)
	}
	if cred, err := fx.svc.openAnthropic(fx.env.ctx, fx.userID, &fx.otherTok); err != nil || len(cred.Token) == 0 {
		t.Errorf("openAnthropic(anthropic token) = %d bytes, %v", len(cred.Token), err)
	}
}
