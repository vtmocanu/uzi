import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import {
  CLAUDE_GLOBAL_CONFIG_BYTES,
  helperCommand,
  NODE_INTERMEDIARY,
  prepareClaudeConfigDir,
  runBounded,
  type HelperResult,
  type PrepareDeps,
} from "../src/claude-global-config.js";
import { CODEX_SESSION_GID, COMMAND_UID, RUNNER_UID, setprivRunnerArgs } from "../src/runner-uid.js";
import { claudeConfigDir } from "../src/claude-config-dir.js";
import { rmHomeTree, PURGE_CHILDREN_SCRIPT } from "../src/rmtree.js";
import { spawnDetached } from "../src/sdk-spawn.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { execFile, spawn, spawnSync } from "node:child_process";
import { promisify } from "node:util";

/**
 * Direct tests of the config-directory preparation and of the spawn seam: what is built, what is
 * reused, what is moved aside, what is refused, and that nothing is followed or deleted.
 */

const MALICIOUS = JSON.stringify({ env: { NODE_OPTIONS: "--require x", ANTHROPIC_BASE_URL: "http://127.0.0.1:1" } });
const execFileAsync = promisify(execFile);

const DUMP_SCRIPT = [
  'const fs = require("node:fs");',
  "const d = process.env.CLAUDE_CONFIG_DIR;",
  "let r;",
  'try { r = fs.readFileSync(d + "/.claude.json", "utf8"); } catch { r = "ABSENT"; }',
  "fs.writeFileSync(process.argv[1], r);",
  'fs.writeFileSync(process.argv[1] + ".alt", fs.existsSync(d + "/.config.json") ? "PRESENT" : "ABSENT");',
  'let names; try { names = fs.readdirSync(d).sort().join(","); } catch { names = "UNREADABLE"; }',
  'fs.writeFileSync(process.argv[1] + ".dir", names);',
].join("\n");

let root: string;
let home: string;
let outside: string;
let savedSplit: string | undefined;
let counter = 0;

/** The runner-uid helper stand-in for tests: the same argv, run directly (these tests are one uid). */
const runDirect = (bin: string, args: readonly string[]): HelperResult => {
  const r = spawnSync(bin, [...args], { encoding: "utf8" });
  return { status: r.status, signal: r.signal, stdout: r.stdout ?? "" };
};

/** One line of the stat helper's output, as `stat -c '%u|%g|%a|%F|%n'` prints it; the group defaults to the runner's own. */
const statLine = (uid: number, mode: string, kind: string, p: string, gid: number = RUNNER_UID): string => `${uid}|${gid}|${mode}|${kind}|${p}\n`;

interface ModelOptions {
  /** Force the exit status of the n-th (1-based) mkdir; undefined runs the real mkdir. */
  mkdir?: (args: readonly string[], n: number) => number | undefined;
  /** What the stat helper returns; a function can throw to model a stuck helper. */
  stat?: string | HelperResult | ((args: readonly string[]) => HelperResult);
}

/** A runner-uid helper model that records every call: real mkdir, scripted stat. */
function helpers(o: ModelOptions = {}): { run: (bin: string, args: readonly string[]) => HelperResult; calls: Array<{ bin: string; args: string[] }> } {
  const calls: Array<{ bin: string; args: string[] }> = [];
  let mkdirs = 0;
  const run = (bin: string, args: readonly string[]): HelperResult => {
    calls.push({ bin, args: [...args] });
    if (bin === "/bin/stat") {
      const out = typeof o.stat === "function" ? o.stat(args) : o.stat ?? { status: 1, signal: null, stdout: "" };
      return typeof out === "string" ? { status: 0, signal: null, stdout: out } : out;
    }
    const forced = bin === "/bin/mkdir" ? o.mkdir?.(args, ++mkdirs) : undefined;
    if (forced !== undefined) return { status: forced, signal: null, stdout: "" };
    return runDirect(bin, args);
  };
  return { run, calls };
}

/** Ancestor stats that hide the host's real layout: every path is a root-owned 0755 directory unless overridden. */
function ancestors(over: Record<string, { mode?: number; uid?: number; gid?: number }> = {}): (p: string) => fs.Stats {
  return (p) => {
    const st = fs.lstatSync(p);
    return Object.assign(Object.create(Object.getPrototypeOf(st)), st, { mode: 0o40755, uid: 0, gid: 0, ...over[p] });
  };
}

const prep = (env: Parameters<typeof prepareClaudeConfigDir>[0], deps: PrepareDeps = {}): void =>
  prepareClaudeConfigDir(env, {
    runAsRunner: runDirect,
    ...(process.env.UZI_UID_SPLIT === "1" ? { lstatAncestor: ancestors() } : {}),
    ...deps,
  });
/** rm -rf that first restores owner write on directories (the one-uid directory is 0555). */
function rmAll(p: string): void {
  let st: fs.Stats;
  try {
    st = fs.lstatSync(p);
  } catch {
    return;
  }
  if (st.isDirectory()) {
    fs.chmodSync(p, 0o755);
    for (const e of fs.readdirSync(p)) rmAll(path.join(p, e));
  }
  fs.rmSync(p, { recursive: true, force: true });
}

const dirOf = (): string => claudeConfigDir(home);
const envOf = (h: string = home): Record<string, string> => ({ HOME: h, CLAUDE_CONFIG_DIR: claudeConfigDir(h) });
const staleNames = (): string[] => fs.readdirSync(home).filter((f) => f.includes(".stale-"));

/** An independent description of "this is the fixed directory", not derived from the module's checks. */
function assertFixedDir(dir: string): void {
  const st = fs.lstatSync(dir);
  assert.ok(st.isDirectory() && !st.isSymbolicLink(), "a real directory");
  const split = process.env.UZI_UID_SPLIT === "1";
  assert.strictEqual(st.mode & 0o7777 & ~0o2000, split ? 0o755 : 0o555, "0755 under the split, 0555 with one uid; never group- or world-writable");
  assert.deepStrictEqual(fs.readdirSync(dir).sort(), [".claude.json", "projects", "tasks"]);
  assert.strictEqual(fs.readFileSync(path.join(dir, ".claude.json"), "utf8"), CLAUDE_GLOBAL_CONFIG_BYTES);
  assert.strictEqual(fs.lstatSync(path.join(dir, ".claude.json")).mode & 0o777, 0o444);
  assert.strictEqual(fs.readlinkSync(path.join(dir, "projects")), "../.claude/projects");
  assert.strictEqual(fs.readlinkSync(path.join(dir, "tasks")), "../.claude/tasks");
}

beforeEach(() => {
  root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "uzi-cfgdir-")));
  home = path.join(root, `home${++counter}`);
  outside = path.join(root, "outside");
  fs.mkdirSync(home);
  fs.mkdirSync(outside);
  fs.writeFileSync(path.join(outside, "sentinel"), "SENTINEL");
  savedSplit = process.env.UZI_UID_SPLIT;
  delete process.env.UZI_UID_SPLIT;
});
afterEach(() => {
  rmAll(root);
  if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
  else process.env.UZI_UID_SPLIT = savedSplit;
});

describe("prepareClaudeConfigDir: build and reuse", () => {
  it("builds exactly the fixed directory and the private targets (the real runner-uid mkdir path, one uid)", () => {
    prepareClaudeConfigDir(envOf());
    assertFixedDir(dirOf());
    for (const t of [".claude", ".claude/projects", ".claude/tasks"]) {
      const st = fs.statSync(path.join(home, t));
      assert.ok(st.isDirectory(), t);
      assert.strictEqual(st.mode & 0o7777, 0o700, `${t} is private`);
    }
    assert.deepStrictEqual(fs.readdirSync(home).sort(), [".claude", ".uzi-claude-config"], "no temp or stale entry left");
  });

  it("creates the targets through the runner-uid helper seam only, with a fixed argv, and repeats nothing", () => {
    const h = helpers();
    prepareClaudeConfigDir(envOf(), { runAsRunner: h.run });
    const claude = path.join(home, ".claude");
    assert.deepStrictEqual(h.calls, [
      { bin: "/bin/mkdir", args: ["-p", "-m", "700", "--", claude] },
      { bin: "/bin/mkdir", args: ["-p", "-m", "700", "--", path.join(claude, "projects"), path.join(claude, "tasks")] },
    ]);
    prepareClaudeConfigDir(envOf(), { runAsRunner: h.run });
    assert.strictEqual(h.calls.length, 2, "existing targets spawn nothing");
    fs.rmSync(path.join(claude, "tasks"), { recursive: true });
    prepareClaudeConfigDir(envOf(), { runAsRunner: h.run });
    assert.deepStrictEqual(h.calls[2], { bin: "/bin/mkdir", args: ["-p", "-m", "700", "--", path.join(claude, "tasks")] });
  });

  it("a failing runner-uid mkdir of .claude refuses the start and builds no config directory", () => {
    const h = helpers({ mkdir: () => 1 });
    assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: h.run }), /the runner could not create \.claude/);
    assert.strictEqual(h.calls.length, 1, "the .claude creation is not retried and no stat probe runs");
    assert.ok(!fs.existsSync(dirOf()));
  });

  for (const status of [124, 125, 126, 127, 137, 2]) {
    it(`a runner mkdir exiting ${status} (not mkdir's own failure status 1) refuses without moving .claude aside`, () => {
      fs.mkdirSync(path.join(home, ".claude", "projects"), { recursive: true });
      fs.writeFileSync(path.join(home, ".claude", "projects", "keep.jsonl"), "x");
      const h = helpers({ mkdir: () => status });
      assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: h.run }), /did not complete/);
      assert.strictEqual(h.calls.length, 1);
      assert.strictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")).length, 0, "no aside");
      assert.ok(fs.existsSync(path.join(home, ".claude", "projects", "keep.jsonl")));
    });
  }

  it("a transient runner helper failure (a throw, a signal, a null status) refuses this start and keeps .claude in place", () => {
    const outcomes: Array<(bin: string, args: readonly string[]) => HelperResult> = [
      () => { throw new TrustedExecutionRefusal("transient"); },
      () => ({ status: null, signal: "SIGKILL", stdout: "" }),
      () => ({ status: null, signal: null, stdout: "" }),
    ];
    for (const outcome of outcomes) {
      fs.mkdirSync(path.join(home, ".claude", "projects"), { recursive: true });
      fs.writeFileSync(path.join(home, ".claude", "projects", "keep.jsonl"), "x");
      assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: outcome }), TrustedExecutionRefusal);
      assert.strictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")).length, 0, "no aside on a transient failure");
      assert.ok(fs.existsSync(path.join(home, ".claude", "projects", "keep.jsonl")), "the transcripts stay where resume finds them");
    }
  });

  it("one uid: a CLI that tries to write the directory fails, so repeated starts reuse it and leave no stale copy", { skip: process.getuid?.() === 0 }, () => {
    prep(envOf());
    const ino = fs.lstatSync(dirOf()).ino;
    fs.writeFileSync(path.join(home, "cli-tmp"), "x");
    for (let i = 0; i < 3; i++) {
      // What the pinned CLI does at start and exit: create backups/plugins/sessions, a cleanup
      // marker, and save the config through a temp file and rename.
      for (const name of ["backups", "plugins", "sessions"]) {
        assert.throws(() => fs.mkdirSync(path.join(dirOf(), name)), { code: "EACCES" }, name);
      }
      assert.throws(() => fs.writeFileSync(path.join(dirOf(), ".last-cleanup"), "1"), { code: "EACCES" });
      assert.throws(() => fs.writeFileSync(path.join(dirOf(), ".claude.json.tmp"), "{}"), { code: "EACCES" });
      assert.throws(() => fs.renameSync(path.join(home, "cli-tmp"), path.join(dirOf(), ".claude.json")), { code: "EACCES" });
      prep(envOf());
    }
    assert.strictEqual(fs.lstatSync(dirOf()).ino, ino, "the same directory every time");
    assert.deepStrictEqual(staleNames(), []);
    assertFixedDir(dirOf());
  });

  it("reuses a good directory: same inode, no stale copy, no churn", () => {
    prep(envOf());
    const before = fs.lstatSync(dirOf()).ino;
    prep(envOf());
    prep(envOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, before);
    assert.deepStrictEqual(staleNames(), []);
  });

  it("under the split, rewrites a .claude.json with other bytes or mode in place, without moving the directory", () => {
    process.env.UZI_UID_SPLIT = "1";
    prep(envOf());
    const ino = fs.lstatSync(dirOf()).ino;
    const file = path.join(dirOf(), ".claude.json");
    fs.chmodSync(file, 0o644);
    fs.writeFileSync(file, MALICIOUS);
    prep(envOf());
    fs.chmodSync(file, 0o444);
    assertFixedDir(dirOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, ino);
    assert.deepStrictEqual(staleNames(), []);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".cfg-")), []);
    fs.chmodSync(file, 0o644);
    prep(envOf());
    assertFixedDir(dirOf());
    assert.deepStrictEqual(staleNames(), []);
  });

  it("under the split, a successful rewrite on the last attempt after three re-corruptions is not a refusal", () => {
    process.env.UZI_UID_SPLIT = "1";
    prep(envOf());
    const file = path.join(dirOf(), ".claude.json");
    fs.chmodSync(file, 0o644);
    fs.writeFileSync(file, MALICIOUS);
    let rewrites = 0;
    prep(envOf(), {
      onStep: (name) => {
        if (name !== "rewritten") return;
        if (++rewrites <= 3) {
          fs.chmodSync(file, 0o644);
          fs.writeFileSync(file, MALICIOUS);
        }
      },
    });
    assert.strictEqual(rewrites, 4);
    assertFixedDir(dirOf());
  });

  it("a session transcript under HOME/.claude/projects is visible through the directory (resume)", () => {
    const t = path.join(home, ".claude", "projects", "-slug", "sid.jsonl");
    fs.mkdirSync(path.dirname(t), { recursive: true });
    fs.writeFileSync(t, "TRANSCRIPT");
    prep(envOf());
    assert.strictEqual(fs.readFileSync(path.join(dirOf(), "projects", "-slug", "sid.jsonl"), "utf8"), "TRANSCRIPT");
  });

  it("does nothing without a HOME", () => {
    const before = fs.readdirSync(home);
    for (const env of [undefined, {}, { HOME: undefined }, { HOME: "" }]) prep(env);
    assert.deepStrictEqual(fs.readdirSync(home), before);
  });
});

describe("prepareClaudeConfigDir: refusals", () => {
  it("refuses a HOME that is not a canonical absolute path", () => {
    for (const h of ["relative/home", `${home}/../${path.basename(home)}`, `${home}/`]) {
      assert.throws(() => prep({ HOME: h, CLAUDE_CONFIG_DIR: claudeConfigDir(h) }), TrustedExecutionRefusal, h);
    }
  });

  it("refuses a CLAUDE_CONFIG_DIR that is not <HOME>/.uzi-claude-config, and builds nothing", () => {
    for (const dir of [undefined, "", path.join(home, ".claude"), outside, `${dirOf()}/`, `${home}/x/../.uzi-claude-config`]) {
      assert.throws(
        () => prep({ HOME: home, ...(dir === undefined ? {} : { CLAUDE_CONFIG_DIR: dir }) }),
        TrustedExecutionRefusal,
        String(dir),
      );
    }
    assert.deepStrictEqual(fs.readdirSync(home), []);
  });

  it("through spawnDetached a mismatching CLAUDE_CONFIG_DIR refuses and starts no process", () => {
    const marker = path.join(root, "ran");
    assert.throws(
      () =>
        spawnDetached({
          command: "/bin/sh",
          args: ["-c", `echo ran > '${marker}'`],
          env: { PATH: "/usr/bin:/bin", HOME: home, CLAUDE_CONFIG_DIR: outside },
          signal: new AbortController().signal,
        } as SpawnOptions),
      TrustedExecutionRefusal,
    );
    assert.ok(!fs.existsSync(marker));
  });

  it("through spawnDetached a provider credential without a HOME refuses and starts no process; a credential-free spawn without a HOME still runs", async () => {
    const marker = path.join(root, "ran");
    const spawnWith = (env: Record<string, string | undefined>) =>
      spawnDetached({
        command: "/bin/sh",
        args: ["-c", `echo ran > '${marker}'`],
        env,
        signal: new AbortController().signal,
      } as SpawnOptions);
    for (const credential of ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]) {
      for (const noHome of [{}, { HOME: "" }]) {
        assert.throws(() => spawnWith({ PATH: "/usr/bin:/bin", [credential]: "x", ...noHome }), TrustedExecutionRefusal, credential);
        assert.ok(!fs.existsSync(marker), `${credential}: nothing was spawned`);
      }
    }
    const child = spawnWith({ PATH: "/usr/bin:/bin" });
    await new Promise<void>((resolve) => child.once("close", () => resolve()));
    assert.ok(fs.existsSync(marker), "a credential-free probe without a HOME is unaffected");
  });

  it("refuses a HOME that is a symlink", () => {
    const link = path.join(root, "homelink");
    fs.symlinkSync(home, link);
    assert.throws(() => prep(envOf(link)), TrustedExecutionRefusal);
    assert.deepStrictEqual(fs.readdirSync(home), []);
  });

  it("a directory that cannot be moved aside refuses", () => {
    fs.mkdirSync(dirOf());
    fs.writeFileSync(path.join(dirOf(), "planted"), "x");
    const real = fs.renameSync;
    (fs as { renameSync: typeof fs.renameSync }).renameSync = ((from: fs.PathLike, to: fs.PathLike) => {
      if (String(to).includes(".stale-")) throw Object.assign(new Error("EACCES"), { code: "EACCES" });
      real(from, to);
    }) as typeof fs.renameSync;
    try {
      assert.throws(() => prep(envOf()), TrustedExecutionRefusal);
    } finally {
      (fs as { renameSync: typeof fs.renameSync }).renameSync = real;
    }
    assert.ok(fs.existsSync(path.join(dirOf(), "planted")), "nothing deleted");
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".tmp-")), [], "the temp build is cleaned up");
  });
});

describe("prepareClaudeConfigDir: a planted directory is moved aside and rebuilt, nothing followed", () => {
  // Under the split the directory is 0755, so these plants can be written by the test's own uid.
  beforeEach(() => {
    process.env.UZI_UID_SPLIT = "1";
  });

  const planted: Array<[string, () => void]> = [
    ["a directory with an extra file", () => {
      fs.mkdirSync(dirOf());
      fs.writeFileSync(path.join(dirOf(), ".config.json"), MALICIOUS);
    }],
    ["a good-looking directory plus remote-settings.json", () => {
      prep(envOf());
      fs.writeFileSync(path.join(dirOf(), "remote-settings.json"), MALICIOUS);
    }],
    ["a regular file", () => fs.writeFileSync(dirOf(), MALICIOUS)],
    ["a symlink to an outside directory", () => fs.symlinkSync(outside, dirOf())],
    ["a dangling symlink", () => fs.symlinkSync(path.join(outside, "nope"), dirOf())],
    ["a directory with a group-writable mode", () => {
      prep(envOf());
      fs.chmodSync(dirOf(), 0o775);
    }],
    ["a directory whose projects link points elsewhere", () => {
      prep(envOf());
      fs.rmSync(path.join(dirOf(), "projects"));
      fs.symlinkSync(outside, path.join(dirOf(), "projects"));
    }],
    ["a directory whose projects link is a real directory", () => {
      prep(envOf());
      fs.rmSync(path.join(dirOf(), "projects"));
      fs.mkdirSync(path.join(dirOf(), "projects"));
    }],
  ];
  for (const [label, plant] of planted) {
    it(label, () => {
      plant();
      prep(envOf());
      assertFixedDir(dirOf());
      assert.strictEqual(staleNames().length, 1, "the old one is kept aside");
      assert.deepStrictEqual(fs.readdirSync(outside), ["sentinel"], "nothing was written outside");
      assert.strictEqual(fs.readFileSync(path.join(outside, "sentinel"), "utf8"), "SENTINEL");
    });
  }
});

describe("prepareClaudeConfigDir: the .claude targets", () => {
  it("a symlinked .claude is moved aside, not followed: the outside directory is untouched", () => {
    fs.writeFileSync(path.join(outside, ".config.json"), "SENTINEL-CONFIG");
    fs.symlinkSync(outside, path.join(home, ".claude"));
    prep(envOf());
    assert.ok(fs.lstatSync(path.join(home, ".claude")).isDirectory());
    assert.ok(fs.statSync(path.join(home, ".claude", "projects")).isDirectory());
    assert.ok(fs.statSync(path.join(home, ".claude", "tasks")).isDirectory());
    assert.deepStrictEqual(fs.readdirSync(outside).sort(), [".config.json", "sentinel"]);
    assert.strictEqual(fs.readFileSync(path.join(outside, ".config.json"), "utf8"), "SENTINEL-CONFIG");
    assert.ok(fs.readdirSync(home).some((f) => f.startsWith(".claude.stale-")));
  });

  it("a symlinked HOME/.claude/projects is not followed either: .claude is moved aside", () => {
    fs.mkdirSync(path.join(home, ".claude"));
    fs.symlinkSync(outside, path.join(home, ".claude", "projects"));
    prep(envOf());
    assert.ok(fs.lstatSync(path.join(home, ".claude", "projects")).isDirectory());
    assert.deepStrictEqual(fs.readdirSync(outside), ["sentinel"]);
  });

  it("a regular file at .claude is moved aside and a real directory takes its place", () => {
    fs.writeFileSync(path.join(home, ".claude"), "FILE");
    prep(envOf());
    assert.ok(fs.statSync(path.join(home, ".claude", "projects")).isDirectory());
    assert.strictEqual(fs.readFileSync(path.join(home, fs.readdirSync(home).find((f) => f.startsWith(".claude.stale-"))!), "utf8"), "FILE");
  });

  it("an existing .claude with its own projects is kept as is (resume data stays in place)", () => {
    const t = path.join(home, ".claude", "projects", "-s", "x.jsonl");
    fs.mkdirSync(path.dirname(t), { recursive: true });
    fs.writeFileSync(t, "T");
    fs.writeFileSync(path.join(home, ".claude", "history.jsonl"), "H");
    prep(envOf());
    assert.strictEqual(fs.readFileSync(t, "utf8"), "T");
    assert.ok(fs.statSync(path.join(home, ".claude", "tasks")).isDirectory(), "the missing tasks dir was added");
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")), []);
  });

  const claudeStale = (): string[] => fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-"));
  const assertTargets = (): void => {
    for (const t of ["projects", "tasks"]) {
      const st = fs.lstatSync(path.join(home, ".claude", t));
      assert.ok(st.isDirectory() && !st.isSymbolicLink(), t);
      assert.strictEqual(st.mode & 0o7777, 0o700, t);
    }
  };

  it("a regular file at .claude/projects or .claude/tasks: .claude is moved aside and the start is not refused (default mkdir)", () => {
    for (const name of ["projects", "tasks"]) {
      const h = path.join(root, `file-${name}`);
      fs.mkdirSync(path.join(h, ".claude"), { recursive: true });
      fs.writeFileSync(path.join(h, ".claude", name), "FILE");
      prepareClaudeConfigDir(envOf(h));
      for (const t of ["projects", "tasks"]) assert.ok(fs.lstatSync(path.join(h, ".claude", t)).isDirectory(), `${name}: ${t}`);
      assert.strictEqual(fs.readdirSync(h).filter((f) => f.startsWith(".claude.stale-")).length, 1, name);
    }
  });

  it("the default runner mkdir failing (unsearchable .claude) moves .claude aside once and recreates the targets", { skip: process.getuid?.() === 0 }, () => {
    fs.mkdirSync(path.join(home, ".claude"));
    fs.chmodSync(path.join(home, ".claude"), 0o000);
    prepareClaudeConfigDir(envOf());
    assertTargets();
    assert.strictEqual(claudeStale().length, 1);
    assertFixedDir(dirOf());
  });

  describe("a failed targets mkdir moves .claude aside only on a positive obstruction (uid split)", () => {
    const claude = (): string => path.join(home, ".claude");
    const clean = (): string => statLine(RUNNER_UID, "700", "directory", claude());
    const failTargetsOnce = (_args: readonly string[], n: number): number | undefined => (n === 1 ? 1 : undefined);
    const failEveryTargets = (args: readonly string[]): number | undefined => (args.at(-1) === claude() ? undefined : 1);
    const statArgs = (...missing: string[]): string[] => ["-c", "%u|%g|%a|%F|%n", "--", claude(), ...missing];
    beforeEach(() => {
      process.env.UZI_UID_SPLIT = "1";
      fs.mkdirSync(claude());
    });
    const untouched = (): void => {
      assert.strictEqual(claudeStale().length, 0, "no aside");
      assert.ok(fs.statSync(claude()).isDirectory());
      assert.ok(!fs.existsSync(dirOf()), "no config directory is built");
    };

    it("a file planted at a target: .claude is moved aside once, the targets are recreated, the start proceeds", () => {
      const projects = path.join(claude(), "projects");
      const h = helpers({
        mkdir: failTargetsOnce,
        stat: clean() + statLine(RUNNER_UID, "600", "regular empty file", projects),
      });
      prep(envOf(), { runAsRunner: h.run });
      assert.strictEqual(claudeStale().length, 1);
      assertTargets();
      assertFixedDir(dirOf());
      assert.deepStrictEqual(h.calls[1], { bin: "/bin/stat", args: statArgs(projects, path.join(claude(), "tasks")) });
      assert.deepStrictEqual(h.calls.map((c) => c.bin), ["/bin/mkdir", "/bin/stat", "/bin/mkdir", "/bin/mkdir"]);
    });

    const obstructions: Array<[string, () => string]> = [
      ["a regular file at a target", () => clean() + statLine(RUNNER_UID, "600", "regular file", "PROJECTS")],
      ["an empty regular file at a target", () => clean() + statLine(RUNNER_UID, "600", "regular empty file", "PROJECTS")],
      ["a symlink at a target", () => clean() + statLine(RUNNER_UID, "777", "symbolic link", "PROJECTS")],
      ["a fifo at a target", () => clean() + statLine(RUNNER_UID, "600", "fifo", "PROJECTS")],
      ["a .claude owned by another uid with no write and search for the runner", () => statLine(RUNNER_UID + 100, "700", "directory", "CLAUDE", 12345)],
      ["a .claude owned by another uid, group runner-cmd (not a runner group) with group rwx", () => statLine(RUNNER_UID + 100, "770", "directory", "CLAUDE", COMMAND_UID)],
      ["a .claude without owner write", () => statLine(RUNNER_UID, "500", "directory", "CLAUDE")],
      ["a .claude without owner search", () => statLine(RUNNER_UID, "600", "directory", "CLAUDE")],
      ["a .claude with mode 0", () => statLine(RUNNER_UID, "0", "directory", "CLAUDE")],
    ];
    for (const [label, make] of obstructions) {
      it(`${label} is a positive obstruction`, () => {
        const out = make().replaceAll("PROJECTS", path.join(claude(), "projects")).replaceAll("CLAUDE", claude());
        const h = helpers({ mkdir: failTargetsOnce, stat: out });
        prep(envOf(), { runAsRunner: h.run });
        assert.strictEqual(claudeStale().length, 1);
        assertTargets();
      });
    }

    // Cases where the runner can write and search .claude (or the mode evidence says nothing), so the
    // capacity failure is transient: the start is refused and the existing transcripts stay in place.
    const nonObstructions: Array<[string, () => string]> = [
      ["a foreign owner whose group is a runner group with group rwx", () => statLine(RUNNER_UID + 100, "770", "directory", "CLAUDE", RUNNER_UID)],
      ["a foreign owner whose group is codex-session with group rwx", () => statLine(RUNNER_UID + 100, "770", "directory", "CLAUDE", CODEX_SESSION_GID)],
      ["a foreign owner with other rwx", () => statLine(RUNNER_UID + 100, "777", "directory", "CLAUDE", 12345)],
      ["a foreign owner with other wx and no read", () => statLine(RUNNER_UID + 100, "703", "directory", "CLAUDE", 12345)],
      ["a runner-owned .claude without owner read", () => statLine(RUNNER_UID, "300", "directory", "CLAUDE")],
      ["a runner-owned .claude with a setgid bit", () => statLine(RUNNER_UID, "2700", "directory", "CLAUDE")],
    ];
    for (const [label, make] of nonObstructions) {
      it(`${label} is not an obstruction: the start is refused and .claude stays`, () => {
        const out = make().replaceAll("CLAUDE", claude());
        const h = helpers({ mkdir: failTargetsOnce, stat: out });
        assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /did not complete/);
        untouched();
      });
    }

    it("a capacity failure with a writable root keeps an existing transcript in place and refuses as transient", () => {
      const transcript = path.join(claude(), "projects", "-p", "s.jsonl");
      fs.mkdirSync(path.dirname(transcript), { recursive: true });
      fs.writeFileSync(transcript, "TRANSCRIPT");
      const roots = [
        statLine(RUNNER_UID + 100, "770", "directory", claude(), RUNNER_UID),
        statLine(RUNNER_UID + 100, "777", "directory", claude(), 12345),
        statLine(RUNNER_UID, "300", "directory", claude()),
      ];
      for (const stat of roots) {
        const h = helpers({ mkdir: () => 1, stat });
        assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /did not complete/);
        assert.strictEqual(fs.readFileSync(transcript, "utf8"), "TRANSCRIPT");
        untouched();
      }
    });

    it("a second obstruction after the move refuses, with exactly one aside", () => {
      const out = clean() + statLine(RUNNER_UID, "600", "regular file", path.join(claude(), "projects"));
      const h = helpers({ mkdir: failEveryTargets, stat: out });
      assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /could not create the \.claude targets/);
      assert.strictEqual(claudeStale().length, 1);
      assert.deepStrictEqual(h.calls.map((c) => c.bin), ["/bin/mkdir", "/bin/stat", "/bin/mkdir", "/bin/mkdir", "/bin/stat"]);
      assert.ok(!fs.existsSync(dirOf()));
    });

    it("a setpriv user-lookup failure (mkdir exit 1, no stat output) refuses and leaves .claude in place", () => {
      const h = helpers({ mkdir: failTargetsOnce, stat: { status: 1, signal: null, stdout: "" } });
      assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /did not complete/);
      untouched();
    });

    it("a capacity failure (mkdir exit 1, stat shows clean directories) refuses and leaves .claude in place", () => {
      const h = helpers({ mkdir: failTargetsOnce, stat: clean() });
      assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /did not complete/);
      untouched();
    });

    it("output for paths that were not asked about, or only share a prefix, is not evidence", () => {
      const projects = path.join(claude(), "projects");
      const stat = clean() + statLine(RUNNER_UID, "600", "regular file", `${projects}x`) + statLine(RUNNER_UID, "600", "regular file", path.join(home, "other"))
        + statLine(RUNNER_UID, "600", "regular file", `${projects}/`) + `garbage\n${RUNNER_UID}|${RUNNER_UID}|600|regular file\n${RUNNER_UID}|600|regular file|${projects}\n`;
      const h = helpers({ mkdir: failTargetsOnce, stat });
      assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /did not complete/);
      untouched();
    });

    it("a stuck or killed stat helper refuses and leaves .claude in place", () => {
      const outcomes: Array<(args: readonly string[]) => HelperResult> = [
        () => { throw new TrustedExecutionRefusal("stuck"); },
        () => ({ status: null, signal: "SIGKILL", stdout: "" }),
      ];
      for (const stat of outcomes) {
        const h = helpers({ mkdir: failTargetsOnce, stat });
        assert.throws(() => prep(envOf(), { runAsRunner: h.run }), TrustedExecutionRefusal);
        untouched();
      }
    });

    it("stat's exit status is ignored: partial output with a positive obstruction still counts", () => {
      const out = statLine(RUNNER_UID, "600", "regular file", path.join(claude(), "projects"));
      const h = helpers({ mkdir: failTargetsOnce, stat: { status: 124, signal: null, stdout: out } });
      prep(envOf(), { runAsRunner: h.run });
      assert.strictEqual(claudeStale().length, 1);
    });

    it("a total deadline passed between helpers refuses before the next spawn and keeps the tree", () => {
      let t = 0;
      const h = helpers({ mkdir: () => 1, stat: clean() });
      const slow = (bin: string, args: readonly string[]): HelperResult => {
        const r = h.run(bin, args);
        t += 11_000;
        return r;
      };
      assert.throws(() => prep(envOf(), { runAsRunner: slow, now: () => t }), /time limit/);
      assert.deepStrictEqual(h.calls.map((c) => c.bin), ["/bin/mkdir"], "no helper starts once the deadline has passed");
      untouched();
    });

    it("a deadline not yet reached lets every helper run", () => {
      let t = 0;
      const h = helpers({ mkdir: failTargetsOnce, stat: clean() + statLine(RUNNER_UID, "600", "regular file", path.join(claude(), "projects")) });
      const step = (bin: string, args: readonly string[]): HelperResult => {
        t += 2_000;
        return h.run(bin, args);
      };
      prep(envOf(), { runAsRunner: step, now: () => t });
      assert.strictEqual(claudeStale().length, 1);
    });
  });

  describe("one uid: no stat or timeout helper; the worker inspects .claude itself", () => {
    const claude = (): string => path.join(home, ".claude");
    beforeEach(() => fs.mkdirSync(claude(), { mode: 0o700 }));

    it("mkdir failing with clean directories refuses, spawns only mkdir and leaves .claude in place", () => {
      const h = helpers({ mkdir: () => 1 });
      assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: h.run }), /did not complete/);
      assert.deepStrictEqual([...new Set(h.calls.map((c) => c.bin))], ["/bin/mkdir"]);
      assert.strictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")).length, 0);
    });

    it("mkdir failing with a runner-owned 0300 .claude (writable and searchable) refuses and leaves it in place", { skip: process.getuid?.() === 0 ? "root can always write and search, so the mode is not distinguishing" : false }, () => {
      fs.chmodSync(claude(), 0o300);
      try {
        const h = helpers({ mkdir: () => 1 });
        assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: h.run }), /did not complete/);
        assert.strictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")).length, 0);
        assert.ok(fs.lstatSync(claude()).isDirectory());
      } finally {
        fs.chmodSync(claude(), 0o700);
      }
    });

    it("mkdir failing with an owner-unwritable .claude moves it aside once and the start proceeds", () => {
      fs.chmodSync(claude(), 0o500);
      const h = helpers({ mkdir: (_a, n) => (n === 1 ? 1 : undefined) });
      prepareClaudeConfigDir(envOf(), { runAsRunner: h.run });
      assert.strictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")).length, 1);
      assert.deepStrictEqual([...new Set(h.calls.map((c) => c.bin))], ["/bin/mkdir"]);
      assertFixedDir(dirOf());
    });
  });

  describe("one uid: a 0555 config directory that was tampered with is detected and rebuilt", { skip: process.getuid?.() === 0 }, () => {
    const tamper = (fn: (dir: string) => void): void => {
      prep(envOf());
      fs.chmodSync(dirOf(), 0o755);
      fn(dirOf());
      fs.chmodSync(dirOf(), 0o555);
      prep(envOf());
      assertFixedDir(dirOf());
      assert.strictEqual(staleNames().length, 1, "the tampered directory is kept aside");
    };
    it("an extra entry", () => tamper((d) => fs.writeFileSync(path.join(d, "remote-settings.json"), MALICIOUS)));
    it("a retargeted link", () =>
      tamper((d) => {
        fs.rmSync(path.join(d, "projects"));
        fs.symlinkSync(outside, path.join(d, "projects"));
      }));
    it("other bytes in .claude.json", () =>
      tamper((d) => {
        fs.chmodSync(path.join(d, ".claude.json"), 0o644);
        fs.writeFileSync(path.join(d, ".claude.json"), '{"hasCompletedOnboarding":true,"numStartups":2}\n');
        fs.chmodSync(path.join(d, ".claude.json"), 0o444);
      }));
    it("another mode on .claude.json", () => tamper((d) => fs.chmodSync(path.join(d, ".claude.json"), 0o644)));
  });
});

describe("prepareClaudeConfigDir: the uid-split checks", () => {
  beforeEach(() => {
    process.env.UZI_UID_SPLIT = "1";
  });

  it("makes HOME sticky and builds the directory (the test's own uid stands in for the worker)", () => {
    fs.chmodSync(home, 0o775);
    prep(envOf());
    assert.ok((fs.statSync(home).mode & 0o1000) !== 0, "HOME is sticky");
    assert.strictEqual(fs.statSync(home).mode & 0o777, 0o775, "the other mode bits are kept");
    assertFixedDir(dirOf());
  });

  it("a planted symlinked .claude and config directory are moved aside, never followed", () => {
    fs.writeFileSync(path.join(outside, ".config.json"), "SENTINEL-CONFIG");
    fs.symlinkSync(outside, path.join(home, ".claude"));
    fs.symlinkSync(outside, dirOf());
    prep(envOf());
    assertFixedDir(dirOf());
    assert.deepStrictEqual(fs.readdirSync(outside).sort(), [".config.json", "sentinel"]);
    assert.strictEqual(fs.readFileSync(path.join(outside, ".config.json"), "utf8"), "SENTINEL-CONFIG");
  });

  const self = (): number => process.getuid?.() ?? 0;
  const parentOf = (): string => path.dirname(home);
  const grandparentOf = (): string => path.dirname(parentOf());
  const withAncestors = (over: Record<string, { mode?: number; uid?: number; gid?: number }>): PrepareDeps => ({ lstatAncestor: ancestors(over) });
  const refusedNothingBuilt = (deps: PrepareDeps, message?: RegExp): void => {
    assert.throws(() => prep(envOf(), deps), message ?? TrustedExecutionRefusal);
    assert.deepStrictEqual(fs.readdirSync(home), [], "nothing was built");
  };

  it("refuses a HOME whose parent is world-writable and not sticky; a sticky world-writable parent is accepted", () => {
    refusedNothingBuilt(withAncestors({ [parentOf()]: { mode: 0o40777, uid: self() } }), /world-writable/);
    prep(envOf(), withAncestors({ [parentOf()]: { mode: 0o41777, uid: self() } }));
    assertFixedDir(dirOf());
  });

  it("single-uid: no ancestor check is made", () => {
    delete process.env.UZI_UID_SPLIT;
    prep(envOf(), withAncestors({ [parentOf()]: { mode: 0o40777, uid: 12345 } }));
    assertFixedDir(dirOf());
  });

  it("refuses a parent owned by a runner uid, and any ancestor owned by neither root nor the worker", () => {
    for (const uid of [10002, 10003, 12345].filter((u) => u !== self())) {
      refusedNothingBuilt(withAncestors({ [parentOf()]: { uid } }), /owned by neither root nor the worker/);
      refusedNothingBuilt(withAncestors({ [grandparentOf()]: { uid } }), /owned by neither root nor the worker/);
      refusedNothingBuilt(withAncestors({ "/": { uid } }), /owned by neither root nor the worker/);
    }
    prep(envOf(), withAncestors({ [parentOf()]: { uid: self() }, [grandparentOf()]: { uid: 0 } }));
    assertFixedDir(dirOf());
  });

  it("refuses a HOME owned by another uid and builds nothing; a HOME owned by the worker is accepted", () => {
    const asOwnedBy = (uid: number) => (p: string): fs.Stats => Object.assign(Object.create(Object.getPrototypeOf(fs.lstatSync(p))), fs.lstatSync(p), { uid });
    refusedNothingBuilt({ lstatHome: asOwnedBy(self() + 1) }, /HOME is not owned by the worker/);
    prep(envOf(), { lstatHome: asOwnedBy(self()) });
    assertFixedDir(dirOf());
  });

  it("refuses an ancestor that is a symlink, at any depth", () => {
    for (const p of [parentOf(), grandparentOf(), "/"]) {
      refusedNothingBuilt(withAncestors({ [p]: { mode: 0o120777 } }), /not a real directory/);
    }
  });

  it("refuses a HOME that is reached through a symlinked directory", () => {
    const link = path.join(root, "linkdir");
    fs.symlinkSync(parentOf(), link);
    const viaLink = path.join(link, path.basename(home));
    assert.throws(() => prep(envOf(viaLink)), /not its own real path/);
    assert.deepStrictEqual(fs.readdirSync(home), []);
  });

  it("refuses a non-sticky group-writable ancestor in a runner group, at any depth", () => {
    for (const gid of [10002, 10003, 10004]) {
      refusedNothingBuilt(withAncestors({ [parentOf()]: { mode: 0o40775, gid } }), /group-writable by a runner group/);
      refusedNothingBuilt(withAncestors({ [grandparentOf()]: { mode: 0o42775, gid } }), /group-writable by a runner group/);
    }
  });

  it("refuses a non-sticky world-writable ancestor above the parent", () => {
    refusedNothingBuilt(withAncestors({ [grandparentOf()]: { mode: 0o40777 } }), /world-writable/);
  });

  it("accepts the hosted 2775 data root (group 10001), a sticky ancestor in a runner group, and ancestors that are not group-writable", () => {
    for (const [mode, gid] of [[0o42775, 10001], [0o41775, 10002], [0o40755, 10002], [0o43775, 10001]] as const) {
      for (const p of [parentOf(), grandparentOf()]) {
        prep(envOf(), withAncestors({ [p]: { mode, gid, uid: self() } }));
        assertFixedDir(dirOf());
        fs.rmSync(dirOf(), { recursive: true, force: true });
      }
    }
  });
});

describe("prepareClaudeConfigDir: a hostile process acting between any two steps", () => {
  // Under the split the directory is writable by the test's own uid, so each mutation really lands.
  beforeEach(() => {
    process.env.UZI_UID_SPLIT = "1";
  });

  type Mutation = [string, (d: string) => void];
  const mutations: Mutation[] = [
    ["replace .claude.json bytes", (d) => {
      fs.chmodSync(path.join(d, ".claude.json"), 0o644);
      fs.writeFileSync(path.join(d, ".claude.json"), MALICIOUS);
    }],
    ["add .config.json", (d) => fs.writeFileSync(path.join(d, ".config.json"), MALICIOUS)],
    ["add remote-settings.json", (d) => fs.writeFileSync(path.join(d, "remote-settings.json"), MALICIOUS)],
    ["retarget the projects link", (d) => {
      fs.rmSync(path.join(d, "projects"));
      fs.symlinkSync(outside, path.join(d, "projects"));
    }],
    ["rename the directory away", (d) => fs.renameSync(d, `${d}.moved`)],
    ["replace the directory with a symlink to an outside dir", (d) => {
      fs.renameSync(d, `${d}.moved`);
      fs.symlinkSync(outside, d);
    }],
  ];
  const scenarios: Array<[string, () => void]> = [
    ["no directory yet", () => {}],
    ["a planted bad directory", () => {
      fs.mkdirSync(dirOf());
      fs.writeFileSync(path.join(dirOf(), "extra"), "x");
    }],
    ["a good directory", () => prep(envOf())],
  ];

  for (const [scenarioLabel, setup] of scenarios) {
    it(`${scenarioLabel}: at every step, the start either rebuilds before the spawn or refuses; the child never sees other bytes`, async () => {
      // Dry run: how many steps does this scenario pass through?
      setup();
      const names: string[] = [];
      prep(envOf(), { onStep: (n) => names.push(n) });
      fs.rmSync(home, { recursive: true, force: true });
      fs.mkdirSync(home);
      assert.ok(names.length >= 3, "the scenario passes through several steps");
      let started = 0;
      for (let at = 0; at < names.length; at++) {
        for (const [mLabel, mutate] of mutations) {
          fs.rmSync(home, { recursive: true, force: true });
          fs.mkdirSync(home);
          setup();
          let n = 0;
          const deps: PrepareDeps = {
            runAsRunner: runDirect,
            lstatAncestor: ancestors(),
            onStep: () => {
              if (n++ === at) {
                try {
                  mutate(dirOf());
                } catch {
                  /* nothing there to mutate at this step */
                }
              }
            },
          };
          const out = path.join(root, `dump-${at}`);
          const label = `${scenarioLabel} / step ${at} (${names[at]}) / ${mLabel}`;
          // spawnDetached would wrap the child in setpriv under the split flag this suite sets; the
          // preparation it runs first is the code under test, so run that and then start the child.
          let child: ReturnType<typeof spawn>;
          try {
            prep(envOf(), deps);
            child = spawn(process.execPath, ["-e", DUMP_SCRIPT, out], { env: envOf(), cwd: root, stdio: "ignore" });
          } catch (err) {
            assert.ok(err instanceof TrustedExecutionRefusal, label);
            assert.ok(!fs.existsSync(out), `${label}: a refused start spawned nothing`);
            continue;
          }
          await new Promise<void>((resolve) => child.once("close", () => resolve()));
          started++;
          assert.strictEqual(fs.readFileSync(out, "utf8"), CLAUDE_GLOBAL_CONFIG_BYTES, label);
          assert.strictEqual(fs.readFileSync(`${out}.alt`, "utf8"), "ABSENT", label);
          assert.strictEqual(fs.readFileSync(`${out}.dir`, "utf8"), ".claude.json,projects,tasks", label);
          fs.rmSync(out, { force: true });
          fs.rmSync(`${out}.alt`, { force: true });
          fs.rmSync(`${out}.dir`, { force: true });
          assert.deepStrictEqual(fs.readdirSync(outside), ["sentinel"], `${label}: nothing written outside`);
        }
      }
      assert.ok(started > 0, "a start went through");
    });
  }
});

describe("prepareClaudeConfigDir: concurrent prepares on one HOME (the shared chat root)", () => {
  it("a second prepare finishing while the first has built but not installed leaves one good directory", () => {
    let inodeAfterNested: number | undefined;
    let nested = false;
    prep(envOf(), {
      onStep: (name) => {
        if (name === "built" && !nested) {
          nested = true;
          prep(envOf());
          inodeAfterNested = fs.lstatSync(dirOf()).ino;
        }
      },
    });
    assert.ok(nested);
    assertFixedDir(dirOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, inodeAfterNested, "the good directory the second prepare built was kept");
    assert.deepStrictEqual(staleNames(), []);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".tmp-")), [], "the first prepare's temp was removed");
  });

  it("two prepares racing over a bad directory move it aside once and keep the good one", () => {
    fs.mkdirSync(dirOf());
    fs.writeFileSync(path.join(dirOf(), "extra"), "x");
    let inodeAfterNested: number | undefined;
    let nested = false;
    prep(envOf(), {
      onStep: (name) => {
        if (name === "built" && !nested) {
          nested = true;
          prep(envOf());
          inodeAfterNested = fs.lstatSync(dirOf()).ino;
        }
      },
    });
    assertFixedDir(dirOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, inodeAfterNested);
    assert.strictEqual(staleNames().length, 1);
  });
});

describe("a HOME that contains the config directory reclaims fully", () => {
  it("rmHomeTree removes the HOME with the config directory, its links and an aside copy", async () => {
    fs.writeFileSync(path.join(home, ".claude.json.keep"), "x");
    fs.mkdirSync(path.join(home, "go", "pkg", "mod"), { recursive: true });
    fs.writeFileSync(path.join(home, "go", "pkg", "mod", "f"), "m");
    prep(envOf());
    fs.chmodSync(dirOf(), 0o755); // the one-uid agent can reopen its own 0555 directory
    fs.writeFileSync(path.join(dirOf(), "extra"), "x");
    prep(envOf()); // leaves an aside copy
    fs.mkdirSync(path.join(home, ".claude", "projects", "-s"), { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "projects", "-s", "t.jsonl"), "T");
    assert.strictEqual(staleNames().length, 1);
    await rmHomeTree(home);
    assert.ok(!fs.existsSync(home));
  });

  it("the one-uid 0555 config directory is removed with its HOME", async () => {
    prep(envOf());
    assert.strictEqual(fs.lstatSync(dirOf()).mode & 0o777, 0o555);
    await rmHomeTree(home);
    assert.ok(!fs.existsSync(home));
  });

  it("with a runner pass that cannot write the worker-owned directory, the worker's final pass still removes the HOME", async () => {
    process.env.UZI_UID_SPLIT = "1";
    prep(envOf());
    fs.mkdirSync(path.join(home, ".claude", "projects", "-s"), { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "projects", "-s", "t.jsonl"), "T");
    let purged = 0;
    await rmHomeTree(home, {
      splitActive: true,
      // The first worker pass fails as it would on a runner-private tree; the runner pass is the
      // real purge script run here as the same uid (a stand-in that is stronger than the real one).
      removeTree: (() => {
        let calls = 0;
        return async (target: string) => {
          if (calls++ === 0) throw Object.assign(new Error("EACCES"), { code: "EACCES" });
          fs.rmSync(target, { recursive: true, force: true });
        };
      })(),
      purgeChildrenAsAgents: async (target: string) => {
        purged++;
        await execFileAsync(process.execPath, ["-e", PURGE_CHILDREN_SCRIPT, target]).catch(() => undefined);
      },
    });
    assert.strictEqual(purged, 1);
    assert.ok(!fs.existsSync(home));
  });
});

describe("the bounded runner helper", () => {
  it("under the split each helper is the runner-uid setpriv around timeout -s KILL 2", () => {
    assert.deepStrictEqual(helperCommand("/bin/mkdir", ["-p", "--", "/x"], true), {
      command: "/bin/setpriv",
      args: [...setprivRunnerArgs(), "/usr/bin/timeout", "-s", "KILL", "2", "/bin/mkdir", "-p", "--", "/x"],
    });
    assert.deepStrictEqual(helperCommand("/bin/stat", ["-c", "%u", "--", "/x"], false), {
      command: "/bin/stat",
      args: ["-c", "%u", "--", "/x"],
    });
  });

  it("the helper's stdout reaches the caller through the intermediary's output file", () => {
    const r = runBounded("/bin/sh", ["-c", "printf '10002|2700|directory|/x\\n'"]);
    assert.deepStrictEqual(r, { status: 0, signal: null, stdout: "10002|2700|directory|/x\n" });
    assert.strictEqual(runBounded("/bin/sh", ["-c", "exit 1"]).status, 1);
  });

  it("a helper that ignores its own timeout is cut off by the intermediary's kill within the bound, and the preparation treats it as transient", { skip: !fs.existsSync("/bin/sh") || !fs.existsSync("/bin/sleep") }, () => {
    const SHORT_TIMEOUT_MS = 200;
    const t0 = Date.now();
    assert.throws(() => runBounded("/bin/sh", ["-c", "trap '' TERM; exec sleep 3"], SHORT_TIMEOUT_MS), TrustedExecutionRefusal);
    const elapsed = Date.now() - t0;
    assert.ok(elapsed < 2_000, `returned after ${elapsed} ms`);
    // The same through the preparation: it refuses and moves nothing.
    fs.mkdirSync(path.join(home, ".claude", "projects"), { recursive: true });
    const stuck = (): HelperResult => runBounded("/bin/sh", ["-c", "trap '' TERM; exec sleep 3"], SHORT_TIMEOUT_MS);
    assert.throws(() => prepareClaudeConfigDir(envOf(), { runAsRunner: stuck }), TrustedExecutionRefusal);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".stale-")), []);
  });

  it("output over 64 KiB refuses, and a stat helper doing that moves nothing", () => {
    const big = (): HelperResult => runBounded("/bin/sh", ["-c", "head -c 70000 /dev/zero"]);
    assert.throws(() => big(), /output limit/);
    process.env.UZI_UID_SPLIT = "1";
    const claude = path.join(home, ".claude");
    fs.mkdirSync(claude);
    const h = helpers({ mkdir: () => 1, stat: () => big() });
    assert.throws(() => prep(envOf(), { runAsRunner: h.run }), /output limit/);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".stale-")), []);
    assert.ok(fs.statSync(claude).isDirectory());
  });

  it("a command that does not exist returns status 127", () => {
    const r = runBounded(path.join(root, "no-such-binary"), []);
    assert.strictEqual(r.status, 127);
  });

  const PRINT_ARGV = "process.stdout.write(JSON.stringify(process.argv.slice(1)))";

  it("arguments with spaces and shell metacharacters reach the helper verbatim and nothing is evaluated", () => {
    const marker = path.join(root, "injected");
    const meta = [`$(touch ${marker})`, `;touch ${marker}`, "`touch " + marker + "`", 'a "b" c', "it's", "x y\tz", "*", "-n", "--"];
    const r = runBounded(process.execPath, ["-e", PRINT_ARGV, "--", ...meta]);
    assert.strictEqual(r.status, 0);
    assert.deepStrictEqual(JSON.parse(r.stdout), meta);
    assert.ok(!fs.existsSync(marker), "no metacharacter was evaluated");
  });

  it("a TMPDIR with spaces and shell metacharacters is used as a plain path", () => {
    // A single path component (no slash) so mkdirSync works; a shell evaluating it would create
    // `injected-tmp` relative to its cwd, which is the cwd of this process (and so of the helper).
    const marker = "injected-tmp";
    const priv = path.join(root, `tmp dir $(touch ${marker}); "q" 'r'`);
    fs.mkdirSync(priv);
    const probes = [path.resolve(marker), path.join(root, marker), path.join(priv, marker)];
    const savedTmp = process.env.TMPDIR;
    process.env.TMPDIR = priv;
    try {
      const r = runBounded(process.execPath, ["-e", PRINT_ARGV, "--", "ok"]);
      assert.strictEqual(r.status, 0);
      assert.deepStrictEqual(JSON.parse(r.stdout), ["ok"]);
    } finally {
      if (savedTmp === undefined) delete process.env.TMPDIR;
      else process.env.TMPDIR = savedTmp;
    }
    for (const p of probes) assert.ok(!fs.existsSync(p), `no metacharacter in the directory name was evaluated (${p})`);
    assert.deepStrictEqual(fs.readdirSync(priv), [], "the output directory is removed");
  });

  // CodeQL alert 229 guard: runBounded must start the intermediary directly, never through a shell.
  it("runBounded spawns process.execPath and no shell (source guard)", () => {
    const src = fs.readFileSync(new URL("../src/claude-global-config.ts", import.meta.url), "utf8");
    const start = src.indexOf("export function runBounded");
    assert.ok(start >= 0, "runBounded found");
    const end = src.indexOf("\n}\n", start);
    assert.ok(end > start, "runBounded end found after its start (the slice is not vacuous)");
    const body = src.slice(start, end);
    assert.ok(NODE_INTERMEDIARY.includes("shell: false"), "the intermediary sets shell: false");
    assert.ok(!NODE_INTERMEDIARY.includes("/bin/sh"), "the intermediary does not name /bin/sh");
    assert.ok(!NODE_INTERMEDIARY.includes("shell: true"), "the intermediary does not set shell: true");
    assert.ok(body.includes("process.execPath"), "runBounded spawns process.execPath");
    assert.ok(!body.includes('"/bin/sh"'), "runBounded does not spawn /bin/sh");
    assert.ok(!/shell:\s*true/.test(body), "runBounded does not set shell: true");
  });

  // Without the try/catch node exits 1 on the uncaught openSync throw, which mkdirAsRunner would read
  // as "mkdir ran and failed".
  it("the intermediary exits 127, not 1, when opening the output file throws", () => {
    const dir = fs.mkdtempSync(path.join(root, "intermediary-"));
    const target = path.join(dir, "target");
    fs.writeFileSync(target, "unchanged");
    const link = path.join(dir, "link");
    fs.symlinkSync(target, link);
    for (const out of [path.join(dir, "missing", "out"), link]) {
      const r = spawnSync(process.execPath, ["-e", NODE_INTERMEDIARY, "--", out, "/bin/true"], { stdio: "ignore" });
      assert.strictEqual(r.status, 127, `status for ${out}`);
    }
    assert.strictEqual(fs.readFileSync(target, "utf8"), "unchanged", "the symlink target is untouched");
  });

  it("a helper killed by a signal reports 128 plus the signal number", () => {
    const r = runBounded("/bin/sh", ["-c", "kill -9 $$"]);
    assert.strictEqual(r.status, 128 + 9);
    assert.strictEqual(r.signal, null);
  });

  it("leaves no output directory behind", () => {
    const savedTmp = process.env.TMPDIR;
    const priv = fs.mkdtempSync(path.join(root, "tmpdir-"));
    process.env.TMPDIR = priv;
    try {
      runBounded("/bin/sh", ["-c", "true"]);
      assert.throws(() => runBounded("/bin/sh", ["-c", "head -c 70000 /dev/zero"]));
    } finally {
      if (savedTmp === undefined) delete process.env.TMPDIR;
      else process.env.TMPDIR = savedTmp;
    }
    assert.deepStrictEqual(fs.readdirSync(priv), []);
  });
});

describe("prepareClaudeConfigDir: the final rename failing", () => {
  const eacces = (): never => {
    throw Object.assign(new Error("EACCES"), { code: "EACCES" });
  };

  it("EACCES with a valid directory a concurrent prepare installed reuses it and builds nothing more", () => {
    let calls = 0;
    prep(envOf(), {
      rename: (from, to) => {
        calls++;
        prep(envOf()); // the winner installs its own directory first
        assert.ok(from !== to);
        eacces();
      },
    });
    assert.strictEqual(calls, 1);
    assertFixedDir(dirOf());
    assert.deepStrictEqual(staleNames(), []);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".tmp-")), [], "the loser's temp is removed");
  });

  it("EACCES with nothing valid at the destination refuses and leaves no temp", () => {
    assert.throws(() => prep(envOf(), { rename: () => eacces() }), TrustedExecutionRefusal);
    assert.ok(!fs.existsSync(dirOf()));
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".tmp-")), []);
  });

  it("a changed entry at the destination re-enters the loop: the bad entry is moved aside and the next attempt installs", () => {
    let n = 0;
    prep(envOf(), {
      rename: (from, to) => {
        if (++n === 1) {
          fs.mkdirSync(dirOf());
          fs.writeFileSync(path.join(dirOf(), "planted"), "x");
          eacces();
        }
        fs.renameSync(from, to);
      },
    });
    assert.strictEqual(n, 2);
    assertFixedDir(dirOf());
    assert.strictEqual(staleNames().length, 1);
  });

  it("a successful install on the last attempt after three failed ones is not a refusal", () => {
    let n = 0;
    prep(envOf(), {
      rename: (from, to) => {
        if (++n <= 3) {
          fs.mkdirSync(dirOf());
          fs.writeFileSync(path.join(dirOf(), "planted"), "x");
          throw Object.assign(new Error("EEXIST"), { code: "EEXIST" });
        }
        fs.renameSync(from, to);
      },
    });
    assert.strictEqual(n, 4);
    assertFixedDir(dirOf());
  });

  it("EEXIST-class errors with another valid directory present reuse it", () => {
    prep(envOf(), {
      rename: () => {
        prep(envOf());
        throw Object.assign(new Error("EEXIST"), { code: "EEXIST" });
      },
    });
    assertFixedDir(dirOf());
  });
});
