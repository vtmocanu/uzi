import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { createServer } from "node:http";
import { lstat, mkdtemp, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";

export const CODEX_VERSION = "0.153.2";
const DEADLINE_MS = 15_000;
const MAX_BYTES = 4 * 1024 * 1024;

export function deadline(promise, label, milliseconds = DEADLINE_MS) {
  let timer;
  return Promise.race([
    promise,
    new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} timed out`)), milliseconds);
    }),
  ]).finally(() => clearTimeout(timer));
}

export function shellQuote(value) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

export async function poll(predicate, label, milliseconds = DEADLINE_MS) {
  const end = Date.now() + milliseconds;
  while (Date.now() < end) {
    const value = await predicate();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`${label} timed out`);
}

export function tool(callId, name, args, namespace) {
  return {
    type: "function_call", call_id: callId, name,
    arguments: JSON.stringify(args), ...(namespace ? { namespace } : {}),
  };
}

export function patchTool(callId, filename = "patch-marker") {
  assert.match(filename, /^[a-z-]+$/);
  return { type: "custom_tool_call", call_id: callId, name: "apply_patch",
    input: `*** Begin Patch\n*** Add File: ${filename}\n+M0 marker\n*** End Patch` };
}

export function message(text = "M0 finished") {
  return { type: "message", role: "assistant", id: "m0-message",
    content: [{ type: "output_text", text }] };
}

function sse(id, items) {
  const frames = [
    { type: "response.created", response: { id } },
    ...items.map((item) => ({ type: "response.output_item.done", item })),
    { type: "response.completed", response: { id, usage: {
      input_tokens: 0, output_tokens: 0, total_tokens: 0,
      input_tokens_details: null, output_tokens_details: null,
    } } },
  ];
  return frames.map((frame) => `event: ${frame.type}\ndata: ${JSON.stringify(frame)}\n\n`).join("");
}

export function callOutput(body, id) {
  return body.input?.find((item) =>
    ["function_call_output", "custom_tool_call_output"].includes(item.type) && item.call_id === id);
}

export class Probe {
  constructor(root) {
    this.root = root;
    this.workspace = path.join(root, "workspace");
    this.home = path.join(root, "home");
    this.codexHome = path.join(root, "codex-home");
    this.messages = [];
    this.requests = [];
    this.approvals = [];
    this.dynamicCalls = [];
    this.requestStates = [];
    this.threadIds = new Set();
    this.activeTurns = new Map();
    this.errors = [];
    this.pending = new Map();
    this.events = new EventEmitter();
    this.nextId = 1;
    this.stderr = "";
  }

  static async create(t, options = {}) {
    const root = await mkdtemp(path.join(tmpdir(), "cdr-codex-m0-"));
    const probe = new Probe(root);
    t.after(async () => {
      await probe.close();
      assert.deepEqual(probe.errors, [], "harness transport/handler errors");
    });
    await probe.start(options);
    return probe;
  }

  async start({ respond = () => [message()], approval = () => ({ decision: "decline" }),
    rules, hookConfig = "", stdinApproval = false, multiAgent = false, unifiedExec = true,
    dynamicTool } = {}) {
    const bin = process.env.M0_CODEX_BIN;
    assert.ok(bin && path.isAbsolute(bin), "set M0_CODEX_BIN to an external absolute Codex binary path");
    // Fresh homes do not override these fixed system paths. Managed tests
    // independently require the exact read-only container fixture.
    for (const filename of ["config.toml", "managed_config.toml", "requirements.toml"]) {
      if (filename === "requirements.toml" && !unifiedExec && process.platform === "linux"
        && process.env.M0_MANAGED_ONESHOT === "1") continue;
      await assert.rejects(lstat(`/etc/codex/${filename}`), { code: "ENOENT" },
        `M0 refuses pre-existing system Codex ${filename}; use the isolated container`);
    }
    for (const directory of [this.workspace, this.home, this.codexHome,
      "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"].map((dir) =>
      path.isAbsolute(dir) ? dir : path.join(this.root, dir))) {
      await mkdir(directory, { mode: 0o700 });
    }
    // Deliberately replace the whole environment. No inherited login, proxy,
    // credential, NODE_OPTIONS, Git configuration, or host home reaches Codex.
    this.env = {
      HOME: this.home, CODEX_HOME: this.codexHome,
      XDG_CONFIG_HOME: path.join(this.root, "xdg-config"),
      XDG_CACHE_HOME: path.join(this.root, "xdg-cache"),
      XDG_DATA_HOME: path.join(this.root, "xdg-data"),
      XDG_STATE_HOME: path.join(this.root, "xdg-state"),
      TMPDIR: path.join(this.root, "tmp"),
      PATH: "/usr/bin:/bin:/usr/sbin:/sbin", SHELL: "/bin/sh",
      LANG: "C", TERM: "dumb", M0_DUMMY_KEY: "credential-free-local-fixture",
    };
    const version = spawn(bin, ["--version"], { cwd: this.workspace, env: this.env, stdio: ["ignore", "pipe", "pipe"] });
    this.versionChild = version;
    let versionText = "";
    version.stdout.on("data", (data) => { versionText += data; });
    this.versionExit = new Promise((resolve, reject) => {
      version.once("error", reject);
      version.once("exit", (code, signal) => resolve({ code, signal }));
    });
    assert.deepEqual(await deadline(this.versionExit, "Codex version"), { code: 0, signal: null });
    assert.equal(versionText.trim(), `codex-cli ${CODEX_VERSION}`, "M0 requires the exact characterized version");

    this.respond = respond;
    this.approval = approval;
    this.dynamicTool = dynamicTool;
    this.server = createServer((request, response) => {
      const timeout = setTimeout(() => response.destroy(new Error("M0 request deadline")), DEADLINE_MS);
      response.on("close", () => clearTimeout(timeout));
      request.on("error", (error) => { if (!this.closing) this.errors.push(error.message); });
      response.on("error", (error) => { if (!this.closing) this.errors.push(error.message); });
      if (request.method !== "POST" || request.url !== "/v1/responses") {
        request.resume();
        response.writeHead(404).end();
        return;
      }
      let raw = "";
      request.setEncoding("utf8");
      request.on("data", (chunk) => {
        raw += chunk;
        if (Buffer.byteLength(raw) > MAX_BYTES) request.destroy(new Error("M0 request too large"));
      });
      request.on("end", () => {
        void (async () => {
          try {
            assert.equal(request.headers["content-encoding"], undefined);
            assert.equal(request.headers.authorization, `Bearer ${this.env.M0_DUMMY_KEY}`);
            const body = JSON.parse(raw);
            this.requests.push(body);
            const state = { body, closed: false, sent: false };
            this.requestStates.push(state);
            response.on("close", () => { state.closed = true; this.events.emit("change"); });
            this.events.emit("change");
            const items = await deadline(this.respond(body, this), "fake Responses handler");
            if (response.destroyed) return;
            const data = sse(`m0-response-${this.requests.indexOf(body) + 1}`, items);
            response.writeHead(200, { "content-type": "text/event-stream", "content-length": Buffer.byteLength(data) });
            state.sent = true;
            response.end(data);
          } catch (error) {
            this.errors.push(error.message);
            if (!response.destroyed) response.writeHead(500).end(JSON.stringify({ error: { message: "M0 fixture error" } }));
          }
        })();
      });
    });
    this.server.requestTimeout = DEADLINE_MS;
    this.server.headersTimeout = DEADLINE_MS;
    await deadline(new Promise((resolve, reject) => {
      this.server.once("error", reject);
      this.server.listen(0, "127.0.0.1", resolve);
    }), "fake Responses bind");
    const address = this.server.address();
    assert.equal(address.address, "127.0.0.1");
    if (rules) {
      await mkdir(path.join(this.codexHome, "rules"));
      await writeFile(path.join(this.codexHome, "rules", "m0.rules"), rules);
    }
    await writeFile(path.join(this.codexHome, "config.toml"), `
model = "gpt-5.5"
model_provider = "m0"
project_doc_max_bytes = 0
check_for_update_on_startup = false
web_search = "disabled"

[analytics]
enabled = false
[feedback]
enabled = false
[features]
apps = false
plugins = false
shell_snapshot = false
shell_snapshot_v2 = false
code_mode = false
code_mode_only = false
code_mode_host = false
code_mode_prewarm = false
remote_models = false
unified_exec = ${unifiedExec}
hooks = true
multi_agent = ${multiAgent}
multi_agent_v2 = ${multiAgent}
enable_request_compression = false
write_stdin_approval = ${stdinApproval}

[model_providers.m0]
name = "M0 localhost fixture"
base_url = "http://127.0.0.1:${address.port}/v1"
env_key = "M0_DUMMY_KEY"
wire_api = "responses"
supports_websockets = false
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = ${DEADLINE_MS}
${typeof hookConfig === "function" ? hookConfig(this) : hookConfig}
`, { mode: 0o600 });
    this.child = spawn(bin, ["--dangerously-bypass-hook-trust", "app-server"], {
      cwd: this.workspace, env: this.env, detached: true, stdio: ["pipe", "pipe", "pipe"],
    });
    this.exit = new Promise((resolve, reject) => {
      this.child.once("error", (error) => { this.exited = true; reject(error); });
      this.child.once("exit", (code, signal) => {
        this.exited = true;
        for (const { reject: rejectRequest } of this.pending.values()) rejectRequest(new Error("app-server exited"));
        this.pending.clear();
        this.events.emit("change");
        resolve({ code, signal });
      });
    });
    // Register the rejection handler immediately; a later startup await still
    // receives the error, without an unhandled rejection during cleanup.
    this.exit.catch(() => {});
    this.child.stderr.on("data", (chunk) => {
      this.stderr = (this.stderr + chunk).slice(-MAX_BYTES);
    });
    this.child.stdin.on("error", (error) => { if (!this.closing) this.errors.push(error.message); });
    this.lines = createInterface({ input: this.child.stdout });
    this.lines.on("line", (line) => {
      try {
        assert.ok(Buffer.byteLength(line) <= MAX_BYTES, "M0 JSONL message too large");
        const value = JSON.parse(line);
        this.messages.push(value);
        if (value.method === "thread/started") this.threadIds.add(value.params.thread.id);
        if (value.method === "turn/started") this.activeTurns.set(value.params.threadId, value.params.turn.id);
        if (value.method === "turn/completed") this.activeTurns.delete(value.params.threadId);
        this.events.emit("change");
        if (value.method && value.id !== undefined) {
          if (value.method === "item/tool/call" && this.dynamicTool) {
            this.dynamicCalls.push(value);
            this.events.emit("change");
            void this.answerDynamic(value);
            return;
          }
          if (!value.method.endsWith("/requestApproval")) {
            this.send({ id: value.id, error: { code: -32601, message: "unsupported M0 client method" } });
            return;
          }
          this.approvals.push(value);
          this.events.emit("change");
          const result = this.approval(value, this);
          if (result !== undefined) this.send({ id: value.id, result });
        } else if (value.id !== undefined) {
          const pending = this.pending.get(value.id);
          if (!pending) return;
          this.pending.delete(value.id);
          if (value.error) pending.reject(new Error(JSON.stringify(value.error)));
          else pending.resolve(value.result);
        }
      } catch (error) {
        this.errors.push(error.message);
        this.events.emit("change");
      }
    });
    await this.rpc("initialize", { clientInfo: { name: "uzi-m0", version: "0.0.0" }, capabilities: { experimentalApi: true } });
    this.send({ method: "initialized" });
  }

  send(value) {
    assert.ok(!this.child.stdin.destroyed, "app-server stdin is closed");
    this.child.stdin.write(`${JSON.stringify(value)}\n`);
  }

  async answerDynamic(request) {
    try {
      const reply = await deadline(this.dynamicTool(request, this), "dynamic fixture callback");
      this.send({ id: request.id, ...reply });
    } catch (error) {
      this.errors.push(error.message);
      if (!this.child.stdin.destroyed && !this.child.stdin.writableEnded) {
        this.send({ id: request.id, error: { code: -32000, message: "M0 callback failed" } });
      }
    }
  }

  async rpc(method, params) {
    const id = this.nextId++;
    const response = new Promise((resolve, reject) => this.pending.set(id, { resolve, reject }));
    this.send({ id, method, params });
    try { return await deadline(response, method); }
    finally { this.pending.delete(id); }
  }

  async until(predicate, label, milliseconds = DEADLINE_MS) {
    let listener;
    try {
      return await deadline(new Promise((resolve, reject) => {
        listener = () => {
          try {
            const result = predicate();
            if (result) resolve(result);
            else if (this.exited) reject(new Error(`${label}: app-server exited; ${this.stderr}`));
          } catch (error) { reject(error); }
        };
        this.events.on("change", listener);
        listener();
      }), label, milliseconds);
    } finally { this.events.off("change", listener); }
  }

  async thread(approvalPolicy = "on-request", dynamicTools) {
    const result = await this.rpc("thread/start", {
      model: "gpt-5.5", modelProvider: "m0", cwd: this.workspace,
      approvalPolicy, approvalsReviewer: "user", sandbox: "danger-full-access", ephemeral: true,
      // app-server resolves each thread's config independently; the CLI's
      // global hook-trust flag alone does not arm these fixture hooks.
      config: { bypass_hook_trust: true },
      ...(dynamicTools ? { dynamicTools } : {}),
    });
    return result.thread.id;
  }

  async turn(threadId, text = "M0 deterministic fixture") {
    const result = await this.rpc("turn/start", { threadId, input: [{ type: "text", text }] });
    return result.turn.id;
  }

  completed(threadId, turnId) {
    return this.until(() => this.messages.find((value) => value.method === "turn/completed"
      && value.params.threadId === threadId && value.params.turn.id === turnId), "turn completion");
  }

  async marker(filename = "shell-marker") {
    try { return await readFile(path.join(this.workspace, filename), "utf8"); }
    catch (error) { if (error.code === "ENOENT") return null; throw error; }
  }

  async terminateTerminals(threadId) {
    const terminals = await this.rpc("thread/backgroundTerminals/list", { threadId });
    assert.equal(terminals.nextCursor, null, "M0 fixture terminal list must fit one page");
    for (const terminal of terminals.data) {
      const result = await this.rpc("thread/backgroundTerminals/terminate", { threadId, processId: terminal.processId });
      assert.equal(result.terminated, true, "owned fixture terminal termination acknowledged");
    }
    const remaining = await this.rpc("thread/backgroundTerminals/list", { threadId });
    assert.deepEqual(remaining.data, [], "owned fixture terminal registry is empty");
    return terminals.data.length;
  }

  async close() {
    if (this.closing) return;
    this.closing = true;
    let cleanupError;
    if (this.exited && this.threadIds.size > 0 && !this.expectedDisconnectedExit) {
      cleanupError = new Error("app-server exited before owned thread/terminal cleanup could be confirmed");
    }
    if (this.child && !this.exited && !this.child.stdin.writableEnded) {
      try {
        for (const [threadId, turnId] of this.activeTurns) {
          await this.rpc("turn/interrupt", { threadId, turnId });
          await this.completed(threadId, turnId);
        }
        for (const threadId of this.threadIds) {
          await this.terminateTerminals(threadId);
        }
      } catch (error) { cleanupError = error; }
    }
    if (this.child && !this.exited) {
      this.child.stdin.end();
      try { await deadline(this.exit, "graceful app-server exit", 1000); }
      catch {
        for (const signal of ["SIGTERM", "SIGKILL"]) {
          if (this.exited) break;
          try { process.kill(-this.child.pid, signal); }
          catch (error) { if (error.code !== "ESRCH") throw error; }
          try { await deadline(this.exit, `${signal} app-server exit`, 2000); }
          catch { if (signal === "SIGKILL") throw new Error("owned app-server did not exit"); }
        }
      }
    }
    if (this.versionChild?.exitCode === null && this.versionChild?.signalCode === null) {
      this.versionChild.kill("SIGKILL");
      await deadline(this.versionExit, "version child exit", 2000);
    }
    this.lines?.close();
    if (this.server) {
      this.server.closeAllConnections();
      await deadline(new Promise((resolve) => this.server.close(resolve)), "fake server close", 2000);
    }
    // Only the exact directory created by this Probe is removed.
    if (cleanupError) throw new Error(`M0 cleanup failed; retained ${this.root}`, { cause: cleanupError });
    await rm(this.root, { recursive: true, force: true });
  }
}
