package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for PRD #4320 M1: the model_providers tables, the anthropic_compatible_key
// secret kind, the users.claude_provider_id source, the runs binding columns and the queries
// over them. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh.

func newProvider(ctx context.Context, t *testing.T, q *store.Queries, owner uuid.UUID, name string) store.ModelProvider {
	t.Helper()
	p, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: owner, Name: name, Engine: "claude"})
	if err != nil {
		t.Fatalf("insert provider %q: %v", name, err)
	}
	addRevision(ctx, t, q, p.ID, 1, "https://gw.example.com/anthropic")
	return p
}

func addRevision(ctx context.Context, t *testing.T, q *store.Queries, id uuid.UUID, rev int32, base string) {
	t.Helper()
	if err := q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
		ProviderID: id, Revision: rev, Protocol: "anthropic_messages", BaseUrl: base, AuthStyle: "bearer",
		ModelMap: []byte(`{"haiku":{"model":"corp-haiku"}}`),
	}); err != nil {
		t.Fatalf("insert revision %d: %v", rev, err)
	}
}

func insertProviderKey(ctx context.Context, q *store.Queries, owner, provider uuid.UUID) (store.InsertProviderKeySecretRow, error) {
	return q.InsertProviderKeySecret(ctx, store.InsertProviderKeySecretParams{
		UserID: owner, Label: provider.String(), Ciphertext: []byte("ct"), SealedWith: "master",
		ProviderID: pgtype.UUID{Bytes: provider, Valid: true},
	})
}

func pgCodeC(err error) (string, string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}

func TestModelProviderCapUnderConcurrencyLiveDB(t *testing.T) {
	ctx, pool, _ := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)

	const attempts = 30
	var created atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := store.New(tx)
			if err := q.LockModelProviderOwner(ctx, owner); err != nil {
				t.Errorf("lock: %v", err)
				return
			}
			n, err := q.CountModelProvidersForOwner(ctx, owner)
			if err != nil {
				t.Errorf("count: %v", err)
				return
			}
			if n >= 20 {
				return
			}
			if _, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{
				OwnerUserID: owner, Name: "p" + uuid.NewString()[:8], Engine: "claude"}); err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			created.Add(1)
		}(i)
	}
	wg.Wait()
	if got := created.Load(); got != 20 {
		t.Fatalf("created %d providers under %d concurrent attempts, want exactly 20", got, attempts)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM model_providers WHERE owner_user_id = $1`, owner).Scan(&n); err != nil || n != 20 {
		t.Fatalf("rows = %d, %v; want 20", n, err)
	}
}

func TestModelProviderPruneKeepsRunReferencedRevisionsLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "prune")
	for rev := int32(2); rev <= 4; rev++ {
		next, err := q.BumpModelProviderRevision(ctx, store.BumpModelProviderRevisionParams{ID: p.ID, OwnerUserID: owner})
		if err != nil || next != rev {
			t.Fatalf("bump = %d, %v; want %d", next, err, rev)
		}
		addRevision(ctx, t, q, p.ID, rev, "https://gw.example.com/anthropic")
	}
	// A run row (any status) bound to revision 2 retains it.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, issue_title, issue_description, kind, job_type,
		        model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
		 VALUES ($1, 'job', 'x', 'job', 'research', $2, 2, 'user_key', $3, 1)`, owner, p.ID, uuid.New())

	n, err := q.PruneModelProviderRevisions(ctx, store.PruneModelProviderRevisionsParams{ProviderID: p.ID, OwnerUserID: owner})
	if err != nil || n != 2 {
		t.Fatalf("prune deleted %d, %v; want 2 (revisions 1 and 3)", n, err)
	}
	rows, err := pool.Query(ctx, `SELECT revision FROM model_provider_revisions WHERE provider_id = $1 ORDER BY revision`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil || !slices.Equal(got, []int32{2, 4}) {
		t.Fatalf("revisions left = %v, %v; want [2 4] (the referenced one and the current one)", got, err)
	}
	// A foreign owner prunes nothing.
	other, _ := newProductUser(ctx, t, pool)
	if n, err := q.PruneModelProviderRevisions(ctx, store.PruneModelProviderRevisionsParams{ProviderID: p.ID, OwnerUserID: other}); err != nil || n != 0 {
		t.Fatalf("foreign prune = %d, %v; want 0", n, err)
	}
}

func TestModelProviderEditWaitsForFreezeLockLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "freeze")

	// Session A: the run-create freeze lock.
	txA, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	locked, err := store.New(txA).LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: p.ID, OwnerUserID: owner})
	if err != nil || locked.CurrentRevision != 1 {
		t.Fatalf("share lock = %+v, %v", locked, err)
	}

	// Session B: the edit's first statement must wait for A.
	done := make(chan error, 1)
	go func() {
		txB, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = txB.Rollback(ctx) }()
		if _, err := store.New(txB).BumpModelProviderRevision(ctx, store.BumpModelProviderRevisionParams{ID: p.ID, OwnerUserID: owner}); err != nil {
			done <- err
			return
		}
		done <- txB.Commit(ctx)
	}()
	select {
	case err := <-done:
		t.Fatalf("the edit finished while the freeze lock was held (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("edit after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the edit never proceeded after the freeze lock was released")
	}
	var cur int32
	if err := pool.QueryRow(ctx, `SELECT current_revision FROM model_providers WHERE id = $1`, p.ID).Scan(&cur); err != nil || cur != 2 {
		t.Fatalf("current_revision = %d, %v; want 2", cur, err)
	}
}

func TestProviderKeyInsertFlagsAndPoolExclusionLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "keyed")

	row, err := insertProviderKey(ctx, q, owner, p.ID)
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	if row.Kind != store.KindAnthropicCompatibleKey || row.IsDefault || row.AutoEligible {
		t.Fatalf("key row = %+v; want kind anthropic_compatible_key, is_default=false, auto_eligible=false", row)
	}
	cands, err := q.ListAutoSelectCandidates(ctx, owner)
	if err != nil || len(cands) != 0 {
		t.Fatalf("auto-select candidates = %v, %v; the provider key must never be one", cands, err)
	}
	// One key per (user, provider).
	if _, err := insertProviderKey(ctx, q, owner, p.ID); err == nil {
		t.Fatal("a second key for the same provider was accepted")
	} else if code, _ := pgCodeC(err); code != "23505" {
		t.Fatalf("second key error = %v, want a unique violation", err)
	}
	// The generic secret mutation path cannot see the key.
	if _, err := q.GetUserSecretForUpdate(ctx, store.GetUserSecretForUpdateParams{ID: row.ID, UserID: owner}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetUserSecretForUpdate on a provider key = %v, want no rows", err)
	}
	meta, err := q.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: owner, ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil || meta.ID != row.ID || meta.DisabledAt.Valid {
		t.Fatalf("provider key meta = %+v, %v", meta, err)
	}
	// Rotation bumps enablement_rev (the frozen-material revision).
	if _, err := q.RotateUserSecret(ctx, store.RotateUserSecretParams{ID: row.ID, UserID: owner, Ciphertext: []byte("ct2"), SealedWith: "master"}); err != nil {
		t.Fatal(err)
	}
	after, err := q.GetProviderKeyForUpdate(ctx, store.GetProviderKeyForUpdateParams{UserID: owner, ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil || after.EnablementRev != meta.EnablementRev+1 {
		t.Fatalf("enablement_rev %d -> %d, %v; want +1", meta.EnablementRev, after.EnablementRev, err)
	}
	// The list joins the key state.
	rows, err := q.ListModelProvidersForOwner(ctx, owner)
	if err != nil || len(rows) != 1 || !rows[0].KeySecretID.Valid || rows[0].KeyDisabledAt.Valid || rows[0].ProbeAuthResult.Valid {
		t.Fatalf("list = %+v, %v", rows, err)
	}
}

func TestProviderKeyCompositeForeignKeyLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	pa := newProvider(ctx, t, q, a, "a-provider")
	if _, err := insertProviderKey(ctx, q, b, pa.ID); err == nil {
		t.Fatal("user B stored a key pointing at user A's provider")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-owner key error = %v, want no owned provider row", err)
	}
	// Direct writes remain protected by the owner-bound FK too.
	_, err := pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id, provider_scope_id)
        VALUES ($1, 'anthropic_compatible_key', 'foreign', 'ct', 'master', $2, $1)`, b, pa.ID)
	if code, c := pgCodeC(err); code != "23503" || c != "user_secrets_provider_fkey" {
		t.Fatalf("cross-owner direct key error = %v (%s %s)", err, code, c)
	}
	// B cannot select A's provider as a source either.
	if _, err := pool.Exec(ctx, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, b, pa.ID); err == nil {
		t.Fatal("user B selected user A's provider as the Claude source")
	} else if code, c := pgCodeC(err); code != "23503" || c != "users_claude_provider_fkey" {
		t.Fatalf("cross-owner source error = %v (%s %s)", err, code, c)
	}
}

func TestUserSecretsProviderKeyChecksLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "checks")

	// kind CHECK pinned, in the style of the job-kind pin.
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'user_secrets_kind_check' AND conrelid = 'user_secrets'::regclass`).Scan(&def); err != nil {
		t.Fatalf("read user_secrets_kind_check: %v", err)
	}
	for _, kind := range []string{"anthropic_token", "openai_api_key", "codex_auth", "anthropic_compatible_key"} {
		if !strings.Contains(def, "'"+kind+"'") {
			t.Errorf("user_secrets_kind_check %q lacks %s", def, kind)
		}
	}
	// A provider key without a provider, and another kind with one, are refused.
	_, err := pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with)
		VALUES ($1, 'anthropic_compatible_key', 'orphan', 'x', 'master')`, owner)
	if code, c := pgCodeC(err); code != "23514" || c != "user_secrets_provider_key_check" {
		t.Errorf("keyless provider-kind insert = %v (%s %s), want user_secrets_provider_key_check", err, code, c)
	}
	_, err = pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id, provider_scope_id)
		VALUES ($1, 'anthropic_token', 'x', 'x', 'master', $2, $1)`, owner, p.ID)
	if code, c := pgCodeC(err); code != "23514" || c != "user_secrets_provider_key_check" {
		t.Errorf("provider_id on an anthropic_token = %v (%s %s), want user_secrets_provider_key_check", err, code, c)
	}
	// auto_eligible stays unreachable for the kind.
	_, err = pool.Exec(ctx, `INSERT INTO user_secrets (user_id, kind, label, ciphertext, sealed_with, provider_id, provider_scope_id, auto_eligible)
		VALUES ($1, 'anthropic_compatible_key', 'ae', 'x', 'master', $2, $1, true)`, owner, p.ID)
	if err == nil {
		t.Error("an auto_eligible provider key was accepted")
	}
}

func TestRunModelProviderBindingChecksLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "bind")
	insert := func(extra string, args ...any) error {
		_, err := pool.Exec(ctx, `INSERT INTO runs (user_id, issue_title, issue_description, kind, job_type`+extra, args...)
		return err
	}
	// Partial binding refused.
	err := insert(`, model_provider_id) VALUES ($1, 'j', 'x', 'job', 'research', $2)`, owner, p.ID)
	if code, c := pgCodeC(err); code != "23514" || c != "runs_model_provider_binding_check" {
		t.Errorf("partial binding = %v (%s %s), want runs_model_provider_binding_check", err, code, c)
	}
	// Unknown key mode refused.
	err = insert(`, model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
		VALUES ($1, 'j', 'x', 'job', 'research', $2, 1, 'operator_key', $3, 1)`, owner, p.ID, uuid.New())
	if code, c := pgCodeC(err); code != "23514" || c != "runs_model_provider_key_mode_check" {
		t.Errorf("unknown key mode = %v (%s %s)", err, code, c)
	}
	// Exclusive with a vendor credential.
	secret := uuid.New()
	err = insert(`, model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev, anthropic_secret_id)
		VALUES ($1, 'j', 'x', 'job', 'research', $2, 1, 'user_key', $3, 1, $4)`, owner, p.ID, uuid.New(), secret)
	if code, c := pgCodeC(err); code != "23514" || c != "runs_model_provider_exclusive_check" {
		t.Errorf("bound run with anthropic_secret_id = %v (%s %s)", err, code, c)
	}
	// A fully bound claude run is accepted, and the streak defaults to zero.
	if err := insert(`, model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
		VALUES ($1, 'j', 'x', 'job', 'research', $2, 1, 'user_key', $3, 7)`, owner, p.ID, uuid.New()); err != nil {
		t.Fatalf("valid binding: %v", err)
	}
	var streak int
	if err := pool.QueryRow(ctx, `SELECT model_provider_limit_streak FROM runs WHERE model_provider_id = $1`, p.ID).Scan(&streak); err != nil || streak != 0 {
		t.Fatalf("streak = %d, %v; want 0", streak, err)
	}
}

func TestModelProviderDeleteAndSourceLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "selected")
	if _, err := insertProviderKey(ctx, q, owner, p.ID); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, owner, p.ID)

	// Deleting a selected provider is refused by ON DELETE RESTRICT and removes nothing.
	_, err := q.DeleteModelProvider(ctx, store.DeleteModelProviderParams{ID: p.ID, OwnerUserID: owner})
	if code, c := pgCodeC(err); code != "23503" || c != "users_claude_provider_fkey" {
		t.Fatalf("delete of a selected provider = %v (%s %s), want users_claude_provider_fkey", err, code, c)
	}
	var keys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_secrets WHERE provider_id = $1`, p.ID).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("keys after the refused delete = %d, %v; want 1", keys, err)
	}
	// Retiring is allowed and keeps the source set.
	if n, err := q.RetireModelProvider(ctx, store.RetireModelProviderParams{ID: p.ID, OwnerUserID: owner}); err != nil || n != 1 {
		t.Fatalf("retire = %d, %v", n, err)
	}
	var src pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT claude_provider_id FROM users WHERE id = $1`, owner).Scan(&src); err != nil || !src.Valid {
		t.Fatalf("source after retire = %+v, %v; want still set", src, err)
	}
	// A foreign owner deletes nothing.
	other, _ := newProductUser(ctx, t, pool)
	if n, err := q.DeleteModelProvider(ctx, store.DeleteModelProviderParams{ID: p.ID, OwnerUserID: other}); err != nil || n != 0 {
		t.Fatalf("foreign delete = %d, %v; want 0", n, err)
	}
	// Once unselected, the delete cascades to the revisions and the key.
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, owner)
	if n, err := q.DeleteModelProvider(ctx, store.DeleteModelProviderParams{ID: p.ID, OwnerUserID: owner}); err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
	var revs int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_provider_revisions WHERE provider_id = $1)
		+ (SELECT count(*) FROM user_secrets WHERE provider_id = $1)`, p.ID).Scan(&revs); err != nil || revs != 0 {
		t.Fatalf("rows left after delete = %d, %v; want 0", revs, err)
	}
}

func TestDeleteUserCascadesWithSelectedProviderLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "doomed")
	if _, err := insertProviderKey(ctx, q, owner, p.ID); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, owner, p.ID)
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner); err != nil {
		t.Fatalf("deleting a user whose selected provider cascades: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_providers WHERE id = $1)
		+ (SELECT count(*) FROM user_secrets WHERE provider_id = $1)`, p.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("provider rows left = %d, %v; want 0", n, err)
	}
}

func TestModelProviderNameUniquePerOwnerLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	a, _ := newProductUser(ctx, t, pool)
	b, _ := newProductUser(ctx, t, pool)
	newProvider(ctx, t, q, a, "same")
	newProvider(ctx, t, q, b, "same") // another owner may reuse the name
	_, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "same", Engine: "claude"})
	if code, c := pgCodeC(err); code != "23505" || c != "model_providers_owner_name_lower_key" {
		t.Fatalf("duplicate name = %v (%s %s)", err, code, c)
	}
	_, err = q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "SAME", Engine: "claude"})
	if code, c := pgCodeC(err); code != "23505" || c != "model_providers_owner_name_lower_key" {
		t.Fatalf("case-variant name = %v (%s %s)", err, code, c)
	}
	if _, err := q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "other", Engine: "codex"}); err != nil {
		t.Fatalf("valid codex provider = %v, want accepted", err)
	}
	// Names are unique per owner across engines, including case variants.
	for _, name := range []string{"same", "SAME"} {
		_, err = q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: name, Engine: "codex"})
		if code, c := pgCodeC(err); code != "23505" || c != "model_providers_owner_name_lower_key" {
			t.Fatalf("cross-engine duplicate %q = %v (%s %s)", name, err, code, c)
		}
	}
	_, err = q.InsertModelProvider(ctx, store.InsertModelProviderParams{OwnerUserID: a, Name: "unknown-engine", Engine: "unknown"})
	if code, _ := pgCodeC(err); code != "23514" {
		t.Fatalf("unknown engine = %v, want a CHECK violation", err)
	}
}

// TestListRunProviderNamesLiveDB: the run-surface name lookup returns the provider's current name
// for a bound run, nothing for a vendor run, nothing for a run whose provider row was deleted
// (runs carry no foreign key to it), and nothing for a run whose owner is not the provider's owner.
func TestListRunProviderNamesLiveDB(t *testing.T) {
	ctx, pool, q := productLiveDB(t)
	owner, _ := newProductUser(ctx, t, pool)
	other, _ := newProductUser(ctx, t, pool)
	p := newProvider(ctx, t, q, owner, "corp gateway")
	gone := newProvider(ctx, t, q, owner, "short lived")

	insertRun := func(user uuid.UUID, provider *uuid.UUID) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		var err error
		if provider == nil {
			err = pool.QueryRow(ctx, `INSERT INTO runs (user_id, issue_title, issue_description, kind, job_type)
				VALUES ($1, 'j', 'x', 'job', 'research') RETURNING id`, user).Scan(&id)
		} else {
			err = pool.QueryRow(ctx, `INSERT INTO runs (user_id, issue_title, issue_description, kind, job_type,
				model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
				VALUES ($1, 'j', 'x', 'job', 'research', $2, 1, 'user_key', $3, 1) RETURNING id`, user, *provider, uuid.New()).Scan(&id)
		}
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}
	vendor := insertRun(owner, nil)
	bound := insertRun(owner, &p.ID)
	orphan := insertRun(owner, &gone.ID)
	foreign := insertRun(other, &p.ID)
	if n, err := q.DeleteModelProvider(ctx, store.DeleteModelProviderParams{ID: gone.ID, OwnerUserID: owner}); err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}

	rows, err := q.ListRunProviderNames(ctx, []uuid.UUID{vendor, bound, orphan, foreign})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != bound || rows[0].Name != "corp gateway" {
		t.Fatalf("rows = %+v, want exactly the bound run named %q", rows, "corp gateway")
	}
}
