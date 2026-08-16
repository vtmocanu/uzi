import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  buildFindingsToolsServer,
  makeFindingsToolHandlers,
  reportIncidentalIssueToolName,
  FINDINGS_SERVER_NAME,
  REPORT_INCIDENTAL_ISSUE_TOOL,
} from "../src/findings-tools.js";
import { isSignalToolName, scanSignals } from "../src/signals.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { EmittedMessage } from "../src/executor.js";
import type { ReportFindingRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// The incidental-findings tool (PRD #333 M2) is a PLAIN working tool on the run lane
// (D2): it POSTs to the api, emits a `finding` card, returns an ack, and the turn
// continues. These tests drive its raw handler with a fake WorkerClient — no live SDK,
// no HTTP — and prove: it records against the CURRENT run id only; the emitted card
// carries the returned finding id; a non-2xx (cap reached) is a SOFT ACK, never a
// throw; and — the M2 success criterion — it is NOT a signal, so it can never be
// promoted to turn-ending.

interface Calls {
  reportFinding: Array<{ runId: string; body: ReportFindingRequest }>;
}
function fakeClient(over: { id?: string; throws?: unknown } = {}): { client: WorkerClient; calls: Calls } {
  const calls: Calls = { reportFinding: [] };
  const client = {
    async reportFinding(runId: string, body: ReportFindingRequest): Promise<string> {
      calls.reportFinding.push({ runId, body });
      if (over.throws) throw over.throws;
      return over.id ?? "find-1";
    },
  } as unknown as WorkerClient;
  return { client, calls };
}

function handlersWith(client: WorkerClient, runId = "run-current"): {
  h: ReturnType<typeof makeFindingsToolHandlers>;
  emits: EmittedMessage[];
} {
  const emits: EmittedMessage[] = [];
  const h = makeFindingsToolHandlers({ client, runId, emit: (m) => emits.push(m), log: nullLogger() });
  return { h, emits };
}
const bodyText = (r: { content: { text: string }[] }): string => r.content[0]!.text;

describe("findings tool — report_incidental_issue happy path (PRD #333 M2)", () => {
  it("posts the finding to the CURRENT run id and emits a `finding` card carrying the returned id", async () => {
    const { client, calls } = fakeClient({ id: "find-7" });
    const { h, emits } = handlersWith(client, "run-current");
    const res = await h.reportIncidentalIssue({
      title: "Leaked ticker in the sweeper",
      description: "sweepLoop starts a time.Ticker it never Stops on the early-return path.",
      location: "api/internal/foo.go#sweepLoop",
      labels: ["bug"],
      confidence: "high",
    });

    assert.strictEqual(calls.reportFinding.length, 1);
    const call = calls.reportFinding[0]!;
    assert.strictEqual(call.runId, "run-current", "records against the current run, never an arbitrary id");
    assert.strictEqual(call.body.title, "Leaked ticker in the sweeper");
    assert.strictEqual(call.body.location, "api/internal/foo.go#sweepLoop");
    assert.deepStrictEqual(call.body.labels, ["bug"]);
    assert.strictEqual(call.body.confidence, "high");

    // Exactly one `finding` card, keyed on the returned id so the UI acts on it.
    assert.strictEqual(emits.length, 1, "exactly one card emitted");
    const card = emits[0]!;
    assert.strictEqual(card.kind, "finding");
    assert.strictEqual(card.payload["id"], "find-7");
    assert.strictEqual(card.payload["title"], "Leaked ticker in the sweeper");
    assert.strictEqual(card.payload["location"], "api/internal/foo.go#sweepLoop");
    assert.strictEqual(card.payload["confidence"], "high");
    assert.deepStrictEqual(card.payload["labels"], ["bug"]);

    // The ack tells the model the turn continues.
    assert.match(bodyText(res), /find-7/);
    assert.match(bodyText(res), /does NOT end your turn/i);
    assert.notStrictEqual(res.isError, true, "a recorded finding is not an error result");
  });

  it("omits confidence from the card and body when not supplied", async () => {
    const { client, calls } = fakeClient();
    const { h, emits } = handlersWith(client);
    await h.reportIncidentalIssue({ title: "T", description: "D", location: "a/b.go#f" });
    assert.ok(!("confidence" in calls.reportFinding[0]!.body), "no confidence key when unset");
    assert.ok(!("confidence" in emits[0]!.payload), "card omits confidence when unset");
    assert.deepStrictEqual(calls.reportFinding[0]!.body.labels, [], "labels default to []");
  });
});

describe("findings tool — errors are a SOFT ACK, never a throw (D11, turn continues)", () => {
  it("the per-run cap (429) returns the cap ack and emits NO card", async () => {
    const { client } = fakeClient({ throws: new RequestError("POST", "/x", 429, "finding cap reached for this run") });
    const { h, emits } = handlersWith(client);
    // If it threw, this await would reject and fail the test — the turn must continue.
    const res = await h.reportIncidentalIssue({ title: "T", description: "D", location: "a/b.go#f" });
    assert.strictEqual(emits.length, 0, "a failed record must not leave a phantom card in the feed");
    assert.match(bodyText(res), /finding cap reached for this run/i);
  });

  it("any other api error returns a generic soft ack and emits no card", async () => {
    const { client } = fakeClient({ throws: new RequestError("POST", "/x", 500, "boom") });
    const { h, emits } = handlersWith(client);
    const res = await h.reportIncidentalIssue({ title: "T", description: "D", location: "a/b.go#f" });
    assert.strictEqual(emits.length, 0);
    assert.match(bodyText(res), /could not record the finding/i);
  });

  it("a non-RequestError transport failure also returns (never throws)", async () => {
    const { client } = fakeClient({ throws: new Error("ECONNREFUSED") });
    const { h } = handlersWith(client);
    const res = await h.reportIncidentalIssue({ title: "T", description: "D", location: "a/b.go#f" });
    assert.ok(bodyText(res).length > 0);
  });
});

describe("findings tool — wiring and NON-signal posture (PRD #333 M2 success criterion)", () => {
  it("the built server exposes exactly report_incidental_issue under the `findings` name", () => {
    assert.strictEqual(FINDINGS_SERVER_NAME, "findings");
    const { server, toolName } = buildFindingsToolsServer({
      client: fakeClient().client,
      runId: "r",
      emit: () => {},
      log: nullLogger(),
    });
    assert.strictEqual(toolName, "mcp__findings__report_incidental_issue");
    const registered = (server as unknown as { instance: { _registeredTools: Record<string, unknown> } })
      .instance._registeredTools;
    assert.deepStrictEqual(Object.keys(registered), [REPORT_INCIDENTAL_ISSUE_TOOL]);
  });

  it("is NOT a signal: isSignalToolName is false and scanSignals never captures it", () => {
    const qualified = reportIncidentalIssueToolName();
    assert.strictEqual(isSignalToolName(qualified), false, "report_incidental_issue must never be in the signal/park set");

    // A main-thread assistant frame carrying a report_incidental_issue tool_use yields
    // NO signal — so it can never latch done or the plan (turn-ending).
    const frame = {
      type: "assistant",
      message: {
        content: [
          {
            type: "tool_use",
            name: qualified,
            input: { title: "T", description: "D", location: "a/b.go#f" },
          },
        ],
      },
    };
    assert.deepStrictEqual(scanSignals(frame), {}, "scanSignals does not capture the findings tool");
  });
});
