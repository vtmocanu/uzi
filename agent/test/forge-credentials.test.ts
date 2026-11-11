import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { beginForgeCredentialLease, resolveForgeCredential, forgeGitUsername } from "../src/forge-credentials.js";
import { gitEnv, gitBasicCredential } from "../src/git.js";
import { GitHubClient } from "../src/forge.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";

const first = "installation-token-first-canary";
const second = "installation-token-second-canary";
const username = "app[bot]";
const basic = (token: string) => Buffer.from(`${username}:${token}`).toString("base64");
function secretLog() {
  const counts = new Map<string, number>();
  return {
    counts,
    addSecret(value: string) { counts.set(value, (counts.get(value) ?? 0) + 1); },
    removeSecret(value: string) { const next = (counts.get(value) ?? 0) - 1; if (next > 0) counts.set(value, next); else counts.delete(value); },
  };
}

describe("forge installation credential rotation", () => {
  it("resolves captured claim credentials after renewal and dynamically scrubs every generation", async () => {
    const now = Date.now(); const log = secretLog();
    const lease = beginForgeCredentialLease(first, new Date(now + 3600000).toISOString(), username,
      async () => ({ forge_pat: second, expires_at: new Date(now + 7200000).toISOString() }), log);
    try {
      const captured = first;
      const text = makeTextRedactor([captured, basic(captured)]);
      const payload = makeRedactor([captured]);
      assert.equal(resolveForgeCredential(captured), first);
      assert.equal(await lease.refresh(), true);
      assert.equal(resolveForgeCredential(captured), second);
      assert.equal(forgeGitUsername(captured, username), "x-access-token");
      const networkBasic = Buffer.from(`x-access-token:${second}`).toString("base64");
      assert.equal(gitBasicCredential(captured, username), Buffer.from(`x-access-token:${first}`).toString("base64"));
      const env = gitEnv(captured, "https://github.com/", username);
      assert.ok(Object.values(env).includes(`Authorization: Basic ${networkBasic}`));
      let header = "";
      await new GitHubClient({ fetchFn: async (_url, init) => {
        header = init.headers.Authorization ?? "";
        return { status: 201, text: async () => JSON.stringify({ number: 1, html_url: "https://github.com/owner/repo/pull/1" }) };
      } }).createMergeRequest({ repoUrl: "https://github.com/owner/repo", pat: captured,
        sourceBranch: "trial", targetBranch: "main", title: "Trial", description: "Trial" });
      assert.equal(header, `Bearer ${second}`);
      assert.equal(text(networkBasic), "***REDACTED***");
      assert.equal(text(`${first} ${second} ${basic(second)}`), "***REDACTED*** ***REDACTED*** ***REDACTED***");
      assert.deepEqual(payload({ nested: [second, basic(second)] }), { nested: ["***REDACTED***", "***REDACTED***"] });
      assert.equal(log.counts.get(second), 1);
    } finally { lease.release(); }
    assert.equal(log.counts.size, 0);
  });
  it("keeps sibling registrations alive and releases secret registrations exactly once", async () => {
    const log = secretLog(); const expiry = new Date(Date.now() + 3600000).toISOString();
    const refresh = async () => ({ forge_pat: second, expires_at: expiry });
    const one = beginForgeCredentialLease(first, expiry, username, refresh, log);
    const two = beginForgeCredentialLease(first, expiry, username, refresh, log);
    try {
      one.release(); one.release();
      assert.equal(log.counts.get(first), 1);
      assert.equal(resolveForgeCredential(first), first);
      assert.equal(await two.refresh(), true);
      assert.equal(resolveForgeCredential(first), second);
    } finally { two.release(); }
    assert.equal(log.counts.size, 0);
  });
  it("coalesces concurrent renewals and ignores a late response after disposal", async () => {
    const log = secretLog(); const expiry = new Date(Date.now() + 3600000).toISOString();
    let finish!: (value: { forge_pat: string; expires_at: string }) => void;
    let calls = 0;
    const lease = beginForgeCredentialLease(first, expiry, username, () => {
      calls++; return new Promise((resolve) => { finish = resolve; });
    }, log);
    const a = lease.refresh(); const b = lease.refresh();
    assert.equal(a, b); await Promise.resolve(); assert.equal(calls, 1);
    lease.release(); finish({ forge_pat: second, expires_at: expiry });
    assert.equal(await a, false); assert.equal(log.counts.size, 0);
  });
  it("fails closed after expiry when renewal fails and never exposes provider errors", async (t) => {
    const now = Date.now(); let clock = now;
    t.mock.method(Date, "now", () => clock);
    const log = secretLog();
    const lease = beginForgeCredentialLease(first, new Date(now + 3600000).toISOString(), username,
      async () => { throw new Error(second); }, log);
    try {
      assert.equal(await lease.refresh(), false);
      assert.equal(resolveForgeCredential(first), first);
      clock += 3600000;
      assert.throws(() => resolveForgeCredential(first), /installation credential expired/);
      assert.equal(log.counts.has(second), false);
    } finally { lease.release(); }
  });
  it("rejects malformed/expired replies without replacing a still valid credential", async () => {
    const log = secretLog(); const expiry = new Date(Date.now() + 3600000).toISOString();
    const lease = beginForgeCredentialLease(first, expiry, username,
      async () => ({ forge_pat: second, expires_at: "bad" }), log);
    try {
      assert.equal(await lease.refresh(), false);
      assert.equal(resolveForgeCredential(first), first);
      assert.equal(log.counts.has(second), false);
    } finally { lease.release(); }
    assert.throws(() => beginForgeCredentialLease(first, "2000-01-01", username,
      async () => ({ forge_pat: second, expires_at: expiry }), log), /invalid forge/);
    assert.equal(resolveForgeCredential("legacy-pat-canary"), "legacy-pat-canary");
  });
});
