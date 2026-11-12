import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { CredentialHolder } from "../src/credential-holder.js";
import { inspect } from "node:util";

describe("CredentialHolder", () => {
  it("starts at the base epoch with the seed token", () => {
    const h = new CredentialHolder({ token: "tok-base", ordinal: 0 });
    assert.deepEqual(h.current(), { token: "tok-base", ordinal: 0 });
  });

  it("publishes strictly increasing ordinals and earlier snapshots never change", () => {
    const h = new CredentialHolder({ token: "tok-base", ordinal: 0 });
    const first = h.current();
    h.publish({ token: "tok-one", ordinal: 1 });
    const second = h.current();
    h.publish({ token: "tok-three", ordinal: 3 });
    assert.deepEqual(first, { token: "tok-base", ordinal: 0 });
    assert.deepEqual(second, { token: "tok-one", ordinal: 1 });
    assert.deepEqual(h.current(), { token: "tok-three", ordinal: 3 });
  });

  it("snapshots are frozen", () => {
    const h = new CredentialHolder({ token: "tok-base", ordinal: 0 });
    const snap = h.current() as { token: string; ordinal: number };
    assert.ok(Object.isFrozen(snap));
    assert.throws(() => { snap.token = "other"; }, TypeError);
    h.publish({ token: "tok-one", ordinal: 1 });
    assert.ok(Object.isFrozen(h.current()));
  });

  it("refuses a non-increasing ordinal, an unsafe ordinal and an empty token, keeping the current credential", () => {
    const h = new CredentialHolder({ token: "tok-base", ordinal: 0 });
    h.publish({ token: "tok-two", ordinal: 2 });
    for (const bad of [
      { token: "x", ordinal: 2 },
      { token: "x", ordinal: 1 },
      { token: "x", ordinal: 2.5 },
      { token: "x", ordinal: Number.NaN },
      { token: "x", ordinal: Number.MAX_SAFE_INTEGER + 2 },
      { token: "", ordinal: 3 },
    ]) {
      assert.throws(() => h.publish(bad));
    }
    assert.deepEqual(h.current(), { token: "tok-two", ordinal: 2 });
  });

  it("refuses an empty seed token", () => {
    assert.throws(() => new CredentialHolder({ token: "", ordinal: 0 }));
  });
});

describe("CredentialHolder redaction", () => {
  const TOKEN = "tok-" + "secret-value";
  it("keeps the token out of JSON and inspect output for the holder and its snapshots", () => {
    const h = new CredentialHolder({ token: TOKEN, ordinal: 0 });
    const snap = h.current();
    for (const out of [JSON.stringify(h), inspect(h), JSON.stringify(snap), inspect(snap), JSON.stringify({ h, snap })]) {
      assert.ok(!out.includes(TOKEN), out);
    }
    assert.deepEqual(JSON.parse(JSON.stringify(h)), { ordinal: 0, token: "[redacted]" });
    assert.deepEqual(JSON.parse(JSON.stringify(snap)), { ordinal: 0, token: "[redacted]" });
    h.publish({ token: TOKEN + "-2", ordinal: 1 });
    assert.ok(!JSON.stringify(h.current()).includes(TOKEN));
    assert.ok(!inspect(h.current()).includes(TOKEN));
  });

  it("still returns the real token from current().token", () => {
    const h = new CredentialHolder({ token: TOKEN, ordinal: 0 });
    assert.equal(h.current().token, TOKEN);
  });
});
