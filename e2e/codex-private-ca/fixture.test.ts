// Only /app/src from the baked image is used. No executor/OS dependency injection, except the
// internal provider first-response allowance (FIRST_RESPONSE_MS below), shortened so the
// typed timeout fires before the 20 s wall / 10 s idle bounds; no env or claim override exists.
import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { createServer as httpsServer } from "node:https";
import { createServer as httpServer, type ServerResponse } from "node:http";
import { createServer as netServer, type Socket } from "node:net";

/** Production is 120 s; the negative rows need the typed timeout well inside the 10 s idle bound. */
const FIRST_RESPONSE_MS = 4000;

// The fixture is mounted outside /app; load baked source by absolute URL.


const source = "/run/cdr-private-ca/source.pem";

// These startup controls call baked loadConfig without constructing an executor.
test("loadConfig accepts exactly 1 MiB valid CA and captures every byte", async () => {
  assert.equal(process.getuid?.(), 10001);
  const { loadConfig } = await import("/app/src/config.ts");
  const { extraCaPem } = await import("/app/src/codex/extra-ca.ts");
  const file = "/run/cdr-private-ca/exact-limit.pem";
  const stat = await fs.stat(file);
  assert.deepEqual([stat.uid, stat.gid, stat.mode & 0o777], [0, 10001, 0o440]);
  const bytes = await fs.readFile(file);
  const ca = await fs.readFile("/fixture/ca.pem");
  const oversized = await fs.readFile("/run/cdr-private-ca/oversized.pem");
  assert.equal(bytes.length, 1048576);
  assert.ok(bytes.subarray(0, ca.length).equals(ca), "generated valid CA prefix");
  assert.ok(bytes.subarray(ca.length).every(byte => byte === 0x20), "ASCII space padding only");
  assert.equal(oversized.length, 1048577);
  assert.ok(oversized.subarray(0, -1).equals(bytes), "inputs differ only by final space");
  assert.equal(oversized.at(-1), 0x20);
  const cfg = loadConfig({
    UZI_API_URL: "http://127.0.0.1:1",
    UZI_WORKER_TOKEN: ["cdr", "synthetic", "worker"].join("-"),
    UZI_DATA_DIR: "/data", UZI_EXECUTOR: "sdk", UZI_CODEX_CA_FILE: file,
  });
  assert.ok(cfg.codexExtraCa);
  assert.ok(Buffer.from(cfg.codexExtraCa.pem, "ascii").equals(bytes), "public bundle preserves exact bytes");
  assert.ok(Buffer.from(extraCaPem(cfg.codexExtraCa), "ascii").equals(bytes), "captured bundle provenance");
});

for (const kind of ["missing", "unreadable", "malformed", "oversized", "65certs"] as const) {
  test(`loadConfig rejects ${kind} CA with sanitized error`, async () => {
    assert.equal(process.getuid?.(), 10001);
    const { loadConfig } = await import("/app/src/config.ts");
    const file = `/run/cdr-private-ca/${kind}.pem`;
    if (kind === "missing") {
      await assert.rejects(fs.stat(file), { code: "ENOENT" });
    } else {
      const stat = await fs.stat(file);
      assert.deepEqual([stat.uid, stat.gid, stat.mode & 0o777],
        [0, 10001, kind === "unreadable" ? 0o400 : 0o440]);
      if (kind === "unreadable") {
        await assert.rejects(fs.readFile(file), { code: "EACCES" });
      } else {
        const bytes = await fs.readFile(file);
        if (kind === "oversized") assert.equal(bytes.length, 1024 * 1024 + 1);
        if (kind === "65certs") {
          assert.equal(bytes.toString().match(/-----BEGIN CERTIFICATE-----/g)?.length, 65);
          assert.ok(bytes.length < 1024 * 1024);
        }
        if (kind === "malformed") assert.equal(bytes.toString(), "-----BEGIN CERTIFICATE-----\ninvalid\n-----END CERTIFICATE-----\n");
      }
    }
    assert.throws(() => loadConfig({
      UZI_API_URL: "http://127.0.0.1:1",
      UZI_WORKER_TOKEN: ["cdr", "synthetic", "worker"].join("-"),
      UZI_DATA_DIR: "/data", UZI_EXECUTOR: "sdk", UZI_CODEX_CA_FILE: file,
    }), (error: unknown) => {
      assert.ok(error instanceof Error);
      const fixed = "Invalid UZI_CODEX_CA_FILE configuration";
      assert.equal(error.message, fixed);
      assert.equal(error.stack, fixed);
      assert.equal(error.cause, undefined);
      const exposed = [String(error), error.stack, JSON.stringify(error)].join("\n");
      assert.ok(!exposed.includes(file));
      assert.ok(!exposed.includes("-----BEGIN CERTIFICATE-----"));
      assert.ok(!exposed.includes("invalid\n"));
      return true;
    });
  });
}

for (const row of ["stalled", "trusted", "no-extra", "unrelated"] as const) {
  const trusted = row === "stalled" || row === "trusted";
  const caFile = row === "unrelated" ? "/run/cdr-private-ca/unrelated.pem" : source;
  test(row === "trusted" ? "private CA completes Responses" : row === "no-extra" ? "no extra CA refuses private TLS" : row === "unrelated" ? "unrelated CA refuses private TLS" : "stalled TLS has bounded typed timeout", async () => {
    console.log("row-start", row, "container-elapsed-seconds", Date.now() / 1000 - Number(process.env.FIXTURE_CONTAINER_STARTED_SECONDS));
    const { CodexTransportError } = await import("/app/src/codex/transport.ts");
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
    const stat = await fs.stat(caFile);
    assert.deepEqual([stat.uid, stat.gid, stat.mode & 0o777], [0, 10001, 0o440]);
    assert.match(await fs.readFile(caFile, "utf8"), /BEGIN CERTIFICATE/);
    const read = runnerCommand("/bin/cat", [caFile]);
    assert.throws(() => execFileSync(read.command, read.args, { env: { PATH: "/usr/bin:/bin" }, stdio: "pipe" }),
      (error: any) => error.status !== 0 && /Permission denied/.test(error.stderr.toString()));
    const credential = ["cdr", "synthetic", "provider", "claim"].join("-");
    const id = `ca-${row}`;
    // A literal filesystem-only program: no process, environment or cross-run inspection.
    // Input and output stay private; only fixed assertion labels reach the test log.
    const inspectOwned = `
      const fs = require("node:fs"), path = require("node:path");
      const input = JSON.parse(fs.readFileSync(0, "utf8"));
      const parent = input.root + "/codex-data";
      const epochs = fs.readdirSync(parent).filter(n => n.includes("-epoch-") &&
        fs.lstatSync(path.join(parent, n)).isDirectory());
      const results = epochs.map(n => {
        const home = path.join(parent, n, "codex");
        const config = fs.readFileSync(path.join(home, "config.toml"), "utf8");
        const ca = path.join(home, "provider-extra-ca.crt");
        const exists = fs.existsSync(ca);
        const stat = exists ? fs.lstatSync(ca) : undefined;
        const sessions = path.join(home, "sessions");
        function clean(dir) {
          if (!fs.existsSync(dir)) return true;
          return fs.readdirSync(dir).every(name => {
            const file = path.join(dir, name), s = fs.lstatSync(file);
            if (s.isSymbolicLink()) return false;
            if (s.isDirectory()) return clean(file);
            return name !== "provider-extra-ca.crt" &&
              !fs.readFileSync(file).includes(Buffer.from(input.pem));
          });
        }
        return { root: path.join(parent, n), configClean: !config.includes(input.source) && !config.includes(input.key),
          authRoot: home, exists, uid: stat?.uid, mode: stat && (stat.mode & 511),
          regular: stat?.isFile(), exact: exists && fs.readFileSync(ca).equals(Buffer.from(input.pem)),
          outsideSessions: !ca.startsWith(sessions + "/"), sessionsClean: clean(sessions) };
      });
      process.stdout.write(JSON.stringify({ uid: process.getuid(), results }));
    `;
    let ownedRoots: string[] = [];
    let resumeSessionId: string | undefined;
    let executionPhase = 1, phaseReceipts = 0;
    const firstController = new AbortController();
    let heldResponse: ServerResponse | undefined;
    let continuationReady!: () => void;
    const continuation = new Promise<void>(resolve => { continuationReady = resolve; });
    let firstRunObserved: Promise<{ status: "fulfilled" } | { status: "rejected"; error: unknown }> | undefined;
    const assertCallbackHistory = (input: unknown) => {
      assert.ok(Array.isArray(input), "actual Responses input is an item array");
      assert.ok(input.some(item => item?.type === "function_call" &&
        item.call_id === "ca-done-1" && item.name === "signal_done"), "original callback call retained");
      assert.ok(input.some(item => item?.type === "function_call_output" &&
        item.call_id === "ca-done-1" && typeof item.output === "string"),
        "matching original callback output retained");
    };
    const probeRoots = (roots?: string[]) => {
      const probe = runnerCommand(process.execPath, ["-e", `
        const fs = require("node:fs"), path = require("node:path");
        const input = JSON.parse(fs.readFileSync(0, "utf8"));
        const roots = input.roots ?? fs.readdirSync(input.parent)
          .filter(n => n.includes("-epoch-"))
          .map(n => path.join(input.parent, n));
        const results = roots.map(root => {
          try { return { root, directory: fs.lstatSync(root).isDirectory() }; }
          catch (error) { return { root, error: error.code }; }
        });
        process.stdout.write(JSON.stringify({ uid: process.getuid(), results }));
      `]);
      const result = JSON.parse(execFileSync(probe.command, probe.args, {
        env: { PATH: "/usr/bin:/bin" }, encoding: "utf8", timeout: 5000,
        input: JSON.stringify({ parent: `/data/agent-home/${id}/codex-data`, roots }),
        stdio: ["pipe", "pipe", "pipe"],
      }));
      assert.equal(result.uid, 10002, "root probe runs as runner");
      return result.results as { root: string; directory?: boolean; error?: string }[];
    };
    let ownedObserved = false;
    let ownedObservationError: unknown;
    const observeOwned = () => {
      if (ownedObserved || ownedObservationError) return;
      try {
        const probe = runnerCommand(process.execPath, ["-e", inspectOwned]);
        const result = JSON.parse(execFileSync(probe.command, probe.args, {
          env: { PATH: "/usr/bin:/bin" }, encoding: "utf8", timeout: 5000,
          input: JSON.stringify({ root: `/data/agent-home/${id}`, source: caFile,
            key: credential, pem: execFileSync("cat", [caFile], { encoding: "utf8" }) }),
          stdio: ["pipe", "pipe", "pipe"],
        }));
        assert.equal(result.uid, 10002, "filesystem probe runs as runner");
        assert.equal(result.results.length, 1, "one live owned epoch observed");
        for (const observed of result.results) {
          assert.equal(observed.configClean, true, "config excludes mount source and runtime key");
          assert.equal(observed.sessionsClean, true, "live sessions exclude configured public PEM");
          assert.equal(observed.exists, row !== "no-extra", "staging follows configured extra CA");
          if (row !== "no-extra") {
            assert.equal(observed.uid, 10002, "staged CA owner");
            assert.equal(observed.mode, 0o600, "staged CA mode");
            assert.equal(observed.regular, true, "staged CA is a regular file");
            assert.equal(observed.exact, true, "staged CA has exact configured public PEM bytes");
            assert.equal(observed.outsideSessions, true, "staged CA outside sessions");
          }
        }
        ownedRoots = result.results.map((observed: { root: string }) => observed.root);
        ownedObserved = true;
        console.log("owned-filesystem-observed", row);
      } catch (error) { ownedObservationError = error; }
    };
    const receipts: unknown[] = [], unexpected: unknown[] = [], apiRequests: string[] = [];
    let tlsErrors = 0, connections = 0, httpRequests = 0;
    const sockets = new Set<Socket>();
    const stalled = netServer();
    stalled.on("connection", socket => {
      connections++;
      sockets.add(socket);
      socket.on("close", () => sockets.delete(socket));
      socket.on("error", () => {});
      socket.resume(); // Accept TCP and consume ClientHello, never send TLS bytes.
    });
    const api = httpServer((req, res) => {
      apiRequests.push(req.url ?? "");
      res.writeHead(500).end("unexpected worker API request");
    });
    const server = httpsServer({
      key: await fs.readFile("/fixture/server.key"), cert: await fs.readFile("/fixture/server.pem"),
    }, (req, res) => {
      httpRequests++;
      assert.ok(httpRequests <= 16, "fixture request capacity exceeded");
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
        if (row === "trusted" && phaseReceipts === 0) observeOwned();
        receipts.push(receipt);
        phaseReceipts++;
        const id = `cdr-ca-${receipts.length}`;
        const done = parsed.tools?.find((tool: { type?: string; name?: string }) =>
          tool.type === "function" && tool.name === "signal_done");
        if (phaseReceipts === 1 && !done) {
          unexpected.push("signal_done function missing from request.tools");
          res.writeHead(400).end();
          return;
        }
        if (row === "trusted" && executionPhase === 2 && phaseReceipts === 1) {
          assertCallbackHistory(parsed.input);
        }
        if (row === "trusted" && executionPhase === 1 && phaseReceipts === 2) {
          assertCallbackHistory(parsed.input);
          heldResponse = res;
          continuationReady();
          return; // Keep the active continuation open; no terminal assistant response.
        }
        const item = phaseReceipts === 1
          ? { type: "function_call", call_id: `ca-done-${executionPhase}`, name: done.name, arguments: "{}" }
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
    server.on("tlsClientError", error => {
      tlsErrors++;
      if (row === "no-extra") observeOwned();
      console.log("tlsClientError", error.message);
    });
    const observe = (stage: string) => console.log("fixture-stage", row, stage,
      { api: api.listening, server: server.listening, stalled: stalled.listening });
    observe("await-api-listen");
    await new Promise<void>(resolve => api.listen(0, "127.0.0.1", resolve));
    observe("api-listened-await-server");
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    observe("server-listened-await-stalled");
    if (row === "stalled") await new Promise<void>(resolve => stalled.listen(0, "127.0.0.1", resolve));
    observe("listeners-ready");
    let executor: any;
    let disposedCurrent = false;
    const disposeOwned = async () => {
        assert.ok(executor?.safety, "native safety cleanup required");
        if (executor?.safety) {
          try {
            // Retain live-probe paths; other rows enumerate only their own epochs.
            const roots = probeRoots(ownedRoots.length ? ownedRoots : undefined);
            assert.ok(roots.length > 0, "positive owned-root existence before cleanup");
            for (const observed of roots) {
              assert.equal(observed.directory, true, "owned epoch exists before cleanup");
              assert.equal(observed.error, undefined);
            }
            ownedRoots = roots.map(observed => observed.root);
            console.log("owned-roots-present", row, ownedRoots.length);
            console.log("await-boundary", row, "withCredentialFreeCapture");
            const captured = await executor.withCredentialFreeCapture(10000, async () => undefined, "cancel");
            console.log("capture", JSON.stringify(captured));
            assert.equal(captured.kind, "observed_empty");
          } finally {
            heldResponse?.destroy();
            heldResponse = undefined;
            console.log("await-boundary", trusted, "safety.dispose");
            const disposed = await executor.safety.dispose({ boundary: "shutdown", deadlineMs: 10000 });
            console.log("disposal", JSON.stringify(disposed));
            assert.equal(executor.safety.acceptsTerminalDisposal(disposed), true);
            assert.ok(ownedRoots.length > 0, "previously observed owned roots required");
            const removed = probeRoots(ownedRoots);
            assert.equal(removed.length, ownedRoots.length);
            for (const observed of removed) {
              assert.equal(observed.error, "ENOENT", "previously observed owned epoch removed");
            }
            console.log("owned-roots-absent", row, removed.length,
              row === "no-extra" ? "no-staged-ca-control" : "staged-ca-gone-with-root");
          }
        }
      disposedCurrent = true;
    };
    try {
      const cfg = loadConfig({ UZI_API_URL: `http://127.0.0.1:${(api.address() as any).port}`,
        UZI_WORKER_TOKEN: ["cdr", "synthetic", "worker"].join("-"), UZI_DATA_DIR: "/data", UZI_EXECUTOR: "sdk",
        ...(row !== "no-extra" ? { UZI_CODEX_CA_FILE: caFile } : {}) });
      const workspace = `/data/${id}-work`;
      await fs.mkdir(workspace, { mode: 0o2770 });
      await fs.chown(workspace, 10001, 10002);
      await fs.chmod(workspace, 0o2770);
      const factoryDeps = {
        log, client: new WorkerClient(cfg.apiUrl, cfg.workerToken, "cdr-private-ca", log),
        sdkHomeRoot: "/data/agent-home", executorKind: cfg.executor, stubPlanGate: cfg.stubPlanGate,
        dockerWiring: cfg.dockerWiring, codexExtraCa: cfg.codexExtraCa,
        codexCommandSandbox: cfg.codexCommandSandbox, codexSandboxDegraded: cfg.codexHarness.degraded,
        codexProviderFirstResponseMs: FIRST_RESPONSE_MS,
      };
      const address = { provider: { provider_id: "cdr-private-ca", revision: 1, engine: "codex",
        protocol: "openai_responses", base_url: `https://127.0.0.1:${((row === "stalled" ? stalled : server).address() as any).port}/v1`,
        auth_style: "bearer", credential, model_map: { default: { model: "corp-code", context_window: 32768 } } } };
      const freshExecutor = () => buildRunExecutor(id, undefined, factoryDeps, address).executor;
      executor = freshExecutor();
      assert.equal(executor.constructor.name, "CodexExecutor", "actual Codex executor selected");
      console.log("factory-config", { trusted, sandbox: cfg.codexCommandSandbox });
      const emitted: unknown[] = [];
      const context = { runId: id, kind: "task", issueIid: 42, issueTitle: "Private CA proof",
        issueDescription: "Complete approved proof", worktreePath: workspace, branch: "agent/issue-42",
        planApproved: true, approvedPlan: "Signal completion", agents: [],
        config: { run_timeout_seconds: 20, idle_timeout_seconds: 10 }, emit: (m: any) => {
          emitted.push(m);
          // Fixed labels only: never echo payloads, credentials, bodies or env.
          const text = m?.payload?.text;
          const phase = m?.kind === "status" && typeof text === "string"
            ? text === "tools provisioned" ? "tools-provisioned"
              : text.startsWith("provisioning ") ? "provisioning"
              : "other-status" : "other-event";
          observe(`emit-${phase}`);
        } };
      const run = executor.run({ ...context, signal: firstController.signal });
      let firstRunPending = true;
      // Attach both branches immediately, including while readiness is being awaited.
      firstRunObserved = run.then(
        () => { firstRunPending = false; return { status: "fulfilled" as const }; },
        (error: unknown) => { firstRunPending = false; return { status: "rejected" as const, error }; },
      );
      observe("await-executor.run");
      console.log("await-boundary", id, "executor.run");
      const started = performance.now();
      if (row === "trusted") {
        let readinessTimer: ReturnType<typeof setTimeout> | undefined;
        try {
          await Promise.race([
            continuation,
            new Promise<never>((_, reject) => {
              readinessTimer = setTimeout(() => reject(new Error("active continuation readiness deadline exceeded")), 10000);
            }),
          ]);
          assert.equal(firstRunPending, true, "first run pending at active continuation barrier");
        } finally {
          if (readinessTimer) clearTimeout(readinessTimer);
        }
        const firstRoots = [...ownedRoots];
        firstController.abort();
        // Start public capture immediately; never pass the spent run signal to it.
        // Join both branches even if capture or the run fails.
        const [cleanup, interrupted] = await Promise.allSettled([disposeOwned(), firstRunObserved]);
        if (cleanup.status === "rejected") throw cleanup.reason;
        assert.equal(interrupted.status, "fulfilled", "run observer joined");
        if (interrupted.status !== "fulfilled") throw interrupted.reason;
        assert.equal(interrupted.value.status, "rejected", "interrupted run must reject");
        if (interrupted.value.status !== "rejected") assert.fail("interrupted run fulfilled");
        assert.ok(interrupted.value.error instanceof Error);
        assert.equal(interrupted.value.error.message, "run cancelled");
        console.log("interrupted-root-settled-observed", "trusted", "active-turn", "cancelled", "owned-root-absent");
        // Read the production generation only after cancellation and public disposal settle.
        // Never invoke persist or manufacture sessions in this observation.
        const store = "/data/agent-home/ca-trusted/codex-session-store";
        const current = (await fs.readFile(store + "/current", "utf8")).trim();
        assert.ok(/^[a-zA-Z0-9_-]+$/.test(current), "published generation is a safe path component");
        const generation = store + "/generations/" + current;
        const pem = await fs.readFile(caFile);
        let transcripts = 0, transcriptBytes = 0, runtimeMetadata = false;
        const inspectSubset = async (dir: string, relative = ""): Promise<void> => {
          for (const name of await fs.readdir(dir)) {
            const file = dir + "/" + name, rel = relative ? relative + "/" + name : name;
            const stat = await fs.lstat(file);
            assert.equal(stat.isSymbolicLink(), false, "copied subset has no symlinks");
            assert.equal(/auth|config|provider-extra-ca\.crt/i.test(name), false,
              "copied subset excludes auth/config/staged CA artifacts");
            if (stat.isDirectory()) {
              assert.ok(rel === "sessions" || rel.startsWith("sessions/"), "only sessions subtree copied");
              await inspectSubset(file, rel);
            } else {
              assert.ok(stat.isFile() && rel.startsWith("sessions/") && name.endsWith(".jsonl"),
                "only runtime session JSONL files copied");
              const bytes = await fs.readFile(file);
              assert.equal(bytes.includes(pem), false, "copied subset excludes configured public PEM");
              assert.equal(bytes.includes(Buffer.from(credential)), false, "copied subset excludes runtime key");
              assert.equal(bytes.includes(Buffer.from("-----BEGIN CERTIFICATE-----")), false,
                "copied subset excludes PEM certificate blocks");
              assert.ok(bytes.length > 0, "copied runtime transcript is nonempty");
              transcripts++;
              transcriptBytes += bytes.length;
              for (const line of bytes.toString("utf8").split("\n").filter(Boolean)) {
                let entry: any;
                try { entry = JSON.parse(line); } catch { assert.fail("copied transcript contains invalid JSONL"); }
                if (entry.type === "session_meta" && typeof entry.payload?.id === "string" &&
                    /^[0-9a-f-]{36}$/i.test(entry.payload.id)) {
                  runtimeMetadata = true;
                  if (resumeSessionId) assert.equal(entry.payload.id, resumeSessionId, "one runtime session identity");
                  resumeSessionId = entry.payload.id;
                }
              }
            }
          }
        };
        await inspectSubset(generation);
        assert.ok(transcripts > 0 && transcriptBytes > 0, "production persist captured nonempty runtime sessions");
        assert.equal(runtimeMetadata, true, "copied transcript contains real runtime session metadata");
        console.log("persisted-session-subset-observed", "trusted", "nonempty-runtime-jsonl", "exclusions-pass");
        assert.equal(receipts.length, 2, "callback continuation held without terminal completion");
        assertCallbackHistory(JSON.parse((receipts[1] as { body: string }).body).input);
        assert.ok(resumeSessionId, "real persisted runtime session ID required for resume");
        executionPhase = 2;
        phaseReceipts = 0;
        ownedRoots = [];
        ownedObserved = false;
        ownedObservationError = undefined;
        executor = freshExecutor();
        disposedCurrent = false;
        assert.equal(executor.constructor.name, "CodexExecutor");
        const firstReceiptCount = receipts.length;
        emitted.length = 0;
        await executor.run({ ...context, sessionId: resumeSessionId, signal: new AbortController().signal });
        if (ownedObservationError) throw ownedObservationError;
        assert.equal(ownedObserved, true, "second live owned root observed");
        assert.equal(ownedRoots.length, 1);
        assert.ok(ownedRoots.every(root => !firstRoots.includes(root)), "fresh owned root and codex auth home distinct");
        for (const observed of probeRoots(firstRoots)) assert.equal(observed.error, "ENOENT", "old root remains absent after resume");
        assert.ok(phaseReceipts >= 2, "second execution completes callback continuation");
        const resumedRequest = JSON.parse((receipts[firstReceiptCount] as { body: string }).body);
        assertCallbackHistory(resumedRequest.input);
        assert.ok(JSON.stringify(emitted).includes("private CA complete"), "resumed execution completes");
        const resumedGeneration = (await fs.readFile(store + "/current", "utf8")).trim();
        assert.match(resumedGeneration, /^[a-zA-Z0-9_-]+$/);
        transcripts = 0;
        transcriptBytes = 0;
        runtimeMetadata = false;
        await inspectSubset(store + "/generations/" + resumedGeneration);
        assert.ok(transcripts > 0 && transcriptBytes > 0, "resumed generation contains nonempty runtime sessions");
        assert.equal(runtimeMetadata, true, "resumed generation retains original runtime session metadata");
        console.log("fresh-executor-resume-observed", "trusted", "same-runtime-session", "prior-history", "distinct-root-auth-home", "old-root-absent");

      } else {
        await assert.rejects(run, (error: unknown) => {
          console.log("actual-rejection", error);
          assert.ok(error instanceof CodexTransportError, "baked transport class required");
          assert.deepEqual(error.failure, { category: "timeout", message: "codex provider initial response deadline exceeded" });
          return true;
        });
        const elapsed = performance.now() - started;
        console.log("typed-timeout-elapsed-ms", row, elapsed);
        assert.ok(elapsed < 20000, "typed rejection precedes production wall deadline");
        console.log("await-returned", id, "executor.run rejection");
        assert.equal(receipts.length, 0);
        assert.equal(httpRequests, 0, "zero HTTP requests, including rejected routes");
        if (row === "no-extra" || row === "unrelated") assert.ok(tlsErrors > 0, "our listener observed failed handshakes");
        else assert.ok(connections > 0, "our stalled listener accepted TCP");
      }
      if (ownedObservationError) throw ownedObservationError;
      if (row === "trusted" || row === "no-extra") {
        assert.equal(ownedObserved, true, "live filesystem observation before completion/disposal");
      }
      assert.deepEqual(unexpected, []);
      assert.deepEqual(apiRequests, [], "zero vendor/control API requests");
    } finally {
      observe("entered-finally");
      try {
        firstController.abort();
        const joined = await Promise.allSettled([
          !disposedCurrent ? disposeOwned() : Promise.resolve(),
          firstRunObserved ?? Promise.resolve(),
        ]);
        if (joined[0].status === "rejected") throw joined[0].reason;
      } finally {
        for (const socket of sockets) socket.destroy();
        observe("await-stalled-close");
        if (row === "stalled") await new Promise<void>(r => stalled.close(() => r()));
        observe("stalled-closed");
        server.closeAllConnections();
        api.closeAllConnections();
        observe("await-server-api-close");
        await Promise.all([new Promise<void>(r => server.close(() => r())), new Promise<void>(r => api.close(() => r()))]);
        observe("server-api-closed");
      }
    }
  });
}
