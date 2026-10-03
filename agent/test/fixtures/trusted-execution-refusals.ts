/**
 * MR !2217 milestone 1 audit, frozen at 71911eb81852f66b6d79c91f08ae9dea864af063.
 * This is case data for the public RunRunner.execute seam, not a runtime classifier.
 * Fixed strings below are verbatim; families enumerate producer templates and legal
 * interpolation domains. No arbitrary substring/near-miss semantics are asserted.
 *
 * SDK and Codex run lanes share phasePreflightHandoff's executor.run wrapper,
 * which records the identical rejected value. Settlement failure clears eligibility. dispatchFailure then uses
 * untypedExecutionFailure / terminalDiskExcludedReason before occupancy reclaim.
 * Capture uses canonicalRecoveryInterruption in source/proof/fetch/report catches;
 * selected source and fetch cases require the refusal to escape exactly once.
 * The frozen M1 audit below is retained; milestone2Handling records current migration state.
 *
 * M2 migration: give decision-generated execution/contract refusals a trusted type,
 * retain existing canonical dispositions, and recognize bounded legacy message
 * envelopes (context: reason, repeated Error:) without trusting arbitrary I/O text.
 * Wrappers preserve their OUTER display reason; cause/interruption carry the refusal.
 */
import { TrustedExecutionRefusal } from "../../src/trusted-execution-refusal.js";

const fixedAudit = [
  {
    source: "agent/src/codex/codex-harness.ts",
    messages: ["codex harness received conflicting authentication inputs",
      "codex provider root is not launched", "codex harness is closed",
      "codex provider root is unavailable after launch",
      "codex app-server stream ended before turn completion",
      "codex provider launch admission is closed", "codex provider root failed registry admission",
      "codex thread/start returned no thread id", "codex turn/start returned no turn id"],
    caller: "ensureProvider/startTurn events → driveTurn → run; launch catch cancels reservation and rethrows; admission teardown catches disposal only",
    typed: "CodexHarnessError (already protected); message-only legacy representations tested here",
    migration: "retain existing type and canonical disposition; support legacy envelope",
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "messages": [
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
      "supervisor has exited; snapshot unavailable"
    ],
    "caller": "validateLaunchContract / launchCodexRoot / launchCodexEffectRoot / createHandle → provider launch or startProviderEpoch; launch catches rethrow; later command broker catches deny",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/codex/config.ts",
    "messages": [
      "Codex production config received an unsupported option",
      "Codex production config requires an explicit supported auth mode",
      "Codex production config codeModeHost must be a boolean",
      "Codex loopback test base URL is invalid",
      "Codex loopback test base URL must be http://127.0.0.1:<port>/v1 with no credentials or query"
    ],
    "caller": "renderProductionCodexConfig / loopback config → launcher → provider launch; harness launch catch cancels reservation and rethrows",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/codex/fileop-client.ts",
    "messages": [
      "fileop helper process is missing a stdin/stdout channel"
    ],
    "caller": "wireFileopHelper → startProviderEpoch; reapRoot then rethrow; epoch catch teardown then rethrow",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/codex/safety.ts",
    "messages": [
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
      "boundary process produced no terminal result"
    ],
    "caller": "spawnBoundaryProcess → runner capture git seam; plain errors; withBoundary also wraps failed boundary in CodexBoundaryError",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/codex/codex-executor.ts",
    "messages": [
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
      "codex provider root is missing a stdio transport channel"
    ],
    "caller": "run / startProviderEpoch / buildRunRequest / refresh bridge; turn catch rethrows Error unchanged; planning refusals escape; boundary argv failure replaced by safety spawn failed",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/sdk-executor.ts",
    "messages": [
      "plan gate is not wired for this run"
    ],
    "caller": "phasePlan gate absence → run; normal catch rethrows; both runner run lanes record executor rejection",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  },
  {
    "source": "agent/src/codex/transport.ts",
    "messages": [
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
      "codex transport write stream error"
    ],
    "caller": "fail / terminate → request or notifications rejection → CodexHarness → driveTurn; CodexTransportError typed; JSON-RPC resume failure can become CodexResumeError",
    "typed": "CodexTransportError; message-only replay intentionally loses type",
    "migration": "retain existing type and classify message-only legacy envelope"
  },
  {
    "source": "agent/src/claude-harness.ts",
    "messages": [
      "claude harness: prepareTurn was not called for this turn"
    ],
    "caller": "startTurn → SDK driveTurn; Error propagated unless a trip wins",
    "typed": "plain Error",
    "migration": "type decision-generated refusal at producer; bounded legacy reason recognition in runner"
  }
] as const;

const familyAudit = [
  {
    source: "agent/src/codex/launcher.ts",
    caller: "createHandle lines/control/child error event → fail → started rejection or whenFailed; the protocol wrapper retains recognizable origin",
    template: "evidence stream error: ${message}; control stream error: ${message}; supervisor process error: ${message}",
    domain: "native error message is arbitrary; only the worker-authored protocol wrapper identifies the failure, not its external detail",
    messages: ["evidence stream error: opaque", "control stream error: opaque", "supervisor process error: opaque"],
    typed: "plain Error",
    migration: "type the worker-authored protocol wrapper; keep raw external errors opaque outside that wrapper",
  },
  {
    "source": "agent/src/codex/codex-executor.ts",
    "caller": "launchRegisteredEffectRoot → startProviderEpoch fileop startup; cleanup then rethrow",
    "template": "${kind} launch admission is closed / ${kind} root failed registry admission",
    "domain": "RootKind = provider | command | boundary_action; current helper callers pass command only",
    "messages": [
      "command launch admission is closed",
      "command root failed registry admission"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "validateLaunchContract / launchCodexEffectRoot → startProviderEpoch or provider launch; startup rethrows",
    "template": "supervisorBin must equal the immutable image path ${SUPERVISOR_BIN}",
    "domain": "SUPERVISOR_BIN fixed image path",
    "messages": [
      "supervisorBin must equal the immutable image path /usr/local/bin/uzi-codex-supervisor"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "defaultResolveRunnerUid / launchCodexRoot; provider startup rethrows",
    "template": "cannot resolve the runner uid (id -u runner exited ${status}); runner uid resolution produced an invalid uid: ${stdout}; resolved runner uid is invalid: ${uid}; effect identity resolved unexpected uid ${uid}",
    "domain": "status number|null; stdout string; uid must be positive integer / exact identity uid",
    "messages": [
      "cannot resolve the runner uid (id -u runner exited 1)",
      "runner uid resolution produced an invalid uid: invalid",
      "resolved runner uid is invalid: 0",
      "effect identity resolved unexpected uid 0"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "createHandle channel validation → launch; fileop startup rethrows",
    "template": "supervisor ${label} channel (fd) is unavailable",
    "domain": "label = control | evidence",
    "messages": [
      "supervisor control channel (fd) is unavailable",
      "supervisor evidence channel (fd) is unavailable"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "createHandle fail → started rejection; provider whenFailed; command waitChild",
    "template": "evidence line budget exceeded (> ${MAX_EVIDENCE_LINES}); oversized evidence line (> ${MAX_EVIDENCE_LINE_BYTES} bytes); control frame exceeds ${MAX_CONTROL_BYTES} bytes",
    "domain": "fixed limits 256 / 65536 / 8192",
    "messages": [
      "evidence line budget exceeded (> 256)",
      "oversized evidence line (> 65536 bytes)",
      "control frame exceeds 8192 bytes"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "createHandle dispatch → fail → started rejection or whenFailed",
    "template": "supervisor reported an unsafe start posture (pid=${pid} expectedPid=${child.pid}, subreaper=${flag}, nondumpable=${flag}, liveCapsZero=${flag}, capBoundingSet=${caps}, noNewPrivs=${flag}, uid=${uid} expected ${expectedUid})",
    "domain": "decoded evidence fields are unknown; expected pid/uid integers; each field independently checked",
    "messages": [
      "supervisor reported an unsafe start posture (pid=12 expectedPid=12, subreaper=false, nondumpable=true, liveCapsZero=true, capBoundingSet=0x0, noNewPrivs=true, uid=10002 expected 10002)"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "createHandle dispatch → fail; launch before started escapes; later command broker converts to denial",
    "template": "supervised provider child exited unexpectedly (code=${code}); supervisor abnormal: ${reason}; unknown supervisor evidence event: ${event}; supervisor exited (code=${code}, signal=${signal}) without confirmed disposal",
    "domain": "child code integer 0..255; abnormal reason/event unknown coerced by String; exit code number|null and signal NodeJS.Signals|null",
    "messages": [
      "supervised provider child exited unexpectedly (code=1)",
      "supervisor abnormal: unknown",
      "unknown supervisor evidence event: unexpected",
      "supervisor exited (code=1, signal=null) without confirmed disposal"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/launcher.ts",
    "caller": "snapshot sendAndWait → snapshot; registry wraps as boundary error; launch failure earlier escapes",
    "template": "control write failure (channel unavailable); expected snapshot evidence, got ${event}; ${label} deadline exceeded (${ms}ms)",
    "domain": "event is snapshot|dispose; deadline labels started|snapshot|dispose|supervisor exit; milliseconds finite configured budget",
    "messages": [
      "control write failure (channel unavailable)",
      "expected snapshot evidence, got dispose",
      "started deadline exceeded (10000ms)",
      "snapshot deadline exceeded (5000ms)"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/config.ts",
    "caller": "render config → launchCodexRoot → harness launchRoot catch rethrows",
    "template": "Codex provider name must match ${PROVIDER_NAME_RE} (got ${JSON.stringify(name)}); Codex M3a supports only the \"responses\" wire (got ${JSON.stringify(wireApi)})",
    "domain": "name arbitrary string failing /^[A-Za-z0-9._-]+$/; wireApi invalid string",
    "messages": [
      "Codex provider name must match /^[A-Za-z0-9._-]+$/ (got \"invalid name\")",
      "Codex M3a supports only the \"responses\" wire (got \"chat\")"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/runner.ts",
    "caller": "gatePlan autopilot callback → both executor.run lanes",
    "template": "autopilot plan not durably stored — the run is ${ack.status ?? fallback}",
    "domain": "StateAck status domain or absent",
    "messages": [
      "autopilot plan not durably stored — the run is failed",
      "autopilot plan not durably stored — the run is no longer running"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/runner.ts",
    "caller": "askUser / awaitFollowUp callbacks → both executor.run lanes",
    "template": "could not park the run to ask a question (server reports ${status}); could not park the interactive task to await a follow-up (server reports ${status})",
    "domain": "ACK status domain|undefined; ownership 404 fixed text; terminal ownership status",
    "messages": [
      "could not park the run to ask a question (server reports failed)",
      "could not park the run to ask a question (server reports an unreadable status)",
      "could not park the interactive task to await a follow-up (server reports the run is not owned by this worker)"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/sdk-executor.ts",
    "caller": "phase plan verdict / runner gatePlan input delivery → executor.run rethrows",
    "template": "unexpected plan verdict: ${kind}; plan-gate input delivery failed: ${message}",
    "domain": "unexpected verdict kind string; definitive GateInputDeliveryError.message only",
    "messages": [
      "unexpected plan verdict: invalid",
      "plan-gate input delivery failed: refused"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/provision-run.ts",
    "caller": "provisionRunTools → both executor.run lanes",
    "template": "tool provisioning failed before the agent could start: ${detail}",
    "domain": "invalid run id or arbitrary provisioning detail; wrapper is trusted fatal pre-start disposition, not a classification of the raw I/O",
    "messages": [
      "tool provisioning failed before the agent could start: invalid run id"
    ],
    "typed": "plain Error",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  },
  {
    "source": "agent/src/codex/transport.ts",
    "caller": "jsonRpcError → request rejection → CodexHarness / driveTurn",
    "template": "codex app-server returned a JSON-RPC error${suffix}",
    "domain": "suffix built from decoded RPC code/message; typed CodexTransportError; raw provider text not independently trusted",
    "messages": [
      "codex app-server returned a JSON-RPC error"
    ],
    "typed": "CodexTransportError",
    "migration": "retain typed state where present; type only decision-generated refusals; protect legacy envelope"
  }
] as const;

// Already protected canonical reasons. Source is the consumer's exact fixed list;
// producers are SDK trip/plan/completion, Codex run/setup, select FailClosedExecutor,
// guardrails, and safety.spawnBoundaryProcess. Selection's typed error is deliberately
// flattened by FailClosedExecutor.run; these fixed security refusals must stay protected.
const protectedReasons = [
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
  "Codex shared data directory has an unexpected owner or group",
  "Codex shared data directory has an unexpected mode",
  "Codex run HOME must be a canonical absolute non-root path",
  "Codex run HOME must be prepared by the worker identity",
  "codex clarification rounds exhausted during planning",
  "codex plan turn produced no plan",
  "codex plan revision budget exhausted",
  "codex plan turn produced no plan on revision",
  "codex clarification rounds exhausted during implementation",
  "Codex command worktree lacks the required runner-group setgid/write posture",
  "command cwd escapes the worktree sandbox",
  "command private tmp must be an absolute non-root path",
  "command cache must be a clean child of the command cache root",
  "boundary process executable must be absolute",
  "boundary process launch unavailable",
  "boundary process spawn failed"
] as const;

export const trustedExecutionAudit = { fixed: fixedAudit, families: familyAudit,
  milestone2Handling: {
    leaf: "agent/src/trusted-execution-refusal.ts: independent Error subclass and legacyTrustedExecutionRefusal; no provider imports or API/protocol fields",
    producers: "launcher/config/fileop/safety/codex-executor/sdk-executor/claude-harness/provision-run/runner import the independent leaf for worker-authored decisions; existing specialized error types are preserved",
    commandAdmission: "makeDefaultSpawnCommand → launchRegisteredEffectRoot: command launch admission is closed / command root failed registry admission; rejected admission disposes root then throws TrustedExecutionRefusal",
    erasure: "FailClosedExecutor mints TrustedExecutionRefusal from its selection message; normal executor catches preserve already-typed errors; raw provider/filesystem errors remain unchanged",
    reader: "runner untypedExecutionFailure rejects custom prototypes/names and traverses at most eight cause/interruption values; terminalDiskExcludedReason uses legacy helper with existing control predicate; canonicalRecoveryInterruption shares the filter in capture catches",
    posture: "launcher verifyRunnerPosture reads metadata after tree/share/file actions: successfully parsed owner/group/mode mismatch throws TrustedExecutionRefusal with original provisioningFailure message; failed stat or malformed output stays plain Error; cleanup catches remain best effort",
    broker: "command/fileop handler catches still return tool denial, never promote to executor rejection",
    disposition: "original display reason retained; plan_missing, credential_unavailable and provisioning_failed mappings unchanged",
  },
  caughtOrMixed: [
    {
      source: "agent/src/codex/session-state.ts",
      messages: ["codex session store: refusing suspicious path component",
        "codex session store: refusing to operate on the filesystem root"],
      typed: "CodexSessionStoreError / CodexSessionStoreBoundError for source/bounds/I/O",
      caller: "startProviderEpoch adopt and persistSession catch to undefined; inspect walk catches to unknown; runner terminal remove is best effort",
      migration: "retain store types and safe result domains; do not promote swallowed store failures",
    },
    {
      source: "agent/src/codex/broker.ts",
      messages: ['the "${safeId(name)}" handler failed'],
      typed: "CallbackResult denial",
      caller: "dispatchMcp catches handler errors; dispatch Bash/fileops also returns tool denial",
      migration: "no promotion: tool-result errors are not executor rejection",
    },
    {
      source: "agent/src/codex/codex-executor.ts",
      messages: ["codex child sink notifications() is single-consumer",
        "codex child thread/start returned no thread id", "codex child turn/start returned no turn id",
        "codex advice refresh is not permitted for an api_key credential",
        "codex advice refresh has no observed generation", "codex advice subscription bridge has no account id",
        "codex advice root is missing a stdio transport channel",
        "${kind} supervisor root disposal not clean", "command aborted",
        "command supervisor root did not reap cleanly",
        "codex boundary-action spawn seam is not wired (the runner drives spawnBoundaryProcess)"],
      typed: "plain Error; registry disposal errors become structured HarnessError / CodexBoundaryError",
      caller: "child start dispatch/delegation returns tool denial; advice model passes are advisory; registeredRoot disposal caught during rejected admission / teardown; command abort is a broker denial or clean unverified environment probe, while an unclean probe becomes typed EnvProbeCleanupError; legacy boundary-root stub is unused by the runner",
      migration: "retain catches/results; no message-only promotion from these swallowed paths",
    },
    {
      source: "agent/src/codex/safety.ts",
      messages: ["boundary process argv is empty", "boundary process spawn failed"],
      typed: "plain Error then CodexBoundaryError at boundary",
      caller: "spawnBoundaryProcess catches spawnProcess and replaces any error with independently protected boundary process spawn failed",
      migration: "retain replacement; type boundary decision, not arbitrary underlying launch error",
    },
    {
      source: "agent/src/codex/launcher.ts",
      messages: ["refusing to remove a non-canonical owned data root", "refusing to remove an unexpected owned data root",
        "runner-owned tree creation failed…", "runner-owned session export posture failed…",
        "runner-owned session seed failed…", "runner-owned file write failed…", "runner-owned tree removal failed…"],
      typed: "plain Error; provisioningFailure combines status/signal/spawn-code and redacted stderr",
      caller: "tree creation/export/write can escape launch; tree removal cleanup is best effort; command cache start and settle catches warn",
      migration: "split decision-generated posture/contract failure from raw I/O before typing; NEVER classify arbitrary stderr/status as trusted posture",
    },
  ],
  variableErrors: [
    {
      source: "agent/src/config.ts",
      caller: "loadConfig duration/env/token/executor/sandbox validation runs at worker startup, before executor.run",
      migration: "outside terminal-execution eligibility; no new exclusions",
    },
    {
      source: "agent/src/sdk-executor.ts / claude-harness.ts / codex/codex-harness.ts",
      caller: "driveTurn trip wins; materialize throws original provider/SDK/query/iterator error; native Error propagated, primitive normalized with errMessage; handler onSessionId/onFirstEvent errors warn",
      typed: "LimitReachedError, ProviderTransientError, CliSignalDeathError, CodexHarnessError, CodexResumeError, CodexCredentialDeferredError",
      migration: "retain canonical trip/transient/credential routing; provider failure text and unknown EOF fallback remain opaque unless carrying independent trusted state",
    },
    {
      source: "agent/src/plan-missing.ts / sdk-executor.ts / runner.ts",
      caller: "askForMissingPlan unwired/unattended throws REASON_PLAN_MISSING; PlanRejectedError verdict preserves owner reason; definitive input delivery wraps GateInputDeliveryError; transient delivery parks; progress and session callbacks swallow; gatePlan/askUser/checkpoint can escape",
      typed: "PlanRejectedError, GateInputDeliveryError, TransientRecoveryError; fixed plan-missing reason maps plan_missing",
      migration: "retain owner rejection, timeout, plan_missing and receipt dispositions; do not classify arbitrary callback transport error text",
    },
    {
      source: "agent/src/runner.ts / git.ts / codex/safety.ts",
      caller: "capture source/proof/fetch/report catches use canonicalRecoveryInterruption; git publication errors typed ScratchPublicationError; unknown Git/stdout/I/O remains bounded capture retry; quiescence uncertainty becomes residue-blocked",
      typed: "ScratchPublicationError, GitBoundaryAbortError, CheckpointSoftDeadlineError, RunResidueBlockedError, CodexBoundaryError",
      migration: "retain custody and original canonical disposition; propagate trusted capture refusal once; keep opaque byte/inode recovery controls",
    },
  ],
} as const;

const reasons = [...new Set([
  ...trustedExecutionAudit.fixed.flatMap((entry) => entry.messages),
  ...trustedExecutionAudit.families.flatMap((entry) => entry.messages),
  ...protectedReasons,
  "the planning turn ended without a plan or a structured question after a corrective nudge",
  "clarification timed out", "plan approval timed out",
])];

export const trustedExecutionCases = reasons.flatMap((reason) => [
  { reason, representation: "direct Error", error: () => new Error(reason), display: reason },
  { reason, representation: "context + repeated Error", error: () => new Error(`startup: Error: Error: ${reason}`),
    display: `startup: Error: Error: ${reason}` },
]);

export const trustedEnvelopeCases = [
  "command launch admission is closed", "command root failed registry admission",
  "provider envKey is invalid or reserved by the launcher allowlist", "plan gate is not wired for this run",
].flatMap((reason) => [
  { reason, representation: "context Error", error: () => new Error(`startup: ${reason}`), display: `startup: ${reason}` },
  { reason, representation: "string", error: () => reason, display: reason },
  // Own toString makes the plain-object public display contract explicit, while
  // the classifier still traverses an Object.prototype message-only envelope.
  { reason, representation: "message object", error: () => ({ message: reason, toString: () => reason }), display: reason },
  { reason, representation: "Error cause", error: () => new Error("startup wrapper", { cause: new Error(reason) }), display: "startup wrapper" },
  { reason, representation: "string cause", error: () => new Error("startup wrapper", { cause: reason }), display: "startup wrapper" },
  { reason, representation: "interruption object", error: () => Object.assign(new Error("startup wrapper"), { interruption: { message: reason } }), display: "startup wrapper" },
]);

export const trustedCaptureCases = [
  { reason: "apostrophe context", representation: "context Error", error: () => new Error("worker's capture: command root failed registry admission"), display: "worker's capture: command root failed registry admission" },
  ...["command root failed registry admission", "plan gate is not wired for this run"].flatMap((reason) => [
    { reason, representation: "context Error", error: () => new Error(`capture: ${reason}`), display: `capture: ${reason}` },
    { reason, representation: "Error cause", error: () => new Error("capture wrapper", { cause: new Error(reason) }), display: "capture wrapper" },
  ]),
  { reason: "typed capture refusal", representation: "mixed typed wrapper", error: () => new Error("capture wrapper", { cause: { interruption: new TrustedExecutionRefusal("opaque capture decision") } }), display: "capture wrapper" },
  { reason: "command launch admission is closed", representation: "direct Error", error: () => new Error("command launch admission is closed"), display: "command launch admission is closed" },
  { reason: "command cwd escapes the worktree sandbox", representation: "context Error", error: () => new Error("capture: command cwd escapes the worktree sandbox"), display: "capture: command cwd escapes the worktree sandbox" },
  { reason: "provider envKey is invalid or reserved by the launcher allowlist", representation: "string cause", error: () => new Error("capture wrapper", { cause: "provider envKey is invalid or reserved by the launcher allowlist" }), display: "capture wrapper" },
  { reason: "boundary process spawn failed", representation: "direct Error", error: () => new Error("boundary process spawn failed"), display: "boundary process spawn failed" },
];

// Focused additions avoid multiplying every audited reason across every envelope.
function opaqueChain(values: number): Error {
  let error = new Error("opaque leaf");
  for (let i = 1; i < values; i++) error = new Error("opaque wrapper", { cause: error });
  return error;
}

export const trustedBoundaryCases = [
  ["apostrophe context", () => new Error("worker's startup: command launch admission is closed")],
  ["typed leaf", () => new TrustedExecutionRefusal("opaque typed decision")],
  ["typed cause", () => new Error("outer", { cause: new TrustedExecutionRefusal("opaque typed decision") })],
  ["typed interruption", () => Object.assign(new Error("outer"), { interruption: new TrustedExecutionRefusal("opaque typed decision") })],
  ["mixed wrappers", () => new Error("outer", { cause: { message: "middle", interruption: new TrustedExecutionRefusal("opaque typed decision") } })],
  ["nine opaque values exceed bound", () => opaqueChain(9)],
  ["cycle", () => { const error = new Error("outer"); error.cause = error; return error; }],
  ["throwing getter", () => Object.defineProperty(new Error("outer"), "cause", { get() { throw new Error("unreadable"); } })],
  ["twelve contexts", () => new Error("Error: ".repeat(12) + "command root failed registry admission")],
  ["parenthesized/bracketed contexts", () => new Error("(startup): [provider]: command launch admission is closed")],
  ["future status", () => new Error("autopilot plan not durably stored — the run is future_status")],
  ["future park status", () => new Error("could not park the run to ask a question (server reports future_status)")],
  ["String-coerced posture", () => new Error("supervisor reported an unsafe start posture (pid=[1,2], expectedPid=42, subreaper=(bad), nondumpable=true, liveCapsZero=true, capBoundingSet=0xc0, noNewPrivs=true, uid=(bad), expected 10002)")],
] as const;

export const opaqueBoundaryCases = [
  ["newline after colon", () => new Error("startup:\ncommand launch admission is closed")],
  ["snapshot near-miss", () => new Error("expected snapshot evidence, got snapshot")],
  ["eight opaque values within bound", () => opaqueChain(8)],
  ["quoted reason", () => new Error('filesystem says: "command root failed registry admission"')],
  ["unrelated diagnostic", () => new Error("filesystem says\ncommand root failed registry admission")],
  ["invalid root kind", () => new Error("other root failed registry admission")],
  ["trailing suffix", () => new Error("command root failed registry admission trailing")],
  ["invalid deadline parameter", () => new Error("started deadline exceeded (-1ms)")],
  ["invalid child exit parameter", () => new Error("supervised provider child exited unexpectedly (code=256)")],
  ["quoted guardrail", () => new Error('filesystem says: "denied by guardrail: reading the process environment is not permitted"')],
  ["valid config near-miss", () => new Error('Codex M3a supports only the "responses" wire (got "responses")')],
  ["valid uid near-miss", () => new Error("runner uid resolution produced an invalid uid: 10002")],
] as const;
