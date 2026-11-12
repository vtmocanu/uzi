package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M1: a worker that is not awake claims nothing, at the SQL claim fence
// (ClaimRun / ClaimChatRun), through fn_worker_accepts_new_intake, and in its Go
// mirror store.WorkerAcceptsNewIntake. Skipped without UZI_TEST_DATABASE_URL.

var hibernationStates = []string{"none", "preparing", "hibernated", "waking"}

// setHibernationSQL writes the state directly, satisfying the coherence CHECKs.
func setHibernationSQL(fx *fleetFixture, worker uuid.UUID, state string) {
	fx.t.Helper()
	if state == "none" {
		return
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base',
		hibernation_state=$2, hibernation_op_id=gen_random_uuid(), hibernation_requested_at=now(),
		hibernated_at=now(), wake_requested_at=now() WHERE id=$1`, worker, state)
}

func TestHibernationClaimFenceLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		shape string // SET list applied to a fresh queued issue run; $2 = a completed target run
		pin   bool   // pin the run to the claimant (a resume)
		chat  bool
		cross bool
	}{
		{name: "ordinary"},
		{name: "resume-pinned", pin: true},
		{name: "judge", shape: `kind='judge', repo_id=NULL, issue_iid=NULL, branch=NULL, target_run_id=$2`},
		{name: "cross_check", cross: true},
		{name: "chat", shape: `kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL`, chat: true},
		{name: "chat-pinned", shape: `kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL`, chat: true, pin: true},
	}
	for _, c := range cases {
		for _, state := range hibernationStates {
			t.Run(fmt.Sprintf("%s/%s", c.name, state), func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id=$1`, fx.userID) })
				w := fx.worker("w", capOf(4), false)
				var run uuid.UUID
				caps := []string{}
				if c.cross {
					caps = claudeCheckerProtocols(true, 1)
					mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, caps)
					run, _ = seedClaudeChild(fx, w, 1)
				} else {
					target := fx.queuedRun()
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='completed' WHERE id=$1`, target)
					run = fx.queuedRun()
					if c.shape != "" {
						mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET `+c.shape+` WHERE id=$1`, append([]any{run}, targetArg(c.shape, target)...)...)
					}
					if c.pin {
						mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, w)
					}
				}
				setHibernationSQL(fx, w, state)

				now := time.Now()
				var got store.Run
				var err error
				if c.chat {
					got, err = fx.q.ClaimChatRun(fx.ctx, store.ClaimChatRunParams{WorkerID: pgU(w), UserID: fx.userID,
						AffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true}})
				} else {
					got, err = fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
						CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
						CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
						AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
						SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
						HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
						WorkerCaps:               []string{}, WorkerProtocolCaps: caps})
				}
				if state == "none" {
					if err != nil || got.ID != run {
						t.Fatalf("control: an awake worker must claim %s, got %s err=%v", run, got.ID, err)
					}
					return
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("worker in %q claimed %s err=%v, want no rows", state, got.ID, err)
				}
				var status string
				if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id=$1`, run).Scan(&status); err != nil || status != "queued" {
					t.Fatalf("run status=%q err=%v, want queued", status, err)
				}
			})
		}
	}
}

func TestHibernationIntakeGoSQLParityLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id=$1`, fx.userID) })
	extra := map[string]string{
		"plain":        ``,
		"auto-drain":   `, draining_since=now()`,
		"fenced":       `, maintenance_fenced=true`,
		"maint-phase":  `, maintenance_phase='requested'`,
		"manual":       `, manual_cordoned_at=now(), manual_cordoned_by=user_id, manual_cordoned_by_label='op'`,
		"maint-resume": `, maintenance_phase='complete'`,
	}
	for _, state := range hibernationStates {
		for name, set := range extra {
			t.Run(state+"/"+name, func(t *testing.T) {
				w := fx.worker("p", capOf(1), false)
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base',
					hibernation_state=$2, hibernation_op_id=CASE WHEN $2='none' THEN NULL ELSE gen_random_uuid() END,
					hibernated_at=now(), wake_requested_at=now()`+set+` WHERE id=$1`, w, state)
				row, err := fx.q.GetWorkerByID(fx.ctx, w)
				if err != nil {
					t.Fatal(err)
				}
				if row.HibernationState != state {
					t.Fatalf("row state %q want %q", row.HibernationState, state)
				}
				var sqlAccepts bool
				if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_worker_accepts_new_intake(w) FROM workers w WHERE id=$1`, w).Scan(&sqlAccepts); err != nil {
					t.Fatal(err)
				}
				if goAccepts := store.WorkerAcceptsNewIntake(row); goAccepts != sqlAccepts {
					t.Fatalf("Go=%v SQL=%v", goAccepts, sqlAccepts)
				}
				// A partial struct that omits HibernationState (an old-style caller) must
				// still treat a non-none row as closed when it passes the field.
				partial := store.Worker{ManualCordonedAt: row.ManualCordonedAt, DrainingSince: row.DrainingSince,
					MaintenanceFenced: row.MaintenanceFenced, MaintenancePhase: row.MaintenancePhase, HibernationState: row.HibernationState}
				if store.WorkerAcceptsNewIntake(partial) != sqlAccepts {
					t.Fatalf("partial struct disagrees with SQL=%v", sqlAccepts)
				}
				if state == "none" && name == "plain" && !sqlAccepts {
					t.Fatal("control: an awake plain worker must accept intake")
				}
				if state != "none" && sqlAccepts {
					t.Fatalf("state %q must close intake", state)
				}
			})
		}
	}
}

func TestHibernationControllerSnapshotLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id=$1`, fx.userID) })
	want := map[uuid.UUID]string{}
	for _, state := range hibernationStates {
		w := fx.worker(state, capOf(1), false)
		setHibernationSQL(fx, w, state)
		if state == "none" {
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base' WHERE id=$1`, w)
		}
		want[w] = state
	}
	rows, err := fx.q.ListHostedWorkersForController(fx.ctx, store.ListHostedWorkersForControllerParams{
		DiskPressureMinStreak: 2, HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range rows {
		state, ok := want[r.ID]
		if !ok {
			continue
		}
		seen++
		if r.HibernationState != state {
			t.Errorf("worker %s state %q want %q", r.ID, r.HibernationState, state)
		}
		if (state == "none") == r.HibernationOpID.Valid {
			t.Errorf("worker %s op id valid=%v for state %q", r.ID, r.HibernationOpID.Valid, state)
		}
	}
	if seen != len(want) {
		t.Fatalf("snapshot returned %d of %d workers", seen, len(want))
	}
}
