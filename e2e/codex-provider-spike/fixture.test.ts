import { spawn } from "node:child_process";
import { ownChild } from "./ownership.js";
import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { createServer, request } from "node:http";
import { FakeProvider, Budget, stream } from "./fake-provider.js";
import { ownedDeadline } from "./timers.js";
import { validateFixtureTable, validateFixtureEnv } from "./driver.js";
import { connect } from "node:net";
import { once } from "node:events";
import { validateName, Evidence, runOpenSSL, wireFixture, inventoryEvidence, resumeDeadlineObserved, settleReceiverResponse, type Row } from "./runner.js";

test("owned child timer expires and cleanup clears the exact handle", () => {
  let expired = false; let callback = () => {}; let cleared: unknown;
  const handle = {};
  const cleanup = ownedDeadline(() => { expired = true; }, {
    set: (cb, ms) => { callback = cb; assert.equal(ms, 25000); return handle; },
    clear: value => { cleared = value; },
  });
  callback(); assert.equal(expired, true); cleanup(); assert.equal(cleared, handle);
});

test("aggregate byte and item exhaustion reject before accumulation", () => {
  const bytes = new Budget(4, 3); bytes.take(4);
  assert.throws(() => bytes.take(1), /budget/); assert.equal(bytes.bytes, 4);
  const items = new Budget(100, 1); items.take(0);
  assert.throws(() => items.take(0), /budget/); assert.equal(items.items, 1);
  assert.throws(() => bytes.take(-1), /budget/);
});
test("invalid case names fail closed", () => {
  assert.throws(() => validateName("../escape"), /invalid-case/);
  assert.throws(() => validateName("baseline\nsecret"), /invalid-case/);
  validateName("baseline");
});
test("evidence aggregate has finite item and byte ceilings", () => {
  const evidence = new Evidence();
  assert.equal(evidence.budget.maxBytes, 1024 * 1024);
  assert.equal(evidence.budget.maxItems, 64);
});
test("request authentication flips when guard removed; no canary retained", async () => {
  const credential = randomBytes(24).toString("hex");
  for (const guard of [true, false]) {
    const provider = await FakeProvider.start({ credential, guard });
    try {
      const status = await new Promise<number>(resolve => {
        const req = request(provider.baseUrl + "/responses", { method: "POST", headers: { authorization: "Bearer wrong" } },
          res => { res.resume(); res.once("end", () => resolve(res.statusCode!)); });
        req.end(JSON.stringify({ synthetic: credential }));
      });
      assert.equal(status, guard ? 401 : 200);
      assert.equal(JSON.stringify(provider.requests).includes(credential), false);
      assert.equal(JSON.stringify(provider.errors).includes(credential), false);
    } finally { await provider.close(); }
    assert.equal(provider.sockets.size, 0);
  }
});
test("receiver observes header and body independently; close settles open socket", async () => {
  const credential = randomBytes(24).toString("hex");
  const provider = await FakeProvider.start({ credential });
  try {
    await new Promise<void>(resolve => {
      const req = request(`http://127.0.0.1:${provider.port}/target`, { method: "POST", headers: { authorization: `Bearer ${credential}` } },
        res => { res.resume(); res.once("end", resolve); });
      req.end("synthetic");
    });
    assert.equal(provider.header, true); assert.equal(provider.body, true);
    assert.ok(Buffer.byteLength(stream) <= 1024 * 1024);
  } finally { await provider.close(); }
});

test("receiver records forwarding before a partial request disconnects", async () => {
  const provider = await FakeProvider.start({ credential: "synthetic" });
  const socket = connect(provider.port, "127.0.0.1");
  try {
    await once(socket, "connect");
    const received = new Promise<void>(resolve => {
      provider.server.once("request", req => { req.once("data", () => resolve()); });
    });
    socket.write("POST /target HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer synthetic\r\nContent-Length: 100\r\n\r\npartial");
    await received;
    socket.destroy();
    assert.equal(provider.header, true);
    assert.equal(provider.body, true);
  } finally { socket.destroy(); await provider.close(); }
});

test("inventory evidence requires a measured request and excludes priming observations", async () => {
  const provider = await FakeProvider.start({ credential: "synthetic" });
  const send = (name: string) => new Promise<void>((resolve, reject) => {
    const req = request(provider.baseUrl + "/responses", { method: "POST", headers: { authorization: "Bearer synthetic" } },
      res => settleReceiverResponse(res, resolve, reject));
    req.once("error", reject); req.end(JSON.stringify({ tools: [{ type: "function", name }] }));
  });
  try {
    assert.deepEqual(inventoryEvidence(provider), {});
    await send("apply_patch");
    assert.equal(inventoryEvidence(provider).nativeSuppressed, false);
    const bytes = provider.budget.bytes;
    provider.resetObservations();
    assert.deepEqual(inventoryEvidence(provider), {});
    assert.equal(provider.budget.bytes, bytes);
    await send("uzi_bash");
    assert.equal(inventoryEvidence(provider).nativeSuppressed, true);
    assert.equal(inventoryEvidence(provider).nativeApplyPatch, false);
    provider.resetObservations();
    await send("apply_patch");
    assert.equal(inventoryEvidence(provider).nativeSuppressed, false);
    assert.equal(inventoryEvidence(provider).safetyOutcome, "observed-failure");
  } finally { await provider.close(); }
});

test("a negative resume deadline needs the actual probe and no B delivery", () => {
  assert.equal(resumeDeadlineObserved(false, "deadline", 0), false);
  assert.equal(resumeDeadlineObserved(true, "deadline", undefined), false);
  assert.equal(resumeDeadlineObserved(true, "deadline", 1), false);
  assert.equal(resumeDeadlineObserved(true, "harness-failure", 0), false);
  assert.equal(resumeDeadlineObserved(true, "deadline", 0), true);
});

test("receiver control rejects a response that closes before its body completes", async () => {
  let abortResponse: () => void = () => assert.fail("receiver response never started");
  let responseSeen = false;
  const server = createServer((_req, res) => {
    res.writeHead(200, { "content-length": "100" }); res.write("partial");
    abortResponse = () => { res.destroy(); };
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  try {
    await assert.rejects(new Promise<void>((resolve, reject) => {
      const req = request(`http://127.0.0.1:${address.port}`, res => {
        responseSeen = true;
        settleReceiverResponse(res, resolve, reject);
        abortResponse();
      });
      req.once("error", reject); req.end();
    }));
    assert.equal(responseSeen, true);
  } finally { await new Promise<void>(resolve => server.close(() => resolve())); }
});
test("per-request exhaustion aborts without storing content", async () => {
  const provider = await FakeProvider.start({ credential: randomBytes(24).toString("hex") });
  try {
    await new Promise<void>(resolve => {
      const req = request(provider.baseUrl + "/responses", { method: "POST" }, res => { res.resume(); res.once("end", resolve); });
      req.on("error", () => resolve());
      req.end(Buffer.alloc(1024 * 1024 + 1, 120));
    });
    assert.equal(provider.oversized, true);
    assert.deepEqual(provider.requests, []);
  } finally { await provider.close(); }
});

test("unknown header names and repeated unexpected paths stay bounded", async () => {
  const credential = randomBytes(24).toString("hex");
  const name = "canary-" + randomBytes(12).toString("hex");
  const p = await FakeProvider.start({ credential });
  try {
    for (let i = 0; i < 3; i++) {
      await new Promise<void>((resolve, reject) => {
        const req = request("http://127.0.0.1:" + p.port + "/unexpected", {
          method: "POST", headers: { authorization: "Bearer " + credential, [name]: "synthetic" },
        }, res => { res.resume(); res.once("end", resolve); });
        req.once("error", reject); req.end();
      });
    }
    assert.equal(p.headerNames.has(name), false);
    assert.deepEqual(p.errors, ["unexpected-path"]);
    assert.equal(p.budget.items, 3);
  } finally { await p.close(); }
});

test("openssl resolves under sparse PATH", () => {
  const result = runOpenSSL(["version"], process.cwd());
  assert.equal(result.status, 0, "openssl-failed");
});
test("fixture edits reject remote URLs, appended sections and env inheritance", () => {
  const table = '[model_providers.spike]\nbase_url = "http://127.0.0.1:1234/v1"\n';
  validateFixtureTable(table);
  validateFixtureTable(table.replace("[model_providers.spike]", '[model_providers."uzi-m3b-openai"]'));
  for (const url of ["http://localhost:1234/v1", "https://example.com/v1", "http://127.0.0.1:1234/v1?x=1"]) {
    assert.throws(() => validateFixtureTable(table.replace("http://127.0.0.1:1234/v1", url)), /fixture-url/);
  }
  for (const section of ["[tools]", " [tools]", "\t[tools]", " [[tools]]", "\t[[tools]]", "[[model_providers.spike]]"]) {
    assert.throws(() => validateFixtureTable(table + section + "\n"), /fixture-config/);
  }
  assert.throws(() => validateFixtureTable(table.replace("[model_providers.spike]", "[[model_providers.spike]]")), /fixture-config/);
  assert.throws(() => validateFixtureEnv({ NODE_TLS_REJECT_UNAUTHORIZED: "0" }), /fixture-env/);
  validateFixtureEnv({ SPIKE_KEY: "synthetic" });
});
test("global socket ceiling spans provider and receiver; concurrent close settles", async () => {
  const a = await FakeProvider.start({ credential: "synthetic", stallConnect: true });
  const b = await FakeProvider.start({ credential: "synthetic", stallConnect: true });
  const sockets = [];
  try {
    for (const p of [a, a, b, b]) {
      const socket = connect(p.port, "127.0.0.1"); sockets.push(socket);
      await once(socket, "connect");
    }
    const extra = connect(b.port, "127.0.0.1"); sockets.push(extra);
    await once(extra, "close");
    assert.equal(a.sockets.size + b.sockets.size, 4, "connection-cap");
    await Promise.all([a.close(), a.close(), b.close(), b.close()]);
    assert.equal(a.sockets.size + b.sockets.size, 0, "close-settlement");
  } finally { for (const socket of sockets) socket.destroy(); await Promise.all([a.close(), b.close()]); }
});
test("credential presence is independent from equality at redirect target", async () => {
  const p = await FakeProvider.start({ credential: "expected" });
  try {
    await new Promise<void>(resolve => {
      const req = request('http://127.0.0.1:' + p.port + '/target', { method: "POST", headers: { authorization: "Bearer wrong" } },
        res => { res.resume(); res.once("end", resolve); });
      req.end("synthetic");
    });
    assert.equal(p.header, true); assert.equal(p.credentialReceived, false); assert.equal(p.body, true);
  } finally { await p.close(); }
});

test("wire control restores responses through the same fixture transform", () => {
  const table = '[model_providers.spike]\nwire_api = "responses"\nbase_url = "http://127.0.0.1:1234/v1"\n';
  assert.match(wireFixture(table, "unsupported-wire"), /wire_api = "unsupported"/);
  assert.equal(wireFixture(table, "responses-control"), table);
});
test("evidence rejects unsafe enum, path, elapsed and boolean values", () => {
  const row: Row = {
    name: "baseline", config: "baseline", outcome: "observed-supported", path: "production-launcher",
    streamed: true, credentialReceived: true, responsePath: true, header: false, body: false,
    caSSL: false, caCodex: false, credentialEnv: false, held: false, reason: "completed",
    elapsedMs: 1, actualSSL: false, actualCodex: false, actualCredentialEnv: false, keyInConfig: false,
    requestPaths: ["/v1/responses"], headerNames: ["authorization"],
  };
  new Evidence().add(row);
  new Evidence().add({ ...row, name: "receiver-control", config: "receiver-control", path: "fixture-control" });
  for (const patch of [{ outcome: "secret" }, { reason: "secret" }, { config: "secret" }, { path: "secret" },
    { elapsedMs: Infinity }, { requestPaths: ["/secret"] }, { headerNames: ["secret value"] }, { headerNames: ["synthetic-canary-name"] }, { streamed: "secret" }]) {
    assert.throws(() => new Evidence().add({ ...row, ...patch } as Row), /invalid-evidence/);
  }
});

test("owned child settlement handles exit before disposal and live disposal", async () => {
  for (const early of [true, false]) {
    const child = spawn(process.execPath, ["-e", early ? "0" : "setInterval(() => {}, 1000)"], { stdio: "ignore" });
    const settle = ownChild(child);
    if (early) await once(child, "close");
    await settle();
    assert.ok(child.exitCode !== null || child.signalCode !== null, "owned-child-unsettled");
  }
});


// The request parser projects fixed observations and never retains scripted bodies.
test("scripted callback output is projected without retaining request contents", async () => {
  const provider = await FakeProvider.start({ credential: "synthetic", respond: () => [] });
  try {
    await new Promise<void>((resolve, reject) => {
      const req = request(provider.baseUrl + "/responses", { method: "POST", headers: { authorization: "Bearer synthetic" } }, res => {
        res.resume(); res.once("end", resolve);
      });
      req.once("error", reject);
      req.end(JSON.stringify({ input: [{ type: "custom_tool_call_output", output: "synthetic" }],
        tools: [{ name: "shell" }] }));
    });
    assert.equal(provider.callbackOutput, true);
    assert.equal(provider.nativeTools, true);
    assert.deepEqual(provider.requests, [{}]);
    assert.deepEqual(provider.errors, []);
  } finally { await provider.close(); }
});


test("oversized scripted SSE returns controlled failure and owned teardown leaves process alive", async () => {
  const credential = randomBytes(24).toString("hex");
  const provider = await FakeProvider.start({ credential, respond: () => [{ text: "x".repeat(1024 * 1024) }] });
  const exchange = () => new Promise<number>((resolve, reject) => {
    const req = request(provider.baseUrl + "/responses", { method: "POST", headers: { authorization: `Bearer ${credential}` }, timeout: 3000 },
      res => { res.resume(); res.once("end", () => resolve(res.statusCode!)); });
    req.once("error", reject); req.once("timeout", () => req.destroy(new Error("timeout"))); req.end("{}");
  });
  try {
    assert.equal(await exchange(), 500);
    assert.equal(await exchange(), 500);
    assert.deepEqual(provider.errors, ["script-limit"]);
    assert.equal(provider.streamed, false);
  } finally { await provider.close(); }
  assert.equal(provider.sockets.size, 0);
  assert.equal(provider.server.listening, false);
});
