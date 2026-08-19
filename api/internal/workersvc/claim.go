package workersvc

import (
	"encoding/json"
	"log/slog"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ClaimPayload is the complete, self-contained handoff a worker receives when it
// claims a run: everything it needs to execute without a second server round
// trip. It is returned only over the claim response (the sole secret-delivery
// channel) and must never be logged — it carries the decrypted forge PAT and
// Anthropic token.
//
// Wire shape reconciled with the M2 worker: run fields are flat at the top
// level; repo/secrets/config are nested; secret field names are forge_pat and
// anthropic_oauth_token. Deviation from M2's assumption: agents is an ARRAY of
// structured templates (PRD #3 provides several subagents that map to
// programmatic SDK AgentDefinitions), not a single `template`.
type ClaimPayload struct {
	RunID string `json:"run_id"`
	// Kind is the run kind (issue|ci_fix, PRD #6). The worker branches on it: an
	// issue run works IssueIID's card; a ci_fix run diagnoses + fixes Pipeline.
	Kind string `json:"kind"`
	// IssueIID is the worked issue for an issue run, null for a ci_fix run (which
	// has no issue). IssueTitle/IssueDescription always carry a human summary (for
	// ci_fix, a synthesized one) so the run stays displayable and self-contained.
	IssueIID         *int64 `json:"issue_iid"`
	IssueTitle       string `json:"issue_title"`
	IssueDescription string `json:"issue_description"`
	// IssueComments is the structured, bot/system-filtered, bounded snapshot of the
	// issue's HUMAN comments captured at run creation (PRD #381). nil for every
	// non-issue kind, a comment-less issue, and a connection with an unknown bot id
	// (D9). The agent renders it under a per-prompt nonce fence (M3).
	IssueComments  *IssueCommentsSnapshot `json:"issue_comments,omitempty"`
	Status         string                 `json:"status"`
	Branch         *string                `json:"branch"`     // resume: attach existing branch
	SessionID      *string                `json:"session_id"` // resume: continue SDK session
	LastSeq        int32                  `json:"last_seq"`   // resume: continue message numbering
	IterationCount int32                  `json:"iteration_count"`
	RequeueCount   int32                  `json:"requeue_count"`
	PlanMd         *string                `json:"plan_md"` // resume: plan already captured
	// AutoApprove flags an autopilot run (PRD #19): the worker resolves the plan
	// gate with an approve verdict instead of parking at awaiting_approval. It is
	// top-level (read from the runs row), NOT in ClaimConfig — ClaimConfig is
	// worker-enforced instance caps, this is a per-run fact. Because assembleClaim
	// reads it from the row, a requeued/resumed autopilot run re-delivers it
	// unchanged; without that an unattended resume would hang at the gate forever.
	AutoApprove bool `json:"auto_approve"`
	// OpenQuestionID is the clarification question this run is already parked on
	// (PRD #88 M1), read from the runs row and therefore re-delivered on every
	// resume — the same reason AutoApprove is top-level rather than in ClaimConfig.
	//
	// It is what makes the stale-answer guard survive a worker death. The resumed
	// worker re-parks on the SAME question and re-stamps this SAME id, so an answer
	// the user submitted before the death still names the open question and is
	// honoured. Minting a fresh id on the re-park (or keying the guard on a clock)
	// would reject that answer silently, and the user would have no way to tell.
	// Absent for a run that is not parked.
	OpenQuestionID *string `json:"open_question_id,omitempty"`
	// WaitOnLimit is this run's usage-limit opt-in (PRD #35 Decision 7): on a
	// sustained Anthropic usage limit the worker parks the run instead of failing it.
	// Top-level and re-read from the runs row on every claim, exactly like
	// AutoApprove above and for the same reason — a resumed or re-queued run must
	// keep the behaviour the user chose, and a park-resume-park cycle re-reads it
	// each time.
	WaitOnLimit bool `json:"wait_on_limit"`
	// PlanApproved says this run's plan is already approved (PRD #35 Decision 6b),
	// derived here rather than by the worker: a consumed approve_plan input exists
	// for the run, OR the run is autopilot. On a resume with a resumable session the
	// worker uses it to skip the Phase-1 planning turn and the gate, replaying
	// PlanMd instead. Without it a park-and-resume re-plans, re-parks at
	// awaiting_approval in front of a human who already approved, and can fail with
	// REASON_NO_PLAN when the resumed session declines to re-emit signal_plan.
	PlanApproved bool `json:"plan_approved"`
	// PlanSource is where PlanMd came from (runs.plan_source, PRD #209): 'agent' for a
	// worker-authored plan (or a pre-#209 run), 'seeded' for a plan supplied at create
	// time over the API. The worker needs it to disambiguate the two plan_approved
	// runs that arrive WITHOUT a resumable session: a seeded run (implement the plan,
	// no gate — D4 row 2) versus a run whose session was dropped mid-flight (re-plan —
	// D4 row 3). Read from the runs row, so it re-delivers unchanged on every resume,
	// exactly like AutoApprove and PlanApproved. Always present (the column is NOT
	// NULL); an old worker ignores it and behaves as it does today.
	PlanSource string `json:"plan_source"`
	// PlannedBaseCommit is the commit a SEEDED plan was written against (runs.planned_base_commit,
	// PRD #209 M4), forwarded so the worker can compare it to the clone's resolved base
	// after checkout. Present only for a seeded run created with --planned-commit; omitted
	// otherwise, in which case the worker's staleness compare is inert. Read from the runs
	// row, so it re-delivers unchanged on every resume, like PlanSource above.
	PlannedBaseCommit *string `json:"planned_base_commit,omitempty"`
	// RequireBaseMatch makes a base-commit divergence FAIL the run rather than warn into
	// the feed (runs.require_base_match, PRD #209 M4 Open Question 3). Always present (the
	// column is NOT NULL); false for every run that did not opt in, so an old worker that
	// ignores it, and every non-seeded run, keep today's behaviour.
	RequireBaseMatch bool `json:"require_base_match"`
	// AgentSelection is the run's PERSISTED subagent selection (runs.agent_source /
	// agent_exclusions), replayed on every claim. Omitted when the run has none.
	//
	// 🔴 IT SHIPS BECAUSE PlanApproved SHIPS, AND THE TWO COME FROM ONE HUMAN
	// VERDICT. Normally the selection reaches the worker on the approve_plan
	// verdict — but a run resumed with plan_approved already true HAS NO VERDICT to
	// carry it, so the worker falls through to resolveAgentSelection's "absent"
	// default (repo when a roster was detected, else own, NO exclusions). The
	// consequence is concrete: a human who excluded a subagent at the plan gate gets
	// it back after a park, silently.
	//
	// The argument for fixing it is NOT that exclusions are a security control —
	// they are not, every subagent still comes from the same vetted set and the
	// Agent-guard hook is still frozen to it. It is that Decision 6b's whole premise
	// is "we may skip the gate BECAUSE the human already approved". Propagating the
	// approval while dropping the exclusions that were part of the same decision
	// honours half a verdict, and that is worse than honouring none, because the
	// user gets no signal: they excluded something deliberately, the run resumes,
	// and it comes back.
	//
	// Nil, not an empty selection, when the run never reached a gate — the worker's
	// absent-default is correct there and must stay reachable. Additive on the wire:
	// an old worker ignores the key and behaves exactly as it does today.
	AgentSelection *AgentSelection `json:"agent_selection,omitempty"`

	// Milestones is the run's FROZEN, human-approved milestone list (PRD #122 M1),
	// decoded from runs.milestones_frozen and replayed on every claim so a resumed
	// worker carries the immutable list. omitempty because a run with no frozen list
	// (never proposed one, or is not an issue run) keeps today's claim wire shape
	// exactly — an old worker ignores the key. Nil when the run has none.
	Milestones []Milestone `json:"milestones,omitempty"`

	// TargetRunID is the run a JUDGE run reviews (PRD #46 Decision 1). Present only
	// for kind=judge (omitted otherwise). The judge fetches that run's trace through
	// the Bearer worker trace endpoint (M3); the claim itself stays small and carries
	// no forge PAT and no repo — a judge never does git.
	TargetRunID *string `json:"target_run_id,omitempty"`
	// JudgeModel is the model alias a JUDGE run runs on (PRD #46 Decision 7), resolved
	// from the judge_model setting at claim assembly. Present only for kind=judge.
	JudgeModel *string `json:"judge_model,omitempty"`
	// JudgeSignal is the API-side deterministic command-not-found pre-scan of the
	// reviewed run's tool output (PRD #46 Decision 4). Present only for kind=judge (and
	// omitted when the scan found nothing). The judge interprets it; if the model call
	// fails it is the deterministic fallback recommendation. The regex only flags.
	JudgeSignal *JudgeSignal `json:"judge_signal,omitempty"`
	// FailureClass is the reviewed run's TRUSTED failure ORIGIN — the runs.fail_origin
	// closed-enum VALUE ONLY (never failure_reason free text), read from the target run
	// at judge-claim assembly (PRD #69 M7a Pass B). Present only for kind=judge, and
	// omitted when the reviewed run did not fail with a recognised origin (a NULL
	// fail_origin, or a completed run) — omitempty for the SAME reason JudgeSignal and
	// KnownImproveUziTargets carry it: an ordinary (non-judge) claim keeps today's wire
	// shape byte-for-byte, and an older worker ignores the key. The judge weighs the
	// class — e.g. a policy/config-denied class is NOT retryable. Distinct from
	// autostop.go's unrelated `failure_class` slog key: this is a claim DTO field
	// carrying only the enum value.
	FailureClass *string `json:"failure_class,omitempty"`

	// KnownImproveUziTargets is the run owner's existing improve_uzi target coordinates
	// (issue #232): the judge reuses a matching one verbatim instead of inventing a new
	// phrasing, so future recurrences land on the same exact key the cross-run dedup
	// already collapses. Present only for a judge claim; omitempty because an empty menu
	// (a new user with no improve_uzi history) is normal and must not appear on the wire.
	// Inert data rendered nonce-fenced by the agent — never instructions.
	KnownImproveUziTargets []string `json:"known_improve_uzi_targets,omitempty"`

	// InflightTargets is a best-effort list of work already IN FLIGHT on the same repo
	// at claim time (issue #297), attached only to a self_improve claim so the picker
	// avoids selecting a recommendation whose fix another active run is already doing.
	// Each entry is one compact coordinate line (issue iid + title + kind/status +
	// frozen milestone titles). UNTRUSTED CONTENT — the titles are issue/milestone text
	// anyone who can file an issue can influence — rendered nonce-fenced by the agent,
	// never as instructions. omitempty: an empty set (nothing in flight) is normal and
	// must not appear on the wire; an older worker ignores it.
	InflightTargets []string `json:"inflight_targets,omitempty"`

	// Pipeline is the failed-pipeline snapshot for a ci_fix run (PRD #6): the
	// pipeline the agent diagnoses + fixes, with its failed jobs and log tails.
	// Present only for kind=ci_fix (omitted for issue runs). Log tails are untrusted
	// data — the worker frames them as quoted evidence, never instructions.
	Pipeline *ClaimPipeline `json:"pipeline,omitempty"`

	Repo    ClaimRepo    `json:"repo"`
	Secrets ClaimSecrets `json:"secrets"`
	Agents  []ClaimAgent `json:"agents"`
	Config  ClaimConfig  `json:"config"`

	// Skills is the deduplicated per-run union of every skill allocated to any
	// template for this run's owner (shared allocations ∪ the owner's overlay),
	// after name-collision precedence (user > global > builtin) and the
	// SKILLS_MAX_PER_RUN cap. The worker synthesizes a local plugin dir from this
	// and passes the names as the SDK's explicit top-level enable-list (M4). Always
	// present (possibly empty), never null — the worker always passes an explicit
	// list. Repo-borne skills (opt-in) are resolved worker-side and are not here.
	Skills []ClaimSkill `json:"skills"`
	// SkillsDropped records every skill dropped during assembly — shadowed by a
	// higher-precedence skill of the same name, or over the per-run cap — so the
	// worker can emit the run-message log lines (the worker owns the gapless per-run
	// seq; the server never writes run_messages). Always present (possibly empty).
	SkillsDropped []ClaimSkillDrop `json:"skills_dropped"`
}

// ClaimSkill is one delivered skill in the per-run union: the name+description the
// model routes on and the body it loads on demand. The SKILL.md frontmatter is
// synthesized worker-side from name+description (never stored/sent as frontmatter).
type ClaimSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// ClaimSkillDrop names a skill that assembly dropped and why. Reason is a stable
// code the worker maps to a run-message log line: DropShadowed or DropOverLimit.
type ClaimSkillDrop struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// ClaimRepo carries the repo facts the worker needs. CloneURL is the https clone
// target the worker authenticates with the PAT (via a per-invocation
// http.extraHeader, never writing the PAT into git config); URL is the GitLab
// web URL. Clone from CloneURL, not URL.
type ClaimRepo struct {
	ID            string  `json:"id"`
	URL           string  `json:"url"`
	CloneURL      string  `json:"clone_url"`
	DefaultBranch *string `json:"default_branch"`
	// ForgeType is which forge this repo's connection speaks ("gitlab"|"forgejo"|
	// "github"; PRD #65 D9, GitHub added by #238). Additive + optional on the wire
	// (R8): the server always sends it
	// (forge_connections.forge_type is NOT NULL, default 'gitlab'), and an OLD worker
	// simply ignores the unknown key, so a GitLab run keeps working. A new worker
	// reads it to pick its minimal forge client for MR/PR creation.
	ForgeType string `json:"forge_type"`
	// SkillsEnabled is the repo owner's opt-in to load skills from the repo's own
	// .claude/skills at run time (PRD #16). Default false. When true the worker
	// enumerates repo skills after checkout, applies the same caps, and ranks them
	// below every delivered skill (M6). Skills only — the repo's hooks/settings/
	// commands are never loaded, flag or no flag.
	SkillsEnabled bool `json:"skills_enabled"`
	// ClaudemdEnabled is the repo owner's opt-in (PRD #246) for the lead to read the
	// clone's root CLAUDE.md as nonce-fenced UNTRUSTED/ADVISORY context. Default false.
	// A sibling trust flag of SkillsEnabled; the worker reads and injects it through
	// its own channel (settingSources stays []). Additive on the wire — an old worker
	// ignores the unknown key.
	ClaudemdEnabled bool `json:"claudemd_enabled"`
}

// ClaimSecrets are the decrypted secrets for this run only. The worker holds the
// PAT (the agent subprocess never sees it) and uses the Anthropic token as the
// SDK's OAuth credential. Never logged; never persisted on the worker beyond the
// run. ForgeUsername is the bot login (not sensitive; travels with the PAT for
// git identity / MR authorship).
type ClaimSecrets struct {
	ForgeUsername       string `json:"forge_username"`
	ForgePAT            string `json:"forge_pat"`
	AnthropicOAuthToken string `json:"anthropic_oauth_token"`
}

// ClaimAgent is a PRD #3 agent template as structured fields, ready to map onto
// a programmatic SDK AgentDefinition (not a .claude/agents/*.md file — the
// worker runs with settingSources off, so those files would never load). Tools
// nil means inherit-all; Model nil means inherit.
type ClaimAgent struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	PromptBody  string   `json:"prompt_body"`
	Tools       []string `json:"tools"`
	Model       *string  `json:"model"`
	// Skills is this template's allocated skill names (shared ∪ owner's overlay),
	// restricted to names that survived in the per-run union (a name dropped over
	// the cap is removed; a shadowed name stays because the name is still delivered,
	// backed by the higher-precedence body). The worker maps this onto the
	// subagent's AgentDefinition.skills (M4). Always present (possibly empty).
	Skills []string `json:"skills"`
}

// ClaimConfig are the caps the worker enforces (wall clock, idle, loop count)
// plus the run owner's per-user default model. DefaultModel is the SDK model the
// worker applies to the lead/main thread, taking precedence over the lead
// template's model (PRD #17 Decision 6); it is omitted when the owner has no
// default set (NULL), so the worker falls back to the lead template's model.
type ClaimConfig struct {
	RunTimeoutSeconds  int `json:"run_timeout_seconds"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	MaxIterations      int `json:"max_iterations"`
	// PlanMaxRevisions is the PRD #41 plan-revision cap the worker enforces at the
	// approval gate (server-authoritative; the server also caps in SubmitInput).
	PlanMaxRevisions int `json:"plan_max_revisions"`
	// QuestionMax and QuestionTimeoutSeconds bound the PRD #88 clarification loop:
	// how many times one run may stop to ask the human, and how long a single park
	// waits before the run fails with "clarification timed out". Both are enforced
	// worker-side — the cap counts in-process ask_user parks (there is no input row to
	// count, unlike a revise_plan), and the deadline is a worker-held timer.
	//
	// The deadline is therefore NOT durable: a worker death re-queues the run and the
	// resumed worker starts a fresh clock, so the honest worst case is
	// QuestionTimeoutSeconds × (RUN_MAX_REQUEUES + 1).
	QuestionMax            int     `json:"question_max"`
	QuestionTimeoutSeconds int     `json:"question_timeout_seconds"`
	DefaultModel           *string `json:"default_model,omitempty"`
	// OverrideSubagentModel, when true, tells the worker to apply the run's resolved
	// model (DefaultModel / the lead model) to EVERY subagent, overriding each
	// template's own model: pin — across both the own roster and the cloned repo's
	// roster (PRD #305). Frozen onto the run at fire time (runs.override_subagent_model).
	// omitempty: false is omitted so an off run's claim is byte-identical to today's
	// wire; an un-upgraded worker ignores the field and degrades safely to pins-win.
	OverrideSubagentModel bool `json:"override_subagent_model,omitempty"`
	// SkillMaxBytes and SkillsMaxPerRun are the skill caps the server configured,
	// delivered so the worker enforces the same limits (no hardcoded drift): the
	// worker applies SkillMaxBytes to repo-borne skills and re-enforces
	// SkillsMaxPerRun over the combined delivered ∪ repo set (M4/M6). The server
	// already applied SkillMaxBytes at save and SkillsMaxPerRun at this assembly.
	SkillMaxBytes   int `json:"skill_max_bytes"`
	SkillsMaxPerRun int `json:"skills_max_per_run"`
	// ToolPackages is the run's resolved tier-1 tool package list (PRD #18 M3),
	// already validated against the allowlist. The worker synthesizes a devbox.json
	// from it and provisions in a secret-scrubbed subprocess before the SDK starts.
	// Empty ⇒ no provisioning (today's behavior). Always sent (possibly empty).
	ToolPackages []string `json:"tool_packages"`
	// RepoDevboxOptIn is whether the worker may union the repo's own devbox.json
	// packages into the provisioned set (PRD #18 M5). Delivered from M3 but always
	// false until M5 wires the per-repo trust toggle.
	RepoDevboxOptIn bool `json:"repo_devbox_opt_in"`
	// DeniedToolPackages is the server's Decision 6 denylist BASE NAMES (PRD #123 M1b),
	// shipped so the worker applies the SAME credential-CLI policy to TIER-2 (repo
	// devbox.json opt-in) packages, which are filtered by shape server-side and so are
	// never denylist-checked there. The worker drops any tier-2 package whose base name
	// is in this set before provisioning (repo-tools.ts filterDeniedPackages). It is a
	// compile-time constant list (toolprofile.DenylistNames), NOT a per-run DB read, and
	// applies to tier-2 ONLY — tier-1 is already denylist-checked server-side and must
	// never be filtered locally. Always sent as a non-nil slice (`[]`, never null),
	// matching ToolPackages; an old worker that ignores the key keeps today's behavior
	// (no tier-2 denylist filtering).
	DeniedToolPackages []string `json:"denied_tool_packages"`
	// CIConfigPaths is the ci_fix run's guard watch set (PRD #71 M2): the CI-config
	// glob patterns a fix may touch, resolved server-side at queue time. omitempty is
	// REQUIRED — a non-ci_fix run's runs.ci_config_paths column is NULL, so the field
	// is nil and omitted, keeping every issue/chat claim byte-identical to pre-M2.
	CIConfigPaths []string `json:"ci_config_paths,omitempty"`
}

// agentsFromTemplates maps stored templates to claim-payload agents, decoding
// the jsonb tools allowlist (NULL/empty ⇒ inherit-all ⇒ nil) and attaching each
// template's surviving skill names from perTemplateSkills (nil ⇒ empty list, so
// the JSON carries `skills: []`, never null).
func agentsFromTemplates(rows []store.AgentTemplate, perTemplateSkills map[string][]string) []ClaimAgent {
	out := make([]ClaimAgent, 0, len(rows))
	for _, t := range rows {
		skills := perTemplateSkills[t.Name]
		if skills == nil {
			skills = []string{}
		}
		a := ClaimAgent{
			Name:        t.Name,
			Description: t.Description,
			PromptBody:  t.PromptBody,
			Tools:       decodeTools(t.Tools),
			Skills:      skills,
		}
		if t.Model.Valid {
			m := t.Model.String
			a.Model = &m
		}
		out = append(out, a)
	}
	return out
}

// decodeTools turns the stored jsonb allowlist into a slice; a NULL/empty column
// (inherit-all) yields nil. A malformed value is logged and treated as
// inherit-all rather than failing the whole claim.
func decodeTools(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		slog.Error("workersvc: decode template tools", "error", err)
		return nil
	}
	return out
}
