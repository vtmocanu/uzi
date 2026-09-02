package uzicli

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_runs.go holds the FakeClient run-lifecycle methods (uzi run / uzi handoff)
// split out of fake.go (PRD #1017).

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
	// The hook wins over the static map so a sequencing test drives the poll loop
	// directly; it also takes precedence over RunByID for the same reason a test
	// would set it — to return something the static lookup cannot express (a changing
	// status, a transient error).
	if f.GetRunHook != nil {
		return f.GetRunHook(id)
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
	// The hook wins over the static map so a sequencing test drives the poll loop
	// directly, for the same reason GetRunHook exists — to return something the
	// static filter cannot express (a changing message set across calls).
	if f.RunLogsHook != nil {
		return f.RunLogsHook(id, after)
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

// SetRunPriority records the (run id, expedite) it was called with and returns the
// canned run. SetRunPriorityErr wins over the blanket Err — mirroring SetDispositionErr
// — so a test can model the non-queued 409 on the WRITE while the capture still proves
// the write was REACHED with the right args.
func (f *FakeClient) SetRunPriority(_ context.Context, id string, expedite bool) (apitypes.RunDTO, error) {
	f.LastPriorityRunID = id
	f.LastPriorityExpedite = expedite
	if f.SetRunPriorityErr != nil {
		return apitypes.RunDTO{}, f.SetRunPriorityErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.PriorityRun, nil
}

// ResumeRunNow records the run id it was called with and returns the canned run.
// ResumeRunNowErr wins over the blanket Err — mirroring SetRunPriorityErr — so a test can
// model the non-held 409 on the write while the capture still proves it was REACHED.
func (f *FakeClient) ResumeRunNow(_ context.Context, id string) (apitypes.RunDTO, error) {
	f.LastResumeRunID = id
	if f.ResumeRunNowErr != nil {
		return apitypes.RunDTO{}, f.ResumeRunNowErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.ResumedRun, nil
}

// SetRunMrRework records the run id and the tri-state pointer it was called with and
// returns the canned run. It captures BEFORE the error branch (mirroring SetRunPriority)
// so a test asserting a 404 still proves the write was reached; SetRunMrReworkErr wins
// over the blanket Err.
func (f *FakeClient) SetRunMrRework(_ context.Context, id string, enabled *bool) (apitypes.RunDTO, error) {
	f.LastMrReworkRunID = id
	f.LastMrReworkEnabled = enabled
	if f.SetRunMrReworkErr != nil {
		return apitypes.RunDTO{}, f.SetRunMrReworkErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.MrReworkRun, nil
}

func (f *FakeClient) CreateRun(_ context.Context, repoID string, issueIID int64, waitOnLimit *bool, mrReworkEnabled *bool, force bool, seed *CreateRunSeed) (apitypes.RunDTO, error) {
	f.LastCreateRepoID = repoID
	f.LastCreateIssueIID = issueIID
	f.LastCreateWaitOnLimit = waitOnLimit
	f.LastCreateMrRework = mrReworkEnabled
	f.LastCreateForce = force
	f.LastCreateSeed = seed
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.CreatedRun, nil
}

// CreateTaskRun records the handoff-create args and returns CreatedTaskRun. It
// captures BEFORE the error branch (mirroring CreateRun) so a test asserting a
// refusal still proves the write was reached; CreateTaskRunErr wins over Err so a
// 422 can be modelled on this verb alone. It appends "create" to TaskCalls so the
// create → push → dispatch ordering is observable.
func (f *FakeClient) CreateTaskRun(_ context.Context, repoID, taskContext, baseBranch string, openMR, reviewRequested, thenFixRequested, interactive bool) (apitypes.RunDTO, error) {
	f.LastCreateTaskRepoID = repoID
	f.LastCreateTaskContext = taskContext
	f.LastCreateTaskBaseBranch = baseBranch
	f.LastCreateTaskOpenMr = openMR
	f.LastCreateTaskInteractive = interactive
	f.LastCreateTaskReview = reviewRequested
	f.LastCreateTaskThenFix = thenFixRequested
	f.TaskCalls = append(f.TaskCalls, "create")
	if f.CreateTaskRunErr != nil {
		return apitypes.RunDTO{}, f.CreateTaskRunErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.CreatedTaskRun, nil
}

// GetTaskReview returns the canned TaskReview (nil ⇒ "no review yet"). GetTaskReviewErr
// wins over Err so a 404 can be modelled on this verb alone; LastTaskReviewID records the
// requested target run id.
func (f *FakeClient) GetTaskReview(_ context.Context, id string) (*apitypes.TaskReviewDTO, error) {
	f.LastTaskReviewID = id
	if f.GetTaskReviewErr != nil {
		return nil, f.GetTaskReviewErr
	}
	if f.Err != nil {
		return nil, f.Err
	}
	return f.TaskReview, nil
}

// DispatchTaskRun records the run id it was asked to dispatch and returns
// DispatchedRun. DispatchTaskRunErr wins over Err so a 404 can be modelled on this
// verb alone; it appends "dispatch" to TaskCalls (after the create and any push)
// so a test can assert the Decision-6 ordering.
func (f *FakeClient) DispatchTaskRun(_ context.Context, runID string) (apitypes.RunDTO, error) {
	f.LastDispatchRunID = runID
	f.TaskCalls = append(f.TaskCalls, "dispatch")
	if f.DispatchTaskRunErr != nil {
		return apitypes.RunDTO{}, f.DispatchTaskRunErr
	}
	if f.Err != nil {
		return apitypes.RunDTO{}, f.Err
	}
	return f.DispatchedRun, nil
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
