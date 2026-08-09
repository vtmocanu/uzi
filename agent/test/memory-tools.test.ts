import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  makeMemoryToolHandlers,
  buildMemoryServer,
  memoryToolNames,
  MEMORY_SERVER_NAME,
  MEMORY_TITLE_MAX_BYTES,
  MEMORY_BODY_MAX_BYTES,
  MEMORY_EVIDENCE_MAX_BYTES,
} from "../src/memory-tools.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import type { MemoryEntry, SaveMemoryRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// save_memory is exercised through its raw handler (makeMemoryToolHandlers) with a
// fake WorkerClient — no live SDK, no HTTP. Proves: a valid call POSTs the trimmed
// {title, body} for the CURRENT run id; the size caps are enforced CLIENT-side (a
// clear tool error, never a throw, and no network call); and a 429/409/400 from the
// server is surfaced as a concise NON-FATAL tool message (PRD #90 M2).

interface Calls {
  saveMemory: Array<{ runId: string; body: SaveMemoryRequest }>;
}
function fakeClient(over: { entry?: MemoryEntry; throws?: unknown } = {}): { client: WorkerClient; calls: Calls } {
  const calls: Calls = { saveMemory: [] };
  const client = {
    async saveMemory(runId: string, body: SaveMemoryRequest): Promise<MemoryEntry> {
      calls.saveMemory.push({ runId, body });
      if (over.throws) throw over.throws;
      return (
        over.entry ?? {
          id: "mem-1",
          title: body.title,
          body: body.body,
          created_at: "2026-07-19T00:00:00Z",
        }
      );
    },
  } as unknown as WorkerClient;
  return { client, calls };
}

function handlers(client: WorkerClient, runId = "run-current") {
  return makeMemoryToolHandlers({ client, runId, log: nullLogger() });
}
const bodyText = (r: { content: { text: string }[] }): string => r.content[0]!.text;

describe("save_memory handler (PRD #90 M2)", () => {
  it("POSTs the trimmed {title, body} for the CURRENT run id and confirms", async () => {
    const { client, calls } = fakeClient({
      entry: { id: "mem-9", title: "gcc baked in", body: "no build-essential needed", created_at: "2026-07-19T00:00:00Z" },
    });
    const res = await handlers(client, "run-current").saveMemory({ title: "  gcc baked in  ", body: "no build-essential needed" });
    assert.strictEqual(calls.saveMemory.length, 1);
    assert.deepStrictEqual(calls.saveMemory[0], {
      runId: "run-current",
      body: { title: "gcc baked in", body: "no build-essential needed", basis: "inferred", evidence: undefined },
    });
    assert.notStrictEqual(res.isError, true);
    assert.match(bodyText(res), /Saved cross-run memory "gcc baked in"/);
    assert.match(bodyText(res), /mem-9/);
    assert.match(bodyText(res), /advisory/i);
  });

  it("defaults an OMITTED basis to `inferred` and never returns isError (PRD #266 M2)", async () => {
    const { client, calls } = fakeClient();
    const res = await handlers(client).saveMemory({ title: "t", body: "a durable fact" });
    assert.strictEqual(calls.saveMemory.length, 1);
    assert.strictEqual(calls.saveMemory[0]!.body.basis, "inferred", "omitted basis defaults to inferred");
    assert.strictEqual(calls.saveMemory[0]!.body.evidence, undefined, "omitted evidence stays undefined");
    assert.notStrictEqual(res.isError, true, "defaulting basis is never a hard failure (PRD #90)");
  });

  it("round-trips an `observed` basis and its evidence to the client (PRD #266 M2)", async () => {
    const { client, calls } = fakeClient();
    const res = await handlers(client).saveMemory({
      title: "coder can edit",
      body: "the coder subagent inherits Edit/Write on the implement turn",
      basis: "observed",
      evidence: "agent/src/agents.ts:27-37",
    });
    assert.strictEqual(calls.saveMemory.length, 1);
    assert.strictEqual(calls.saveMemory[0]!.body.basis, "observed");
    assert.strictEqual(calls.saveMemory[0]!.body.evidence, "agent/src/agents.ts:27-37");
    assert.notStrictEqual(res.isError, true);
  });

  it("normalizes empty/whitespace evidence to undefined (PRD #266 M2)", async () => {
    const { client, calls } = fakeClient();
    const res = await handlers(client).saveMemory({ title: "t", body: "a durable fact", basis: "observed", evidence: "   " });
    assert.strictEqual(calls.saveMemory.length, 1);
    assert.strictEqual(calls.saveMemory[0]!.body.evidence, undefined, "whitespace evidence normalizes to undefined");
    assert.notStrictEqual(res.isError, true);
  });

  it("rejects over-cap evidence by BYTES with a clear non-fatal tool error and NO network call (PRD #266 M2)", async () => {
    const { client, calls } = fakeClient();
    // 101 chars of a 2-byte code point = 202 bytes > 200, but only 101 chars —
    // proves the evidence cap is byte-measured, matching title/body.
    const evidence = "é".repeat(101);
    assert.ok(evidence.length <= MEMORY_EVIDENCE_MAX_BYTES, "under the byte cap by char count");
    assert.ok(Buffer.byteLength(evidence, "utf8") > MEMORY_EVIDENCE_MAX_BYTES, "over the byte cap by byte count");
    const res = await handlers(client).saveMemory({ title: "t", body: "b", basis: "observed", evidence });
    assert.strictEqual(res.isError, true, "over-cap evidence is a non-fatal tool error, not a throw");
    assert.match(bodyText(res), /evidence is too long/);
    assert.strictEqual(calls.saveMemory.length, 0, "no POST on a client-side rejection");
  });

  it("appends a NON-FATAL nudge when the body reads like a volatile snapshot (still saves)", async () => {
    for (const body of ["1156 pass, 0 fail", "1156/1157 green", "1 of 227 suites failed", "3 fail after the fix"]) {
      const { client, calls } = fakeClient();
      const res = await handlers(client).saveMemory({ title: "t", body });
      assert.strictEqual(calls.saveMemory.length, 1, `${body} must still be POSTed`);
      assert.notStrictEqual(res.isError, true, `${body} must NOT be rejected`);
      assert.match(bodyText(res), /Saved cross-run memory/);
      assert.match(bodyText(res), /volatile snapshot/i, `${body} should trigger the nudge`);
    }
  });

  it("does NOT nudge a DIGIT-FREE durable fact (nothing that looks like a snapshot)", async () => {
    for (const body of ["gcc is baked in; no build-essential needed", "set GOFLAGS=-buildvcs=false in linked worktrees"]) {
      const { client } = fakeClient();
      const res = await handlers(client).saveMemory({ title: "t", body });
      assert.notStrictEqual(res.isError, true);
      assert.doesNotMatch(bodyText(res), /volatile snapshot/i, `${body} must not trigger the nudge`);
    }
  });

  it("nudges an ACCEPTED false positive: a legit numeric durable fact still gets warned (never rejected)", async () => {
    // The heuristic is warn-only by design (memory-tools.ts): it fires on any body
    // wearing a snapshot shape, INCLUDING durable numeric facts — an "of <N>" phrase,
    // a CIDR, a date. Documenting that here keeps the test honest: the nudge is
    // over-broad on purpose, and the memory is still saved regardless.
    for (const body of ["worker idle timeout of 120000 ms is the wedged-daemon ceiling", "the pod network is 10.0.0.0/24", "chromium pinned since 2026/08"]) {
      const { client, calls } = fakeClient();
      const res = await handlers(client).saveMemory({ title: "t", body });
      assert.strictEqual(calls.saveMemory.length, 1, `${body} must still be POSTed`);
      assert.notStrictEqual(res.isError, true, `${body} must NOT be rejected — warn-only`);
      assert.match(bodyText(res), /volatile snapshot/i, `${body} is an accepted false positive and should still nudge`);
    }
  });

  it("appends a NON-FATAL config-claim nudge when the body asserts subagent tool/roster config (still saves) (PRD #266 M4)", async () => {
    // Each body asserts a roster subject's write/tool CAPABILITY in a possession or
    // negation frame — the run's own config, which should be read live, not remembered.
    const positives = [
      "the coder subagent has no Edit/Write",
      "coder lacks Edit and Write tools; it edits via Bash",
      "the reviewer subagent is read-only and cannot edit files",
      // Affirmative-capability config claim: still a runtime-config fact to read live.
      "the coder inherits all tools including Edit and Write on the implement turn",
      // Directly-framed capability phrases via copula (is/are) — the incident class.
      "the coder is read-only",
      "the reviewer is write-capable",
      "the coder subagent is read-only",
    ];
    for (const body of positives) {
      const { client, calls } = fakeClient();
      const res = await handlers(client).saveMemory({ title: "t", body });
      assert.strictEqual(calls.saveMemory.length, 1, `${body} must still be POSTed`);
      assert.notStrictEqual(res.isError, true, `${body} must NOT be rejected — advisory only`);
      assert.match(bodyText(res), /Saved cross-run memory/);
      assert.match(bodyText(res), /read that live from the per-turn roster/i, `${body} should trigger the config-claim nudge`);
    }
  });

  it("does NOT nudge when a roster subject and a filename/tool merely co-occur without a capability assertion (PRD #266 M4)", async () => {
    const negatives = [
      // The PRD near-miss: subagent name + filenames, no capability assertion. MUST NOT fire.
      "when adding a forge driver the coder must update forge.ts and protocol.ts",
      "the coder should run the project's tests before reporting done",
      // Mentions the tools, but no subagent-capability assertion (no role subject).
      "use the Edit and Write tools carefully when refactoring the store package",
      // A memory ABOUT this feature: role + roster, but asserts no tool possession/negation.
      "the lead roster now annotates each subagent — see prompt.ts delegatesLine",
      // Copula + role but NO tool/capability token within range — must stay quiet.
      "the coder is responsible for running tests before reporting done",
      "the reviewer is slow to respond on large diffs",
      // "writer" must not match the \\bWrite\\b tool token.
      "the architect is a great writer of design docs, never source code",
    ];
    for (const body of negatives) {
      const { client, calls } = fakeClient();
      const res = await handlers(client).saveMemory({ title: "t", body });
      assert.strictEqual(calls.saveMemory.length, 1, `${body} must still be POSTed`);
      assert.notStrictEqual(res.isError, true);
      assert.doesNotMatch(bodyText(res), /read that live from the per-turn roster/i, `${body} must not trigger the config-claim nudge`);
    }
  });

  it("appends BOTH nudges when a body matches the snapshot AND the config-claim shapes (PRD #266 M4)", async () => {
    const { client, calls } = fakeClient();
    // "3 of 5" trips the snapshot regex; "coder ... has no Edit/Write" trips the config regex.
    const body = "3 of 5 turns the coder subagent has no Edit/Write access";
    const res = await handlers(client).saveMemory({ title: "t", body });
    assert.strictEqual(calls.saveMemory.length, 1, "the memory is still saved");
    assert.notStrictEqual(res.isError, true, "neither nudge is a rejection");
    assert.match(bodyText(res), /volatile snapshot/i, "the snapshot nudge appears");
    assert.match(bodyText(res), /read that live from the per-turn roster/i, "the config-claim nudge appears too — one must not clobber the other");
  });

  it("rejects an empty title/body client-side with a tool error and NO network call", async () => {
    const { client, calls } = fakeClient();
    const noTitle = await handlers(client).saveMemory({ title: "   ", body: "x" });
    assert.strictEqual(noTitle.isError, true);
    assert.match(bodyText(noTitle), /non-empty title/);
    const noBody = await handlers(client).saveMemory({ title: "t", body: "   " });
    assert.strictEqual(noBody.isError, true);
    assert.match(bodyText(noBody), /non-empty body/);
    assert.strictEqual(calls.saveMemory.length, 0, "no POST on a client-side rejection");
  });

  it("rejects an over-cap title by BYTES, not chars (multi-byte counts), with NO network call", async () => {
    const { client, calls } = fakeClient();
    // A 101-char string of a 2-byte code point = 202 bytes > 200, but only 101 chars —
    // proves the title cap is byte-measured (matching the server), not char-measured.
    const title = "é".repeat(101);
    assert.ok(title.length <= MEMORY_TITLE_MAX_BYTES, "under the byte cap by char count");
    assert.ok(Buffer.byteLength(title, "utf8") > MEMORY_TITLE_MAX_BYTES, "over the byte cap by byte count");
    const res = await handlers(client).saveMemory({ title, body: "b" });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /title is too long/);
    assert.strictEqual(calls.saveMemory.length, 0);
  });

  it("rejects an over-cap body by BYTES, not chars (multi-byte counts), with NO network call", async () => {
    const { client, calls } = fakeClient();
    // A 1025-char string of a 2-byte code point = 2050 bytes > 2048, but only 1025
    // chars — proves the cap is byte-measured, not char-measured.
    const body = "é".repeat(1025);
    assert.ok(body.length <= MEMORY_BODY_MAX_BYTES, "under the byte cap by char count");
    assert.ok(Buffer.byteLength(body, "utf8") > MEMORY_BODY_MAX_BYTES, "over the byte cap by byte count");
    const res = await handlers(client).saveMemory({ title: "t", body });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /body is too long/);
    assert.strictEqual(calls.saveMemory.length, 0);
  });

  it("surfaces the per-run write cap (429) as a concise, non-fatal tool message", async () => {
    const { client } = fakeClient({ throws: new RequestError("POST", "/x", 429, "write cap reached") });
    const res = await handlers(client).saveMemory({ title: "t", body: "b" });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /reached its save_memory limit/);
    assert.match(bodyText(res), /Do not retry/i);
  });

  it("surfaces a repo-less run (409) as a non-fatal tool message", async () => {
    const { client } = fakeClient({ throws: new RequestError("POST", "/x", 409, "run has no repo") });
    const res = await handlers(client).saveMemory({ title: "t", body: "b" });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /not associated with a repository/);
  });

  it("surfaces a server 400 (empty/oversize) as a non-fatal tool message carrying the server body", async () => {
    const { client } = fakeClient({ throws: new RequestError("POST", "/x", 400, "title too long") });
    const res = await handlers(client).saveMemory({ title: "t", body: "b" });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /Memory not saved: title too long/);
  });

  it("never throws on a transport error — returns a non-fatal message so the run continues", async () => {
    const { client } = fakeClient({ throws: new Error("network down") });
    const res = await handlers(client).saveMemory({ title: "t", body: "b" });
    assert.strictEqual(res.isError, true);
    assert.match(bodyText(res), /network down/);
  });
});

describe("save_memory wiring", () => {
  it("exposes the qualified tool name under the `memory` server (mcp__memory__save_memory)", () => {
    assert.strictEqual(MEMORY_SERVER_NAME, "memory");
    assert.deepStrictEqual(memoryToolNames(), ["mcp__memory__save_memory"]);
  });

  it("buildMemoryServer returns the server, tool names, and handlers over the same deps", () => {
    const { client } = fakeClient();
    const built = buildMemoryServer({ client, runId: "run-current", log: nullLogger() });
    assert.ok(built.server, "an MCP server config is returned");
    assert.deepStrictEqual(built.toolNames, ["mcp__memory__save_memory"]);
    assert.strictEqual(typeof built.handlers.saveMemory, "function");
  });
});
