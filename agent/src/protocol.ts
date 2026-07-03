// Worker↔API wire contract (PRD #4 §Worker protocol).
//
// M1 (the server side) is built in parallel on a sibling branch, so these
// shapes are the worker's *assumed* contract, derived from the PRD schema and
// the multica daemon reference. Field names follow the DB column names (PRD #4
// §Schema) and snake_case JSON, matching the Go server's conventions.
//
// Every field whose exact wire name/nesting is not yet pinned by M1 is called
// out in AMBIGUITY comments and surfaced in the M2 report for reconciliation;
// parseClaim() below is deliberately lenient so a rename on the server side is a
// one-line fix here, not a reshape.

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
  // AMBIGUITY: the PRD identifies the worker by its Bearer token on every
  // subsequent call (heartbeat/claim carry no worker id in the path), so the
  // worker does not strictly need this. Captured for logging when present.
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
}

/** Repo coordinates for the clone (PRD #2 repos row). */
export interface ClaimRepo {
  id: string;
  /** GitLab WEB url of the repo (for display/links); NOT the clone target. */
  url: string;
  /** The clone target the worker clones from. Tokenless https: the PAT is
   *  supplied out-of-band via the PRIVATE-TOKEN header, never embedded in the
   *  URL (so it can't rest in the bare repo's on-disk config). */
  clone_url: string;
  default_branch?: string | null;
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

/** Per-run caps the server may push down (PRD §Configuration). Advisory in M2. */
export interface ClaimConfig {
  run_timeout_ms?: number;
  idle_timeout_ms?: number;
  max_iterations?: number;
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
  /** completed carries the pushed branch + opened MR (mr_iid arrives in M4). */
  branch?: string;
  mr_iid?: number;
  /** failed carries a human-readable reason. */
  failure_reason?: string;
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
