package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestValidateAnthropicToken(t *testing.T) {
	good := []string{
		"sk-ant-oat01-abcdefghijklmnop",
		"  padded-but-trims-clean  ",
		strings.Repeat("a", maxTokenBytes),
	}
	for _, in := range good {
		if _, err := validateAnthropicToken(in); err != nil {
			t.Errorf("validateAnthropicToken(%q) = %v, want ok", in, err)
		}
	}

	bad := []string{
		"",
		"   ",
		"has interior space",
		"has\ttab",
		"has\nnewline",
		"has\x00null",
		strings.Repeat("a", maxTokenBytes+1),
	}
	for _, in := range bad {
		if _, err := validateAnthropicToken(in); err == nil {
			t.Errorf("validateAnthropicToken(%q) = nil error, want rejection", in)
		}
	}
}

func TestValidateTrimsSurroundingWhitespace(t *testing.T) {
	got, err := validateAnthropicToken("\n  token-value  \t")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "token-value" {
		t.Fatalf("got %q, want trimmed %q", got, "token-value")
	}
}

// TestAnthropicTokenNeverLeaks is the redaction test: the plaintext token must
// never appear in an error string, a serialized response, the sealed
// ciphertext, or any log line.
func TestAnthropicTokenNeverLeaks(t *testing.T) {
	const fixture = "sk-ant-oat01-REDACTIONTEST-abcdef1234567890"

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	// 1. A validation error on a token-bearing input must not echo it.
	if _, err := validateAnthropicToken(fixture + " x"); err == nil {
		t.Fatal("expected validation error for interior whitespace")
	} else if strings.Contains(err.Error(), fixture) {
		t.Fatal("validation error leaked the token")
	}

	// 2. Sealed ciphertext must not contain the plaintext.
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	sealed, err := box.Seal([]byte(fixture))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte(fixture)) {
		t.Fatal("sealed ciphertext contains the plaintext token")
	}

	// 3. The metadata DTO carries no secret value.
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	b, _ := json.Marshal(secretMeta(anthropicTokenKind, now, now))
	if strings.Contains(string(b), fixture) {
		t.Fatal("secret metadata response leaked the token")
	}

	// 4. Nothing above logged the plaintext.
	if strings.Contains(logs.String(), fixture) {
		t.Fatal("a log line leaked the token")
	}
}
