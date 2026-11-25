import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import path from "node:path";
import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { FreshResumeReplyError, RequestError, WorkerClient, decodeFreshResumeState, freshResumeRefusalReason } from "../src/client.js";
import { FRESH_RESUME_CAPABILITY, FreshResumeClaimError, decodeFreshResumeClaim, freshResumeCapabilities, isFreshPreSeedAck, isHostedSlot } from "../src/fresh-resume.js";
import { api, client as harnessClient, fakeGitlab, fx, installHarness, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import { pollUntil } from "./worker-sweep-rig.js";

// issue #4628: the wire half of the destination: the capability, the claim object, the four worker routes and the pre-seed refusal.

const POD_UID = "3f2c1b9e-5a6d-4c7e-8f90-a1b2c3d4e5f6";
const FRID = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
const SHA = "a".repeat(40);

describe("the fresh_resume_v1 capability", () => {
  it("is offered only to a hosted slot (a UUID pod uid)", () => {
    assert.equal(isHostedSlot({ UZI_POD_UID: POD_UID }), true);
    assert.equal(isHostedSlot({ UZI_POD_UID: "not-a-uuid" }), false);
    assert.equal(isHostedSlot({}), false);
    assert.deepEqual(freshResumeCapabilities({ UZI_POD_UID: POD_UID }), [FRESH_RESUME_CAPABILITY]);
    assert.deepEqual(freshResumeCapabilities({}), []);
    assert.equal(FRESH_RESUME_CAPABILITY, "fresh_resume_v1");
  });

  describe("on the production registration list", () => {
    installHarness();
    const saved = process.env.UZI_POD_UID;
    afterEach(() => {
      if (saved === undefined) delete process.env.UZI_POD_UID; else process.env.UZI_POD_UID = saved;
    });

    async function registeredCapabilities(): Promise<string[]> {
      const outbox = new Outbox({ root: path.join(fx.dataDir, "fresh-cap"), log: nullLogger(), runMaxBytes: 1e6, maxBytes: 1e7, retentionMs: 86400000 });
      await outbox.init();
      const activeRuns = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
      api.setRegisterProtocolFeatures(["recovery_inventory_v1"]);
      let capabilities: string[] | undefined;
      const register = harnessClient.register.bind(harnessClient);
      harnessClient.register = (async (...args: Parameters<typeof register>) => { capabilities = args[4]; return register(...args); }) as typeof harnessClient.register;
      const runner = runnerWith(() => ({ executor: { run: async () => { throw new Error("unused"); } } }), fakeGitlab().gitlab, undefined, nullLogger(), { outbox, activeRuns });
      const controller = new AbortController();
      const config = { workerName: "fresh-cap", workerTemplate: "base", pollIntervalMs: 1, heartbeatIntervalMs: 5, chatPollMs: 1, chatSessions: 1,
        maxConcurrentRuns: 1, crossCheckSlots: 0, dockerWiring: {}, dataDir: "/tmp/does-not-matter", gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as unknown as Config;
      const worker = new Worker(config, harnessClient, runner as RunRunner, { execute: async () => {} } as unknown as ChatRunner,
        { execute: async () => {} } as unknown as JudgeRunner, { execute: async () => {} } as unknown as ReviewRunner,
        nullLogger(), () => ({ ok: true, missing: [] }), outbox, new Map(), activeRuns);
      const done = worker.run(controller.signal);
      try {
        await pollUntil(() => harnessClient.hasFeature("recovery_inventory_v1"), 5000, "worker registration");
      } finally {
        controller.abort();
        await done;
      }
      return capabilities ?? [];
    }

    it("a hosted slot advertises fresh_resume_v1 alongside recovery_inventory_v1", async () => {
      process.env.UZI_POD_UID = POD_UID;
      const list = await registeredCapabilities();
      assert.ok(list.includes("fresh_resume_v1"));
      assert.ok(list.includes("recovery_inventory_v1"));
    });

    it("a slot that is not hosted does not", async () => {
      delete process.env.UZI_POD_UID;
      const list = await registeredCapabilities();
      assert.equal(list.includes("fresh_resume_v1"), false);
      assert.ok(list.includes("recovery_inventory_v1"));
    });
  });
});

describe("the claim's fresh_resume object", () => {
  const capture = { id: FRID, kind: "capture", capture_id: "11111111-2222-4333-8444-555555555555", sha: SHA, evidence_generation: 1 };
  const checkpoint = { id: FRID, kind: "checkpoint_ref", checkpoint_ref: "refs/uzi-checkpoints/agent/issue-9", sha: SHA, evidence_generation: 3 };

  it("decodes both kinds, and absent as undefined", () => {
    assert.deepEqual(decodeFreshResumeClaim(capture), { id: FRID, kind: "capture", captureId: capture.capture_id, sha: SHA, evidenceGeneration: 1 });
    assert.deepEqual(decodeFreshResumeClaim(checkpoint), { id: FRID, kind: "checkpoint_ref", checkpointRef: checkpoint.checkpoint_ref, sha: SHA, evidenceGeneration: 3 });
    assert.equal(decodeFreshResumeClaim(undefined), undefined);
  });

  it("refuses a malformed object instead of reading it as absent", () => {
    for (const bad of [{ ...capture, sha: "xyz" }, { ...capture, kind: "other" }, { ...capture, capture_id: undefined }, { ...capture, evidence_generation: 0 },
      { ...checkpoint, checkpoint_ref: "refs/heads/main" }, { ...checkpoint, checkpoint_ref: "refs/uzi-checkpoints/../x" }, { ...capture, id: "nope" }, "text"]) {
      assert.throws(() => decodeFreshResumeClaim(bad), FreshResumeClaimError);
    }
  });
});

describe("the state reply decode", () => {
  const good = { state: "seeded", seeded_at: "2026-01-01T00:00:00Z", destination_attempt_id: "20260101T000000Z-g2-0123456789abcdef", seeded_generation: 2, current_authority: true };

  it("decodes the documented shape, with the seeded fields nullable", () => {
    assert.deepEqual(decodeFreshResumeState(good), good);
    assert.deepEqual(decodeFreshResumeState({ state: "refused", seeded_at: null, destination_attempt_id: null, seeded_generation: null, current_authority: false }),
      { state: "refused", seeded_at: null, destination_attempt_id: null, seeded_generation: null, current_authority: false });
  });

  it("never reads a missing or non-boolean current_authority as either answer", () => {
    for (const bad of [{ ...good, current_authority: undefined }, { ...good, current_authority: "true" }, { ...good, state: "" }, { ...good, seeded_generation: 0 }, null, []]) {
      assert.throws(() => decodeFreshResumeState(bad), FreshResumeReplyError);
    }
  });
});

describe("the four routes and the pre-seed refusal over HTTP", () => {
  let server: http.Server;
  let baseUrl: string;
  const seen: Array<{ method: string; url: string; body: string; auth: string | undefined }> = [];
  let respond: (req: http.IncomingMessage, res: http.ServerResponse, body: string) => void;

  beforeEach(async () => {
    seen.length = 0;
    server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", c => chunks.push(c as Buffer));
      req.on("end", () => {
        const body = Buffer.concat(chunks).toString("utf8");
        seen.push({ method: req.method ?? "", url: req.url ?? "", body, auth: req.headers.authorization });
        respond(req, res, body);
      });
    });
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });
  afterEach(async () => { await new Promise<void>(resolve => server.close(() => resolve())); });

  const newClient = () => new WorkerClient(baseUrl, "tok", "0.1.0-test", nullLogger());
  const json = (res: http.ServerResponse, status: number, body: unknown) => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(body)); };

  it("streams the bundle with the claim generation, and a 409 names its reason", async () => {
    respond = (_req, res) => { res.writeHead(200); res.end("BUNDLE-BYTES"); };
    let got = "";
    await newClient().freshResumeBundle("run-1", FRID, 4, async body => { for await (const c of body) got += (c as Buffer).toString("utf8"); });
    assert.equal(got, "BUNDLE-BYTES");
    assert.equal(seen[0]!.method, "GET");
    assert.equal(seen[0]!.url, `/api/worker/runs/run-1/fresh-resume/${FRID}/bundle?claim_generation=4`);
    assert.equal(seen[0]!.auth, "Bearer tok");
    respond = (_req, res) => json(res, 409, { error: "checkpoint ref", reason: "fresh_resume_no_bundle" });
    const err = await newClient().freshResumeBundle("run-1", FRID, 4, async () => {}).catch((e: unknown) => e);
    assert.ok(err instanceof RequestError);
    assert.equal(freshResumeRefusalReason(err), "fresh_resume_no_bundle");
  });

  it("posts abandon with the claim generation", async () => {
    respond = (_req, res) => json(res, 200, { id: FRID, state: "refused" });
    await newClient().abandonFreshResume("run-1", FRID, 4);
    assert.equal(seen[0]!.method, "POST");
    assert.equal(seen[0]!.url, `/api/worker/runs/run-1/fresh-resume/${FRID}/abandon`);
    assert.deepEqual(JSON.parse(seen[0]!.body), { claim_generation: 4 });
  });

  it("posts the seeded receipt, decodes the authority, and surfaces the two 409 reasons", async () => {
    respond = (_req, res) => json(res, 200, { state: "seeded", seeded_at: "2026-01-01T00:00:00Z", destination_attempt_id: "a", seeded_generation: 4, current_authority: false });
    const receipt = await newClient().reportFreshResumeSeeded("run-1", FRID, { claim_generation: 4, attempt_id: "20260101T000000Z-g4-0123456789abcdef", live_generation: 4 });
    assert.equal(receipt.current_authority, false);
    assert.equal(seen[0]!.url, `/api/worker/runs/run-1/fresh-resume/${FRID}/seeded`);
    assert.deepEqual(JSON.parse(seen[0]!.body), { claim_generation: 4, attempt_id: "20260101T000000Z-g4-0123456789abcdef", live_generation: 4 });
    for (const reason of ["fresh_resume_attempt_mismatch", "fresh_resume_not_claimed"]) {
      respond = (_req, res) => json(res, 409, { error: "no", reason });
      const err = await newClient().reportFreshResumeSeeded("run-1", FRID, { claim_generation: 4, attempt_id: "x", live_generation: 4 }).catch((e: unknown) => e);
      assert.equal(freshResumeRefusalReason(err), reason);
    }
    respond = (_req, res) => json(res, 200, { state: "seeded" });
    await assert.rejects(newClient().reportFreshResumeSeeded("run-1", FRID, { claim_generation: 4, attempt_id: "x", live_generation: 4 }), FreshResumeReplyError);
  });

  it("reads the state route, and a 404 is null", async () => {
    respond = (_req, res) => json(res, 200, { state: "seeded", seeded_at: "2026-01-01T00:00:00Z", destination_attempt_id: "a", seeded_generation: 4, current_authority: true });
    assert.equal((await newClient().getFreshResumeState("run-1", FRID, 4))?.current_authority, true);
    assert.equal(seen[0]!.url, `/api/worker/runs/run-1/fresh-resume/${FRID}/state?live_generation=4`);
    respond = (_req, res) => json(res, 404, { error: "not found", reason: "fresh_resume_not_found" });
    assert.equal(await newClient().getFreshResumeState("run-1", FRID, 4), null);
  });

  it("decodes the pre-seed 409 on a park report into a typed outcome, and only on the 409", async () => {
    const run = { id: "run-1", status: "running" };
    respond = (_req, res) => json(res, 409, { run, reason: "fresh_resume_pre_seed" });
    const refused = await newClient().reportState("run-1", { status: "recovery_wait", claim_generation: 4 } as never);
    assert.equal(refused.applied, false);
    assert.equal(isFreshPreSeedAck(refused), true);
    assert.equal(refused.reason, "fresh_resume_pre_seed");
    // A different 409 reason, and the same reason string on a 200, are not the pre-seed outcome.
    respond = (_req, res) => json(res, 409, { run, reason: "stale_revision_source" });
    assert.equal(isFreshPreSeedAck(await newClient().reportState("run-1", { status: "recovery_wait", claim_generation: 4 } as never)), false);
    respond = (_req, res) => json(res, 200, { run, reason: "fresh_resume_pre_seed" });
    assert.equal(isFreshPreSeedAck(await newClient().reportState("run-1", { status: "recovery_wait", claim_generation: 4 } as never)), false);
  });
});
