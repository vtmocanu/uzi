import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { test } from "node:test";
import {
  parsePodEpochEvidence, assertPodEpochEvidenceBinding, PodEpochEvidenceError,
} from "../src/pod-epoch-evidence.js";
import { parsePodEpochEvidence as parseFrozenV1Consumer } from "./pod-epoch-evidence-v1-fixture.js";

const root = new URL("../../fixtures/pod-epoch/", import.meta.url);
const positive = await readFile(new URL("valid/positive.json", root), "utf8");
test("all shared Go producer fixtures retain their raw bytes", async () => {
  for (const directory of ["valid", "invalid"]) {
    const names = await readdir(new URL(directory + "/", root));
    assert.ok(names.length > 0);
    for (const name of names) {
      const bytes = await readFile(new URL(directory + "/" + name, root));
      if (directory === "valid") assert.ok(parsePodEpochEvidence(bytes), name);
      else assert.throws(() => parsePodEpochEvidence(bytes), PodEpochEvidenceError, name);
    }
  }
  assert.ok(parsePodEpochEvidence(await readFile(new URL("positive.json", root))));
});
test("successor and declared runtime must match independently", () => {
  const e = parsePodEpochEvidence(positive);
  assert.ok("version" in e);
  assert.doesNotThrow(() => assertPodEpochEvidenceBinding(e, e.successor, e.runtime));
  assert.throws(() => assertPodEpochEvidenceBinding(e, e.predecessor, e.runtime));
  assert.throws(() => assertPodEpochEvidenceBinding(e, e.successor, "crio-crun"));
});
test("rejects integer spelling, int32 overflow, nested duplicates and actual UTF8 overflow", () => {
  for (const number of ["1.0", "1e0", "2147483648", "-1", "137", "255"])
    assert.throws(() => parsePodEpochEvidence(positive.replace('"exitCode":0', '"exitCode":' + number)));
  for (const raw of [
    positive.replace('"name":"worker"', '"name":"worker","na\\u006de":"other"'),
    positive.replace('"data":{', '"data":{"extra":{"a":1,"a":2},'),
    positive.replace('"version":1', '"version":1e0'),
    '{"handoff":"none","reason":null}',
    '{"handoff":"none","reason":"x","extra":0}',
    '{"handoff":"none","reason":"   "}',
    '{"handoff":"none","reason":"' + "é".repeat(4096) + '"}',
    positive + " false", positive.replace('"name":"worker"', '"name":null'),
  ]) assert.throws(() => parsePodEpochEvidence(raw), PodEpochEvidenceError);
  assert.throws(() => parsePodEpochEvidence(Buffer.from([0xff])), PodEpochEvidenceError);
  assert.throws(() => parsePodEpochEvidence("\ud800"), PodEpochEvidenceError);
  assert.ok(parsePodEpochEvidence(positive.padEnd(8192)));
  assert.throws(() => parsePodEpochEvidence(positive.padEnd(8193)));
});
test("semantic identities, terminal entries, exact shapes and maintenance binding", () => {
  const baseline = JSON.parse(positive);
  const changes: Array<(e: typeof baseline) => void> = [
    e => { e.successor = e.predecessor; },
    e => { e.session = e.session.toUpperCase().replace("2", "A"); },
    e => { e.runtime = "containerd"; },
    e => { e.data.name = e.dind.name; },
    e => { e.data.uid = "not-uuid"; },
    e => { e.dind.volumeName = ""; },
    e => { e.containers = []; },
    e => { e.containers.push(e.containers[0]); },
    e => { e.initContainers.push(e.initContainers[0]); },
    e => { e.containers[0].restartCount = -1; },
    e => { e.containers[0].restartCount = 2147483648; },
    e => { e.containers[0].containerID = ""; },
    e => { delete e.containers[0].restartCount; },
    e => { e.initContainers = null; },
    e => { e.maintenance = null; },
    e => { e.maintenance = { operationID: "x", oldClaimUID: e.data.uid, newClaimUID: e.data.uid }; },
    e => { e.maintenance = { operationID: "x", oldClaimUID: e.dind.uid, newClaimUID: e.dind.uid }; },
  ];
  for (const change of changes) {
    const e = structuredClone(baseline); change(e);
    assert.throws(() => parsePodEpochEvidence(JSON.stringify(e)), PodEpochEvidenceError);
  }
});
test("strict calendar validity and nanosecond observation ordering", () => {
  for (const time of ["0001-01-01T00:00:00Z", "2026-02-29T00:00:00Z",
    "2026-04-31T00:00:00Z", "2026-10-26T24:00:00Z", "2026-10-26T00:60:00Z",
    "2026-10-26T00:00:60Z", "2026-10-26T00:00:00+24:00",
    "2026-10-26T00:00:00.1234567890Z", "2026-10-26 00:00:00Z"]) {
    const e = JSON.parse(positive); e.sessionStarted = time;
    assert.throws(() => parsePodEpochEvidence(JSON.stringify(e)), PodEpochEvidenceError, time);
  }
  const e = JSON.parse(positive);
  e.sessionStarted = "2026-10-26T00:00:00.000000002Z";
  e.terminalObserved = "2026-10-26T00:00:00.000000001Z";
  assert.throws(() => parsePodEpochEvidence(JSON.stringify(e)));
  e.terminalObserved = "2026-10-26T01:00:00.000000002+01:00";
  assert.ok(parsePodEpochEvidence(JSON.stringify(e)));
  e.successorAdded = "2026-10-25T23:59:59Z";
  assert.throws(() => parsePodEpochEvidence(JSON.stringify(e)));
});
test("v2 corpus: raw bytes, version discrimination, strict presence rule", async () => {
  for (const directory of ["valid", "invalid"]) {
    const names = await readdir(new URL("v2/" + directory + "/", root));
    assert.ok(names.length > 0);
    for (const name of names) {
      const bytes = await readFile(new URL("v2/" + directory + "/" + name, root));
      if (directory === "invalid") {
        assert.throws(() => parsePodEpochEvidence(bytes), PodEpochEvidenceError, name);
        continue;
      }
      const e = parsePodEpochEvidence(bytes);
      assert.ok("version" in e, name);
      assert.equal(e.version, 2, name);
      if (e.version === 2) {
        assert.ok(e.successorContainers.length > 0 && e.attestation.reason.length > 0, name);
      }
    }
  }
  const v2 = parsePodEpochEvidence(await readFile(new URL("v2/positive.json", root)));
  assert.ok("version" in v2 && v2.version === 2);
  assert.deepEqual(v2.version === 2 && v2.attestation, { result: "consistent", reason: "observations_consistent" });
  const v1 = parsePodEpochEvidence(positive);
  assert.ok("version" in v1 && v1.version === 1);
  assert.equal("successorContainers" in v1, false);
});
test("old workers (frozen v0.86.0-rc.30 consumer) refuse every v2 document and still parse v1", async () => {
  for (const name of await readdir(new URL("v2/valid/", root))) {
    const bytes = await readFile(new URL("v2/valid/" + name, root));
    assert.throws(() => parseFrozenV1Consumer(bytes), Error, name);
  }
  assert.throws(() => parseFrozenV1Consumer(positive.replace('"version":1', '"version":2')));
  for (const name of await readdir(new URL("valid/", root))) {
    const bytes = await readFile(new URL("valid/" + name, root));
    assert.ok(parseFrozenV1Consumer(bytes), name);
  }
});
