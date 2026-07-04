import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { makeRedactor } from "../src/redact.js";
import { recordingLogger } from "./helpers.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

// A WorkerClient stub that just records what was flushed.
function fakeClient(): { client: WorkerClient; sent: OutgoingMessage[] } {
  const sent: OutgoingMessage[] = [];
  const client = {
    async postMessages(_runId: string, messages: OutgoingMessage[]): Promise<void> {
      sent.push(...messages);
    },
  } as unknown as WorkerClient;
  return { client, sent };
}

// PRD #11 M4: every emitted run message is logged at debug (the single
// MessageBatcher.emit chokepoint) with a redacted payload, so UZI_LOG_LEVEL=debug
// surfaces the raw frames in `docker logs` without any secret reaching them.
describe("MessageBatcher debug logging (PRD #11 M4)", () => {
  const secret = "super-secret-token-abcdef123456";

  it("logs each emitted message at debug with kind, agent, seq and a redacted payload", () => {
    const { logger, lines } = recordingLogger();
    const { client } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 500, logger, makeRedactor([secret]));

    batcher.emit({
      kind: "tool_result",
      agent: "lead",
      payload: { tool_use_id: "tu1", content: `token is ${secret}`, is_error: false },
    });

    const debugLines = lines.filter(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" && (l as { level?: string }).level === "debug" &&
        (l as { msg?: string }).msg === "run event",
    );
    assert.equal(debugLines.length, 1, "exactly one debug run-event line per emit");
    const rec = debugLines[0];
    assert.ok(rec);
    assert.equal(rec["kind"], "tool_result");
    assert.equal(rec["agent"], "lead");
    assert.equal(rec["seq"], 1);

    const serialized = JSON.stringify(rec["payload"]);
    assert.ok(!serialized.includes(secret), "the secret must not survive in the logged payload");
    assert.ok(serialized.includes("***REDACTED***"), "the secret is replaced by the redaction marker");
  });

  it("logs a debug line for every emitted message (one per call)", () => {
    const { logger, lines } = recordingLogger();
    const { client } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 500, logger, makeRedactor([secret]));

    batcher.emit({ kind: "status", agent: "worker", payload: { text: "worktree ready" } });
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "working on it" } });

    const debugLines = lines.filter(
      (l) => !!l && typeof l === "object" && (l as { msg?: string }).msg === "run event",
    );
    assert.equal(debugLines.length, 2);
  });
});
