package store_test

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2460: a Codex lead is checked by a CLAUDE child. These tests cover the store half:
// the opposite-family guarded writes, the runs_kind_shape arm, the migration Down, and the
// Claude-child claim and placement requirement for cross_check_codex_lead_v1.

// seedCrossCheckLead makes the fixture's queued run an auto-approved, plan-checked lead of
// the given harness, running on a fresh worker at generation 1.
func seedCrossCheckLead(fx *fleetFixture, harness string) (lead uuid.UUID, worker uuid.UUID) {
	fx.t.Helper()
	worker = fx.worker("lead-"+harness, capOf(4), false)
	lead = fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status='running',worker_id=$2,claim_generation=1,harness=$3,
		auto_approve=true,plan_cross_check_required=true WHERE id=$1`, lead, worker, harness)
	return lead, worker
}

func insertPlanCheck(fx *fleetFixture, lead, child uuid.UUID) (store.CrossCheck, error) {
	return fx.q.InsertPlanCrossCheck(fx.ctx, store.InsertPlanCrossCheckParams{
		LeadRunID: lead, LeadClaimGeneration: 1, Round: 1,
		PlanMd: pgtype.Text{String: "plan", Valid: true}, Milestones: []byte("[]"),
		RequiredCapabilities: []string{}, RequiredTools: []string{},
		SizeClass: pgtype.Text{String: "s", Valid: true}, BaseCommit: pgtype.Text{String: strings.Repeat("a", 40), Valid: true},
		PlanningDiff: pgtype.Text{String: "", Valid: true}, CandidateDigest: []byte("claude-checker-digest"),
		CheckerRunID: child, DeadlineAt: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Minute), Valid: true},
	})
}

// A Codex lead creates a claude child and a Claude lead a codex child; a same-family child
// is refused by BOTH guarded writes (the child INSERT and the check-row INSERT, which takes
// checker_harness from the child row).
func TestPlanCrossCheckOppositeFamilyGuardedWritesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, lead, child string
		allowed           bool
	}{
		{"codex lead creates a claude child", "codex", "claude", true},
		{"claude lead creates a codex child", "claude", "codex", true},
		{"claude lead refuses a claude child", "claude", "claude", false},
		{"codex lead refuses a codex child", "codex", "codex", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			lead, worker := seedCrossCheckLead(fx, tc.lead)
			childID := uuid.New()
			child, err := fx.q.CreatePlanCrossCheckChild(fx.ctx, store.CreatePlanCrossCheckChildParams{
				ChildID: childID, ChildHarness: tc.child, LeadRunID: lead, UserID: fx.userID,
				WorkerID: pgU(worker), ClaimGeneration: 1, BudgetWallSeconds: 300})
			if !tc.allowed {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("same-family child create err=%v, want no rows", err)
				}
				var present bool
				if err := fx.pool.QueryRow(fx.ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id=$1)`, childID).Scan(&present); err != nil || present {
					t.Fatalf("refused child was inserted: present=%v err=%v", present, err)
				}
				// A same-family child can still be forced into the table by a raw writer
				// (runs_kind_shape admits both families); the check-row write must refuse it.
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,
					budget_wall_seconds,issue_title,issue_description)
					VALUES ($1,$2,$3,'cross_check',$4,$5,true,300,'c','c')`, childID, fx.userID, fx.repoID, lead, tc.child)
				if _, err := insertPlanCheck(fx, lead, childID); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("same-family check row err=%v, want no rows", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("opposite-family child: %v", err)
			}
			if child.Harness != tc.child || child.Kind != "cross_check" || !child.ReportOnly || child.TargetRunID != pgU(lead) {
				t.Fatalf("child shape: harness=%s kind=%s report_only=%v target=%v", child.Harness, child.Kind, child.ReportOnly, child.TargetRunID)
			}
			cc, err := insertPlanCheck(fx, lead, child.ID)
			if err != nil {
				t.Fatalf("check row: %v", err)
			}
			if cc.CheckerHarness.String != tc.child || !cc.CheckerRunID.Valid || cc.CheckerRunID.Bytes != child.ID {
				t.Fatalf("checker_harness=%q checker_run=%v, want %s from the child row", cc.CheckerHarness.String, cc.CheckerRunID, tc.child)
			}
		})
	}
}

// The guarded child write also needs a lead of a known family and the existing lead fences.
func TestPlanCrossCheckClaudeChildStillFencedByLeadStateLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	lead, worker := seedCrossCheckLead(fx, "codex")
	for name, sql := range map[string]string{
		"not auto-approved":   `UPDATE runs SET auto_approve=false WHERE id=$1`,
		"not plan-checked":    `UPDATE runs SET plan_cross_check_required=false WHERE id=$1`,
		"stale generation":    `UPDATE runs SET claim_generation=2 WHERE id=$1`,
		"claim released":      `UPDATE runs SET claim_released_at=now() WHERE id=$1`,
		"not running/claimed": `UPDATE runs SET status='awaiting_approval' WHERE id=$1`,
	} {
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='running',claim_generation=1,claim_released_at=NULL,
			auto_approve=true,plan_cross_check_required=true WHERE id=$1`, lead)
		mustExec(fx.ctx, t, fx.pool, sql, lead)
		if _, err := fx.q.CreatePlanCrossCheckChild(fx.ctx, store.CreatePlanCrossCheckChildParams{
			ChildID: uuid.New(), ChildHarness: "claude", LeadRunID: lead, UserID: fx.userID,
			WorkerID: pgU(worker), ClaimGeneration: 1, BudgetWallSeconds: 300}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: err=%v, want no rows", name, err)
		}
	}
}

// runs_kind_shape admits a claude cross_check child but still demands the rest of the arm.
func TestRunsKindShapeAdmitsClaudeCrossCheckLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	lead, _ := seedCrossCheckLead(fx, "codex")
	insert := func(harness string, reportOnly bool, budget any) error {
		_, err := fx.pool.Exec(fx.ctx, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,
			budget_wall_seconds,issue_title,issue_description) VALUES ($1,$2,$3,'cross_check',$4,$5,$6,$7,'c','c')`,
			uuid.New(), fx.userID, fx.repoID, lead, harness, reportOnly, budget)
		return err
	}
	if err := insert("claude", true, 300); err != nil {
		t.Fatalf("claude cross_check child refused by runs_kind_shape: %v", err)
	}
	if err := insert("codex", true, 300); err != nil {
		t.Fatalf("codex cross_check child: %v", err)
	}
	for name, err := range map[string]error{
		"not report-only":     insert("claude", false, 300),
		"missing wall budget": insert("claude", true, nil),
	} {
		if err == nil || !strings.Contains(err.Error(), "runs_kind_shape") {
			t.Fatalf("%s: err=%v, want a runs_kind_shape violation", name, err)
		}
	}
}

// Down must be executable with a pending Claude child: it settles the check failed/
// superseded with the lead's wait banked exactly once, deletes the child, keeps the
// check history with a NULL checker_run_id, and restores the Codex-only shape.
func TestCrossCheckClaudeCheckerMigrationDownLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "cross_check_claude_down_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("down-%s@e2e", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES ($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES ($1,$2,1,'g/r','https://forge.e2e/g/r','main',true)`, repo, conn)
	mustExec(ctx, t, pool, `INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,'w',$3,'online')`, worker, user, worker[:])
	iid := 0
	lead := func(harness string) uuid.UUID {
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,
			harness,auto_approve,plan_cross_check_required,claim_generation)
			VALUES ($1,$2,$3,$4,$5,'lead','body','running',$6,true,true,1)`, id, user, repo, worker, iid, harness)
		return id
	}
	child := func(leadID uuid.UUID, harness string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,
			issue_title,issue_description,status) VALUES ($1,$2,$3,'cross_check',$4,$5,true,300,'c','c','running')`,
			id, user, repo, leadID, harness)
		return id
	}
	check := func(leadID, childID uuid.UUID, harness, verdict string, credited bool) {
		reason := "NULL"
		if verdict != "pending" {
			reason = "'" + verdict + "'"
		}
		mustExec(ctx, t, pool, `INSERT INTO cross_checks (lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,
			size_class,base_commit,candidate_digest,checker_harness,verdict,reason_class,deadline_at,created_at,wait_credited)
			VALUES ($1,$2,'plan',1,1,'plan','[]','s',repeat('a',40),$3,$4,$5,`+reason+`,now()+interval '30 minutes',
			now()-interval '90 seconds',$6)`, leadID, childID, []byte("digest-"+leadID.String()), harness, verdict, credited)
	}
	pendingLead, creditedLead, decidedLead, codexLead := lead("codex"), lead("codex"), lead("codex"), lead("claude")
	pendingChild, creditedChild, decidedChild, codexChild := child(pendingLead, "claude"), child(creditedLead, "claude"),
		child(decidedLead, "claude"), child(codexLead, "codex")
	check(pendingLead, pendingChild, "claude", "pending", false)
	check(creditedLead, creditedChild, "claude", "pending", true)
	check(decidedLead, decidedChild, "claude", "approve", true)
	check(codexLead, codexChild, "codex", "pending", false)

	if err := store.MigrateDownTo(ctx, dsn, 312); err != nil {
		t.Fatalf("Down with a pending Claude child: %v", err)
	}
	var exists bool
	for _, id := range []uuid.UUID{pendingChild, creditedChild, decidedChild} {
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id=$1)`, id).Scan(&exists); err != nil || exists {
			t.Fatalf("claude child %s survived Down: exists=%v err=%v", id, exists, err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id=$1)`, codexChild).Scan(&exists); err != nil || !exists {
		t.Fatalf("codex child must survive Down: exists=%v err=%v", exists, err)
	}
	type row struct {
		verdict, reason string
		credited        bool
		checker         pgtype.UUID
		decided         pgtype.Timestamptz
		paused          int32
	}
	read := func(leadID uuid.UUID) row {
		var r row
		var reason pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT cc.verdict,cc.reason_class,cc.wait_credited,cc.checker_run_id,cc.decided_at,lead.budget_paused_seconds
			FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id WHERE cc.lead_run_id=$1`, leadID).
			Scan(&r.verdict, &reason, &r.credited, &r.checker, &r.decided, &r.paused); err != nil {
			t.Fatal(err)
		}
		r.reason = reason.String
		return r
	}
	p := read(pendingLead)
	if p.verdict != "failed" || p.reason != "superseded" || !p.credited || p.checker.Valid || !p.decided.Valid {
		t.Fatalf("pending claude check not settled: %+v", p)
	}
	if p.paused < 85 || p.paused > 150 {
		t.Fatalf("lead wait banked %ds, want about the 90s the check was pending (exactly once)", p.paused)
	}
	if c := read(creditedLead); c.verdict != "failed" || c.reason != "superseded" || c.paused != 0 {
		t.Fatalf("already-credited wait must not be banked again: %+v", c)
	}
	if d := read(decidedLead); d.verdict != "approve" || d.reason != "approve" || d.checker.Valid || d.paused != 0 {
		t.Fatalf("decided history must be kept with a NULL checker: %+v", d)
	}
	if k := read(codexLead); k.verdict != "pending" || !k.checker.Valid || k.paused != 0 {
		t.Fatalf("codex check must be untouched: %+v", k)
	}
	// The restored shape is Codex-only again.
	_, err = pool.Exec(ctx, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
		VALUES ($1,$2,$3,'cross_check',$4,'claude',true,300,'c','c')`, uuid.New(), user, repo, pendingLead)
	if err == nil || !strings.Contains(err.Error(), "runs_kind_shape") {
		t.Fatalf("down shape still admits claude: %v", err)
	}
	// Up again re-admits it.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description)
		VALUES ($1,$2,$3,'cross_check',$4,'claude',true,300,'c','c')`, uuid.New(), user, repo, pendingLead)
}

// ---- claim and placement copies -------------------------------------------------------

// A queued Claude checker child of a Codex lead, with a pending first-round check.
func seedClaudeChild(fx *fleetFixture, leadWorker uuid.UUID, round int) (child, lead uuid.UUID) {
	fx.t.Helper()
	lead = fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status='running',worker_id=$2,claim_generation=1,harness='codex',
		auto_approve=true,plan_cross_check_required=true WHERE id=$1`, lead, leadWorker)
	child = fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,harness='claude',
		report_only=true,budget_wall_seconds=1800,required_capabilities='{}',required_tools='{}',
		status_since=now()-interval '1 hour' WHERE id=$1`, child, lead)
	limit, enabled := 0, false
	if round > 1 {
		limit, enabled = 2, true
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
			(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
			 checker_harness,verdict,reason_class,decided_at,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
			VALUES ($1,'plan',1,1,'plan','[]','s',repeat('a',40),$2,'claude','revise','revise',now(),
			 now()+interval '5 minutes',true,2)`, lead, []byte("first"))
	}
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
		(lead_run_id,checker_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,
		 checker_harness,deadline_at,automatic_rounds_enabled,automatic_revision_limit)
		VALUES ($1,$2,'plan',$3,1,'plan','[]','s',repeat('a',40),$4,'claude',now()+interval '5 minutes',$5,$6)`,
		lead, child, round, []byte("second"), enabled, limit)
	return child, lead
}

func claudeCheckerProtocols(withCodexLead bool, round int) []string {
	caps := []string{capability.CrossCheckV1}
	if round > 1 {
		caps = append(caps, capability.CrossCheckRoundsV1)
	}
	if withCodexLead {
		caps = append(caps, capability.CrossCheckCodexLeadV1)
	}
	return caps
}

// A worker without cross_check_codex_lead_v1 is never counted as placement for, and never
// suppresses provisioning for, a Claude child: CountOnlineWorkersClaimableForRun, the
// capability-gap trigger and the saturation trigger all apply the requirement.
func TestPlanCrossCheckClaudeChildPlacementCopiesLiveDB(t *testing.T) {
	for _, round := range []int{1, 2} {
		for _, tc := range []struct {
			name string
			with bool
		}{{"without the capability", false}, {"with the capability", true}} {
			t.Run(fmt.Sprintf("round %d/%s", round, tc.name), func(t *testing.T) {
				fx := newFleetFixture(t)
				optInEphemeral(fx)
				t.Cleanup(func() {
					mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
					mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID)
				})
				w := fx.worker("persistent", capOf(1), false)
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, claudeCheckerProtocols(tc.with, round))
				// The lead runs on a separate, capability-less worker so it does not occupy w.
				child, _ := seedClaudeChild(fx, fx.worker("lead-host", nil, false), round)
				iv := leaseInterval(leaseTwoHours)
				want := int64(0)
				if tc.with {
					want = 1
				}
				if got := claimableCount(fx, child, iv); got != want {
					t.Fatalf("claimable=%d, want %d", got, want)
				}
				if gap := listedUnplaceable(fx, 1000, iv)[child]; gap != !tc.with {
					t.Fatalf("capability-gap trigger listed=%v, want %v", gap, !tc.with)
				}
				// A capable but full worker is saturation; an incapable one stays a gap.
				fx.holdActive(w, 1)
				if sat := listedSaturation(fx, 1000, iv)[child]; sat != tc.with {
					t.Fatalf("saturation trigger listed=%v, want %v", sat, tc.with)
				}
			})
		}
	}
}

// ClaimRun's own gate: only a worker advertising the capability claims a Claude child.
func TestPlanCrossCheckClaudeChildClaimGateLiveDB(t *testing.T) {
	for _, round := range []int{1, 2} {
		for _, with := range []bool{false, true} {
			t.Run(fmt.Sprintf("round %d/capability=%v", round, with), func(t *testing.T) {
				fx := newFleetFixture(t)
				t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
				w := fx.worker("persistent", capOf(4), false)
				caps := claudeCheckerProtocols(with, round)
				mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, caps)
				child, _ := seedClaudeChild(fx, w, round)
				now := time.Now()
				got, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
					CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
					CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
					HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
					WorkerCaps:               []string{}, WorkerProtocolCaps: caps, CapabilityAware: false})
				if with {
					if err != nil || got.ID != child || got.Harness != "claude" {
						t.Fatalf("capable worker could not claim the claude child: run=%s err=%v", got.ID, err)
					}
					return
				}
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("worker without the capability claimed: run=%s err=%v", got.ID, err)
				}
			})
		}
	}
}

// The peer-deferral mirror inside ClaimRun must apply the same requirement: a busy capable
// claimant defers a Claude child to an idle peer only when that peer could actually take it.
func TestPlanCrossCheckClaudeChildPeerDeferralLiveDB(t *testing.T) {
	for _, peerCapable := range []bool{true, false} {
		t.Run(fmt.Sprintf("peer capable=%v", peerCapable), func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			a, b := fx.worker("busy", capOf(2), false), fx.worker("idle", capOf(2), false)
			aCaps, bCaps := claudeCheckerProtocols(true, 1), claudeCheckerProtocols(peerCapable, 1)
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", a, aCaps)
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", b, bCaps)
			child, _ := seedClaudeChild(fx, a, 1)
			// The lead already occupies one of a's two slots (1/2): busy next to the idle peer, yet
			// still holding the free slot a run-lane child needs. Do not fill a to 2/2.
			now := time.Now()
			claim := func(w uuid.UUID, caps []string) (store.Run, error) {
				return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
					CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
					CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
					HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
					WorkerCaps:               []string{}, WorkerProtocolCaps: caps, CapabilityAware: false})
			}
			got, err := claim(a, aCaps)
			if peerCapable {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("busy claimant must defer to a capable idle peer: run=%s err=%v", got.ID, err)
				}
				if got, err = claim(b, bCaps); err != nil || got.ID != child {
					t.Fatalf("capable idle peer must claim: run=%s err=%v", got.ID, err)
				}
				return
			}
			// An incapable peer is no deferral target, so the busy capable worker claims.
			if err != nil || got.ID != child {
				t.Fatalf("incapable peer must not suppress the claim: run=%s err=%v", got.ID, err)
			}
		})
	}
}

// The requirement is written in all six runtime.sql claim and placement copies, next to the
// cross_check_v1 conjunct each mirrors. The generated Go holds the text that executes.
func TestCrossCheckCodexLeadConjunctPresentInEverySQLCopy(t *testing.T) {
	raw, err := os.ReadFile("runtime.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	base := regexp.MustCompile(`'cross_check_v1' = ANY\(`).FindAllStringIndex(src, -1)
	conj := regexp.MustCompile(`AND \(NOT \((\w+)\.kind = 'cross_check' AND \w+\.harness = 'claude'\)\s+OR 'cross_check_codex_lead_v1' = ANY\(`).FindAllStringSubmatchIndex(src, -1)
	if len(base) != 6 {
		t.Fatalf("cross_check_v1 copies = %d, want 6 (claim, peer mirror, count, capability gap, two saturation arms)", len(base))
	}
	if len(conj) != len(base) {
		t.Fatalf("cross_check_codex_lead_v1 conjunct in %d copies, want %d (one beside each cross_check_v1)", len(conj), len(base))
	}
	for i := range base {
		// Each conjunct directly follows its cross_check_v1 line.
		if gap := conj[i][0] - base[i][1]; gap < 0 || gap > 120 {
			t.Fatalf("copy %d: conjunct is %d bytes from its cross_check_v1 line", i, gap)
		}
	}
}
