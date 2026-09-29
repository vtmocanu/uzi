// PRD #1906 M4: the in-process fetch tool against a local fake uzi-fetcher over real TLS.
// The CAs and server certificates are generated in-test with the openssl CLI (no key
// material is committed): one CA the tool trusts, and a second, untrusted one.

import { after, before, beforeEach, afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import https from "node:https";
import net, { type AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";

import { fetchIntoWorkspace, FETCH_TOOL_QUALIFIED, type FetchToolDeps } from "../src/fetch-tools.js";
import { nullLogger } from "./helpers.js";

const CREDENTIAL = "fetch-cred-" + "test-only-0123456789";

interface Pki {
  trustedCa: string;
  goodKey: string;
  goodCert: string;
  rogueKey: string;
  rogueCert: string;
}

function openssl(dir: string, args: string[]): void {
  execFileSync("openssl", args, { cwd: dir, stdio: ["ignore", "ignore", "pipe"] });
}

/** A CA, and a localhost/127.0.0.1 server certificate it signs. */
function makeCaAndServer(dir: string, name: string): { ca: string; key: string; cert: string } {
  const ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes"];
  openssl(dir, ["req", "-x509", ...ec, "-keyout", `${name}-ca.key`, "-out", `${name}-ca.crt`, "-days", "2", "-subj", `/CN=${name}-test-ca`,
    "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign"]);
  openssl(dir, ["req", ...ec, "-keyout", `${name}-srv.key`, "-out", `${name}-srv.csr`, "-subj", "/CN=localhost"]);
  fs.writeFileSync(path.join(dir, `${name}-ext.cnf`), "subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n");
  openssl(dir, ["x509", "-req", "-in", `${name}-srv.csr`, "-CA", `${name}-ca.crt`, "-CAkey", `${name}-ca.key`, "-CAcreateserial",
    "-out", `${name}-srv.crt`, "-days", "2", "-extfile", `${name}-ext.cnf`]);
  const read = (f: string): string => fs.readFileSync(path.join(dir, f), "utf8");
  return { ca: read(`${name}-ca.crt`), key: read(`${name}-srv.key`), cert: read(`${name}-srv.crt`) };
}

type Handler = (req: { auth?: string; body: string }, res: import("node:http").ServerResponse) => void;

/** A fake fetcher: records every HTTP request it receives, answers with `handler`. */
async function fakeFetcher(key: string, cert: string, handler: Handler): Promise<{ url: string; requests: Array<{ auth?: string; body: string }>; close: () => Promise<void> }> {
  const requests: Array<{ auth?: string; body: string }> = [];
  const server = https.createServer({ key, cert }, (req, res) => {
    let body = "";
    req.on("data", (c: Buffer) => (body += c.toString("utf8")));
    req.on("end", () => {
      const r = { auth: req.headers.authorization, body };
      requests.push(r);
      handler(r, res);
    });
  });
  server.on("tlsClientError", () => undefined);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `https://localhost:${port}`,
    requests,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

function sha(b: Buffer | string): string {
  return createHash("sha256").update(b).digest("hex");
}

let pkiDir: string;
let pki: Pki;
let workspace: string;

before(() => {
  pkiDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-fetch-pki-"));
  const good = makeCaAndServer(pkiDir, "good");
  const rogue = makeCaAndServer(pkiDir, "rogue");
  pki = { trustedCa: good.ca, goodKey: good.key, goodCert: good.cert, rogueKey: rogue.key, rogueCert: rogue.cert };
});
after(() => fs.rmSync(pkiDir, { recursive: true, force: true }));
beforeEach(() => {
  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-fetch-ws-"));
});
afterEach(() => fs.rmSync(workspace, { recursive: true, force: true }));

function deps(url: string, over: Partial<FetchToolDeps> = {}): FetchToolDeps {
  return { fetcherUrl: url, ca: pki.trustedCa, credential: CREDENTIAL, workspace, log: nullLogger(), timeoutMs: 10_000, ...over };
}

function sources(): string[] {
  const d = path.join(workspace, "sources");
  return fs.existsSync(d) ? fs.readdirSync(d) : [];
}

describe("fetch tool (uzi_fetch / fetch_url)", () => {
  it("names the qualified tool the isolated tool set lists", () => {
    assert.equal(FETCH_TOOL_QUALIFIED, "mcp__uzi_fetch__fetch_url");
  });

  it("saves the body as sources/<computed sha256> and returns its metadata", async () => {
    const body = Buffer.from("%PDF-1.7 official datasheet bytes");
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, {
        "Content-Type": "application/pdf",
        "X-Uzi-Final-Url": "https://docs.example.com/guide.pdf",
        "X-Uzi-Sha256": sha(body),
        "X-Uzi-Bytes": String(body.length),
      });
      res.end(body);
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url), "https://docs.example.com/guide.pdf");
      assert.equal(out.ok, true, JSON.stringify(out));
      if (!out.ok) return;
      assert.equal(out.sha256, sha(body));
      assert.equal(out.path, `sources/${sha(body)}`);
      assert.equal(out.bytes, body.length);
      assert.equal(out.content_type, "application/pdf");
      assert.equal(out.final_url, "https://docs.example.com/guide.pdf");
      assert.deepEqual(sources(), [sha(body)], "the file name is the computed sha, never a name from the URL");
      assert.deepEqual(fs.readFileSync(path.join(workspace, out.path)), body);
      assert.equal(f.requests.length, 1);
      assert.equal(f.requests[0]!.auth, `Bearer ${CREDENTIAL}`);
      assert.deepEqual(JSON.parse(f.requests[0]!.body), { url: "https://docs.example.com/guide.pdf" });
    } finally {
      await f.close();
    }
  });

  it("refuses a body whose sha256 does not match X-Uzi-Sha256 and keeps no file", async () => {
    const body = Buffer.from("the real bytes");
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, { "X-Uzi-Final-Url": "https://a.example/x", "X-Uzi-Sha256": sha("other bytes"), "X-Uzi-Bytes": String(body.length) });
      res.end(body);
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url), "https://a.example/x");
      assert.equal(out.ok, false);
      assert.equal(!out.ok && out.reason, "sha_mismatch");
      assert.deepEqual(sources(), [], "no file, not even a partial, is left");
    } finally {
      await f.close();
    }
  });

  it("refuses a body whose length does not match X-Uzi-Bytes", async () => {
    const body = Buffer.from("twelve bytes");
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, { "X-Uzi-Final-Url": "https://a.example/x", "X-Uzi-Sha256": sha(body), "X-Uzi-Bytes": String(body.length + 5) });
      res.end(body);
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url), "https://a.example/x");
      assert.equal(!out.ok && out.reason, "size_mismatch");
      assert.deepEqual(sources(), []);
    } finally {
      await f.close();
    }
  });

  it("surfaces the fetcher's refusal reason, status and admission reason", async () => {
    let n = 0;
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      n++;
      if (n === 1) {
        res.writeHead(403, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "host is not on the site list", reason: "off_list" }));
      } else {
        res.writeHead(429, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "run total reached", reason: "admission_refused", admission_reason: "run_bytes" }));
      }
    });
    try {
      const off = await fetchIntoWorkspace(deps(f.url), "https://evil.example/");
      assert.equal(off.ok, false);
      if (off.ok) return;
      assert.equal(off.reason, "off_list");
      assert.equal(off.status, 403);
      assert.match(off.message ?? "", /not on the site list/);
      const adm = await fetchIntoWorkspace(deps(f.url), "https://a.example/");
      assert.ok(!adm.ok && adm.reason === "admission_refused" && adm.admission_reason === "run_bytes" && adm.status === 429);
      assert.deepEqual(sources(), []);
    } finally {
      await f.close();
    }
  });

  it("refuses a fetcher whose certificate does not chain to the configured CA, before sending the credential", async () => {
    const f = await fakeFetcher(pki.rogueKey, pki.rogueCert, (_r, res) => {
      res.writeHead(200);
      res.end("x");
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url), "https://a.example/");
      assert.equal(out.ok, false);
      assert.equal(!out.ok && out.reason, "fetcher_tls");
      assert.equal(f.requests.length, 0, "the fake fetcher never received a request, so never saw the credential");
    } finally {
      await f.close();
    }
  });

  it("trusts only the configured CA: a valid fetcher certificate from another CA is refused", async () => {
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200);
      res.end("x");
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url, { ca: pki.rogueCert }), "https://a.example/");
      assert.equal(!out.ok && out.reason, "fetcher_tls");
      assert.equal(f.requests.length, 0);
    } finally {
      await f.close();
    }
  });

  it("refuses a non-https fetcher URL without connecting", async () => {
    const out = await fetchIntoWorkspace(deps("http://127.0.0.1:1"), "https://a.example/");
    assert.equal(!out.ok && out.reason, "fetcher_misconfigured");
  });

  it("the run's abort signal abandons an in-flight fetch and keeps no file", async () => {
    const body = Buffer.from("x".repeat(1000));
    const run = new AbortController();
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, { "X-Uzi-Final-Url": "https://a.example/x", "X-Uzi-Sha256": sha(body), "X-Uzi-Bytes": String(body.length) });
      res.write(body.subarray(0, 10)); // ...and never the rest: the fetch hangs until aborted
      setTimeout(() => run.abort(), 50);
    });
    try {
      const started = Date.now();
      const out = await fetchIntoWorkspace(deps(f.url, { signal: run.signal }), "https://a.example/x");
      assert.equal(!out.ok && out.reason, "cancelled", JSON.stringify(out));
      assert.ok(Date.now() - started < 5_000, "returned on the abort, not on a timeout");
      assert.deepEqual(sources(), [], "no file, not even a partial, is left");
    } finally {
      await f.close();
    }
  });

  it("an already-aborted signal sends nothing", async () => {
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => res.end());
    try {
      const out = await fetchIntoWorkspace(deps(f.url, { signal: AbortSignal.abort() }), "https://a.example/x");
      assert.equal(!out.ok && out.reason, "cancelled");
      assert.equal(f.requests.length, 0);
    } finally {
      await f.close();
    }
  });

  it("a trickling body cannot outlive the total deadline (the socket idle timer alone never fires)", async () => {
    const body = Buffer.from("y".repeat(60));
    let timer: NodeJS.Timeout | undefined;
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, { "X-Uzi-Final-Url": "https://a.example/x", "X-Uzi-Sha256": sha(body), "X-Uzi-Bytes": String(body.length) });
      let i = 0;
      timer = setInterval(() => {
        if (res.destroyed || i >= body.length) {
          clearInterval(timer);
          if (!res.destroyed) res.end();
          return;
        }
        res.write(body.subarray(i, i + 1));
        i++;
      }, 25);
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url, { timeoutMs: 400 }), "https://a.example/x");
      assert.equal(!out.ok && out.reason, "fetcher_timeout", JSON.stringify(out));
      assert.deepEqual(sources(), []);
    } finally {
      clearInterval(timer);
      await f.close();
    }
  });

  it("a fetcher that accepts TCP but never completes TLS is reported fetcher_unreachable at the connect bound", async () => {
    const sockets = new Set<net.Socket>();
    const server = net.createServer((s) => {
      sockets.add(s);
      s.on("error", () => undefined);
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const port = (server.address() as AddressInfo).port;
    try {
      const started = Date.now();
      const out = await fetchIntoWorkspace(deps(`https://localhost:${port}`, { timeoutMs: 10_000, connectTimeoutMs: 200 }), "https://a.example/x");
      assert.equal(!out.ok && out.reason, "fetcher_unreachable", JSON.stringify(out));
      assert.ok(Date.now() - started < 5_000, "returned at the connect bound, not the total deadline");
      assert.deepEqual(sources(), []);
    } finally {
      for (const s of sockets) s.destroy();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });

  it("never recreates sources/ (or the workspace) once the run directory is gone", async () => {
    const body = Buffer.from("late bytes");
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, { "X-Uzi-Final-Url": "https://a.example/x", "X-Uzi-Sha256": sha(body), "X-Uzi-Bytes": String(body.length) });
      res.end(body);
    });
    try {
      fs.rmSync(workspace, { recursive: true, force: true });
      const out = await fetchIntoWorkspace(deps(f.url), "https://a.example/x");
      assert.equal(out.ok, false);
      assert.equal(!out.ok && out.reason, "workspace_error");
      assert.ok(!fs.existsSync(workspace), "the removed workspace was not recreated");
    } finally {
      await f.close();
    }
  });

  it("escapes control characters in site-controlled metadata", async () => {
    const body = Buffer.from("b");
    const f = await fakeFetcher(pki.goodKey, pki.goodCert, (_r, res) => {
      res.writeHead(200, {
        "Content-Type": "text/html",
        "X-Uzi-Final-Url": "https://a.example/\u009bevil",
        "X-Uzi-Sha256": sha(body),
        "X-Uzi-Bytes": "1",
      });
      res.end(body);
    });
    try {
      const out = await fetchIntoWorkspace(deps(f.url), "https://a.example/");
      assert.ok(out.ok);
      assert.equal(out.ok && out.final_url, "https://a.example/\\u009bevil");
    } finally {
      await f.close();
    }
  });
});
