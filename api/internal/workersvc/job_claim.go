package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/toolprofile"
)

// ClaimJob is the job block of a claim (PRD #1908): what a job runner needs to do the work and
// nothing about a repo. type is the job type the caller asked for, title/prompt are the caller's
// own words, and inputs are the named text documents attached to the job, in ordinal order.
type ClaimJob struct {
	Type   string          `json:"type"`
	Title  string          `json:"title"`
	Prompt string          `json:"prompt"`
	Inputs []ClaimJobInput `json:"inputs"`
	// Files is the manifest of the job's attached binary/text input files (PRD #1909 D8), always
	// present ([] when none), in (created_at, id) order. The worker downloads each through
	// GET /api/worker/runs/{id}/files/{fileID} and verifies its sha256.
	Files []ClaimJobFile `json:"files"`
}

// ClaimJobFile is one attached input file of a job. Name is the storage name
// (`<sha256>.<ext>`, the on-disk name under inputs/); DisplayName is the uploader's label and is
// UNTRUSTED text.
type ClaimJobFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
}

// ClaimJobInput is one named input document of a job.
type ClaimJobInput struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// jobCredentialErr converts an empty auto pool into a TERMINAL credential failure for a job.
// The run lane treats errAutoPoolEmpty as transient and requeues into pool_wait, but a job never
// parks (PRD #1908 D-E) and RequeueClaimAssemblyExact excludes it from pool_wait, so a requeue
// would match no row, roll the claim back and strand the run in 'claimed' for the never-started
// sweep to requeue into the same loop. The error deliberately does NOT wrap errAutoPoolEmpty:
// finishRunClaim then fails the run closed with fail_origin 'credential_unavailable'.
func jobCredentialErr(err error) error {
	if errors.Is(err, errAutoPoolEmpty) {
		return fmt.Errorf("%w: the worker's auto credential pool has no usable token, and a job does not wait for one", errCredentialUnavailable)
	}
	return err
}

// assembleJobClaim builds the claim payload for an already-claimed kind='job' run (PRD #1908).
// A job has no repo, no forge connection, no memory and no skills, so it forks in assembleClaim
// BEFORE GetRunClaimContext (which INNER-JOINs repos) and before any PAT decrypt: the payload
// carries the model credential, the job block and the wall budget, and nothing else. The
// credential resolves through the SAME ladder the ordinary Claude run lane uses (a per-run
// override, else the claiming worker's binding, else the owner's default), so a job spends the
// account the worker or the request selected and not the judge's binding.
//
// The wire keeps the (empty) repo and forge_pat keys because a job rides the ordinary
// ClaimPayload; the no-PAT guarantee is that assembly never decrypts one.
func (s *Service) assembleJobClaim(ctx context.Context, wkr store.Worker, run store.Run) (*ClaimPayload, error) {
	if run.Harness == harnessCodex {
		// Jobs are created with an explicit Claude pin; a Codex job is a data error and must
		// fail closed rather than run without a credential.
		return nil, fmt.Errorf("%w: a job run cannot use the Codex harness", errCredentialUnavailable)
	}
	// A requeued job is claimed again (worker death, the never-started sweep). job_results is
	// keyed by run_id and never otherwise cleared, so drop an earlier flight's result and findings
	// before this flight starts: the no-result invariant must judge THIS flight. ClaimRun already
	// bumped the claim generation, so an older flight cannot write one back after this.
	if err := s.q.ClearJobResultForRun(ctx, run.ID); err != nil {
		return nil, fmt.Errorf("clear earlier job result: %w", err)
	}
	choice, err := s.claimSecretID(ctx, wkr, run)
	if err != nil {
		return nil, jobCredentialErr(err)
	}
	cred, choice, err := s.openWithAutoRetry(ctx, run, choice)
	if err != nil {
		return nil, jobCredentialErr(err)
	}
	// emitSwitchMessage=false: a job is not credential-switchable (D-E), so it never emits a
	// 'credential_switch' message and last_seq cannot diverge from run.LastSeq.
	if _, err := s.recordRunCredential(ctx, run, cred, choice, false); err != nil {
		return nil, err
	}

	inputs, err := s.q.ListJobInputsForClaim(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("list job inputs: %w", err)
	}
	job := &ClaimJob{
		Type:   run.JobType.String,
		Title:  run.IssueTitle,
		Prompt: run.IssueDescription,
		Inputs: make([]ClaimJobInput, 0, len(inputs)),
		Files:  make([]ClaimJobFile, 0),
	}
	for _, in := range inputs {
		job.Inputs = append(job.Inputs, ClaimJobInput{Name: in.Name, Content: in.ContentMd})
	}
	files, err := s.q.ListJobInputFilesForClaim(ctx, pgconv.UUID(run.ID))
	if err != nil {
		return nil, fmt.Errorf("list job input files: %w", err)
	}
	for _, f := range files {
		job.Files = append(job.Files, ClaimJobFile{
			ID:          f.ID.String(),
			Name:        f.StorageName.String,
			DisplayName: f.DisplayName,
			Size:        f.ByteSize,
			SHA256:      f.Sha256.String,
			ContentType: f.ContentType.String,
		})
	}

	// The owner's Claude model lane, then a frozen per-run model when it is compatible. Best
	// effort: a lookup error logs and the runner uses its own default, it never fails the claim.
	var defaultModel *string
	if lanes, lerr := s.q.GetUserHarnessModelDefaults(ctx, run.UserID); lerr != nil {
		slog.Warn("job claim: read user model defaults", "user", run.UserID.String(), "error", lerr)
	} else {
		defaultModel = textPtr(lanes.DefaultClaudeModel)
	}
	if run.Model.Valid && harnessModelCompatible(Harness(run.Harness), run.Model.String) {
		defaultModel = textPtr(run.Model)
	}
	defaultEffort, eerr := s.q.GetUserDefaultEffort(ctx, run.UserID)
	if eerr != nil {
		slog.Warn("job claim: read user default effort", "user", run.UserID.String(), "error", eerr)
	}

	wall := coalesceInt(run.BudgetWallSeconds, int(s.p.RunTimeout.Seconds()))
	wall32 := int32(wall) //nolint:gosec // G115: a clamped wall budget, at most budgetWallCeilingSeconds
	cfg := ClaimConfig{
		RunTimeoutSeconds:      wall,
		IdleTimeoutSeconds:     int(s.p.RunIdleTimeout.Seconds()),
		MaxIterations:          s.p.RunMaxIterations,
		PlanMaxRevisions:       s.p.PlanMaxRevisions,
		QuestionMax:            s.p.QuestionMax,
		QuestionTimeoutSeconds: s.p.QuestionTimeoutSeconds,
		DefaultModel:           defaultModel,
		DefaultEffort:          resolveEffortPtr(defaultEffort),
		ToolPackages:           []string{},
		DeniedToolPackages:     toolprofile.DenylistNames(),
	}
	if s.jobFiles != nil {
		l := s.jobFiles.Limits()
		cfg.JobInputFileMaxBytes = l.InputFileMaxBytes
		cfg.JobInputsMaxFiles = l.InputsMaxFiles
		cfg.JobInputsMaxBytes = l.InputsMaxBytes
	}
	return &ClaimPayload{
		RunID:             run.ID.String(),
		Kind:              run.Kind,
		ClaimGeneration:   run.ClaimGeneration,
		IssueTitle:        run.IssueTitle,
		IssueDescription:  run.IssueDescription,
		Status:            run.Status,
		Job:               job,
		BudgetWallSeconds: &wall32,
		LastSeq:           run.LastSeq,
		IterationCount:    run.IterationCount,
		RequeueCount:      run.RequeueCount,
		Secrets:           ClaimSecrets{AnthropicOAuthToken: string(cred.Token)},
		Agents:            []ClaimAgent{},
		Skills:            []ClaimSkill{},
		SkillsDropped:     []ClaimSkillDrop{},
		Config:            cfg,
	}, nil
}
