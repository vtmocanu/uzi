package uzicli

import (
	"context"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// FakeClient is an in-memory Client for command tests. It makes no network
// calls: set the canned fields, or set Err to have every method fail with it.
// GetRun returns an ExitNotFound error when the id is absent.
//
// RunReview mirrors the live client's tri-state (Reviews is keyed by run id):
//   - key present, non-nil value → the judged review
//   - key present, nil value     → a visible-but-unjudged run: (nil, nil), exit 0
//   - key absent                 → a real 404: ExitNotFound, exit 4
//
// so a test can exercise the "not judged" (nil) path WITHOUT reusing the 404
// path — the two are deliberately distinct (PRD #64 M7).
type FakeClient struct {
	User         apitypes.UserDTO
	Runs         []apitypes.RunListItemDTO
	RunByID      map[string]apitypes.RunDTO
	LogsByID     map[string][]apitypes.MessageDTO
	InputsByID   map[string][]apitypes.SteerInputDTO
	Reviews      map[string]*apitypes.ReviewDTO
	Workers      []apitypes.WorkerDTO
	Repos        []apitypes.RepoDTO
	AdminUsers   []apitypes.UserDTO
	AdminRuns    []apitypes.RunListItemDTO
	AdminWorkers []apitypes.AdminWorkerDTO
	AdminUsageV  apitypes.AdminUsageDTO
	RateLimits   []apitypes.AdminRateLimitRowDTO

	// Auth-flow canned replies (uzi login). StartCLIAuth returns AuthStart;
	// PollCLIAuth pops the front of AuthPolls each call (and repeats the last entry
	// once drained), so a test can script start → pending → authorized.
	AuthStart CLIAuthStartResult
	AuthPolls []CLIAuthPollResult

	// Write-verb capture + canned replies. CreateRun returns CreatedRun and records
	// its args; SubmitRunInput returns InputResp and records kind/body/selection, so
	// a test can assert the exact wire mapping of each write verb.
	CreatedRun         apitypes.RunDTO
	LastCreateRepoID   string
	LastCreateIssueIID int64
	InputResp          apitypes.RunInputResponse
	LastInputRunID     string
	LastInputKind      string
	LastInputBody      string
	LastInputSelection *apitypes.AgentSelection

	// DeleteWorker capture: records the id it was asked to delete.
	LastDeletedWorkerID string

	// Secrets drives ListSecrets (PRD #104 M2).
	Secrets []apitypes.SecretDTO

	// SetWorkerToken capture (PRD #104 M3): the worker id and the label it was asked
	// to bind. LastSetTokenLabel is "" for the clear-the-binding form, which is the
	// same value the command passes for --default, so the tests assert on both.
	LastSetTokenWorkerID string
	LastSetTokenLabel    string
	SetTokenWorker       apitypes.WorkerDTO

	// Agent memory (PRD #90): ListMemory returns Memories; DeleteMemory records the
	// id it was asked to purge.
	Memories            []apitypes.AgentMemoryDTO
	LastDeletedMemoryID string

	// Disposition triage (PRD #94): SetDisposition/DeleteDisposition record the
	// (run, rec) they were called with and the wire status/reason, so a test can
	// assert the exact mapping. JudgeStatsResult is the canned `stats` reply.
	// SetDispositionErr / DeleteDispositionErr, when set, are returned by the
	// matching write in preference to Err, so a test can model the OWNER-ONLY
	// refusal precisely — a uza_ token READS the review fine (RunReview succeeds)
	// and is refused only on the WRITE (404) — which a global Err cannot express
	// (it would fail the resolve read first). The (run, rec) capture still records,
	// so a test can assert the write was actually REACHED before being refused.
	LastDispositionRunID  string
	LastDispositionRecID  string
	LastDispositionStatus string
	LastDispositionReason string
	JudgeStatsResult      apitypes.TriageDTO
	SetDispositionErr     error
	DeleteDispositionErr  error

	// Judge backlog + bulk group disposition (PRD #98 M7). JudgeBacklogResult is the
	// canned `review backlog` reply and LastBacklogBucket records the bucket the command
	// forwarded (empty = the flag was unset and the parameter omitted, so the SERVER's
	// default applies — the fake must not substitute one, or a test could not tell the
	// two apart).
	//
	// BulkDispositionResult / LastBulk* capture the fan-out. BulkDispositionErr, like
	// SetDispositionErr above, is returned by the WRITE in preference to Err, so a test
	// can model a read-succeeds/write-fails sequence precisely; a global Err would fail
	// whichever call came first and prove nothing about the write.
	JudgeBacklogResult    apitypes.JudgeBacklogDTO
	LastBacklogBucket     string
	LastBacklogRun        string
	BulkDispositionResult apitypes.JudgeDispositionResultDTO
	LastBulkItems         []apitypes.JudgeDispositionCoordDTO
	LastBulkStatus        string
	LastBulkReason        string
	BulkDispositionErr    error

	// Err, when non-nil, is returned by every method (before any lookup).
	Err error
}

var _ Client = (*FakeClient)(nil)

func (f *FakeClient) Whoami(context.Context) (apitypes.UserDTO, error) {
	if f.Err != nil {
		return apitypes.UserDTO{}, f.Err
	}
	return f.User, nil
}

func (f *FakeClient) ListRuns(context.Context) ([]apitypes.RunListItemDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Runs, nil
}

func (f *FakeClient) GetRun(_ context.Context, id string) (apitypes.RunDTO, error) {
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	if r, ok := f.RunByID[id]; ok {
		return r, nil
	}
	return apitypes.RunDTO{}, Exitf(ExitNotFound, "run %s not found", id)
}

func (f *FakeClient) RunLogs(_ context.Context, id string, after int32) ([]apitypes.MessageDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	msgs := f.LogsByID[id]
	out := make([]apitypes.MessageDTO, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq > after {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *FakeClient) RunReview(_ context.Context, id string) (*apitypes.ReviewDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if r, ok := f.Reviews[id]; ok {
		return r, nil // r may be nil: a visible-but-unjudged run
	}
	return nil, Exitf(ExitNotFound, "run %s not found", id)
}

// RunInputs mirrors the live owner-only endpoint: a key present in InputsByID is
// the caller's own run (return its queue, possibly empty); a key absent is a
// non-owner / unknown run → ExitNotFound (exit 4), matching the server's 404.
func (f *FakeClient) RunInputs(_ context.Context, id string) ([]apitypes.SteerInputDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if in, ok := f.InputsByID[id]; ok {
		return in, nil
	}
	return nil, Exitf(ExitNotFound, "run %s not found", id)
}

func (f *FakeClient) ListWorkers(context.Context) ([]apitypes.WorkerDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Workers, nil
}

func (f *FakeClient) ListSecrets(context.Context) ([]apitypes.SecretDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Secrets, nil
}

func (f *FakeClient) DeleteWorker(_ context.Context, id string) error {
	f.LastDeletedWorkerID = id
	return f.Err
}

func (f *FakeClient) SetWorkerToken(_ context.Context, id, label string) (apitypes.WorkerDTO, error) {
	f.LastSetTokenWorkerID = id
	f.LastSetTokenLabel = label
	if f.Err != nil {
		return apitypes.WorkerDTO{}, f.Err
	}
	return f.SetTokenWorker, nil
}

func (f *FakeClient) ListMemory(context.Context) ([]apitypes.AgentMemoryDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Memories, nil
}

func (f *FakeClient) DeleteMemory(_ context.Context, id string) error {
	f.LastDeletedMemoryID = id
	return f.Err
}

func (f *FakeClient) SetDisposition(_ context.Context, runID, recID, status, reason string) error {
	f.LastDispositionRunID = runID
	f.LastDispositionRecID = recID
	f.LastDispositionStatus = status
	f.LastDispositionReason = reason
	if f.SetDispositionErr != nil {
		return f.SetDispositionErr
	}
	return f.Err
}

func (f *FakeClient) DeleteDisposition(_ context.Context, runID, recID string) error {
	f.LastDispositionRunID = runID
	f.LastDispositionRecID = recID
	if f.DeleteDispositionErr != nil {
		return f.DeleteDispositionErr
	}
	return f.Err
}

func (f *FakeClient) JudgeStats(context.Context) (apitypes.TriageDTO, error) {
	if f.Err != nil {
		return apitypes.TriageDTO{}, f.Err
	}
	return f.JudgeStatsResult, nil
}

func (f *FakeClient) JudgeBacklog(_ context.Context, bucket, runAnchor string) (apitypes.JudgeBacklogDTO, error) {
	f.LastBacklogBucket = bucket
	f.LastBacklogRun = runAnchor
	if f.Err != nil {
		return apitypes.JudgeBacklogDTO{}, f.Err
	}
	return f.JudgeBacklogResult, nil
}

// BulkSetDispositions records the fan-out and returns the canned result. It captures
// BEFORE the error branch — mirroring SetDisposition — so a test asserting a refusal can
// still prove the write was REACHED, rather than passing on any earlier failure.
func (f *FakeClient) BulkSetDispositions(_ context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error) {
	f.LastBulkItems = items
	f.LastBulkStatus = status
	f.LastBulkReason = reason
	if f.BulkDispositionErr != nil {
		return apitypes.JudgeDispositionResultDTO{}, f.BulkDispositionErr
	}
	if f.Err != nil {
		return apitypes.JudgeDispositionResultDTO{}, f.Err
	}
	return f.BulkDispositionResult, nil
}

func (f *FakeClient) ListRepos(context.Context) ([]apitypes.RepoDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Repos, nil
}

func (f *FakeClient) AdminListUsers(context.Context) ([]apitypes.UserDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminUsers, nil
}

func (f *FakeClient) AdminListRuns(context.Context) ([]apitypes.RunListItemDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminRuns, nil
}

func (f *FakeClient) AdminListWorkers(context.Context) ([]apitypes.AdminWorkerDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminWorkers, nil
}

func (f *FakeClient) AdminUsage(context.Context) (apitypes.AdminUsageDTO, error) {
	if f.Err != nil {
		return apitypes.AdminUsageDTO{}, f.Err
	}
	return f.AdminUsageV, nil
}

func (f *FakeClient) AdminRateLimits(context.Context) ([]apitypes.AdminRateLimitRowDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.RateLimits, nil
}

func (f *FakeClient) StartCLIAuth(context.Context, string, string) (CLIAuthStartResult, error) {
	if f.Err != nil {
		return CLIAuthStartResult{}, f.Err
	}
	return f.AuthStart, nil
}

func (f *FakeClient) PollCLIAuth(context.Context, string, string) (CLIAuthPollResult, error) {
	if f.Err != nil {
		return CLIAuthPollResult{}, f.Err
	}
	if len(f.AuthPolls) == 0 {
		return CLIAuthPollResult{}, nil
	}
	res := f.AuthPolls[0]
	if len(f.AuthPolls) > 1 {
		f.AuthPolls = f.AuthPolls[1:]
	}
	return res, nil
}

func (f *FakeClient) CreateRun(_ context.Context, repoID string, issueIID int64) (apitypes.RunDTO, error) {
	f.LastCreateRepoID = repoID
	f.LastCreateIssueIID = issueIID
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.CreatedRun, nil
}

func (f *FakeClient) SubmitRunInput(_ context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error) {
	f.LastInputRunID = runID
	f.LastInputKind = kind
	f.LastInputBody = body
	f.LastInputSelection = sel
	if f.Err != nil {
		return apitypes.RunInputResponse{}, f.Err
	}
	return f.InputResp, nil
}
