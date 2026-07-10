package config

import (
	"encoding/base64"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
)

// slackBaseEnv sets the minimum valid environment for a full Load(): DB URL, a
// non-placeholder signing key, and a varied-byte secret key.
func slackBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
}

func TestLoadSlackEnvOverlay(t *testing.T) {
	slackBaseEnv(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-env-bot")
	t.Setenv("SLACK_APP_TOKEN", "xapp-env-app")
	t.Setenv("UZI_PUBLIC_BASE_URL", "https://uzi.example")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.SlackBotToken != "xoxb-env-bot" {
		t.Errorf("SlackBotToken = %q, want xoxb-env-bot", cfg.SlackBotToken)
	}
	if cfg.SlackAppToken != "xapp-env-app" {
		t.Errorf("SlackAppToken = %q, want xapp-env-app", cfg.SlackAppToken)
	}
	if cfg.PublicBaseURL != "https://uzi.example" {
		t.Errorf("PublicBaseURL = %q, want https://uzi.example", cfg.PublicBaseURL)
	}
}

func TestLoadSlackDefaultsEmpty(t *testing.T) {
	slackBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	// Unset: the whole Slack integration is a no-op until configured.
	if cfg.SlackBotToken != "" || cfg.SlackAppToken != "" || cfg.PublicBaseURL != "" {
		t.Errorf("unset Slack env should leave empty fields; got %q/%q/%q",
			cfg.SlackBotToken, cfg.SlackAppToken, cfg.PublicBaseURL)
	}
	// The outbound Slack HTTP bound has a safe non-zero default so live validation
	// and the socket handshake are never unbounded.
	if cfg.SlackHTTPTimeout != 15*time.Second {
		t.Errorf("SlackHTTPTimeout default = %v, want 15s", cfg.SlackHTTPTimeout)
	}
}

func TestLoadPublicBaseURLValidation(t *testing.T) {
	// A non-http(s) or malformed UZI_PUBLIC_BASE_URL is a loud boot failure — it
	// becomes a button URL in every DM.
	for name, bad := range map[string]string{
		"ftp scheme": "ftp://uzi.example",
		"no scheme":  "uzi.example",
		"no host":    "https://",
	} {
		t.Run(name, func(t *testing.T) {
			slackBaseEnv(t)
			t.Setenv("UZI_PUBLIC_BASE_URL", bad)
			if _, err := Load(); err == nil {
				t.Errorf("Load() = nil for %s UZI_PUBLIC_BASE_URL=%q, want boot failure", name, bad)
			}
		})
	}
	// A valid http base URL loads.
	t.Run("valid http", func(t *testing.T) {
		slackBaseEnv(t)
		t.Setenv("UZI_PUBLIC_BASE_URL", "http://127.0.0.1:9999")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() rejected a valid URL: %v", err)
		}
		if cfg.PublicBaseURL != "http://127.0.0.1:9999" {
			t.Errorf("PublicBaseURL = %q", cfg.PublicBaseURL)
		}
	})
}
