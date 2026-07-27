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
// The terminal/non-terminal distinction is enforced where it is actually read: the
// `RunState` union above, the server's status CHECK, and `uzicli`'s own
// terminalRunStatuses. If a consumer for a Set like this ever appears, add it back
// with that consumer — not before.

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
  /** PRD #41: the user's revision feedback at the approval gate (payload
   *  `{ feedback: string }`), echoed to the feed so the revision is auditable. */
  | "plan_feedback"
  /** PRD #41: the lead is revising the plan for round N (payload `{ round: number }`);
   *  the UI derives its "revising" gate state from this. */
  | "plan_revising";

/** run_user_inputs.kind (PRD #4 §Schema; PRD #41 adds `revise_plan` — the user asks
 *  for a new plan version at the gate, body = their feedback text). */
export type InputKind = "follow_up" | "approve_plan" | "reject_plan" | "cancel" | "revise_plan";

/** runs.kind (PRD #6; PRD #46 adds "judge" and "self_improve"). self_improve is
 *  issue-shaped and runs the ordinary run lane (RunRunner), with a fixed branch and
 *  its own MR evidence (Decision 10). */
export type RunKind = "issue" | "ci_fix" | "judge" | "self_improve";

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
  /** Which forge this repo's connection speaks (PRD #65 D9). Additive and OPTIONAL:
   *  absent ⇒ "gitlab", so an old api (which never sends it) still drives a GitLab
   *  run on a new worker (R8). The worker selects its forge client from this. */
  forge_type?: "gitlab" | "forgejo";
  /** Repo owner's opt-in (PRD #16): load skills from the repo's own
   *  .claude/skills at run time. Default false. When true the worker enumerates
   *  repo skills after checkout, applies the caps, and ranks them below every
   *  delivered skill (M6). Skills only — repo hooks/settings/commands never load. */
  skills_enabled?: boolean;
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
  /** The run owner's per-user default model (PRD #17). When present it overrides
   *  the lead template's model for the main thread; absent when the owner set no
   *  default, so the worker falls back to the lead template's model. */
  default_model?: string;
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
  /** Repo devbox.json packages opt-in (PRD #18 M5): whether the worker may union
   *  the repo's own devbox.json packages (packages-only) into the provisioned set.
   *  Delivered from M3 but always false until M5 wires the per-repo toggle. */
  repo_devbox_opt_in?: boolean;
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
  /** The run this JUDGE run reviews (PRD #46). Present only for kind="judge"; the
   *  worker fetches that run's trace via GET /worker/runs/{target_run_id}/trace. */
  target_run_id?: string | null;
  /** The model a judge run runs on (PRD #46), resolved from the judge_model setting.
   *  Present only for kind="judge". */
  judge_model?: string | null;
  /** The API's deterministic command-not-found pre-scan of the reviewed run's tool
   *  output (PRD #46 Decision 4). Present only for kind="judge" (omitted when empty).
   *  The judge interprets it; if the model call fails it is the fallback. */
  judge_signal?: JudgeSignal | null;
  /** The plan already captured for this run (PRD #35 Decision 6b). The server has
   *  always sent this (`ClaimPayload.PlanMd`); it was simply undeclared here while
   *  nothing read it. A resumed run whose plan was already approved replays THIS
   *  text instead of re-planning, so the field is now load-bearing.
   *  Null on a fresh run and on any run that never reached the gate. */
  plan_md?: string | null;
  /** Whether this run's plan is already approved (PRD #35 Decision 6b), derived
   *  SERVER-side as "a consumed approve_plan input exists for the run, OR the run is
   *  autopilot". On a resume with a resumable session this lets the worker skip the
   *  Phase-1 planning turn and the gate entirely: without it a park-and-resume
   *  re-generates a plan, re-parks the run at awaiting_approval for a human who
   *  already approved one, and can fail outright with REASON_NO_PLAN when the resumed
   *  session declines to re-emit signal_plan.
   *  Absent on an older server ⇒ treat as false, i.e. plan as today. */
  plan_approved?: boolean;
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
    | "improve_uzi";
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

/** Request body for POST /api/worker/runs/:id/memory (PRD #90). The server derives
 *  (user_id, repo_id) from the run claim — the worker NEVER sends them (its join
 *  token is not user-scoped). Caps (title ≤200 chars, body ≤2048 bytes, ≤5 writes/
 *  run) are enforced server-side; the tool schema mirrors them client-side. */
export interface SaveMemoryRequest {
  title: string;
  body: string;
}

/** One cross-run memory entry (PRD #90). The write endpoint returns id/title/body/
 *  created_at; the read endpoint also carries run_id provenance. */
export interface MemoryEntry {
  id: string;
  title: string;
  body: string;
  /** Provenance: the run that saved it. Present on the read surface, absent on the
   *  write response. */
  run_id?: string;
  created_at: string;
}

/** Response for GET /api/worker/runs/:id/memory (PRD #90): the run's (user, repo)
 *  memory, newest first. UNTRUSTED — composed into the lead prompt nonce-fenced. */
export interface MemoryListResponse {
  memories?: MemoryEntry[];
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
  return JSON.stringify({ source: selection.source, exclusions: selection.exclusions });
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
export function parseAgentSelection(raw: string | null | undefined): AgentSelectionParse {
  if (typeof raw !== "string" || raw.trim() === "") return { status: "absent" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { status: "invalid" };
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return { status: "invalid" };

  const { source, exclusions } = parsed as { source?: unknown; exclusions?: unknown };
  if (source !== "repo" && source !== "own") return { status: "invalid" };

  if (exclusions === undefined || exclusions === null) return { status: "ok", selection: { source, exclusions: [] } };
  if (!Array.isArray(exclusions) || exclusions.length > AGENT_EXCLUSIONS_MAX) return { status: "invalid" };

  const out: string[] = [];
  for (const e of exclusions) {
    if (typeof e !== "string") return { status: "invalid" };
    const name = e.trim();
    if (name.length > AGENT_NAME_MAX_LEN || !AGENT_NAME_RE.test(name)) return { status: "invalid" };
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
      return { selection: { source: repoAvailable ? "repo" : "own", exclusions: [] } };
  }
}

/** Body of POST /runs/:id/state. Fields are set per target status. */
export interface StateRequest {
  status: RunState;
  /** awaiting_approval carries the captured plan. */
  plan_md?: string;
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
