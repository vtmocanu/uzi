// The agent half of the Codex model vocabulary seam (PRD #3245 M2): the role-model set the
// advice renderer accepts must equal the shared fixture the API and web tests also read.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import { CODEX_ROLE_MODELS, isCodexRoleModel } from "../src/codex/render.js";

const fixture = JSON.parse(
  readFileSync(new URL("../../fixtures/codex-models/models.json", import.meta.url), "utf8"),
) as { role: string[] };

describe("CODEX_ROLE_MODELS", () => {
  it("equals the role list in fixtures/codex-models/models.json", () => {
    assert.deepEqual(
      [...CODEX_ROLE_MODELS].sort(),
      [...fixture.role].sort(),
      "CODEX_ROLE_MODELS drifted from fixtures/codex-models/models.json (role)",
    );
  });

  it("isCodexRoleModel accepts every member", () => {
    for (const m of CODEX_ROLE_MODELS) assert.equal(isCodexRoleModel(m), true, m);
  });

  it("isCodexRoleModel rejects non-members, including untrimmed and foreign ids", () => {
    for (const m of ["", "opus", " gpt-6-sol ", "gpt-7-x"]) assert.equal(isCodexRoleModel(m), false, JSON.stringify(m));
  });
});
