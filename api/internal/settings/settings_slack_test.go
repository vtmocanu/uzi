package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// testBox builds a Box on a varied-byte key (an all-identical key trips the
// low-entropy guard), for the seal/decrypt-accessor tests.
func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return box
}

func TestSlackEnabledAccessor(t *testing.T) {
	// Empty table → default OFF (unlike PrdlessEnabled, which defaults on).
	c := New(&fakeStore{}, time.Minute)
	if got, err := c.SlackEnabled(context.Background()); err != nil || got != false {
		t.Fatalf("SlackEnabled default = %v, %v; want false", got, err)
	}
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"", false},       // empty → default false
		{"banana", false}, // junk → default OFF, never silently on
		{"TRUE", false},   // non-canonical → default, not a lenient parse
		{"1", false},
	} {
		c := New(&fakeStore{rows: []store.AppSetting{row(KeySlackEnabled, tc.stored)}}, time.Minute)
		if got, _ := c.SlackEnabled(context.Background()); got != tc.want {
			t.Errorf("SlackEnabled(stored=%q) = %v, want %v", tc.stored, got, tc.want)
		}
	}
}

func TestPublicBaseURLAccessorAndEnvOverride(t *testing.T) {
	// Empty table → loopback default.
	c := New(&fakeStore{}, time.Minute)
	if got, _ := c.PublicBaseURL(context.Background()); got != DefaultPublicBaseURL {
		t.Fatalf("PublicBaseURL default = %q, want %q", got, DefaultPublicBaseURL)
	}
	// A stored row wins over the default.
	c = New(&fakeStore{rows: []store.AppSetting{row(KeyPublicBaseURL, "https://uzi.example")}}, time.Minute)
	if got, _ := c.PublicBaseURL(context.Background()); got != "https://uzi.example" {
		t.Fatalf("PublicBaseURL stored = %q, want https://uzi.example", got)
	}
	// ENV wins over the stored row.
	c.ConfigureSecrets(nil, map[string]string{KeyPublicBaseURL: "https://env.example"})
	if got, _ := c.PublicBaseURL(context.Background()); got != "https://env.example" {
		t.Fatalf("PublicBaseURL env = %q, want https://env.example (env over db)", got)
	}
}

// TestSecretKeysStructurallyExcluded is the byte-level guarantee: a configured
// secret's value — sealed OR plaintext — never appears in All() or the admin
// view's Values, only a `configured` flag and a source.
func TestSecretKeysStructurallyExcluded(t *testing.T) {
	box := testBox(t)
	const plain = "xoxb-SUPER-SECRET-9f8e7d"
	sealed, err := SealSecret(box, plain)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	fs := &fakeStore{rows: []store.AppSetting{row(KeySlackBotToken, sealed)}}
	c := New(fs, time.Minute)
	c.ConfigureSecrets(box, nil)

	// All() never carries the secret key at all.
	all, err := c.All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if _, present := all[KeySlackBotToken]; present {
		t.Fatal("All() leaked a secret key into the value map")
	}

	view, err := c.AdminView(context.Background())
	if err != nil {
		t.Fatalf("AdminView: %v", err)
	}
	if _, present := view.Values[KeySlackBotToken]; present {
		t.Fatal("AdminView.Values leaked a secret key")
	}
	if !view.Secrets[KeySlackBotToken] {
		t.Error("configured secret should report Secrets[slack_bot_token]=true")
	}
	if view.Sources[KeySlackBotToken] != "db" {
		t.Errorf("Sources[slack_bot_token] = %q, want db", view.Sources[KeySlackBotToken])
	}
	// Byte-level: neither the plaintext nor the sealed bytes appear anywhere in the
	// serialized value map or the whole admin view.
	valuesJSON, _ := json.Marshal(view.Values)
	allJSON, _ := json.Marshal(all)
	viewJSON, _ := json.Marshal(view)
	for _, blob := range []string{string(valuesJSON), string(allJSON), string(viewJSON)} {
		if strings.Contains(blob, plain) {
			t.Error("plaintext secret appeared in a serialized settings read")
		}
		if strings.Contains(blob, sealed) {
			t.Error("sealed secret ciphertext appeared in a serialized settings read")
		}
	}
}

func TestSecretDecryptAccessor(t *testing.T) {
	box := testBox(t)
	const plain = "xoxb-decrypt-me-123"
	sealed, err := SealSecret(box, plain)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	c := New(&fakeStore{rows: []store.AppSetting{row(KeySlackBotToken, sealed)}}, time.Minute)
	c.ConfigureSecrets(box, nil)
	got, err := c.SlackBotToken(context.Background())
	if err != nil || got != plain {
		t.Fatalf("SlackBotToken = %q, %v; want %q", got, err, plain)
	}

	// Unset secret → empty, no error.
	empty := New(&fakeStore{}, time.Minute)
	empty.ConfigureSecrets(box, nil)
	if got, err := empty.SlackAppToken(context.Background()); err != nil || got != "" {
		t.Fatalf("unset SlackAppToken = %q, %v; want empty", got, err)
	}

	// ENV-sourced secret is returned verbatim and needs no box.
	envOnly := New(&fakeStore{}, time.Minute)
	envOnly.ConfigureSecrets(nil, map[string]string{KeySlackAppToken: "xapp-from-env"})
	if got, err := envOnly.SlackAppToken(context.Background()); err != nil || got != "xapp-from-env" {
		t.Fatalf("env SlackAppToken = %q, %v; want xapp-from-env", got, err)
	}

	// A DB-stored secret with no box configured is an error, not a silent empty.
	noBox := New(&fakeStore{rows: []store.AppSetting{row(KeySlackBotToken, sealed)}}, time.Minute)
	if _, err := noBox.SlackBotToken(context.Background()); err == nil {
		t.Error("decrypt with no box should error, not return empty")
	}
}

func TestSourceReporting(t *testing.T) {
	box := testBox(t)
	sealed, _ := SealSecret(box, "xoxb-x")
	fs := &fakeStore{rows: []store.AppSetting{
		row(KeyPRDLabel, "Feature"),   // db
		row(KeySlackBotToken, sealed), // db (secret)
	}}
	c := New(fs, time.Minute)
	c.ConfigureSecrets(box, map[string]string{KeyPublicBaseURL: "https://env.example"})

	view, err := c.AdminView(context.Background())
	if err != nil {
		t.Fatalf("AdminView: %v", err)
	}
	want := map[string]string{
		KeyPRDLabel:       "db",      // a stored non-secret
		KeyAutopilotLabel: "default", // never set
		KeyPublicBaseURL:  "env",     // ENV overlay
		KeySlackBotToken:  "db",      // stored secret
		KeySlackAppToken:  "default", // unset secret
	}
	for k, w := range want {
		if got := view.Sources[k]; got != w {
			t.Errorf("Sources[%s] = %q, want %q", k, got, w)
		}
	}
	// An env-sourced key is flagged for the PUT-409 path.
	if !c.IsEnvSourced(KeyPublicBaseURL) {
		t.Error("IsEnvSourced(public_base_url) = false, want true")
	}
	if c.IsEnvSourced(KeyPRDLabel) {
		t.Error("IsEnvSourced(prd_label) = true, want false")
	}
}

func TestKnownAndIsSecret(t *testing.T) {
	for _, k := range []string{KeySlackBotToken, KeySlackAppToken} {
		if !IsSecret(k) {
			t.Errorf("IsSecret(%s) = false, want true", k)
		}
		if !Known(k) {
			t.Errorf("Known(%s) = false, want true (secret keys are writable)", k)
		}
	}
	for _, k := range []string{KeySlackEnabled, KeyPublicBaseURL, KeyPRDLabel} {
		if IsSecret(k) {
			t.Errorf("IsSecret(%s) = true, want false", k)
		}
		if !Known(k) {
			t.Errorf("Known(%s) = false, want true", k)
		}
	}
	if Known("nope") || IsSecret("nope") {
		t.Error("an unknown key must be neither Known nor IsSecret")
	}
}

func TestValidateSlackBranches(t *testing.T) {
	// slack_enabled routes to the strict bool.
	for _, ok := range []string{"true", "false"} {
		if err := Validate(KeySlackEnabled, ok); err != nil {
			t.Errorf("Validate(slack_enabled, %q) = %v, want nil", ok, err)
		}
	}
	if err := Validate(KeySlackEnabled, "banana"); err == nil {
		t.Error("Validate(slack_enabled, banana) = nil, want a bool rejection")
	}

	// public_base_url routes to the URL rule.
	if err := Validate(KeyPublicBaseURL, "https://ok.example"); err != nil {
		t.Errorf("Validate(public_base_url, https) = %v, want nil", err)
	}
	if err := Validate(KeyPublicBaseURL, "ftp://nope"); err == nil {
		t.Error("Validate(public_base_url, ftp) = nil, want a scheme rejection")
	}

	// Tokens route to the prefix check, and the error NEVER echoes the value.
	const badBot = "not-a-token-but-secret-value-abc"
	err := Validate(KeySlackBotToken, badBot)
	if err == nil {
		t.Fatal("Validate(slack_bot_token, wrong-prefix) = nil, want a rejection")
	}
	if strings.Contains(err.Error(), badBot) {
		t.Errorf("token validation error echoed the submitted value: %q", err)
	}
	if err := Validate(KeySlackBotToken, "xoxb-good"); err != nil {
		t.Errorf("Validate(slack_bot_token, xoxb-…) = %v, want nil", err)
	}
	if err := Validate(KeySlackAppToken, "xoxb-wrong-prefix"); err == nil {
		t.Error("Validate(slack_app_token, xoxb-…) = nil; an app token must start with xapp-")
	}
	if err := Validate(KeySlackAppToken, "xapp-good"); err != nil {
		t.Errorf("Validate(slack_app_token, xapp-…) = %v, want nil", err)
	}
}

func TestValidatePublicBaseURL(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:8080", "https://uzi.example", "http://host"} {
		if err := ValidatePublicBaseURL(ok); err != nil {
			t.Errorf("ValidatePublicBaseURL(%q) = %v, want nil", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"empty":     "",
		"no scheme": "uzi.example",
		"ftp":       "ftp://uzi.example",
		"no host":   "http://",
	} {
		if err := ValidatePublicBaseURL(bad); err == nil {
			t.Errorf("%s: ValidatePublicBaseURL(%q) = nil, want rejection", name, bad)
		}
	}
}

func TestSealDecodeRoundtrip(t *testing.T) {
	box := testBox(t)
	const plain = "xapp-notarealapptoken"
	enc, err := SealSecret(box, plain)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	if strings.Contains(enc, plain) {
		t.Fatal("sealed encoding contains the plaintext")
	}
	got, err := DecodeSecret(box, enc)
	if err != nil || got != plain {
		t.Fatalf("DecodeSecret = %q, %v; want %q", got, err, plain)
	}
	// A different key cannot open it (GCM auth failure), and the error carries no
	// plaintext.
	other := New(nil, time.Minute) // unused; just need another box
	_ = other
	badKey := make([]byte, secretbox.KeySize)
	for i := range badKey {
		badKey[i] = byte(i + 99)
	}
	otherBox, _ := secretbox.New(badKey)
	if _, err := DecodeSecret(otherBox, enc); err == nil {
		t.Error("DecodeSecret with the wrong key should fail")
	}
}

// TestValueForStorage is the write-side seam the settings PUT uses: a secret key
// is sealed (the stored bytes are NOT the token, and decode round-trips), while a
// non-secret key is stored verbatim.
func TestValueForStorage(t *testing.T) {
	box := testBox(t)

	// Secret: stored form is not the plaintext, and decodes back to it.
	const token = "xoxb-store-me-please"
	stored, err := ValueForStorage(box, KeySlackBotToken, token)
	if err != nil {
		t.Fatalf("ValueForStorage(secret): %v", err)
	}
	if stored == token || strings.Contains(stored, token) {
		t.Fatal("secret stored in the clear")
	}
	if got, err := DecodeSecret(box, stored); err != nil || got != token {
		t.Fatalf("round-trip = %q, %v; want %q", got, err, token)
	}

	// Non-secret: stored verbatim (no sealing).
	if stored, err := ValueForStorage(box, KeyPRDLabel, "Feature"); err != nil || stored != "Feature" {
		t.Fatalf("ValueForStorage(non-secret) = %q, %v; want verbatim", stored, err)
	}
}

// TestLabelChangedIgnoresNonBoardKeys pins auditor pre-flag #5: only the two
// board-filtering labels force a resync; the Slack keys (and every other
// non-label key) never do.
func TestLabelChangedIgnoresNonBoardKeys(t *testing.T) {
	committed := map[string]string{
		KeyPRDLabel:       "PRD",
		KeyAutopilotLabel: "autopilot",
		KeySlackEnabled:   "false",
		KeyPublicBaseURL:  DefaultPublicBaseURL,
	}
	for _, updates := range []map[string]string{
		{KeySlackEnabled: "true"},
		{KeyPublicBaseURL: "https://uzi.example"},
		{KeySlackBotToken: "xoxb-whatever"},
		{KeySlackAppToken: "xapp-whatever"},
	} {
		if LabelChanged(committed, updates) {
			t.Errorf("LabelChanged(%v) = true; a Slack-key change must not force a resync", updates)
		}
	}
	// A real label change still triggers, alongside a Slack key.
	if !LabelChanged(committed, map[string]string{KeyPRDLabel: "Feature", KeySlackEnabled: "true"}) {
		t.Error("a prd_label change must still force a resync")
	}
}

// TestValidateMergedIgnoresSlackKeys confirms the pairwise-distinct label rule
// does not reach the Slack keys: a slack value equal to a label value is fine.
func TestValidateMergedIgnoresSlackKeys(t *testing.T) {
	if err := ValidateMerged(map[string]string{
		KeyPRDLabel:          "PRD",
		KeyAutopilotLabel:    "autopilot",
		KeyPrdlessLabel:      "PRDLESS",
		KeyRunEligibleLabels: "PRD",       // PRD #196: eligible set must contain the primary
		KeySlackEnabled:      "PRD",       // colliding value, but not a label key
		KeyPublicBaseURL:     "autopilot", // ditto
	}); err != nil {
		t.Errorf("ValidateMerged rejected on a non-label key collision: %v", err)
	}
}
