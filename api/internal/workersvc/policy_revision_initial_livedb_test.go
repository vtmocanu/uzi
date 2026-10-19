package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Public services execute the real INSERTs and createRunAtomic initializer.
// No source is seeded: the only fixtures are credentials, repo, issue/schedule.
func TestPolicyRevisionInitialCreationLiveDB(t *testing.T) {
	for _, harness := range []Harness{HarnessCodex, HarnessClaude} {
		for _, kind := range []string{"issue", "task", "prompt", "self_improve", "interactive", "ci_fix", "judge", "task_review", "then_fix"} {
			t.Run(string(harness)+"/"+kind, func(t *testing.T) {
				e := setupCodexLiveDB(t)
				var user, repo uuid.UUID
				if harness == HarnessCodex {
					user, repo = seedCodexOnlyOriginUser(t, e)
				} else {
					user, repo = seedClaudeOnlyOriginUser(t, e)
				}
				svc := atomicSvc(e) // All policy feature flags are off.
				var r store.Run
				var err error
				switch kind {
				case "issue":
					seedEligibleIssue(t, e, repo, 1)
					r, err = svc.CreateRun(e.ctx, user, repo, 1, "description", nil, nil, false, nil, nil, nil)
				case "task", "interactive":
					r, err = svc.CreateTaskRun(e.ctx, user, repo, "context", "", false, false, false, kind == "interactive")
				case "prompt":
					schedule := seedPromptScheduleRow(t, e, user, repo)
					r, err = svc.CreatePromptRun(e.ctx, user, repo, schedule, "title", "prompt", false, false, nil, nil, false, nil, nil)
				case "self_improve":
					r, err = svc.CreateSelfImproveRun(e.ctx, user, repo, 1, "title", "description", nil, nil, false, nil)
				case "ci_fix":
					r, err = svc.CreateAutoCIFixRun(e.ctx, user, repo, "initial-"+uuid.NewString(), "title", "description", sampleSnapshot(), nil)
				case "task_review", "then_fix":
					// Bridge: execute each child's real SQL INSERT through the atomic
					// seam; public follow-on dispatch needs unrelated terminal events.
					// The original is created publicly so its repo and branch are real.
					target, createErr := svc.CreateTaskRun(e.ctx, user, repo, "context", "", false, false, false, false)
					if createErr != nil {
						t.Fatal(createErr)
					}
					r, _, err = svc.createRunAtomic(e.ctx, user, nil, func(q *store.Queries, res resolvedHarness) (store.Run, error) {
						if kind == "task_review" {
							return q.CreateTaskReviewRun(e.ctx, store.CreateTaskReviewRunParams{
								RunID: uuid.New(), UserID: user, RepoID: repo, Branch: target.Branch, BaseBranch: target.BaseBranch,
								TargetRunID: pgtype.UUID{Bytes: target.ID, Valid: true}, IssueTitle: "review", Harness: string(res.Harness),
							})
						}
						return q.CreateThenFixRun(e.ctx, store.CreateThenFixRunParams{
							RunID: uuid.New(), UserID: user, RepoID: repo, Branch: target.Branch, BaseBranch: target.BaseBranch,
							ThenFixOfRunID: pgtype.UUID{Bytes: target.ID, Valid: true}, IssueTitle: "fix", Harness: string(res.Harness),
						})
					})
					if err == nil {
						if r.Kind != "task" || r.TargetRunID.Valid ||
							(kind == "task_review" && !r.ReviewTargetRunID.Valid) ||
							(kind == "then_fix" && !r.ThenFixOfRunID.Valid) {
							t.Fatalf("child INSERT did not establish its task provenance: %+v", r)
						}
						if got := mustRun(t, e, target.ID); !bytes.Equal(target.PolicyRevisionSource, got.PolicyRevisionSource) {
							t.Fatal("child creation changed original coverage")
						}
					}
				case "judge":
					// Bridge: judge's actual INSERT via the shared atomic seam; the
					// public retrospective service requires an unrelated finished run.
					target, _, createErr := svc.createRunAtomic(e.ctx, user, nil, manualIssueInsert(e, user, repo))
					if createErr != nil {
						t.Fatal(createErr)
					}
					r, _, err = svc.createRunAtomic(e.ctx, user, nil, func(q *store.Queries, res resolvedHarness) (store.Run, error) {
						return q.CreateJudgeRun(e.ctx, store.CreateJudgeRunParams{
							UserID: user, TargetRunID: pgtype.UUID{Bytes: target.ID, Valid: true},
							IssueTitle: "judge", IssueDescription: "description", TriggerSource: "manual", Harness: string(res.Harness),
						})
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				persisted := mustRun(t, e, r.ID)
				if !bytes.Equal(r.PolicyRevisionSource, persisted.PolicyRevisionSource) {
					t.Fatalf("returned source=%s persisted=%s", r.PolicyRevisionSource, persisted.PolicyRevisionSource)
				}
				eligible := harness == HarnessCodex && slices.Contains([]string{"issue", "task", "prompt", "self_improve"}, kind)
				if !eligible {
					var isNull bool
					if err := e.pool.QueryRow(e.ctx, "SELECT policy_revision_source IS NULL FROM runs WHERE id=$1", r.ID).Scan(&isNull); err != nil || !isNull {
						t.Fatalf("excluded run must retain SQL NULL coverage: %v %v source=%s", isNull, err, persisted.PolicyRevisionSource)
					}
					return
				}
				for _, run := range []store.Run{r, persisted} {
					source, parseErr := apitypes.ParsePolicyRevisionSourceV1(run.PolicyRevisionSource)
					if parseErr != nil || source.Version != 1 || source.Gate != nil || source.PrimaryRevision != nil ||
						source.AssociatedCrossChecks == nil || len(source.AssociatedCrossChecks) != 0 {
						t.Fatalf("initial source=%s err=%v", run.PolicyRevisionSource, parseErr)
					}
					var fields map[string]any
					if err := json.Unmarshal(run.PolicyRevisionSource, &fields); err != nil || len(fields) != 4 {
						t.Fatalf("initial source must contain exactly the four V1 fields: %s", run.PolicyRevisionSource)
					}
					if run.PolicyRefusalCount != 0 || run.GateRevision != 0 || run.GatePresentationID.Valid ||
						len(run.GatePayloadDigest) != 0 || len(run.GatePresentedPayload) != 0 || len(run.PolicyStopResolution) != 0 {
						t.Fatalf("creation established policy/gate state: %+v", run)
					}
					// auto_approve on task/self_improve is an ordinary creation flag;
					// the empty source itself grants no candidate approval.
					if policyRevisionPlanApproved(e.ctx, e.q, run, false) {
						t.Fatal("initial source granted approval")
					}
				}
				assertRunHarness(t, e, r.ID, harness)
			})
		}
	}
}

type revisionInitialBeginner struct {
	TxBeginner
	point string
	hits  int
}
type revisionInitialTx struct {
	pgx.Tx
	owner *revisionInitialBeginner
	wrote bool
}

func (b *revisionInitialBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &revisionInitialTx{Tx: tx, owner: b}, nil
}
func (tx *revisionInitialTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: InitializePolicyRevisionSource :one") {
		tx.owner.hits++
		if tx.owner.point == "initializer" {
			return revisionWriterFailureRow{err: errors.New("injected initializer failure")}
		}
		tx.wrote = true
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *revisionInitialTx) Commit(ctx context.Context) error {
	if tx.wrote && tx.owner.point == "commit" {
		tx.owner.hits++
		_ = tx.Rollback(ctx)
		return errors.New("injected creation commit failure")
	}
	return tx.Tx.Commit(ctx)
}

func TestPolicyRevisionInitialRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"initializer", "freeze", "commit"} {
		t.Run(point, func(t *testing.T) {
			e := setupCodexLiveDB(t)
			user, repo := seedCodexOnlyOriginUser(t, e)
			svc := atomicSvc(e)
			b := &revisionInitialBeginner{TxBeginner: e.pool, point: point}
			svc.SetTxBeginner(b)
			if point == "freeze" {
				svc.codexFreezeFn = func(context.Context, codexFreezeStore, uuid.UUID, uuid.UUID, uuid.UUID, string) error {
					b.hits++
					return errors.New("injected freeze failure")
				}
			}
			// The rollback bridge uses the real issue INSERT and the production
			// atomic boundary, with faults at the initializer, freeze and commit.
			r, _, err := svc.createRunAtomic(e.ctx, user, nil, manualIssueInsert(e, user, repo))
			if err == nil || r.ID != uuid.Nil || b.hits == 0 {
				t.Fatalf("create result=%s err=%v fault hits=%d", r.ID, err, b.hits)
			}
			if n := e.countRunsForUser(t, user); n != 0 {
				t.Fatalf("rollback left %d runs (including any run binding)", n)
			}
		})
	}
}

func TestPolicyRevisionUnsupportedInvalidationLiveDB(t *testing.T) {
	for _, mode := range []string{"feature", "snapshot", "lease", "policy_capability", "credential_capability", "both_capabilities", "legacy_generation", "same_batch_later_plan", "same_batch_later_plan_unsupported"} {
		t.Run(mode, func(t *testing.T) {
			e, source, id := revisionWriterHuman(t)
			gen := i64p(1)
			sameBatch := mode == "same_batch_later_plan" || mode == "same_batch_later_plan_unsupported"
			if sameBatch {
				// Start with production-created empty V1. Only claim ownership is
				// seeded; no source UPDATE simulates initialization or acceptance.
				alias := e.seedStaticAPIKey(t, e.userID, "initial-batch-"+uuid.NewString(), codexToken("sk"))
				makeCodexDefault(t, e.codexTestEnv, alias)
				r, err := e.svc.CreateTaskRun(e.ctx, e.userID, e.repoID, "context", "", false, false, false, false)
				if err != nil {
					t.Fatal(err)
				}
				source, err = apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
				if err != nil || source.PrimaryRevision != nil || source.Gate != nil || len(source.AssociatedCrossChecks) != 0 {
					t.Fatalf("new run must start with empty V1: %s %v", r.PolicyRevisionSource, err)
				}
				e.runID = r.ID
				e.exec("UPDATE runs SET status='claimed',worker_id=$2,claim_generation=1 WHERE id=$1", e.runID, e.wkr.ID)
				input, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "revise_plan"})
				if err != nil {
					t.Fatal(err)
				}
				id = input.ID
				if _, err := e.svc.AckInputs(e.ctx, e.wkr, e.runID, 1, []int64{id}); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "feature", "same_batch_later_plan_unsupported":
				e.svc.p.PolicyHoldNegotiated = false
			case "snapshot":
				e.svc.p.PolicyHoldSnapshotEnabled = false
			case "lease":
				e.svc.p.TerminalPendingLease = 0
			case "policy_capability":
				e.wkr.ProtocolCapabilities = slices.DeleteFunc(e.wkr.ProtocolCapabilities, func(c string) bool { return c == capability.ProviderPolicyHoldV1 })
			case "credential_capability":
				e.wkr.ProtocolCapabilities = slices.DeleteFunc(e.wkr.ProtocolCapabilities, func(c string) bool { return c == capability.CredentialSwitchV1 })
			case "both_capabilities", "legacy_generation":
				e.wkr.ProtocolCapabilities = nil
				if mode == "legacy_generation" {
					gen = nil
				}
			}
			e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.wkr.ID, e.wkr.ProtocolCapabilities)
			payload := revisionWriterPayload(t, source, revisionWriterHumanRef(id))
			msgs := []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: payload}}
			if sameBatch {
				msgs = append(msgs, IncomingMessage{Seq: 2, Kind: "plan", Payload: []byte("{}")})
			}
			err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, gen)
			if err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, e.codexTestEnv, e.runID)
			if len(r.PolicyRevisionSource) != 0 {
				t.Fatalf("unsupported frame retained coverage: %s", r.PolicyRevisionSource)
			}
			var isNull bool
			if err := e.pool.QueryRow(e.ctx, "SELECT policy_revision_source IS NULL FROM runs WHERE id=$1", e.runID).Scan(&isNull); err != nil || !isNull {
				t.Fatalf("coverage must be SQL NULL: %v %v", isNull, err)
			}
			e.exec("UPDATE runs SET policy_refusal_count=1,gate_revision=0,gate_presentation_id=NULL,gate_payload_digest=NULL,gate_presented_payload=NULL WHERE id=$1", e.runID)
			r = mustRun(t, e.codexTestEnv, e.runID)
			if policyRevisionPlanApproved(e.ctx, e.q, r, true) {
				t.Fatal("invalidated history revived approval")
			}
		})
	}
}

func TestPolicyRevisionUnsupportedAuthorityLiveDB(t *testing.T) {
	for _, mode := range []string{"duplicate", "old", "wrong_generation", "wrong_worker", "released", "accepted_stop", "ineligible", "missing_required_generation"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _ := revisionWriterHuman(t)
			e.svc.p.PolicyHoldNegotiated = false
			gen := i64p(1)
			seq := int32(3)
			switch mode {
			case "duplicate":
				revisionWriterAppend(t, e, IncomingMessage{Seq: seq, Kind: "status", Payload: []byte("{}")})
			case "old":
				revisionWriterAppend(t, e, IncomingMessage{Seq: 4, Kind: "plan", Payload: []byte("{}")})
			case "wrong_generation":
				gen = i64p(2)
			case "wrong_worker":
				e.wkr.ID = uuid.New()
			case "released":
				e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", e.runID)
			case "accepted_stop":
				scope, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", e.runID, scope.ID)
			case "ineligible":
				e.exec("UPDATE runs SET interactive=true WHERE id=$1", e.runID)
			case "missing_required_generation":
				gen = nil
			}
			before := mustRun(t, e.codexTestEnv, e.runID).PolicyRevisionSource
			err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: seq, Kind: "plan_revising", Payload: []byte("{}")}}, gen)
			if slices.Contains([]string{"wrong_generation", "wrong_worker", "released", "missing_required_generation"}, mode) {
				if err == nil {
					t.Fatal("invalid authority accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if after := mustRun(t, e.codexTestEnv, e.runID).PolicyRevisionSource; !bytes.Equal(before, after) {
				t.Fatal("non-authoritative frame changed coverage")
			}
		})
	}
}

func TestPolicyRevisionUnsupportedRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"writer", "zero", "commit"} {
		t.Run(point, func(t *testing.T) {
			e, _, _ := revisionWriterHuman(t)
			e.svc.p.PolicyHoldNegotiated = false
			before := mustRun(t, e.codexTestEnv, e.runID)
			b := &revisionWriterBeginner{TxBeginner: e.pool, point: point}
			e.svc.SetTxBeginner(b)
			err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")}}, i64p(1))
			if err == nil || b.hits == 0 {
				t.Fatalf("err=%v fault hits=%d", err, b.hits)
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			var messages int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_messages WHERE run_id=$1", e.runID).Scan(&messages); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.PolicyRevisionSource, after.PolicyRevisionSource) || before.LastSeq != after.LastSeq || messages != 0 {
				t.Fatal("failed invalidation committed source, sequence or message")
			}
		})
	}
}
