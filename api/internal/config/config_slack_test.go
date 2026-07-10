package config

import (
	"encoding/base64"
	"strings"
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

func TestLoadSeedSlack(t *testing.T) {
	slackBaseEnv(t)
	t.Setenv("UZI_SEED_SLACK_BOT_TOKEN", "xoxb-seed-bot")
	t.Setenv("UZI_SEED_SLACK_APP_TOKEN", "xapp-seed-app")
	t.Setenv("UZI_SEED_PUBLIC_BASE_URL", "https://uzi.example")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.SeedSlackBotToken != "xoxb-seed-bot" {
		t.Errorf("SeedSlackBotToken = %q", cfg.SeedSlackBotToken)
	}
	if cfg.SeedSlackAppToken != "xapp-seed-app" {
		t.Errorf("SeedSlackAppToken = %q", cfg.SeedSlackAppToken)
	}
	if cfg.SeedPublicBaseURL != "https://uzi.example" {
		t.Errorf("SeedPublicBaseURL = %q", cfg.SeedPublicBaseURL)
	}
}

func TestLoadSeedSlackRejectsMisconfiguration(t *testing.T) {
	// Each is a loud boot failure — a set-but-invalid seed must never be a
	// silent skip. Error text must name the variable but never carry the token.
	cases := map[string]map[string]string{
		"bot token without app token": {
			"UZI_SEED_SLACK_BOT_TOKEN": "xoxb-alone",
		},
		"app token without bot token": {
			"UZI_SEED_SLACK_APP_TOKEN": "xapp-alone",
		},
		"wrong bot prefix": {
			"UZI_SEED_SLACK_BOT_TOKEN": "xapp-swapped",
			"UZI_SEED_SLACK_APP_TOKEN": "xapp-seed-app",
		},
		"wrong app prefix": {
			"UZI_SEED_SLACK_BOT_TOKEN": "xoxb-seed-bot",
			"UZI_SEED_SLACK_APP_TOKEN": "xoxb-swapped",
		},
		"seed conflicts with token overlay": {
			"UZI_SEED_SLACK_BOT_TOKEN": "xoxb-seed-bot",
			"UZI_SEED_SLACK_APP_TOKEN": "xapp-seed-app",
			"SLACK_BOT_TOKEN":          "xoxb-env-bot",
			"SLACK_APP_TOKEN":          "xapp-env-app",
		},
		"seed conflicts with base URL overlay": {
			"UZI_SEED_PUBLIC_BASE_URL": "https://uzi.example",
			"UZI_PUBLIC_BASE_URL":      "https://uzi.example",
		},
		"bad seed base URL": {
			"UZI_SEED_PUBLIC_BASE_URL": "ftp://uzi.example",
		},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			slackBaseEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, loadErr := Load(); loadErr == nil {
				t.Errorf("Load() = nil, want boot failure")
			} else if strings.Contains(loadErr.Error(), "seed-bot") || strings.Contains(loadErr.Error(), "seed-app") {
				t.Errorf("error leaks token bytes: %v", loadErr)
			}
		})
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
