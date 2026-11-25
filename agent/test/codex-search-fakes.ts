import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fsp from "node:fs/promises";
import path from "node:path";

import type { FileopRequest, FileopResponse } from "../src/codex/broker.js";

/** Answer a `read_bytes` request from an in-memory body the way the Go helper does:
 *  at most `limit` bytes at `offset`, `size` and `ident` echoing the opening fstat. */
export function readBytesReply(
  body: Buffer,
  req: FileopRequest,
  opts: { size?: number; ident?: string } = {},
): FileopResponse {
  const offset = req.offset ?? 0;
  const limit = req.limit ?? 0;
  const start = Math.min(offset, body.length);
  const data = body.subarray(start, Math.min(start + limit, body.length));
  return { ok: true, data: data.toString("base64"), size: opts.size ?? body.length, offset, ident: opts.ident ?? "1:1:0:0" };
}

const repoRoot = path.resolve(import.meta.dirname, "../..");

/** Build the fileop helper from this tree into a fresh directory under <repo>/.uzi/scratch.
 *  Returns undefined when the build is unsupported (non-Linux). The caller removes `dir`. */
export async function buildFileopHelper(prefix: string): Promise<{ dir: string; binary: string } | undefined> {
  if (process.platform !== "linux") return undefined;
  await fsp.mkdir(path.join(repoRoot, ".uzi/scratch"), { recursive: true });
  const dir = await fsp.mkdtemp(path.join(repoRoot, ".uzi/scratch", prefix));
  const binary = path.join(dir, "fileop");
  const build = spawnSync("go", ["build", "-o", binary, "./fileop"], {
    cwd: path.join(repoRoot, "agent/codex/supervisor"), timeout: 120000, encoding: "utf8",
    env: { ...process.env, GOTOOLCHAIN: "local", GOFLAGS: "-buildvcs=false -mod=vendor" },
  });
  if (build.status !== 0) await fsp.rm(dir, { recursive: true, force: true });
  assert.equal(build.status, 0, build.stderr);
  return { dir, binary };
}
