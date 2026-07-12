package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestSlackLinkResolutionLiveDB pins the two SQL-level guarantees the fake-store
// unit tests cannot cover (PRD #25 M3): the inbound authz join resolves a Slack
// id to EXACTLY ONE confirmed user (and to nothing when unconfirmed), and the
// unique partial index refuses a second user claiming an id already linked. These
// are the backstops behind "no ambiguous or squatted link can act on a run".
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the e2e
// runner provides one); `go test ./...` without it SKIPs.
func TestSlackLinkResolutionLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	mk := func(email string) store.User {
		u, err := q.CreateUser(ctx, store.CreateUserParams{Email: email, PasswordHash: pgtype.Text{String: "x", Valid: true}})
		if err != nil {
			t.Fatalf("create user %s: %v", email, err)
		}
		return u
	}
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	alice := mk("alice-" + uniq(t) + "@example.com")
	bob := mk("bob-" + uniq(t) + "@example.com")
	slackID := "U" + uniq(t)

	// Alice resolves to slackID but has not confirmed yet: the authz join must find
	// NOTHING (an unconfirmed match resolves to no user, so it can authorize nothing).
	if _, err := q.SetUserSlackResolvedID(ctx, store.SetUserSlackResolvedIDParams{
		SlackResolvedID: txt(slackID), ID: alice.ID,
	}); err != nil {
		t.Fatalf("set alice resolved id: %v", err)
	}
	if _, err := q.GetConfirmedUserBySlackID(ctx, txt(slackID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unconfirmed match must resolve to no user, got err=%v", err)
	}

	// Bob cannot claim the same effective id — the unique partial index rejects it.
	_, err = q.SetUserSlackOverride(ctx, store.SetUserSlackOverrideParams{
		SlackMemberID: txt(slackID), SlackResolvedID: txt(slackID), ID: bob.ID,
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("a colliding override must fail with unique violation 23505, got %v", err)
	}

	// Alice confirms: now the join resolves to EXACTLY her, and only her.
	n, err := q.ConfirmUserSlackLink(ctx, txt(slackID))
	if err != nil || n != 1 {
		t.Fatalf("confirm alice: rows=%d err=%v (want 1, nil)", n, err)
	}
	got, err := q.GetConfirmedUserBySlackID(ctx, txt(slackID))
	if err != nil {
		t.Fatalf("confirmed lookup: %v", err)
	}
	if got.ID != alice.ID {
		t.Fatalf("confirmed lookup resolved to %s, want alice %s", got.ID, alice.ID)
	}

	// A second confirm is a no-op (already confirmed) — idempotent, never a second row.
	if n, err := q.ConfirmUserSlackLink(ctx, txt(slackID)); err != nil || n != 0 {
		t.Fatalf("re-confirm should affect 0 rows, got rows=%d err=%v", n, err)
	}

	// Clearing the link makes the authz join resolve to nothing again.
	if _, err := q.ClearUserSlackLink(ctx, txt(slackID)); err != nil {
		t.Fatalf("clear link: %v", err)
	}
	if _, err := q.GetConfirmedUserBySlackID(ctx, txt(slackID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cleared link must resolve to no user, got err=%v", err)
	}
}

// TestSlackConfirmedLookupSkipsDeactivatedLiveDB pins the M5 audit fix: the
// inbound authz join (GetConfirmedUserBySlackID) — the single chokepoint for every
// inbound Slack action (gate buttons + thread replies) — must exclude deactivated
// accounts, mirroring the webui's RequireAuth block. Without it a confirmed-linked
// user who was later deactivated could still Approve/Reject/reply from Slack.
func TestSlackConfirmedLookupSkipsDeactivatedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: "dz-" + uniq(t) + "@example.com", PasswordHash: pgtype.Text{String: "x", Valid: true}})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	slackID := "U" + uniq(t)
	if _, err := q.SetUserSlackOverride(ctx, store.SetUserSlackOverrideParams{
		SlackMemberID: txt(slackID), SlackResolvedID: txt(slackID), ID: u.ID,
	}); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if n, err := q.ConfirmUserSlackLink(ctx, txt(slackID)); err != nil || n != 1 {
		t.Fatalf("confirm: rows=%d err=%v", n, err)
	}
	// Active + confirmed → resolves.
	if _, err := q.GetConfirmedUserBySlackID(ctx, txt(slackID)); err != nil {
		t.Fatalf("active confirmed user must resolve: %v", err)
	}
	// Deactivate → the SAME confirmed link must now resolve to nothing.
	if _, err := q.SetUserActive(ctx, store.SetUserActiveParams{IsActive: false, ID: u.ID}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := q.GetConfirmedUserBySlackID(ctx, txt(slackID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a deactivated (but confirmed) user must resolve to no user, got err=%v", err)
	}
}

// TestSlackBotTokenSealedAtRestLiveDB is the auditor's M1 end-to-end at-rest
// assertion, exercising the REAL seal (secretbox+base64) + REAL Postgres write +
// raw read. It bypasses only the HTTP hop, because the settings PUT live-validates
// a bot token against Slack (AuthTest) which cannot run in an offline gate — the
// seal/store/read path it proves is identical. It pins: (1) the raw app_settings
// value is base64 that neither equals nor contains the token, and (2) the admin
// view (the GET's data source) reports configured=true with no token/ciphertext
// bytes, and the decrypt accessor round-trips.
func TestSlackBotTokenSealedAtRestLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	// A non-secret 32-byte AES key for the test (New only checks length).
	box, err := secretbox.New(bytes.Repeat([]byte("ab"), 16))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}

	const token = "xoxb-not-a-real-bot-token-value"
	sealed, err := settings.ValueForStorage(box, settings.KeySlackBotToken, token)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := q.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
		Key: settings.KeySlackBotToken, Value: sealed, UpdatedBy: pgtype.UUID{},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// (1) Raw DB read: the stored value is base64 and never the token bytes.
	var raw string
	if err := pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key=$1", settings.KeySlackBotToken).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if raw == token || strings.Contains(raw, token) {
		t.Fatalf("stored value must neither equal nor contain the token: %q", raw)
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("stored value must be base64, got %q (%v)", raw, err)
	}
	if strings.Contains(string(dec), token) {
		t.Fatalf("decoded ciphertext must not contain the token plaintext")
	}

	// (2) Admin view: configured=true, secret key absent from Values, and no token
	// or ciphertext bytes anywhere in the view.
	cache := settings.New(q, 0)
	cache.ConfigureSecrets(box, nil)
	av, err := cache.AdminView(ctx)
	if err != nil {
		t.Fatalf("admin view: %v", err)
	}
	if !av.Secrets[settings.KeySlackBotToken] {
		t.Fatalf("admin view must report %s configured=true", settings.KeySlackBotToken)
	}
	if _, present := av.Values[settings.KeySlackBotToken]; present {
		t.Fatalf("admin view Values must not carry the secret key at all")
	}
	if blob := fmt.Sprintf("%+v", av); strings.Contains(blob, token) || strings.Contains(blob, raw) {
		t.Fatalf("admin view must not contain token or ciphertext bytes: %q", blob)
	}

	// The decrypt accessor round-trips to the original token (proves reversible
	// sealing with the key, not merely an opaque blob).
	got, err := cache.SlackBotToken(ctx)
	if err != nil {
		t.Fatalf("decrypt accessor: %v", err)
	}
	if got != token {
		t.Fatalf("decrypt accessor = %q, want the original token", got)
	}
}

// uniq returns a short unique suffix so re-runs against the same DB never collide
// on the users.email unique constraint or on a reused Slack id.
func uniq(t *testing.T) string {
	t.Helper()
	return uuid.NewString()[:8]
}
