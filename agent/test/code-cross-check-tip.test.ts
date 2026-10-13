import assert from "node:assert/strict";
import { it } from "node:test";
import { checkCode } from "../src/code-cross-check-gate.js";
import { RequestError, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { decodeCodeCrossCheckStatus, decodeCrossCheckClaim, type CodeCrossCheckRecord } from "../src/code-cross-check-contract.js";

const HEAD = "a".repeat(40), BASE = "b".repeat(40), TIP = "d".repeat(40);
const row: CodeCrossCheckRecord = {
  stage: "code", round: 1, candidate_generation: 1, head_commit: HEAD, base_commit: BASE,
  repo_instructions_commit: TIP, candidate_digest: "c".repeat(64), checker_run_id: null,
  checker_harness: "codex", checker_model: "m", checker_effort: null, outcome: "completed",
  reason_class: null, findings: [], interrupted_at: null, finalized_at: null,
  deadline_at: new Date(Date.now() + 60_000).toISOString(),
};
const noRow = { stage: "code" as const, result: "no_row" as const, repo_instructions_enabled: true };

it("consent refusal rebuilds text twice at the same pin after forge advancement with one snapshot", async () => {
  let snapshots = 0, submits = 0, forgeTip = TIP;
  const reads: string[] = [];
  const result = await checkCode({
    runId: "lead", generation: 1, signal: new AbortController().signal,
    snapshot: async () => { snapshots++; return { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: forgeTip }; },
    readInstructions: async pin => { reads.push(pin); assert.equal(pin, TIP); return "original tip"; },
    client: {
      codeCrossCheckStatus: async () => noRow,
      submitCodeCrossCheck: async (_id, _gen, snapshot, _signal, instructions) => {
        assert.deepEqual(snapshot, { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP });
        assert.deepEqual(instructions, { repo_instructions_enabled: true, repo_instructions_text: "original tip" });
        if (++submits === 1) { forgeTip = "e".repeat(40); throw new RequestError("POST", "/cross-checks", 409, "consent changed"); }
        return row;
      },
    },
  });
  assert.deepEqual(result, row);
  assert.deepEqual(reads, [TIP, TIP]);
  assert.equal(snapshots, 1);
  assert.equal(submits, 2);
});

it("consent off submits the instruction pin without reading any repository file", async () => {
  let reads = 0, submits = 0;
  assert.deepEqual(await checkCode({
    runId: "lead", generation: 1, signal: new AbortController().signal,
    snapshot: async () => ({ head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP }),
    readInstructions: async () => { reads++; assert.fail("consent off must not read repository instructions"); },
    client: {
      codeCrossCheckStatus: async () => ({ ...noRow, repo_instructions_enabled: false }),
      submitCodeCrossCheck: async (_id, _gen, snapshot, _signal, instructions) => {
        submits++;
        assert.deepEqual(snapshot, { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP });
        assert.deepEqual(instructions, { repo_instructions_enabled: false });
        return row;
      },
    },
  }), row);
  assert.equal(reads, 0);
  assert.equal(submits, 1);
});

it("a successful snapshot with unreadable tip instructions keeps its pin and submits empty text", async () => {
  const reads: string[] = [];
  assert.deepEqual(await checkCode({
    runId: "lead", generation: 1, signal: new AbortController().signal,
    snapshot: async () => ({ head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP }),
    readInstructions: async pin => { reads.push(pin); throw new Error("unreadable tip"); },
    client: {
      codeCrossCheckStatus: async () => noRow,
      submitCodeCrossCheck: async (_id, _gen, snapshot, _signal, instructions) => {
        assert.deepEqual(snapshot, { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP });
        assert.deepEqual(instructions, { repo_instructions_enabled: true, repo_instructions_text: "" });
        return row;
      },
    },
  }), row);
  assert.deepEqual(reads, [TIP]);
});

it("WorkerClient serializes the instruction pin separately from the diff base", async t => {
  const calls: Array<{ url: string; body: unknown }> = [];
  t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), body: JSON.parse(String(init?.body)) });
    return Response.json(row);
  });
  const client = new WorkerClient("http://example.com", "worker-token", "test", nullLogger());
  assert.deepEqual(await client.submitCodeCrossCheck("lead", 1,
    { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP },
    undefined, { repo_instructions_enabled: true, repo_instructions_text: "tip rules" }), { ...row, dispositions: null });
  assert.deepEqual(calls, [{
    url: "http://example.com/api/worker/runs/lead/cross-checks",
    body: { stage: "code", claim_generation: 1, head_commit: HEAD, base_commit: BASE,
      repo_instructions_commit: TIP, repo_instructions_enabled: true, repo_instructions_text: "tip rules" },
  }]);
});

it("strict old server rejection stays bounded and never retries without the pin", async () => {
  let submits = 0, snapshots = 0;
  assert.equal(await checkCode({
    runId: "lead", generation: 1, signal: new AbortController().signal,
    snapshot: async () => { snapshots++; return { head_commit: HEAD, base_commit: BASE, repo_instructions_commit: TIP }; },
    client: {
      codeCrossCheckStatus: async () => noRow,
      submitCodeCrossCheck: async (_id, _gen, snapshot) => {
        assert.ok("repo_instructions_commit" in snapshot);
        assert.equal(snapshot.repo_instructions_commit, TIP);
        submits++;
        throw new RequestError("POST", "/cross-checks", 400, "unknown field");
      },
    },
  }), undefined);
  assert.equal(submits, 2);
  assert.equal(snapshots, 1);
});

it("pin-only polling mutation invalidates unchanged digest/head/base/deadline", async () => {
  let reads = 0;
  assert.equal(await checkCode({
    runId: "lead", generation: 1, signal: new AbortController().signal, pollMs: 1,
    snapshot: async () => { assert.fail("persisted row must not snapshot"); },
    client: {
      codeCrossCheckStatus: async () => ++reads === 1 ? { ...row, outcome: "pending" } : { ...row, repo_instructions_commit: "e".repeat(40) },
      submitCodeCrossCheck: async () => { assert.fail("persisted row must not submit"); },
    },
  }), undefined);
  assert.equal(reads, 2);
});

it("status preserves absent legacy pin shape and validates nullable owner pin, code claims forbid null and plan pins", () => {
  const { repo_instructions_commit: _pin, ...legacy } = row;
  assert.equal("repo_instructions_commit" in decodeCodeCrossCheckStatus(legacy), false);
  for (const pin of [TIP, null]) {
    const decoded = decodeCodeCrossCheckStatus({ ...row, repo_instructions_commit: pin });
    assert.ok(!("result" in decoded));
    assert.equal(decoded.repo_instructions_commit, pin);
  }
  const code = { stage: "code", lead_run_id: "lead", round: 1, candidate_digest: "c".repeat(64),
    deadline_at: row.deadline_at, base_commit: BASE, head_commit: HEAD, plan_md: "", code_context: {} };
  assert.equal("repo_instructions_commit" in decodeCrossCheckClaim(code), false);
  assert.equal((decodeCrossCheckClaim({ ...code, repo_instructions_commit: TIP }) as { repo_instructions_commit: string }).repo_instructions_commit, TIP);
  for (const pin of ["D".repeat(40), TIP + "\n", "", 1, null]) {
    assert.throws(() => decodeCrossCheckClaim({ ...code, repo_instructions_commit: pin }), /invalid/);
    if (pin !== null) assert.throws(() => decodeCodeCrossCheckStatus({ ...row, repo_instructions_commit: pin }), /invalid/);
  }
  assert.throws(() => decodeCrossCheckClaim({ ...code, stage: "plan", repo_instructions_commit: TIP }), /invalid/);
});
