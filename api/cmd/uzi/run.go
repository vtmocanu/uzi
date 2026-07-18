package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
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
)

// agentSources are the two rosters a plan approval may draw its subagents from
// (apitypes.AgentSelection.Source). "own" is the run owner's template roster; "repo"
// is the set the worker detected in the clone's .claude/agents/.
const (
	agentSourceOwn  = "own"
	agentSourceRepo = "repo"
)

// logsPollInterval is how often `uzi run logs --follow` re-polls
// /api/runs/{id}/messages?after=<seq>. REST polling ships instead of a WebSocket
// (PRD #64 Out of scope). A var, not a const, only so tests can shrink the wait;
// nothing at runtime reassigns it.
var logsPollInterval = 2 * time.Second

// terminalRunStatuses are the run states from which no further messages can
// arrive. `uzi run logs --follow` stops once the run reaches one of them (after a
// final drain) instead of polling forever — otherwise an agent capturing a
// finished run hangs. Mirrors the store's `status NOT IN (...)` active filter.
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

	review := &cobra.Command{
		Use:   "review <run-id>",
		Short: "Show the judge's review for a run (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rv, err := c.RunReview(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// Pass the envelope through: {"review": <dto>|null}. A nil rv
				// serializes to {"review": null} — the exact shape the endpoint
				// returns for a visible-but-unjudged run (D21).
				return p.JSON(map[string]any{"review": rv})
			}
			return renderReview(p, args[0], rv)
		},
	}

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
			run, err := c.CreateRun(cmd.Context(), repoID, issue)
			if err != nil {
				return err
			}
			return renderCreatedRun(env, gf, run)
		},
	}
	create.Flags().String("repo", "", "repo id to run against (see 'uzi repo list')")
	create.Flags().Int64("issue", 0, "the PRD issue IID to run")

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

	cmd.AddCommand(list, get, logs, review, create, approve, reject, cancel, followUp)
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
		fmt.Fprintf(env.Stderr, "warning: %s\n", sanitizeTTY(strings.TrimSpace(*run.HealthReason)))
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
func renderRunDetail(p *uzicli.Printer, r apitypes.RunDTO) error {
	rows := [][]string{
		{"ID", r.ID},
		{"KIND", r.Kind},
		{"STATUS", r.Status},
		{"TITLE", runTitle(r)},
		{"BRANCH", strOr(r.Branch, "-")},
		{"MR", int64Or(r.MrIID, "-")},
		{"HEALTH", r.Health},
	}
	if r.HealthReason != nil && *r.HealthReason != "" {
		rows = append(rows, []string{"HEALTH_REASON", sanitizeTTY(*r.HealthReason)})
	}
	if r.FailureReason != nil && *r.FailureReason != "" {
		rows = append(rows, []string{"FAILURE_REASON", sanitizeTTY(*r.FailureReason)})
	}
	return p.Table(nil, rows)
}

// sanitizeTTY strips terminal control characters from UNTRUSTED free text before
// it is written to a human TTY (Risk 13). Judge/run content can carry attacker-
// shaped bytes that repo/issue/CI text fed the LLM; printed verbatim, an embedded
// ANSI escape/CSI sequence could clear the screen, recolour, hide, or spoof
// output. It removes C0 controls (0x00–0x1F) except tab and newline, and C1
// controls (0x80–0x9F); every other rune, including all printable UTF-8, passes
// through unchanged. It iterates runes (not bytes) so a multibyte UTF-8 codepoint
// whose bytes fall in 0x80–0x9F is never corrupted. Human render path ONLY —
// --json output stays byte-exact (structural JSON encoding already escapes these
// and agents decode it verbatim, so sanitizing there would corrupt payloads).
func sanitizeTTY(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

// renderMessage prints one run message. In --json mode it emits one compact JSON
// object per line (NDJSON), so `--follow` streams cleanly and an agent parses it
// line by line; in human mode it prints a single #seq/kind/agent/payload line.
func renderMessage(p *uzicli.Printer, m apitypes.MessageDTO) error {
	if p.Format == uzicli.FormatJSON {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		p.Println(string(b))
		return nil
	}
	agent := "-"
	if m.Agent != nil && *m.Agent != "" {
		agent = *m.Agent
	}
	p.Printf("#%-4d %-16s %-10s %s\n", m.Seq, m.Kind, agent, compactPayload(m.Payload))
	return nil
}

// compactPayload renders a message payload as a single truncated line for the
// human table. The payload is server-forwarded run content; it is DATA, printed
// verbatim (never interpreted).
func compactPayload(raw json.RawMessage) string {
	s := sanitizeTTY(strings.TrimSpace(string(raw)))
	s = strings.ReplaceAll(s, "\n", " ")
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// renderReview prints the judge's review: a verdict line, an incomplete caveat
// when the judge run did not finish, the summary, and one block per
// recommendation (category, target, confidence) with its rationale beneath.
//
// target, rationale_md and summary_md are UNTRUSTED judge free text (Risk 13):
// repo/issue/CI content the judge LLM read can shape them, and ingest cannot
// strip instruction-shaped prose. They are printed as DATA here, never
// interpreted — the same standing the SPA gives them.
func renderReview(p *uzicli.Printer, id string, rv *apitypes.ReviewDTO) error {
	if rv == nil {
		// 200 {"review": null}: visible but unjudged. Not an error (exit 0) — the
		// API deliberately does not raise one, so the CLI must not invent a 404.
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
	if s := sanitizeTTY(strings.TrimSpace(rv.SummaryMd)); s != "" {
		p.Println()
		p.Println(s)
	}
	if len(rv.Recommendations) > 0 {
		p.Println()
		p.Printf("recommendations (%d):\n", len(rv.Recommendations))
		for _, rec := range rv.Recommendations {
			p.Printf("- [%s] %s → %s\n", rec.Confidence, rec.Category, sanitizeTTY(rec.Target))
			if r := sanitizeTTY(strings.TrimSpace(rec.RationaleMd)); r != "" {
				for _, line := range strings.Split(r, "\n") {
					p.Printf("    %s\n", line)
				}
			}
		}
	}
	return nil
}
