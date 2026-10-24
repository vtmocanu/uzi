package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// These are deliberately SQL-composed publication phases. The real lost-ACK
// network publication is covered by the combined workersvc proof; this matrix
// isolates resume/claim/State composition over the resulting durable evidence.
func TestTaskCheckpointSourceAffineAdmissionPhasesLiveDB(t *testing.T) {
	for _, phase := range []string{"known first refusal", "confirmed anchor", "lost ACK"} {
		t.Run(phase, func(t *testing.T) {
			f := newTaskCheckpointHandlerFixture(t)
			gen := f.firstGeneration
			var wantTip *string
			wantAttempt := 0
			switch phase {
			case "confirmed anchor":
				tip := settlePushed
				wantTip = &tip
				f.exec("UPDATE runs SET checkpoint_tip=$2,checkpoint_tip_at=now() WHERE id=$1", f.run, tip)
			case "lost ACK":
				wantAttempt = 1
				f.exec(`INSERT INTO checkpoint_publish_attempts(id,run_id,branch,ref,tip,reconcile_ready_at)
					VALUES($1,$2,$3,$4,$5,now())`, uuid.New(), f.run, f.childKey,
					"refs/uzi-checkpoints/"+f.childKey, settlePushed)
				f.fake.mu.Lock()
				f.fake.head = settlePushed
				f.fake.mu.Unlock()
			}
			var tip *string
			var attempts, ready int
			if err := f.pool.QueryRow(f.ctx, `SELECT checkpoint_tip,
				(SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1),
				(SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1
					AND branch=$2 AND ref=$3 AND tip=$4 AND reconcile_ready_at IS NOT NULL)
				FROM runs WHERE id=$1`, f.run, f.childKey, "refs/uzi-checkpoints/"+f.childKey, settlePushed).
				Scan(&tip, &attempts, &ready); err != nil {
				t.Fatal(err)
			}
			if (tip == nil) != (wantTip == nil) || (tip != nil && *tip != *wantTip) ||
				attempts != wantAttempt || ready != wantAttempt {
				t.Fatalf("phase=%s tip=%v attempts=%d ready=%d", phase, tip, attempts, ready)
			}
			if phase == "lost ACK" {
				// Exercise the ref fake, then establish a new call baseline. This
				// is a remote-state precondition, not another network-push proof.
				got, err := f.lf.RefHead(f.ctx, 7, "refs/uzi-checkpoints/"+f.childKey)
				if err != nil || got != settlePushed {
					t.Fatalf("lost ACK ref=%s err=%v", got, err)
				}
			}
			forgeCalls := f.lf.total()
			f.exec(`UPDATE runs SET status='paused',status_since=now(),started_at=now(),
				budget_wall_seconds=3600,claim_released_at=NULL,required_capabilities='{}' WHERE id=$1`, f.run)
			// The registered row is incapable, while this previously obtained
			// snapshot still claims both protocols. Claim must refresh the row.
			staleCapable := f.worker()
			f.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1",
				f.workerA, []string{capability.RecoveryArchiveV1})
			holdsBefore := f.custodySnapshot()
			token := cliMintToken(t, f.pool, f.user, "user")
			rec := bearerReq(f.router, http.MethodPost, "/api/runs/"+f.run.String()+"/resume-now", token)
			if rec.Code != http.StatusOK {
				t.Fatalf("owner resume=%d %s", rec.Code, rec.Body.String())
			}
			var resume struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resume); err != nil {
				t.Fatal(err)
			}
			r := f.runRow()
			if resume.Run.Status != "queued" || r.Status != "queued" || !r.WorkerID.Valid ||
				uuid.UUID(r.WorkerID.Bytes) != f.workerA || r.ClaimGeneration != gen || r.ClaimReleasedAt.Valid ||
				string(r.CheckpointCoordinates) != string(f.coordinates) {
				t.Fatalf("resume granted/changed execution authority: status=%s worker=%+v gen=%d released=%v",
					r.Status, r.WorkerID, r.ClaimGeneration, r.ClaimReleasedAt.Valid)
			}
			var audits int
			if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", f.run).
				Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("resume audit=%d err=%v", audits, err)
			}
			if f.custodySnapshot() != holdsBefore {
				t.Fatal("owner resume changed custody")
			}

			before := f.authoritySnapshot()
			rec = bearerReq(f.router, http.MethodPost, "/api/worker/runs/claim", f.tokenA)
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("incapable source claim=%d %s, want idle", rec.Code, rec.Body.String())
			}
			if f.authoritySnapshot() != before {
				t.Fatal("idle source claim changed authority/custody")
			}
			p, err := f.wsvc.Claim(f.ctx, staleCapable, nil)
			if err != nil || p != nil {
				t.Fatalf("stale capable snapshot delivered=%v err=%v", p != nil, err)
			}
			if f.authoritySnapshot() != before {
				t.Fatal("stale worker snapshot changed authority/custody")
			}

			// A compatible peer cannot take a fresh source-affine continuation.
			rec = bearerReq(f.router, http.MethodPost, "/api/worker/runs/claim", f.tokenB)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("peer stole source-affine run: %d %s", rec.Code, rec.Body.String())
			}
			if f.authoritySnapshot() != before {
				t.Fatal("peer idle changed authority/custody")
			}

			// A queued row never accepts a delayed running report, independently
			// of protocol support. A legitimate resume must first take a fresh claim.
			stateBody := fmt.Sprintf("{\"status\":\"running\",\"claim_generation\":%d}", gen)
			rec = bearerReqBody(f.router, http.MethodPost, "/api/worker/runs/"+f.run.String()+"/state", f.tokenA, stateBody)
			if rec.Code != http.StatusConflict {
				t.Fatalf("incapable running State=%d %s", rec.Code, rec.Body.String())
			}
			if f.authoritySnapshot() != before {
				t.Fatal("refused running State restored execution or changed custody")
			}

			f.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1",
				f.workerA, []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			rec = bearerReq(f.router, http.MethodPost, "/api/worker/runs/claim", f.tokenA)
			if rec.Code != http.StatusOK {
				t.Fatalf("compatible source claim=%d %s", rec.Code, rec.Body.String())
			}
			var claimed workersvc.ClaimPayload
			if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil {
				t.Fatal(err)
			}
			if claimed.RunID != f.run.String() || claimed.ClaimGeneration != gen+1 ||
				claimed.CheckpointWriteUnresolved == nil || *claimed.CheckpointWriteUnresolved != (phase == "lost ACK") ||
				(claimed.CheckpointTip == nil) != (wantTip == nil) ||
				(claimed.CheckpointTip != nil && *claimed.CheckpointTip != *wantTip) {
				t.Fatalf("compatible source run=%s generation=%d tip=%v unresolved=%v", claimed.RunID, claimed.ClaimGeneration, claimed.CheckpointTip, claimed.CheckpointWriteUnresolved)
			}
			f.assertCoordinates(&claimed)
			r = f.runRow()
			if r.Status != "claimed" || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerA ||
				r.ClaimGeneration != gen+1 || r.ClaimReleasedAt.Valid {
				t.Fatalf("compatible source durable authority status=%s worker=%+v gen=%d", r.Status, r.WorkerID, r.ClaimGeneration)
			}
			nextHold := f.holdID(f.run, claimed.ClaimGeneration)
			f.assertOpen(f.pred, f.rootHold, f.sibWork, nextHold)
			stateBody = fmt.Sprintf("{\"status\":\"running\",\"claim_generation\":%d}", claimed.ClaimGeneration)
			rec = bearerReqBody(f.router, http.MethodPost, "/api/worker/runs/"+f.run.String()+"/state", f.tokenA, stateBody)
			if rec.Code != http.StatusOK || f.runStatus() != "running" {
				t.Fatalf("compatible running State=%d %s status=%s", rec.Code, rec.Body.String(), f.runStatus())
			}
			// An answered question is a valid SAME-generation authority restore.
			// Unlike a queued row, it reaches SetRunRunning and the DB protocol
			// admission trigger. Only stored worker support changes between controls.
			question := "task-checkpoint-" + uuid.NewString()
			park := fmt.Sprintf("{\"status\":\"awaiting_input\",\"claim_generation\":%d,\"open_question_id\":%q}", claimed.ClaimGeneration, question)
			rec = bearerReqBody(f.router, http.MethodPost, "/api/worker/runs/"+f.run.String()+"/state", f.tokenA, park)
			if rec.Code != http.StatusOK || f.runStatus() != "awaiting_input" {
				t.Fatalf("question park=%d %s", rec.Code, rec.Body.String())
			}
			f.exec(`INSERT INTO run_user_inputs(run_id,kind,body,question_id,consumed_at,applied_at,disposition)
 VALUES($1,'answer','{}',$2,now(),now(),'applied')`, f.run, question)
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1",
				f.workerA, []string{capability.RecoveryArchiveV1})
			questionBefore := f.authoritySnapshot()
			rec = bearerReqBody(f.router, http.MethodPost, "/api/worker/runs/"+f.run.String()+"/state", f.tokenA, stateBody)
			if rec.Code != http.StatusInternalServerError || f.authoritySnapshot() != questionBefore {
				t.Fatalf("incapable answered-question restore=%d changed_authority=%v", rec.Code, f.authoritySnapshot() != questionBefore)
			}
			f.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1",
				f.workerA, []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			questionCustody := f.custodySnapshot()
			rec = bearerReqBody(f.router, http.MethodPost, "/api/worker/runs/"+f.run.String()+"/state", f.tokenA, stateBody)
			if rec.Code != http.StatusOK || f.runStatus() != "running" ||
				f.runRow().ClaimGeneration != claimed.ClaimGeneration ||
				string(f.runRow().CheckpointCoordinates) != string(f.coordinates) || f.custodySnapshot() != questionCustody {
				t.Fatalf("compatible answered-question restore=%d status=%s", rec.Code, f.runStatus())
			}
			if f.lf.total() != forgeCalls {
				t.Fatal("resume/admission/State unexpectedly used forge IO")
			}
		})
	}
}
