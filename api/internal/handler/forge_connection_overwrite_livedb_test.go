package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// A create that targets an already-connected (user, forge_type, base_url) must
// not silently rotate that connection's credential.
//
// The bug was in the STORE: the connect path upserts on
// (user_id, forge_type, base_url), so a second POST to the same host was an
// UPDATE of the first row — new bot identity, new sealed token — while the
// Settings form reads as "add a repo". The fix moves the refusal onto the store's
// create-only insert (ON CONFLICT DO NOTHING), which is why these tests need a
// real Postgres: Handler.q is a concrete *store.Queries with no fake seam, and
// the atomicity under test is the database's.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB
// suffix.

type forgeOverwriteFixture struct {
	h     *Handler
	pool  *pgxpool.Pool
	box   *secretbox.Box
	owner store.User
	// connID is the pre-existing connection the guard must protect.
	connID uuid.UUID
}

const forgeOverwriteBaseURL = "https://forge.example"

// forgeProbeForge is the driver the connect path builds, one per token. It maps
// a PAT to a bot identity derived FROM that token, so an overwrite is visible as a
// changed bot_username, and reports a compliant least-privilege token so
// validation reaches the store.
type forgeProbeForge struct {
	forgetest.BaseFake
	username string
	userID   int64
	// scopes is the least-privilege set the requested forge type requires, so a
	// connection of either type passes the token gate and reaches the store.
	scopes []string
}

func (f *forgeProbeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{Username: f.username, ForgeUserID: f.userID}, nil
}

func (f *forgeProbeForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{Scopes: f.scopes, Active: true}, nil
}

// buildForge mirrors *forgesvc.Service's builder seam: the driver gets the token
// the caller passed, which is what makes a rotation observable.
func buildForgeProbe(ft forge.Type, _ string, token string, _ time.Duration) (forge.Forge, error) {
	suffix := strings.TrimPrefix(token, "tok-")
	name := "bot-" + suffix
	scopes := []string{"api"}
	if ft == forge.TypeForgejo {
		scopes = []string{"read:user", "write:issue", "write:repository"}
	}
	return &forgeProbeForge{username: name, userID: int64(len(name)), scopes: scopes}, nil
}

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
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, buildForgeProbe)

	f := forgeOverwriteFixture{
		pool:  pool,
		box:   box,
		owner: store.User{ID: uuid.New(), Email: fmt.Sprintf("fo-owner-%s@e2e", uuid.NewString()[:8])},
	}
	f.h = &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg: config.Config{
			// The connect path validates the base URL against the SSRF allowlist
			// before it touches the store, so the guard is only reachable for a
			// host the instance actually permits.
			ForgeAllowedBaseURLs: []string{forgeOverwriteBaseURL, "https://other.example"},
		},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyUziLabel, Value: "uzi"},
		}}, time.Minute),
		svc:    svc,
		pcheck: privcheck.NewService(q, svc),
		wsvc:   workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
	}

	sealed, err := box.Seal([]byte("tok-original"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	f.connID = uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot-original', 7, $4)`,
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

// storedRow reads the bot identity and the decrypted token of the row the guard
// protects. Both are asserted together: a guard that kept the username but
// rewrote the ciphertext (or the reverse) would still have destroyed part of the
// credential.
func (f forgeOverwriteFixture) storedRow(ctx context.Context, t *testing.T, id uuid.UUID) (bot string, token string) {
	t.Helper()
	var sealed []byte
	if err := f.pool.QueryRow(ctx, `SELECT bot_username, token_ciphertext FROM forge_connections WHERE id = $1`, id).Scan(&bot, &sealed); err != nil {
		t.Fatalf("read connection row: %v", err)
	}
	plain, err := f.box.Open(sealed)
	if err != nil {
		t.Fatalf("open sealed token: %v", err)
	}
	return bot, string(plain)
}

// The guard fires on the NORMALIZED target, not the raw request string. The row
// stores `https://` + lowercased host with any path dropped, so a user who types
// a trailing slash, a path, or a capital letter is targeting the same connection
// — and a guard comparing the raw value would miss it and write over the row it
// exists to prevent. Each case here is a spelling that normalizes onto the
// stored row.
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
			rec := f.call(t, fmt.Sprintf(`{"forge_type":"gitlab","base_url":%q,"token":"tok-rotated"}`, baseURL))
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
			bot, token := f.storedRow(ctx, t, f.connID)
			if bot != "bot-original" || token != "tok-original" {
				t.Fatalf("row = (%q, %q), want the original untouched", bot, token)
			}
		})
	}
}

// replace=true is the explicit opt-in: the guard steps aside and the create
// rotates the existing row, rather than adding a second one.
func TestCreateConnectionReplaceRotatesInPlaceLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, fmt.Sprintf(`{"forge_type":"gitlab","base_url":%q,"token":"tok-rotated","replace":true}`, forgeOverwriteBaseURL))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if body.Connection.ID != f.connID.String() {
		t.Fatalf("connection id = %q, want the rotated row %q", body.Connection.ID, f.connID)
	}
	bot, token := f.storedRow(ctx, t, f.connID)
	if bot != "bot-rotated" || token != "tok-rotated" {
		t.Fatalf("row = (%q, %q), want the replacement credential", bot, token)
	}
	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM forge_connections WHERE user_id = $1`, f.owner.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("connection rows = %d, want the replace to rotate in place", rows)
	}
}

// A different host is a different connection and must not be refused.
func TestCreateConnectionAllowsDifferentTargetLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, `{"forge_type":"gitlab","base_url":"https://other.example","token":"tok-other"}`)
	f.assertSecondConnection(ctx, t, rec, "gitlab", "https://other.example", "bot-other", "tok-other")
}

// A different forge TYPE on the same host is a different connection: the
// uniqueness key is (user, forge_type, base_url), so the guard has to match on
// all three or it refuses a legitimate second connection.
func TestCreateConnectionAllowsDifferentForgeTypeLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	rec := f.call(t, fmt.Sprintf(`{"forge_type":"forgejo","base_url":%q,"token":"tok-forgejo"}`, forgeOverwriteBaseURL))
	f.assertSecondConnection(ctx, t, rec, "forgejo", forgeOverwriteBaseURL, "bot-forgejo", "tok-forgejo")
}

// assertSecondConnection requires a create for a target distinct from the
// fixture's to succeed as a NEW row carrying the requested target and
// credential, with the original connection left exactly as it was. A refusal,
// a validation error or an in-place rotation all fail it.
func (f forgeOverwriteFixture) assertSecondConnection(ctx context.Context, t *testing.T, rec *httptest.ResponseRecorder, wantType, wantBaseURL, wantBot, wantToken string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a distinct target (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
	}
	newID, err := uuid.Parse(body.Connection.ID)
	if err != nil {
		t.Fatalf("parse new connection id %q: %v", body.Connection.ID, err)
	}
	if newID == f.connID {
		t.Fatalf("the distinct target rotated the original connection %s instead of creating one", f.connID)
	}
	var gotType, gotBaseURL string
	if err := f.pool.QueryRow(ctx, `SELECT forge_type, base_url FROM forge_connections WHERE id = $1`, newID).Scan(&gotType, &gotBaseURL); err != nil {
		t.Fatalf("read new row: %v", err)
	}
	if gotType != wantType || gotBaseURL != wantBaseURL {
		t.Fatalf("new row target = (%q, %q), want (%q, %q)", gotType, gotBaseURL, wantType, wantBaseURL)
	}
	if bot, token := f.storedRow(ctx, t, newID); bot != wantBot || token != wantToken {
		t.Fatalf("new row credential = (%q, %q), want (%q, %q)", bot, token, wantBot, wantToken)
	}
	if bot, token := f.storedRow(ctx, t, f.connID); bot != "bot-original" || token != "tok-original" {
		t.Fatalf("original row = (%q, %q), want it untouched (bot-original, tok-original)", bot, token)
	}
	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM forge_connections WHERE user_id = $1`, f.owner.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("connection rows = %d, want 2 (the original plus the distinct target)", rows)
	}
}

// Two concurrent creates for the same absent target. This is the race a
// read-then-upsert cannot close: both lookups see no row, both proceed, and the
// second write silently replaces the first connection's credential.
//
// The create-only insert makes exactly one of them win. The assertions cover
// both halves of the credential — the bot identity and the stored ciphertext —
// because a race that kept one and replaced the other is still a destroyed
// credential, and the row that survives must be one of the two requests intact,
// never a blend of both.
func TestCreateConnectionConcurrentCreatesKeepOneCredentialLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newForgeOverwriteFixture(ctx, t)

	// The race needs the target to be ABSENT, so both requests can get past
	// validation and reach the insert. The fixture's row is the pre-existing
	// connection the other tests protect; this one deletes it.
	mustExecT(ctx, t, f.pool, `DELETE FROM forge_connections WHERE id = $1`, f.connID)

	tokens := []string{"tok-concurrent-a", "tok-concurrent-b"}
	type result struct {
		status int
		connID string
		// token is the request's own PAT, carried so the winner's expected
		// credential is read off the result rather than re-indexed from tokens.
		token string
	}
	results := make([]result, len(tokens))

	// A start barrier, so the two requests overlap rather than run in sequence.
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	ready.Add(len(tokens))
	done.Add(len(tokens))
	for i, tok := range tokens {
		go func(i int, tok string) {
			defer done.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/forge/connections",
				strings.NewReader(fmt.Sprintf(`{"forge_type":"gitlab","base_url":%q,"token":%q}`, forgeOverwriteBaseURL, tok)))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(mw.ContextWithUser(req.Context(), f.owner))
			ready.Done()
			<-start
			rec := httptest.NewRecorder()
			f.h.CreateConnection(rec, req)
			var body struct {
				Connection struct {
					ID string `json:"id"`
				} `json:"connection"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			results[i] = result{status: rec.Code, connID: body.Connection.ID, token: tok}
		}(i, tok)
	}
	ready.Wait()
	close(start)
	done.Wait()

	var created, refused int
	winner := -1
	for i, r := range results {
		switch r.status {
		case http.StatusCreated:
			created++
			winner = i
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("create %d status = %d, want 201 or 409", i, r.status)
		}
	}
	if created != 1 || refused != 1 {
		t.Fatalf("created=%d refused=%d, want exactly one of each", created, refused)
	}
	if winner < 0 || winner >= len(tokens) {
		t.Fatalf("winner index = %d, want one of the %d requests", winner, len(tokens))
	}

	var rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM forge_connections WHERE user_id = $1`, f.owner.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("connection rows = %d, want 1 (a second row means the loser wrote)", rows)
	}
	if results[winner].connID == "" {
		t.Fatal("the winning create returned no connection id")
	}
	rowID, err := uuid.Parse(results[winner].connID)
	if err != nil {
		t.Fatalf("parse winning connection id %q: %v", results[winner].connID, err)
	}
	// The surviving row must be the winner's credential, intact: identity and
	// ciphertext both derived from the same token.
	bot, token := f.storedRow(ctx, t, rowID)
	wantToken := results[winner].token
	if token != wantToken {
		t.Fatalf("stored token = %q, want the winner's %q (the race overwrote the credential)", token, wantToken)
	}
	if bot != "bot-concurrent-"+strings.TrimPrefix(wantToken, "tok-concurrent-") {
		t.Fatalf("stored bot = %q, which does not match the stored token %q", bot, token)
	}
}
