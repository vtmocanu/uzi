import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { CLAUDE_GLOBAL_CONFIG_BYTES, prepareClaudeConfigDir, type PrepareDeps } from "../src/claude-global-config.js";
import { claudeConfigDir } from "../src/claude-config-dir.js";
import { rmHomeTree, PURGE_CHILDREN_SCRIPT } from "../src/rmtree.js";
import { spawnDetached } from "../src/sdk-spawn.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { execFile, execFileSync, spawn } from "node:child_process";
import { promisify } from "node:util";

/**
 * PRD #4416: direct tests of the config-directory preparation and of the spawn seam. One uid cannot
 * show the kernel's sticky/ownership enforcement (the runner being unable to rename or write into
 * the worker-owned directory); these tests show the worker-side behaviour: what is built, what is
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

/** The runner-uid mkdir stand-in for tests: the same argv, run directly (these tests are one uid). */
const mkdirStub = (args: readonly string[]): void => {
  execFileSync("/bin/mkdir", [...args]);
};
const prep = (env: Parameters<typeof prepareClaudeConfigDir>[0], deps: PrepareDeps = {}): void =>
  prepareClaudeConfigDir(env, { mkdirAsRunner: mkdirStub, ...deps });
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

/** Run `fn` with fs.statSync reporting the parent directory with the given mode and gid. */
function withParent(mode: number, gid: number, fn: () => void, parentOf?: string): void {
  const real = fs.statSync;
  const parent = parentOf ?? path.dirname(home);
  (fs as { statSync: typeof fs.statSync }).statSync = ((p: fs.PathLike, ...rest: unknown[]) => {
    const st = (real as (...a: unknown[]) => fs.Stats)(p, ...rest);
    return String(p) === parent ? Object.assign(Object.create(Object.getPrototypeOf(st)), st, { mode, gid }) : st;
  }) as typeof fs.statSync;
  try {
    fn();
  } finally {
    (fs as { statSync: typeof fs.statSync }).statSync = real;
  }
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

  it("creates the targets through the runner-uid mkdir seam only, with a fixed argv, and repeats nothing", () => {
    const calls: string[][] = [];
    const rec = (args: readonly string[]): void => {
      calls.push([...args]);
      mkdirStub(args);
    };
    prepareClaudeConfigDir(envOf(), { mkdirAsRunner: rec });
    const claude = path.join(home, ".claude");
    assert.deepStrictEqual(calls, [
      ["-p", "-m", "700", "--", claude],
      ["-p", "-m", "700", "--", path.join(claude, "projects"), path.join(claude, "tasks")],
    ]);
    prepareClaudeConfigDir(envOf(), { mkdirAsRunner: rec });
    assert.strictEqual(calls.length, 2, "existing targets spawn nothing");
    fs.rmSync(path.join(claude, "tasks"), { recursive: true });
    prepareClaudeConfigDir(envOf(), { mkdirAsRunner: rec });
    assert.deepStrictEqual(calls[2], ["-p", "-m", "700", "--", path.join(claude, "tasks")]);
  });

  it("a failing runner-uid mkdir refuses the start and builds no config directory", () => {
    assert.throws(
      () => prepareClaudeConfigDir(envOf(), { mkdirAsRunner: () => { throw new Error("mkdir failed"); } }),
      TrustedExecutionRefusal,
    );
    assert.ok(!fs.existsSync(dirOf()));
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

  it("a runner mkdir that fails once is retried after .claude is moved aside; failing twice refuses", () => {
    fs.mkdirSync(path.join(home, ".claude"));
    let calls = 0;
    prepareClaudeConfigDir(envOf(), {
      mkdirAsRunner: (a) => {
        if (++calls === 1) throw new Error("mkdir failed");
        mkdirStub(a);
      },
    });
    assert.strictEqual(calls, 3, "targets (fails), .claude, targets");
    assert.strictEqual(claudeStale().length, 1);
    assertTargets();

    const h = path.join(root, "twice");
    fs.mkdirSync(path.join(h, ".claude"), { recursive: true });
    let n = 0;
    assert.throws(
      () => prepareClaudeConfigDir(envOf(h), { mkdirAsRunner: () => { n++; throw new Error("mkdir failed"); } }),
      TrustedExecutionRefusal,
    );
    assert.strictEqual(n, 2, "the targets mkdir fails, .claude moves aside, the retry fails, then refusal");
    assert.ok(!fs.existsSync(claudeConfigDir(h)), "no config directory is built");
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

  it("refuses a HOME whose parent is world-writable and not sticky; group-writable (a hosted 2775 data root) and sticky parents are accepted", () => {
    const parent = path.join(root, "parent");
    fs.mkdirSync(parent);
    const h = path.join(parent, "h");
    fs.mkdirSync(h);
    fs.chmodSync(parent, 0o777);
    assert.throws(() => prep(envOf(h)), TrustedExecutionRefusal);
    assert.deepStrictEqual(fs.readdirSync(h), []);
    fs.chmodSync(parent, 0o2775);
    // The hosted data root is group worker (10001); this test process's own group is not that.
    withParent(0o42775, 10001, () => prep(envOf(h)), parent);
    assertFixedDir(claudeConfigDir(h));
    fs.rmSync(claudeConfigDir(h), { recursive: true });
    fs.chmodSync(parent, 0o777);
    assert.throws(() => prep(envOf(h)), TrustedExecutionRefusal);
    fs.chmodSync(parent, 0o1777);
    prep(envOf(h));
    assertFixedDir(claudeConfigDir(h));
    // Single-uid: no such check.
    delete process.env.UZI_UID_SPLIT;
    fs.rmSync(claudeConfigDir(h), { recursive: true });
    fs.chmodSync(parent, 0o777);
    prep(envOf(h));
  });
});

describe("prepareClaudeConfigDir: a group-writable non-sticky parent in a runner group", () => {
  beforeEach(() => {
    process.env.UZI_UID_SPLIT = "1";
  });

  for (const gid of [10002, 10003, 10004]) {
    it(`refuses gid ${gid} with mode 2775 (group-writable, not sticky)`, () => {
      withParent(0o40775, gid, () => assert.throws(() => prep(envOf()), TrustedExecutionRefusal));
      assert.deepStrictEqual(fs.readdirSync(home), [], "nothing was built");
    });
  }

  it("accepts the hosted 2775 data root (group 10001), a sticky parent in a runner group, and a parent not group-writable", () => {
    for (const [mode, gid] of [[0o42775, 10001], [0o41775, 10002], [0o40755, 10002]] as const) {
      withParent(mode, gid, () => prep(envOf()));
      assertFixedDir(dirOf());
      fs.rmSync(dirOf(), { recursive: true, force: true });
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
            mkdirAsRunner: mkdirStub,
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
