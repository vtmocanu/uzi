package workersvc

import (
	"context"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

const (
	// RunKindJudge is the run-judge kind (PRD #46 Decision 1): a worker-executed
	// retrospective of another finished run. It has no repo, no issue, no branch, no
	// MR — it rides the run machinery only for token delivery and message
	// persistence, and points at the reviewed run via target_run_id.
	RunKindJudge = "judge"
	// RunKindSelfImprove is the self-improvement kind (PRD #46 Decision 10): an
	// autonomous improvement run against uzi's own repo. It is issue-shaped and flows
	// through the ordinary repo-ful claim path — no fork needed here.
	RunKindSelfImprove = "self_improve"
)

// assembleJudgeClaim builds the claim payload for a judge run (PRD #46 Decisions 1
// & 3). Unlike the ordinary run lane it does NOT call GetRunClaimContext (no repo,
// no forge connection) and NEVER opens the bot PAT: least privilege, and a judge
// must not spuriously fail when the reviewed run's forge connection is gone (audit
// H2). The only secret it delivers is the run owner's Anthropic token — opened
// through the same vault-aware openAnthropic path as every other run, so a locked
// vault requeues (not fails) the judge run. The reviewed run's trace is fetched
// out-of-band through the Bearer trace endpoint (M3), so the claim only carries its
// id; the judge model and the command-not-found signal are added in M3.
func (s *Service) assembleJudgeClaim(ctx context.Context, run store.Run) (*ClaimPayload, error) {
	anthropic, err := s.openAnthropic(ctx, run.UserID)
	if err != nil {
		return nil, err
	}

	var targetRunID *string
	if run.TargetRunID.Valid {
		id := uuid.UUID(run.TargetRunID.Bytes).String()
		targetRunID = &id
	}

	return &ClaimPayload{
		RunID:            run.ID.String(),
		Kind:             run.Kind,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		Status:           run.Status,
		TargetRunID:      targetRunID,
		SessionID:        textPtr(run.SessionID),
		LastSeq:          run.LastSeq,
		IterationCount:   run.IterationCount,
		RequeueCount:     run.RequeueCount,
		Secrets: ClaimSecrets{
			// ForgeUsername/ForgePAT are left empty by design: a judge never touches a
			// repo. The wire still carries the (empty) forge_pat key because a judge run
			// rides the ordinary ClaimPayload; the no-PAT guarantee is that assembly
			// never decrypts one, asserted in judge_test.go.
			AnthropicOAuthToken: string(anthropic),
		},
		Agents:        []ClaimAgent{},
		Skills:        []ClaimSkill{},
		SkillsDropped: []ClaimSkillDrop{},
		Config: ClaimConfig{
			RunTimeoutSeconds:  int(s.p.RunTimeout.Seconds()),
			IdleTimeoutSeconds: int(s.p.RunIdleTimeout.Seconds()),
			MaxIterations:      s.p.RunMaxIterations,
			ToolPackages:       []string{},
		},
	}, nil
}
