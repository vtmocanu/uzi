package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These serial-only wrappers inject transaction failures, not SQL fence
// regressions. QueryRow faults skip the target INSERT; commit faults either
// roll back before committing or lose the acknowledgement after a real commit.
type associationBeginner struct {
	TxBeginner
	failure error
	point   string
	hits    int
}
type associationTx struct {
	pgx.Tx
	b          *associationBeginner
	associated bool
}

func (b *associationBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &associationTx{Tx: tx, b: b}, nil
}

type associationFailureRow struct{ err error }

func (r associationFailureRow) Scan(...any) error { return r.err }

func (tx *associationTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if (tx.b.point == "child" && strings.HasPrefix(sql, "-- name: CreatePlanCrossCheckChild :one")) ||
		(tx.b.point == "check" && strings.HasPrefix(sql, "-- name: InsertPlanCrossCheck :one")) {
		tx.b.hits++
		return associationFailureRow{err: tx.b.failure}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *associationTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.HasPrefix(sql, "-- name: SetPolicyCandidateCrossCheckAssociation :execrows") {
		return tx.Tx.Exec(ctx, sql, args...)
	}
	if tx.b.point == "zero" || tx.b.point == "error" {
		tx.b.hits++
		return pgconn.NewCommandTag("UPDATE 0"), tx.b.failure
	}
	tag, err := tx.Tx.Exec(ctx, sql, args...)
	tx.associated = err == nil && tag.RowsAffected() == 1
	return tag, err
}

func (tx *associationTx) Commit(ctx context.Context) error {
	// Preflight/retry transactions have no association write and commit normally.
	if !tx.associated || (tx.b.point != "commit" && tx.b.point != "lost_ack") {
		return tx.Tx.Commit(ctx)
	}
	tx.b.hits++
	if tx.b.point == "commit" {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		return tx.b.failure
	}
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	return tx.b.failure
}

func TestPolicyCandidateCrossCheckAssociationAtomicLiveDB(t *testing.T) {
	for _, mode := range []string{"ordinary", "preupgrade", "child", "check", "zero", "error", "commit"} {
		t.Run(mode, func(t *testing.T) {
			env, svc, w, lead, candidate := newAutomaticRoundsFixture(t, 2)
			if mode == "ordinary" {
				env.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", lead,
					[]byte(`{"version":1,"gate":null,"primary_revision":null,"associated_cross_checks":[]}`))
			}
			ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
			defer cancel()
			env.ctx = ctx
			before := policyOwnerStopSnapshot(t, env, lead)
			var b *associationBeginner
			injected := errors.New("injected creation transaction failure")
			if mode != "ordinary" && mode != "preupgrade" {
				b = &associationBeginner{TxBeginner: env.pool, point: mode}
				if mode != "zero" {
					b.failure = injected
				}
				svc.SetTxBeginner(b)
			}
			cc, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, candidate)
			if b != nil {
				if b.hits != 1 {
					t.Fatalf("fault injections=%d err=%v", b.hits, err)
				}
				want := ErrCrossCheckRefused
				if mode != "zero" {
					want = injected
				}
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want=%v", err, want)
				}
				if policyOwnerStopSnapshot(t, env, lead) != before {
					t.Fatal("failed creation retained run/check/selection writes")
				}
				var children int
				if err := env.pool.QueryRow(env.ctx, "SELECT count(*) FROM runs WHERE target_run_id=$1", lead).Scan(&children); err != nil || children != 0 {
					t.Fatalf("rolled-back children=%d err=%v", children, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, env, lead)
			if !r.PolicyCandidateCrossCheckID.Valid || uuid.UUID(r.PolicyCandidateCrossCheckID.Bytes) != cc.ID ||
				cc.ID == uuid.UUID(cc.CheckerRunID.Bytes) {
				t.Fatal("selection is not the returned cross_checks row identity")
			}
			// Association writes do not manufacture source coverage.
			if mode == "preupgrade" && len(r.PolicyRevisionSource) != 0 {
				t.Fatal("Submit manufactured V1 absence")
			}
			for _, selected := range []any{nil, uuid.New()} {
				env.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", lead, selected)
				before := policyOwnerStopSnapshot(t, env, lead)
				retry, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate)
				if err != nil || retry.ID != cc.ID {
					t.Fatalf("exact recovery refused: %v", err)
				}
				if policyOwnerStopSnapshot(t, env, lead) != before {
					t.Fatal("exact recovery repaired selection or mutated rows")
				}
			}
		})
	}
}

// Observe actual rows and run one competing Submit only after the initial
// missing-exact-row transaction has committed and released the lead lock.
// All calls are serial and share the test's bounded context.
type associationRecoveryBeginner struct {
	TxBeginner
	afterCommit func(context.Context) error
	hookCalls   int
	hookDone    bool
	exactReads  int
}

type associationRecoveryTx struct {
	pgx.Tx
	b         *associationRecoveryBeginner
	exactMiss bool
}

type associationObservedRow struct {
	pgx.Row
	afterScan func(error)
}

func (r associationObservedRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.afterScan(err)
	return err
}

func (b *associationRecoveryBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &associationRecoveryTx{Tx: tx, b: b}, nil
}

func (tx *associationRecoveryTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if !strings.HasPrefix(sql, "-- name: GetExactPlanCrossCheck :one") {
		return row
	}
	return associationObservedRow{Row: row, afterScan: func(err error) {
		if errors.Is(err, pgx.ErrNoRows) {
			tx.exactMiss = true
		}
		if err == nil && tx.b.hookDone {
			tx.b.exactReads++
		}
	}}
}

func (tx *associationRecoveryTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	if tx.exactMiss && tx.b.afterCommit != nil {
		hook := tx.b.afterCommit
		tx.b.afterCommit = nil // Latch before recursively calling the same Service.
		tx.b.hookCalls++
		if err := hook(ctx); err != nil {
			return err
		}
		tx.b.hookDone = true
	}
	return nil
}

func TestPolicyCandidateCrossCheckCreateRecoveryLiveDB(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "null", true: "mismatched"}[mismatch], func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
			ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
			defer cancel()
			env.ctx = ctx
			var inserted store.CrossCheck
			var baseline, childBaseline string
			b := &associationRecoveryBeginner{TxBeginner: env.pool}
			b.afterCommit = func(ctx context.Context) error {
				var err error
				inserted, err = svc.SubmitPlanCrossCheck(ctx, w, lead, 1, c)
				if err != nil {
					return err
				}
				var selection any
				if mismatch {
					selection = uuid.New()
				}
				if _, err := env.pool.Exec(ctx, "UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", lead, selection); err != nil {
					return err
				}
				baseline = policyOwnerStopSnapshot(t, env, lead)
				childBaseline = policyOwnerStopSnapshot(t, env, uuid.UUID(inserted.CheckerRunID.Bytes))
				return nil
			}
			svc.SetTxBeginner(b)
			recovered, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, c)
			if err != nil || baseline == "" || recovered.ID != inserted.ID ||
				b.hookCalls != 1 || !b.hookDone || b.exactReads != 1 {
				t.Fatalf("create recovery: hooks=%d done=%t exact reads after hook=%d err=%v",
					b.hookCalls, b.hookDone, b.exactReads, err)
			}
			var checks, children int
			if err := env.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
				(SELECT count(*) FROM runs WHERE target_run_id=$1)`, lead).Scan(&checks, &children); err != nil || checks != 1 || children != 1 {
				t.Fatalf("recovery checks=%d children=%d err=%v", checks, children, err)
			}
			if policyOwnerStopSnapshot(t, env, lead) != baseline ||
				policyOwnerStopSnapshot(t, env, uuid.UUID(inserted.CheckerRunID.Bytes)) != childBaseline {
				t.Fatal("create recovery repaired selection or changed prior rows")
			}
		})
	}
}

func TestPolicyCandidateCrossCheckLostCommitACKLiveDB(t *testing.T) {
	env, svc, w, lead, candidate := newAutomaticRoundsFixture(t, 2)
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	env.ctx = ctx
	// Creation may change only the selected check on the lead: in particular,
	// it must not write plan approval or policy source coverage.
	leadWithoutSelection := func() string {
		t.Helper()
		var snapshot string
		if err := env.pool.QueryRow(ctx, `SELECT (to_jsonb(r)-'policy_candidate_cross_check_id')::text
			FROM runs r WHERE id=$1`, lead).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := leadWithoutSelection()
	injected := errors.New("injected lost creation commit acknowledgement")
	b := &associationBeginner{TxBeginner: env.pool, point: "lost_ack", failure: injected}
	svc.SetTxBeginner(b)
	if _, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, candidate); !errors.Is(err, injected) || b.hits != 1 {
		t.Fatalf("lost ACK: hits=%d err=%v", b.hits, err)
	}
	committed, err := env.q.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	r := mustRun(t, env, lead)
	child := mustRun(t, env, uuid.UUID(committed.CheckerRunID.Bytes))
	if !committed.CheckerRunID.Valid || !r.PolicyCandidateCrossCheckID.Valid ||
		uuid.UUID(r.PolicyCandidateCrossCheckID.Bytes) != committed.ID ||
		!child.TargetRunID.Valid || uuid.UUID(child.TargetRunID.Bytes) != lead || child.Kind != "cross_check" {
		t.Fatal("lost ACK did not leave a consistent child/check/association")
	}
	if leadWithoutSelection() != before {
		t.Fatal("creation changed lead fields beyond the selected check")
	}
	baseline := policyOwnerStopSnapshot(t, env, lead)
	childBaseline := policyOwnerStopSnapshot(t, env, child.ID)
	retry, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, candidate)
	if err != nil || retry.ID != committed.ID || b.hits != 1 {
		t.Fatalf("lost ACK exact retry: hits=%d err=%v", b.hits, err)
	}
	if policyOwnerStopSnapshot(t, env, lead) != baseline ||
		policyOwnerStopSnapshot(t, env, child.ID) != childBaseline {
		t.Fatal("lost ACK exact retry mutated committed rows")
	}
	var checks, children int
	if err := env.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
		(SELECT count(*) FROM runs WHERE target_run_id=$1)`, lead).Scan(&checks, &children); err != nil || checks != 1 || children != 1 {
		t.Fatalf("lost ACK checks=%d children=%d err=%v", checks, children, err)
	}
}

func TestPolicyCandidateCrossCheckConcurrentLiveDB(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact_retry", true: "different_candidate"}[conflict], func(t *testing.T) {
			env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
			ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
			defer cancel()
			type result struct {
				cc  store.CrossCheck
				err error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			// Two single attempts share one deadline; collect both before assertions.
			for i := 0; i < 2; i++ {
				candidate := c
				if conflict && i == 1 {
					candidate.PlanMd += " changed"
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					cc, err := svc.SubmitPlanCrossCheck(ctx, w, lead, 1, candidate)
					results <- result{cc, err}
				}()
			}
			close(start)
			wg.Wait()
			first, second := <-results, <-results
			if first.err != nil {
				first, second = second, first
			}
			if first.err != nil {
				t.Fatal(first.err)
			}
			if conflict {
				if !errors.Is(second.err, ErrCrossCheckRefused) {
					t.Fatalf("conflicting request=%v", second.err)
				}
			} else if second.err != nil || first.cc.ID != second.cc.ID {
				t.Fatalf("exact requests diverged: %v", second.err)
			}
			r := mustRun(t, env, lead)
			if !r.PolicyCandidateCrossCheckID.Valid || uuid.UUID(r.PolicyCandidateCrossCheckID.Bytes) != first.cc.ID {
				t.Fatal("selection differs from winning attempt")
			}
			var checks, children int
			if err := env.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
				(SELECT count(*) FROM runs WHERE target_run_id=$1)`, lead).Scan(&checks, &children); err != nil || checks != 1 || children != 1 {
				t.Fatalf("checks=%d children=%d err=%v", checks, children, err)
			}
		})
	}
}
