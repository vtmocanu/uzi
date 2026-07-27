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
	b, _ := json.Marshal(secretMeta(uuid.New(), store.KindAnthropicToken, "default", true, true, now, now))
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

// Scan fills whichever of the RETURNING columns the caller asked for, by type
// rather than by a fixed arity: UpsertDefaultUserSecret returns
// (id, kind, label, is_default, created_at, updated_at) since PRD #104 M1, and
// pinning the shape here would make this leak test fail for a reason that has
// nothing to do with leaking.
func (fakeUpsertRow) Scan(dest ...any) error {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for _, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = store.KindAnthropicToken
		case *pgtype.Timestamptz:
			*p = now
		case *uuid.UUID:
			*p = uuid.New()
		case *bool:
			*p = true
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
	if !strings.Contains(respBody, store.KindAnthropicToken) {
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

// TestValidateSecretLabelRejectsInvisibles pins the PRD #111 M2 addition to
// validateSecretLabel: Unicode FORMAT characters (category Cf) are refused
// alongside the control characters PRD #104 already refused.
//
// Why a label is different from other free text, and why this is not merely
// terminal hygiene: since PRD #111 the label IS the string a user reads to answer
// "which account did this run bill?". Two properties follow, and both are asserted
// rather than described:
//
//   - A bidi override makes a label READ as a different account than it names.
//   - A zero-width makes two DISTINCT tokens render identically, while 00077's
//     unique index on lower(label) does not fold them — so the collision the index
//     exists to prevent stays reachable while looking prevented.
func TestValidateSecretLabelRejectsInvisibles(t *testing.T) {
	// The six the auditor measured storing intact against the previous validator.
	// Named individually so a failure says WHICH class regressed.
	for _, tc := range []struct {
		name  string
		label string
	}{
		{"U+202E right-to-left override", "safe\u202edrowssap"},
		{"U+2066 left-to-right isolate", "safe\u2066key"},
		{"U+200B zero-width space", "work\u200b"},
		{"U+200D zero-width joiner", "work\u200dkey"},
		{"U+FEFF byte-order mark", "\ufeffwork"},
		{"U+00AD soft hyphen", "con\u00adsole"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateSecretLabel(tc.label); err == nil {
				t.Fatalf("label %q was accepted; an invisible codepoint lets a label lie about which account it names", tc.label)
			}
		})
	}

	// The confusability property, stated as the pair rather than as one string: the
	// two are DIFFERENT rows that a browser draws identically. Only the second can
	// still be created, which is what closes the collision.
	if _, err := validateSecretLabel("work"); err != nil {
		t.Fatalf("the plain label was rejected: %v", err)
	}
	if _, err := validateSecretLabel("work\u200b"); err == nil {
		t.Fatal("`work` and `work`+U+200B are distinct rows that render identically; the second must be refused")
	}

	// Controls stay refused (PRD #104's half, unchanged).
	for _, label := range []string{"bel\a", "esc\x1b[31m", "del\x7f", "bad�"} {
		if _, err := validateSecretLabel(label); err == nil {
			t.Errorf("control-bearing label %q was accepted", label)
		}
	}

	// 🔴 THE ACCEPTED COST, PINNED SO IT IS A DECISION AND NOT A SURPRISE.
	// U+200D is itself a format character, so multi-part emoji built from ZWJ
	// sequences are NOT storable. That is deliberate: the identical-rendering hazard
	// applies to a zero-width joiner exactly as it does to a bidi override, and a
	// reject-list of only the bidi controls would leave the collision live while
	// looking like it had solved something. A future reader who wants ZWJ emoji back
	// is changing this decision, not fixing an oversight.
	if _, err := validateSecretLabel("family \U0001F468\u200d\U0001F469\u200d\U0001F467 key"); err == nil {
		t.Fatal("a ZWJ emoji sequence was accepted; the reject-all-Cf decision means it must not be")
	}
	// The cost is bounded to JOINED sequences: a single-codepoint emoji is fine, so
	// "no emoji in labels" would be the wrong summary of this rule.
	if _, err := validateSecretLabel("🔑 console"); err != nil {
		t.Fatalf("a single-codepoint emoji label was rejected: %v — the rule is about JOINERS, not emoji", err)
	}

	// Ordinary labels are untouched: interior spaces, accents, and non-Latin scripts
	// all pass, and trimming still happens.
	for _, tc := range []struct{ in, want string }{
		{"  console key  ", "console key"},
		{"cheia mea", "cheia mea"},
		{"日本語のキー", "日本語のキー"},
	} {
		got, err := validateSecretLabel(tc.in)
		if err != nil {
			t.Errorf("ordinary label %q was rejected: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateSecretLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
