// Issue #1607: HOME cleanup under the REAL PRD #51 worker/runner uid split.
//
// Runs INSIDE the worker image, started through its root entrypoint, so this process is
// the `worker` uid (10001) holding only the controller's SETUID/SETGID, exactly as in
// production. The fixture trees are written as `runner` (10002) through the production
// setpriv wrapper, reproducing the ownership measured on leaked hosted HOMEs: a
// worker-owned HOME root, runner-owned `0555` Go module-cache directories and a
// runner-owned private `0700` `.claude/projects`.
//
// A host or single-uid run cannot manufacture that split, and a run executed wholly as
// root bypasses DAC, so the first test REFUSES to pass anywhere else instead of skipping.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

type RmtreeModule = typeof import("../../agent/src/rmtree.js");
type RunnerUidModule = typeof import("../../agent/src/runner-uid.js");
type ReclaimModule = typeof import("../../agent/src/home-reclaim.js");
type ConfigModule = typeof import("../../agent/src/claude-global-config.js");
type ConfigDirModule = typeof import("../../agent/src/claude-config-dir.js");

// The code under test is loaded from the image's /app/src by default, so a built image
// proves what it ships. HOME_SPLIT_SRC points it at a mounted source tree instead.
const SRC = process.env.HOME_SPLIT_SRC ?? "/app/src";
const load = async <T>(name: string): Promise<T> =>
  (await import(pathToFileURL(path.join(SRC, name)).href)) as T;

const HOME_ROOT = "/data/agent-home";
const SENTINEL_DIR = "/data/home-split-sentinel";
const SENTINEL_FILE = path.join(SENTINEL_DIR, "keep.txt");

let rmtree: RmtreeModule;
let runnerUid: RunnerUidModule;
let reclaim: ReclaimModule;
let config: ConfigModule;
let configDir: ConfigDirModule;

/** Run a shell snippet as `runner` through the production setpriv wrapper. */
function asRunner(script: string, ...args: string[]): void {
  const wrapped = runnerUid.runnerCommand("/bin/sh", ["-c", script, "sh", ...args]);
  execFileSync(wrapped.command, wrapped.args, { stdio: "inherit", env: { PATH: "/usr/bin:/bin" } });
}

/** Run a shell snippet as the Codex command-root uid `runner-cmd` (10003). */
function asCommand(script: string, ...args: string[]): void {
  const wrapped = runnerUid.commandRootCommand("/bin/sh", ["-c", script, "sh", ...args]);
  execFileSync(wrapped.command, wrapped.args, { stdio: "inherit", env: { PATH: "/usr/bin:/bin" } });
}

/** Run a shell snippet as `runner` or `runner-cmd` and require the kernel to refuse it. */
function assertDenied(as: "runner" | "command", script: string, ...args: string[]): void {
  const argv = ["-c", script, "sh", ...args];
  const wrapped = as === "runner" ? runnerUid.runnerCommand("/bin/sh", argv) : runnerUid.commandRootCommand("/bin/sh", argv);
  try {
    execFileSync(wrapped.command, wrapped.args, { stdio: ["ignore", "pipe", "pipe"], env: { PATH: "/usr/bin:/bin" } });
  } catch (e) {
    const err = e as { status?: number | null; stderr?: Buffer };
    assert.ok(typeof err.status === "number" && err.status !== 0, `denied: ${script}`);
    assert.match(String(err.stderr), /Permission denied|Operation not permitted|Read-only/i, `refused by the kernel: ${script}`);
    return;
  }
  assert.fail(`${as} must be refused, but this succeeded: ${script}`);
}

/**
 * Create a HOME the way production does (worker-owned root, group `runner` via the setgid
 * parent) and fill it as `runner` with the leaked shape, including symlinks that point at
 * a worker-owned sentinel outside the HOME.
 */
async function makeLeakedHome(name: string, rootMode: number): Promise<string> {
  const home = path.join(HOME_ROOT, name);
  await fs.mkdir(home);
  await fs.chmod(home, 0o2770);
  asRunner(
    `set -e
     umask 002
     h="$1"; s="$2"
     mod="$h/go/pkg/mod/gopkg.in/inf.v0@v0.9.1"
     mkdir -p "$mod" "$h/.claude/projects/-app"
     echo 'package inf' > "$mod/dec.go"
     echo '{}' > "$h/.claude/projects/-app/session.jsonl"
     ln -s "$s" "$h/escape-dir"
     ln -s "$s/keep.txt" "$mod/escape-file"
     chmod 0444 "$mod/dec.go"
     chmod 0555 "$mod" "$h/go/pkg/mod/gopkg.in"
     chmod 0700 "$h/.claude/projects" "$h/.claude/projects/-app"`,
    home,
    SENTINEL_DIR,
  );
  // Codex command roots (runner-cmd, a member of group runner) can write into the
  // group-writable HOME and into runner's group-writable dirs. Their private 0700 dirs are
  // opaque to both the worker and runner (#1607 review).
  asCommand(
    `set -e
     umask 002
     h="$1"
     mkdir -p "$h/cmd-private/deep" "$h/go/cmd-private"
     echo x > "$h/cmd-private/deep/out.txt"
     echo x > "$h/go/cmd-private/out.txt"
     chmod 0700 "$h/cmd-private/deep" "$h/cmd-private" "$h/go/cmd-private"`,
    home,
  );
  // The root mode last: advice HOMEs were observed at 2700 on a hosted worker.
  await fs.chmod(home, rootMode);
  return home;
}

async function exists(p: string): Promise<boolean> {
  return fs.lstat(p).then(() => true, () => false);
}

async function assertSentinelIntact(): Promise<void> {
  assert.equal(await fs.readFile(SENTINEL_FILE, "utf8"), "keep\n", "the sentinel outside the HOME must be untouched");
  const st = await fs.stat(SENTINEL_FILE);
  assert.equal(st.mode & 0o777, 0o644, "the sentinel's mode must be unchanged");
}

const silentLog = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} };

describe("HOME cleanup under the real uid split (#1607)", () => {
  before(async () => {
    rmtree = await load<RmtreeModule>("rmtree.ts");
    runnerUid = await load<RunnerUidModule>("runner-uid.ts");
    reclaim = await load<ReclaimModule>("home-reclaim.ts");
    config = await load<ConfigModule>("claude-global-config.ts");
    configDir = await load<ConfigDirModule>("claude-config-dir.ts");
    await fs.mkdir(SENTINEL_DIR, { recursive: true });
    await fs.writeFile(SENTINEL_FILE, "keep\n");
    await fs.chmod(SENTINEL_FILE, 0o644);
  });

  it("runs as the worker uid under the split, not as root or single-uid", () => {
    assert.equal(process.getuid?.(), runnerUid.WORKER_UID, "the fixture must run as the worker uid");
    assert.equal(runnerUid.uidSplitActive(), true, "the entrypoint must have established UZI_UID_SPLIT=1");
  });

  it("POSITIVE CONTROL: the worker-uid rmTreeForce cannot remove a runner-written HOME", async () => {
    const home = await makeLeakedHome("00000000-0000-4000-8000-000000000001", 0o2770);
    await assert.rejects(rmtree.rmTreeForce(home), (e: NodeJS.ErrnoException) =>
      e.code === "EACCES" || e.code === "EPERM",
    );
    assert.equal(await exists(home), true, "the pre-fix path must strand the HOME in this fixture");
    await rmtree.rmHomeTree(home);
  });

  it("rmHomeTree removes a runner-written run HOME without following symlinks out of it", async () => {
    const home = await makeLeakedHome("00000000-0000-4000-8000-000000000002", 0o2770);
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false, "the whole run HOME must be gone");
    await assertSentinelIntact();
  });

  it("rmHomeTree removes an advice scratch HOME whose root is 2700", async () => {
    const home = await makeLeakedHome("uzi-summary-fixture", 0o2700);
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false, "the advice HOME must be gone");
    await assertSentinelIntact();
  });

  it("a partial pass never widens a runner-private dir to group (runner-cmd must not read it)", async () => {
    // A runner-private dir that a runner pass CANNOT empty: runner-cmd wrote into it while it
    // was group-writable, then runner made it private. Run one runner pass of the production
    // helper script and assert the dir it failed to remove is still owner-only.
    const home = path.join(HOME_ROOT, "uzi-review-private-mode");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2770);
    asRunner(`umask 002; mkdir -p "$1/priv"`, home);
    asCommand(`umask 077; mkdir -p "$1/priv/cmd" && echo x > "$1/priv/cmd/out.txt"`, home);
    asRunner(`chmod 0700 "$1/priv"`, home);
    const wrapped = runnerUid.runnerCommand(process.execPath, ["-e", rmtree.PURGE_CHILDREN_SCRIPT, home]);
    try {
      execFileSync(wrapped.command, wrapped.args, { env: { PATH: "/usr/local/bin:/usr/bin:/bin" } });
    } catch {
      // Expected: the runner pass cannot remove runner-cmd's private entry.
    }
    const out = execFileSync(
      runnerUid.runnerCommand("/bin/stat", ["-c", "%a", path.join(home, "priv")]).command,
      runnerUid.runnerCommand("/bin/stat", ["-c", "%a", path.join(home, "priv")]).args,
      { env: { PATH: "/usr/bin:/bin" } },
    ).toString().trim();
    assert.equal(out, "700", "the runner-private dir must stay owner-only after a partial pass");
  });

  // PRD #1809 M1: a process-ending park drops the run's caches from its preserved HOME. The
  // exact split shape: a worker-owned HOME, a runner-owned `0555` module cache with a
  // runner-cmd-private dir inside it (so one agent uid cannot finish alone), the build
  // cache, and the runner-private transcript that must survive.
  it("rmHomeSubtree drops the runner-owned caches and keeps the HOME root, siblings and transcript (#1809)", async () => {
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000001809");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    asRunner(
      `set -e
       umask 002
       h="$1"
       mod="$h/go/pkg/mod/gopkg.in/inf.v0@v0.9.1"
       mkdir -p "$mod" "$h/go/pkg/mod/cache/download" "$h/go/bin" "$h/.cache/go-build/0a" "$h/.cache/other" \\
         "$h/.claude/projects/-app"
       echo 'package inf' > "$mod/dec.go"
       echo o > "$h/.cache/go-build/0a/obj"
       echo s > "$h/.cache/other/state"
       echo b > "$h/go/bin/gopls"
       echo '{}' > "$h/.claude/projects/-app/session.jsonl"
       chmod 0700 "$h/.claude/projects" "$h/.claude/projects/-app"`,
      home,
    );
    asCommand(
      `set -e
       umask 077
       mkdir -p "$1/go/pkg/mod/cache/download/cmd-private"
       echo x > "$1/go/pkg/mod/cache/download/cmd-private/out"`,
      home,
    );
    asRunner(`chmod 0444 "$1/go/pkg/mod/gopkg.in/inf.v0@v0.9.1/dec.go" && chmod 0555 "$1/go/pkg/mod/gopkg.in/inf.v0@v0.9.1" "$1/go/pkg/mod/gopkg.in"`, home);
    const rootMode = (await fs.lstat(home)).mode;

    assert.equal(await rmtree.rmHomeSubtree(home, "go/pkg/mod"), "removed");
    assert.equal(await rmtree.rmHomeSubtree(home, ".cache/go-build"), "removed");
    assert.equal(await rmtree.rmHomeSubtree(home, ".npm/_cacache"), "absent");

    // Checked as runner: the worker cannot see inside runner-private dirs.
    asRunner(
      `set -e; h="$1"
       test ! -e "$h/go/pkg/mod"; test ! -e "$h/.cache/go-build"; test -d "$h/go/pkg"
       test -f "$h/go/bin/gopls"; test -f "$h/.cache/other/state"; test -f "$h/.claude/projects/-app/session.jsonl"`,
      home,
    );
    assert.equal((await fs.lstat(home)).mode, rootMode, "the HOME root's mode is never widened");
    await rmtree.rmHomeTree(home);
  });

  it("rmHomeSubtree refuses a runner-planted symlinked `.cache` and never touches its target (#1809)", async () => {
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-00000000180a");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    const target = path.join(SENTINEL_DIR, "go-build");
    await fs.mkdir(target, { recursive: true });
    await fs.writeFile(path.join(target, "keep.txt"), "keep\n");
    asRunner(`ln -s "$2" "$1/.cache"`, home, SENTINEL_DIR);
    await assert.rejects(rmtree.rmHomeSubtree(home, ".cache/go-build"), /refusing/);
    assert.equal(await fs.readFile(path.join(target, "keep.txt"), "utf8"), "keep\n", "the symlink target's contents survive");
    await assertSentinelIntact();
    await rmtree.rmHomeTree(home);
  });

  // Issue #3870 M1: the managed Go work leaf. The worker prepares `.uzi-go-work/g<N>` (2770, group
  // runner); `go build` as runner then fills it with 0700 `go-build*` trees the worker cannot even list,
  // and the park-only removal of that one leaf must still succeed while the parent, a newer generation
  // and the HOME's other files stay.
  it("a prepared Go work leaf, filled by runner with 0700 trees, is removed alone by rmHomeSubtree (#3870)", async () => {
    const gowork = await load<typeof import("../../agent/src/go-work.js")>("go-work.ts");
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000003870");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    asRunner(`set -e; umask 002; echo '{}' > "$1/.claude.json"`, home);

    const g3 = await gowork.prepareManagedGoWork(home, 3, silentLog as never);
    const g4 = await gowork.prepareManagedGoWork(home, 4, silentLog as never);
    assert.deepEqual(g3, { dir: path.join(home, ".uzi-go-work", "g3") });
    assert.ok("dir" in g4);
    const parentStat = await fs.lstat(path.join(home, ".uzi-go-work"));
    assert.equal(parentStat.mode & 0o7777, 0o2770, "the parent is 2770 under the split");
    assert.equal(parentStat.gid, runnerUid.RUNNER_UID, "the parent is group runner");

    asRunner(
      `set -e
       umask 077
       mkdir -p "$1/.uzi-go-work/g3/go-build123/b001"
       echo o > "$1/.uzi-go-work/g3/go-build123/b001/a.o"
       echo k > "$1/.uzi-go-work/g4/keep"
       chmod 0700 "$1/.uzi-go-work/g3/go-build123/b001" "$1/.uzi-go-work/g3/go-build123"`,
      home,
    );

    assert.equal(await rmtree.rmHomeSubtree(home, ".uzi-go-work/g3"), "removed");
    assert.equal(await rmtree.rmHomeSubtree(home, ".uzi-go-work/g3"), "absent");
    assert.equal(await exists(path.join(home, ".uzi-go-work", "g3")), false, "the leaf is gone");
    assert.equal(await exists(path.join(home, ".uzi-go-work")), true, "the parent stays");
    asRunner(`set -e; test -f "$1/.uzi-go-work/g4/keep"; test -f "$1/.claude.json"`, home);
    await rmtree.rmHomeTree(home);
  });

  // Issue #3870: a runner-owned `.uzi-go-work` is a parent the worker must refuse, never repair.
  it("a runner-owned Go work parent is refused by prepareManagedGoWork and left unrepaired (#3870)", async () => {
    const gowork = await load<typeof import("../../agent/src/go-work.js")>("go-work.ts");
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000003871");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    asRunner(`set -e; mkdir "$1/.uzi-go-work"; chmod 2770 "$1/.uzi-go-work"`, home);
    const parent = path.join(home, ".uzi-go-work");
    const before = await fs.lstat(parent);

    const r = await gowork.prepareManagedGoWork(home, 5, silentLog as never);
    assert.deepEqual(r, { skipped: true, reason: "parent_refused" });

    const after = await fs.lstat(parent);
    assert.equal(after.uid, before.uid, "no chown");
    assert.equal(after.gid, before.gid, "no chown");
    assert.equal(after.mode & 0o7777, before.mode & 0o7777, "no chmod repair");
    asRunner(`set -e; test -z "$(ls -A "$1/.uzi-go-work")"`, home);
    await rmtree.rmHomeTree(home);
  });

  // PRD #4416: the Claude CLI config directory is worker-owned inside a runner-writable, sticky HOME;
  // neither the runner nor runner-cmd may change it, and the targets under HOME/.claude are
  // runner-private so runner-cmd cannot tamper with a Claude run's transcripts.
  it("the runner cannot change, move or replace the prepared Claude config directory, and a re-prepare reuses it (#4416)", async () => {
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000004416");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    const dir = configDir.claudeConfigDir(home);
    const cliEnv = { HOME: home, CLAUDE_CONFIG_DIR: dir };

    config.prepareClaudeConfigDir(cliEnv);
    const before = await fs.lstat(dir);
    const bytes = await fs.readFile(path.join(dir, ".claude.json"), "utf8");
    assert.equal(bytes, config.CLAUDE_GLOBAL_CONFIG_BYTES);
    assert.equal(before.uid, runnerUid.WORKER_UID, "the directory is worker-owned");
    assert.equal(before.mode & 0o7777 & ~0o2000, 0o755);
    assert.ok((await fs.lstat(home)).mode & 0o1000, "HOME is sticky");

    // The targets were created by the runner uid, private to it (as the CLI itself would make them).
    const claude = await fs.lstat(path.join(home, ".claude"));
    assert.equal(claude.uid, runnerUid.RUNNER_UID, ".claude is runner-owned");
    assert.equal(claude.mode & 0o7777, 0o700, ".claude is private");
    asRunner(
      `set -e; for t in projects tasks; do test "$(stat -c %a "$1/.claude/$t")" = 700; test "$(stat -c %u "$1/.claude/$t")" = 10002; done`,
      home,
    );

    // The runner, as the agent's Bash tool would be, tries every way to change what the CLI reads.
    assertDenied("runner", `echo '{"env":{"NODE_OPTIONS":"x"}}' > "$1/.claude.json"`, dir);
    assertDenied("runner", `rm -f "$1/.claude.json" && echo x > "$1/.claude.json"`, dir);
    assertDenied("runner", `: > "$1/.config.json"`, dir);
    assertDenied("runner", `: > "$1/remote-settings.json"`, dir);
    assertDenied("runner", `mv "$1" "$1.moved"`, dir);
    assertDenied("runner", `mv "$1" "$1.moved" && ln -s /tmp "$1"`, dir);
    assertDenied("runner", `mv "$1" "$1.moved"`, home);
    assertDenied("runner", `rm "$1/projects" && ln -s /tmp "$1/projects"`, dir);
    assertDenied("runner", `chmod 0777 "$1"`, dir);
    assertDenied("runner", `chmod 0666 "$1/.claude.json"`, dir);
    // runner-cmd (group runner) cannot touch the config directory or a Claude run's transcripts either.
    assertDenied("command", `echo x > "$1/.claude.json"`, dir);
    assertDenied("command", `mv "$1" "$1.moved"`, dir);
    assertDenied("command", `mkdir "$1/.claude/projects/planted"`, home);
    assertDenied("command", `echo x > "$1/.claude/projects/planted"`, home);
    assertDenied("command", `mv "$1/.claude" "$1/.claude.moved"`, home);

    // Nothing changed; the next start reuses the very same directory.
    const mid = await fs.lstat(dir);
    assert.equal(mid.ino, before.ino);
    assert.equal(await fs.readlink(path.join(dir, "projects")), "../.claude/projects", "the projects link was not retargeted");
    assert.deepEqual((await fs.readdir(dir)).sort(), [".claude.json", "projects", "tasks"], "nothing was added to the directory");
    assert.equal(await fs.readFile(path.join(dir, ".claude.json"), "utf8"), bytes);
    config.prepareClaudeConfigDir(cliEnv);
    const after = await fs.lstat(dir);
    assert.equal(after.ino, before.ino, "the directory is reused, not rebuilt");
    assert.equal(after.isDirectory() && !after.isSymbolicLink(), true);
    assert.equal(await fs.readFile(path.join(dir, ".claude.json"), "utf8"), bytes, "the bytes are the fixed bytes");
    assert.deepEqual((await fs.readdir(home)).filter((n) => n.includes(".stale-")), [], "no aside copy was made");

    // Reclaim: an aside copy, the directory and the runner-private transcripts all go with the HOME.
    await fs.rename(dir, `${dir}.stale-aaaaaaaaaaaaaaaa`);
    config.prepareClaudeConfigDir(cliEnv);
    asRunner(
      `set -e; umask 077; mkdir -p "$1/.claude/projects/-s"; echo T > "$1/.claude/projects/-s/t.jsonl"`,
      home,
    );
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false, "the whole HOME, config directory and aside copy included, is gone");
    await assertSentinelIntact();
  });

  // Under the split HOME/.claude is runner-private, so the worker cannot look inside it. A
  // runner-planted file at a target does not refuse the start: the failed runner mkdir is followed by
  // the runner-uid stat probe, which shows the file, and .claude is moved aside once and recreated.
  it("a runner-planted file at .claude/projects does not refuse the start: .claude is moved aside and rebuilt (#4416)", async () => {
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000004417");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    const cliEnv = { HOME: home, CLAUDE_CONFIG_DIR: configDir.claudeConfigDir(home) };
    asRunner(`set -e; umask 077; mkdir -p "$1/.claude"; echo x > "$1/.claude/projects"; mkdir "$1/.claude/tasks"`, home);

    config.prepareClaudeConfigDir(cliEnv);

    assert.equal((await fs.readdir(home)).filter((n) => n.startsWith(".claude.stale-")).length, 1, ".claude was moved aside once");
    const claude = await fs.lstat(path.join(home, ".claude"));
    assert.equal(claude.uid, runnerUid.RUNNER_UID, "the fresh .claude is runner-owned");
    assert.equal(claude.mode & 0o7777, 0o700, "and private");
    asRunner(
      `set -e; for t in projects tasks; do test -d "$1/.claude/$t"; test ! -L "$1/.claude/$t"; test "$(stat -c %a "$1/.claude/$t")" = 700; test "$(stat -c %u "$1/.claude/$t")" = 10002; done`,
      home,
    );
    assert.equal(await fs.readFile(path.join(cliEnv.CLAUDE_CONFIG_DIR, ".claude.json"), "utf8"), config.CLAUDE_GLOBAL_CONFIG_BYTES);
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false);
  });

  // The runner creates a world-writable sticky directory; a worker HOME inside it has a parent the
  // runner owns, so the runner could rename the HOME's entry. The trusted layout refuses it.
  it("a worker HOME whose parent the runner created is refused before anything is built (#4416)", async () => {
    const outer = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000004418");
    await fs.mkdir(outer);
    await fs.chmod(outer, 0o2770);
    const runnerDir = path.join(outer, "runner-made");
    asRunner(`set -e; umask 000; mkdir "$1"; chmod 1777 "$1"`, runnerDir);
    const home = path.join(runnerDir, "home");
    await fs.mkdir(home);
    const cliEnv = { HOME: home, CLAUDE_CONFIG_DIR: configDir.claudeConfigDir(home) };

    assert.throws(() => config.prepareClaudeConfigDir(cliEnv), /ancestor of HOME is owned by neither root nor the worker/);

    assert.deepEqual(await fs.readdir(home), [], "no .claude, config directory or temp was created");
    await rmtree.rmHomeTree(outer);
    assert.equal(await exists(outer), false);
  });

  // The runner-uid helpers the preparation runs, in their real wrapper: the stat probe prints the
  // exact line format the module parses (the setgid bit HOME carries is inherited by .claude, so the
  // fields are checked, not a mode string), and a helper that does not finish returns control to the
  // worker within the bound.
  it("the runner-uid stat helper prints the parsed line format (#4416)", async () => {
    const home = path.join(HOME_ROOT, "00000000-0000-4000-8000-000000004419");
    await fs.mkdir(home);
    await fs.chmod(home, 0o2775);
    asRunner(`set -e; umask 077; mkdir -p "$1/.claude"; echo x > "$1/.claude/projects"`, home);
    const claude = path.join(home, ".claude");
    const projects = path.join(claude, "projects");

    const stat = config.helperCommand("/bin/stat", ["-c", "%u|%g|%a|%F|%n", "--", claude, projects], true);
    const r = config.runBounded(stat.command, stat.args);
    assert.equal(r.status, 0);
    const lines = r.stdout.split("\n").filter((l) => l !== "").map((l) => l.split("|"));
    assert.equal(lines.length, 2);
    for (const l of lines) assert.equal(l.length, 5, "uid|gid|mode|kind|name");
    // The runner creates the entries in the setgid HOME, so they carry HOME's group (the runner's own
    // group 10002 when HOME's group is the runner group); the field is the numeric gid of the entry.
    const homeGid = (await fs.stat(home)).gid;
    const [dirLine, fileLine] = lines;
    assert.equal(dirLine![0], String(runnerUid.RUNNER_UID));
    assert.match(dirLine![1]!, /^\d+$/);
    assert.equal(dirLine![1], String(homeGid), "the setgid HOME's group is inherited");
    assert.equal(Number.parseInt(dirLine![2]!, 8) & 0o700, 0o700, "the owner has rwx");
    assert.equal(dirLine![3], "directory");
    assert.equal(dirLine![4], claude);
    assert.equal(fileLine![0], String(runnerUid.RUNNER_UID));
    assert.equal(fileLine![1], String(homeGid));
    assert.equal(fileLine![3], "regular file");
    assert.equal(fileLine![4], projects);

    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false);
  });

  it("the worker regains control within the bound from a runner helper it cannot signal (#4416)", async () => {
    // Under the split the worker uid cannot signal the runner-uid helper; only the worker-uid
    // intermediary is killed, so runBounded returns whether the helper dies on its own or is stopped.
    const sleep = config.helperCommand("/bin/sleep", ["10"], true);
    const t0 = Date.now();
    const killed = config.runBounded(sleep.command, sleep.args);
    const elapsed = Date.now() - t0;
    assert.ok(elapsed < 4500, `the helper returned after ${elapsed} ms`);
    assert.notEqual(killed.status, 0);

    // Another runner process stops both the helper and its timeout: nothing runner-side can end it.
    const stopper = [
      'sig="$1"; n="$2"; i=0',
      'while [ "$i" -lt "$n" ]; do',
      '  for p in /proc/[0-9]*; do',
      '    id="${p#/proc/}"; [ "$id" = "$$" ] && continue',
      `    c=$(tr '\\0' ' ' < "$p/cmdline" 2>/dev/null) || continue`,
      '    case "$c" in *"sle""ep 7 "*) kill -"$sig" "$id" 2>/dev/null ;; esac',
      "  done",
      "  i=$((i+1)); sleep 0.1",
      "done",
    ].join("\n");
    const wrapped = runnerUid.runnerCommand("/bin/sh", ["-c", stopper, "sh", "STOP", "45"]);
    const watcher = spawn(wrapped.command, wrapped.args, { stdio: "ignore" });
    // The watcher runs as the runner uid, which the worker cannot signal: it ends on its own after
    // its fixed number of passes, and the test only waits for that exit.
    watcher.on("error", () => undefined);
    const watcherExited = new Promise<void>((resolve) => {
      const bound = setTimeout(resolve, 20_000);
      watcher.once("exit", () => {
        clearTimeout(bound);
        resolve();
      });
      watcher.once("error", () => {
        clearTimeout(bound);
        resolve();
      });
    });
    const stuck = config.helperCommand("/bin/sleep", ["7"], true);
    const t1 = Date.now();
    try {
      assert.throws(() => config.runBounded(stuck.command, stuck.args));
      const stoppedElapsed = Date.now() - t1;
      assert.ok(stoppedElapsed < 5000, `the stopped helper returned after ${stoppedElapsed} ms`);
    } finally {
      asRunner(stopper, "KILL", "3");
      await watcherExited;
    }
  });

  it("the startup reclaim removes only API-confirmed-terminal HOMEs", async () => {
    const terminal = await makeLeakedHome("00000000-0000-4000-8000-00000000000a", 0o2770);
    const running = await makeLeakedHome("00000000-0000-4000-8000-00000000000b", 0o2770);
    const unknown = await makeLeakedHome("00000000-0000-4000-8000-00000000000c", 0o2770);
    const statuses: Record<string, string | undefined> = {
      [path.basename(terminal)]: "completed",
      [path.basename(running)]: "running",
    };
    const summary = await reclaim.reclaimStrandedRunHomes(
      HOME_ROOT,
      async (id) => statuses[id],
      silentLog as never,
      { minAgeMs: 0 },
    );
    assert.equal(await exists(terminal), false, "the terminal run's HOME must be reclaimed");
    assert.equal(await exists(running), true, "a non-terminal run's HOME must be untouched");
    assert.equal(await exists(unknown), true, "an unknown-status HOME must be untouched");
    // The transcript sits in a runner-private 0700 dir the worker cannot stat, so check as runner.
    asRunner(`test -f "$1/.claude/projects/-app/session.jsonl"`, running);
    assert.equal(summary.removed, 1);
    assert.equal(summary.failed, 0);
    await assertSentinelIntact();
    await rmtree.rmHomeTree(running);
    await rmtree.rmHomeTree(unknown);
  });
});
