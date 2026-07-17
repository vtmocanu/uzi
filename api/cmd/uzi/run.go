package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// logsPollInterval is how often `uzi run logs --follow` re-polls
// /api/runs/{id}/messages?after=<seq>. REST polling ships instead of a WebSocket
// (PRD #64 Out of scope).
const logsPollInterval = 2 * time.Second

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
			for {
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
				if !follow {
					return nil
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
		Short: "Start a run on a repo/issue",
		RunE:  stubRunE("run create"),
	}
	create.Flags().StringSlice("agents", nil, "restrict the run to these agents")

	approve := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run approve"),
	}
	reject := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run reject"),
	}
	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run cancel"),
	}
	followUp := &cobra.Command{
		Use:   "follow-up <run-id>",
		Short: "Send a follow-up message to a run",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run follow-up"),
	}

	cmd.AddCommand(list, get, logs, review, create, approve, reject, cancel, followUp)
	return cmd
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
		rows = append(rows, []string{"HEALTH_REASON", *r.HealthReason})
	}
	if r.FailureReason != nil && *r.FailureReason != "" {
		rows = append(rows, []string{"FAILURE_REASON", *r.FailureReason})
	}
	return p.Table(nil, rows)
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
	s := strings.TrimSpace(string(raw))
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
	if s := strings.TrimSpace(rv.SummaryMd); s != "" {
		p.Println()
		p.Println(s)
	}
	if len(rv.Recommendations) > 0 {
		p.Println()
		p.Printf("recommendations (%d):\n", len(rv.Recommendations))
		for _, rec := range rv.Recommendations {
			p.Printf("- [%s] %s → %s\n", rec.Confidence, rec.Category, rec.Target)
			if r := strings.TrimSpace(rec.RationaleMd); r != "" {
				for _, line := range strings.Split(r, "\n") {
					p.Printf("    %s\n", line)
				}
			}
		}
	}
	return nil
}
