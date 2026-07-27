package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestRunAnthropicSecretLiveDB pins the schema half of PRD #111 M1 — migration
// 00086 — against a REAL Postgres. Everything here is invisible to a fake store,
// and the first two are invisible to `sqlc generate` as well: a query that
// generates clean Go can still be rejected by the server the first time it is
// prepared (measured on this repo: SQLSTATE 42P08), so "sqlc regenerated cleanly"
// is not evidence that any of this runs.
//
// Five properties:
//
//  1. SetRunAnthropicSecret EXECUTES, and writes what it claims to.
//
//  2. 🔴 Deleting the recorded token nulls ONLY anthropic_secret_id. This is the
//     one that is silent when wrong. A bare ON DELETE SET NULL on a COMPOSITE FK
//     nulls EVERY referencing column, and runs.user_id is NOT NULL — so with the
//     column list missing, this DELETE would ERROR instead of unbinding, and
//     nothing would notice until a real user deleted a token that happened to have
//     run history. The assertion is deliberately three-part: the run still EXISTS,
//     its user_id is intact, and its LABEL survived. The label surviving is the
//     whole reason the snapshot column exists — it is what keeps "which account
//     paid for this run?" answerable after the credential is gone.
//
//  3. Recording ANOTHER user's credential is refused by the schema, not by a Go
//     check. The composite FK (user_id, anthropic_secret_id) → user_secrets
//     (user_id, id) has no matching pair for a foreign secret.
//
//  4. The write is owner-scoped in its own predicate: a mismatched user affects 0
//     rows rather than writing.
//
//  5. The two metadata reads D8 introduced are owner-scoped, and the by-id one
//     yields pgx.ErrNoRows for a foreign id rather than that user's label — which
//     matters because they run BEFORE the open, so an unscoped read would put
//     another user's label in hand at exactly the point M1 records it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestRunAnthropicSecretLiveDB(t *testing.T) {
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

	owner, stranger := uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{owner, stranger} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("runcred-%s@e2e", u))
	}
	mkSecret := func(user uuid.UUID, label string, isDefault bool) uuid.UUID {
		row, serr := q.InsertUserSecret(ctx, store.InsertUserSecretParams{
			UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
			Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
		})
		if serr != nil {
			t.Fatalf("insert secret %q: %v", label, serr)
		}
		return row.ID
	}
	ownerDefault := mkSecret(owner, "default", true)
	ownerConsole := mkSecret(owner, "console-key", false)
	strangerToken := mkSecret(stranger, "default", true)

	connID, repoID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mkRun := func(iid int64) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'claimed')`, id, owner, repoID, iid)
		return id
	}

	// --- 1. The write executes and records what it was given -----------------
	runID := mkRun(1)
	n, err := q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		ID: runID, UserID: owner,
		AnthropicSecretID:     pgtype.UUID{Bytes: ownerConsole, Valid: true},
		AnthropicSecretLabel:  pgtype.Text{String: "console-key", Valid: true},
		AnthropicSelectReason: pgtype.Text{String: "pinned", Valid: true},
		AnthropicHeadroomPct:  pgtype.Int2{},
	})
	if err != nil {
		t.Fatalf("SetRunAnthropicSecret: %v", err)
	}
	if n != 1 {
		t.Fatalf("SetRunAnthropicSecret affected %d rows, want 1", n)
	}
	readRun := func(id uuid.UUID) store.Run {
		t.Helper()
		r, gerr := q.GetRunByID(ctx, id)
		if gerr != nil {
			t.Fatalf("GetRunByID: %v", gerr)
		}
		return r
	}
	got := readRun(runID)
	if !got.AnthropicSecretID.Valid || uuid.UUID(got.AnthropicSecretID.Bytes) != ownerConsole {
		t.Fatalf("recorded secret id = %+v, want %v", got.AnthropicSecretID, ownerConsole)
	}
	if got.AnthropicSecretLabel.String != "console-key" {
		t.Fatalf("recorded label = %q, want console-key", got.AnthropicSecretLabel.String)
	}
	if got.AnthropicSelectReason.String != "pinned" {
		t.Fatalf("recorded reason = %q, want pinned", got.AnthropicSelectReason.String)
	}
	if got.AnthropicHeadroomPct.Valid {
		t.Fatalf("headroom = %+v, want NULL — M1 has nothing to measure and must not invent a value", got.AnthropicHeadroomPct)
	}

	// --- 2. Deleting the token unbinds the run and KEEPS the label -----------
	// The property the SET NULL column list exists for. Note the DELETE itself is
	// the assertion: without the column list Postgres would try to null the NOT NULL
	// runs.user_id and fail here, so an error is the failure mode, not a wrong value.
	if _, derr := q.DeleteUserSecret(ctx, store.DeleteUserSecretParams{ID: ownerConsole, UserID: owner}); derr != nil {
		t.Fatalf("deleting a token that a run recorded failed: %v\n"+
			"this is what a MISSING `SET NULL (anthropic_secret_id)` column list looks like: the composite FK "+
			"tried to null every referencing column, including runs.user_id (NOT NULL)", derr)
	}
	after := readRun(runID) // must still exist: SET NULL, never CASCADE
	if after.AnthropicSecretID.Valid {
		t.Fatalf("secret id survived the token delete (%+v); the FK did not unbind the run", after.AnthropicSecretID)
	}
	if after.AnthropicSecretLabel.String != "console-key" {
		t.Fatalf("label = %q after the token delete, want the SNAPSHOT console-key to survive — "+
			"the snapshot is the ONLY thing that keeps a finished run's attribution readable once the token is gone",
			after.AnthropicSecretLabel.String)
	}
	if after.UserID != owner {
		t.Fatalf("run.user_id = %v after the token delete, want %v — the SET NULL column list is missing or wrong",
			after.UserID, owner)
	}
	if after.AnthropicSelectReason.String != "pinned" {
		t.Fatalf("reason = %q after the token delete, want it untouched", after.AnthropicSelectReason.String)
	}

	// --- 3. A cross-user record is refused by the SCHEMA ---------------------
	crossRun := mkRun(2)
	_, err = q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		ID: crossRun, UserID: owner,
		AnthropicSecretID:     pgtype.UUID{Bytes: strangerToken, Valid: true},
		AnthropicSecretLabel:  pgtype.Text{String: "not-mine", Valid: true},
		AnthropicSelectReason: pgtype.Text{String: "pinned", Valid: true},
	})
	if err == nil {
		t.Fatal("recording ANOTHER user's credential on a run succeeded — the composite FK is not doing its job, " +
			"and the run view would name a stranger's billing account")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("cross-user record failed for the wrong reason: %v (want a foreign key violation)", err)
	}

	// --- 4. The write is owner-scoped in its own predicate -------------------
	n, err = q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		ID: crossRun, UserID: stranger, // a run that is not this user's
		AnthropicSecretID:     pgtype.UUID{Bytes: strangerToken, Valid: true},
		AnthropicSecretLabel:  pgtype.Text{String: "not-mine", Valid: true},
		AnthropicSelectReason: pgtype.Text{String: "pinned", Valid: true},
	})
	if err != nil {
		t.Fatalf("owner-mismatched write errored instead of matching no rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("owner-mismatched write affected %d rows, want 0", n)
	}

	// --- 5. The two D8 metadata reads, and their owner scoping ---------------
	def, err := q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{
		UserID: owner, Kind: store.KindAnthropicToken,
	})
	if err != nil {
		t.Fatalf("GetDefaultUserSecretMeta: %v", err)
	}
	if def.ID != ownerDefault || def.Label != "default" {
		t.Fatalf("default meta = (%v,%q), want (%v,\"default\")", def.ID, def.Label, ownerDefault)
	}
	// The claim path maps pgx.ErrNoRows here to its existing "no Anthropic token
	// configured for this user" credential failure, so the SENTINEL is part of the
	// contract, not just the absence of a row.
	if _, merr := q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{
		UserID: uuid.New(), Kind: store.KindAnthropicToken,
	}); !errors.Is(merr, pgx.ErrNoRows) {
		t.Fatalf("default meta for a token-less user = %v, want pgx.ErrNoRows", merr)
	}
	byID, err := q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{ID: ownerDefault, UserID: owner})
	if err != nil {
		t.Fatalf("GetUserSecretMetaByID: %v", err)
	}
	if byID.Label != "default" {
		t.Fatalf("by-id meta label = %q, want default", byID.Label)
	}
	if _, merr := q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{
		ID: strangerToken, UserID: owner, // a real secret, the wrong owner
	}); !errors.Is(merr, pgx.ErrNoRows) {
		t.Fatalf("by-id meta for a FOREIGN secret returned %v, want pgx.ErrNoRows — "+
			"this read runs BEFORE the open, so an unscoped one hands the claim another user's label", merr)
	}

	// --- The headroom CHECK, which M4 is the first to exercise for real ------
	// Written now because the constraint ships now: a column that silently accepts
	// 150% headroom would make M5 render a number no gauge can produce.
	hr := mkRun(3)
	if _, herr := q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		ID: hr, UserID: owner,
		AnthropicSecretID:     pgtype.UUID{Bytes: ownerDefault, Valid: true},
		AnthropicSecretLabel:  pgtype.Text{String: "default", Valid: true},
		AnthropicSelectReason: pgtype.Text{String: "auto", Valid: true},
		AnthropicHeadroomPct:  pgtype.Int2{Int16: 100, Valid: true},
	}); herr != nil {
		t.Fatalf("headroom 100 rejected: %v", herr)
	}
	if _, herr := q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		ID: hr, UserID: owner,
		AnthropicSecretID:     pgtype.UUID{Bytes: ownerDefault, Valid: true},
		AnthropicSecretLabel:  pgtype.Text{String: "default", Valid: true},
		AnthropicSelectReason: pgtype.Text{String: "auto", Valid: true},
		AnthropicHeadroomPct:  pgtype.Int2{Int16: 101, Valid: true},
	}); herr == nil {
		t.Fatal("headroom 101 was accepted; the 0..100 CHECK is missing")
	}
}
