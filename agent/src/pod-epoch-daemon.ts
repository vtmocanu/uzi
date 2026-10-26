import http from "node:http";
import type { ClientRequest, IncomingMessage, RequestOptions } from "node:http";

export type PodEpochDaemonReason = "daemon_endpoint" | "daemon_aborted" | "daemon_deadline" |
  "daemon_timeout" | "daemon_transport" | "daemon_response" | "daemon_body_limit" |
  "daemon_inventory" | "daemon_predicate" | "daemon_event" | "daemon_event_invalid" |
  "daemon_events_closed" | "daemon_closed";
export class PodEpochDaemonError extends Error {
  constructor(readonly reason: PodEpochDaemonReason) {
    super(reason); this.name = "PodEpochDaemonError";
  }
}
export interface PodEpochDaemonGuard {
  readonly signal: AbortSignal;
  assertHealthy(): void;
  close(): Promise<void>;
}
export interface PodEpochDaemonBarrierOptions {
  dockerHost: string;
  signal: AbortSignal;
  /** Absolute Unix milliseconds, shared by the entire EX pass. */
  deadline: number;
  now?: () => number;
  /** Timer seam pairs with now; cancellation must disarm the scheduled callback. */
  schedule?: (callback: () => void, milliseconds: number) => () => void;
}
const BODY_LIMIT = 8 * 1024 * 1024;
const FRAME_LIMIT = 64 * 1024;
const REQUEST_TIMEOUT = 10_000;
function fail(reason: PodEpochDaemonReason): never { throw new PodEpochDaemonError(reason); }
function record(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
function endpoint(host: string): RequestOptions {
  if (/^unix:\/\/[^?#\s]+$/.test(host) && [...host].every(c => c.charCodeAt(0) >= 33))
    return { socketPath: host.slice(7) };
  if (!/^tcp:\/\/[^/?#@\s]+:\d+$/.test(host)) return fail("daemon_endpoint");
  try {
    const u = new URL(host);
    if (!u.hostname || !u.port || Number(u.port) < 1 || Number(u.port) > 65535)
      return fail("daemon_endpoint");
    return { hostname: u.hostname.replace(/^\[|\]$/g, ""), port: Number(u.port) };
  } catch { return fail("daemon_endpoint"); }
}
function id(v: unknown): v is string {
  return typeof v === "string" && /^[a-f0-9]{64}$/.test(v);
}
function stopped(v: unknown): boolean {
  return record(v) && v.Running === false && v.Restarting === false && v.Paused === false &&
    (v.Status === "exited" || v.Status === "created");
}
/** Counts array elements before retaining each incoming body chunk. Strings and nested
 * objects cannot disguise separators. Inventory array caps also bound nested arrays.
 */
function inventoryCounter(limit: number): (chunk: Buffer) => void {
  const stack: Array<{ array: boolean; count: number; expecting: boolean }> = [];
  let quoted = false, escaped = false;
  return chunk => {
    for (const byte of chunk) {
      if (quoted) {
        if (escaped) escaped = false;
        else if (byte === 92) escaped = true;
        else if (byte === 34) quoted = false;
        continue;
      }
      if (byte === 32 || byte === 9 || byte === 10 || byte === 13) continue;
      const parent = stack.at(-1);
      if (parent?.array && parent.expecting && byte !== 93) {
        if (++parent.count > limit) fail("daemon_inventory");
        parent.expecting = false;
      }
      if (byte === 34) quoted = true;
      else if (byte === 91 || byte === 123) {
        if (stack.length >= 64) fail("daemon_inventory");
        stack.push({ array: byte === 91, count: 0, expecting: true });
      } else if (byte === 93 || byte === 125) stack.pop();
      else if (byte === 44 && parent?.array) parent.expecting = true;
    }
  };
}
async function body(response: IncomingMessage, cap = BODY_LIMIT, arrayLimit?: number): Promise<Buffer> {
  const length = response.headers["content-length"];
  if (length !== undefined && (!/^\d+$/.test(length) || Number(length) > cap))
    return fail("daemon_body_limit");
  let size = 0;
  const count = arrayLimit === undefined ? undefined : inventoryCounter(arrayLimit);
  const chunks: Buffer[] = [];
  for await (const raw of response) {
    const chunk = Buffer.isBuffer(raw) ? raw : Buffer.from(raw as Uint8Array);
    if (chunk.length > cap - size) return fail("daemon_body_limit");
    count?.(chunk);
    size += chunk.length; chunks.push(chunk);
  }
  if (!response.complete) return fail("daemon_response");
  return Buffer.concat(chunks, size);
}
async function json(response: IncomingMessage, arrayLimit?: number): Promise<unknown> {
  const bytes = await body(response, BODY_LIMIT, arrayLimit);
  try { return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)); }
  catch { return fail("daemon_inventory"); }
}

/** Caller invokes only after acquiring tentative EX. No work starts at module import.
 * Events are a veto guard only: Docker flushes 200 before installing its subscription.
 * Neither headers nor a silent stream acknowledge subscription or prove exclusion.
 */
export async function openPodEpochDaemonBarrier(
  options: PodEpochDaemonBarrierOptions,
): Promise<PodEpochDaemonGuard> {
  const target = endpoint(options.dockerHost);
  const now = options.now ?? Date.now;
  const schedule = options.schedule ?? ((callback, ms) => {
    const timer = setTimeout(callback, ms);
    return () => clearTimeout(timer);
  });
  const controller = new AbortController();
  const requests = new Set<ClientRequest>();
  const joins = new Set<Promise<void>>();
  let reason: PodEpochDaemonError | undefined;
  let monitoring: Promise<void> | undefined;
  let closed: Promise<void> | undefined;
  const veto = (why: PodEpochDaemonReason) => {
    if (!reason) {
      reason = new PodEpochDaemonError(why);
      controller.abort(reason);
      for (const request of requests) request.destroy(reason);
    }
  };
  const assertHealthy = () => {
    if (!reason && now() >= options.deadline) veto("daemon_deadline");
    if (reason) throw reason;
  };
  const abort = () => veto("daemon_aborted");
  options.signal.addEventListener("abort", abort, { once: true });
  if (options.signal.aborted) abort();
  if (!Number.isFinite(options.deadline) || options.deadline <= now()) veto("daemon_deadline");
  const cancelDeadline = schedule(() => veto("daemon_deadline"),
    Math.max(0, Math.min(2147483647, options.deadline - now())));

  const request = (path: string, stream = false): Promise<IncomingMessage> => {
    assertHealthy();
    // The client owns this fixed GET-only allowlist; IDs never become path syntax.
    if (!/^(?:\/_ping|\/info|\/containers\/json\?all=1|\/containers\/[a-f0-9]{64}\/json|\/volumes|\/plugins|\/events\?since=\d+)$/.test(path))
      return fail("daemon_endpoint");
    return new Promise((resolve, reject) => {
      let headers = false;
      const req = http.request({ ...target, method: "GET", path, agent: false,
        maxHeaderSize: 16 * 1024 }, response => {
        headers = true;
        response.on("error", () => veto("daemon_transport"));
        response.once("end", () => cancelTimeout());
        if (response.statusCode !== 200) {
          veto("daemon_response"); reject(reason); return;
        }
        if (stream) cancelTimeout();
        resolve(response);
      });
      requests.add(req);
      let join!: Promise<void>;
      join = new Promise<void>(done => req.once("close", () => {
        cancelTimeout(); requests.delete(req); joins.delete(join); done();
        if (!headers) reject(reason ?? new PodEpochDaemonError("daemon_transport"));
      }));
      joins.add(join);
      const cancelTimeout = schedule(() => veto("daemon_timeout"),
        Math.max(0, Math.min(REQUEST_TIMEOUT, options.deadline - now())));
      req.on("error", () => {
        veto("daemon_transport"); reject(reason);
      });
      req.end();
    });
  };
  const get = async (path: string): Promise<unknown> => json(await request(path),
    path === "/plugins" ? 50 : path === "/volumes" || path === "/containers/json?all=1" ? 500 : undefined);
  const close = (): Promise<void> => {
    closed ??= (async () => {
      veto("daemon_closed");
      cancelDeadline();
      options.signal.removeEventListener("abort", abort);
      // No retries. Destroy all owned requests and join their close events and reader.
      await Promise.allSettled(joins);
      await monitoring;
    })();
    return closed;
  };
  try {
    const ping = await body(await request("/_ping"), 16);
    if (ping.toString("utf8") !== "OK") fail("daemon_response");
    const since = Math.floor(now() / 1000);
    if (!Number.isSafeInteger(since) || since < 0) fail("daemon_deadline");
    const events = await request(`/events?since=${since}`, true);
    monitoring = (async () => {
      const decoder = new TextDecoder("utf-8", { fatal: true });
      let frame = "", frameBytes = 0, streamBytes = 0;
      // Count actual stream bytes, including whitespace and delimiters, before parsing.
      try {
        for await (const raw of events) {
          const chunk = Buffer.isBuffer(raw) ? raw : Buffer.from(raw as Uint8Array);
          if (chunk.length > FRAME_LIMIT - streamBytes) fail("daemon_event_invalid");
          streamBytes += chunk.length;
          let start = 0;
          for (let i = 0; i <= chunk.length; i++) {
            if (i !== chunk.length && chunk[i] !== 10) continue;
            const segment = chunk.subarray(start, i);
            if (segment.length > FRAME_LIMIT - frameBytes) fail("daemon_event_invalid");
            frameBytes += segment.length;
            frame += decoder.decode(segment, { stream: true });
            // A complete JSON object vetoes even without a final newline.
            if (frame.trim()) {
              let parsed: unknown;
              // Partial event payloads also refuse immediately. Only whitespace can
              // remain healthy; buffering a payload supplies no useful authority.
              try { parsed = JSON.parse(frame); } catch { fail("daemon_event_invalid"); }
              if (parsed !== undefined) fail(record(parsed) ? "daemon_event" : "daemon_event_invalid");
            }
            if (i < chunk.length) {
              frame += decoder.decode();
              if (frame.trim()) fail("daemon_event_invalid");
              frame = ""; frameBytes = 0;
            }
            start = i + 1;
          }
        }
        decoder.decode();
        veto("daemon_events_closed");
      } catch (error) {
        veto(error instanceof PodEpochDaemonError ? error.reason : "daemon_event_invalid");
      }
    })();
    const info = await get("/info");
    if (!record(info) || !record(info.Swarm) || info.Swarm.LocalNodeState !== "inactive" ||
        info.LiveRestoreEnabled !== false) fail("daemon_predicate");
    const containers = await get("/containers/json?all=1");
    if (!Array.isArray(containers) || containers.length > 500) fail("daemon_inventory");
    const ids = new Set<string>();
    for (const entry of containers) {
      if (!record(entry) || !id(entry.Id) || ids.has(entry.Id)) fail("daemon_inventory");
      if (entry.State !== "exited" && entry.State !== "created") fail("daemon_predicate");
      ids.add(entry.Id);
    }
    // Four workers consume the bounded inventory lazily. A veto aborts siblings;
    // all workers settle before the error escapes (no early Promise.all rejection).
    const list = [...ids];
    let next = 0;
    const workers = Array.from({ length: Math.min(4, list.length) }, async () => {
      try {
        while (next < list.length) {
          assertHealthy();
          const containerID = list[next++]!;
          const inspected = await get(`/containers/${encodeURIComponent(containerID)}/json`);
          if (!record(inspected) || inspected.Id !== containerID) fail("daemon_inventory");
          if (!stopped(inspected.State) || !record(inspected.HostConfig) ||
              !record(inspected.HostConfig.RestartPolicy) ||
              inspected.HostConfig.RestartPolicy.Name !== "no") fail("daemon_predicate");
        }
      } catch (error) {
        veto(error instanceof PodEpochDaemonError ? error.reason : "daemon_transport");
        throw error;
      }
    });
    const results = await Promise.allSettled(workers);
    assertHealthy();
    if (results.some(result => result.status === "rejected")) fail("daemon_inventory");
    const volumes = await get("/volumes");
    if (!record(volumes) || !Array.isArray(volumes.Volumes) || volumes.Volumes.length > 500)
      fail("daemon_inventory");
    for (const volume of volumes.Volumes) {
      if (!record(volume) || volume.Driver !== "local" || !Object.hasOwn(volume, "Options") ||
          !(volume.Options === null || (record(volume.Options) && Object.keys(volume.Options).length === 0)))
        fail("daemon_predicate");
    }
    const plugins = await get("/plugins");
    if (!Array.isArray(plugins) || plugins.length > 50) fail("daemon_inventory");
    for (const plugin of plugins)
      if (!record(plugin) || plugin.Enabled !== false) fail("daemon_predicate");
    assertHealthy();
    return { signal: controller.signal, assertHealthy, close };
  } catch (error) {
    veto(error instanceof PodEpochDaemonError ? error.reason : "daemon_transport");
    await close();
    throw reason;
  }
}
