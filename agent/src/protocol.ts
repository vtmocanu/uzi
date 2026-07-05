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

export interface RegisterRequest {
  name: string;
  version: string;
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
}

/**
 * Response body of a successful (200) claim.
 *
 * The server also sends top-level run fields the worker does not consume in M2
 * (status, iteration_count, requeue_count, plan_md); they are ignored here.
 */
export interface ClaimResponse {
  run_id: string;
  issue_iid: number;
  /** Snapshotted at queue time so the run is self-contained (PRD §Schema). */
  issue_title: string;
  issue_description: string;
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
