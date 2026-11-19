import { createServer, type Server, type IncomingMessage, type ServerResponse } from "node:http";
import { createServer as createTLS } from "node:https";
import { createServer as createTCP, type Socket } from "node:net";

export type ResponseItem = Record<string, unknown>;
export type ResponsesBody = Record<string, unknown>;
export class Budget {
  bytes = 0;
  items = 0;
  constructor(readonly maxBytes: number, readonly maxItems: number) {}
  take(bytes: number): void {
    if (!Number.isSafeInteger(bytes) || bytes < 0 || this.bytes + bytes > this.maxBytes || this.items + 1 > this.maxItems)
      throw new Error("budget-exhausted");
    this.bytes += bytes; this.items++;
  }
}
export const stream = [
  { type: "response.created", response: { id: "spike" } },
  { type: "response.output_item.done", item: { type: "message", role: "assistant", content: [{ type: "output_text", text: "synthetic completion" }] } },
  { type: "response.completed", response: { id: "spike", usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 } } },
].map(f => `event: ${f.type}\ndata: ${JSON.stringify(f)}\n\n`).join("");
export interface ServerOptions {
  credential: string;
  holdStream?: boolean;
  onStream?: () => void;
  respond?: (body: ResponsesBody) => ResponseItem[];
  guard?: boolean;
  host?: "127.0.0.1" | "127.0.0.2";
  redirect?: { status: 307 | 308; target: string };
  sameOrigin?: 307 | 308;
  tls?: { key: Buffer; cert: Buffer };
  stall?: boolean;
  stallConnect?: boolean;
  alternate?: boolean;
}
export const SAFE_HEADER_NAMES = new Set([
  "x-spike-key", "x-codex-beta-features", "x-codex-window-id", "x-codex-turn-metadata",
  "x-openai-internal-codex-responses-lite", "x-client-request-id", "session-id", "thread-id",
  "accept", "content-type", "authorization", "originator", "user-agent", "host",
  "content-length", "referer", "connection", "accept-encoding", "transfer-encoding",
]);
const connections = new Set<Socket>();
export class FakeProvider {
  readonly requests: ResponsesBody[] = []; // Only count placeholders; bodies never retained.
  readonly errors: string[] = [];
  readonly paths = new Set<string>();
  readonly headerNames = new Set<string>();
  header = false;
  body = false;
  accepted = false;
  credentialReceived = false;
  responsePath = false;
  streamed = false;
  oversized = false;
  callbackOutput = false;
  callbackResultMatched = false;
  nativeTools = false;
  nativeApplyPatch = false;
  unknownInventory = false;
  inventoryObserved = false;
  readonly sockets = new Set<Socket>();
  readonly budget = new Budget(4 * 1024 * 1024, 16384);
  private constructor(readonly server: Server, readonly port: number, readonly options: ServerOptions) {}
  get baseUrl(): string { return `http://127.0.0.1:${this.port}/v1`; }
  static async start(options: ServerOptions): Promise<FakeProvider> {
    const server = options.stallConnect ? createTCP() as unknown as Server : options.tls ? createTLS(options.tls) : createServer();
    const holder: { value?: FakeProvider } = {};
    server.on("connection", socket => {
      const p = holder.value;
      if (!p || connections.size >= 4) { socket.destroy(); return; }
      p.sockets.add(socket); connections.add(socket);
      socket.setTimeout(25000, () => socket.destroy());
      socket.on("error", () => {});
      socket.once("close", () => { p.sockets.delete(socket); connections.delete(socket); });
    });
    server.on("request", (req, res) => holder.value!.handle(req, res));
    server.headersTimeout = 25000;
    server.requestTimeout = 25000;
    await new Promise<void>((resolve, reject) => { server.once("error", reject); server.listen(0, options.host ?? "127.0.0.1", resolve); });
    const address = server.address();
    if (!address || typeof address === "string") throw new Error("bind-failed");
    const p = new FakeProvider(server, address.port, options); holder.value = p;
    return p;
  }
  private handle(req: IncomingMessage, res: ServerResponse): void {
    if (req.url === "/target") {
      this.header ||= req.headers.authorization !== undefined || req.headers["x-spike-key"] !== undefined;
    }
    try { this.budget.take(0); } catch { req.destroy(); return; }
    if (["/v1/responses", "/v1/models", "/target"].includes(req.url ?? "")) this.paths.add(req.url!);
    else if (this.errors.length === 0) this.errors.push("unexpected-path");
    for (const name of Object.keys(req.headers)) {
      if (SAFE_HEADER_NAMES.has(name)) this.headerNames.add(name);
    }
    req.on("error", () => {});
    let bytes = 0;
    const chunks: Buffer[] = [];
    req.on("data", (chunk: Buffer) => {
      if (req.url === "/target" && chunk.length > 0) this.body = true;
      try {
        this.budget.take(chunk.length);
        if (bytes + chunk.length > 1024 * 1024) throw new Error("request-limit");
        bytes += chunk.length;
        if (this.options.respond || req.url === "/v1/responses") chunks.push(chunk);
      } catch { this.oversized = true; req.destroy(); }
    });
    req.on("end", () => {
      const valid = this.options.alternate === true
        ? req.headers["x-spike-key"] === this.options.credential
        : req.headers.authorization === `Bearer ${this.options.credential}`;
      this.credentialReceived ||= valid;
      this.responsePath ||= req.url === "/v1/responses";
      if (this.options.stall) return;
      if (!valid && this.options.guard !== false) { res.writeHead(401).end(); return; }
      this.accepted = true;
      if (this.options.redirect || (this.options.sameOrigin && req.url !== "/target")) {
        const status = this.options.redirect?.status ?? this.options.sameOrigin!;
        const target = this.options.redirect?.target ?? `http://127.0.0.1:${this.port}/target`;
        res.writeHead(status, { location: target }).end(); return;
      }
      if (this.requests.length >= 16) { res.writeHead(429).end(); return; }
      this.requests.push({});
      let data = stream;
      if (this.options.respond || req.url === "/v1/responses") {
        try {
          const body = JSON.parse(Buffer.concat(chunks).toString("utf8")) as ResponsesBody;
          chunks.length = 0;
          const input = Array.isArray(body.input) ? body.input : [];
          this.callbackOutput ||= input.some((v: unknown) => v !== null && typeof v === "object" && ["custom_tool_call_output", "function_call_output"].includes(String((v as Record<string, unknown>).type)));
          this.callbackResultMatched ||= input.some(callbackResultMatches);
          const inventory = inventoryProjection(body);
          this.inventoryObserved = true;
          this.nativeTools ||= !inventory.nativeSuppressed;
          this.nativeApplyPatch ||= inventory.nativeApplyPatch;
          this.unknownInventory ||= inventory.unknownInventory;
          const items = this.options.respond?.(body);
          if (items) {
          const frames = [{ type: "response.created", response: { id: "spike" } },
            ...items.map(item => ({ type: "response.output_item.done", item })),
            { type: "response.completed", response: { id: "spike", usage: { input_tokens: 0, output_tokens: 0, total_tokens: 0 } } }];
          data = frames.map(f => `event: ${f.type}\ndata: ${JSON.stringify(f)}\n\n`).join("");
          }
        } catch {
          if (this.errors.length === 0) this.errors.push("script-failure");
          res.writeHead(500).end(); return;
        }
      }
      if (Buffer.byteLength(data) > 1024 * 1024) {
        if (this.errors.length === 0) this.errors.push("script-limit");
        res.writeHead(500).end(); return;
      }
      this.streamed = true;
      res.writeHead(200, { "content-type": "text/event-stream" });
      if (this.options.holdStream) { res.write('event: response.created\ndata: {"type":"response.created","response":{"id":"spike"}}\n\n'); }
      else res.end(data);
      this.options.onStream?.();
    });
  }
  private closing?: Promise<void>;
  resetObservations(): void {
    this.requests.length = 0;
    this.paths.clear(); this.headerNames.clear();
    this.header = false; this.body = false; this.accepted = false;
    this.credentialReceived = false; this.responsePath = false; this.streamed = false;
    this.callbackOutput = false; this.callbackResultMatched = false;
    this.nativeTools = false; this.nativeApplyPatch = false; this.unknownInventory = false;
    this.inventoryObserved = false;
    // Budgets, errors and oversized remain cumulative across controls and measurements.
  }
  close(): Promise<void> {
    return this.closing ??= this.closeOwned();
  }
  private async closeOwned(): Promise<void> {
    const closed = [...this.sockets].map(socket => new Promise<void>(resolve => {
      socket.once("close", () => resolve()); socket.destroy();
    }));
    this.server.closeAllConnections?.();
    await new Promise<void>(resolve => this.server.close(() => resolve()));
    await Promise.all(closed);
  }
}

// Finite names only; never persist descriptions or model inputs.
export function inventoryProjection(body: ResponsesBody) {
  const allowed = new Set(["functions", "exec", "wait", "request_user_input", "request_user_input_async", "clock", "curr_time", "clock.curr_time", "clock__curr_time", "uzi_bash", "uzi_apply_patch"]);
  let count = 0, nativeApplyPatch = false, unknownInventory = false;
  const name = (n: string) => {
    if (n === "apply_patch") nativeApplyPatch = true;
    else if (!allowed.has(n)) unknownInventory = true;
  };
  const visit = (value: unknown): void => {
    if (++count > 1024) { unknownInventory = true; return; }
    if (Array.isArray(value)) { for (const child of value) visit(child); return; }
    if (!value || typeof value !== "object") { unknownInventory = true; return; }
    const v = value as Record<string, unknown>;
    if ("name" in v) {
      if (typeof v.name === "string" && v.name.length > 0) name(v.name);
      else unknownInventory = true;
    }
    if ("type" in v && (typeof v.type !== "string" || !["function", "custom", "namespace"].includes(v.type))) unknownInventory = true;
    if (typeof v.description === "string") {
      for (const declaration of v.description.matchAll(/declare const tools:\s*\{/g)) {
        const start = declaration.index! + declaration[0].length;
        let depth = 1, end = start;
        for (; end < v.description.length && depth; end++) {
          if (v.description[end] === "{") depth++;
          if (v.description[end] === "}") depth--;
        }
        if (depth) unknownInventory = true;
        const members = v.description.slice(start, end - 1);
        let seen = false;
        for (const member of members.matchAll(/\b([A-Za-z_][A-Za-z0-9_.]*)\s*\(/g)) { seen = true; name(member[1]); }
        if (!seen) unknownInventory = true;
      }
    }
    for (const key of ["tools", "function", "namespaces", "additional_tools"]) if (key in v) visit(v[key]);
    if (!("name" in v) && !["tools", "function", "namespaces", "additional_tools"].some(k => k in v)) unknownInventory = true;
  };
  if (body.tools !== undefined) visit(body.tools);
  if (body.additional_tools !== undefined) visit(body.additional_tools);
  const input = Array.isArray(body.input) ? body.input : [];
  for (const v of input) if (v && typeof v === "object" && (v as Record<string, unknown>).type === "additional_tools") visit((v as Record<string, unknown>).tools);
  return { nativeSuppressed: !nativeApplyPatch && !unknownInventory, nativeApplyPatch, unknownInventory };
}
export function allowedInventory(body: ResponsesBody): boolean { return inventoryProjection(body).nativeSuppressed; }

export function callbackResultMatches(value: unknown): boolean {
  if (!value || typeof value !== "object") return false;
  const v = value as Record<string, unknown>;
  if (!["custom_tool_call_output", "function_call_output"].includes(String(v.type)) || v.call_id !== "spike-exec") return false;
  const texts = typeof v.output === "string" ? [v.output] : Array.isArray(v.output)
    ? v.output.flatMap(c => c && typeof c === "object" && typeof c.text === "string" ? [c.text] : []) : [];
  return texts.some(output => {
    const marker = "\nOutput:\n";
    const payload = output.startsWith("Script completed") && output.includes(marker)
      ? output.slice(output.indexOf(marker) + marker.length).trim() : output.trim();
    try {
      const result = JSON.parse(payload) as Record<string, unknown>;
      return result !== null && typeof result === "object" && !Array.isArray(result)
        && result.code === 0 && result.stdout === "synthetic" && result.stderr === "";
    } catch { return false; }
  });
}
