import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { it } from "node:test";
import { decodeCodeCrossCheckStatus, type CodeCrossCheckStatus } from "../src/code-cross-check-contract.js";
import { CodeCrossCheckGate } from "../src/code-cross-check-gate.js";

// Issue #2870. The bodies are recorded byte for byte from WorkerCodeCrossCheckStatus by
// api/internal/handler/code_cross_check_contract_test.go, so this test sees the server's real
// pending shape rather than a hand-written `findings: []`.
const wire = (name: string): CodeCrossCheckStatus =>
  decodeCodeCrossCheckStatus(JSON.parse(readFileSync(new URL(`../../fixtures/code-cross-check/${name}`, import.meta.url), "utf8")));

it("lead waits through the server's pending row and repairs the checker's finding", async () => {
  const reads: string[] = [];
  let polls = 0;
  const gate = new CodeCrossCheckGate({
    client: {
      codeCrossCheckStatus: async () => {
        if (reads.push("status") === 1) return { stage: "code", result: "no_row", repo_instructions_enabled: false };
        return wire(++polls === 1 ? "status-pending.json" : "status-completed.json");
      },
      submitCodeCrossCheck: async () => { reads.push("submit"); return wire("status-pending.json"); },
      reportCodeCrossCheckDispositions: async () => { throw new Error("not reached"); },
    },
    runId: "00000000-0000-4000-8000-0000000000a3", generation: 7, signal: new AbortController().signal, pollMs: 1,
    snapshot: async () => ({ head_commit: "b".repeat(40), base_commit: "a".repeat(40) }),
  });
  const decision = await gate.done();
  assert.equal(decision.action, "repair");
  assert.deepEqual(reads, ["status", "submit", "status", "status"]);
  const summary = gate.observed();
  assert.ok(summary && !("incomplete" in summary));
  assert.equal(summary.outcome, "completed");
  assert.deepEqual(summary.findings.map(f => f.id), ["F_1"]);
});
