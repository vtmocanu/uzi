package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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

	cmd.AddCommand(
		newRunListCmd(env, gf), newRunGetCmd(env, gf), newRunLogsCmd(env, gf), newRunWaitCmd(env, gf),
		newRunReviewCmd(env, gf), newRunCreateCmd(env, gf), newRunApproveCmd(env, gf), newRunRejectCmd(env, gf),
		newRunReviseCmd(env, gf), newRunCancelCmd(env, gf), newRunStopCmd(env, gf), newRunScopeCmd(env, gf),
		newRunFollowUpCmd(env, gf), newRunAnswerCmd(env, gf), newRunInputsCmd(env, gf), newRunExpediteCmd(env, gf),
		newRunResumeNowCmd(env, gf), newRunMrReworkCmd(env, gf),
	)
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
