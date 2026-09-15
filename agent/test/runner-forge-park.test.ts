import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { WorkerClient } from "../src/client.js";
import type {
  RecoveryReleaseResponse,
  RunOwnershipResponse,
  StateAck,
  StateRequest,
} from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import {
  TOKEN,
  api,
  baseUrl,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// PRD #1392 M2 — the pre-clone forge-unreachable park in the worker. The fake client below
// returns controlled state-report acks, ownership-probe results and release responses (the M2
// contract surface); `git.ensureClone` is overridden per test to throw a transient (park) or
// permanent (fail) forge error, so the run never actually clones.

/** A UUID-shaped SDK session id (matches sdk-session's SESSION_ID_RE), for the resume-leg cases. */
const SESSION_ID = "11111111-1111-1111-1111-111111111111";

const A_TRANSIENT = () => new Error("fatal: unable to access 'https://github.com/x/y.git/': Could not resolve host: github.com");
const A_PERMANENT = () => new Error("fatal: Authentication failed for 'https://github.com/x/y.git/'");

interface ParkHooks {
  /** Per park report (status === "recovery_wait"), n is 1-based. May throw to model transport. */
  onParkReport?: (body: StateRequest, n: number) => StateAck;
  /** Per ownership probe, n is 1-based. May throw (e.g. a RequestError 404). */
  onOwnership?: (n: number) => RunOwnershipResponse;
  /** Per release call. May throw to model a release transport failure. */
  onRelease?: (generation?: number) => RecoveryReleaseResponse;
}

/** A WorkerClient whose control-plane methods (reportState / getRunOwnership /
 *  releaseRecoveryCustody) are stubbed, while messages + inputs still hit the real FakeApi (so
 *  feed events land in `api.messages`). `protocolFeatures` is set directly (register is skipped). */
class ForgeParkClient extends WorkerClient {
  readonly reported: StateRequest[] = [];
  readonly releaseCalls: Array<{ generation?: number; evidence?: string }> = [];
  ownershipCalls = 0;
  private parkN = 0;

  constructor(features: string[], private readonly hooks: ParkHooks = {}) {
    super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      terminalRetrySchedule: [1, 1],
    });
    this.protocolFeatures = features;
  }

  override async reportState(_runId: string, body: StateRequest): Promise<StateAck> {
    this.reported.push({ ...body });
    if (body.status !== "recovery_wait") {
      // The initial `running` report and any generic `failed` report: a benign applied ack.
      return { applied: body.status !== undefined, status: body.status };
    }
    const n = ++this.parkN;
    if (this.hooks.onParkReport) return this.hooks.onParkReport(body, n);
    return { applied: true, status: "recovery_wait" };
  }

  override async getRunOwnership(_runId: string): Promise<RunOwnershipResponse> {
    const n = ++this.ownershipCalls;
    if (this.hooks.onOwnership) return this.hooks.onOwnership(n);
    return { status: "running" };
  }

  override async releaseRecoveryCustody(
    _runId: string,
    generation?: number,
    releaseEvidence?: string,
  ): Promise<RecoveryReleaseResponse> {
    this.releaseCalls.push({ generation, evidence: releaseEvidence });
    if (this.hooks.onRelease) return this.hooks.onRelease(generation);
    return { run_id: "x", released: true, holds_released: 1, generation };
  }
}

/** A factory whose HOME dir (and optionally a resumable transcript) is materialized when the
 *  factory runs — the executor's run() is NEVER reached on a pre-clone park, so a real run's
 *  HOME must exist by then to make its removal/preservation observable. */
function parkFactory(
  homeRoot: string,
  opts: { sessionId?: string } = {},
): { factory: ExecutorFactory; runHome: () => string } {
  let runHome = "";
  const factory: ExecutorFactory = (runId) => {
    runHome = path.join(homeRoot, runId);
    fs.mkdirSync(runHome, { recursive: true });
    if (opts.sessionId) {
      const proj = path.join(runHome, ".claude", "projects", "proj");
      fs.mkdirSync(proj, { recursive: true });
      fs.writeFileSync(path.join(proj, `${opts.sessionId}.jsonl`), "transcript", "utf8");
    }
    return {
      homeDir: runHome,
      executor: {
        run: async () => {
          throw new Error("executor.run() must never be reached on a pre-clone park");
        },
      },
    };
  };
  return { factory, runHome: () => runHome };
}

function makeRunner(client: WorkerClient, factory: ExecutorFactory): RunRunner {
  const { gitlab } = fakeGitlab();
  // No join token ⇒ the RecoveryCoordinator is disabled, matching a pre-clone park's "nothing to
  // capture" (fact 4). recoveryRetryMs is tiny so the probe-reconcile loop never stalls a test.
  return new RunRunner(client, git, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    recoveryRetryMs: 5,
  });
}

const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

const reportedStatuses = (client: ForgeParkClient): string[] =>
  client.reported.map((b) => String(b.status));

const parkReport = (client: ForgeParkClient): StateRequest | undefined =>
  client.reported.find((b) => b.status === "recovery_wait");

describe("RunRunner — pre-clone forge-unreachable park (PRD #1392 M2)", () => {
  it("permanent verdict (401/403/404 shape): fails immediately, never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-perm-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause", "recovery_release_exact_echo"]);
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_PERMANENT();
      };
      const claim = gitlabClaim(701, { claim_generation: 5 });
      await makeRunner(client, factory).execute(claim);

      assert.ok(!parkReport(client), "a permanent verdict must NOT send a recovery_wait report");
      assert.ok(
        reportedStatuses(client).includes("failed"),
        "a permanent verdict fails the run (today's path)",
      );
      assert.deepStrictEqual(client.releaseCalls, [], "no release endpoint on a permanent fail");
      assert.strictEqual(fs.existsSync(runHome()), false, "a failed pre-clone run leaves nothing behind");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: sends the typed report first, then parks on a recovery_wait ack (fresh claim leaves nothing behind)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-typed-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause", "recovery_release_exact_echo"], {
        onParkReport: () => ({
          applied: true,
          status: "recovery_wait",
          recoveryRetryNotBefore: "2026-09-15T16:00:00Z",
        }),
      });
      const { factory, runHome } = parkFactory(homeRoot); // fresh claim: no session id
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(702, { claim_generation: 9 });
      await makeRunner(client, factory).execute(claim);

      // The TYPED report is sent first, carrying the cause + generation.
      const park = parkReport(client);
      assert.ok(park, "the typed recovery_wait report must be sent");
      assert.strictEqual(park!.recovery_cause, "forge_unreachable");
      assert.strictEqual(park!.claim_generation, 9);
      // No terminal report, no release endpoint call (the api settles custody in the transaction).
      assert.ok(!reportedStatuses(client).includes("failed"), "a parked run reports no failure");
      assert.deepStrictEqual(client.releaseCalls, [], "recovery_park_cause api parks with NO release call");
      // Exactly ONE feed event, quoting the acked retry time.
      const texts = statusTexts(claim.run_id).filter((t) => /forge unreachable at clone; parked/.test(t));
      assert.strictEqual(texts.length, 1, "exactly one park feed event");
      assert.match(texts[0]!, /retry at 2026-09-15T16:00:00Z/);
      // Fresh claim (no resolvable transcript): HOME is removed.
      assert.strictEqual(fs.existsSync(runHome()), false, "a fresh forge-parked claim leaves nothing behind");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a cancelled ack cleans up, emits no park event, sends no second report", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-cancel-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: true, status: "cancelled" }),
      });
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(703, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "a cancelled ack emits NO park event",
      );
      // The park report was the only recovery_wait report; no failed re-report.
      assert.deepStrictEqual(
        reportedStatuses(client),
        ["running", "recovery_wait"],
        "no second report after a cancelled ack",
      );
      assert.strictEqual(fs.existsSync(runHome()), false, "a cancelled run cleans up its HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a failed (cap) ack cleans up and re-reports nothing", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-cap-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: true, status: "failed" }),
      });
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(704, { claim_generation: 6 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "the cap ack emits NO park event",
      );
      // The cap branch already committed the terminal state; the worker sends no second report.
      assert.deepStrictEqual(
        reportedStatuses(client),
        ["running", "recovery_wait"],
        "no re-report after a cap (failed) ack",
      );
      assert.strictEqual(fs.existsSync(runHome()), false, "a capped run cleans up its HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a stale_claim ack stops silently (no further report, no park)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-stale-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: false, status: "queued", reason: "stale_claim" }),
      });
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(705, { claim_generation: 2 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "a stale_claim ack emits NO park event",
      );
      assert.deepStrictEqual(
        reportedStatuses(client),
        ["running", "recovery_wait"],
        "a stale_claim ack sends no further report",
      );
      assert.strictEqual(fs.existsSync(runHome()), false, "a stale-claim run cleans up its HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a custody_unsettled ack keeps the batcher open and takes today's failed path", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-custody-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: false, status: "running", reason: "custody_unsettled" }),
      });
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(706, { claim_generation: 4 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "custody_unsettled emits NO park event",
      );
      assert.ok(
        reportedStatuses(client).includes("failed"),
        "custody_unsettled takes today's failed path (the still-open batcher lets the failed report + error land)",
      );
      // The error line proves the batcher was left open for the failed path to write.
      const errorTexts = api.messages(claim.run_id).filter((m) => m.kind === "error");
      assert.ok(errorTexts.length >= 1, "the failed path emitted its error line on the open batcher");
      assert.strictEqual(fs.existsSync(runHome()), false, "the failed path cleans up its HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a transport failure after the send reconciles by probe — recovery_wait preserves and emits", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-park-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up"); // transport failure AFTER the send
        },
        onOwnership: () => ({ status: "recovery_wait", recovery_retry_not_before: "2026-09-15T17:30:00Z" }),
      });
      const { factory, runHome } = parkFactory(homeRoot, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(707, { claim_generation: 8, session_id: SESSION_ID });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.ownershipCalls, 1, "reconciled via exactly one ownership probe");
      const texts = statusTexts(claim.run_id).filter((t) => /forge unreachable at clone; parked/.test(t));
      assert.strictEqual(texts.length, 1, "one park feed event on the probe-reconciled park");
      assert.match(texts[0]!, /retry at 2026-09-15T17:30:00Z/, "quotes the probe's retry time");
      assert.ok(!reportedStatuses(client).includes("failed"), "an unknown outcome is NEVER a failure");
      // A resume leg with a resolvable transcript: HOME + session are preserved.
      assert.strictEqual(fs.existsSync(runHome()), true, "a resolvable resume transcript keeps HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a transport failure reconciled to running resends the same report", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-resend-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: (_body, n) => {
          if (n === 1) throw new Error("socket hang up");
          return { applied: true, status: "recovery_wait" }; // the idempotent resend lands
        },
        onOwnership: () => ({ status: "running" }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(708, { claim_generation: 1 });
      await makeRunner(client, factory).execute(claim);

      const parkReports = client.reported.filter((b) => b.status === "recovery_wait");
      assert.strictEqual(parkReports.length, 2, "the same report is resent after a running probe");
      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        1,
        "one park event after the resend lands recovery_wait",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a transport failure reconciled to a terminal status cleans up (no park)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-term-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up");
        },
        onOwnership: () => ({ status: "failed" }),
      });
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(709, { claim_generation: 1 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "a terminal probe answer emits no park event",
      );
      assert.ok(!reportedStatuses(client).includes("failed"), "no failed re-report; the run is already terminal");
      assert.strictEqual(fs.existsSync(runHome()), false, "a terminal reconcile cleans up HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a transport failure reconciled to 404 (stale_claim) stops silently, never a failure", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-404-"));
    try {
      const { RequestError } = await import("../src/client.js");
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up");
        },
        onOwnership: () => {
          // A parked run keeps its worker_id, so the probe would answer recovery_wait; a 404 means
          // the run is genuinely no longer this worker's (reclaimed / stale_claim) — the park did
          // NOT land under our claim. Stop silently: no park, no failure.
          throw new RequestError("GET", "/ownership", 404, "not owned");
        },
      });
      const { factory, runHome } = parkFactory(homeRoot, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(710, { claim_generation: 1, session_id: SESSION_ID });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "a 404 reconcile emits no park event",
      );
      assert.ok(!reportedStatuses(client).includes("failed"), "an unknown-then-404 outcome is never a failure");
      // The run is definitively not ours (a parked-under-us run would return recovery_wait, not
      // 404), so this worker's HOME is cleaned as on any stale_claim (execute() serializes a
      // same-worker re-claim behind this cleanup, so no live session is deleted).
      assert.strictEqual(fs.existsSync(runHome()), false, "a not-ours run cleans up its HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: a TRANSIENT (non-404) probe throw is retried with the backoff honored; the eventual recovery_wait preserves (never cleaned mid-loop)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-retry-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up"); // transport failure AFTER the send → reconcile
        },
        onOwnership: (n) => {
          if (n <= 2) throw new Error("socket hang up"); // two transient (non-404) probe failures
          return { status: "recovery_wait", recovery_retry_not_before: "2026-09-15T18:00:00Z" };
        },
      });
      const { factory, runHome } = parkFactory(homeRoot, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(719, { claim_generation: 2, session_id: SESSION_ID });
      const runner = makeRunner(client, factory);
      // FIX A pin: spy on waitRecoveryRetry to capture the cancelStopsWait argument each reconcile
      // retry passes. Pre-fix the retries called waitRecoveryRetry(flight) (cancelStopsWait defaults
      // to true), so a simultaneous sticky cancel would make each wait return immediately and the
      // loop tight-spin; post-fix every reconcile retry passes false, honoring the backoff. Asserting
      // the argument is the "smallest reliable assertion" the task asks for (no wall-clock timing).
      const waitArgs: Array<boolean | undefined> = [];
      const target = runner as unknown as {
        waitRecoveryRetry: (flight: unknown, cancelStopsWait?: boolean) => Promise<void>;
      };
      const origWait = target.waitRecoveryRetry.bind(runner);
      target.waitRecoveryRetry = (flight: unknown, cancelStopsWait?: boolean) => {
        waitArgs.push(cancelStopsWait);
        return origWait(flight, cancelStopsWait);
      };
      await runner.execute(claim);

      assert.strictEqual(
        client.ownershipCalls,
        3,
        "the probe was retried (two transient throws, then the resolving probe) — call count >= 2",
      );
      const texts = statusTexts(claim.run_id).filter((t) => /forge unreachable at clone; parked/.test(t));
      assert.strictEqual(texts.length, 1, "the eventual recovery_wait parks");
      assert.match(texts[0]!, /retry at 2026-09-15T18:00:00Z/, "quotes the resolving probe's retry time");
      assert.ok(!reportedStatuses(client).includes("failed"), "a transient probe failure is NEVER a failure");
      assert.strictEqual(
        fs.existsSync(runHome()),
        true,
        "HOME/session preserved (resolvable transcript) — nothing was cleaned mid-loop",
      );
      assert.ok(waitArgs.length >= 2, "the loop waited between the two transient probe retries");
      assert.ok(
        waitArgs.every((c) => c === false),
        "every reconcile retry passes cancelStopsWait=false, so a sticky cancel cannot turn it into a tight loop",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("recovery_park_cause api: an INTERMEDIATE ownership status (queued) is retained and retried until recovery_wait", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-probe-queued-"));
    try {
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up");
        },
        onOwnership: (n) => (n === 1 ? { status: "queued" } : { status: "recovery_wait" }),
      });
      const { factory, runHome } = parkFactory(homeRoot, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(720, { claim_generation: 3, session_id: SESSION_ID });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.ownershipCalls, 2, "the intermediate `queued` status is retried, not resolved");
      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        1,
        "the eventual recovery_wait parks",
      );
      assert.ok(!reportedStatuses(client).includes("failed"), "an intermediate status is never a failure");
      assert.strictEqual(
        fs.existsSync(runHome()),
        true,
        "the possibly-parked session is retained across the intermediate status, never cleaned",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("worker shutdown DURING reconcile preserves a resolvable session (leaves the run for requeue, never cleans it)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-reconcile-shutdown-"));
    try {
      let runnerRef: RunRunner | undefined;
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up"); // transport failure → reconcile
        },
        onOwnership: () => {
          // The worker begins draining mid-reconcile. Return a non-terminal status; the loop then
          // waits (cancelStopsWait=false) and re-checks shuttingDownGlobal at the top, taking the
          // shutdown arm before probing again.
          runnerRef!.shutdown();
          return { status: "queued" };
        },
      });
      const { factory, runHome } = parkFactory(homeRoot, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(721, { claim_generation: 1, session_id: SESSION_ID });
      runnerRef = makeRunner(client, factory);
      await runnerRef.execute(claim);

      assert.strictEqual(client.ownershipCalls, 1, "the shutdown arm stops before the next probe");
      assert.strictEqual(
        statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length,
        0,
        "a shutdown interrupt emits NO park event",
      );
      assert.ok(!reportedStatuses(client).includes("failed"), "a shutdown interrupt is never a failure (left for requeue)");
      assert.strictEqual(
        fs.existsSync(runHome()),
        true,
        "the possibly-parked, resolvable session is PRESERVED for a same-worker requeue, not cleaned (FIX B)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("worker shutdown DURING reconcile with NO resolvable transcript cleans HOME (preserve is conditional on D4)", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-reconcile-shutdown-noxscript-"));
    try {
      let runnerRef: RunRunner | undefined;
      const client = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => {
          throw new Error("socket hang up");
        },
        onOwnership: () => {
          runnerRef!.shutdown();
          return { status: "queued" };
        },
      });
      const { factory, runHome } = parkFactory(homeRoot); // session id on the claim, but NO transcript planted
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(722, { claim_generation: 1, session_id: SESSION_ID });
      runnerRef = makeRunner(client, factory);
      await runnerRef.execute(claim);

      assert.strictEqual(
        fs.existsSync(runHome()),
        false,
        "no resolvable transcript ⇒ the shutdown arm cleans HOME (the D4 preserve rule is conditional, not unconditional)",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // ── D7 fallback: capability-aware degradation on an older api ──────────────────────

  it("D7 (recovery_release_exact_echo + claim_generation_fence): parks via an untyped report that KEEPS claim_generation after a proven exact release", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-fence-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo", "claim_generation_fence"], {
        onRelease: (gen) => ({ run_id: "x", released: true, holds_released: 1, generation: gen }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(711, { claim_generation: 12 });
      await makeRunner(client, factory).execute(claim);

      // Exactly one exact-generation release, with NO release_evidence (the proof is the echo).
      assert.strictEqual(client.releaseCalls.length, 1);
      assert.strictEqual(client.releaseCalls[0]!.generation, 12);
      assert.strictEqual(client.releaseCalls[0]!.evidence, undefined, "the fallback release stamps NO evidence");
      // The UNTYPED park report KEEPS claim_generation (fence advertised) and carries NO cause.
      const park = parkReport(client);
      assert.ok(park, "an untyped recovery_wait report must be sent");
      assert.strictEqual(park!.claim_generation, 12, "keeps claim_generation with the fence");
      assert.strictEqual(park!.recovery_cause, undefined, "the untyped fallback carries no cause");
      assert.strictEqual(statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length, 1, "one park event");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (recovery_release_exact_echo only): parks via an untyped report with NEITHER field after a proven exact release", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-echo-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        onRelease: (gen) => ({ run_id: "x", released: true, holds_released: 1, generation: gen }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(712, { claim_generation: 20 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.releaseCalls.length, 1);
      assert.strictEqual(client.releaseCalls[0]!.generation, 20);
      const park = parkReport(client);
      assert.ok(park, "an untyped recovery_wait report must be sent");
      assert.strictEqual(park!.claim_generation, undefined, "no generation field without the fence");
      assert.strictEqual(park!.recovery_cause, undefined, "no cause on the baseline fallback");
      assert.strictEqual(statusTexts(claim.run_id).filter((t) => /parked/.test(t)).length, 1, "one park event");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (neither token): never parks — takes today's failed path, sends nothing negotiated", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-none-"));
    try {
      const client = new ForgeParkClient([]); // an older api advertised no features
      const { factory, runHome } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(713, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.ok(!parkReport(client), "no recovery_wait report is sent when nothing is advertised");
      assert.deepStrictEqual(client.releaseCalls, [], "no release call when nothing is advertised");
      assert.ok(reportedStatuses(client).includes("failed"), "takes today's failed path");
      assert.strictEqual(fs.existsSync(runHome()), false, "the failed path cleans up HOME");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (sole-older-hold): a release echoing a DIFFERENT generation → proof fails → never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-mismatch-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        onRelease: () => ({ run_id: "x", released: true, holds_released: 1, generation: 99 }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(714, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.releaseCalls.length, 1, "the release was attempted");
      assert.ok(!parkReport(client), "a generation mismatch never parks");
      assert.ok(reportedStatuses(client).includes("failed"), "proof-failure takes today's failed path");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (sole-older-hold): a release with holds_released !== 1 → proof fails → never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-holds-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        onRelease: (gen) => ({ run_id: "x", released: true, holds_released: 2, generation: gen }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(715, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.ok(!parkReport(client), "holds_released !== 1 never parks");
      assert.ok(reportedStatuses(client).includes("failed"), "proof-failure takes today's failed path");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (echo present, release throws): a release transport failure → never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-relthrow-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        onRelease: () => {
          throw new Error("socket hang up");
        },
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(716, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.ok(!parkReport(client), "a thrown release never parks");
      assert.ok(reportedStatuses(client).includes("failed"), "a thrown release takes today's failed path");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (proof conjunct): a release with released:false (all else matching) → proof fails → never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-relfalse-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        // generation echoes the claim, holds_released === 1, NOT retained — only released is false.
        onRelease: (gen) => ({ run_id: "x", released: false, holds_released: 1, generation: gen }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(723, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.releaseCalls.length, 1, "the release was attempted");
      assert.ok(!parkReport(client), "released:false never parks — the proof requires released===true");
      assert.ok(reportedStatuses(client).includes("failed"), "proof-failure takes today's failed path");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("D7 (proof conjunct): a release that RETAINED (retained:true, released:true) → proof fails → never parks", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-d7-retained-"));
    try {
      const client = new ForgeParkClient(["recovery_release_exact_echo"], {
        // released:true and the generation/holds match, but the server RETAINED the hold — the
        // exact-generation release was NOT positively confirmed (rel.retained !== true fails).
        onRelease: (gen) => ({ run_id: "x", released: true, holds_released: 1, generation: gen, retained: true }),
      });
      const { factory } = parkFactory(homeRoot);
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claim = gitlabClaim(724, { claim_generation: 3 });
      await makeRunner(client, factory).execute(claim);

      assert.strictEqual(client.releaseCalls.length, 1, "the release was attempted");
      assert.ok(!parkReport(client), "a retained release never parks — the proof requires retained!==true");
      assert.ok(reportedStatuses(client).includes("failed"), "proof-failure takes today's failed path");
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // ── HOME / session preservation across the resume-leg vs fresh distinction ──────────

  it("resume leg WITH a resolvable transcript keeps HOME + session; WITHOUT does not", async () => {
    // With a resolvable transcript.
    const homeRootA = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-resume-yes-"));
    // Without one (session id present, but no transcript on this worker).
    const homeRootB = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-forgepark-resume-no-"));
    try {
      const clientA = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: true, status: "recovery_wait" }),
      });
      const { factory: fA, runHome: hA } = parkFactory(homeRootA, { sessionId: SESSION_ID });
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claimA = gitlabClaim(717, { claim_generation: 1, session_id: SESSION_ID });
      await makeRunner(clientA, fA).execute(claimA);
      assert.strictEqual(fs.existsSync(hA()), true, "a resolvable transcript keeps HOME");

      const clientB = new ForgeParkClient(["recovery_park_cause"], {
        onParkReport: () => ({ applied: true, status: "recovery_wait" }),
      });
      const { factory: fB, runHome: hB } = parkFactory(homeRootB); // no transcript planted
      git.ensureClone = async () => {
        throw A_TRANSIENT();
      };
      const claimB = gitlabClaim(718, { claim_generation: 1, session_id: SESSION_ID });
      await makeRunner(clientB, fB).execute(claimB);
      assert.strictEqual(fs.existsSync(hB()), false, "no resolvable transcript ⇒ HOME removed");
    } finally {
      fs.rmSync(homeRootA, { recursive: true, force: true });
      fs.rmSync(homeRootB, { recursive: true, force: true });
    }
  });
});
