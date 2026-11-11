import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { readEpochReclaimDeletionActivation } from "../src/pod-epoch-reclaim.js";

const NAME = "UZI_EPOCH_RECLAIM_DELETION";
const read = (env: Record<string, string | undefined>) => readEpochReclaimDeletionActivation(env as NodeJS.ProcessEnv);

test("activation is off when unset, empty or 0, whatever else the env carries", () => {
  assert.equal(read({}), false);
  assert.equal(read({ [NAME]: "" }), false);
  assert.equal(read({ [NAME]: "0" }), false);
  assert.equal(read({ [NAME]: "0", UZI_EPOCH_LOCK: "1", DOCKER_HOST: "tcp://127.0.0.1:2375" }), false);
});

test("activation 1 needs the epoch lock and a Docker wiring signal (DOCKER_HOST or UZI_DIND_SOCKET)", () => {
  assert.equal(read({ [NAME]: "1", UZI_EPOCH_LOCK: "1", DOCKER_HOST: "tcp://127.0.0.1:2375" }), true);
  assert.equal(read({ [NAME]: "1", UZI_EPOCH_LOCK: "1", UZI_DIND_SOCKET: "/run/docker.sock" }), true);
  const refusal = /^Error: UZI_EPOCH_RECLAIM_DELETION=1 requires an epoch-locked, Docker-wired worker \(UZI_EPOCH_LOCK=1 and DOCKER_HOST or UZI_DIND_SOCKET\)$/;
  for (const env of [
    { [NAME]: "1", DOCKER_HOST: "tcp://127.0.0.1:2375" },
    { [NAME]: "1", UZI_EPOCH_LOCK: "1" },
    { [NAME]: "1", UZI_EPOCH_LOCK: "1", DOCKER_HOST: "   " },
    { [NAME]: "1", UZI_EPOCH_LOCK: "0", DOCKER_HOST: "tcp://127.0.0.1:2375" },
  ]) assert.throws(() => read(env), (e: Error) => refusal.test(String(e)), JSON.stringify(env));
});

test("any other activation value is a startup error, without trimming", () => {
  for (const value of [" 1", "1 ", "true", "2", "on", "00", "false"]) {
    assert.throws(() => read({ [NAME]: value, UZI_EPOCH_LOCK: "1", DOCKER_HOST: "tcp://127.0.0.1:2375" }),
      /^Error: UZI_EPOCH_RECLAIM_DELETION must be unset, empty, 0 or 1$/, JSON.stringify(value));
  }
});

test("the env name lives in one agent/src file and main parses it once from process.env before the epoch is created", async () => {
  const root = fileURLToPath(new URL("../src/", import.meta.url));
  const files = (await readdir(root, { recursive: true })).filter(f => /\.(ts|mts|cts|js|mjs|cjs)$/.test(f));
  const holders: string[] = [];
  for (const f of files) if ((await readFile(join(root, f), "utf8")).includes(NAME)) holders.push(f);
  assert.deepEqual(holders, ["pod-epoch-reclaim.ts"]);
  const main = await readFile(join(root, "main.ts"), "utf8");
  const calls = main.match(/readEpochReclaimDeletionActivation\(/g) ?? [];
  assert.equal(calls.length, 1);
  assert.equal(main.match(/readEpochReclaimDeletionActivation\(process\.env\)/g)?.length, 1);
  assert.ok(main.indexOf("readEpochReclaimDeletionActivation(process.env)") < main.indexOf("createPodEpoch("),
    "the activation is parsed before the epoch runtime exists");
  assert.ok(main.includes('"pod_epoch_reclaim_activation"'));
});
