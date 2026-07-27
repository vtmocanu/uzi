package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// fakeRLDB is a store.DBTX answering the rate-limit read queries from fixed
// in-memory results, so the handlers run end to end without a database. Since #104
// M5 both /me and /admin return per-TOKEN rows (a Query, not a QueryRow), so the
// fixtures are per-token; it also records whether the D3b DeleteRateLimits ran.
type fakeRLDB struct {
	// selfRows drives GET /api/me/rate-limits (ListRateLimitsForUser).
	selfRows []store.ListRateLimitsForUserRow
	// listRows drives GET /api/admin/rate-limits (ListRateLimits).
	listRows []store.ListRateLimitsRow
}

func (f *fakeRLDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeRLDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

func (f *fakeRLDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	// The two list queries differ by their column count; dispatch on which one the
	// caller asked for so the right scanner runs.
	if strings.Contains(sql, "FROM user_secrets s") && strings.Contains(sql, "WHERE s.user_id") {
		scans := make([]func(...any) error, 0, len(f.selfRows))
		for _, r := range f.selfRows {
			scans = append(scans, scanSelfRateLimit(r))
		}
		return &fakeNotifRows{scans: scans}, nil
	}
	scans := make([]func(...any) error, 0, len(f.listRows))
	for _, r := range f.listRows {
		scans = append(scans, scanListRateLimit(r))
	}
	return &fakeNotifRows{scans: scans}, nil
}

func scanSelfRateLimit(r store.ListRateLimitsForUserRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = r.UserSecretID
		*dest[1].(*string) = r.Label
		*dest[2].(*bool) = r.IsDefault
		// PRD #111 M2: auto_eligible is projected right after is_default, so the
		// scan positions below all shift by one. Positional scanning is why this
		// fake has to move in step with the query at all — a mismatch is a runtime
		// interface-conversion panic, not a compile error.
		*dest[3].(*bool) = r.AutoEligible
		*dest[4].(*pgtype.Int2) = r.FiveHourPct
		*dest[5].(*pgtype.Timestamptz) = r.FiveHourResetsAt
		*dest[6].(*pgtype.Int2) = r.SevenDayPct
		*dest[7].(*pgtype.Timestamptz) = r.SevenDayResetsAt
		*dest[8].(*pgtype.Text) = r.Source
		*dest[9].(*pgtype.Timestamptz) = r.SyncedAt
		return nil
	}
}

func scanListRateLimit(r store.ListRateLimitsRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = r.UserID
		*dest[1].(*string) = r.Email
		*dest[2].(*pgtype.Text) = r.DisplayName
		*dest[3].(*pgtype.UUID) = r.UserSecretID
		*dest[4].(*pgtype.Text) = r.Label
		*dest[5].(*pgtype.Bool) = r.IsDefault
		*dest[6].(*pgtype.Bool) = r.AutoEligible // PRD #111 M2; shifts the rest by one
		*dest[7].(*pgtype.Int2) = r.FiveHourPct
		*dest[8].(*pgtype.Timestamptz) = r.FiveHourResetsAt
		*dest[9].(*pgtype.Int2) = r.SevenDayPct
		*dest[10].(*pgtype.Timestamptz) = r.SevenDayResetsAt
		*dest[11].(*pgtype.Text) = r.Source
		*dest[12].(*pgtype.Timestamptz) = r.SyncedAt
		return nil
	}
}

func pgInt2(v int16) pgtype.Int2          { return pgtype.Int2{Int16: v, Valid: true} }
func pgTs(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func pgTxt(s string) pgtype.Text          { return pgtype.Text{String: s, Valid: true} }
func pgUUIDv(u uuid.UUID) pgtype.UUID     { return pgtype.UUID{Bytes: u, Valid: true} }
func pgBool(b bool) pgtype.Bool           { return pgtype.Bool{Bool: b, Valid: true} }

// okSelfRow is a token with a reading, for /me.
func okSelfRow(secretID uuid.UUID, label string, isDefault bool, synced time.Time) store.ListRateLimitsForUserRow {
	return store.ListRateLimitsForUserRow{
		UserSecretID:     secretID,
		Label:            label,
		IsDefault:        isDefault,
		FiveHourPct:      pgInt2(55),
		FiveHourResetsAt: pgTs(time.Unix(1784000000, 0)),
		SevenDayPct:      pgInt2(10),
		SevenDayResetsAt: pgtype.Timestamptz{}, // no reset ⇒ null
		Source:           pgTxt("usage_endpoint"),
		SyncedAt:         pgTs(synced),
	}
}

func rlHandler(db store.DBTX, interval time.Duration) *Handler {
	return &Handler{q: store.New(db), cfg: config.Config{UsagePollInterval: interval}}
}

func rlReq(user uuid.UUID, admin bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me/rate-limits", nil)
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: user, IsAdmin: admin, IsActive: true}))
}

// selfTokens decodes {"tokens": [...]} from a /me response.
func selfTokens(t *testing.T, rec *httptest.ResponseRecorder) []struct {
	SecretID  string `json:"secret_id"`
	Label     string `json:"label"`
	IsDefault bool   `json:"is_default"`
	Limits    struct {
		Status   string           `json:"status"`
		Source   string           `json:"source"`
		Stale    *bool            `json:"stale"`
		SyncedAt string           `json:"synced_at"`
		FiveHour *json.RawMessage `json:"five_hour"`
	} `json:"limits"`
} {
	t.Helper()
	var body struct {
		Tokens []struct {
			SecretID  string `json:"secret_id"`
			Label     string `json:"label"`
			IsDefault bool   `json:"is_default"`
			Limits    struct {
				Status   string           `json:"status"`
				Source   string           `json:"source"`
				Stale    *bool            `json:"stale"`
				SyncedAt string           `json:"synced_at"`
				FiveHour *json.RawMessage `json:"five_hour"`
			} `json:"limits"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return body.Tokens
}

// --- /api/me/rate-limits ---

func TestSelfRateLimitsRequireAuth(t *testing.T) {
	h := &Handler{} // no user in context ⇒ 401 before any store access
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, httptest.NewRequest(http.MethodGet, "/api/me/rate-limits", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

// A token-less user gets an empty tokens array — the client's no_token signal.
func TestSelfRateLimitsNoToken(t *testing.T) {
	h := rlHandler(&fakeRLDB{selfRows: nil}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(uuid.New(), false))
	if got := selfTokens(t, rec); len(got) != 0 {
		t.Fatalf("token-less user returned %d tokens, want an empty array", len(got))
	}
}

// A token with no reading yet is listed as `unavailable`, not omitted.
func TestSelfRateLimitsUnavailable(t *testing.T) {
	secretID := uuid.New()
	rows := []store.ListRateLimitsForUserRow{{UserSecretID: secretID, Label: "default", IsDefault: true /* SyncedAt invalid */}}
	h := rlHandler(&fakeRLDB{selfRows: rows}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(uuid.New(), false))

	tokens := selfTokens(t, rec)
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens, want 1", len(tokens))
	}
	if tokens[0].Limits.Status != "unavailable" {
		t.Fatalf("status = %q, want unavailable", tokens[0].Limits.Status)
	}
	if tokens[0].Label != "default" || !tokens[0].IsDefault {
		t.Errorf("token metadata lost: %+v", tokens[0])
	}
	// unavailable carries no window/source/stale.
	if tokens[0].Limits.FiveHour != nil || tokens[0].Limits.Stale != nil {
		t.Errorf("unavailable leaked ok-only fields: %+v", tokens[0].Limits)
	}
}

// One reading per token, default first, each carrying its own label + reading.
func TestSelfRateLimitsMultipleTokens(t *testing.T) {
	def, console := uuid.New(), uuid.New()
	rows := []store.ListRateLimitsForUserRow{
		okSelfRow(def, "default", true, time.Now()),
		okSelfRow(console, "console-key", false, time.Now()),
	}
	h := rlHandler(&fakeRLDB{selfRows: rows}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(uuid.New(), false))

	tokens := selfTokens(t, rec)
	if len(tokens) != 2 {
		t.Fatalf("got %d tokens, want 2", len(tokens))
	}
	if tokens[0].Label != "default" || !tokens[0].IsDefault {
		t.Errorf("first token = %+v, want the default", tokens[0])
	}
	if tokens[1].Label != "console-key" || tokens[1].IsDefault {
		t.Errorf("second token = %+v, want console-key non-default", tokens[1])
	}
	for _, tk := range tokens {
		if tk.Limits.Status != "ok" || tk.Limits.Source != "usage_endpoint" {
			t.Errorf("token %s status/source = %s/%s", tk.Label, tk.Limits.Status, tk.Limits.Source)
		}
		if tk.SecretID == "" {
			t.Errorf("token %s missing secret_id", tk.Label)
		}
	}
}

func TestSelfRateLimitsStale(t *testing.T) {
	// synced 20m ago with a 5m interval (3× = 15m) ⇒ stale.
	rows := []store.ListRateLimitsForUserRow{okSelfRow(uuid.New(), "default", true, time.Now().Add(-20*time.Minute))}
	h := rlHandler(&fakeRLDB{selfRows: rows}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(uuid.New(), false))
	tokens := selfTokens(t, rec)
	if tokens[0].Limits.Stale == nil || !*tokens[0].Limits.Stale {
		t.Fatal("reading older than 3× interval should be stale")
	}
}

func TestSelfRateLimitsPollerDisabledAlwaysStale(t *testing.T) {
	rows := []store.ListRateLimitsForUserRow{okSelfRow(uuid.New(), "default", true, time.Now())}
	h := rlHandler(&fakeRLDB{selfRows: rows}, 0) // poller disabled
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(uuid.New(), false))
	tokens := selfTokens(t, rec)
	if tokens[0].Limits.Status != "ok" {
		t.Fatalf("status = %q, want ok (rows still served when poller off)", tokens[0].Limits.Status)
	}
	if tokens[0].Limits.Stale == nil || !*tokens[0].Limits.Stale {
		t.Fatal("with the poller disabled every reading is stale")
	}
}

// --- /api/admin/rate-limits ---

func TestAdminRateLimitsRequiresAdmin(t *testing.T) {
	h := rlHandler(&fakeRLDB{}, 5*time.Minute)
	gated := mw.RequireAdmin(http.HandlerFunc(h.AdminRateLimits))

	nonAdmin := httptest.NewRecorder()
	gated.ServeHTTP(nonAdmin, rlReq(uuid.New(), false))
	if nonAdmin.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", nonAdmin.Code)
	}

	admin := httptest.NewRecorder()
	gated.ServeHTTP(admin, rlReq(uuid.New(), true))
	if admin.Code != http.StatusOK {
		t.Fatalf("admin = %d, want 200", admin.Code)
	}
}

// TestAdminRateLimitsGroupsByUserThenToken: the fold collapses consecutive rows for
// one user into one entry with a tokens array; a token-less user is one row with a
// NULL secret id and an empty tokens array; a multi-token user gets several.
func TestAdminRateLimitsGroupsByUserThenToken(t *testing.T) {
	multiUser, noTokUser, unavailUser := uuid.New(), uuid.New(), uuid.New()
	tokA, tokB, tokC := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()
	rows := []store.ListRateLimitsRow{
		// multiUser holds two tokens, one with a reading and one without.
		{
			UserID: multiUser, Email: "a@x", DisplayName: pgTxt("Ana"),
			UserSecretID: pgUUIDv(tokA), Label: pgTxt("default"), IsDefault: pgBool(true),
			FiveHourPct: pgInt2(90), SevenDayPct: pgInt2(30), Source: pgTxt("header_probe"), SyncedAt: pgTs(now),
		},
		{
			UserID: multiUser, Email: "a@x", DisplayName: pgTxt("Ana"),
			UserSecretID: pgUUIDv(tokB), Label: pgTxt("console"), IsDefault: pgBool(false),
			// SyncedAt invalid ⇒ unavailable.
		},
		// A token-less user: one row, NULL secret id.
		{UserID: noTokUser, Email: "b@x"},
		// A user with a token but no reading yet.
		{
			UserID: unavailUser, Email: "c@x",
			UserSecretID: pgUUIDv(tokC), Label: pgTxt("default"), IsDefault: pgBool(true),
		},
	}
	h := rlHandler(&fakeRLDB{listRows: rows}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.AdminRateLimits(rec, rlReq(uuid.New(), true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}

	var body struct {
		Users []struct {
			Email       string `json:"email"`
			Name        string `json:"name"`
			VaultLocked bool   `json:"vault_locked"`
			Tokens      []struct {
				Label  string `json:"label"`
				Limits struct {
					Status string `json:"status"`
				} `json:"limits"`
			} `json:"tokens"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) != 3 {
		t.Fatalf("got %d users, want 3 (multi, no_token, unavailable)", len(body.Users))
	}
	byEmail := map[string]int{}
	for _, u := range body.Users {
		byEmail[u.Email] = len(u.Tokens)
		if u.VaultLocked {
			t.Errorf("%s vault_locked = true with no vault wired", u.Email)
		}
	}
	if byEmail["a@x"] != 2 {
		t.Fatalf("multi-token user has %d tokens, want 2", byEmail["a@x"])
	}
	if byEmail["b@x"] != 0 {
		t.Fatalf("token-less user has %d tokens, want 0 (empty array = no_token)", byEmail["b@x"])
	}
	if byEmail["c@x"] != 1 {
		t.Fatalf("token-with-no-reading user has %d tokens, want 1", byEmail["c@x"])
	}
	// Ana's two token statuses.
	for _, u := range body.Users {
		if u.Email != "a@x" {
			continue
		}
		if u.Name != "Ana" {
			t.Errorf("name = %q, want Ana", u.Name)
		}
		st := map[string]string{}
		for _, tk := range u.Tokens {
			st[tk.Label] = tk.Limits.Status
		}
		if st["default"] != "ok" || st["console"] != "unavailable" {
			t.Errorf("Ana token statuses = %v, want default=ok console=unavailable", st)
		}
	}
}

// vault_locked reflects the live vault: a user whose DEK is not cached reads locked.
func TestAdminRateLimitsVaultLocked(t *testing.T) {
	u := uuid.New()
	rows := []store.ListRateLimitsRow{{
		UserID: u, Email: "a@x",
		UserSecretID: pgUUIDv(uuid.New()), Label: pgTxt("default"), IsDefault: pgBool(true),
		SyncedAt: pgTs(time.Now()), FiveHourPct: pgInt2(1), SevenDayPct: pgInt2(1), Source: pgTxt("usage_endpoint"),
	}}
	h := rlHandler(&fakeRLDB{listRows: rows}, 5*time.Minute)
	box, _ := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	h.vault = vault.New(box, nil) // empty cache ⇒ every user is locked

	rec := httptest.NewRecorder()
	h.AdminRateLimits(rec, rlReq(uuid.New(), true))
	var body struct {
		Users []struct {
			VaultLocked bool `json:"vault_locked"`
		} `json:"users"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Users) != 1 || !body.Users[0].VaultLocked {
		t.Fatal("a user with no cached DEK should read vault_locked=true")
	}
}

// Deleting a token dropping its gauge row is now the DATABASE's job, not the
// handler's: the ON DELETE CASCADE on anthropic_rate_limits' composite FK (PRD #104
// M5) drops the row when the token is deleted, so no handler calls DeleteRateLimits
// on the delete path anymore. That cascade is proven end-to-end in
// TestRateLimitsPerTokenLiveDB (real Postgres); there is nothing left to assert with
// a fake DBTX here, and the former TestDeleteTokenDeletesRateLimits was removed
// rather than left asserting a call the code no longer makes.
