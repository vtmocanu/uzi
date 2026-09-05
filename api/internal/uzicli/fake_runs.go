package uzicli

import (
	"context"
	"sort"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// fake_runs.go holds the FakeClient run-lifecycle methods (uzi run / uzi handoff)
// split out of fake.go (PRD #1017).

func (f *FakeClient) ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	f.LastListRunsCtx = ctx
	f.ListRunsCalls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Runs, nil
}

func (f *FakeClient) GetRun(ctx context.Context, id string) (apitypes.RunDTO, error) {
	f.LastGetRunCtx = ctx
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

// RunLogsPage mirrors the live single-request paging verb: it runs the SAME
// client-side exclusivity validation as the real client (a forbidden combination
// returns ExitUsage and is NOT recorded, matching the real client which validates
// before the request), then records the query in RunLogsPageCalls and filters the
// seeded LogsByID[id] slice — the same source RunLogs filters — by the query window.
// PayloadMax has no fake behaviour: the fake returns untrimmed DTOs (a test that
// cares seeds PayloadTruncated itself).
func (f *FakeClient) RunLogsPage(_ context.Context, id string, q LogsPageQuery) ([]apitypes.MessageDTO, error) {
	if err := q.validate(); err != nil {
		// A forbidden combination is rejected before any request would be made, so it
		// is NOT recorded — a test asserts zero calls on the bad combo.
		return nil, err
	}
	f.RunLogsPageCalls = append(f.RunLogsPageCalls, q)
	if f.Err != nil {
		return nil, f.Err
	}
	// Work on a seq-sorted copy so the tail / before windows are correct regardless of
	// the seeded order.
	src := f.LogsByID[id]
	sorted := make([]apitypes.MessageDTO, len(src))
	copy(sorted, src)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	var out []apitypes.MessageDTO
	switch {
	case q.Tail > 0:
		// The newest Tail messages, ascending.
		start := len(sorted) - int(q.Tail)
		if start < 0 {
			start = 0
		}
		out = append(out, sorted[start:]...)
	case q.Before > 0:
		// The newest Limit messages with seq < Before, ascending.
		var below []apitypes.MessageDTO
		for _, m := range sorted {
			if m.Seq < q.Before {
				below = append(below, m)
			}
		}
		start := 0
		if q.Limit > 0 && len(below) > int(q.Limit) {
			start = len(below) - int(q.Limit)
		}
		out = append(out, below[start:]...)
	default:
		// After path: messages with seq > After, ascending, capped to Limit if set.
		for _, m := range sorted {
			if m.Seq > q.After {
				out = append(out, m)
			}
			if q.Limit > 0 && len(out) >= int(q.Limit) {
				break
			}
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
