package uzicli

import (
	"context"
	"fmt"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_runs.go holds the run-lifecycle verbs (uzi run / uzi handoff) of the
// Client/HTTPClient split out of client.go (PRD #1017).

func (c *HTTPClient) ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) GetRun(ctx context.Context, id string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id), &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

// RunLogs fetches a run's messages after `after` in bounded pages (?after=&limit=)
// and reassembles them, so a large history cannot die mid-body on one unbounded
// request (issue #160). Paging is internal and transparent: callers still get one
// complete slice or an error. The contract is ALL-OR-NOTHING — if any page fails,
// everything accumulated so far is discarded and (nil, err) is returned, so a
// caller never mistakes a partial history for a complete one.
//
// The loop stops ONLY on an empty page, never on a merely-short one: the server
// clamps ?limit= to its own maxRunMessagesPage, so a short page can mean the server
// capped the request below logsPageSize rather than "this is the last page". Treating
// a short page as terminal would silently truncate the history yet return it with a
// nil error — a partial that looks complete, which this contract forbids. Stopping on
// empty is correct regardless of how the server clamps limit, at the cost of one extra
// trailing (empty) request. maxLogsMessages is the backstop: a hostile server that
// never returns an empty page cannot grow the accumulator without bound.
func (c *HTTPClient) RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error) {
	var all []apitypes.MessageDTO
	seq := after
	for {
		var env struct {
			Messages []apitypes.MessageDTO `json:"messages"`
		}
		path := fmt.Sprintf("/api/runs/%s/messages?after=%d&limit=%d", url.PathEscape(id), seq, logsPageSize)
		if err := c.get(ctx, path, &env); err != nil {
			// All-or-nothing: discard partial history so an error can never look
			// like a complete fetch.
			return nil, err
		}
		if len(env.Messages) == 0 {
			// An empty page is the last page — robust to the server clamping ?limit=
			// below logsPageSize, where a short page is NOT necessarily the end.
			return all, nil
		}
		all = append(all, env.Messages...)
		if len(all) > maxLogsMessages {
			// Hostile/compromised-server backstop: a server that always returns a full
			// page with strictly-increasing seqs never returns an empty page and keeps
			// advancing. Abort all-or-nothing rather than accumulate toward OOM.
			return nil, Exitf(ExitGeneric, "run logs: history exceeded %d messages; aborting", maxLogsMessages)
		}
		// Advance past the last message. On the real server seq is gapless and the
		// query is seq > @after, so max seq is the last element's; the guard below
		// only trips against a misbehaving server that would otherwise loop forever.
		next := env.Messages[len(env.Messages)-1].Seq
		if next <= seq {
			return nil, Exitf(ExitGeneric, "run logs: server returned a page that did not advance past seq %d", seq)
		}
		seq = next
	}
}

func (c *HTTPClient) RunReview(ctx context.Context, id string) (*apitypes.ReviewDTO, *apitypes.PendingJudgeDTO, error) {
	// The envelope is {"review": <dto>|null, "pending_judge": <dto>|null} — both keys
	// always present, either nullable (PRD #119 M1). A 200 with review:null is a
	// visible-but-unjudged run — return a nil review so the command exits 0 rather
	// than inventing a 404, which arrives here as *ExitError{ExitNotFound} from get.
	//
	// pending_judge is decoded as a SECOND return rather than folded into the review
	// because review:null is two states, not one: a run nobody ever judged, and a run
	// whose auto-enqueued judge is still queued or running. The CLI printed "not
	// judged" for both. A pre-#119 server omits the key, which decodes to nil — the
	// same value it sends for "no judge in flight", and the same output as before.
	var env struct {
		Review       *apitypes.ReviewDTO       `json:"review"`
		PendingJudge *apitypes.PendingJudgeDTO `json:"pending_judge"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/review", &env); err != nil {
		return nil, nil, err
	}
	return env.Review, env.PendingJudge, nil
}

func (c *HTTPClient) RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error) {
	var env struct {
		Inputs []apitypes.SteerInputDTO `json:"inputs"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/inputs", &env); err != nil {
		return nil, err
	}
	return env.Inputs, nil
}

func (c *HTTPClient) SetRunPriority(ctx context.Context, id string, expedite bool) (apitypes.RunDTO, error) {
	body := struct {
		Expedite bool `json:"expedite"`
	}{Expedite: expedite}
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.patch(ctx, "/api/runs/"+url.PathEscape(id)+"/priority", body, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) ResumeRunNow(ctx context.Context, id string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// No request body — resume-now is a payload-less POST verb.
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(id)+"/resume-now", nil, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) SetRunMrRework(ctx context.Context, id string, enabled *bool) (apitypes.RunDTO, error) {
	// `enabled` is a *bool with NO omitempty, deliberately: this endpoint's null is
	// meaningful — a nil pointer marshals `"enabled": null`, which the server reads as
	// "clear the override back to inherit". &false must marshal `"enabled": false` (explicit
	// opt-out), and omitempty on a pointer drops only nil, so both cases survive. This is the
	// inverse of CreateRun's mr_rework_enabled, where absence (not null) means inherit.
	body := struct {
		Enabled *bool `json:"enabled"`
	}{Enabled: enabled}
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.put(ctx, "/api/runs/"+url.PathEscape(id)+"/mr-rework", body, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

// CreateRunSeed is PRD #209's optional seeded plan for CreateRun: an
// externally-authored plan and the run's subagent roster. Nil ⇒ an ordinary run
// planned from the issue. It mirrors the server's workersvc.SeededPlan and keeps the
// coupling STRUCTURAL — a Selection cannot ride without a plan, which is the server's
// own rule ("agent_selection requires plan_md"): a nil-vs-set seed carries the choice,
// so no CreateRun caller can express the rejected combination in the first place.
type CreateRunSeed struct {
	// PlanMD is the plan text, forwarded verbatim. Empty is a valid value to SEND —
	// the server returns 422 on an empty/whitespace plan, and letting it decide keeps
	// the scrub-and-empty rule in one place rather than duplicated client-side.
	PlanMD string
	// Selection is the run's roster, or nil for "no selection" (the worker then applies
	// its default: repo agents when the clone has a roster, else own — Success Criterion 5).
	Selection *apitypes.AgentSelection
	// PlannedCommit is the commit the plan was written against (PRD #209 M4), forwarded on
	// `--planned-commit`. Empty ⇒ not sent, so the worker's staleness compare stays inert.
	PlannedCommit string
	// RequireBase makes a base-commit divergence FAIL the run rather than warn
	// (`--require-base`). Only meaningful with a PlannedCommit; the CLI and the server both
	// reject it otherwise.
	RequireBase bool
}

func (c *HTTPClient) CreateRun(ctx context.Context, repoID string, issueIID int64, waitOnLimit *bool, mrReworkEnabled *bool, force bool, seed *CreateRunSeed) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// A struct rather than the map this used to build, because the map could not
	// express the tri-state: `omitempty` on a POINTER omits only nil, so a non-nil
	// false still marshals as `"wait_on_limit": false`. That is the whole contract —
	// omitted means "inherit my default", present-and-false means "explicitly not this
	// run". A `map[string]any` with an `if != nil` guard would work too and is worse:
	// it puts the rule in a conditional instead of in the type.
	//
	// plan_md/agent_selection stay `omitempty` so a nil seed sends NEITHER key and the
	// body is byte-identical to a pre-#209 create (Success Criterion 2). A non-nil seed
	// always sets plan_md (via &PlanMD, so even an empty plan is SENT and the server's
	// 422 fires) and rides the selection only when one was chosen.
	//
	// planned_commit/require_base (M4) are `omitempty` for the same reason — a nil seed,
	// AND a seed that set neither, sends neither key, so the byte-identical guarantee
	// holds for a plain --plan-file create too. planned_commit rides only when non-empty
	// (a *string so omitempty drops it); require_base only when true.
	//
	// mr_rework_enabled (PRD #841 M3) rides the SAME tri-state contract as wait_on_limit: a
	// `*bool` with `omitempty`, so a nil pointer omits the key (the server inherits the
	// account default) while a non-nil &false still marshals `"mr_rework_enabled": false`
	// (explicit opt-out for this run). A bare create sends neither, byte-identical to before.
	//
	// force (issue #856) is a plain `omitempty` bool: a false force sends NO `force` key, so
	// a plain create stays byte-identical to before, while `force:true` (only ever set on
	// --force) asks the server to bypass ONLY the open-MR guard — a run already in progress
	// is never bypassed. false is the common, correct value, so omitempty is the whole point.
	reqBody := struct {
		IssueIID        int64                    `json:"issue_iid"`
		WaitOnLimit     *bool                    `json:"wait_on_limit,omitempty"`
		MrReworkEnabled *bool                    `json:"mr_rework_enabled,omitempty"`
		Force           bool                     `json:"force,omitempty"`
		PlanMD          *string                  `json:"plan_md,omitempty"`
		Selection       *apitypes.AgentSelection `json:"agent_selection,omitempty"`
		PlannedCommit   *string                  `json:"planned_commit,omitempty"`
		RequireBase     bool                     `json:"require_base,omitempty"`
	}{IssueIID: issueIID, WaitOnLimit: waitOnLimit, MrReworkEnabled: mrReworkEnabled, Force: force}
	if seed != nil {
		reqBody.PlanMD = &seed.PlanMD
		reqBody.Selection = seed.Selection
		if seed.PlannedCommit != "" {
			reqBody.PlannedCommit = &seed.PlannedCommit
		}
		reqBody.RequireBase = seed.RequireBase
	}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/runs", reqBody, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) CreateTaskRun(ctx context.Context, repoID, taskContext, baseBranch string, openMR, reviewRequested, thenFixRequested, interactive bool) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// base_branch is `omitempty` so an unset --base sends no key and the worker
	// branches from the caller's seeded HEAD — the CreateRun tri-state convention: an
	// omitted field means "use the default", present means "this, explicitly". open_mr,
	// review_requested and then_fix_requested are plain bools (a task defaults to no MR,
	// no review, no fix; false is the common, correct value to send).
	reqBody := struct {
		Context          string `json:"context"`
		BaseBranch       string `json:"base_branch,omitempty"`
		OpenMr           bool   `json:"open_mr"`
		Interactive      bool   `json:"interactive"`
		ReviewRequested  bool   `json:"review_requested"`
		ThenFixRequested bool   `json:"then_fix_requested"`
	}{Context: taskContext, BaseBranch: baseBranch, OpenMr: openMR, Interactive: interactive, ReviewRequested: reviewRequested, ThenFixRequested: thenFixRequested}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/task-runs", reqBody, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) GetTaskReview(ctx context.Context, id string) (*apitypes.TaskReviewDTO, error) {
	// The envelope is {"task_review": <dto>|null} — a 200 with task_review:null is a
	// visible-but-unreviewed task, so a nil DTO is returned (the command exits 0 and
	// prints the "no review yet" hint); a 404 arrives as *ExitError{ExitNotFound} from get.
	var env struct {
		TaskReview *apitypes.TaskReviewDTO `json:"task_review"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/task-review", &env); err != nil {
		return nil, err
	}
	return env.TaskReview, nil
}

func (c *HTTPClient) DispatchTaskRun(ctx context.Context, runID string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/dispatch", nil, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) SubmitRunInput(ctx context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error) {
	reqBody := apitypes.RunInputRequest{Kind: kind, Body: body, Selection: sel}
	var out apitypes.RunInputResponse
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/inputs", reqBody, &out); err != nil {
		return apitypes.RunInputResponse{}, err
	}
	return out, nil
}
