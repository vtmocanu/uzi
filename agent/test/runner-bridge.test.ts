import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger, noProofReseed } from "./helpers.js";
import { type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { GitHubClient } from "../src/forge.js";
import {
  api,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  installHarness,
  runner,
  runnerWith,
} from "./runner-harness.js";
import { RunRunner } from "../src/runner.js";
import { ScratchPublicationError, type ScratchPublicationKind, type ScratchPublicationStep } from "../src/git.js";
import type { Logger } from "../src/log.js";
import { makeTextRedactor } from "../src/redact.js";
import { sanitizeForLog } from "../src/run-quiescence.js";

installHarness();

// PRD #1416 M3 — the ancestry bridge at the finalize/park/reseed publication boundaries, driven
// through the REAL runner over a REAL on-disk fixture origin. A history rewrite below the published
// floor P is modelled by amending the published tip in the runner clone (the M2 test's technique),
// so the clone HEAD H genuinely diverges from origin's P.

const ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Commit `files` onto the fixture origin's checked-out main. */
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

/** Publish `name` on the fixture origin with a commit of its own (from the current main), so its
 *  tip P is a real forge tip. Restores origin's HEAD to main. Returns P. */
function publishBranch(name: string): string {
  gitIn(fx.originPath, ["checkout", "-b", name]);
  fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
  gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `published commit on ${name}`]);
  const sha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  return sha;
}

/** True when `ancestor` is an ancestor of (or equal to) the origin ref `ref`. */
function originAncestor(ancestor: string, ref: string): boolean {
  try {
    execFileSync("git", ["-C", fx.originPath, "merge-base", "--is-ancestor", ancestor, ref], {
      env: ENV,
    });
    return true;
  } catch {
    return false;
  }
}

/** An executor that amends the published tip in the runner clone (rewriting history BELOW P), then
 *  optionally advances the fixture origin's main. Returns the rewritten tip H via `obs`. */
function rewritingExecutor(
  obs: { H?: string },
  mainFiles?: Record<string, string>,
): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "REWRITE.md"), `rewrite ${randomUUID()}\n`);
      gitIn(ctx.worktreePath, ["add", "."]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "--no-edit"]);
      obs.H = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      if (mainFiles) commitToOriginMain(mainFiles, "main advances");
      return { branch: ctx.branch };
    },
  };
}

function githubRunner(github: GitHubClient, executor: Executor): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    github,
  });
}

function taskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    run_id: (overrides.run_id as string | undefined) ?? randomUUID(),
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: published-branch work",
    issue_description: "Continue the work on the already-published branch.",
    branch,
    open_mr: false,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

function githubTaskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return taskClaim(branch, {
    repo: {
      id: "r1",
      url: "https://github.com/org/repo",
      clone_url: fx.originPath,
      forge_type: "github",
    },
    ...overrides,
  });
}

const statusesFor = (runId: string): string[] =>
  api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

const bridgeStatusLines = (runId: string): string[] =>
  api
    .messages(runId)
    .filter(
      (m) => m.kind === "status" && String(m.payload.text).includes("contains a history bridge"),
    )
    .map((m) => String(m.payload.text));

describe("RunRunner — scratch publication refusal", () => {
  for (const aligned of [false, true]) {
    it(`${aligned ? "align" : "normal"} finalize refuses a scratch-bearing H before bridge custody or remote push`, async () => {
      if (aligned) commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
      const branch = `feature/scratch-${aligned ? "align" : "plain"}`;
      const P = publishBranch(branch);
      const { gitlab } = fakeGitlab();
      const { github } = fakeGitHub();
      let moves = 0;
      const originalMove = git.updateTrackingRef.bind(git);
      git.updateTrackingRef = (async (...args: Parameters<typeof git.updateTrackingRef>) => {
        moves++;
        return originalMove(...args);
      }) as typeof git.updateTrackingRef;
      const executor: Executor = {
        run: async (ctx) => {
          fs.mkdirSync(path.join(ctx.worktreePath, ".uzi", "scratch"), { recursive: true });
          fs.writeFileSync(path.join(ctx.worktreePath, ".uzi", "scratch", "note"), "local only\n");
          gitIn(ctx.worktreePath, ["add", "-f", ".uzi/scratch/note"]);
          gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "scratch in history"]);
          if (aligned) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances");
          return { branch: ctx.branch };
        },
      };
      const claim = aligned ? githubTaskClaim(branch) : taskClaim(branch);
      try {
        await (aligned ? githubRunner(github, executor) : runner(executor, gitlab)).execute(claim);
      } finally {
        git.updateTrackingRef = originalMove;
      }
      assert.equal(gitIn(fx.originPath, ["rev-parse", branch]), P, "no remote push changed the branch");
      assert.equal(moves, 0, "bridge never moved custody");
      const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
      assert.notEqual(failed?.fail_origin, "history_rewritten");
      assert.notEqual(failed?.branch_moved, true);
    });
  }
});

describe("RunRunner — bridge candidate preflight", () => {
  it("refuses B without advancing tracking custody or attempting a push", async () => {
    const branch = "feature/scratch-bridge-candidate";
    const P = publishBranch(branch);
    const { gitlab } = fakeGitlab();
    const obs: { H?: string } = {};
    const originalPreflight = git.scratchPublicationPreflight.bind(git);
    const originalMove = git.updateTrackingRef.bind(git);
    const originalPush = git.pushBranch.bind(git);
    let moves = 0;
    let pushes = 0;
    git.scratchPublicationPreflight = (async (bare, name, candidate) => {
      if (candidate && obs.H && candidate !== obs.H) {
        throw new ScratchPublicationError("candidate history contains scratch");
      }
      return originalPreflight(bare, name, candidate);
    }) as typeof git.scratchPublicationPreflight;
    git.updateTrackingRef = (async (...args: Parameters<typeof git.updateTrackingRef>) => {
      moves++;
      return originalMove(...args);
    }) as typeof git.updateTrackingRef;
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushes++;
      return originalPush(...args);
    }) as typeof git.pushBranch;
    const claim = taskClaim(branch);
    try {
      await runner(rewritingExecutor(obs), gitlab).execute(claim);
    } finally {
      git.scratchPublicationPreflight = originalPreflight;
      git.updateTrackingRef = originalMove;
      git.pushBranch = originalPush;
    }
    assert.equal(gitIn(fx.originPath, ["rev-parse", branch]), P);
    assert.equal(moves, 0);
    assert.equal(pushes, 0);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
  });
});

interface LoggedLine { level: string; msg: string; fields?: Record<string, unknown> }

/** A Logger recording every line at every level across children. */
function recordingLogger(): { logger: Logger; lines: LoggedLine[] } {
  const lines: LoggedLine[] = [];
  const self: Logger = {
    debug: (msg, fields) => void lines.push({ level: "debug", msg, fields }),
    info: (msg, fields) => void lines.push({ level: "info", msg, fields }),
    warn: (msg, fields) => void lines.push({ level: "warn", msg, fields }),
    error: (msg, fields) => void lines.push({ level: "error", msg, fields }),
    addSecret() {},
    removeSecret() {},
    child: () => self,
  };
  return { logger: self, lines };
}

// The claim's forge PAT is the secret the run's redactor knows (see taskClaim).
const PAT = "fixture-forge-pat-000000";

interface RefusalCase {
  name: string;
  kind: ScratchPublicationKind;
  step: ScratchPublicationStep;
  reason: string;
  detail?: string;
  cause?: unknown;
  expectReason: string;
  expectCause?: string;
}

const REFUSAL_CASES: RefusalCase[] = [
  {
    name: "tip_unavailable", kind: "tip_unavailable", step: "resolve_tip", reason: "candidate commit is unavailable",
    detail: "ref does not resolve to a commit",
    expectReason: "scratch_publication_refused: candidate history cannot be published (tip_unavailable at resolve_tip: ref does not resolve to a commit)",
  },
  {
    name: "shallow_history", kind: "shallow_history", step: "shallow_check", reason: "cannot prove scratch-free candidate history",
    detail: "history is shallow",
    expectReason: "scratch_publication_refused: candidate history cannot be published (shallow_history at shallow_check: history is shallow)",
  },
  {
    name: "object_walk_failed", kind: "object_walk_failed", step: "object_walk", reason: "cannot prove scratch-free candidate history",
    detail: "exit 128; fatal: bad tree",
    cause: new Error("git rev-list failed"),
    expectReason: "scratch_publication_refused: candidate history cannot be published (object_walk_failed at object_walk: exit 128; fatal: bad tree)",
    expectCause: "git rev-list failed",
  },
  {
    name: "checkpoint_range", kind: "checkpoint_range", step: "checkpoint_floor", reason: "checkpoint floor is unavailable or not an ancestor of candidate",
    detail: "floor is unavailable or not an ancestor of candidate",
    expectReason: "scratch_publication_refused: candidate history cannot be published (checkpoint_range at checkpoint_floor: floor is unavailable or not an ancestor of candidate)",
  },
  {
    name: "missing_objects", kind: "missing_objects", step: "object_walk", reason: "cannot prove scratch-free candidate history",
    detail: "bad object abc123",
    expectReason: "scratch_publication_refused: candidate history cannot be published (missing_objects at object_walk: bad object abc123)",
  },
  {
    name: "exec_failed", kind: "exec_failed", step: "scratch_walk", reason: "cannot prove scratch-free candidate history",
    detail: "exit 128",
    cause: new Error("fatal: spawn git ENOENT"),
    expectReason: "scratch_publication_refused: candidate history cannot be published (exec_failed at scratch_walk: exit 128)",
    expectCause: "fatal: spawn git ENOENT",
  },
  {
    name: "scratch_present", kind: "scratch_present", step: "scratch_walk", reason: "candidate history contains scratch",
    detail: ".uzi/scratch appears in candidate history",
    expectReason: "scratch_publication_refused: candidate history cannot be published (scratch_present at scratch_walk: .uzi/scratch appears in candidate history)",
  },
  {
    name: "floor_unverified", kind: "floor_unverified", step: "floor_refresh", reason: "cannot verify fresh remote floor",
    detail: "remote said: forbidden",
    cause: new Error("outer failure", { cause: "inner transport reset" }),
    expectReason: "scratch_publication_refused: cannot verify fresh remote floor (floor_unverified at floor_refresh)",
    expectCause: "outer failure <- inner transport reset",
  },
];

describe("RunRunner — scratch publication refusal diagnostics (issue #2054)", () => {
  for (const c of REFUSAL_CASES) {
    it(`finalize ${c.name}: distinct failure_reason and a structured log line`, async () => {
      const branch = `feature/refusal-${c.name}`;
      publishBranch(branch);
      const { gitlab } = fakeGitlab();
      const obs: { H?: string } = {};
      const original = git.scratchPublicationPreflight.bind(git);
      git.scratchPublicationPreflight = (async (bare, name, candidate) => {
        if (candidate && obs.H && candidate !== obs.H) {
          throw new ScratchPublicationError(c.reason, c.cause, { kind: c.kind, step: c.step, detail: c.detail });
        }
        return original(bare, name, candidate);
      }) as typeof git.scratchPublicationPreflight;
      const { logger, lines } = recordingLogger();
      const claim = taskClaim(branch);
      try {
        await runnerWith(() => ({ executor: rewritingExecutor(obs) }), gitlab, undefined, logger).execute(claim);
      } finally {
        git.scratchPublicationPreflight = original;
      }
      const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.equal(failed?.failure_reason, c.expectReason);
      assert.equal(failed?.fail_origin, undefined);
      const line = lines.find((l) => l.msg === "scratch publication refused");
      assert.ok(line, JSON.stringify(lines.map((l) => l.msg)));
      assert.equal(line.fields?.site, "finalize");
      assert.equal(line.fields?.kind, c.kind);
      assert.equal(line.fields?.step, c.step);
      assert.equal(line.fields?.detail, c.detail);
      assert.equal(line.fields?.cause, c.expectCause);
    });
  }

  it("redacts a secret carried by the detail and cause in both failure_reason and the log", async () => {
    const branch = "feature/refusal-redact";
    publishBranch(branch);
    const { gitlab } = fakeGitlab();
    const obs: { H?: string } = {};
    const original = git.scratchPublicationPreflight.bind(git);
    git.scratchPublicationPreflight = (async (bare, name, candidate) => {
      if (candidate && obs.H && candidate !== obs.H) {
        throw new ScratchPublicationError("cannot prove scratch-free candidate history", new Error(`auth ${PAT} rejected`), {
          kind: "exec_failed", step: "object_walk", detail: `exit 1; token ${PAT}`,
        });
      }
      return original(bare, name, candidate);
    }) as typeof git.scratchPublicationPreflight;
    const { logger, lines } = recordingLogger();
    const claim = taskClaim(branch);
    try {
      await runnerWith(() => ({ executor: rewritingExecutor(obs) }), gitlab, undefined, logger).execute(claim);
    } finally {
      git.scratchPublicationPreflight = original;
    }
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    const line = lines.find((l) => l.msg === "scratch publication refused");
    assert.ok(line);
    assert.match(String(failed?.failure_reason), /^scratch_publication_refused: candidate history cannot be published \(exec_failed at object_walk: /);
    assert.ok(!String(failed?.failure_reason).includes(PAT));
    assert.equal(line.fields?.site, "finalize");
    assert.ok(!JSON.stringify(line.fields).includes(PAT));
    assert.ok(JSON.stringify(line.fields).includes("REDACTED"));
  });

  /** Drive a finalize refusal whose error carries the given detail fields (as git.ts builds them: the
   *  sanitized, capped `detail` beside the unsanitized `rawDetail`) and return the failure_reason and
   *  the logged fields. */
  async function finalizeRefusal(
    name: string, cause: unknown, detail: string, rawDetail: string,
  ): Promise<{ reason: string; fields: string }> {
    const branch = `feature/refusal-${name}`;
    publishBranch(branch);
    const { gitlab } = fakeGitlab();
    const obs: { H?: string } = {};
    const original = git.scratchPublicationPreflight.bind(git);
    git.scratchPublicationPreflight = (async (bare, n, candidate) => {
      if (candidate && obs.H && candidate !== obs.H) {
        throw new ScratchPublicationError("cannot prove scratch-free candidate history", cause, {
          kind: "exec_failed", step: "object_walk", detail, rawDetail,
        });
      }
      return original(bare, n, candidate);
    }) as typeof git.scratchPublicationPreflight;
    const { logger, lines } = recordingLogger();
    const claim = taskClaim(branch);
    try {
      await runnerWith(() => ({ executor: rewritingExecutor(obs) }), gitlab, undefined, logger).execute(claim);
    } finally {
      git.scratchPublicationPreflight = original;
    }
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    const line = lines.find((l) => l.msg === "scratch publication refused");
    assert.ok(line);
    return { reason: String(failed?.failure_reason), fields: JSON.stringify(line.fields) };
  }

  function assertNoPatPrefix(text: string): void {
    for (let k = 8; k <= PAT.length; k++) {
      assert.ok(!text.includes(PAT.slice(0, k)), `a ${k}-char PAT prefix survived: ${text}`);
    }
    assert.ok(text.includes("REDACTED"), text);
  }

  it("redacts a secret split by a control character before sanitizing (failure_reason and log)", async () => {
    const raw = `exit 1; token ${PAT.slice(0, 10)}\t${PAT.slice(10)}`;
    const detail = sanitizeForLog(raw, 197);
    assert.ok(!detail.includes(PAT), "the sanitized copy alone no longer matches the redactor");
    const { reason, fields } = await finalizeRefusal("redact-tab", undefined, detail, raw);
    assertNoPatPrefix(reason);
    assertNoPatPrefix(fields);
  });

  for (const [label, sep] of [["CR", "\r"], ["LF", "\n"], ["U+2028", "\u2028"], ["U+2029", "\u2029"]] as const) {
    it(`redacts a secret split by a ${label} in the multi-line rawDetail (failure_reason and log)`, async () => {
      const raw = `fatal: ${PAT.slice(0, 16)}${sep}${PAT.slice(16)}\nhint: later line`;
      const detail = "fatal: " + PAT.slice(0, 16);
      const { reason, fields } = await finalizeRefusal(`redact-${label}`, undefined, detail, raw);
      assertNoPatPrefix(reason);
      assertNoPatPrefix(fields);
    });
  }

  for (const [label, sep] of [["U+2028", "\u2028"], ["U+2029", "\u2029"]] as const) {
    it(`redacts a secret split by ${label} in a logged cause`, async () => {
      const cause = new Error(`fatal: ${PAT.slice(0, 16)}${sep}${PAT.slice(16)}`);
      const { fields } = await finalizeRefusal(`redact-cause-${label}`, cause, "exit 1", "exit 1");
      assertNoPatPrefix(fields);
    });
  }

  for (const [label, pad] of [["spaces", " "], ["blank lines", "\n"]] as const) {
    it(`redacts a secret pushed onto the raw bound by leading ${label} (failure_reason and log)`, async () => {
      const raw = `${pad.repeat(4080)}${PAT}`;
      const { reason, fields } = await finalizeRefusal(`redact-pad-${label.replace(" ", "-")}`, undefined, "exit 1", raw);
      assertNoPatPrefix(reason);
      assertNoPatPrefix(fields);
    });
  }

  function assertOmittedNoPatPrefix(text: string): void {
    for (let k = 8; k <= PAT.length; k++) {
      assert.ok(!text.includes(PAT.slice(0, k)), `a ${k}-char PAT prefix survived: ${text}`);
    }
    assert.match(text, /characters omitted: over the redaction bound/);
  }

  it("omits an oversized rawDetail whose secret is split by interior padding (failure_reason and log)", async () => {
    const raw = `fatal: ${PAT.slice(0, 16)}${"\n".repeat(5000)}${PAT.slice(16)}`;
    const { reason, fields } = await finalizeRefusal("redact-interior-pad", undefined, "exit 1", raw);
    assertOmittedNoPatPrefix(reason);
    assertOmittedNoPatPrefix(fields);
  });

  it("omits an oversized logged cause whose secret is split by interior padding", async () => {
    const cause = new Error(`fatal: ${PAT.slice(0, 16)}${"\n".repeat(5000)}${PAT.slice(16)}`);
    const { fields } = await finalizeRefusal("redact-cause-interior-pad", cause, "exit 1", "exit 1");
    assertOmittedNoPatPrefix(fields);
  });

  it("redacts a secret that straddles the sanitized detail cap (failure_reason and log)", async () => {
    const raw = `${"x".repeat(185)}${PAT}`;
    const detail = sanitizeForLog(raw, 197);
    assert.ok(detail.endsWith("..."));
    const { reason, fields } = await finalizeRefusal("redact-cap", undefined, detail, raw);
    assertNoPatPrefix(reason);
    assertNoPatPrefix(fields);
  });

  it("bounds the cause chain at depth 4", async () => {
    const chain = new Error("p1", {
      cause: new Error("p2", { cause: new Error("p3", { cause: new Error("p4", { cause: new Error("p5") }) }) }),
    });
    const { fields } = await finalizeRefusal("cause-depth", chain, "exit 1", "exit 1");
    assert.equal((JSON.parse(fields) as { cause: string }).cause, "p1 <- p2 <- p3 <- p4");
  });

  it("bounds the joined cause at 600 characters", async () => {
    const chain = new Error(`${"a".repeat(300)}1`, {
      cause: new Error(`${"b".repeat(300)}2`, {
        cause: new Error(`${"c".repeat(300)}3`, { cause: new Error(`${"d".repeat(300)}4`, { cause: new Error("fifth") }) }),
      }),
    });
    const { fields } = await finalizeRefusal("cause-bound", chain, "exit 1", "exit 1");
    const cause = String((JSON.parse(fields) as { cause: string }).cause);
    assert.ok(cause.length <= 600, String(cause.length));
    assert.ok(cause.endsWith("..."));
    assert.ok(!cause.includes("fifth"));
    assert.ok(cause.startsWith("a".repeat(200)));
  });

  it("checkpoint publish keeps the feed line byte-identical and logs the structured refusal", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/refusal-checkpoint";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    await git.runnerCloneForBranch(bare, branch, "feature-rc", noProofReseed, "R1");
    const { logger, lines } = recordingLogger();
    const feed: string[] = [];
    const flight = {
      runId: "R1",
      publishedTip: P,
      checkpointFloor: P,
      lastCheckpointRefTip: "CONFIRMED",
      lastAttemptedCheckpointRefTip: "ATTEMPTED",
      reportedPublishOutcomes: new Set<string>(),
      runLog: logger,
      redactText: makeTextRedactor([PAT]),
      batcher: { emit(message: { payload?: { text?: string } }) {
        if (message.payload?.text) feed.push(message.payload.text);
      } },
    };
    const original = git.checkpointPack.bind(git);
    git.checkpointPack = (async () => {
      throw new ScratchPublicationError("cannot verify fresh remote floor", new Error(`leak ${PAT}`), {
        kind: "floor_unverified", step: "floor_refresh", detail: "remote text",
      });
    }) as typeof git.checkpointPack;
    let outcome: unknown;
    try {
      outcome = await (runner({ run: async (c) => ({ branch: c.branch }) }, gitlab) as unknown as {
        publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
      }).publishCheckpointOutcome(flight, bare, branch);
    } finally {
      git.checkpointPack = original;
    }
    assert.deepEqual(outcome, { published: false, reason: "scratch_publication_refused" });
    assert.deepEqual(feed, ["checkpoint publish failed: scratch_publication_refused"]);
    const line = lines.find((l) => l.msg === "scratch publication refused");
    assert.ok(line);
    assert.equal(line.fields?.site, "checkpoint_publish");
    assert.equal(line.fields?.kind, "floor_unverified");
    assert.equal(line.fields?.step, "floor_refresh");
    assert.equal(line.fields?.detail, "remote text");
    assert.ok(!JSON.stringify(line.fields).includes(PAT));
  });

  it("checkpoint publish without a redactor logs kind and step but omits free text", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/refusal-noredactor";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    await git.runnerCloneForBranch(bare, branch, "feature-rc", noProofReseed, "R1");
    const { logger, lines } = recordingLogger();
    const feed: string[] = [];
    const flight = {
      runId: "R1",
      publishedTip: P,
      checkpointFloor: P,
      lastCheckpointRefTip: "CONFIRMED",
      lastAttemptedCheckpointRefTip: "ATTEMPTED",
      reportedPublishOutcomes: new Set<string>(),
      runLog: logger,
      batcher: { emit(message: { payload?: { text?: string } }) {
        if (message.payload?.text) feed.push(message.payload.text);
      } },
    };
    const original = git.checkpointPack.bind(git);
    git.checkpointPack = (async () => {
      throw new ScratchPublicationError("cannot verify fresh remote floor", new Error(`leak ${PAT}`), {
        kind: "exec_failed", step: "object_walk", detail: "remote text",
      });
    }) as typeof git.checkpointPack;
    let outcome: unknown;
    try {
      outcome = await (runner({ run: async (c) => ({ branch: c.branch }) }, gitlab) as unknown as {
        publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
      }).publishCheckpointOutcome(flight, bare, branch);
    } finally {
      git.checkpointPack = original;
    }
    assert.deepEqual(outcome, { published: false, reason: "scratch_publication_refused" });
    const line = lines.find((l) => l.msg === "scratch publication refused");
    assert.ok(line);
    assert.equal(line.fields?.site, "checkpoint_publish");
    assert.equal(line.fields?.kind, "exec_failed");
    assert.equal(line.fields?.step, "object_walk");
    assert.equal(line.fields?.detail, undefined);
    assert.equal(line.fields?.cause, undefined);
  });
});

describe("RunRunner — finalize ancestry bridge (PRD #1416 M3)", () => {
  it("(plain path) a rewritten branch with no workflow drift is bridged, B is pushed, the run completes", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/rewritten-plain";
    const P = publishBranch(branch);
    const obs: { H?: string } = {};
    const claim = taskClaim(branch, { open_mr: false });
    await runner(rewritingExecutor(obs), gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed (not the generic catch)");
    // The pushed branch fast-forwards from P: P is an ancestor of what landed on origin.
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "the rewritten H is also an ancestor of the pushed tip");
    // The pushed tip is NOT P or H themselves — it is the synthesised bridge B.
    const pushed = gitIn(fx.originPath, ["rev-parse", branch]);
    assert.notStrictEqual(pushed, P);
    assert.notStrictEqual(pushed, obs.H);
  });

  it("(overlay arm) rewrite + workflow drift → the overlaid+bridged tip has BOTH P and H as ancestors", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-overlay";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    const obs: { H?: string } = {};
    // The agent rewrites below P AND main advances a workflow file → behind-on-workflows + divergent.
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "H is an ancestor of the pushed tip (overlay preserved it)");
    // The aligned workflow tree matches the current default (v2).
    assert.strictEqual(
      gitIn(fx.originPath, ["show", `${branch}:.github/workflows/ci.yml`]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file is aligned to the current default",
    );
  });

  it("(published workflows) null diff bypasses alignment and the bridged tip has BOTH P and H as ancestors", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-merge";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    // A null diff cannot prove alignment safe; preserve the permitted published workflow.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const obs: { H?: string } = {};
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "H is an ancestor of the pushed tip (bridge preserved it)");
    assert.strictEqual(gitIn(fx.originPath, ["show", `${branch}:.github/workflows/ci.yml`]).trim(), CI_V1.trim(),
      "the permitted published workflow is preserved when the diff is unavailable");
  });

  it("(rebase fallback) merge push rejected for workflow scope → rebase → bridged + pushed with P an ancestor", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-rebase";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    // Unknown precheck, then known nonempty fresh eligibility reaches alignment; null diff forbids overlay.
    // Keep real workflow classifications before both strategy pushes.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const realWorkflowFiles = git.branchWorkflowFiles.bind(git);
    let workflowChecks = 0;
    git.branchWorkflowFiles = (async (...args: Parameters<typeof git.branchWorkflowFiles>) => {
      workflowChecks++;
      if (workflowChecks === 1) return null;
      if (workflowChecks === 2) return [".github/workflows/ci.yml"];
      return realWorkflowFiles(...args);
    }) as typeof git.branchWorkflowFiles;
    const strategies: string[] = [];
    const realAlign = git.alignBranchWithDefault.bind(git);
    git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      strategies.push(args[4]);
      return realAlign(...args);
    }) as typeof git.alignBranchWithDefault;
    // Reject the FIRST push as GitHub's workflow-scope rejection, then let it through — so the merge
    // push is rejected and the rebase fallback (which rewrites P, fact 14) runs and is bridged.
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "remote rejected refs/uzi-runner -> ci.yml without workflow scope",
        );
      }
      return realPush(...args);
    }) as typeof git.pushBranch;
    const obs: { H?: string } = {};
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed via the rebase fallback");
    assert.strictEqual(pushCalls, 2, "the first push was rejected and a second push landed");
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "the merge rejection reached the real rebase fallback");
    assert.strictEqual(workflowChecks, 4, "unknown precheck, nonempty fresh eligibility, then real checks before both pushes");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed (rebased+bridged) tip");
    assert.strictEqual(gitIn(fx.originPath, ["show", `${branch}:.github/workflows/ci.yml`]).trim(), CI_V2.trim());
  });

  it("(no rewrite) a clean fast-forward branch is NOT bridged and completes as today", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/clean";
    const P = publishBranch(branch);
    // The agent adds a commit ON TOP of P (no rewrite) → still fast-forwards, no bridge needed.
    const cleanExec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "ontop.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "clean work on top"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { open_mr: false });
    await runner(cleanExec, gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"));
    assert.ok(originAncestor(P, branch), "P is still an ancestor (a plain fast-forward)");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), [], "no history-bridge status on a clean branch");
  });
});

describe("RunRunner — the reseed after a bridge adopts B (PRD #1416 M3, SC2)", () => {
  it("a divergent tracking tip advanced to B is adopted by the reseed (seededFrom='tracking') where H alone is set aside", async () => {
    const branch = "feature/reseed";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    // A run owns the branch: seed from origin (P), then the agent rewrites below P → H.
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-reseed", noProofReseed, "R1");
    assert.strictEqual(rc.seededFrom, "origin");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = H (divergent), owner R1

    // CONTROL — today's behaviour: with the tracking ref left at the divergent H, a reseed sets it
    // aside and seeds from origin, losing the rewritten work.
    const before = await git.runnerCloneForBranch(bare, branch, "feature-reseed", noProofReseed, "R1");
    assert.strictEqual(before.seededFrom, "origin", "an un-bridged divergent tip is set aside (origin wins)");

    // BRIDGE the tracking ref to B (what bridgeBareTrackingRefIfDivergent does at the park sink).
    const bridgeResult = await git.bridgeToFloors(bare, H, [P]);
    assert.strictEqual(bridgeResult.kind, "built", "a bridge was built");
    const B = (bridgeResult as { kind: "built"; sha: string }).sha;
    await git.updateTrackingRef(bare, branch, B);

    // Now the reseed adopts B — the run resumes on its rewritten work.
    const after = await git.runnerCloneForBranch(bare, branch, "feature-reseed", noProofReseed, "R1");
    assert.strictEqual(after.seededFrom, "tracking", "the bridged tip descends from P → adopted");
    const head = gitIn(after.path, ["rev-parse", "HEAD"]);
    assert.strictEqual(head, B, "the clone HEAD is the bridge B");
  });
});

describe("RunRunner — the MR bridge-note is history-derived, not flight-local (PRD #1416 M3, Part D)", () => {
  it("a reclaim whose finalize builds NO new bridge still reports the earlier bridge in the pushed history", async () => {
    const { gitlab, calls } = fakeGitlab();
    const branch = "feature/reclaim-note";
    const P = publishBranch(branch);
    // Model a RECLAIM: the branch's committed history already contains a bridge from a prior run (the
    // agent's own `git merge -s ours P` from the M2 steer). This finalize builds NO new bridge — the
    // clone HEAD already descends from P — so a flight-local flag would be false; only the
    // history-derived rangeContainsBridge over P..pushedTip finds it.
    const reclaimExec: Executor = {
      run: async (ctx) => {
        // clone HEAD = P (seeded from origin). Diverge below P, then restore P as an ancestor.
        gitIn(ctx.worktreePath, ["reset", "--hard", "HEAD~1"]); // to the root, below P
        fs.writeFileSync(path.join(ctx.worktreePath, "impl.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rewritten work"]);
        gitIn(ctx.worktreePath, [...IDENT, "merge", "-s", "ours", P, "-m", "restore published tip"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { open_mr: true });
    await runner(reclaimExec, gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    // The finalize STATUS line names the history bridge (derived from the pushed history).
    assert.ok(bridgeStatusLines(claim.run_id).length === 1, "exactly one history-bridge status line");
    // The MR body carries the generic bridge sentence.
    const post = calls.find((c) => c.method === "POST" && c.body?.includes("history bridge"));
    assert.ok(post, "the opened MR body contains the generic history-bridge sentence");
    assert.match(post!.body!, /git log --first-parent/, "the sentence is the generic one");
    assert.doesNotMatch(post!.body!, /the worker bridged/, "the sentence never says the worker bridged it");
  });
});

describe("RunRunner — a concurrent remote advance is not a rewrite (PRD #1416 M3)", () => {
  it("an aligned GitHub mr_rework whose remote branch advances reports branch_moved", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "agent/issue-779";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    let concurrentTip = "";
    let alignCalls = 0;
    const originalAlign = git.alignBranchWithDefault.bind(git);
    git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      alignCalls++;
      return originalAlign(...args);
    }) as typeof git.alignBranchWithDefault;
    const executor: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "rework.ts"]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances workflows");
        gitIn(fx.originPath, ["checkout", branch]);
        fs.writeFileSync(path.join(fx.originPath, "concurrent.md"), "someone else\n");
        gitIn(fx.originPath, ["add", "concurrent.md"]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent writer"]);
        concurrentTip = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const remoteTip = () => gitIn(fx.originPath, ["rev-parse", branch]);
    const claim = githubTaskClaim(branch, { kind: "mr_rework", open_mr: true });
    try {
      await githubRunner(github, executor).execute(claim);
    } finally {
      git.alignBranchWithDefault = originalAlign;
    }
    assert.ok(alignCalls > 0, "the GitHub workflow alignment ran before publication");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.equal(failed?.branch_moved, true);
    assert.equal(failed?.fail_origin, undefined);
    assert.notEqual(concurrentTip, P);
    assert.equal(remoteTip(), concurrentTip, "the concurrent writer's commit remains the remote tip");
    assert.equal(statusesFor(claim.run_id).includes("completed"), false);
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), []);
  });

  it("an mr_rework whose branch was advanced by a concurrent writer takes branch_moved, no bridge", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "agent/issue-777";
    publishBranch(branch);
    // The agent adds work on top of P (a plain fast-forward — NOT a rewrite), so P stays an ancestor.
    const exec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        // A concurrent writer advances the remote branch AFTER the clone → non-fast-forward push.
        gitIn(fx.originPath, ["checkout", branch]);
        fs.writeFileSync(path.join(fx.originPath, "concurrent.md"), "someone else\n");
        gitIn(fx.originPath, ["add", "."]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent writer"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, {
      kind: "mr_rework",
      open_mr: true,
    });
    await runner(exec, gitlab).execute(claim).catch(() => undefined);

    const branchMoved = api.states.some(
      (s) => s.runId === claim.run_id && s.body.branch_moved === true,
    );
    assert.ok(branchMoved, "the concurrent advance is reported branch_moved (never history_rewritten)");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), [], "no bridge on a concurrent advance");
  });

  it("refuses a concurrent remote advance whose new history contains scratch", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "agent/issue-778";
    publishBranch(branch);
    const exec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "rework.ts"]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        gitIn(fx.originPath, ["checkout", branch]);
        fs.mkdirSync(path.join(fx.originPath, ".uzi", "scratch"), { recursive: true });
        fs.writeFileSync(path.join(fx.originPath, ".uzi", "scratch", "note"), "scratch\n");
        gitIn(fx.originPath, ["add", "-f", ".uzi/scratch/note"]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent scratch writer"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { kind: "mr_rework", open_mr: true });
    await runner(exec, gitlab).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
    assert.notEqual(failed?.branch_moved, true);
    assert.equal(gitIn(fx.originPath, ["show", `${branch}:.uzi/scratch/note`]), "scratch");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), []);
  });
});

describe("RunRunner.bridgeBareTrackingRefIfDivergent (PRD #1416 M3 — the shared boundary helper)", () => {
  const callBridge = (r: RunRunner, bare: string, branch: string, flight: unknown) =>
    (r as unknown as {
      bridgeBareTrackingRefIfDivergent: (
        b: string,
        br: string,
        f: unknown,
        l: unknown,
      ) => Promise<{ kind: string; bridge?: string }>;
    }).bridgeBareTrackingRefIfDivergent(bare, branch, flight, nullLogger());

  it("bridges a divergent tracking tip, advances the ref to B, and advances C to B", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-divergent";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-h", noProofReseed, "R1");
    gitIn(rc.path, ["add", "."]);
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");

    const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    const outcome = await callBridge(r, bare, branch, flight);
    assert.strictEqual(outcome.kind, "bridged");
    const B = outcome.bridge!;
    assert.strictEqual(await git.trackingTip(bare, branch), B, "the tracking ref now points at B");
    assert.strictEqual(flight.checkpointFloor, B, "C advanced to B");
    assert.strictEqual(await git.ancestry(bare, P, B), "ancestor", "P is an ancestor of B");
    assert.strictEqual(await git.ancestry(bare, H, B), "ancestor", "H is an ancestor of B");
  });

  it("is a no-op (clean) when P is already an ancestor of the tracking tip", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-clean";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hc", noProofReseed, "R1");
    fs.writeFileSync(path.join(rc.path, "ontop.ts"), "1\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "clean work on top of P"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const tipBefore = await git.trackingTip(bare, branch);

    const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    const outcome = await callBridge(r, bare, branch, flight);
    assert.strictEqual(outcome.kind, "clean");
    assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "the ref was not advanced");
  });

  it("leaves scratch-bearing clean checkpoint custody to the best-effort publish outcome", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-clean-scratch";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hcs", noProofReseed, "R1");
    fs.mkdirSync(path.join(rc.path, ".uzi", "scratch"), { recursive: true });
    fs.writeFileSync(path.join(rc.path, ".uzi", "scratch", "note"), "local only\n");
    gitIn(rc.path, ["add", "-f", ".uzi/scratch/note"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "scratch checkpoint"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const H = await git.trackingTip(bare, branch);
    const lines: string[] = [];
    const flight = {
      runId: "R1",
      publishedTip: P,
      checkpointFloor: P,
      lastCheckpointRefTip: "CONFIRMED",
      lastAttemptedCheckpointRefTip: "ATTEMPTED",
      reportedPublishOutcomes: new Set<string>(),
      runLog: nullLogger(),
      batcher: { emit(message: { payload?: { text?: string } }) {
        if (message.payload?.text) lines.push(message.payload.text);
      } },
    };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    assert.deepEqual(await callBridge(r, bare, branch, flight), { kind: "clean" });
    const outcome = await (r as unknown as {
      publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
    }).publishCheckpointOutcome(flight, bare, branch);
    assert.deepEqual(outcome, { published: false, reason: "scratch_publication_refused" });
    assert.equal(await git.trackingTip(bare, branch), H);
    assert.equal(flight.checkpointFloor, P);
    assert.equal(flight.lastCheckpointRefTip, "CONFIRMED");
    assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPTED");
    assert.deepEqual(lines, ["checkpoint publish failed: scratch_publication_refused"]);
  });

  it("returns 'unknown' (never bridges) when ancestry cannot be determined", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-unknown";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hu", noProofReseed, "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const tipBefore = await git.trackingTip(bare, branch);

    const origAncestry = git.ancestry.bind(git);
    (git as unknown as { ancestry: unknown }).ancestry = async () => "unknown";
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "unknown");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a broken read never bridges");
    } finally {
      (git as unknown as { ancestry: unknown }).ancestry = origAncestry;
    }
  });

  it("returns 'failed' when the built bridge is DEFINITIVELY malformed (tree resolves but differs from H's) — FIX 3", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-validate-fail";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hvf", noProofReseed, "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = divergent H
    const tipBefore = await git.trackingTip(bare, branch);

    // A malformed "bridge": parents H and P (so BOTH are definitively ancestors) but P's tree, NOT
    // H's → the tree-equality check must fail DEFINITIVELY (both trees resolve and differ). Stub
    // bridgeToFloors to return it so only the tree-mismatch decides the verdict.
    const pTree = gitIn(bare, ["rev-parse", `${P}^{tree}`]);
    const malformed = gitIn(bare, [...IDENT, "commit-tree", pTree, "-p", H, "-p", P, "-m", "wrong-tree bridge"]);
    const origBridge = git.bridgeToFloors.bind(git);
    (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = async () => ({
      kind: "built",
      sha: malformed,
    });
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "failed", "a tree-mismatched bridge is a definitive failure");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a failed validation never advances the ref");
    } finally {
      (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = origBridge;
    }
  });

  it("returns 'unknown' (never 'failed') when a validation READ is transient (revParse null) — FIX 3", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-validate-unknown";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hvu", noProofReseed, "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = divergent H
    const tipBefore = await git.trackingTip(bare, branch);

    // The divergence detection + the REAL bridge build both run; only the post-build validation's
    // tree read is stubbed transient (revParse → null). That must map to 'unknown', NOT 'failed' —
    // a finalize sink turns 'failed' into a thrown HistoryRewrittenError that would fail the run.
    const origRevParse = git.revParse.bind(git);
    (git as unknown as { revParse: unknown }).revParse = async () => null;
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "unknown", "a transient validation read maps to unknown, not failed");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a transient read never advances the ref");
    } finally {
      (git as unknown as { revParse: unknown }).revParse = origRevParse;
    }
  });
});
