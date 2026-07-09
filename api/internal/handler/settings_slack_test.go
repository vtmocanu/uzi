package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSlackValidator records the tokens it was asked to validate and returns a
// configured error, so the settings PUT's live-validation branch runs without a
// network call to Slack.
type fakeSlackValidator struct {
	botErr, appErr error
	gotBot, gotApp string
}

func (f *fakeSlackValidator) ValidateBotToken(_ context.Context, token string) error {
	f.gotBot = token
	return f.botErr
}

func (f *fakeSlackValidator) ValidateAppToken(_ context.Context, token string) error {
	f.gotApp = token
	return f.appErr
}

func slackTestBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 3)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

// TestGetSettingsSecretShape asserts the admin GET reveals only `configured` +
// source for a secret key, never the token bytes (sealed or plaintext), and that
// the non-secret Slack keys are present in the value map.
func TestGetSettingsSecretShape(t *testing.T) {
	box := slackTestBox(t)
	const plain = "xoxb-SECRET-abc123"
	sealed, err := settings.SealSecret(box, plain)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	cache := settings.New(&settingsStore{rows: []store.AppSetting{
		{Key: settings.KeySlackBotToken, Value: sealed},
	}}, time.Minute)
	cache.ConfigureSecrets(box, nil)
	h := &Handler{settings: cache}

	rec := httptest.NewRecorder()
	h.GetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, plain) || strings.Contains(body, sealed) {
		t.Fatal("admin GET leaked a secret value (plaintext or sealed)")
	}

	var resp struct {
		Settings map[string]string `json:"settings"`
		Secrets  map[string]bool   `json:"secrets"`
		Sources  map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := resp.Settings[settings.KeySlackBotToken]; present {
		t.Error("secret key present in the settings value map")
	}
	if !resp.Secrets[settings.KeySlackBotToken] {
		t.Error("secrets[slack_bot_token] = false, want true (a value is stored)")
	}
	if resp.Sources[settings.KeySlackBotToken] != "db" {
		t.Errorf("sources[slack_bot_token] = %q, want db", resp.Sources[settings.KeySlackBotToken])
	}
	// The non-secret Slack keys appear as ordinary values with their defaults.
	if resp.Settings[settings.KeySlackEnabled] != settings.DefaultSlackEnabled {
		t.Errorf("slack_enabled = %q, want default %q", resp.Settings[settings.KeySlackEnabled], settings.DefaultSlackEnabled)
	}
	if resp.Settings[settings.KeyPublicBaseURL] != settings.DefaultPublicBaseURL {
		t.Errorf("public_base_url = %q, want default", resp.Settings[settings.KeyPublicBaseURL])
	}
}

// TestUpdateSettingsRejectsEnvSourcedKey covers the PUT-409: an env-fixed key
// cannot be written from the webui (the greying reflects enforced policy).
func TestUpdateSettingsRejectsEnvSourcedKey(t *testing.T) {
	admin := adminUser()
	cache := settings.New(&settingsStore{}, time.Minute)
	cache.ConfigureSecrets(slackTestBox(t), map[string]string{
		settings.KeyPublicBaseURL: "https://env.example",
	})
	h := &Handler{settings: cache}

	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"public_base_url":"https://other.example"}}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestUpdateSettingsLiveValidationScrubsToken proves the live-validation branch
// runs, is fed the submitted plaintext, and that a validation error NEVER echoes
// the token — even a poorly-behaved error that embeds it.
func TestUpdateSettingsLiveValidationScrubsToken(t *testing.T) {
	admin := adminUser()
	const token = "xoxb-realtoken-SECRETVALUE"
	fake := &fakeSlackValidator{botErr: errors.New("boom " + token + " boom")}
	cache := settings.New(&settingsStore{}, time.Minute)
	cache.ConfigureSecrets(slackTestBox(t), nil)
	h := &Handler{settings: cache, slackValidator: fake}

	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"slack_bot_token":"`+token+`"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if fake.gotBot != token {
		t.Errorf("validator received %q, want the submitted plaintext %q", fake.gotBot, token)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("validation error echoed the submitted token: %s", rec.Body.String())
	}
}

// TestUpdateSettingsRejectsMalformedToken covers the format gate (before any
// network call): a bot token without the xoxb- prefix is rejected and the value
// is not echoed.
func TestUpdateSettingsRejectsMalformedToken(t *testing.T) {
	admin := adminUser()
	const token = "totally-not-a-slack-token-SECRET"
	fake := &fakeSlackValidator{}
	cache := settings.New(&settingsStore{}, time.Minute)
	cache.ConfigureSecrets(slackTestBox(t), nil)
	h := &Handler{settings: cache, slackValidator: fake}

	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"slack_bot_token":"`+token+`"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// A format rejection happens before the live check — the validator is untouched.
	if fake.gotBot != "" {
		t.Error("malformed token reached live validation; the format gate should have rejected it first")
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("format error echoed the token: %s", rec.Body.String())
	}
}

// TestUpdateSettingsRejectsBadSlackEnabled covers the slack_enabled strict-bool
// branch routed through the settings registry.
func TestUpdateSettingsRejectsBadSlackEnabled(t *testing.T) {
	admin := adminUser()
	h := &Handler{settings: settings.New(&settingsStore{}, time.Minute)}
	rec := httptest.NewRecorder()
	h.UpdateSettings(rec, putSettings(&admin, `{"settings":{"slack_enabled":"maybe"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
