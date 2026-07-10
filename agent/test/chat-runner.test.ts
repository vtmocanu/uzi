import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { ChatExecutor, UZI_SRC_DIR, type SdkQueryFn } from "../src/chat-executor.js";
import { ChatRunner, type ChatClaim } from "../src/chat-runner.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// The ChatRunner is exercised with a fake in-memory client (state + message
// captures) and the real ChatExecutor behind a fake queryFn — no HTTP, no live
// session. The load-bearing property proved here is that a chat is claimed →
// answered → completed with NO git collaborator anywhere in the runner (no clone,
// no worktree, no push, no MR) and NO forge PAT on the claim (Decision 9/13).

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const JOIN = "dummy-join-token-do-not-scan-2222";

const DEFAULTS = { maxTurns: 50, turnTimeoutMs: 100_000, idleTimeoutMs: 100_000 };

interface FakeClient {
  states: StateRequest[];
  messages: OutgoingMessage[];
  client: WorkerClient;
}
function fakeClient(): FakeClient {
  const states: StateRequest[] = [];
  const messages: OutgoingMessage[] = [];
  const client = {
    async reportState(_runId: string, body: StateRequest): Promise<void> {
      states.push(body);
    },
    async postMessages(_runId: string, batch: OutgoingMessage[]): Promise<void> {
      messages.push(...batch);
    },
  } as unknown as WorkerClient;
  return { states, messages, client };
}

function assistantText(text: string, sid = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sid, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function resultSuccess(sid = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, session_id: sid } as unknown as SDKMessage;
}

/** A fake query that replays [assistantText, resultSuccess] per turn and records
 *  the options each turn was driven with. */
function fakeQuery(): { queryFn: SdkQueryFn; options: SdkOptions[] } {
  const options: SdkOptions[] = [];
  const queryFn: SdkQueryFn = (params) => {
    options.push(params.options);
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      yield assistantText("answer");
      yield resultSuccess();
    })();
  };
  return { queryFn, options };
}

/** Feed a fixed list of user messages, then undefined (idle-complete). */
function messageQueue(msgs: string[]): () => Promise<string | undefined> {
  const q = [...msgs];
  return () => Promise.resolve<string | undefined>(q.length ? q.shift() : undefined);
}

function baseClaim(overrides: Partial<ChatClaim> = {}): ChatClaim {
  return {
    run_id: "chat-run-1",
    kind: "chat",
    last_seq: 0,
    secrets: { anthropic_oauth_token: OAUTH },
    ...overrides,
  };
}

let homeDir: string;
beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-chatrunner-"));
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
});

describe("ChatRunner — claim → session loop → complete (no clone, no MR)", () => {
  it("reports running then completed and drives the executor against the baked source", async () => {
    const { states, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    const executor = new ChatExecutor(nullLogger(), homeDir, { queryFn });
    const runner = new ChatRunner(client, executor, nullLogger(), 5, DEFAULTS, JOIN, {
      nextUserMessage: messageQueue(["how does the plan-approval gate work?"]),
    });

    await runner.execute(baseClaim());

    // No failed state; ends completed. (An intermediate running report from the
    // session-id pin may appear between the two.)
    assert.strictEqual(states[0]!.status, "running");
    assert.strictEqual(states.at(-1)!.status, "completed");
    assert.ok(!states.some((s) => s.status === "failed"));
    // Nothing branch/MR-shaped was ever reported — a chat produces no branch.
    assert.ok(!states.some((s) => "branch" in s || "mr_iid" in s));
    // The one turn ran with cwd = the baked read-only source, never a worktree.
    assert.strictEqual(options.length, 1);
    assert.strictEqual(options[0]!.cwd, UZI_SRC_DIR);
    // The session id was pinned for resume.
    assert.ok(states.some((s) => s.session_id === "sess-1"));
  });

  it("carries NO forge PAT on a chat claim (Decision 9) and has no git collaborator", async () => {
    const { client } = fakeClient();
    const { queryFn } = fakeQuery();
    const runner = new ChatRunner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), nullLogger(), 5, DEFAULTS, JOIN, {
      nextUserMessage: messageQueue(["q"]),
    });
    const claim = baseClaim();
    // The claim's secrets are exactly the Anthropic token — no forge_pat/forge_username.
    assert.deepStrictEqual(Object.keys(claim.secrets), ["anthropic_oauth_token"]);
    // The ChatRunner constructor takes no GitCache / GitLabClient — clone is
    // structurally impossible. This just proves execute() completes without one.
    await runner.execute(claim);
  });

  it("prefers the claim's chat_max_turns over the worker default (turn cap)", async () => {
    const { messages, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    const runner = new ChatRunner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), nullLogger(), 5, DEFAULTS, JOIN, {
      // endless supply — only the per-run cap of 1 stops the loop
      nextUserMessage: () => Promise.resolve<string | undefined>("more"),
    });

    await runner.execute(baseClaim({ config: { chat_max_turns: 1 } }));

    assert.strictEqual(options.length, 1, "exactly one turn — the per-run cap won over the default 50");
    assert.ok(messages.some((m) => m.kind === "status" && /turn limit/.test(String(m.payload["text"]))));
  });

  it("says so honestly when a Continue cannot resume the prior session (Decision 11)", async () => {
    const { messages, client } = fakeClient();
    const { queryFn } = fakeQuery();
    const runner = new ChatRunner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), nullLogger(), 5, DEFAULTS, JOIN, {
      nextUserMessage: messageQueue([]),
    });

    await runner.execute(baseClaim({ resume_of_run_id: "prior-run", session_id: null }));

    assert.ok(messages.some((m) => m.kind === "status" && /without its earlier context/.test(String(m.payload["text"]))));
  });

  it("reports failed with a reason when the executor throws (no token)", async () => {
    const { states, client } = fakeClient();
    const { queryFn } = fakeQuery();
    const runner = new ChatRunner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), nullLogger(), 5, DEFAULTS, JOIN, {
      nextUserMessage: messageQueue(["q"]),
    });

    await runner.execute(baseClaim({ secrets: {} }));

    const failed = states.find((s) => s.status === "failed");
    assert.ok(failed, "a failed state was reported");
    assert.match(String(failed!.failure_reason), /no Anthropic OAuth token/);
  });
});
