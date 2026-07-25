package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

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
						fmt.Fprintln(env.Stderr, "note: chat run — this queue lists every chat turn as a follow-up (chat has its own web composer, unaffected)")
					}
				}
			}
			return renderRunInputs(p, list, status)
		},
	}

	cmd.AddCommand(list, get, logs, review, create, approve, reject, cancel, followUp, inputs)
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
// DEL. 0x7f is outside sanitizeTTY's C0 (<0x20) and C1 (0x80–0x9f) ranges, so it
// survives too. It advances no column but some terminals draw a glyph for it, so it
// is dropped rather than folded.
//
// Both are cosmetic — no cursor motion, no erase, no OSC, and `\n` cannot survive
// compactText — but the actor column's whole purpose is to stay aligned down a
// `uzi run logs --follow` stream, and model-authored prose is exactly where a stray
// tab comes from.
func cellText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\t':
			return ' '
		case 0x7f:
			return -1
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
//   - not consumed, otherwise     → "queued"
//   - consumed, run at plan gate  → "delivered (applies after approval)"
//   - consumed, otherwise         → "delivered"
//
// runStatus may be "" when the run's status could not be fetched (Decision 10
// floor): the gate/terminal nuance is then dropped and only queued/delivered
// show — the acceptable CLI minimum.
func steerState(consumedAt *time.Time, runStatus string) string {
	if consumedAt == nil {
		if terminalRunStatuses[runStatus] {
			return "not delivered (run finished)"
		}
		return "queued"
	}
	if runStatus == "awaiting_approval" {
		return "delivered (applies after approval)"
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
