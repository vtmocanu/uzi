// issue #1786 M0b — the operator retained-source channel: a worker-local Unix socket (never TCP)
// that lets an operator `exec` into the worker container to inspect and, explicitly and by digest,
// discard a stopped generation's retained source without stopping the worker.
//
// The wire is NDJSON, one request per connection, bounded both ways. The destructive sequence is
// the whole point of this file, and its order is load-bearing:
//
//   1. locate the source (a read-only inspect: the bare, the kind and the clone paths);
//   2. close the shared claim gate under the `retained_ops` owner;
//   3. wait (bounded) for in-flight claims to drain;
//   4. require Worker.isIdle();
//   5. enter RunRunner.withRetainedSourceBoundary, which installs the run's execution tail and
//      inventory reservation (background recovery refuses the run while it is held);
//   6. INSIDE the boundary and BEFORE GitCache takes the bare lock, read the server's holds and the
//      run's lifecycle for the run (no network call may run under the lock);
//   7. GitCache discards under ONE bare-lock section;
//   8. finally: release the boundary and reopen only the `retained_ops` gate owner.
//
// Why the pre-lock hold snapshot is current: the claim CTE is the only production path that
// inserts a custody hold on a run. With the gate closed (2), claims drained (3), the worker proven
// idle (4) and the run's reservation held until the disposal ends (5-8), nothing on this worker can
// open a new hold between the read (6) and the deletion (7).
//
// The handler never cancels a run and never stops the worker. Busy, timeout, disconnect and unknown
// each refuse or report `unknown`.
import { createServer, type Server, type Socket } from "node:net";
import fs from "node:fs";
import path from "node:path";
import { ATTEMPT_ID_RE } from "../attempt-path.js";
import type { DindPruneGate } from "../dind-prune.js";
import type {
  GitCache, RetainedDiscardResult, RetainedHoldsSnapshot, RetainedInspectResult, RetainedLifecycleSnapshot, RetainedSourceSelector,
} from "../git.js";
import type { Logger } from "../log.js";
import type { RecoveryHoldsResponse, RunOwnershipResponse } from "../protocol.js";
import { RUN_TERMINAL_STATUSES } from "../recovery-settlement.js";
import { errMessage, sleep as realSleep } from "../util.js";

/** Protocol constants. The self-contained client (retained-source.mjs) duplicates them on purpose
 *  (it imports nothing from the worker); retained-source-client-packaging.test.ts asserts they agree. */
export const RETAINED_SOURCE_REQUEST_MAX_BYTES = 8192;
export const RETAINED_SOURCE_RESPONSE_MAX_BYTES = 65536;
export const RETAINED_SOURCE_SOCKET_RELATIVE = "ops/retained-source.sock";

/** `sun_path` holds 108 bytes including the terminating NUL. */
const SOCKET_PATH_MAX_BYTES = 107;
const RUN_ID_RE = /^[A-Za-z0-9_-]{1,160}$/;
const CONFIRM_RE = /^[0-9a-f]{64}$/;
const DRAIN_BUDGET_MS = 5_000;
const DRAIN_POLL_MS = 50;
const REQUEST_READ_TIMEOUT_MS = 10_000;
/** Absolute cap from connect to the request newline; the idle timer alone is reset by every byte. */
const REQUEST_DEADLINE_MS = 15_000;
const HOLDS_READ_TIMEOUT_MS = 10_000;
const SHUTDOWN_SETTLE_MS = 5_000;
const MAX_CONNECTIONS = 8;
const LAST_DISPOSITIONS_MAX = 16;

/** The part of RunRunner the channel uses. */
export interface RetainedSourceRunnerPort {
  isLiveAttemptPath(clonePath: string): boolean;
  withRetainedSourceBoundary<T>(
    runId: string,
    clonePaths: readonly string[],
    action: (ctx: {
      open: () => boolean;
      quiesce: (paths: string[]) => Promise<boolean>;
      isLivePath: (clonePath: string) => boolean;
    }) => Promise<T>,
  ): Promise<{ entered: false } | { entered: true; value: T }>;
}

export interface RetainedSourceServerDeps {
  dataDir: string;
  git: Pick<GitCache, "inspectRetainedSources" | "discardStoppedSource" | "discardProtectedSource">;
  client: {
    listRecoveryHolds(runId: string): Promise<RecoveryHoldsResponse>;
    getRunOwnership(runId: string): Promise<RunOwnershipResponse>;
  };
  /** Undefined ⇒ every destructive request refuses. */
  gate: DindPruneGate | undefined;
  isIdle: () => boolean;
  runner: RetainedSourceRunnerPort;
  log: Logger;
  /** Test seams: the drain clock/poll and the timeouts. */
  now?: () => number;
  sleep?: (ms: number) => Promise<void>;
  drainBudgetMs?: number;
  drainPollMs?: number;
  holdsTimeoutMs?: number;
  requestReadTimeoutMs?: number;
  /** Absolute connect-to-newline deadline (a slow-trickle client is cut off even while bytes arrive). */
  requestDeadlineMs?: number;
  /** GitCache's destructive deadline override (default: GitCache's 30 s). */
  deadlineMs?: number;
}

type Request =
  | { op: "inspect"; selector: RetainedSourceSelector | undefined }
  | { op: "discard"; selector: RetainedSourceSelector; confirm: string; protectedSource: boolean };

interface LastDisposition {
  runId: string;
  generation: number | "none";
  attempt: string;
  at: string;
  outcome: string;
  reason?: string;
}

/** The operator-facing refusal reasons the channel itself adds to GitCache's. */
type ChannelRefusal =
  | "busy" | "shutting_down" | "no_claim_gate" | "claims_not_drained" | "worker_not_idle" | "run_reserved"
  | "holds_unknown" | "client_disconnected" | "not_found" | "inspect_partial" | "ambiguous" | "wrong_discard_kind" | "inspect_failed";

function parseRequest(line: string): Request | string {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch {
    return "invalid_json";
  }
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return "invalid_request";
  const o = raw as Record<string, unknown>;
  const allowed = ["op", "run", "generation", "attempt", "confirm", "protected"];
  if (Object.keys(o).some((k) => !allowed.includes(k))) return "invalid_request";
  const any = o.run !== undefined || o.generation !== undefined || o.attempt !== undefined;
  let selector: RetainedSourceSelector | undefined;
  if (any) {
    if (typeof o.run !== "string" || !RUN_ID_RE.test(o.run)) return "invalid_request";
    const g = o.generation;
    if (g !== "none" && !(typeof g === "number" && Number.isSafeInteger(g) && g >= 0)) return "invalid_request";
    if (typeof o.attempt !== "string" || (o.attempt !== "canonical" && !ATTEMPT_ID_RE.test(o.attempt))) return "invalid_request";
    selector = { runId: o.run, generation: g, attempt: o.attempt };
  }
  if (o.op === "inspect") {
    if (o.confirm !== undefined || o.protected !== undefined) return "invalid_request";
    return { op: "inspect", selector };
  }
  if (o.op === "discard") {
    if (!selector || typeof o.confirm !== "string" || !CONFIRM_RE.test(o.confirm)) return "invalid_request";
    if (o.protected !== undefined && typeof o.protected !== "boolean") return "invalid_request";
    return { op: "discard", selector, confirm: o.confirm, protectedSource: o.protected === true };
  }
  return "invalid_request";
}

/** Serialize a response within the byte cap; a source list that does not fit is truncated with a
 *  flag, never sent over the cap. */
function encodeResponse(body: Record<string, unknown> & { sources?: unknown[] }): string {
  let line = `${JSON.stringify(body)}\n`;
  if (Buffer.byteLength(line) <= RETAINED_SOURCE_RESPONSE_MAX_BYTES) return line;
  const sources = [...(body.sources ?? [])];
  const total = sources.length;
  while (sources.length > 0) {
    sources.pop();
    line = `${JSON.stringify({ ...body, sources, truncated: true, omitted: total - sources.length })}\n`;
    if (Buffer.byteLength(line) <= RETAINED_SOURCE_RESPONSE_MAX_BYTES) return line;
  }
  return `${JSON.stringify({ ok: false, error: "response_too_large" })}\n`;
}

export class RetainedSourceServer {
  private server: Server | undefined;
  private socketPath: string | undefined;
  private readonly connections = new Set<Socket>();
  private closing = false;
  private busy = false;
  private inflight: Promise<unknown> | undefined;
  private readonly shutdown = new AbortController();
  private readonly lastDispositions: LastDisposition[] = [];
  private reporting = false;

  /** Settles once {@link serve} has finished its setup attempt, whether or not it succeeded. */
  readonly ready: Promise<void>;
  private markReady!: () => void;

  constructor(private readonly deps: RetainedSourceServerDeps) {
    this.ready = new Promise<void>((resolve) => { this.markReady = resolve; });
  }

  /** Open client connections (tests wait for a disconnect to be observed). */
  get activeConnections(): number {
    return this.connections.size;
  }

  /** The socket path this server uses (also under tests that need to connect). */
  get path(): string {
    return path.join(this.deps.dataDir, RETAINED_SOURCE_SOCKET_RELATIVE);
  }

  /** Whether the socket is listening. */
  get listening(): boolean {
    return this.server?.listening === true;
  }

  /**
   * Set up the channel and serve until `signal` aborts. NEVER throws: a setup failure is logged and
   * the worker continues with the channel unavailable. Resolves once the socket is closed.
   */
  async serve(signal: AbortSignal): Promise<void> {
    if (signal.aborted) {
      this.markReady();
      return;
    }
    try {
      await this.listen();
    } catch (err) {
      this.deps.log.warn("retained-source channel unavailable; the worker continues without it", { error: errMessage(err) });
      this.markReady();
      return;
    }
    this.markReady();
    if (!signal.aborted) await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    await this.close();
  }

  /** Stop accepting, drop connections, stop an in-flight discard at its next safe boundary and
   *  remove our socket. Bounded: a stuck operation does not hold the shutdown past a few seconds. */
  async close(): Promise<void> {
    this.closing = true;
    this.shutdown.abort();
    const server = this.server;
    this.server = undefined;
    if (server) await new Promise<void>((resolve) => { server.close(() => resolve()); for (const c of this.connections) c.destroy(); });
    const inflight = this.inflight;
    if (inflight) await Promise.race([inflight.catch(() => undefined), realSleep(SHUTDOWN_SETTLE_MS)]);
    const sock = this.socketPath;
    this.socketPath = undefined;
    if (sock) {
      try {
        const st = fs.lstatSync(sock);
        if (st.isSocket() && st.uid === process.geteuid?.()) fs.unlinkSync(sock);
      } catch {
        // already gone
      }
    }
  }

  private async listen(): Promise<void> {
    const opsDir = path.dirname(this.path);
    const euid = process.geteuid?.();
    if (euid === undefined) throw new Error("no effective uid on this platform");
    if (Buffer.byteLength(this.path) > SOCKET_PATH_MAX_BYTES) throw new Error("socket path exceeds the sun_path limit");
    // The directory: created 0700 (chmod after mkdir, because a setgid parent gives 2700), never a
    // symlink, ours, and exactly 0700 by its permission bits.
    let created = false;
    try {
      fs.mkdirSync(opsDir, { mode: 0o700 });
      created = true;
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err;
    }
    if (created) fs.chmodSync(opsDir, 0o700);
    const dst = fs.lstatSync(opsDir);
    if (dst.isSymbolicLink() || !dst.isDirectory()) throw new Error("ops dir is not a plain directory");
    if (dst.uid !== euid) throw new Error("ops dir is not owned by the worker");
    if ((dst.mode & 0o777) !== 0o700) throw new Error(`ops dir mode is ${(dst.mode & 0o777).toString(8)}, want 700`);
    const sock = this.path;
    try {
      const st = fs.lstatSync(sock);
      if (!st.isSocket() || st.uid !== euid) throw new Error("a non-socket or foreign entry occupies the socket path");
      fs.unlinkSync(sock);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
    }
    const server = createServer((conn) => this.onConnection(conn));
    server.maxConnections = MAX_CONNECTIONS;
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(sock, () => {
        server.off("error", reject);
        resolve();
      });
    });
    server.on("error", (err) => this.deps.log.warn("retained-source socket error", { error: errMessage(err) }));
    try {
      fs.chmodSync(sock, 0o600);
    } catch (err) {
      server.close();
      throw err;
    }
    this.server = server;
    this.socketPath = sock;
    this.deps.log.info("retained-source channel listening", { socket: sock });
  }

  private onConnection(conn: Socket): void {
    this.connections.add(conn);
    let gone = false;
    conn.on("close", () => { gone = true; this.connections.delete(conn); });
    conn.on("error", () => undefined);
    conn.setTimeout(this.deps.requestReadTimeoutMs ?? REQUEST_READ_TIMEOUT_MS, () => conn.destroy());
    const deadline = setTimeout(() => conn.destroy(), this.deps.requestDeadlineMs ?? REQUEST_DEADLINE_MS);
    deadline.unref();
    conn.on("close", () => clearTimeout(deadline));
    let buf = Buffer.alloc(0);
    let handled = false;
    const reply = (body: Record<string, unknown> & { sources?: unknown[] }): void => {
      if (gone || conn.destroyed) return;
      conn.end(encodeResponse(body));
    };
    conn.on("data", (chunk: Buffer) => {
      if (handled) return;
      buf = Buffer.concat([buf, chunk]);
      const nl = buf.indexOf(0x0a);
      // Bound BEFORE parse: the cap applies to the line, not to what follows it.
      if ((nl === -1 ? buf.length : nl) > RETAINED_SOURCE_REQUEST_MAX_BYTES) {
        handled = true;
        reply({ ok: false, error: "request_too_large" });
        return;
      }
      if (nl === -1) return;
      handled = true;
      conn.setTimeout(0);
      clearTimeout(deadline);
      const parsed = parseRequest(buf.subarray(0, nl).toString("utf8"));
      if (typeof parsed === "string") {
        reply({ ok: false, error: parsed });
        return;
      }
      void this.handle(parsed, () => gone).then(reply, (err) => {
        this.deps.log.warn("retained-source request failed", { error: errMessage(err) });
        reply({ ok: false, error: "internal_error" });
      });
    });
  }

  private async handle(req: Request, clientGone: () => boolean): Promise<Record<string, unknown> & { sources?: unknown[] }> {
    if (req.op === "inspect") return this.inspect(req.selector);
    return this.discard(req, clientGone);
  }

  // ─── inspect ────────────────────────────────────────────────────────────────────────────

  private async inspect(selector: RetainedSourceSelector | undefined): Promise<Record<string, unknown> & { sources?: unknown[] }> {
    // Read-only: the claim gate stays open. Holds are per run, so only a selected source gets them.
    const [holds, lifecycle] = selector
      ? await Promise.all([this.readHolds(selector.runId), this.readLifecycle(selector.runId)])
      : [undefined, undefined];
    const result: RetainedInspectResult = await this.deps.git.inspectRetainedSources({
      ...(selector ? { selector } : {}),
      ...(holds ? { holds } : {}),
      ...(lifecycle ? { lifecycle } : {}),
      isLivePath: (p) => this.deps.runner.isLiveAttemptPath(p),
    });
    return {
      ok: true, op: "inspect",
      sources: result.sources, partial: result.partial, unreadable: result.unreadable, sampledAt: result.sampledAt,
      ...(holds ? { holdsRead: holds.state } : {}),
      ...(lifecycle ? { lifecycleRead: lifecycle.state } : {}),
      lastDiscards: this.lastDispositions.filter((d) => !selector || d.runId === selector.runId),
    };
  }

  /** The server's open holds of this worker on the run, or `unknown` on any failure. */
  private async readHolds(runId: string): Promise<RetainedHoldsSnapshot> {
    let timer: NodeJS.Timeout | undefined;
    try {
      const response = await Promise.race([
        this.deps.client.listRecoveryHolds(runId),
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error("recovery hold read timed out")), this.deps.holdsTimeoutMs ?? HOLDS_READ_TIMEOUT_MS);
        }),
      ]);
      if (response?.run_id !== runId || !Array.isArray(response.holds)) return { state: "unknown" };
      const holds: Array<{ id: string; generation: number }> = [];
      for (const h of response.holds) {
        if (typeof h?.hold_id !== "string" || h.hold_id === "" || !Number.isSafeInteger(h.generation)) return { state: "unknown" };
        holds.push({ id: h.hold_id, generation: h.generation });
      }
      return { state: "read", holds };
    } catch (err) {
      this.deps.log.warn("retained-source: recovery hold read failed", { run_id: runId, error: errMessage(err) });
      return { state: "unknown" };
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  /**
   * The server's lifecycle verdict for the run via the worker-scoped ownership probe. Only a
   * terminal status is positive evidence; a 404 (run not held by this worker), any error, a
   * timeout or a body without a string `status` is `unknown`.
   */
  private async readLifecycle(runId: string): Promise<RetainedLifecycleSnapshot> {
    let timer: NodeJS.Timeout | undefined;
    try {
      const response = await Promise.race([
        this.deps.client.getRunOwnership(runId),
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new Error("run ownership read timed out")), this.deps.holdsTimeoutMs ?? HOLDS_READ_TIMEOUT_MS);
        }),
      ]);
      const status: unknown = response?.status;
      if (typeof status !== "string" || status === "") return { state: "unknown" };
      if (RUN_TERMINAL_STATUSES.has(status)) return { state: "terminal", status: status as "completed" | "failed" | "cancelled" };
      return { state: "not_terminal", status };
    } catch (err) {
      this.deps.log.warn("retained-source: run lifecycle read failed", { run_id: runId, error: errMessage(err) });
      return { state: "unknown" };
    } finally {
      if (timer) clearTimeout(timer);
    }
  }

  // ─── discard ────────────────────────────────────────────────────────────────────────────

  private refusal(reason: ChannelRefusal | string, consumers: string[] = []): { ok: false; op: "discard"; result: RetainedDiscardResult } {
    return { ok: false, op: "discard", result: { kind: "refused", reason, consumers } };
  }

  private remember(sel: RetainedSourceSelector, outcome: string, reason?: string): void {
    this.lastDispositions.unshift({
      runId: sel.runId, generation: sel.generation, attempt: sel.attempt, at: new Date().toISOString(), outcome,
      ...(reason ? { reason } : {}),
    });
    this.lastDispositions.length = Math.min(this.lastDispositions.length, LAST_DISPOSITIONS_MAX);
  }

  private async discard(
    req: Extract<Request, { op: "discard" }>, clientGone: () => boolean,
  ): Promise<Record<string, unknown> & { sources?: unknown[] }> {
    // One destructive request at a time; no queue. Set before any await so two requests cannot both pass.
    if (this.closing) return this.refusal("shutting_down");
    if (this.busy) return this.refusal("busy");
    const gate = this.deps.gate;
    if (!gate) return this.refusal("no_claim_gate");
    this.busy = true;
    // Only the request that took `busy` is the in-flight operation close() waits for; a refused
    // one must never replace it.
    const run = (async () => {
      try {
        const out = await this.discardSequence(req, gate, clientGone);
        const result = out.result;
        this.remember(req.selector, result.kind, result.kind === "refused" ? result.reason : undefined);
        return out;
      } finally {
        this.busy = false;
        void this.reportRetained();
      }
    })();
    this.inflight = run;
    return run;
  }

  private async discardSequence(
    req: Extract<Request, { op: "discard" }>, gate: DindPruneGate, clientGone: () => boolean,
  ): Promise<{ ok: boolean; op: "discard"; result: RetainedDiscardResult }> {
    const sel = req.selector;
    // 1. Locate the source (read-only): the bare to pass back to GitCache and the paths to guard.
    let located: RetainedInspectResult;
    try {
      located = await this.deps.git.inspectRetainedSources({ selector: sel, isLivePath: (p) => this.deps.runner.isLiveAttemptPath(p) });
    } catch (err) {
      this.deps.log.warn("retained-source: locate failed", { error: errMessage(err) });
      return this.refusal("inspect_failed");
    }
    if (located.sources.length === 0) return this.refusal(located.partial || located.unreadable > 0 ? "inspect_partial" : "not_found");
    if (located.sources.length > 1) return this.refusal("ambiguous");
    const ev = located.sources[0]!;
    if ((ev.kind === "protected") !== req.protectedSource) return this.refusal("wrong_discard_kind", [ev.kind]);
    // A wrong digest is refused before the claim gate is touched (GitCache re-checks it under its lock).
    if (req.confirm !== ev.descriptor) return this.refusal("descriptor_changed");
    const clonePaths = ev.sources.map((s) => s.clonePath);

    const now = this.deps.now ?? Date.now;
    const sleep = this.deps.sleep ?? ((ms: number) => realSleep(ms));
    // 2. Close the claim gate. From here until the finally, only `retained_ops` is ours to reopen.
    gate.close("retained_ops");
    try {
      // 3. Wait for in-flight claims to drain (a claim stays counted until its run is in the active set).
      const started = now();
      while (gate.inFlightClaims() > 0) {
        if (this.closing) return this.refusal("shutting_down");
        if (now() - started >= (this.deps.drainBudgetMs ?? DRAIN_BUDGET_MS)) return this.refusal("claims_not_drained");
        await sleep(this.deps.drainPollMs ?? DRAIN_POLL_MS);
      }
      // 4. A claim that completed has put its run in an active set: not idle, refuse.
      if (!this.deps.isIdle()) return this.refusal("worker_not_idle");
      // A client that left before anything destructive began must not get a deletion it never saw.
      if (clientGone()) return this.refusal("client_disconnected");
      // 5. Reserve the run through the disposal.
      const boundary = await this.deps.runner.withRetainedSourceBoundary(sel.runId, clonePaths, async (ctx) => {
        // 6. The holds read, inside the reservation and before GitCache takes the bare lock.
        const [holds, lifecycle] = await Promise.all([this.readHolds(sel.runId), this.readLifecycle(sel.runId)]);
        if (holds.state === "unknown") return this.refusal("holds_unknown");
        if (clientGone()) return this.refusal("client_disconnected");
        const opts = {
          selector: sel, confirm: req.confirm, holds, lifecycle, isLivePath: ctx.isLivePath, quiesce: ctx.quiesce,
          barePath: ev.barePath, signal: this.shutdown.signal,
          ...(this.deps.deadlineMs === undefined ? {} : { deadlineMs: this.deps.deadlineMs }),
        };
        // 7. One bare-lock section inside GitCache.
        let result: RetainedDiscardResult;
        try {
          result = req.protectedSource
            ? await this.deps.git.discardProtectedSource(opts)
            : await this.deps.git.discardStoppedSource(opts);
        } catch (err) {
          this.deps.log.warn("retained-source: discard ended with an error; whatever is not proven gone stays", { error: errMessage(err) });
          result = { kind: "unknown", sources: [], metadata: "kept" };
        }
        return { ok: result.kind === "done", op: "discard" as const, result };
      });
      if (!boundary.entered) return this.refusal("run_reserved");
      return boundary.value;
    } finally {
      // 8. The boundary was released by withRetainedSourceBoundary; reopen only our own gate owner.
      gate.open("retained_ops");
    }
  }

  // ─── diagnostics ────────────────────────────────────────────────────────────────────────

  /**
   * Log the bounded retained-source count and allocated bytes (structured, content-free). Called
   * after every discard operation. Public so the runner's retention exits can call it too. Single-flight,
   * never throws and never touches fleet.disk or telemetry.
   */
  async reportRetained(): Promise<void> {
    if (this.reporting || this.closing) return;
    this.reporting = true;
    try {
      const r = await this.deps.git.inspectRetainedSources({ isLivePath: (p) => this.deps.runner.isLiveAttemptPath(p) });
      let bytes = 0;
      let unmeasured = 0;
      for (const s of r.sources) {
        if (s.allocatedBytes === null) unmeasured++;
        else bytes += s.allocatedBytes;
      }
      this.deps.log.info("retained sources", {
        retained_count: r.sources.length, retained_bytes: bytes, unmeasured_sources: unmeasured,
        partial: r.partial, unreadable_bares: r.unreadable,
      });
    } catch (err) {
      this.deps.log.warn("retained sources could not be counted", { error: errMessage(err) });
    } finally {
      this.reporting = false;
    }
  }
}
