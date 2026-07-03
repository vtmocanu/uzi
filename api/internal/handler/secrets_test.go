package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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

// fakeUpsertDB is a store.DBTX that records the arguments passed to QueryRow and
// returns a canned metadata row, so PutAnthropicToken can be driven end to end
// without a real database. It exists to catch a future edit that logs or echoes
// the plaintext token anywhere in the handler's request path.
type fakeUpsertDB struct {
	lastArgs []any
}

func (f *fakeUpsertDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeUpsertDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeUpsertDB: Query not used")
}

func (f *fakeUpsertDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.lastArgs = args
	return fakeUpsertRow{}
}

type fakeUpsertRow struct{}

func (fakeUpsertRow) Scan(dest ...any) error {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if len(dest) == 3 {
		if p, ok := dest[0].(*string); ok {
			*p = anthropicTokenKind
		}
		if p, ok := dest[1].(*pgtype.Timestamptz); ok {
			*p = now
		}
		if p, ok := dest[2].(*pgtype.Timestamptz); ok {
			*p = now
		}
	}
	return nil
}

// TestPutAnthropicTokenNeverLeaksEndToEnd drives the real handler through its
// full request path (decode -> validate -> seal -> store -> respond) and asserts
// the plaintext token appears in no log line, no response byte, and not in the
// arguments handed to the store (which must be sealed ciphertext). This is the
// regression guard the isolated validate/DTO test cannot give: a future debug
// log of req.Token inside the handler must fail here.
func TestPutAnthropicTokenNeverLeaksEndToEnd(t *testing.T) {
	const fixture = "sk-ant-oat01-HANDLERLEAKTEST-abcdef1234567890"

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	db := &fakeUpsertDB{}
	h := &Handler{q: store.New(db), box: box}

	body, _ := json.Marshal(map[string]string{"token": fixture})
	req := httptest.NewRequest(http.MethodPut, "/api/me/secrets/anthropic_token", bytes.NewReader(body))
	req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
	rec := httptest.NewRecorder()

	h.PutAnthropicToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if strings.Contains(respBody, fixture) {
		t.Fatal("response body leaked the token")
	}
	if !strings.Contains(respBody, anthropicTokenKind) {
		t.Fatalf("response should carry the kind metadata, got: %s", respBody)
	}

	// The value handed to the store layer must be sealed ciphertext, never the
	// plaintext (nor a plaintext-bearing string arg).
	for _, a := range db.lastArgs {
		if b, ok := a.([]byte); ok && bytes.Contains(b, []byte(fixture)) {
			t.Fatal("ciphertext arg to the store contains the plaintext token")
		}
		if s, ok := a.(string); ok && strings.Contains(s, fixture) {
			t.Fatal("a string arg to the store contains the plaintext token")
		}
	}

	if strings.Contains(logs.String(), fixture) {
		t.Fatal("a handler log line leaked the token")
	}
}
