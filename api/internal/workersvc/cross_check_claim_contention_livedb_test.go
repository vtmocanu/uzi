package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type crossCheckContentionFixture struct {
	codexRunFixture
	env       codexTestEnv
	lead      uuid.UUID
	candidate PlanCrossCheckCandidate
}

func newCrossCheckContentionFixture(t *testing.T) crossCheckContentionFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	t.Cleanup(func() {
		env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
			released_at=now(),updated_at=now() WHERE user_id=$1 AND state='open'`, f.userID)
		env.exec(`DELETE FROM users WHERE id=$1`, f.userID)
	})
	resealBotPAT(t, env, f.userID)
	seedDefaultAnthropicToken(t, env, f.userID)
	original := mustRun(t, env, f.runID)
	lead := uuid.New()
	leadWorker := uuid.New()
	env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,$3,$4,'online')`,
		leadWorker, f.userID, "lead-"+leadWorker.String(), leadWorker[:])
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,
		status,harness,auto_approve,plan_cross_check_required,claim_generation)
		VALUES ($1,$2,$3,$4,42,'lead issue','lead body','running','claude',true,true,1)`,
		lead, f.userID, original.RepoID, leadWorker)
	env.exec(`UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,report_only=true,
		budget_wall_seconds=1800,status='queued',worker_id=NULL,claim_generation=0,priority=2,
		issue_title='lead issue',issue_description='lead body' WHERE id=$1`, f.runID, lead)
	c := PlanCrossCheckCandidate{PlanMd: "Review this plan", Milestones: json.RawMessage(`[]`),
		RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
		BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff --git a/file b/file"}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,deadline_at)
		VALUES ($1,'plan',1,1,$2,$3,'s',$4,$5,$6,$7,'codex',now()+interval '30 minutes')`,
		lead, c.PlanMd, c.Milestones, c.BaseCommit, c.PlanningDiff, digest, f.runID)
	env.exec(`UPDATE users SET default_codex_model=$2,default_codex_effort='high' WHERE id=$1`, f.userID, customCodexModel)
	env.exec(`UPDATE workers SET protocol_capabilities=$2,snapshot_register_nonce='nonce-U2',snapshot_epoch=0 WHERE id=$1`,
		f.workerID, []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2,
			capability.RecoveryArchiveV1, capability.CodexCustomModelV1})
	f.svc = New(env.q, env.box, testParams())
	f.svc.SetTxBeginner(env.pool)
	return crossCheckContentionFixture{f, env, lead, c}
}

// claim holds a second connection's trusted row lock through Service.Claim.
// Every call and rollback is bounded; release the lock before any assertions.
func (f crossCheckContentionFixture) claim(t *testing.T, snapshot bool, lockSQL string, lockID uuid.UUID) *ClaimPayload {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.env.ctx, 3*time.Second)
	defer cancel()
	var unlock func()
	if lockSQL != "" {
		tx, err := f.env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		unlock = func() {
			rollbackCtx, rollbackCancel := context.WithTimeout(f.env.ctx, 3*time.Second)
			defer rollbackCancel()
			if err := tx.Rollback(rollbackCtx); err != nil {
				t.Errorf("rollback held lock: %v", err)
			}
		}
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()
		var id uuid.UUID
		if err := tx.QueryRow(ctx, lockSQL, lockID).Scan(&id); err != nil {
			t.Fatal(err)
		}
	}
	var active *ActiveSnapshot
	if snapshot {
		active = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{}}
	}
	payload, err := f.svc.Claim(ctx, wkrRow(t, f.env, f.workerID), active)
	if unlock != nil {
		unlock()
		unlock = nil
	}
	if err != nil {
		t.Fatalf("Service.Claim must return without a deadline/lock error: %v", err)
	}
	return payload
}

func (f crossCheckContentionFixture) unchanged(t *testing.T, before store.Run) {
	t.Helper()
	after := mustRun(t, f.env, before.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("skipped/sibling run changed")
	}
	var holds int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1`, before.ID).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	if holds != 0 {
		t.Fatalf("skipped/report-only checker custody holds = %d, want zero", holds)
	}
	if before.Kind == "cross_check" && before.Status == "queued" {
		var untouched bool
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT checker_model IS NULL AND checker_effort IS NULL
			FROM cross_checks WHERE checker_run_id=$1`, before.ID).Scan(&untouched); err != nil {
			t.Fatal(err)
		}
		if !untouched {
			t.Fatal("skipped checker recorded execution model/effort")
		}
	}
}

func (f crossCheckContentionFixture) claimed(t *testing.T, p *ClaimPayload) {
	t.Helper()
	if p == nil || p.RunID != f.runID.String() || p.Kind != "cross_check" || p.ClaimGeneration != 1 {
		t.Fatal("expected own checker at generation one")
	}
	r := mustRun(t, f.env, f.runID)
	if r.Status != "claimed" || r.ClaimGeneration != p.ClaimGeneration || !r.WorkerID.Valid ||
		uuid.UUID(r.WorkerID.Bytes) != f.workerID || len(r.CodexCapHash) == 0 || r.CodexClaimEpoch <= 0 {
		t.Fatal("checker ownership/generation/capability not persisted")
	}
	if p.Secrets.Codex == nil || p.Secrets.Codex.Capability == "" || p.Secrets.Codex.AccessToken != f.accessToken ||
		p.Secrets.AnthropicOAuthToken != "" {
		t.Fatal("checker missing Codex credential/capability or carries Claude fallback")
	}
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != customCodexModel ||
		p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "high" ||
		p.CrossCheck == nil || p.CrossCheck.LeadRunID != f.lead.String() ||
		!reflect.DeepEqual(p.CrossCheck.PlanCrossCheckCandidate, f.candidate) ||
		p.IssueTitle != "lead issue" || p.IssueDescription != "lead body" {
		t.Fatal("checker model/effort or child context differs from stored candidate")
	}
	var model, effort string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT checker_model,checker_effort FROM cross_checks WHERE lead_run_id=$1`, f.lead).Scan(&model, &effort); err != nil {
		t.Fatal(err)
	}
	if model != *p.Config.DefaultModel || effort != *p.Config.DefaultEffort {
		t.Fatal("persisted checker model/effort differs from claim")
	}
	f.unchanged(t, r) // Includes the report-only custody exclusion after a real claim.
}

func TestCrossCheckServiceClaimContentionLiveDB(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, locked := range []string{"none", "parent", "cross-check", "checker", "unrelated-parent"} {
			t.Run(map[bool]string{false: "nil", true: "empty-snapshot"}[snapshot]+"/"+locked, func(t *testing.T) {
				f := newCrossCheckContentionFixture(t)
				before := mustRun(t, f.env, f.runID)
				lockSQL, lockID := "", f.lead
				switch locked {
				case "parent", "unrelated-parent":
					lockSQL = `SELECT id FROM runs WHERE id=$1 FOR UPDATE`
					if locked == "unrelated-parent" {
						lead := mustRun(t, f.env, f.lead)
						lockID = f.env.seedCodexRun(t, f.userID, uuid.UUID(lead.WorkerID.Bytes), uuid.UUID(before.RepoID.Bytes))
					}
				case "cross-check":
					lockSQL = `SELECT lead_run_id FROM cross_checks WHERE lead_run_id=$1 FOR UPDATE`
				case "checker":
					lockSQL, lockID = `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, f.runID
				}
				p := f.claim(t, snapshot, lockSQL, lockID)
				if locked == "none" || locked == "unrelated-parent" {
					f.claimed(t, p)
				} else {
					if p != nil {
						t.Fatal("locked checker delivered an executable payload")
					}
					f.unchanged(t, before)
				}
			})
		}
	}
}

func TestCrossCheckServiceClaimRefusalsLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"decided", `UPDATE cross_checks SET verdict='approve' WHERE lead_run_id=$1`},
		{"expired", `UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1`},
		{"stale-generation", `UPDATE runs SET claim_generation=2 WHERE id=$1`},
		{"settled-child", `UPDATE runs SET status='completed' WHERE target_run_id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			f.env.exec(tc.sql, f.lead)
			before := mustRun(t, f.env, f.runID)
			if f.claim(t, false, "", uuid.Nil) != nil {
				t.Fatal("refused checker delivered an executable payload")
			}
			f.unchanged(t, before)
		})
	}
}

func TestCrossCheckServiceClaimPriorityLiveDB(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "priority-winner", true: "blocked-parent-fallback"}[blocked], func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			checker := mustRun(t, f.env, f.runID)
			repo := uuid.UUID(checker.RepoID.Bytes)
			ordinary := f.env.seedCodexRun(t, f.userID, f.workerID, repo)
			f.env.exec(`UPDATE runs SET issue_iid=2 WHERE id=$1`, ordinary)
			sibling := f.env.seedCodexRun(t, f.userID, f.workerID, repo)
			f.env.exec(`UPDATE runs SET issue_iid=3 WHERE id=$1`, sibling)
			f.env.exec(`UPDATE runs SET status='queued',worker_id=NULL,harness='claude',priority=1,
				created_at=now()-interval '2 hours' WHERE id=$1`, ordinary)
			f.env.exec(`UPDATE runs SET status='queued',worker_id=NULL,harness='claude',priority=1,
				created_at=now()-interval '1 hour' WHERE id=$1`, sibling)
			ordinaryBefore, siblingBefore := mustRun(t, f.env, ordinary), mustRun(t, f.env, sibling)
			lockSQL := ""
			if blocked {
				lockSQL = `SELECT id FROM runs WHERE id=$1 FOR UPDATE`
			}
			p := f.claim(t, false, lockSQL, f.lead)
			if blocked {
				if p == nil || p.RunID != ordinary.String() || p.Kind != "issue" || p.ClaimGeneration != 1 {
					t.Fatal("blocked checker did not fall back to FIFO ordinary run")
				}
				f.unchanged(t, checker)
			} else {
				f.claimed(t, p)
				f.unchanged(t, ordinaryBefore)
			}
			f.unchanged(t, siblingBefore)
			after := mustRun(t, f.env, f.runID)
			if blocked && (!bytes.Equal(checker.CodexCapHash, after.CodexCapHash) || checker.CodexClaimEpoch != after.CodexClaimEpoch) {
				t.Fatal("skipped checker capability changed")
			}
		})
	}
}
