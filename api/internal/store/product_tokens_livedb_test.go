package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/producttoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for PRD #1907 M1 (migration 00270, queries/product_tokens.sql).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. Every unique value (emails, product names, token hashes) is
// derived from a fresh uuid, because the database is shared across packages.

func productLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
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
	t.Cleanup(pool.Close)
	return ctx, pool, store.New(pool)
}

func newProductUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	email := fmt.Sprintf("product-%s@e2e", id)
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, email)
	return id, email
}

func newProduct(ctx context.Context, t *testing.T, q *store.Queries, createdBy uuid.UUID) store.Product {
	t.Helper()
	p, err := q.CreateProduct(ctx, store.CreateProductParams{
		Name:        "product-" + uuid.NewString(),
		Description: "an external product",
		CreatedBy:   pgtype.UUID{Bytes: createdBy, Valid: true},
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	return p
}

type mintedProductToken struct {
	row   store.CreateProductTokenRow
	token string
	hash  []byte
}

func mintProductToken(ctx context.Context, t *testing.T, q *store.Queries, user, product uuid.UUID, expires pgtype.Timestamptz) mintedProductToken {
	t.Helper()
	token, hash, prefix, err := producttoken.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	row, err := q.CreateProductToken(ctx, store.CreateProductTokenParams{
		UserID:      user,
		ProductID:   product,
		Name:        "token-" + uuid.NewString(),
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      []string{producttoken.ScopeJobsRun, producttoken.ScopeJobsRead},
		ExpiresAt:   expires,
	})
	if err != nil {
		t.Fatalf("create product token: %v", err)
	}
	return mintedProductToken{row: row, token: token, hash: hash}
}

var neverExpires = pgtype.Timestamptz{}

func expiresIn(d time.Duration) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().Add(d), Valid: true}
}

// authOK asserts the auth lookup resolves hash to exactly this token's identity.
func authOK(ctx context.Context, t *testing.T, q *store.Queries, m mintedProductToken, wantProductName string) {
	t.Helper()
	got, err := q.GetProductTokenForAuth(ctx, m.hash)
	if err != nil {
		t.Fatalf("auth lookup of a live token: %v", err)
	}
	if got.ID != m.row.ID || got.UserID != m.row.UserID || got.ProductID != m.row.ProductID {
		t.Fatalf("auth lookup resolved the wrong identity: got %+v, want token %s user %s product %s",
			got, m.row.ID, m.row.UserID, m.row.ProductID)
	}
	if got.ProductName != wantProductName {
		t.Fatalf("auth lookup product_name = %q, want %q", got.ProductName, wantProductName)
	}
	if !slices.Equal(got.Scopes, []string{producttoken.ScopeJobsRun, producttoken.ScopeJobsRead}) {
		t.Fatalf("auth lookup scopes = %v", got.Scopes)
	}
}

// authClosed asserts the auth lookup fails closed: no row, not an error of any other kind.
func authClosed(ctx context.Context, t *testing.T, q *store.Queries, hash []byte, why string) {
	t.Helper()
	_, err := q.GetProductTokenForAuth(ctx, hash)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s: auth lookup must fail closed with no row, got err=%v", why, err)
	}
}

// TestProductTokenAuthLookupLiveDB pins GetProductTokenForAuth: a live token with a NULL
// expiry (the NULL trap) and with a future expiry both resolve, and each fail-closed
// condition — revoked, expired, disabled product, soft-deleted product, inactive user —
// turns a token that DID resolve (asserted first, per case) into no row. Asserting the
// positive first is what makes each negative mean "this condition closed it" rather
// than "the fixture never worked".
func TestProductTokenAuthLookupLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)

	t.Run("null expiry resolves", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
		if m.row.ExpiresAt.Valid {
			t.Fatalf("expires_at stored as %v, want NULL", m.row.ExpiresAt)
		}
		authOK(ctx, t, q, m, p.Name)
	})

	t.Run("future expiry resolves", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		authOK(ctx, t, q, mintProductToken(ctx, t, q, user, p.ID, expiresIn(time.Hour)), p.Name)
	})

	t.Run("unknown hash", func(t *testing.T) {
		authClosed(ctx, t, q, producttoken.Hash("uz"+"p_"+uuid.NewString()), "unknown hash")
	})

	t.Run("revoked", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
		authOK(ctx, t, q, m, p.Name)
		n, err := q.RevokeProductToken(ctx, store.RevokeProductTokenParams{ID: m.row.ID, UserID: user})
		if err != nil || n != 1 {
			t.Fatalf("revoke: n=%d err=%v", n, err)
		}
		authClosed(ctx, t, q, m.hash, "revoked")
	})

	t.Run("expired", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, expiresIn(time.Hour))
		authOK(ctx, t, q, m, p.Name)
		mustExec(ctx, t, pool, `UPDATE product_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, m.row.ID)
		authClosed(ctx, t, q, m.hash, "expired")
	})

	t.Run("disabled product", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
		authOK(ctx, t, q, m, p.Name)
		if _, err := q.UpdateProduct(ctx, store.UpdateProductParams{ID: p.ID, Enabled: pgtype.Bool{Bool: false, Valid: true}}); err != nil {
			t.Fatalf("disable: %v", err)
		}
		authClosed(ctx, t, q, m.hash, "disabled product")
		// Re-enabling a disabled (not deleted) product restores its tokens: the disable,
		// not some side effect, is what closed it.
		if _, err := q.UpdateProduct(ctx, store.UpdateProductParams{ID: p.ID, Enabled: pgtype.Bool{Bool: true, Valid: true}}); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		authOK(ctx, t, q, m, p.Name)
	})

	t.Run("soft-deleted product", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
		authOK(ctx, t, q, m, p.Name)
		n, err := q.SoftDeleteProduct(ctx, p.ID)
		if err != nil || n != 1 {
			t.Fatalf("soft delete: n=%d err=%v", n, err)
		}
		authClosed(ctx, t, q, m.hash, "soft-deleted product")
		// The soft delete sets both terms the lookup checks; TestProductTokenAuthDeletedAtTermLiveDB
		// isolates deleted_at from enabled.
		var enabled bool
		var deleted pgtype.Timestamptz
		if err := pool.QueryRow(ctx, `SELECT enabled, deleted_at FROM products WHERE id = $1`, p.ID).Scan(&enabled, &deleted); err != nil {
			t.Fatal(err)
		}
		if enabled || !deleted.Valid {
			t.Fatalf("soft delete left enabled=%v deleted_at=%v, want false/set", enabled, deleted)
		}
	})

	t.Run("inactive user", func(t *testing.T) {
		user, _ := newProductUser(ctx, t, pool)
		p := newProduct(ctx, t, q, user)
		m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
		authOK(ctx, t, q, m, p.Name)
		mustExec(ctx, t, pool, `UPDATE users SET is_active = false WHERE id = $1`, user)
		authClosed(ctx, t, q, m.hash, "inactive user")
	})
}

// TestProductTokenAuthDeletedAtTermLiveDB isolates the auth lookup's
// `p.deleted_at IS NULL` term from its `p.enabled` term. SoftDeleteProduct sets both,
// so the soft-delete case above cannot tell them apart; here the CHECK is dropped for
// the duration of ONE transaction (rolled back, so nothing persists and no other test
// sees it) to build the otherwise-unrepresentable "deleted but enabled" row.
func TestProductTokenAuthDeletedAtTermLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	user, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, user)
	m := mintProductToken(ctx, t, q, user, p.ID, neverExpires)
	authOK(ctx, t, q, m, p.Name)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE products DROP CONSTRAINT products_deleted_is_disabled`); err != nil {
		t.Fatalf("drop check in tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE products SET deleted_at = now(), enabled = true WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("deleted-but-enabled row: %v", err)
	}
	if _, err := store.New(tx).GetProductTokenForAuth(ctx, m.hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted_at alone must fail the auth lookup closed, got err=%v", err)
	}
}

// TestProductTokenSchemaConstraintsLiveDB pins the CHECKs and FK actions of migration
// 00270: the scopes vocabulary and non-empty CHECKs, ON DELETE RESTRICT on product_id,
// ON DELETE SET NULL on products.created_by, the deleted-is-disabled CHECK, and the
// live-name partial unique index.
func TestProductTokenSchemaConstraintsLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	user, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, user)

	insertScopes := func(scopes []string) error {
		_, hash, prefix, err := producttoken.Generate()
		if err != nil {
			t.Fatal(err)
		}
		_, err = q.CreateProductToken(ctx, store.CreateProductTokenParams{
			UserID: user, ProductID: p.ID, Name: "scopes", TokenHash: hash, TokenPrefix: prefix,
			Scopes: scopes,
		})
		return err
	}
	for name, scopes := range map[string][]string{
		"unknown scope":         {"jobs:write"},
		"known plus unknown":    {producttoken.ScopeJobsRun, "admin"},
		"empty":                 {},
		"case variant":          {"JOBS:RUN"},
		"known with whitespace": {"jobs:run "},
	} {
		if err := insertScopes(scopes); pgCode(err) != "23514" {
			t.Errorf("%s %v: want check_violation 23514, got %v", name, scopes, err)
		}
	}
	// A NULL element cannot be sent through []string, so it is inserted raw.
	_, hash, _, _ := producttoken.Generate()
	_, err := pool.Exec(ctx, `INSERT INTO product_tokens (user_id, product_id, name, token_hash, token_prefix, scopes)
		VALUES ($1, $2, 'null-scope', $3, 'x', ARRAY['jobs:run', NULL]::text[])`, user, p.ID, hash)
	if pgCode(err) != "23514" {
		t.Errorf("NULL scope element: want check_violation 23514, got %v", err)
	}
	for _, scopes := range [][]string{{producttoken.ScopeJobsRun}, {producttoken.ScopeJobsRead}, producttoken.Scopes} {
		if err := insertScopes(scopes); err != nil {
			t.Errorf("valid scopes %v rejected: %v", scopes, err)
		}
	}

	t.Run("hard product delete is restricted while tokens exist", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, p.ID)
		if pgCode(err) != "23503" {
			t.Fatalf("hard delete of a product with tokens: want foreign_key_violation 23503, got %v", err)
		}
		// Positive control: a product with no tokens hard-deletes, so the RESTRICT, not
		// something else, is what blocked the delete above.
		empty := newProduct(ctx, t, q, user)
		if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id = $1`, empty.ID); err != nil {
			t.Fatalf("hard delete of a token-less product: %v", err)
		}
	})

	t.Run("deleting the creating admin sets created_by NULL", func(t *testing.T) {
		admin, _ := newProductUser(ctx, t, pool)
		mustExec(ctx, t, pool, `UPDATE users SET is_admin = true WHERE id = $1`, admin)
		made := newProduct(ctx, t, q, admin)
		if !made.CreatedBy.Valid || made.CreatedBy.Bytes != admin {
			t.Fatalf("created_by = %v, want %s", made.CreatedBy, admin)
		}
		// The product has a token owned by ANOTHER user, so it must survive the admin's
		// delete with its tokens intact.
		m := mintProductToken(ctx, t, q, user, made.ID, neverExpires)
		mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, admin)
		got, err := q.GetProduct(ctx, made.ID)
		if err != nil {
			t.Fatalf("product must survive its creator's delete: %v", err)
		}
		if got.CreatedBy.Valid {
			t.Fatalf("created_by = %v after the creator was deleted, want NULL", got.CreatedBy)
		}
		authOK(ctx, t, q, m, made.Name)
	})

	t.Run("deleting the token owner cascades their tokens", func(t *testing.T) {
		owner, _ := newProductUser(ctx, t, pool)
		m := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
		mustExec(ctx, t, pool, `DELETE FROM users WHERE id = $1`, owner)
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM product_tokens WHERE id = $1`, m.row.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("token of a deleted user survived (ON DELETE CASCADE)")
		}
	})

	t.Run("a deleted product cannot be enabled", func(t *testing.T) {
		d := newProduct(ctx, t, q, user)
		if n, err := q.SoftDeleteProduct(ctx, d.ID); err != nil || n != 1 {
			t.Fatalf("soft delete: n=%d err=%v", n, err)
		}
		if n, err := q.SoftDeleteProduct(ctx, d.ID); err != nil || n != 0 {
			t.Fatalf("second soft delete: n=%d err=%v, want 0 rows", n, err)
		}
		if _, err := q.UpdateProduct(ctx, store.UpdateProductParams{ID: d.ID, Description: pgtype.Text{String: "x", Valid: true}, Enabled: pgtype.Bool{Bool: true, Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("UpdateProduct on a deleted product must match no row, got %v", err)
		}
		_, err := pool.Exec(ctx, `UPDATE products SET enabled = true WHERE id = $1`, d.ID)
		if pgCode(err) != "23514" {
			t.Fatalf("raw re-enable of a deleted product: want check_violation 23514, got %v", err)
		}
	})

	t.Run("live names are unique case-insensitively; a soft delete frees the name", func(t *testing.T) {
		name := "Acme-" + uuid.NewString()
		first, err := q.CreateProduct(ctx, store.CreateProductParams{Name: name})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if first.CreatedBy.Valid || !first.Enabled || first.DeletedAt.Valid || first.Description != "" {
			t.Fatalf("defaults: %+v", first)
		}
		if _, err := q.CreateProduct(ctx, store.CreateProductParams{Name: strings.ToLower(name)}); pgCode(err) != "23505" {
			t.Fatalf("case-variant duplicate live name: want unique_violation 23505, got %v", err)
		}
		if n, err := q.SoftDeleteProduct(ctx, first.ID); err != nil || n != 1 {
			t.Fatalf("soft delete: n=%d err=%v", n, err)
		}
		if _, err := q.CreateProduct(ctx, store.CreateProductParams{Name: name}); err != nil {
			t.Fatalf("re-registering a soft-deleted product's name: %v", err)
		}
		if _, err := q.CreateProduct(ctx, store.CreateProductParams{Name: ""}); pgCode(err) != "23514" {
			t.Fatalf("empty product name: want check_violation 23514, got %v", err)
		}
		if _, err := q.CreateProduct(ctx, store.CreateProductParams{Name: "n-" + uuid.NewString(), Description: strings.Repeat("d", 1001)}); pgCode(err) != "23514" {
			t.Fatalf("1001-byte description: want check_violation 23514, got %v", err)
		}
	})
}

// TestProductTokenLifecycleQueriesLiveDB drives the list, revoke, touch and count
// queries: owner scoping, the NULL trap in the active counts, the at-most-once-a-minute
// touch, and the admin inventory's projection.
func TestProductTokenLifecycleQueriesLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, ownerEmail := newProductUser(ctx, t, pool)
	other, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, owner)
	p2 := newProduct(ctx, t, q, owner)

	never := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
	future := mintProductToken(ctx, t, q, owner, p.ID, expiresIn(time.Hour))
	expired := mintProductToken(ctx, t, q, owner, p.ID, expiresIn(time.Hour))
	mustExec(ctx, t, pool, `UPDATE product_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.row.ID)
	revoked := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
	onP2 := mintProductToken(ctx, t, q, owner, p2.ID, neverExpires)
	foreign := mintProductToken(ctx, t, q, other, p.ID, neverExpires)

	if n, err := q.RevokeProductToken(ctx, store.RevokeProductTokenParams{ID: revoked.row.ID, UserID: other}); err != nil || n != 0 {
		t.Fatalf("cross-user revoke must touch 0 rows: n=%d err=%v", n, err)
	}
	if n, err := q.RevokeProductToken(ctx, store.RevokeProductTokenParams{ID: revoked.row.ID, UserID: owner}); err != nil || n != 1 {
		t.Fatalf("owner revoke: n=%d err=%v", n, err)
	}
	if n, err := q.RevokeProductToken(ctx, store.RevokeProductTokenParams{ID: revoked.row.ID, UserID: owner}); err != nil || n != 0 {
		t.Fatalf("repeat revoke: n=%d err=%v, want 0", n, err)
	}

	count, err := q.CountActiveProductTokensForUserProduct(ctx, store.CountActiveProductTokensForUserProductParams{UserID: owner, ProductID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 { // never + future; not expired, not revoked, not onP2, not foreign
		t.Fatalf("active count for (owner, p) = %d, want 2 (NULL-expiry and future-expiry tokens)", count)
	}
	perProduct, err := q.CountActiveProductTokensForProduct(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if perProduct != 3 { // never + future + foreign
		t.Fatalf("active count for p = %d, want 3", perProduct)
	}

	t.Run("list own", func(t *testing.T) {
		rows, err := q.ListProductTokensForUser(ctx, store.ListProductTokensForUserParams{UserID: owner, MaxRows: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, r := range rows {
			ids = append(ids, r.ID)
			if r.ID == foreign.row.ID {
				t.Fatalf("owner list carries another user's token")
			}
			wantName := p.Name
			if r.ProductID == p2.ID {
				wantName = p2.Name
			}
			if r.ProductName != wantName {
				t.Fatalf("row %s product_name = %q, want %q", r.ID, r.ProductName, wantName)
			}
		}
		want := []uuid.UUID{never.row.ID, future.row.ID, expired.row.ID, revoked.row.ID, onP2.row.ID}
		if len(ids) != len(want) {
			t.Fatalf("owner list = %v, want exactly %v (revoked and expired included)", ids, want)
		}
		for _, id := range want {
			if !slices.Contains(ids, id) {
				t.Fatalf("owner list missing %s: %v", id, ids)
			}
		}
		// Active first (the ordering TestProductTokenListsActiveFirstLiveDB pins), then
		// newest first within each group.
		inactive := func(r store.ListProductTokensForUserRow) bool {
			return r.Revoked || (r.ExpiresAt.Valid && !r.ExpiresAt.Time.After(time.Now()))
		}
		for i := 1; i < len(rows); i++ {
			a, b := inactive(rows[i-1]), inactive(rows[i])
			if a && !b {
				t.Fatalf("owner list puts inactive %s ahead of active %s", rows[i-1].ID, rows[i].ID)
			}
			if a == b && rows[i-1].CreatedAt.Time.Before(rows[i].CreatedAt.Time) {
				t.Fatalf("owner list not newest first within a group at %d", i)
			}
		}
	})

	t.Run("touch", func(t *testing.T) {
		if err := q.TouchProductToken(ctx, store.TouchProductTokenParams{ClientIp: "203.0.113.7", ID: never.row.ID}); err != nil {
			t.Fatal(err)
		}
		var at1 pgtype.Timestamptz
		var ip1 *string
		if err := pool.QueryRow(ctx, `SELECT last_used_at, host(last_used_ip) FROM product_tokens WHERE id = $1`, never.row.ID).Scan(&at1, &ip1); err != nil {
			t.Fatal(err)
		}
		if !at1.Valid || ip1 == nil || *ip1 != "203.0.113.7" {
			t.Fatalf("first touch: last_used_at=%v ip=%v", at1, ip1)
		}
		// Within the minute: skipped, so neither column moves.
		if err := q.TouchProductToken(ctx, store.TouchProductTokenParams{ClientIp: "198.51.100.9", ID: never.row.ID}); err != nil {
			t.Fatal(err)
		}
		var ip2 *string
		if err := pool.QueryRow(ctx, `SELECT host(last_used_ip) FROM product_tokens WHERE id = $1`, never.row.ID).Scan(&ip2); err != nil {
			t.Fatal(err)
		}
		if ip2 == nil || *ip2 != "203.0.113.7" {
			t.Fatalf("second touch within a minute moved last_used_ip to %v", ip2)
		}
		// An empty ip becomes NULL rather than an inet cast error.
		if err := q.TouchProductToken(ctx, store.TouchProductTokenParams{ClientIp: "", ID: future.row.ID}); err != nil {
			t.Fatalf("empty ip touch: %v", err)
		}
	})

	t.Run("admin list and admin revoke", func(t *testing.T) {
		rows, err := q.ListAllProductTokensForAdmin(ctx, 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range rows {
			if r.ID == revoked.row.ID {
				found = true
				if r.OwnerEmail != ownerEmail || r.UserID != owner || r.ProductName != p.Name || !r.Revoked {
					t.Fatalf("admin row for the revoked token: %+v", r)
				}
			}
		}
		if !found {
			t.Fatalf("admin inventory must include revoked tokens")
		}
		if n, err := q.AdminRevokeProductToken(ctx, foreign.row.ID); err != nil || n != 1 {
			t.Fatalf("admin revoke: n=%d err=%v", n, err)
		}
		if n, err := q.AdminRevokeProductToken(ctx, foreign.row.ID); err != nil || n != 0 {
			t.Fatalf("repeat admin revoke: n=%d err=%v, want 0", n, err)
		}
		authClosed(ctx, t, q, foreign.hash, "admin-revoked")
	})

	t.Run("revoke all is per user", func(t *testing.T) {
		bystander := mintProductToken(ctx, t, q, other, p.ID, neverExpires)
		if err := q.RevokeAllProductTokens(ctx, owner); err != nil {
			t.Fatal(err)
		}
		for _, m := range []mintedProductToken{never, future, onP2} {
			authClosed(ctx, t, q, m.hash, "revoke-all")
		}
		authOK(ctx, t, q, bystander, p.Name)
		if err := q.RevokeAllProductTokens(ctx, owner); err != nil {
			t.Fatalf("idempotent revoke-all: %v", err)
		}
	})

	t.Run("product lists", func(t *testing.T) {
		disabled := newProduct(ctx, t, q, owner)
		if _, err := q.UpdateProduct(ctx, store.UpdateProductParams{ID: disabled.ID, Description: pgtype.Text{String: "off", Valid: true}, Enabled: pgtype.Bool{Bool: false, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		deleted := newProduct(ctx, t, q, owner)
		if n, err := q.SoftDeleteProduct(ctx, deleted.ID); err != nil || n != 1 {
			t.Fatalf("soft delete: n=%d err=%v", n, err)
		}
		live := newProduct(ctx, t, q, owner)
		mintProductToken(ctx, t, q, owner, live.ID, neverExpires)

		enabled, err := q.ListEnabledProducts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var enabledIDs []uuid.UUID
		for _, e := range enabled {
			enabledIDs = append(enabledIDs, e.ID)
		}
		if !slices.Contains(enabledIDs, live.ID) || slices.Contains(enabledIDs, disabled.ID) || slices.Contains(enabledIDs, deleted.ID) {
			t.Fatalf("ListEnabledProducts must include the live product and exclude disabled and deleted ones")
		}

		all, err := q.ListProducts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[uuid.UUID]int64{}
		for _, r := range all {
			counts[r.ID] = r.ActiveTokenCount
		}
		for _, id := range []uuid.UUID{live.ID, disabled.ID, deleted.ID} {
			if _, ok := counts[id]; !ok {
				t.Fatalf("ListProducts must include every product, deleted ones too (missing %s)", id)
			}
		}
		if counts[live.ID] != 1 || counts[disabled.ID] != 0 {
			t.Fatalf("active_token_count: live=%d disabled=%d, want 1/0", counts[live.ID], counts[disabled.ID])
		}
	})
}

// TestProductTokenMintLockLiveDB proves LockProductTokenMint is a real per-(user,
// product) mutex: while one transaction holds it, a second transaction on the SAME pair
// cannot take it (lock_timeout fires, 55P03), a transaction on a DIFFERENT product can,
// and after the holder commits the same pair is free again.
func TestProductTokenMintLockLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	user, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, user)
	p2 := newProduct(ctx, t, q, user)
	same := store.LockProductTokenMintParams{UserID: user, ProductID: p.ID}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if err := store.New(holder).LockProductTokenMint(ctx, same); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	try := func(params store.LockProductTokenMintParams) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
			t.Fatal(err)
		}
		return store.New(tx).LockProductTokenMint(ctx, params)
	}
	if err := try(same); pgCode(err) != "55P03" {
		t.Fatalf("same (user, product) while held: want lock_not_available 55P03, got %v", err)
	}
	if err := try(store.LockProductTokenMintParams{UserID: user, ProductID: p2.ID}); err != nil {
		t.Fatalf("different product must not contend: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := try(same); err != nil {
		t.Fatalf("same pair after the holder committed: %v", err)
	}
}

// TestUpdateProductConcurrentPatchesLiveDB pins UpdateProduct's COALESCE shape against
// a lost update. Transaction A sets only the description and holds the row; transaction
// B sets only enabled and is proven BLOCKED on A's row lock (pg_stat_activity) before A
// commits. When A commits, B's UPDATE re-evaluates on A's row version, so its COALESCE
// keeps A's description: both writes survive. A read-merge-write (B reading the
// description first, then writing it back) would restore the old description here.
func TestUpdateProductConcurrentPatchesLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	user, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, user)

	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := store.New(txA).UpdateProduct(ctx, store.UpdateProductParams{
		ID: p.ID, Description: pgtype.Text{String: "from A", Valid: true},
	}); err != nil {
		t.Fatalf("A: %v", err)
	}

	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	var pidB int32
	if err := txB.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pidB); err != nil {
		t.Fatal(err)
	}
	bDone := make(chan error, 1)
	go func() {
		_, err := store.New(txB).UpdateProduct(ctx, store.UpdateProductParams{
			ID: p.ID, Enabled: pgtype.Bool{Bool: false, Valid: true},
		})
		bDone <- err
	}()

	// B must be waiting on A's row lock, or this is not the interleaving under test.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`,
			pidB).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-bDone:
			t.Fatalf("B finished (%v) without waiting on A's row lock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("B never blocked on A's row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B: %v", err)
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := q.GetProduct(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "from A" || got.Enabled {
		t.Fatalf("after two concurrent single-field PATCHes: description=%q enabled=%t, want %q and false (both writes kept)",
			got.Description, got.Enabled, "from A")
	}
}

// TestProductTokenListsActiveFirstLiveDB pins the bound on both product-token lists (PRD
// #1907 M4/M5 security audit, H1): ListProductTokensForUser and
// ListAllProductTokensForAdmin order ACTIVE tokens (not revoked, not expired; a NULL
// expiry IS active) ahead of revoked and expired ones, newest first within each group,
// and cut at max_rows, so a small max_rows drops the inactive rows first.
//
// Every inactive fixture row is made NEWER than every active one (created_at set
// explicitly), so a query ordered by created_at alone returns the inactive rows first
// and fails here. The admin list reads the whole shared table, so its case dates this
// test's rows 100 years ahead: they are then the newest rows in the table (a rerun's rows
// are newer still), and the only rows a small max_rows can reach are this test's own.
// The rows are deleted again on cleanup.
func TestProductTokenListsActiveFirstLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProduct(ctx, t, q, owner)
	t.Cleanup(func() {
		mustExec(context.Background(), t, pool, `DELETE FROM product_tokens WHERE user_id = $1`, owner)
	})

	// In created_at order, oldest first: the three active rows, then the three inactive.
	activeNever := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
	activeFuture := mintProductToken(ctx, t, q, owner, p.ID, expiresIn(time.Hour))
	activeNewest := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
	expired := mintProductToken(ctx, t, q, owner, p.ID, expiresIn(time.Hour))
	revokedNever := mintProductToken(ctx, t, q, owner, p.ID, neverExpires)
	revokedNewest := mintProductToken(ctx, t, q, owner, p.ID, expiresIn(time.Hour))
	mustExec(ctx, t, pool, `UPDATE product_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired.row.ID)
	mustExec(ctx, t, pool, `UPDATE product_tokens SET revoked = true WHERE id = ANY($1)`,
		[]uuid.UUID{revokedNever.row.ID, revokedNewest.row.ID})
	ordered := []mintedProductToken{activeNever, activeFuture, activeNewest, expired, revokedNever, revokedNewest}
	setAges := func(base string) {
		t.Helper()
		for i, m := range ordered {
			mustExec(ctx, t, pool,
				`UPDATE product_tokens SET created_at = `+base+` + make_interval(secs => $2) WHERE id = $1`,
				m.row.ID, float64(i))
		}
	}
	wantActive := []uuid.UUID{activeNewest.row.ID, activeFuture.row.ID, activeNever.row.ID}
	const nActive int32 = 3 // len(wantActive), typed for max_rows
	wantInactive := []uuid.UUID{revokedNewest.row.ID, revokedNever.row.ID, expired.row.ID}
	wantAll := append(slices.Clone(wantActive), wantInactive...)

	t.Run("user list", func(t *testing.T) {
		setAges("now() - interval '1 hour'")
		list := func(maxRows int32) []uuid.UUID {
			t.Helper()
			rows, err := q.ListProductTokensForUser(ctx, store.ListProductTokensForUserParams{UserID: owner, MaxRows: maxRows})
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]uuid.UUID, 0, len(rows))
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			return ids
		}
		if got := list(100); !slices.Equal(got, wantAll) {
			t.Fatalf("full user list = %v, want active newest-first %v then inactive newest-first %v", got, wantActive, wantInactive)
		}
		if got := list(nActive); !slices.Equal(got, wantActive) {
			t.Fatalf("user list cut at %d = %v, want exactly the active rows %v (inactive rows cut first)", len(wantActive), got, wantActive)
		}
		if got := list(nActive + 1); !slices.Equal(got, wantAll[:len(wantActive)+1]) {
			t.Fatalf("user list cut at %d = %v, want the active rows then the newest inactive one %v", len(wantActive)+1, got, wantAll[:len(wantActive)+1])
		}
	})

	t.Run("admin list", func(t *testing.T) {
		setAges("now() + interval '100 years'")
		list := func(maxRows int32) []store.ListAllProductTokensForAdminRow {
			t.Helper()
			rows, err := q.ListAllProductTokensForAdmin(ctx, maxRows)
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		// The cut: only the active rows survive a max_rows equal to their count, although
		// every inactive row of this test is newer than all of them.
		cut := list(nActive)
		var got []uuid.UUID
		for _, r := range cut {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, wantActive) {
			t.Fatalf("admin list cut at %d = %v, want exactly this test's active rows %v (inactive rows cut first)", len(wantActive), got, wantActive)
		}
		// Uncut: every active row of this test precedes every inactive one, and each group
		// is newest first. Other tests' rows interleave, so only the relative order of this
		// test's own rows is asserted.
		pos := map[uuid.UUID]int{}
		for i, r := range list(1_000_000) {
			pos[r.ID] = i
		}
		for _, id := range wantAll {
			if _, ok := pos[id]; !ok {
				t.Fatalf("uncut admin list is missing %s", id)
			}
		}
		for i := 1; i < len(wantAll); i++ {
			if pos[wantAll[i-1]] >= pos[wantAll[i]] {
				t.Fatalf("uncut admin list puts %s (position %d) at or after %s (position %d); want order %v",
					wantAll[i-1], pos[wantAll[i-1]], wantAll[i], pos[wantAll[i]], wantAll)
			}
		}
	})
}
