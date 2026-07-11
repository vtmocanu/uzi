import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Writable, Readable } from "node:stream";
import { EventEmitter } from "node:events";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options as SdkOptions, SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { selectSubagents } from "../src/agents.js";
import { buildLeadSystemPrompt, REPO_SUBAGENT_UNTRUSTED_APPEND } from "../src/prompt.js";
import type { AgentTemplate } from "../src/protocol.js";

// PRD #37 M6 — the one must-verify-live check no fake-`queryFn` test can make.
//
// The gate-boundary rebuild (M3) hands the IMPLEMENT turn a NEW agents map + a
// repo-sourced lead system prompt, and that turn RESUMES the plan turn's session
// (`options.resume` set). The M3 tests fake `queryFn`, so they prove the executor
// *passes* those options but NOT that the real SDK *transmits* them on a resumed
// turn — the worry being a "resume guard" that reuses the session's original
// config and silently ignores the swap.
//
// This test drives the REAL SDK `query()` with a shimmed `spawnClaudeCodeProcess`
// (no `claude` binary, no network, no Anthropic session) and captures the JSON the
// SDK writes to the CLI's stdin. The SDK sends its config as an `initialize`
// control_request in response to the CLI announcing itself; the fake CLI emits that
// announcement and nothing else, so the query never advances past init — which is
// all we need. We then assert the transmitted initialize carries the swapped agents
// and the untrusted-review passage EVEN THOUGH `--resume` is on the command line:
// empirical proof there is no resume guard.

interface FakeProc {
  captured: string[];
  stdout: Readable;
  spawnArgs: string[];
  proc: Record<string, unknown>;
}

/** A minimal SpawnedProcess that captures stdin and, on request, plays the CLI's
 *  `initialize` control_request so the SDK writes its config back. */
function fakeCli(): FakeProc {
  const captured: string[] = [];
  const stdin = new Writable({
    write(chunk, _enc, cb) {
      captured.push(chunk.toString("utf8"));
      cb();
    },
  });
  const stdout = new Readable({ read() {} });
  const stderr = new Readable({ read() {} });
  const ee = new EventEmitter();
  const proc: Record<string, unknown> = {
    pid: 4242,
    stdin,
    stdout,
    stderr,
    killed: false,
    exitCode: null,
    kill: () => true,
    on: (...a: [string, (...x: unknown[]) => void]) => (ee.on(...a), proc),
    once: (...a: [string, (...x: unknown[]) => void]) => (ee.once(...a), proc),
    off: (...a: [string, (...x: unknown[]) => void]) => (ee.off(...a), proc),
  };
  return { captured, stdout, spawnArgs: [], proc };
}

/** Parse the SDK's `initialize` control_request out of what it wrote to stdin. */
function findInitialize(chunks: string[]): Record<string, unknown> | undefined {
  for (const line of chunks.join("").split("\n")) {
    if (!line.trim()) continue;
    let msg: { type?: string; request?: { subtype?: string } };
    try {
      msg = JSON.parse(line);
    } catch {
      continue;
    }
    if (msg.type === "control_request" && msg.request?.subtype === "initialize") {
      return msg.request as Record<string, unknown>;
    }
  }
  return undefined;
}

const repoCoder: AgentTemplate = { name: "coder", description: "repo coder", prompt_body: "REPO CODER BODY", tools: ["Read", "Edit", "Bash"] };
const repoReviewer: AgentTemplate = { name: "reviewer", description: "repo reviewer", prompt_body: "REPO REVIEWER BODY", tools: ["Read", "Grep"] };
const repoAuditor: AgentTemplate = { name: "auditor", description: "repo auditor", prompt_body: "REPO AUDITOR BODY" };

describe("SDK honors a swapped agents map + repo-sourced prompt on a RESUMED turn (PRD #37 M6 capstone)", () => {
  it("transmits the implement roster and the untrusted-review passage even with resume set", async () => {
    // The exact shape sdk-executor builds for the implement turn: the repo roster
    // (auditor excluded) via the real selectSubagents, and the repo-sourced lead
    // prompt via the real buildLeadSystemPrompt.
    const agents = selectSubagents("repo", {}, [repoCoder, repoReviewer, repoAuditor], ["auditor"]);
    const systemPrompt = buildLeadSystemPrompt("LEAD BODY", { repoSourced: true });
    assert.deepEqual(Object.keys(agents).sort(), ["coder", "reviewer"], "sanity: auditor excluded");

    const fake = fakeCli();
    const abort = new AbortController();

    // Streaming-input prompt kept open so the query does not self-close before init.
    async function* prompt() {
      yield { type: "user", message: { role: "user", content: "implement" }, parent_tool_use_id: null, session_id: "s" } as never;
      await new Promise((r) => setTimeout(r, 5000));
    }

    const options: SdkOptions = {
      abortController: abort,
      resume: "plan-turn-session-id", // the implement turn RESUMES the plan turn
      systemPrompt,
      agents,
      spawnClaudeCodeProcess: (opts: SpawnOptions) => {
        fake.spawnArgs = opts.args;
        // The CLI announces itself; the SDK replies with the initialize config.
        setTimeout(() => {
          fake.stdout.push(JSON.stringify({ type: "control_request", request_id: "cli-init", request: { subtype: "initialize" } }) + "\n");
        }, 40);
        return fake.proc as never;
      },
    };

    const q = query({ prompt: prompt(), options });
    // Drain in the background; it never completes (the fake CLI sends no result),
    // so we abort once we have the captured init.
    const drain = (async () => {
      try {
        for await (const _ of q) void _;
      } catch {
        /* aborted — expected */
      }
    })();

    const start = Date.now();
    let init: Record<string, unknown> | undefined;
    while (Date.now() - start < 5000) {
      init = findInitialize(fake.captured);
      if (init) break;
      await new Promise((r) => setTimeout(r, 20));
    }

    try {
      assert.ok(init, "the SDK must write an initialize control_request to the CLI");

      // 1. Resume IS on the command line — this is genuinely a resumed turn.
      assert.ok(fake.spawnArgs.includes("--resume"), "spawn args must carry --resume");
      assert.equal(fake.spawnArgs[fake.spawnArgs.indexOf("--resume") + 1], "plan-turn-session-id");

      // 2. …yet the SWAPPED agents map is transmitted: exactly the selected repo
      //    subagents, and NOT the excluded one.
      const sentAgents = init.agents as Record<string, unknown> | undefined;
      assert.ok(sentAgents, "initialize must carry an agents map");
      assert.deepEqual(Object.keys(sentAgents).sort(), ["coder", "reviewer"], "swapped roster transmitted on resume");
      assert.ok(!("auditor" in sentAgents), "an excluded agent must not reach the resumed turn");

      // 3. …and the repo-sourced lead system prompt rode along (the untrusted-review
      //    passage appears in the transmitted config, wherever the SDK places the
      //    preset append).
      const raw = JSON.stringify(init);
      assert.ok(
        raw.includes(REPO_SUBAGENT_UNTRUSTED_APPEND.slice(0, 60)),
        "the repo-sourced untrusted-review passage must be transmitted on resume",
      );
    } finally {
      abort.abort();
      try {
        await q.return?.(undefined as never);
      } catch {
        /* best-effort */
      }
      await drain;
    }
  });
});
