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
    try { this.budget.take(0); } catch { req.destroy(); return; }
    if (["/v1/responses", "/target"].includes(req.url ?? "")) this.paths.add(req.url!);
    else if (this.errors.length === 0) this.errors.push("unexpected-path");
    for (const name of Object.keys(req.headers)) {
      if (SAFE_HEADER_NAMES.has(name)) this.headerNames.add(name);
    }
    req.on("error", () => {});
    let bytes = 0;
    req.on("data", (chunk: Buffer) => {
      try {
        this.budget.take(chunk.length);
        if (bytes + chunk.length > 1024 * 1024) throw new Error("request-limit");
        bytes += chunk.length;
      } catch { this.oversized = true; req.destroy(); }
    });
    req.on("end", () => {
      const valid = this.options.alternate === true
        ? req.headers["x-spike-key"] === this.options.credential
        : req.headers.authorization === `Bearer ${this.options.credential}`;
      this.credentialReceived ||= valid;
      this.responsePath ||= req.url === "/v1/responses";
      if (req.url === "/target") {
        this.header ||= req.headers.authorization !== undefined || req.headers["x-spike-key"] !== undefined;
        this.body ||= bytes > 0;
      }
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
      if (Buffer.byteLength(stream) > 1024 * 1024) throw new Error("script-limit");
      this.streamed = true;
      res.writeHead(200, { "content-type": "text/event-stream" }).end(stream);
    });
  }
  private closing?: Promise<void>;
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
