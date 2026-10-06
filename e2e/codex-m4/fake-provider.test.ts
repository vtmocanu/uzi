import { test } from "node:test";
import assert from "node:assert/strict";
import { request } from "node:http";
import { FakeProvider, dummyCredential } from "./fake-provider.js";

const CAP = 4 * 1024 * 1024;
function post(provider: FakeProvider, credential: string, body: string): Promise<number> {
  return new Promise((resolve, reject) => {
    const req = request(`${provider.baseUrl}/responses`, {
      method: "POST", headers: { authorization: `Bearer ${credential}`, "content-type": "application/json" },
    }, res => {
      res.resume();
      res.on("end", () => resolve(res.statusCode!));
      res.on("error", reject);
    });
    req.on("error", reject);
    req.setTimeout(5000, () => req.destroy(new Error("fixture post deadline")));
    req.end(body);
  });
}

test("fake provider keeps default 4 MiB request bound", async () => {
  const credential = dummyCredential();
  const provider = await FakeProvider.start({ credential, respond: () => [] });
  try {
    assert.equal(await post(provider, credential, '{"input":[]}'), 200);
    await assert.rejects(post(provider, credential, JSON.stringify({ input: ["x".repeat(CAP)] })));
    assert.equal(provider.requests.length, 1, "oversized request never reaches responder");
    assert.ok(provider.errors.includes("m4 request too large"));
  } finally { await provider.close(); }
});

test("fake provider accepts bounded 16 MiB opt-in above default cap", async () => {
  const credential = dummyCredential();
  const provider = await FakeProvider.start({ credential, maxRequestBytes: 16 * 1024 * 1024, respond: () => [] });
  try {
    assert.equal(await post(provider, credential, JSON.stringify({ input: ["x".repeat(CAP)] })), 200);
    assert.equal(provider.requests.length, 1);
    assert.deepEqual(provider.errors, []);
  } finally { await provider.close(); }
});

test("fake provider enforces a selected request cap", async () => {
  const credential = dummyCredential();
  const provider = await FakeProvider.start({ credential, maxRequestBytes: 128, respond: () => [] });
  try {
    assert.equal(await post(provider, credential, '{"input":[]}'), 200);
    await assert.rejects(post(provider, credential, JSON.stringify({ input: ["x".repeat(128)] })));
    assert.equal(provider.requests.length, 1);
    assert.ok(provider.errors.includes("m4 request too large"));
  } finally { await provider.close(); }
});

test("fake provider rejects invalid opt-in bounds before binding", async () => {
  for (const maxRequestBytes of [0, -1, 1.5, NaN, Infinity, 16 * 1024 * 1024 + 1]) {
    await assert.rejects(FakeProvider.start({
      credential: dummyCredential(), maxRequestBytes, respond: () => [],
    }), /request cap must be an integer from 1 to 16 MiB/);
  }
});
