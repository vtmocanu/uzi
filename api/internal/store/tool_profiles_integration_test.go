package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestRepoToolProfileOwnershipLiveDB pins the write-authz guarantee the fake-store
// unit tests cannot cover: UpsertRepoToolProfileForOwner's ownership join
// (PRD #18 M4). Only the user whose connection owns the repo may write its profile;
// a non-owner (or unknown repo) writes nothing → no rows → the handler's 404.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the e2e
// runner provides one); `go test ./...` without it SKIPs.
func TestRepoToolProfileOwnershipLiveDB(t *testing.T) {
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

	owner, other, connID, repoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, other} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("toolprof-%s@e2e", u))
	}
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// The owner can write their profile.
	row, err := q.UpsertRepoToolProfileForOwner(ctx, store.UpsertRepoToolProfileForOwnerParams{
		UserID: owner, RepoID: repoID, Packages: []byte(`["kubectl@1.31"]`),
	})
	if err != nil {
		t.Fatalf("owner upsert: %v", err)
	}
	if string(row.Packages) != `["kubectl@1.31"]` {
		t.Fatalf("packages not stored: %s", row.Packages)
	}

	// Upserting again updates in place (unique per user,repo).
	if _, err := q.UpsertRepoToolProfileForOwner(ctx, store.UpsertRepoToolProfileForOwnerParams{
		UserID: owner, RepoID: repoID, Packages: []byte(`["jq"]`),
	}); err != nil {
		t.Fatalf("owner re-upsert: %v", err)
	}
	got, err := q.GetRepoToolProfile(ctx, store.GetRepoToolProfileParams{UserID: owner, RepoID: repoID})
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if string(got.Packages) != `["jq"]` {
		t.Fatalf("re-upsert did not replace packages: %s", got.Packages)
	}

	// A non-owner writes NOTHING: the ownership join matches no row.
	if _, err := q.UpsertRepoToolProfileForOwner(ctx, store.UpsertRepoToolProfileForOwnerParams{
		UserID: other, RepoID: repoID, Packages: []byte(`["opentofu"]`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-owner upsert err = %v, want pgx.ErrNoRows", err)
	}
	// ...and no profile row exists for the non-owner.
	if _, err := q.GetRepoToolProfile(ctx, store.GetRepoToolProfileParams{UserID: other, RepoID: repoID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-owner profile should not exist, err = %v", err)
	}
}
