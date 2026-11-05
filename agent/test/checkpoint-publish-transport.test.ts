import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import https from "node:https";
import http2 from "node:http2";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { Readable, type Duplex } from "node:stream";
import { it, mock } from "node:test";
import { fileURLToPath } from "node:url";
import { Agent, getGlobalDispatcher } from "undici";
import { checkpointPublishDispatcher } from "../src/checkpoint-publish-transport.js";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40);
const ACK = { published: true, ref: "refs/uzi-checkpoints/test" };
const pack = () => Readable.from(["pack"]);

if (process.env.UZI_PUBLISH_TRANSPORT_CHILD) {
  const url = process.env.UZI_PUBLISH_TRANSPORT_URL!;
  const original = getGlobalDispatcher();
  const transportLimit = async (phase: string) => {
    // Scale the production 300 s transport limit down to 200 ms, with the server waiting 2.5 s
    // (undici's fast timers tick coarsely). No signal: the specific transport timeout must fire.
    const scaled = checkpointPublishDispatcher(200);
    try {
      await assert.rejects(async () => {
        const response = await fetch(`${url}/${phase}`, { dispatcher: scaled } as RequestInit);
        await response.text();
      }, (error: { cause?: { code?: string } }) =>
        error.cause?.code === (phase === "headers" ? "UND_ERR_HEADERS_TIMEOUT" : "UND_ERR_BODY_TIMEOUT"));
    } finally { await scaled.destroy(); }
  };
  // No Headers construction, warm-up fetch, or global dispatcher replacement before the first
  // publish. Exercise the package's public Dispatcher API against Node's BUNDLED fetch, not npm fetch.
  if (process.env.UZI_PUBLISH_TRANSPORT_CHILD === "proxy-limits") {
    for (const phase of ["headers", "body"]) await transportLimit(phase);
  } else if (process.env.UZI_PUBLISH_TRANSPORT_CHILD === "timeouts") {
    const agents = new Set<Agent>();
    const dispatch = Agent.prototype.dispatch;
    mock.method(Agent.prototype, "dispatch", function (this: Agent, ...args: Parameters<Agent["dispatch"]>) {
      agents.add(this);
      return dispatch.apply(this, args);
    });
    const client = new WorkerClient(url, "test", "test", nullLogger(), {
      httpTimeoutMs: 100, checkpointPublishTimeoutMs: 6_000,
    });
    for (const phase of ["headers", "body"]) {
      assert.deepEqual(await client.publishCheckpoint(phase, TIP, pack()), { ok: true, body: ACK });
      assert.equal(agents.size, phase === "headers" ? 1 : 3, "publish uses its own Agent");
      assert.ok([...agents].every(a => a.destroyed), "publish returns only after its pool is destroyed");
      await transportLimit(phase);
    }
    for (const mode of ["status", "invalid", "abort"]) {
      const result = client.publishCheckpoint(mode, TIP, pack(), mode === "abort" ? AbortSignal.timeout(200) : undefined);
      if (mode === "status") assert.deepEqual(await result, { ok: false, httpStatus: 503 });
      else await assert.rejects(result);
      assert.ok([...agents].every(a => a.destroyed), `pool destroyed after ${mode}`);
    }
    assert.equal(agents.size, 7, "each publish/control has exactly one fresh pool");
    mock.restoreAll();
  } else {
    const client = new WorkerClient(url, "test", "test", nullLogger(), { checkpointPublishTimeoutMs: 5_000 });
    if (process.env.UZI_PUBLISH_TRANSPORT_CHILD === "untrusted") {
      const untrusted = (e: { cause?: { code?: string } }) =>
        ["UNABLE_TO_VERIFY_LEAF_SIGNATURE", "SELF_SIGNED_CERT_IN_CHAIN"].includes(e.cause?.code ?? "");
      await assert.rejects(fetch(url), untrusted);
      await assert.rejects(client.publishCheckpoint("quick", TIP, pack()), untrusted);
    } else {
      assert.deepEqual(await client.publishCheckpoint("quick", TIP, pack()), { ok: true, body: ACK });
      assert.deepEqual(await fetch(url).then(r => r.json()), ACK);
    }
  }
  assert.equal(getGlobalDispatcher(), original, "publish does not replace the global dispatcher");
  process.stdout.write("transport passed\n");
} else {
  it("checkpoint transport uses native fetch on h1/h2, owns its pools, and honours TLS and proxy opt-in", async (t) => {
    const dir = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "checkpoint-transport-"));
    t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
    const ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes"];
    const openssl = (args: string[]) => execFileSync("openssl", args, { cwd: dir, stdio: ["ignore", "ignore", "pipe"] });
    openssl(["req", "-x509", ...ec, "-keyout", "ca.key", "-out", "ca.crt", "-days", "2", "-subj", "/CN=publish-test-ca",
      "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign"]);
    openssl(["req", ...ec, "-keyout", "srv.key", "-out", "srv.csr", "-subj", "/CN=localhost"]);
    fs.writeFileSync(path.join(dir, "ext.cnf"), "subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n");
    openssl(["x509", "-req", "-in", "srv.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial",
      "-out", "srv.crt", "-days", "2", "-extfile", "ext.cnf"]);
    const tls = { key: fs.readFileSync(path.join(dir, "srv.key")), cert: fs.readFileSync(path.join(dir, "srv.crt")) };
    const script = fileURLToPath(import.meta.url);
    const child = async (url: string, mode: string, env: NodeJS.ProcessEnv = {}, flags: string[] = []) => {
      const clean = { ...process.env };
      // Deterministic fixture proxy settings; preserve machine caps and other worker-like settings.
      for (const key of ["HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "NODE_USE_ENV_PROXY"]) delete clean[key];
      const proc = spawn(process.execPath, [...flags, "--import", "tsx", script], {
        cwd: path.resolve(path.dirname(script), ".."),
        env: { ...clean, NODE_EXTRA_CA_CERTS: path.join(dir, "ca.crt"), ...env,
          NODE_OPTIONS: `${clean.NODE_OPTIONS ?? ""} ${env.NODE_OPTIONS ?? ""}`,
          UZI_PUBLISH_TRANSPORT_CHILD: mode, UZI_PUBLISH_TRANSPORT_URL: url },
        stdio: ["ignore", "pipe", "pipe"],
      });
      const killer = setTimeout(() => proc.kill("SIGKILL"), 35_000);
      let stdout = "", stderr = "";
      proc.stdout.setEncoding("utf8").on("data", d => { stdout += d; });
      proc.stderr.setEncoding("utf8").on("data", d => { stderr += d; });
      const status = await new Promise<number | null>((resolve, reject) => { proc.on("close", resolve); proc.on("error", reject); });
      clearTimeout(killer);
      assert.equal(status, 0, stderr || "child timed out or failed");
      assert.equal(stdout.trim(), "transport passed");
    };
    const sessions = new Set<http2.ServerHttp2Session>();
    const sockets = new Set<Duplex>();
    const timers = new Set<NodeJS.Timeout>();
    const versions: string[] = [];
    const handler = (req: http.IncomingMessage | http2.Http2ServerRequest,
      res: { destroyed: boolean; statusCode: number; end(): unknown; end(chunk: string): unknown; write(chunk: string): unknown }) => {
      versions.push(req.httpVersion);
      req.resume();
      req.on("end", () => {
        if (req.url?.includes("abort")) return;
        if (req.url?.includes("status")) { res.statusCode = 503; res.end(); return; }
        if (req.url?.includes("invalid")) { res.end("{"); return; }
        const delayed = req.url?.includes("headers") || req.url?.includes("body");
        const body = req.url?.includes("body");
        if (body) res.write('{"published":');
        const finish = () => { if (!res.destroyed) res.end(body ? 'true,"ref":"refs/uzi-checkpoints/test"}' : JSON.stringify(ACK)); };
        if (!delayed) finish();
        else {
          const timer = setTimeout(() => { timers.delete(timer); finish(); }, 2_500);
          timers.add(timer);
        }
      });
    };
    const h1 = https.createServer(tls, handler);
    const h2 = http2.createSecureServer({ ...tls, allowHTTP1: true }, handler);
    const plain = http.createServer(handler);
    h2.on("session", session => { sessions.add(session); session.on("close", () => sessions.delete(session)); });
    for (const server of [h1, h2, plain]) {
      server.on("connection", socket => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
      await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    }
    t.after(() => {
      for (const timer of timers) clearTimeout(timer);
      for (const session of sessions) session.destroy();
      for (const socket of sockets) socket.destroy();
      h1.close(); h2.close(); plain.close();
    });
    const url = (server: typeof h1 | typeof h2) => `https://127.0.0.1:${(server.address() as net.AddressInfo).port}`;
    for (const [server, version] of [[h1, "1.1"], [h2, "2.0"]] as const) {
      await t.test(`fresh native-fetch ${version}: header/body limits and cleanup`, async () => {
        versions.length = 0;
        await child(url(server), "timeouts");
        assert.equal(versions.length, 7);
        assert.ok(versions.every(v => v === version), JSON.stringify(versions));
      });
    }
    await t.test("untrusted CA is rejected by native fetch and publish", () => child(url(h1), "untrusted", { NODE_EXTRA_CA_CERTS: "" }));
    let connects = 0;
    const proxy = http.createServer();
    proxy.on("connection", socket => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
    proxy.on("request", (req, res) => {
      connects++;
      const target = new URL(req.url!);
      const plainPort = (plain.address() as net.AddressInfo).port;
      assert.equal(target.hostname, "127.0.0.1");
      assert.equal(Number(target.port), plainPort);
      // Forward only to the fixture's own server: host and port are fixed, the request supplies the path.
      const upstream = http.request({ host: "127.0.0.1", port: plainPort, path: target.pathname + target.search,
        method: req.method, headers: req.headers }, response => {
        res.writeHead(response.statusCode!, response.headers);
        response.pipe(res);
      });
      upstream.on("error", () => res.destroy());
      req.pipe(upstream);
    });
    proxy.on("connect", (req, socket, head) => {
      connects++;
      const port = Number(new URL(`http://${req.url}`).port);
      assert.ok([h1, plain].some(server => (server.address() as net.AddressInfo).port === port));
      const upstream = net.connect(port, "127.0.0.1", () => {
        socket.write("HTTP/1.1 200 Connection Established\r\n\r\n");
        upstream.write(head);
        upstream.pipe(socket); socket.pipe(upstream);
      });
      sockets.add(socket); sockets.add(upstream);
      socket.on("close", () => { sockets.delete(socket); upstream.destroy(); });
      upstream.on("close", () => { sockets.delete(upstream); socket.destroy(); });
      socket.on("error", () => upstream.destroy()); upstream.on("error", () => socket.destroy());
    });
    await new Promise<void>(resolve => proxy.listen(0, "127.0.0.1", resolve));
    t.after(() => proxy.close());
    const proxyUrl = `http://127.0.0.1:${(proxy.address() as net.AddressInfo).port}`;
    const proxyEnv = { HTTPS_PROXY: proxyUrl, HTTP_PROXY: proxyUrl };
    await t.test("HTTP proxy forwarding preserves both transport limits", async () => {
      connects = 0;
      await child(`http://127.0.0.1:${(plain.address() as net.AddressInfo).port}`, "proxy-limits",
        { ...proxyEnv, NODE_USE_ENV_PROXY: "1" });
      assert.equal(connects, 2);
    });
    const enabled = [
      { name: "environment", env: { NODE_USE_ENV_PROXY: "1" }, flags: [] },
      { name: "CLI", env: {}, flags: ["--use-env-proxy"] },
      { name: "NODE_OPTIONS quotes/escapes", env: { NODE_OPTIONS: '"--use-env-\\proxy"' }, flags: [] },
    ];
    for (const setting of enabled) {
      await t.test(`proxy opt-in and NO_PROXY via ${setting.name}`, async () => {
        for (const bypass of [false, true]) {
          connects = 0;
          const env = { ...proxyEnv, ...setting.env, ...(bypass ? { NO_PROXY: "127.0.0.1" } : {}) };
          await child(url(h1), "quick", env, setting.flags);
          assert.equal(connects, bypass ? 0 : 2, "native fetch and publish use the same TLS proxy route");
          connects = 0;
          await child(`http://127.0.0.1:${(plain.address() as net.AddressInfo).port}`, "quick", env, setting.flags);
          assert.equal(connects, bypass ? 0 : 2, "native fetch and publish use the same HTTP proxy route");
        }
      });
    }
    await t.test("proxy flag precedence, normalization and quoting match native fetch", async () => {
      const cases = [
        { env: {}, flags: [], count: 0 },
        { env: { NODE_USE_ENV_PROXY: "1", NODE_OPTIONS: "--no-use-env-proxy" }, flags: [], count: 0 },
        { env: { NODE_OPTIONS: "--use-env-proxy" }, flags: ["--no-use-env-proxy"], count: 0 },
        { env: { NODE_OPTIONS: "--no-use-env-proxy" }, flags: ["--use-env-proxy"], count: 2 },
        { env: { NODE_OPTIONS: '--title="not a --use-env-proxy flag"' }, flags: [], count: 0 },
        { env: { NODE_OPTIONS: "--use-env-proxy --no-use-env-proxy" }, flags: [], count: 0 },
        { env: {}, flags: ["--use_env_proxy=false"], count: 2 },
      ];
      for (const setting of cases) {
        connects = 0;
        await child(url(h1), "quick", { ...proxyEnv, ...setting.env }, setting.flags);
        assert.equal(connects, setting.count);
      }
    });
  });
}
