// Prepare the Claude CLI's config directory before every CLI start (PRD #4416).
//
// Threat. The pinned Claude CLI honours an `env` block in its global config (for example
// OTEL_LOG_RAW_API_BODIES, NODE_OPTIONS or ANTHROPIC_BASE_URL), regardless of `settingSources: []`.
// The CLI runs as the runner uid and so does the agent's Bash tool, so an agent that can write
// the config can change the next CLI start's environment, with the provider credential already in
// it. The same holds for a config the CLI reads from `<HOME>/.claude.json`, `<HOME>/.claude/.config.json`
// and `<HOME>/.claude/remote-settings.json` when CLAUDE_CONFIG_DIR is unset.
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
// plugins, hooks, skills, commands and agents (configuration and code) are not linked,
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
// Boundary. Under the uid split (uidSplitActive) the trusted layout is:
//   - HOME is a real directory (fs.realpathSync(HOME) === HOME, lstat is not a symlink) owned by the
//     worker uid; this module adds the sticky bit to it, so the group-writable runner can create
//     entries in HOME but cannot rename, replace or unlink the worker-owned config directory, and
//     the directory itself grants the runner no write.
//   - HOME's parent and every ancestor up to `/` is a real directory (lstat, not a symlink) owned by
//     uid 0 or the worker uid, not world-writable unless sticky, and not group-writable by a runner
//     group (runner 10002, runner-cmd 10003, codex-session 10004) unless sticky. A group-writable
//     ancestor whose group is not a runner group is accepted: the data root is 2775 group worker
//     (10001) on a hosted volume (entrypoint.sh align_pvc_root) and the chat HOME is
//     `<dataDir>/agent-home` itself. Per-run HOMEs live in the sticky `agent-home` root (3775), job
//     HOMEs under a 2750 root, cross-check and model-pass HOMEs (mkdtemp) in that same sticky root
//     (main.ts `homeRoot: sdkHomeRoot`; ADR-4416, "Global CLI config input").
// Any other layout refuses the start before anything is built. Within the trusted layout the runner
// cannot create or change anything in the config directory for the whole launch. One uid (a k8s
// single-uid start, or these tests) gives NO protection: the CLI and the agent are the worker's own
// uid, so the agent can alter the directory between the last check and the CLI's read; the checks
// below then only repair what an earlier turn left, and no ownership or ancestor check is made.
// Kernel enforcement of the sticky and ownership boundary cannot be shown with one uid and is not
// tested there.
//
// Run variables. A run's user variables reach the CLI environment only under the split
// (sdk-executor.ts refuses a run that carries any when uidSplitActive() is false, and sdk-env.ts
// buildSdkEnv throws the same refusal): with one uid this directory gives no protection, so a
// variable could carry the same effect as a planted config `env` block.
//
// Reuse and rebuild. The directory is never opened through a symlink and nothing in it is ever
// deleted. A real directory with exactly the expected entries in the expected state is reused (so
// concurrent chats sharing the chat HOME see no churn); under the split a `.claude.json` with other
// bytes or mode is replaced through a temp file and rename (the directory is 0755 there; with one
// uid it is 0555, so that case is rebuilt like the rest). Anything else (absent, a file, a symlink, another
// owner or mode, an unexpected entry, a different link target) is renamed aside within HOME to
// `.uzi-claude-config.stale-<hex>` (rename does not follow a symlink) and a fresh directory is
// built under a temp name and renamed into place, so it is never visible half built and two
// prepares cannot destroy each other's good directory. A failed rename aside refuses the start. When
// the final rename fails (macOS can report EACCES where Linux reports EEXIST), the directory is
// inspected again: a valid directory another prepare installed is reused; a changed entry or
// EEXIST/ENOTEMPTY/ENOTDIR/EISDIR re-enters the loop; any other error with no valid directory
// there refuses the start. The aside copies stay until the HOME is reclaimed with the rest of it
// (rmtree.ts rmHomeTree).
//
// Targets. `<HOME>/.claude/projects` and `<HOME>/.claude/tasks` must exist and be writable by the
// CLI. Missing ones are created AS THE RUNNER UID (a fixed worker-authored `/bin/mkdir -p -m 700`
// argv, workerSpawnEnv on a minimal PATH env, no shell), so they are runner-owned and private
// (0700), and neither runner-cmd (the Codex command uid, which shares group runner) nor the worker
// ever holds a write path into a Claude transcript directory. The worker never writes or renames
// anything under `.claude`; the only thing it ever moves is the `.claude` entry itself in HOME. The
// mkdir follows a runner-planted symlink only with the runner's own permissions. `.claude` that is
// not a real directory (a symlink, a file) is renamed aside (rename does not follow) and recreated.
// Where the worker can look inside `.claude`, a `projects` or `tasks` that is not a real directory
// makes it rename `.claude` aside and recreate it (the transcripts stay in the aside copy, so a
// resume of that session finds nothing). A `.claude` the worker cannot open (runner-private, the
// split's normal state) is not inspected and the mkdir is repeated (it is idempotent).
//
// Obstruction. A non-zero mkdir exit status alone moves nothing: exit 1 also comes from ENOSPC,
// EDQUOT, EIO and from setpriv failing to resolve the runner user. After a runner mkdir of the
// targets exits non-zero, the module establishes a POSITIVE obstruction and only then moves
// `.claude` aside, once; a second obstruction refuses the start. Under the split a second fixed
// runner-uid helper, `/bin/stat -c '%u|%a|%F|%n' -- <HOME>/.claude <missing targets>`, prints one
// line per entry that exists; its stdout is captured (utf8, 64 KiB) and its exit status ignored.
// Lines are matched by exact `%n` equal to a path that was passed. An obstruction is: a matched line
// whose `%F` is not exactly `directory` (a regular file, a symlink, ...), or the `.claude` line
// showing an owner other than RUNNER_UID, or an owner mode without rwx. No lines (the helper was
// killed, setpriv could not resolve the user, the paths are absent), a lookup failure and a full
// disk are not an obstruction: the start is refused and `.claude` stays in place. With one uid there
// is no stat or timeout helper: the worker is the runner, lstat-inspects `.claude` and the missing
// targets itself and applies the same rule (the owner is compared with the worker's uid), and mkdir
// is spawned directly.
//
// Bound. Under the split each helper is `runnerCommand("/usr/bin/timeout", ["-s", "KILL", "2",
// <bin>, ...args])` run by spawnSync with a 4 s timeout and SIGKILL. The whole preparation has one
// absolute deadline of 10 s, read from PrepareDeps.now and checked before each helper spawn: past
// it the start is refused and the tree is kept. A helper stuck in uninterruptible I/O is not
// terminated by SIGKILL until the I/O returns, so neither limit shortens a call that is already
// blocked; they bound how many helpers start and how long a responsive one runs. A spawn error, a
// signal, a null status, and (for mkdir) any status other than 0 and 1, including those of timeout
// itself (124, 125, 126, 127, 137), are transient: they refuse this start and move nothing.
//
// The CLI reads the linked `<HOME>/.claude/projects` (transcripts, per-project memory) and
// `<HOME>/.claude/tasks`, which stay runner-writable data. `<HOME>/.claude.json` and the
// configuration files under `<HOME>/.claude` (`.config.json`, `settings.json`,
// `remote-settings.json`) are neither replaced nor removed: they were not applied in the probes once
// CLAUDE_CONFIG_DIR is set.
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
/** Groups the runner uids hold: a group-writable non-sticky ancestor in one of them lets the runner
 *  rename the entry below it. runner (10002), runner-cmd (10003), codex-session (10004). */
const RUNNER_GIDS: readonly number[] = [RUNNER_UID, COMMAND_UID, CODEX_SESSION_GID];
const MKDIR_BIN = "/bin/mkdir";
const STAT_BIN = "/bin/stat";
const TIMEOUT_BIN = "/usr/bin/timeout";
/** The helper's own kill after this many seconds (timeout -s KILL). */
const HELPER_KILL_AFTER_S = "2";
/** spawnSync's timeout for one helper, longer than timeout's own kill. */
const HELPER_SPAWN_TIMEOUT_MS = 4_000;
/** The absolute limit for one whole preparation, measured from its start. */
const PREPARE_DEADLINE_MS = 10_000;
const HELPER_MAX_BUFFER = 64 * 1024;
/** The exit status of a mkdir that ran and failed (a planted file, an unsearchable directory, ENOSPC). */
const MKDIR_FAILED_STATUS = 1;
const STAT_FORMAT = "%u|%a|%F|%n";

/** The outcome of one helper run: its exit status (null when it did not exit), the signal that ended
 *  it, and its captured stdout. */
export interface HelperResult {
  status: number | null;
  signal: NodeJS.Signals | null;
  stdout: string;
}

/** Test-only seams. `onStep` is called with a step name before each stage, so a test can act as a
 *  hostile process at that point. Production callers pass nothing. */
export interface PrepareDeps {
  onStep?: (name: string) => void;
  /** Run `<bin> <args>` as the runner uid, bounded (the production implementation is {@link runBounded}
   *  with the uid-split wrapper); throws on a transient failure. */
  runAsRunner?: (bin: string, args: readonly string[]) => HelperResult;
  /** lstat of HOME's parent and each ancestor (default `fs.lstatSync`). */
  lstatAncestor?: (p: string) => fs.Stats;
  /** The final rename of the built temp directory into place (default `fs.renameSync`). */
  rename?: (from: string, to: string) => void;
  /** The clock for the preparation deadline, in milliseconds (default `Date.now`). */
  now?: () => number;
}

const errCode = (err: unknown): string | undefined => (err as NodeJS.ErrnoException | undefined)?.code;
const hex = (): string => randomBytes(8).toString("hex");

function refuse(reason: string): TrustedExecutionRefusal {
  return new TrustedExecutionRefusal(`Claude CLI config directory refused: ${reason}`);
}

/** What one preparation carries: the helper runner, the clock and its absolute deadline. */
interface Prep {
  run: (bin: string, args: readonly string[]) => HelperResult;
  now: () => number;
  deadline: number;
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
  const now = deps.now ?? Date.now;
  const prep: Prep = { run: deps.runAsRunner ?? defaultRunAsRunner, now, deadline: now() + PREPARE_DEADLINE_MS };
  try {
    step("home");
    guardHome(home, deps.lstatAncestor ?? ((p) => fs.lstatSync(p)));
    step("targets");
    ensureTargets(home, prep, step);
    ensureConfigDir(home, dir, step, deps.rename ?? ((from, to) => fs.renameSync(from, to)));
  } catch (err) {
    if (err instanceof TrustedExecutionRefusal) throw err;
    throw new TrustedExecutionRefusal(REFUSAL, { cause: err });
  }
}

/** HOME must be a real directory; under the split it must be the worker's and sticky, and sit under
 *  ancestors that are all trusted (see the header's Boundary). */
function guardHome(home: string, lstatAncestor: (p: string) => fs.Stats): void {
  const st = fs.lstatSync(home);
  if (!st.isDirectory()) throw refuse("HOME is not a real directory");
  if (!uidSplitActive()) return;
  const uid = process.getuid?.();
  if (uid !== undefined && st.uid !== uid) throw refuse("HOME is not owned by the worker");
  if (fs.realpathSync(home) !== home) throw refuse("HOME is not its own real path");
  for (let p = path.dirname(home); ; p = path.dirname(p)) {
    const a = lstatAncestor(p);
    if (!a.isDirectory()) throw refuse(`an ancestor of HOME is not a real directory (${p})`);
    if (a.uid !== 0 && a.uid !== uid) throw refuse(`an ancestor of HOME is owned by neither root nor the worker (${p})`);
    const sticky = (a.mode & 0o1000) !== 0;
    if ((a.mode & 0o002) !== 0 && !sticky) throw refuse(`an ancestor of HOME is world-writable and not sticky (${p})`);
    if ((a.mode & 0o020) !== 0 && !sticky && RUNNER_GIDS.includes(a.gid)) {
      throw refuse(`an ancestor of HOME is group-writable by a runner group and not sticky (${p})`);
    }
    if (path.dirname(p) === p) break;
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

/** Spawn `command` with the helper bounds: a fixed worker-authored argv on workerSpawnEnv with a
 *  minimal PATH, no shell, stdout captured (utf8, bounded), spawnSync's 4 s timeout and SIGKILL. A
 *  spawn error (including a timeout or an exceeded buffer) throws a refusal. */
export function runBounded(command: string, args: readonly string[]): HelperResult {
  const r = spawnSync(command, [...args], {
    env: workerSpawnEnv({ PATH: "/usr/bin:/bin" }),
    shell: false,
    stdio: ["ignore", "pipe", "ignore"],
    encoding: "utf8",
    maxBuffer: HELPER_MAX_BUFFER,
    timeout: HELPER_SPAWN_TIMEOUT_MS,
    killSignal: "SIGKILL",
  });
  if (r.error) throw refuse("a runner helper did not complete");
  return { status: r.status, signal: r.signal, stdout: typeof r.stdout === "string" ? r.stdout : "" };
}

/** The argv the helper runs as: under the split the runner uid via setpriv around `timeout -s KILL 2`,
 *  with one uid the binary directly. */
export function helperCommand(bin: string, args: readonly string[], split: boolean): { command: string; args: string[] } {
  if (!split) return { command: bin, args: [...args] };
  return runnerCommand(TIMEOUT_BIN, ["-s", "KILL", HELPER_KILL_AFTER_S, bin, ...args], true);
}

function defaultRunAsRunner(bin: string, args: readonly string[]): HelperResult {
  const wrapped = helperCommand(bin, args, uidSplitActive());
  return runBounded(wrapped.command, wrapped.args);
}

/** Run one helper after checking the preparation deadline. */
function runHelper(prep: Prep, bin: string, args: readonly string[]): HelperResult {
  if (prep.now() > prep.deadline) throw refuse("the preparation exceeded its time limit");
  return prep.run(bin, args);
}

/** `/bin/mkdir <args>` as the runner: true when it exited 0, false when it exited 1 (it ran and
 *  failed); any other outcome is transient and throws. */
function mkdirAsRunner(prep: Prep, args: readonly string[]): boolean {
  const r = runHelper(prep, MKDIR_BIN, args);
  if (r.signal !== null || r.status === null) throw refuse("the runner mkdir of the .claude targets did not complete");
  if (r.status === 0) return true;
  if (r.status === MKDIR_FAILED_STATUS) return false;
  throw refuse("the runner mkdir of the .claude targets did not complete");
}

interface EntryFact {
  path: string;
  uid: number;
  mode: number;
  kind: string;
}

/** True when `facts` show a positive obstruction (see the header's Obstruction). */
function obstructs(claude: string, facts: readonly EntryFact[], runnerUid: number | undefined): boolean {
  for (const f of facts) {
    if (f.kind !== "directory") return true;
    if (f.path === claude && ((runnerUid !== undefined && f.uid !== runnerUid) || (f.mode & 0o700) !== 0o700)) return true;
  }
  return false;
}

/** Parse the stat helper's stdout into facts for the exact paths asked about. */
function parseStat(stdout: string, asked: readonly string[]): EntryFact[] {
  const facts: EntryFact[] = [];
  for (const line of stdout.split("\n")) {
    const m = /^(\d+)\|([0-7]+)\|([^|]*)\|(.*)$/.exec(line);
    if (m === null || !asked.includes(m[4]!)) continue;
    facts.push({ path: m[4]!, uid: Number(m[1]), mode: Number.parseInt(m[2]!, 8), kind: m[3]! });
  }
  return facts;
}

/** Whether `.claude` or one of the missing targets is positively obstructed, after a failed mkdir. */
function positiveObstruction(prep: Prep, claude: string, missing: readonly string[]): boolean {
  const asked = [claude, ...missing];
  if (uidSplitActive()) {
    const r = runHelper(prep, STAT_BIN, ["-c", STAT_FORMAT, "--", ...asked]);
    if (r.signal !== null || r.status === null) throw refuse("the runner stat of the .claude targets did not complete");
    return obstructs(claude, parseStat(r.stdout, asked), RUNNER_UID);
  }
  const facts: EntryFact[] = [];
  for (const p of asked) {
    let st: fs.Stats;
    try {
      st = fs.lstatSync(p);
    } catch {
      continue;
    }
    facts.push({ path: p, uid: st.uid, mode: st.mode & 0o7777, kind: st.isDirectory() ? "directory" : "other" });
  }
  return obstructs(claude, facts, process.getuid?.());
}

/** Make `<HOME>/.claude/{projects,tasks}` exist, writable by the CLI (see the Targets section). */
function ensureTargets(home: string, prep: Prep, step: (name: string) => void): void {
  const claude = path.join(home, ".claude");
  let movedAfterObstruction = false;
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
    const st = fs.lstatSync(claude, { throwIfNoEntry: false });
    if (st !== undefined && !st.isDirectory()) {
      renameAside(claude);
      step("claude-aside");
      continue;
    }
    if (st === undefined && !mkdirAsRunner(prep, ["-p", "-m", "700", "--", claude])) {
      throw refuse("the runner could not create .claude");
    }
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
    if (missing.length > 0 && !mkdirAsRunner(prep, ["-p", "-m", "700", "--", ...missing])) {
      // Only a positive obstruction moves the HOME entry aside, once; anything else refuses this start.
      if (!positiveObstruction(prep, claude, missing)) throw refuse("the runner mkdir of the .claude targets did not complete");
      if (movedAfterObstruction) throw refuse("the runner could not create the .claude targets");
      movedAfterObstruction = true;
      renameAside(claude);
      step("claude-aside");
      continue;
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
function ensureConfigDir(home: string, dir: string, step: (name: string) => void, rename: (from: string, to: string) => void): void {
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
        rename(tmp, dir);
        installed = true;
      } catch (err) {
        // The entry at `dir` is absent here (never replaced blindly). If it is now a valid
        // directory another prepare installed, reuse it; if anything else appeared, or the error is
        // an exists-class one, inspect again; any other error with nothing there refuses.
        let after: Seen | undefined;
        try {
          after = inspect(dir);
        } catch {
          after = undefined;
        }
        if (after?.state === "ok") return;
        const existsClass = ["EEXIST", "ENOTEMPTY", "ENOTDIR", "EISDIR"].includes(errCode(err) ?? "");
        const changed = fs.lstatSync(dir, { throwIfNoEntry: false }) !== undefined;
        if (!existsClass && !changed) throw err;
      }
      if (installed) step("installed");
    } finally {
      if (!installed) removeTemp(tmp);
    }
  }
  throw refuse("the config directory kept changing while it was prepared");
}
