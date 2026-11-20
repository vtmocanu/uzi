package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Issue #4476: Codex claim provenance through the real ClaimRun and the guard trigger. Against
// a REAL Postgres; skipped unless UZI_TEST_DATABASE_URL points at a throwaway one. Serial: the
// shared fixture helpers must not run in parallel.
func TestClaimRunCodexProvenanceSeamLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("prov-a", 0, false)
	b := fx.worker("prov-b", 0, false)

	prov := func(run uuid.UUID) (w *uuid.UUID, g *int64) {
		t.Helper()
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT claim_provenance_worker_id, claim_provenance_generation FROM runs WHERE id = $1`, run).Scan(&w, &g); err != nil {
			t.Fatalf("read provenance: %v", err)
		}
		return w, g
	}
	wantProv := func(run, worker uuid.UUID, gen int64, why string) {
		t.Helper()
		w, g := prov(run)
		if w == nil || *w != worker || g == nil || *g != gen {
			t.Fatalf("%s: provenance=(%v,%v), want (%s,%d)", why, w, g, worker, gen)
		}
	}

	// (a) a Codex run at generation 0 claimed by A records (A, 1).
	codex := uuid.New()
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex')`, codex, fx.userID, fx.repoID, fx.nextIID())
	fx.mustClaim(a, codex, 0, staleCeiling, "first Codex claim")
	wantProv(codex, a, 1, "after the first Codex claim")

	// (b) a Claude run claimed leaves both columns NULL.
	claude := uuid.New()
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, claude, fx.userID, fx.repoID, fx.nextIID())
	fx.mustClaim(b, claude, 0, staleCeiling, "Claude claim")
	if w, g := prov(claude); w != nil || g != nil {
		t.Fatalf("Claude run provenance=(%v,%v), want NULL", w, g)
	}
	// Retire the Claude run so it cannot be handed to a later claim in this test.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, claude)

	// (c) requeue the Codex run; B is refused, A re-claims with (A, 2).
	mustExec(fx.ctx, t, fx.pool,
		`UPDATE runs SET status = 'queued', status_since = now() - interval '1 hour', updated_at = now() - interval '1 hour' WHERE id = $1`, codex)
	if _, err := fx.staleClaim(b, 0, staleCeiling); err != pgx.ErrNoRows {
		t.Fatalf("worker B claiming a Codex run recorded for A: got %v, want pgx.ErrNoRows", err)
	}
	fx.mustClaim(a, codex, 0, staleCeiling, "recorded claimant re-claim")
	wantProv(codex, a, 2, "after the recorded claimant's re-claim")

	// The guard: a direct write of provenance outside a +1 claim is refused.
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET claim_provenance_worker_id = $2 WHERE id = $1`, codex, b); !isProvenanceGuardErr(err) {
		t.Fatalf("direct provenance rewrite: got %v, want the guard trigger's check_violation", err)
	}

	// The guard fails closed on a NULL conjunct: a claim-shaped 0 -> 1 step that records A while
	// leaving worker_id NULL must raise (an IF NOT (...) on a NULL conjunction would not).
	fresh := uuid.New()
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, claim_generation)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 0)`, fresh, fx.userID, fx.repoID, fx.nextIID())
	if _, err := fx.pool.Exec(fx.ctx,
		`UPDATE runs SET claim_generation = 1, claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`,
		fresh, a); !isProvenanceGuardErr(err) {
		t.Fatalf("provenance stamped with a NULL worker_id: got %v, want the guard trigger's check_violation", err)
	}
}

// isProvenanceGuardErr reports the guard trigger's own refusal: SQLSTATE 23514 with its message,
// so an unrelated constraint can never stand in for it.
func isProvenanceGuardErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		strings.HasPrefix(pgErr.Message, "claim provenance may change only through an admitted claim")
}
