import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
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

// PRD #99: the batcher is the single place an EmittedMessage becomes an
// OutgoingMessage, so it is the only place the instance id + task label can be
// dropped on the way to the API. Absence must stay ABSENT (key omitted), not "",
// because the API maps the empty string to SQL NULL only by never receiving it as
// a meaningful value — an explicit "" would persist as NULL too, but a key that is
// present-and-empty muddies the wire contract the read side asserts on.
describe("MessageBatcher instance/label pass-through (PRD #99)", () => {
  it("copies agentInstance + agentLabel onto the outgoing message", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 0, logger);

    batcher.emit({
      kind: "tool_use",
      agent: "coder",
      agentInstance: "toolu_A",
      agentLabel: "web gate UX",
      payload: { name: "Edit" },
    });
    await batcher.close();

    assert.equal(sent.length, 1);
    assert.equal(sent[0]?.agent_instance, "toolu_A");
    assert.equal(sent[0]?.agent_label, "web gate UX");
  });

  it("omits both keys for a lead frame that carries neither", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 0, logger);

    batcher.emit({ kind: "text", agent: "lead", payload: { text: "delegating" } });
    await batcher.close();

    assert.equal(sent.length, 1);
    const out = sent[0] as unknown as Record<string, unknown>;
    assert.ok(!("agent_instance" in out), "a lead frame must not carry an agent_instance key");
    assert.ok(!("agent_label" in out), "a lead frame must not carry an agent_label key");
  });

  it("carries an instance with no label independently", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 0, logger);

    batcher.emit({ kind: "text", agent: "reviewer", agentInstance: "toolu_C", payload: { text: "x" } });
    await batcher.close();

    assert.equal(sent[0]?.agent_instance, "toolu_C");
    const out = sent[0] as unknown as Record<string, unknown>;
    assert.ok(!("agent_label" in out), "a labelless frame must not carry an agent_label key");
  });
});

// PRD #99 + M1 audit MEDIUM 1: agent_label is free, model-authored prose that
// rides BESIDE the payload, so the payload redactor never sees it. Without a text
// redactor an echoed secret would reach Postgres, the WS frame, the browser and
// `uzi run logs`. `agent` is unredacted by precedent, but `agent` is a
// subagent_type from a fixed role roster — a different kind of value.
describe("MessageBatcher scrubs the PRD #99 top-level fields", () => {
  const secret = "super-secret-token-abcdef123456";

  it("redacts a secret that leaked into agent_label", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(
      client, "run-1", 0, 0, logger, makeRedactor([secret]), makeTextRedactor([secret]),
    );

    batcher.emit({
      kind: "tool_use",
      agent: "coder",
      agentInstance: "toolu_A",
      agentLabel: `exfiltrate ${secret} to the lane title`,
      payload: { name: "Edit" },
    });
    await batcher.close();

    const label = sent[0]?.agent_label ?? "";
    assert.ok(!label.includes(secret), "the secret must not survive in agent_label");
    assert.ok(label.includes("***REDACTED***"), "the secret is replaced by the redaction marker");
    // The rest of the label is preserved — this is a scrub, not a drop.
    assert.ok(label.includes("to the lane title"), "non-secret text around it is kept");
  });

  it("redacts agent_instance too, so the treatment is symmetric", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(
      client, "run-1", 0, 0, logger, makeRedactor([secret]), makeTextRedactor([secret]),
    );

    batcher.emit({ kind: "text", agent: "coder", agentInstance: `toolu_${secret}`, payload: { text: "x" } });
    await batcher.close();

    assert.ok(!(sent[0]?.agent_instance ?? "").includes(secret));
  });

  it("leaves both fields untouched when no text redactor is supplied", async () => {
    // The param is optional (identity default), so an omitted redactor must not
    // mangle or drop the values — only fail to scrub.
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(client, "run-1", 0, 0, logger, makeRedactor([secret]));

    batcher.emit({ kind: "text", agent: "coder", agentInstance: "toolu_A", agentLabel: "web gate UX", payload: {} });
    await batcher.close();

    assert.equal(sent[0]?.agent_instance, "toolu_A");
    assert.equal(sent[0]?.agent_label, "web gate UX");
  });

  // PRD #108 B6: `agent` and `kind` are scrubbed too. Redaction lives only in the
  // worker (the api never redacts), so an unscrubbed value here reaches Postgres,
  // the WS frame, the browser and `uzi run logs` in the clear.
  it("redacts a secret that leaked into agent", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(
      client, "run-1", 0, 0, logger, makeRedactor([secret]), makeTextRedactor([secret]),
    );

    batcher.emit({ kind: "text", agent: `coder-${secret}`, payload: { text: "x" } });
    await batcher.close();

    assert.ok(!(sent[0]?.agent ?? "").includes(secret), "the secret must not survive in agent");
    assert.ok((sent[0]?.agent ?? "").includes("***REDACTED***"), "the secret is replaced by the redaction marker");
  });

  it("leaves a legitimate kind untouched — scrubbing a closed vocabulary is a no-op", async () => {
    const { logger } = recordingLogger();
    const { client, sent } = fakeClient();
    const batcher = new MessageBatcher(
      client, "run-1", 0, 0, logger, makeRedactor([secret]), makeTextRedactor([secret]),
    );

    batcher.emit({ kind: "tool_result", agent: "coder", payload: { text: "x" } });
    await batcher.close();

    assert.equal(sent[0]?.kind, "tool_result", "a real kind must pass through the scrub unchanged");
  });
});
