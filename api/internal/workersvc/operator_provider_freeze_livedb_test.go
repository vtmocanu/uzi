package workersvc

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/operatorkey"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// operator_provider_freeze_livedb_test.go is PRD #4540 M1's freeze fence: an organization-funded
// definition forced as a user's Claude source (by SQL, since no route can select it) reads as missing
// to the freeze, which is the only thing that keeps a run from binding it. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// queryRecord is one traced statement: its SQL text, the command tag it ended with and its error.
type queryRecord struct {
	sql, tag string
	err      error
}

// queryTrace is a pgx.QueryTracer that records every statement of the pool it is installed on.
type queryTrace struct {
	mu      sync.Mutex
	records []queryRecord
}

type queryTraceKey struct{}

func (q *queryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryTraceKey{}, data.SQL)
}

func (q *queryTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(queryTraceKey{}).(string)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.records = append(q.records, queryRecord{sql: sql, tag: data.CommandTag.String(), err: data.Err})
}

// named returns the records of the statement whose SQL starts with marker (a sqlc "-- name: X :one").
func (q *queryTrace) named(marker string) []queryRecord {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queryRecord
	for _, r := range q.records {
		if strings.HasPrefix(r.sql, marker) {
			out = append(out, r)
		}
	}
	return out
}

func (q *queryTrace) anyMentions(needle string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.records {
		if strings.Contains(r.sql, needle) {
			return true
		}
	}
	return false
}

// seedOperatorDefinition creates an organization-funded definition with one revision and a sealed key,
// and registers its cleanup (selections cleared first, since a selected definition cannot be deleted).
func seedOperatorDefinition(t *testing.T, env codexTestEnv) uuid.UUID {
	t.Helper()
	p, err := env.q.InsertSharedOperatorModelProvider(env.ctx, "operator-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("insert operator definition: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE claude_provider_id = $1`, p.ID); err != nil {
			t.Errorf("clear forced selections in cleanup: %v", err)
		}
		if _, err := env.pool.Exec(ctx, `DELETE FROM model_providers WHERE id = $1`, p.ID); err != nil {
			t.Errorf("delete operator definition in cleanup: %v", err)
		}
	})
	if err := env.q.InsertModelProviderRevision(env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(pvDefaultMap),
	}); err != nil {
		t.Fatalf("insert operator revision: %v", err)
	}
	sealed, err := operatorkey.Seal(env.box, p.ID, []byte("org-gw-key-"+uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.q.InsertOperatorProviderKey(env.ctx, store.InsertOperatorProviderKeyParams{ProviderID: p.ID, Ciphertext: sealed, SetBy: uuid.New()}); err != nil {
		t.Fatalf("insert operator custody row: %v", err)
	}
	return p.ID
}

// TestOperatorSelectionFreezeFenceLiveDB: with a user's claude_provider_id and claude_provider_scope_id
// forced (in SQL) to an organization-funded definition that has a custody row, freezeProviderSource
// refuses it as a missing provider. The error alone does not prove the fence: a widened
// LockModelProviderForShare would still produce provider_missing from the re-read, which keeps its
// per-user-key join. The oracle is the statement trace on a test-owned pool: the share lock ended with
// no row (SELECT 0) and the source read ran exactly once, so the in-lock re-read never ran.
func TestOperatorSelectionFreezeFenceLiveDB(t *testing.T) {
	pf := newPCFix(t)
	op := seedOperatorDefinition(t, pf.env)
	pf.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = '`+sharedScopeID+`' WHERE id = $1`, pf.userID, op)

	trace := &queryTrace{}
	cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(pf.env.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(pf.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(pf.env.ctx) }()

	_, err = pf.svc.freezeProviderSource(pf.env.ctx, tx, store.New(tx), pf.userID)
	var unusable *ProviderSourceUnusableError
	if !errors.As(err, &unusable) || unusable.Reason != SourceUnusableProviderMissing || unusable.ProviderID != op {
		t.Fatalf("freezeProviderSource on a forced operator selection = %v, want provider_missing for %s", err, op)
	}

	lock := trace.named("-- name: LockModelProviderForShare :one")
	if len(lock) != 1 || lock[0].tag != "SELECT 0" {
		t.Fatalf("LockModelProviderForShare trace = %+v, want exactly one statement that ended SELECT 0", lock)
	}
	if reads := trace.named("-- name: GetUserProviderSource :one"); len(reads) != 1 {
		t.Fatalf("GetUserProviderSource ran %d times, want exactly 1 (the in-lock re-read must never run)", len(reads))
	}
	if trace.anyMentions("model_provider_operator_keys") {
		t.Fatal("the freeze read the organization key custody table")
	}
}
