package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func codeFixture(t *testing.T) crossCheckContentionFixture {
	t.Helper()
	f := laneFixture(t)
	f.env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", f.aliasID)
	f.env.exec("DELETE FROM cross_checks WHERE lead_run_id=$1", f.lead)
	f.env.exec("DELETE FROM runs WHERE id=$1", f.runID)
	f.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,'cross_check_code_v1') WHERE id=$1", f.workerID)
	f.env.exec("UPDATE runs SET worker_id=$2,code_cross_check_required=true,plan_cross_check_required=false,auto_approve=false WHERE id=$1", f.lead, f.workerID)
	return f
}

func TestCodeCrossCheckOwnWorkerAdmissionLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		worker     store.Worker
		generation int64
		base       string
	}{
		{store.Worker{ID: uuid.New(), UserID: w.UserID}, 1, base}, {w, 2, base}, {w, 1, strings.Repeat("A", 40)},
	} {
		if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, tc.worker, f.lead, tc.generation, tc.base, head); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid submission: %v", err)
		}
	}
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head)
	if err != nil || cc.Outcome.String != "pending" || !cc.CheckerRunID.Valid {
		t.Fatalf("submit: %+v %v", cc, err)
	}
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	f.runID = child
	if p := laneClaim(t, f, "run", nil); p != nil {
		t.Fatal("code child escaped run lane")
	}
	foreignID := uuid.New()
	f.env.exec("INSERT INTO workers(id,user_id,name,token_hash,status,max_cross_check_slots,protocol_capabilities) SELECT $2,user_id,'foreign',$2::uuid::text::bytea,'online',1,protocol_capabilities FROM workers WHERE id=$1", f.workerID, foreignID)
	foreign := wkrRow(t, f.env, foreignID)
	if p, err := f.svc.ClaimCrossCheck(f.env.ctx, foreign, nil); err != nil || p != nil {
		t.Fatalf("foreign worker claimed: %v %v", p, err)
	}
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil || p.RunID != child.String() || p.CrossCheck == nil || p.CrossCheck.Stage != "code" ||
		p.CrossCheck.HeadCommit != head || !mustRun(t, f.env, child).CrossCheckLane {
		t.Fatalf("own lane claim: %+v", p)
	}
	if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, child, p.ClaimGeneration+1, "completed", "", nil); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("stale verdict accepted")
	}
	if _, err := f.svc.DecideCodeCrossCheck(f.env.ctx, foreign, child, p.ClaimGeneration, "completed", "", nil); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("foreign verdict accepted")
	}
	if _, err := f.svc.DecidePlanCrossCheck(f.env.ctx, w, child, p.ClaimGeneration, "approve", "approve", []byte(`{"summary":"ok","items":[]}`)); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatal("code child approved plan")
	}
	settled, err := f.svc.DecideCodeCrossCheck(f.env.ctx, w, child, p.ClaimGeneration, "completed", "", []CodeCrossCheckFinding{{ID: "F-1", Severity: "major", Title: "verify"}})
	if err != nil || settled.Outcome.String != "completed" {
		t.Fatalf("settlement: %v", err)
	}
	before := mustRun(t, f.env, f.lead)
	if before.Status != "running" || before.AutoApprove {
		t.Fatal("code verdict gained approval or parked lead")
	}
	retry, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, head)
	if err != nil || retry.ID != cc.ID {
		t.Fatal("same candidate retry replaced row")
	}
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, base, strings.Repeat("c", 40)); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatal("changed snapshot retry accepted")
	}
	f.env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", f.lead)
	status, err := f.svc.CodeCrossCheckStatus(f.env.ctx, w, f.lead, 2)
	if err != nil || !status.InterruptedAt.Valid || status.Outcome.String != "failed" {
		t.Fatalf("interrupted status: %+v %v", status, err)
	}
	if _, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 2, base, head); !errors.Is(err, ErrCrossCheckInterrupted) {
		t.Fatal("interrupted attempt reran")
	}
}

func TestCodeCrossCheckWorkerVersionRefusalLiveDB(t *testing.T) {
	for _, reason := range []string{"snapshot_failed", "worker_unsupported", "checker_unavailable", "family_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			f := codeFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			var cc store.CrossCheck
			var err error
			if reason == "snapshot_failed" || reason == "worker_unsupported" {
				cc, err = f.svc.RecordCodeCrossCheckFailure(f.env.ctx, w, f.lead, 1, "", "", reason)
			} else {
				if reason == "checker_unavailable" {
					f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_code_v1') WHERE id=$1", w.ID)
				} else {
					f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind IN ('openai_api_key','codex_auth')", w.UserID)
				}
				w = wkrRow(t, f.env, w.ID)
				cc, err = f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
			}
			if err != nil || cc.Outcome.String != "failed" || cc.CheckerRunID.Valid {
				t.Fatalf("persisted refusal: %+v %v", cc, err)
			}
			if (reason == "snapshot_failed" || reason == "worker_unsupported") != (!cc.HeadCommit.Valid && !cc.BaseCommit.Valid) {
				t.Fatal("failure snapshot identity mismatch")
			}
			if lead := mustRun(t, f.env, f.lead); lead.Status != "running" || lead.AutoApprove {
				t.Fatal("failure changed lead authority")
			}
		})
	}
}

// The lead's worker may hold a cooled origin and the lead's own half-open
// reservation; the code child reads the local snapshot only, so it is claimed
// without touching that cooldown or freezing an origin mapping.
func TestCodeCrossCheckClaimUnderLeadOriginReservationLiveDB(t *testing.T) {
	f := codeFixture(t)
	w := wkrRow(t, f.env, f.workerID)
	f.env.exec(`INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
 SELECT r.user_id,$2,a.origin,now()+interval '1 hour','transient_clone_fetch_exhausted',now()
 FROM runs r JOIN authorized_forge_origins a ON a.repo_id=r.repo_id AND a.user_id=r.user_id
 WHERE r.id=$1 AND a.origin IS NOT NULL`, f.lead, f.workerID)
	f.env.exec(`UPDATE forge_origin_cooldowns SET reservation_run_id=$2,reservation_generation=1,reservation_incarnation=incarnation,
 reservation_version=observation_version,reservation_observation_version=observation_version WHERE worker_id=$1`, f.workerID, f.lead)
	cooldown := func() (rows int, state string) {
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*), COALESCE(string_agg(
 concat_ws('/',reservation_run_id,reservation_generation,observation_version,cooled_until > now()),','),'')
 FROM forge_origin_cooldowns WHERE worker_id=$1`, f.workerID).Scan(&rows, &state); err != nil {
			t.Fatal(err)
		}
		return rows, state
	}
	rows, before := cooldown()
	if rows != 1 {
		t.Fatalf("fixture cooldown rows = %d", rows)
	}
	cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, w, f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil || !cc.CheckerRunID.Valid {
		t.Fatalf("submit: %+v %v", cc, err)
	}
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	f.runID = child
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil || p.RunID != child.String() || p.CrossCheck == nil || p.CrossCheck.Stage != "code" {
		t.Fatalf("code child stranded by the lead's origin reservation: %+v", p)
	}
	if _, after := cooldown(); after != before {
		t.Fatalf("code child changed the cooldown: %s -> %s", before, after)
	}
	var mapped int
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT count(*) FROM run_forge_origins WHERE run_id=$1", child).Scan(&mapped); err != nil || mapped != 0 {
		t.Fatalf("code child froze an origin mapping: %d %v", mapped, err)
	}
}
