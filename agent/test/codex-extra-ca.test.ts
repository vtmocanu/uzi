import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { rootCertificates } from "node:tls";
import { loadConfig } from "../src/config.js";

const base = { UZI_API_URL: "http://api:8080", UZI_WORKER_TOKEN: "test-worker" };
const cert = rootCertificates[0]!;
const message = "Invalid UZI_CODEX_CA_FILE configuration";

function fixture(run: (file: string) => void) {
  // TMPDIR is supplied as an absolute worktree scratch path by the focused command.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "extra-ca-"));
  try { run(path.join(dir, "ca.pem")); }
  finally { fs.rmSync(dir, { recursive: true, force: true }); }
}

function capture(file: string) {
  return loadConfig({ ...base, UZI_CODEX_CA_FILE: file }).codexExtraCa!;
}

function rejects(file: string) {
  assert.throws(() => capture(file), (error: unknown) => {
    assert.ok(error instanceof Error);
    assert.equal(error.message, message);
    assert.equal(error.stack, message);
    assert.equal(error.cause, undefined);
    return true;
  });
}

describe("trusted startup Codex extra CA", () => {
  it("preserves single, multiple and 64 certificate bundle bytes exactly", () => fixture((file) => {
    for (const pem of [cert, "\r\n" + cert + "\n" + rootCertificates[1]! + "\t\n", (cert + "\n").repeat(64)]) {
      fs.writeFileSync(file, pem);
      assert.equal(capture(file).pem, pem);
    }
  }));

  it("bounds actual bytes, including multibyte junk and the exact 1MiB boundary", () => fixture((file) => {
    const limit = 1024 * 1024;
    const exact = cert + " ".repeat(limit - Buffer.byteLength(cert));
    fs.writeFileSync(file, exact);
    assert.equal(capture(file).pem, exact);
    for (const content of [exact + " ", cert + "é".repeat(Math.floor(limit / 2))]) {
      fs.writeFileSync(file, content);
      rejects(file);
    }
  }));

  it("rejects empty, private keys, malformed certificates, junk and count 65", () => fixture((file) => {
    for (const pem of ["", " \n", "-----BEGIN " + "PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----",
      "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----", cert + "junk",
      "junk" + cert, cert.replace("CERTIFICATE", "PUBLIC KEY"), (cert + "\n").repeat(65),
      cert.replace(/\n/, "\n!"), cert.replace(/\n-----END/, "-----END"), cert + "\u0080"]) {
      fs.writeFileSync(file, pem);
      rejects(file);
    }
  }));

  it("bounds reads by actual bytes even when file metadata understates size", () => fixture((file) => {
    fs.writeFileSync(file, cert + " ".repeat(1024 * 1024));
    const original = fs.fstatSync;
    const stub = mock.method(fs, "fstatSync", (...args: Parameters<typeof original>) => {
      const stat = original(...args);
      stat.size = typeof stat.size === "bigint" ? 0n : 0;
      return stat;
    });
    try { rejects(file); } finally { stub.mock.restore(); }
  }));

  it("sanitizes unreadable source errors", () => fixture((file) => {
    const stub = mock.method(fs, "openSync", () => {
      throw new Error("EACCES sensitive source diagnostics");
    });
    try { rejects(file); } finally { stub.mock.restore(); }
  }));

  it("fails closed for missing, blank and nonregular sources", () => fixture((file) => {
    rejects(file);
    rejects("");
    rejects(path.dirname(file));
    const link = file + ".link";
    fs.symlinkSync(file, link);
    rejects(link);
  }));

  it("accepts managed symlinks and snapshots bytes without rebinding env or files", () => fixture((file) => {
    fs.writeFileSync(file, cert);
    const link = file + ".link";
    fs.symlinkSync(file, link);
    const env = { ...base, UZI_CODEX_CA_FILE: link };
    const captured = loadConfig(env).codexExtraCa!;
    assert.equal(captured.pem, cert);
    assert.ok(Object.isFrozen(captured));
    assert.throws(() => { (captured as { pem: string }).pem = "replacement"; }, TypeError);
    fs.writeFileSync(file, "invalid replacement");
    env.UZI_CODEX_CA_FILE = "another-source";
    fs.unlinkSync(link);
    assert.equal(captured.pem, cert);
    rejects(file);
  }));

  it("ignores CLI and Node CA sources, even when dedicated configuration is invalid", () => fixture((file) => {
    fs.writeFileSync(file, cert);
    const alternate = { ...base, UZI_CLI_CA_FILE: file, NODE_EXTRA_CA_CERTS: file };
    assert.equal(loadConfig(alternate).codexExtraCa, undefined);
    assert.throws(() => loadConfig({ ...alternate, UZI_CODEX_CA_FILE: file + ".missing" }), { message });
    assert.equal(loadConfig(base).codexExtraCa, undefined);
  }));
});
