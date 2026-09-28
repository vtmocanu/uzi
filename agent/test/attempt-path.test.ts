import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  ATTEMPT_ID_RE,
  ATTEMPT_SEPARATOR,
  cloneKeyOf,
  formatAttemptId,
  isWithinPath,
  parseAttemptPath,
} from "../src/attempt-path.js";
import { buildSdkEnv } from "../src/sdk-env.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV, WORKER_SPAWN_ENV, workerSpawnEnv, workerSpawnNonce } from "../src/worker-spawn-mark.js";

// issue #1783 — the shared clone-path / attempt grammar (M1 uses it to scope the reaper; M2 seeds
// attempt clones with it) and the R2/R4 environment markers.

describe("isWithinPath (whole-component containment)", () => {
  it("never matches issue-17 against issue-1769, in either direction", () => {
    assert.equal(isWithinPath("/x/issue-1769", "/x/issue-17"), false);
    assert.equal(isWithinPath("/x/issue-17", "/x/issue-1769"), false);
    assert.equal(isWithinPath("/x/issue-1769/a", "/x/issue-17"), false);
  });

  it("matches the root itself and anything below it", () => {
    assert.equal(isWithinPath("/x/issue-17", "/x/issue-17"), true);
    assert.equal(isWithinPath("/x/issue-17/", "/x/issue-17"), true);
    assert.equal(isWithinPath("/x/issue-17/a/b", "/x/issue-17"), true);
  });

  it("resolves .. before comparing, and rejects parents and unrelated paths", () => {
    assert.equal(isWithinPath("/x/issue-17/../issue-1769", "/x/issue-17"), false);
    assert.equal(isWithinPath("/x", "/x/issue-17"), false);
    assert.equal(isWithinPath("/y/issue-17", "/x/issue-17"), false);
    // A name that merely starts with ".." is a real child, not a parent reference.
    assert.equal(isWithinPath("/x/issue-17/..hidden", "/x/issue-17"), true);
  });
});

describe("attempt ids", () => {
  const at = new Date(Date.UTC(2026, 8, 7, 3, 4, 5));

  it("formats <UTC stamp>-g<gen>-<16 hex>", () => {
    const id = formatAttemptId(at, 12, "0123456789abcdef");
    assert.equal(id, "20260907T030405Z-g12-0123456789abcdef");
    assert.match(id, ATTEMPT_ID_RE);
  });

  it("uses gx when the claim carries no generation", () => {
    assert.equal(formatAttemptId(at, undefined, "fedcba9876543210"), "20260907T030405Z-gx-fedcba9876543210");
  });

  it("refuses randomness that is not 16 lowercase hex characters", () => {
    assert.throws(() => formatAttemptId(at, 1, "ABCDEF0123456789"));
    assert.throws(() => formatAttemptId(at, 1, "abc"));
  });
});

describe("parseAttemptPath", () => {
  const root = "/data/runner";
  const id = "20260907T030405Z-g3-0123456789abcdef";

  it("accepts <runnerRoot>/<repoDir>/<key>.attempt-<id>", () => {
    assert.deepEqual(parseAttemptPath(`${root}/github.com+o+r/issue-17${ATTEMPT_SEPARATOR}${id}`, root), {
      repoDir: "github.com+o+r",
      key: "issue-17",
      attemptId: id,
    });
  });

  it("keeps a key that itself contains dots", () => {
    assert.deepEqual(parseAttemptPath(`${root}/r/ci-fix.v2.attempt-${id}`, root)?.key, "ci-fix.v2");
  });

  it("rejects a canonical clone, extra separators, .. and paths outside the root", () => {
    for (const p of [
      `${root}/r/issue-17`,
      `${root}/r/sub/issue-17.attempt-${id}`,
      `${root}/issue-17.attempt-${id}`,
      `${root}/../r/issue-17.attempt-${id}`,
      `${root}/r/../r/issue-17.attempt-${id}`,
      `/elsewhere/r/issue-17.attempt-${id}`,
      `${root}/r/issue-17.attempt-${id}/`,
      `${root}/r/issue-17.attempt-${id}x`,
      `${root}/r/issue-17.attempt-20260907T030405Z-g3-0123456789ABCDEF`,
      `${root}/r/.attempt-${id}`,
      `r/issue-17.attempt-${id}`,
    ]) {
      assert.equal(parseAttemptPath(p, root), undefined, p);
    }
  });

  it("derives one clone key for the canonical and attempt shapes", () => {
    assert.deepEqual(cloneKeyOf(`${root}/r/issue-17`), { cloneKey: "r/issue-17", canonicalPath: `${root}/r/issue-17` });
    assert.deepEqual(cloneKeyOf(`${root}/r/issue-17.attempt-${id}`), {
      cloneKey: "r/issue-17",
      canonicalPath: `${root}/r/issue-17`,
    });
  });
});

describe("R2 attempt marker / R4 worker mark", () => {
  const attempt = { marker: "run-1:20260907T030405Z-g3-0123456789abcdef", clonePath: "/data/runner/r/issue-17", cloneKey: "r/issue-17" };

  it("buildSdkEnv emits the three attempt keys only when an attempt is given", () => {
    const withAttempt = buildSdkEnv("tok", "/home", {}, undefined, attempt);
    assert.equal(withAttempt[RUN_ATTEMPT_ENV], attempt.marker);
    assert.equal(withAttempt[RUN_CLONE_ENV], attempt.clonePath);
    assert.equal(withAttempt[RUN_CLONE_KEY_ENV], attempt.cloneKey);
    const without = buildSdkEnv("tok", "/home");
    assert.equal(RUN_ATTEMPT_ENV in without, false);
    assert.equal(RUN_CLONE_KEY_ENV in without, false);
  });

  it("a provisioned tool env can neither forge nor override the markers", () => {
    const forged = {
      [RUN_ATTEMPT_ENV]: "forged",
      [RUN_CLONE_ENV]: "/forged",
      [RUN_CLONE_KEY_ENV]: "forged/key",
      [WORKER_SPAWN_ENV]: "forged-nonce",
    };
    const env = buildSdkEnv("tok", "/home", forged, undefined, attempt);
    assert.equal(env[RUN_ATTEMPT_ENV], attempt.marker);
    assert.equal(env[RUN_CLONE_ENV], attempt.clonePath);
    assert.equal(env[RUN_CLONE_KEY_ENV], attempt.cloneKey);
    assert.equal(env[WORKER_SPAWN_ENV], undefined);
    const noAttempt = buildSdkEnv("tok", "/home", forged);
    assert.equal(noAttempt[RUN_ATTEMPT_ENV], undefined);
  });

  it("workerSpawnEnv adds this worker's nonce and strips every attempt marker", () => {
    const env = workerSpawnEnv({ PATH: "/bin", [RUN_ATTEMPT_ENV]: "x", [RUN_CLONE_ENV]: "/c", [RUN_CLONE_KEY_ENV]: "r/k" });
    assert.deepEqual(env, { PATH: "/bin", [WORKER_SPAWN_ENV]: workerSpawnNonce() });
    assert.match(workerSpawnNonce(), /^[0-9a-f]{32}$/);
  });
});
