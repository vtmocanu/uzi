package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// Run-input kinds, the exact wire strings the inputs endpoint accepts
// (handler/workers.go runInputKinds). Named so a typo is a compile error, not a
// silent 400.
const (
	kindApprovePlan = "approve_plan"
	kindRejectPlan  = "reject_plan"
	kindCancel      = "cancel"
	kindFollowUp    = "follow_up"
	kindAnswer      = "answer"
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

// newRunCmd — `uzi run` and its verbs. list/get/logs/review are wired to the
// Client; create/approve/reject/cancel/follow-up are stubs (M8).
func newRunCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "List, inspect, and drive runs",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(runs)
			}
			rows := make([][]string, 0, len(runs))
			for _, r := range runs {
				rows = append(rows, []string{r.ID, r.Kind, r.Status, runTitle(r.RunDTO)})
			}
			return p.Table([]string{"ID", "KIND", "STATUS", "TITLE"}, rows)
		},
	}

	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's status and details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.GetRun(cmd.Context(), args[0])
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

	logs := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Print a run's message history (REST polling)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			follow, _ := cmd.Flags().GetBool("follow")
			after, _ := cmd.Flags().GetInt32("after")
			p := env.printer(gf)
			seq := after
			drain := func() error {
				msgs, err := c.RunLogs(cmd.Context(), args[0], seq)
				if err != nil {
					return err
				}
				for _, m := range msgs {
					if err := renderMessage(p, m); err != nil {
						return err
					}
					if m.Seq > seq {
						seq = m.Seq
					}
				}
				return nil
			}
			// parked tracks whether the LAST poll saw the run parked on a usage limit,
			// so the notice below fires on the EDGE into the park rather than every
			// 2 seconds for the hours one lasts.
			parked := false
			for {
				if err := drain(); err != nil {
					return err
				}
				if !follow {
					return nil
				}
				// Stop once the run reaches a terminal state: no further messages can
				// arrive, so an agent running `--follow` on a finished run must exit
				// (exit 0), not poll forever. Check AFTER draining this round; on a
				// terminal run drain once more (messages are persisted before the run
				// flips terminal — a gapless-seq guarantee) so nothing is dropped.
				run, err := c.GetRun(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if terminalRunStatuses[run.Status] {
					return drain()
				}
				// A park is the only status this loop rides out that produces NOTHING
				// for hours, and it is indistinguishable from a hang or a wedged agent
				// from the outside — the failure mode the milestone exists to fix.
				//
				// STDERR, not the Printer: the Printer is stdout, and `--json` streams
				// NDJSON there for an agent to parse line by line (renderMessage). A
				// human-readable notice on that stream would corrupt the contract. This
				// is the same split cobra's deprecation notice already uses here.
				if run.Status == statusLimitWait {
					if !parked {
						parked = true
						_, _ = fmt.Fprintf(env.Stderr, "run %s %s — still following; it resumes on its own\n",
							args[0], limitWaitLine(run, time.Now()))
					}
				} else if parked {
					parked = false
					// cellText, NOT sanitizeTTY, and the difference is the whole point:
					// sanitizeTTY spares "\n", so a status carrying one would inject a
					// line onto stderr. Unreachable today because runs_status_check
					// constrains status to eight values — which is precisely the argument
					// limitWaitLine's own comment REJECTS for rate_limit_type ("server-
					// controlled today" is exactly the assumption that rots). Holding one
					// line of this file to a weaker standard than the line beside it, on a
					// premise that line disowns, is not a defensible split.
					_, _ = fmt.Fprintf(env.Stderr, "run %s resumed (%s)\n", args[0], cellText(run.Status))
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(logsPollInterval):
				}
			}
		},
	}
	logs.Flags().Bool("follow", false, "keep polling for new messages")
	logs.Flags().Int32("after", 0, "only show messages after this sequence number")

	// review is now a HIDDEN, DEPRECATED alias of `uzi review show` (PRD #94
	// Decision 10). It shares runReviewShow so the two stay byte-identical.
	// SetOut(env.Stderr) forces cobra's deprecation notice — printed via
	// OutOrStderr, which the root's SetOut(env.Stdout) would otherwise route to
	// STDOUT — onto stderr, keeping --json output pure (TestRunReviewJSON*).
	review := &cobra.Command{
		Use:        "review <run-id>",
		Short:      "Show the judge's review for a run (read-only)",
		Args:       cobra.ExactArgs(1),
		Hidden:     true,
		Deprecated: "use \"uzi review show\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runReviewShow(env, gf, c, cmd, args[0])
		},
	}
	review.SetOut(env.Stderr)

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
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.CreateRun(cmd.Context(), repoID, issue, waitOnLimitFlag(cmd))
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
			return submitInput(env, gf, c, cmd, args[0], kindRejectPlan, msg, nil)
		},
	}
	reject.Flags().StringP("message", "m", "", "reason to send back to the agent (optional)")

	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return submitInput(env, gf, c, cmd, args[0], kindCancel, "", nil)
		},
	}

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
		Long: "List the follow-ups sent to a run (the steer queue, PRD #95), newest first, " +
			"each with its delivery state — queued (the worker has not drained it yet) or " +
			"delivered (handed to the worker for its next turn) — plus a relative age. " +
			"Owner-only: a read-only admin token gets 404 on another user's run.\n\n" +
			"Only kind='follow_up' rows are shown (approve/reject/cancel are omitted). " +
			"A chat run seeds every chat turn as a follow_up, so `uzi run inputs` on a " +
			"chat run lists the seeded prompt and all chat turns; an issue run's queue " +
			"starts empty (its prompt rides the claim payload, not an input row).",
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
					if run.Kind == "chat" {
						// N3: a chat run's queue is every chat turn. Note it only when it
						// actually applies, so an issue run's output stays clean.
						_, _ = fmt.Fprintln(env.Stderr, "note: chat run — this queue lists every chat turn as a follow-up (chat has its own web composer, unaffected)")
					}
				}
			}
			return renderRunInputs(p, list, status)
		},
	}

	cmd.AddCommand(list, get, logs, review, create, approve, reject, cancel, followUp, answer, inputs)
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

// inputOutcome is the human confirmation line for a submitted input.
func inputOutcome(kind string, serverSide bool) string {
	switch kind {
	case kindApprovePlan:
		return "plan approved"
	case kindRejectPlan:
		if serverSide {
			return "plan rejected (run stopped)"
		}
		return "plan rejection sent"
	case kindCancel:
		if serverSide {
			return "run cancelled"
		}
		return "cancellation sent"
	case kindFollowUp:
		return "follow-up sent"
	case kindAnswer:
		return "answer sent"
	default:
		return "input submitted"
	}
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

// resolveMessage returns the -m flag value when set, else a message piped on stdin
// (non-TTY only, mirroring `uzi auth token`), capped so a hostile pipe cannot make
// the CLI allocate without bound.
func resolveMessage(env Env, flagVal string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal
	}
	if env.Stdin != nil && !env.StdinTTY {
		b, _ := io.ReadAll(io.LimitReader(env.Stdin, 1<<20))
		return strings.TrimSpace(string(b))
	}
	return ""
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

// renderRunDetail prints a run as an aligned key/value block. Health + the health
// reason are surfaced here (Risk 4): a run parked behind a locked vault carries
// its reason on the DTO, so `uzi run get` shows it without a webui round-trip.
//
// STOP_KIND is here for PRD #108 M9b, and it is the one row this block genuinely
// owed. Without it an auto-stopped run reads
//
//	STATUS           failed
//	FAILURE_REASON   run cancelled
//
// — byte-identical to a user cancel, because on the live-poller half the worker's
// own SetRunFailed overwrites failure_reason with REASON_CANCELLED. stop_kind is
// the ONLY field that survives both halves of that stop, so without this row the
// CLI cannot distinguish "you stopped this" from "uzi stopped this because your
// updates could not be saved". The reasons need no change here: this block already
// prints HEALTH_REASON and FAILURE_REASON generically, switching on no vocabulary,
// so PRD #108's new reason strings flow through untouched.
func renderRunDetail(p *uzicli.Printer, r apitypes.RunDTO) error {
	rows := [][]string{
		{"ID", r.ID},
		{"KIND", r.Kind},
		{"STATUS", r.Status},
		{"TITLE", runTitle(r)},
		{"BRANCH", strOr(r.Branch, "-")},
		{mrAbbrev(r.ForgeType), int64Or(r.MrIID, "-")},
		{"HEALTH", r.Health},
	}
	if r.HealthReason != nil && *r.HealthReason != "" {
		rows = append(rows, []string{"HEALTH_REASON", sanitizeTTY(*r.HealthReason)})
	}
	if r.FailureReason != nil && *r.FailureReason != "" {
		rows = append(rows, []string{"FAILURE_REASON", sanitizeTTY(*r.FailureReason)})
	}
	// Emitted only when set, like its two neighbours — every non-stopped run would
	// otherwise carry a blank row. It is a server-controlled enum, so sanitizeTTY is
	// not strictly required; applied anyway for uniformity with the free text above,
	// and because "server-controlled today" is exactly the assumption that rots.
	if r.StopKind != nil && *r.StopKind != "" {
		rows = append(rows, []string{"STOP_KIND", sanitizeTTY(*r.StopKind)})
	}
	// Which Anthropic credential this run spent (PRD #111 M1). Emitted only when the
	// server recorded one — a pre-feature or still-queued run has nothing to say and
	// must not print a blank row.
	//
	// Through cellText, NOT sanitizeTTY, and the difference is load-bearing here in a
	// way it is not for the rows above. This is the first genuinely USER-AUTHORED
	// string in this block, and uzicli.Printer.Table does not sanitize what it is
	// handed. cellText is the table-cell wrapper: sanitizeTTY plus newline folding,
	// tab folding and a length cap.
	//
	// 🔴 THE PREMISE THIS USED TO GIVE IS NOW FALSE, AND THE CONCLUSION STILL HOLDS —
	// which is exactly why the premise is being corrected rather than deleted. It read
	// "validateSecretLabel rejects control characters and U+FFFD but NOT unicode.Cf, so
	// a bidi-override label is storable". Since PRD #111 M2 the validator DOES reject
	// Cf, so a reader checking that premise would find it false and reasonably conclude
	// that sanitizeTTY now suffices here.
	//
	// It does not, for two reasons that survive the validator:
	//   - NEWLINE FOLDING. sanitizeTTY deliberately spares "\n"; a newline in a table
	//     cell breaks the rail. Only cellText folds it. That is the difference the
	//     mutation control in render_test.go pins, and the one an earlier version of
	//     that control failed to test.
	//   - The validator governs what the SERVER ACCEPTS ON WRITE. Rows stored before
	//     it landed were never re-validated and nothing re-validates on read, so a
	//     hostile label still reaches this line through history, through a future
	//     write path that skips validation, and through a direct database write.
	if r.AnthropicSecretLabel != nil && *r.AnthropicSecretLabel != "" {
		rows = append(rows, []string{"ANTHROPIC_TOKEN", credentialCell(r)})
	}
	rows = append(rows, limitWaitRows(r, time.Now())...)
	return p.Table(nil, rows)
}

// limitWaitRows is the usage-limit park block of `uzi run get` (PRD #35), split out
// because it is the one part of this render with a live clock in it and therefore the
// one part worth testing without a Printer.
//
// It answers TWO different questions off the SAME columns, and conflating them would
// be the bug. The server leaves limit_resets_at / retry_not_before / rate_limit_type in
// place across a promotion, deliberately, as history — so on a parked run they say
// "here is when this resumes", and on a run that has already resumed the identical
// columns say "this run spent part of its wall clock waiting on a usage limit". A
// completed run rendering "resumes in 4h12m" would be the natural result of ignoring
// the distinction, and it would be nonsense.
func limitWaitRows(r apitypes.RunDTO, now time.Time) [][]string {
	// WAIT_ON_LIMIT rides every run, unlike its neighbours above, because it is
	// meaningful BEFORE any park has happened — it is the answer to "will this run
	// survive a usage limit, or just die on one?", which is a question about a queued
	// run as much as a parked one. A conditional row would make "off" indistinguishable
	// from an old CLI that does not know the field.
	rows := [][]string{{"WAIT_ON_LIMIT", boolStr(r.WaitOnLimit)}}
	if r.Status == statusLimitWait {
		rows = append(rows, []string{"LIMIT_WAIT", limitWaitLine(r, now)})
		// The reported window reset, as CONTEXT for the countdown above and never as a
		// substitute for it: a pool-aware promotion can land hours earlier than this.
		if r.LimitResetsAt != nil {
			rows = append(rows, []string{"LIMIT_RESETS_AT", r.LimitResetsAt.UTC().Format(time.RFC3339)})
		}
		return rows
	}
	if r.LimitWaitCount > 0 {
		rows = append(rows, []string{"LIMIT_WAITS", itoa(int(r.LimitWaitCount)) + " (resumed)"})
	}
	return rows
}

// credentialCell renders WHICH credential a run spent and WHY (PRD #111 M5, D20).
//
// The label alone was never enough, and this is the sentence that says why: an auto
// pick and a default fallback can name the SAME token, so "console-key" answers
// "which account paid" and leaves "why that account" unanswered. PRD #104's
// compatibility path also creates a row labelled literally `default`, so the label is
// not even a reliable hint at the mode.
//
// The three FALLBACK reasons are rendered as `default (auto: …)` rather than as their
// own thing, because that is what actually happened: the worker is configured for
// auto, the selector declined to pick, and the owner's default paid. A user reading
// `default` alone on an auto worker would reasonably think their configuration had
// been lost.
//
// An UNRECOGNISED reason prints as itself. The CLI is versioned separately from the
// API, so a newer server can ship a ninth reason this binary has never heard of, and
// inventing a rendering for it — or dropping it — would be worse than passing it
// through. Same stance as the web's autoStatusChip.
//
// A nil reason prints the bare label: a run claimed before M1 recorded no mode, and
// guessing one is exactly the wrong answer.
func credentialCell(r apitypes.RunDTO) string {
	label := cellText(*r.AnthropicSecretLabel)
	// F8's CLI half. The id goes null when the token is deleted while the snapshotted
	// label survives (00086's SET NULL), so this is a normal historical run — and the
	// difference between "go look at this token" and "this token is gone" is worth one
	// word. The web chip says the same thing; CLI parity is a rule here, not a nicety.
	if r.AnthropicSecretID == nil {
		label += " (deleted)"
	}
	if r.AnthropicSelectReason == nil || *r.AnthropicSelectReason == "" {
		return label
	}
	return label + " — " + selectReasonText(autoselect.Reason(*r.AnthropicSelectReason), r.AnthropicHeadroomPct)
}

// selectReasonText is the mode half, EXHAUSTIVE over autoselect.AllReasons() and
// pinned as such by TestCredentialCellCoversEveryReason — asserted over the
// vocabulary rather than by sampling, because a reason with no rendering is invisible
// until the one user it happens to reaches support.
//
// headroom is rendered only where it exists. It is present on an auto pick and nil
// everywhere else, including on D14's retry, where the measurement described the
// credential that would NOT open.
//
// 🔴 A LATENT COUPLING WITH THE WEB, recorded because no test can currently see it.
// This appends the suffix only under `auto` and `best_of_pool`; web's
// lib/runCredential.ts appends it for ANY reason with a non-null headroom. The two
// agree today by accident of the server rather than by construction — headroom is
// written only on an auto pick, and D14 explicitly nulls it on the retry, so no
// reachable state distinguishes them. The day something records a headroom on a
// fallback, the CLI silently drops it and the web silently shows it. Whichever
// surface changes first should make the other match.
func selectReasonText(reason autoselect.Reason, headroom *int) string {
	pct := ""
	if headroom != nil {
		pct = ", " + strconv.Itoa(*headroom) + "% headroom"
	}
	switch reason {
	case autoselect.ReasonDefault:
		return "default"
	case autoselect.ReasonPinned:
		return "pinned"
	case autoselect.ReasonJudge:
		return "judge binding"
	case autoselect.ReasonAuto:
		return "auto" + pct
	case autoselect.ReasonBestOfPool:
		// Named rather than folded into `auto`: every pooled token was under the
		// headroom floor and the emptiest was picked anyway (D10). The run worked, and
		// the user's pool is nearly exhausted — a thing to know, not an error.
		return "auto (best of pool)" + pct
	case autoselect.ReasonPoolEmpty:
		return "default (auto: no tokens in the pool)"
	case autoselect.ReasonPoolStale:
		return "default (auto: no fresh usage readings)"
	case autoselect.ReasonOpenFailed:
		return "default (auto: the chosen token would not open)"
	}
	return string(reason)
}

// sanitizeTTY strips terminal control characters from UNTRUSTED free text before
// it is written to a human TTY (Risk 13). Judge/run content can carry attacker-
// shaped bytes that repo/issue/CI text fed the LLM; printed verbatim, an embedded
// ANSI escape/CSI sequence could clear the screen, recolour, hide, or spoof
// output. It removes every control character (C0, C1 and DEL) except tab and
// newline, plus every Unicode format character (category Cf); all printable UTF-8
// passes through unchanged. It iterates runes (not bytes) so a multibyte codepoint
// whose bytes fall in 0x80–0x9F is never corrupted. Human render path ONLY —
// --json output stays byte-exact, and sanitizing there would corrupt the payload an
// agent decodes.
//
// 🔴 THE REASON --json IS SAFE IS THE DESTINATION, NOT THE ENCODER. This sentence
// used to read "structural JSON encoding already escapes these", where "these" is
// the C0/C1/DEL/Cf set enumerated above — false for three of those four families.
// Measured on encoding/json (issue #144, re-derived independently by two people):
// C0 and U+2028/U+2029 are escaped; DEL (0x7f), the C1 range including U+009B, and
// the Cf family including U+202E and the zero-widths all pass through UNESCAPED.
// What makes --json safe is that its bytes go to a PARSER rather than to a terminal.
// A caller who pipes --json straight to a TTY is outside that guarantee, and no
// encoder property will save them.
//
// (Stated as an escaping fact on purpose. Whether a terminal HONOURS a UTF-8-encoded
// U+009B as a CSI introducer depends on its 8-bit control handling and has not been
// tested here — do not upgrade this into a claim about exploitability.)
//
// The Cf half and DEL arrived with PRD #112 M3. This comment used to say it removed
// "C0 controls (0x00–0x1F) except tab and newline, and C1 controls (0x80–0x9F)",
// which was an accurate description of a predicate with two holes: `r < 0x20` let
// DEL (0x7f) through, and no range test can catch Cf at all.
// It uses CATEGORY PREDICATES, not codepoint ranges, and deliberately the SAME pair
// workersvc.hasUnsafeChar already settled on (agent_selection.go:236-240):
// unicode.IsControl covers C0 and C1 (so the old hand-rolled 0x00-0x1F / 0x80-0x9F
// ranges are subsumed) and it covers DEL 0x7f, which the old `r < 0x20` test let
// through; unicode.In(r, unicode.Cf) covers the format characters a range test can
// never enumerate — the bidi overrides U+202A-202E, the isolates U+2066-2069, U+200F,
// the zero-widths, the BOM, and SHY. A bidi override is the one that matters most: it
// visually reorders text, so an agent label or judge target can be made to READ as
// something it is not, which is precisely the spoof a TUI's fixed-width rails invite.
//
// TWO THINGS THIS DOES NOT COVER, so nobody reads it as more than it is:
//
//   - Combining marks are Mn, not Cf. "Zalgo" text stays a grapheme-WIDTH problem and
//     is not addressed here; a width-aware layout is the fix, not a stripper.
//   - Cf codepoints are ZERO-WIDTH while capCell pads by RUNES, so before this change
//     a label full of them consumed column budget while drawing nothing, silently
//     misaligning the rail. That is the same root cause as the tab bug whose comment
//     notes the rune offset stayed pinned "which is why the existing alignment test
//     could not see it". Stripping Cf fixes the spoof and that drift together.
func sanitizeTTY(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}

// renderMessage prints one run message. In --json mode it emits one compact JSON
// object per line (NDJSON), so `--follow` streams cleanly and an agent parses it
// line by line — and since it marshals the DTO whole, `agent_instance` and
// `agent_label` have been in the JSON output since they were added to MessageDTO
// (PRD #99 M1), with no work here. In human mode it prints a single
// #seq/kind/actor/payload line, where the actor cell is what M4 widens.
func renderMessage(p *uzicli.Printer, m apitypes.MessageDTO) error {
	if p.Format == uzicli.FormatJSON {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.Println(string(b))
		return nil
	}
	// %-*s pads to a fixed RUNE width (Go's fmt measures width in runes, not bytes),
	// and actorCell has already capped its result to that width, so the payload column
	// starts at the same rune offset on every line.
	p.Printf("#%-4d %-16s %-*s %s\n", m.Seq, m.Kind, actorCellWidth, actorCell(m), compactPayload(m.Payload))
	return nil
}

// actorCellWidth bounds the human table's actor column. The payload column beside
// it already runs to 200 characters, so terminal width is not the scarce resource
// this trades against — the cap exists to keep the columns ALIGNED down the stream,
// which is what `uzi run logs --follow` is read through. (Rune alignment, like the
// rest of this CLI's tables: a CJK label still occupies two terminal columns per
// rune, which no part of this tool accounts for — out of scope, not a regression.)
const actorCellWidth = 34

// actorCell renders WHO produced a message: the role, the short id of the
// invocation that produced it when the frame carries one, and that invocation's
// task label. Two parallel `coder` subagents therefore read distinctly (PRD #99
// Decision 10) instead of as two identical `coder` rows:
//
//	#12   tool_use   coder/3v6ptu · API wi…  {"name":"Edit"}
//	#13   tool_use   coder/2k9xqf · web ga…  {"name":"Edit"}
//
// The short id alone already separates them, so a truncated label costs clarity,
// never correctness. A frame with no instance — the orchestrator's own turns, infra
// frames, and every pre-migration message — renders as the bare role exactly as it
// did before, and a missing role still renders "-".
//
// Both new fields are worker-supplied text bound for a TTY, and `agent_label` is
// free model-authored prose rather than a role drawn from a fixed roster, so each
// goes through cellText's sanitize-and-fold (Risk 13: a raw CSI sequence in this
// cell could clear the screen or spoof output). That also closes the same hole for
// `agent`, which this function inherited printing verbatim.
func actorCell(m apitypes.MessageDTO) string {
	cell := "-"
	if m.Agent != nil {
		if role := cellText(*m.Agent); role != "" {
			cell = role
		}
	}
	if m.AgentInstance != nil {
		if id := cellText(shortInstanceID(*m.AgentInstance)); id != "" {
			cell += "/" + id
		}
	}
	if m.AgentLabel != nil {
		if label := cellText(*m.AgentLabel); label != "" {
			cell += " · " + label
		}
	}
	return capCell(cell, actorCellWidth)
}

// cellText is compactText plus the two folds a FIXED-WIDTH column needs and the
// shared compactText must not do — it also backs the payload and steer columns,
// which are free text where a tab is ordinary content and where nothing is
// promised about width.
//
// Tab. sanitizeTTY spares `\t` deliberately, and compactText folds only `\n`, so a
// tab in `agent_label` reaches the cell. `%-*s` then pads to actorCellWidth in
// RUNES and a tab is one rune, so every invariant the code checks still holds while
// the terminal expands it to the next 8-column stop and the payload column walks
// right. MEASURED before the fix: a benign label put the payload at rendered column
// 58; `a\tb\tc\td\te` put it at 76; eight interior tabs at 107 — with the rune
// offset pinned at 58 throughout, which is why the existing alignment test could
// not see it. Folded to a space, which preserves the word break the tab was doing.
//
// DEL used to need handling here too, and NO LONGER DOES. This paragraph read "0x7f
// is outside sanitizeTTY's C0 (<0x20) and C1 (0x80–0x9f) ranges, so it survives too",
// which described the predicate PRD #112 M3 replaced: sanitizeTTY now tests
// unicode.IsControl, which is true for 0x7f, so compactText strips DEL before this
// Map ever sees it. The `case 0x7f: return -1` arm was dead — deleting it reddened
// nothing — and is gone. The tab arm stays live, because sanitizeTTY spares tab
// deliberately.
//
// The tab fold is cosmetic — no cursor motion, no erase, no OSC, and `\n` cannot survive
// compactText — but the actor column's whole purpose is to stay aligned down a
// `uzi run logs --follow` stream, and model-authored prose is exactly where a stray
// tab comes from.
func cellText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		return r
	}, compactText(s))
	// Folding can leave an edge space behind (compactText trimmed before the fold).
	return strings.TrimSpace(s)
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

// capCell truncates a table cell to max RUNES, appending an ellipsis. Rune-based
// per the house idiom (workersvc.deriveChatTitle): byte-slicing splits a multibyte
// codepoint into invalid UTF-8. Note the neighbouring compactText still byte-slices
// at its 200-char cap — milder there, since a mangled rune reaches a terminal rather
// than an INSERT, but the two are inconsistent and compactText is the one that is
// wrong. Left alone here: it is shared with the payload and steer-body columns, so
// changing it changes output this milestone does not own.
func capCell(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}

// compactPayload renders a message payload as a single truncated line for the
// human table. The payload is server-forwarded run content; it is DATA, printed
// verbatim (never interpreted).
func compactPayload(raw json.RawMessage) string {
	return compactText(string(raw))
}

// compactText sanitizes untrusted free text and folds it to a single truncated
// line for a human table cell (Risk 13): C0/C1 controls stripped, newlines
// collapsed to spaces, capped at 200 chars. Shared by compactPayload (message
// payloads) and the steer-queue body column.
func compactText(s string) string {
	s = sanitizeTTY(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// renderRunInputs prints the steer queue (follow-ups, newest-first) as a
// body/state/age table (PRD #95 M4). The body is the user's own follow-up text,
// sanitized like any free text bound for a TTY. State is derived from
// (consumed_at, runStatus); age is relative to created_at.
func renderRunInputs(p *uzicli.Printer, inputs []apitypes.SteerInputDTO, runStatus string) error {
	rows := make([][]string, 0, len(inputs))
	for _, in := range inputs {
		body := "-"
		if in.Body != nil {
			body = compactText(*in.Body)
		}
		rows = append(rows, []string{body, steerState(in.ConsumedAt, runStatus), relAge(in.CreatedAt)})
	}
	return p.Table([]string{"BODY", "STATE", "AGE"}, rows)
}

// steerState derives a follow-up's delivery label from its consumed_at and the
// run's live status, mirroring PRD #95 Decision 7 as closely as the CLI can:
//   - not consumed, run terminal  → "not delivered (run finished)"
//   - not consumed, run parked    → "queued (run paused on a usage limit)"
//   - not consumed, otherwise     → "queued"
//   - consumed, run at plan gate  → "delivered (applies after approval)"
//   - consumed, run parked        → "delivered (run paused on a usage limit)"
//   - consumed, otherwise         → "delivered"
//
// runStatus may be "" when the run's status could not be fetched (Decision 10
// floor): the gate/terminal nuance is then dropped and only queued/delivered
// show — the acceptable CLI minimum.
//
// The two statusLimitWait arms are a SUFFIX on the existing answer, never a
// replacement for it (PRD #35). A park changes nothing about whether a follow-up was
// handed to the worker; it changes only whether anything is currently acting on it.
// Rewriting "queued" to something else on a parked run would be the same lie as
// dropping the park entirely, one level down: the queue state is still queued.
func steerState(consumedAt *time.Time, runStatus string) string {
	// A parked run is deliberately NOT terminal here, matching terminalRunStatuses:
	// its queue survives the park and drains when the run resumes, so "not delivered
	// (run finished)" would be false.
	const parkedSuffix = " (run paused on a usage limit)"
	if consumedAt == nil {
		if terminalRunStatuses[runStatus] {
			return "not delivered (run finished)"
		}
		if runStatus == statusLimitWait {
			return "queued" + parkedSuffix
		}
		return "queued"
	}
	if runStatus == "awaiting_approval" {
		return "delivered (applies after approval)"
	}
	if runStatus == "awaiting_input" {
		// PRD #88: the run is parked on a question, so a follow-up sits behind the
		// answer rather than behind an approval. Distinct wording because the action
		// the user owes is different, and telling them to approve something would be
		// simply wrong.
		return "delivered (applies after the question is answered)"
	}
	if runStatus == statusLimitWait {
		return "delivered" + parkedSuffix
	}
	return "delivered"
}

// relAge renders a coarse relative age (e.g. "5s", "12m", "3h", "2d") for a
// timestamp, for the steer-queue AGE column. A zero or future time renders "-"
// and "0s" respectively; sub-second precision is not useful here.
func relAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
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

// limitWaitLine is the ONE sentence every CLI surface renders for a parked run
// (PRD #35): `uzi run get`'s detail block, the `--follow` notice and the TUI's run
// header all call this, so the terminal cannot give three readings of one park.
//
// It returns "" for a run that is not parked, which is what makes it safe to call
// unconditionally at each of those sites.
//
// The countdown is off RetryNotBefore, NOT LimitResetsAt, and the two routinely
// differ — RetryNotBefore carries jitter, is clamped to RUN_LIMIT_MAX_PARK, is
// cross-checked against the owner's gauge and is pool-aware, so a user with a second
// credential that still has headroom is promoted long BEFORE the window it hit
// reopens. RetryNotBefore is when work resumes; LimitResetsAt is context, and
// renderRunDetail prints it as its own row rather than folding it in here.
//
// RateLimitType is server-allowlisted (anything outside the SDK union is coerced to
// "unknown"), so it is an enum by the time it reaches here — and it still goes through
// cellText, for the same reason the STOP_KIND row does: "server-controlled today" is
// exactly the assumption that rots, and this string lands in a table cell where a
// newline breaks the rail. An UNRECOGNISED type prints as itself; the CLI is versioned
// separately from the API, so inventing a rendering for a member a newer server ships,
// or dropping it, would both be worse than passing it through.
func limitWaitLine(r apitypes.RunDTO, now time.Time) string {
	if r.Status != statusLimitWait {
		return ""
	}
	line := "paused: Anthropic usage limit"
	if t := cellText(strOr(r.RateLimitType, "")); t != "" {
		line += " (" + t + ")"
	}
	line += " · " + resumesIn(r.RetryNotBefore, now)
	// Attempt N, never "N/cap": the cap is one server constant and is deliberately not
	// on RunDTO, so a denominator here would have to be a second copy of it in a binary
	// that ships on its own release cadence. 0 is a run that has not parked, which this
	// function has already excluded — but the guard stays, because a server that
	// reports a park without a count should print no number rather than "attempt 0".
	if r.LimitWaitCount > 0 {
		line += " · attempt " + itoa(int(r.LimitWaitCount))
	}
	return line
}

// resumesIn renders the promotion clock for a parked run.
//
// A nil RetryNotBefore is a real case, not a defensive branch: an older server, or a
// park whose stamp failed to record. It must not render as "resumes in 0s" (a promise
// the CLI cannot keep) nor be silently omitted (the park then reads as an unexplained
// hang), so it says what is actually known.
//
// A stamp already in the past is the NORMAL steady state for a few seconds — the
// sweeper runs on a ticker, so there is always a window where the clock has passed and
// the promotion has not fired yet.
func resumesIn(retryNotBefore *time.Time, now time.Time) string {
	if retryNotBefore == nil {
		return "no resume time recorded"
	}
	d := retryNotBefore.Sub(now)
	if d <= 0 {
		return "resuming shortly"
	}
	return "resumes in " + fmtUntil(d)
}

// fmtUntil renders a forward-looking duration at TWO units, which is where it parts
// company with relAge's single unit next door.
//
// relAge answers "how old is this row" for a table column, where 3h is precise enough.
// This answers "when do I come back", over a range that runs from seconds to the
// seven-day window RUN_LIMIT_MAX_PARK clamps to — and across that range a single unit
// is unusable: a park with 3h59m left and one with 3h01m left both read "3h", which is
// an hour of error on the only number the user came for.
func fmtUntil(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// pendingJudgePhrase renders a pending judge's normalized state as this CLI's display
// phrase: "scheduled" (enqueued, unclaimed) or "in progress" (a worker has it).
//
// state is NOT untrusted text and deliberately skips sanitizeTTY, unlike every judge
// free-text field beside it: it is produced by the server's total mapper
// (handler.pendingJudgeState) out of a fixed vocabulary, never by the judge model. It is
// also never printed verbatim — only compared — so no server string reaches the terminal
// from here at all.
//
// The default arm is defensive, not reachable today. The mapper answers only
// "scheduled" or "running", but an older CLI against a newer server (the deployment
// order this repo actually ships in) could meet a third value, and the failure mode to
// avoid is "run r1: judge " with a blank phrase. Anything that is not "scheduled" —
// including "" — degrades to the in-progress wording, which is true of every member of
// the active-judge set by construction.
func pendingJudgePhrase(state string) string {
	if state == "scheduled" {
		return "scheduled"
	}
	return "in progress"
}

// renderReview prints the judge's review: a verdict line, an incomplete caveat
// when the judge run did not finish, the summary, and one block per
// recommendation (category, target, confidence) with its rationale beneath.
//
// pj is the ACTIVE judge run for this target (PRD #119) and is independent of rv: all
// four combinations are real states, and the point of the parameter is that two of them
// used to print the same line.
//
//   - rv nil, pj nil → "not judged": nobody has ever judged this run. UNCHANGED, and
//     the one case #119 does not touch.
//   - rv nil, pj set → a judge is already coming; saying "not judged" here told the
//     user a review was missing at the moment one was being written.
//   - rv set, pj set → a re-judge in flight over an existing verdict: the review still
//     renders in full, with a note that it is about to be replaced.
//   - rv set, pj nil → unchanged.
//
// target, rationale_md and summary_md are UNTRUSTED judge free text (Risk 13):
// repo/issue/CI content the judge LLM read can shape them, and ingest cannot
// strip instruction-shaped prose. They are printed as DATA here, never
// interpreted — the same standing the SPA gives them.
func renderReview(p *uzicli.Printer, id string, rv *apitypes.ReviewDTO, pj *apitypes.PendingJudgeDTO) error {
	if rv == nil {
		// 200 {"review": null}: visible but unjudged. Not an error (exit 0) — the
		// API deliberately does not raise one, so the CLI must not invent a 404.
		if pj != nil {
			p.Printf("run %s: judge %s\n", id, pendingJudgePhrase(pj.State))
			return nil
		}
		p.Printf("run %s: not judged\n", id)
		return nil
	}
	p.Printf("verdict: %s", rv.Verdict)
	if rv.JudgeModel != "" {
		p.Printf("    model: %s", rv.JudgeModel)
	}
	p.Println()
	if rv.Status == "failed" {
		// The judge run did not complete: the recommendation set is a fallback and
		// may be partial. Wire value is "failed" (workersvc/judge_review.go enum),
		// NOT "incomplete" (that is only badge copy) — a --json consumer keying on
		// "incomplete" would silently treat every fallback review as complete.
		p.Println("note: judge incomplete — this review is a fallback and may be partial")
	}
	if pj != nil {
		// A re-judge is in flight over the verdict just printed, so what is on screen
		// is the OLD review (the ingest upserts in place). Same "note:" shape as the
		// incomplete caveat above, and both print when both apply — they are separate
		// claims about the same review, not two spellings of one.
		p.Printf("note: judge %s — a re-judge is in flight and will replace this review\n", pendingJudgePhrase(pj.State))
	}
	if s := sanitizeTTY(strings.TrimSpace(rv.SummaryMd)); s != "" {
		p.Println()
		p.Println(s)
	}
	if len(rv.Recommendations) > 0 {
		p.Println()
		p.Println(triageLine(rv.Triage))
		p.Printf("recommendations (%d):\n", len(rv.Recommendations))
		disp := dispositionsByCoord(rv.Dispositions)
		for _, rec := range rv.Recommendations {
			// The short id (git-style, first 8 hex of the rec UUID) is what the
			// mutation verbs accept — printing it makes `uzi review resolve/dismiss`
			// usable straight from this output (Decision 10). Its disposition, if
			// any, is matched on the (category, target) coordinate.
			p.Printf("- %s [%s] %s → %s%s\n",
				shortRecID(rec.ID), rec.Confidence, rec.Category, sanitizeTTY(rec.Target),
				dispositionSuffix(disp[coordKey(rec.Category, rec.Target)]))
			if r := sanitizeTTY(strings.TrimSpace(rec.RationaleMd)); r != "" {
				for _, line := range strings.Split(r, "\n") {
					p.Printf("    %s\n", line)
				}
			}
		}
	}
	return nil
}

// shortRecID is the git-style short recommendation id: the first 8 hex characters
// of the rec UUID. The mutation verbs accept it (resolved back to the full id
// against the current review); --json always carries the full id.
func shortRecID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// coordKey is the (category, target) coordinate a disposition is keyed on — the
// same coordinate the filed-issue link uses. NUL-joined so no category/target
// pair can collide with another.
func coordKey(category, target string) string {
	return category + "\x00" + target
}

// dispositionsByCoord indexes a review's dispositions by their coordinate so each
// recommendation row can find its own verdict in one lookup (mirrors the panel's
// dispByCoord map).
func dispositionsByCoord(ds []apitypes.DispositionDTO) map[string]apitypes.DispositionDTO {
	out := make(map[string]apitypes.DispositionDTO, len(ds))
	for _, d := range ds {
		out[coordKey(d.Category, d.Target)] = d
	}
	return out
}

// dispositionSuffix renders a recommendation's disposition as a trailing chip,
// e.g. "  (done)" / "  (dismissed: not_an_issue, stale)". An empty (zero-value)
// disposition — the common "to do" case — renders nothing.
func dispositionSuffix(d apitypes.DispositionDTO) string {
	if d.Status == "" {
		return ""
	}
	label := d.Status
	if d.Reason != "" {
		label += ": " + d.Reason
	}
	if d.Stale {
		label += ", stale"
	}
	return "  (" + label + ")"
}

// triageLine is the one-line per-review tally the panel's triage bar mirrors
// (Decision 7): rendered straight from the server-computed TriageDTO so the CLI
// and web never disagree.
func triageLine(t apitypes.TriageDTO) string {
	return fmt.Sprintf("triage: %d total · %d to do · %d filed · %d done · %d dismissed (%d false positive)",
		t.Total, t.Todo, t.Filed, t.Done, t.Dismissed, t.FalsePositives)
}
