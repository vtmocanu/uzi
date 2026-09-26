package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSetStatePlanRejectedSettlesRejectInputsLiveDB pins issue #1604's plan_rejected terminal
// write: the worker's `failed` report for a stop_kind='plan_rejected' run fails the run AND
// settles its still-unapplied reject_plan inputs in one statement (SetRunFailedPlanRejected), so
// no later claim can replay a reject for a run that is already failed. The result counts the
// TRANSITIONED run, never the settled inputs, and a declined transition touches no input.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestSetStatePlanRejectedSettlesRejectInputsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	svc := New(q, nil, Params{})
	svc.SetTxBeginner(pool)

	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("prs-%s@e2e", userID))
	exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/prs', 'https://forge.e2e/g/prs', 'main', true)`, repoID, connID)
	newWorker := func(t *testing.T) store.Worker {
		t.Helper()
		w := store.Worker{ID: uuid.New(), UserID: userID}
		exec(t, `INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w-prs', $3)`, w.ID, userID, w.ID[:])
		return w
	}
	wkr := newWorker(t)

	nextIID := int64(0)
	// seedRun inserts an issue run this worker holds at generation 1, parked on a live
	// plan-reject verdict (stop_kind stamped, as CreateStopVerdictInput does), with the columns
	// a terminal write clears armed so their clearing is observable.
	seedRun := func(t *testing.T) uuid.UUID {
		t.Helper()
		nextIID++
		id := uuid.New()
		exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		           claim_generation, stop_kind, session_id, plan_md, plan_source,
		           milestones_in_progress, milestones_agents, pause_requested_at, pause_mode, pause_after_count,
		           credential_switch_requested_at, credential_switch_generation, health, health_reason, health_since)
		         VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5,
		           1, 'plan_rejected', 'sess-1', 'the plan', 'agent',
		           '["m1"]'::jsonb, '[{"id":"m1","agent":"coder"}]'::jsonb, now(), 'milestone', 2,
		           now(), 1, 'stalled', 'idle', now())`,
			id, userID, repoID, 1000+nextIID, wkr.ID)
		return id
	}
	type inputs struct{ ackedReject, pendingReject, followUp, revise int64 }
	addInput := func(t *testing.T, run uuid.UUID, kind string, acked bool) int64 {
		t.Helper()
		var id int64
		sql := `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, $2, 'x') RETURNING id`
		if acked {
			sql = `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, consumed_claim_generation, consumed_worker_id)
			       VALUES ($1, $2, 'x', now() - interval '1 minute', 1, $3) RETURNING id`
		}
		args := []any{run, kind}
		if acked {
			args = append(args, wkr.ID)
		}
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("insert %s input: %v", kind, err)
		}
		return id
	}
	seedInputs := func(t *testing.T, run uuid.UUID) inputs {
		t.Helper()
		return inputs{
			ackedReject:   addInput(t, run, "reject_plan", true),
			pendingReject: addInput(t, run, "reject_plan", false),
			followUp:      addInput(t, run, "follow_up", false),
			revise:        addInput(t, run, "revise_plan", true),
		}
	}
	type inputState struct {
		consumed, applied string // text of the timestamps, "" when NULL
	}
	readInput := func(t *testing.T, id int64) inputState {
		t.Helper()
		var c, a pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT consumed_at::text, applied_at::text FROM run_user_inputs WHERE id = $1`, id).Scan(&c, &a); err != nil {
			t.Fatalf("read input %d: %v", id, err)
		}
		return inputState{consumed: c.String, applied: a.String}
	}
	snapshot := func(t *testing.T, in inputs) map[int64]inputState {
		t.Helper()
		out := make(map[int64]inputState)
		for _, id := range []int64{in.ackedReject, in.pendingReject, in.followUp, in.revise} {
			out[id] = readInput(t, id)
		}
		return out
	}
	requireUntouched := func(t *testing.T, stage string, in inputs, before map[int64]inputState) {
		t.Helper()
		for id, want := range before {
			if got := readInput(t, id); got != want {
				t.Fatalf("%s changed input %d: %+v, want %+v", stage, id, got, want)
			}
		}
	}
	requireSettled := func(t *testing.T, stage string, in inputs, before map[int64]inputState) {
		t.Helper()
		acked := readInput(t, in.ackedReject)
		if acked.applied == "" || acked.consumed != before[in.ackedReject].consumed {
			t.Fatalf("%s: ACKed reject = %+v, want applied with consumed_at kept %q", stage, acked, before[in.ackedReject].consumed)
		}
		if pending := readInput(t, in.pendingReject); pending.applied == "" || pending.consumed == "" {
			t.Fatalf("%s: pending reject = %+v, want consumed and applied", stage, pending)
		}
		// Other input kinds are not the verdict this terminal write settles.
		for _, id := range []int64{in.followUp, in.revise} {
			if got := readInput(t, id); got != before[id] {
				t.Fatalf("%s touched a non-reject input %d: %+v, want %+v", stage, id, got, before[id])
			}
		}
	}
	requireStatus := func(t *testing.T, run uuid.UUID, want string) store.Run {
		t.Helper()
		r, err := q.GetRunByID(ctx, run)
		if err != nil {
			t.Fatalf("GetRunByID: %v", err)
		}
		if r.Status != want {
			t.Fatalf("status = %q, want %q", r.Status, want)
		}
		return r
	}
	gen := func(g int64) *int64 { return &g }
	failedReport := func(g *int64) StateRequest {
		return StateRequest{State: "failed", ClaimGeneration: g}
	}
	params := func(run uuid.UUID, w store.Worker, g pgtype.Int8) store.SetRunFailedPlanRejectedParams {
		return store.SetRunFailedPlanRejectedParams{
			FailureReason: pgconv.TextOrNull("rejected"),
			FailOrigin:    pgconv.TextOrNull("plan_rejected"),
			ID:            run, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: g,
		}
	}

	t.Run("a fenced report fails the run and settles its reject inputs", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		before := snapshot(t, in)
		_, applied, err := svc.SetState(ctx, wkr, run, failedReport(gen(1)))
		if err != nil || !applied {
			t.Fatalf("SetState = (applied=%v, %v), want (true, nil)", applied, err)
		}
		r := requireStatus(t, run, "failed")
		if !r.FailOrigin.Valid || r.FailOrigin.String != "plan_rejected" {
			t.Fatalf("fail_origin = %+v, want plan_rejected", r.FailOrigin)
		}
		requireSettled(t, "fenced report", in, before)
	})

	t.Run("a duplicate terminal report changes nothing", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		if _, applied, err := svc.SetState(ctx, wkr, run, failedReport(gen(1))); err != nil || !applied {
			t.Fatalf("first SetState = (applied=%v, %v)", applied, err)
		}
		// A reject that arrives after the run failed must not be settled by a replayed report.
		late := addInput(t, run, "reject_plan", false)
		first := requireStatus(t, run, "failed")
		before := snapshot(t, in)
		before[late] = readInput(t, late)
		if _, applied, _ := svc.SetState(ctx, wkr, run, failedReport(nil)); applied {
			t.Fatal("duplicate terminal report applied")
		}
		n, err := q.SetRunFailedPlanRejected(ctx, params(run, wkr, pgtype.Int8{}))
		if err != nil || n != 0 {
			t.Fatalf("duplicate SetRunFailedPlanRejected = (%d, %v), want (0, nil)", n, err)
		}
		if again := requireStatus(t, run, "failed"); again.UpdatedAt != first.UpdatedAt || again.FinishedAt != first.FinishedAt {
			t.Fatal("duplicate terminal report rewrote the failed row")
		}
		requireUntouched(t, "duplicate terminal report", in, before)
	})

	t.Run("returns 1 when every reject input was already applied", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		// An older worker APPLIED the reject before reporting the terminal state.
		exec(t, `UPDATE run_user_inputs SET applied_at = now() WHERE id = ANY($1)`, []int64{in.ackedReject, in.pendingReject})
		before := snapshot(t, in)
		n, err := q.SetRunFailedPlanRejected(ctx, params(run, wkr, pgtype.Int8{Int64: 1, Valid: true}))
		if err != nil || n != 1 {
			t.Fatalf("SetRunFailedPlanRejected = (%d, %v), want (1, nil): the count is transitioned runs, not settled inputs", n, err)
		}
		requireStatus(t, run, "failed")
		requireUntouched(t, "already-applied rejects", in, before)
	})

	t.Run("a declined transition leaves inputs untouched", func(t *testing.T) {
		stranger := newWorker(t)
		for _, tc := range []struct {
			name  string
			setup func(run uuid.UUID)
			w     store.Worker
		}{
			{"already terminal", func(run uuid.UUID) {
				exec(t, `UPDATE runs SET status = 'cancelled', finished_at = now() WHERE id = $1`, run)
			}, wkr},
			{"wrong worker", func(uuid.UUID) {}, stranger},
			{"released claim", func(run uuid.UUID) {
				exec(t, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
			}, wkr},
		} {
			run := seedRun(t)
			in := seedInputs(t, run)
			tc.setup(run)
			before := snapshot(t, in)
			n, err := q.SetRunFailedPlanRejected(ctx, params(run, tc.w, pgtype.Int8{}))
			if err != nil || n != 0 {
				t.Fatalf("%s: SetRunFailedPlanRejected = (%d, %v), want (0, nil)", tc.name, n, err)
			}
			requireUntouched(t, tc.name, in, before)
		}
	})

	t.Run("a stale generation is refused and leaves inputs untouched", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		before := snapshot(t, in)
		if _, _, err := svc.SetState(ctx, wkr, run, failedReport(gen(2))); !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("stale-generation SetState err = %v, want ErrStaleClaim", err)
		}
		n, err := q.SetRunFailedPlanRejected(ctx, params(run, wkr, pgtype.Int8{Int64: 2, Valid: true}))
		if err != nil || n != 0 {
			t.Fatalf("stale-generation SetRunFailedPlanRejected = (%d, %v), want (0, nil)", n, err)
		}
		requireStatus(t, run, "running")
		requireUntouched(t, "stale generation", in, before)
	})

	t.Run("a rolled-back transaction leaves the run and inputs as they were", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		before := snapshot(t, in)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n, err := store.New(tx).SetRunFailedPlanRejected(ctx, params(run, wkr, pgtype.Int8{Int64: 1, Valid: true}))
		if err != nil || n != 1 {
			_ = tx.Rollback(ctx)
			t.Fatalf("in-tx SetRunFailedPlanRejected = (%d, %v), want (1, nil)", n, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		requireStatus(t, run, "running")
		requireUntouched(t, "rolled-back transaction", in, before)
	})

	t.Run("a generation-less legacy report settles atomically", func(t *testing.T) {
		run := seedRun(t)
		in := seedInputs(t, run)
		before := snapshot(t, in)
		_, applied, err := svc.SetState(ctx, wkr, run, failedReport(nil))
		if err != nil || !applied {
			t.Fatalf("legacy SetState = (applied=%v, %v), want (true, nil)", applied, err)
		}
		requireStatus(t, run, "failed")
		requireSettled(t, "legacy report", in, before)
	})

	// The failed row must carry every field SetRunFailed sets: two identical runs, one failed by
	// each query with the same parameters, must read back identical apart from the row identity
	// and the instants the statements stamped (compared by presence).
	t.Run("the failed row matches SetRunFailed's", func(t *testing.T) {
		viaFailed, viaPlanRejected := seedRun(t), seedRun(t)
		patch := pgconv.TextOrNull("diff --git a/x b/x")
		n, err := q.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: pgconv.TextOrNull("rejected"), FailOrigin: pgconv.TextOrNull("plan_rejected"),
			PreservedPatch: patch, SessionID: pgconv.TextOrNull("sess-2"), ID: viaFailed, WorkerID: pgconv.UUID(wkr.ID),
		})
		if err != nil || n != 1 {
			t.Fatalf("SetRunFailed = (%d, %v)", n, err)
		}
		p := params(viaPlanRejected, wkr, pgtype.Int8{})
		p.PreservedPatch, p.SessionID = patch, pgconv.TextOrNull("sess-2")
		if n, err = q.SetRunFailedPlanRejected(ctx, p); err != nil || n != 1 {
			t.Fatalf("SetRunFailedPlanRejected = (%d, %v)", n, err)
		}
		row := func(id uuid.UUID) map[string]any {
			t.Helper()
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT to_jsonb(r) - 'id' - 'issue_iid' FROM runs r WHERE id = $1`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
		stamped := map[string]bool{"status_since": true, "finished_at": true, "updated_at": true, "move_pending_since": true, "created_at": true,
			"pause_requested_at": true, "credential_switch_requested_at": true, "health_since": true}
		a, b := row(viaFailed), row(viaPlanRejected)
		if len(a) != len(b) {
			t.Fatalf("column sets differ: %d vs %d", len(a), len(b))
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok {
				t.Fatalf("column %s missing from the SetRunFailedPlanRejected row", k)
			}
			if stamped[k] {
				av, bv = av != nil, bv != nil
			}
			if fmt.Sprint(av) != fmt.Sprint(bv) {
				t.Errorf("column %s: SetRunFailed %v, SetRunFailedPlanRejected %v", k, av, bv)
			}
		}
		// Non-vacuity: the compared rows really are failed rows with the cleared columns.
		r := requireStatus(t, viaPlanRejected, "failed")
		if !r.MovePendingSince.Valid || r.PauseRequestedAt.Valid || r.CredentialSwitchRequestedAt.Valid || r.Health != "ok" || r.MilestonesInProgress != nil {
			t.Fatalf("failed row did not take SetRunFailed's writes: %+v", r)
		}
	})
}
