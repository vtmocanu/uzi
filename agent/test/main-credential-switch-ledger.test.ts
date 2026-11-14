import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { buildCredentialSwitchLedger } from "../src/main.js";
import { CredentialSwitchOpLedger } from "../src/credential-switch-op-ledger.js";

// Issue #4201: the worker advertises credential_switch_in_place_v1 unconditionally, so startup must hand the
// RunRunner exactly one CredentialSwitchOpLedger on the worker's data dir. Without it the runner's in-place route
// stays closed and a worker that told the api it applies switches in place would silently run the v1 release.

test("the startup ledger is a CredentialSwitchOpLedger rooted at the data dir", async () => {
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "main-ledger-"));
  try {
    const ledger = buildCredentialSwitchLedger(dataDir);
    assert.ok(ledger instanceof CredentialSwitchOpLedger);
    await ledger.markSending("11111111-1111-4111-8111-111111111111", "op-1", 1);
    const written = fs.readdirSync(path.join(dataDir, "credential-switch-ops"));
    assert.deepEqual(written, ["11111111-1111-4111-8111-111111111111.json"], "its write-ahead entry lands under the data dir");
  } finally {
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
});

test("main.ts wires exactly one ledger into the RunRunner options, built by that function", () => {
  const main = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");
  assert.equal((main.match(/credentialSwitchLedger: buildCredentialSwitchLedger\(config\.dataDir\),/g) ?? []).length, 1,
    "the RunRunner options carry the ledger on the worker's data dir");
  assert.equal((main.match(/new CredentialSwitchOpLedger\(/g) ?? []).length, 1, "the only construction site is the factory");
  assert.equal((main.match(/new RunRunner\(/g) ?? []).length, 1, "one runner");
  const runner = main.slice(main.indexOf("new RunRunner("));
  assert.ok(runner.indexOf("credentialSwitchLedger: buildCredentialSwitchLedger(") >= 0 &&
    runner.indexOf("credentialSwitchLedger: buildCredentialSwitchLedger(") < runner.indexOf("\n  });"),
    "the ledger is inside the RunRunner options object");
});
