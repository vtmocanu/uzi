import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import http2 from "node:http2";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import { fileURLToPath } from "node:url";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

// Live stand-in for the production failure: the api resets an HTTP/2 upload stream at its write
// deadline. The server is node:http2 (the recorded run used Go net/http; the client-side chain is the
// same); the client is WorkerClient.publishCheckpoint in a child process, i.e. the production fetch
// path with its owned dispatcher. The CA is generated in-test with openssl (no key material is committed).
const fixtureUrl = new URL("./fixtures/checkpoint-publish-h2-reset.json", import.meta.url);

if (process.env.UZI_H2_RESET_CHILD === "1") {
  const client = new WorkerClient(process.env.UZI_H2_RESET_URL!, "test", "test", nullLogger());
  const pack = Readable.from((async function* () { for (let i = 0; i < 4; i++) yield Buffer.alloc(64 * 1024, 1); })());
  const chain: Array<{ constructor: string; code: string | null; message: string }> = [];
  try {
    await client.publishCheckpoint("run-h2", "a".repeat(40), pack);
    chain.push({ constructor: "none", code: null, message: "publish unexpectedly resolved" });
  } catch (e) {
    let current: unknown = e;
    for (let depth = 0; depth < 4 && current; depth++) {
      const node = current as { code?: unknown; message?: unknown; cause?: unknown };
      chain.push({ constructor: (current as object).constructor.name,
        code: typeof node.code === "string" ? node.code : null, message: String(node.message) });
      current = node.cause;
    }
  }
  process.stdout.write(JSON.stringify(chain));
  process.exit(0);
} else {
  it("an HTTP/2 stream reset during the upload surfaces the recorded error chain", async (t) => {
    const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "uzi-h2-reset-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes"];
    const openssl = (args: string[]) => execFileSync("openssl", args, { cwd: dir, stdio: ["ignore", "ignore", "pipe"] });
    openssl(["req", "-x509", ...ec, "-keyout", "ca.key", "-out", "ca.crt", "-days", "2", "-subj", "/CN=h2-reset-test-ca",
      "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign"]);
    openssl(["req", ...ec, "-keyout", "srv.key", "-out", "srv.csr", "-subj", "/CN=localhost"]);
    fs.writeFileSync(path.join(dir, "ext.cnf"), "subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n");
    openssl(["x509", "-req", "-in", "srv.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial",
      "-out", "srv.crt", "-days", "2", "-extfile", "ext.cnf"]);

    let resets = 0;
    const server = http2.createSecureServer({ key: fs.readFileSync(path.join(dir, "srv.key")), cert: fs.readFileSync(path.join(dir, "srv.crt")) });
    server.on("stream", (stream) => {
      stream.on("error", () => undefined);
      stream.resume();
      stream.on("end", () => { resets++; stream.close(http2.constants.NGHTTP2_INTERNAL_ERROR); });
    });
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    t.after(() => { server.close(); });
    const { port } = server.address() as { port: number };

    const script = fileURLToPath(import.meta.url);
    // Async spawn: the server above lives in this process and must keep serving while the child runs.
    const child = spawn(process.execPath, ["--import", "tsx", script], {
      cwd: path.resolve(path.dirname(script), ".."),
      env: { ...process.env, TMPDIR: dir, UZI_H2_RESET_CHILD: "1", UZI_H2_RESET_URL: `https://127.0.0.1:${port}`,
        NODE_EXTRA_CA_CERTS: path.join(dir, "ca.crt") },
      stdio: ["ignore", "pipe", "pipe"],
    });
    const killer = setTimeout(() => child.kill("SIGKILL"), 30_000);
    let stdout = "", stderr = "";
    child.stdout.setEncoding("utf8").on("data", d => { stdout += d; });
    child.stderr.setEncoding("utf8").on("data", d => { stderr += d; });
    const status = await new Promise<number | null>(resolve => child.on("close", resolve));
    clearTimeout(killer);
    assert.equal(status, 0, stderr || "child failed");
    assert.equal(resets, 1, "the server must have reset exactly one stream");
    const chain = JSON.parse(stdout.trim()) as Array<{ constructor: string; code: string | null; message: string }>;
    const recorded = (JSON.parse(fs.readFileSync(fixtureUrl, "utf8")) as { chain: typeof chain }).chain;
    // The live chain must be the recorded one: same classes, codes and messages, node for node.
    assert.deepEqual(chain.slice(0, recorded.length).map(n => ({ constructor: n.constructor, code: n.code, message: n.message })),
      recorded.map(n => ({ constructor: n.constructor, code: n.code, message: n.message })));
    assert.ok(chain.some(n => n.code === "ERR_HTTP2_STREAM_ERROR"), JSON.stringify(chain));
  });
}
