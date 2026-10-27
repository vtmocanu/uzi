package workersvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claimPlanReader is shared by ordinary assembly and the final transaction. Keep
// the concrete reader intact: policy approval also uses optional source/history
// and semantic identity methods implemented by transaction-bound store.Queries.
type claimPlanReader interface {
	GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error)
	LatestPlanSeqForRun(context.Context, uuid.UUID) (int64, error)
	LatestPersistedPlanFrameAtForRun(context.Context, store.LatestPersistedPlanFrameAtForRunParams) (pgtype.Timestamptz, error)
}

// claimPlanProjection performs database reads and pure derivation only. Its reads
// are single attempts; legacy plan-frame failures degrade and log, while main
// projection failures return no projection. The caller supplies one run snapshot
// and its matching reader; final handoff supplies the owned locked row and tx.
func (s *Service) claimPlanProjection(ctx context.Context, q claimPlanReader, run store.Run, rc store.GetRunClaimContextRow) (*ClaimPayload, error) {
	// PRD #122 M1: replay the FROZEN milestone list on every claim. A malformed column
	// degrades to nil-and-log rather than failing the claim, matching the repo_agents
	// decode on the DTO path — the column is data a prior write left, not an invariant
	// of this claim, and stranding a run over it would be worse than serving no list.
	milestones, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		slog.Error("workersvc: decode run milestones", "run_id", run.ID, "error", err)
		milestones = nil
	}

	var executionMilestones *[]Milestone
	if len(run.MilestonesFrozen) > 0 && !bytes.Equal(bytes.TrimSpace(run.MilestonesFrozen), []byte("null")) && err == nil {
		frozen := append([]Milestone{}, milestones...)
		executionMilestones = &frozen
	}

	// plan_approved is computed once here so both the PlanApproved field and the
	// resume_phase derivation (PRD #1247 M5, D13) read the SAME value — see the payload
	// literal for the full provenance of the three ordinary disjuncts. Constrained
	// revision/source authority overrides them before resume phase selection.
	planApproved := policyRevisionPlanApproved(ctx, q, run, run.AutoApprove || rc.HumanPlanApproved || run.PlanSource == planSourceSeeded)
	// PRD #1247 M5, D13: the server-derived phase a resumed claim RESTORES instead of
	// re-entering the planning turn. Empty for a fresh run (omitted by omitempty).
	resumePhase := resumePhaseFor(run, planApproved)
	sessionID, err := s.reconciledClaimSession(ctx, q, run)
	if err != nil {
		return nil, err
	}
	// The submitted plan's seq rides ONLY the awaiting_approval resume (D13: "awaiting_approval
	// with the submitted plan's seq"), so the query stays off the hot path for every other
	// claim. A recovered query error degrades to seq 0 — the same degrade-and-log
	// posture as the milestone/issue-comments decodes above; the run still resumes its gate, the
	// worker just cannot correlate a buffered approve_plan to a specific plan revision.
	var resumePlanSeq int64
	if resumePhase == "awaiting_approval" {
		// A gate resume re-presents the submitted candidate with its plan, for both legacy and
		// interlocked runs. This candidate can differ from an earlier frozen contract awaiting
		// explicit owner reconciliation. A valid candidate, including [], replaces the execution
		// list for gate presentation only; approved executing claims use the frozen list above.
		// Missing or invalid candidate data must not present frozen definitions as the gate.
		milestones = nil
		if len(run.MilestonesCandidate) > 0 {
			if cand, cerr := DecodeMilestones(run.MilestonesCandidate); cerr != nil {
				slog.Error("workersvc: decode run milestone candidate", "run_id", run.ID, "error", cerr)
			} else if _, valid := validateMilestones(cand); valid {
				milestones = cand
			}
		}
		if seq, err := q.LatestPlanSeqForRun(ctx, run.ID); err != nil {
			if errors.Is(err, errClaimPlanFrameTx) {
				return nil, err
			}
			slog.Warn("workersvc: latest plan seq for resume", "run_id", run.ID, "error", err)
		} else {
			resumePlanSeq = seq
		}
	}
	// PRD #1795 M1: the gate resume carries the persisted gate revision, its presentation id
	// (nil for an id-less gate) and the presented requirements, all from the immutable snapshot,
	// never the live requirement columns. A gate published before the migration (revision 0)
	// carries none of them.
	var resumeGateRevision int64
	var resumeGatePresentationID *uuid.UUID
	var resumeGatePresented *GatePresentedRequirements
	if resumePhase == "awaiting_approval" && run.GateRevision > 0 {
		resumeGateRevision = run.GateRevision
		if run.GatePresentationID.Valid {
			id := uuid.UUID(run.GatePresentationID.Bytes)
			resumeGatePresentationID = &id
		}
		if snap, ok, derr := decodeGatePresentedPayload(run.GatePresentedPayload); derr != nil {
			slog.Error("workersvc: decode gate presented payload", "run_id", run.ID, "error", derr)
		} else if ok {
			resumeGatePresented = snap.requirements()
		}
	}
	// Issue #1604: when the persisted plan is UNAPPROVED (both the awaiting_approval gate resume
	// and the "" re-plan resume of a session-less run), carry when THAT plan was last shown: the
	// latest `plan` frame whose plan_md equals the persisted run.PlanMd, so a newer frame for a
	// plan whose awaiting_approval report never persisted cannot move it. The worker discards a
	// replayed gate verdict written against an earlier plan. Same hasPlan test resumePhaseFor
	// applies, same planApproved value. A recovered query error or no matching plan frame
	// omits the field (no fallback to the latest frame). Savepoint failures are fatal.
	var resumePlanAt *time.Time
	if run.PlanMd.Valid && strings.TrimSpace(run.PlanMd.String) != "" && !planApproved {
		if at, err := q.LatestPersistedPlanFrameAtForRun(ctx, store.LatestPersistedPlanFrameAtForRunParams{
			RunID: run.ID, PlanMd: run.PlanMd.String,
		}); err != nil {
			if errors.Is(err, errClaimPlanFrameTx) {
				return nil, err
			}
			slog.Warn("workersvc: latest plan frame time for resume", "run_id", run.ID, "error", err)
		} else if at.Valid {
			t := at.Time
			resumePlanAt = &t
		}
	}

	pending, err := CompletionPending(run)
	if err != nil {
		return nil, err
	}
	return &ClaimPayload{
		PolicyHoldResume:       run.PolicyRefusalCount > 0,
		PolicyHoldFreshSession: run.PolicyRefusalCount > 0 && run.PolicyFreshClaimGeneration.Valid && run.PolicyFreshClaimGeneration.Int64 == run.ClaimGeneration,
		PlanMd:                 textPtr(run.PlanMd), AutoApprove: run.AutoApprove,
		PlanSource: run.PlanSource, PlanApproved: planApproved,
		ResumePhase: resumePhase, OpenQuestionID: textPtr(run.OpenQuestionID),
		ResumePlanSeq: resumePlanSeq, ResumePlanAt: resumePlanAt,
		ResumeGateRevision: resumeGateRevision, ResumeGatePresentationID: resumeGatePresentationID,
		ResumeGatePresented: resumeGatePresented,
		Milestones:          milestones, ExecutionMilestones: executionMilestones,
		AgentSelection: persistedSelection(run), SessionID: sessionID,
		PlanCrossCheckGateReason: textPtr(run.PlanCrossCheckGateReason),
		// These adjacent plan-policy/base guards travel with the same locked plan.
		PlanCrossCheckRequired:  run.PlanCrossCheckRequired,
		CodeCrossCheckRequired:  run.CodeCrossCheckRequired,
		PlanCrossCheckCodexLead: planCrossCheckCodexLeadSupported(run),
		PlannedBaseCommit:       textPtr(run.PlannedBaseCommit), RequireBaseMatch: run.RequireBaseMatch,
		Config: ClaimConfig{
			CompletionContractVersion: intPtr(run.CompletionContractVersion),
			ContractRevision:          intPtr(run.ContractRevision),
			CompletionScope:           CompletionScopeClaim(run.CompletionContract, run.MilestonesFrozen),
			CompletionPending:         pending, NonClosing: completionNonClosingClaim(run),
			RunTimeoutSeconds: coalesceInt(run.BudgetWallSeconds, int(s.p.RunTimeout.Seconds())) + int(run.BudgetExtensionSeconds) + int(run.BudgetFinalizeSeconds),
			MaxIterations:     coalesceInt(run.BudgetMaxIterations, s.p.RunMaxIterations),
		},
	}, nil
}

func applyClaimPlanProjection(payload, projection *ClaimPayload) {
	payload.PolicyHoldResume = projection.PolicyHoldResume
	payload.PolicyHoldFreshSession = projection.PolicyHoldFreshSession
	payload.PlanMd = projection.PlanMd
	payload.AutoApprove = projection.AutoApprove
	payload.PlanSource = projection.PlanSource
	payload.PlanApproved = projection.PlanApproved
	payload.ResumePhase = projection.ResumePhase
	payload.OpenQuestionID = projection.OpenQuestionID
	payload.ResumePlanSeq = projection.ResumePlanSeq
	payload.ResumePlanAt = projection.ResumePlanAt
	payload.ResumeGateRevision = projection.ResumeGateRevision
	payload.ResumeGatePresentationID = projection.ResumeGatePresentationID
	payload.ResumeGatePresented = projection.ResumeGatePresented
	payload.Milestones = projection.Milestones
	payload.ExecutionMilestones = projection.ExecutionMilestones
	payload.AgentSelection = projection.AgentSelection
	payload.SessionID = projection.SessionID
	payload.PlanCrossCheckGateReason = projection.PlanCrossCheckGateReason
	payload.PlanCrossCheckRequired = projection.PlanCrossCheckRequired
	payload.CodeCrossCheckRequired = projection.CodeCrossCheckRequired
	payload.PlanCrossCheckCodexLead = projection.PlanCrossCheckCodexLead
	payload.PlannedBaseCommit = projection.PlannedBaseCommit
	payload.RequireBaseMatch = projection.RequireBaseMatch
	payload.Config.CompletionContractVersion = projection.Config.CompletionContractVersion
	payload.Config.ContractRevision = projection.Config.ContractRevision
	payload.Config.CompletionScope = projection.Config.CompletionScope
	payload.Config.CompletionPending = projection.Config.CompletionPending
	payload.Config.NonClosing = projection.Config.NonClosing
	payload.Config.RunTimeoutSeconds = projection.Config.RunTimeoutSeconds
	payload.Config.MaxIterations = projection.Config.MaxIterations
}
