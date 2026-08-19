// Worker↔API wire contract (PRD #4 §Worker protocol).
//
// These shapes are pinned against the M1 server, not assumed: the claim payload
// was reconciled in f74bf6a (structured agents[], tokenless clone_url), and the
// /state body's `status` key matches M1's reconciled JSON tag. Field names
// follow the DB column names (PRD #4 §Schema) and snake_case JSON, matching the
// Go server's conventions; `config` timeouts are in *seconds*
// (run_timeout_seconds / idle_timeout_seconds), matching M1's claim.go. See the
// PRD #4 Decision Log for the reconciliation history.

/** All worker endpoints live under this prefix and take a Bearer join token. */
export const WORKER_API_PREFIX = "/api/worker";

/** Non-terminal + terminal run states the worker reports via /state. */
export type RunState =
  | "running"
  | "awaiting_approval"
  /** PRD #88: parked on a clarification question. The state report MUST carry
   *  open_question_id; the api rejects it otherwise, because a park with no question
   *  identity can never satisfy SetRunRunning's resume guard. */
  | "awaiting_input"
  | "limit_wait"
  | "completed"
  | "failed";

// TERMINAL_STATES lived here and was DELETED (PRD #35). A repo-wide grep found
// exactly one line — its own definition. No reader anywhere, not even in this file,
// and `noUnusedLocals` could never flag it because it was exported.
//
// PRD #35 first added a warning comment telling a future editor not to put
// `limit_wait` in the Set. That comment's own reasoning is the argument for this
// deletion: adding a member was type-legal and nothing would have failed, so the Set
// could not enforce anything. Leaving it would have left a no-op looking like a
// guardrail, which is worse than either having it or not.
//
// WHERE THE DISTINCTION IS ACTUALLY ENFORCED — corrected, because the first version
// of this note named three places and two of them do not enforce terminality at all:
// the `RunState` union above is the reportable DOMAIN (all five statuses a worker may
// send, terminal or not), and the server's status CHECK is likewise a domain
// constraint over all eight. Neither says anything about which are terminal.
//
// The one that matters, and that the first version omitted, is
// **`TERMINAL_RUN_STATUSES` in `home-reclaim.ts`** — same package, the only live
// terminal set in the agent, and the one whose modification breaks PRD #35. Adding
// `limit_wait` there makes the stranded-HOME sweep delete a PARKED run's SDK
// transcript, which is the ~170 MB the whole resume depends on. The warning this
// deleted Set used to carry now lives there, where it has teeth: unlike here, that
// change has a destructive consequence AND a test that catches it.
//
// `uzicli`'s `terminalRunStatuses` is genuinely a terminal set, but it lives in
// another service and gates whether `uzi run logs --follow` stops polling.
//
// If a consumer for a Set like this ever appears here, add it back with that
// consumer — not before.

/** run_messages.kind (PRD #4 §Schema; PRD #39 adds user_message + proposal; PRD #41
 *  adds plan_feedback + plan_revising — the DB column carries no CHECK, so these need
 *  no migration, Decision 8/D12). */
export type MessageKind =
  | "text"
  | "thinking"
  | "tool_use"
  | "tool_result"
  | "status"
  | "error"
  | "user_message"
  | "plan"
  | "proposal"
  /** PRD #191 M5: a start_run REQUEST card (payload `{ repo_path, issue_iid, title }`).
   *  Like `proposal` it is human-gated — the model emits the card, only the user's
   *  Start click actually queues the run (through their own connection). */
  | "run_request"
  /** PRD #322 M1: a cancel_run REQUEST card (payload `{ run_id }`). Like `run_request`
   *  it is human-gated — the model emits the card, only the user's Cancel click actually
   *  cancels the run (through their own connection); run_id is re-resolved server-side. */
  | "cancel_request"
  /** PRD #322 M3: a steer_run REQUEST card (payload `{ run_id, message }`). Like
   *  `cancel_request` it is human-gated — the model emits the card with a PROPOSED
   *  follow-up, only the user's Send click (after they edit the message) delivers it
   *  (through their own connection); run_id is re-resolved server-side. */
  | "steer_request"
  /** PRD #333 M2: an INCIDENTAL FINDING card the run-lane `report_incidental_issue`
   *  tool emits (payload `{ id, title, location, confidence?, labels }`). The finding
   *  is already persisted server-side (the api derived user/repo from the run and
   *  returned the id the card carries); the card is a non-blocking side note — the
   *  human files or dismisses it later from the backlog. All fields are model-authored
   *  from attacker-influenceable repo content: escaped sinks only (web M7). */
  | "finding"
  /** PRD #41: the user's revision feedback at the approval gate (payload
   *  `{ feedback: string }`), echoed to the feed so the revision is auditable. */
  | "plan_feedback"
  /** PRD #41: the lead is revising the plan for round N (payload `{ round: number }`);
   *  the UI derives its "revising" gate state from this. */
  | "plan_revising"
  /** PRD #88: the lead is asking the human a question and the run is parking.
   *  Payload is a QuestionPayload. Emitted BEFORE the awaiting_input state report,
   *  so the question is durable before any surface learns the run parked — and so a
   *  resumed worker can recover it. Every string in it is model-authored from
   *  attacker-influenceable repo/issue content: escaped sinks only. */
  | "question"
  /** PRD #88: the answer that resolved a question (payload `{ question_id, answers }`),
   *  echoed to the feed so the round-trip is auditable, mirroring plan_feedback. */
  | "answer"
  /** PRD #35 Decision 10: the run is being PARKED until the owner's Anthropic usage
   *  window reopens. Payload `{ rate_limit_type?: string, resets_at?: string }` —
   *  both OMITTED rather than null when unknown, so "unknown" has one shape.
   *
   *  Structured rather than a `status` line with the facts baked into prose, and the
   *  reason is not tidiness: the renderer must map `rate_limit_type` through a
   *  known-value lookup with a neutral fallback, because a feed payload is
   *  WORKER-AUTHORED and the server's enum allowlist never reaches it. A value
   *  interpolated into a sentence cannot be mapped without re-parsing the sentence,
   *  and re-parsing worker text to decide how to render it is worse than the bare
   *  string we had.
   *
   *  Decision 10 also lists an `attempt` key. It is deliberately NOT emitted — see
   *  RunRunner.handleLimitReached; the worker cannot know it, and the run row's
   *  authoritative `limit_wait_count` is already on the DTO. */
  | "limit_wait"
  /** PRD #35 Decision 8: the run hit a usage limit and is NOT waiting for it — an
   *  opted-out run, or one whose park the server declined. Same payload as
   *  `limit_wait`. The two are distinct kinds because they mean opposite things to a
   *  reader: one says "this will resume", the other says "this is over". */
  | "limit_hit";

/** run_user_inputs.kind (PRD #4 §Schema; PRD #41 adds `revise_plan` — the user asks
 *  for a new plan version at the gate, body = their feedback text; PRD #88 adds
 *  `answer` — the human's reply to an ask_user question, body = JSON AnswerBody).
 *
 *  Widening this union is NOT sufficient on its own: SteeringChannel.route() must
 *  also learn the kind. `GET /inputs` is consume-on-read, so an unrouted kind is
 *  marked consumed server-side and then dropped by route()'s default arm — the input
 *  is destroyed with no error and no retry, and it looks exactly like a user who
 *  never answered. route() takes a bare string, so nothing here typechecks that. */
export type InputKind =
  | "follow_up"
  | "approve_plan"
  | "reject_plan"
  | "cancel"
  | "revise_plan"
  | "answer";

/** One question in an ask_user call (PRD #88 M1), mirroring the local
 *  AskUserQuestion tool's shape so the model has a familiar contract and the UI can
 *  render it richly. Free-text answers are ALWAYS allowed, including when options are
 *  offered — options are a convenience, never a closed set. */
export interface AskUserQuestion {
  /** The question itself. UNTRUSTED, model-authored. */
  question: string;
  /** A short label for the question. UNTRUSTED, worker-clamped. */
  header: string;
  /** Optional suggested answers. Absent or empty both mean "free text only". */
  options?: { label: string; description: string }[];
  /** Whether several options may be picked. Default false. */
  multiSelect?: boolean;
}

/** Payload of a `question` run-message (PRD #88 M1). */
export interface QuestionPayload {
  /** The question's stable identity, minted by the worker at the first park and
   *  RE-USED verbatim when a resumed worker re-parks on the same question. This is
   *  what the answer names, and what SetRunRunning's resume guard compares.
   *
   *  It is the ONLY staleness key in this feature, deliberately. There is no
   *  generation counter, no epoch and no timestamp anywhere on the answer path: every
   *  such key is bumped by a requeue re-park, which would silently reject an answer
   *  the user submitted correctly against the live question before the worker died.
   *  Do not add one alongside this — a conjunction is only as good as its weaker
   *  clause, so an arrival-ordinal check sitting next to identity would re-introduce
   *  exactly the rejection identity exists to prevent. */
  question_id: string;
  questions: AskUserQuestion[];
}

/** Body of an `answer` steering input, and payload of the `answer` run-message
 *  (PRD #88 M1). JSON rather than the bare prose every other kind carries, because
 *  an answer must name the question it answers; `approve_plan` already establishes
 *  the JSON-body idiom. Slack's inbound is free text, so the replier resolves the
 *  thread anchor to the question id and constructs this same shape server-side —
 *  one wire contract, not two. */
export interface AnswerBody {
  question_id: string;
  /** Index-aligned with the question payload's `questions`. */
  answers: string[];
}

/** runs.kind (PRD #6; PRD #46 adds "judge" and "self_improve"; PRD #241 adds
 *  "prompt"). "chat" is the chat lane's kind (see `ChatClaimResponse.kind: "chat"`):
 *  repo-less and issue-less. self_improve is issue-shaped and runs the ordinary run
 *  lane (RunRunner), with a fixed branch and its own MR evidence (Decision 10).
 *  "prompt" is an ad-hoc SCHEDULED run (PRD #241 Decision 10): repo-ful and
 *  ISSUE-LESS — the `ci_fix` shape, not `self_improve` (which carries a real
 *  tracking issue). For a `prompt` claim `issue_iid` is null (like ci_fix),
 *  `repo` is set, and `issue_description` carries the schedule's stored task
 *  text; it opens a repo→MR run with no issue to close.
 *
 *  RUN_KINDS is mirrored from the DB `runs_kind_check` constraint (in DB CHECK
 *  order); agent/test/run-kind-db-parity.test.ts keeps the two in sync. */
export const RUN_KINDS = ["issue", "ci_fix", "chat", "judge", "self_improve", "prompt"] as const;
export type RunKind = (typeof RUN_KINDS)[number];

/** How a run's plan_md was produced (PRD #209 D4). "agent": the worker's own Phase-1
 *  planning turn — today's only case and the DEFAULT. "seeded": the user supplied the
 *  plan at create time, so it was AUTHORED rather than gate-approved; the worker
 *  implements it with no planning turn and no approval gate. Absent on an older server
 *  ⇒ treat as "agent". Not exported: only ClaimResponse below reads it, and the runner's
 *  discriminator compares the literal `"seeded"` rather than the type. */
type PlanSource = "agent" | "seeded";

/** A ci_fix run's outbound verdict (PRD #6). Only "not_code" travels the wire;
 *  verified/fix_failed are stamped server-side from the post-fix pipeline. */
export type FixVerdict = "not_code";

/** The failed-pipeline snapshot a ci_fix run works from (PRD #6). Frozen at queue
 *  time so the run is self-contained. Log tails are UNTRUSTED (quoted evidence). */
export interface ClaimPipeline {
  id: number;
  ref: string;
  sha: string;
  web_url: string;
  failed_jobs: ClaimFailedJob[];
}

/** One failed job in a ClaimPipeline: identity + a bounded tail of its trace. */
export interface ClaimFailedJob {
  name: string;
  stage: string;
  web_url: string;
  log_tail: string;
}

export interface RegisterRequest {
  name: string;
  version: string;
  /** The template this worker's image was built from (PRD #18), baked in as
   *  ENV WORKER_TEMPLATE. Optional: omitted by images without it (older workers),
   *  in which case the server stores NULL for template_reported. Soft signal for
   *  drift display only — never an authn/authz input. */
  template?: string;
  /** The worker's advertised RUN-lane concurrency cap (PRD #42 Decision 3,
   *  WORKER_MAX_CONCURRENT_RUNS). Observability the server records (clamped server-
   *  side to [1,256]) and the fleet UI renders as "N/M runs"; never enforced. A
   *  pre-#42 worker omits it and the column stays NULL. Distinct from the chat
   *  lane's own concurrency (WORKER_CHAT_SESSIONS). */
  max_concurrent_runs?: number;
  /** The capabilities this worker self-reports as REACHABLE realities (PRD #83 Q1).
   *  An ARRAY (not a `docker` bool) so #84 can grow the capability vocabulary without
   *  another wire change; #83 only ever puts `["docker"]` here (a daemon is reachable,
   *  resolved by docker-wiring.ts) or omits it. In M1 the api declares-and-ignores this
   *  field (accept-and-ignore, no storage) — #84 owns the vocabulary + the consuming
   *  query. Sent only when non-empty (same "only send when known" shape as `template`);
   *  compat rule: the api MUST tolerate it in the same release the worker sends it. */
  capabilities?: string[];
}

export interface RegisterResponse {
  // The worker is identified by its Bearer token on every subsequent call
  // (heartbeat/claim carry no worker id in the path), so this is not strictly
  // needed; M1 returns it and we capture it for logging when present.
  worker_id?: string;
}

/**
 * Container resource sample the worker self-reports on the heartbeat (PRD #49).
 * Read from the worker's own cgroup v2 files (or a process-level fallback), so one
 * sample covers the worker plus every SDK/git/devbox child in the same container
 * cgroup. Display-only server-side (Decision 5): four numbers and an enum, never a
 * scheduling input, and it can carry no secret or repo content.
 */
export interface WorkerStats {
  /** Container CPU as a percentage of the cgroup's ALLOWED CPUs (cpu.max quota when
   *  set, else host core count), so 100 = "all the CPU this container may use".
   *  Omitted on the first tick after start (no delta yet) and whenever the
   *  elapsed/usage delta is undefined (Decision 2). */
  cpu_pct?: number;
  /** Working-set memory in bytes: `memory.current − inactive_file` under cgroup
   *  (matches `docker stats`, not the cache-inflated raw current), or
   *  `process.memoryUsage().rss` under the process fallback. */
  mem_bytes: number;
  /** The cgroup memory limit in bytes, or null when unlimited (`memory.max` = "max")
   *  or unknown (process fallback). null ⇒ the UI shows absolute usage, no bar. */
  mem_limit_bytes?: number | null;
  /** Which mechanism produced the sample: "cgroup" (container-wide, covers children)
   *  or "process" (this worker process only, children-blind — the UI labels it). */
  source: "cgroup" | "process";
}

export interface HeartbeatRequest {
  version: string;
  /** Optional container resource sample (PRD #49), same absent-optional convention
   *  as `template` on register: the worker omits it when the collector produced
   *  nothing, and the server both tolerates its absence and (Decision 3) decodes it
   *  defensively so a malformed sample drops the stats without failing the heartbeat. */
  stats?: WorkerStats;
}

/** Kebab-case agent name. Mirrors the API's template nameRe
 *  (api/internal/handler/agent_templates.go:28) so a repo-detected agent, a uzi
 *  template, and an exclusion entry all answer to one identity rule. */
export const AGENT_NAME_RE = /^[a-z0-9]+(-[a-z0-9]+)*$/;

/** Cap on an agent name. The regex admits arbitrary length; this bounds it. */
export const AGENT_NAME_MAX_LEN = 64;

/** Structured agent-template fields (PRD #3), consumed programmatically by M3. */
export interface AgentTemplate {
  name: string;
  description: string;
  /** null/absent = inherit; else alias or full model id. */
  model?: string | null;
  /** null/absent = inherit all tools; else an allowlist. */
  tools?: string[] | null;
  prompt_body: string;
  /** This template's allocated skill names (PRD #16), restricted server-side to
   *  names present in the run's skill union. The worker maps these onto the
   *  subagent's AgentDefinition.skills (M4). Always sent (possibly empty). */
  skills?: string[];
}

/** One delivered skill in the per-run union (PRD #16): the name+description the
 *  model routes on plus the body it loads on demand. The worker synthesizes the
 *  SKILL.md frontmatter from name+description (M4); the body is placed below it. */
export interface ClaimSkill {
  name: string;
  description: string;
  body: string;
}

/** A skill assembly dropped, with a stable reason code the worker turns into a
 *  run-message log line (PRD #16): "shadowed" (a higher-precedence skill of the
 *  same name won) or "over_limit" (past SKILLS_MAX_PER_RUN). */
export interface ClaimSkillDrop {
  name: string;
  reason: string;
}

/** Repo coordinates for the clone (PRD #2 repos row). */
export interface ClaimRepo {
  id: string;
  /** GitLab WEB url of the repo (for display/links); NOT the clone target. */
  url: string;
  /** The clone target the worker clones from. Tokenless https: the PAT is
   *  supplied out-of-band via an env-scoped HTTP auth header (Basic, since
   *  git-over-HTTPS uses Basic — not GitLab's REST-only PRIVATE-TOKEN), never
   *  embedded in the URL (so it can't rest in the bare repo's on-disk config). */
  clone_url: string;
  default_branch?: string | null;
  /** Which forge this repo's connection speaks (PRD #65 D9; GitHub added by #238).
   *  Additive and OPTIONAL: absent ⇒ "gitlab", so an old api (which never sends it)
   *  still drives a GitLab run on a new worker (R8). The worker selects its forge
   *  client from this. */
  forge_type?: "gitlab" | "forgejo" | "github";
  /** Repo owner's opt-in (PRD #16): load skills from the repo's own
   *  .claude/skills at run time. Default false. When true the worker enumerates
   *  repo skills after checkout, applies the caps, and ranks them below every
   *  delivered skill (M6). Skills only — repo hooks/settings/commands never load. */
  skills_enabled?: boolean;
  /** Repo owner's opt-in (PRD #246): let the LEAD read the clone's root CLAUDE.md
   *  as a nonce-fenced UNTRUSTED/ADVISORY block. Default false. A sibling trust flag
   *  of skills_enabled; read and injected through the worker's own channel, so
   *  settingSources stays []. Additive/optional — absent ⇒ false. */
  claudemd_enabled?: boolean;
}

/**
 * Per-run secrets. Delivered ONLY in the claim response (PRD: "the claim
 * payload is the only delivery path"). Never persisted beyond the run, never
 * logged. The worker (not the agent) holds forge_pat and performs every
 * authenticated git/MR op with it.
 */
export interface ClaimSecrets {
  forge_pat: string;
  // The Anthropic subscription OAuth token (M3 sets it as
  // CLAUDE_CODE_OAUTH_TOKEN). Unused in M2; captured so the shape is stable.
  anthropic_oauth_token?: string;
  // Bot login for the git commit identity + MR authorship. Used in M4; M2
  // ignores it.
  forge_username?: string;
}

/** Per-run caps the server may push down (PRD §Configuration). Advisory in M2.
 *  Field names + units match M1's claim.go: timeouts are in *seconds*; any
 *  future ms consumer converts at the use site. */
export interface ClaimConfig {
  run_timeout_seconds?: number;
  idle_timeout_seconds?: number;
  max_iterations?: number;
  /** Bound on plan-revision rounds at the approval gate (PRD #41, env
   *  PLAN_MAX_REVISIONS). The worker enforces the same cap the server does. */
  plan_max_revisions?: number;
  /** PRD #88 clarification bounds: how many times one run may stop to ask the human,
   *  and how long a single park waits before the run fails "clarification timed out".
   *  Both enforced worker-side (a question is an in-process ask_user signal, so unlike
   *  a revise_plan there is no input row for the server to count) but configured
   *  server-side and shipped here, because a worker env var is unreachable on hosted
   *  k8s. Absent or <= 0 from an older server ⇒ the worker's own defaults. */
  question_max?: number;
  question_timeout_seconds?: number;
  /** The run owner's per-user default model (PRD #17). When present it overrides
   *  the lead template's model for the main thread; absent when the owner set no
   *  default, so the worker falls back to the lead template's model. */
  default_model?: string;
  /** PRD #305: apply the run's resolved model to every subagent, overriding pins,
   *  across both rosters. Absent/false = pins win (today's behavior). */
  override_subagent_model?: boolean;
  /** Skill caps the server configured (PRD #16), delivered so the worker enforces
   *  the same limits (no drift): skill_max_bytes bounds a skill body (applied to
   *  repo-borne skills worker-side), skills_max_per_run bounds the combined
   *  delivered ∪ repo union (re-enforced worker-side, M4/M6). */
  skill_max_bytes?: number;
  skills_max_per_run?: number;
  /** Tier-1 tool packages (PRD #18 M3): the resolved, allowlist-validated devbox
   *  package list the worker provisions before the SDK starts. Empty/absent ⇒ no
   *  provisioning (today's behavior). The server resolves this; the worker only
   *  installs it (in a secret-scrubbed subprocess). */
  tool_packages?: string[];
  /** PRD #71 M5: server-produced protected CI-config paths (globs) for THIS ci_fix
   *  run — the static CI_AUTOFIX_CONFIG_PATHS defaults unioned with the project's
   *  configured ci_config_path. The pre-push guard refuses an AUTO-approved ci_fix
   *  diff that touches any of these. omitted/empty for non-ci_fix runs. */
  ci_config_paths?: string[];
  /** Repo devbox.json packages opt-in (PRD #18 M5): whether the worker may union
   *  the repo's own devbox.json packages (packages-only) into the provisioned set.
   *  Delivered from M3 but always false until M5 wires the per-repo toggle. */
  repo_devbox_opt_in?: boolean;
  /** The server's Decision 6 denylist BASE NAMES (PRD #123 M1b), delivered so the
   *  worker applies the same credential-CLI policy to TIER-2 (repo devbox.json opt-in)
   *  packages — the server filters those by shape only, so they are never
   *  denylist-checked server-side. The worker drops any tier-2 package whose base name
   *  is in this set before provisioning (repo-tools.ts filterDeniedPackages). Tier-1
   *  (tool_packages) is already denylist-checked server-side and is NEVER filtered
   *  locally. Absent on an older server ⇒ undefined ⇒ no filtering (today's behavior). */
  denied_tool_packages?: string[];
}

/**
 * A single milestone in a plan's breakdown (PRD #122 M1). `id` is a short stable
 * handle (e.g. "m1") the lead assigns; `title` is a terse human label. The pair is
 * the whole wire shape — additive, optional, and validated server-side (Decision
 * 12); a worker-side cap is hygiene only, never the authoritative control.
 */
export interface Milestone {
  id: string;
  title: string;
}

/**
 * The lead's live progress over the frozen milestone list (PRD #122 M2). `completed`
 * and `in_progress` are sets of milestone ids the lead reports as it works; the server
 * unions `completed` (monotone, Decision 3) and overwrites `in_progress`. Reported on
 * `running` state reports only, fire-and-forget — an informational field never fails a
 * run. The worker carries these unchanged from the `report_progress` signal to the wire;
 * the api is the authority on membership + shape (Decision 12).
 */
export interface MilestoneProgress {
  completed: string[];
  in_progress: string[];
}

/**
 * The server-computed effective per-run budget (PRD #122 M2, Decision 5/5b), read off
 * a state-report ACK and applied by the implement loop. Both optional: a field absent
 * means "no update for this dimension, keep the worker's current budget". `maxIterations`
 * is the total implement/review turn ceiling scaled by milestone count; `wallSeconds` is
 * the effective wall-clock the sweeper enforces, mirrored here as the worker's own soft
 * reference so a legitimately-scaled run does not self-trip its wall.
 */
export interface IterationBudget {
  maxIterations?: number;
  wallSeconds?: number;
}

/**
 * Response body of a successful (200) claim.
 *
 * The server also sends top-level run fields the worker does not consume
 * (status, iteration_count, requeue_count); they are ignored here.
 *
 * `plan_md` used to be in that ignored list and no longer is — PRD #35's resume
 * path consumes it, so it is declared below.
 */
export interface ClaimResponse {
  run_id: string;
  /** Run kind (PRD #6). "issue": work issue_iid's card. "ci_fix": diagnose + fix
   *  the failed `pipeline`. Absent on older servers ⇒ treat as "issue". */
  kind?: RunKind;
  /** The worked issue for an issue run; null for a ci_fix run (no issue). */
  issue_iid: number | null;
  /** Snapshotted at queue time so the run is self-contained (PRD §Schema). For a
   *  ci_fix run these carry a synthesized summary, not a real issue. */
  issue_title: string;
  issue_description: string;
  /** The failed-pipeline snapshot for a ci_fix run (PRD #6): what the agent
   *  diagnoses + fixes. Present only for kind="ci_fix". Log tails are UNTRUSTED
   *  data — quoted evidence, never instructions. */
  pipeline?: ClaimPipeline | null;
  repo: ClaimRepo;
  secrets: ClaimSecrets;
  /** Existing branch on resume/attach; usually `agent/issue-{iid}`. */
  branch?: string | null;
  /** SDK session to resume (M3); null/absent for a fresh run. */
  session_id?: string | null;
  /** High-water mark of run_messages.seq; the worker continues numbering here. */
  last_seq: number;
  /** Structured PRD #3 templates — the lead plus any subagents — consumed
   *  programmatically by M3 (mapped to SDK AgentDefinitions). M2 ignores them. */
  agents: AgentTemplate[];
  config?: ClaimConfig | null;
  /** The per-run skill union (PRD #16): every skill allocated to any template for
   *  this run's owner, deduped, precedence-resolved (user > global > builtin), and
   *  capped. The worker builds a local plugin dir from these and passes their names
   *  as the SDK's explicit top-level enable-list (M4). Always sent (possibly empty). */
  skills?: ClaimSkill[];
  /** Skills dropped during assembly (shadowed or over the cap), for the worker to
   *  log (PRD #16). Always sent (possibly empty). */
  skills_dropped?: ClaimSkillDrop[];
  /** Autopilot run (PRD #19): the worker resolves the plan gate with an approve
   *  verdict instead of parking at awaiting_approval. Top-level (read from the
   *  runs row), NOT inside config — config is instance caps, this is a per-run
   *  fact. Re-delivered on every resume/requeue of the same run (the server reads
   *  it from the row), so an unattended resume never hangs at the gate. */
  auto_approve?: boolean;
  /** PRD #88: the clarification question this run is ALREADY parked on, read from the
   *  runs row and so re-delivered on every resume — the same shape as auto_approve and
   *  for the same reason. A resumed worker re-parks with this SAME id rather than
   *  minting a fresh one, which is what lets an answer the user submitted before the
   *  worker died still name the open question and be honoured. Absent when not parked. */
  open_question_id?: string | null;
  /** The run this JUDGE run reviews (PRD #46). Present only for kind="judge"; the
   *  worker fetches that run's trace via GET /worker/runs/{target_run_id}/trace. */
  target_run_id?: string | null;
  /** The model a judge run runs on (PRD #46), resolved from the judge_model setting.
   *  Present only for kind="judge". */
  judge_model?: string | null;
  /** The model the inline run-summary generator runs on (PRD #362), resolved
   *  user-value-wins from users.summary_model over the instance summary_model at
   *  ISSUE-run claim assembly. Rides the issue-run claim (unlike judge_model, which
   *  is judge-only); absent on older servers. */
  summary_model?: string | null;
  /** True when the run's INTENT summary is already set (PRD #362 M3c, Decision 3):
   *  the executor then skips intent generation on a resume/re-claim so the owner's
   *  token is not re-spent. Absent/false on a fresh run and on older servers. */
  summary_intent_present?: boolean;
  /** The API's deterministic command-not-found pre-scan of the reviewed run's tool
   *  output (PRD #46 Decision 4). Present only for kind="judge" (omitted when empty).
   *  The judge interprets it; if the model call fails it is the fallback. */
  judge_signal?: JudgeSignal | null;
  /** The reviewed run's TRUSTED failure ORIGIN — the runs.fail_origin closed-enum VALUE
   *  ONLY (never failure_reason free text), computed API-side from status + fail_origin
   *  (PRD #69 M7a Pass B). Present only for kind="judge"; null when the reviewed run did
   *  not fail with a recognised origin. The judge weighs the class, e.g. a
   *  policy/config-denied class is not retryable. */
  failure_class?: string | null;
  /** The run owner's existing improve_uzi target coordinates (issue #232), delivered on
   *  a judge claim so the judge reuses a matching target string verbatim instead of
   *  inventing a new phrasing — future recurrences then land on the same exact key the
   *  server's cross-run dedup collapses. Absent/empty for a user with no improve_uzi
   *  history. UNTRUSTED-posture data (it is the user's own prior targets, but the judge
   *  runs toolless over untrusted traces): rendered nonce-fenced, never as instructions. */
  known_improve_uzi_targets?: string[];
  /** Best-effort list of work already IN FLIGHT on the same repo at claim time
   *  (issue #297), delivered ONLY on a self_improve claim so the picker skips a
   *  recommendation whose fix another active run is already doing. Each entry is one
   *  compact coordinate line (issue iid + title + kind/status + frozen milestone
   *  titles). Absent/empty when nothing overlapping is in flight. UNTRUSTED CONTENT —
   *  the titles are issue/milestone text anyone who can file an issue can influence —
   *  rendered nonce-fenced by the worker, never as instructions. */
  inflight_targets?: string[];
  /** The plan already captured for this run (PRD #35 Decision 6b). The server has
   *  always sent this (`ClaimPayload.PlanMd`); it was simply undeclared here while
   *  nothing read it. A resumed run whose plan was already approved replays THIS
   *  text instead of re-planning, so the field is now load-bearing.
   *  Null on a fresh run and on any run that never reached the gate. */
  plan_md?: string | null;
  /** The FROZEN milestone list for this run (PRD #122 M1, Decision 11), carried so a
   *  future resume's planning prompt can name what is already committed. Null/absent
   *  when the approved plan had no milestones. Additive + optional; nothing consumes
   *  it in M1 — the field is declared now so the wire contract is complete. */
  milestones?: Milestone[] | null;
  /** Whether this run's plan is already approved (PRD #35 Decision 6b), derived
   *  SERVER-side as "a consumed approve_plan input exists for the run, OR the run is
   *  autopilot". On a resume with a resumable session this lets the worker skip the
   *  Phase-1 planning turn and the gate entirely: without it a park-and-resume
   *  re-generates a plan, re-parks the run at awaiting_approval for a human who
   *  already approved one, and can fail outright with REASON_NO_PLAN when the resumed
   *  session declines to re-emit signal_plan.
   *  Absent on an older server ⇒ treat as false, i.e. plan as today. */
  plan_approved?: boolean;
  /** How this run's plan_md was produced (PRD #209 D4). "seeded" means the user
   *  supplied the plan at create time, so the run is approved with NO server-side
   *  approve_plan input and (on a fresh run) NO SDK session — the worker implements it
   *  directly, skipping the planning turn and the gate (D4 row 2). "agent" (the
   *  DEFAULT, and how an older server that omits the field is treated) means the plan
   *  came from the worker's own Phase-1 turn, leaving the session/approval discriminator
   *  unchanged. Read alongside plan_approved: the server sets plan_approved true for a
   *  seeded run through its third disjunct (D8), which is the mechanism that makes row 2
   *  reachable at all. */
  plan_source?: PlanSource;
  /** The commit a SEEDED plan was written against (PRD #209 M4), forwarded so the worker
   *  can compare it to the clone's resolved base after checkout. Present only for a seeded
   *  run created with --planned-commit; absent (or null) ⇒ no planned commit, so the
   *  staleness compare is inert and the run proceeds silently. Read from the runs row, so
   *  it re-delivers unchanged on every resume. */
  planned_base_commit?: string | null;
  /** Whether a base-commit divergence should FAIL the run rather than warn into the feed
   *  (PRD #209 M4 Open Question 3, `--require-base`). Meaningful only alongside
   *  planned_base_commit. Absent on an older server ⇒ false, i.e. warn-and-implement. */
  require_base_match?: boolean;
  /** The run's PERSISTED subagent selection (`runs.agent_source` /
   *  `agent_exclusions`), replayed on every claim (PRD #35).
   *
   *  It ships because `plan_approved` ships, and the two come from ONE human verdict.
   *  Normally the selection reaches the worker on the approve_plan verdict — but a
   *  run resumed with `plan_approved` already true HAS NO VERDICT to carry it, so
   *  without this the worker falls through to resolveAgentSelection's "absent"
   *  default and a human who excluded a subagent at the gate gets it back after a
   *  park, silently. Decision 6b's premise is "we may skip the gate BECAUSE the human
   *  approved"; honouring the approval while dropping the exclusions that were part
   *  of the same decision honours half a verdict.
   *
   *  ABSENT — not an empty selection — when the run never reached a gate. The
   *  worker's absent-default is correct there and must stay reachable, so treat a
   *  missing key as "absent", never as "source own with no exclusions". Also absent
   *  from an older server, which is the same case and wants the same handling. */
  agent_selection?: AgentSelection;
  /** This run's usage-limit opt-in (PRD #35 Decision 7), read from the runs row on
   *  EVERY claim so a resumed or re-queued run keeps it — the same convention as
   *  `auto_approve` above, and for the same reason: an unattended resume must not
   *  silently lose the behaviour the user chose. Absent on an older server ⇒ false,
   *  i.e. a limit death fails the run as it does today. */
  wait_on_limit?: boolean;
}

/** One deterministic missing-executable hit (PRD #46 Decision 4). */
export interface JudgeToolMiss {
  command: string;
  evidence: string;
}

/** The command-not-found pre-scan carried in a judge claim. */
export interface JudgeSignal {
  missing_tools: JudgeToolMiss[];
}

/** The reviewed run's metadata (GET /worker/runs/{id}/trace → `target`). */
export interface JudgeTraceTarget {
  id: string;
  kind: string;
  status: string;
  issue_title: string;
  issue_description: string;
  branch: string | null;
  mr_iid: number | null;
  failure_reason: string | null;
  fix_verdict: string | null;
  plan_md: string | null;
  iteration_count: number;
  repo_agents: unknown;
}

/** One steering-log entry in the trace. */
export interface JudgeTraceInput {
  kind: string;
  body: string | null;
  created_at: string;
}

/** A page of the reviewed run's trace (GET /worker/runs/{id}/trace). Messages are
 *  UNTRUSTED (arbitrary tool output); the judge frames them as evidence. */
export interface JudgeTraceResponse {
  target: JudgeTraceTarget;
  messages: WorkerRunMessage[];
  inputs: JudgeTraceInput[];
}

/** One structured recommendation the judge posts back (PRD #46 Decision 5). */
export interface ReviewRecommendation {
  category:
    | "enable_tool"
    | "install_worker_tool"
    | "adjust_template"
    | "improve_agent"
    | "add_agent"
    | "improve_uzi"
    | "cost_efficiency";
  target: string;
  rationale: string;
  confidence?: "" | "low" | "medium" | "high";
}

/** The judge's review POST body (POST /worker/runs/{id}/review). */
export interface ReviewRequest {
  verdict: "ideal" | "ok" | "issues";
  summary: string;
  model: string;
  /** "complete" = a real LLM verdict; "failed" = the deterministic fallback. */
  status: "complete" | "failed";
  recommendations: ReviewRecommendation[];
}

/**
 * Chat claim lane payload (PRD #39, the chat lane's counterpart to ClaimResponse).
 * Pinned against M1's landed `workersvc.ChatClaimPayload` (chat.go): a narrower,
 * repo-less shape claimed via `POST /api/worker/runs/claim?lane=chat`. Carries the
 * Anthropic token ONLY — there is structurally no forge_pat/forge_username key
 * (Decision 9) and no repo (Decision 12). There is NO prompt field: the first user
 * message is a seeded `follow_up` run input the worker consumes through the normal
 * input path, uniform with every later turn. A Continue run (Decision 11) carries
 * `resume_of_run_id` and no seeded input (it parks awaiting the next message).
 */
export interface ChatClaimResponse {
  run_id: string;
  kind: "chat";
  /** Conversation display title (server-derived; may be empty). */
  title: string;
  /** Current run status at claim time (e.g. "claimed"). */
  status: string;
  /** SDK session to resume: the run's own (requeue/resume) or, for a Continue run,
   *  the resumed-from run's session (Decision 11). null for a fresh chat. */
  session_id: string | null;
  /** Set only for a Continue run, so the worker can say "continuing without prior
   *  context" honestly when the session is gone. */
  resume_of_run_id: string | null;
  /** High-water mark of run_messages.seq; the worker continues numbering here. */
  last_seq: number;
  requeue_count: number;
  secrets: ChatClaimSecrets;
  config: ChatClaimConfig;
}

/** A chat claim's secrets — the Anthropic token and NOTHING else (Decision 9). */
export interface ChatClaimSecrets {
  anthropic_oauth_token: string;
}

/** Chat lifecycle caps the server pushes down so the worker's clocks match (no
 *  drift, Decision 3). Timeouts are in SECONDS, matching the run claim convention
 *  (converted to ms at the worker use site). */
export interface ChatClaimConfig {
  idle_timeout_seconds: number;
  turn_timeout_seconds: number;
  max_turns: number;
  /** The owner's per-user default model (PRD #17); omitted when unset. */
  default_model?: string;
}

// ── Chat agent read surface (PRD #39 M3, Decision 7) ─────────────────────────
// The worker-authenticated endpoints the chat agent's uzi-tools MCP server calls to
// investigate its OWNER'S runs. Pinned against api/internal/handler/worker_chat.go.
// Every text field here is UNTRUSTED (forge/model-derived) and MUST be wrapped in the
// evidence envelope before it reaches the model (uzi-tools.ts), never fed as prose.

/** One run in the compact worker list (GET /api/worker/chat/runs). repo_path/mr_url
 *  are null for a chat run; issue_iid/branch null when absent. */
export interface WorkerRunListItem {
  id: string;
  kind: string;
  status: string;
  repo_path: string | null;
  issue_iid: number | null;
  title: string;
  branch: string | null;
  mr_url: string | null;
  failure_reason: string | null;
  created_at: string;
  updated_at: string;
}

/** Single-run detail (GET /api/worker/chat/runs/:id): the list fields plus the
 *  diagnostics the agent needs to answer "why did run X fail?". */
export interface WorkerRunDetail extends WorkerRunListItem {
  mr_state: string | null;
  stop_kind: string | null;
  fix_verdict: string | null;
  iteration_count: number;
  plan_md: string | null;
}

/** One run message page item (GET /api/worker/chat/runs/:id/messages). payload is
 *  arbitrary per kind and UNTRUSTED. */
export interface WorkerRunMessage {
  seq: number;
  kind: string;
  agent: string | null;
  /** PRD #99 subagent invocation id + task label. The API serves these on every
   *  MessageDTO (including the judge's trace of a target issue run, where two
   *  parallel same-role subagents differ only here), so the READ type declares
   *  them; optional because a pre-#99 API omits the keys entirely. */
  agent_instance?: string | null;
  agent_label?: string | null;
  payload: Record<string, unknown> | null;
  created_at: string;
}

/** A created issue proposal (POST /api/worker/runs/:id/proposals → 201). Pending
 *  until the user confirms in the browser; the worker never writes the forge. No
 *  repo_id: the worker only handles the human-readable repo_path (Decision 7). */
export interface WorkerProposal {
  id: string;
  run_id: string;
  title: string;
  description: string;
  labels: string[];
  status: string;
  created_at: string;
}

/** Request body for POST /api/worker/runs/:id/proposals (Phase-3 wire catalog).
 *  The agent sends `repo_path` — the exact string the read endpoints expose — which
 *  the server resolves to the internal id (user-scoped), so UUIDs stay off the
 *  worker. `repo_id` is accepted for back-compat (repo_path wins server-side). At
 *  least one must be set. */
export interface CreateProposalRequest {
  repo_path?: string;
  repo_id?: string;
  title: string;
  description: string;
  labels: string[];
}

/** Request body for POST /api/worker/runs/:id/findings (PRD #333 M2). The server
 *  derives (user_id, repo_id) from the run claim — the worker NEVER sends them (D2/D3).
 *  `location` is a symbol-anchored, repo-root-relative coordinate with NO line number
 *  (D3); the server canonicalises + sanitises title/description/location before storing
 *  inert text. Caps (title/description/labels/location byte bounds, MaxFindingsPerRun)
 *  are enforced server-side; the tool clamps client-side as defense-in-depth only. */
export interface ReportFindingRequest {
  title: string;
  description: string;
  location: string;
  labels?: string[];
  confidence?: string;
}

/** Writer-declared provenance for a saved memory (PRD #266 M2). `observed` = the
 *  claim is backed by something the writer can name (a tool result, command output,
 *  or a `file:line`); `inferred` = an untested deduction. Carried per-entry to the
 *  reader so an inferred claim can be individually marked "re-verify before acting"
 *  (M3), rather than diluted across the blanket advisory frame. A save omitting it
 *  defaults to `inferred` — never a hard failure (PRD #90: memory writes must not
 *  fail a run). */
export type MemoryBasis = "observed" | "inferred";

/** Request body for POST /api/worker/runs/:id/memory (PRD #90). The server derives
 *  (user_id, repo_id) from the run claim — the worker NEVER sends them (its join
 *  token is not user-scoped). Caps (title ≤200 chars, body ≤2048 bytes, ≤5 writes/
 *  run) are enforced server-side; the tool schema mirrors them client-side.
 *  `basis`/`evidence` (PRD #266 M2) are writer-declared provenance; the API does not
 *  parse them until M3 (extra JSON fields are ignored server-side, so this is
 *  back-compat). */
export interface SaveMemoryRequest {
  title: string;
  body: string;
  /** Writer-declared provenance; a save omitting it is defaulted to `inferred`. */
  basis: MemoryBasis;
  /** Optional short pointer to what backs an `observed` claim (a tool result,
   *  command output, or a `file:line`). Byte-capped, normalized to undefined when
   *  empty. */
  evidence?: string;
}

/** One cross-run memory entry (PRD #90). The write endpoint returns id/title/body/
 *  created_at; the read endpoint also carries run_id provenance plus writer-declared
 *  `basis`/`evidence` (PRD #266 M3). The read endpoint always populates `basis`; both
 *  are optional on the type for back-compat with legacy rows and pre-M3 responses (a
 *  missing `basis` is treated as `inferred` by the reader). */
export interface MemoryEntry {
  id: string;
  title: string;
  body: string;
  /** Provenance: the run that saved it. Present on the read surface, absent on the
   *  write response. */
  run_id?: string;
  created_at: string;
  /** Writer-declared provenance (PRD #266 M3). The read endpoint always sets it;
   *  absent only on legacy rows / pre-M3 responses, where the reader treats it as
   *  `inferred`. */
  basis?: MemoryBasis;
  /** Optional short pointer to what backs an `observed` claim. */
  evidence?: string;
}

/** Response for GET /api/worker/runs/:id/memory (PRD #90): the run's (user, repo)
 *  memory, newest first. UNTRUSTED — composed into the lead prompt nonce-fenced. */
export interface MemoryListResponse {
  memories?: MemoryEntry[];
}

// ── Forge read tools (PRD #158) ────────────────────────────────────────────────
// Response shapes for the six worker-mediated forge READ endpoints the run-lane
// forge MCP server (forge-tools.ts) calls via WorkerClient. Field names are EXACT
// snake_case as the uzi API sends them (the API is built to this wire contract in
// parallel). Every field is forge data — UNTRUSTED; the forge server nonce-fences it.

/** One issue's detail (GET /worker/runs/:id/forge/issues/:iid). `description` may be
 *  truncated by the API, flagged by `description_truncated`. */
export interface IssueDTO {
  iid: number;
  title: string;
  state: string;
  labels: string[];
  author: string;
  updated_at: string;
  description: string;
  description_truncated: boolean;
}

/** A lightweight issue row in a list (no description). */
export interface IssueSummary {
  iid: number;
  title: string;
  state: string;
  labels: string[];
  author: string;
  updated_at: string;
}

/** Response for GET /worker/runs/:id/forge/issues. `truncated` marks a capped list;
 *  `returned` is the count actually included. */
export interface IssueListDTO {
  items: IssueSummary[];
  truncated: boolean;
  returned: number;
}

/** One label-add/remove event on an issue. */
export interface LabelEventDTO {
  id: number;
  action: string;
  label_name: string;
  username: string;
  created_at: string;
}

/** Response for GET /worker/runs/:id/forge/issues/:iid/label-events. */
export interface LabelEventListDTO {
  items: LabelEventDTO[];
  truncated: boolean;
  returned: number;
}

/** One merge request's state (GET /worker/runs/:id/forge/merge-requests/:iid). */
export interface MergeRequestDTO {
  iid: number;
  state: string;
}

/** One CI job within a pipeline. */
export interface JobDTO {
  id: number;
  name: string;
  stage: string;
  status: string;
}

/** Response for GET /worker/runs/:id/forge/pipelines/:pipelineId/jobs. */
export interface JobListDTO {
  items: JobDTO[];
  truncated: boolean;
  returned: number;
}

/** One pipeline's summary. */
export interface PipelineDTO {
  id: number;
  ref: string;
  sha: string;
  status: string;
  created_at: string;
  updated_at: string;
}

/** Response for GET /worker/runs/:id/forge/latest-pipeline. `pipeline` is null when
 *  no pipeline matches the ref/mr_iid selector. */
export interface LatestPipelineDTO {
  pipeline: PipelineDTO | null;
}

/** One appended message; the server is idempotent on (run_id, seq). */
export interface OutgoingMessage {
  seq: number;
  kind: MessageKind;
  /** Which (sub)agent produced it (lead|coder|reviewer|…); worker for infra. */
  agent?: string;
  /** Which INVOCATION of that agent produced it (PRD #99) — the SDK's per-frame
   *  `parent_tool_use_id`. Absent when the frame carries no `parent_tool_use_id`
   *  -- NOT the same as `agent === "lead"`, since a repo agent may be NAMED lead
   *  and is a real subagent. Two parallel
   *  same-role subagents differ only here, so the pane can lane them apart. */
  agent_instance?: string;
  /** What that invocation was asked to do (PRD #99) — the SDK's per-frame
   *  `task_description`. Absent whenever the SDK frame omits that field. */
  agent_label?: string;
  payload: Record<string, unknown>;
}

export interface MessagesRequest {
  messages: OutgoingMessage[];
}

/**
 * One agent detected in the cloned repo's `.claude/agents/` (PRD #37). Names and
 * descriptions ONLY: the prompt bodies never leave the worker — the API stores a
 * roster the plan gate can render, not the untrusted prompts themselves.
 */
export interface RepoAgentSummary {
  name: string;
  description: string;
}

/** Which roster a run's SUBAGENTS come from (PRD #37 Decision 4: either/or, no
 *  mixing). The `lead` orchestrator is always uzi's builtin and is never
 *  selectable, under either source. */
export type AgentSource = "repo" | "own";

/**
 * The per-run subagent selection (PRD #37). `exclusions` are names removed from
 * the chosen source's roster; at least one subagent must survive (enforced by the
 * UI and re-validated API-side in M2 — the worker treats this as a request, not
 * as authority).
 *
 * It travels on two paths, both landing in the same `runs` columns:
 *   - human gate: JSON-encoded into the `approve_plan` UserInput `body`;
 *   - autopilot: on the worker's own `running` state report (Decision 6), since
 *     a self-approved run never receives a SubmitInput.
 */
export interface AgentSelection {
  source: AgentSource;
  exclusions: string[];
}

/** Cap on `exclusions`. Twice the repo-agent file cap, so excluding the whole of
 *  either roster still fits (the user's own template list is not file-capped). */
export const AGENT_EXCLUSIONS_MAX = 32;

/** JSON-encode a selection for the `approve_plan` input body. */
export function encodeAgentSelection(selection: AgentSelection): string {
  return JSON.stringify({
    source: selection.source,
    exclusions: selection.exclusions,
  });
}

/**
 * The outcome of parsing an `approve_plan` body — three cases the caller MUST keep
 * apart (PRD #37, ↳review B2/F5):
 *   - "absent":  no body was sent (autopilot, Slack today, or an older client).
 *                The run uses its DEFAULT source, which may be the detected repo
 *                roster — an absence is not a signal to distrust.
 *   - "invalid": a body WAS sent but is malformed. It must NEVER resolve toward the
 *                untrusted repo source; the caller forces `own` and notes it.
 *   - "ok":      a well-formed selection.
 * Folding "invalid" into "absent" (as returning `undefined` for both did) would let
 * `{"source":"own","exclusions":"oops"}` silently fall back to the repo default — the
 * exact "fall back toward the untrusted source" bug the review flagged.
 */
export type AgentSelectionParse =
  | { status: "absent" }
  | { status: "invalid" }
  | { status: "ok"; selection: AgentSelection };

/**
 * Parse + validate an `approve_plan` body. Absent/blank → "absent"; anything sent
 * but not a well-formed selection (non-JSON, unknown source, malformed/over-cap
 * exclusions) → "invalid". Exclusion names are held to AGENT_NAME_RE so a body can
 * carry nothing but identities; membership (⊂ the chosen roster) is the API's check
 * (and M3 re-clamps worker-side), not this one.
 */
export function parseAgentSelection(
  raw: string | null | undefined,
): AgentSelectionParse {
  if (typeof raw !== "string" || raw.trim() === "") return { status: "absent" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { status: "invalid" };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed))
    return { status: "invalid" };

  const { source, exclusions } = parsed as {
    source?: unknown;
    exclusions?: unknown;
  };
  if (source !== "repo" && source !== "own") return { status: "invalid" };

  if (exclusions === undefined || exclusions === null)
    return { status: "ok", selection: { source, exclusions: [] } };
  if (!Array.isArray(exclusions) || exclusions.length > AGENT_EXCLUSIONS_MAX)
    return { status: "invalid" };

  const out: string[] = [];
  for (const e of exclusions) {
    if (typeof e !== "string") return { status: "invalid" };
    const name = e.trim();
    if (name.length > AGENT_NAME_MAX_LEN || !AGENT_NAME_RE.test(name))
      return { status: "invalid" };
    if (!out.includes(name)) out.push(name);
  }
  return { status: "ok", selection: { source, exclusions: out } };
}

/**
 * Resolve a parsed `approve_plan` body to the selection the run will use, applying
 * the fallback policy (PRD #37 ↳review B2/F5). `repoAvailable` is whether the run
 * detected a non-empty repo roster:
 *   - ok      → the parsed selection, as sent.
 *   - invalid → FORCE `own` (never the repo source), plus a note for the feed. A
 *               malformed selection must not be able to activate attacker-authored
 *               repo agents.
 *   - absent  → the run default: the repo roster when one was detected, else `own`.
 * M3 consumes this at the gate boundary; the note (when present) is emitted to the
 * run stream so the choice is visible.
 */
export function resolveAgentSelection(
  parse: AgentSelectionParse,
  repoAvailable: boolean,
): { selection: AgentSelection; note?: string } {
  switch (parse.status) {
    case "ok":
      return { selection: parse.selection };
    case "invalid":
      return {
        selection: { source: "own", exclusions: [] },
        note: "the submitted agent selection was malformed; using your own agent templates",
      };
    case "absent":
      return {
        selection: { source: repoAvailable ? "repo" : "own", exclusions: [] },
      };
  }
}

/** Body of POST /runs/:id/state. Fields are set per target status.
 *
 *  NOTE FOR ANYONE GREPPING THIS FILE: `plan_md` is declared on FOUR separate
 *  interfaces here (this one, `ClaimResponse`, `WorkerRunDetail`, `JudgeTraceTarget`).
 *  A bare grep for the symbol returns four hits with no indication of which owns
 *  which line, and that ambiguity has already produced one wrong citation acted on by
 *  three people — `protocol.ts:451` was read as the claim payload when it is
 *  `WorkerRunDetail`. Scope to the enclosing interface before concluding anything. */
/**
 * PRD #122 M8 — response to `POST /api/worker/runs/{id}/publish`, the brokered origin
 * publish of a checkpoint pack. The REQUEST has no JSON type: the packfile is the raw
 * `application/octet-stream` body and the checkpoint tip OID travels in the
 * `X-Uzi-Checkpoint-Tip: <40-hex-lowercase-sha>` header. On success `published` is true
 * and `ref` names the landed `refs/uzi-checkpoints/<branch>`; a best-effort skip sets
 * `published` false and carries `skipped`. Any non-2xx is ignored by the worker — a
 * publish failure never fails the run.
 */
export interface PublishResponse {
  published: boolean;
  ref: string;
  skipped?: "no_ref" | "not_descendant" | "unsupported";
}

export interface StateRequest {
  status: RunState;
  /** awaiting_approval carries the captured plan. */
  plan_md?: string;
  /** awaiting_input carries the identity of the question being asked (PRD #88).
   *  REQUIRED on that transition — the api rejects the report without it, because a
   *  park with no question identity can never satisfy the resume guard, so the run
   *  would park and then be unresumable no matter what the user answered. */
  open_question_id?: string;
  /** completed carries the pushed branch + opened MR. */
  branch?: string;
  mr_iid?: number;
  /** The MR/PR web URL as the forge reported it (PRD #65 D8), reported on completion
   *  alongside mr_iid. Additive + optional: an old worker omits it, the server stores
   *  NULL, and the web falls back to reconstructing the URL from mr_iid (forgeUrls.ts). */
  mr_web_url?: string;
  /** The repo-relative path the lead declared its PRD moved to (PRD #72 M4),
   *  reported on completion alongside branch/mr_iid. Additive + optional and
   *  OMITTED ENTIRELY when there is none — never `null` or `""` — so an old
   *  worker's payload and a new worker's "no move" payload stay the same shape on
   *  the wire. Issue runs only; the server re-gates on `runs.kind` and validates
   *  the path, dropping it rather than failing the report. */
  prd_done_path?: string;
  /** issue #279: a report-only/evidence completion — the run's deliverable was a report,
   *  command output, or verification result with NO code change, so the worker records the
   *  findings and opens NO merge request. Sent (true) on the `completed` report of such a
   *  run, alongside `report_md`. Additive + optional and OMITTED ENTIRELY on an ordinary
   *  push+MR completion — never `false` — so an old worker's payload and a new worker's
   *  normal completion stay the same shape on the wire. Issue runs only; the server re-gates
   *  on `runs.kind`. */
  report_only?: boolean;
  /** issue #279: the lead's short findings summary from signal_done, persisted as the
   *  report body on a report-only completion (worker→api). Sent alongside `report_only`.
   *  The worker clamps length (REPORT_MD_MAX_LEN) for transport hygiene ONLY — the api is
   *  the authoritative control and validates/scrubs/clamps it. Additive + optional and
   *  OMITTED ENTIRELY when the lead declared no summary. */
  report_md?: string;
  /** failed carries a human-readable reason. */
  failure_reason?: string;
  /** implement⇄review loop counter, reported on running reports (M4). The
   *  server persists it with GREATEST, so a resume never regresses it. */
  iteration_count?: number;
  /** Pinned mid-flight so a resume has the SDK session (M3). */
  session_id?: string;
  /** A ci_fix run's outbound verdict on completion (PRD #6): only "not_code" (the
   *  agent judged the failure not a code problem — the run completes with the
   *  diagnosis and no MR). verified/fix_failed are stamped server-side by the
   *  pipeline sync, never reported here; an issue run omits this. */
  fix_verdict?: FixVerdict;
  /** The roster detected in the clone's `.claude/agents/` (PRD #37 Decision 6).
   *  Sent on the first `running` report AFTER checkout — on the state report, not
   *  the gate, so an autopilot run (which never reports awaiting_approval) records
   *  its roster too. Always sent once detection ran, possibly `[]`: an empty array
   *  means "detected nothing" (the gate's repo card goes inert), while an absent
   *  field/NULL column means "pre-feature run". */
  repo_agents?: RepoAgentSummary[];
  /** The selection an AUTOPILOT run resolved for itself (PRD #37 Decision 6):
   *  repo agents when detected, else the owner's templates, with no exclusions.
   *  Human-gated runs persist their selection through the `approve_plan` input
   *  instead, so this is absent there. */
  agent_selection?: AgentSelection;
  /** The plan's milestone breakdown (PRD #122 M1, Decision 2). Additive + optional
   *  and OMITTED ENTIRELY when absent — never `null` or `[]` — so an old worker's
   *  payload and a new worker's "no milestones" payload stay the same shape on the
   *  wire. Sent on exactly two reports: the CANDIDATE list on the `awaiting_approval`
   *  report (what the human approves at the gate), and the FROZEN list on the
   *  autopilot `running` report (an autopilot run never reports awaiting_approval).
   *  Never sent on any other report in M1. Reporting is fire-and-forget: the server
   *  validates the list (Decision 12) and an informational field never fails a run. */
  milestones?: Milestone[];
  /** The lead's live milestone progress (PRD #122 M2, Decision 3). Additive + optional
   *  and OMITTED ENTIRELY when the lead has reported no progress — never `null` or `[]` —
   *  so an old worker's payload and a new worker's "no progress" payload stay the same
   *  shape on the wire. Sent on `running` reports, usually alongside `iteration_count`,
   *  when a `report_progress` signal has been observed — EXCEPT the PRD #122 M6 checkpoint
   *  report, which sends these WITHOUT `iteration_count` (a checkpoint is not an iteration
   *  boundary, so omitting it keeps the GREATEST-merged counter from moving). The two fields
   *  are independent on the wire, so a consumer must not assume they co-occur.
   *  `milestones_completed` is unioned
   *  server-side (monotone); `milestones_in_progress` is a snapshot the server overwrites.
   *  PRD #265 M1: `milestones_completed` ALSO rides the terminal `completed` report — the
   *  lead's signal_done declaration of what it finished — where the server unions it into
   *  the tracker (the same monotone union) so a run that never reported mid-run progress
   *  still reconciles at completion. `milestones_in_progress` is never sent on a terminal
   *  report (it is meaningless there; the server clears it on every terminal transition).
   *  Reporting is fire-and-forget: the server validates membership + shape (Decision 12)
   *  and an informational field never fails a run. */
  milestones_completed?: string[];
  milestones_in_progress?: string[];
  /** limit_wait (PRD #35): the epoch at which the exhausted Anthropic usage window
   *  reopens, taken from the SDK's `SDKRateLimitInfo.resetsAt`. That field is a bare
   *  `number` in the typings with no unit declared, so the WORKER normalizes it
   *  (< 10^12 ⇒ seconds ⇒ ×1000) before sending and the server re-validates rather
   *  than trusting the normalization. Absent when the frames carried no usable
   *  reset — the server then falls back to its exponential park schedule. */
  limit_resets_at?: number;
  /** limit_wait (PRD #35): the SDK's `rateLimitType` verbatim, e.g. "five_hour".
   *  Sent unvalidated ON PURPOSE — the server allowlists it against the SDK union
   *  and coerces anything else to "unknown" before it reaches the DB, the DTO, the
   *  feed or Slack. Doing the allowlisting here as well would put the authoritative
   *  copy of the vocabulary on the untrusted side. */
  rate_limit_type?: string;
  /** failed (PRD #69 M7a): the worker's structured guess at WHY this run is failing,
   *  mapped from the known reason CONSTANT at the report site — e.g.
   *  "provisioning_failed", "credential_unavailable" or "rate_limited". Additive +
   *  optional and OMITTED for an ordinary agent failure (the server then defaults it
   *  to 'agent_failure'). Sent UNVALIDATED on purpose, exactly like `rate_limit_type`:
   *  the server allowlists it against its own closed enum and drops anything else,
   *  so the authoritative copy of the vocabulary stays on the trusted side. */
  fail_origin?: string;
}

/**
 * What the server answered a state report with (PRD #35's park acknowledgement
 * contract). Both the 200 and the 409 path return `{"run": <RunDTO>}`, so the run's
 * REAL status has always been on the wire; `reportState` used to discard it.
 *
 * 🔴 THE PARK DECISION KEYS ON `status`, NEVER ON `applied`. A caller deciding
 * whether to preserve on-disk state must test `status === "limit_wait"` positively.
 * `applied` is diagnostics only, and using it here is a trap with a live failure
 * mode: "not parked" has five causes, and on THREE of them — the retry budget
 * exhausted, the RUN_LIMIT_MAX_PARK clamp exceeded, and the `wait_on_limit=false`
 * coercion — the server *fails the run and answers 200*, so `applied` is **true**.
 * Those three are the designed terminal paths for a run that keeps hitting limits,
 * i.e. the exact population this feature serves, so an `applied`-keyed branch leaks
 * the clone, the plugin dir and up to ~170 MB of run HOME on the most common cause
 * of all.
 *
 * Testing one literal positively is also why there is no enumeration of the five
 * causes: the default arm becomes "the run is not parked", so an unforeseen cause, a
 * future status, an older server and a parse failure all land on the safe side by
 * construction. An enumeration would go stale; this cannot.
 */
export interface StateAck {
  /** Whether the server applied the transition. Diagnostics and logging only —
   *  see the warning above before branching on it. */
  applied: boolean;
  /** The run's authoritative status after the report, as the server reported it.
   *  A plain string, NOT `RunState`: `RunState` is what a worker may REPORT, while
   *  this is any status a run may hold ("queued", "cancelled", …).
   *  `undefined` when the server's answer carried no readable status — an older
   *  server, an unparseable body, or a 4xx recognised as already-terminal by its
   *  text. Undefined must always be treated as "not parked". */
  status?: string;
  /** The server-computed effective per-run budget (PRD #122 M2, Decisions 5/5b), read
   *  off the SAME `{run: RunDTO}` body as `status`. `budgetMaxIterations` is the scaled
   *  total implement/review turn ceiling; `budgetWallSeconds` is the effective wall clock
   *  the sweeper enforces. Both are the mechanism by which a FRESH run (frozen mid-run at
   *  plan-approval, after its claim was already issued) learns its scaled budget — a
   *  resume gets the same numbers on the claim config instead. Null/absent (older server,
   *  a run with no milestones, an unparseable body, a non-numeric value) ⇒ leave the
   *  worker's current budget unchanged. */
  budgetMaxIterations?: number;
  budgetWallSeconds?: number;
}

export interface UserInput {
  id: number;
  kind: InputKind;
  /** Per kind: `follow_up` carries the user's message, `reject_plan` the reason,
   *  `cancel` nothing — and `approve_plan` carries the JSON-encoded AgentSelection
   *  (PRD #37). Parse it with parseAgentSelection + resolveAgentSelection: an ABSENT
   *  body uses the run default (repo if detected), but a malformed one resolves to
   *  `own`, never the untrusted repo source. */
  body?: string | null;
  created_at?: string;
}

export interface InputsResponse {
  inputs: UserInput[];
}
