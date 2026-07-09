import http from "node:http";
import { randomUUID } from "node:crypto";
import type { AddressInfo } from "node:net";
import type {
  ClaimResponse,
  OutgoingMessage,
  StateRequest,
  UserInput,
} from "../src/protocol.js";

interface RecordedRegister {
  name: string;
  version: string;
  /** The self-reported worker template (PRD #18), or undefined when the worker
   *  sends no `template` field (older image). */
  template?: string;
  authorized: boolean;
}

/**
 * In-process HTTP server that speaks the worker protocol (PRD §Worker
 * protocol). Records every request for assertions and supports failure
 * injection for the backoff / terminal-idempotency paths. Not a fake of the
 * real server's behavior beyond the wire contract the worker depends on.
 */
export class FakeApi {
  private readonly server: http.Server;

  // --- control -------------------------------------------------------------
  private readonly claimQueue: ClaimResponse[] = [];
  private readonly inputsByRun = new Map<string, UserInput[]>();
  private stateFailRemaining = 0;
  private stateFailStatus = 503;
  private msgFailRemaining = 0;
  private msgFailStatus = 503;
  private readonly alreadyTerminal = new Set<string>();

  // --- records -------------------------------------------------------------
  readonly registers: RecordedRegister[] = [];
  heartbeats = 0;
  unauthorized = 0;
  stateAttempts = 0;
  readonly states: Array<{ runId: string; body: StateRequest }> = [];
  private readonly messagesByRun = new Map<string, OutgoingMessage[]>();
  private readonly seenSeqByRun = new Map<string, Set<number>>();

  constructor(private readonly token: string) {
    this.server = http.createServer((req, res) => {
      this.handle(req, res).catch((err) => {
        res.writeHead(500);
        res.end(String(err));
      });
    });
  }

  async listen(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    const { port } = this.server.address() as AddressInfo;
    return `http://127.0.0.1:${port}`;
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) =>
      this.server.close((err) => (err ? reject(err) : resolve())),
    );
  }

  enqueueClaim(claim: ClaimResponse): void {
    this.claimQueue.push(claim);
  }

  /** Make the next `times` /state calls fail with `status` before succeeding. */
  failStateNext(times: number, status = 503): void {
    this.stateFailRemaining = times;
    this.stateFailStatus = status;
  }

  /** Make the next `times` /messages calls fail with `status` before succeeding. */
  failMessagesNext(times: number, status = 503): void {
    this.msgFailRemaining = times;
    this.msgFailStatus = status;
  }

  /** Make /state answer 409 for this run's terminal reports (already finalized). */
  markAlreadyTerminal(runId: string): void {
    this.alreadyTerminal.add(runId);
  }

  setInputs(runId: string, inputs: UserInput[]): void {
    this.inputsByRun.set(runId, inputs);
  }

  messages(runId: string): OutgoingMessage[] {
    return this.messagesByRun.get(runId) ?? [];
  }

  // --- routing -------------------------------------------------------------
  private async handle(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    if (req.headers.authorization !== `Bearer ${this.token}`) {
      this.unauthorized++;
      return send(res, 401, { error: "unauthorized" });
    }
    const url = new URL(req.url ?? "/", "http://fake");
    const body = await readBody(req);
    const json: Record<string, unknown> = body ? JSON.parse(body) : {};
    const p = url.pathname;

    if (req.method === "POST" && p === "/api/worker/register") {
      const rec: RecordedRegister = { name: String(json.name), version: String(json.version), authorized: true };
      // Only record the key when the worker actually sent one, so an old-style
      // {name,version} register stays byte-for-byte that shape (PRD #18).
      if (json.template !== undefined) rec.template = String(json.template);
      this.registers.push(rec);
      return send(res, 200, { worker_id: randomUUID() });
    }
    if (req.method === "POST" && p === "/api/worker/heartbeat") {
      this.heartbeats++;
      return send(res, 200, {});
    }
    if (req.method === "POST" && p === "/api/worker/runs/claim") {
      const claim = this.claimQueue.shift();
      if (!claim) return sendEmpty(res, 204);
      return send(res, 200, claim);
    }

    const runMatch = /^\/api\/worker\/runs\/([^/]+)\/(messages|state|inputs)$/.exec(p);
    if (runMatch) {
      const runId = runMatch[1] as string;
      const kind = runMatch[2] as string;
      if (req.method === "POST" && kind === "messages") return this.handleMessages(res, runId, json);
      if (req.method === "POST" && kind === "state") return this.handleState(res, runId, json);
      if (req.method === "GET" && kind === "inputs") {
        // Consume-on-read, FIFO — matches M1's ConsumeInputs (each GET returns
        // then clears the pending inputs; there is no separate ack).
        const pending = this.inputsByRun.get(runId) ?? [];
        this.inputsByRun.set(runId, []);
        return send(res, 200, { inputs: pending });
      }
    }
    return send(res, 404, { error: "not found", path: p });
  }

  private handleMessages(res: http.ServerResponse, runId: string, json: Record<string, unknown>): void {
    if (this.msgFailRemaining > 0) {
      this.msgFailRemaining--;
      return send(res, this.msgFailStatus, { error: "injected failure" });
    }
    const incoming = (json.messages ?? []) as OutgoingMessage[];
    const list = this.messagesByRun.get(runId) ?? [];
    const seen = this.seenSeqByRun.get(runId) ?? new Set<number>();
    for (const m of incoming) {
      if (seen.has(m.seq)) continue; // server is idempotent on (run_id, seq)
      seen.add(m.seq);
      list.push(m);
    }
    this.messagesByRun.set(runId, list);
    this.seenSeqByRun.set(runId, seen);
    send(res, 200, { accepted: incoming.length });
  }

  private handleState(res: http.ServerResponse, runId: string, json: Record<string, unknown>): void {
    this.stateAttempts++;
    if (this.stateFailRemaining > 0) {
      this.stateFailRemaining--;
      return send(res, this.stateFailStatus, { error: "injected failure" });
    }
    const body = json as unknown as StateRequest;
    const terminal = body.status === "completed" || body.status === "failed";
    if (terminal && this.alreadyTerminal.has(runId)) {
      return send(res, 409, { error: "run already terminal" });
    }
    this.states.push({ runId, body });
    send(res, 200, {});
  }
}

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let data = "";
    req.on("data", (chunk) => (data += chunk));
    req.on("end", () => resolve(data));
    req.on("error", reject);
  });
}

function send(res: http.ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(payload);
}

function sendEmpty(res: http.ServerResponse, status: number): void {
  res.writeHead(status);
  res.end();
}
