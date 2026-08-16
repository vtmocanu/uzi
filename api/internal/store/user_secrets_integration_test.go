package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserSecretsRewrapLiveDB is the PRD #104 D10 regression gate at the SQL layer:
// the vault's lazy rewrap must update exactly the row it opened, never every row of
// the same kind.
//
// Before the fix, RewrapUserSecret's predicate was
// `WHERE user_id = $2 AND kind = $3 AND sealed_with = 'master'` and
// ListMasterSealedSecrets returned no id, so the rewrap loop's first iteration
// matched EVERY master-sealed row of the kind and overwrote the siblings with the
// first row's resealed bytes; the later iterations then matched nothing and the loop
// completed without an error. The failure mode is silent data loss, which is why the
// gate lives here (real SQL, real set-based UPDATE) and not only against the vault's
// fake store: a fake can be written to match one row per call and would go green
// against the broken statement.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserSecretsRewrapLiveDB(t *testing.T) {
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

	// Two users: the owner holds two master-sealed tokens of one kind; the bystander
	// holds one, to prove the rewrap is scoped to its owner as well as to its row.
	owner, bystander := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, bystander} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("secrets-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, ct []byte) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
			 VALUES ($1, $2, $3, $4, $5, $6, 'master')`,
			id, user, store.KindAnthropicToken, label, label == "default", ct)
		return id
	}
	first := mkSecret(owner, "default", []byte("sealed-token-one"))
	second := mkSecret(owner, "console-key", []byte("sealed-token-two"))
	foreign := mkSecret(bystander, "default", []byte("sealed-token-bystander"))

	// The listing must carry the id — without it the loop below cannot address a row.
	rows, err := q.ListMasterSealedSecrets(ctx, owner)
	if err != nil {
		t.Fatalf("list master-sealed: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListMasterSealedSecrets returned %d rows, want 2 (both of the owner's tokens)", len(rows))
	}
	listed := map[uuid.UUID][]byte{}
	for _, r := range rows {
		listed[r.ID] = r.Ciphertext
	}
	if string(listed[first]) != "sealed-token-one" || string(listed[second]) != "sealed-token-two" {
		t.Fatalf("listing did not pair each id with its own ciphertext: %v", listed)
	}

	// Rewrap each row exactly as vault.rewrapMasterSecrets does: one statement per
	// listed row, carrying that row's id. Ordered, not a map, so a failure names the
	// same row every run.
	for _, tc := range []struct {
		id   uuid.UUID
		want []byte
	}{
		{first, []byte("rewrapped-token-one")},
		{second, []byte("rewrapped-token-two")},
	} {
		n, rerr := q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
			ID: tc.id, UserID: owner, Ciphertext: tc.want,
		})
		if rerr != nil {
			t.Fatalf("rewrap %s: %v", tc.id, rerr)
		}
		// Errorf, NOT Fatalf, and deliberately so: an over-matching predicate is the
		// CAUSE, and the read-back below is the DAMAGE. Aborting here would stop the
		// test at "affected 2 rows" and leave a reader to infer that a sibling was
		// destroyed; continuing makes the failure output show the destroyed plaintext
		// itself. The row count stays as the diagnostic that explains why.
		if n != 1 {
			t.Errorf("rewrap %s affected %d rows, want exactly 1 — the predicate is matching siblings", tc.id, n)
		}
	}

	readBack := func(id uuid.UUID) (ct []byte, sealedWith string) {
		if err := pool.QueryRow(ctx, `SELECT ciphertext, sealed_with FROM user_secrets WHERE id = $1`, id).
			Scan(&ct, &sealedWith); err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		return ct, sealedWith
	}
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		ct   string
		sw   string
	}{
		{"first", first, "rewrapped-token-one", store.SealedWithDEK},
		{"second", second, "rewrapped-token-two", store.SealedWithDEK},
		{"bystander", foreign, "sealed-token-bystander", store.SealedWithMaster},
	} {
		ct, sw := readBack(tc.id)
		if string(ct) != tc.ct {
			t.Errorf("%s row ciphertext = %q, want %q — a sibling's rewrap overwrote it", tc.name, ct, tc.ct)
		}
		if sw != tc.sw {
			t.Errorf("%s row sealed_with = %q, want %q", tc.name, sw, tc.sw)
		}
	}

	// The sealed_with guard is the double-rewrap protection PRD #32 relies on: a
	// second unlock racing the first must not clobber a now-'dek' row with stale
	// master-era bytes.
	n, err := q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
		ID: first, UserID: owner, Ciphertext: []byte("stale-bytes"),
	})
	if err != nil {
		t.Fatalf("double rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("re-rewrapping a 'dek' row affected %d rows, want 0 (the sealed_with guard is gone)", n)
	}

	// Defensive scope: another user's id is a no-op, not a cross-user write.
	n, err = q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
		ID: foreign, UserID: owner, Ciphertext: []byte("cross-user-bytes"),
	})
	if err != nil {
		t.Fatalf("cross-user rewrap: %v", err)
	}
	if n != 0 {
		t.Fatalf("rewrapping another user's secret affected %d rows, want 0", n)
	}
}

// TestUserSecretsDefaultResolutionLiveDB pins the invariant that makes every
// existing "does this user have a token?" gate keep telling the truth after 00077,
// and proves the two indexes it adds actually bite. Real SQL, because all three
// claims are properties of the schema and of the ON CONFLICT arbiter, none of which
// a fake store has.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestUserSecretsDefaultResolutionLiveDB(t *testing.T) {
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

	// --- A user's FIRST token must be born default, or it is invisible ---
	//
	// The existence gates (UserHasAnthropicToken here, plus autopilot's
	// has_anthropic_token and the two judge gates) are EXISTS queries with no
	// is_default filter, while every resolution path is by-kind AND is_default. A
	// create path that leaves is_default false therefore produces a token the UI
	// reports as "Set" and the gates green-light runs against, while every open
	// returns ErrNoSecret — a silent failure with no error anywhere. The migration's
	// backfill cannot cover this: it only touched rows that existed at migration
	// time, and the alias route and the seed keep creating rows afterwards.
	fresh := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		fresh, fmt.Sprintf("first-token-%s@e2e", fresh))

	created, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{
		UserID: fresh, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("first-token"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("create first token: %v", err)
	}
	if !created.IsDefault || created.Label != store.LabelDefaultSecret {
		t.Fatalf("first token created as (label=%q, is_default=%v), want (%q, true)",
			created.Label, created.IsDefault, store.LabelDefaultSecret)
	}

	// The two halves must agree: what the gates see exists, the read path resolves.
	exists, err := q.UserHasAnthropicToken(ctx, fresh)
	if err != nil {
		t.Fatalf("existence gate: %v", err)
	}
	got, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: fresh, Kind: store.KindAnthropicToken,
	})
	if err != nil {
		t.Fatalf("the existence gate says the user has a token (%v) but the read path cannot resolve it: %v — "+
			"every run for this user would fail on credential-unavailable with nothing logged as wrong", exists, err)
	}
	if string(got.Ciphertext) != "first-token" {
		t.Fatalf("resolved ciphertext = %q, want first-token", got.Ciphertext)
	}

	// Rotating through the alias hits the arbiter, keeps ONE row, and keeps it the
	// default — it must not mint a second credential per save.
	rotated, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{
		UserID: fresh, Kind: store.KindAnthropicToken,
		Ciphertext: []byte("rotated-token"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("rotate via the alias: %v", err)
	}
	if rotated.ID != created.ID {
		t.Fatalf("rotation created a NEW row (%s vs %s) instead of updating the default", rotated.ID, created.ID)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_secrets WHERE user_id = $1`, fresh).Scan(&n); err != nil {
		t.Fatalf("count after rotate: %v", err)
	}
	if n != 1 {
		t.Fatalf("user holds %d secrets after one create + one rotate, want 1", n)
	}

	// --- A first token is forced default even when the caller asks otherwise ---
	//
	// @want_default is a request, not a guarantee: InsertUserSecret ORs it with
	// "this user holds no row of this kind". The invariant stops depending on every
	// call site being right, which matters because M2 adds a create route whose flag
	// comes from a REQUEST BODY. Scoped to (user_id, kind), so a user's first token
	// of a NEW kind is also forced — user-scoping would resurrect the same hazard
	// one kind over the day D9's openai_token arrives.
	forced := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		forced, fmt.Sprintf("forced-%s@e2e", forced))
	first, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: forced, Kind: store.KindAnthropicToken, Label: "not-called-default",
		WantDefault: false, // deliberately wrong
		Ciphertext:  []byte("ct"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert first token: %v", err)
	}
	if !first.IsDefault {
		t.Fatal("a user's FIRST token was created non-default — it is invisible: the existence gates " +
			"report it present while every read path resolves ErrNoSecret")
	}
	// And it really is resolvable, which is the property that was at risk.
	if _, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: forced, Kind: store.KindAnthropicToken,
	}); err != nil {
		t.Fatalf("the forced-default first token does not resolve: %v", err)
	}
	// A SECOND token still obeys the caller: forcing only ever applies to the first.
	second, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
		UserID: forced, Kind: store.KindAnthropicToken, Label: "second",
		WantDefault: false, Ciphertext: []byte("ct2"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert second token: %v", err)
	}
	if second.IsDefault {
		t.Fatal("a second token was forced default — the subquery is not scoped to existing rows, " +
			"and M2 would lose control of set-default")
	}

	// --- RotateUserSecret: id-targeted, owner-scoped, leaves label/default alone ---
	//
	// It has no non-generated caller until M2, which is exactly why it is exercised
	// here: an unexercised query is one M2 would debug rather than use.
	rot, err := q.RotateUserSecret(ctx, store.RotateUserSecretParams{
		ID: second.ID, UserID: forced,
		Ciphertext: []byte("rotated-value"), SealedWith: store.SealedWithDEK,
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rot.ID != second.ID {
		t.Fatalf("rotate returned %v, want the row it was asked for (%v)", rot.ID, second.ID)
	}
	// Rotating a VALUE must not silently rename the credential or move the default —
	// they are separate operations on purpose (M2 exposes them separately).
	if rot.Label != "second" || rot.IsDefault {
		t.Fatalf("rotate changed label/default: label=%q is_default=%v, want (\"second\", false)",
			rot.Label, rot.IsDefault)
	}
	var rotCT []byte
	var rotSealed string
	if err := pool.QueryRow(ctx, `SELECT ciphertext, sealed_with FROM user_secrets WHERE id = $1`, second.ID).
		Scan(&rotCT, &rotSealed); err != nil {
		t.Fatalf("read back rotated: %v", err)
	}
	if string(rotCT) != "rotated-value" || rotSealed != store.SealedWithDEK {
		t.Fatalf("rotated row = (%q,%q), want (rotated-value,%s)", rotCT, rotSealed, store.SealedWithDEK)
	}
	// The sibling is untouched — rotate is single-row, unlike the pre-D10 write paths.
	var firstCT []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM user_secrets WHERE id = $1`, first.ID).Scan(&firstCT); err != nil {
		t.Fatalf("read back sibling: %v", err)
	}
	if string(firstCT) != "ct" {
		t.Fatalf("rotate touched a sibling row: %q", firstCT)
	}
	// A foreign owner rotates nothing (pgx.ErrNoRows from :one), never another
	// user's credential.
	if _, err := q.RotateUserSecret(ctx, store.RotateUserSecretParams{
		ID: second.ID, UserID: uuid.New(),
		Ciphertext: []byte("stolen"), SealedWith: store.SealedWithMaster,
	}); err == nil {
		t.Fatal("rotating another user's secret must affect no rows")
	}

	// --- D11: the by-id read is owner-scoped IN SQL ---
	//
	// The Go-side re-check in secretopen.OpenByID and M3's composite FK are the other
	// two layers; this is the one nothing else covers, because a fake store cannot
	// observe a WHERE clause. An edit that kept both parameters but weakened the
	// predicate would compile and pass every other test.
	if _, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
		ID: first.ID, UserID: fresh, // fresh owns a token, but not THIS one
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("by-id read of another user's secret returned %v, want pgx.ErrNoRows — "+
			"the predicate is no longer owner-scoped", err)
	}
	// The owner still reads it, so the scoping is not just breaking the query.
	if _, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
		ID: first.ID, UserID: forced,
	}); err != nil {
		t.Fatalf("the owner's own by-id read failed: %v", err)
	}

	// --- The indexes 00077 adds must actually bite ---
	insertSecond := func(label string, isDefault bool) error {
		_, err := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: fresh, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("second"), SealedWith: store.SealedWithMaster,
		})
		return err
	}
	if err := insertSecond("DEFAULT", false); err == nil {
		t.Fatal("a label differing only by case was accepted — the lower(label) unique index is not enforcing (D7)")
	}
	if err := insertSecond("console-key", true); err == nil {
		t.Fatal("a second is_default row was accepted — the partial unique index is not enforcing (D2)")
	}
	// The same second token, correctly non-default, is allowed — the constraint is
	// on defaults and label collisions, not on holding several credentials.
	if err := insertSecond("console-key", false); err != nil {
		t.Fatalf("a second non-default token must be storable, got: %v", err)
	}
}
