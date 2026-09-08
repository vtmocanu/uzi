// PRD #1171 (M3, milestone 2) — the production {@link FileopClient}: the NDJSON
// client that speaks to the Go `openat2` file-operation helper
// (agent/codex/supervisor/fileop) over a COMMAND-ROOT process.
//
// This is the sole production implementation of the broker's injected `fileop` seam.
// EVERY model-selected file effect the broker admits (read / write / apply_patch /
// stat / list / mkdir / rename / unlink / rmdir) is executed by the helper under
// RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS, so the KERNEL is the
// containment and there is NO check-then-use window: this client NEVER touches a
// model path with node `fs`, and it NEVER realpath-checks-then-opens-by-pathname. A
// patch (`apply`) rides the helper's fd-anchored read-modify-write on ONE held fd, so
// a symlink/rename swap racing the edit is refused (E_SYMLINK) or lands on the pinned
// inode, never followed out of the worktree.
//
// The helper is a long-lived per-run process launched as the credential-free COMMAND
// identity (uid 10003, `commandRootCommand`) with the worktree root passed ONCE on its
// trusted argv (`--root <abs>`), never model-controlled. This client owns only the
// wire: request/response id correlation, bounded newline framing, per-request
// deadlines and fail-closed transport-failure mapping. It is decoupled from the
// process (it takes injected streams) so unit tests drive it with in-memory pipes and
// no real helper binary; {@link spawnFileopHelper} is the thin production wiring.
//
// SECURITY: this module NEVER logs a raw path, file body, request or response. A
// transport failure maps to a bounded typed code (`E_IO` / `E_TIMEOUT` / `E_OVERSIZE`
// / `E_MALFORMED`) the broker renders as a neutral `fileop_denied`; the helper's own
// codes are already bounded and path/content-free.

import { spawn } from "node:child_process";
import type { Readable, Writable } from "node:stream";

import { commandRootCommand } from "../runner-uid.js";
import type { FileopClient, FileopRequest, FileopResponse } from "./broker.js";

/** Per-line ceiling on a decoded response frame. Mirrors the helper's own
 *  `maxRequestLine` (2 MiB): a read of the 1 MiB `maxRead` body rides base64 (~1.34
 *  MiB) inside one line, so 2 MiB is generous headroom while still bounding a hostile
 *  or corrupt stream to a bounded protocol failure rather than an unbounded read
 *  buffer. A line past the cap terminates the client fail-closed. */
const DEFAULT_MAX_LINE_BYTES = 2 * 1024 * 1024;

/** Per-request wall-clock deadline. The helper is single-threaded and answers each op
 *  promptly; a request that outlives this is a stuck/dead helper, so it resolves a
 *  bounded `E_TIMEOUT` denial rather than wedge the awaiting broker callback forever. */
const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

export interface FileopHelperClientOptions {
  /** The helper's stdout — one JSON response object per line is read from here. */
  readonly inbound: Readable;
  /** The helper's stdin — one JSON request object per line is written here. */
  readonly outbound: Writable;
  readonly maxLineBytes?: number;
  readonly requestTimeoutMs?: number;
}

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

/** Coerce a parsed response object into the broker's {@link FileopResponse}, reading
 *  ONLY the known, typed fields (an unexpected/extra field is dropped, never trusted).
 *  A non-object or an object missing `ok:true` is a bounded failed response. */
function coerceResponse(value: unknown): FileopResponse {
  const o = asObject(value);
  if (!o) return { ok: false, code: "E_MALFORMED" };
  const ok = o.ok === true;
  const entriesRaw = o.entries;
  let entries: FileopResponse["entries"];
  if (Array.isArray(entriesRaw)) {
    const out: { name: string; type: string }[] = [];
    for (const e of entriesRaw) {
      const eo = asObject(e);
      if (eo && typeof eo.name === "string" && typeof eo.type === "string") {
        out.push({ name: eo.name, type: eo.type });
      }
    }
    entries = out;
  }
  return {
    ok,
    code: typeof o.code === "string" ? o.code : undefined,
    size: typeof o.size === "number" ? o.size : undefined,
    data: typeof o.data === "string" ? o.data : undefined,
    exists: typeof o.exists === "boolean" ? o.exists : undefined,
    type: typeof o.type === "string" ? o.type : undefined,
    entries,
    truncated: typeof o.truncated === "boolean" ? o.truncated : undefined,
  };
}

interface PendingOp {
  readonly resolve: (r: FileopResponse) => void;
  timer: ReturnType<typeof setTimeout> | undefined;
}

/**
 * The persistent NDJSON client over the helper's stdio. One instance per per-run
 * command-root helper process; every broker file effect flows through {@link op}. All
 * failure surfaces (a closed/failed transport, an over-cap line, a per-request
 * deadline) resolve a bounded FAILED {@link FileopResponse} so the broker always maps
 * to a neutral denial and no op ever hangs or leaks a raw error.
 */
export class FileopHelperClient implements FileopClient {
  private readonly outbound: Writable;
  private readonly maxLineBytes: number;
  private readonly requestTimeoutMs: number;

  private nextId = 1;
  private readonly pending = new Map<number, PendingOp>();
  private readBuf = "";
  private closed = false;

  private readonly onData = (chunk: string): void => this.ingest(chunk);
  private readonly onEnd = (): void => this.terminate();
  private readonly onError = (): void => this.terminate();

  constructor(opts: FileopHelperClientOptions) {
    this.outbound = opts.outbound;
    this.maxLineBytes = opts.maxLineBytes ?? DEFAULT_MAX_LINE_BYTES;
    this.requestTimeoutMs = opts.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
    opts.inbound.setEncoding("utf8");
    opts.inbound.on("data", this.onData);
    opts.inbound.on("end", this.onEnd);
    opts.inbound.on("close", this.onEnd);
    opts.inbound.on("error", this.onError);
    this.outbound.on("error", this.onError);
  }

  op(request: FileopRequest): Promise<FileopResponse> {
    return new Promise<FileopResponse>((resolve) => {
      if (this.closed) {
        resolve({ ok: false, code: "E_IO" });
        return;
      }
      const id = this.nextId++;
      // Only the KNOWN wire fields are forwarded; the request shape is the broker's,
      // already screened/relativized (the helper re-validates every path itself).
      const frame: Record<string, unknown> = { id, op: request.op, path: request.path };
      if (request.newPath !== undefined) frame.newPath = request.newPath;
      if (request.data !== undefined) frame.data = request.data;
      if (request.old !== undefined) frame.old = request.old;
      let line: string;
      try {
        line = `${JSON.stringify(frame)}\n`;
      } catch {
        resolve({ ok: false, code: "E_MALFORMED" });
        return;
      }
      if (Buffer.byteLength(line, "utf8") > this.maxLineBytes) {
        resolve({ ok: false, code: "E_OVERSIZE" });
        return;
      }
      const entry: PendingOp = { resolve, timer: undefined };
      entry.timer = setTimeout(() => {
        if (this.pending.delete(id)) resolve({ ok: false, code: "E_TIMEOUT" });
      }, this.requestTimeoutMs);
      entry.timer.unref?.();
      this.pending.set(id, entry);
      try {
        this.outbound.write(line);
      } catch {
        if (this.pending.delete(id)) {
          if (entry.timer) clearTimeout(entry.timer);
          resolve({ ok: false, code: "E_IO" });
        }
      }
    });
  }

  /** Idempotent client-side closure: stop reading and fail every in-flight op closed.
   *  Does NOT end the injected streams or kill the helper process — the command-root
   *  process lifecycle is owned by the registry/safety reap lane, not this client. */
  close(): void {
    this.terminate();
  }

  private ingest(chunk: string): void {
    if (this.closed) return;
    this.readBuf += chunk;
    for (;;) {
      const nl = this.readBuf.indexOf("\n");
      if (nl < 0) {
        // No terminator yet: a partial line past the cap is a fail-closed transport fault.
        if (Buffer.byteLength(this.readBuf, "utf8") > this.maxLineBytes) this.terminate();
        return;
      }
      const line = this.readBuf.slice(0, nl);
      this.readBuf = this.readBuf.slice(nl + 1);
      if (Buffer.byteLength(line, "utf8") > this.maxLineBytes) {
        this.terminate();
        return;
      }
      const trimmed = line.trim();
      if (trimmed.length === 0) continue;
      let parsed: unknown;
      try {
        parsed = JSON.parse(trimmed);
      } catch {
        // A malformed line desyncs correlation; fail closed rather than guess an id.
        this.terminate();
        return;
      }
      const id = asObject(parsed)?.id;
      if (typeof id !== "number") continue; // an id-less frame is liveness; ignore it
      const entry = this.pending.get(id);
      if (entry === undefined) continue; // unknown/late id (already timed out): ignore
      this.pending.delete(id);
      if (entry.timer) clearTimeout(entry.timer);
      entry.resolve(coerceResponse(parsed));
    }
  }

  private terminate(): void {
    if (this.closed) return;
    this.closed = true;
    const inflight = [...this.pending.values()];
    this.pending.clear();
    for (const entry of inflight) {
      if (entry.timer) clearTimeout(entry.timer);
      entry.resolve({ ok: false, code: "E_IO" });
    }
  }
}

// --- production process wiring -------------------------------------------------

/** The trusted, launcher-fixed inputs for spawning the helper. `fileopBin` and
 *  `worktreePath` are absolute and worker-chosen, NEVER model-controlled. */
export interface FileopHelperSpec {
  readonly fileopBin: string;
  readonly worktreePath: string;
  /** The command-root env (the credential-free command identity's PATH/TMPDIR); the
   *  helper needs no credentials. Never merged with the worker's own environment. */
  readonly env?: NodeJS.ProcessEnv;
}

/** A live helper process plus its client. `dispose` best-effort ends the request
 *  stream (the helper exits on stdin EOF) and closes the client; process REAPING is
 *  the registry/safety lane's job, not this handle's. */
export interface FileopHelperHandle {
  readonly client: FileopClient;
  dispose(): Promise<void>;
}

/** The minimal spawned-process surface the wiring needs; Node's ChildProcess satisfies
 *  it structurally and a test injects a fake. */
export interface FileopProcess {
  readonly stdin: Writable | null;
  readonly stdout: Readable | null;
}
export type SpawnFileopProcess = (
  command: string,
  args: readonly string[],
  env: NodeJS.ProcessEnv | undefined,
) => FileopProcess;

const defaultSpawnFileopProcess: SpawnFileopProcess = (command, args, env) =>
  spawn(command, [...args], { stdio: ["pipe", "pipe", "pipe"], env }) as unknown as FileopProcess;

export interface SpawnFileopHelperDeps {
  readonly spawnProcess?: SpawnFileopProcess;
  readonly maxLineBytes?: number;
  readonly requestTimeoutMs?: number;
}

/**
 * Launch the fileop helper as the credential-free COMMAND identity and return a wired
 * {@link FileopClient}. The helper argv is trusted and launcher-fixed
 * (`<fileopBin> --root <abs-worktree>`); the worktree root is the anchor the kernel
 * containment resolves beneath, never a model path. Composed via `commandRootCommand`
 * so the helper runs under uid 10003 (setpriv cap-clear) under the uid split, exactly
 * like the shell effect surface, and unchanged single-uid.
 */
export function spawnFileopHelper(spec: FileopHelperSpec, deps: SpawnFileopHelperDeps = {}): FileopHelperHandle {
  const wrapped = commandRootCommand(spec.fileopBin, ["--root", spec.worktreePath]);
  const proc = (deps.spawnProcess ?? defaultSpawnFileopProcess)(wrapped.command, wrapped.args, spec.env);
  if (!proc.stdin || !proc.stdout) {
    throw new Error("fileop helper process is missing a stdin/stdout channel");
  }
  const client = new FileopHelperClient({
    inbound: proc.stdout,
    outbound: proc.stdin,
    maxLineBytes: deps.maxLineBytes,
    requestTimeoutMs: deps.requestTimeoutMs,
  });
  const stdin = proc.stdin;
  return {
    client,
    dispose: async (): Promise<void> => {
      client.close();
      try {
        stdin.end();
      } catch {
        /* the process may already be gone; reaping is the safety lane's job */
      }
    },
  };
}
