// PRD #1976: a profile-bound job end to end on the isolated lane. A claim carrying a job block
// AND an `isolated_fetch` grant is dispatched by the Worker to the JobRunner's lane mode, which
// downloads the input file, runs a scripted session that fetches through the REAL in-process fetch
// tool against a fake uzi-fetcher over real TLS (one allowed URL, one the fetcher refuses as
// off_list), writes an output, submits a result, uploads the output, posts the result and reports
// completed. The scripted session stands in for the CLI: it emits the lane surface as its init frame
// (which the production init check verifies) and calls the MCP tool handlers directly, so the
// PreToolUse tool gate is built into the options but NOT consulted here; that gate's allow/deny
// behaviour is covered in job-runner.test.ts (the lane tool gate test).

import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import https from "node:https";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

import type { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import { ISOLATED_TOOLS } from "../src/isolated-executor.js";
import { JobRunner } from "../src/job-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ChatClaimResponse, ClaimResponse, JobFileUploadMeta, JobResultRequest, StateRequest } from "../src/protocol.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { RunRunner } from "../src/runner.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { Worker } from "../src/worker.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// Assembled at runtime so no complete token-shaped literal sits in the source.
const CRED = "lane-e2e-cred-" + "0123456789abcdef";
const SUBMIT = "mcp__job__submit_job_result";
const sha = (b: Buffer | string): string => createHash("sha256").update(b).digest("hex");

let pkiDir: string;
let pki: { ca: string; key: string; cert: string };

function openssl(args: string[]): void {
  execFileSync("openssl", args, { cwd: pkiDir, stdio: ["ignore", "ignore", "pipe"] });
}

before(() => {
  pkiDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-lane-pki-"));
  const ec = ["-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes"];
  openssl(["req", "-x509", ...ec, "-keyout", "ca.key", "-out", "ca.crt", "-days", "2", "-subj", "/CN=lane-test-ca",
    "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign"]);
  openssl(["req", ...ec, "-keyout", "srv.key", "-out", "srv.csr", "-subj", "/CN=localhost"]);
  fs.writeFileSync(path.join(pkiDir, "ext.cnf"), "subjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=CA:FALSE\nextendedKeyUsage=serverAuth\n");
  openssl(["x509", "-req", "-in", "srv.csr", "-CA", "ca.crt", "-CAkey", "ca.key", "-CAcreateserial", "-out", "srv.crt", "-days", "2", "-extfile", "ext.cnf"]);
  const read = (f: string): string => fs.readFileSync(path.join(pkiDir, f), "utf8");
  pki = { ca: read("ca.crt"), key: read("srv.key"), cert: read("srv.crt") };
});
after(() => fs.rmSync(pkiDir, { recursive: true, force: true }));

/** A fake uzi-fetcher over real TLS: serves docs.example.com, refuses every other host as off_list. */
async function fakeFetcher(
  onRequest: (outcome: "allowed" | "refused", url: string, auth: string | undefined) => void,
  page: Buffer,
): Promise<{ url: string; close: () => Promise<void> }> {
  const server = https.createServer({ key: pki.key, cert: pki.cert }, (req, res) => {
    let body = "";
    req.on("data", (c: Buffer) => (body += c.toString("utf8")));
    req.on("end", () => {
      const target = (JSON.parse(body) as { url: string }).url;
      if (new URL(target).hostname === "docs.example.com") {
        onRequest("allowed", target, req.headers.authorization);
        res.writeHead(200, { "Content-Type": "text/html", "X-Uzi-Final-Url": target, "X-Uzi-Sha256": sha(page), "X-Uzi-Bytes": String(page.length) });
        res.end(page);
      } else {
        onRequest("refused", target, req.headers.authorization);
        res.writeHead(403, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ error: "host is not on the site list", reason: "off_list" }));
      }
    });
  });
  server.on("tlsClientError", () => undefined);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    url: `https://localhost:${(server.address() as AddressInfo).port}`,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

type Handler = (args: unknown, extra: unknown) => Promise<{ isError?: boolean; content: Array<{ text?: string }> }>;
function toolHandler(options: SdkOptions, server: string, tool: string): Handler {
  const servers = options.mcpServers as unknown as Record<string, { instance: { _registeredTools: Record<string, { handler: Handler }> } }>;
  return servers[server]!.instance._registeredTools[tool]!.handler;
}

interface Harness {
  order: string[];
  states: StateRequest[];
  results: JobResultRequest[];
  uploads: JobFileUploadMeta[];
  capabilities: string[] | undefined;
  workspace: { cwd?: string };
  fetcherAuth: Array<string | undefined>;
}

/** Drive one claim through Worker dispatch with the given scripted session; resolves on the first
 *  terminal state report (observed event, not a sleep). */
async function runOnLane(script: (ctx: { options: SdkOptions; order: string[]; h: Harness }) => AsyncGenerator<unknown>, opts: { fetcher?: boolean } = {}): Promise<Harness> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-lane-job-"));
  const page = Buffer.from("<html>official datasheet: rated 5V</html>");
  const input = Buffer.from("the caller's input document");
  const h: Harness = { order: [], states: [], results: [], uploads: [], capabilities: undefined, workspace: {}, fetcherAuth: [] };
  const fetcher = await fakeFetcher((outcome, _url, auth) => {
    h.order.push(`fetch:${outcome}`);
    h.fetcherAuth.push(auth);
  }, page);
  const caFile = path.join(dir, "fetcher-ca.pem");
  fs.writeFileSync(caFile, pki.ca);

  const inputFile = { id: randomUUID(), name: `${sha(input)}.txt`, display_name: "brief.txt", size: input.length, sha256: sha(input), content_type: "text/plain" };
  const claim = {
    run_id: randomUUID(),
    kind: "job",
    issue_iid: null,
    issue_title: "",
    issue_description: "",
    repo: undefined,
    secrets: { forge_pat: "", anthropic_oauth_token: "sk-lane-fixture-model-credential" },
    last_seq: 0,
    agents: [],
    claim_generation: 4,
    budget_wall_seconds: 60,
    config: {},
    isolated_fetch: { credential: CRED, profile: "vendor-docs", hosts: ["docs.example.com"] },
    job: { type: "research", title: "Check the datasheet", prompt: "Find the rated voltage.", inputs: [], files: [inputFile] },
  } as unknown as ClaimResponse;

  const controller = new AbortController();
  let finished!: () => void;
  const terminal = new Promise<void>((resolve) => (finished = resolve));
  let gave = false;
  const client = {
    register: async (_n: string, _t: string, _m: number, _c?: string[], protocolCapabilities?: string[]) => {
      h.capabilities = protocolCapabilities;
      return {};
    },
    heartbeat: async () => {},
    claimRun: async (): Promise<ClaimResponse | null> => {
      if (gave) return null;
      gave = true;
      h.order.push("claim");
      return claim;
    },
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    reportState: async (_id: string, body: StateRequest) => {
      h.states.push(body);
      h.order.push(`state:${body.status}`);
      if (body.status === "completed" || body.status === "failed") finished();
      return { applied: true, status: body.status };
    },
    postMessages: async () => ({}),
    getInputs: async () => ({ inputs: [], receipts: false }),
    ackInputs: async () => ({ active: true, inputs: [] }),
    applyInputs: async () => ({ active: true, inputs: [] }),
    downloadJobFile: async (_id: string, fileId: string, _g: number, sink: (body: Readable) => Promise<void>) => {
      h.order.push(`download:${fileId === inputFile.id ? "input" : fileId}`);
      await sink(Readable.from([input]));
    },
    uploadJobFile: async (_id: string, meta: JobFileUploadMeta, body: Buffer | (() => Readable | Promise<Readable>)) => {
      if (!Buffer.isBuffer(body)) for await (const _ of await body()) void _;
      h.uploads.push(meta);
      h.order.push(`upload:${meta.display_name}`);
      return { status: 201, file: { id: "f", display_name: meta.display_name, storage_name: `${meta.sha256}.txt`, content_type: "text/plain", byte_size: meta.size, sha256: meta.sha256, state: "attached", expires_at: null } };
    },
    postJobResult: async (_id: string, body: JobResultRequest) => {
      h.order.push("result");
      h.results.push(body);
    },
    hasFeature: () => false,
  } as unknown as WorkerClient;

  const queryFn = (({ options }: { options: SdkOptions }) => {
    h.workspace.cwd = options.cwd;
    return script({ options, order: h.order, h });
  }) as unknown as SdkQueryFn;
  const jobRunner = new JobRunner(client, nullLogger(), {
    queryFn,
    jobsRoot: path.join(dir, "jobs"),
    batchMs: 5,
    cancelPollMs: 10,
    outputRetryDelaysMs: [1, 1],
    ...(opts.fetcher === false ? {} : { fetcherUrl: fetcher.url, fetcherCaFile: caFile }),
    splitActive: false,
  });
  const config = {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: {},
    fetcherUrl: fetcher.url,
    fetcherCaFile: caFile,
  } as unknown as Config;
  const runRunner = { resumePendingRecoveries: async () => {}, execute: async () => void h.order.push("RUN-RUNNER") } as unknown as RunRunner;
  const worker = new Worker(
    config, client, runRunner, {} as unknown as ChatRunner,
    { execute: async () => void h.order.push("JUDGE") } as unknown as JudgeRunner,
    { execute: async () => void h.order.push("REVIEW") } as unknown as ReviewRunner,
    recordingLogger().logger, () => ({ ok: true as const, missing: [] }),
    undefined, undefined, undefined, undefined, undefined, undefined, undefined, undefined,
    { execute: async () => void h.order.push("ISOLATED-RUNNER") } as never,
    jobRunner,
  );
  const done = worker.run(controller.signal);
  try {
    await terminal;
  } finally {
    controller.abort();
    await done;
    await fetcher.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
  return h;
}

const LANE_INIT = {
  type: "system",
  subtype: "init",
  tools: [...ISOLATED_TOOLS, SUBMIT],
  mcp_servers: [{ name: "uzi_fetch" }, { name: "job" }],
  plugins: [],
  agents: ["claude", "general-purpose", "statusline-setup"],
  skills: ["doctor"],
};
const RESULT_OK = { type: "result", subtype: "success", is_error: false, result: "done", usage: { input_tokens: 1, output_tokens: 1 }, modelUsage: {} };

describe("lane job end to end (PRD #1976)", { skip: process.platform !== "linux" ? "requires Linux descriptor-relative output opens" : false }, () => {
  it("claim, input download, an allowed fetch, a refused off-list fetch, output upload, result post, completed", async () => {
    let allowedPath = "";
    let sourceBytes = "";
    const h = await runOnLane(async function* ({ options }) {
      yield LANE_INIT;
      const fetchTool = toolHandler(options, "uzi_fetch", "fetch_url");
      const ok = await fetchTool({ url: "https://docs.example.com/datasheet.html" }, {});
      assert.notEqual(ok.isError, true, JSON.stringify(ok));
      const meta = JSON.parse(ok.content[0]!.text!) as { ok: boolean; path: string };
      allowedPath = meta.path;
      sourceBytes = fs.readFileSync(path.join(options.cwd!, meta.path), "utf8");
      const refused = await fetchTool({ url: "https://evil.example.net/x" }, {});
      assert.equal(refused.isError, true);
      assert.match(refused.content[0]!.text!, /off_list/);
      fs.writeFileSync(path.join(options.cwd!, "outputs", "findings.csv"), "voltage,5V\n");
      const res = await toolHandler(options, "job", "submit_job_result")(
        { status: "completed", report_md: "# Done\nRated 5V.", findings: [{ severity: "info", message_md: "5V", url: "https://docs.example.com/datasheet.html" }], output_files: ["outputs/findings.csv", meta.path] },
        {},
      );
      assert.notEqual(res.isError, true, JSON.stringify(res));
      yield RESULT_OK;
    });

    const pageSha = sha(sourceBytes);
    assert.equal(allowedPath, `sources/${pageSha}`, "the fetched page is saved under sources/<sha256>");
    assert.deepEqual(h.order, [
      "claim",
      "state:running",
      "download:input",
      "fetch:allowed",
      "fetch:refused",
      "upload:findings.csv",
      `upload:${pageSha}`,
      "result",
      "state:completed",
    ]);
    assert.ok(h.fetcherAuth.every((a) => a === `Bearer ${CRED}`), "the fetch credential rode only the fetcher requests");
    assert.equal(h.states.at(-1)!.claim_generation, 4);
    assert.equal(h.results.length, 1);
    assert.equal(h.results[0]!.claim_generation, 4);
    assert.match(h.results[0]!.report_md, /Rated 5V/);
    assert.ok(!JSON.stringify({ ...h, fetcherAuth: [] }).includes(CRED), "the credential is in no report, result or upload");
    assert.ok(h.capabilities?.includes("isolated_job_v1"), "the worker advertised isolated_job_v1 for this lane config");
    assert.ok(!h.order.includes("ISOLATED-RUNNER") && !h.order.includes("RUN-RUNNER"), "dispatched to the JobRunner only");
  });

  it("without the lane config on the worker the job is reported failed and nothing runs", async () => {
    let ran = false;
    const h = await runOnLane(
      async function* () {
        ran = true;
        yield RESULT_OK;
      },
      { fetcher: false },
    );
    assert.equal(ran, false);
    assert.deepEqual(h.order, ["claim", "state:failed"]);
    assert.match(h.states.at(-1)!.failure_reason ?? "", /UZI_FETCHER_URL/);
  });
});
