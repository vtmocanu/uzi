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

// fakeRLDB is a store.DBTX that answers the rate-limit read queries from fixed
// in-memory results, so the handlers run end to end without a database. It also
// records whether the D3b DeleteRateLimits ran.
type fakeRLDB struct {
	hasToken  bool
	getRow    *store.AnthropicRateLimit // nil ⇒ pgx.ErrNoRows (no reading yet)
	listRows  []store.ListRateLimitsRow
	deletedRL bool
}

func (f *fakeRLDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM anthropic_rate_limits") {
		f.deletedRL = true
	}
	return pgconn.CommandTag{}, nil
}

func (f *fakeRLDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "SELECT EXISTS") && strings.Contains(sql, "user_secrets"):
		return fakeScanRow{func(dest ...any) error { *dest[0].(*bool) = f.hasToken; return nil }}
	case strings.Contains(sql, "FROM anthropic_rate_limits") && strings.Contains(sql, "WHERE user_id"):
		if f.getRow == nil {
			return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
		}
		return fakeScanRow{scanRateLimit(*f.getRow)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

func (f *fakeRLDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	scans := make([]func(...any) error, 0, len(f.listRows))
	for _, r := range f.listRows {
		scans = append(scans, scanListRateLimit(r))
	}
	return &fakeNotifRows{scans: scans}, nil
}

func scanRateLimit(r store.AnthropicRateLimit) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = r.UserID
		*dest[1].(*pgtype.Int2) = r.FiveHourPct
		*dest[2].(*pgtype.Timestamptz) = r.FiveHourResetsAt
		*dest[3].(*pgtype.Int2) = r.SevenDayPct
		*dest[4].(*pgtype.Timestamptz) = r.SevenDayResetsAt
		*dest[5].(*pgtype.Text) = r.Source
		*dest[6].(*pgtype.Timestamptz) = r.SyncedAt
		return nil
	}
}

func scanListRateLimit(r store.ListRateLimitsRow) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = r.UserID
		*dest[1].(*string) = r.Email
		*dest[2].(*pgtype.Text) = r.DisplayName
		*dest[3].(*bool) = r.HasToken
		*dest[4].(*pgtype.Int2) = r.FiveHourPct
		*dest[5].(*pgtype.Timestamptz) = r.FiveHourResetsAt
		*dest[6].(*pgtype.Int2) = r.SevenDayPct
		*dest[7].(*pgtype.Timestamptz) = r.SevenDayResetsAt
		*dest[8].(*pgtype.Text) = r.Source
		*dest[9].(*pgtype.Timestamptz) = r.SyncedAt
		return nil
	}
}

func pgInt2(v int16) pgtype.Int2          { return pgtype.Int2{Int16: v, Valid: true} }
func pgTs(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func pgTxt(s string) pgtype.Text          { return pgtype.Text{String: s, Valid: true} }
func okRow(u uuid.UUID, synced time.Time) *store.AnthropicRateLimit {
	return &store.AnthropicRateLimit{
		UserID:           u,
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

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return m
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

func TestSelfRateLimitsNoToken(t *testing.T) {
	u := uuid.New()
	h := rlHandler(&fakeRLDB{hasToken: false}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(u, false))

	m := decodeMap(t, rec)
	if m["status"] != "no_token" {
		t.Fatalf("status = %v, want no_token", m["status"])
	}
	// Status-only: no window/source/synced_at/stale keys.
	for _, k := range []string{"five_hour", "seven_day", "source", "synced_at", "stale"} {
		if _, present := m[k]; present {
			t.Errorf("no_token response leaked key %q: %v", k, m)
		}
	}
}

func TestSelfRateLimitsUnavailable(t *testing.T) {
	u := uuid.New()
	// Token held, but no gauge row yet (ErrNoRows) ⇒ unavailable.
	h := rlHandler(&fakeRLDB{hasToken: true, getRow: nil}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(u, false))

	m := decodeMap(t, rec)
	if m["status"] != "unavailable" {
		t.Fatalf("status = %v, want unavailable", m["status"])
	}
	for _, k := range []string{"five_hour", "seven_day", "source", "synced_at", "stale"} {
		if _, present := m[k]; present {
			t.Errorf("unavailable response leaked key %q", k)
		}
	}
}

func TestSelfRateLimitsOK(t *testing.T) {
	u := uuid.New()
	h := rlHandler(&fakeRLDB{hasToken: true, getRow: okRow(u, time.Now())}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(u, false))

	m := decodeMap(t, rec)
	if m["status"] != "ok" || m["source"] != "usage_endpoint" {
		t.Fatalf("status/source = %v/%v", m["status"], m["source"])
	}
	if m["stale"] != false {
		t.Errorf("stale = %v, want false (fresh reading)", m["stale"])
	}
	if _, ok := m["synced_at"].(string); !ok {
		t.Errorf("synced_at should be an ISO string, got %v", m["synced_at"])
	}
	five := m["five_hour"].(map[string]any)
	if five["pct"].(float64) != 55 {
		t.Errorf("five_hour.pct = %v, want 55", five["pct"])
	}
	if _, ok := five["resets_at"].(float64); !ok {
		t.Errorf("five_hour.resets_at should be an epoch number, got %v", five["resets_at"])
	}
	seven := m["seven_day"].(map[string]any)
	if _, present := seven["resets_at"]; !present || seven["resets_at"] != nil {
		t.Errorf("seven_day.resets_at should be present and null, got %v (present=%v)", seven["resets_at"], present)
	}
	// No internal fields leak.
	if _, bad := m["is_admin"]; bad {
		t.Error("response must not carry is_admin")
	}
}

func TestSelfRateLimitsStale(t *testing.T) {
	u := uuid.New()
	// synced 20m ago with a 5m interval (3× = 15m) ⇒ stale.
	h := rlHandler(&fakeRLDB{hasToken: true, getRow: okRow(u, time.Now().Add(-20*time.Minute))}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(u, false))
	if decodeMap(t, rec)["stale"] != true {
		t.Fatal("reading older than 3× interval should be stale")
	}
}

func TestSelfRateLimitsPollerDisabledAlwaysStale(t *testing.T) {
	u := uuid.New()
	// Poller disabled (interval 0): a just-synced row is still served, always stale.
	h := rlHandler(&fakeRLDB{hasToken: true, getRow: okRow(u, time.Now())}, 0)
	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, rlReq(u, false))
	m := decodeMap(t, rec)
	if m["status"] != "ok" {
		t.Fatalf("status = %v, want ok (rows still served when poller off)", m["status"])
	}
	if m["stale"] != true {
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

func TestAdminRateLimitsShape(t *testing.T) {
	okUser, noTokUser, unavailUser := uuid.New(), uuid.New(), uuid.New()
	rows := []store.ListRateLimitsRow{
		{
			UserID: okUser, Email: "a@x", DisplayName: pgTxt("Ana"), HasToken: true,
			FiveHourPct: pgInt2(90), SevenDayPct: pgInt2(30),
			Source: pgTxt("header_probe"), SyncedAt: pgTs(time.Now()),
		},
		{UserID: noTokUser, Email: "b@x", HasToken: false},                         // no token
		{UserID: unavailUser, Email: "c@x", HasToken: true /* SyncedAt invalid */}, // token, no reading
	}
	h := rlHandler(&fakeRLDB{listRows: rows}, 5*time.Minute)
	rec := httptest.NewRecorder()
	h.AdminRateLimits(rec, rlReq(uuid.New(), true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}

	var body struct {
		Users []struct {
			ID          string `json:"id"`
			Email       string `json:"email"`
			Name        string `json:"name"`
			VaultLocked bool   `json:"vault_locked"`
			Limits      struct {
				Status string `json:"status"`
				Source string `json:"source"`
			} `json:"limits"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) != 3 {
		t.Fatalf("got %d users, want 3 (every user incl. no_token)", len(body.Users))
	}
	byEmail := map[string]string{}
	for _, u := range body.Users {
		byEmail[u.Email] = u.Limits.Status
		if u.VaultLocked { // nil vault ⇒ vaultUnlocked=true ⇒ not locked
			t.Errorf("%s vault_locked = true with no vault wired", u.Email)
		}
	}
	if byEmail["a@x"] != "ok" || byEmail["b@x"] != "no_token" || byEmail["c@x"] != "unavailable" {
		t.Fatalf("statuses = %v, want a=ok b=no_token c=unavailable", byEmail)
	}
	if body.Users[0].Name != "Ana" {
		t.Errorf("users[0].name = %q, want Ana", body.Users[0].Name)
	}
}

// vault_locked reflects the live vault: a user whose DEK is not cached reads locked.
func TestAdminRateLimitsVaultLocked(t *testing.T) {
	u := uuid.New()
	rows := []store.ListRateLimitsRow{{UserID: u, Email: "a@x", HasToken: true, SyncedAt: pgTs(time.Now()), FiveHourPct: pgInt2(1), SevenDayPct: pgInt2(1), Source: pgTxt("usage_endpoint")}}
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
	if !body.Users[0].VaultLocked {
		t.Fatal("a user with no cached DEK should read vault_locked=true")
	}
}

// D3b: deleting the token also drops the gauge row.
func TestDeleteTokenDeletesRateLimits(t *testing.T) {
	db := &fakeRLDB{}
	h := &Handler{q: store.New(db)} // nil vault ⇒ master-box seal path unused on delete
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/me/secrets/anthropic_token", nil)
	h.DeleteAnthropicToken(rec, req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsActive: true})))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", rec.Code)
	}
	if !db.deletedRL {
		t.Fatal("token delete must also DeleteRateLimits (D3b)")
	}
}
