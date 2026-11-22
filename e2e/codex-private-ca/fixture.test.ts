// Only /app/src from the baked image is used. No executor/OS dependency injection.
import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { createServer as httpsServer } from "node:https";
import { createServer as httpServer } from "node:http";

// The fixture is mounted outside /app; load baked source by absolute URL.


const source = "/run/cdr-private-ca/source.pem";

for (const trusted of [true, false]) {
  test(trusted ? "private CA completes Responses" : "no extra CA refuses private TLS", async () => {
    const { createLogger } = await import("/app/src/log.ts");
    const log = createLogger("debug");
    const { loadConfig } = await import("/app/src/config.ts");
    const { buildRunExecutor } = await import("/app/src/main.ts");
    const { WorkerClient } = await import("/app/src/client.ts");
    const { runnerCommand } = await import("/app/src/runner-uid.ts");
    console.log("runtime", process.version, process.platform, process.arch);
    assert.equal(process.getuid?.(), 10001);
    assert.equal(process.env.UZI_UID_SPLIT, "1");
    assert.equal(process.env.NODE_EXTRA_CA_CERTS, undefined);
    assert.equal(process.env.UZI_CLI_CA_FILE, undefined);
    const stat = await fs.stat(source);
    assert.deepEqual([stat.uid, stat.gid, stat.mode & 0o777], [0, 10001, 0o440]);
    assert.match(await fs.readFile(source, "utf8"), /BEGIN CERTIFICATE/);
    const read = runnerCommand("/bin/cat", [source]);
    assert.throws(() => execFileSync(read.command, read.args, { env: { PATH: "/usr/bin:/bin" }, stdio: "pipe" }),
      (error: any) => error.status !== 0 && /Permission denied/.test(error.stderr.toString()));
    const credential = ["cdr", "synthetic", "provider", "claim"].join("-");
    const receipts: unknown[] = [], unexpected: unknown[] = [], apiRequests: string[] = [];
    let tlsErrors = 0;
    const api = httpServer((req, res) => {
      apiRequests.push(req.url ?? "");
      res.writeHead(500).end("unexpected worker API request");
    });
    const server = httpsServer({
      key: await fs.readFile("/fixture/server.key"), cert: await fs.readFile("/fixture/server.pem"),
    }, (req, res) => {
      const chunks: Buffer[] = [];
      let bytes = 0;
      req.on("data", (c: Buffer) => {
        bytes += c.length;
        if (bytes > 4 * 1024 * 1024) {
          chunks.length = 0;
          res.writeHead(413).end();
          req.destroy();
          return;
        }
        chunks.push(c);
      });
      req.on("end", () => {
        if (bytes > 4 * 1024 * 1024) return;
        const receipt = { method: req.method, url: req.url, authorization: req.headers.authorization,
          body: Buffer.concat(chunks).toString() };
        console.log("cdr-private-ca-receipt", req.method, req.url?.split("?")[0]?.slice(0, 128), receipts.length + 1);
        if (req.method !== "POST" || req.url !== "/v1/responses" || req.headers.authorization !== `Bearer ${credential}`) {
          unexpected.push(receipt);
          res.writeHead(401).end();
          return;
        }
        const parsed = JSON.parse(receipt.body);
        if (parsed.model !== "corp-code") {
          unexpected.push(receipt);
          res.writeHead(400).end();
          return;
        }
        receipts.push(receipt);
        const id = `cdr-ca-${receipts.length}`;
        const done = parsed.tools?.find((tool: { type?: string; name?: string }) =>
          tool.type === "function" && tool.name === "signal_done");
        if (receipts.length === 1 && !done) {
          unexpected.push("signal_done function missing from request.tools");
          res.writeHead(400).end();
          return;
        }
        const item = receipts.length === 1
          ? { type: "function_call", call_id: "ca-done", name: done.name, arguments: "{}" }
          : { type: "message", role: "assistant", content: [{ type: "output_text", text: "private CA complete" }] };
        const frames = [
          { type: "response.created", response: { id } },
          { type: "response.output_item.done", item },
          { type: "response.completed", response: { id, usage: { input_tokens: 0, output_tokens: 0,
            total_tokens: 0, input_tokens_details: null, output_tokens_details: null } } },
        ];
        res.writeHead(200, { "content-type": "text/event-stream" });
        res.end(frames.map(f => `event: ${f.type}\ndata: ${JSON.stringify(f)}\n\n`).join(""));
      });
    });
    server.on("tlsClientError", error => { tlsErrors++; console.log("tlsClientError", error.message); });
    await new Promise<void>(resolve => api.listen(0, "127.0.0.1", resolve));
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    let executor: any;
    try {
      const cfg = loadConfig({ UZI_API_URL: `http://127.0.0.1:${(api.address() as any).port}`,
        UZI_WORKER_TOKEN: ["cdr", "synthetic", "worker"].join("-"), UZI_DATA_DIR: "/data", UZI_EXECUTOR: "sdk",
        ...(trusted ? { UZI_CODEX_CA_FILE: source } : {}) });
      const id = trusted ? "ca-trusted" : "ca-absent";
      const workspace = `/data/${id}-work`;
      await fs.mkdir(workspace, { mode: 0o2770 });
      await fs.chown(workspace, 10001, 10002);
      await fs.chmod(workspace, 0o2770);
      const built = buildRunExecutor(id, undefined, {
        log, client: new WorkerClient(cfg.apiUrl, cfg.workerToken, "cdr-private-ca", log),
        sdkHomeRoot: "/data/agent-home", executorKind: cfg.executor, stubPlanGate: cfg.stubPlanGate,
        dockerWiring: cfg.dockerWiring, codexExtraCa: cfg.codexExtraCa,
        codexCommandSandbox: cfg.codexCommandSandbox, codexSandboxDegraded: cfg.codexHarness.degraded,
      }, { provider: { provider_id: "cdr-private-ca", revision: 1, engine: "codex",
        protocol: "openai_responses", base_url: `https://127.0.0.1:${(server.address() as any).port}/v1`,
        auth_style: "bearer", credential, model_map: { default: { model: "corp-code", context_window: 32768 } } } });
      executor = built.executor;
      assert.equal(executor.constructor.name, "CodexExecutor", "actual Codex executor selected");
      console.log("factory-config", { trusted, sandbox: cfg.codexCommandSandbox });
      const emitted: unknown[] = [];
      const run = executor.run({ runId: id, kind: "task", issueIid: 42, issueTitle: "Private CA proof",
        issueDescription: "Complete approved proof", worktreePath: workspace, branch: "agent/issue-42",
        planApproved: true, approvedPlan: "Signal completion", agents: [],
        config: { run_timeout_seconds: 20, idle_timeout_seconds: 10 }, emit: (m: unknown) => emitted.push(m) });
      console.log("await-boundary", id, "executor.run");
      if (trusted) {
        await run;
        console.log("await-returned", id, "executor.run");
        assert.ok(receipts.length >= 2, "tool call then assistant completion reached our listener");
        assert.ok(JSON.stringify(emitted).includes("private CA complete"));
      } else {
        await assert.rejects(run, (error: any) =>
          /httpConnectionFailed|certificate|TLS|tls/.test(String(error)) ||
          (error?.name === "TransientRecoveryError" &&
            error.message === "silence after a provider-reported rate limit or transport retry"));
        console.log("await-returned", id, "executor.run rejection");
        assert.equal(receipts.length, 0);
        assert.ok(tlsErrors > 0, "our listener observed failed handshakes");
      }
      assert.deepEqual(unexpected, []);
      assert.deepEqual(apiRequests, [], "zero vendor/control API requests");
    } finally {
      try {
        if (executor?.safety) {
          try {
            console.log("await-boundary", trusted, "settleForCredentialFreeCapture");
            const settled = await executor.settleForCredentialFreeCapture(10000, "cancel");
            console.log("settlement", JSON.stringify(settled));
            assert.equal(settled.kind, "observed_empty");
          } finally {
            console.log("await-boundary", trusted, "safety.dispose");
            const disposed = await executor.safety.dispose({ boundary: "shutdown", deadlineMs: 10000 });
            console.log("disposal", JSON.stringify(disposed));
            assert.equal(executor.safety.acceptsTerminalDisposal(disposed), true);
          }
        }
      } finally {
        server.closeAllConnections();
        api.closeAllConnections();
        await Promise.all([new Promise<void>(r => server.close(() => r())), new Promise<void>(r => api.close(() => r()))]);
      }
    }
  });
}
