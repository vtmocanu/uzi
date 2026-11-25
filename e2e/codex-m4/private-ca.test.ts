// Parser/control tests only: these never record a packaged proof pass.
import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";
import {
  PRIVATE_CA_CASES, PRIVATE_CA_OBSERVATIONS,
  candidateCaEnvironment, requirePrivateCaProof, runPrivateCa,
} from "./private-ca.js";
import { PRIVATE_CA_TITLE } from "./private-ca-title.js";

const complete = [
  ...PRIVATE_CA_CASES.map((title, i) => `ok ${i + 1} - ${title}`),
  ...PRIVATE_CA_OBSERVATIONS.map(value => `# ${value}1`),
  "1..10", "# tests 10", "# pass 10", "# fail 0", "# cancelled 0",
  "# skipped 0", "# todo 0", "CONTAINER_EXIT=0",
].join("\n");

test("private CA parser accepts a complete transcript shape only", () => {
  requirePrivateCaProof(complete);
  for (const line of complete.split("\n")) {
    assert.throws(() => requirePrivateCaProof(complete.split("\n").filter(value => value !== line).join("\n")));
  }
  assert.throws(() => requirePrivateCaProof(""));
  for (const [before, after] of [
    ["# pass 10", "# pass 9"], ["# fail 0", "# fail 1"],
    ["# skipped 0", "# skipped 1"], ["# cancelled 0", "# cancelled 1"],
    ["# todo 0", "# todo 1"],
  ]) assert.throws(() => requirePrivateCaProof(complete.replace(before!, after!)));
  assert.throws(() => requirePrivateCaProof(complete.replace("CONTAINER_EXIT=0", "CONTAINER_EXIT=1")));
  assert.throws(() => requirePrivateCaProof(complete + "\nnot ok 11 - extra"));
  assert.throws(() => requirePrivateCaProof(complete + "\n# tests 10"));
});

test("M4 clears both standalone image overrides without changing caller environment", () => {
  const original = { FIXTURE_IMAGE: "stale", FIXTURE_EXPECTED_ID: "stale-id", PATH: "/bin" };
  assert.deepEqual(candidateCaEnvironment(original), { PATH: "/bin" });
  assert.equal(original.FIXTURE_IMAGE, "stale");
});

test("missing child executable rejects and leaves failed evidence", async () => {
  const dir = mkdtempSync(resolve(__dirname, "../../.uzi/scratch/ca-control-"));
  const prior = { ...process.env };
  try {
    process.env.PATH = "";
    process.env.CODEX_M4_EVIDENCE = join(dir, "evidence.jsonl");
    await assert.rejects(runPrivateCa(), { code: "ENOENT" });
    assert.deepEqual(readFileSync(process.env.CODEX_M4_EVIDENCE, "utf8").trim().split("\n").map(line => JSON.parse(line)),
      [{ title: PRIVATE_CA_TITLE, result: "fail" }]);
  } finally {
    process.env = prior;
    rmSync(dir, { recursive: true, force: true });
  }
});
