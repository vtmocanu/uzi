import assert from "node:assert/strict";
import { it } from "node:test";
import { authorizeCodeDispositions, reportCodeDispositions, type CodeCrossCheckToolAccess } from "../src/code-cross-check-tools.js";
import type { CodeCrossCheckRecord } from "../src/code-cross-check-contract.js";

const row: CodeCrossCheckRecord = {
  "stage": "code",
  "round": 1,
  "candidate_generation": 7,
  "base_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "head_commit": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "candidate_digest": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
  "checker_run_id": "child",
  "checker_harness": "codex",
  "checker_model": "m",
  "checker_effort": "high",
  "outcome": "completed",
  "reason_class": null,
  "findings": [
    {
      "id": "F_1",
      "severity": "major",
      "path": "a.ts",
      "line": 2,
      "title": "Verify guard",
      "detail": "untrusted advice"
    }
  ],
  "dispositions": null,
  "finalized_at": null,
  "interrupted_at": null,
  "deadline_at": "2030-01-01T00:00:00Z"
};
const disposition = { finding_id: "F_1", disposition: "addressed", reason: "verified guard and affected tests" };
const args = { dispositions: [disposition] };

it("U3 D9 helper requires a live current repair row and worker generation", async () => {
  let current: CodeCrossCheckRecord | undefined = row;
  let effects = 0;
  const access: CodeCrossCheckToolAccess = {
    generation: 7, codeCrossCheckRepairActive: () => true, codeCrossCheckRepair: async () => current,
    reportCrossCheckDispositions: async batch => { effects++; assert.deepEqual(batch, args.dispositions); return row; },
  };
  await assert.rejects(authorizeCodeDispositions(undefined, args));
  for (const bad of [undefined, { ...row, stage: "plan" }, { ...row, round: 2 },
    { ...row, head_commit: "cached" }, { ...row, base_commit: "f".repeat(39) }, { ...row, candidate_generation: 6 },
    { ...row, interrupted_at: "2026-10-10T00:00:00Z" }, { ...row, outcome: "pending" },
    { ...row, outcome: "failed" }, { ...row, head_commit: null }, { ...row, base_commit: null },
    { ...row, findings: [] }]) {
    current = bad as CodeCrossCheckRecord | undefined;
    await assert.rejects(reportCodeDispositions(access, args));
  }
  assert.equal(effects, 0);
  current = row;
  for (const generation of [0, -1, 1.5, Number.NaN, Number.MAX_SAFE_INTEGER + 1])
    await assert.rejects(authorizeCodeDispositions({ ...access, generation }, args));
  await authorizeCodeDispositions(access, args);
  current = undefined;
  await assert.rejects(reportCodeDispositions(access, args), /not active/);
  assert.equal(effects, 0, "report revalidates instead of trusting prior authorization");
  current = row;
  await reportCodeDispositions(access, args);
  assert.equal(effects, 1);
});

it("U3 D9 asynchronous repair read cannot authorize a closed local pass", async () => {
  let active = true;
  let release!: (value: CodeCrossCheckRecord) => void;
  let posts = 0;
  const access: CodeCrossCheckToolAccess = {
    generation: 7, codeCrossCheckRepairActive: () => active,
    codeCrossCheckRepair: () => new Promise(resolve => { release = resolve; }),
    reportCrossCheckDispositions: async () => { posts++; return row; },
  };
  const rejected = assert.rejects(reportCodeDispositions(access, args), /not active/);
  active = false;
  release(row);
  await rejected;
  assert.equal(posts, 0);
});

it("U3 D9 helper rejects injected identity and strict malformed IDs and honors UTF-8 byte cap", async () => {
  const access: CodeCrossCheckToolAccess = {
    generation: 7, codeCrossCheckRepairActive: () => true, codeCrossCheckRepair: async () => row,
    reportCrossCheckDispositions: async () => { throw new Error("validation must not post"); },
  };
  for (const extra of ["generation", "candidate_generation", "run_id", "lead_run_id", "head_commit"])
    await assert.rejects(authorizeCodeDispositions(access, { ...args, [extra]: 7 }));
  for (const bad of [null, [], {}, { dispositions: "[]" },
    { dispositions: [disposition, disposition] },
    ...["unknown", "F_1\n", "F_1\r", "F_1\u0000", "Ｆ_1", " F_1"].map(finding_id => ({ dispositions: [{ ...disposition, finding_id }] })),
    { dispositions: [{ ...disposition, disposition: "not_reported", reason: "" }] },
    { dispositions: [{ ...disposition, generation: 7 }] },
    { dispositions: [{ ...disposition, reason: "界".repeat(341) + "ab" }] }])
    await assert.rejects(authorizeCodeDispositions(access, bad));
  for (const finding_id of ["F_1\n", "F_1\r", "F_1\u0000", "Ｆ_1", " F_1", "x".repeat(65)]) {
    const malformedKnown = { ...access, codeCrossCheckRepairActive: () => true, codeCrossCheckRepair: async () => ({
      ...row, findings: [{ ...row.findings[0]!, id: finding_id }],
    }) };
    await assert.rejects(authorizeCodeDispositions(malformedKnown, { dispositions: [{ ...disposition, finding_id }] }),
      /invalid/, "known-ID membership must not bypass strict ASCII validation");
  }
  const boundary = { dispositions: [{ ...disposition, reason: "界".repeat(341) + "a" }] };
  assert.equal(Buffer.byteLength(boundary.dispositions[0]!.reason), 1024);
  assert.deepEqual(await authorizeCodeDispositions(access, boundary), boundary.dispositions);
});
