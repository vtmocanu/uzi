import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { spawn } from "node:child_process";
import {
  RETAINED_SOURCE_REQUEST_MAX_BYTES,
  RETAINED_SOURCE_RESPONSE_MAX_BYTES,
  RETAINED_SOURCE_SOCKET_RELATIVE,
} from "../src/ops/retained-source-server.js";
import { mkdtempForSocket } from "./short-sock-dir.js";

// issue #1786 M0b — the operator client is a self-contained `.mjs` that the image runs by path under
// a scrubbed environment and the image-owned node binary. These tests run the EXACT documented
// invocation form against a fixture server, with a hostile environment that would execute planted
// code on any node started without `env -i`.

const AGENT_DIR = path.resolve(import.meta.dirname, "..");
const REPO_DIR = path.resolve(AGENT_DIR, "..");
const CLIENT = path.join(AGENT_DIR, "src", "ops", "retained-source.mjs");

/** The documented invocation prefix; the runbook (docs/run-recovery.md) carries the same text. */
const DOCUMENTED_PREFIX = "/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node /app/src/ops/retained-source.mjs";

/** The documented form names /usr/bin/env by absolute path; a host without it cannot run the form. */
const ENV_SKIP = fs.existsSync("/usr/bin/env") ? false : "/usr/bin/env is absent on this host";

const scratch: string[] = [];
const servers: net.Server[] = [];
afterEach(async () => {
  for (const s of servers.splice(0)) await new Promise<void>((res) => s.close(() => res()));
  for (const d of scratch.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
function shortDir(): string {
  const d = mkdtempForSocket("rsc-");
  scratch.push(d);
  return d;
}

/** A fixture server: records the request line and answers with `respond(line)`. */
async function fixtureServer(respond: (line: string) => string | Buffer): Promise<{ sock: string; requests: string[] }> {
  const sock = path.join(shortDir(), "s.sock");
  const requests: string[] = [];
  const server = net.createServer((c) => {
    let buf = "";
    c.on("data", (d) => {
      buf += d.toString("utf8");
      const nl = buf.indexOf("\n");
      if (nl === -1) return;
      requests.push(buf.slice(0, nl));
      c.end(respond(buf.slice(0, nl)));
    });
    c.on("error", () => undefined);
  });
  servers.push(server);
  await new Promise<void>((res) => server.listen(sock, res));
  return { sock, requests };
}

interface Ran { code: number | null; stdout: string; stderr: string }
/** Run `<prefix> <args>` with `parentEnv` as the PARENT's environment (what `docker exec` would hand `env`). */
function run(command: string, args: string[], parentEnv: NodeJS.ProcessEnv): Promise<Ran> {
  const env = { ...parentEnv };
  delete env.NODE_TEST_CONTEXT; // a child node test process must not inherit the runner's context
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
}

/** The exact documented form (argv derived from DOCUMENTED_PREFIX), with the image's node path and
 *  client path replaced by this test's node binary and the source client. */
function documented(clientArgs: string[]): { command: string; args: string[] } {
  const [command, ...rest] = DOCUMENTED_PREFIX.split(" ").map((tok) =>
    tok === "/usr/local/bin/node" ? process.execPath : tok.endsWith("/ops/retained-source.mjs") ? CLIENT : tok);
  return { command: command!, args: [...rest, ...clientArgs] };
}
const RUN_ID = "11111111-2222-3333-4444-555555555555";
const ATTEMPT = "20261008T171401Z-g1-0123456789abcdef";

describe("the documented invocation", { skip: ENV_SKIP }, () => {
  it("starts with the scrubbed-environment form and the image-owned node", () => {
    assert.ok(DOCUMENTED_PREFIX.startsWith("/usr/bin/env -i PATH=/usr/local/bin:/usr/bin:/bin /usr/local/bin/node "), DOCUMENTED_PREFIX);
  });

  it("is the text the runbook carries", () => {
    const runbook = fs.readFileSync(path.join(REPO_DIR, "docs", "run-recovery.md"), "utf8");
    assert.ok(runbook.includes(DOCUMENTED_PREFIX), "docs/run-recovery.md documents the exact /usr/bin/env -i form");
  });

  it("names every binary in the runbook's Invocation commands by absolute path", () => {
    const runbook = fs.readFileSync(path.join(REPO_DIR, "docs", "run-recovery.md"), "utf8");
    const start = runbook.indexOf("- **Invocation.**");
    assert.ok(start !== -1, "the Invocation bullet exists");
    const fence = runbook.indexOf("```sh", start);
    const end = runbook.indexOf("```", fence + 5);
    assert.ok(fence !== -1 && end !== -1, "the Invocation code block exists");
    // Join backslash continuations, drop comments and blanks: one logical command per line.
    const lines = runbook.slice(fence + 5, end).replace(/\\\n\s*/g, " ").split("\n")
      .map((l) => l.trim().replace(/\s+/g, " ")).filter((l) => l.length > 0 && !l.startsWith("#"));
    const clientLines = lines.filter((l) => l.includes("retained-source.mjs"));
    assert.equal(clientLines.length, 3, "compose, single-UID and split-UID forms");
    for (const l of clientLines) {
      assert.match(l, /\/usr\/bin\/env -i PATH=\/usr\/local\/bin:\/usr\/bin:\/bin \/usr\/local\/bin\/node \/app\/src\/ops\/retained-source\.mjs /, l);
      assert.doesNotMatch(l, /(^|[\s;|&])env -i/, `bare env: ${l}`);
      assert.doesNotMatch(l, /(^|[\s;|&])setpriv /, `bare setpriv: ${l}`);
    }
    const split = clientLines.find((l) => l.includes("setpriv"));
    assert.ok(split, "the split-UID line exists");
    assert.match(split, /-- \/bin\/setpriv --reuid=10001 --regid=10001 /, split);
    assert.ok(split.includes("--no-new-privs"), split);
    assert.ok(!lines.some((l) => /(^|[\s;|&])setpriv /.test(l)), "no bare setpriv anywhere in the block");
  });

  it("sends one NDJSON request, prints the response and exits 0 on ok", async () => {
    const { sock, requests } = await fixtureServer(() => '{"ok":true,"op":"inspect","sources":[]}\n');
    const { command, args } = documented(["inspect", "--run", RUN_ID, "--generation", "none", "--attempt", "canonical", "--socket", sock]);
    const r = await run(command, args, process.env);
    assert.equal(r.code, 0, r.stderr);
    assert.deepEqual(JSON.parse(requests[0]!), { op: "inspect", run: RUN_ID, generation: "none", attempt: "canonical" });
    assert.deepEqual(JSON.parse(r.stdout), { ok: true, op: "inspect", sources: [] });
    assert.equal(requests.length, 1);
  });

  it("sends a numeric generation, the confirm digest and the protected flag for a discard", async () => {
    const { sock, requests } = await fixtureServer(() => '{"ok":true,"op":"discard","result":{"kind":"done"}}\n');
    const digest = "ab".repeat(32);
    const { command, args } = documented(["discard", "--run", RUN_ID, "--generation", "2", "--attempt", ATTEMPT, "--confirm", digest, "--protected", "--socket", sock]);
    const r = await run(command, args, process.env);
    assert.equal(r.code, 0, r.stderr);
    assert.deepEqual(JSON.parse(requests[0]!), { op: "discard", run: RUN_ID, generation: 2, attempt: ATTEMPT, confirm: digest, protected: true });
  });

  it("exits non-zero on a refusal, an unknown outcome, a malformed answer and an unreachable socket", async () => {
    const sel = ["--run", RUN_ID, "--generation", "1", "--attempt", ATTEMPT, "--confirm", "cd".repeat(32)];
    for (const body of [
      '{"ok":false,"op":"discard","result":{"kind":"refused","reason":"busy"}}\n',
      '{"ok":false,"op":"discard","result":{"kind":"unknown"}}\n',
      "not json\n",
      "",
    ]) {
      const { sock } = await fixtureServer(() => body);
      const { command, args } = documented(["discard", ...sel, "--socket", sock]);
      const r = await run(command, args, process.env);
      assert.notEqual(r.code, 0, `exit for ${JSON.stringify(body)}`);
    }
    const missing = documented(["inspect", "--socket", path.join(shortDir(), "absent.sock")]);
    const r = await run(missing.command, missing.args, process.env);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /cannot reach the worker channel/);
  });

  it("reads at most 64 KiB of a response", async () => {
    const { sock } = await fixtureServer(() => Buffer.alloc(RETAINED_SOURCE_RESPONSE_MAX_BYTES + 4096, 0x61));
    const { command, args } = documented(["inspect", "--socket", sock]);
    const r = await run(command, args, process.env);
    assert.equal(r.code, 1);
    assert.match(r.stderr, /size limit/);
  });

  it("rejects a malformed command line with exit 2 and no connection", async () => {
    const { sock, requests } = await fixtureServer(() => '{"ok":true}\n');
    for (const bad of [
      [], ["frobnicate"], ["inspect", "--run", RUN_ID], ["discard", "--run", RUN_ID, "--generation", "1", "--attempt", ATTEMPT],
      ["inspect", "--run", RUN_ID, "--generation", "x", "--attempt", ATTEMPT], ["inspect", "--bogus", "1"],
      ["inspect", "--confirm", "ab".repeat(32)], ["inspect", "--protected"],
    ]) {
      const { command, args } = documented([...bad, "--socket", sock]);
      const r = await run(command, args, process.env);
      assert.equal(r.code, 2, JSON.stringify(bad));
    }
    assert.equal(requests.length, 0);
  });
});

describe("a hostile environment cannot reach the client's process", () => {
  it("planted NODE_OPTIONS, NODE_PATH and a tsx cache directory never run under the documented form", async () => {
    const root = shortDir();
    const marker = path.join(root, "MARKER");
    const preload = path.join(root, "preload.cjs");
    fs.writeFileSync(preload, `require("node:fs").writeFileSync(${JSON.stringify(marker)}, "preload ran");\n`);
    const modules = path.join(root, "modules");
    fs.mkdirSync(path.join(modules, "node_modules"), { recursive: true });
    fs.writeFileSync(path.join(modules, "node_modules", "planted.cjs"), `require("node:fs").writeFileSync(${JSON.stringify(marker)}, "module ran");\n`);
    const tsxDir = path.join(root, `tsx-${process.getuid?.() ?? 0}`);
    fs.mkdirSync(tsxDir, { recursive: true });
    fs.writeFileSync(path.join(tsxDir, "planted.cjs"), `require("node:fs").writeFileSync(${JSON.stringify(marker)}, "tsx cache ran");\n`);
    const hostile: NodeJS.ProcessEnv = {
      ...process.env,
      NODE_OPTIONS: `--require ${preload}`,
      NODE_PATH: path.join(modules, "node_modules"),
      TMPDIR: root,
    };
    const { sock } = await fixtureServer(() => '{"ok":true}\n');

    // Positive control: without `env -i`, the same hostile environment DOES run planted code, so
    // the assertion below proves the scrub and not an inert fixture.
    const control = await run(process.execPath, [CLIENT, "inspect", "--socket", sock], hostile);
    assert.equal(control.code, 0, control.stderr);
    assert.equal(fs.existsSync(marker), true, "the planted preload runs when the environment is not scrubbed");
    fs.rmSync(marker);

    const { command, args } = documented(["inspect", "--socket", sock]);
    const r = await run(command, args, hostile);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(fs.existsSync(marker), false, "no planted code ran under env -i");
  });
});

describe("what the image ships", () => {
  it("the client imports only node: builtins", () => {
    const source = fs.readFileSync(CLIENT, "utf8");
    const specifiers = [
      ...source.matchAll(/\bfrom\s+["']([^"']+)["']/g),
      ...source.matchAll(/\bimport\s*\(\s*["']([^"']+)["']\s*\)/g),
      ...source.matchAll(/\brequire\s*\(\s*["']([^"']+)["']\s*\)/g),
      ...source.matchAll(/^\s*import\s+["']([^"']+)["']/gm),
    ].map((m) => m[1]!);
    assert.ok(specifiers.length >= 3, `parsed imports: ${specifiers.join()}`);
    for (const s of specifiers) assert.ok(s.startsWith("node:"), `non-builtin import: ${s}`);
  });

  it("duplicates the protocol constants the server exports", () => {
    const source = fs.readFileSync(CLIENT, "utf8");
    const num = (name: string): number => Number(new RegExp(`const ${name} = (\\d+);`).exec(source)?.[1]);
    const str = (name: string): string | undefined => new RegExp(`const ${name} = "([^"]+)";`).exec(source)?.[1];
    assert.equal(num("REQUEST_MAX_BYTES"), RETAINED_SOURCE_REQUEST_MAX_BYTES);
    assert.equal(num("RESPONSE_MAX_BYTES"), RETAINED_SOURCE_RESPONSE_MAX_BYTES);
    assert.equal(str("SOCKET_RELATIVE"), RETAINED_SOURCE_SOCKET_RELATIVE);
    assert.equal(str("DEFAULT_DATA_DIR"), "/data", "the client default matches config.ts UZI_DATA_DIR");
  });

  it("both image templates still copy agent/src into /app and run node:24", () => {
    for (const tpl of ["base", "jvm"]) {
      const dockerfile = fs.readFileSync(path.join(AGENT_DIR, "templates", tpl, "Dockerfile"), "utf8");
      assert.match(dockerfile, /^COPY agent\/src \.\/src$/m, `${tpl}: COPY agent/src ./src`);
      assert.match(dockerfile, /^FROM node:24-alpine@sha256:[0-9a-f]{64} AS runtime$/m, `${tpl}: node:24 runtime`);
    }
  });
});
