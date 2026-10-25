package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// restoreIntakePause resets the instance-global singleton to its seeded inactive state
// (re-inserting it if a test deleted it). The shared test database outlives each test,
// so every test that touches the pause registers this first.
func restoreIntakePause(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		mustExec(ctx, t, pool, `INSERT INTO instance_intake_pause (id) VALUES (true)
			ON CONFLICT (id) DO UPDATE SET active=false, since=NULL, by_user_id=NULL, by_label=NULL, reason=NULL`)
		mustExec(ctx, t, pool, `DELETE FROM instance_intake_pause_events`)
	})
}

// setIntakePauseSQL flips the singleton directly in SQL, independent of the Go transition
// helper, so the claim tests do not depend on the code under test elsewhere.
func setIntakePauseSQL(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	if on {
		mustExec(context.Background(), t, pool, `UPDATE instance_intake_pause SET active=true, since=now() WHERE id`)
		return
	}
	mustExec(context.Background(), t, pool, `UPDATE instance_intake_pause SET active=false, since=NULL, reason=NULL WHERE id`)
}

func registryKinds(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "run-kinds", "registry.json"))
	if err != nil {
		t.Fatalf("run-kind registry unreadable: %v", err)
	}
	var reg struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(b, &reg); err != nil || len(reg.Kinds) == 0 {
		t.Fatalf("run-kind registry: %v (%d kinds)", err, len(reg.Kinds))
	}
	return reg.Kinds
}

// intakeKindCase shapes a fresh queued issue run (fleetFixture.queuedRun) into one kind.
// Every UPDATE satisfies runs_kind_shape in one statement.
type intakeKindCase struct {
	name, kind string
	shape      string // SQL SET list applied to the run; $2 = a completed target run
}

var intakeKindCases = []intakeKindCase{
	{"issue", "issue", ``},
	{"ci_fix", "ci_fix", `kind='ci_fix', issue_iid=NULL, pipeline_id=7, pipeline_ref='main'`},
	{"chat", "chat", `kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL`},
	{"judge", "judge", `kind='judge', repo_id=NULL, issue_iid=NULL, branch=NULL, target_run_id=$2`},
	{"self_improve", "self_improve", `kind='self_improve'`},
	{"prompt", "prompt", `kind='prompt', issue_iid=NULL`},
	{"task", "task", `kind='task', issue_iid=NULL, branch='uzi/task/x', dispatched_at=now()`},
	{"task_review", "task", `kind='task', issue_iid=NULL, branch='uzi/task/r', dispatched_at=now(), trigger_source='task_review', review_target_run_id=$2`},
	{"task_then_fix", "task", `kind='task', issue_iid=NULL, branch='uzi/task/f', dispatched_at=now(), trigger_source='then_fix', then_fix_of_run_id=$2`},
	{"mr_rework", "mr_rework", `kind='mr_rework', issue_iid=NULL, pipeline_ref='agent/x', mr_iid=3, target_run_id=$2`},
	{"job", "job", `kind='job', repo_id=NULL, issue_iid=NULL, branch=NULL, job_type='research'`},
}

// TestIntakePauseAdmissionTableLiveDB enumerates every run kind (from the shared
// run-kind registry) x admitted/new-root x pause off/on through ClaimRun, and
// ClaimChatRun for chat. cross_check children are admitted by kind and have their own
// rows below. PRD #3454 D2/D4.
func TestIntakePauseAdmissionTableLiveDB(t *testing.T) {
	covered := map[string]bool{"cross_check": true}
	for _, c := range intakeKindCases {
		covered[c.kind] = true
	}
	for _, k := range registryKinds(t) {
		if !covered[k] {
			t.Errorf("run kind %q has no intake-pause admission row; add one", k)
		}
	}
	for _, c := range intakeKindCases {
		for _, admitted := range []bool{false, true} {
			for _, paused := range []bool{false, true} {
				name := fmt.Sprintf("%s/admitted=%v/paused=%v", c.name, admitted, paused)
				t.Run(name, func(t *testing.T) {
					fx := newFleetFixture(t)
					t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id=$1`, fx.userID) })
					restoreIntakePause(t, fx.pool)
					w := fx.worker("w", capOf(4), false)
					target := fx.queuedRun()
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='completed' WHERE id=$1`, target)
					run := fx.queuedRun()
					if c.shape != "" {
						mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET `+c.shape+` WHERE id=$1`, append([]any{run}, targetArg(c.shape, target)...)...)
					}
					if admitted {
						mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claim_generation=1 WHERE id=$1`, run)
					}
					setIntakePauseSQL(t, fx.pool, paused)

					var got store.Run
					var err error
					if c.kind == "chat" {
						got, err = fx.q.ClaimChatRun(fx.ctx, store.ClaimChatRunParams{WorkerID: pgU(w), UserID: fx.userID,
							AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true}})
					} else {
						// job_runner_v1/job_files_v1: the job kind's non-bypassable rollout gate.
						got, err = fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
							AffinityCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
							SpreadCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
							HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
							WorkerProtocolCaps: []string{"job_runner_v1", "job_files_v1"}})
					}
					wantClaim := !paused || admitted
					if wantClaim {
						if err != nil || got.ID != run {
							t.Fatalf("expected claim of %s, got %s err=%v", run, got.ID, err)
						}
						return
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("new root claimed during pause: %s err=%v", got.ID, err)
					}
					var status string
					if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id=$1`, run).Scan(&status); err != nil || status != "queued" {
						t.Fatalf("held root status=%q err=%v, want queued", status, err)
					}
				})
			}
		}
	}
}

func targetArg(shape string, target uuid.UUID) []any {
	if strings.Contains(shape, "$2") {
		return []any{target}
	}
	return nil
}

// A cross_check child is admitted by kind (its lead is already claimed): it claims
// under a pause even at claim_generation 0.
func TestIntakePauseCrossCheckChildAdmittedLiveDB(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			restoreIntakePause(t, fx.pool)
			w := fx.worker("w", capOf(4), false)
			caps := claudeCheckerProtocols(true, 1)
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, caps)
			child, _ := seedClaudeChild(fx, w, 1)
			setIntakePauseSQL(t, fx.pool, paused)
			now := time.Now()
			got, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
				CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
				CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
				AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
				SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
				HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
				WorkerCaps:               []string{}, WorkerProtocolCaps: caps})
			if err != nil || got.ID != child {
				t.Fatalf("cross_check child must claim (paused=%v): %s err=%v", paused, got.ID, err)
			}
		})
	}
}

// A requeue after a real claim keeps the run admitted: it re-claims during a pause.
func TestIntakePauseRequeuedRunStaysAdmittedLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	restoreIntakePause(t, fx.pool)
	w := fx.worker("w", capOf(4), false)
	run := fx.queuedRun()
	first, err := fx.claim(w, false, nil)
	if err != nil || first.ID != run {
		t.Fatalf("initial claim: %s %v", first.ID, err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='queued', status_since=now(), updated_at=now() WHERE id=$1`, run)
	setIntakePauseSQL(t, fx.pool, true)
	again, err := fx.claim(w, false, nil)
	if err != nil || again.ID != run || again.ClaimGeneration != 2 {
		t.Fatalf("requeued admitted run must re-claim during pause: %+v err=%v", again, err)
	}
}

// A deleted singleton reads as paused: new roots are refused, admitted runs still claim.
func TestIntakePauseMissingRowLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	restoreIntakePause(t, fx.pool)
	w := fx.worker("w", capOf(4), false)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM instance_intake_pause`)

	var paused bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_instance_intake_paused()`).Scan(&paused); err != nil || !paused {
		t.Fatalf("missing row must read as paused: %v err=%v", paused, err)
	}
	st, err := store.ReadInstanceIntakePause(fx.ctx, fx.q)
	if err != nil || !st.RowMissing || !st.Active {
		t.Fatalf("Go read of missing row: %+v err=%v", st, err)
	}
	root := fx.queuedRun()
	if _, err := fx.claim(w, false, nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new root claimed with missing row: %v", err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claim_generation=1 WHERE id=$1`, root)
	if got, err := fx.claim(w, false, nil); err != nil || got.ID != root {
		t.Fatalf("admitted run must claim with missing row: %s %v", got.ID, err)
	}

	// Lifting: an "off" transition re-seeds the row (inactive); the effective state changed
	// from paused to unpaused, so it is a change audited as an "off" event by the caller.
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM instance_intake_pause_events`)
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	by := pgtype.UUID{Bytes: fx.userID, Valid: true}
	lifted, changed, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), false, "", by, "Admin")
	if err != nil || !changed || lifted.Active {
		t.Fatalf("lift on missing row: %+v changed=%v err=%v", lifted, changed, err)
	}
	if err := tx.Commit(fx.ctx); err != nil {
		t.Fatal(err)
	}
	evs, err := fx.q.ListInstanceIntakePauseEvents(fx.ctx, 10)
	if err != nil || len(evs) != 1 || evs[0].Action != "off" || evs[0].ByUserID != by || evs[0].ByLabel.String != "Admin" {
		t.Fatalf("lifting a missing singleton must write one off event by the caller: %+v err=%v", evs, err)
	}
	// The row's attribution matches the event, as after a normal off.
	if cur, err := fx.q.GetInstanceIntakePause(fx.ctx); err != nil || cur.ByUserID != by || cur.ByLabel.String != "Admin" {
		t.Fatalf("re-seeded lift must attribute the row to the caller: %+v err=%v", cur, err)
	}
	var rows int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM instance_intake_pause`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("singleton not re-seeded: rows=%d err=%v", rows, err)
	}
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_instance_intake_paused()`).Scan(&paused); err != nil || paused {
		t.Fatalf("pause not lifted: paused=%v err=%v", paused, err)
	}
	newRoot := fx.queuedRun()
	if got, err := fx.claim(w, false, nil); err != nil || got.ID != newRoot {
		t.Fatalf("new root must claim after lift: %s %v", got.ID, err)
	}
}

// A reason-only edit by a second admin keeps the row's attribution (the event records
// the editor).
func TestIntakePauseReasonEditKeepsAttributionLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	restoreIntakePause(t, fx.pool)
	userB := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userB, userB.String()+"@intake-pause.test")
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", userB) })
	a := pgtype.UUID{Bytes: fx.userID, Valid: true}
	b := pgtype.UUID{Bytes: userB, Valid: true}

	apply := func(by pgtype.UUID, label, reason string) {
		t.Helper()
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		if _, _, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), true, reason, by, label); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
	}
	apply(a, "Admin A", "first")
	apply(b, "Admin B", "second")

	st, err := store.ReadInstanceIntakePause(fx.ctx, fx.q)
	if err != nil || st.ByUserID != a || st.ByLabel.String != "Admin A" || st.Reason.String != "second" {
		t.Fatalf("row attribution must stay with A, reason updated: %+v err=%v", st, err)
	}
	evs, err := fx.q.ListInstanceIntakePauseEvents(fx.ctx, 10)
	if err != nil || len(evs) != 2 || evs[0].Action != "reason" || evs[0].ByUserID != b || evs[0].ByLabel.String != "Admin B" {
		t.Fatalf("reason event must record B: %+v err=%v", evs, err)
	}
}

// The row change and its audit event are atomic: a rolled-back transaction leaves neither.
func TestIntakePauseRollbackIsAtomicLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	restoreIntakePause(t, fx.pool)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM instance_intake_pause_events`)
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), true, "rolled back",
		pgtype.UUID{Bytes: fx.userID, Valid: true}, "Admin"); err != nil || !changed {
		t.Fatalf("set in tx: changed=%v err=%v", changed, err)
	}
	if err := tx.Rollback(fx.ctx); err != nil {
		t.Fatal(err)
	}
	st, err := store.ReadInstanceIntakePause(fx.ctx, fx.q)
	if err != nil || st.Active || st.RowMissing || st.Reason.Valid {
		t.Fatalf("row change persisted after rollback: %+v err=%v", st, err)
	}
	evs, err := fx.q.ListInstanceIntakePauseEvents(fx.ctx, 10)
	if err != nil || len(evs) != 0 {
		t.Fatalf("event persisted after rollback: %+v err=%v", evs, err)
	}
}

// Transitions: events, since semantics, no-ops, reason sanitizing.
func TestIntakePauseTransitionsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	restoreIntakePause(t, fx.pool)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM instance_intake_pause_events`)
	by := pgtype.UUID{Bytes: fx.userID, Valid: true}

	apply := func(active bool, reason string) (store.InstanceIntakePause, bool) {
		t.Helper()
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		st, changed, err := store.SetInstanceIntakePause(fx.ctx, store.New(tx), active, reason, by, "Admin")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
		return st, changed
	}
	actions := func() string {
		t.Helper()
		evs, err := fx.q.ListInstanceIntakePauseEvents(fx.ctx, 50)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for i := len(evs) - 1; i >= 0; i-- {
			out = append(out, evs[i].Action)
		}
		return strings.Join(out, ",")
	}

	if _, changed := apply(false, ""); changed || actions() != "" {
		t.Fatalf("off->off must be a no-op without an event: %q", actions())
	}
	on, changed := apply(true, "node upgrade")
	if !changed || !on.Active || !on.Since.Valid || on.Reason.String != "node upgrade" || on.ByUserID != by || on.ByLabel.String != "Admin" {
		t.Fatalf("off->on: %+v changed=%v", on, changed)
	}
	if _, changed := apply(true, "node upgrade"); changed || actions() != "on" {
		t.Fatalf("on->on same reason must be a no-op: %q", actions())
	}
	re, changed := apply(true, "db upgrade")
	if !changed || !re.Since.Time.Equal(on.Since.Time) || re.Reason.String != "db upgrade" || actions() != "on,reason" {
		t.Fatalf("on->on new reason must keep since and log reason: %+v changed=%v actions=%q", re, changed, actions())
	}
	off, changed := apply(false, "ignored")
	if !changed || off.Active || off.Since.Valid || off.Reason.Valid || actions() != "on,reason,off" {
		t.Fatalf("on->off: %+v changed=%v actions=%q", off, changed, actions())
	}
	read, err := store.ReadInstanceIntakePause(fx.ctx, fx.q)
	if err != nil || read.Active || read.RowMissing {
		t.Fatalf("read after off: %+v err=%v", read, err)
	}

	// Reason is sanitized (ANSI stripped, flattened) and capped at 280 runes, never rejected.
	long := "\x1b[31mred\x1b[0m\n" + strings.Repeat("é", 300)
	san, _ := apply(true, long)
	if n := len([]rune(san.Reason.String)); n == 0 || n > store.IntakePauseReasonMax || strings.ContainsAny(san.Reason.String, "\x1b\n") {
		t.Fatalf("reason not sanitized: %d runes %q", n, san.Reason.String)
	}
	if !strings.HasPrefix(san.Reason.String, "red") {
		t.Fatalf("ANSI not stripped: %q", san.Reason.String)
	}
}
