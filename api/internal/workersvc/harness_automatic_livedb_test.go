package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Automatic ("Default · automatic") harness creation, #3845. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.

// newAutoFix is the overflow fixture with the stored default unset: both harnesses usable
// (Claude default token, pooled Codex alias), the overflow toggle as given.
func newAutoFix(t *testing.T, overflow bool) *overflowFix {
	t.Helper()
	fx := newOverflowFix(t)
	fx.env.exec(`UPDATE users SET default_harness = NULL, harness_overflow_enabled = $2 WHERE id = $1`, fx.userID, overflow)
	return fx
}

// claudeWeekly writes a fresh Claude reading at pct used whose weekly window resets in resetsIn.
func (fx *overflowFix) claudeWeekly(id uuid.UUID, pct int, resetsIn string) {
	fx.t.Helper()
	fx.claudeReading(id, pct, "1 minute")
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = now() + $2::interval WHERE user_secret_id = $1`, id, resetsIn)
}

// codexWeekly writes a fresh Codex reading at util used whose weekly window resets resetAfter
// seconds after poolEpoch.
func (fx *overflowFix) codexWeekly(a poolAccount, util float64, resetAfter int64) {
	fx.t.Helper()
	fx.readingRaw(a, poolBuckets(true, false, poolWindow(fiveHourSecs, util, resetAfter), poolWindow(weekSecs, util, resetAfter)), "1 minute")
}

func (fx *overflowFix) claudeResetOf(id uuid.UUID) time.Time {
	fx.t.Helper()
	var at time.Time
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT seven_day_resets_at FROM anthropic_rate_limits WHERE user_secret_id = $1`, id).Scan(&at); err != nil {
		fx.t.Fatal(err)
	}
	return at
}

func TestHarnessAutomaticOutcomesLiveDB(t *testing.T) {
	tests := []struct {
		name       string
		claudePct  int
		claudeIn   string
		claudeAgo  string
		codexUtil  float64
		codexAfter int64
		codexAgo   string
		harness    string
		reason     string
	}{
		{"codex resets sooner", 20, "72 hours", "1 minute", 20, 86400, "1 minute", "codex", "auto_soonest_reset"},
		{"claude resets sooner", 20, "10 hours", "1 minute", 20, 259200, "1 minute", "claude", "auto_soonest_reset"},
		{"only codex qualified", 99, "10 hours", "1 minute", 20, 259200, "1 minute", "codex", "auto_only_qualified"},
		{"only claude qualified", 20, "72 hours", "1 minute", 99, 86400, "1 minute", "claude", "auto_only_qualified"},
		{"both depleted", 99, "10 hours", "1 minute", 99, 86400, "1 minute", "claude", "auto_fallback"},
		{"codex reading stale", 20, "72 hours", "1 minute", 20, 86400, "2 hours", "claude", "auto_fallback"},
		{"claude reading stale", 20, "72 hours", "2 hours", 20, 86400, "1 minute", "claude", "auto_fallback"},
	}
	for _, overflow := range []bool{false, true} {
		for _, tt := range tests {
			t.Run(tt.name+map[bool]string{false: "/overflow off", true: "/overflow on"}[overflow], func(t *testing.T) {
				fx := newAutoFix(t, overflow)
				fx.claudeWeekly(fx.token, tt.claudePct, tt.claudeIn)
				fx.env.exec(`UPDATE anthropic_rate_limits SET synced_at = now() - $2::interval WHERE user_secret_id = $1`, fx.token, tt.claudeAgo)
				fx.readingRaw(fx.codex, poolBuckets(true, false, poolWindow(fiveHourSecs, tt.codexUtil, tt.codexAfter), poolWindow(weekSecs, tt.codexUtil, tt.codexAfter)), tt.codexAgo)
				run, err := fx.issue()
				if err != nil {
					t.Fatal(err)
				}
				got := mustRun(t, fx.env, run.ID)
				if got.Harness != tt.harness || got.HarnessDecisionReason.String != tt.reason || got.HarnessPreferred.String != "claude" {
					t.Fatalf("harness=%s preferred=%v reason=%v want %s/%s", got.Harness, got.HarnessPreferred, got.HarnessDecisionReason, tt.harness, tt.reason)
				}
				if !got.HarnessPreferredHeld.Valid || !got.HarnessOtherHeld.Valid {
					t.Fatalf("held flags must be recorded: %v/%v", got.HarnessPreferredHeld, got.HarnessOtherHeld)
				}
				if tt.harness == "codex" {
					if !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != fx.codex.alias {
						t.Fatalf("codex binding=%v", got.CodexSecretID)
					}
				} else if got.CodexSecretID.Valid {
					t.Fatalf("claude run froze a codex binding: %v", got.CodexSecretID)
				}
			})
		}
	}
}

func TestHarnessAutomaticFreezesDecisionAndResetsLiveDB(t *testing.T) {
	for _, codexWins := range []bool{true, false} {
		name := "claude wins"
		if codexWins {
			name = "codex wins"
		}
		t.Run(name, func(t *testing.T) {
			fx := newAutoFix(t, false)
			claudeIn, codexAfter := "10 hours", int64(259200)
			if codexWins {
				claudeIn, codexAfter = "72 hours", 86400
			}
			fx.claudeWeekly(fx.token, 20, claudeIn)
			fx.codexWeekly(fx.codex, 30, codexAfter)
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, fx.env, run.ID)
			want := "claude"
			if codexWins {
				want = "codex"
			}
			if got.Harness != want || got.HarnessDecisionReason.String != "auto_soonest_reset" {
				t.Fatalf("harness=%s reason=%v", got.Harness, got.HarnessDecisionReason)
			}
			if !got.HarnessPreferredWeeklyResetAt.Valid || !got.HarnessPreferredWeeklyResetAt.Time.Equal(fx.claudeResetOf(fx.token)) {
				t.Fatalf("preferred reset=%v want claude's %v", got.HarnessPreferredWeeklyResetAt, fx.claudeResetOf(fx.token))
			}
			if !got.HarnessOtherWeeklyResetAt.Valid || got.HarnessOtherWeeklyResetAt.Time.Unix() != poolEpoch+codexAfter {
				t.Fatalf("other reset=%v want unix %d", got.HarnessOtherWeeklyResetAt, poolEpoch+codexAfter)
			}
			if !got.HarnessPreferredScore.Valid || got.HarnessPreferredScore.Float64 != 80 || !got.HarnessPreferredMeasured.Valid ||
				!got.HarnessOtherScore.Valid || !got.HarnessOtherMeasured.Valid || got.HarnessPreferredHeld.Bool || got.HarnessOtherHeld.Bool {
				t.Fatalf("evidence: %v/%v measured %v/%v held %v/%v", got.HarnessPreferredScore, got.HarnessOtherScore,
					got.HarnessPreferredMeasured, got.HarnessOtherMeasured, got.HarnessPreferredHeld, got.HarnessOtherHeld)
			}
			// The record is write-once: moving the gauges leaves it and the harness as created.
			fx.claudeWeekly(fx.token, 90, "1 hour")
			fx.codexWeekly(fx.codex, 90, 3600)
			after := mustRun(t, fx.env, run.ID)
			if after.Harness != got.Harness || after.HarnessDecisionReason != got.HarnessDecisionReason ||
				after.HarnessPreferredWeeklyResetAt != got.HarnessPreferredWeeklyResetAt || after.HarnessOtherWeeklyResetAt != got.HarnessOtherWeeklyResetAt {
				t.Fatal("automatic decision changed after the gauges moved")
			}
		})
	}
}

func TestHarnessAutomaticDefaultAccountLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	fx.mode("default")
	fx.claudeWeekly(fx.token, 20, "72 hours")
	fx.codexWeekly(fx.codex, 20, 86400)
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.Harness != "codex" || got.HarnessDecisionReason.String != "auto_soonest_reset" ||
		!got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != fx.codex.alias || got.CodexSelectReason.String != "default" {
		t.Fatalf("default account: harness=%s reason=%v alias=%v select=%v", got.Harness, got.HarnessDecisionReason, got.CodexSecretID, got.CodexSelectReason)
	}
}

// A Codex state change at the fence: a Codex winner is refused with no run; a Claude winner is
// created and its record recomputed with Codex unknown.
func TestHarnessAutomaticFenceDegradeLiveDB(t *testing.T) {
	for _, mode := range []string{"auto", "default"} {
		t.Run(mode+"/codex chosen is refused", func(t *testing.T) {
			fx := newAutoFix(t, false)
			fx.mode(mode)
			fx.claudeWeekly(fx.token, 20, "72 hours")
			fx.codexWeekly(fx.codex, 20, 86400)
			fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
			if _, err := fx.issue(); !errors.Is(err, errHarnessOverflowFence) {
				t.Fatalf("error=%v want %v", err, errHarnessOverflowFence)
			}
			if fx.runs() != 0 {
				t.Fatal("fence refusal left a run")
			}
		})
		t.Run(mode+"/claude chosen is recomputed", func(t *testing.T) {
			fx := newAutoFix(t, false)
			fx.mode(mode)
			fx.claudeWeekly(fx.token, 20, "10 hours")
			fx.codexWeekly(fx.codex, 20, 259200)
			fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			got := mustRun(t, fx.env, run.ID)
			if got.Harness != "claude" || got.HarnessDecisionReason.String != "auto_fallback" || got.CodexSecretID.Valid {
				t.Fatalf("harness=%s reason=%v codex=%v", got.Harness, got.HarnessDecisionReason, got.CodexSecretID)
			}
			if got.HarnessOtherScore.Valid || got.HarnessOtherMeasured.Valid || got.HarnessOtherWeeklyResetAt.Valid ||
				!got.HarnessOtherHeld.Valid || got.HarnessOtherHeld.Bool {
				t.Fatalf("codex side must read unknown: score=%v measured=%v reset=%v held=%v",
					got.HarnessOtherScore, got.HarnessOtherMeasured, got.HarnessOtherWeeklyResetAt, got.HarnessOtherHeld)
			}
			if !got.HarnessPreferredWeeklyResetAt.Valid {
				t.Fatal("the Claude side keeps its evidence")
			}
		})
	}
}

// Two qualified pooled accounts; the first pick is poisoned at the fence and the replacement
// supplies the binding, identity, select reason and every Codex evidence column.
func TestHarnessAutomaticFenceReplacementLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	replacement := fx.newAccount("replacement")
	fx.pool(replacement.alias)
	fx.claudeWeekly(fx.token, 20, "120 hours")
	fx.codexWeekly(fx.codex, 10, 86400)
	fx.codexWeekly(replacement, 40, 200000)
	fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.Harness != "codex" || got.HarnessDecisionReason.String != "auto_soonest_reset" {
		t.Fatalf("harness=%s reason=%v", got.Harness, got.HarnessDecisionReason)
	}
	if !got.CodexSecretID.Valid || uuid.UUID(got.CodexSecretID.Bytes) != replacement.alias ||
		got.CodexAccountKey.String != accountKeyOf(fx.poolFix, replacement) || got.CodexSelectReason.String != "auto" {
		t.Fatalf("frozen binding is not the replacement: alias=%v key=%v select=%v", got.CodexSecretID, got.CodexAccountKey, got.CodexSelectReason)
	}
	if !got.HarnessOtherMeasured.Valid || got.HarnessOtherMeasured.Float64 >= 80 || got.HarnessOtherMeasured.Float64 <= 30 {
		t.Fatalf("other measured=%v describes the poisoned first pick", got.HarnessOtherMeasured)
	}
	if !got.HarnessOtherWeeklyResetAt.Valid || got.HarnessOtherWeeklyResetAt.Time.Unix() != poolEpoch+200000 {
		t.Fatalf("other reset=%v want unix %d (the replacement's)", got.HarnessOtherWeeklyResetAt, poolEpoch+200000)
	}
}

// The explicit-default overflow fence keeps its refusals: a pool that empties at the fence is
// errCodexPoolUnavailable, a default alias that stops being usable is errHarnessOverflowFence.
func TestHarnessOverflowExplicitDefaultFenceUnchangedLiveDB(t *testing.T) {
	t.Run("pool unavailable", func(t *testing.T) {
		fx := newOverflowFix(t)
		fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
		if _, err := fx.issue(); !errors.Is(err, errCodexPoolUnavailable) {
			t.Fatalf("error=%v want %v", err, errCodexPoolUnavailable)
		}
		if fx.runs() != 0 {
			t.Fatal("refusal left a run")
		}
	})
	t.Run("default alias unusable", func(t *testing.T) {
		fx := newOverflowFix(t)
		fx.mode("default")
		fx.markUnusableBeforeFence(`UPDATE codex_provider_account SET coord_state='quarantined' WHERE id=$1`)
		if _, err := fx.issue(); !errors.Is(err, errHarnessOverflowFence) {
			t.Fatalf("error=%v want %v", err, errHarnessOverflowFence)
		}
		if fx.runs() != 0 {
			t.Fatal("refusal left a run")
		}
	})
	t.Run("overflow rows record no weekly resets", func(t *testing.T) {
		fx := newOverflowFix(t)
		run, err := fx.issue()
		if err != nil {
			t.Fatal(err)
		}
		got := mustRun(t, fx.env, run.ID)
		if got.HarnessDecisionReason.String != "overflow" || got.HarnessPreferredWeeklyResetAt.Valid || got.HarnessOtherWeeklyResetAt.Valid {
			t.Fatalf("reason=%v resets=%v/%v", got.HarnessDecisionReason, got.HarnessPreferredWeeklyResetAt, got.HarnessOtherWeeklyResetAt)
		}
	})
}

// A transaction holding the exclusive secret-mutation lock blocks an automatic create before
// it decides anything, with the overflow toggle OFF; after the holder commits a change that
// flips the answer, the create reflects the post-release state.
func TestHarnessAutomaticSharedLockLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	fx.claudeWeekly(fx.token, 20, "72 hours")
	fx.codexWeekly(fx.codex, 20, 86400) // Codex resets sooner while the lock is held
	var fired atomic.Bool
	fx.svc.codexSelectLockedHook = func(context.Context) { fired.Store(true) }
	iid := fx.nextIssue()
	ctx, cancel := context.WithTimeout(fx.env.ctx, poolTestTimeout)
	defer cancel()

	holder, err := fx.aux.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if err := store.New(holder).LockSecretMutation(ctx, fx.userID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		run store.Run
		err error
	}
	done := make(chan result, 1)
	go func() {
		run, err := fx.svc.CreateRun(ctx, fx.userID, fx.repoID, iid, "locked", nil, nil, false, nil, nil, nil)
		done <- result{run, err}
	}()
	fx.waitBlocked(1)
	select {
	case r := <-done:
		t.Fatalf("create finished while the exclusive lock was held: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if fired.Load() || fx.runs() != 0 {
		t.Fatalf("create progressed under the exclusive lock: locked hook=%t runs=%d", fired.Load(), fx.runs())
	}
	if _, err := holder.Exec(ctx, `UPDATE anthropic_rate_limits SET seven_day_resets_at = now() + interval '1 hour' WHERE user_secret_id = $1`, fx.token); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		got := mustRun(t, fx.env, r.run.ID)
		if !fired.Load() || got.Harness != "claude" || got.HarnessDecisionReason.String != "auto_soonest_reset" {
			t.Fatalf("post-release state not reflected: locked hook=%t harness=%s reason=%v", fired.Load(), got.Harness, got.HarnessDecisionReason)
		}
	case <-time.After(poolTestTimeout):
		t.Fatal("create did not finish after the lock was released")
	}
}

func TestHarnessAutomaticInheritOverrideLiveDB(t *testing.T) {
	t.Run("codex pick clears inherit", func(t *testing.T) {
		fx := newAutoFix(t, false)
		fx.claudeWeekly(fx.token, 20, "72 hours")
		fx.codexWeekly(fx.codex, 20, 86400)
		run, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "inherit", nil, nil, false, nil, nil, &RawCredentialOverride{Mode: CredentialOverrideModeInherit})
		if err != nil {
			t.Fatal(err)
		}
		got := mustRun(t, fx.env, run.ID)
		if got.Harness != "codex" || got.HarnessDecisionReason.String != "auto_soonest_reset" ||
			got.CredentialOverrideMode.Valid || got.CredentialOverrideSecretID.Valid {
			t.Fatalf("harness=%s reason=%v override=%v/%v", got.Harness, got.HarnessDecisionReason, got.CredentialOverrideMode, got.CredentialOverrideSecretID)
		}
	})
	t.Run("explicit codex keeps the refusal", func(t *testing.T) {
		fx := newAutoFix(t, false)
		codex := HarnessCodex
		_, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "inherit", nil, nil, false, nil, &codex, &RawCredentialOverride{Mode: CredentialOverrideModeInherit})
		if !errors.Is(err, ErrCredentialOverrideHarnessUnsupported) {
			t.Fatalf("error=%v", err)
		}
		if fx.runs() != 0 {
			t.Fatal("refused override left a run")
		}
	})
}

func TestHarnessAutomaticMissingTransactionLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	fx.svc.txBeginner = nil
	if _, err := fx.issue(); !errors.Is(err, errHarnessOverflowRequiresTx) {
		t.Fatalf("error=%v", err)
	}
	if fx.runs() != 0 {
		t.Fatal("nontransactional automatic create left a run")
	}
}

// The usage-independent readers never consult a gauge, so moving both gauges (which flips the
// automatic winner) leaves their answers unchanged.
func TestHarnessAutomaticReadersUsageIndependentLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	check := func() {
		t.Helper()
		if h, e := fx.svc.ResolveHarnessForUser(fx.env.ctx, fx.userID, nil); e != nil || h != HarnessClaude {
			t.Fatalf("user resolver=%s/%v", h, e)
		}
		if h, e := fx.svc.ResolveHarnessForUserOn(fx.env.ctx, fx.env.q, fx.userID, nil); e != nil || h != HarnessClaude {
			t.Fatalf("on resolver=%s/%v", h, e)
		}
		if h, e := fx.svc.ResolveSettingsHarness(fx.env.ctx, fx.userID); e != nil || h != HarnessClaude {
			t.Fatalf("settings resolver=%s/%v", h, e)
		}
		proposed := HarnessCodex
		if h, e := fx.svc.ResolveSettingsHarnessWithDefault(fx.env.ctx, fx.userID, &proposed); e != nil || h != HarnessCodex {
			t.Fatalf("proposed resolver=%s/%v", h, e)
		}
		if both, e := fx.svc.BothHarnessesUsable(fx.env.ctx, fx.userID); e != nil || !both {
			t.Fatalf("both=%v/%v", both, e)
		}
	}
	fx.claudeWeekly(fx.token, 20, "72 hours")
	fx.codexWeekly(fx.codex, 20, 86400)
	check()
	fx.claudeWeekly(fx.token, 99, "1 hour")
	fx.codexWeekly(fx.codex, 5, 3600)
	check()
}

type autoFaultBeginner struct{ inner TxBeginner }

func (b autoFaultBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &autoFaultTx{Tx: tx}, nil
}

var errAutoInjected = errors.New("injected claude side read failure")

// autoFaultTx keeps Postgres real and fails only the Claude side's candidate read.
type autoFaultTx struct{ pgx.Tx }

func (tx *autoFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: ListAutoSelectCandidates :many") {
		return nil, errAutoInjected
	}
	return tx.Tx.Query(ctx, sql, args...)
}

// A database error while evaluating a side is not an expected absence: creation fails.
func TestHarnessAutomaticSideEvaluationErrorFailsCreateLiveDB(t *testing.T) {
	fx := newAutoFix(t, false)
	fx.svc.txBeginner = autoFaultBeginner{inner: fx.env.pool}
	if _, err := fx.issue(); !errors.Is(err, errAutoInjected) {
		t.Fatalf("error=%v want the injected read failure", err)
	}
	if fx.runs() != 0 {
		t.Fatal("failed side evaluation left a run")
	}
}

// The DB CHECK admits exactly the Go reason vocabulary and ties the automatic shape to a
// Claude-preferred record, with weekly resets only on automatic rows.
func TestHarnessAutomaticDecisionConstraintLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	var def string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'runs_harness_decision_shape'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	for _, reason := range harnessDecisionReasons {
		if !strings.Contains(def, "'"+reason+"'") {
			t.Fatalf("constraint does not admit %q: %s", reason, def)
		}
	}
	run, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	check := func(query string, wantCode string) {
		t.Helper()
		_, err := fx.env.pool.Exec(fx.env.ctx, query, run.ID)
		var pgErr *pgconn.PgError
		switch {
		case wantCode == "" && err != nil:
			t.Fatalf("valid shape rejected: %s: %v", query, err)
		case wantCode != "" && (!errors.As(err, &pgErr) || pgErr.Code != wantCode):
			t.Fatalf("%s: err=%v want SQLSTATE %s", query, err, wantCode)
		}
	}
	// A non-automatic row may not carry a weekly reset.
	check(`UPDATE runs SET harness_preferred_weekly_reset_at = now() WHERE id = $1`, "23514")
	check(`UPDATE runs SET harness_other_weekly_reset_at = now() WHERE id = $1`, "23514")
	// An automatic row is Claude-preferred.
	check(`UPDATE runs SET harness_decision_reason = 'auto_soonest_reset', harness_preferred = 'codex' WHERE id = $1`, "23514")
	// An automatic row for Claude may carry resets.
	check(`UPDATE runs SET harness_decision_reason = 'auto_soonest_reset', harness_preferred = 'claude',
		harness_preferred_weekly_reset_at = now(), harness_other_weekly_reset_at = now() WHERE id = $1`, "")
}
