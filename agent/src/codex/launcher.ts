// PRD #1156 (M3a) — the isolated per-root Codex launch primitive.
//
// This is the reusable library the FUTURE worker adapter (and the m3a lifecycle
// test) calls to launch ONE supervisor root: a static Go supervisor
// (`/usr/local/bin/uzi-codex-supervisor`, built by a sibling unit) which forks the
// pinned Codex app-server (`/opt/uzi-codex/0.153.2/bin/codex app-server`). It
// composes the UNCHANGED worker→runner uid boundary in `runner-uid.ts`
// (`runnerCommand` / `uidSplitActive`) — this file edits none of those helpers.
//
// SUPPORTED PROFILE (fail-closed): this primitive is supported ONLY under the A1
// uid-split (a trusted controller at the worker uid, an untrusted runner at a
// DISTINCT runner uid). Under shared-uid / single-uid it REFUSES to launch: the
// per-root containment rests on the distinct runner uid, and provisioning a distinct
// runner uid on k8s is a later worker-integration prerequisite.
//
// WIRE PROTOCOL (the Go supervisor emits/consumes exactly this — we implement the
// launcher side to match; we invent no fields):
//   control  → fd3 (we WRITE): {"op":"snapshot","id"} / {"op":"dispose","id","timeoutMs"}
//   evidence ← fd4 (we READ):  started / snapshot / dispose(drained|unconfirmed) / abnormal
//   argv: <supervisor> --expect-uid <N> -- <codexBin> <childArgv...>   (launcher-fixed)
//   stdio: [pipe0, pipe1, pipe2, pipe3=control, pipe4=evidence]; 0/1/2 are the
//          app-server TRANSPORT the child inherits — exposed on the handle so a
//          caller speaks app-server JSONL RPC over them.
//   supervisor exit: 0 IFF a dispose reached "drained"; non-zero otherwise.
//
// Disposal is via the supervisor (write dispose, await the `dispose` evidence,
// require `state:"drained"` AND supervisor exit 0). We NEVER process-group kill: the
// child setsid's, so a group kill is not proof. Every abnormal path (supervisor exit
// non-zero, evidence `abnormal`, control-write failure, deadline, an `unconfirmed`
// dispose) reports FAILURE / not-clean-disposal for exactly THIS owned root; we never
// mint a run-level permit or `observed_empty`.

import { spawn, spawnSync } from "node:child_process";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { createInterface } from "node:readline";
import type { Readable, Writable } from "node:stream";

import { runnerCommand, uidSplitActive } from "../runner-uid.js";
import {
  assertNoUnexpectedSystemConfig as defaultAssertNoUnexpectedSystemConfig,
  buildCodexConfigToml,
} from "./config.js";

// ─── Bounds (fixture-derived; supervisor-side limits are matched, not trusted) ────
const MAX_EVIDENCE_LINES = 256;
const MAX_EVIDENCE_LINE_BYTES = 65536; // 64 KiB per evidence line
const MAX_CONTROL_BYTES = 8192; // 8 KiB per control frame

/** The fully-REPLACED PATH: system dirs + the pinned worker toolchain, and NOT the
 *  Codex bundle (`/opt/uzi-codex/.../bin` and `codex-path/rg` are resolved by
 *  absolute path INSIDE the launch, never via PATH — PRD #1156 build facts). */
const CODEX_LAUNCH_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin";
const CODEX_BIN = "/opt/uzi-codex/0.153.2/bin/codex";
const SUPERVISOR_BIN = "/usr/local/bin/uzi-codex-supervisor";
const PROVIDER_CHILD_ARGV = ["app-server"] as const;
const RESERVED_PROVIDER_ENV_KEYS = new Set([
  "HOME", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
  "TMPDIR", "PATH", "SHELL", "LANG", "TERM", "NODE_OPTIONS", "BASH_ENV", "ENV", "SHELLOPTS",
  "LD_PRELOAD", "LD_LIBRARY_PATH",
]);

// ─── Public spec/handle types ─────────────────────────────────────────────────
export type CodexRootKind = "provider" | "command";

export interface CodexLaunchProvider {
  readonly name: string;
  readonly baseUrl: string;
  readonly envKey: string;
  /** The dummy/real credential for `kind:"provider"`; absent for `kind:"command"`. */
  readonly credentialValue?: string;
}

export interface CodexLaunchSpec {
  /** A RUNNER-WRITABLE root, OUTSIDE the target repo, under which fresh per-launch
   *  HOME/CODEX_HOME/XDG/TMPDIR trees are created runner-owned mode 0700. */
  readonly ownedDataRoot: string;
  readonly provider: CodexLaunchProvider;
  readonly model: string;
  /** Provider roots require the pinned Codex path. Command roots may select a trusted
   *  absolute child executable, but never the supervisor trust anchor. */
  readonly codexBin: string;
  /** Must equal the immutable image path; retained on the wire only for explicit
   *  compatibility and rejected if it names anything else. */
  readonly supervisorBin: string;
  readonly kind: CodexRootKind;
  /** The Codex sub-argv (e.g. `["app-server"]`); launcher-fixed, never model-controlled. */
  readonly childArgv: readonly string[];
  /** The canonical project / launch cwd, provisioned untrusted in config.toml. */
  readonly cwd: string;
}

/** The privileged step that creates the runner-owned trees + writes the config. The
 *  post-drop worker LACKS CAP_CHOWN, so it cannot chown; the dirs must be CREATED as
 *  the runner uid (setpriv). Injectable so unit tests fake it (no setpriv/root). */
export interface RunnerTreeRequest {
  readonly uid: number;
  readonly root: string;
  readonly dirs: readonly string[];
  readonly files: readonly { readonly path: string; readonly content: string; readonly mode: number }[];
}
export type MakeRunnerTrees = (request: RunnerTreeRequest) => void;

/** The minimal supervisor-process surface the launcher uses. Node's `ChildProcess`
 *  satisfies it structurally; the unit tests inject a fake that does too. */
export interface SupervisorProcess {
  readonly pid?: number;
  readonly stdin: Writable | null;
  readonly stdout: Readable | null;
  readonly stderr: Readable | null;
  readonly stdio: ReadonlyArray<Readable | Writable | null | undefined>;
  once(event: string, listener: (...args: never[]) => void): unknown;
  on(event: string, listener: (...args: never[]) => void): unknown;
}
export type SpawnSupervisor = (
  command: string,
  args: readonly string[],
  options: { cwd: string; env: NodeJS.ProcessEnv; stdio: readonly ("pipe")[] },
) => SupervisorProcess;

/** Bounded deadlines (ms) for the three lifecycle awaits; injectable for tests. */
export interface LauncherDeadlines {
  readonly started: number;
  readonly snapshot: number;
  /** Slack added to the caller's dispose `timeoutMs` before the launcher gives up. */
  readonly dispose: number;
  readonly exit: number;
}

export interface LauncherDeps {
  /** Env consulted for the supported-profile gate (defaults to process.env). */
  readonly env?: NodeJS.ProcessEnv;
  /** Resolve the intended runner uid `<N>` (defaults to `id -u runner`). */
  readonly resolveRunnerUid?: () => number;
  readonly makeRunnerTrees?: MakeRunnerTrees;
  readonly spawnSupervisor?: SpawnSupervisor;
  readonly assertNoUnexpectedSystemConfig?: (etcCodexDir?: string) => void;
  readonly etcCodexDir?: string;
  readonly deadlines?: Partial<LauncherDeadlines>;
}

// ─── Evidence frame shapes (READ from fd4; we do not invent fields) ─────────────
export interface StartedEvidence {
  readonly event: "started";
  readonly supervisorPid: number;
  readonly childPid: number;
  readonly subreaper: boolean;
  readonly nondumpable: boolean;
  readonly uid: number;
  readonly liveCapsZero: boolean;
  readonly capBoundingSet: "0x0" | "0xc0";
  readonly noNewPrivs: boolean;
}
export interface SnapshotEvidence {
  readonly event: "snapshot";
  readonly id: number;
  readonly processes: ReadonlyArray<{ readonly pid: number; readonly ppid: number; readonly pgid: number; readonly comm: string }>;
}
export interface DisposeEvidence {
  readonly event: "dispose";
  readonly id: number;
  readonly state: "drained" | "unconfirmed";
  readonly authority?: string;
  readonly reason?: string;
  readonly killed?: readonly number[];
  readonly reaped?: readonly number[];
  readonly children?: readonly number[];
}

/** The result of a disposal attempt for exactly THIS owned root. `clean` is true ONLY
 *  when the supervisor reported `state:"drained"` AND exited 0. Never a run-level fact. */
export type DisposeOutcome =
  | { readonly clean: true; readonly event: DisposeEvidence }
  | { readonly clean: false; readonly reason: string; readonly event?: DisposeEvidence };

export interface CodexRootHandle {
  /** The `started` evidence (present once `launchCodexRoot` resolves). */
  readonly started: StartedEvidence;
  readonly supervisorPid: number | undefined;
  /** The app-server TRANSPORT (fds 0/1/2) — a caller speaks app-server JSONL RPC here. */
  readonly transport: { readonly stdin: Writable | null; readonly stdout: Readable | null; readonly stderr: Readable | null };
  snapshot(timeoutMs?: number): Promise<SnapshotEvidence>;
  dispose(timeoutMs?: number): Promise<DisposeOutcome>;
  /** The sticky failure for THIS root, if any (abnormal, early exit, protocol breach). */
  readonly failed: Error | undefined;
  /** Resolves once this root enters a failure state (used by the CLI's shutdown race). */
  readonly whenFailed: Promise<Error>;
}

/** Thrown by the fail-closed supported-profile gate. */
export class CodexUnsupportedProfileError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CodexUnsupportedProfileError";
  }
}

const DEFAULT_DEADLINES: LauncherDeadlines = { started: 10000, snapshot: 5000, dispose: 5000, exit: 5000 };

/** Look up the `runner` user's uid (default resolver; injected away in unit tests). */
function defaultResolveRunnerUid(): number {
  const r = spawnSync("id", ["-u", "runner"], { encoding: "utf8" });
  if (r.status !== 0) {
    throw new Error(`cannot resolve the runner uid (id -u runner exited ${String(r.status)})`);
  }
  const uid = Number.parseInt(String(r.stdout).trim(), 10);
  if (!Number.isInteger(uid) || uid <= 0) throw new Error(`runner uid resolution produced an invalid uid: ${String(r.stdout)}`);
  return uid;
}

/** Default privileged provisioning: create the dirs 0700 and write the config file,
 *  ALL as the runner uid via setpriv (`runnerCommand`), never chown. File content is
 *  fed on stdin so it is never embedded in an argv. Not exercised by unit tests. */
function defaultMakeRunnerTrees(request: RunnerTreeRequest): void {
  // The helper itself runs as the shared runner uid, so it must not inherit the worker
  // process environment: another concurrent runner can read a normal helper process via
  // /proc/<pid>/environ. Only inert locale/path values cross this short provisioning step.
  const provisionEnv: NodeJS.ProcessEnv = { PATH: "/usr/bin:/bin", LANG: "C" };
  const mk = runnerCommand("/bin/sh", [
    "-ceu",
    `root="$1"; uid="$2"; shift 2; umask 077; mkdir -m 700 -- "$root"; trap 'rc=$?; [ "$rc" -eq 0 ] || rm -rf -- "$root"; exit "$rc"' EXIT; chmod 700 "$root"; for d in "$@"; do mkdir -m 700 -- "$d"; chmod 700 "$d"; done; for d in "$root" "$@"; do [ "$(stat -c %u "$d")" = "$uid" ] && [ "$(stat -c %a "$d")" = 700 ]; done; trap - EXIT`,
    "sh", request.root, String(request.uid), ...request.dirs,
  ]);
  const r = spawnSync(mk.command, mk.args, { env: provisionEnv, stdio: ["ignore", "ignore", "pipe"] });
  if (r.status !== 0) throw new Error(`runner-owned tree creation failed (exit ${String(r.status)}): ${String(r.stderr)}`);
  try {
    for (const file of request.files) {
      const octal = (file.mode & 0o777).toString(8).padStart(3, "0");
      const w = runnerCommand("/bin/sh", ["-ceu", `umask 077; cat > "$1"; chmod ${octal} "$1"; [ "$(stat -c %u "$1")" = "$2" ] && [ "$(stat -c %a "$1")" = ${octal} ]`, "sh", file.path, String(request.uid)]);
      const wr = spawnSync(w.command, w.args, { env: provisionEnv, input: file.content, stdio: ["pipe", "ignore", "pipe"] });
      if (wr.status !== 0) throw new Error(`runner-owned file write failed for ${file.path} (exit ${String(wr.status)}): ${String(wr.stderr)}`);
    }
  } catch (error) {
    const rm = runnerCommand("/bin/rm", ["-rf", "--", request.root]);
    spawnSync(rm.command, rm.args, { env: provisionEnv, stdio: "ignore" });
    throw error;
  }
}

const defaultSpawnSupervisor: SpawnSupervisor = (command, args, options) =>
  spawn(command, [...args], { cwd: options.cwd, env: options.env, stdio: [...options.stdio] }) as unknown as SupervisorProcess;

/** The per-launch owned trees derived under `ownedDataRoot`. */
interface OwnedTrees {
  readonly home: string;
  readonly codexHome: string;
  readonly xdgConfig: string;
  readonly xdgCache: string;
  readonly xdgData: string;
  readonly xdgState: string;
  readonly tmpdir: string;
  readonly all: readonly string[];
}
function deriveOwnedTrees(root: string): OwnedTrees {
  const home = join(root, "home");
  const codexHome = join(root, "codex");
  const xdgConfig = join(root, "xdg-config");
  const xdgCache = join(root, "xdg-cache");
  const xdgData = join(root, "xdg-data");
  const xdgState = join(root, "xdg-state");
  const tmpdir = join(root, "tmp");
  return {
    home, codexHome, xdgConfig, xdgCache, xdgData, xdgState, tmpdir,
    all: [home, codexHome, xdgConfig, xdgCache, xdgData, xdgState, tmpdir],
  };
}

function pathsOverlap(a: string, b: string): boolean {
  const ar = resolve(a);
  const br = resolve(b);
  const aToB = relative(ar, br);
  const bToA = relative(br, ar);
  return aToB === "" || (!aToB.startsWith(`..${sep}`) && aToB !== ".." && !isAbsolute(aToB))
    || (!bToA.startsWith(`..${sep}`) && bToA !== ".." && !isAbsolute(bToA));
}

/** Validate the trusted construction contract for every caller, not only the JSON CLI. */
function validateLaunchContract(spec: CodexLaunchSpec): void {
  if (spec.kind !== "provider" && spec.kind !== "command") throw new Error("kind must be provider or command");
  if (!isAbsolute(spec.ownedDataRoot) || resolve(spec.ownedDataRoot) === sep) {
    throw new Error("ownedDataRoot must be an absolute non-root path");
  }
  if (!isAbsolute(spec.cwd)) throw new Error("cwd must be an absolute path");
  if (pathsOverlap(spec.ownedDataRoot, spec.cwd)) {
    throw new Error("ownedDataRoot and cwd must be disjoint");
  }
  if (spec.supervisorBin !== SUPERVISOR_BIN) {
    throw new Error(`supervisorBin must equal the immutable image path ${SUPERVISOR_BIN}`);
  }
  if (!isAbsolute(spec.codexBin)) throw new Error("child executable must be absolute");
  if (spec.kind === "provider") {
    if (spec.codexBin !== CODEX_BIN || spec.childArgv.length !== PROVIDER_CHILD_ARGV.length
      || spec.childArgv.some((arg, index) => arg !== PROVIDER_CHILD_ARGV[index])) {
      throw new Error("provider roots must use the pinned Codex app-server target");
    }
  }
  if (!/^[A-Z][A-Z0-9_]*$/.test(spec.provider.envKey) || RESERVED_PROVIDER_ENV_KEYS.has(spec.provider.envKey)) {
    throw new Error("provider envKey is invalid or reserved by the launcher allowlist");
  }
}

/** The fully-REPLACED env allowlist (never a merge of the worker's env): HOME,
 *  CODEX_HOME, the four XDG_*_HOME, TMPDIR, a fixed PATH, SHELL/LANG/TERM, and the
 *  provider credential var ONLY for `kind:"provider"`. No inherited worker/forge/
 *  Anthropic creds, proxy, NODE_OPTIONS, git config, or host HOME. */
function buildReplacedEnv(trees: OwnedTrees, spec: CodexLaunchSpec): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    HOME: trees.home,
    CODEX_HOME: trees.codexHome,
    XDG_CONFIG_HOME: trees.xdgConfig,
    XDG_CACHE_HOME: trees.xdgCache,
    XDG_DATA_HOME: trees.xdgData,
    XDG_STATE_HOME: trees.xdgState,
    TMPDIR: trees.tmpdir,
    PATH: CODEX_LAUNCH_PATH,
    SHELL: "/bin/sh",
    LANG: "C",
    TERM: "dumb",
  };
  if (spec.kind === "provider" && spec.provider.credentialValue !== undefined) {
    env[spec.provider.envKey] = spec.provider.credentialValue;
  }
  return env;
}

function withDeadline<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`${label} deadline exceeded (${ms}ms)`)), ms);
    if (typeof timer.unref === "function") timer.unref();
    p.then(
      (value) => { clearTimeout(timer); resolve(value); },
      (error: unknown) => { clearTimeout(timer); reject(error instanceof Error ? error : new Error(String(error))); },
    );
  });
}

function asWritable(stream: Readable | Writable | null | undefined, label: string): Writable {
  if (!stream || typeof (stream as Writable).write !== "function") throw new Error(`supervisor ${label} channel (fd) is unavailable`);
  return stream as Writable;
}
function asReadable(stream: Readable | Writable | null | undefined, label: string): Readable {
  if (!stream || typeof (stream as Readable).on !== "function") throw new Error(`supervisor ${label} channel (fd) is unavailable`);
  return stream as Readable;
}

/**
 * Launch ONE supervisor root and return its handle. Fail-closed under a non-split
 * profile. Everything is bounded (deadlines on started / snapshot / dispose).
 */
export async function launchCodexRoot(spec: CodexLaunchSpec, deps: LauncherDeps = {}): Promise<CodexRootHandle> {
  // 1. Supported-profile gate — fail closed under shared-uid / single-uid.
  const env = deps.env ?? process.env;
  if (!uidSplitActive(env)) {
    throw new CodexUnsupportedProfileError(
      "Codex per-root launch is supported ONLY under the A1 uid-split (a trusted worker uid and a "
      + "DISTINCT untrusted runner uid). This process is shared-uid / single-uid (UZI_UID_SPLIT unset); "
      + "provisioning a distinct runner uid on k8s is a later worker-integration prerequisite. Refusing to launch.",
    );
  }

  validateLaunchContract(spec);

  // Resolve the intended runner uid <N> for --expect-uid and the runner-owned trees.
  const uid = (deps.resolveRunnerUid ?? defaultResolveRunnerUid)();
  if (!Number.isInteger(uid) || uid <= 0) throw new Error(`resolved runner uid is invalid: ${String(uid)}`);

  // 2. Reject unexpected system config (a fresh HOME does not neutralize /etc/codex).
  (deps.assertNoUnexpectedSystemConfig ?? defaultAssertNoUnexpectedSystemConfig)(deps.etcCodexDir);

  // 3. Fresh per-launch trees, created RUNNER-OWNED 0700 via the injectable step.
  const trees = deriveOwnedTrees(spec.ownedDataRoot);
  const configPath = join(trees.codexHome, "config.toml");
  // 4. Stock config.toml (native-off; canonical project untrusted), written 0600 as runner.
  const configText = buildCodexConfigToml({
    model: spec.model,
    provider: { name: spec.provider.name, baseUrl: spec.provider.baseUrl, envKey: spec.provider.envKey, wireApi: "responses" },
    projectPath: spec.cwd,
  });
  (deps.makeRunnerTrees ?? defaultMakeRunnerTrees)({
    uid,
    root: spec.ownedDataRoot,
    dirs: trees.all,
    files: [{ path: configPath, content: configText, mode: 0o600 }],
  });

  // 5. Replaced env allowlist (NOT merged).
  const replacedEnv = buildReplacedEnv(trees, spec);

  // 6. Spawn the supervisor AS the runner uid (compose the UNCHANGED runnerCommand),
  //    with 5-fd stdio (0/1/2 transport, 3 control, 4 evidence). The supervisor argv
  //    is trusted & launcher-fixed: never model-controlled.
  const supervisorArgv = ["--expect-uid", String(uid), "--", spec.codexBin, ...spec.childArgv];
  const wrapped = runnerCommand(spec.supervisorBin, supervisorArgv);
  const child = (deps.spawnSupervisor ?? defaultSpawnSupervisor)(wrapped.command, wrapped.args, {
    cwd: spec.cwd,
    env: replacedEnv,
    stdio: ["pipe", "pipe", "pipe", "pipe", "pipe"],
  });

  // 7. Parse evidence (bounded), await `started`, expose snapshot/dispose + transport.
  return createHandle(child, uid, { ...DEFAULT_DEADLINES, ...deps.deadlines });
}

async function createHandle(child: SupervisorProcess, expectedUid: number, deadlines: LauncherDeadlines): Promise<CodexRootHandle> {
  const control = asWritable(child.stdio[3], "control");
  const evidence = asReadable(child.stdio[4], "evidence");

  let nextId = 1;
  const pending = new Map<number, { resolve: (e: SnapshotEvidence | DisposeEvidence) => void; reject: (err: Error) => void }>();
  let failure: Error | undefined;
  let startedEvent: StartedEvidence | undefined;
  let exited = false;
  let exitInfo: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  let disposeInFlight = false;
  let cleanDisposed = false;
  let lastDrained: DisposeEvidence | undefined;
  let lineCount = 0;

  let resolveStarted!: (e: StartedEvidence) => void;
  let rejectStarted!: (err: Error) => void;
  const startedPromise = new Promise<StartedEvidence>((resolve, reject) => { resolveStarted = resolve; rejectStarted = reject; });
  let resolveExit!: (info: { code: number | null; signal: NodeJS.Signals | null }) => void;
  const exitPromise = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) => { resolveExit = resolve; });
  let resolveFailed!: (err: Error) => void;
  const whenFailed = new Promise<Error>((resolve) => { resolveFailed = resolve; });

  function fail(err: Error): void {
    if (!failure) {
      failure = err;
      resolveFailed(err);
      if (!startedEvent) rejectStarted(err);
    }
    for (const p of pending.values()) p.reject(failure);
    pending.clear();
    // Closing the trusted control endpoint makes the real supervisor take its abnormal
    // controller-loss path and perform a bounded best-effort drain. The evidence channel
    // is already untrusted/unavailable on these paths, so no clean result is inferred.
    try { if (!control.destroyed && !control.writableEnded) control.end(); } catch { /* primary failure already recorded */ }
  }

  const lines = createInterface({ input: evidence });
  lines.on("line", (line) => {
    lineCount += 1;
    if (lineCount > MAX_EVIDENCE_LINES) { fail(new Error(`evidence line budget exceeded (> ${MAX_EVIDENCE_LINES})`)); lines.close(); return; }
    if (Buffer.byteLength(line) > MAX_EVIDENCE_LINE_BYTES) { fail(new Error(`oversized evidence line (> ${MAX_EVIDENCE_LINE_BYTES} bytes)`)); return; }
    let value: unknown;
    try { value = JSON.parse(line); } catch { fail(new Error("malformed evidence line (not JSON)")); return; }
    dispatch(value);
  });
  lines.on("error", (err: Error) => fail(new Error(`evidence stream error: ${err.message}`)));
  lines.on("close", () => {
    if (failure || cleanDisposed || exited) return;
    if (pending.size > 0 || !disposeInFlight) fail(new Error("evidence stream closed before confirmed disposal"));
  });
  control.on("error", (err: Error) => fail(new Error(`control stream error: ${err.message}`)));

  function dispatch(value: unknown): void {
    if (typeof value !== "object" || value === null) { fail(new Error("evidence line is not a JSON object")); return; }
    const record = value as Record<string, unknown>;
    switch (record.event) {
      case "started": {
        const ev = record as unknown as StartedEvidence;
        if (!Number.isInteger(ev.supervisorPid) || ev.supervisorPid !== child.pid
          || !Number.isInteger(ev.childPid) || ev.childPid <= 0
          || ev.subreaper !== true || ev.nondumpable !== true || ev.liveCapsZero !== true
          || (ev.capBoundingSet !== "0x0" && ev.capBoundingSet !== "0xc0")
          || ev.noNewPrivs !== true || ev.uid !== expectedUid) {
          // Do NOT set startedEvent: a bad posture must REJECT the launch, not resolve it.
          // Re-validate every posture field, including nondumpability and the explicit
          // bounding-set residue — the controller's independent
          // check must be symmetric with the fields it receives, not a subset of them.
          fail(new Error(
            `supervisor reported an unsafe start posture (pid=${String(ev.supervisorPid)} expectedPid=${String(child.pid)}, `
            + `subreaper=${String(ev.subreaper)}, nondumpable=${String(ev.nondumpable)}, liveCapsZero=${String(ev.liveCapsZero)}, `
            + `capBoundingSet=${String(ev.capBoundingSet)}, noNewPrivs=${String(ev.noNewPrivs)}, uid=${String(ev.uid)} expected ${expectedUid})`,
          ));
          return;
        }
        startedEvent = ev;
        resolveStarted(ev);
        return;
      }
      case "snapshot":
      case "dispose": {
        const id = record.id;
        const waiter = typeof id === "number" ? pending.get(id) : undefined;
        if (waiter && typeof id === "number") { pending.delete(id); waiter.resolve(record as unknown as SnapshotEvidence | DisposeEvidence); }
        return;
      }
      case "abnormal": {
        fail(new Error(`supervisor abnormal: ${String(record.reason ?? "unknown")}`));
        return;
      }
      default:
        // An unknown event is a protocol breach — fail closed rather than guess.
        fail(new Error(`unknown supervisor evidence event: ${String(record.event)}`));
    }
  }

  child.once("exit", (code: number | null, signal: NodeJS.Signals | null) => {
    exited = true;
    exitInfo = { code, signal };
    resolveExit(exitInfo);
    // A dispose in flight evaluates the exit code itself (the `dispose` evidence may
    // still be buffered and read AFTER this event — Node's `exit` can precede the final
    // pipe read). A spontaneous exit with no dispose pending is a failure.
    if (!disposeInFlight && !cleanDisposed) {
      fail(new Error(`supervisor exited (code=${String(code)}, signal=${String(signal)}) without confirmed disposal`));
    }
  });
  child.on("error", (err: Error) => fail(new Error(`supervisor process error: ${err.message}`)));
  child.once("close", () => {
    if (pending.size > 0) fail(new Error("supervisor channels closed with an unanswered control request"));
  });

  function writeControl(frame: Record<string, unknown>): boolean {
    if (control.destroyed || exited) return false;
    const encoded = `${JSON.stringify(frame)}\n`;
    if (Buffer.byteLength(encoded) > MAX_CONTROL_BYTES) throw new Error(`control frame exceeds ${MAX_CONTROL_BYTES} bytes`);
    try { control.write(encoded); return true; } catch { return false; }
  }

  function sendAndWait(id: number, frame: Record<string, unknown>): Promise<SnapshotEvidence | DisposeEvidence> {
    return new Promise<SnapshotEvidence | DisposeEvidence>((resolve, reject) => {
      if (failure) { reject(failure); return; }
      pending.set(id, { resolve, reject });
      if (!writeControl(frame)) { pending.delete(id); reject(new Error("control write failure (channel unavailable)")); }
    });
  }

  async function snapshot(timeoutMs: number = deadlines.snapshot): Promise<SnapshotEvidence> {
    if (failure) throw failure;
    if (exited) throw new Error("supervisor has exited; snapshot unavailable");
    const id = nextId++;
    const ev = await withDeadline(sendAndWait(id, { op: "snapshot", id }), timeoutMs, "snapshot");
    if (ev.event !== "snapshot") throw new Error(`expected snapshot evidence, got ${String(ev.event)}`);
    return ev;
  }

  async function dispose(timeoutMs = 2000): Promise<DisposeOutcome> {
    if (cleanDisposed && lastDrained) return { clean: true, event: lastDrained };
    if (failure) return { clean: false, reason: failure.message };
    if (exited) return { clean: false, reason: `supervisor already exited (code=${String(exitInfo?.code)})` };
    if (control.destroyed) return { clean: false, reason: "control channel unavailable; disposal unconfirmed" };
    disposeInFlight = true;
    try {
      const id = nextId++;
      let ev: SnapshotEvidence | DisposeEvidence;
      try {
        ev = await withDeadline(sendAndWait(id, { op: "dispose", id, timeoutMs }), deadlines.dispose + timeoutMs, "dispose");
      } catch (error) {
        return { clean: false, reason: error instanceof Error ? error.message : String(error) };
      }
      if (ev.event !== "dispose") return { clean: false, reason: `expected dispose evidence, got ${String(ev.event)}` };
      const disposeEv = ev;
      if (disposeEv.state !== "drained") {
        return { clean: false, reason: `dispose ${String(disposeEv.state)}${disposeEv.reason ? `: ${disposeEv.reason}` : ""}`, event: disposeEv };
      }
      if (disposeEv.authority !== "ECHILD+__WALL") {
        return { clean: false, reason: `drained dispose missing required authority (got ${String(disposeEv.authority)})`, event: disposeEv };
      }
      // Drained reported — a clean supervisor exit (0) must confirm it.
      let exit: { code: number | null; signal: NodeJS.Signals | null };
      try {
        exit = await withDeadline(exitPromise, deadlines.exit, "supervisor exit");
      } catch (error) {
        return { clean: false, reason: error instanceof Error ? error.message : String(error), event: disposeEv };
      }
      if (exit.code !== 0) {
        return { clean: false, reason: `supervisor exited non-zero (code=${String(exit.code)}, signal=${String(exit.signal)}) after a drained report`, event: disposeEv };
      }
      cleanDisposed = true;
      lastDrained = disposeEv;
      return { clean: true, event: disposeEv };
    } finally {
      disposeInFlight = false;
    }
  }

  // Await `started` (bounded). A pre-started failure/exit rejects the launch; we end
  // the control channel so the supervisor runs its own bounded abnormal cleanup — we
  // NEVER process-group kill.
  try {
    startedEvent = await withDeadline(startedPromise, deadlines.started, "started");
  } catch (error) {
    try { if (!control.destroyed) control.end(); } catch { /* cleanup error is separate from the primary failure */ }
    lines.close();
    throw error;
  }

  const started = startedEvent;
  return {
    started,
    supervisorPid: child.pid,
    transport: { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr },
    snapshot,
    dispose,
    get failed() { return failure; },
    whenFailed,
  };
}
