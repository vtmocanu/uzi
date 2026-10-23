import { it } from "node:test";
import assert from "node:assert/strict";
import { buildSdkEnv, buildCheckEnv } from "../src/sdk-env.js";
import { buildCommandEnv } from "../src/codex/codex-executor.js";

it("the baked sqlc cache crosses scrubbed run environments without a provision override", () => {
  const previous = process.env.UZI_SQLC_DIR;
  const cache = "/usr/local/share/uzi-sqlc";
  try {
    process.env.UZI_SQLC_DIR = cache;
    const hostile = { UZI_SQLC_DIR: "/untrusted/cache" };
    const sdk = buildSdkEnv("fixture", "/run-home", hostile);
    const command = buildCommandEnv("/scratch", hostile);
    const check = buildCheckEnv({ UZI_SQLC_DIR: cache, UZI_WORKER_TOKEN: "must-not-cross" }, "/run-home", hostile);
    for (const env of [sdk, command, check]) {
      assert.equal(env.UZI_SQLC_DIR, cache);
      assert.equal(env.UZI_WORKER_TOKEN, undefined);
    }
    delete process.env.UZI_SQLC_DIR;
    assert.equal(buildSdkEnv("fixture", "/run-home", hostile).UZI_SQLC_DIR, undefined);
    assert.equal(buildCommandEnv("/scratch", hostile).UZI_SQLC_DIR, undefined);
    assert.equal(buildCheckEnv({}, "/run-home", hostile).UZI_SQLC_DIR, undefined);
  } finally {
    if (previous === undefined) delete process.env.UZI_SQLC_DIR;
    else process.env.UZI_SQLC_DIR = previous;
  }
});
