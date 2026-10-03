/** Worker-authored execution decisions, independent of any provider's implementation. */
export class TrustedExecutionRefusal extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "TrustedExecutionRefusal";
  }
}

// Verbatim fixed reasons from the committed execution-refusal catalogue and runner controls.
// Raw filesystem, provider and transport detail is never a reason by itself.
const FIXED_REASONS = new Set<string>([
  "codex harness received conflicting authentication inputs",
  "codex provider root is not launched",
  "codex harness is closed",
  "codex provider root is unavailable after launch",
  "codex app-server stream ended before turn completion",
  "codex provider launch admission is closed",
  "codex provider root failed registry admission",
  "codex thread/start returned no thread id",
  "codex turn/start returned no turn id",
  "runner-owned session seed requires a managed-auth provider tree",
  "kind must be provider or command",
  "ownedDataRoot must be a canonical absolute non-root path",
  "cwd must be an absolute path",
  "ownedDataRoot and cwd must be disjoint",
  "child executable must be absolute",
  "provider roots must use the pinned Codex app-server target",
  "provider envKey is invalid or reserved by the launcher allowlist",
  "app-server auth is only supported for a provider root",
  "app-server auth requires an explicit subscription or api_key mode",
  "Codex loopback test base URL requires app-server auth",
  "Codex session seeding requires a managed-auth provider root",
  "effect command and cwd must be absolute paths",
  "effect cleanup token must be a lowercase UUID",
  "malformed evidence line (not JSON)",
  "evidence stream closed before confirmed disposal",
  "evidence line is not a JSON object",
  "malformed tmpCleanup evidence",
  "malformed or duplicate child_exit evidence",
  "supervisor channels closed with an unanswered control request",
  "supervisor has exited; snapshot unavailable",
  "Codex production config received an unsupported option",
  "Codex production config requires an explicit supported auth mode",
  "Codex production config codeModeHost must be a boolean",
  "Codex loopback test base URL is invalid",
  "Codex loopback test base URL must be http://127.0.0.1:<port>/v1 with no credentials or query",
  "fileop helper process is missing a stdin/stdout channel",
  "boundary process deadline exceeded",
  "codex boundary process refused: stale permit",
  "codex boundary process refused: boundary deadline exceeded",
  "codex boundary process timeout must be positive and finite",
  "recoverable child timeout is checkpoint-only",
  "codex boundary process refused: admission not closed",
  "codex boundary process refused: command roots live",
  "boundary process failed registry admission",
  "boundary process child deadline exceeded during launch",
  "checkpoint child disposal failed",
  "checkpoint child root did not reap cleanly before boundary deadline",
  "boundary process root did not reap cleanly",
  "boundary process produced no terminal result",
  "codex app-server refresh bridge requires a subscription binding",
  "codex app-server refresh has no observed generation",
  "Codex shared data directory has an unexpected owner or group",
  "Codex shared data directory has an unexpected mode",
  "Codex run HOME must be a canonical absolute non-root path",
  "Codex run HOME must be prepared by the worker identity",
  "codex clarification rounds exhausted during planning",
  "codex plan turn produced no plan",
  "codex plan revision budget exhausted",
  "codex plan turn produced no plan on revision",
  "codex clarification rounds exhausted during implementation",
  "codex implement request built without a resolved agent selection",
  "Codex command worktree lacks the required runner-group setgid/write posture",
  "command cwd escapes the worktree sandbox",
  "command private tmp must be an absolute non-root path",
  "command cache must be a clean child of the command cache root",
  "codex provider root is missing a stdio transport channel",
  "plan gate is not wired for this run",
  "codex transport closed",
  "codex transport could not serialize outbound frame",
  "codex transport inbound frame exceeds size cap",
  "codex transport inbound notification buffer overflow",
  "codex transport inbound notification byte overflow",
  "codex transport is closed",
  "codex transport notifications() is single-consumer",
  "codex transport outbound byte queue is full",
  "codex transport outbound frame exceeds size cap",
  "codex transport outbound queue is full",
  "codex transport read stream error",
  "codex transport received a non-object frame",
  "codex transport received an unparseable frame",
  "codex transport request aborted",
  "codex transport request aborted before send",
  "codex transport request deadline exceeded",
  "codex transport server-request interceptor failed",
  "codex transport server-request interceptor is already installed",
  "codex transport stream closed mid-request",
  "codex transport write stream error",
  "claude harness: prepareTurn was not called for this turn",
  "run exceeded its wall-clock timeout",
  "run stalled: no agent activity within the idle timeout",
  "run cancelled",
  "run paused (now)",
  "run released for a credential switch",
  "no Anthropic OAuth token was provided for this run",
  "the agent ended the planning turn without submitting a plan",
  "the agent kept asking clarifying questions without ever submitting a plan",
  "run reached its milestone-scaled implement/review iteration budget without completing",
  "the lead declined the task and made no progress: it repeated the same response with no new commits, no working-tree changes, and no subagent activity across consecutive iterations. Stopped early rather than exhausting the iteration budget; see the lead's response on the run feed.",
  "codex run idle timeout",
  "codex run wall-clock timeout",
  "codex run paused",
  "codex run deferred: vault locked",
  "codex run reached its implement/review iteration budget without completing",
  "completion blocked: the lead declared the run complete but the frozen completion contract still has unmet milestones, and repeated completion attempts made no progress (the same unmet set, branch head and worktree across attempts). Held for an owner decision rather than shipping an incomplete run.",
  "completion budget exhausted: the server flagged this run past its completion budget after a completion attempt. Held for an owner decision rather than continuing to spend budget.",
  "denied by guardrail: git push is not permitted (the worker opens MRs; the agent never pushes)",
  "denied by guardrail: git remote mutation is not permitted",
  "denied by guardrail: forced git operations are not permitted",
  "denied by guardrail: reading git config values is not permitted",
  "denied by guardrail: modifying remote/core/http/credential git config is not permitted",
  "denied by guardrail: reading the process environment is not permitted",
  "denied by guardrail: inspecting the process table is not permitted",
  "denied by guardrail: reading /proc is not permitted",
  "denied by guardrail: mass-signal kill commands (pkill, killall, skill, fuser -k, kill of a broadcast/process-group target, or kill of PIDs enumerated by lsof/pgrep/ps/fuser/pidof) can kill the agent's own process tree; stop a background task through the harness, or kill \"$pid\" with the exact PID saved at launch (run it as its own command, without lsof/pgrep/ps/fuser/pidof in the same command)",
  "denied by guardrail: reading the worker credential file is not permitted",
  "denied by guardrail: docker requires a daemon sidecar, which this worker has none wired",
  "denied by guardrail: redirecting the docker client to a different daemon is not permitted",
  "denied by guardrail: command wrapping is nested too deeply to screen safely",
  "denied by guardrail: command screening failed; refusing to run Bash",
  "denied by guardrail: file access outside the run worktree is not permitted; use .uzi/scratch/ inside the run worktree for temporary files",
  "denied by guardrail: accessing the .git directory is not permitted",
  "denied by guardrail: only the run's assembled subagents may be invoked",
  "denied by guardrail: this dispatch has no text prompt to carry the run's operator constraints",
  "denied by guardrail: operator constraints could not be loaded; retry the run",
  "denied by guardrail: the run's operator constraints are too large to attach to a subagent; shorten or consolidate them",
  "Codex claim block is present but is not an object",
  "Codex claim block has a missing or unrecognized auth_mode",
  "Codex claim block is missing a valid access_token",
  "Codex claim block is missing a valid capability",
  "Codex subscription claim is missing its generation",
  "Codex subscription claim generation is not a finite number",
  "Codex subscription claim generation is not a safe integer",
  "Codex subscription claim generation must be nonnegative",
  "Codex subscription claim is missing its verified account id",
  "Codex subscription claim plan type must be null",
  "Codex api_key claim must not carry subscription metadata",
  "boundary process executable must be absolute",
  "boundary process launch unavailable",
  "boundary process spawn failed",
  "clarification timed out",
  "plan approval timed out",
  "the planning turn ended without a plan or a structured question after a corrective nudge",
  "evidence line budget exceeded (> 256)",
  "oversized evidence line (> 65536 bytes)",
  "control frame exceeds 8192 bytes",
  "control write failure (channel unavailable)",
  "supervisorBin must equal the immutable image path /usr/local/bin/uzi-codex-supervisor",
]);

// Every family is anchored to its complete producer grammar. Limits and labels are
// concrete launcher domains; details belong only to independently identifiable wrappers.
const REASON_FAMILIES = [
  /^(?:provider|command|boundary_action) (?:launch admission is closed|root failed registry admission)$/,
  /^supervisor (?:control|evidence) channel \(fd\) is unavailable$/,
  /^supervised provider child exited unexpectedly \(code=(?:\d|[1-9]\d|1\d\d|2[0-4]\d|25[0-5])\)$/,
  /^supervisor exited \(code=(?:null|-?\d+), signal=(?:null|SIG(?:ABRT|ALRM|BUS|CHLD|CONT|FPE|HUP|ILL|INT|IO|IOT|KILL|PIPE|POLL|PROF|PWR|QUIT|SEGV|STKFLT|STOP|SYS|TERM|TRAP|TSTP|TTIN|TTOU|URG|USR1|USR2|VTALRM|WINCH|XCPU|XFSZ|BREAK|INFO|LOST|UNUSED))\) without confirmed disposal$/,
  /^expected snapshot evidence, got dispose$/,
  // Posture evidence has arbitrary String-coerced fields; parsed separately below.
  /^autopilot plan not durably stored — the run is [\s\S]*$/, // StateAck.status is a free string.
  /^could not park (?:the run to ask a question|the interactive task to await a follow-up) \(server reports [\s\S]*\)$/, // Same status domain.
  /^codex app-server returned a JSON-RPC error$/,
  // These wrappers themselves identify protocol or fatal pre-start origin. Their
  // detail is arbitrary, as at the producer; matching it never trusts a bare detail.
  /^(?:evidence stream error|control stream error|supervisor process error|supervisor abnormal|unknown supervisor evidence event|tool provisioning failed before the agent could start): [\s\S]*$/,
];

// Avoid hashing a long context wrapper at every peel; no fixed reason exceeds this.
const MAX_FIXED_REASON_LENGTH = Math.max(...[...FIXED_REASONS].map((reason) => reason.length));

function postureReason(reason: string): boolean {
  if (!reason.startsWith("supervisor reported an unsafe start posture (pid=")
    || !reason.endsWith(")") || !/ expected \d+\)$/.test(reason)) return false;
  const pid = / expectedPid=(?:\d+|undefined), subreaper=/.exec(reason);
  if (pid === null) return false;
  let cursor = pid.index + pid[0].length;
  for (const field of [", nondumpable=", ", liveCapsZero=", ", capBoundingSet=", ", noNewPrivs=", ", uid="]) {
    const next = reason.indexOf(field, cursor);
    if (next < 0) return false;
    cursor = next + field.length;
  }
  return true;
}

// UID diagnostics retain their source domains: stdout is arbitrary text but must
// actually fail parseInt's positive-integer check; numeric values use String(number).
function numericReason(reason: string): boolean {
  const invalidOutput = "runner uid resolution produced an invalid uid: ";
  if (reason.startsWith(invalidOutput)) {
    const uid = Number.parseInt(reason.slice(invalidOutput.length).trim(), 10);
    return !Number.isInteger(uid) || uid <= 0;
  }
  const uid = /^(resolved runner uid is invalid: |effect identity resolved unexpected uid )(.+)$/.exec(reason);
  if (uid !== null && uid[0] === reason) {
    const value = Number(uid[2]);
    return String(value) === uid[2]
      && (uid[1] === "effect identity resolved unexpected uid " || !Number.isInteger(value) || value <= 0);
  }
  const status = /^cannot resolve the runner uid \(id -u runner exited (null|-?\d+)\)$/.exec(reason);
  if (status !== null && status[0] === reason) {
    return status[1] === "null" || (Number.isInteger(Number(status[1]))
      && Number(status[1]) !== 0 && String(Number(status[1])) === status[1]);
  }
  const rpc = /^codex app-server returned a JSON-RPC error \(code (.+)\)$/.exec(reason);
  if (rpc !== null && rpc[0] === reason) return String(Number(rpc[1])) === rpc[1];
  const deadline = /^(?:started|snapshot|dispose|supervisor exit) deadline exceeded \((.+)ms\)$/.exec(reason);
  if (deadline !== null && deadline[0] === reason) {
    const ms = Number(deadline[1]);
    return Number.isFinite(ms) && ms >= 0 && String(ms) === deadline[1];
  }
  return false;
}

function configReason(reason: string): boolean {
  const namePrefix = "Codex provider name must match /^[A-Za-z0-9._-]+$/ (got ";
  const wirePrefix = 'Codex M3a supports only the "responses" wire (got ';
  const prefix = reason.startsWith(namePrefix) ? namePrefix : reason.startsWith(wirePrefix) ? wirePrefix : undefined;
  if (prefix === undefined || !reason.endsWith(")")) return false;
  const encoded = reason.slice(prefix.length, -1);
  try {
    const value: unknown = JSON.parse(encoded);
    return typeof value === "string" && JSON.stringify(value) === encoded
      && (prefix === namePrefix ? !/^[A-Za-z0-9._-]+$/.test(value) : value !== "responses");
  } catch {
    return false;
  }
}

/** Recognize legacy text while preserving the runner's existing control mappings.
 * Leading diagnostic contexts are peeled while each consumes text. Double quotes and
 * CR/LF prevent context peeling; apostrophes are ordinary context characters.
 * The optional predicate is the runner's existing reason/prefix policy. */
export function legacyTrustedExecutionRefusal(reason: string, existingReason?: (reason: string) => boolean): boolean {
  let candidate = reason;
  while (candidate.length > 0) {
    if ((candidate.length <= MAX_FIXED_REASON_LENGTH && FIXED_REASONS.has(candidate))
      || existingReason?.(candidate) || configReason(candidate) || numericReason(candidate) || postureReason(candidate)
      || REASON_FAMILIES.some((family) => family.exec(candidate)?.[0] === candidate)) return true;
    const context = /^[^"\r\n]+?: /.exec(candidate);
    if (context === null) return false;
    candidate = candidate.slice(context[0].length);
  }
  return false;
}
