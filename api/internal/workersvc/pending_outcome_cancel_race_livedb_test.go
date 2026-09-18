package workersvc

import (
	"errors"
	"testing"
	"time"
)

func TestCancelPendingOutcomeRunZeroRowsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	o := seedReevalOwner(t, env, BindModeAuto, false)

	t.Run("active run reports a race conflict", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7301, 0)
		setRunGeneration(t, env, id, 4)
		seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(-time.Minute))
		run, err := svc.GetRun(env.ctx, o.userID, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}

		_, err = svc.cancelPendingOutcomeRun(env.ctx, o.userID, run, "discard")
		if !errors.Is(err, ErrOutcomePendingCancelRaced) {
			t.Fatalf("err = %v, want ErrOutcomePendingCancelRaced", err)
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want running", got)
		}
	})

	t.Run("terminal winner remains a successful no-op", func(t *testing.T) {
		id := seedRunningRun(t, env, o, 7302, 0)
		setRunGeneration(t, env, id, 4)
		seedTerminalPendingLease(t, env, o.workerID, id, 4, time.Now().Add(time.Hour))
		run, err := svc.GetRun(env.ctx, o.userID, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, id)

		res, err := svc.cancelPendingOutcomeRun(env.ctx, o.userID, run, "discard")
		if err != nil {
			t.Fatalf("terminal winner err = %v, want nil", err)
		}
		if !res.ServerSide {
			t.Fatal("terminal winner no-op must report server-side completion")
		}
		if got := statusOf(t, env, id); got != "completed" {
			t.Fatalf("status = %q, want completed", got)
		}
	})
}
