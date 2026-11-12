// Issue #4140: a local node:http emulation of the worker held-publication route
// (POST /api/worker/runs/:id/held-publication), pinned to the status map of the real handler
// (api/internal/handler/worker_held_publication.go writeHeldError) and to the body shapes the
// production WorkerClient.publishHeld parses. The production client runs over a real socket against
// it, so the client's own abort/timeout path is exercised.
//
// The emulator never answers unconditional success. Its per-(run, generation) invocation marker is
// process memory standing in for the committed server row; it is set only on a 200 path that was
// actually answered (or lost after being applied), so it proves the worker's protocol progression
// and nothing about DB durability.
import http from "node:http";
import type { AddressInfo } from "node:net";
import { createHash } from "node:crypto";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

export const EMULATED_PUBLICATION_ID = "00000000-0000-4000-8000-000000000009";
const WORKER_ID = "00000000-0000-4000-8000-0000000000aa";

/** How a full (non-empty) pack that finds no marker is answered. */
export type HeldCreateMode =
  /** After the preparation delay: set the marker, answer 200 state "created". */
  | { kind: "created" }
  /** After the preparation delay: set the marker, answer 200 state "refused" reason "create_refused". */
  | { kind: "refused" }
  /** After the preparation delay: set the marker (the create applied), then destroy the socket. */
  | { kind: "lost_reply" }
  /** Answered at once with a stated refusal status; no marker. 503 forge_unavailable carries Retry-After 30. */
  | { kind: "status"; status: number; reason: string };

export interface HeldRequestRecord {
  seq: number;
  runId: string;
  generation: number;
  tip: string;
  coverage: string;
  empty: boolean;
  bytes: number;
  packSha256: string;
  /** The service-side view: set once the whole request body was read. */
  bodyComplete: boolean;
  /** Set when the marker was created by this request (a create that reached the API's invocation point). */
  setMarker: boolean;
  answered?: { status: number; reason?: string; state?: string };
  /** The response or socket closed after the full body arrived, before any answer was written. */
  closedBeforeAnswer?: { elapsedMs: number };
}

export interface HeldRouteEmulator {
  url: string;
  requests: HeldRequestRecord[];
  /** How a non-empty pack without a marker is answered. Mutable between passes. */
  mode: HeldCreateMode;
  /** The controlled "API preparation" delay of the created/refused/lost_reply modes. */
  prepareDelayMs: number;
  /** The answer to an empty pack with no marker. Default: 422 pack_invalid (the real route). */
  emptyWithoutMarker: { status: number; reason: string };
  /** Runs when a request body was fully received, before the preparation delay. */
  onBodyComplete?: (record: HeldRequestRecord) => void;
  markers: Map<string, "created" | "refused">;
  seedMarker(runId: string, generation: number, state: "created" | "refused"): void;
  createsReachingMarker(): number;
  closedBeforeAnswer(): HeldRequestRecord[];
  /** Resolves once every received request has been answered or its connection has closed (bounded). */
  idle(maxMs?: number): Promise<void>;
  /** The production client, registered against this server with the V2 capability and feature. */
  client: WorkerClient;
  close(): Promise<void>;
}

const markerKey = (runId: string, generation: number): string => `${runId}:${generation}`;

export async function startHeldRouteEmulator(): Promise<HeldRouteEmulator> {
  const requests: HeldRequestRecord[] = [];
  const markers = new Map<string, "created" | "refused">();
  let creates = 0;
  const sockets = new Set<import("node:net").Socket>();
  const closes: Promise<void>[] = [];

  const writeJson = (res: http.ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void => {
    res.writeHead(status, { "Content-Type": "application/json", ...headers });
    res.end(JSON.stringify(body));
  };
  const refusal = (reason: string) => ({ error: "held publication refused", reason });
  const readback = (runId: string, generation: number, tip: string, state: "created" | "refused") => ({
    publication_id: EMULATED_PUBLICATION_ID, ref: `refs/uzi-held/${runId}/${generation}`, tip, state,
    ...(state === "refused" ? { reason: "create_refused" } : {}),
  });

  const emulator: HeldRouteEmulator = {
    url: "", requests, markers,
    mode: { kind: "created" }, prepareDelayMs: 0,
    emptyWithoutMarker: { status: 422, reason: "pack_invalid" },
    seedMarker: (runId, generation, state) => { markers.set(markerKey(runId, generation), state); },
    createsReachingMarker: () => creates,
    closedBeforeAnswer: () => requests.filter(r => r.closedBeforeAnswer !== undefined),
    idle: async (maxMs = 10_000) => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      const limit = new Promise<void>(resolve => { timer = setTimeout(resolve, maxMs); });
      try { await Promise.race([Promise.all(closes).then(() => undefined), limit]); }
      finally { clearTimeout(timer); }
    },
    client: undefined as never, close: async () => undefined,
  };

  const server = http.createServer((req, res) => {
    const path = new URL(req.url ?? "/", "http://emulator.test").pathname;
    if (req.method === "POST" && path.endsWith("/register")) {
      req.resume();
      req.on("end", () => writeJson(res, 200, {
        worker_id: WORKER_ID, register_nonce: "emulator-nonce", protocol_features: ["recovery_held_publication_v2"],
      }));
      return;
    }
    const match = /\/runs\/([^/]+)\/held-publication$/.exec(path);
    if (req.method !== "POST" || !match) { req.resume(); writeJson(res, 404, { error: "not found" }); return; }
    const runId = decodeURIComponent(match[1]!);
    const chunks: Buffer[] = [];
    req.on("data", chunk => chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)));
    req.on("error", () => undefined);
    req.on("end", () => {
      const pack = Buffer.concat(chunks);
      const generation = Number(req.headers["x-uzi-held-generation"]);
      const tip = String(req.headers["x-uzi-held-tip"] ?? "");
      const record: HeldRequestRecord = {
        seq: requests.length + 1, runId, generation, tip, coverage: String(req.headers["x-uzi-held-coverage"] ?? ""),
        empty: pack.length === 0, bytes: pack.length, packSha256: createHash("sha256").update(pack).digest("hex"),
        bodyComplete: true, setMarker: false,
      };
      requests.push(record);
      const bodyCompleteAt = process.hrtime.bigint();
      let answeredOrDestroyed = false, timer: ReturnType<typeof setTimeout> | undefined;
      closes.push(new Promise<void>(resolve => res.once("close", () => resolve())));
      res.once("close", () => {
        if (timer) clearTimeout(timer);
        timer = undefined;
        if (!res.writableFinished && !answeredOrDestroyed) {
          record.closedBeforeAnswer = { elapsedMs: Number((process.hrtime.bigint() - bodyCompleteAt) / 1_000_000n) };
        }
      });
      const answer = (status: number, body: unknown, headers?: Record<string, string>) => {
        answeredOrDestroyed = true;
        record.answered = { status, reason: (body as { reason?: string }).reason, state: (body as { state?: string }).state };
        writeJson(res, status, body, headers);
      };
      const key = markerKey(runId, generation);
      const existing = markers.get(key);
      // The service gate reconciles an existing invocation before reading the body: an empty or
      // full pack both get the marker's read-back.
      if (existing) { answer(200, readback(runId, generation, tip, existing)); return; }
      if (record.empty) {
        answer(emulator.emptyWithoutMarker.status, refusal(emulator.emptyWithoutMarker.reason),
          emulator.emptyWithoutMarker.status === 503 ? { "Retry-After": "30" } : undefined);
        return;
      }
      const mode = emulator.mode;
      if (mode.kind === "status") {
        answer(mode.status, refusal(mode.reason), mode.status === 503 ? { "Retry-After": "30" } : undefined);
        return;
      }
      emulator.onBodyComplete?.(record);
      timer = setTimeout(() => {
        timer = undefined;
        if (res.destroyed || res.writableEnded) return;
        const state = mode.kind === "refused" ? "refused" : "created";
        markers.set(key, state);
        creates++;
        record.setMarker = true;
        if (mode.kind === "lost_reply") {
          answeredOrDestroyed = true;
          res.socket?.destroy();
          return;
        }
        answer(200, readback(runId, generation, tip, state));
      }, emulator.prepareDelayMs);
    });
  });
  server.on("connection", socket => { sockets.add(socket); socket.once("close", () => sockets.delete(socket)); });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  emulator.url = `http://127.0.0.1:${port}`;
  const client = new WorkerClient(emulator.url, "test-token", "test", nullLogger());
  await client.register("emulator-worker", undefined, 1, undefined, ["recovery_held_publication_v2"], undefined, 0);
  emulator.client = client;
  emulator.close = async () => {
    for (const socket of sockets) socket.destroy();
    await new Promise<void>(resolve => server.close(() => resolve()));
  };
  return emulator;
}
