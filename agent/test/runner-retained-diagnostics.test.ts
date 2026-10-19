import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { formatRetainedRecoveryDiagnostic, RetainedReadinessCleanupUnconfirmedError } from "../src/git.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
const warning = "retained recovery iteration failed; reserved budget retained";
const secret = "glpat-" + "abcdefghijklmnopqrst";
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed() {
  const claim = gitlabClaim(2512, { claim_generation: 2, session_id: "predecessor-session" });
  claim.secrets.forge_pat = secret;
  const bare = await git.ensureClone(fx.originPath);
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "local work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}

for (const mode of ["third", "deadline", "generic", "cleanup"] as const) {
  it("actual retained entry logs sanitized cause before terminal exit: " + mode, async t => {
    const { claim, bare, clone } = await seed();
    const log = recordingLogger();
    let faults = 0, models = 0;
    const terminalLogCounts: number[] = [];
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      if (args[1].status === "failed") {
        terminalLogCounts.push(log.lines.filter((line: any) => line.msg === warning).length);
      }
      return report(...args);
    };
    if (mode === "generic" || mode === "cleanup") {
      git.ensureClone = async () => {
        faults++;
        if (mode === "cleanup") throw new RetainedReadinessCleanupUnconfirmedError();
        throw new Error("connection reset " + secret);
      };
    } else {
      const cache = git as any;
      const exec = cache.execScoped.bind(git);
      cache.execScoped = async (file: string, args: string[], ...rest: unknown[]) => {
        if (file === "git" && args.includes("fetch") && args.some(arg => arg === "file://" + clone.path)) {
          faults++;
          throw Object.assign(new Error("transport fixture reached"), {
            stderr: "fatal: cannot open /" + "p".repeat(4000) + " " +
              secret.slice(0, 10) + "\u2028" + secret.slice(10) + ": Permission denied",
          });
        }
        return exec(file, args, ...rest);
      };
    }
    let expired = false;
    if (mode === "deadline") {
      const operation = git.withRecoveryOperation.bind(git);
      // Advance only after the actual transport rejection has cleared scoped exec
      // and lock cleanup. The unchanged runner then chooses its deadline branch.
      git.withRecoveryOperation = async (signal, deadline, action, budget) => {
        try { return await operation(signal, deadline, action, budget); }
        catch (error) {
          if (!expired && formatRetainedRecoveryDiagnostic(error, s => s).diagnostic_code === "git_failed") {
            expired = true;
            t.mock.method(Date, "now", () => deadline);
          }
          throw error;
        }
      };
    }
    await runnerWith(() => ({ executor: {
      sandboxesCommands: false, // Explicit host legacy profile: no real UID helper.
      run: async () => { models++; throw new Error("must not enter model"); },
    } }), fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
    const expected = mode === "deadline" || mode === "cleanup" ? 1 : 3;
    assert.equal(faults, expected, "the intended fault reached retained recovery");
    assert.equal(models, 0);
    assert.equal(expired, mode === "deadline");
    const diagnostics = log.lines.filter((line: any) => line.msg === warning) as Array<Record<string, any>>;
    assert.equal(diagnostics.length, expected);
    assert.ok(terminalLogCounts.length > 0);
    assert.ok(terminalLogCounts.every(count => count === expected), "all causes logged before terminal report");
    assert.deepEqual(diagnostics.map(line => line.iteration), Array.from({ length: expected }, (_, i) => i + 1));
    for (const diagnostic of diagnostics) {
      assert.equal(diagnostic.error, undefined, "no duplicate raw error field");
      assert.ok(!JSON.stringify(diagnostic).includes(secret));
      assert.ok(!JSON.stringify(diagnostic).includes(secret.slice(0, 10)));
      assert.ok(!JSON.stringify(diagnostic).includes("ijklmnopqrst"));
      if (mode === "cleanup") {
        assert.equal(diagnostic.diagnostic_code, "retained_readiness_cleanup");
        assert.equal(diagnostic.cause, "");
      } else if (mode === "generic") {
        assert.match(diagnostic.cause, /connection reset.*REDACTED/);
      } else {
        assert.equal(diagnostic.diagnostic_code, "git_failed");
        assert.match(diagnostic.cause, /^fatal: cannot open/);
        assert.match(diagnostic.cause, /REDACTED.*Permission denied$/);
        assert.ok(diagnostic.cause.length <= 1024);
        assert.ok(diagnostic.context.length <= 256);
      }
    }
    const journal = JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(journal.recovery.attempts, expected);
    assert.equal(journal.recovery.blocker, mode === "cleanup" ? "readiness_cleanup_unconfirmed" : "budget_exhausted");
    const failed = api.states.filter(state => state.body.status === "failed");
    assert.ok(failed.length > 0);
    assert.doesNotMatch(JSON.stringify(failed), /Permission denied|connection reset|ijklmnopqrst/);
    assert.equal(api.states.some(state => state.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "local work\n");
  });
}

it("an asynchronous readiness refusal stops retained recovery before successor admission", async () => {
  const { claim, bare, clone } = await seed();
  const log = recordingLogger();
  let refusals = 0, models = 0;
  // The refusal settles after a microtask, so only an awaited readiness pass can propagate it.
  git.normalizeRetainedSourceObjects = async () => {
    await Promise.resolve();
    refusals++;
    throw new Error("retained_readiness_access");
  };
  await runnerWith(() => ({ executor: {
    sandboxesCommands: true,
    run: async () => { models++; throw new Error("must not enter model"); },
  } }), fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
  assert.ok(refusals > 0, "the readiness refusal was reached");
  assert.equal(models, 0, "no successor is admitted after a readiness refusal");
  const diagnostics = log.lines.filter((line: any) => line.msg === warning) as Array<Record<string, any>>;
  assert.equal(diagnostics.length, refusals, "every refusal surfaced in its own iteration");
  assert.ok(diagnostics.every(line => line.diagnostic_code === "retained_readiness_access"));
  const journal = JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
  assert.equal(journal.recovery.stage, "blocked");
  assert.equal(journal.recovery.attempts, refusals);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "local work\n");
});
