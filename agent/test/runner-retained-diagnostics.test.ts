import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import type http from "node:http";
import { execFileSync } from "node:child_process";
import type { RunRunner } from "../src/runner.js";
import { formatRetainedRecoveryDiagnostic, RetainedReadinessCleanupUnconfirmedError } from "../src/git.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { codexRig } from "./runner-codex-sinks-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

// issue #3994: the post-adoption ownership read goes through this per-test transport hook, so a
// fault is injected at the WorkerClient transport only (never by replacing a runner method).
let transportHook: typeof fetch | undefined;
installHarness({ fetch: (input, init) => (transportHook ?? globalThis.fetch)(input, init) });
afterEach(() => { transportHook = undefined; });
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
  await runnerWith(() => {
    const rig = codexRig();
    return { executor: {
      sandboxesCommands: true,
      settleForCredentialFreeCapture: rig.settle,
      withCredentialFreeCapture: rig.capture,
      safety: rig.safety,
      run: async () => { models++; throw new Error("must not enter model"); },
    } };
  }, fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
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

for (const mode of ["operational", "non-error", "abort", "ownership", "credential", "getter-boundary", "getter-name"] as const) {
  it("M1 post-adoption check: " + mode, async () => {
    const { claim, bare, clone } = await seed();
    const log = recordingLogger();
    const operational = new TypeError("check /private/source/" + "a".repeat(40) + " " + secret);
    const before = Object.getOwnPropertyDescriptors(operational);
    if (mode === "getter-boundary" || mode === "getter-name") {
      Object.defineProperty(operational, mode === "getter-boundary" ? "checkpointBoundary" : "name", {
        get() { throw new Error("classification getter failed"); },
      });
    }
    let adopted = false, faulted = false, models = 0, factories = 0;
    let observedFailure: unknown;
    const recovery = git as any;
    const operation = recovery.withRecoveryOperation.bind(git);
    recovery.withRecoveryOperation = async (...args: unknown[]) => {
      try { return await operation(...args); }
      catch (error) {
        if (adopted && faulted && observedFailure === undefined) observedFailure = error;
        throw error;
      }
    };
    const prepare = git.prepareRecoverySuccessor.bind(git);
    const runner = runnerWith(() => {
      factories++;
      return { executor: { sandboxesCommands: false, run: async () => { models++; throw new Error("must not enter model"); } } };
    }, fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
    let flight: any;
    const ownership = (runner as any).requireRetainedOwnership.bind(runner);
    (runner as any).requireRetainedOwnership = async (...args: any[]) => {
      flight = args[0];
      if (adopted && !faulted) {
        faulted = true;
        if (mode === "operational" || mode === "getter-boundary" || mode === "getter-name") throw operational;
        if (mode === "non-error") throw "postcheck ordinary value";
        if (mode === "abort") throw Object.assign(new Error("postcheck aborted"), { name: "AbortError" });
      }
      return ownership(...args);
    };
    let successor: Awaited<ReturnType<typeof prepare>> | undefined;
    git.prepareRecoverySuccessor = async (...args) => {
      successor = await prepare(...args);
      adopted = true;
      assert.equal(JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`)).recovery.stage, "ready-for-model");
      if (mode === "ownership") api.setOwnershipStatus(claim.run_id, "running", 3);
      if (mode === "credential") flight.steering.tripCredentialSwitch(2);
      return successor;
    };
    await runner.execute(claim);
    assert.equal(adopted, true);
    assert.equal(faulted, true);
    assert.equal(models, 0);
    assert.equal(factories, 1, "postcheck failure never recreates the executor");
    const diagnostics = log.lines.filter((line: any) => line.msg === warning) as Array<Record<string, any>>;
    if (mode === "operational") {
      assert.equal(diagnostics.length, 1);
      const diagnostic = diagnostics[0]!;
      assert.equal(diagnostic.diagnostic_stage, "successor");
      assert.equal(diagnostic.diagnostic_code, "retained_successor_post_adoption_check");
      assert.equal(diagnostic.cause, "");
      assert.equal(diagnostic.context, undefined);
      assert.equal(diagnostic.detail, undefined);
      assert.doesNotMatch(JSON.stringify(diagnostic), /private|aaaaaaa|ijklmnopqrst/);
      assert.deepEqual(Object.getOwnPropertyDescriptors(operational), before);
    } else if (mode === "non-error" || mode === "abort") {
      assert.equal(diagnostics.length, 1);
      assert.equal(diagnostics[0]!.diagnostic_code, "recovery_failed");
      assert.equal(diagnostics[0]!.cause, mode === "abort" ? "postcheck aborted" : "postcheck ordinary value");
    } else if (mode === "getter-boundary" || mode === "getter-name") {
      assert.equal(observedFailure === operational, true, "classification must preserve original Error identity");
      assert.equal(diagnostics.length, 1);
      assert.equal(diagnostics[0]!.diagnostic_code, "recovery_failed");
      assert.doesNotMatch(diagnostics[0]!.cause, /classification getter failed/);
    } else {
      assert.equal(diagnostics.length, 0, "runtime stop sentinels bypass operational warnings");
      assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "recovery_wait"), false);
    }
    assert.ok(successor);
    assert.ok(fs.existsSync(successor.path), "successor custody remains");
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "local work\n");
    const journal = JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
    assert.doesNotMatch(JSON.stringify(journal), /diagnostic_/);
  });
}

// ---- issue #3994: transient failures of the post-adoption ownership read ----------------------------
// The window opens when the real prepareRecoverySuccessor returns (the successor is adopted) and
// closes when the model is entered or the iteration-failure warning is logged. Every ownership GET
// inside it is counted and optionally faulted by the transport hook above.
type Fault = "ok" | "econnrefused" | "econnreset" | "und_err_socket" | "enotfound" | "nocause" |
  "malformed" | "truncated" | "other-generation" | "other-status" | "not-owned" | 502 | 503 | 504 | 403;
type Hooked = { waitRecoveryRetry: (...args: unknown[]) => Promise<void> };
interface DriveOptions {
  /** Fault for window GET n (1-based); "ok" beyond the list. */
  script?: Fault[];
  /** Iterations failed by the generic ensureClone fault before the one under test. */
  failIterations?: number;
  /** A Codex-like executor: its `safety` makes the runner re-check ownership a second time. */
  safety?: boolean;
  /** Runs inside the transport while GET n of the window is in flight (before its fault applies). */
  onGet?: (n: number, deadline: number) => void;
  /** Runs before / after the real retry wait of the window. */
  onWait?: (flight: any, runner: RunRunner, n: number, deadline: number) => void;
  afterWait?: (flight: any, runner: RunRunner, n: number, deadline: number) => void;
  /** The runner's recoveryRetryMs (default 1). */
  recoveryRetryMs?: number;
}
async function freedPort(): Promise<number> {
  const server = net.createServer();
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as net.AddressInfo;
  await new Promise<void>(resolve => server.close(() => resolve()));
  return port;
}
const causeClass = (fault: Fault): string => typeof fault === "number" ? `http_${fault}` : fault;
const TRANSIENT: Fault[] = ["econnrefused", "econnreset", "und_err_socket", 502, 503, 504];

async function drive(options: DriveOptions = {}) {
  const { claim, bare, clone } = await seed();
  const runId = claim.run_id;
  const log = recordingLogger();
  const realFetch = globalThis.fetch;
  const deadPort = await freedPort();
  client.protocolFeatures = ["recovery_cause_retained_recovery_adoption_failed"];
  // A confirmed adoption_failed park is a recovery_wait report whose ACK carries status and cause.
  api.stateAckWire = (body, run) => body.status === "recovery_wait" && body.recovery_cause === "retained_recovery_adoption_failed"
    ? { status: 200, body: { run: { ...run, status: "recovery_wait", recovery_wait_cause: body.recovery_cause } } }
    : undefined;
  const warnings = () => log.lines.filter((line: any) => line.msg === warning).length;
  let faults = 0, models = 0, prepares = 0, reserves = 0, reservesAfterHandoff = 0;
  let windowGets = 0, windowWaits = 0, armed = false, warnAtArm = 0, deadline = 0;
  const modelRuns: Array<{ cwd: string }> = [];
  const windowWaitMs: unknown[] = [];
  let closed = false, depth = 0, armedDepth = -1;
  const inWindow = () => armed && !closed && models === 0 && warnings() === warnAtArm;
  const operation = git.withRecoveryOperation.bind(git);
  git.withRecoveryOperation = (async (...args: Parameters<typeof operation>) => {
    depth++;
    try { return await operation(...args); }
    finally { if (armed && depth === armedDepth) closed = true; depth--; }
  }) as typeof git.withRecoveryOperation;
  const realEnsure = git.ensureClone.bind(git);
  git.ensureClone = (async (...args: Parameters<typeof realEnsure>) => {
    if (faults < (options.failIterations ?? 0)) { faults++; throw new Error("connection reset " + secret); }
    return realEnsure(...args);
  }) as typeof git.ensureClone;
  const reserve = git.reserveRecoveryIteration.bind(git);
  git.reserveRecoveryIteration = async (...args) => {
    reserves++;
    if (armed) reservesAfterHandoff++;
    const progress = await reserve(...args);
    deadline = (progress as { deadline: number }).deadline;
    return progress;
  };
  const prepare = git.prepareRecoverySuccessor.bind(git);
  let successor: Awaited<ReturnType<typeof prepare>> | undefined;
  git.prepareRecoverySuccessor = async (...args) => {
    prepares++;
    successor = await prepare(...args);
    armed = true;
    armedDepth = depth;
    warnAtArm = warnings();
    return successor;
  };
  const dirty = new Set<Fault>();
  const normal = () => {
    api.ownershipWire.delete(runId);
    api.setOwnershipStatus(runId, "running", 2);
  };
  transportHook = async (input, init) => {
    const url = String(input);
    if ((init?.method ?? "GET") !== "GET" || !url.endsWith(`/runs/${runId}/ownership`)) return realFetch(input, init);
    if (!inWindow()) { if (dirty.size > 0) { normal(); dirty.clear(); } return realFetch(input, init); }
    const n = ++windowGets;
    options.onGet?.(n, deadline);
    const fault = options.script?.[n - 1] ?? "ok";
    normal();
    if (fault !== "ok") dirty.add(fault);
    if (typeof fault === "number") api.failOwnership(runId, fault);
    else if (fault === "econnreset") api.ownershipWire.set(runId, res => { res.socket!.resetAndDestroy(); });
    else if (fault === "und_err_socket") api.ownershipWire.set(runId, res => { res.socket!.destroy(); });
    else if (fault === "malformed") api.ownershipWire.set(runId, (res: http.ServerResponse) => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end("{not json");
    });
    else if (fault === "truncated") api.ownershipWire.set(runId, (res: http.ServerResponse) => {
      res.writeHead(200, { "Content-Type": "application/json", "Content-Length": "1000" });
      res.flushHeaders();
      res.write("{\"status\":");
      setImmediate(() => res.destroy());
    });
    else if (fault === "other-generation") api.setOwnershipStatus(runId, "running", 3);
    else if (fault === "other-status") api.setOwnershipStatus(runId, "paused", 2);
    else if (fault === "not-owned") api.setOwnershipNotOwned(runId);
    else if (fault === "econnrefused") {
      const target = new URL(url);
      target.port = String(deadPort);
      return realFetch(target, init);
    } else if (fault === "enotfound") {
      throw new TypeError("fetch failed", { cause: Object.assign(new Error("getaddrinfo ENOTFOUND"), { code: "ENOTFOUND" }) });
    } else if (fault === "nocause") throw new TypeError("fetch failed");
    return realFetch(input, init);
  };
  const executor = () => {
    const rig = options.safety ? codexRig() : undefined;
    return {
    sandboxesCommands: false,
    ...(rig ? { settleForCredentialFreeCapture: rig.settle, withCredentialFreeCapture: rig.capture, safety: rig.safety } : {}),
    run: async (ctx: { worktreePath: string }) => {
      models++;
      modelRuns.push({ cwd: ctx.worktreePath });
      throw new Error("model reached");
    },
    };
  };
  const runner = runnerWith(() => ({ executor: executor() as never }), fakeGitlab().gitlab, undefined, log.logger,
    { recoveryRetryMs: options.recoveryRetryMs ?? 1 });
  const wait = (runner as unknown as Hooked).waitRecoveryRetry.bind(runner);
  (runner as unknown as Hooked).waitRecoveryRetry = async (flight: any, ...rest: unknown[]) => {
    if (!inWindow()) return wait(flight, ...rest);
    const n = ++windowWaits;
    windowWaitMs.push(rest[1]);
    options.onWait?.(flight, runner, n, deadline);
    try { return await wait(flight, ...rest); } finally { options.afterWait?.(flight, runner, n, deadline); }
  };
  const journal = () => JSON.parse(command(bare, "config", `uzi-recovery.${clone.branch}.clone`));
  await runner.execute(claim);
  const states = (status: string) => api.states.filter(s => s.runId === runId && s.body.status === status);
  return {
    claim, bare, clone, runner, log, journal, states,
    parks: () => api.states.filter(state => state.runId === runId && state.body.status === "recovery_wait"),
    get successor() { return successor; },
    get windowGets() { return windowGets; },
    get windowWaits() { return windowWaits; },
    get windowWaitMs() { return windowWaitMs; },
    get deadline() { return deadline; },
    models: () => models, prepares: () => prepares, reserves: () => reserves,
    reservesAfterHandoff: () => reservesAfterHandoff, modelRuns,
    retryLogs: () => log.lines.filter((line: any) => line.msg === "retained post-adoption ownership read transient; retrying") as Array<Record<string, any>>,
    warnings: () => log.lines.filter((line: any) => line.msg === warning) as Array<Record<string, any>>,
  };
}
type Driven = Awaited<ReturnType<typeof drive>>;



const successorDirs = (clonePath: string): string[] =>
  fs.readdirSync(path.dirname(clonePath)).filter(name => name.startsWith(path.basename(clonePath) + ".attempt-"));
const retriesLogOnlyClass = (r: Driven, fault: Fault) => {
  const logs = r.retryLogs();
  assert.equal(logs.length, 1);
  assert.equal(logs[0]!.attempt, 1);
  assert.equal(logs[0]!.cause_class, causeClass(fault));
  assert.equal(logs[0]!.claim_generation, 2);
  assert.deepEqual(Object.keys(logs[0]!).sort(),
    ["attempt", "cause_class", "claim_generation", "issue_iid", "level", "msg", "run_id"], "a bounded token, never error text");
  assert.doesNotMatch(JSON.stringify(logs[0]), /fetch failed|127\.0\.0\.1/);
};

it("acceptance: a transient first post-adoption read in the third iteration retries in place and enters the model once", async () => {
  const r = await drive({ script: ["econnrefused"], failIterations: 2 });
  assert.equal(r.models(), 1, "the model is entered exactly once");
  assert.equal(r.prepares(), 1, "one successor is prepared, in iteration 3");
  assert.equal(r.reserves(), 3, "three iterations were reserved in total");
  assert.equal(r.reservesAfterHandoff(), 0, "no iteration is reserved after the handoff");
  assert.equal(r.windowGets, 2);
  assert.equal(r.windowWaits, 1);
  retriesLogOnlyClass(r, "econnrefused");
  const journal = r.journal();
  assert.equal(journal.recovery.attempts, 3);
  assert.equal(journal.recovery.stage, "ready-for-model");
  assert.equal(journal.recovery.claimGeneration, 2);
  const successor = r.successor!;
  assert.equal(r.modelRuns[0]!.cwd, successor.path, "the model runs at the adopted successor path");
  assert.equal(journal.clonePath, successor.path);
  assert.equal(journal.recovery.successor.attemptId, successor.attemptId);
  assert.deepEqual(successorDirs(r.clone.path), [path.basename(successor.path)], "no second successor path");
  assert.equal(r.warnings().length, 2, "only the two generic iteration failures were logged");
  assert.ok(r.warnings().every(line => line.diagnostic_code !== "retained_successor_post_adoption_check"));
  assert.deepEqual(r.parks(), []);
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "local work\n");
});

for (const fault of TRANSIENT) {
  it("control: a transient post-adoption read failure continues to the model: " + String(fault), async () => {
    const r = await drive({ script: [fault] });
    assert.equal(r.windowGets, 2);
    assert.equal(r.windowWaits, 1);
    assert.equal(r.models(), 1);
    assert.equal(r.reserves(), 1);
    assert.equal(r.prepares(), 1);
    assert.equal(r.journal().recovery.attempts, 1);
    assert.equal(r.warnings().length, 0);
    assert.deepEqual(r.parks(), []);
    retriesLogOnlyClass(r, fault);
  });
}

for (const fault of ["enotfound", "nocause", 403, "malformed", "truncated"] as Fault[]) {
  it("control: a non-transient post-adoption read failure is today's confirmed adoption_failed park: " + String(fault), async () => {
    const r = await drive({ script: [fault] });
    assert.equal(r.windowGets, 1, "no retry");
    assert.equal(r.windowWaits, 0);
    assert.equal(r.models(), 0);
    assert.equal(r.retryLogs().length, 0);
    assert.deepEqual(r.warnings().map(line => line.diagnostic_code), ["retained_successor_post_adoption_check"]);
    assert.equal(r.parks().length, 1);
    assert.equal(r.parks()[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(r.states("failed").length, 0);
    assert.equal(r.journal().recovery.stage, "blocked");
    assert.equal(r.journal().recovery.blocker, "adoption_failed");
  });
}

const stops: Array<[string, Fault[]]> = [
  ["a wrong generation on the first read", ["other-generation"]],
  ["a wrong status on the first read", ["other-status"]],
  ["a strict 404 on the first read", ["not-owned"]],
  ["a generation change after a transient failure", [503, "other-generation"]],
  ["a strict 404 after a transient failure", [503, "not-owned"]],
];
for (const [name, script] of stops) {
  it("control: " + name + " stops without a failure, park or model", async () => {
    const r = await drive({ script });
    assert.equal(r.windowGets, script.length, "exactly one further read after the transient failure, then stop");
    assert.equal(r.windowWaits, script.length - 1);
    assert.equal(r.models(), 0);
    assert.equal(r.warnings().length, 0);
    assert.deepEqual(r.parks(), []);
    assert.equal(r.states("failed").length, 0);
    assert.equal(r.journal().recovery.stage, "ready-for-model");
  });
}

it("control: a wait the iteration deadline cannot afford is not started and ends as today's adoption_failed park", async t => {
  const r = await drive({ script: [503], onGet: (n, deadline) => { if (n === 1) t.mock.method(Date, "now", () => deadline - 1); } });
  assert.equal(r.windowWaits, 0);
  assert.equal(r.windowGets, 1);
  assert.equal(r.retryLogs().length, 0);
  assert.equal(r.models(), 0);
  assert.deepEqual(r.warnings().map(line => line.diagnostic_code), ["retained_successor_post_adoption_check"]);
  assert.equal(r.parks().length, 1);
  assert.equal(r.journal().recovery.blocker, "adoption_failed");
});

it("control: a deadline that expires during the read ends as budget_exhausted without another read", async t => {
  const r = await drive({ script: [503], onGet: (n, deadline) => { if (n === 1) t.mock.method(Date, "now", () => deadline); } });
  assert.equal(r.windowGets, 1);
  assert.equal(r.windowWaits, 0);
  assert.equal(r.models(), 0);
  assert.equal(r.journal().recovery.blocker, "budget_exhausted");
  assert.deepEqual(r.parks(), []);
  assert.equal(r.states("failed").length, 1);
});

it("control: a deadline that expires during the wait refuses the next read before any request", async t => {
  const r = await drive({ script: [503], afterWait: (_flight, _runner, _n, deadline) => { t.mock.method(Date, "now", () => deadline + 1); } });
  assert.equal(r.windowWaits, 1);
  assert.equal(r.windowGets, 1, "the next read was refused before a request was made");
  assert.equal(r.models(), 0);
  assert.equal(r.journal().recovery.blocker, "budget_exhausted");
  assert.deepEqual(r.parks(), []);
  assert.equal(r.states("failed").length, 1);
});

it("control: five transient failures exhaust the attempt cap and end as today's adoption_failed park", async () => {
  const r = await drive({ script: Array<Fault>(5).fill("econnreset"), recoveryRetryMs: 3 });
  assert.equal(r.windowGets, 5, "no sixth ownership read inside the post-adoption retry window");
  assert.equal(r.windowWaits, 4);
  assert.deepEqual(r.windowWaitMs, [1, 2, 4, 8].map(f => f * 3), "the backoff doubles from recoveryRetryMs");
  assert.equal(r.retryLogs().length, 4);
  assert.deepEqual(r.retryLogs().map(line => line.attempt), [1, 2, 3, 4]);
  assert.equal(r.models(), 0);
  assert.deepEqual(r.warnings().map(line => line.diagnostic_code), ["retained_successor_post_adoption_check"]);
  assert.equal(r.parks().length, 1);
  assert.equal(r.journal().recovery.blocker, "adoption_failed");
});

const interruptions = ["cancel", "pause now", "pause wall", "disk", "switch", "shutdown"] as const;
for (const kind of interruptions) {
  const outcome = kind === "pause now"
    ? "completes ownership confirmation before pause settlement"
    : "ends the retry with no further read, park or model";
  it("control: " + kind + " during the retry wait " + outcome, async () => {
    const r = await drive({
      script: [503],
      onWait: (flight, runner) => {
        const route = (k: string, body?: string) => flight.steering.route(k, body, 1, false, undefined);
        if (kind === "cancel") route("cancel");
        else if (kind === "pause now") route("pause", "now");
        else if (kind === "pause wall") route("pause", "wall");
        else if (kind === "disk") flight.steering.requestDiskStop();
        else if (kind === "switch") flight.steering.tripCredentialSwitch(2);
        else runner.shutdown();
      },
    });
    assert.equal(r.windowWaits, 1);
    // #3995 defers owner-now during retained recovery; the transient ownership read completes.
    assert.equal(r.windowGets, kind === "pause now" ? 2 : 1, "only the deferred owner pause allows the retry");
    assert.equal(r.models(), 0);
    assert.equal(r.warnings().length, 0, "runtime stop sentinels bypass operational warnings");
    assert.deepEqual(r.parks(), []);
    const failed = r.states("failed");
    if (kind === "cancel") assert.deepEqual(failed.map(state => state.body.failure_reason), ["run cancelled"]);
    else assert.equal(failed.length, 0);
    assert.equal(r.journal().recovery.stage, "ready-for-model");
    assert.ok(fs.existsSync(r.successor!.path), "successor custody remains");
  });
}

it("control: a milestone pause request does not interrupt the retry", async () => {
  const r = await drive({
    script: [503],
    onWait: flight => { flight.steering.route("pause", "milestone", 1, false, undefined); },
  });
  assert.equal(r.windowGets, 2);
  assert.equal(r.windowWaits, 1);
  assert.equal(r.models(), 1);
});

it("control: a Codex-like executor's second ownership re-check retries a transient failure and continues", async () => {
  const r = await drive({ safety: true, script: ["ok", 503] });
  assert.equal(r.windowGets, 3, "check, failed re-check, retried re-check");
  assert.equal(r.windowWaits, 1);
  assert.equal(r.models(), 1);
  assert.equal(r.retryLogs().length, 1);
  assert.deepEqual(r.parks(), []);
});

it("control: a Codex-like executor's second ownership re-check with an unknown failure is today's quiescence_failed", async () => {
  const r = await drive({ safety: true, script: ["ok", "enotfound"] });
  assert.equal(r.windowGets, 2);
  assert.equal(r.windowWaits, 0);
  assert.equal(r.models(), 0);
  assert.equal(r.journal().recovery.blocker, "quiescence_failed");
  assert.deepEqual(r.parks(), []);
  assert.match(r.states("failed")[0]!.body.failure_reason ?? "", /quiescent/);
});
