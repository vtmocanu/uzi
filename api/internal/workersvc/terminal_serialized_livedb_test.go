package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3), the admissions that are ALREADY serialized with a retirement by the
// source run's own row lock, not by the consumer gate. Each takes the source run FOR UPDATE (plan and
// code cross-check admission and the code failure record: GetRunOwnedByWorkerForUpdate, which locks
// the lead FOR UPDATE OF lead; the active snapshot: LockFrozenWorkerSnapshotRuns, FOR UPDATE OF r;
// SetRunCompleted: the UPDATE's own row lock), and tx-R1 takes the same run NOWAIT after the gate.
//
// WHAT THESE TESTS DO AND DO NOT PROVE. The real admissions refuse a terminal source whose claim is
// released (the shape a retirement needs), so they cannot be driven end to end against it. Each case
// therefore plays the admission's database footprint from a test transaction: the same row lock on the
// source, then the same committed rows the admission writes (through the store queries the admission
// uses where they can run, with the source briefly reopened inside the same transaction because those
// statements require a live claim, else the equivalent SQL). That pins the retire side of the interleaving:
// a pass that meets the lock does not retire, and the pass after the admission committed keeps the
// ref on the matching blocker. It does not execute the admission's own validation code.
// Nor does it prove the real admission takes that row lock: the test takes its own FOR UPDATE, so the
// lock clauses of GetRunOwnedByWorkerForUpdate and LockFrozenWorkerSnapshotRuns are pinned elsewhere.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// serialSourceHarness pins the source run's harness: a plan cross-check checker must differ from it.
const serialSourceHarness = "claude"

// serialCheckerRun inserts the checker child of a cross-check of lead in tx.
func serialCheckerRun(ctx context.Context, tx pgx.Tx, f *sweepFix, lead uuid.UUID) (uuid.UUID, error) {
	child := uuid.New()
	_, err := tx.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, worker_id, kind, target_run_id, harness, report_only,
		budget_wall_seconds, status, claim_generation, issue_title, issue_description)
		VALUES ($1, $2, $3, $4, 'cross_check', $5, 'codex', true, 1800, 'running', 1, 'check', 'check')`,
		child, f.e.userID, f.e.repoID, f.worker.ID, lead)
	return child, err
}

// serialAsActive runs fn with the source reopened (running, claim held), the state the real code
// cross-check statements require, and restores the terminal shape afterwards, all inside tx. A trigger
// cancels the source's checker runs when the source goes terminal, so the checker (when there is one)
// is put back to running: the footprint under test is a live checker beside a terminal source.
func serialAsActive(ctx context.Context, tx pgx.Tx, f *sweepFix, checker uuid.UUID, fn func() error) error {
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'running', claim_released_at = NULL WHERE id = $1`, f.run); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'completed', claim_released_at = now() WHERE id = $1`, f.run); err != nil {
		return err
	}
	if checker == uuid.Nil {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE runs SET status = 'running', finished_at = NULL WHERE id = $1`, checker)
	return err
}

func TestTerminalSerializedAdmissionsLiveDB(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name string
		// effect is the admission's committed footprint, run in the transaction that holds the source row.
		effect func(ctx context.Context, tx pgx.Tx, f *sweepFix) error
		// reason is the blocker the pass after the commit records; "" means the footprint protects
		// nothing and the pass retires.
		reason string
	}{
		{
			name: "plan cross-check admission",
			effect: func(ctx context.Context, tx pgx.Tx, f *sweepFix) error {
				child, err := serialCheckerRun(ctx, tx, f, f.run)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones,
					required_capabilities, required_tools, size_class, base_commit, planning_diff, candidate_digest,
					checker_run_id, checker_harness, deadline_at)
					VALUES ($1, 'plan', 1, 1, 'plan', '[]', '{}', '{}', 's', $2, '', $3, $4, 'codex', now() + interval '1 hour')`,
					f.run, sha, []byte("digest"), child)
				return err
			},
			// The active checker (target_run_id = the source) is reported before the pending verdict.
			reason: "dependent_run_active",
		},
		{
			name: "code cross-check admission",
			effect: func(ctx context.Context, tx pgx.Tx, f *sweepFix) error {
				child, err := serialCheckerRun(ctx, tx, f, f.run)
				if err != nil {
					return err
				}
				return serialAsActive(ctx, tx, f, child, func() error {
					_, err := store.New(tx).InsertCodeCrossCheck(ctx, store.InsertCodeCrossCheckParams{
						HeadCommit: sha, BaseCommit: strings.Repeat("b", 40), CandidateDigest: []byte("digest"),
						DeadlineAt:  pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
						CodeContext: []byte("{}"), GuidanceText: "", RepoInstructionsText: "",
						CheckerRunID: child, LeadRunID: f.run,
					})
					return err
				})
			},
			reason: "dependent_run_active",
		},
		{
			// A recorded failure is a finished row: no checker run, no pending verdict. It protects
			// nothing, so after the lock the pass retires.
			name: "code cross-check failure record",
			effect: func(ctx context.Context, tx pgx.Tx, f *sweepFix) error {
				return serialAsActive(ctx, tx, f, uuid.Nil, func() error {
					_, err := store.New(tx).InsertFailedCodeCrossCheck(ctx, store.InsertFailedCodeCrossCheckParams{
						ReasonClass: "snapshot_failed", CodeContext: []byte("{}"),
						LeadRunID: f.run, WorkerID: pgtype.UUID{Bytes: f.worker.ID, Valid: true}, UserID: f.e.userID, ClaimGeneration: 1,
					})
					return err
				})
			},
			reason: "",
		},
		{
			name: "active snapshot",
			effect: func(ctx context.Context, tx pgx.Tx, f *sweepFix) error {
				_, err := tx.Exec(ctx, `INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, snapshot_epoch, reported_at)
					VALUES ($1, $2, 1, 'running', false, 1, now())`, f.worker.ID, f.run)
				return err
			},
			reason: "claim_live",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			f := retireFix(t)
			f.e.exec(t, `UPDATE runs SET harness = $2, claim_generation = 1, code_cross_check_required = true WHERE id = $1`, f.run, serialSourceHarness)

			tx, err := f.e.pool.Begin(f.e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			var locked uuid.UUID
			if err = tx.QueryRow(f.e.ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, f.run).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			if err = tc.effect(f.e.ctx, tx, f); err != nil {
				t.Fatalf("admission footprint: %v", err)
			}

			// The admission holds the run: the pass must not retire, whatever the facts say.
			if n := f.sweep(); n != 0 {
				t.Fatalf("pass changed %d rows while the admission held the source run", n)
			}
			p := f.mustPres("live")
			if f.deleteCount() != 0 {
				t.Fatalf("deleted %d remote refs while the admission held the source run", f.deleteCount())
			}
			if strings.HasPrefix(p.LastError.String, "retire kept: consumer_gate_busy") {
				t.Fatalf("last_error = %q: the gate is free here, the run lock is what held the pass", p.LastError.String)
			}

			if err = tx.Commit(f.e.ctx); err != nil {
				t.Fatal(err)
			}
			f.due()
			f.sweep()
			if tc.reason == "" {
				f.mustPres("deleted")
			} else {
				p = f.mustPres("live")
				if want := "retire kept: " + tc.reason; p.LastError.String != want || f.deleteCount() != 0 {
					t.Fatalf("last_error = %q deletes=%d, want %q and no delete", p.LastError.String, f.deleteCount(), want)
				}
			}
			t.Logf("runtime %v", time.Since(start))
		})
	}
}

// SetRunCompleted on the source rewrites its merge request (a plain assignment) and takes the row
// lock with its UPDATE. Two interleavings: it holds the run while a pass runs (the pass backs off),
// and it commits between the pass's lifetime observation and its run lock (the locked run no longer
// has the observed association, so the decision is discarded without a retirement).
func TestTerminalSerializedSetRunCompletedLiveDB(t *testing.T) {
	const mr = int64(905)
	t.Run("holds the run during a pass", func(t *testing.T) {
		start := time.Now()
		f := retireFix(t)
		tx, err := f.e.pool.Begin(f.e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err = tx.Exec(f.e.ctx, `UPDATE runs SET status = 'completed', mr_iid = $2, finished_at = now() WHERE id = $1`, f.run, mr); err != nil {
			t.Fatal(err)
		}
		if n := f.sweep(); n != 0 {
			t.Fatalf("pass changed %d rows while the completion held the source run", n)
		}
		f.mustPres("live")
		if f.deleteCount() != 0 {
			t.Fatalf("deleted %d remote refs while the completion held the source run", f.deleteCount())
		}
		if err = tx.Commit(f.e.ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("runtime %v", time.Since(start))
	})

	t.Run("commits between the observation and the run lock", func(t *testing.T) {
		start := time.Now()
		f := retireFix(t)
		fired := false
		f.svc.terminal.afterRetireGate = func() {
			if fired {
				return
			}
			fired = true
			f.e.exec(t, `UPDATE runs SET status = 'completed', mr_iid = $2, finished_at = now() WHERE id = $1`, f.run, mr)
		}
		if n := f.sweep(); n != 0 {
			t.Fatalf("pass changed %d rows although the association changed under it", n)
		}
		if !fired {
			t.Fatal("the pass never reached the gate hook")
		}
		p := f.mustPres("live")
		if f.deleteCount() != 0 {
			t.Fatalf("deleted %d remote refs on a discarded decision", f.deleteCount())
		}
		if p.Attempts != 0 {
			t.Fatalf("attempts = %d: a discarded decision must not count an attempt", p.Attempts)
		}

		// The next pass observes the new association and decides on it.
		f.due()
		f.sweep()
		f.mustPres("deleted")
		t.Logf("runtime %v", time.Since(start))
	})
}
