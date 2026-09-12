package main

// run_steer.go holds the run steering verbs — scope/follow-up/revise/answer/
// inputs (PRD #1009 M4).

import (
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

// newRunPauseCmd builds `uzi run pause` (PRD #1190 M4).
//
// It rides the SAME POST /inputs path the sibling steering verbs use (c.SubmitRunInput),
// but prints its own copy instead of submitInput's generic one-liner because a pause has
// three distinct outcomes to word. --now and --cancel are mutually exclusive (they express
// opposite intents); the default posts kind=pause with body "milestone", --now posts body
// "now", --cancel posts kind=pause_cancel.
//
// None of the three is synchronous: the park (default or --now) lands on the worker's next
// report, so even --now prints "Pause requested (now) …" and points at `uzi run get` rather
// than claiming the run is already parked. The kind-allowlist and the wrong-status 409s
// (chat runs already park, the run is at a gate, …) are the server's and flow through as
// exit-5 errors with the server's message.
func newRunPauseCmd(env Env, gf *globalFlags) *cobra.Command {
	pause := &cobra.Command{
		Use:   "pause <run-id>",
		Short: "Pause a running issue/task/prompt/self-improve run, parked on a pushed checkpoint (--now, or --cancel to withdraw)",
		Long: "Ask a running run to park on a pushed checkpoint and spend nothing until you resume it " +
			"(PRD #1190). Owner-only, and valid only on a running issue, non-interactive task, prompt or " +
			"self-improve run (a 409 with the reason otherwise — chat runs already park between turns, " +
			"interactive tasks park after each turn, and judge/mr-rework/ci-fix runs finish on their own).\n\n" +
			"The default finishes the milestone (or turn) in flight, pushes a checkpoint, then parks; the " +
			"run stays `running` until then. `--now` drops the turn in flight and parks on the last " +
			"checkpoint. `--cancel` withdraws a pending request. None is synchronous: the park lands on the " +
			"worker's next report, so watch `uzi run get <id>`.\n\n" +
			"Resume it later with `uzi run resume <id>`; the remaining budget is preserved (the clock stops " +
			"while paused).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			now, _ := cmd.Flags().GetBool("now")
			cancel, _ := cmd.Flags().GetBool("cancel")
			if now && cancel {
				return uzicli.Exitf(uzicli.ExitUsage, "--now and --cancel are mutually exclusive")
			}
			runID := args[0]
			kind, body := kindPause, "milestone"
			switch {
			case cancel:
				kind, body = kindPauseCancel, ""
			case now:
				body = "now"
			}
			res, err := c.SubmitRunInput(cmd.Context(), runID, kind, body, nil)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if gf.quiet {
				return nil
			}
			switch {
			case cancel:
				p.Printf("Pause withdrawn for %s.\n", runID)
			case now:
				p.Printf("Pause requested (now) for %s. The turn in flight is dropped and it parks on the last checkpoint; watch: uzi run get %s\n", runID, runID)
			default:
				p.Printf("Pause requested. The run finishes the current milestone, pushes a checkpoint, then parks. Status stays running until then.\n")
				p.Printf("Withdraw: uzi run pause %s --cancel · park at once: --now\n", runID)
			}
			return nil
		},
	}
	pause.Flags().Bool("now", false, "drop the turn in flight and park on the last checkpoint (default: finish the current milestone first)")
	pause.Flags().Bool("cancel", false, "withdraw a pending pause request (the run keeps running)")
	return pause
}

// newRunExtendCmd builds `uzi run extend` (PRD #1189 M3).
//
// It rides the SAME POST /inputs path the sibling steering verbs use (c.SubmitRunInput),
// posting kind=extend with a body of whole seconds parsed from --by, but prints its own
// data-carrying line (like newRunPauseCmd) because a successful extend comes back with the
// new deadline + running total on the response. The cap ("of 16h allowed") is NOT on the
// response, so it is read best-effort off a `run get` fetch; if that read fails the line
// stays truthful by dropping the "of <cap> allowed" tail rather than inventing a number.
//
// The kind allowlist and the extend guards are the server's: a disabled cap
// (ErrExtendDisabled), an over-cap request (ErrExtensionCapExceeded) and an untimed kind
// (ErrExtendNotTimed) all come back as 409 (exit 5), and a body under the minimum as 400
// (exit 2), each with the server's message printed verbatim — the same `return err` flow
// scope/pause use.
func newRunExtendCmd(env Env, gf *globalFlags) *cobra.Command {
	extend := &cobra.Command{
		Use:   "extend <run-id>",
		Short: "Extend a run's wall-clock budget by a duration (owner-only): --by 2h",
		Long: "Grant a non-terminal, time-limited run MORE wall-clock time so the sweep does not " +
			"kill it at its current deadline (PRD #1189). Owner-only; the added time stacks on the " +
			"frozen budget rather than replacing it, and the run's per-run extension allowance is an " +
			"admin setting.\n\n" +
			"--by takes a duration: Go's `2h`, `90m`, `1h30m`, plus a `d` (day = 24h) unit — `1d` is " +
			"24h. It must be at least 1 second; `0`, a sub-second value, a negative value or an " +
			"unparseable one is a usage error (exit 2).\n\n" +
			"Valid on any non-terminal run of a kind the sweep can time out (an issue, task, prompt, " +
			"self-improve, mr-rework or ci-fix run), including a queued or parked one. A chat, judge " +
			"or interactive-task run never times out, so extending one is a 409 (exit 5); an over-cap " +
			"request, or extending while the admin has turned extensions off, is also a 409, with the " +
			"server's message printed verbatim.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("by") {
				// One line only, like the sibling verbs (run scope/pause): Main already
				// prints this Exitf message with the "uzi: " prefix, so a separate Fprintln
				// guidance line would double-print near-identical text on stderr.
				return uzicli.Exitf(uzicli.ExitUsage, "run extend needs --by <duration> (e.g. --by 2h, --by 90m, --by 1h30m, --by 1d)")
			}
			by, _ := cmd.Flags().GetString("by")
			seconds, err := parseExtendDuration(by)
			if err != nil {
				return err
			}
			runID := args[0]
			res, err := c.SubmitRunInput(cmd.Context(), runID, kindExtend, strconv.Itoa(seconds), nil)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if gf.quiet {
				return nil
			}
			// The response carries the new total + deadline; the per-run cap is a stable admin
			// setting the response does not repeat, so read it best-effort off the run. A failed
			// read leaves capSeconds 0 and extendSuccessLine drops the "of <cap> allowed" tail.
			capSeconds := 0
			if run, gerr := c.GetRun(cmd.Context(), runID); gerr == nil {
				capSeconds = run.BudgetExtensionCapSeconds
			}
			p.Printf("%s\n", extendSuccessLine(runID, seconds, res, capSeconds, time.Now()))
			return nil
		},
	}
	extend.Flags().String("by", "", "how much wall-clock time to add: 2h, 90m, 1h30m, 1d (day = 24h); required, at least 1 second")
	return extend
}

// extendDaysRe matches a LEADING days token ("1d", "2d", "1d12h", "-1d") so parseExtendDuration
// can rewrite it into hours — time.ParseDuration knows h/m/s but not `d`.
var extendDaysRe = regexp.MustCompile(`^(-?)(\d+(?:\.\d+)?)d(.*)$`)

// parseExtendDuration parses the --by value into whole seconds for the `extend` body. It
// accepts Go's time.ParseDuration syntax (2h, 90m, 1h30m) PLUS a `d` (day = 24h) unit that
// time.ParseDuration does not know (1d, 1d12h). A value that resolves to zero whole seconds
// — zero, a negative value, or a sub-second value like 500ms that floors to 0 — or an
// unparseable value is a usage error (ExitUsage) so a typo (or a too-small unit) fails fast
// locally rather than posting a no-op "0" extension.
func parseExtendDuration(s string) (int, error) {
	raw := strings.TrimSpace(s)
	d, err := parseDurationWithDays(raw)
	if err != nil {
		return 0, uzicli.Exitf(uzicli.ExitUsage, "--by %q is not a valid duration (try 2h, 90m, 1h30m, 1d)", s)
	}
	// Reject on the WHOLE-SECONDS value, not the raw nanosecond duration: a sub-second
	// input (500ms, 0.5s) is > 0 as a time.Duration but floors to 0 seconds, so guarding
	// `d <= 0` would let it post a no-op "0". Rejecting seconds <= 0 (equivalently
	// d < time.Second) fails it fast locally, like 0/negative/unparseable.
	seconds := int(d / time.Second)
	if seconds <= 0 {
		return 0, uzicli.Exitf(uzicli.ExitUsage, "--by must be at least 1 second (got %q)", s)
	}
	return seconds, nil
}

// parseDurationWithDays is time.ParseDuration extended with a leading `d` (day) unit. A
// leading days token is rewritten into hours (1 day = 24h) and the remainder handed to
// time.ParseDuration, whose repeated units sum — so "1d12h" becomes "24h12h" = 36h. A
// leading sign applies to the whole duration. Everything without a `d` token goes straight
// to time.ParseDuration unchanged.
func parseDurationWithDays(s string) (time.Duration, error) {
	if m := extendDaysRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return 0, err
		}
		rewritten := m[1] + strconv.FormatFloat(days*24, 'f', -1, 64) + "h" + m[3]
		return time.ParseDuration(rewritten)
	}
	return time.ParseDuration(s)
}

// extendSuccessLine is the human confirmation for a successful `run extend` (PRD #1189 M3):
//
//	Extended <id> by 2h. 3h05m left, times out 17:20. Extensions on this run: 2h of 16h allowed.
//
// "by" is the REQUESTED duration; the "left"/"times out" clause is derived from the response's
// deadline_at (local zone, via the CLI's existing formatters); and the allowance is the new
// running total (response) against the per-run cap (fetched run DTO). Clauses the response does
// not carry are dropped rather than fabricated: a run extended while queued/parked carries no
// deadline_at yet (the sweep only computes one for a running run), so the deadline clause is
// omitted; and a failed cap read drops just the "of <cap> allowed" tail.
func extendSuccessLine(runID string, requestedSeconds int, res apitypes.RunInputResponse, capSeconds int, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Extended %s by %s.", runID, shortDuration(time.Duration(requestedSeconds)*time.Second))
	if res.DeadlineAt != nil {
		left := res.DeadlineAt.Sub(now)
		if left < 0 {
			left = 0
		}
		fmt.Fprintf(&b, " %s left, times out %s.", fmtUntil(left), res.DeadlineAt.Local().Format("15:04"))
	}
	if res.ExtensionSeconds != nil {
		total := shortDuration(time.Duration(*res.ExtensionSeconds) * time.Second)
		if capSeconds > 0 {
			fmt.Fprintf(&b, " Extensions on this run: %s of %s allowed.", total, shortDuration(time.Duration(capSeconds)*time.Second))
		} else {
			fmt.Fprintf(&b, " Extensions on this run: %s.", total)
		}
	}
	return b.String()
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
