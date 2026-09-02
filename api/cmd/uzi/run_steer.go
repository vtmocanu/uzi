package main

// run_steer.go holds the run steering verbs — scope/follow-up/revise/answer/
// inputs (PRD #1009 M4).

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunScopeCmd builds `uzi run scope`.
func newRunScopeCmd(env Env, gf *globalFlags) *cobra.Command {
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
	return scope
}

// newRunFollowUpCmd builds `uzi run follow-up`.
func newRunFollowUpCmd(env Env, gf *globalFlags) *cobra.Command {
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
	return followUp
}

// newRunReviseCmd builds `uzi run revise`.
func newRunReviseCmd(env Env, gf *globalFlags) *cobra.Command {
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
	return revise
}

// newRunAnswerCmd builds `uzi run answer`.
func newRunAnswerCmd(env Env, gf *globalFlags) *cobra.Command {
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
	return answer
}

// newRunInputsCmd builds `uzi run inputs`.
func newRunInputsCmd(env Env, gf *globalFlags) *cobra.Command {
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
	return inputs
}
