package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAnthropicRejectionReplacementFenceLiveDB(t *testing.T) {
	for _, method := range []string{"rotate", "default upsert"} {
		t.Run(method, func(t *testing.T) {
			ctx, pool, q, user := rateLimitEnablementDB(t)
			id := enablementToken(ctx, t, q, user, "default", true)
			mark := func(rev int64) int64 {
				t.Helper()
				n, err := q.MarkAnthropicTokenRejected(ctx, store.MarkAnthropicTokenRejectedParams{UserSecretID: id, UserID: user, EnablementRev: rev, AnthropicSuccessGeneration: 0})
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			if n := mark(0); n != 1 {
				t.Fatalf("mark = %d", n)
			}
			switch method {
			case "rotate":
				if _, err := q.RotateUserSecret(ctx, store.RotateUserSecretParams{ID: id, UserID: user, Ciphertext: []byte("replacement"), SealedWith: store.SealedWithMaster}); err != nil {
					t.Fatal(err)
				}
			case "default upsert":
				if _, err := q.UpsertDefaultUserSecret(ctx, store.UpsertDefaultUserSecretParams{UserID: user, Kind: store.KindAnthropicToken, Ciphertext: []byte("replacement"), SealedWith: store.SealedWithMaster}); err != nil {
					t.Fatal(err)
				}
			}
			var rev int64
			var rejected bool
			if err := pool.QueryRow(ctx, `SELECT enablement_rev, anthropic_rejected_at IS NOT NULL FROM user_secrets WHERE id=$1`, id).Scan(&rev, &rejected); err != nil {
				t.Fatal(err)
			}
			if rev != 1 || rejected {
				t.Fatalf("replacement rev=%d rejected=%v", rev, rejected)
			}
			if n := mark(0); n != 0 {
				t.Fatalf("old poll marked replacement: %d", n)
			}
			if n := fencedUpsert(ctx, t, q, user, id, 0, 40); n != 0 {
				t.Fatalf("old success wrote replacement: %d", n)
			}
			if n := mark(1); n != 1 {
				t.Fatalf("current rejection = %d", n)
			}
			ownerRows, err := q.ListRateLimitsForUser(ctx, user)
			if err != nil || len(ownerRows) != 1 || !ownerRows[0].Rejected {
				t.Fatalf("owner rejection projection: %+v, %v", ownerRows, err)
			}
			adminRows, err := q.ListRateLimits(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foundAdmin := false
			for _, row := range adminRows {
				if row.UserID == user && row.UserSecretID.Valid {
					foundAdmin = row.Rejected
				}
			}
			if !foundAdmin {
				t.Fatal("admin rejection projection missing")
			}
			cands, err := q.ListAutoSelectCandidates(ctx, user)
			if err != nil || len(cands) != 1 || !cands[0].Rejected {
				t.Fatalf("ranker rejection projection: %+v, %v", cands, err)
			}
			if got := autoselect.Classify(autoselectrow.FromCandidateRow(cands[0]), autoselect.Policy{MaxStaleness: time.Hour}, time.Now()).Status; got != autoselect.StatusRejected {
				t.Fatalf("ranker status = %s", got)
			}
			if n := fencedUpsert(ctx, t, q, user, id, 1, 20); n != 1 {
				t.Fatalf("current success = %d", n)
			}
			if err := pool.QueryRow(ctx, `SELECT anthropic_rejected_at IS NOT NULL FROM user_secrets WHERE id=$1`, id).Scan(&rejected); err != nil {
				t.Fatal(err)
			}
			if rejected {
				t.Fatal("success did not clear rejection")
			}
		})
	}
}

func TestAnthropicSameRevisionRejectionSuccessRaceLiveDB(t *testing.T) {
	for _, successFirst := range []bool{true, false} {
		name := "rejection waits for success"
		if !successFirst {
			name = "success waits for rejection"
		}
		t.Run(name, func(t *testing.T) {
			ctx, pool, q, user := rateLimitEnablementDB(t)
			id := enablementToken(ctx, t, q, user, "default", true)

			mark := func(queries *store.Queries) (int64, error) {
				return queries.MarkAnthropicTokenRejected(ctx, store.MarkAnthropicTokenRejectedParams{
					UserSecretID: id, UserID: user, EnablementRev: 0,
					AnthropicSuccessGeneration: 0,
				})
			}
			success := func(queries *store.Queries) (int64, error) {
				return queries.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
					UserSecretID: id, UserID: user, EnablementRev: 0,
					FiveHourPct: pgtype.Int2{Int16: 40, Valid: true},
					Source:      pgtype.Text{String: "usage_endpoint", Valid: true},
					SyncedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
				})
			}
			first, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Release()
			second, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Release()
			tx, err := first.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			firstWrite, secondWrite := mark, success
			if successFirst {
				firstWrite, secondWrite = success, mark
			}
			if n, err := firstWrite(q.WithTx(tx)); err != nil || n != 1 {
				t.Fatalf("first write rows=%d err=%v", n, err)
			}
			finished := make(chan rejectionRaceResult, 1)
			go func() {
				n, err := secondWrite(store.New(second))
				finished <- rejectionRaceResult{n, err}
			}()
			waitForRejectionRaceLock(t, ctx, pool, second.Conn().PgConn().PID(), finished)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-finished:
				want := int64(1)
				if successFirst {
					want = 0
				}
				if got.err != nil || got.rows != want {
					t.Fatalf("second write rows=%d err=%v, want %d", got.rows, got.err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("second write did not finish")
			}
			var rejected bool
			var generation int64
			if err := pool.QueryRow(ctx, `SELECT anthropic_rejected_at IS NOT NULL,
				anthropic_success_generation FROM user_secrets WHERE id=$1`, id).
				Scan(&rejected, &generation); err != nil {
				t.Fatal(err)
			}
			if rejected || generation != 1 {
				t.Fatalf("same-revision final state rejected=%v generation=%d", rejected, generation)
			}
		})
	}
}

type rejectionRaceResult struct {
	rows int64
	err  error
}

func TestAnthropicRejectionRotationRaceLiveDB(t *testing.T) {
	for _, rotateFirst := range []bool{true, false} {
		name := "poll commits first"
		if rotateFirst {
			name = "rotation commits first"
		}
		t.Run(name, func(t *testing.T) {
			ctx, pool, q, user := rateLimitEnablementDB(t)
			id := enablementToken(ctx, t, q, user, "default", true)
			if n := fencedUpsert(ctx, t, q, user, id, 0, 30); n != 1 {
				t.Fatalf("initial gauge rows = %d", n)
			}
			if n, err := q.MarkAnthropicTokenRejected(ctx, store.MarkAnthropicTokenRejectedParams{UserSecretID: id, UserID: user, EnablementRev: 0, AnthropicSuccessGeneration: 1}); err != nil || n != 1 {
				t.Fatalf("initial rejection rows = %d, %v", n, err)
			}
			first, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Release()
			second, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Release()
			tx, err := first.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			rotate := func(queries *store.Queries) error {
				_, err := queries.RotateUserSecret(ctx, store.RotateUserSecretParams{ID: id, UserID: user, Ciphertext: []byte("replacement"), SealedWith: store.SealedWithMaster})
				return err
			}
			upsert := func(queries *store.Queries) (int64, error) {
				return queries.UpsertRateLimits(ctx, store.UpsertRateLimitsParams{
					UserSecretID: id, UserID: user, EnablementRev: 0,
					FiveHourPct: pgtype.Int2{Int16: 40, Valid: true},
					SevenDayPct: pgtype.Int2{Int16: 40, Valid: true},
					Source:      pgtype.Text{String: "usage_endpoint", Valid: true},
					SyncedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
				})
			}
			if rotateFirst {
				if err := rotate(q.WithTx(tx)); err != nil {
					t.Fatal(err)
				}
			} else if n, err := upsert(q.WithTx(tx)); err != nil || n != 1 {
				t.Fatalf("first upsert rows = %d, %v", n, err)
			}
			pid := second.Conn().PgConn().PID()
			finished := make(chan rejectionRaceResult, 1)
			go func() {
				if rotateFirst {
					n, err := upsert(store.New(second))
					finished <- rejectionRaceResult{n, err}
				} else {
					finished <- rejectionRaceResult{err: rotate(store.New(second))}
				}
			}()
			waitForRejectionRaceLock(t, ctx, pool, pid, finished)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-finished:
				if got.err != nil || (rotateFirst && got.rows != 0) {
					t.Fatalf("second statement rows = %d, err = %v", got.rows, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("second statement did not finish after commit")
			}
			var rev int64
			var rejected bool
			var pct int16
			if err := pool.QueryRow(ctx, `SELECT s.enablement_rev, s.anthropic_rejected_at IS NOT NULL, rl.five_hour_pct FROM user_secrets s JOIN anthropic_rate_limits rl ON rl.user_secret_id=s.id WHERE s.id=$1`, id).Scan(&rev, &rejected, &pct); err != nil {
				t.Fatal(err)
			}
			wantPct := int16(40)
			if rotateFirst {
				wantPct = 30
			}
			if rev != 1 || rejected || pct != wantPct {
				t.Fatalf("after commits: rev=%d rejected=%v gauge=%d, want rev=1 rejected=false gauge=%d", rev, rejected, pct, wantPct)
			}
		})
	}
}

func waitForRejectionRaceLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid uint32, finished <-chan rejectionRaceResult) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err == nil && waiting {
			return
		}
		select {
		case got := <-finished:
			t.Fatalf("second statement completed before first commit: %+v", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("second connection never waited for secret row lock")
}
