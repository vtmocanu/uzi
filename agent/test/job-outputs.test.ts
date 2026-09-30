import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import { RequestError, type WorkerClient } from "../src/client.js";
import {
  JOB_UPLOAD_ALLOWANCE_MS,
  logSafe,
  OUTPUT_FILES_MAX,
  outputDisplayName,
  resolveOutputFile,
  uploadJobOutputs,
  uploadPhaseDeadline,
  validateOutputFilePaths,
} from "../src/job-outputs.js";
import type { JobFileUploadMeta } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// PRD #1909 M4: the lexical and filesystem checks on the files a job may hand back.

const roots: string[] = [];
afterEach(async () => {
  while (roots.length) await fsp.rm(roots.pop()!, { recursive: true, force: true });
});
async function workspace(): Promise<{ work: string; outside: string }> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-joboutputs-"));
  roots.push(d);
  const work = path.join(d, "work");
  await fsp.mkdir(path.join(work, "outputs"), { recursive: true });
  await fsp.mkdir(path.join(work, "sources"));
  await fsp.mkdir(path.join(work, "inputs"));
  const outside = path.join(d, "outside.txt");
  await fsp.writeFile(outside, "secret");
  return { work, outside };
}

describe("validateOutputFilePaths", () => {
  it("accepts an absent list, and paths under outputs/ or sources/, dropping duplicates and keeping order", () => {
    assert.deepStrictEqual(validateOutputFilePaths(undefined), { ok: true, value: [] });
    assert.deepStrictEqual(validateOutputFilePaths(null), { ok: true, value: [] });
    assert.deepStrictEqual(validateOutputFilePaths(["outputs/b.txt", "sources/abc", "outputs/b.txt", "outputs/d/e.csv"]), {
      ok: true,
      value: ["outputs/b.txt", "sources/abc", "outputs/d/e.csv"],
    });
  });

  const bad: Array<[string, unknown]> = [
    ["a non-array", "outputs/a"],
    ["a non-string entry", [1]],
    ["an empty string", [""]],
    ["an absolute path", ["/etc/passwd"]],
    ["a home-relative path", ["~/x"]],
    ["a parent segment", ["outputs/../inputs/01-doc.md"]],
    ["a leading parent segment", ["../x"]],
    ["a current-directory segment", ["outputs/./a"]],
    ["an empty segment", ["outputs//a"]],
    ["a trailing slash", ["outputs/a/"]],
    ["a backslash", ["outputs\\a"]],
    ["a control character", ["outputs/a\nb"]],
    ["a bidi override", ["outputs/a\u202eb.txt"]],
    ["a zero-width space", ["outputs/a\u200bb.txt"]],
    ["a byte order mark", ["outputs/\ufeffa.txt"]],
    ["a line separator", ["outputs/a\u2028b.txt"]],
    ["a paragraph separator", ["outputs/a\u2029b.txt"]],
    ["a next-line control", ["outputs/a\u0085b.txt"]],
    ["a private-use character", ["outputs/a\ue000b.txt"]],
    ["a lone surrogate", ["outputs/a\ud800b.txt"]],
    ["report.md", ["outputs/report.md"]],
    ["report.md in any case", ["outputs/sub/REPORT.MD"]],
    ["findings.json", ["sources/findings.json"]],
    ["the same file name under two directories", ["outputs/a/x.csv", "outputs/b/x.csv"]],
    ["the same file name in another case", ["outputs/x.csv", "outputs/sub/X.CSV"]],
    ["a NUL", ["outputs/a\0b"]],
    ["the bare directory", ["outputs"]],
    ["another workspace directory", ["inputs/01-doc.md"]],
    ["a look-alike directory", ["outputs2/a"]],
    ["a path over 1024 bytes", ["outputs/" + "a".repeat(1025)]],
    ["more than the cap", Array.from({ length: OUTPUT_FILES_MAX + 1 }, (_, i) => `outputs/${i}`)],
  ];
  for (const [name, input] of bad) {
    it(`refuses ${name}`, () => {
      assert.strictEqual(validateOutputFilePaths(input).ok, false);
    });
  }

  it("accepts exactly the cap", () => {
    const r = validateOutputFilePaths(Array.from({ length: OUTPUT_FILES_MAX }, (_, i) => `outputs/${i}`));
    assert.ok(r.ok && r.value.length === OUTPUT_FILES_MAX);
  });
});

describe("resolveOutputFile", () => {
  it("resolves a regular file to its real path with a basename display name", async () => {
    const { work } = await workspace();
    await fsp.mkdir(path.join(work, "outputs/sub"));
    await fsp.writeFile(path.join(work, "outputs/sub/r.md"), "x");
    const r = resolveOutputFile(work, "outputs/sub/r.md");
    assert.ok(r.ok);
    assert.strictEqual(r.value.real, await fsp.realpath(path.join(work, "outputs/sub/r.md")));
    assert.strictEqual(r.value.displayName, "r.md");
  });

  it("accepts a symlink that stays inside outputs/ or sources/, and refuses one that leaves them", async () => {
    const { work, outside } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    await fsp.writeFile(path.join(work, "sources/s"), "s");
    await fsp.writeFile(path.join(work, "inputs/i.txt"), "i");
    await fsp.symlink(path.join(work, "outputs/a.txt"), path.join(work, "outputs/in-outputs"));
    await fsp.symlink(path.join(work, "sources/s"), path.join(work, "outputs/in-sources"));
    await fsp.symlink(outside, path.join(work, "outputs/out"));
    await fsp.symlink(path.join(work, "inputs/i.txt"), path.join(work, "outputs/to-inputs"));
    await fsp.symlink("../../outside.txt", path.join(work, "outputs/rel-out"));
    await fsp.symlink("/etc/hostname", path.join(work, "outputs/system"));
    assert.ok(resolveOutputFile(work, "outputs/in-outputs").ok);
    assert.ok(resolveOutputFile(work, "outputs/in-sources").ok);
    for (const rel of ["outputs/out", "outputs/to-inputs", "outputs/rel-out", "outputs/system"]) {
      assert.strictEqual(resolveOutputFile(work, rel).ok, false, rel);
    }
  });

  it("refuses a symlinked outputs/ directory that points elsewhere", async () => {
    const { work } = await workspace();
    const elsewhere = path.join(path.dirname(work), "elsewhere");
    await fsp.mkdir(elsewhere);
    await fsp.writeFile(path.join(elsewhere, "f.txt"), "f");
    await fsp.rm(path.join(work, "outputs"), { recursive: true });
    await fsp.symlink(elsewhere, path.join(work, "outputs"));
    assert.strictEqual(resolveOutputFile(work, "outputs/f.txt").ok, false);
  });

  it("refuses a directory, a missing file and a non-regular file", async () => {
    const { work } = await workspace();
    await fsp.mkdir(path.join(work, "outputs/d"));
    assert.strictEqual(resolveOutputFile(work, "outputs/d").ok, false);
    assert.strictEqual(resolveOutputFile(work, "outputs/none.txt").ok, false);
    try {
      execFileSync("mkfifo", [path.join(work, "outputs/pipe")]);
    } catch {
      return; // no mkfifo on this host: the directory case above already covers "not a regular file"
    }
    assert.strictEqual(resolveOutputFile(work, "outputs/pipe").ok, false, "a FIFO must never be opened for reading");
  });

  it("denies a secret path through the path guard", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/token"), "t");
    const real = await fsp.realpath(path.join(work, "outputs/token"));
    assert.strictEqual(resolveOutputFile(work, "outputs/token", [real]).ok, false);
  });
});

describe("outputDisplayName", () => {
  it("is the basename, without control or invisible characters", () => {
    assert.strictEqual(outputDisplayName("outputs/a/b/report.pdf"), "report.pdf");
    assert.strictEqual(outputDisplayName("outputs/x‮y.txt"), "xy.txt");
    assert.strictEqual(outputDisplayName("outputs/ ​ "), "output");
    assert.strictEqual(outputDisplayName("outputs/résumé 報告.md"), "résumé 報告.md");
  });
  it("bounds a long name to 100 code points and keeps a short extension", () => {
    const n = outputDisplayName("outputs/" + "a".repeat(300) + ".csv");
    assert.strictEqual(Array.from(n).length, 100);
    assert.ok(n.endsWith(".csv"));
    assert.strictEqual(Array.from(outputDisplayName("outputs/" + "b".repeat(300))).length, 100);
  });
});


describe("uploadJobOutputs (PRD #1909 M4 rework)", () => {
  const sha = (b: string | Buffer): string => createHash("sha256").update(b).digest("hex");
  type Upload = (meta: JobFileUploadMeta, body: Buffer | (() => Readable | Promise<Readable>), signal?: AbortSignal, timeoutMs?: number, attempt?: number) => Promise<{ status: number; file: never }>;

  async function drain(body: Buffer | (() => Readable | Promise<Readable>)): Promise<Buffer> {
    if (Buffer.isBuffer(body)) return body;
    const parts: Buffer[] = [];
    for await (const c of await body()) parts.push(Buffer.from(c as Uint8Array));
    return Buffer.concat(parts);
  }

  /** A client whose uploads run `fn`; every attempt is recorded with the bytes it sent. */
  function client(fn?: Upload): { client: Pick<WorkerClient, "uploadJobFile">; sent: Array<{ meta: JobFileUploadMeta; bytes: Buffer }>; attempts: () => number } {
    const sent: Array<{ meta: JobFileUploadMeta; bytes: Buffer }> = [];
    let attempts = 0;
    return {
      sent,
      attempts: () => attempts,
      client: {
        uploadJobFile: (async (_id: string, meta: JobFileUploadMeta, body: Buffer | (() => Readable | Promise<Readable>), signal?: AbortSignal, timeoutMs?: number) => {
          attempts++;
          if (fn) return fn(meta, body, signal, timeoutMs, attempts);
          sent.push({ meta, bytes: await drain(body) });
          return { status: 201, file: {} as never };
        }) as unknown as WorkerClient["uploadJobFile"],
      },
    };
  }

  const run = (
    work: string,
    c: Pick<WorkerClient, "uploadJobFile">,
    files: string[],
    over: Partial<Parameters<typeof uploadJobOutputs>[0]> = {},
  ): ReturnType<typeof uploadJobOutputs> =>
    uploadJobOutputs({ client: c, log: nullLogger(), runId: "r", generation: 3, workDir: work, secretPaths: [], outputFiles: files, deadlineAt: Date.now() + 60_000, retryDelaysMs: [1, 1], ...over });

  it("uploads each listed file from one handle: hash, size and bytes agree even when the path is swapped mid-upload", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "original bytes");
    const c = client(async (meta, body) => {
      // The path is replaced BEFORE the body is read: the upload must still send the file it hashed.
      await fsp.writeFile(path.join(work, "outputs/swap.tmp"), "REPLACED CONTENT!!");
      await fsp.rename(path.join(work, "outputs/swap.tmp"), path.join(work, "outputs/a.txt"));
      const bytes = await drain(body);
      c.sent.push({ meta, bytes });
      return { status: 201, file: {} as never };
    });
    const summary = await run(work, c.client, ["outputs/a.txt"]);
    assert.strictEqual(summary.stored, 1);
    assert.strictEqual(c.sent[0]!.bytes.toString(), "original bytes", "the upload read the opened handle, not the path");
    assert.strictEqual(c.sent[0]!.meta.sha256, sha("original bytes"));
    assert.strictEqual(c.sent[0]!.meta.size, "original bytes".length);
  });

  it("a retry re-reads the same handle from offset 0", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "retry me");
    const bodies: string[] = [];
    const c = client(async (_meta, body, _s, _t, attempt) => {
      bodies.push((await drain(body)).toString());
      if (attempt === 1) throw new TypeError("fetch failed");
      return { status: 201, file: {} as never };
    });
    assert.strictEqual((await run(work, c.client, ["outputs/a.txt"])).stored, 1);
    assert.deepStrictEqual(bodies, ["retry me", "retry me"]);
  });

  it("refuses a file with more than one hard link, reported as worker_unreadable", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    await fsp.link(path.join(work, "outputs/a.txt"), path.join(work, "outputs/linked.txt"));
    const c = client();
    const summary = await run(work, c.client, ["outputs/linked.txt"]);
    assert.strictEqual(c.attempts(), 0);
    assert.deepStrictEqual(summary.dropped, [{ display_name: "linked.txt", reason: "worker_unreadable" }]);
  });

  it("refuses an empty file (worker_empty) and a missing one (worker_unreadable) without uploading, and goes on", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/empty.csv"), "");
    await fsp.writeFile(path.join(work, "outputs/ok.txt"), "ok");
    const c = client();
    const summary = await run(work, c.client, ["outputs/empty.csv", "outputs/gone.txt", "outputs/ok.txt"]);
    assert.deepStrictEqual(c.sent.map((u) => u.meta.display_name), ["ok.txt"]);
    assert.strictEqual(summary.stored, 1);
    assert.deepStrictEqual(summary.dropped, [
      { display_name: "gone.txt", reason: "worker_unreadable" }, // resolved (and refused) first
      { display_name: "empty.csv", reason: "worker_empty" },
    ]);
  });

  it("checks the per-file ceiling from fstat BEFORE reading a byte: a sparse 2 GiB file is refused at once (worker_too_large)", async () => {
    const { work } = await workspace();
    const big = path.join(work, "outputs/huge.bin");
    const fh = await fsp.open(big, "w");
    try {
      await fh.truncate(2 * 1024 * 1024 * 1024);
    } finally {
      await fh.close();
    }
    const c = client();
    const t0 = Date.now();
    const summary = await run(work, c.client, ["outputs/huge.bin"]);
    const took = Date.now() - t0;
    assert.strictEqual(c.attempts(), 0);
    assert.deepStrictEqual(summary.dropped, [{ display_name: "huge.bin", reason: "worker_too_large" }]);
    assert.ok(took < 1500, `refused after ${took} ms: hashing 2 GiB would take longer, so the file was read before the ceiling check`);
  });

  it("checks the deadline while hashing: a file still being hashed when the deadline passes stops the phase and is reported", async () => {
    const { work } = await workspace();
    const fh = await fsp.open(path.join(work, "outputs/slow.bin"), "w");
    try {
      await fh.truncate(200 * 1024 * 1024); // under the 256 MiB ceiling: hashed, not refused
    } finally {
      await fh.close();
    }
    await fsp.writeFile(path.join(work, "outputs/after.txt"), "after");
    const c = client();
    const summary = await run(work, c.client, ["outputs/slow.bin", "outputs/after.txt"], { deadlineAt: Date.now() + 20 });
    assert.strictEqual(c.attempts(), 0, "nothing is uploaded once the deadline passed mid-hash");
    assert.match(summary.stopped ?? "", /time is spent/);
    assert.deepStrictEqual(summary.dropped, [
      { display_name: "slow.bin", reason: "worker_upload_failed" },
      { display_name: "after.txt", reason: "worker_upload_failed" },
    ]);
  });

  it("an aborted signal stops the phase", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    const ac = new AbortController();
    ac.abort();
    const c = client();
    const summary = await run(work, c.client, ["outputs/a.txt"], { signal: ac.signal });
    assert.strictEqual(c.attempts(), 0);
    assert.match(summary.stopped ?? "", /aborted/);
  });

  it("honours Retry-After on a 503 uploads_busy instead of the fixed delay, and reports worker_busy when the retries run out", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    const stamps: number[] = [];
    const c = client(async () => {
      stamps.push(Date.now());
      throw new RequestError("POST", "/x", 503, '{"reason":"uploads_busy"}', 120);
    });
    const summary = await run(work, c.client, ["outputs/a.txt"], { retryDelaysMs: [1, 1] });
    assert.strictEqual(stamps.length, 3);
    assert.ok(stamps[1]! - stamps[0]! >= 100 && stamps[2]! - stamps[1]! >= 100, `gaps ${stamps[1]! - stamps[0]!}, ${stamps[2]! - stamps[1]!} ms must follow the 120 ms Retry-After`);
    assert.deepStrictEqual(summary.dropped, [{ display_name: "a.txt", reason: "worker_busy" }]);
  });

  it("caps a long Retry-After by the deadline", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    const c = client(async () => {
      throw new RequestError("POST", "/x", 503, '{"reason":"uploads_busy"}', 600_000);
    });
    const t0 = Date.now();
    const summary = await run(work, c.client, ["outputs/a.txt"], { deadlineAt: Date.now() + 300 });
    assert.ok(Date.now() - t0 < 3000, "the ten-minute Retry-After was cut to the time left");
    assert.strictEqual(c.attempts(), 1);
    assert.deepStrictEqual(summary.dropped, [{ display_name: "a.txt", reason: "worker_upload_failed" }]);
  });

  it("never logs an untrusted path or name raw: control and invisible characters are escaped", async () => {
    const { work } = await workspace();
    const { logger, lines } = recordingLogger();
    const c = client();
    await uploadJobOutputs({
      client: c.client,
      log: logger,
      runId: "r",
      generation: 1,
      workDir: work,
      secretPaths: [],
      outputFiles: ["outputs/a\u202eb\u001b[31m.txt"], // bypasses the lexical check on purpose
      deadlineAt: Date.now() + 1000,
    });
    assert.ok(lines.length > 0);
    for (const l of lines as Array<Record<string, unknown>>) {
      for (const [k, v] of Object.entries(l)) {
        if (typeof v === "string") assert.ok(!/[\p{C}\p{Zl}\p{Zp}]/u.test(v), `log field ${k} carries a raw control or invisible character: ${JSON.stringify(v)}`);
      }
    }
    assert.match(JSON.stringify(lines), /\\\\u202e/, "escaped, not dropped");
  });

  it("logSafe escapes format, control and separator characters and bounds the length", () => {
    assert.strictEqual(logSafe("a\u202eb\u0000c\u2028d"), "a\\u202eb\\u0000c\\u2028d");
    assert.strictEqual(logSafe("x".repeat(500), 10), "x".repeat(10));
  });

  it("the upload phase ends JOB_UPLOAD_ALLOWANCE_MS after the wall budget deadline, inside the api's 300 s backstop grace", () => {
    assert.strictEqual(uploadPhaseDeadline(1_000_000), 1_000_000 + JOB_UPLOAD_ALLOWANCE_MS);
    assert.ok(JOB_UPLOAD_ALLOWANCE_MS > 0 && JOB_UPLOAD_ALLOWANCE_MS <= 240_000, "leaves at least 60 s of the 300 s grace for the result post and the terminal report");
  });
});
