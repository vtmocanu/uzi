package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func planAgentSourceOf(t *testing.T, env codexTestEnv, id uuid.UUID) pgtype.Text {
	t.Helper()
	run, err := env.q.GetRunByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return run.PlanAgentSource
}

func TestPlanAgentSourceRecordLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	id := seedRunningRun(t, env, o, 39141, 0)

	if got := planAgentSourceOf(t, env, id); got.Valid {
		t.Fatalf("a run that reported no plan source stores %v, want NULL", got)
	}

	repo, own := AgentSourceRepo, AgentSourceOwn
	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", PlanAgentSource: &repo}); err != nil || !applied {
		t.Fatalf("plan source report = (applied=%v, %v), want applied", applied, err)
	}
	if got := planAgentSourceOf(t, env, id); got.String != AgentSourceRepo {
		t.Fatalf("stored plan source = %v, want repo", got)
	}

	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running"}); err != nil || !applied {
		t.Fatalf("a heartbeat = (applied=%v, %v), want applied", applied, err)
	}
	if got := planAgentSourceOf(t, env, id); got.String != AgentSourceRepo {
		t.Fatalf("a heartbeat moved the plan source to %v", got)
	}

	if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", PlanAgentSource: &own}); err != nil || !applied {
		t.Fatalf("replan report = (applied=%v, %v), want applied", applied, err)
	}
	if got := planAgentSourceOf(t, env, id); got.String != AgentSourceOwn {
		t.Fatalf("a replan stores %v, want own", got)
	}

	invalid := "both"
	if _, _, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "running", PlanAgentSource: &invalid}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("an unknown plan source err = %v, want ErrInvalidState", err)
	}
	if got := planAgentSourceOf(t, env, id); got.String != AgentSourceOwn {
		t.Fatalf("a refused report moved the plan source to %v", got)
	}

	if _, err := env.pool.Exec(env.ctx, `UPDATE runs SET plan_agent_source = 'both' WHERE id = $1`, id); err == nil {
		t.Fatal("the column CHECK must refuse a value other than own or repo")
	}
}
