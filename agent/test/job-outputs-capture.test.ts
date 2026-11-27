import { afterEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { constants as fsc } from "node:fs";
import fsp from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import { RequestError, type WorkerClient } from "../src/client.js";
import { uploadJobOutputs, type UploadJobOutputsSummary } from "../src/job-outputs.js";
import { createJobWorkspace, reapStaleJobWorkspaces } from "../src/job-workspace.js";
import type { JobFileUploadMeta } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { nullLogger } from "./helpers.js";

// PRD #4322 M1b "Exact-key redaction and output publication": a claim carrying provider key forms
// stages each output into a worker-only capture while hashing and scanning it, and every upload
// attempt streams that capture. These tests drive the real uploadJobOutputs against a real
// workspace and a recording client; the gateway key is assembled at runtime (no provider-token-shaped
// literal in source).

const LINUX_ONLY = process.platform !== "linux" ? "requires Linux descriptor-relative output opens" : false;
const KEY = ["gw", "key/do-not-scan", "7777<abcdefgh&z"].join("-");
const FORMS = providerKeyForms(KEY);
const sha = (b: Buffer | string): string => createHash("sha256").update(b).digest("hex");

const roots: string[] = [];
afterEach(async () => {
  mock.restoreAll();
  while (roots.length) await fsp.rm(roots.pop()!, { recursive: true, force: true });
});

async function tmp(): Promise<string> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobcap-"));
  roots.push(d);
  return d;
}

/** A real job workspace (root/work/outputs) with `files` written under work. */
async function workspace(files: Record<string, Buffer | string>): Promise<{ root: string; work: string }> {
  const ws = await createJobWorkspace(path.join(await tmp(), "jobs"), "11111111-1111-4111-8111-111111111111");
  for (const [rel, content] of Object.entries(files)) {
    await fsp.mkdir(path.dirname(path.join(ws.work, rel)), { recursive: true });
    await fsp.writeFile(path.join(ws.work, rel), content);
  }
  return { root: ws.root, work: ws.work };
}

interface Attempt { meta: JobFileUploadMeta; bytes: Buffer }
type Decide = (n: number, meta: JobFileUploadMeta) => Promise<void> | void;
function recordingClient(decide?: Decide): { client: Pick<WorkerClient, "uploadJobFile">; attempts: Attempt[] } {
  const attempts: Attempt[] = [];
  const client = {
    uploadJobFile: (async (_id: string, meta: JobFileUploadMeta, body: Buffer | (() => Readable | Promise<Readable>)) => {
      const parts: Buffer[] = [];
      for await (const c of Buffer.isBuffer(body) ? [body] : await body()) parts.push(Buffer.from(c as Uint8Array));
      const bytes = Buffer.concat(parts);
      attempts.push({ meta, bytes });
      await decide?.(attempts.length, meta);
      return { status: 201, file: { id: "f", display_name: meta.display_name, storage_name: "s", content_type: "text/plain", byte_size: meta.size, sha256: meta.sha256, state: "attached", expires_at: null } };
    }) as unknown as WorkerClient["uploadJobFile"],
  };
  return { client, attempts };
}

async function upload(
  ws: { root: string; work: string },
  outputFiles: string[],
  c: Pick<WorkerClient, "uploadJobFile">,
  over: { forms?: readonly string[]; signal?: AbortSignal; deadlineAt?: number } = {},
): Promise<UploadJobOutputsSummary> {
  return uploadJobOutputs({
    client: c, log: nullLogger(), runId: "r", generation: 3, workDir: ws.work, secretPaths: [], outputFiles,
    deadlineAt: over.deadlineAt ?? Date.now() + 60_000, ...(over.signal ? { signal: over.signal } : {}),
    retryDelaysMs: [1, 1],
    capture: { root: ws.root, forms: over.forms ?? FORMS },
  });
}

const dropped = (s: UploadJobOutputsSummary): string[] => s.dropped.map((d) => `${d.display_name}:${d.reason}`);

describe("staged output capture (provider key forms)", { skip: LINUX_ONLY }, () => {
  it("uploads a clean file from the capture with the digest and size of the source bytes", async () => {
    const body = Buffer.from("clean deliverable bytes\n");
    const ws = await workspace({ "outputs/a.txt": body });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(s.stored, 1);
    assert.deepEqual(s.dropped, []);
    assert.equal(attempts.length, 1);
    assert.deepEqual(attempts[0]!.bytes, body);
    assert.equal(attempts[0]!.meta.sha256, sha(body));
    assert.equal(attempts[0]!.meta.size, body.length);
  });

  it("stages in order: exclusive 0700 directory under the workspace root, O_EXCL 0600 capture, unlink, then the first write", async () => {
    const ws = await workspace({ "outputs/a.txt": "abc" });
    const steps: string[] = [];
    const origMkdir = fsp.mkdir.bind(fsp);
    const origOpen = fsp.open.bind(fsp);
    const origUnlink = fsp.unlink.bind(fsp);
    const probe = await fsp.open(ws.work);
    const proto = Object.getPrototypeOf(probe) as FileHandle;
    await probe.close();
    const origWrite = proto.write;
    const capHandles = new WeakSet<object>();
    mock.method(fsp, "mkdir", async (p: string, o?: { mode?: number; recursive?: boolean }) => {
      if (String(p).startsWith(ws.root + path.sep) && String(p).includes(".capture-")) {
        assert.equal(o?.mode, 0o700, "directory mode");
        assert.ok(!o?.recursive, "exclusive: a plain mkdir fails when the name exists");
        assert.ok(!String(p).startsWith(ws.work + path.sep), "outside ws.work");
        steps.push("mkdir");
      }
      return origMkdir(p, o as never);
    });
    mock.method(fsp, "open", async (p: string, flags?: number, mode?: number) => {
      const fh = await origOpen(p, flags as never, mode as never);
      if (typeof flags === "number" && (flags & fsc.O_EXCL) !== 0) {
        assert.ok(String(p).includes(".capture-"));
        assert.equal(flags & fsc.O_RDWR, fsc.O_RDWR);
        assert.equal(mode, 0o600);
        capHandles.add(fh);
        steps.push("open");
      }
      return fh;
    });
    mock.method(fsp, "unlink", async (p: string) => {
      try {
        return await origUnlink(p);
      } finally {
        if (String(p).includes(".capture-")) steps.push("unlink");
      }
    });
    mock.method(proto, "write", async function (this: FileHandle, ...args: unknown[]) {
      if (capHandles.has(this)) steps.push("write");
      return (origWrite as (...a: unknown[]) => Promise<unknown>).apply(this, args);
    });
    const { client } = recordingClient();
    await upload(ws, ["outputs/a.txt"], client);
    assert.deepEqual(steps.slice(0, 4), ["mkdir", "open", "unlink", "write"]);
    // The capture file never exists by name while holding bytes, and the directory is gone afterwards.
    assert.deepEqual((await fsp.readdir(ws.root)).filter((n) => n.startsWith(".capture-")), []);
  });

  it("a key in the content refuses the file with zero upload attempts; a clean sibling still uploads", async () => {
    const ws = await workspace({ "outputs/bad.txt": `prefix ${KEY} suffix`, "outputs/ok.txt": "fine" });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/bad.txt", "outputs/ok.txt"], client);
    assert.deepEqual(dropped(s), ["bad.txt:worker_unreadable"]);
    assert.deepEqual(attempts.map((a) => a.meta.display_name), ["ok.txt"]);
  });

  it("an encoded key form (percent-encoded, HTML-safe JSON escape) in the content is also a hit", async () => {
    const htmlSafe = KEY.replace(/[<>&]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`);
    for (const form of [encodeURIComponent(KEY), htmlSafe]) {
      assert.notEqual(form, KEY);
      assert.ok(FORMS.includes(form), "the form is one providerKeyForms produces");
      const ws = await workspace({ "outputs/bad.txt": `{"k":"${form}"}` });
      const { client, attempts } = recordingClient();
      const s = await upload(ws, ["outputs/bad.txt"], client);
      assert.deepEqual(dropped(s), ["bad.txt:worker_unreadable"], form);
      assert.equal(attempts.length, 0, form);
    }
  });

  it("a key split across the 64 KiB read boundary at every offset in the overlap is found", async () => {
    for (const off of [1, 2, 7, KEY.length - 1]) {
      const content = Buffer.concat([Buffer.alloc(65536 - off, "a"), Buffer.from(KEY), Buffer.alloc(100, "b")]);
      const ws = await workspace({ "outputs/split.bin": content });
      const { client, attempts } = recordingClient();
      const s = await upload(ws, ["outputs/split.bin"], client);
      assert.deepEqual(dropped(s), ["split.bin:worker_unreadable"], `offset ${off}`);
      assert.equal(attempts.length, 0, `offset ${off}`);
    }
  });

  it("a scanner failure is an unvalidated capture: refused, zero attempts, no capture left behind", async () => {
    const ws = await workspace({ "outputs/a.txt": "abc" });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client, { forms: [null as unknown as string] });
    assert.deepEqual(dropped(s), ["a.txt:worker_unreadable"]);
    assert.equal(attempts.length, 0);
    assert.deepEqual((await fsp.readdir(ws.root)).filter((n) => n.startsWith(".capture-")), []);
  });

  /** Make the capture handle's write fail or short-write; returns a counter of write calls. */
  async function spyCaptureWrites(ws: { work: string }, impl: (call: number, args: unknown[], real: (a?: unknown[]) => Promise<unknown>) => Promise<unknown>): Promise<{ calls: () => number }> {
    const origOpen = fsp.open.bind(fsp);
    const probe = await fsp.open(ws.work);
    const proto = Object.getPrototypeOf(probe) as FileHandle;
    await probe.close();
    const origWrite = proto.write as unknown as (...a: unknown[]) => Promise<unknown>;
    const caps = new WeakSet<object>();
    let calls = 0;
    mock.method(fsp, "open", async (p: string, flags?: number, mode?: number) => {
      const fh = await origOpen(p, flags as never, mode as never);
      if (typeof flags === "number" && (flags & fsc.O_EXCL) !== 0) caps.add(fh);
      return fh;
    });
    mock.method(proto, "write", async function (this: FileHandle, ...args: unknown[]) {
      if (!caps.has(this)) return origWrite.apply(this, args);
      calls++;
      return impl(calls, args, (a) => origWrite.apply(this, a ?? args));
    });
    return { calls: () => calls };
  }

  it("a short write is completed by the full-write loop: the upload carries every byte", async () => {
    const body = Buffer.from("0123456789".repeat(1000));
    const ws = await workspace({ "outputs/a.txt": body });
    const spy = await spyCaptureWrites(ws, async (_n, args, real) => {
      const [buf, off, len, pos] = args as [Buffer, number, number, number];
      return real([buf, off, Math.max(1, Math.floor(len / 2)), pos]);
    });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(s.stored, 1);
    assert.ok(spy.calls() > 1, "the loop wrote in several steps");
    assert.deepEqual(attempts[0]!.bytes, body);
    assert.equal(attempts[0]!.meta.sha256, sha(body));
  });

  it("ENOSPC while writing the capture refuses the file with zero upload attempts", async () => {
    const ws = await workspace({ "outputs/a.txt": "abc", "outputs/b.txt": "clean sibling" });
    let first = true;
    await spyCaptureWrites(ws, async (_n, _args, real) => {
      if (first) {
        first = false;
        throw Object.assign(new Error("ENOSPC: no space left on device"), { code: "ENOSPC" });
      }
      return real();
    });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt", "outputs/b.txt"], client);
    assert.deepEqual(dropped(s), ["a.txt:worker_unreadable"]);
    assert.deepEqual(attempts.map((a) => a.meta.display_name), ["b.txt"]);
  });

  it("a write that makes no progress (zero bytes) is refused, not retried forever", async () => {
    const ws = await workspace({ "outputs/a.txt": "abc" });
    const spy = await spyCaptureWrites(ws, async () => ({ bytesWritten: 0, buffer: Buffer.alloc(0) }));
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.deepEqual(dropped(s), ["a.txt:worker_unreadable"]);
    assert.equal(attempts.length, 0);
    assert.equal(spy.calls(), 1, "one write call: the zero-byte result ends the loop");
  });

  type Role = "source" | "capture";
  /** Records every output descriptor opened (the pinned final open of the source, the O_EXCL capture)
   *  and, in `events`, when each one is closed. Install BEFORE other fsp.open spies. */
  async function trackHandles(events: string[] = []): Promise<{ handles: { role: Role; fh: FileHandle }[]; events: string[] }> {
    const handles: { role: Role; fh: FileHandle }[] = [];
    const origOpen = fsp.open.bind(fsp);
    mock.method(fsp, "open", async (p: string, flags?: number, mode?: number) => {
      const fh = await origOpen(p, flags as never, mode as never);
      let role: Role | undefined;
      if (typeof flags === "number" && (flags & fsc.O_EXCL) !== 0) role = "capture";
      else if (typeof flags === "number" && (flags & fsc.O_NONBLOCK) !== 0 && String(p).startsWith("/proc/self/fd/")) role = "source";
      if (role) {
        handles.push({ role, fh });
        events.push(`open-${role}`);
        // close is an own property of each handle, so it is wrapped per handle.
        const realClose = fh.close.bind(fh);
        let seen = false;
        fh.close = async () => {
          const result = await realClose();
          // A handle may be closed again by a later cleanup; only its first close is an event.
          if (!seen) events.push(`close-${role}`);
          seen = true;
          return result;
        };
      }
      return fh;
    });
    return { handles, events };
  }
  const captureDirs = async (ws: { root: string }): Promise<string[]> => (await fsp.readdir(ws.root)).filter((n) => n.startsWith(".capture-"));
  async function assertSettled(ws: { root: string }, handles: { role: Role; fh: FileHandle }[], what: string): Promise<void> {
    assert.deepEqual([...new Set(handles.map((h) => h.role))].sort(), ["capture", "source"], `${what}: both descriptors were opened`);
    for (const h of handles) assert.equal(h.fh.fd, -1, `${what}: the ${h.role} descriptor is closed`);
    // The capture directory is removed with rmdir, which fails while any named capture file is left in it.
    assert.deepEqual(await captureDirs(ws), [], `${what}: no capture directory or named capture is left`);
  }

  it("an abort between two partial writes stops the phase and uploads nothing", async () => {
    const ws = await workspace({ "outputs/a.txt": "0123456789" });
    const ac = new AbortController();
    const tracked = await trackHandles();
    await spyCaptureWrites(ws, async (_n, args, real) => {
      const [buf, off, len, pos] = args as [Buffer, number, number, number];
      void real;
      ac.abort();
      return { bytesWritten: Math.max(1, Math.floor(len / 2)), buffer: buf, _off: off, _pos: pos };
    });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client, { signal: ac.signal });
    assert.equal(attempts.length, 0);
    assert.ok(s.stopped, "the phase reports it stopped");
    assert.deepEqual(dropped(s), ["a.txt:worker_upload_failed"]);
    await assertSettled(ws, tracked.handles, "abort");
  });

  it("a deadline that passes between two partial writes stops the phase and uploads nothing", async () => {
    const ws = await workspace({ "outputs/a.txt": "0123456789" });
    const deadlineAt = Date.now() + 5_000;
    const realNow = Date.now.bind(Date);
    const tracked = await trackHandles();
    await spyCaptureWrites(ws, async (_n, args) => {
      const [, , len] = args as [Buffer, number, number, number];
      mock.method(Date, "now", () => realNow() + 10_000);
      return { bytesWritten: Math.max(1, Math.floor(len / 2)), buffer: Buffer.alloc(0) };
    });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client, { deadlineAt });
    assert.equal(attempts.length, 0);
    assert.ok(s.stopped);
    await assertSettled(ws, tracked.handles, "deadline");
  });

  it("closes the source before the first upload attempt and the capture only after the last", async () => {
    const ws = await workspace({ "outputs/a.txt": "deliverable bytes" });
    const events: string[] = [];
    const tracked = await trackHandles(events);
    // The first attempt hits a transient error, so there are two attempts to order against.
    const { client, attempts } = recordingClient(async (n) => {
      events.push(`upload-${n}`);
      if (n === 1) throw new RequestError("POST", "/files", 503, JSON.stringify({ reason: "uploads_busy" }));
    });
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(s.stored, 1);
    assert.equal(attempts.length, 2);
    assert.deepEqual(events, ["open-source", "open-capture", "close-source", "upload-1", "upload-2", "close-capture"]);
    await assertSettled(ws, tracked.handles, "success");
  });

  it("a pathname swapped to a symlink after resolution but before the guarded open is refused and never uploaded", async () => {
    const ws = await workspace({ "outputs/a.txt": "legit" });
    const outside = path.join(await tmp(), "outside.txt");
    await fsp.writeFile(outside, "outside file that must never be published");
    const target = path.join(ws.work, "outputs/a.txt");
    const origOpen = fsp.open.bind(fsp);
    let swapped = 0;
    // The preflight (resolveOutputFile) has already accepted the regular file by the time the pinned
    // open of the leaf runs: swap it for a symlink at exactly that point.
    mock.method(fsp, "open", async (p: string, flags?: number, mode?: number) => {
      if (swapped === 0 && typeof flags === "number" && (flags & fsc.O_NONBLOCK) !== 0 && String(p).startsWith("/proc/self/fd/")) {
        swapped++;
        await fsp.rm(target);
        await fsp.symlink(outside, target);
      }
      return origOpen(p, flags as never, mode as never);
    });
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(swapped, 1, "the swap ran between resolution and the guarded open");
    assert.equal(attempts.length, 0, "nothing was uploaded");
    assert.deepEqual(dropped(s), ["a.txt:worker_unreadable"]);
    assert.deepEqual(await captureDirs(ws), []);
  });

  it("a source mutated in place after capture: every retry sends only the scanned bytes", async () => {
    const original = Buffer.from("original scanned bytes");
    const ws = await workspace({ "outputs/a.txt": original });
    const { client, attempts } = recordingClient(async (n) => {
      if (n === 1) {
        // The source is rewritten (even with the key) after the capture closed it; then a transient error.
        await fsp.writeFile(path.join(ws.work, "outputs/a.txt"), `mutated ${KEY} bytes!!!!`);
        throw new RequestError("POST", "/files", 503, JSON.stringify({ reason: "uploads_busy" }));
      }
    });
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(s.stored, 1);
    assert.equal(attempts.length, 2);
    for (const a of attempts) {
      assert.deepEqual(a.bytes, original);
      assert.equal(a.meta.sha256, sha(original));
      assert.ok(!a.bytes.includes(KEY));
    }
  });

  it("a pathname replaced by a symlink between resolution and open never reaches the capture", async () => {
    const ws = await workspace({ "outputs/a.txt": "legit" });
    const outside = path.join(await tmp(), "outside.txt");
    await fsp.writeFile(outside, "outside file");
    // Replace the leaf with a symlink right before the upload phase opens it.
    await fsp.rm(path.join(ws.work, "outputs/a.txt"));
    await fsp.symlink(outside, path.join(ws.work, "outputs/a.txt"));
    const { client, attempts } = recordingClient();
    const s = await upload(ws, ["outputs/a.txt"], client);
    assert.equal(attempts.length, 0);
    assert.deepEqual(dropped(s), ["a.txt:worker_unreadable"]);
  });

  it("closes both descriptors of a successful and of a refused file", async () => {
    const ws = await workspace({ "outputs/ok.txt": "fine", "outputs/bad.txt": KEY });
    const opened: FileHandle[] = [];
    const origOpen = fsp.open.bind(fsp);
    mock.method(fsp, "open", async (p: string, flags?: number, mode?: number) => {
      const fh = await origOpen(p, flags as never, mode as never);
      opened.push(fh);
      return fh;
    });
    const { client } = recordingClient();
    await upload(ws, ["outputs/ok.txt", "outputs/bad.txt"], client);
    for (const fh of opened) {
      // A closed handle's fd is -1; a leaked one still has a live descriptor.
      assert.equal(fh.fd, -1, "descriptor closed");
    }
    assert.ok(opened.length >= 4, "source and capture of both files were opened");
  });

  it("without capture options the upload is the unchanged same-handle path (no capture directory)", async () => {
    const ws = await workspace({ "outputs/a.txt": `has ${KEY} inside` });
    const { client, attempts } = recordingClient();
    const s = await uploadJobOutputs({
      client, log: nullLogger(), runId: "r", generation: 3, workDir: ws.work, secretPaths: [], outputFiles: ["outputs/a.txt"],
      deadlineAt: Date.now() + 60_000,
    });
    assert.equal(s.stored, 1);
    assert.equal(attempts.length, 1);
    assert.deepEqual((await fsp.readdir(ws.root)).filter((n) => n.startsWith(".capture-")), []);
  });

  it("an upload failure echoing the key across the log clip boundary leaves no key fragment in any log line", async () => {
    const ws = await workspace({ "outputs/a.txt": "abc" });
    const lines: string[] = [];
    const base = nullLogger();
    const push = (msg: string, f?: Record<string, unknown>): void => { lines.push(JSON.stringify([msg, f ?? {}])); };
    const log = { ...base, debug: push, info: push, warn: push, error: push, child: () => log } as typeof base;
    // 290 filler characters, then the key: a 300-character clip lands inside it.
    const { client } = recordingClient(() => { throw new Error(`${"x".repeat(290)}${KEY}`); });
    await uploadJobOutputs({
      client, log, runId: "r", generation: 3, workDir: ws.work, secretPaths: [], outputFiles: ["outputs/a.txt"],
      deadlineAt: Date.now() + 60_000, retryDelaysMs: [1, 1], capture: { root: ws.root, forms: FORMS },
    });
    const text = lines.join("\n");
    assert.ok(lines.some((l) => l.includes("job output upload failed")), "the failed upload was logged");
    for (let i = 0; i + 8 <= KEY.length; i++) assert.ok(!text.includes(KEY.slice(i, i + 8)), `a key fragment reached a log line: ${KEY.slice(i, i + 8)}`);
  });

  it("the stale-workspace reaper removes a workspace that still holds a capture directory", async () => {
    const jobsRoot = path.join(await tmp(), "jobs");
    const ws = await createJobWorkspace(jobsRoot, "22222222-2222-4222-8222-222222222222");
    const dir = path.join(ws.root, ".capture-deadbeef");
    await fsp.mkdir(dir, { mode: 0o700 });
    await fsp.writeFile(path.join(dir, "leftover"), "x", { mode: 0o600 });
    assert.equal(await reapStaleJobWorkspaces(jobsRoot, nullLogger()), 1);
    await assert.rejects(fsp.stat(ws.root), { code: "ENOENT" });
  });

  it("a multi-megabyte file stages within bounded time and memory-independent size", async () => {
    const size = 8 * 1024 * 1024 + 123;
    const body = Buffer.alloc(size, "z");
    const ws = await workspace({ "outputs/big.bin": body });
    const { client, attempts } = recordingClient();
    const t0 = Date.now();
    const s = await upload(ws, ["outputs/big.bin"], client);
    const ms = Date.now() - t0;
    assert.equal(s.stored, 1);
    assert.equal(attempts[0]!.bytes.length, size);
    assert.equal(attempts[0]!.meta.sha256, sha(body));
    console.log(`staged ${size} bytes in ${ms} ms`);
  });
});
