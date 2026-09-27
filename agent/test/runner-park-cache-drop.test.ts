import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// PRD #1809 M1 (D2): a park that ends the run's process drops the rebuildable caches
// (`.cache/go-build`, `go/pkg/mod`, `.npm/_cacache`) from the preserved HOME and keeps
// everything else a resume needs. On the base code a parked HOME kept every cache, so the
// first test is the regression: it fails there on the three "cache dropped" assertions.

const SID = "aaaaaaaa-bbbb-cccc-dddd-000000001809";
const PROJECT = "-data-runner-clone";
const TRANSCRIPT_REL = path.join(".claude", "projects", PROJECT, `${SID}.jsonl`);
const TRANSCRIPT = '{"type":"user","message":"the session before the park"}\n';

const CACHES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
/** Resume state, `go/bin`, and files this code has never heard of: all must survive a drop. */
const KEPT = [
  TRANSCRIPT_REL,
  path.join(".claude", "projects", PROJECT, SID, "subagents", "agent-1.jsonl"),
  path.join(".claude", "todos", "t.json"),
  ".claude.json",
  path.join("go", "bin", "gopls"),
  path.join(".cache", "unknown-tool", "state"),
  "unknown.txt",
];

/** A HOME shaped like a Go + npm run's: the three caches (the module cache with the Go
 *  toolchain's read-only `0555` package dirs) beside the files a resume needs. */
function seedHome(home: string): void {
  const mod = path.join(home, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
  fs.mkdirSync(mod, { recursive: true });
  fs.writeFileSync(path.join(mod, "dec.go"), "package inf\n");
  fs.mkdirSync(path.join(home, "go", "pkg", "mod", "cache", "download"), { recursive: true });
  fs.mkdirSync(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
  fs.writeFileSync(path.join(home, ".cache", "go-build", "0a", "0a1b-d"), "x".repeat(4096));
  fs.mkdirSync(path.join(home, ".npm", "_cacache", "index-v5"), { recursive: true });
  fs.writeFileSync(path.join(home, ".npm", "_cacache", "index-v5", "entry"), "{}");
  for (const rel of KEPT) {
    fs.mkdirSync(path.dirname(path.join(home, rel)), { recursive: true });
    fs.writeFileSync(path.join(home, rel), rel === TRANSCRIPT_REL ? TRANSCRIPT : "keep\n");
  }
  // Last, so no later mkdir under them undoes the mode.
  fs.chmodSync(mod, 0o555);
  fs.chmodSync(path.dirname(mod), 0o555);
}

function assertCachesDropped(home: string): void {
  for (const rel of CACHES) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), false, `${rel} must be dropped on a process-ending park`);
  }
}

function assertCachesKept(home: string): void {
  for (const rel of CACHES) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), true, `${rel} must be kept`);
  }
}

function assertResumeStateKept(home: string): void {
  for (const rel of KEPT) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), true, `${rel} must survive`);
  }
  assert.strictEqual(fs.readFileSync(path.join(home, TRANSCRIPT_REL), "utf8"), TRANSCRIPT);
}

/** The test's own cleanup must not leak a `0555` tree. */
function forceRm(dir: string): void {
  const widen = (p: string): void => {
    let st;
    try {
      st = fs.lstatSync(p);
    } catch {
      return;
    }
    if (!st.isDirectory()) return;
    fs.chmodSync(p, 0o700);
    for (const e of fs.readdirSync(p)) widen(path.join(p, e));
  };
  widen(dir);
  fs.rmSync(dir, { recursive: true, force: true });
}

describe("RunRunner — cache drop on a process-ending park (PRD #1809 M1)", () => {
  it("a usage-limit park drops the caches, keeps the resume state, and the resume reads its transcript back", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-park-"));
    try {
      const iid = 1809;
      const runId = "18090000-0000-4000-8000-000000001809";
      let home = "";
      const parkFactory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (): Promise<ExecutorResult> => {
              seedHome(home);
              throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
            },
          },
        };
      };
      await runnerWith(parkFactory, gitlab).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true }));
      assert.ok(
        api.states.some((s) => s.runId === runId && s.body.status === "limit_wait"),
        "precondition: the run parked at limit_wait",
      );
      const modeBefore = fs.lstatSync(home).mode;
      assertCachesDropped(home);
      assertResumeStateKept(home);
      assert.strictEqual(fs.lstatSync(home).mode, modeBefore, "the HOME root's mode is untouched");

      // The resume continues the ACTUAL session: the runner's transcript preflight keeps the
      // session id only when `<sid>.jsonl` is still under the HOME, and the executor reads the
      // transcript bytes back through the same HOME.
      let seenSession: string | null | undefined;
      let readBack = "";
      const resumeFactory: ExecutorFactory = (id) => ({
        homeDir: path.join(homeRoot, id),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            seenSession = ctx.sessionId;
            readBack = fs.readFileSync(path.join(homeRoot, id, TRANSCRIPT_REL), "utf8");
            return { branch: ctx.branch };
          },
        },
      });
      await runnerWith(resumeFactory, gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true, session_id: SID, last_seq: 1000 }),
      );
      assert.strictEqual(seenSession, SID, "the resume keeps the parked session id (transcript found)");
      assert.strictEqual(readBack, TRANSCRIPT, "the resume reads the parked transcript back");
    } finally {
      forceRm(homeRoot);
    }
  });

  it("a worker-shutdown interrupt (preserveSession only) keeps the caches", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-shut-"));
    try {
      let home = "";
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              started();
              await new Promise<void>((_, reject) =>
                ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }),
              );
              return { branch: ctx.branch };
            },
          },
        };
      };
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(1810, { wait_on_limit: true });
      const done = runner.execute(claim);
      await startedP;
      runner.shutdown();
      await done;
      assert.strictEqual(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"),
        false,
        "precondition: a shutdown requeues, it does not fail",
      );
      assertCachesKept(home);
      assertResumeStateKept(home);
    } finally {
      forceRm(homeRoot);
    }
  });

  it("a terminal run still removes its whole HOME, caches included", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-term-"));
    try {
      let home = "";
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              return { branch: ctx.branch };
            },
          },
        };
      };
      await runnerWith(factory, gitlab).execute(gitlabClaim(1811));
      assert.ok(home !== "", "precondition: the executor ran");
      assert.strictEqual(fs.existsSync(home), false, "a terminal run's HOME is removed as before");
    } finally {
      forceRm(homeRoot);
    }
  });
});
