import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient, RequestError, isTransient } from "../src/client.js";
import { MessageBatcher } from "../src/batcher.js";

const TOKEN = "worker-join-token-0123456789";

let api: FakeApi;
let baseUrl: string;

function newClient(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    // Instant sleeps + a short schedule keep the retry tests fast.
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
}

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  baseUrl = await api.listen();
});

afterEach(async () => {
  await api.close();
});

describe("register / heartbeat / claim", () => {
  it("registers with name + version and Bearer auth", async () => {
    const res = await newClient().register("vlad-laptop");
    assert.ok(res.worker_id);
    assert.deepStrictEqual(api.registers, [{ name: "vlad-laptop", version: "0.1.0-test", authorized: true }]);
    assert.strictEqual(api.unauthorized, 0);
  });

  it("sends the template when the image declares one (PRD #18)", async () => {
    await newClient().register("vlad-laptop", "jvm");
    assert.deepStrictEqual(api.registers, [
      { name: "vlad-laptop", version: "0.1.0-test", template: "jvm", authorized: true },
    ]);
  });

  it("omits the template field when none is set", async () => {
    await newClient().register("vlad-laptop", undefined);
    const rec = api.registers[0];
    assert.ok(rec);
    // No `template` key at all, so an older image's register wire shape is intact.
    assert.strictEqual(rec.template, undefined);
    assert.ok(!("template" in rec));
  });

  it("sends capabilities when the worker has a daemon wired (PRD #83 Q1)", async () => {
    await newClient().register("vlad-laptop", "base", 1, ["docker"]);
    const rec = api.registers[0];
    assert.ok(rec);
    assert.deepStrictEqual(rec.capabilities, ["docker"]);
  });

  it("omits the capabilities field when none are reported (empty or undefined)", async () => {
    // Both an empty array and undefined must produce the SAME byte-identical wire as a
    // pre-#83 worker: no `capabilities` key at all (only send when non-empty).
    await newClient().register("vlad-laptop", "base", 1, []);
    await newClient().register("vlad-laptop", "base", 1, undefined);
    for (const rec of api.registers) {
      assert.strictEqual(rec.capabilities, undefined);
    }
  });

  it("rejects a wrong token with 401", async () => {
    const bad = new WorkerClient(baseUrl, "nope", "0.1.0-test", nullLogger());
    await assert.rejects(bad.heartbeat(), RequestError);
    assert.strictEqual(api.unauthorized, 1);
  });

  it("heartbeats", async () => {
    await newClient().heartbeat();
    assert.strictEqual(api.heartbeats, 1);
  });

  it("returns the claim on 200 and null on 204", async () => {
    const claim = makeClaim({ issue_iid: 42 });
    api.enqueueClaim(claim);
    const client = newClient();

    const got = await client.claimRun();
    assert.strictEqual(got?.run_id, claim.run_id);
    assert.strictEqual(got?.issue_iid, 42);
    assert.strictEqual(got?.secrets.forge_pat, claim.secrets.forge_pat);

    assert.strictEqual(await client.claimRun(), null);
  });
});

describe("reportState", () => {
  it("posts a non-terminal state once", async () => {
    const client = newClient();
    await client.reportState("run-1", { status: "running" });
    assert.deepStrictEqual(api.states, [{ runId: "run-1", body: { status: "running" } }]);
    assert.strictEqual(api.stateAttempts, 1);
  });

  it("retries a non-terminal report through transient failures", async () => {
    api.failStateNext(2, 503);
    const client = newClient();
    await client.reportState("run-1", { status: "running" });
    // 2 injected failures + 1 success — same backoff path as terminal reports.
    assert.strictEqual(api.stateAttempts, 3);
    assert.deepStrictEqual(api.states, [{ runId: "run-1", body: { status: "running" } }]);
  });

  it("gives up on a permanent 4xx for a non-terminal report", async () => {
    api.failStateNext(1, 400);
    const client = newClient();
    await assert.rejects(client.reportState("run-1", { status: "running" }), RequestError);
    assert.strictEqual(api.stateAttempts, 1); // no retry on a permanent error
  });

  it("retries a terminal report through transient failures", async () => {
    api.failStateNext(2, 503);
    const client = newClient();
    await client.reportState("run-1", { status: "completed", branch: "agent/issue-1" });
    // 2 injected failures + 1 success.
    assert.strictEqual(api.stateAttempts, 3);
    assert.deepStrictEqual(api.states, [{ runId: "run-1", body: { status: "completed", branch: "agent/issue-1" } }]);
  });

  it("treats an already-terminal (409) terminal report as success", async () => {
    api.markAlreadyTerminal("run-1");
    const client = newClient();
    // The server rejected it as already-terminal, so nothing was recorded, yet
    // the client did not surface an error. It now also reports WHAT happened
    // instead of collapsing the outcome to undefined.
    const ack = await client.reportState("run-1", { status: "failed", failure_reason: "boom" });
    // completedCount rides every ACK now: the RunDTO's milestones_completed is null (a nil Go
    // slice, no progress reported), which readRunAck reads as the fresh completed count 0.
    assert.deepStrictEqual(ack, { applied: false, status: "cancelled", completedCount: 0 });
    assert.strictEqual(api.states.length, 0);
  });

  it("gives up on a permanent 4xx for a terminal report", async () => {
    api.failStateNext(1, 400);
    const client = newClient();
    await assert.rejects(client.reportState("run-1", { status: "completed", branch: "b" }), RequestError);
    assert.strictEqual(api.stateAttempts, 1); // no retry on a permanent error
  });
});

// PRD #35's park acknowledgement contract, at the transport level. The
// consequences (which filesystem paths survive a park) are asserted in M1's runner
// tests; what is pinned HERE is that reportState surfaces the server's real answer
// at all, since that is the input those decisions are made from.
describe("reportState acknowledgement (PRD #35)", () => {
  it("reports the parked status back when the server applied the park", async () => {
    const client = newClient();
    const ack = await client.reportState("run-1", { status: "limit_wait", rate_limit_type: "five_hour" });
    // completedCount 0 rides the ACK: the parked RunDTO's milestones_completed is null (no
    // progress reported yet), which readRunAck reads as a fresh completed count of 0.
    assert.deepStrictEqual(ack, { applied: true, status: "limit_wait", completedCount: 0 });
  });

  // 🔴 THE DISCRIMINATING CASE. Everything else in this file passes against an
  // implementation that keys the park decision on `applied` instead of on `status`.
  //
  // The server DECLINED the park and failed the run — the designed outcome when the
  // retry budget is exhausted, when the RUN_LIMIT_MAX_PARK clamp is exceeded, or
  // when wait_on_limit is false and the report is coerced. It is a 200, because a
  // transition WAS applied; it was simply not the one that was asked for. So
  // `applied` is true here while the run is emphatically not parked, and a caller
  // reading `applied` would preserve the clone, the skills plugin dir and up to
  // ~170 MB of run HOME for a run that will never be claimed again.
  //
  // Budget exhaustion is the ordinary end of a run that keeps hitting limits, not an
  // error path, so this is the COMMON case rather than a corner of one.
  it("distinguishes a declined park from an applied one even though both answer 200", async () => {
    api.overrideStateStatus("run-1", "failed");
    const client = newClient();
    const ack = await client.reportState("run-1", { status: "limit_wait", rate_limit_type: "five_hour" });
    assert.strictEqual(ack.applied, true, "the server applied a transition, just not the requested one");
    assert.notStrictEqual(ack.status, "limit_wait", "the run is NOT parked and the status must say so");
    assert.strictEqual(ack.status, "failed");
  });

  it("yields an undefined status rather than throwing when the body is unreadable", async () => {
    // An older server, a truncated body, a proxy that rewrote the response. Every
    // one of them must read as "not parked" rather than as a failed report: the
    // caller's rule is a positive test for one literal, so absent is already safe.
    api.sendRawState("run-1", 200, "not json at all");
    const client = newClient();
    const ack = await client.reportState("run-1", { status: "limit_wait" });
    assert.deepStrictEqual(ack, { applied: true, status: undefined });
  });
});

describe("MessageBatcher seq numbering", () => {
  it("continues gapless numbering from last_seq across flushes", async () => {
    const client = newClient();
    const runId = "run-seq";
    const batcher = new MessageBatcher(client, runId, 5, 60_000, nullLogger());

    batcher.emit({ kind: "status", payload: { i: 1 } });
    batcher.emit({ kind: "text", payload: { i: 2 } });
    batcher.emit({ kind: "text", payload: { i: 3 } });
    await batcher.flush();

    batcher.emit({ kind: "text", payload: { i: 4 } });
    batcher.emit({ kind: "text", payload: { i: 5 } });
    await batcher.close();

    const seqs = api.messages(runId).map((m) => m.seq);
    assert.deepStrictEqual(seqs, [6, 7, 8, 9, 10]);
  });

  it("re-sends a failed batch on the next flush, preserving order and gaps", async () => {
    api.failMessagesNext(1, 503); // first /messages POST fails
    const client = newClient();
    const runId = "run-retry";
    const batcher = new MessageBatcher(client, runId, 0, 60_000, nullLogger());

    batcher.emit({ kind: "text", payload: { i: 1 } });
    await batcher.flush(); // fails; seq 1 is put back at the head of the buffer
    assert.strictEqual(api.messages(runId).length, 0);

    batcher.emit({ kind: "text", payload: { i: 2 } });
    await batcher.close(); // succeeds; delivers [1, 2] in order

    assert.deepStrictEqual(api.messages(runId).map((m) => m.seq), [1, 2]);
  });
});

describe("MessageBatcher close race", () => {
  it("awaits an in-flight flush so a batch that fails mid-flight is not stranded", async () => {
    // Gate the first postMessages open so a flush is provably in flight when
    // close() runs; that first attempt then fails and re-buffers its batch.
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let calls = 0;
    const fakeClient = {
      postMessages: async () => {
        calls += 1;
        if (calls === 1) {
          await gate;
          throw new Error("boom"); // in-flight flush fails after close() is called
        }
        // The drain retry from close() succeeds.
      },
    } as unknown as WorkerClient;

    const batcher = new MessageBatcher(fakeClient, "run-race", 0, 60_000, nullLogger());
    batcher.emit({ kind: "text", payload: { i: 1 } });

    const flushing = batcher.flush(); // starts, blocks on the gate
    const closing = batcher.close(); // runs while the flush is in flight
    release(); // let the in-flight flush fail and re-buffer seq 1
    await Promise.all([flushing, closing]);

    // Without awaiting the in-flight flush, close() would have returned before
    // the failure re-buffered and seq 1 would be lost (calls === 1).
    assert.strictEqual(calls, 2);
  });
});

describe("isTransient", () => {
  it("classifies retryable vs permanent", () => {
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 500, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 503, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 429, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 408, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 400, "")), false);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 404, "")), false);
    assert.strictEqual(isTransient(new Error("network down")), true);
  });
});

// Chat agent read surface (PRD #39 M3): the four worker-authenticated endpoints the
// uzi-tools MCP server calls. Verifies each hits the right path with the right query
// params, parses the wire shape, and that a foreign run id surfaces as 404.
describe("chat read surface (PRD #39 M3)", () => {
  it("listChatRuns hits /worker/chat/runs with ?limit and parses the runs", async () => {
    api.setChatRuns([
      {
        id: "r1", kind: "chat", status: "running", repo_path: null, issue_iid: null,
        title: "How does the plan gate work?", branch: null, mr_url: null, failure_reason: null,
        created_at: "2026-07-10T00:00:00Z", updated_at: "2026-07-10T00:00:00Z",
      },
    ]);
    const runs = await newClient().listChatRuns(25);
    assert.strictEqual(api.chatListLimits.at(-1), "25");
    assert.strictEqual(runs.length, 1);
    assert.strictEqual(runs[0]!.title, "How does the plan gate work?");
    assert.strictEqual(runs[0]!.repo_path, null);
  });

  it("listChatRuns omits ?limit when unset", async () => {
    api.setChatRuns([]);
    await newClient().listChatRuns();
    assert.strictEqual(api.chatListLimits.at(-1), null);
  });

  it("getChatRun parses the detail; a foreign/unknown id is 404", async () => {
    api.setChatRunDetail("r1", {
      id: "r1", kind: "issue", status: "failed", repo_path: "g/p", issue_iid: 57,
      title: "Fix login", branch: "agent/issue-57", mr_url: null, failure_reason: "tests failed",
      created_at: "2026-07-10T00:00:00Z", updated_at: "2026-07-10T00:00:00Z",
      mr_state: null, stop_kind: null, fix_verdict: null, iteration_count: 3, plan_md: "## Plan",
    });
    const d = await newClient().getChatRun("r1");
    assert.strictEqual(d.failure_reason, "tests failed");
    assert.strictEqual(d.iteration_count, 3);
    await assert.rejects(
      newClient().getChatRun("missing"),
      (e: unknown) => e instanceof RequestError && e.status === 404,
    );
  });

  it("getChatRunMessages passes after + limit and parses the page", async () => {
    api.setChatMessages("r1", [
      { seq: 4, kind: "text", agent: "lead", payload: { text: "working" }, created_at: "2026-07-10T00:00:00Z" },
    ]);
    const msgs = await newClient().getChatRunMessages("r1", 3, 100);
    const q = api.chatMessageQueries.at(-1)!;
    assert.strictEqual(q.runId, "r1");
    assert.strictEqual(q.after, "3");
    assert.strictEqual(q.limit, "100");
    assert.strictEqual(msgs[0]!.seq, 4);
  });

  it("createProposal POSTs to the run's proposals with repo_path and returns the pending proposal", async () => {
    const p = await newClient().createProposal("chat-1", {
      repo_path: "group/project", title: "Add dashboard", description: "please", labels: ["PRD"],
    });
    const req = api.proposalRequests.at(-1)!;
    assert.strictEqual(req.runId, "chat-1");
    assert.strictEqual(req.body.repo_path, "group/project");
    assert.strictEqual(p.status, "pending");
    assert.deepStrictEqual(p.labels, ["PRD"]);
  });
});
