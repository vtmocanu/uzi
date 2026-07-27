import http from "node:http";
import { randomUUID } from "node:crypto";
import type { AddressInfo } from "node:net";
import type {
  ClaimResponse,
  OutgoingMessage,
  StateRequest,
  UserInput,
  WorkerRunDetail,
  WorkerRunListItem,
  WorkerRunMessage,
} from "../src/protocol.js";

interface RecordedRegister {
  name: string;
  version: string;
  /** The self-reported worker template (PRD #18), or undefined when the worker
   *  sends no `template` field (older image). */
  template?: string;
  /** The self-reported capability set (PRD #83 Q1), or undefined when the worker
   *  sends no `capabilities` field (no daemon wired / older image). */
  capabilities?: string[];
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
  private readonly stateStatusOverride = new Map<string, string>();
  private readonly stateRawOverride = new Map<string, { status: number; body: string }>();

  // --- records -------------------------------------------------------------
  readonly registers: RecordedRegister[] = [];
  heartbeats = 0;
  unauthorized = 0;
  stateAttempts = 0;
  readonly states: Array<{ runId: string; body: StateRequest }> = [];
  private readonly messagesByRun = new Map<string, OutgoingMessage[]>();
  private readonly seenSeqByRun = new Map<string, Set<number>>();

  // --- chat read surface (PRD #39 M3) --------------------------------------
  private chatRunsList: WorkerRunListItem[] = [];
  private readonly chatRunDetails = new Map<string, WorkerRunDetail>();
  private readonly chatMessagesByRun = new Map<string, WorkerRunMessage[]>();
  readonly chatListLimits: (string | null)[] = [];
  readonly chatMessageQueries: Array<{ runId: string; after: string | null; limit: string | null }> = [];
  readonly proposalRequests: Array<{ runId: string; body: Record<string, unknown> }> = [];

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
    // unref the listening handle so it never keeps the test PROCESS alive on its own.
    // Every test drives this server through an awaited client call, so the run's own
    // pending work holds the loop open while a test is in flight; once the tests
    // finish, the server must not be what keeps the file wrapper from draining.
    // Without this, node's server handle lingered after close() on musl/alpine (CI,
    // node:22-alpine, root) and the whole runner.test.ts file timed out at the
    // per-file --test-timeout — even though every subtest passed (a leaked-handle
    // hang, not a slow test). afterEach still close()s each server; this is the
    // belt-and-braces that makes draining deterministic across platforms.
    this.server.unref();
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

  /**
   * Make /state answer 200 while reporting a DIFFERENT status than the one asked
   * for — the shape the real server produces when it declines a park on one of its
   * designed paths (retry budget exhausted, RUN_LIMIT_MAX_PARK clamp exceeded, or
   * the wait_on_limit=false coercion) and fails the run instead.
   *
   * This is the case an `applied`-keyed implementation gets wrong, because the
   * server applied a transition — just not the requested one — so `applied` is
   * true (PRD #35's park acknowledgement contract). Without this hook the fake can
   * only express "applied" and "409", and a test suite built on those two passes
   * against the broken implementation.
   */
  overrideStateStatus(runId: string, status: string): void {
    this.stateStatusOverride.set(runId, status);
  }

  /** Answer /state for this run with a verbatim body — for the malformed and
   *  older-server shapes a JSON-typed helper cannot express. */
  sendRawState(runId: string, status: number, body: string): void {
    this.stateRawOverride.set(runId, { status, body });
  }

  setInputs(runId: string, inputs: UserInput[]): void {
    this.inputsByRun.set(runId, inputs);
  }

  setChatRuns(runs: WorkerRunListItem[]): void {
    this.chatRunsList = runs;
  }
  setChatRunDetail(id: string, detail: WorkerRunDetail): void {
    this.chatRunDetails.set(id, detail);
  }
  setChatMessages(id: string, msgs: WorkerRunMessage[]): void {
    this.chatMessagesByRun.set(id, msgs);
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
      // Capabilities (PRD #83 Q1): recorded only when present, so a daemon-less worker's
      // register wire stays byte-identical. Mirrors the api's accept-and-ignore.
      if (json.capabilities !== undefined) rec.capabilities = (json.capabilities as unknown[]).map(String);
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

    // Chat read surface (PRD #39 M3), user-scoped server-side. Records query params
    // and proposal bodies for assertions.
    if (req.method === "GET" && p === "/api/worker/chat/runs") {
      this.chatListLimits.push(url.searchParams.get("limit"));
      return send(res, 200, { runs: this.chatRunsList });
    }
    const chatMsgs = /^\/api\/worker\/chat\/runs\/([^/]+)\/messages$/.exec(p);
    if (req.method === "GET" && chatMsgs) {
      const id = chatMsgs[1] as string;
      this.chatMessageQueries.push({ runId: id, after: url.searchParams.get("after"), limit: url.searchParams.get("limit") });
      return send(res, 200, { messages: this.chatMessagesByRun.get(id) ?? [] });
    }
    const chatDetail = /^\/api\/worker\/chat\/runs\/([^/]+)$/.exec(p);
    if (req.method === "GET" && chatDetail) {
      const detail = this.chatRunDetails.get(chatDetail[1] as string);
      if (!detail) return send(res, 404, { error: "run not found" });
      return send(res, 200, { run: detail });
    }
    const propMatch = /^\/api\/worker\/runs\/([^/]+)\/proposals$/.exec(p);
    if (req.method === "POST" && propMatch) {
      const runId = propMatch[1] as string;
      this.proposalRequests.push({ runId, body: json });
      const labels = Array.isArray(json.labels) ? (json.labels as string[]) : [];
      return send(res, 201, {
        proposal: {
          id: "prop-1",
          run_id: runId,
          title: String(json.title ?? ""),
          description: String(json.description ?? ""),
          labels,
          status: "pending",
          created_at: "2026-07-10T00:00:00Z",
        },
      });
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
    const raw = this.stateRawOverride.get(runId);
    if (raw) {
      res.writeHead(raw.status, { "Content-Type": "application/json" });
      res.end(raw.body);
      return;
    }
    const body = json as unknown as StateRequest;
    const terminal = body.status === "completed" || body.status === "failed";
    // Both answers carry `{"run": {...}}` because BOTH real handlers do
    // (handler/worker_protocol.go, the 409 at :483 and the 200 at :486). This fake
    // used to answer `{}` and `{"error": ...}`, which was harmless while the client
    // discarded the body and became a lie the moment PRD #35 made the run's real
    // status load-bearing — the exact "two lenient fakes" drift the claim wire
    // contract exists to prevent, one endpoint over.
    if (terminal && this.alreadyTerminal.has(runId)) {
      return send(res, 409, { error: "run already terminal", run: { id: runId, status: "cancelled" } });
    }
    this.states.push({ runId, body });
    send(res, 200, { run: { id: runId, status: this.stateStatusOverride.get(runId) ?? body.status } });
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
