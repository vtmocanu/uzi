package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #412: a create that targets an already-connected (user, forge_type,
// base_url) must not silently rotate that connection's credential.
//
// The bug is in the STORE, not the handler: ConnectForge upserts on
// (user_id, forge_type, base_url), so a second POST to the same host is an
// UPDATE of the first row — new bot identity, new sealed token — and the
// Settings form reads as "add a repo". The guard is a handler-level lookup, so
// it needs a real Postgres to be observable at all: Handler.q is a concrete
// *store.Queries with no fake seam.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB
// suffix.

type forgeOverwriteFixture struct {
	h     *Handler
	pool  *pgxpool.Pool
	owner store.User
	// connID is the pre-existing connection the guard must protect.
	connID uuid.UUID
}

const forgeOverwriteBaseURL = "https://forge.example"

func newForgeOverwriteFixture(ctx context.Context, t *testing.T) forgeOverwriteFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)

	f := forgeOverwriteFixture{
		pool:  pool,
		owner: store.User{ID: uuid.New(), Email: fmt.Sprintf("fo-owner-%s@e2e", uuid.NewString()[:8])},
	}
	f.h = &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: "uzi"},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
	}

	sealed, err := box.Seal([]byte("glpat-original-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	f.connID = uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'original-bot', 7, $4)`,
		f.connID, f.owner.ID, forgeOverwriteBaseURL, sealed)
	return f
}

// call POSTs /forge/connections as the owner and returns the recorder.
func (f forgeOverwriteFixture) call(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/forge/connections", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(mw.ContextWithUser(req.Context(), f.owner))
	rec := httptest.NewRecorder()
	f.h.CreateConnection(rec, req)
	return rec
}

// storedBotUsername reads the row the guard is protecting.
func (f forgeOverwriteFixture) storedBotUsername(ctx context.Context, t *testing.T) string {
	t.Helper()
	var bot string
	if err := f.pool.QueryRow(ctx, `SELECT bot_username FROM forge_connections WHERE id = $1`, f.connID).Scan(&bot); err != nil {
		t.Fatalf("read bot_username: %v", err)
	}
	return bot
}

// The guard fires on the NORMALIZED target, not the raw request string. The row
// stores `https://` + lowercased host with any path dropped, so a user who types
// a trailing slash, a path, or a capital letter is targeting the same connection
// — and a guard comparing the raw value would miss it and fall through to the
// upsert it exists to prevent. Each case here is a spelling that normalizes onto
// the stored row.
func TestCreateConnectionRefusesExistingTargetLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	for _, baseURL := range []string{
		forgeOverwriteBaseURL,
		"https://forge.example/",
		"https://Forge.Example",
		"https://FORGE.EXAMPLE/gitlab",
		"  https://forge.example  ",
	} {
		t.Run(baseURL, func(t *testing.T) {
			rec := f.call(t, fmt.Sprintf(`{"forge_type":"gitlab","base_url":%q,"token":"glpat-new-token"}`, baseURL))
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Code         string `json:"code"`
				ConnectionID string `json:"connection_id"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if body.Code != "forge_connection_exists" {
				t.Fatalf("code = %q, want forge_connection_exists", body.Code)
			}
			if body.ConnectionID != f.connID.String() {
				t.Fatalf("connection_id = %q, want the existing row %q", body.ConnectionID, f.connID)
			}
			// The credential is the thing the bug destroyed. A 409 that still
			// rotated it would be worse than no guard at all.
			if got := f.storedBotUsername(ctx, t); got != "original-bot" {
				t.Fatalf("bot_username = %q, want the original untouched", got)
			}
		})
	}
}

// replace=true is the explicit opt-in: the guard steps aside and the create
// proceeds to validation. An empty token stops it there with a 400, which proves
// the request got PAST the guard without needing a live forge to talk to.
func TestCreateConnectionReplaceBypassesGuardLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, fmt.Sprintf(`{"forge_type":"gitlab","base_url":%q,"token":"","replace":true}`, forgeOverwriteBaseURL))
	if rec.Code == http.StatusConflict {
		t.Fatalf("replace=true must not be refused as a duplicate (body %s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from validation (body %s)", rec.Code, rec.Body.String())
	}
	if got := f.storedBotUsername(ctx, t); got != "original-bot" {
		t.Fatalf("bot_username = %q, want the original untouched", got)
	}
}

// A different host is a different connection and must not be refused. Without
// this, a guard that returned 409 unconditionally would pass the tests above.
func TestCreateConnectionAllowsDifferentTargetLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, `{"forge_type":"gitlab","base_url":"https://other.example","token":""}`)
	if rec.Code == http.StatusConflict {
		t.Fatalf("a different host must not be refused as a duplicate (body %s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from validation (body %s)", rec.Code, rec.Body.String())
	}
}

// A different forge TYPE on the same host is a different connection: the
// uniqueness key is (user, forge_type, base_url), so the guard has to match on
// all three or it refuses a legitimate second connection.
func TestCreateConnectionAllowsDifferentForgeTypeLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, fmt.Sprintf(`{"forge_type":"forgejo","base_url":%q,"token":""}`, forgeOverwriteBaseURL))
	if rec.Code == http.StatusConflict {
		t.Fatalf("a different forge type must not be refused as a duplicate (body %s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 from validation (body %s)", rec.Code, rec.Body.String())
	}
}
