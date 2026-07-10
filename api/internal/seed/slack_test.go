package seed

import (
	"context"
	"errors"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSettingsStore records the Slack-settings seed's reads/writes, standing in
// for *store.Queries.
type fakeSettingsStore struct {
	rows     []store.AppSetting
	listErr  error
	upserted map[string]string // key → stored value
}

func (s *fakeSettingsStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return s.rows, s.listErr
}
func (s *fakeSettingsStore) UpsertAppSetting(_ context.Context, arg store.UpsertAppSettingParams) (store.AppSetting, error) {
	if s.upserted == nil {
		s.upserted = map[string]string{}
	}
	s.upserted[arg.Key] = arg.Value
	return store.AppSetting{Key: arg.Key, Value: arg.Value}, nil
}

// testBox builds a real secretbox so the sealed values can be round-tripped
// through settings.DecodeSecret — proving the seed stores exactly what the
// settings PUT would (ValueForStorage), not plaintext.
func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	return box
}

func slackCfg() config.Config {
	return config.Config{
		SeedSlackBotToken: "xoxb-seed-bot",
		SeedSlackAppToken: "xapp-seed-app",
		SeedPublicBaseURL: "https://uzi.example",
	}
}

func TestSlackSettingsSeedsWhenAbsent(t *testing.T) {
	st := &fakeSettingsStore{}
	box := testBox(t)

	if err := SlackSettings(context.Background(), st, box, slackCfg()); err != nil {
		t.Fatalf("SlackSettings: %v", err)
	}

	// Secret keys are sealed, never stored in the clear.
	for key, want := range map[string]string{
		settings.KeySlackBotToken: "xoxb-seed-bot",
		settings.KeySlackAppToken: "xapp-seed-app",
	} {
		stored, ok := st.upserted[key]
		if !ok {
			t.Fatalf("%s not seeded", key)
		}
		if stored == want {
			t.Fatalf("%s stored in the clear", key)
		}
		plain, err := settings.DecodeSecret(box, stored)
		if err != nil {
			t.Fatalf("%s does not decode: %v", key, err)
		}
		if plain != want {
			t.Fatalf("%s round-trip = %q, want %q", key, plain, want)
		}
	}

	if got := st.upserted[settings.KeySlackEnabled]; got != "true" {
		t.Errorf("slack_enabled = %q, want true", got)
	}
	if got := st.upserted[settings.KeyPublicBaseURL]; got != "https://uzi.example" {
		t.Errorf("public_base_url = %q, want https://uzi.example", got)
	}
}

func TestSlackSettingsCreateOnlyPerKey(t *testing.T) {
	// slack_bot_token and slack_enabled rows already exist (UI-set): they must be
	// left untouched, while the still-absent keys are filled in.
	st := &fakeSettingsStore{rows: []store.AppSetting{
		{Key: settings.KeySlackBotToken, Value: "ui-sealed-token"},
		{Key: settings.KeySlackEnabled, Value: "false"},
	}}

	if err := SlackSettings(context.Background(), st, testBox(t), slackCfg()); err != nil {
		t.Fatalf("SlackSettings: %v", err)
	}

	if _, touched := st.upserted[settings.KeySlackBotToken]; touched {
		t.Error("existing slack_bot_token row was overwritten")
	}
	if _, touched := st.upserted[settings.KeySlackEnabled]; touched {
		t.Error("existing slack_enabled row was overwritten (an admin's off flip must survive)")
	}
	if _, ok := st.upserted[settings.KeySlackAppToken]; !ok {
		t.Error("absent slack_app_token was not seeded")
	}
	if _, ok := st.upserted[settings.KeyPublicBaseURL]; !ok {
		t.Error("absent public_base_url was not seeded")
	}
}

func TestSlackSettingsUnconfiguredIsNoOp(t *testing.T) {
	st := &fakeSettingsStore{listErr: errors.New("must not be called")}
	if err := SlackSettings(context.Background(), st, testBox(t), config.Config{}); err != nil {
		t.Fatalf("SlackSettings: %v", err)
	}
	if st.upserted != nil {
		t.Errorf("unconfigured seed wrote settings: %v", st.upserted)
	}
}

func TestSlackSettingsBaseURLOnly(t *testing.T) {
	// UZI_SEED_PUBLIC_BASE_URL without the token pair seeds just that row.
	st := &fakeSettingsStore{}
	cfg := config.Config{SeedPublicBaseURL: "http://uzi.lan:8080"}

	if err := SlackSettings(context.Background(), st, testBox(t), cfg); err != nil {
		t.Fatalf("SlackSettings: %v", err)
	}
	if got := st.upserted[settings.KeyPublicBaseURL]; got != "http://uzi.lan:8080" {
		t.Errorf("public_base_url = %q", got)
	}
	for _, key := range []string{settings.KeySlackBotToken, settings.KeySlackAppToken, settings.KeySlackEnabled} {
		if _, ok := st.upserted[key]; ok {
			t.Errorf("%s seeded without a token pair configured", key)
		}
	}
}

func TestSlackSettingsListErrorIsFatal(t *testing.T) {
	st := &fakeSettingsStore{listErr: errors.New("db down")}
	if err := SlackSettings(context.Background(), st, testBox(t), slackCfg()); err == nil {
		t.Fatal("expected a DB list error to abort boot")
	}
}
