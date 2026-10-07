import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { makeFixture, type Fixture } from "./fixture-repo.js";

const SELF = fileURLToPath(import.meta.url);
const GUARD = path.resolve(path.dirname(SELF), "../../scripts/tmpdir-leak-guard.sh");
const AGENT_DIR = path.resolve(path.dirname(SELF), "..");

// The guard's end-to-end child (same-file pattern as checkpoint-publish-early-response.test.ts):
// create a fixture, clean it up, then recreate a directory under it, as the leak in #2020 did.
function child(): void {
  const fx = makeFixture({}, { testName: "recreator" });
  fx.cleanup();
  fs.mkdirSync(path.join(path.dirname(fx.dataDir), "data", "x"), { recursive: true });
}

function readLedger(file: string): Array<Record<string, unknown>> {
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l !== "")
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

const POISON_SCENARIOS = ["repository", "index", "objects", "runtime-config", "config-files", "combined"] as const;
type PoisonScenario = (typeof POISON_SCENARIOS)[number];

// This environment is independent of both makeFixture and inherited Git variables.
// Real Git creation and inspection must not accidentally follow the poison being tested.
function cleanGitEnv(): NodeJS.ProcessEnv {
  return {
    PATH: process.env.PATH,
    TMPDIR: process.env.TMPDIR,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_SYSTEM: "/dev/null",
    GIT_TERMINAL_PROMPT: "0",
  };
}

function repoBytes(dir: string): Record<string, string> {
  const result: Record<string, string> = {};
  function visit(current: string): void {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const target = path.join(current, entry.name);
      if (entry.isDirectory()) visit(target);
      else result[path.relative(dir, target)] = fs.readFileSync(target).toString("hex");
    }
  }
  visit(dir);
  return result;
}

function poisonedChild(scenario: PoisonScenario): void {
  const outer = process.env.TMPDIR!;
  const poison = path.join(outer, "poison");
  const configFile = path.join(outer, "hostile.config");
  const env = cleanGitEnv();
  const git = (repo: string, args: string[]): string =>
    execFileSync("git", ["-C", repo, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
  fs.mkdirSync(poison);
  let fx: Fixture | undefined;
  try {
    git(poison, ["init", "-b", "poison"]);
    for (const [key, value] of [
      ["user.name", "poison"],
      ["user.email", "poison@example.invalid"],
      ["commit.gpgsign", "false"],
      ["maintenance.auto", "false"],
      ["gc.auto", "0"],
      ["core.fsmonitor", "false"],
    ]) git(poison, ["config", key!, value!]);
    fs.writeFileSync(path.join(poison, "sentinel.txt"), "leave poison alone\n");
    git(poison, ["add", "."]);
    git(poison, ["commit", "-m", "poison"]);
    fs.writeFileSync(configFile, "[core]\n\tbare = true\n[fixture]\n\thostile = true\n");
    fs.writeFileSync(path.join(poison, ".git", "hooks", "pre-commit"), "#!/bin/sh\nexit 1\n", { mode: 0o755 });
    const beforeRepo = repoBytes(poison);
    const poisonHead = git(poison, ["rev-parse", "HEAD"]);
    const groups: Record<Exclude<PoisonScenario, "combined">, NodeJS.ProcessEnv> = {
      repository: { GIT_DIR: path.join(poison, ".git"), GIT_WORK_TREE: poison },
      index: { GIT_INDEX_FILE: path.join(poison, ".git", "index") },
      objects: { GIT_OBJECT_DIRECTORY: path.join(poison, ".git", "objects") },
      "runtime-config": {
        GIT_CONFIG_COUNT: "2",
        GIT_CONFIG_KEY_0: "core.hooksPath",
        GIT_CONFIG_VALUE_0: path.join(poison, ".git", "hooks"),
        GIT_CONFIG_KEY_1: "fixture.hostile",
        GIT_CONFIG_VALUE_1: "true",
      },
      "config-files": { GIT_CONFIG_GLOBAL: configFile, GIT_CONFIG_SYSTEM: configFile, GIT_CONFIG: configFile },
    };
    Object.assign(process.env, scenario === "combined" ? Object.assign({}, ...Object.values(groups)) : groups[scenario]);
    const beforeEnv = { ...process.env };
    try {
      fx = makeFixture({ "owned.txt": "fixture owns this\n" }, { testName: scenario });
    } finally {
      // Check the actual child environment even if construction throws.
      assert.deepEqual({ ...process.env }, beforeEnv);
    }
    assert.equal(path.dirname(path.dirname(fx.originPath)), outer, "TMPDIR determines fixture location");
    assert.equal(process.env.PATH, env.PATH, "Git remains discoverable through inherited PATH");
    // Git reports the physical path (macOS TMPDIR /var is a symlink to /private/var).
    assert.equal(git(fx.originPath, ["rev-parse", "--show-toplevel"]), fs.realpathSync(fx.originPath));
    assert.equal(git(fx.originPath, ["symbolic-ref", "--short", "HEAD"]), "main");
    assert.equal(git(fx.originPath, ["rev-list", "--count", "HEAD"]), "1");
    assert.equal(git(fx.originPath, ["log", "-1", "--format=%s"]), "init");
    assert.equal(git(fx.originPath, ["show", "HEAD:README.md"]), "# fixture");
    assert.equal(git(fx.originPath, ["show", "HEAD:owned.txt"]), "fixture owns this");
    assert.equal(git(fx.originPath, ["ls-files"]), "README.md\nowned.txt");
    assert.ok(fs.statSync(path.join(fx.originPath, ".git", "index")).isFile());
    assert.equal(git(fx.originPath, ["diff", "--cached", "--name-only", "HEAD"]), "");
    for (const [key, value] of [
      ["user.name", "fixture"],
      ["user.email", "fixture@uzi.local"],
      ["commit.gpgsign", "false"],
      ["maintenance.auto", "false"],
      ["gc.auto", "0"],
      ["core.fsmonitor", "false"],
    ]) assert.equal(git(fx.originPath, ["config", "--local", "--get", key!]), value);
    assert.doesNotMatch(git(fx.originPath, ["config", "--list"]), /fixture\.hostile/);
    assert.equal(git(poison, ["rev-parse", "HEAD"]), poisonHead);
    assert.deepEqual(repoBytes(poison), beforeRepo, "poison worktree, config, index and objects are unchanged");
    assert.equal(fs.readFileSync(configFile, "utf8"), "[core]\n\tbare = true\n[fixture]\n\thostile = true\n");
  } finally {
    try {
      fx?.cleanup();
    } finally {
      fs.rmSync(poison, { recursive: true, force: true });
      fs.rmSync(configFile, { force: true });
    }
  }
  assert.deepEqual(fs.readdirSync(outer), [], "child teardown leaves its private TMPDIR empty");
}

if (process.env.UZI_FIXTURE_REPO_CHILD === "1") {
  child();
} else if (process.env.UZI_FIXTURE_REPO_POISON_CHILD) {
  poisonedChild(process.env.UZI_FIXTURE_REPO_POISON_CHILD as PoisonScenario);
} else {
  describe("makeFixture inherited Git environment isolation", () => {
    for (const scenario of POISON_SCENARIOS) {
      it(`isolates ${scenario} without mutating the child environment or poison repo`, () => {
        // The parent owns the outer directory even when the baseline constructor
        // throws before returning a Fixture with a cleanup method.
        const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-poison-"));
        try {
          const childEnv: NodeJS.ProcessEnv = {
            ...process.env,
            TMPDIR: outer,
            TSX_DISABLE_CACHE: "1", // the loader must not leave its own cache in the private TMPDIR
            UZI_FIXTURE_REPO_POISON_CHILD: scenario,
          };
          for (const key of Object.keys(childEnv)) {
            if (key.startsWith("GIT_")) delete childEnv[key];
          }
          delete childEnv.UZI_FIXTURE_REPO_CHILD;
          delete childEnv.UZI_TMPDIR_GUARD_LEDGER;
          const r = spawnSync(process.execPath, ["--import", "tsx", SELF], {
            cwd: AGENT_DIR,
            env: childEnv,
            encoding: "utf8",
            timeout: 60_000,
          });
          assert.ifError(r.error);
          assert.equal(r.status, 0, r.stdout + r.stderr);
          assert.deepEqual(fs.readdirSync(outer), []);
        } finally {
          fs.rmSync(outer, { recursive: true, force: true });
        }
      });
    }
  });

  describe("makeFixture ledger (scripts/tmpdir-leak-guard.sh evidence)", () => {
    it("records a created line, then a removed line on cleanup", () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-ledger-"));
      const ledger = path.join(dir, "ledger");
      const saved = process.env.UZI_TMPDIR_GUARD_LEDGER;
      process.env.UZI_TMPDIR_GUARD_LEDGER = ledger;
      let fx: Fixture | undefined;
      try {
        fx = makeFixture({}, { testName: "x" });
        const base = path.dirname(fx.dataDir);
        let lines = readLedger(ledger);
        assert.equal(lines.length, 1);
        const created = lines[0] as Record<string, unknown>;
        assert.equal(created.event, "created");
        assert.equal(created.entry, path.basename(base));
        assert.equal(created.path, base);
        assert.equal(created.test, "x");
        assert.equal(created.pid, process.pid);
        assert.match(String(created.file), /fixture-repo\.test\.ts$/);
        assert.ok(Array.isArray(created.site));
        assert.ok((created.site as string[]).some((s) => s.includes("fixture-repo.test.ts:")));

        fx.cleanup();
        assert.equal(fs.existsSync(base), false);
        lines = readLedger(ledger);
        assert.equal(lines.length, 2);
        assert.equal((lines[1] as Record<string, unknown>).event, "removed");
        assert.equal((lines[1] as Record<string, unknown>).entry, path.basename(base));
      } finally {
        // A failed assertion above must not leak the fixture into the real guard's TMPDIR,
        // where its created line (in this private ledger) would be gone.
        if (saved === undefined) delete process.env.UZI_TMPDIR_GUARD_LEDGER;
        else process.env.UZI_TMPDIR_GUARD_LEDGER = saved;
        try {
          if (fx) fs.rmSync(path.dirname(fx.dataDir), { recursive: true, force: true });
        } finally {
          fs.rmSync(dir, { recursive: true, force: true });
        }
      }
    });

    it("names the creator of a directory recreated after cleanup, under the real guard", () => {
      const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-guard-"));
      try {
        const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: outer, UZI_FIXTURE_REPO_CHILD: "1" };
        delete childEnv.UZI_TMPDIR_GUARD_LEDGER; // the guard sets its own
        const r = spawnSync(GUARD, [process.execPath, "--import", "tsx", SELF], {
          cwd: AGENT_DIR,
          env: childEnv,
          encoding: "utf8",
          timeout: 60_000,
        });
        assert.equal(r.status, 1, r.stderr);
        assert.ok(r.stderr.includes("fixture-repo.test.ts"), r.stderr);
        assert.ok(r.stderr.includes("recreator"), r.stderr);
        assert.ok(r.stderr.includes("cleanup ran; the directory was recreated afterwards"), r.stderr);
        assert.ok(r.stderr.includes("data/x"), r.stderr);
      } finally {
        fs.rmSync(outer, { recursive: true, force: true });
      }
    });
  });
}
