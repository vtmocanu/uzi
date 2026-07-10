package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

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
		u, err := q.CreateUser(ctx, store.CreateUserParams{Email: email, PasswordHash: "x"})
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

	u, err := q.CreateUser(ctx, store.CreateUserParams{Email: "dz-" + uniq(t) + "@example.com", PasswordHash: "x"})
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

// uniq returns a short unique suffix so re-runs against the same DB never collide
// on the users.email unique constraint or on a reused Slack id.
func uniq(t *testing.T) string {
	t.Helper()
	return uuid.NewString()[:8]
}
