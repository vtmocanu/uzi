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
//
// PendingJudges (PRD #119) is a SEPARATE map on the same run ids, and it does not
// participate in that tri-state: only Reviews decides 404-vs-found, exactly as
// before, because the server answers both keys off one visibility gate. Its own
// two cases are:
//   - key present, non-nil value → an active judge for that run ("scheduled"/"running")
//   - key present-nil or absent  → no judge in flight
//
// Both spellings of "no pending judge" are the same reply, deliberately: the server
// sends `"pending_judge": null` and a pre-#119 one omits the key, and the CLI must
// render those identically. A test that wants a pending judge over a run with NO
// review still needs the nil Reviews entry — otherwise it is asking about a 404.
type FakeClient struct {
	User           apitypes.UserDTO
	Runs           []apitypes.RunListItemDTO
	RunByID        map[string]apitypes.RunDTO
	LogsByID       map[string][]apitypes.MessageDTO
	InputsByID     map[string][]apitypes.SteerInputDTO
	Reviews        map[string]*apitypes.ReviewDTO
	PendingJudges  map[string]*apitypes.PendingJudgeDTO
	Workers        []apitypes.WorkerDTO
	Repos          []apitypes.RepoDTO
	AdminUsers     []apitypes.UserDTO
	AdminRuns      []apitypes.RunListItemDTO
	AdminWorkers   []apitypes.AdminWorkerDTO
	AdminCLITokens []apitypes.AdminCLITokenDTO
	AdminUsageV    apitypes.AdminUsageDTO
	RateLimits     []apitypes.AdminRateLimitRowDTO

	// Build is the canned GET /api/version reply; BuildErr fails that one call
	// without failing the rest (see BuildInfo). BuildInfoCalls counts the calls —
	// no mutex, like the rest of this fake, because every consumer is a
	// single-goroutine command test.
	Build          apitypes.BuildInfoDTO
	BuildErr       error
	BuildInfoCalls int

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
	// LastCreateWaitOnLimit keeps the POINTER rather than a bool, so a test can tell
	// the three cases apart: nil (the flag was not passed — inherit the user's
	// default), &false and &true. A bool field here would silently collapse "not
	// passed" into "passed false", which is the exact distinction the flag exists to
	// carry, and no test could then catch its loss.
	LastCreateWaitOnLimit *bool
	// LastCreateSeed captures PRD #209's optional seeded plan (nil when the run was
	// created without --plan-file), so a test can assert the plan body and the roster
	// the CLI forwarded — the assertion M3's flag parsing is proven against.
	LastCreateSeed     *CreateRunSeed
	InputResp          apitypes.RunInputResponse
	LastInputRunID     string
	LastInputKind      string
	LastInputBody      string
	LastInputSelection *apitypes.AgentSelection

	// DeleteWorker capture: records the id it was asked to delete.
	LastDeletedWorkerID string

	// SetTokenAutoEligible capture (PRD #111 M2): the secret id the command
	// RESOLVED the label to, and the boolean it sent. The id is what proves the
	// label→id resolution happened client-side against the caller's own list rather
	// than the label being posted to the server.
	LastPoolSecretID string
	LastPoolValue    bool
	PoolSecret       apitypes.SecretDTO

	// SelfMeters drives SelfRateLimits (PRD #111 D23): the caller's own per-token
	// meters, each carrying the server-computed auto-selection status.
	SelfMeters []apitypes.TokenRateLimitDTO

	// Secrets drives ListSecrets (PRD #104 M2).
	Secrets []apitypes.SecretDTO

	// SetWorkerToken capture (PRD #104 M3): the worker id and the label it was asked
	// to bind. LastSetTokenLabel is "" for the clear-the-binding form, which is the
	// same value the command passes for --default, so the tests assert on both.
	LastSetTokenWorkerID string
	LastSetTokenLabel    string
	// LastSetTokenMode is the PRD #111 M3 bind mode the command sent. Recorded
	// separately from the label because the two are what a caller can get WRONG
	// together — "auto" with a leftover label is the realistic half-updated client.
	LastSetTokenMode string
	SetTokenWorker   apitypes.WorkerDTO

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
	LastBacklogCategory   string
	BulkDispositionResult apitypes.JudgeDispositionResultDTO
	LastBulkItems         []apitypes.JudgeDispositionCoordDTO
	LastBulkStatus        string
	LastBulkReason        string
	BulkDispositionErr    error

	// Live stream (PRD #112 M2). StreamEvents is replayed to the subscriber in
	// order; StreamErr models a socket that cannot be opened at all.
	StreamEvents    []apitypes.RunEventDTO
	StreamErr       error
	LastStreamRunID string

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

func (f *FakeClient) RunReview(_ context.Context, id string) (*apitypes.ReviewDTO, *apitypes.PendingJudgeDTO, error) {
	if f.Err != nil {
		return nil, nil, f.Err
	}
	if r, ok := f.Reviews[id]; ok {
		// r may be nil: a visible-but-unjudged run. The pending judge is looked up
		// independently and may be set alongside either — a nil review with a pending
		// judge is "a verdict is on its way", a non-nil one with a pending judge is a
		// re-judge in flight.
		return r, f.PendingJudges[id], nil
	}
	return nil, nil, Exitf(ExitNotFound, "run %s not found", id)
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

func (f *FakeClient) SetTokenAutoEligible(_ context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	f.LastPoolSecretID = id
	f.LastPoolValue = eligible
	if f.Err != nil {
		return apitypes.SecretDTO{}, f.Err
	}
	return f.PoolSecret, nil
}

func (f *FakeClient) SelfRateLimits(context.Context) ([]apitypes.TokenRateLimitDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.SelfMeters, nil
}

func (f *FakeClient) DeleteWorker(_ context.Context, id string) error {
	f.LastDeletedWorkerID = id
	return f.Err
}

func (f *FakeClient) SetWorkerBindMode(_ context.Context, id, mode, label string) (apitypes.WorkerDTO, error) {
	f.LastSetTokenWorkerID = id
	f.LastSetTokenLabel = label
	f.LastSetTokenMode = mode
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

func (f *FakeClient) JudgeBacklog(_ context.Context, bucket, runAnchor, category string) (apitypes.JudgeBacklogDTO, error) {
	f.LastBacklogBucket = bucket
	f.LastBacklogRun = runAnchor
	f.LastBacklogCategory = category
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

// BuildInfo returns Build, or BuildErr when set. BuildErr is SEPARATE from the
// blanket Err on purpose: `uzi version` must succeed when the server is
// unreachable, so a test needs to fail this one call without failing everything
// else the command might do. Err still applies when BuildErr is unset, so the
// usual "every method fails" fixture keeps working.
//
// It counts its calls, and that counter is not a convenience: with the version-skew
// hook on the root command, "the probe was skipped" and "the probe ran and printed
// nothing" produce IDENTICAL output, so every exemption, suppression and cache-hit
// claim is unobservable without it. Asserting on absent stderr would pass against an
// implementation that probes on every command and merely stays quiet.
func (f *FakeClient) BuildInfo(context.Context) (apitypes.BuildInfoDTO, error) {
	f.BuildInfoCalls++
	if f.BuildErr != nil {
		return apitypes.BuildInfoDTO{}, f.BuildErr
	}
	if f.Err != nil {
		return apitypes.BuildInfoDTO{}, f.Err
	}
	return f.Build, nil
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

func (f *FakeClient) AdminListCLITokens(context.Context) ([]apitypes.AdminCLITokenDTO, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.AdminCLITokens, nil
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

func (f *FakeClient) CreateRun(_ context.Context, repoID string, issueIID int64, waitOnLimit *bool, seed *CreateRunSeed) (apitypes.RunDTO, error) {
	f.LastCreateRepoID = repoID
	f.LastCreateIssueIID = issueIID
	f.LastCreateWaitOnLimit = waitOnLimit
	f.LastCreateSeed = seed
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

// StreamRun replays StreamEvents to a subscriber and then holds the stream open
// until the caller cancels, mirroring a live socket that has simply gone quiet
// rather than one that ended. StreamErr models an unusable socket (the D8
// fall-back-to-polling path); it is returned in preference to Err so a test can
// have the REST reads succeed while only the stream fails, which is exactly the
// degradation the TUI has to handle and which a global Err cannot express.
//
// The events go through NormalizeRunEvent, like the live decode boundary, so a
// fake cannot deliver a frame shape the real client would have made inert.
func (f *FakeClient) StreamRun(ctx context.Context, runID string) (*RunStream, error) {
	f.LastStreamRunID = runID
	if f.StreamErr != nil {
		return nil, f.StreamErr
	}
	if f.Err != nil {
		return nil, f.Err
	}
	return NewRunStream(ctx, f.StreamEvents), nil
}
