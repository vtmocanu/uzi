package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestConsumeInputsTerminalCancelledLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	env.exec("UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE id=$1", o.workerID)
	for i, shape := range []string{"ordinary", "released", "pending switch"} {
		t.Run(shape, func(t *testing.T) {
			id := seedRunningRun(t, env, o, 290700+int64(i), 0)
			setRunGeneration(t, env, id, 12)
			env.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'follow_up','retained steering')", id)
			const tip = "3333333333333333333333333333333333333333"
			branch := agentIssueBranch(290700 + int64(i))
			env.exec("INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES($1,$2,$3,$4,$5,$6,'retained')", id, o.userID, o.repoID, branch, checkpointRefPrefix+branch, tip)
			env.exec("UPDATE runs SET checkpoint_tip=$2, checkpoint_tip_at='2026-01-02 03:04:05+00' WHERE id=$1", id, tip)
			env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
    VALUES($1,$2,$3,$4,12,'open',$5,'ident',$5,$4)`, uuid.New(), o.userID, o.repoID, id, o.workerID)
			// Snapshot full retained rows and all delivery/receipt fields, not just row counts.
			snapshot := func() string {
				var value string
				err := env.pool.QueryRow(env.ctx, `SELECT jsonb_build_object(
     'worker',worker_id,'generation',claim_generation,'tip',checkpoint_tip,'tip_at',checkpoint_tip_at,
     'retentions',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.run_id) FROM checkpoint_retentions c WHERE run_id=$1),
     'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM recovery_custody_holds h WHERE run_id=$1),
     'inputs',(SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM run_user_inputs i WHERE run_id=$1))::text FROM runs WHERE id=$1`, id).Scan(&value)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := snapshot()
			if _, err := svc.SubmitInputWithOptions(env.ctx, uuid.New(), id, "cancel", "", nil, SubmitInputOptions{}); !errors.Is(err, ErrRunNotFound) {
				t.Fatalf("foreign owner cancel = %v", err)
			}
			if snapshot() != before || statusOf(t, env, id) != "running" {
				t.Fatal("foreign owner changed run")
			}
			res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, id, "cancel", "owner cancel", nil, SubmitInputOptions{})
			if err != nil || !res.ServerSide {
				t.Fatalf("owner cancel=%+v, %v", res, err)
			}
			if statusOf(t, env, id) != "cancelled" {
				t.Fatal("cancel did not apply")
			}
			if snapshot() != before {
				t.Fatal("cancel changed affinity, retained data or buffered inputs (including cancel rows)")
			}
			switch shape {
			case "released":
				env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			case "pending switch":
				env.exec("UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=claim_generation WHERE id=$1", id)
			}
			for _, capable := range []bool{false, true} {
				w := store.Worker{ID: o.workerID, UserID: o.userID}
				if capable {
					w.ProtocolCapabilities = []string{capability.InputReceiptsV1}
				}
				for poll := 0; poll < 2; poll++ {
					got, err := svc.ConsumeInputs(env.ctx, w, id)
					if err != nil || got.TerminalCancelled == nil || got.TerminalCancelled.RunID != id || got.TerminalCancelled.Generation != 12 {
						t.Fatalf("poll=%+v, %v", got, err)
					}
					if len(got.Inputs) != 0 || got.Receipts || got.CredentialSwitch != nil {
						t.Fatalf("unexpected delivery=%+v", got)
					}
					if snapshot() != before {
						t.Fatal("poll mutated retained data or consumption")
					}
				}
			}
			if got, err := svc.ConsumeInputs(env.ctx, store.Worker{ID: uuid.New(), UserID: o.userID}, id); !errors.Is(err, ErrRunNotOwned) || got.TerminalCancelled != nil {
				t.Fatalf("foreign worker poll=%+v, %v", got, err)
			}
		})
	}
}
