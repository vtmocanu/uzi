package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestCodeCrossCheckLocalLaneEligibilityLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := codeCrossCheckFixture(t)
	f := fx.f
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, "pending", nil); err != nil {
		t.Fatal(err)
	}
	foreign := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash,max_cross_check_slots,protocol_capabilities)
 SELECT $1::uuid,user_id,'foreign',convert_to($1::uuid::text,'UTF8'),1,protocol_capabilities FROM workers WHERE id=$2`, foreign, f.workerID)
	for _, tc := range []struct {
		name, lane      string
		worker          uuid.UUID
		available, want bool
	}{
		{"local lane", "cross_check", f.workerID, true, true},
		{"local strict health", "any", f.workerID, false, true},
		{"no run fallback", "run", f.workerID, true, false},
		{"no strict run fallback", "run", f.workerID, false, false},
		{"foreign claim", "cross_check", foreign, true, false},
		{"no foreign health escape", "any", foreign, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var eligible bool
			err := f.pool.QueryRow(ctx, `SELECT COALESCE(fn_cross_check_child_eligible(w,r,$3,$4,'code',now(),now()+interval '1 day'),false)
  FROM workers w,runs r WHERE w.id=$1 AND r.id=$2`, tc.worker, fx.checkerID, tc.available, tc.lane).Scan(&eligible)
			if err != nil {
				t.Fatal(err)
			}
			if eligible != tc.want {
				t.Fatalf("eligible=%v want=%v", eligible, tc.want)
			}
		})
	}
	for _, caps := range [][]string{{"cross_check_lane_v1"}, {"cross_check_code_v1"}} {
		mustExec(ctx, t, f.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, caps)
		for _, availability := range []bool{true, false} {
			var eligible bool
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(fn_cross_check_child_eligible(w,r,$3,'any','code',now(),now()),false)
    FROM workers w,runs r WHERE w.id=$1 AND r.id=$2`, f.workerID, fx.checkerID, availability).Scan(&eligible); err != nil {
				t.Fatal(err)
			}
			if eligible {
				t.Fatalf("incomplete capabilities admitted child: %v availability=%v", caps, availability)
			}
		}
	}
}

// A code checker reads the lead's local snapshot and never contacts the forge, so a
// cooled origin or the lead's own half-open reservation must not strand it.
func TestCodeCrossCheckIgnoresForgeOriginAdmissionLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := codeCrossCheckFixture(t)
	f := fx.f
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, "pending", nil); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, f.pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
 SELECT r.user_id,$2,a.origin,now()+interval '1 hour','transient_clone_fetch_exhausted',now()
 FROM runs r JOIN authorized_forge_origins a ON a.repo_id=r.repo_id AND a.user_id=r.user_id
 WHERE r.id=$1 AND a.origin IS NOT NULL`, fx.checkerID, f.workerID)
	for _, tc := range []struct{ name, sql string }{
		{"cooled origin", ""},
		{"lead reservation", `UPDATE forge_origin_cooldowns SET cooled_until=now()-interval '1 hour',
 reservation_run_id=$2,reservation_generation=1,reservation_incarnation=incarnation,
 reservation_version=observation_version,reservation_observation_version=observation_version WHERE worker_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sql != "" {
				mustExec(ctx, t, f.pool, tc.sql, f.workerID, f.runID)
			}
			var rows int
			var eligible bool
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1),
  COALESCE(fn_cross_check_child_eligible(w,r,true,'cross_check','code',now(),now()+interval '1 day'),false)
  FROM workers w,runs r WHERE w.id=$1 AND r.id=$2`, f.workerID, fx.checkerID).Scan(&rows, &eligible); err != nil {
				t.Fatal(err)
			}
			if rows != 1 || !eligible {
				t.Fatalf("code checker stranded by forge origin admission: rows=%d eligible=%v", rows, eligible)
			}
		})
	}
}

func TestCodeCrossCheckUnsupportedCompletionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                                        string
		consent, capable, reportOnly, notCode, want bool
	}{
		{"old worker opted in", true, false, false, false, true},
		{"new worker", true, true, false, false, false},
		{"consent disabled", false, false, false, false, false},
		{"report only", true, false, true, false, false},
		{"not code", true, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := codeCrossCheckFixture(t)
			f := fx.f
			var caps []string
			if tc.capable {
				caps = []string{"cross_check_code_v1"}
			} else {
				caps = []string{"cross_check_lane_v1"}
			}
			mustExec(ctx, t, f.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, caps)
			var verdict any
			if tc.notCode {
				verdict = "not_code"
			}
			mustExec(ctx, t, f.pool, "UPDATE runs SET code_cross_check_required=$2,report_only=$3,fix_verdict=$4 WHERE id=$1", f.runID, tc.consent, tc.reportOnly, verdict)
			mustExec(ctx, t, f.pool, "UPDATE runs SET status='completed' WHERE id=$1", f.runID)
			var exists bool
			if err := f.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM cross_checks WHERE lead_run_id=$1 AND stage='code')", f.runID).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("unsupported record exists=%v want=%v", exists, tc.want)
			}
			if exists {
				var shape bool
				if err := f.pool.QueryRow(ctx, `SELECT outcome='failed' AND reason_class='worker_unsupported'
    AND head_commit IS NULL AND base_commit IS NULL AND guidance_text=''
    AND guidance_digest=sha256(convert_to('','UTF8')) AND NOT repo_instructions_enabled
    AND repo_instructions_text='' AND repo_instructions_digest=sha256(convert_to('','UTF8'))
    FROM cross_checks WHERE lead_run_id=$1 AND stage='code'`, f.runID).Scan(&shape); err != nil {
					t.Fatal(err)
				}
				if !shape {
					t.Fatal("unsupported row has wrong frozen/no-snapshot shape")
				}
			}
		})
	}
}
