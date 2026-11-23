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
import { execFile } from "node:child_process";
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

const dirOf = (): string => claudeConfigDir(home);
const envOf = (h: string = home): Record<string, string> => ({ HOME: h, CLAUDE_CONFIG_DIR: claudeConfigDir(h) });
const staleNames = (): string[] => fs.readdirSync(home).filter((f) => f.includes(".stale-"));

/** An independent description of "this is the fixed directory", not derived from the module's checks. */
function assertFixedDir(dir: string): void {
  const st = fs.lstatSync(dir);
  assert.ok(st.isDirectory() && !st.isSymbolicLink(), "a real directory");
  assert.strictEqual(st.mode & 0o7777 & ~0o2000, 0o755, "mode 0755, never group- or world-writable");
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
  fs.rmSync(root, { recursive: true, force: true });
  if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
  else process.env.UZI_UID_SPLIT = savedSplit;
});

describe("prepareClaudeConfigDir: build and reuse", () => {
  it("builds exactly the fixed directory and the writable targets", () => {
    prepareClaudeConfigDir(envOf());
    assertFixedDir(dirOf());
    for (const t of ["projects", "tasks"]) assert.ok(fs.statSync(path.join(home, ".claude", t)).isDirectory(), t);
    assert.deepStrictEqual(fs.readdirSync(home).sort(), [".claude", ".uzi-claude-config"], "no temp or stale entry left");
  });

  it("reuses a good directory: same inode, no stale copy, no churn", () => {
    prepareClaudeConfigDir(envOf());
    const before = fs.lstatSync(dirOf()).ino;
    prepareClaudeConfigDir(envOf());
    prepareClaudeConfigDir(envOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, before);
    assert.deepStrictEqual(staleNames(), []);
  });

  it("rewrites a .claude.json with other bytes or mode in place, without moving the directory", () => {
    prepareClaudeConfigDir(envOf());
    const ino = fs.lstatSync(dirOf()).ino;
    const file = path.join(dirOf(), ".claude.json");
    fs.chmodSync(file, 0o644);
    fs.writeFileSync(file, MALICIOUS);
    prepareClaudeConfigDir(envOf());
    fs.chmodSync(file, 0o444);
    assertFixedDir(dirOf());
    assert.strictEqual(fs.lstatSync(dirOf()).ino, ino);
    assert.deepStrictEqual(staleNames(), []);
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".cfg-")), []);
    fs.chmodSync(file, 0o644);
    prepareClaudeConfigDir(envOf());
    assertFixedDir(dirOf());
    assert.deepStrictEqual(staleNames(), []);
  });

  it("a session transcript under HOME/.claude/projects is visible through the directory (resume)", () => {
    const t = path.join(home, ".claude", "projects", "-slug", "sid.jsonl");
    fs.mkdirSync(path.dirname(t), { recursive: true });
    fs.writeFileSync(t, "TRANSCRIPT");
    prepareClaudeConfigDir(envOf());
    assert.strictEqual(fs.readFileSync(path.join(dirOf(), "projects", "-slug", "sid.jsonl"), "utf8"), "TRANSCRIPT");
  });

  it("does nothing without a HOME", () => {
    const before = fs.readdirSync(home);
    for (const env of [undefined, {}, { HOME: undefined }, { HOME: "" }]) prepareClaudeConfigDir(env);
    assert.deepStrictEqual(fs.readdirSync(home), before);
  });
});

describe("prepareClaudeConfigDir: refusals", () => {
  it("refuses a HOME that is not a canonical absolute path", () => {
    for (const h of ["relative/home", `${home}/../${path.basename(home)}`, `${home}/`]) {
      assert.throws(() => prepareClaudeConfigDir({ HOME: h, CLAUDE_CONFIG_DIR: claudeConfigDir(h) }), TrustedExecutionRefusal, h);
    }
  });

  it("refuses a CLAUDE_CONFIG_DIR that is not <HOME>/.uzi-claude-config, and builds nothing", () => {
    for (const dir of [undefined, "", path.join(home, ".claude"), outside, `${dirOf()}/`, `${home}/x/../.uzi-claude-config`]) {
      assert.throws(
        () => prepareClaudeConfigDir({ HOME: home, ...(dir === undefined ? {} : { CLAUDE_CONFIG_DIR: dir }) }),
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

  it("refuses a HOME that is a symlink", () => {
    const link = path.join(root, "homelink");
    fs.symlinkSync(home, link);
    assert.throws(() => prepareClaudeConfigDir(envOf(link)), TrustedExecutionRefusal);
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
      assert.throws(() => prepareClaudeConfigDir(envOf()), TrustedExecutionRefusal);
    } finally {
      (fs as { renameSync: typeof fs.renameSync }).renameSync = real;
    }
    assert.ok(fs.existsSync(path.join(dirOf(), "planted")), "nothing deleted");
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.includes(".tmp-")), [], "the temp build is cleaned up");
  });
});

describe("prepareClaudeConfigDir: a planted directory is moved aside and rebuilt, nothing followed", () => {
  const planted: Array<[string, () => void]> = [
    ["a directory with an extra file", () => {
      fs.mkdirSync(dirOf());
      fs.writeFileSync(path.join(dirOf(), ".config.json"), MALICIOUS);
    }],
    ["a good-looking directory plus remote-settings.json", () => {
      prepareClaudeConfigDir(envOf());
      fs.writeFileSync(path.join(dirOf(), "remote-settings.json"), MALICIOUS);
    }],
    ["a regular file", () => fs.writeFileSync(dirOf(), MALICIOUS)],
    ["a symlink to an outside directory", () => fs.symlinkSync(outside, dirOf())],
    ["a dangling symlink", () => fs.symlinkSync(path.join(outside, "nope"), dirOf())],
    ["a directory with a group-writable mode", () => {
      prepareClaudeConfigDir(envOf());
      fs.chmodSync(dirOf(), 0o775);
    }],
    ["a directory whose projects link points elsewhere", () => {
      prepareClaudeConfigDir(envOf());
      fs.rmSync(path.join(dirOf(), "projects"));
      fs.symlinkSync(outside, path.join(dirOf(), "projects"));
    }],
    ["a directory whose projects link is a real directory", () => {
      prepareClaudeConfigDir(envOf());
      fs.rmSync(path.join(dirOf(), "projects"));
      fs.mkdirSync(path.join(dirOf(), "projects"));
    }],
  ];
  for (const [label, plant] of planted) {
    it(label, () => {
      plant();
      prepareClaudeConfigDir(envOf());
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
    prepareClaudeConfigDir(envOf());
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
    prepareClaudeConfigDir(envOf());
    assert.ok(fs.lstatSync(path.join(home, ".claude", "projects")).isDirectory());
    assert.deepStrictEqual(fs.readdirSync(outside), ["sentinel"]);
  });

  it("a regular file at .claude is moved aside and a real directory takes its place", () => {
    fs.writeFileSync(path.join(home, ".claude"), "FILE");
    prepareClaudeConfigDir(envOf());
    assert.ok(fs.statSync(path.join(home, ".claude", "projects")).isDirectory());
    assert.strictEqual(fs.readFileSync(path.join(home, fs.readdirSync(home).find((f) => f.startsWith(".claude.stale-"))!), "utf8"), "FILE");
  });

  it("an existing .claude with its own projects is kept as is (resume data stays in place)", () => {
    const t = path.join(home, ".claude", "projects", "-s", "x.jsonl");
    fs.mkdirSync(path.dirname(t), { recursive: true });
    fs.writeFileSync(t, "T");
    fs.writeFileSync(path.join(home, ".claude", "history.jsonl"), "H");
    prepareClaudeConfigDir(envOf());
    assert.strictEqual(fs.readFileSync(t, "utf8"), "T");
    assert.ok(fs.statSync(path.join(home, ".claude", "tasks")).isDirectory(), "the missing tasks dir was added");
    assert.deepStrictEqual(fs.readdirSync(home).filter((f) => f.startsWith(".claude.stale-")), []);
  });
});

describe("prepareClaudeConfigDir: the uid-split checks", () => {
  beforeEach(() => {
    process.env.UZI_UID_SPLIT = "1";
  });

  it("makes HOME sticky and builds the directory (the test's own uid stands in for the worker)", () => {
    fs.chmodSync(home, 0o775);
    prepareClaudeConfigDir(envOf());
    assert.ok((fs.statSync(home).mode & 0o1000) !== 0, "HOME is sticky");
    assert.strictEqual(fs.statSync(home).mode & 0o777, 0o775, "the other mode bits are kept");
    assertFixedDir(dirOf());
  });

  it("a planted symlinked .claude and config directory are moved aside, never followed", () => {
    fs.writeFileSync(path.join(outside, ".config.json"), "SENTINEL-CONFIG");
    fs.symlinkSync(outside, path.join(home, ".claude"));
    fs.symlinkSync(outside, dirOf());
    prepareClaudeConfigDir(envOf());
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
    assert.throws(() => prepareClaudeConfigDir(envOf(h)), TrustedExecutionRefusal);
    assert.deepStrictEqual(fs.readdirSync(h), []);
    fs.chmodSync(parent, 0o2775);
    prepareClaudeConfigDir(envOf(h));
    assertFixedDir(claudeConfigDir(h));
    fs.rmSync(claudeConfigDir(h), { recursive: true });
    fs.chmodSync(parent, 0o777);
    assert.throws(() => prepareClaudeConfigDir(envOf(h)), TrustedExecutionRefusal);
    fs.chmodSync(parent, 0o1777);
    prepareClaudeConfigDir(envOf(h));
    assertFixedDir(claudeConfigDir(h));
    // Single-uid: no such check.
    delete process.env.UZI_UID_SPLIT;
    fs.rmSync(claudeConfigDir(h), { recursive: true });
    fs.chmodSync(parent, 0o777);
    prepareClaudeConfigDir(envOf(h));
  });
});

describe("prepareClaudeConfigDir: a hostile process acting between any two steps", () => {
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
    ["a good directory", () => prepareClaudeConfigDir(envOf())],
  ];

  for (const [scenarioLabel, setup] of scenarios) {
    it(`${scenarioLabel}: at every step, the start either rebuilds before the spawn or refuses; the child never sees other bytes`, async () => {
      // Dry run: how many steps does this scenario pass through?
      setup();
      const names: string[] = [];
      prepareClaudeConfigDir(envOf(), { onStep: (n) => names.push(n) });
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
          let child: ReturnType<typeof spawnDetached>;
          try {
            child = spawnDetached(
              {
                command: process.execPath,
                args: ["-e", DUMP_SCRIPT, out],
                env: envOf(),
                cwd: root,
                signal: new AbortController().signal,
              } as SpawnOptions,
              deps,
            );
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
    prepareClaudeConfigDir(envOf(), {
      onStep: (name) => {
        if (name === "built" && !nested) {
          nested = true;
          prepareClaudeConfigDir(envOf());
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
    prepareClaudeConfigDir(envOf(), {
      onStep: (name) => {
        if (name === "built" && !nested) {
          nested = true;
          prepareClaudeConfigDir(envOf());
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
    prepareClaudeConfigDir(envOf());
    fs.writeFileSync(path.join(dirOf(), "extra"), "x");
    prepareClaudeConfigDir(envOf()); // leaves an aside copy
    fs.mkdirSync(path.join(home, ".claude", "projects", "-s"), { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "projects", "-s", "t.jsonl"), "T");
    assert.strictEqual(staleNames().length, 1);
    await rmHomeTree(home);
    assert.ok(!fs.existsSync(home));
  });

  it("with a runner pass that cannot write the worker-owned directory, the worker's final pass still removes the HOME", async () => {
    prepareClaudeConfigDir(envOf());
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
