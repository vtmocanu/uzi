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
  | "completed"
  | "failed";

export const TERMINAL_STATES: ReadonlySet<RunState> = new Set<RunState>([
  "completed",
  "failed",
]);

/** run_messages.kind (PRD #4 §Schema). */
export type MessageKind =
  | "text"
  | "thinking"
  | "tool_use"
  | "tool_result"
  | "status"
  | "error"
  | "user_message"
  | "plan";

/** run_user_inputs.kind (PRD #4 §Schema). */
export type InputKind = "follow_up" | "approve_plan" | "reject_plan" | "cancel";

/** runs.kind (PRD #6). */
export type RunKind = "issue" | "ci_fix";

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
}

export interface RegisterResponse {
  // The worker is identified by its Bearer token on every subsequent call
  // (heartbeat/claim carry no worker id in the path), so this is not strictly
  // needed; M1 returns it and we capture it for logging when present.
  worker_id?: string;
}

export interface HeartbeatRequest {
  version: string;
}

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
 * The server also sends top-level run fields the worker does not consume in M2
 * (status, iteration_count, requeue_count, plan_md); they are ignored here.
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
  payload: Record<string, unknown> | null;
  created_at: string;
}

/** A created issue proposal (POST /api/worker/runs/:id/proposals → 201). Pending
 *  until the user confirms in the browser; the worker never writes the forge. */
export interface WorkerProposal {
  id: string;
  run_id: string;
  repo_id: string;
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

/** One appended message; the server is idempotent on (run_id, seq). */
export interface OutgoingMessage {
  seq: number;
  kind: MessageKind;
  /** Which (sub)agent produced it (lead|coder|reviewer|…); worker for infra. */
  agent?: string;
  payload: Record<string, unknown>;
}

export interface MessagesRequest {
  messages: OutgoingMessage[];
}

/** Body of POST /runs/:id/state. Fields are set per target status. */
export interface StateRequest {
  status: RunState;
  /** awaiting_approval carries the captured plan. */
  plan_md?: string;
  /** completed carries the pushed branch + opened MR. */
  branch?: string;
  mr_iid?: number;
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
}

export interface UserInput {
  id: number;
  kind: InputKind;
  body?: string | null;
  created_at?: string;
}

export interface InputsResponse {
  inputs: UserInput[];
}
