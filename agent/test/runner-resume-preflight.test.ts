import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import {
  api,
  fakeGitlab,
  fx,
  gitlabClaim,
  installHarness,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// ── Resume preflight (issue #105) ────────────────────────────────────────────
//
// The run lane pins a per-run HOME (`agent-home/<runId>`) that lives on the CLAIMING
// worker's data volume. A requeued run whose affinity grace lapsed can be claimed by a
// DIFFERENT worker, where that HOME never existed — the claim still carries the session
// id, but the transcript it names does not. Because the SDK resolves a resume LOCALLY
// (verified against the shipped CLI: exit 1, "No conversation found with session ID",
// duration_api_ms 0), passing that id through did not start a fresh run — it killed the
// run on its first turn with `error_during_execution`.
describe("RunRunner — resume preflight (issue #105)", () => {
  const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  /** A factory with a per-run HOME under `homeRoot`, capturing the RunContext. */
  function capturingFactory(
    homeRoot: string,
    seen: RunContext[],
  ): ExecutorFactory {
    return (runId) => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen.push(ctx);
          return { branch: ctx.branch };
        },
      },
      homeDir: path.join(homeRoot, runId),
    });
  }

  /** Plant a transcript under this run's HOME. The preflight globs the HOME's project
   *  dirs (sdk-session.ts), so the exact dir name does not matter — a per-run HOME holds
   *  only this run's own, so any project dir stands in for it. */
  function plantTranscript(runHome: string, sessionId: string): void {
    const dir = path.join(
      runHome,
      ".claude",
      "projects",
      "-data-runner-repo-issue-x",
    );
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
  }

  it("drops the resume and says so when the transcript is not on this worker", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(70, { session_id: SID });
    try {
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(
        seen[0]?.sessionId,
        undefined,
        "the dead session id must not reach the executor",
      );
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      const notice = texts.find((t) =>
        /earlier session could not be found/.test(t),
      );
      assert.ok(
        notice,
        `expected an honest resume notice, got ${JSON.stringify(texts)}`,
      );
      // Both facts the user needs to make sense of the feed: context is gone, AND that
      // is why the agent may re-tread ground. A bare "session not found" is not enough.
      assert.match(notice, /WITHOUT its earlier context/);
      assert.match(notice, /work may be repeated/);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("keeps the resume, silently, when the transcript IS on this worker", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    const seen: RunContext[] = [];
    const claim = gitlabClaim(71, { session_id: SID });
    try {
      plantTranscript(path.join(homeRoot, claim.run_id), SID);
      await runnerWith(capturingFactory(homeRoot, seen), gitlab).execute(claim);
      assert.strictEqual(
        seen[0]?.sessionId,
        SID,
        "a resolvable session must still resume",
      );
      const texts = api
        .messages(claim.run_id)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
      assert.ok(
        !texts.some((t) => /earlier session could not be found/.test(t)),
        "must not cry wolf",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("does not preflight a run with no per-run HOME (the stub lane is untouched)", async () => {
    // The stub executor persists no SDK session, so main.ts gives it no per-run HOME.
    // That absence is the discriminator — no executor-kind check lives in the runner.
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const claim = gitlabClaim(72, { session_id: SID });
    await runnerWith(
      () => ({
        executor: {
          run: async (ctx: RunContext) => {
            seen.push(ctx);
            return { branch: ctx.branch };
          },
        },
      }),
      gitlab,
    ).execute(claim);
    assert.strictEqual(
      seen[0]?.sessionId,
      SID,
      "no HOME to check ⇒ the claim's id passes through unchanged",
    );
  });

  it("warns the amnesiac lead about prior commits on the branch — but ONLY when the resume was dropped", async () => {
    // The honest degradation must not become silently redone work: if the branch already
    // carries pushed work and the lead can no longer remember it, say so in the prompt.
    const { gitlab } = fakeGitlab();
    const env = {
      ...process.env,
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_SYSTEM: "/dev/null",
    };
    const gitFx = (args: string[]): void => {
      execFileSync("git", ["-C", fx.originPath, ...args], {
        env,
        stdio: "pipe",
      });
    };
    // A previous COMPLETED run's pushed work: two commits on the issue branch. (An
    // attempt requeued mid-flight leaves nothing — a run pushes once, at the end.)
    gitFx(["checkout", "-b", "agent/issue-73"]);
    fs.writeFileSync(path.join(fx.originPath, "a.txt"), "a\n");
    gitFx(["add", "."]);
    gitFx(["commit", "-m", "prior work 1"]);
    fs.writeFileSync(path.join(fx.originPath, "b.txt"), "b\n");
    gitFx(["add", "."]);
    gitFx(["commit", "-m", "prior work 2"]);
    gitFx(["checkout", "main"]);

    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-resume-"));
    try {
      const dropped: RunContext[] = [];
      const droppedClaim = gitlabClaim(73, { session_id: SID });
      await runnerWith(capturingFactory(homeRoot, dropped), gitlab).execute(
        droppedClaim,
      );
      assert.deepStrictEqual(
        dropped[0]?.priorWork,
        { commits: 2 },
        "counted against the default branch",
      );

      // Same branch, same prior commits — but a resolvable session, so the lead
      // remembers its own work and needs no warning.
      const kept: RunContext[] = [];
      const keptClaim = gitlabClaim(73, { session_id: SID });
      plantTranscript(path.join(homeRoot, keptClaim.run_id), SID);
      await runnerWith(capturingFactory(homeRoot, kept), gitlab).execute(
        keptClaim,
      );
      assert.strictEqual(
        kept[0]?.priorWork,
        undefined,
        "a live resume needs no prior-work warning",
      );

      // And a fresh run (no session at all) on a branch with no prior work gets none.
      const fresh: RunContext[] = [];
      await runnerWith(capturingFactory(homeRoot, fresh), gitlab).execute(
        gitlabClaim(74),
      );
      assert.strictEqual(fresh[0]?.priorWork, undefined);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
