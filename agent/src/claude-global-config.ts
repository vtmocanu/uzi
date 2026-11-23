// Prepare the Claude CLI's config directory before every CLI start (PRD #4416).
//
// Threat. The pinned Claude CLI honours an `env` block in its global config (for example
// OTEL_LOG_RAW_API_BODIES, NODE_OPTIONS or ANTHROPIC_BASE_URL), regardless of `settingSources: []`.
// The CLI runs as the runner uid and so does the agent's Bash tool, so an agent that can write
// the config can change the next CLI start's environment, with the provider credential already in
// it. An earlier form of this module replaced `<HOME>/.claude.json` with fixed bytes before each
// spawn and removed `<HOME>/.claude/.config.json`. Review found that the removal followed a
// symlinked `.claude` out of HOME, and that a live runner-uid process could rewrite either file
// between the reset and the CLI's read, or while the CLI ran, or place `.claude/remote-settings.json`.
//
// Policy. The CLI is pointed at a directory the runner cannot change, with CLAUDE_CONFIG_DIR
// (sdk-env.ts sets it to claudeConfigDir(HOME), a protected key; spawnDetached refuses to start a
// CLI whose CLAUDE_CONFIG_DIR is anything else). The CLI then reads `<dir>/.claude.json`; in the
// probes an env block planted in `<HOME>/.claude.json`, `<HOME>/.claude/.config.json` or
// `<HOME>/.claude/settings.json` was not applied (with controls), nor one in an unsigned
// `<HOME>/.claude/remote-settings.json` (fake-key session, no positive control; ADR-4416). The
// directory holds exactly:
//   .claude.json   the fixed bytes CLAUDE_GLOBAL_CONFIG_BYTES, mode 0444
//   projects       symlink to ../.claude/projects  (transcripts, so resume and -c keep working)
//   tasks          symlink to ../.claude/tasks     (the task-list tools need a writable tasks dir)
// and is worker-owned, never group- or world-writable (a setgid bit inherited from HOME is
// tolerated): mode 0755 under the uid split (the runner cannot write it), 0555 with one uid (the CLI
// is then the directory's owner and would otherwise write it; with 0555 the pinned CLI saves and
// creates nothing, see the probes below, so the next start finds it unchanged and reuses it).
// Only those two links are made: session-env and shell-snapshots (sourced by Bash),
// plugins, hooks, skills, commands and agents (configuration and code) are deliberately not linked,
// so the CLI finds them absent in a directory it cannot create them in. Only ONE of `.claude.json`
// and `.config.json` is present, because the CLI prints a warning when both exist and prefers
// `.config.json`.
//
// Probes (pinned CLI 2.1.325, fake API): with a non-writable directory of exactly this shape the
// CLI starts, its saves fail silently with no temp files, Bash/Read/Write/Edit work, transcripts
// land in `<HOME>/.claude/projects` through the link, and `--resume <id>` and `-c` find them;
// TaskCreate works only with the `tasks` link (target writable). A directory the CLI CAN write
// (one uid, mode 0755) gains backups/, plugins/, sessions/, .last-cleanup and a rewritten
// `.claude.json`, and gives no protection: it renames over 0444 files. In `-p` probes against a
// fake API (no completed turn), an `env` block written to the config while the CLI ran was not
// applied, and an unsigned remote-settings env block was not applied (no positive control); the
// scope of both is in ADR-4416.
//
// Boundary. Under the uid split (uidSplitActive) HOME must be a real directory owned by the
// worker uid; this module adds the sticky bit to it, so the group-writable runner can create
// entries in HOME but cannot rename, replace or unlink the worker-owned config directory, and the
// directory itself grants the runner no write. It also refuses a HOME whose parent is
// world-writable without the sticky bit. A group-writable parent whose group is not one of the
// runner's is NOT refused: the data root is
// 2775 on a hosted volume (entrypoint.sh align_pvc_root) and the chat HOME is `<dataDir>/agent-home`
// itself, so refusing it would stop every chat start. HOME's own entry is therefore protected by the
// data-root layout, not by this module: per-run HOMEs live in the sticky `agent-home` root (3775),
// job HOMEs under a 2750 root, cross-check and model-pass HOMEs (mkdtemp) in that same sticky
// `agent-home` root (main.ts `homeRoot: sdkHomeRoot`; ADR-4416, "Global CLI config input"). A
// group-writable, non-sticky parent whose group is one the runner holds (runner 10002, runner-cmd
// 10003, codex-session 10004) IS refused, since the runner could rename HOME's entry; the hosted
// 2775 data root (group worker, 10001) is not. Within that layout the runner cannot create or
// change anything in the config directory for the whole launch. One uid
// (a k8s single-uid start, or these tests) gives NO protection: the CLI and the agent are the
// worker's own uid, so the agent can alter the directory between the last check and the CLI's
// read; the checks below then only repair what an earlier turn left. Kernel
// enforcement of the sticky and ownership boundary cannot be shown with one uid and is not tested.
//
// Reuse and rebuild. The directory is never opened through a symlink and nothing in it is ever
// deleted. A real directory with exactly the expected entries in the expected state is reused (so
// concurrent chats sharing the chat HOME see no churn); under the split a `.claude.json` with other
// bytes or mode is replaced through a temp file and rename (the directory is 0755 there; with one
// uid it is 0555, so that case is rebuilt like the rest). Anything else (absent, a file, a symlink, another
// owner or mode, an unexpected entry, a different link target) is renamed aside within HOME to
// `.uzi-claude-config.stale-<hex>` (rename does not follow a symlink) and a fresh directory is
// built under a temp name and renamed into place, so it is never visible half built and two
// prepares cannot destroy each other's good directory. A failed rename aside refuses the start.
// The aside copies stay until the HOME is reclaimed with the rest of it (rmtree.ts rmHomeTree).
//
// Targets. `<HOME>/.claude/projects` and `<HOME>/.claude/tasks` must exist and be writable by the
// CLI. Missing ones are created AS THE RUNNER UID (a fixed worker-authored `/bin/mkdir -p -m 700`
// argv through runnerCommand, workerSpawnEnv on a minimal PATH env, no shell; the spawnSync timeout
// is not a hard bound, see defaultMkdirAsRunner; exit status 1 is the planted-entry signal, any
// other non-zero exit, a signal or a spawn error is a transient refusal), so they are runner-owned and private (0700) exactly as when the CLI created them, and
// neither runner-cmd (the Codex command uid, which shares group runner) nor the worker ever holds
// a write path into a Claude transcript directory. The worker never writes or renames anything
// under `.claude`; the only thing it ever moves is the `.claude` entry itself in HOME. That mkdir
// follows a runner-planted symlink only with the runner's own permissions, so it needs no
// confinement. With one uid the same argv is spawned directly (worker == runner). `.claude` that
// is not a real directory (a symlink, a file) is renamed aside (rename does not follow) and
// recreated. Where the worker can look inside `.claude`, a `projects` or `tasks` that is not a real
// directory makes it rename `.claude` aside and recreate it (the transcripts stay in the aside
// copy, so a resume of that session finds nothing). A `.claude` the worker cannot open
// (runner-private, the split's normal state) is not inspected and the mkdir is simply repeated (it
// is idempotent); when that mkdir exits 1 (a file or dangling symlink at a target, an unsearchable
// `.claude`), `.claude` is renamed aside ONCE and the mkdir retried, and a second exit 1 refuses
// the start. That keeps a runner-planted target from refusing every later start of a HOME, the
// shared chat HOME included. Residual: exit 1 is not proof of a planted entry. mkdir also exits 1
// on ENOSPC/EDQUOT/EIO, and setpriv exits 1 when it cannot resolve the runner user (for example
// under descriptor or memory exhaustion); such a failure moves `.claude` aside once (nothing is
// deleted; that session's resume history stays in the aside copy) and the start is refused.
//
// Not reset any more: `<HOME>/.claude.json` and `<HOME>/.claude/*` are not read by the CLI, so
// they are no longer replaced or removed.
//
// No HOME in the env: no-op (the probe-style spawns that carry no HOME do not start a CLI). A spawn
// that carries a provider credential but no HOME is refused by spawnDetached (sdk-spawn.ts) before
// this module runs, since the CLI would then fall back to the runner's passwd HOME.
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { claudeConfigDir, CLAUDE_CONFIG_DIR_NAME } from "./claude-config-dir.js";
import { CODEX_SESSION_GID, COMMAND_UID, RUNNER_UID, runnerCommand, uidSplitActive } from "./runner-uid.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";
import { workerSpawnEnv } from "./worker-spawn-mark.js";

/** The bytes of `<config dir>/.claude.json`: truthy `hasCompletedOnboarding` and `numStartups`,
 *  so the CLI treats it as a populated config and uses it directly (never falling back to a
 *  backup, which it does only for an empty or whitespace file), and no `env` key. */
export const CLAUDE_GLOBAL_CONFIG_BYTES = '{"hasCompletedOnboarding":true,"numStartups":1}\n';

const REFUSAL = "Claude CLI config directory could not be prepared by the worker";

const CONFIG_FILE = ".claude.json";
/** The only symlinks in the config directory, name -> relative target. */
const LINKS: ReadonlyArray<readonly [string, string]> = [
  ["projects", "../.claude/projects"],
  ["tasks", "../.claude/tasks"],
];
const EXPECTED_ENTRIES = [CONFIG_FILE, ...LINKS.map(([n]) => n)].sort();
const MAX_ATTEMPTS = 4;

const DIR_FLAGS = fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
/** Groups the runner uids hold: a group-writable non-sticky parent in one of them lets the runner
 *  rename HOME's entry. runner (10002), runner-cmd (10003), codex-session (10004). */
const RUNNER_GIDS: readonly number[] = [RUNNER_UID, COMMAND_UID, CODEX_SESSION_GID];
const MKDIR_BIN = "/bin/mkdir";
const MKDIR_TIMEOUT_MS = 5_000;
/** The exit status of a mkdir that ran and failed (a planted file, an unsearchable directory). */
const MKDIR_PLANTED_STATUS = 1;

/** Test-only seams. `onStep` is called with a step name before each stage, so a test can act as a
 *  hostile process at that point. `mkdirAsRunner` replaces the runner-uid `mkdir` (it receives the
 *  mkdir arguments and returns its exit status: 0, or 1 for a planted entry; any other value or a
 *  throw is a transient refusal). Production callers pass nothing. */
export interface PrepareDeps {
  onStep?: (name: string) => void;
  /** Run `/bin/mkdir <args>` as the runner: returns its exit status; throws on a transient failure. */
  mkdirAsRunner?: (args: readonly string[]) => number;
}

const errCode = (err: unknown): string | undefined => (err as NodeJS.ErrnoException | undefined)?.code;
const hex = (): string => randomBytes(8).toString("hex");

function refuse(reason: string): TrustedExecutionRefusal {
  return new TrustedExecutionRefusal(`Claude CLI config directory refused: ${reason}`);
}

/**
 * Prepare `<env.HOME>/.uzi-claude-config` for a CLI start, or throw a TrustedExecutionRefusal
 * before any process is spawned. No-op without a HOME. `env.CLAUDE_CONFIG_DIR` must be exactly that
 * directory.
 */
export function prepareClaudeConfigDir(
  env: Record<string, string | undefined> | undefined,
  deps: PrepareDeps = {},
): void {
  const home = env?.HOME;
  if (home === undefined || home === "") return;
  if (!path.isAbsolute(home) || path.resolve(home) !== home) throw refuse("HOME is not a canonical absolute path");
  const dir = claudeConfigDir(home);
  if (env?.CLAUDE_CONFIG_DIR !== dir) throw refuse("CLAUDE_CONFIG_DIR is not the worker-prepared directory");
  const step = (name: string): void => deps.onStep?.(name);
  try {
    step("home");
    guardHome(home);
    step("targets");
    ensureTargets(home, deps.mkdirAsRunner ?? defaultMkdirAsRunner, step);
    ensureConfigDir(home, dir, step);
  } catch (err) {
    if (err instanceof TrustedExecutionRefusal) throw err;
    throw new TrustedExecutionRefusal(REFUSAL, { cause: err });
  }
}

/** HOME must be a real directory; under the split it must be the worker's and sticky, and not in a
 *  world-writable, non-sticky parent (see the header's Boundary). */
function guardHome(home: string): void {
  const st = fs.lstatSync(home);
  if (!st.isDirectory()) throw refuse("HOME is not a real directory");
  if (!uidSplitActive()) return;
  const uid = process.getuid?.();
  if (uid !== undefined && st.uid !== uid) throw refuse("HOME is not owned by the worker");
  const parent = fs.statSync(path.dirname(home));
  if ((parent.mode & 0o002) !== 0 && (parent.mode & 0o1000) === 0) {
    throw refuse("HOME's parent is world-writable and not sticky");
  }
  if ((parent.mode & 0o020) !== 0 && (parent.mode & 0o1000) === 0 && RUNNER_GIDS.includes(parent.gid)) {
    throw refuse("HOME's parent is group-writable by a runner group and not sticky");
  }
  if ((st.mode & 0o1000) === 0) fchmodDir(home, (st.mode & 0o7777) | 0o1000);
}

/** chmod a directory through a descriptor opened with O_NOFOLLOW, never through a symlink. */
function fchmodDir(p: string, mode: number): void {
  const fd = fs.openSync(p, DIR_FLAGS);
  try {
    fs.fchmodSync(fd, mode);
  } finally {
    fs.closeSync(fd);
  }
}

/** Rename `p` to a sibling `<name>.stale-<hex>`. rename(2) does not follow a symlink at `p`. */
function renameAside(p: string): void {
  fs.renameSync(p, `${p}.stale-${hex()}`);
}

/** Run `/bin/mkdir <args>` as the runner uid (directly with one uid). A fixed worker-authored argv:
 *  workerSpawnEnv on a minimal PATH env, no shell; returns 0 or 1 (mkdir's failure); anything else
 *  (spawn error, signal, timeout, another exit status) throws. The spawnSync timeout and
 *  SIGKILL only bound the wait until the kill is attempted: under the split the child is the runner
 *  uid after setpriv and the worker cannot kill(2) it (EPERM, see runner-uid.ts), so no stronger
 *  bound is claimed. The argv is fixed and the command short-lived. */
function defaultMkdirAsRunner(args: readonly string[]): number {
  const wrapped = runnerCommand(MKDIR_BIN, args);
  const r = spawnSync(wrapped.command, wrapped.args, {
    env: workerSpawnEnv({ PATH: "/usr/bin:/bin" }),
    shell: false,
    stdio: "ignore",
    timeout: MKDIR_TIMEOUT_MS,
    killSignal: "SIGKILL",
  });
  // A spawn error, a signal or a timeout is transient: it refuses this start only, so a burst of
  // load never moves the shared chat root's transcripts aside. Only mkdir's own failure status 1
  // (a planted file, an unsearchable directory) is returned as such; setpriv's own failures (126,
  // 127 on a failed credential change or execve, e.g. RLIMIT_NPROC EAGAIN) are transient too.
  if (r.error || r.signal || r.status === null) throw refuse("the runner mkdir of the .claude targets did not complete");
  if (r.status !== 0 && r.status !== MKDIR_PLANTED_STATUS) {
    throw refuse("the runner mkdir of the .claude targets did not complete");
  }
  return r.status;
}

/** Make `<HOME>/.claude/{projects,tasks}` exist, writable by the CLI (see the Targets section). */
function ensureTargets(home: string, mkdir: (args: readonly string[]) => number, step: (name: string) => void): void {
  const claude = path.join(home, ".claude");
  let retriedAfterMkdirFailure = false;
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
    const st = fs.lstatSync(claude, { throwIfNoEntry: false });
    if (st !== undefined && !st.isDirectory()) {
      renameAside(claude);
      step("claude-aside");
      continue;
    }
    if (st === undefined && mkdir(["-p", "-m", "700", "--", claude]) !== 0) {
      throw refuse("the runner could not create .claude");
    }
    // (any non-zero status here refuses without moving anything: HOME holds no .claude to move)
    const missing: string[] = [];
    let aside = false;
    for (const name of ["projects", "tasks"] as const) {
      const target = path.join(claude, name);
      let t: fs.Stats | undefined;
      try {
        t = fs.lstatSync(target);
      } catch (err) {
        const c = errCode(err);
        if (c === "EACCES") {
          missing.push(target); // a private .claude the worker cannot look into: mkdir -p is idempotent
          continue;
        }
        if (c !== "ENOENT") throw err;
      }
      if (t === undefined) missing.push(target);
      else if (!t.isDirectory()) aside = true;
    }
    if (aside) {
      renameAside(claude);
      step("claude-aside");
      continue;
    }
    if (missing.length > 0) {
      const status = mkdir(["-p", "-m", "700", "--", ...missing]);
      if (status !== 0) {
        // Under the split the worker cannot look into the runner-private .claude, so a non-directory
        // (or an unsearchable .claude) shows up only as mkdir's own failure status 1. Only that moves
        // the HOME entry aside, once, and rebuilds; a second such exit refuses the start. Any other
        // status (setpriv's 126/127 on a failed credential change or execve under load, a seam's
        // other value) is transient: it refuses this start and keeps .claude in place.
        if (status !== MKDIR_PLANTED_STATUS) throw refuse("the runner mkdir of the .claude targets did not complete");
        if (retriedAfterMkdirFailure) throw refuse("the runner could not create the .claude targets");
        retriedAfterMkdirFailure = true;
        renameAside(claude);
        step("claude-aside");
        continue;
      }
    }
    return;
  }
  throw refuse("the .claude targets could not be prepared");
}

/** The config directory's mode: 0755 under the split (the runner cannot write it), 0555 with one
 *  uid (the CLI is the owner, so only the mode keeps it from writing). */
const configDirMode = (): number => (uidSplitActive() ? 0o755 : 0o555);

type Seen = { state: "absent" | "ok" | "rewrite" } | { state: "bad"; dev: number; ino: number };

/** Classify the config directory without following anything. `rewrite`: a real, worker-owned
 *  directory in the expected shape whose `.claude.json` has other bytes or mode. */
function inspect(dir: string): Seen {
  let st: fs.Stats;
  try {
    st = fs.lstatSync(dir);
  } catch (err) {
    if (errCode(err) === "ENOENT") return { state: "absent" };
    throw err;
  }
  const bad: Seen = { state: "bad", dev: st.dev, ino: st.ino };
  const uid = process.getuid?.();
  if (!st.isDirectory()) return bad;
  if (uid !== undefined && st.uid !== uid) return bad;
  if (((st.mode & 0o7777) & ~0o2000) !== configDirMode()) return bad;
  const names = fs.readdirSync(dir).sort();
  if (names.length !== EXPECTED_ENTRIES.length || names.some((n, i) => n !== EXPECTED_ENTRIES[i])) return bad;
  for (const [name, target] of LINKS) {
    const l = fs.lstatSync(path.join(dir, name));
    if (!l.isSymbolicLink() || (uid !== undefined && l.uid !== uid)) return bad;
    if (fs.readlinkSync(path.join(dir, name)) !== target) return bad;
  }
  const file = path.join(dir, CONFIG_FILE);
  const f = fs.lstatSync(file);
  if (!f.isFile() || f.nlink !== 1 || (uid !== undefined && f.uid !== uid)) return bad;
  // The in-place rewrite writes into the directory, which one uid makes read-only (0555): rebuild.
  const rewrite: Seen = uidSplitActive() ? { state: "rewrite" } : bad;
  if ((f.mode & 0o7777) !== 0o444 || f.size !== Buffer.byteLength(CLAUDE_GLOBAL_CONFIG_BYTES)) return rewrite;
  const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  try {
    return fs.readFileSync(fd, "utf8") === CLAUDE_GLOBAL_CONFIG_BYTES ? { state: "ok" } : rewrite;
  } finally {
    fs.closeSync(fd);
  }
}

/** Create `file` exclusively with the fixed bytes, mode 0444. */
function writeConfigFile(file: string): void {
  const fd = fs.openSync(
    file,
    fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_WRONLY | fs.constants.O_NOFOLLOW,
    0o444,
  );
  try {
    fs.writeSync(fd, CLAUDE_GLOBAL_CONFIG_BYTES);
    fs.fchmodSync(fd, 0o444);
  } finally {
    fs.closeSync(fd);
  }
}

/** Replace `<dir>/.claude.json` through a temp file next to the directory (so the directory never
 *  shows a transient entry) and a rename. */
function rewriteConfig(home: string, dir: string): void {
  const tmp = path.join(home, `${CLAUDE_CONFIG_DIR_NAME}.cfg-${hex()}`);
  try {
    writeConfigFile(tmp);
    fs.renameSync(tmp, path.join(dir, CONFIG_FILE));
  } catch (err) {
    fs.rmSync(tmp, { force: true });
    throw err;
  }
}

/** Build a complete config directory under a temp name in HOME. Removes it again on failure. */
function buildTemp(home: string): string {
  const tmp = path.join(home, `${CLAUDE_CONFIG_DIR_NAME}.tmp-${hex()}`);
  fs.mkdirSync(tmp, { mode: 0o755 });
  try {
    fchmodDir(tmp, 0o755);
    writeConfigFile(path.join(tmp, CONFIG_FILE));
    for (const [name, target] of LINKS) fs.symlinkSync(target, path.join(tmp, name));
    fchmodDir(tmp, configDirMode());
  } catch (err) {
    removeTemp(tmp);
    throw err;
  }
  return tmp;
}

/** Remove a temp directory this module built (it holds one file and two symlinks, none followed). */
function removeTemp(tmp: string): void {
  try {
    try {
      fchmodDir(tmp, 0o755); // a finished temp is 0555 with one uid, and rm needs write on it
    } catch {
      /* already gone or not ours: the rm below decides */
    }
    fs.rmSync(tmp, { recursive: true, force: true });
  } catch {
    /* best effort: the temp is inert and left for the HOME's reclaim */
  }
}

/** Leave `dir` verified, as the last action: reuse, rewrite the config, or rebuild. */
function ensureConfigDir(home: string, dir: string, step: (name: string) => void): void {
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
    step("inspect");
    const seen = inspect(dir);
    if (seen.state === "ok") return;
    if (seen.state === "rewrite") {
      rewriteConfig(home, dir);
      step("rewritten");
      continue;
    }
    const tmp = buildTemp(home);
    let installed = false;
    try {
      step("built");
      if (seen.state === "bad") {
        // Another prepare may have replaced the bad directory since it was inspected: never move
        // aside a directory that is no longer the one that was judged.
        const now = fs.lstatSync(dir, { throwIfNoEntry: false });
        if (now === undefined || now.dev !== seen.dev || now.ino !== seen.ino) continue;
        renameAside(dir);
        step("aside");
      }
      try {
        fs.renameSync(tmp, dir);
        installed = true;
      } catch (err) {
        // Someone else put a directory (or anything) there first: re-inspect, never replace it blindly.
        if (!["EEXIST", "ENOTEMPTY", "ENOTDIR", "EISDIR"].includes(errCode(err) ?? "")) throw err;
      }
      if (installed) step("installed");
    } finally {
      if (!installed) removeTemp(tmp);
    }
  }
  throw refuse("the config directory kept changing while it was prepared");
}
