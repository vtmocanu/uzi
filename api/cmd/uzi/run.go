package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Run-input kinds, the exact wire strings the inputs endpoint accepts
// (handler/workers.go runInputKinds). Named so a typo is a compile error, not a
// silent 400.
const (
	kindApprovePlan = "approve_plan"
	kindRejectPlan  = "reject_plan"
	kindCancel      = "cancel"
	kindFollowUp    = "follow_up"
	kindRevisePlan  = "revise_plan"
	kindAnswer      = "answer"
	kindStop        = "stop"
	kindScope       = "scope"
)

// agentSources are the two rosters a plan approval may draw its subagents from
// (apitypes.AgentSelection.Source). "own" is the run owner's template roster; "repo"
// is the set the worker detected in the clone's .claude/agents/.
const (
	agentSourceOwn  = "own"
	agentSourceRepo = "repo"
)

// statusLimitWait is the status a run carries while parked behind its owner's
// exhausted Anthropic usage window (PRD #35). Named so the three surfaces that must
// agree about it — the follow loop, the steer queue's delivery label and `uzi run
// get`'s detail block — compare against one literal.
//
// NON-TERMINAL, and that is the whole reason it needs handling rather than a slot in
// terminalRunStatuses: a parked run is promoted back to `queued` by the server's sweep
// once retry_not_before passes, and resumes on the same SDK session. Treating it as
// terminal would make `--follow` exit on a run that is about to produce more messages.
const statusLimitWait = "limit_wait"

// statusPoolWait is the status an `auto` run carries while HELD because its token pool
// is genuinely empty (PRD #754 M4). Named for the same reason as statusLimitWait: the
// follow loop, the steer-queue delivery label and `uzi run get`'s detail block compare
// against one literal.
//
// NON-TERMINAL, and non-locking (it does not lock the issue). A held run resumes when a
// token is pooled (reactively, or manually) in M5. Like limit_wait, treating it as
// terminal would make `--follow` exit on a run that is about to produce more messages.
const statusPoolWait = "pool_wait"

// logsPollInterval is how often `uzi run logs --follow` re-polls
// /api/runs/{id}/messages?after=<seq>. REST polling ships instead of a WebSocket
// (PRD #64 Out of scope). A var, not a const, only so tests can shrink the wait;
// nothing at runtime reassigns it.
var logsPollInterval = 2 * time.Second

// terminalRunStatuses are the run states from which no further messages can
// arrive. `uzi run logs --follow` stops once the run reaches one of them (after a
// final drain) instead of polling forever — otherwise an agent capturing a
// finished run hangs. Mirrors the store's `status NOT IN (...)` active filter.
//
// statusLimitWait is NOT here, and it is the one omission worth writing down because
// it looks like an oversight: a park is a long silence, which is exactly what this map
// is used to end. But the run resumes, and stopping the follow on it would truncate
// the capture in the middle of a run that finishes normally hours later. The silence
// is instead EXPLAINED — see the follow loop's one-shot notice below.
var terminalRunStatuses = map[string]bool{
	"completed": true,
	"failed":    true,
	"cancelled": true,
}

// allRunStatusesOrder is the run status enum in wire/enum order (matching
// runs_status_check — last rewritten by migration 00165, eleven values), the ONE source
// of truth both allRunStatuses (membership)
// and the `--until` validation-error's "valid: …" list derive from — so a status added
// here can never be silently omitted from the human-readable enumeration.
var allRunStatusesOrder = []string{
	"queued",
	"claimed",
	"running",
	"awaiting_approval",
	"awaiting_input",
	"awaiting_followup",
	statusLimitWait,
	statusPoolWait,
	"completed",
	"failed",
	"cancelled",
}

// allRunStatuses is the run status enum the skill documents and migration
// 00165 constrains (runs_status_check). It is the source of truth `run wait`
// validates `--until` against, so a typo'd target is a clean usage error rather than
// a silent forever-wait. A status the SERVER reports that is NOT in this set is a
// newer server than this binary (surfaced, treated non-terminal — never a target,
// since `--until` can only name a member here). Derived from allRunStatusesOrder so the
// membership set and the enumerated list cannot drift.
var allRunStatuses = func() map[string]bool {
	m := make(map[string]bool, len(allRunStatusesOrder))
	for _, s := range allRunStatusesOrder {
		m[s] = true
	}
	return m
}()

// defaultWaitStates is `run wait`'s `--until` default (PRD #264 D2): the "actionable"
// set — every state that needs the caller or ends the run. It INCLUDES awaiting_followup
// (PRD #517 D9): an interactive task parked awaiting the user's next follow-up needs the
// caller and does NOT auto-resume, so a bare `uzi run wait <id>` must stop on it. It
// deliberately OMITS queued/claimed/running (still working), limit_wait (auto-resumes;
// parking on it is legitimate) AND pool_wait (PRD #754: a held run resumes on its own
// once a token is pooled, so it is legitimate to wait through, exactly like limit_wait),
// so a bare `uzi run wait <id>` returns at the plan gate, a clarification park, a
// follow-up park, or a terminal — the common "wait for the gate OR the end" case.
var defaultWaitStates = []string{"awaiting_approval", "awaiting_input", "awaiting_followup", "completed", "failed", "cancelled"}

// run wait poll cadence and transient-blip resilience knobs (PRD #264 D1/D9). Vars,
// not consts, only so tests shrink the waits; nothing at runtime reassigns them.
//
//   - runWaitPollInterval — the default `--interval` between polls of GET /api/runs/:id.
//   - runWaitBackoff — the pause after a mid-wait ExitUnreachable before retrying.
//   - runWaitMaxUnreachable — how many CONSECUTIVE unreachable polls end the wait with
//     exit 6. A single 5xx/network blip (the skill calls it "transient; back off and
//     retry") must not kill a multi-hour gate-wait, so the default is small but > 1.
var (
	runWaitPollInterval   = 3 * time.Second
	runWaitBackoff        = 2 * time.Second
	runWaitMaxUnreachable = 5
)

// runAgeCell renders the AGE column for `uzi run list` (issue #256 M5), the CLI twin
// of the web's runDurationLabel. AGE means different things per state, so the anchor is
// chosen by status (Decision 2), not by a single "created" reading:
//
//   - running   → time since StartedAt (when the agent began), or CreatedAt if unstamped.
//   - claimed   → time since ClaimedAt (when a worker took it), or CreatedAt if unstamped.
//   - queued    → time since CreatedAt (how long it has waited to be claimed).
//   - awaiting_approval / awaiting_input / awaiting_followup / limit_wait / pool_wait →
//     time since UpdatedAt, i.e. how long it has been parked/held in that waiting state.
//   - completed / failed / cancelled → the STATIC span FinishedAt−StartedAt, how long it
//     actually ran, independent of now. A terminal run with no StartedAt (cancelled or
//     failed before it ever started) never ran, so it renders "-".
//   - any other/unknown status → "-".
//
// The live (non-terminal) buckets pass now.Sub(anchor) through formatUptimeDuration, which
// floors negatives (clock skew) to "<1m". now is taken as a parameter, not read from the
// clock inside, so the buckets unit-test deterministically — the same split as
// formatUptimeDuration out of uptimeCell.
func runAgeCell(r apitypes.RunDTO, now time.Time) string {
	var anchor *time.Time
	switch r.Status {
	case "running":
		if r.StartedAt != nil {
			anchor = r.StartedAt
		} else {
			anchor = &r.CreatedAt
		}
	case "claimed":
		if r.ClaimedAt != nil {
			anchor = r.ClaimedAt
		} else {
			anchor = &r.CreatedAt
		}
	case "queued":
		anchor = &r.CreatedAt
	case "awaiting_approval", "awaiting_input", "awaiting_followup", statusLimitWait, statusPoolWait:
		anchor = &r.UpdatedAt
	case "completed", "failed", "cancelled":
		// A static ran-span, not a live age: only meaningful when the run both started
		// and finished. Missing either end (never started) → "-".
		if r.StartedAt == nil || r.FinishedAt == nil {
			return "-"
		}
		return formatUptimeDuration(r.FinishedAt.Sub(*r.StartedAt))
	default:
		return "-"
	}
	if anchor == nil {
		return "-"
	}
	return formatUptimeDuration(now.Sub(*anchor))
}

// newRunCmd — `uzi run` and its verbs. Every verb is wired to the Client: the reads
// (list/get/logs/review/inputs) and the writes (create/approve/reject/cancel/
// follow-up/answer).
func newRunCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "List, inspect, and drive runs",
	}

	wait := &cobra.Command{
		Use:   "wait <run-id>",
		Short: "Block until a run reaches a chosen state",
		Long: "Poll a run until its status enters the `--until` set, then exit 0 (PRD #264).\n\n" +
			"With no `--until`, it stops on any state that needs you or ends the run: " +
			strings.Join(defaultWaitStates, ", ") + ". It does NOT stop " +
			"on queued/claimed/running (still working), limit_wait (auto-resumes), or pool_wait " +
			"(an auto run held on an empty token pool; resumes when a token is pooled), so a bare " +
			"`uzi run wait <id>` waits for the plan gate OR the end.\n\n" +
			"Transitions print to stderr; `--json` prints the final run object (same shape as " +
			"`run get --json`) to stdout. Exit codes: 0 a target state was reached (including if " +
			"the run was already in one); 4 not found; 6 server unreachable after repeated " +
			"failures; 7 `--timeout` elapsed before any target state.\n\n" +
			"After approving a plan, NARROW the second wait — `run wait <id> --until " +
			"completed,failed,cancelled` — because a run lingers at awaiting_approval for a beat " +
			"after a successful approve, so a bare `run wait` would return immediately at the " +
			"gate it just cleared.\n\n" +
			"To wait for a REVISED plan after `run revise`, capture the latest plan seq first " +
			"(`run logs <id> --json | jq -rs '[.[]|select(.kind==\"plan\")|.seq]|max // 0'`), then " +
			"`run wait <id> --min-plan-seq <seq>`. That gates ONLY the awaiting_approval stop — it " +
			"returns at the gate only once a plan message with seq > <seq> exists, so it does not " +
			"return on the stale gate left by the pre-revise plan. Terminal states (and every other " +
			"target) still stop unconditionally.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			until, _ := cmd.Flags().GetStringSlice("until")
			interval, _ := cmd.Flags().GetDuration("interval")
			timeout, _ := cmd.Flags().GetDuration("timeout")
			minPlanSeq, _ := cmd.Flags().GetInt("min-plan-seq")
			return runWait(env, gf, c, cmd, args[0], until, interval, timeout, minPlanSeq)
		},
	}
	wait.Flags().StringSlice("until", nil,
		"comma-separated run statuses to wait for (default: "+strings.Join(defaultWaitStates, ",")+")")
	wait.Flags().Duration("interval", runWaitPollInterval, "how often to poll the run's status")
	wait.Flags().Duration("timeout", 0, "give up with exit 7 after this long (default: no timeout)")
	wait.Flags().Int("min-plan-seq", -1,
		"after `run revise`, only stop at awaiting_approval once a plan message with seq > N exists "+
			"(-1 = off; 0 = wait for any plan, since plan seqs start at 1). Terminal states still stop unconditionally")

	create := &cobra.Command{
		Use:   "create",
		Short: "Start a run on a repo's PRD issue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoID, _ := cmd.Flags().GetString("repo")
			issue, _ := cmd.Flags().GetInt64("issue")
			if strings.TrimSpace(repoID) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--repo is required (a repo id from `uzi repo list`)")
			}
			if issue <= 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "--issue must be a positive issue IID")
			}
			seed, err := seededPlanFlag(env, cmd)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			force, _ := cmd.Flags().GetBool("force")
			run, err := c.CreateRun(cmd.Context(), repoID, issue, waitOnLimitFlag(cmd), mrReworkFlag(cmd), force, seed)
			if err != nil {
				return err
			}
			return renderCreatedRun(env, gf, run)
		},
	}
	create.Flags().String("repo", "", "repo id to run against (see 'uzi repo list')")
	create.Flags().Int64("issue", 0, "the PRD issue IID to run")
	create.Flags().Bool("wait-on-limit", false,
		"park this run until the Anthropic usage window reopens instead of failing it; "+
			"omit to inherit your Settings default, or pass --wait-on-limit=false to force off")
	create.Flags().Bool("mr-rework", false,
		"enable or disable auto-rework of this run's MR review comments; "+
			"omit to inherit the account default, or pass --mr-rework=false to force off")
	create.Flags().Bool("force", false,
		"re-run even if the issue already has an open MR from a completed run "+
			"(bypasses only the open-MR guard; a run already in progress is never bypassed)")
	// PRD #209 seeded plan. --agent-source/--exclude-agents reuse the plan gate's flag
	// names and validation (approveSelection); both are meaningful only alongside a plan.
	create.Flags().String("plan-file", "",
		"seed the run with a pre-written plan from this file (or '-' for stdin), skipping "+
			"the planning turn and the approval gate (PRD #209)")
	create.Flags().String("agent-source", "",
		"which subagent roster the seeded run uses: own|repo (requires --plan-file)")
	create.Flags().StringSlice("exclude-agents", nil,
		"subagents to drop from the chosen source (requires --agent-source and --plan-file)")
	// PRD #209 M4 staleness guard. --planned-commit records the commit the plan was
	// written against; the worker warns (or, with --require-base, fails) if the clone's
	// base has moved since. Both require --plan-file, and --require-base requires
	// --planned-commit.
	create.Flags().String("planned-commit", "",
		"the commit the seeded plan was written against; the worker warns if the clone's "+
			"base has moved since (requires --plan-file)")
	create.Flags().Bool("require-base", false,
		"fail the run instead of warning if the clone's base differs from --planned-commit "+
			"(requires --planned-commit)")

	approve := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			source, _ := cmd.Flags().GetString("agent-source")
			exclude, _ := cmd.Flags().GetStringSlice("exclude-agents")
			sel, err := approveSelection(source, exclude)
			if err != nil {
				return err
			}
			return submitInput(env, gf, c, cmd, args[0], kindApprovePlan, "", sel)
		},
	}
	approve.Flags().String("agent-source", "", "which subagent roster to run: own|repo (default: the run's own default)")
	approve.Flags().StringSlice("exclude-agents", nil, "subagents to drop from the chosen source (requires --agent-source)")

	reject := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a rejection needs a reason: pass -m <reason> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindRejectPlan, msg, nil)
		},
	}
	reject.Flags().StringP("message", "m", "", "reason to send back to the agent (or pipe it on stdin)")

	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// PRD #503 M3: the cancel reason is OPTIONAL — unlike reject, no empty check.
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			return submitInput(env, gf, c, cmd, args[0], kindCancel, msg, nil)
		},
	}
	cancel.Flags().StringP("message", "m", "", "reason for cancelling (optional; or pipe it on stdin)")

	stop := &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Gracefully stop a run: interactive (finalize) or milestone (cap at completed count)",
		Long: "Gracefully wind down a run (PRD #517, #634). Unlike `cancel`, which aborts mid-turn, " +
			"`stop` lets the worker FINALIZE.\n\n" +
			"On an interactive task run it finishes the current turn, pushes the branch, opens the " +
			"merge request (when the run requested one), and reports `completed`. The stop is " +
			"serviced ahead of any buffered follow-up.\n\n" +
			"On a milestone-structured issue run it maps to a scope ceiling at the ALREADY-COMPLETED " +
			"milestone count: the run finalizes the committed slice (pushes the branch, opens the " +
			"merge request when requested) and starts no further milestone — the same graceful " +
			"finalize, just scoped to what is already done. Use `run scope --through N` instead to " +
			"complete through a later milestone before finalizing.\n\n" +
			"An optional message can accompany the stop (pass -m or pipe it on stdin). A stop on a " +
			"run that has already finished answers 409 (exit 5).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// The stop message is OPTIONAL, like a cancel reason — no empty check.
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			return submitInput(env, gf, c, cmd, args[0], kindStop, msg, nil)
		},
	}
	stop.Flags().StringP("message", "m", "", "an optional message to accompany the stop (or pipe it on stdin)")

	scope := &cobra.Command{
		Use:   "scope <run-id>",
		Short: "Cap a milestone run's scope: complete through milestone N, then finalize (issue runs)",
		Long: "Set an operator SCOPE CEILING on an in-flight milestone-structured issue run: the run \n" +
			"completes through milestone N (1-based count over the approved, frozen milestone list), \n" +
			"then finalizes the committed slice (pushes the branch, opens the merge request when \n" +
			"requested) and starts no further milestone. The ceiling is clamped to " +
			"[already-completed, total]. A later `run scope` (or `run stop`) supersedes an earlier one. \n" +
			"Owner-only; valid only on a milestone-structured issue run (409 otherwise).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("through") {
				return uzicli.Exitf(uzicli.ExitUsage, "run scope needs --through N")
			}
			n, _ := cmd.Flags().GetInt("through")
			if err := submitInput(env, gf, c, cmd, args[0], kindScope, strconv.Itoa(n), nil); err != nil {
				return err
			}
			// The server clamps the ceiling to [already-completed, total]; the applied value
			// and its disposition surface via `run inputs`, not here (a read-back would race
			// the worker settling it). Point the operator there.
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr, "run `uzi run inputs %s` to see the applied (clamped) ceiling and its disposition\n", args[0])
			}
			return nil
		},
	}
	scope.Flags().Int("through", 0, "complete through milestone N, then stop (1-based count over the frozen list)")

	followUp := &cobra.Command{
		Use:   "follow-up <run-id>",
		Short: "Send a follow-up message to a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a follow-up needs a message: pass -m <message> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindFollowUp, msg, nil)
		},
	}
	followUp.Flags().StringP("message", "m", "", "the follow-up message (or pipe it on stdin)")

	revise := &cobra.Command{
		Use:   "revise <run-id>",
		Short: "Revise a run's plan at the approval gate (re-plan without stopping the run)",
		Long: "Send feedback to a run parked at its plan-approval gate (`awaiting_approval`) so the " +
			"agent re-plans from your notes and re-gates, WITHOUT stopping the run — unlike `reject`, " +
			"which ends it.\n\n" +
			"The run stays live: the agent revises its plan in place using your feedback and returns to " +
			"the gate for another approve/reject/revise decision. Revisions are capped by the run's " +
			"revision limit; once exhausted — or if the run has already finished — the server answers " +
			"409 (exit 5). Use it on a run parked at its `awaiting_approval` gate; that is where the " +
			"agent folds your feedback into a new plan.\n\n" +
			"A revision needs feedback (an empty one tells the agent nothing to change), so pass -m or " +
			"pipe it on stdin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			msg, _ := cmd.Flags().GetString("message")
			msg = resolveMessage(env, msg)
			if strings.TrimSpace(msg) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a revision needs a message: pass -m <feedback> or pipe it on stdin")
			}
			return submitInput(env, gf, c, cmd, args[0], kindRevisePlan, msg, nil)
		},
	}
	revise.Flags().StringP("message", "m", "", "the plan feedback to send back (or pipe it on stdin)")

	answer := &cobra.Command{
		Use:   "answer <run-id>",
		Short: "Answer the clarifying question a run is waiting on",
		Long: "Answer the question a run asked with ask_user (PRD #88). The run parks in " +
			"`awaiting_input` until you reply, then resumes the same agent session with your " +
			"answer.\n\n" +
			"The open question is read from the run's own feed — the newest `question` " +
			"message — rather than from a run field, so the CLI, the web UI and Slack all " +
			"derive it the same way. Pass -m, or pipe the answer on stdin. Repeat -m once " +
			"per question when the agent asked several; answers are matched in order.\n\n" +
			"The answer names the question it answers, so a reply written against a question " +
			"the agent has already moved on from is rejected rather than applied to the " +
			"current one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			answers, _ := cmd.Flags().GetStringArray("message")
			if len(answers) == 1 {
				answers[0] = resolveMessage(env, answers[0])
			} else if len(answers) == 0 {
				answers = []string{resolveMessage(env, "")}
			}
			if len(answers) == 1 && strings.TrimSpace(answers[0]) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "an answer needs text: pass -m <answer> or pipe it on stdin")
			}
			q, err := openQuestion(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			// Cross-check the count against the QUESTION, not just against the server's
			// upper bound. Answers are index-aligned, so a miscount does not fail — it
			// silently pairs answer 2 with question 3 and hands the agent a confident
			// mismatch. The server cannot catch this: maxAnswerCount bounds the top end
			// but nothing server-side knows how many questions this id asked.
			if len(answers) != len(q.Questions) {
				return uzicli.Exitf(uzicli.ExitUsage,
					"this question has %d part(s) but you gave %d answer(s) — answers are matched in order, so pass one -m per part",
					len(q.Questions), len(answers))
			}
			// Reject a blank among several. A lone blank is already rejected above; a
			// blank in the middle is worse, because it looks like an answer and reads to
			// the agent as "(no answer given)" for a question the user believed they had
			// answered.
			for i, a := range answers {
				if strings.TrimSpace(a) == "" {
					return uzicli.Exitf(uzicli.ExitUsage, "answer %d is empty — every part needs text", i+1)
				}
			}
			body, err := json.Marshal(answerBody{QuestionID: q.QuestionID, Answers: answers})
			if err != nil {
				return err
			}
			return submitInput(env, gf, c, cmd, args[0], kindAnswer, string(body), nil)
		},
	}
	answer.Flags().StringArrayP("message", "m", nil, "the answer (repeat once per question; or pipe a single answer on stdin)")

	inputs := &cobra.Command{
		Use:   "inputs <run-id>",
		Short: "List a run's steer queue (follow-ups) with delivery status",
		Long: "List the steer queue sent to a run (PRD #95, #634), newest first, each with its " +
			"KIND and state plus a relative age. Owner-only: a read-only admin token gets 404 on " +
			"another user's run.\n\n" +
			"The queue lists two kinds. A follow-up (kind='follow_up') carries a delivery state — " +
			"queued (the worker has not drained it yet) or delivered (handed to the worker for its " +
			"next turn). An operator scope directive (kind='scope', PRD #634) carries its " +
			"disposition instead — applied/declined/superseded, or active while the ceiling is still " +
			"pending — because a scope row is never consumed. Approve/reject/cancel are omitted.\n\n" +
			"A chat run seeds every chat turn as a follow_up, so `uzi run inputs` on a chat run lists " +
			"the seeded prompt and all chat turns; an issue run's queue starts empty (its prompt " +
			"rides the claim payload, not an input row).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			list, err := c.RunInputs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// The agent contract: emit the raw DTO list. State is derived
				// client-side (from consumed_at + the run's status), so an agent
				// computes it from these fields itself — the CLI never fetches the
				// run in --json mode.
				return p.JSON(list)
			}
			// Human render only: derive the delivery state (Decision 7), which needs
			// the run's live status for the gate/terminal nuance. One cheap GetRun; if
			// it fails, status stays "" and the state degrades to the queued/delivered
			// floor (Decision 10). Skipped entirely for an empty queue.
			status := ""
			if len(list) > 0 {
				if run, err := c.GetRun(cmd.Context(), args[0]); err == nil {
					status = run.Status
					if run.Kind == runkind.Chat {
						// N3: a chat run's queue is every chat turn. Note it only when it
						// actually applies, so an issue run's output stays clean.
						_, _ = fmt.Fprintln(env.Stderr, "note: chat run — this queue lists every chat turn as a follow-up (chat has its own web composer, unaffected)")
					}
				}
			}
			return renderRunInputs(p, list, status)
		},
	}

	expedite := &cobra.Command{
		Use:   "expedite <run-id>",
		Short: "Bump a queued run to the front of the claim queue (or --clear to undo)",
		Long: "Bump ONE queued run to the front of the claim queue so a worker picks it up before " +
			"the rest (PRD #320). It matters only before a run is claimed: ordering is fixed once a " +
			"worker takes the run, so a non-queued run is a 409 (exit 5). A foreign or unknown run is " +
			"a 404 (exit 4).\n\n" +
			"`--clear` undoes it — it removes the manual override and returns the run to its kind " +
			"default priority (it does NOT demote the run below normal).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// --clear removes the manual override (expedite=false); its absence expedites.
			clear, _ := cmd.Flags().GetBool("clear")
			run, err := c.SetRunPriority(cmd.Context(), args[0], !clear)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	expedite.Flags().Bool("clear", false, "clear the manual expedite (undo), returning the run to its kind default priority")

	resumeNow := &cobra.Command{
		Use:   "resume-now <run-id>",
		Short: "Resume a run held waiting for a pooled Anthropic token, without waiting for the sweeper",
		Long: "Resume ONE run held in `pool_wait` — an `auto` run parked because its owner's Anthropic " +
			"token pool was empty when it claimed (PRD #754). It flips the hold straight to `queued` " +
			"instead of waiting up to a sweeper tick for the reactive pass to notice a token was pooled.\n\n" +
			"A run that is NOT held is a 409 (exit 5); a foreign or unknown run is a 404 (exit 4). No " +
			"token is spent and nothing is written to the forge — it only releases the hold.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.ResumeRunNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}

	mrRework := &cobra.Command{
		Use:   "mr-rework <run-id>",
		Short: "Set whether this run's MR review comments are auto-reworked (--enabled[=false], or --clear to inherit)",
		Long: "Set the per-run override for the MR review-rework watcher (PRD #841): whether new review " +
			"comments on this run's open MR are auto-reworked. Tri-state, editable on a COMPLETED run for as " +
			"long as its MR is still open (the watcher acts after the run finishes):\n\n" +
			"  --enabled            turn auto-rework ON for this run\n" +
			"  --enabled=false      turn it OFF (its MR is never auto-reworked)\n" +
			"  --clear              clear the override back to inherit (follow the account default)\n\n" +
			"A foreign or unknown run is a 404 (exit 4). The write is inert once the MR is merged or closed. " +
			"Prints the run's resulting MR_REWORK state (inherit/on/off).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Three-way: --clear sends null (inherit); otherwise --enabled's value (default
			// true for a bare `mr-rework <id>`) is sent explicitly. --clear and --enabled
			// together is a usage error — they express opposite intents.
			clear, _ := cmd.Flags().GetBool("clear")
			var enabled *bool
			if clear {
				if cmd.Flags().Changed("enabled") {
					return uzicli.Exitf(uzicli.ExitUsage, "--clear and --enabled are mutually exclusive")
				}
				enabled = nil
			} else {
				v, _ := cmd.Flags().GetBool("enabled")
				enabled = &v
			}
			run, err := c.SetRunMrRework(cmd.Context(), args[0], enabled)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	mrRework.Flags().Bool("enabled", true, "whether this run's MR review comments are auto-reworked; pass --enabled=false to turn it off")
	mrRework.Flags().Bool("clear", false, "clear the per-run override back to inherit (follow the account default)")

	cmd.AddCommand(newRunListCmd(env, gf), newRunGetCmd(env, gf), newRunLogsCmd(env, gf), wait, newRunReviewCmd(env, gf), create, approve, reject, revise, cancel, stop, scope, followUp, answer, inputs, expedite, resumeNow, mrRework)
	return cmd
}

// approveSelection maps the plan-gate flags to the wire AgentSelection
// {source, exclusions} (PRD #37 Decision 4: pick a source, exclude individual
// agents — either/or, no mixing, no allow-list). It NEVER fetches the run: the
// server validates the selection against the run's live roster, so the CLI just
// forwards the structured choice.
//
//   - neither flag → nil: send NO selection; the server applies the run's default
//     (repo-when-detected else own, no exclusions). This is the common case.
//   - --agent-source own|repo → that source, with any --exclude-agents.
//   - --exclude-agents without --agent-source → usage error: without a fetch the CLI
//     can't know the run's default source, and exclusions are validated against the
//     chosen source's roster server-side.
//
// `lead` is never excludable; if a user names it, the server rejects it — the CLI
// does not special-case it client-side.
func approveSelection(source string, exclude []string) (*apitypes.AgentSelection, error) {
	exclude = nonEmpty(exclude)
	if source == "" {
		if len(exclude) > 0 {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "specify --agent-source with --exclude-agents")
		}
		return nil, nil
	}
	if source != agentSourceOwn && source != agentSourceRepo {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "--agent-source must be 'own' or 'repo'")
	}
	return &apitypes.AgentSelection{Source: source, Exclusions: exclude}, nil
}

// plannedCommitRe mirrors the server's workersvc.plannedCommitRe (PRD #209 M4): hex, 7-64
// chars. The server is authoritative; this is a pre-flight so a malformed --planned-commit
// is a clean exit-2 usage error before any request rather than a round-trip 400.
var plannedCommitRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// seededPlanFlag assembles PRD #209's optional seeded plan from the `create` flags, and
// returns nil when no --plan-file was given (an ordinary issue-planned run, unchanged).
//
// It reuses approveSelection — so `--exclude-agents` without `--agent-source` is the
// SAME usage error the plan gate raises — and mirrors the server's coupling
// client-side: a roster flag with no --plan-file is a usage error here (the server
// would answer 400 "agent_selection requires plan_md"), so the user gets a clean
// message before any request is sent. The plan's size cap and empty-plan rejection stay
// the SERVER's (422): readPlanFile forwards the bytes.
func seededPlanFlag(env Env, cmd *cobra.Command) (*uzicli.CreateRunSeed, error) {
	source, _ := cmd.Flags().GetString("agent-source")
	exclude, _ := cmd.Flags().GetStringSlice("exclude-agents")
	sel, err := approveSelection(source, exclude)
	if err != nil {
		return nil, err
	}
	plannedCommit, _ := cmd.Flags().GetString("planned-commit")
	plannedCommit = strings.TrimSpace(plannedCommit)
	requireBase, _ := cmd.Flags().GetBool("require-base")
	// M4: --require-base needs a --planned-commit to compare against; without one the
	// flag can never fire (usage error before any request, mirroring the server's 400).
	if requireBase && plannedCommit == "" {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--require-base requires --planned-commit (there is no commit to compare the clone's base against)")
	}
	// A pre-flight mirror of the server's ErrInvalidPlannedCommit (the server is the
	// authority): reject a --planned-commit that is not a plausible git sha before any
	// request, so the user gets exit 2 rather than a round-trip 400. A too-short value is
	// the load-bearing case — it would silently disarm --require-base server-side.
	if plannedCommit != "" && !plannedCommitRe.MatchString(plannedCommit) {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--planned-commit must be a hex commit sha of 7-64 characters")
	}
	planProvided := cmd.Flags().Changed("plan-file")
	// A roster / a planned commit / a require-base is only meaningful for a seeded plan.
	// Reject any of them without --plan-file up front, mirroring the server's
	// "agent_selection requires plan_md" / "planned_commit and require_base require
	// plan_md" 400s with a clean message before any request is sent.
	if (sel != nil || plannedCommit != "" || requireBase) && !planProvided {
		return nil, uzicli.Exitf(uzicli.ExitUsage,
			"--agent-source/--exclude-agents/--planned-commit/--require-base require --plan-file "+
				"(they are only meaningful for a seeded plan)")
	}
	if !planProvided {
		return nil, nil
	}
	path, _ := cmd.Flags().GetString("plan-file")
	plan, err := readPlanFile(env, path)
	if err != nil {
		return nil, err
	}
	return &uzicli.CreateRunSeed{
		PlanMD:        plan,
		Selection:     sel,
		PlannedCommit: plannedCommit,
		RequireBase:   requireBase,
	}, nil
}

// submitInput sends one steering input and reports the outcome. server_side (a
// cancel/reject applied without a live worker) is surfaced so the caller knows the
// action took effect immediately rather than being queued.
func submitInput(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, runID, kind, body string, sel *apitypes.AgentSelection) error {
	res, err := c.SubmitRunInput(cmd.Context(), runID, kind, body, sel)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res)
	}
	if !gf.quiet {
		p.Printf("%s: %s\n", runID, inputOutcome(kind, res.ServerSide))
	}
	return nil
}

// answerBody is the wire shape of an `answer` input (PRD #88 M1). JSON rather than
// the bare prose every other kind carries, because an answer must name the question
// it answers — the server rejects one naming a question that is no longer open, which
// is what stops a reply written against an earlier question from resolving the
// current one.
type answerBody struct {
	QuestionID string   `json:"question_id"`
	Answers    []string `json:"answers"`
}

// questionPayload is the part of a `question` run-message the CLI reads.
type questionPayload struct {
	QuestionID string `json:"question_id"`
	Questions  []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// openQuestion reads the run's newest `question` message and returns its payload.
//
// Derived from the FEED rather than a run field (PRD #88 D-L), so the CLI, the web UI
// and Slack share one derivation rule instead of three. RunLogs returns messages in
// seq order, so the last `question` is the open one.
//
// KNOWN COST, stated rather than hidden: this pulls the run's ENTIRE message history
// to read one field. Nothing new is exposed — it is the same route and the same
// authorization as `uzi run logs` — but `logs` is expected to be heavy and `answer` is
// not, so a long run makes a one-line command download megabytes. Fixing it properly
// needs a server-side "latest message of kind K" read that does not exist yet; adding
// one for a single CLI verb was judged the wrong trade against D-L's one-derivation
// rule, which is what keeps the CLI, web and Slack from drifting.
func openQuestion(ctx context.Context, c uzicli.Client, runID string) (questionPayload, error) {
	msgs, err := c.RunLogs(ctx, runID, 0)
	if err != nil {
		return questionPayload{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind != "question" {
			continue
		}
		var q questionPayload
		if err := json.Unmarshal(msgs[i].Payload, &q); err != nil {
			return questionPayload{}, uzicli.Exitf(uzicli.ExitGeneric, "could not read the run's open question: %v", err)
		}
		if strings.TrimSpace(q.QuestionID) == "" {
			return questionPayload{}, uzicli.Exitf(uzicli.ExitGeneric, "the run's open question carries no id")
		}
		return q, nil
	}
	// No question in the feed at all. Distinct from "the run moved on", which the
	// server reports, because this one is almost always a mistyped run id.
	return questionPayload{}, uzicli.Exitf(uzicli.ExitConflict, "run %s is not waiting for an answer", runID)
}

// renderCreatedRun prints a newly created run and — Risk 4 — warns on stderr when it
// already carries a health reason (e.g. a locked vault parks it queued). The reliable
// surface remains `uzi run get`/`run list`, which poll the same reason; this warns at
// create time so an agent that queues then polls is not left blind on the first read.
func renderCreatedRun(env Env, gf *globalFlags, run apitypes.RunDTO) error {
	if run.HealthReason != nil && strings.TrimSpace(*run.HealthReason) != "" {
		_, _ = fmt.Fprintf(env.Stderr, "warning: %s\n", sanitizeTTY(strings.TrimSpace(*run.HealthReason)))
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"run": run})
	}
	if !gf.quiet {
		return renderRunDetail(p, run)
	}
	return nil
}

// nonEmpty drops blank entries from a StringSlice flag (e.g. --exclude-agents "" or
// a trailing comma), so a flag artifact never rides as an empty exclusion. It
// returns a non-nil slice so a source-only selection marshals as `exclusions: []`
// (matching the web AgentPicker), not `null`.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// printRunFields projects a run to its named top-level scalar field(s), printing each
// value raw and unquoted on its own line, in the order the caller named them (PRD #264
// D5). It is the cheap read a poller wants: no whole-object JSON parse, so the exact
// class of footgun that produced the zsh-`echo` blindness — piping `--json` through a
// shell that reinterprets the CLI's valid \uXXXX escapes — never arises for the common
// status/mr case.
//
// The field enum is DERIVED, not restated: marshaling the concrete RunDTO to
// map[string]json.RawMessage makes its key set the source of truth (self-maintaining as
// the DTO grows), gives `null → empty line` for free, and makes the non-scalar case a
// one-byte test — a RawMessage whose first non-space byte is `[` or `{` (the four array
// fields: milestones, milestones_candidate, milestones_completed, milestones_in_progress)
// has no meaningful one-line raw form and is a usage error. An unknown field is likewise
// a usage error (exit 2), never a silent blank.
//
// Written to Stdout, NOT through the Printer, because on a PIPE this is a machine
// channel whose contract is byte-fidelity — a poller reads `.status`/`.mr_web_url`
// verbatim, so a non-TTY destination gets the raw decoded bytes. On a TTY, though, a
// field can be forge-authored free text (`issue_title`, `title`, `issue_description`)
// carrying raw control/ANSI bytes, and unlike `--json` there is no encoder here to
// escape them — so the value is run through SanitizeTTY when stdout is a terminal.
// That closes the same terminal-injection vector the rest of the CLI's human output
// guards (Risk 13) while leaving the agent/pipe contract exactly raw.
func printRunFields(env Env, run apitypes.RunDTO, fields []string) error {
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	// Validate every field FIRST, so a bad name in a multi-field call fails cleanly
	// (exit 2) rather than printing some lines and then erroring mid-stream.
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		raw, ok := m[f]
		if !ok {
			return uzicli.Exitf(uzicli.ExitUsage, "unknown field %q", f)
		}
		v, err := scalarField(f, raw)
		if err != nil {
			return err
		}
		lines = append(lines, v)
	}
	for _, l := range lines {
		if env.StdoutTTY {
			l = uzicli.SanitizeTTY(l)
		}
		_, _ = fmt.Fprintln(env.Stdout, l)
	}
	return nil
}

// scalarField renders one RunDTO field's RawMessage as a raw one-line value, or a usage
// error if it is non-scalar (an array/object). `null` renders as the empty string (an
// empty line); a JSON string is unquoted to its raw content; a number or bool prints its
// literal bytes.
func scalarField(name string, raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return "", nil
	}
	if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
		return "", uzicli.Exitf(uzicli.ExitUsage, "field %q is not a scalar (it is an array or object); use --json to read it", name)
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	return trimmed, nil
}

// shortInstanceID renders an invocation id compactly: its LAST 6 runes.
//
// A tail, not a prefix, and deliberately NOT shortRecID's first-8 rule. That rule is
// right for a random UUID, but an SDK tool-use id carries a constant `toolu_01`
// prefix, so first-8 would return the same eight characters for every instance in a
// run — the one thing this column exists to tell apart. A tail needs no claim about
// the prefix's shape at all, which matters because the real id format is documented
// (~30 characters, PRD #99) but is not verified here against a live SDK run.
//
// Display only, and lossy: two ids CAN share a 6-rune tail. --json carries the full
// value, and that is the agent contract.
func shortInstanceID(id string) string {
	const n = 6
	r := []rune(id)
	if len(r) <= n {
		return id
	}
	return string(r[len(r)-n:])
}

// waitOnLimitFlag resolves `--wait-on-limit` into the TRI-STATE the create endpoint
// takes (PRD #35 Decision 7): nil omits the key so the server stamps the run from the
// owner's Settings default, &true opts this run in, &false opts it explicitly out.
//
// 🔴 THE DEFAULT VALUE IN THE FLAG DEFINITION IS NOT THE DEFAULT BEHAVIOUR, and that
// is the trap this function exists to remove. `Bool("wait-on-limit", false, …)` makes
// GetBool return false when the flag is absent — identical to `--wait-on-limit=false`.
// Passing that straight through would send `"wait_on_limit": false` on EVERY CLI-created
// run and silently override the user's own Settings default, which is precisely the
// regression the server's field comment cites for taking a *bool. Changed() is what
// separates "the user said false" from "the user said nothing"; pflag sets it for
// `--wait-on-limit` and for `--wait-on-limit=false` alike, and leaves it false when the
// flag is absent, which is exactly the three-way split needed.
//
// There is NO precedent for a tri-state flag in this CLI — `--force`, `--with-token`
// and `--follow` are all plain switches whose false is a real default rather than an
// absence — so this is the first, and the Changed() idiom is the standard pflag answer
// rather than an invention. A `--no-wait-on-limit` twin was the alternative and loses:
// two flags need a mutual-exclusion check, and they can be passed together.
//
// Note `--wait-on-limit false` (a SPACE, not `=`) does not set false — pflag reads a
// bare bool flag as true and leaves `false` as a positional argument. `create` is
// cobra.NoArgs, so that mistake is a loud usage error rather than a silent inversion,
// which is why it needs no guard of its own.
func waitOnLimitFlag(cmd *cobra.Command) *bool {
	if !cmd.Flags().Changed("wait-on-limit") {
		return nil
	}
	v, err := cmd.Flags().GetBool("wait-on-limit")
	if err != nil {
		return nil
	}
	return &v
}
