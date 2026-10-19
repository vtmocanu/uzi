import test from "node:test";
import assert from "node:assert/strict";
import { classifyRecoveryOrigin } from "../src/recovery-origin.js";

const uuid = "12345678-1234-4abc-8abc-123456789abc";
const producer = "maintenance-v1-" + uuid;

test("producer origin requires the exact 51-character lowercase v4 UUID grammar", () => {
  assert.equal(producer.length, 51);
  for (const variant of ["8", "9", "a", "b"])
    assert.equal(classifyRecoveryOrigin("maintenance-v1-" + uuid.replace("8abc", variant + "abc")), "producer");
  const malformed = [
    producer.toUpperCase(), producer.replace("v1", "V1"), producer.replace("v1", "v2"),
    producer.replace("v1", "v01"), producer.replace("maintenance-", "maintenance_"),
    producer.replace("-v1-", "-v1_"), producer.replace("-v1-", "/v1/"),
    producer.replace("4abc", "4ABC"), producer + "-suffix", producer + " ",
    producer + "\n", producer + "\r\n", producer + "\u2028",
    producer.slice(0, -1), producer.replace("123456789abc", "123456789abg"),
    "maintenance", "maintenance-v1-", "maintenan", "MAINTENAN",
    "maintenance\n", "maintenance-v", "maintenance/v1/" + uuid,
  ];
  for (const version of ["0", "1", "2", "3", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"])
    malformed.push("maintenance-v1-" + uuid.replace("4abc", version + "abc"));
  for (const variant of ["0", "1", "2", "3", "4", "5", "6", "7", "c", "d", "e", "f"])
    malformed.push("maintenance-v1-" + uuid.replace("8abc", variant + "abc"));
  for (const id of malformed) assert.equal(classifyRecoveryOrigin(id), "malformed", JSON.stringify(id));
  for (let end = 1; end < producer.length; end++)
    assert.equal(classifyRecoveryOrigin(producer.slice(0, end)), "malformed");
});

test("ordinary IDs stay ordinary and Unicode does not normalize into the ASCII namespace", () => {
  for (const id of [
    uuid, uuid.toUpperCase(), uuid.replace("4abc", "1abc"), "", "other-id",
    " " + producer, "x" + producer, "\n" + producer,
    "maıntenance-v1-" + uuid, "maintenancｅ-v1-" + uuid, "ｍaintenance-v1-" + uuid,
  ]) assert.equal(classifyRecoveryOrigin(id), "ordinary", JSON.stringify(id));
});
