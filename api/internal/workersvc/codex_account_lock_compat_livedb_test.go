package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each probe uses a different transaction and an actual run_messages FK check.
// Rollback keeps its high sequence number out of the re-admission feed.
func codexLockFKProbe(t *testing.T, env codexTestEnv, id uuid.UUID, blocked bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
	defer cancel()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("probe rollback: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '300ms'"); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO run_messages(run_id,seq,kind,payload) VALUES($1,2000000000,'status','{"text":"FK lock probe"}')`, id)
	if blocked {
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
			t.Fatalf("FOR UPDATE control FK insert: %v, want lock timeout", err)
		}
	} else if err != nil {
		t.Fatalf("FK insert must remain KEY SHARE compatible: %v", err)
	}
}

func codexLockTx(t *testing.T, env codexTestEnv, ctx context.Context) pgx.Tx {
	t.Helper()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("holder rollback: %v", err)
		}
	})
	return tx
}

// The embedded surface leaves alias/account ordering and classification in the service.
// Observations occur after the actual SQL, while its transaction is still open.
type codexLockStageQueries struct {
	codexPromoteQueries
	observe func(string)
}

func (q *codexLockStageQueries) LockCodexAccountWaitRunForUpdate(ctx context.Context, id uuid.UUID) (store.Run, error) {
	r, err := q.codexPromoteQueries.LockCodexAccountWaitRunForUpdate(ctx, id)
	if err == nil {
		q.observe("lock")
	}
	return r, err
}
func (q *codexLockStageQueries) ReadmitRunCodexBinding(ctx context.Context, arg store.ReadmitRunCodexBindingParams) (int64, error) {
	n, err := q.codexPromoteQueries.ReadmitRunCodexBinding(ctx, arg)
	if err == nil && n == 1 {
		q.observe("readmit")
	}
	return n, err
}
func (q *codexLockStageQueries) InsertCodexReadmitRunMessage(ctx context.Context, arg store.InsertCodexReadmitRunMessageParams) (int32, error) {
	n, err := q.codexPromoteQueries.InsertCodexReadmitRunMessage(ctx, arg)
	if err == nil {
		q.observe("feed")
	}
	return n, err
}
func (q *codexLockStageQueries) PromoteCodexAccountWaitRun(ctx context.Context, id uuid.UUID) (int64, error) {
	n, err := q.codexPromoteQueries.PromoteCodexAccountWaitRun(ctx, id)
	if err == nil && n == 1 {
		q.observe("promote")
	}
	return n, err
}
func (q *codexLockStageQueries) FailCodexAccountWaitRun(ctx context.Context, arg store.FailCodexAccountWaitRunParams) (int64, error) {
	n, err := q.codexPromoteQueries.FailCodexAccountWaitRun(ctx, arg)
	if err == nil && n == 1 {
		q.observe("fail")
	}
	return n, err
}

func TestCodexAccountCheckerParentLockPostureLiveDB(t *testing.T) {
	for _, operation := range []string{"lock", "promote", "fail"} {
		t.Run(operation, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, worker, repo := env.seedCodexInfra(t)
			lead := env.seedCodexRun(t, user, worker, repo)
			env.exec("UPDATE runs SET status='running',claim_generation=1,harness='claude',auto_approve=true,plan_cross_check_required=true WHERE id=$1", lead)
			child := seedRecoveryCheck(t, env, user, repo, worker, lead)
			env.exec("UPDATE runs SET status='recovery_wait',recovery_wait_cause='codex_account_unavailable' WHERE id=$1", child)
			ctx, cancel := context.WithTimeout(env.ctx, 12*time.Second)
			defer cancel()
			holder := codexLockTx(t, env, ctx)
			if _, err := holder.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", lead); err != nil {
				t.Fatal(err)
			}
			contender := codexLockTx(t, env, ctx)
			var pid int32
			if err := contender.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			q := store.New(contender)
			if operation == "lock" {
				// The initial prelock is nonblocking: refusal must precede any child lock.
				if _, err := q.LockCodexAccountWaitRunForUpdate(ctx, child); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("contended parent: %v, want ErrNoRows", err)
				}
			} else {
				done := make(chan error, 1)
				go func() {
					var n int64
					var err error
					if operation == "promote" {
						n, err = q.PromoteCodexAccountWaitRun(ctx, child)
					} else {
						n, err = q.FailCodexAccountWaitRun(ctx, store.FailCodexAccountWaitRunParams{ID: child, FailureReason: pgconv.Text("fixture failure"), FailOrigin: pgconv.Text("credential_unavailable")})
					}
					if err == nil && n != 1 {
						err = fmt.Errorf("mutation rows=%d, want 1", n)
					}
					done <- err
				}()
				// Always cancel and join before transaction/pool cleanup, including Fatal paths.
				joined := false
				defer func() {
					cancel()
					if !joined {
						<-done
					}
				}()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var blocked bool
					if err := env.pool.QueryRow(ctx, `SELECT COALESCE((SELECT wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) FROM pg_stat_activity WHERE pid=$1),false)`, pid, holder.Conn().PgConn().PID()).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case err := <-done:
						joined = true
						t.Fatalf("mutation completed before parent release: %v", err)
					case <-ctx.Done():
						t.Fatal("no positive parent contention observation:", ctx.Err())
					case <-ticker.C:
					}
				}
				// The observed wait is on the parent; a child UPDATE lock must still be available.
				probe := codexLockTx(t, env, ctx)
				if _, err := probe.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", child); err != nil {
					t.Fatalf("child locked before parent: %v", err)
				}
				if err := probe.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err := holder.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				err := <-done
				joined = true
				if err != nil {
					t.Fatal(err)
				}
				// The completed checker mutation must retain the parent's stronger posture.
				codexLockFKProbe(t, env, lead, true)
				if err := contender.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				want := "queued"
				if operation == "fail" {
					want = "failed"
				}
				if r := mustRun(t, env, child); r.Status != want {
					t.Fatalf("checker status=%s, want %s", r.Status, want)
				}
				return
			}
			probe := codexLockTx(t, env, ctx)
			if _, err := probe.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", child); err != nil {
				t.Fatalf("skipped parent left child locked: %v", err)
			}
			if err := probe.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if err := holder.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := q.LockCodexAccountWaitRunForUpdate(ctx, child); err != nil {
				t.Fatal(err)
			}
			codexLockFKProbe(t, env, lead, true)
			codexLockFKProbe(t, env, child, false)
			if err := contender.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			if r := mustRun(t, env, child); r.Status != "recovery_wait" {
				t.Fatalf("lock helper changed checker: %s", r.Status)
			}
		})
	}
}

// Promotion still requires the live pending plan's deadline, generation and owner.
// These refusals exercise the public SQL surface after the initial helper lock.
func TestCodexAccountCheckerPromotionAdmissionLiveDB(t *testing.T) {
	for _, fence := range []string{"pending", "generation", "deadline", "owner"} {
		t.Run(fence, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, worker, repo := env.seedCodexInfra(t)
			lead := env.seedCodexRun(t, user, worker, repo)
			env.exec("UPDATE runs SET status='running',claim_generation=1,harness='claude',auto_approve=true,plan_cross_check_required=true WHERE id=$1", lead)
			child := seedRecoveryCheck(t, env, user, repo, worker, lead)
			env.exec("UPDATE runs SET status='recovery_wait',recovery_wait_cause='codex_account_unavailable' WHERE id=$1", child)
			switch fence {
			case "pending":
				env.exec("UPDATE cross_checks SET verdict='approve' WHERE checker_run_id=$1", child)
			case "generation":
				env.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE checker_run_id=$1", child)
			case "deadline":
				env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE checker_run_id=$1", child)
			case "owner":
				env.exec("UPDATE runs SET user_id=$2 WHERE id=$1", child, env.seedUser(t))
			}
			ctx, cancel := context.WithTimeout(env.ctx, 5*time.Second)
			defer cancel()
			tx := codexLockTx(t, env, ctx)
			q := store.New(tx)
			if _, err := q.LockCodexAccountWaitRunForUpdate(ctx, child); err != nil {
				t.Fatal(err)
			}
			n, err := q.PromoteCodexAccountWaitRun(ctx, child)
			if err != nil || n != 0 {
				t.Fatalf("promotion fence %s: (%d,%v), want zero rows", fence, n, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if r := mustRun(t, env, child); r.Status != "recovery_wait" {
				t.Fatalf("refused checker status=%s", r.Status)
			}
		})
	}
}

func TestCodexAccountOrdinaryLockCompatibilityLiveDB(t *testing.T) {
	for _, branch := range []string{"promote", "fail", "readmit"} {
		t.Run(branch, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			// Positive controls distinguish the FK's KEY SHARE from an unrelated insert.
			for _, mode := range []string{"FOR UPDATE", "FOR NO KEY UPDATE"} {
				tx := codexLockTx(t, env, env.ctx)
				if _, err := tx.Exec(env.ctx, "SELECT id FROM runs WHERE id=$1 "+mode, fx.runID); err != nil {
					t.Fatal(err)
				}
				codexLockFKProbe(t, env, fx.runID, mode == "FOR UPDATE")
				if err := tx.Rollback(env.ctx); err != nil {
					t.Fatal(err)
				}
			}
			held := mustRun(t, env, fx.runID)
			material := held.CodexMaterialRevision.Int64
			switch branch {
			case "promote":
				relogin(t, env, env.q, fx, codexToken("restored"))
			case "fail":
				fx.setAccount(t, "credential_revision = credential_revision + 1")
			case "readmit":
				_, material = sameIdentityRelogin(t, env, fx, codexToken("readmitted"))
			}
			stages := []string{}
			svc.codexPromoteHooks = &codexPromoteTestHooks{wrapQueries: func(q codexPromoteQueries) codexPromoteQueries {
				return &codexLockStageQueries{codexPromoteQueries: q, observe: func(stage string) {
					stages = append(stages, stage)
					codexLockFKProbe(t, env, fx.runID, false)
				}}
			}}
			n, err := promoteOnly(t, svc, fx.runID)
			if err != nil {
				t.Fatal(err)
			}
			want := "[lock promote]"
			if branch == "fail" {
				want = "[lock fail]"
				if n != 0 {
					t.Fatalf("failed run promoted: %d", n)
				}
				assertHoldFailed(t, env, fx.runID, held, ErrCodexAccountRevisionStale)
			} else {
				if n != 1 {
					t.Fatalf("promoted=%d, want 1", n)
				}
				assertPromoted(t, env, fx.runID, held)
			}
			if branch == "readmit" {
				want = "[lock readmit feed promote]"
			}
			if fmt.Sprint(stages) != want {
				t.Fatalf("stages=%v, want %s", stages, want)
			}
			r := mustRun(t, env, fx.runID)
			if !r.CodexMaterialRevision.Valid || r.CodexMaterialRevision.Int64 != material {
				t.Fatalf("material=%v, want %d", r.CodexMaterialRevision, material)
			}
			lines := readmitLines(t, env, fx.runID)
			if branch != "readmit" {
				if len(lines) != 0 {
					t.Fatalf("unexpected re-admission lines: %v", lines)
				}
			} else {
				if len(lines) != 1 {
					t.Fatalf("re-admission lines=%d", len(lines))
				}
				var payload codexReadmitStatusPayload
				if err := json.Unmarshal([]byte(lines[0].payload), &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Event != codexReadmitEvent || payload.FromMaterialRevision != held.CodexMaterialRevision.Int64 || payload.ToMaterialRevision != material || r.LastSeq != lines[0].seq {
					t.Fatalf("persisted re-admission payload=%+v seq=%d last_seq=%d", payload, lines[0].seq, r.LastSeq)
				}
			}
		})
	}
}
