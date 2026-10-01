import { it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { CODEX_PROBE_EXPECTATION } from "../src/codex/codex-runtime-probe.js";
import { CODEX_BIN } from "../src/codex/launcher.js";

it("a Codex lock upgrade must reconcile the production probe and launcher pins", () => {
  const lock = readFileSync(new URL("../codex/codex-package.lock", import.meta.url), "utf8");
  const value = (key: string): string => {
    const match = lock.match(new RegExp(`^${key}=(.+)$`, "m"));
    assert.ok(match, `missing ${key}`);
    return match[1]!;
  };
  const version = value("CODEX_VERSION");
  assert.equal(value("CODEX_MANIFEST_VERSION"), version);
  assert.equal(value("CODEX_TAG"), `rust-v${version}`);
  assert.equal(CODEX_PROBE_EXPECTATION.version, version,
    "reconcile the runtime expectation and verify the Codex upgrade before merging");
  assert.equal(CODEX_BIN, `/opt/uzi-codex/${version}/bin/codex`);
  for (const arch of ["amd64", "arm64"]) {
    assert.equal(value(`CODEX_TAG_${arch}`), value("CODEX_TAG"));
    assert.equal(CODEX_PROBE_EXPECTATION.lockDigest[arch], value(`CODEX_SHA256_${arch}`));
  }
});
