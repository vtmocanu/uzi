// PRD #1156 (M3a) — the isolated per-root Codex launch primitive.
//
// This is the reusable library the production worker adapter (and the m3a lifecycle
// test) calls to launch ONE supervisor root: a static Go supervisor
// (`/usr/local/bin/uzi-codex-supervisor`, built by a sibling unit) which forks the
// pinned Codex app-server (`/opt/uzi-codex/0.156.1/bin/codex app-server`). It
// composes the worker→runner/runner-cmd uid boundaries in `runner-uid.ts`.
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
//            (a drained dispose, or an abnormal whose cleanup drained, may carry an
//             optional, strictly validated tmpCleanup; on any other event it is a breach)
//   argv: <supervisor> --expect-uid <N> [--cleanup-token <uuid>] -- <child> <argv...>
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
import { lstatSync } from "node:fs";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { createInterface } from "node:readline";
import type { Readable, Writable } from "node:stream";

import {
  CODEX_SESSION_GID,
  COMMAND_UID,
  WORKER_UID,
  commandRootCommand,
  runnerCommand,
  uidSplitActive,
  workerBoundaryCommand,
} from "../runner-uid.js";
import {
  assertNoUnexpectedSystemConfig as defaultAssertNoUnexpectedSystemConfig,
  buildCodexConfigToml,
  buildCodexLoopbackTestConfigToml,
  buildCodexProductionConfigToml,
} from "./config.js";
import type { CodexAppServerAuthMode } from "./appserver-auth.js";
import { SESSION_SEED_ENTRYPOINT } from "./session-seed-cli.js";

// ─── Bounds (fixture-derived; supervisor-side limits are matched, not trusted) ────
const MAX_EVIDENCE_LINES = 256;
const MAX_EVIDENCE_LINE_BYTES = 65536; // 64 KiB per evidence line
const MAX_CONTROL_BYTES = 8192; // 8 KiB per control frame

/** The fully-REPLACED PATH: system dirs + the pinned worker toolchain, and NOT the
 *  Codex bundle (`/opt/uzi-codex/.../bin` and `codex-path/rg` are resolved by
 *  absolute path INSIDE the launch, never via PATH — PRD #1156 build facts). */
const CODEX_LAUNCH_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/uzi-toolchain/bin";
/** The pinned, image-baked Codex app-server binary. Exported so the production adapter
 *  composition (codex-executor.ts) can build a provider {@link CodexLaunchSpec} without
 *  re-typing the literal; the launcher still re-validates a provider spec names exactly it. */
export const CODEX_BIN = "/opt/uzi-codex/0.156.1/bin/codex";
/** The pinned, image-baked supervisor trust anchor. Exported for the same reason. */
export const SUPERVISOR_BIN = "/usr/local/bin/uzi-codex-supervisor";
/** The fixed provider child argv (`codex app-server`). Exported so the production
 *  composition supplies exactly the launcher-required value. */
export const PROVIDER_CHILD_ARGV = ["app-server"] as const;
/** A lowercase (canonical `randomUUID()`) UUID: the only token shape the supervisor's
 *  --cleanup-token / --cache-token flags accept. */
const LOWERCASE_UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
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
   * HOME/CODEX_HOME/XDG/TMPDIR trees are created runner-owned mode 0700. Managed-auth
   * provider roots add only session-reader-group traverse on the root and CODEX_HOME, plus
   * read access on CODEX_HOME/sessions, so the worker can persist that safe subset. */
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
  /** PRD #1171: when true, the root authenticates through the app-server login RPC
   *  (`account/login/start`) rather than an env-key credential. `launchCodexRoot` then
   *  emits the production `config.toml` ({@link buildCodexProductionConfigToml}, the
   *  built-in `openai` provider with NO re-declared `[model_providers.*]` table that would
   *  bypass the login) and {@link buildReplacedEnv} injects NO provider credential var for
   *  this root — the credential enters over the transport, never the environment. Absent /
   *  false keeps the transitional env-key path byte-identical. Provider roots only. */
  readonly useAppServerAuth?: boolean;
  /** The immutable app-server auth mode, REQUIRED when {@link useAppServerAuth} is set (the
   *  production config builder forces an explicit no-fallback choice). Ignored otherwise. */
  readonly authMode?: CodexAppServerAuthMode;
  /** Trusted & launcher-fixed (like {@link authMode}), never model/repo-controlled; only
   *  honoured on the app-server-auth production/loopback path, where it enables ONLY the
   *  code-mode execution host. Absent/false keeps the host disabled. */
  readonly codeModeHost?: boolean;
  /** Managed-auth resume only: copy the executor's deterministic, credential-free
   * sibling staging tree into the runner-owned sessions directory before spawn. */
  readonly seedSession?: boolean;
}

/** The privileged step that creates the runner-owned trees + writes the config. The
 *  post-drop worker LACKS CAP_CHOWN, so it cannot chown; the dirs must be CREATED as
 *  the runner uid (setpriv). Injectable so unit tests fake it (no setpriv/root). */
export interface RunnerTreeRequest {
  readonly uid: number;
  readonly kind: CodexRootKind;
  readonly root: string;
  readonly dirs: readonly string[];
  /** Narrow managed-auth session export path. The worker may traverse root/codexHome
   * and read sessionDir, but cannot list either parent. */
  readonly sharedSessionRead?: {
    readonly gid: number;
    readonly codexHome: string;
    readonly sessionDir: string;
  };
  /** Deterministic credential-free staging source, copied as the provider runner
   * after final tree provisioning and before the app-server process is spawned. */
  readonly sessionSeedDir?: string;
  readonly files: readonly { readonly path: string; readonly content: string; readonly mode: number }[];
}
export type MakeRunnerTrees = (request: RunnerTreeRequest) => void;
export type RemoveRunnerTree = (request: Pick<RunnerTreeRequest, "uid" | "kind" | "root">) => void;

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
  /** Legacy input-shape field, not consulted by disposal. Call `handle.dispose(timeoutMs)` with the
   * total budget explicitly; omitting it uses the handle's 2s default. */
  readonly dispose: number;
  readonly exit: number;
}

export interface LauncherDeps {
  /** Env consulted for the supported-profile gate (defaults to process.env). */
  readonly env?: NodeJS.ProcessEnv;
  /** Resolve the intended runner uid `<N>` (defaults to `id -u runner`). */
  readonly resolveRunnerUid?: () => number;
  readonly resolveCommandUid?: () => number;
  readonly resolveWorkerUid?: () => number;
  readonly makeRunnerTrees?: MakeRunnerTrees;
  /** Remove the runner-owned launch tree after, and only after, its supervisor
   * proves a drained disposal. Tests inject this alongside makeRunnerTrees. */
  readonly removeRunnerTree?: RemoveRunnerTree;
  /** Static, path-free diagnostic for a best-effort tree-removal failure. */
  readonly reportRunnerTreeCleanupFailure?: () => void;
  readonly spawnSupervisor?: SpawnSupervisor;
  readonly assertNoUnexpectedSystemConfig?: (etcCodexDir?: string) => void;
  readonly etcCodexDir?: string;
  readonly deadlines?: Partial<LauncherDeadlines>;
  /** M3b-only authenticated custom-provider redirect. The alternate builder accepts
   * only a credential-free literal loopback URL and disables WebSockets; production
   * leaves this absent and therefore emits the fixed vendor config byte-for-byte. */
  readonly appServerAuthOpenAIBaseUrlForTest?: string;
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
export interface ChildExitEvidence {
  readonly event: "child_exit";
  readonly code: number;
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
  /** Present only for a root launched with a cleanup token: the outcome of the
   *  supervisor removing `/tmp/uzi-codex-command-<token>` after a confirmed drain.
   *  `reason` is "" when removed, otherwise one of the fixed words mismatch, owner,
   *  deadline, io, absent or name. A retained tmp does not make the disposal unclean. */
  readonly tmpCleanup?: TmpCleanupEvidence;
}

export interface TmpCleanupEvidence {
  readonly state: "removed" | "retained";
  readonly reason: string;
}

/** The fixed reasons the supervisor reports for a RETAINED tmp: exactly the
 *  non-empty words Go `safetree.Reason` returns (a test holds the two sets
 *  equal). "deadline" is a removal that ran out of the drain's deadline (the
 *  dispose's op deadline, or the abnormal drain's default) and kept its partial
 *  progress. A removed tmp always reports "". */
export const TMP_RETAINED_REASONS: ReadonlySet<string> = new Set(["mismatch", "owner", "deadline", "io", "absent", "name"]);

/** Strict meaning check for the optional `tmpCleanup` evidence field: an object
 *  with exactly `state` and `reason`, where `removed` pairs only with reason ""
 *  and `retained` only with one of the fixed retained reasons. Anything else is a
 *  protocol breach. */
function isValidTmpCleanup(value: unknown): value is TmpCleanupEvidence {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  const keys = Object.keys(record);
  if (keys.length !== 2 || !keys.includes("state") || !keys.includes("reason")) return false;
  if (record.state === "removed") return record.reason === "";
  if (record.state === "retained") return typeof record.reason === "string" && TMP_RETAINED_REASONS.has(record.reason);
  return false;
}

/** Whether `tmpCleanup` may appear on this evidence record at all. The
 *  supervisor removes the tmp only after a confirmed drain, so the field is
 *  legal ONLY on a dispose whose `state` is "drained" and on an abnormal event
 *  whose `cleanup.state` is "drained"; on any other event it is a breach. */
function tmpCleanupAllowed(record: Record<string, unknown>): boolean {
  if (record.event === "dispose") return record.state === "drained";
  if (record.event === "abnormal") {
    const cleanup = record.cleanup;
    return typeof cleanup === "object" && cleanup !== null && !Array.isArray(cleanup)
      && (cleanup as Record<string, unknown>).state === "drained";
  }
  return false;
}

/** The result of a disposal attempt for exactly THIS owned root. `clean` is true ONLY
 *  when the supervisor reported `state:"drained"` AND exited 0. Never a run-level fact. */
export type DisposeOutcome =
  | { readonly clean: true; readonly event: DisposeEvidence }
  | {
    readonly clean: false;
    readonly reason: string;
    readonly event?: DisposeEvidence;
    /** The validated `tmpCleanup` an `abnormal` event carried (its best-effort
     *  drain was confirmed), surfaced so a retained tmp is still reported. */
    readonly tmpCleanup?: TmpCleanupEvidence;
  };

export interface CodexRootHandle {
  /** The `started` evidence (present once `launchCodexRoot` resolves). */
  readonly started: StartedEvidence;
  readonly supervisorPid: number | undefined;
  /** The app-server TRANSPORT (fds 0/1/2) — a caller speaks app-server JSONL RPC here. */
  readonly transport: { readonly stdin: Writable | null; readonly stdout: Readable | null; readonly stderr: Readable | null };
  snapshot(timeoutMs?: number): Promise<SnapshotEvidence>;
  /** Await the supervised primary child's normalized terminal status. */
  waitChild(timeoutMs?: number): Promise<ChildExitEvidence>;
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

/** Default privileged provisioning: create the dirs as the runner uid via setpriv
 * (`runnerCommand`) and write the config 0600, never chown. Trees stay 0700 except
 * for the narrow managed-auth session export posture on RunnerTreeRequest.
 * File content is fed on stdin so it is never embedded in an argv. */
function defaultMakeRunnerTrees(request: RunnerTreeRequest): void {
  // The helper itself runs as the shared runner uid, so it must not inherit the worker
  // process environment: another concurrent runner can read a normal helper process via
  // /proc/<pid>/environ. Only inert locale/path values cross this short provisioning step.
  const provisionEnv: NodeJS.ProcessEnv = { PATH: "/usr/bin:/bin", LANG: "C" };
  const wrap = request.kind === "command" ? commandRootCommand : runnerCommand;
  const mk = wrap("/bin/sh", [
    "-ceu",
    `root="$1"; uid="$2"; shift 2; umask 077; mkdir -m 700 -- "$root"; trap 'rc=$?; [ "$rc" -eq 0 ] || rm -rf -- "$root"; exit "$rc"' EXIT; chmod 700 "$root"; for d in "$@"; do mkdir -m 700 -- "$d"; chmod 700 "$d"; done; for d in "$root" "$@"; do [ "$(stat -c %u "$d")" = "$uid" ] && [ "$(stat -c %a "$d")" = 700 ]; done; trap - EXIT`,
    "sh", request.root, String(request.uid), ...request.dirs,
  ]);
  const r = spawnSync(mk.command, mk.args, { env: provisionEnv, stdio: ["ignore", "ignore", "pipe"] });
  if (r.status !== 0) throw new Error(`runner-owned tree creation failed (exit ${String(r.status)}): ${String(r.stderr)}`);
  try {
    const shared = request.sharedSessionRead;
    if (shared !== undefined) {
      const share = wrap("/bin/sh", [
        "-ceu",
        `root="$1"; codex="$2"; sessions="$3"; uid="$4"; gid="$5"; chgrp "$gid" "$root" "$codex"; chmod 710 "$root" "$codex"; mkdir -m 2750 -- "$sessions"; chgrp "$gid" "$sessions"; chmod 2750 "$sessions"; [ "$(stat -c %u "$root")" = "$uid" ] && [ "$(stat -c %g "$root")" = "$gid" ] && [ "$(stat -c %a "$root")" = 710 ] && [ "$(stat -c %u "$codex")" = "$uid" ] && [ "$(stat -c %g "$codex")" = "$gid" ] && [ "$(stat -c %a "$codex")" = 710 ] && [ "$(stat -c %u "$sessions")" = "$uid" ] && [ "$(stat -c %g "$sessions")" = "$gid" ] && [ "$(stat -c %a "$sessions")" = 2750 ]`,
        "sh", request.root, shared.codexHome, shared.sessionDir, String(request.uid), String(shared.gid),
      ]);
      const sr = spawnSync(share.command, share.args, { env: provisionEnv, stdio: ["ignore", "ignore", "pipe"] });
      if (sr.status !== 0) {
        throw new Error(`runner-owned session export posture failed (exit ${String(sr.status)}): ${String(sr.stderr)}`);
      }
    }
    if (request.sessionSeedDir !== undefined) {
      if (shared === undefined || request.kind !== "provider") {
        throw new Error("runner-owned session seed requires a managed-auth provider tree");
      }
      const seed = wrap("/app/node_modules/.bin/tsx", [
        SESSION_SEED_ENTRYPOINT,
        request.sessionSeedDir,
        shared.sessionDir,
      ]);
      const seeded = spawnSync(seed.command, seed.args, {
        env: {
          PATH: "/usr/local/bin:/usr/bin:/bin",
          LANG: "C",
          HOME: join(request.root, "home"),
          TMPDIR: join(request.root, "tmp"),
        },
        stdio: ["ignore", "ignore", "pipe"],
      });
      if (seeded.status !== 0) {
        throw new Error(`runner-owned session seed failed (exit ${String(seeded.status)})`);
      }
    }
    for (const file of request.files) {
      const octal = (file.mode & 0o777).toString(8).padStart(3, "0");
      const w = wrap("/bin/sh", ["-ceu", `umask 077; cat > "$1"; chmod ${octal} "$1"; [ "$(stat -c %u "$1")" = "$2" ] && [ "$(stat -c %a "$1")" = ${octal} ]`, "sh", file.path, String(request.uid)]);
      const wr = spawnSync(w.command, w.args, { env: provisionEnv, input: file.content, stdio: ["pipe", "ignore", "pipe"] });
      if (wr.status !== 0) throw new Error(`runner-owned file write failed for ${file.path} (exit ${String(wr.status)}): ${String(wr.stderr)}`);
    }
  } catch (error) {
    const rm = wrap("/bin/rm", ["-rf", "--", request.root]);
    spawnSync(rm.command, rm.args, { env: provisionEnv, stdio: "ignore" });
    throw error;
  }
}

/** Remove one private launch tree as the identity that owns it. The caller has
 * already validated the immutable launch spec; re-check the destructive target
 * here so a future caller cannot turn this helper into a broad delete. */
function defaultRemoveRunnerTree(request: Pick<RunnerTreeRequest, "uid" | "kind" | "root">): void {
  if (!isAbsolute(request.root) || resolve(request.root) === sep || resolve(request.root) !== request.root) {
    throw new Error("refusing to remove a non-canonical owned data root");
  }
  const stat = lstatSync(request.root, { throwIfNoEntry: false });
  if (stat === undefined) return;
  if (stat.isSymbolicLink() || !stat.isDirectory() || stat.uid !== request.uid) {
    throw new Error("refusing to remove an unexpected owned data root");
  }
  const wrap = request.kind === "command" ? commandRootCommand : runnerCommand;
  const rm = wrap("/bin/rm", ["-rf", "--", request.root]);
  const cleanupEnv: NodeJS.ProcessEnv = { PATH: "/usr/bin:/bin", LANG: "C" };
  const result = spawnSync(rm.command, rm.args, { env: cleanupEnv, stdio: ["ignore", "ignore", "pipe"] });
  if (result.status !== 0) {
    throw new Error(`runner-owned tree removal failed (exit ${String(result.status)}): ${String(result.stderr)}`);
  }
}

/** Bind the runner-owned tree to the same proven lifecycle as its supervisor.
 * A failed/unconfirmed disposal leaves the tree intact for forensic recovery;
 * a repeated clean dispose never repeats a successful removal. */
function withOwnedTreeCleanup(
  handle: CodexRootHandle,
  request: Pick<RunnerTreeRequest, "uid" | "kind" | "root">,
  remove: RemoveRunnerTree,
  reportFailure: () => void,
): CodexRootHandle {
  let removed = false;
  return {
    started: handle.started,
    supervisorPid: handle.supervisorPid,
    transport: handle.transport,
    snapshot: (timeoutMs) => handle.snapshot(timeoutMs),
    waitChild: (timeoutMs) => handle.waitChild(timeoutMs),
    dispose: async (timeoutMs) => {
      const outcome = await handle.dispose(timeoutMs);
      if (outcome.clean && !removed) {
        // Disposal evidence and disk reclamation are different facts. Once the
        // supervisor proved ECHILD+__WALL and exited 0, a filesystem anomaly must
        // retain the tree for inspection, not rewrite that clean process result or
        // prevent sibling roots from being reaped by the aggregate boundary.
        removed = true;
        try {
          remove(request);
        } catch {
          try { reportFailure(); } catch { /* diagnostics never replace lifecycle evidence */ }
        }
      }
      return outcome;
    },
    get failed() { return handle.failed; },
    whenFailed: handle.whenFailed,
  };
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
  if (!isAbsolute(spec.ownedDataRoot) || resolve(spec.ownedDataRoot) === sep || resolve(spec.ownedDataRoot) !== spec.ownedDataRoot) {
    throw new Error("ownedDataRoot must be a canonical absolute non-root path");
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
  if (spec.useAppServerAuth) {
    if (spec.kind !== "provider") {
      throw new Error("app-server auth is only supported for a provider root");
    }
    if (spec.authMode !== "subscription" && spec.authMode !== "api_key") {
      throw new Error("app-server auth requires an explicit subscription or api_key mode");
    }
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
  // An app-server-auth root delivers its credential over the login RPC, NOT the env: never
  // inject the provider var for it (the credential must not be readable in /proc/<pid>/environ).
  if (spec.kind === "provider" && !spec.useAppServerAuth && spec.provider.credentialValue !== undefined) {
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
  if (deps.appServerAuthOpenAIBaseUrlForTest !== undefined && !spec.useAppServerAuth) {
    throw new Error("Codex loopback test base URL requires app-server auth");
  }
  if (spec.seedSession && (spec.kind !== "provider" || !spec.useAppServerAuth)) {
    throw new Error("Codex session seeding requires a managed-auth provider root");
  }

  // Resolve the intended runner uid <N> for --expect-uid and the runner-owned trees.
  const uid = spec.kind === "command"
    ? (deps.resolveCommandUid ?? (() => COMMAND_UID))()
    : (deps.resolveRunnerUid ?? defaultResolveRunnerUid)();
  if (!Number.isInteger(uid) || uid <= 0) throw new Error(`resolved runner uid is invalid: ${String(uid)}`);

  // 2. Reject unexpected system config (a fresh HOME does not neutralize /etc/codex).
  (deps.assertNoUnexpectedSystemConfig ?? defaultAssertNoUnexpectedSystemConfig)(deps.etcCodexDir);

  // 3. Fresh per-launch trees, runner-owned via the injectable step. Managed auth adds
  // only the dedicated worker+provider group's traverse/read path to the sessions subtree.
  const trees = deriveOwnedTrees(spec.ownedDataRoot);
  const configPath = join(trees.codexHome, "config.toml");
  const sessionSeedDir = join(`${spec.ownedDataRoot}.session-seed`, "sessions");
  // 4. config.toml (native-off; canonical project untrusted), written 0600 as runner. An
  //    app-server-auth provider root emits the PRODUCTION config (the built-in `openai`
  //    provider, no re-declared table that would bypass the login). The explicit M3b-only
  //    dependency instead emits its authenticated HTTP loopback provider. Every other root
  //    keeps the transitional env-key config, byte-identical to before this seam.
  let configText: string;
  if (spec.useAppServerAuth) {
    // validateLaunchContract already refused an absent/invalid authMode; re-narrow for the
    // (non-optional) production builder input rather than assert non-null.
    const authMode = spec.authMode;
    if (authMode !== "subscription" && authMode !== "api_key") {
      throw new Error("app-server auth requires an explicit subscription or api_key mode");
    }
    const configOpts = { model: spec.model, projectPath: spec.cwd, authMode, codeModeHost: spec.codeModeHost ?? false };
    configText = deps.appServerAuthOpenAIBaseUrlForTest === undefined
      ? buildCodexProductionConfigToml(configOpts)
      : buildCodexLoopbackTestConfigToml(configOpts, deps.appServerAuthOpenAIBaseUrlForTest);
  } else {
    configText = buildCodexConfigToml({
      model: spec.model,
      provider: { name: spec.provider.name, baseUrl: spec.provider.baseUrl, envKey: spec.provider.envKey, wireApi: "responses" },
      projectPath: spec.cwd,
    });
  }
  (deps.makeRunnerTrees ?? defaultMakeRunnerTrees)({
    uid,
    kind: spec.kind,
    root: spec.ownedDataRoot,
    dirs: trees.all,
    ...(spec.kind === "provider" && spec.useAppServerAuth
      ? {
          sharedSessionRead: {
            gid: CODEX_SESSION_GID,
            codexHome: trees.codexHome,
            sessionDir: join(trees.codexHome, "sessions"),
          },
        }
      : {}),
    ...(spec.seedSession ? { sessionSeedDir } : {}),
    files: [{ path: configPath, content: configText, mode: 0o600 }],
  });

  // 5. Replaced env allowlist (NOT merged).
  const replacedEnv = buildReplacedEnv(trees, spec);

  // 6. Spawn the supervisor AS the runner uid (compose the UNCHANGED runnerCommand),
  //    with 5-fd stdio (0/1/2 transport, 3 control, 4 evidence). The supervisor argv
  //    is trusted & launcher-fixed: never model-controlled.
  const supervisorArgv = ["--expect-uid", String(uid), "--", spec.codexBin, ...spec.childArgv];
  const wrapped = spec.kind === "command"
    ? commandRootCommand(spec.supervisorBin, supervisorArgv)
    : runnerCommand(spec.supervisorBin, supervisorArgv);
  const child = (deps.spawnSupervisor ?? defaultSpawnSupervisor)(wrapped.command, wrapped.args, {
    cwd: spec.cwd,
    env: replacedEnv,
    stdio: ["pipe", "pipe", "pipe", "pipe", "pipe"],
  });

  // 7. Parse evidence (bounded), await `started`, expose snapshot/dispose + transport.
  const handle = await createHandle(child, uid, { ...DEFAULT_DEADLINES, ...deps.deadlines }, spec.kind);
  return withOwnedTreeCleanup(
    handle,
    { uid, kind: spec.kind, root: spec.ownedDataRoot },
    deps.removeRunnerTree ?? defaultRemoveRunnerTree,
    deps.reportRunnerTreeCleanupFailure
      ?? (() => process.stderr.write("codex runner-owned tree cleanup failed; retaining the tree\n")),
  );
}

/** A generic supervised effect launch. Unlike {@link launchCodexRoot}, it creates
 * no provider HOME/config tree: the trusted caller supplies a fully replaced env
 * and an already-screened absolute executable/argv. */
export interface CodexEffectLaunchSpec {
  readonly identity: "command" | "worker_pat";
  readonly command: string;
  readonly args: readonly string[];
  readonly cwd: string;
  readonly env: NodeJS.ProcessEnv;
  readonly supervisorBin: string;
  /** Optional UUID token naming the supervisor-owned private tmp
   * `/tmp/uzi-codex-command-<token>`: the supervisor creates and locks it before
   * launch and removes it only after a confirmed drain (reported as `tmpCleanup`). */
  readonly cleanupToken?: string;
}

export async function launchCodexEffectRoot(
  spec: CodexEffectLaunchSpec,
  deps: LauncherDeps = {},
): Promise<CodexRootHandle> {
  const profileEnv = deps.env ?? process.env;
  if (!uidSplitActive(profileEnv)) {
    throw new CodexUnsupportedProfileError("Codex effect roots require the A1 uid split; refusing to launch");
  }
  if (spec.supervisorBin !== SUPERVISOR_BIN) {
    throw new Error(`supervisorBin must equal the immutable image path ${SUPERVISOR_BIN}`);
  }
  if (!isAbsolute(spec.command) || !isAbsolute(spec.cwd)) {
    throw new Error("effect command and cwd must be absolute paths");
  }
  const expectedUid = spec.identity === "command"
    ? (deps.resolveCommandUid ?? (() => COMMAND_UID))()
    : (deps.resolveWorkerUid ?? (() => process.getuid?.() ?? WORKER_UID))();
  const requiredUid = spec.identity === "command" ? COMMAND_UID : WORKER_UID;
  if (expectedUid !== requiredUid) {
    throw new Error(`effect identity resolved unexpected uid ${String(expectedUid)}`);
  }
  if (spec.cleanupToken !== undefined && !LOWERCASE_UUID_RE.test(spec.cleanupToken)) {
    throw new Error("effect cleanup token must be a lowercase UUID");
  }
  const supervisorArgv = [
    "--expect-uid", String(expectedUid),
    ...(spec.cleanupToken ? ["--cleanup-token", spec.cleanupToken] : []),
    ...(spec.identity === "worker_pat" ? ["--drop-controller-caps"] : []),
    "--", spec.command, ...spec.args,
  ];
  const wrapped = spec.identity === "command"
    ? commandRootCommand(spec.supervisorBin, supervisorArgv)
    : workerBoundaryCommand(spec.supervisorBin, supervisorArgv);
  const child = (deps.spawnSupervisor ?? defaultSpawnSupervisor)(wrapped.command, wrapped.args, {
    cwd: spec.cwd,
    env: { ...spec.env },
    stdio: ["pipe", "pipe", "pipe", "pipe", "pipe"],
  });
  return createHandle(child, expectedUid, { ...DEFAULT_DEADLINES, ...deps.deadlines }, "command");
}

async function createHandle(
  child: SupervisorProcess,
  expectedUid: number,
  deadlines: LauncherDeadlines,
  kind: CodexRootKind,
): Promise<CodexRootHandle> {
  const control = asWritable(child.stdio[3], "control");
  const evidence = asReadable(child.stdio[4], "evidence");

  let nextId = 1;
  const pending = new Map<number, { resolve: (e: SnapshotEvidence | DisposeEvidence) => void; reject: (err: Error) => void }>();
  let failure: Error | undefined;
  let startedEvent: StartedEvidence | undefined;
  let childExitEvent: ChildExitEvidence | undefined;
  let exited = false;
  let exitInfo: { code: number | null; signal: NodeJS.Signals | null } | undefined;
  let disposeInFlight = false;
  let cleanDisposed = false;
  let lastDrained: DisposeEvidence | undefined;
  let abnormalTmpCleanup: TmpCleanupEvidence | undefined;
  let lineCount = 0;

  let resolveStarted!: (e: StartedEvidence) => void;
  let rejectStarted!: (err: Error) => void;
  const startedPromise = new Promise<StartedEvidence>((resolve, reject) => { resolveStarted = resolve; rejectStarted = reject; });
  let resolveChildExit!: (e: ChildExitEvidence) => void;
  let rejectChildExit!: (err: Error) => void;
  const childExitPromise = new Promise<ChildExitEvidence>((resolve, reject) => {
    resolveChildExit = resolve;
    rejectChildExit = reject;
  });
  void childExitPromise.catch(() => undefined);
  let resolveExit!: (info: { code: number | null; signal: NodeJS.Signals | null }) => void;
  const exitPromise = new Promise<{ code: number | null; signal: NodeJS.Signals | null }>((resolve) => { resolveExit = resolve; });
  let resolveFailed!: (err: Error) => void;
  const whenFailed = new Promise<Error>((resolve) => { resolveFailed = resolve; });

  function fail(err: Error): void {
    if (!failure) {
      failure = err;
      resolveFailed(err);
      if (!startedEvent) rejectStarted(err);
      if (!childExitEvent) rejectChildExit(err);
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
    if ("tmpCleanup" in record && (!tmpCleanupAllowed(record) || !isValidTmpCleanup(record.tmpCleanup))) {
      fail(new Error("malformed tmpCleanup evidence"));
      return;
    }
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
      case "child_exit": {
        const code = record.code;
        if (!Number.isInteger(code) || Number(code) < 0 || Number(code) > 255 || childExitEvent) {
          fail(new Error("malformed or duplicate child_exit evidence"));
          return;
        }
        const ev: ChildExitEvidence = { event: "child_exit", code: Number(code) };
        childExitEvent = ev;
        resolveChildExit(ev);
        if (kind === "provider") {
          fail(new Error(`supervised provider child exited unexpectedly (code=${ev.code})`));
        }
        return;
      }
      case "abnormal": {
        // Validated above: record it so the unclean dispose outcome still reports it.
        if ("tmpCleanup" in record) abnormalTmpCleanup = record.tmpCleanup as TmpCleanupEvidence;
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

  async function waitChild(timeoutMs = deadlines.exit): Promise<ChildExitEvidence> {
    if (childExitEvent) return childExitEvent;
    if (failure) throw failure;
    return withDeadline(childExitPromise, timeoutMs, "supervised child exit");
  }

  /** Every unclean outcome carries the tmpCleanup an abnormal event reported, however
   *  the disposal came to be unclean (the abnormal may land before or during it). */
  async function dispose(timeoutMs?: number): Promise<DisposeOutcome> {
    const outcome = await disposeOnce(timeoutMs);
    if (outcome.clean || !abnormalTmpCleanup) return outcome;
    return { ...outcome, tmpCleanup: abnormalTmpCleanup };
  }

  async function disposeOnce(timeoutMs = 2000): Promise<DisposeOutcome> {
    if (cleanDisposed && lastDrained) return { clean: true, event: lastDrained };
    if (failure) return { clean: false, reason: failure.message };
    if (exited) return { clean: false, reason: `supervisor already exited (code=${String(exitInfo?.code)})` };
    if (control.destroyed) return { clean: false, reason: "control channel unavailable; disposal unconfirmed" };
    disposeInFlight = true;
    const deadlineAt = Date.now() + Math.max(0, timeoutMs);
    const remaining = (): number => Math.max(0, Math.ceil(deadlineAt - Date.now()));
    try {
      const id = nextId++;
      // Keep part of the caller's total budget for observing the supervisor's clean exit
      // after it reports drained. Giving drain the entire budget makes a successful drain
      // race an immediate 0ms exit-confirmation timeout.
      const exitReserve = Math.min(deadlines.exit, Math.max(1, Math.floor(remaining() / 5)));
      const drainBudget = Math.max(0, remaining() - exitReserve);
      let ev: SnapshotEvidence | DisposeEvidence;
      try {
        ev = await withDeadline(
          sendAndWait(id, { op: "dispose", id, timeoutMs: drainBudget }),
          Math.min(remaining(), drainBudget),
          "dispose",
        );
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
        exit = await withDeadline(exitPromise, Math.min(remaining(), exitReserve), "supervisor exit");
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
    waitChild,
    dispose,
    get failed() { return failure; },
    whenFailed,
  };
}

// ─── Issue #1598: the per-run command cache and the startup orphan reaper ────────
//
// Three STANDALONE supervisor modes (agent/codex/supervisor/doc.go, "Standalone modes"),
// each run as the command uid (COMMAND_UID, via commandRootCommand) with a minimal
// replaced env. None of them uses fd 3/4: each writes JSON lines to stdout, which is
// parsed strictly here: a bounded line length, a bounded line count, exactly the key
// set of a known event, and an exit code consistent with that event. Anything else
// fails closed (an error value, never a guessed success).

/** The worker-only per-run command cache root (entrypoint-prepared, uid 10003, 0700,
 *  present only under the uid split; a k8s emptyDir). Each run's cache is
 *  `<root>/<lowercase uuid>` with the subdirectories gomod, gocache and npm. */
export const CODEX_COMMAND_CACHE_ROOT = "/var/cache/uzi-codex-cmd";

/** The standalone modes print short fixed-shape lines; a line past this is garbage. */
const MAX_MODE_LINE_BYTES = 4096;
/** A reap pass may take up to its own 5-minute budget plus the proof scans. */
const DEFAULT_REAP_TIMEOUT_MS = 6 * 60 * 1000;
/** `--remove-cache` removes one run's cache with no deadline of its own. */
export const DEFAULT_REMOVE_CACHE_TIMEOUT_MS = 2 * 60 * 1000;
/** The holder creates and locks one directory before it reports ready. */
const DEFAULT_HOLDER_READY_TIMEOUT_MS = 10 * 1000;
/** The only env a standalone mode sees: nothing from the worker crosses. */
const STANDALONE_MODE_ENV: NodeJS.ProcessEnv = { PATH: "/usr/bin:/bin", LANG: "C" };

const REAP_PROOFS: ReadonlySet<string> = new Set(["held", "unknown", "user_alive"]);
const REAP_ERROR_REASONS: ReadonlySet<string> = new Set(["fd_hygiene", "args", "uid", "tmp_root", "cache_root", "list"]);
const HOLD_ERROR_REASONS: ReadonlySet<string> = new Set([
  "dumpable", "fd_hygiene", "args", "uid", "root", "create", "subdir", "lock", "recheck",
]);
const REMOVE_ERROR_REASONS: ReadonlySet<string> = new Set(["fd_hygiene", "args", "uid", "root"]);
/** A holder retains with "unattested" or a tmpCleanup word; --remove-cache with "live" or one. */
const HOLD_RETAINED_REASONS: ReadonlySet<string> = new Set([...TMP_RETAINED_REASONS, "unattested"]);
const REMOVE_RETAINED_REASONS: ReadonlySet<string> = new Set([...TMP_RETAINED_REASONS, "live"]);

/** The process surface a standalone mode needs. Node's ChildProcess satisfies it
 *  structurally; unit tests inject a fake. `close` fires with (code, signal) once the
 *  process exited AND its stdio closed; `exit` fires with (code, signal) at exit. */
export interface StandaloneModeProcess {
  readonly pid?: number;
  readonly stdin: Writable | null;
  readonly stdout: Readable | null;
  once(event: string, listener: (...args: never[]) => void): unknown;
  on(event: string, listener: (...args: never[]) => void): unknown;
}
export type SpawnStandaloneMode = (
  command: string,
  args: readonly string[],
  options: { cwd: string; env: NodeJS.ProcessEnv; stdio: readonly ("pipe" | "ignore")[] },
) => StandaloneModeProcess;

export interface StandaloneModeDeps {
  readonly spawn?: SpawnStandaloneMode;
  /** Overrides the mode's default bound (ms). */
  readonly timeoutMs?: number;
  /** Reaper only: signal a reaper still running at its deadline (or on abort). The worker
   *  lacks CAP_KILL over uid 10003, so the default forks a `kill -KILL` as the command uid.
   *  Returns whether the kill was delivered (a thrown error counts as not delivered). */
  readonly killAsCommandUid?: (pid: number) => boolean;
  /** Reaper only: the synchronous runner of the default kill (production: spawnSync). */
  readonly runKill?: (command: string, args: readonly string[]) => { readonly status: number | null; readonly error?: Error };
  /** Reaper only: how long to wait for a killed reaper's `close` (default 10 s). */
  readonly killWaitMs?: number;
  /** Reaper only: abort the pass (worker shutdown). The reaper is killed as on a timeout. */
  readonly signal?: AbortSignal;
}

const defaultSpawnStandaloneMode: SpawnStandaloneMode = (command, args, options) =>
  spawn(command, [...args], { cwd: options.cwd, env: options.env, stdio: [...options.stdio] }) as unknown as StandaloneModeProcess;

/** How long a killed reaper gets to close before it is reported unkilled. */
const DEFAULT_REAPER_KILL_WAIT_MS = 10 * 1000;

function defaultKillAsCommandUid(pid: number, runKill: NonNullable<StandaloneModeDeps["runKill"]>): boolean {
  if (!Number.isInteger(pid) || pid <= 1) return false;
  const kill = commandRootCommand("kill", ["-KILL", String(pid)]);
  const result = runKill(kill.command, kill.args);
  return result.error === undefined && result.status === 0;
}

const defaultRunKill: NonNullable<StandaloneModeDeps["runKill"]> = (command, args) =>
  spawnSync(command, [...args], { env: STANDALONE_MODE_ENV, stdio: "ignore", timeout: 10_000 });

function spawnStandaloneMode(
  mode: "--reap-orphans" | "--hold-cache" | "--remove-cache",
  token: string | undefined,
  stdin: "pipe" | "ignore",
  deps: StandaloneModeDeps,
): StandaloneModeProcess {
  const args = [
    mode,
    "--expect-uid", String(COMMAND_UID),
    "--cache-root", CODEX_COMMAND_CACHE_ROOT,
    ...(token === undefined ? [] : ["--cache-token", token]),
  ];
  const wrapped = commandRootCommand(SUPERVISOR_BIN, args);
  return (deps.spawn ?? defaultSpawnStandaloneMode)(wrapped.command, wrapped.args, {
    cwd: "/",
    env: { ...STANDALONE_MODE_ENV },
    stdio: [stdin, "pipe", "ignore"],
  });
}

/** Split a mode's stdout into bounded lines. Any breach (an over-long line, more lines
 *  than the mode may print, a trailing partial line, a stream error) is reported once and
 *  every later byte is ignored, so garbage can never be half-believed. */
function readModeLines(
  stream: Readable,
  maxLines: number,
  onLine: (line: string) => void,
  onBreach: (why: string) => void,
): void {
  let pending = Buffer.alloc(0);
  let lines = 0;
  let broken = false;
  const breach = (why: string): void => {
    if (broken) return;
    broken = true;
    onBreach(why);
  };
  stream.on("data", (chunk: Buffer | string) => {
    if (broken) return;
    pending = Buffer.concat([pending, typeof chunk === "string" ? Buffer.from(chunk, "utf8") : chunk]);
    for (;;) {
      const nl = pending.indexOf(0x0a);
      if (nl < 0) break;
      const line = pending.subarray(0, nl);
      pending = pending.subarray(nl + 1);
      if (line.length > MAX_MODE_LINE_BYTES) { breach("oversized line"); return; }
      lines += 1;
      if (lines > maxLines) { breach("too many lines"); return; }
      onLine(line.toString("utf8"));
      if (broken) return;
    }
    if (pending.length > MAX_MODE_LINE_BYTES) breach("oversized line");
  });
  stream.on("end", () => { if (pending.length > 0) breach("truncated line"); });
  stream.on("error", () => breach("stream error"));
}

/** Parse one line as a JSON object with EXACTLY `keys`; undefined on anything else. */
function parseExactObject(line: string, keys: readonly string[]): Record<string, unknown> | undefined {
  let value: unknown;
  try { value = JSON.parse(line); } catch { return undefined; }
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  const got = Object.keys(record);
  if (got.length !== keys.length || !keys.every((k) => got.includes(k))) return undefined;
  return record;
}

function eventOf(line: string): unknown {
  try {
    const value: unknown = JSON.parse(line);
    return typeof value === "object" && value !== null ? (value as Record<string, unknown>).event : undefined;
  } catch {
    return undefined;
  }
}

const isCount = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;

function validToken(token: string): boolean {
  return typeof token === "string" && LOWERCASE_UUID_RE.test(token);
}

/** The settled state of one one-line mode run. A `timeout` or `aborted` run is still
 *  running (or its stdio still open): `closed` resolves at its `close`. */
type OneLineRun =
  | { readonly kind: "done"; readonly line: string | undefined; readonly code: number | null; readonly breach?: string }
  | StillRunningMode
  | { readonly kind: "spawn"; readonly message: string };
type StillRunningMode =
  | { readonly kind: "timeout"; readonly pid?: number; readonly closed: Promise<void> }
  | { readonly kind: "aborted"; readonly pid?: number; readonly closed: Promise<void> };

/** One run of a one-line mode: spawn, read at most two lines (a second is a breach),
 *  and settle at `close`, the deadline, or an abort of `signal`. An already-aborted
 *  signal spawns nothing. Never throws. */
function runOneLineMode(
  mode: "--reap-orphans" | "--remove-cache",
  token: string | undefined,
  timeoutMs: number,
  deps: StandaloneModeDeps,
  signal?: AbortSignal,
): Promise<OneLineRun> {
  return new Promise((resolve) => {
    if (signal?.aborted) {
      resolve({ kind: "aborted", closed: Promise.resolve() });
      return;
    }
    let settled = false;
    let child: StandaloneModeProcess | undefined;
    const lines: string[] = [];
    let breach: string | undefined;
    let markClosed!: () => void;
    const closed = new Promise<void>((r) => { markClosed = r; });
    const onAbort = (): void => settle({ kind: "aborted", pid: child?.pid, closed });
    const settle = (value: OneLineRun): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      resolve(value);
    };
    const timer = setTimeout(() => settle({ kind: "timeout", pid: child?.pid, closed }), timeoutMs);
    if (typeof timer.unref === "function") timer.unref();
    signal?.addEventListener("abort", onAbort, { once: true });
    try {
      child = spawnStandaloneMode(mode, token, "ignore", deps);
    } catch (error) {
      settle({ kind: "spawn", message: error instanceof Error ? error.message : String(error) });
      return;
    }
    child.on("error", (err: Error) => settle({ kind: "spawn", message: err.message }));
    if (!child.stdout) {
      settle({ kind: "spawn", message: "no stdout channel" });
      return;
    }
    readModeLines(child.stdout, 1, (line) => lines.push(line), (why) => { breach = why; });
    child.once("close", (code: number | null) => {
      markClosed();
      settle({ kind: "done", line: lines.length === 1 ? lines[0] : undefined, code, ...(breach ? { breach } : {}) });
    });
  });
}

/** Resolve true if `p` settles within `ms`, false otherwise. */
async function settlesWithin(p: Promise<void>, ms: number): Promise<boolean> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      p.then(() => true),
      new Promise<boolean>((resolve) => {
        timer = setTimeout(() => resolve(false), Math.max(0, ms));
        if (typeof timer.unref === "function") timer.unref();
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

/** Kill a reaper that outlived its deadline or was aborted, as the command uid, and
 *  wait (bounded) for its `close`. A reaper that did not close is `timeout_unkilled`:
 *  it may still be scanning, so the caller must not launch a run. */
async function stopReaper(
  run: StillRunningMode,
  deps: StandaloneModeDeps,
): Promise<ReapOrphansResult> {
  let killed = false;
  if (run.pid !== undefined) {
    const kill = deps.killAsCommandUid ?? ((pid: number) => defaultKillAsCommandUid(pid, deps.runKill ?? defaultRunKill));
    try { killed = kill(run.pid) === true; } catch { killed = false; }
  }
  const killNote = run.pid !== undefined && !killed ? "kill not delivered" : undefined;
  if (await settlesWithin(run.closed, deps.killWaitMs ?? DEFAULT_REAPER_KILL_WAIT_MS)) {
    return { ok: false, reason: run.kind, ...(killNote ? { detail: killNote } : {}) };
  }
  return {
    ok: false,
    reason: "timeout_unkilled",
    detail: `${run.kind}; ${killNote ?? "kill delivered"}; the reaper did not exit`,
  };
}

/** The parsed startup reap. `ok:false` carries a reap_error reason or one of the
 *  worker-side reasons "timeout" / "aborted" (the reaper was killed and closed), "spawn",
 *  "protocol" (garbage, a missing line or an exit code inconsistent with the line), or
 *  "timeout_unkilled" (a timed-out or aborted reaper did not close after the kill, so it
 *  may still be running: no run may start). On `ok:true`, `truncated` means the pass
 *  budget stopped scanning or deletion early, so orphans may remain for a later startup,
 *  and `direntsExamined` (never below `scanned`) counts the entries the pass read. */
export type ReapOrphansResult =
  | {
      readonly ok: true;
      readonly scanned: number;
      readonly live: number;
      readonly removed: number;
      readonly retained: number;
      readonly foreign: number;
      readonly proof: "held" | "unknown" | "user_alive";
      readonly truncated: boolean;
      readonly direntsExamined: number;
    }
  | { readonly ok: false; readonly reason: string; readonly detail?: string };

/**
 * Run one `--reap-orphans` pass as the command uid and parse its one line.
 *
 * INVARIANT (the reaper's proof assumes it): call this ONLY before the worker launches
 * any run, in the container's fresh PID namespace. A reaper still running at the
 * deadline, or when `deps.signal` aborts, is SIGKILLed as the command uid (a killed pass
 * leaves its partial progress and releases its candidate locks) and awaited (bounded,
 * 10 s by default) until it closes. One that does not close is `timeout_unkilled`, and
 * the caller must not launch a run. Bounded (6 minutes plus the kill wait by default);
 * never throws.
 */
export async function reapCodexCommandOrphans(deps: StandaloneModeDeps = {}): Promise<ReapOrphansResult> {
  try {
    const run = await runOneLineMode("--reap-orphans", undefined, deps.timeoutMs ?? DEFAULT_REAP_TIMEOUT_MS, deps, deps.signal);
    if (run.kind === "spawn") return { ok: false, reason: "spawn", detail: run.message };
    if (run.kind === "timeout" || run.kind === "aborted") return await stopReaper(run, deps);
    if (run.breach !== undefined || run.line === undefined) {
      return { ok: false, reason: "protocol", detail: run.breach ?? "no result line" };
    }
    const event = eventOf(run.line);
    if (event === "reap") {
      const r = parseExactObject(run.line, [
        "event", "scanned", "live", "removed", "retained", "foreign", "proof", "truncated", "dirents_examined",
      ]);
      if (r && isCount(r.scanned) && isCount(r.live) && isCount(r.removed) && isCount(r.retained) && isCount(r.foreign)
        && r.scanned === r.live + r.removed + r.retained + r.foreign
        && typeof r.proof === "string" && REAP_PROOFS.has(r.proof)
        && typeof r.truncated === "boolean" && isCount(r.dirents_examined) && r.scanned <= r.dirents_examined
        && run.code === 0) {
        return {
          ok: true,
          scanned: r.scanned,
          live: r.live,
          removed: r.removed,
          retained: r.retained,
          foreign: r.foreign,
          proof: r.proof as "held" | "unknown" | "user_alive",
          truncated: r.truncated,
          direntsExamined: r.dirents_examined,
        };
      }
    } else if (event === "reap_error") {
      const r = parseExactObject(run.line, ["event", "reason"]);
      if (r && typeof r.reason === "string" && REAP_ERROR_REASONS.has(r.reason) && run.code === 2) {
        return { ok: false, reason: r.reason };
      }
    }
    return { ok: false, reason: "protocol", detail: "unexpected result line or exit code" };
  } catch (error) {
    return { ok: false, reason: "protocol", detail: error instanceof Error ? error.message : String(error) };
  }
}

/** The outcome of removing one run's cache. `removed`/`absent` carry reason "".
 *  `retained` carries the supervisor's word. `pending` is a holder release or a
 *  `--remove-cache` that did not answer in time (the process is not killed: it keeps
 *  running and settles on its own). `error` is a worker-side failure ("spawn",
 *  "protocol", "exited", "token") or a cache_error reason. */
export interface CacheCleanupResult {
  readonly state: "removed" | "absent" | "retained" | "pending" | "error";
  readonly reason: string;
}

/** Parse a cache_cleanup / cache_error line against the mode's vocabulary. */
function parseCacheOutcome(
  line: string,
  code: number | null | undefined,
  mode: "hold" | "remove",
): CacheCleanupResult | undefined {
  const event = eventOf(line);
  if (event === "cache_cleanup") {
    const r = parseExactObject(line, ["event", "state", "reason"]);
    if (!r || typeof r.reason !== "string") return undefined;
    const retainedWords = mode === "hold" ? HOLD_RETAINED_REASONS : REMOVE_RETAINED_REASONS;
    // `code` is undefined when the caller has the line but not (yet) the exit status.
    const codeOk = (want: number): boolean => code === undefined || code === want;
    if (r.state === "removed" && r.reason === "" && codeOk(0)) return { state: "removed", reason: "" };
    if (mode === "remove" && r.state === "absent" && r.reason === "" && codeOk(0)) return { state: "absent", reason: "" };
    if (r.state === "retained" && retainedWords.has(r.reason) && codeOk(3)) return { state: "retained", reason: r.reason };
    return undefined;
  }
  if (event === "cache_error") {
    const r = parseExactObject(line, ["event", "reason"]);
    const words = mode === "hold" ? HOLD_ERROR_REASONS : REMOVE_ERROR_REASONS;
    if (r && typeof r.reason === "string" && words.has(r.reason) && (code === undefined || code === 2)) {
      return { state: "error", reason: r.reason };
    }
  }
  return undefined;
}

/**
 * Remove one run's cache whose holder died mid-run, as the command uid. The caller
 * attests that every command root that used the cache drained; the supervisor still
 * refuses a held lock ("retained"/"live"). Bounded (2 minutes by default; a slower
 * remover is "pending", never killed); never throws.
 */
export async function removeCommandCache(token: string, deps: StandaloneModeDeps = {}): Promise<CacheCleanupResult> {
  if (!validToken(token)) return { state: "error", reason: "token" };
  try {
    const run = await runOneLineMode("--remove-cache", token, deps.timeoutMs ?? DEFAULT_REMOVE_CACHE_TIMEOUT_MS, deps);
    if (run.kind === "spawn") return { state: "error", reason: "spawn" };
    // Not killed: the remover keeps running and removes or retains on its own.
    if (run.kind !== "done") return { state: "pending", reason: "" };
    if (run.breach !== undefined || run.line === undefined) return { state: "error", reason: "protocol" };
    return parseCacheOutcome(run.line, run.code, "remove") ?? { state: "error", reason: "protocol" };
  } catch {
    return { state: "error", reason: "protocol" };
  }
}

/** Thrown by {@link startCommandCacheHolder} when no usable cache is held. `reason` is a
 *  cache_error word or one of "token", "spawn", "timeout", "protocol", "exited". */
export class CommandCacheStartError extends Error {
  readonly reason: string;
  constructor(reason: string) {
    super(`codex command cache holder did not start (${reason})`);
    this.name = "CommandCacheStartError";
    this.reason = reason;
  }
}

/** A live per-run command cache held by a `--hold-cache` process for the whole run. */
export interface CommandCacheHolder {
  readonly token: string;
  /** `<CODEX_COMMAND_CACHE_ROOT>/<token>`. */
  readonly path: string;
  /** True while the holder runs, has printed nothing past ready, and was not released.
   *  Once false it never becomes true again, and the holder's stdin is ended then (so a
   *  holder that was not released sees EOF and retains its directory as "unattested"). */
  alive(): boolean;
  /**
   * Write the one release line (only to a holder still alive), end stdin, and wait
   * (bounded) for the holder's `close`: its outcome is the cleanup line checked against
   * the exit code (removed means 0, retained means 3). A timeout returns
   * `{state:"pending"}` WITHOUT killing the holder: it keeps running and removes or
   * retains on its own. Idempotent: a second call returns the first result. Never throws.
   */
  release(drained: boolean, timeoutMs: number): Promise<CacheCleanupResult>;
}

/**
 * Start the per-run cache holder as the command uid and wait (bounded, 10 s by default)
 * for `cache_ready` naming exactly `<root>/<token>`.
 *
 * FAILURE CONTRACT: this THROWS a {@link CommandCacheStartError} (never returns a
 * half-usable handle) on an invalid token, a spawn failure, a cache_error, garbage, an
 * early exit or the deadline. On every failure it ends the holder's stdin, so a holder
 * that did get ready sees EOF without a release and retains the directory as
 * "unattested" for the startup reaper; nothing is killed.
 */
export async function startCommandCacheHolder(token: string, deps: StandaloneModeDeps = {}): Promise<CommandCacheHolder> {
  if (!validToken(token)) throw new CommandCacheStartError("token");
  const path = `${CODEX_COMMAND_CACHE_ROOT}/${token}`;
  let child: StandaloneModeProcess;
  try {
    child = spawnStandaloneMode("--hold-cache", token, "pipe", deps);
  } catch {
    throw new CommandCacheStartError("spawn");
  }
  const stdin = child.stdin;
  // An EPIPE on a dead holder's stdin must never become an unhandled 'error'.
  stdin?.on("error", () => undefined);
  const endStdin = (): void => {
    try { if (stdin && !stdin.destroyed && !stdin.writableEnded) stdin.end(); } catch { /* holder already gone */ }
  };

  let ready = false;
  // `exit` makes the holder not alive at once; its OUTCOME waits for `close`, which Node
  // emits only after stdout is drained (an `exit` can precede the cleanup line's data).
  let exitedFlag = false;
  let closed = false;
  let closeCode: number | null = null;
  let breach: string | undefined;
  let afterReady: string | undefined;
  let released = false;
  const waiters = new Set<() => void>();
  const notify = (): void => { for (const w of waiters) w(); };

  let resolveReady!: () => void;
  let rejectReady!: (e: CommandCacheStartError) => void;
  const readyPromise = new Promise<void>((resolve, reject) => { resolveReady = resolve; rejectReady = reject; });
  // A late rejection (an exit after the deadline already won the race) is expected.
  readyPromise.catch(() => undefined);

  const onBreach = (why: string): void => {
    breach = why;
    if (!ready) rejectReady(new CommandCacheStartError("protocol"));
    // Fail closed: an unreadable holder gets EOF with no release and retains.
    endStdin();
    notify();
  };
  const onLine = (line: string): void => {
    if (!ready) {
      const event = eventOf(line);
      if (event === "cache_ready") {
        const r = parseExactObject(line, ["event", "path"]);
        if (r && r.path === path) { ready = true; resolveReady(); return; }
        onBreach("bad ready line");
        return;
      }
      const outcome = parseCacheOutcome(line, undefined, "hold");
      rejectReady(new CommandCacheStartError(outcome?.state === "error" ? outcome.reason : "protocol"));
      endStdin();
      return;
    }
    // Any line past ready makes the holder not alive: end stdin so an unreleased holder
    // sees EOF (and retains) instead of holding its lock until the worker exits.
    afterReady = line;
    endStdin();
    notify();
  };

  child.on("error", () => {
    if (!ready) rejectReady(new CommandCacheStartError("spawn"));
    endStdin();
    notify();
  });
  child.once("exit", () => {
    exitedFlag = true;
    if (!ready) rejectReady(new CommandCacheStartError("exited"));
    endStdin();
    notify();
  });
  child.once("close", (code: number | null) => {
    exitedFlag = true;
    closed = true;
    closeCode = code;
    if (!ready) rejectReady(new CommandCacheStartError("exited"));
    notify();
  });
  if (!child.stdout || !stdin) {
    endStdin();
    throw new CommandCacheStartError("spawn");
  }
  readModeLines(child.stdout, 2, onLine, onBreach);

  const timeoutMs = deps.timeoutMs ?? DEFAULT_HOLDER_READY_TIMEOUT_MS;
  let timer: NodeJS.Timeout | undefined;
  try {
    await Promise.race([
      readyPromise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new CommandCacheStartError("timeout")), timeoutMs);
        if (typeof timer.unref === "function") timer.unref();
      }),
    ]);
  } catch (error) {
    endStdin();
    throw error instanceof CommandCacheStartError ? error : new CommandCacheStartError("protocol");
  } finally {
    if (timer) clearTimeout(timer);
  }

  const isAlive = (): boolean => !exitedFlag && breach === undefined && afterReady === undefined;
  /** The holder's final outcome, or undefined while it has not closed. A breach (garbage,
   *  or a line past the 2-line cap) is final at once. */
  const settledOutcome = (): CacheCleanupResult | undefined => {
    if (breach !== undefined) return { state: "error", reason: "protocol" };
    if (!closed) return undefined;
    if (afterReady === undefined) return { state: "error", reason: "exited" };
    return parseCacheOutcome(afterReady, closeCode, "hold") ?? { state: "error", reason: "protocol" };
  };

  let releasePromise: Promise<CacheCleanupResult> | undefined;
  return {
    token,
    path,
    alive: () => !released && isAlive(),
    release: (drained: boolean, releaseTimeoutMs: number): Promise<CacheCleanupResult> => {
      if (releasePromise) return releasePromise;
      releasePromise = new Promise<CacheCleanupResult>((resolve) => {
        const wasAlive = isAlive();
        released = true;
        if (wasAlive) {
          try {
            stdin.write(`${JSON.stringify({ op: "release", drained: drained === true })}\n`);
          } catch { /* the EOF below still settles the holder (unattested) */ }
        }
        endStdin();
        let releaseTimer: NodeJS.Timeout | undefined;
        const check = (): void => {
          const outcome = settledOutcome();
          if (outcome === undefined) return;
          waiters.delete(check);
          if (releaseTimer) clearTimeout(releaseTimer);
          resolve(outcome);
        };
        waiters.add(check);
        releaseTimer = setTimeout(() => {
          waiters.delete(check);
          resolve({ state: "pending", reason: "" });
        }, Math.max(0, releaseTimeoutMs));
        if (typeof releaseTimer.unref === "function") releaseTimer.unref();
        check();
      });
      return releasePromise;
    },
  };
}
