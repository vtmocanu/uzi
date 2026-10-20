// Single-uid acceptance for the guarded Claude command launcher preflight (issue #2471, M4).
//
// Runs in a container started directly as worker uid 10001 with `--cap-drop ALL`, with NO root
// entrypoint and NO uid split (the Kubernetes runAsUser posture). run.sh starts this one file in
// three containers that differ only in the posture under test, named by UZI_SINGLE_UID_EXPECT:
//   ok                  the production single-uid posture: no-new-privileges, empty bounding set
//   profile:noNewPrivs  the same container started WITHOUT no-new-privileges
//   profile:capBnd      the same container with one capability left in the bounding set
// Each run asserts the container really has the posture its label names (read from the process
// status file, never from the label), then asserts the real preflight, which launches the image
// supervisor and has it verify its own profile, returns exactly the expected verdict.
import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";
import { memoryLauncherPreflight } from "../../agent/src/claude-command/preflight.js";

const EXPECT = process.env.UZI_SINGLE_UID_EXPECT;

function status(): Map<string, string> {
  return new Map(fs.readFileSync("/proc/self/status", "utf8").split("\n").flatMap((line) => {
    const at = line.indexOf(":");
    return at > 0 ? [[line.slice(0, at), line.slice(at + 1).trim()] as [string, string]] : [];
  }));
}

test("single-uid launcher preflight matches the container posture", { timeout: 60_000 }, async () => {
  assert.ok(EXPECT === "ok" || EXPECT === "profile:noNewPrivs" || EXPECT === "profile:capBnd", `UZI_SINGLE_UID_EXPECT=${String(EXPECT)}`);
  assert.equal(process.platform, "linux");
  assert.equal(process.getuid?.(), 10001, "started directly as the worker uid");
  assert.equal(process.env.UZI_UID_SPLIT, undefined, "no uid split: the root entrypoint did not run");
  const posture = status();
  for (const field of ["CapInh", "CapPrm", "CapEff", "CapAmb"]) assert.equal(BigInt("0x" + posture.get(field)!), 0n, field);
  const bounding = BigInt("0x" + posture.get("CapBnd")!);
  assert.equal(posture.get("NoNewPrivs"), EXPECT === "profile:noNewPrivs" ? "0" : "1", "no-new-privileges matches the posture");
  assert.equal(bounding !== 0n, EXPECT === "profile:capBnd", `bounding set ${bounding.toString(16)} matches the posture`);

  const result = await memoryLauncherPreflight();
  assert.deepEqual(result, EXPECT === "ok" ? { ok: true } : { ok: false, reason: EXPECT });
});
