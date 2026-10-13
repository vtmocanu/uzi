// PRD #2170: the code cross-check gate submits the repository instructions read at base_commit
// under the consent the server reports, and survives one consent change between status and submit.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { RequestError } from "../src/client.js";
import { checkCode } from "../src/code-cross-check-gate.js";
import type { CodeCrossCheckInstructions, CodeCrossCheckRecord, CodeCrossCheckStatus } from "../src/code-cross-check-contract.js";

const HEAD = "a".repeat(40);
const BASE = "b".repeat(40);
const noRow = (enabled: boolean): CodeCrossCheckStatus => ({ stage: "code", result: "no_row", repo_instructions_enabled: enabled });
const row: CodeCrossCheckRecord = {
  stage: "code", round: 1, candidate_generation: 1, head_commit: HEAD, base_commit: BASE, candidate_digest: "c".repeat(64),
  checker_run_id: null, checker_harness: "codex", checker_model: "m", checker_effort: null, outcome: "completed",
  reason_class: null, findings: [], interrupted_at: null, finalized_at: null, deadline_at: new Date(Date.now() + 60_000).toISOString(),
};
const refusal = (status = 409) => new RequestError("POST", "/cross-checks", status, "refused");

/** A scripted client: `statuses` are returned in order (the last repeats), `submits` likewise, where
 *  an Error value is thrown. Every call is recorded. */
function rig(opts: { statuses: Array<CodeCrossCheckStatus | Error>; submits: Array<CodeCrossCheckStatus | Error>;
  reads?: Array<string | Error>; snapshot?: () => ReturnType<Parameters<typeof checkCode>[0]["snapshot"]> }) {
  const calls = { status: 0, submit: [] as Array<CodeCrossCheckInstructions | undefined>, reads: [] as string[], snapshots: 0 };
  const next = <T,>(list: T[], i: number): T => list[Math.min(i, list.length - 1)] as T;
  const client = {
    codeCrossCheckStatus: async () => {
      const v = next(opts.statuses, calls.status++);
      if (v instanceof Error) throw v;
      return v;
    },
    submitCodeCrossCheck: async (_id: string, _gen: number, _snap: unknown, _sig?: AbortSignal, instructions?: CodeCrossCheckInstructions) => {
      const v = next(opts.submits, calls.submit.length);
      calls.submit.push(instructions);
      if (v instanceof Error) throw v;
      return v;
    },
  };
  const run = () => checkCode({
    client, runId: "run", generation: 1, signal: new AbortController().signal, pollMs: 1, requestMs: 1000,
    snapshot: opts.snapshot ?? (async () => { calls.snapshots++; return { head_commit: HEAD, base_commit: BASE }; }),
    readInstructions: async (base: string) => {
      assert.equal(base, BASE, "instructions are read at the pinned base_commit");
      const v = next(opts.reads ?? [""], calls.reads.length);
      calls.reads.push(base);
      if (v instanceof Error) throw v;
      return v;
    },
  });
  return { calls, run };
}

describe("code cross-check gate instructions (PRD #2170)", () => {
  it("consent on submits the text read at base_commit", async () => {
    const r = rig({ statuses: [noRow(true)], submits: [row], reads: ["# rules\n"] });
    assert.deepEqual(await r.run(), row);
    assert.deepEqual(r.calls.submit, [{ repo_instructions_enabled: true, repo_instructions_text: "# rules\n" }]);
    assert.equal(r.calls.reads.length, 1);
  });

  it("consent off sends false with no text and never reads the repository", async () => {
    const r = rig({ statuses: [noRow(false)], submits: [row] });
    await r.run();
    assert.deepEqual(r.calls.submit, [{ repo_instructions_enabled: false }]);
    assert.equal(r.calls.reads.length, 0);
  });

  it("a read failure sends enabled with empty text", async () => {
    const r = rig({ statuses: [noRow(true)], submits: [row], reads: [new Error("unreadable")] });
    assert.deepEqual(await r.run(), row);
    assert.deepEqual(r.calls.submit, [{ repo_instructions_enabled: true, repo_instructions_text: "" }]);
  });

  it("a pre-snapshot failure submit carries no instruction fields", async () => {
    const r = rig({ statuses: [noRow(true)], submits: [row], snapshot: async () => ({ reason_class: "snapshot_failed" as const }) });
    await r.run();
    assert.deepEqual(r.calls.submit, [undefined]);
    assert.equal(r.calls.reads.length, 0);
  });

  it("consent on to off between status and submit resubmits once with no text", async () => {
    const r = rig({ statuses: [noRow(true), noRow(false)], submits: [refusal(), row], reads: ["old text"] });
    assert.deepEqual(await r.run(), row);
    assert.deepEqual(r.calls.submit, [
      { repo_instructions_enabled: true, repo_instructions_text: "old text" },
      { repo_instructions_enabled: false },
    ]);
    assert.equal(r.calls.reads.length, 1, "no further read once consent is off");
    assert.equal(r.calls.snapshots, 1, "the pinned snapshot is reused");
  });

  it("consent off to on resubmits after a fresh base read", async () => {
    const r = rig({ statuses: [noRow(false), noRow(true)], submits: [refusal(), row], reads: ["fresh text"] });
    assert.deepEqual(await r.run(), row);
    assert.deepEqual(r.calls.submit, [
      { repo_instructions_enabled: false },
      { repo_instructions_enabled: true, repo_instructions_text: "fresh text" },
    ]);
    assert.equal(r.calls.reads.length, 1);
  });

  it("consent still on rereads rather than reusing the earlier text", async () => {
    const r = rig({ statuses: [noRow(true), noRow(true)], submits: [refusal(400), row], reads: ["first", "second"] });
    await r.run();
    assert.deepEqual(r.calls.submit.map((s) => s && "repo_instructions_text" in s ? s.repo_instructions_text : undefined), ["first", "second"]);
  });

  it("adopts a row that exists on the re-read without resubmitting", async () => {
    const r = rig({ statuses: [noRow(true), row], submits: [refusal()], reads: ["x"] });
    assert.deepEqual(await r.run(), row);
    assert.equal(r.calls.submit.length, 1);
  });

  for (const [name, error] of Object.entries({
    "a timeout": new DOMException("timed out", "TimeoutError"),
    "a network error": new TypeError("fetch failed"),
    "a 500": refusal(500),
    "a 502": refusal(502),
    "a 3xx": refusal(302),
  })) {
    it(`${name} on the submit is never resubmitted`, async () => {
      const r = rig({ statuses: [noRow(true), noRow(false)], submits: [error, row], reads: ["x"] });
      assert.equal(await r.run(), undefined);
      assert.equal(r.calls.submit.length, 1);
      assert.equal(r.calls.status, 1, "no consent re-read for an ambiguous outcome");
    });
  }

  it("resubmits at most once when the second submit is refused too", async () => {
    const r = rig({ statuses: [noRow(true), noRow(false), noRow(true)], submits: [refusal(), refusal(), row], reads: ["x"] });
    assert.equal(await r.run(), undefined);
    assert.equal(r.calls.submit.length, 2);
    assert.equal(r.calls.status, 2);
  });

  it("does not resubmit when the consent re-read fails", async () => {
    const r = rig({ statuses: [noRow(true), new Error("status down")], submits: [refusal(), row], reads: ["x"] });
    assert.equal(await r.run(), undefined);
    assert.equal(r.calls.submit.length, 1);
  });

  it("an ambiguous second submit is not retried either", async () => {
    const r = rig({ statuses: [noRow(true), noRow(false)], submits: [refusal(), refusal(503), row], reads: ["x"] });
    assert.equal(await r.run(), undefined);
    assert.equal(r.calls.submit.length, 2);
  });
});
