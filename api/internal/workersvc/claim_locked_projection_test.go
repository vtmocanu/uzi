package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These direct final-boundary tests use independently controlled locked rows and
// transaction reads. Public Claim/preunit overlay coverage lives in the LiveDB file.
func TestClaimLockedProjectionFinalGroup(t *testing.T) {
	fs, svc, run, identity := finishFixture(t)
	at := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	presentation := uuid.New()
	locked := run
	locked.Status, locked.WorkerID = "claimed", pgconv.UUID(identity.workerID)
	locked.PlanMd = pgtype.Text{String: "locked candidate", Valid: true}
	locked.PlanSource = "agent"
	locked.SessionID = pgtype.Text{String: "predecessor", Valid: true}
	locked.GateRevision, locked.GatePresentationID = 4, pgconv.UUID(presentation)
	locked.GatePresentedPayload = []byte(`{"plan_md":"locked candidate","milestones":[],"required_tools":["snapshot-tool"],"required_capabilities":["snapshot-cap"],"size_class":null}`)
	locked.RequiredTools = []string{"live-tool"}
	locked.MilestonesCandidate = []byte("[]")
	locked.MilestonesFrozen = []byte(`[{"id":"frozen","title":"Frozen work"}]`)
	locked.AgentSource = pgtype.Text{String: "own", Valid: true}
	locked.AgentExclusions = []byte(`["excluded"]`)
	locked.ContractRevision = pgtype.Int4{Int32: 2, Valid: true}
	locked.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
	locked.CompletionContract = []byte(`{"profile":"structural","revision":2,"criteria":[{"id":"frozen.c1","milestone_id":"frozen","text":"Frozen work","audit":null,"finding_ids":[]}],"scope":{"in":[],"out":[{"milestone_id":"frozen","reason":"owner deferred","revision":2}]}}`)
	locked.CompletionNonClosingReason = pgtype.Text{String: "partial", Valid: true}
	locked.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
	locked.BudgetExtensionSeconds, locked.BudgetFinalizeSeconds = 20, 30
	locked.BudgetMaxIterations = pgtype.Int4{Int32: 9, Valid: true}
	locked.PlanCrossCheckGateReason = pgtype.Text{String: "revise", Valid: true}
	locked.PlanCrossCheckRequired, locked.CodeCrossCheckRequired = true, true
	locked.PlannedBaseCommit = pgtype.Text{String: "locked-base", Valid: true}
	locked.RequireBaseMatch = true
	fs.claimFinishLocked = &locked
	fs.claimCtx.HumanPlanApproved = true // Pooled approval must not authorize the final row.
	fs.claimFinishCtx = &store.GetRunClaimContextRow{}
	fs.claimFinishPlanSeq = 17
	fs.claimFinishPlanAt = pgtype.Timestamptz{Time: at, Valid: true}
	fs.claimFinishSessions = [][]byte{[]byte(`"predecessor"`)}
	payload, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{
		PlanApproved: true, AutoApprove: true, PlanSource: "seeded",
		Config: ClaimConfig{CompletionPending: &apitypes.CompletionPendingDTO{}},
	}, nil, identity)
	if err != nil || payload == nil {
		t.Fatalf("finish: payload=%v err=%v", payload, err)
	}
	if payload.PlanMd == nil || *payload.PlanMd != "locked candidate" || payload.PlanApproved || payload.AutoApprove || payload.PlanSource != "agent" ||
		payload.ResumePhase != "awaiting_approval" || payload.ResumePlanSeq != 17 || payload.ResumePlanAt == nil || !payload.ResumePlanAt.Equal(at) {
		t.Fatalf("locked plan/approval/resume not projected: %+v", payload)
	}
	if payload.SessionID != nil {
		t.Fatal("reconciliation predecessor escaped final reader")
	}
	if payload.ResumeGateRevision != 4 || payload.ResumeGatePresentationID == nil || *payload.ResumeGatePresentationID != presentation ||
		payload.ResumeGatePresented == nil || !reflect.DeepEqual(payload.ResumeGatePresented.RequiredTools, []string{"snapshot-tool"}) {
		t.Fatalf("gate snapshot lost: %+v", payload.ResumeGatePresented)
	}
	if payload.Milestones == nil || len(payload.Milestones) != 0 || payload.ExecutionMilestones == nil ||
		!reflect.DeepEqual(*payload.ExecutionMilestones, []Milestone{{ID: "frozen", Title: "Frozen work"}}) {
		t.Fatalf("candidate/frozen distinction lost: %v / %v", payload.Milestones, payload.ExecutionMilestones)
	}
	if payload.AgentSelection == nil || !reflect.DeepEqual(payload.AgentSelection.Exclusions, []string{"excluded"}) ||
		payload.Config.ContractRevision == nil || *payload.Config.ContractRevision != 2 ||
		payload.Config.CompletionContractVersion == nil || *payload.Config.CompletionContractVersion != 1 ||
		payload.Config.CompletionScope == nil || len(payload.Config.CompletionScope.Deferred) != 1 ||
		payload.Config.CompletionScope.Deferred[0].Title != "Frozen work" || payload.Config.CompletionPending != nil ||
		payload.Config.NonClosing == nil || payload.Config.NonClosing.Reason != "partial" ||
		payload.Config.RunTimeoutSeconds != 150 || payload.Config.MaxIterations != 9 ||
		payload.PlanCrossCheckGateReason == nil || *payload.PlanCrossCheckGateReason != "revise" ||
		!payload.PlanCrossCheckRequired || !payload.CodeCrossCheckRequired || !payload.RequireBaseMatch ||
		payload.PlannedBaseCommit == nil || *payload.PlannedBaseCommit != "locked-base" {
		t.Fatalf("locked selection/contract/budget missing: %+v", payload.Config)
	}
	if len(fs.claimFinishPlanAtParams) != 1 || fs.claimFinishPlanAtParams[0].PlanMd != "locked candidate" {
		t.Fatal("plan time query did not match locked plan")
	}
}

func TestClaimLockedProjectionClearsObsoleteFields(t *testing.T) {
	fs, svc, run, identity := finishFixture(t)
	stale := "obsolete"
	payload, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{
		PlanMd: &stale, AutoApprove: true, PlanApproved: true, PlanSource: "seeded",
		ResumePhase: "awaiting_approval", OpenQuestionID: &stale, SessionID: &stale,
		ResumePlanSeq: 42, ResumePlanAt: new(time.Time), ResumeGateRevision: 9,
		ResumeGatePresentationID: new(uuid.UUID), ResumeGatePresented: &GatePresentedRequirements{},
		Milestones: []Milestone{{ID: "old", Title: "Old"}}, ExecutionMilestones: &[]Milestone{},
		AgentSelection: &AgentSelection{}, PlanCrossCheckGateReason: &stale,
		Config: ClaimConfig{CompletionContractVersion: new(int), ContractRevision: new(int),
			CompletionScope: &CompletionScopeConfig{}, CompletionPending: &apitypes.CompletionPendingDTO{}, NonClosing: &CompletionNonClosing{}},
	}, nil, identity)
	if err != nil || payload == nil {
		t.Fatalf("finish: %v", err)
	}
	if payload.PlanMd != nil || payload.AutoApprove || payload.PlanApproved || payload.PlanSource != "" || payload.ResumePhase != "" ||
		payload.OpenQuestionID != nil || payload.SessionID != nil || payload.ResumePlanSeq != 0 || payload.ResumePlanAt != nil ||
		payload.ResumeGateRevision != 0 || payload.ResumeGatePresentationID != nil || payload.ResumeGatePresented != nil ||
		payload.Milestones != nil || payload.ExecutionMilestones != nil || payload.AgentSelection != nil || payload.PlanCrossCheckGateReason != nil ||
		payload.Config.CompletionContractVersion != nil || payload.Config.ContractRevision != nil || payload.Config.CompletionScope != nil ||
		payload.Config.CompletionPending != nil || payload.Config.NonClosing != nil {
		t.Fatalf("obsolete assembly fields survived: %+v", payload)
	}
	if fs.claimFailed != nil {
		t.Fatal("projection wrote terminal state")
	}
}

func TestClaimLockedProjectionApprovalAndPhaseControls(t *testing.T) {
	for _, mode := range []string{"historical", "autopilot", "seeded", "unknown_source", "candidate", "open_question", "sessionless", "empty_frozen"} {
		t.Run(mode, func(t *testing.T) {
			fs, svc, run, id := finishFixture(t)
			locked := run
			locked.Status, locked.WorkerID = "claimed", pgconv.UUID(id.workerID)
			locked.PlanMd = pgtype.Text{String: "plan", Valid: true}
			locked.PlanSource = "agent"
			locked.SessionID = pgtype.Text{String: "session", Valid: true}
			approved := true
			switch mode {
			case "historical":
				fs.claimFinishCtx = &store.GetRunClaimContextRow{HumanPlanApproved: true}
			case "autopilot":
				locked.AutoApprove = true
			case "seeded":
				locked.PlanSource = "seeded"
			case "unknown_source":
				locked.Harness = "codex"
				locked.PolicyRefusalCount = 1
				locked.AutoApprove = true
				approved = false
			// Classifier uses the original Claude snapshot; this test targets projection only.
			case "candidate":
				approved = false
				locked.MilestonesCandidate = []byte(`[{"id":"new","title":"Candidate"}]`)
			case "open_question":
				approved = false
				locked.OpenQuestionID = pgtype.Text{String: "question", Valid: true}
			case "sessionless":
				approved = false
				locked.SessionID = pgtype.Text{}
			case "empty_frozen":
				locked.AutoApprove = true
				locked.MilestonesFrozen = []byte("[]")
			}
			fs.claimFinishLocked = &locked
			p, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{}, nil, id)
			if err != nil || p == nil {
				t.Fatalf("finish: %v", err)
			}
			if p.PlanApproved != approved {
				t.Fatalf("approval=%v want=%v", p.PlanApproved, approved)
			}
			phase := "implementing"
			if !approved {
				phase = "awaiting_approval"
			}
			if mode == "open_question" {
				phase = "awaiting_input"
			}
			if mode == "sessionless" {
				phase = ""
			}
			if p.ResumePhase != phase {
				t.Fatalf("phase=%q want=%q", p.ResumePhase, phase)
			}
			if mode == "candidate" && (len(p.Milestones) != 1 || p.Milestones[0].ID != "new" || p.ExecutionMilestones != nil) {
				t.Fatal("candidate became execution")
			}
			if mode == "empty_frozen" && (p.ExecutionMilestones == nil || *p.ExecutionMilestones == nil || len(*p.ExecutionMilestones) != 0) {
				t.Fatal("explicit empty frozen contract lost")
			}
		})
	}
}

func TestClaimLockedProjectionReadAndCommitFailures(t *testing.T) {
	for _, mode := range []string{"context", "session", "contract", "commit"} {
		t.Run(mode, func(t *testing.T) {
			fs, svc, run, id := finishFixture(t)
			injected := errors.New("projection failure")
			switch mode {
			case "context":
				fs.claimFinishCtxErr = injected
			case "session":
				fs.claimRun.SessionID = pgtype.Text{String: "session", Valid: true}
				fs.claimRun.GateRevision = 1
				fs.claimFinishSessionErr = injected
			case "contract":
				fs.claimRun.CompletionContract = []byte("{")
			case "commit":
				fs.claimFinishCommitErr = injected
			}
			p, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{}, nil, id)
			if p != nil || err == nil {
				t.Fatalf("failure delivered payload: %v / %v", p, err)
			}
			if fs.claimFailed != nil || fs.claimRequeued != nil {
				t.Fatal("read/commit failure mutated claim")
			}
		})
	}
}

func TestClaimLockedProjectionLegacyFrameErrorsDegrade(t *testing.T) {
	fs, svc, run, id := finishFixture(t)
	fs.claimRun.PlanMd = pgtype.Text{String: "locked plan", Valid: true}
	fs.claimRun.SessionID = pgtype.Text{String: "session", Valid: true}
	fs.claimFinishPlanSeqErr = errors.New("legacy seq")
	fs.claimFinishPlanAtErr = errors.New("legacy at")
	p, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{ResumePlanSeq: 99, ResumePlanAt: new(time.Time)}, nil, id)
	if err != nil || p == nil || p.ResumePhase != "awaiting_approval" || p.ResumePlanSeq != 0 || p.ResumePlanAt != nil {
		t.Fatalf("legacy degrade: %v / %v", p, err)
	}
}

func TestClaimLockedProjectionFinalSize(t *testing.T) {
	fs, svc, run, id := finishFixture(t)
	svc.claimSizeLimit = 2048
	fs.claimRun.PlanMd = pgtype.Text{String: strings.Repeat("locked", 1024), Valid: true}
	p, err := svc.finishRunClaim(context.Background(), run, &ClaimPayload{Kind: "issue"}, nil, id)
	if err != nil || p != nil || fs.claimFailed == nil || fs.claimFailed.FailOrigin.String != "provisioning_failed" {
		t.Fatalf("final size not enforced: %v / %v", p, err)
	}
}

// Exercise both production overrides through the shared projection boundary.
// Embedding pgx.Tx keeps unexpected calls loud without a broad handwritten mock.
func TestClaimLockedProjectionSavepointFailures(t *testing.T) {
	for _, target := range []string{"seq", "time"} {
		for _, mode := range []string{"nil_tx", "begin", "nil_savepoint", "rollback", "release", "cancelled", "connection", "query", "success"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				_, svc, run, _ := finishFixture(t)
				run.PlanMd = pgtype.Text{String: "plan", Valid: true}
				run.SessionID = pgtype.Text{String: "session", Valid: true}
				run.PlanSource, run.AutoApprove, run.GateRevision = "agent", false, 0
				injected := errors.New("savepoint failure")
				tx := &lockedPlanFrameUnitTx{target: target, mode: mode, injected: injected}
				var underlying pgx.Tx = tx
				if mode == "nil_tx" {
					underlying = nil
				}
				ctx := context.Background()
				if mode == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					tx.cancel = cancel
				}
				q := pgxClaimFinishTx{tx: underlying}
				p, err := svc.claimPlanProjection(ctx, q, run, store.GetRunClaimContextRow{})
				if mode != "query" && mode != "success" {
					if p != nil || !errors.Is(err, errClaimPlanFrameTx) {
						t.Fatalf("savepoint failure degraded: payload=%v err=%v", p, err)
					}
					if (mode == "begin" || mode == "rollback" || mode == "release") && !errors.Is(err, injected) {
						t.Fatalf("lost lifecycle failure: %v", err)
					}
					return
				}
				if err != nil || p == nil {
					t.Fatalf("recoverable projection: %v", err)
				}
				wantSeq := int64(17)
				if mode == "query" && target == "seq" {
					wantSeq = 0
				}
				wantTime := mode != "query" || target != "time"
				if p.ResumePlanSeq != wantSeq || (p.ResumePlanAt != nil) != wantTime {
					t.Fatalf("degradation: seq=%d at=%v", p.ResumePlanSeq, p.ResumePlanAt)
				}
				if mode == "query" && tx.rollbacks != 1 {
					t.Fatalf("query error returned without recovery: rollbacks=%d", tx.rollbacks)
				}
			})
		}
	}
}

type lockedPlanFrameUnitTx struct {
	pgx.Tx
	target    string
	mode      string
	injected  error
	begins    int
	rollbacks int
	cancel    context.CancelFunc
}

func (tx *lockedPlanFrameUnitTx) Begin(context.Context) (pgx.Tx, error) {
	tx.begins++
	target := tx.begins == 1 && tx.target == "seq" || tx.begins == 2 && tx.target == "time"
	if target && tx.mode == "begin" {
		return nil, tx.injected
	}
	if target && tx.mode == "nil_savepoint" {
		return nil, nil
	}
	return &lockedPlanFrameUnitSavepoint{owner: tx, target: target}, nil
}

type lockedPlanFrameUnitSavepoint struct {
	pgx.Tx
	owner  *lockedPlanFrameUnitTx
	target bool
}

func (sp *lockedPlanFrameUnitSavepoint) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	var err error
	if sp.target {
		switch sp.owner.mode {
		case "rollback", "query":
			err = sp.owner.injected
		case "cancelled":
			sp.owner.cancel()
			err = context.Canceled
		case "connection":
			err = &pgconn.PgError{Code: "08006", Message: "connection failure"}
		}
	}
	return lockedPlanFrameUnitRow{err: err, seq: strings.HasPrefix(sql, "-- name: LatestPlanSeqForRun :one")}
}

func (sp *lockedPlanFrameUnitSavepoint) Rollback(context.Context) error {
	sp.owner.rollbacks++
	if sp.target && sp.owner.mode == "rollback" {
		return sp.owner.injected
	}
	return nil
}

func (sp *lockedPlanFrameUnitSavepoint) Commit(context.Context) error {
	if sp.target && sp.owner.mode == "release" {
		return sp.owner.injected
	}
	return nil
}

type lockedPlanFrameUnitRow struct {
	err error
	seq bool
}

func (r lockedPlanFrameUnitRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.seq {
		*dest[0].(*int64) = 17
	} else {
		*dest[0].(*pgtype.Timestamptz) = pgtype.Timestamptz{Time: time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC), Valid: true}
	}
	return nil
}

// This is deliberately separate from the public-Claim preunit overlay tests: it
// pins the shared helper's optional reader visibility and source fail-closed rule.
type lockedProjectionOptionalReader struct {
	*fakeClaimFinishTx
	history      bool
	historyErr   error
	planningErr  error
	historyRead  bool
	planningRead bool
}

func (q *lockedProjectionOptionalReader) HasPolicyRevisionHistory(context.Context, uuid.UUID) (bool, error) {
	q.historyRead = true
	return q.history, q.historyErr
}
func (q *lockedProjectionOptionalReader) GetSemanticPlanning(context.Context, uuid.UUID) (store.GetSemanticPlanningRow, error) {
	q.planningRead = true
	if q.planningErr != nil {
		return store.GetSemanticPlanningRow{}, q.planningErr
	}
	return store.GetSemanticPlanningRow{}, pgx.ErrNoRows
}
func (q *lockedProjectionOptionalReader) GetSemanticCandidateByDigest(context.Context, store.GetSemanticCandidateByDigestParams) ([]byte, error) {
	return nil, pgx.ErrNoRows
}
func TestClaimPlanProjectionOptionalReader(t *testing.T) {
	for _, mode := range []string{"ordinary_fallback", "revision_history", "history_failure", "identity_failure"} {
		t.Run(mode, func(t *testing.T) {
			fs, svc, run, _ := finishFixture(t)
			run.Harness = "codex"
			run.PolicyRefusalCount = 1
			run.AutoApprove = true
			run.PlanMd = pgtype.Text{String: "legacy accepted plan", Valid: true}
			reader := &lockedProjectionOptionalReader{fakeClaimFinishTx: &fakeClaimFinishTx{f: fs}}
			switch mode {
			case "revision_history":
				reader.history = true
			case "history_failure":
				reader.historyErr = errors.New("history unavailable")
			case "identity_failure":
				reader.planningErr = errors.New("identity unavailable")
			}
			p, err := svc.claimPlanProjection(context.Background(), reader, run, store.GetRunClaimContextRow{})
			if err != nil || p == nil {
				t.Fatalf("projection: %v", err)
			}
			if !reader.historyRead || !reader.planningRead {
				t.Fatal("optional transaction reader methods were hidden")
			}
			if p.PlanApproved != (mode == "ordinary_fallback") {
				t.Fatalf("constrained fallback=%v for %s", p.PlanApproved, mode)
			}
		})
	}
}
