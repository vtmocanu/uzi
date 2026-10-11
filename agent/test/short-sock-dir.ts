import fs from "node:fs";
import os from "node:os";
import path from "node:path";

/** Headroom under the 107-byte sun_path limit for `<dir>/data/ops/retained-source.sock` and a
 *  similar nested suffix, so a socket under the returned dir always fits. */
const SOCKET_SUFFIX_BUDGET = 45;
const SUN_PATH_BUDGET = 100;

/**
 * Create a temp dir that can host a Unix socket regardless of how long TMPDIR is. Under
 * `task gate:agent` TMPDIR is a long private scratch path, so a socket below it can exceed
 * sun_path; then the dir is made under /tmp instead. The caller owns removal (the tests
 * remove it in afterEach), which keeps both bases free of leaks.
 */
export function mkdtempForSocket(prefix: string): string {
  const mkdtempLen = prefix.length + 6;
  const viaTmpdir = os.tmpdir().length + 1 + mkdtempLen + SOCKET_SUFFIX_BUDGET;
  const base = viaTmpdir <= SUN_PATH_BUDGET ? os.tmpdir() : "/tmp";
  return fs.mkdtempSync(path.join(base, prefix));
}
