import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { ChatExecutor, UZI_SRC_DIR, type SdkQueryFn } from "../src/chat-executor.js";
import { ChatRunner, type ChatRunnerDefaults } from "../src/chat-runner.js";
import type { ChatInput, ChatInputSource } from "../src/steering.js";
import type { WorkerClient } from "../src/client.js";
import type { ChatClaimResponse, OutgoingMessage, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// The ChatRunner is exercised with a fake in-memory client (state + message
// captures), a fake input source (scripted ChatInputs — the ChatSteering channel is
// unit-tested separately), and the real ChatExecutor behind a fake queryFn: no HTTP,
// no live session. Proves claim → answer → complete with NO git collaborator (no
// clone/worktree/push/MR), NO forge PAT (Decision 9), one user_message emitted per
// consumed input before the model's reply, and claim-config-wins clock resolution.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const JOIN = "dummy-join-token-do-not-scan-2222";

const DEFAULTS: ChatRunnerDefaults = { maxTurns: 50, turnTimeoutMs: 100_000, idleTimeoutMs: 100_000, pollMs: 1000 };

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

/** A fake input source that yields scripted ChatInputs, then `ended`. */
function fakeSource(script: ChatInput[]): ChatInputSource {
  const q = [...script];
  return {
    start() {},
    async stop() {},
    async awaitFollowUp() {
      return q.length ? q.shift()! : { kind: "ended" };
    },
  };
}
const msg = (text: string): ChatInput => ({ kind: "message", text });

function runner(client: WorkerClient, executor: ChatExecutor, source: ChatInputSource, defaults = DEFAULTS): ChatRunner {
  // Wrap the single executor as a per-session factory (PRD #42): each execute()
  // gets it back. Every test here drives a single session, so one instance is fine.
  return new ChatRunner(client, () => executor, nullLogger(), 5, defaults, JOIN, { makeSource: () => source });
}

function baseClaim(overrides: Partial<ChatClaimResponse> = {}): ChatClaimResponse {
  return {
    run_id: "chat-run-1",
    kind: "chat",
    title: "",
    status: "claimed",
    session_id: null,
    resume_of_run_id: null,
    last_seq: 0,
    requeue_count: 0,
    secrets: { anthropic_oauth_token: OAUTH },
    config: { idle_timeout_seconds: 3600, turn_timeout_seconds: 600, max_turns: 50 },
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
  it("reports running then completed, drives the baked source, and emits the user_message first", async () => {
    const { states, messages, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    const executor = new ChatExecutor(nullLogger(), homeDir, { queryFn });
    await runner(client, executor, fakeSource([msg("how does the plan-approval gate work?"), { kind: "idle" }])).execute(baseClaim());

    assert.strictEqual(states[0]!.status, "running");
    assert.strictEqual(states.at(-1)!.status, "completed");
    assert.ok(!states.some((s) => s.status === "failed"));
    assert.ok(!states.some((s) => "branch" in s || "mr_iid" in s), "a chat produces no branch/MR");
    assert.strictEqual(options.length, 1);
    assert.strictEqual(options[0]!.cwd, UZI_SRC_DIR, "the turn ran against the baked read-only source");
    assert.ok(states.some((s) => s.session_id === "sess-1"), "session id pinned for resume");

    // The user_message is emitted (payload.text, no agent → user bubble) and lands
    // BEFORE the model's reply for that turn (worker owns the gapless seq).
    const userMsg = messages.find((m) => m.kind === "user_message");
    assert.ok(userMsg, "a user_message was emitted");
    assert.strictEqual((userMsg!.payload as { text?: string }).text, "how does the plan-approval gate work?");
    assert.strictEqual(userMsg!.agent, undefined, "user_message carries no agent (rendered as a user bubble)");
    const answer = messages.find((m) => m.kind === "text");
    assert.ok(answer && userMsg!.seq < answer.seq, "user_message precedes the model reply");
  });

  it("carries NO forge PAT on a chat claim (Decision 9) and has no git collaborator", async () => {
    const { client } = fakeClient();
    const { queryFn } = fakeQuery();
    const claim = baseClaim();
    assert.deepStrictEqual(Object.keys(claim.secrets), ["anthropic_oauth_token"]);
    // The ChatRunner constructor takes no GitCache / GitLabClient — clone is
    // structurally impossible; this proves execute() completes without one.
    await runner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q"), { kind: "idle" }])).execute(claim);
  });

  it("prefers the claim's max_turns over the worker default (turn cap)", async () => {
    const { messages, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    // Endless supply of messages — only the per-run cap of 1 stops the loop.
    const endless: ChatInputSource = { start() {}, async stop() {}, async awaitFollowUp() { return msg("more"); } };
    await runner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), endless).execute(
      baseClaim({ config: { idle_timeout_seconds: 3600, turn_timeout_seconds: 600, max_turns: 1 } }),
    );
    assert.strictEqual(options.length, 1, "exactly one turn — the per-run cap won over the default 50");
    assert.ok(messages.some((m) => m.kind === "status" && /turn limit/.test(String(m.payload["text"]))));
  });

  it("says so honestly when a Continue cannot resume the prior session (Decision 11)", async () => {
    const { messages, client } = fakeClient();
    const { queryFn } = fakeQuery();
    await runner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([{ kind: "idle" }])).execute(
      baseClaim({ resume_of_run_id: "prior-run", session_id: null }),
    );
    assert.ok(messages.some((m) => m.kind === "status" && /without its earlier context/.test(String(m.payload["text"]))));
  });

  it("resumes the claim's session id when present (own or resumed-from)", async () => {
    const { states, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    await runner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q"), { kind: "idle" }])).execute(
      baseClaim({ session_id: "prior-session" }),
    );
    assert.strictEqual(options[0]!.resume, "prior-session", "the first turn resumes the claim's session");
    // The claim's session id is carried on the very first state report (before any turn).
    assert.strictEqual(states[0]!.session_id, "prior-session");
  });

  it("reports failed with a reason when the executor throws (no token)", async () => {
    const { states, client } = fakeClient();
    const { queryFn } = fakeQuery();
    await runner(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q")])).execute(
      baseClaim({ secrets: { anthropic_oauth_token: "" } }),
    );
    const failed = states.find((s) => s.status === "failed");
    assert.ok(failed, "a failed state was reported");
    assert.match(String(failed!.failure_reason), /no Anthropic OAuth token/);
  });
});

describe("ChatRunner — per-session executor isolation (PRD #42 Decision 4)", () => {
  it("builds a NEW ChatExecutor per session (its spawnedPids are not shared)", async () => {
    const { client } = fakeClient();
    const built: ChatExecutor[] = [];
    // A per-session factory (what main.ts wires) — one fresh executor per execute().
    const factory = (): ChatExecutor => {
      const { queryFn } = fakeQuery();
      const e = new ChatExecutor(nullLogger(), homeDir, { queryFn });
      built.push(e);
      return e;
    };
    const rnr = new ChatRunner(client, factory, nullLogger(), 5, DEFAULTS, JOIN, {
      makeSource: () => fakeSource([msg("hi"), { kind: "idle" }]),
    });
    await rnr.execute(baseClaim({ run_id: "chat-a" }));
    await rnr.execute(baseClaim({ run_id: "chat-b" }));

    assert.strictEqual(built.length, 2, "one ChatExecutor built per session");
    assert.notStrictEqual(built[0], built[1], "each session gets its OWN instance (private spawnedPids)");
  });
});

// ── Resume preflight (issue #105) ────────────────────────────────────────────
//
// Chat shares ONE SDK HOME across sessions by design (a Continue is a new run id
// resuming the same session, so a per-run HOME would file the transcript under the new
// id and break resume outright). That sidesteps the run lane's per-run HOME hazard but
// NOT the cross-worker one: the server copies the prior run's session id onto a
// Continue unconditionally (GetChatRunClaimContext) and ClaimChatRun hands the run to
// any of the user's workers once the affinity grace lapses.
//
// Before this, the Decision 11 honesty message was gated on `!claim.session_id`, so on
// a cross-worker Continue — the exact case its comment described — it stayed silent
// while the id pointed at another machine's disk. Every turn then died with
// `error_during_execution`, and because an errored chat turn parks for the next message
// instead of ending, the conversation was bricked for good.
describe("ChatRunner — resume preflight (issue #105)", () => {
  const SID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

  function runnerWithHome(
    client: WorkerClient,
    executor: ChatExecutor,
    source: ChatInputSource,
    sdkHomeDir?: string,
  ): ChatRunner {
    return new ChatRunner(client, () => executor, nullLogger(), 5, DEFAULTS, JOIN, {
      makeSource: () => source,
      ...(sdkHomeDir ? { sdkHomeDir } : {}),
    });
  }

  it("drops an unresolvable session and reaches the Decision 11 message it could not reach before", async () => {
    const { messages, states, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    // homeDir is a fresh tmpdir with no .claude/projects — a worker that never held
    // this session, which is what a cross-worker Continue lands on.
    await runnerWithHome(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q"), { kind: "idle" }]), homeDir)
      .execute(baseClaim({ resume_of_run_id: "prior-run", session_id: SID }));

    assert.strictEqual(options[0]!.resume, undefined, "a dead session id must not be passed to the SDK");
    assert.ok(
      messages.some((m) => m.kind === "status" && /without its earlier context/.test(String(m.payload["text"]))),
      "the Continue must admit it lost the context",
    );
    // And the dead id is not carried back to the server as this run's resume target.
    assert.strictEqual(states[0]!.session_id, undefined);
  });

  it("keeps a session whose transcript IS on this worker", async () => {
    const { messages, client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    const dir = path.join(homeDir, ".claude", "projects", "-opt-uzi-src");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, `${SID}.jsonl`), "{}\n");

    await runnerWithHome(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q"), { kind: "idle" }]), homeDir)
      .execute(baseClaim({ resume_of_run_id: "prior-run", session_id: SID }));

    assert.strictEqual(options[0]!.resume, SID, "a resolvable session still resumes");
    assert.ok(
      !messages.some((m) => m.kind === "status" && /without its earlier context/.test(String(m.payload["text"]))),
      "must not cry wolf on a working Continue",
    );
  });

  it("does not preflight without a configured SDK HOME (the stub lane is untouched)", async () => {
    const { client } = fakeClient();
    const { queryFn, options } = fakeQuery();
    await runnerWithHome(client, new ChatExecutor(nullLogger(), homeDir, { queryFn }), fakeSource([msg("q"), { kind: "idle" }]))
      .execute(baseClaim({ resume_of_run_id: "prior-run", session_id: SID }));
    assert.strictEqual(options[0]!.resume, SID, "no HOME configured ⇒ the claim's id passes through unchanged");
  });
});
