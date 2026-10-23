// Standalone scripts: command Landlock cannot read fixture modules outside the checkout.
const nodeFS = String.raw`
const fs = require("node:fs/promises");
const { constants } = require("node:fs");
const path = require("node:path");
const { createHash } = require("node:crypto");
function check(ok, message) { if (!ok) throw new Error(message); }
function identity(s) {
  return { dev: String(s.dev), ino: String(s.ino), uid: String(s.uid),
    gid: String(s.gid), mode: (Number(s.mode) & 0o7777).toString(8),
    mtimeNs: String(s.mtimeNs), size: String(s.size) };
}
async function readBounded(file, cap) {
  const handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const stat = await handle.stat({ bigint: true });
    check(stat.isFile(), "HOME special file");
    check(stat.size <= BigInt(cap), "HOME byte limit");
    const bytes = Buffer.alloc(cap + 1);
    let used = 0;
    // At most cap+1 bytes, chunks <=64KiB; a failure ends this serial snapshot.
    for (;;) {
      const { bytesRead } = await handle.read(bytes, used, Math.min(65536, bytes.length - used), null);
      if (!bytesRead) return { bytes: bytes.subarray(0, used), stat };
      used += bytesRead;
      check(used <= cap, "HOME byte limit");
    }
  } finally { await handle.close(); }
}
`;

export const boundaryProbe = nodeFS + String.raw`
(async () => {
  const [sentinel, mode, scope] = process.argv.slice(1);
  check(mode === "off" || mode === "required", "invalid boundary mode");
  check(scope === undefined || scope === "sentinel", "invalid boundary scope");
  check(/^\/nix\/uxlab-sentinel-[a-f0-9-]+$/.test(sentinel), "invalid sentinel path");
  const files = scope === "sentinel" ? [sentinel] : ["/nix/var/nix/db/db.sqlite", "/nix/var/nix/db/big-lock", sentinel];
  for (const file of files) {
    const stat = await fs.lstat(file, { bigint: true });
    check(stat.isFile(), "boundary requires existing regular file");
    console.log(JSON.stringify({ kind: "boundary-identity", file, ...identity(stat) }));
    let handle;
    try { handle = await fs.open(file, constants.O_RDWR); }
    catch (error) {
      check(error.code === "EACCES" || error.code === "EPERM", "unexpected open error: " + error.code);
      check(file !== sentinel || mode === "required", "sentinel calibration denied");
      console.log("OPEN_DENIED=" + file);
      continue;
    }
    // Immediately close every unexpected open. Real DB/lock never receive writes.
    if (file !== sentinel || mode !== "off") {
      await handle.close();
      throw new Error("unexpected writable open: " + file);
    }
    try {
      const { bytesWritten } = await handle.write(Buffer.from("x"), 0, 1, 0);
      check(bytesWritten === 1, "sentinel calibration short write");
      console.log("SENTINEL_WRITE_CALIBRATED=" + file);
    } finally { await handle.close(); }
  }
})().catch(error => { console.error(String(error)); process.exitCode = 1; });
`;

export const homeSnapshot = nodeFS + String.raw`
(async () => {
  const root = process.env.HOME, out = [];
  let entries = 0, total = 0;
  async function walk(dir, depth) {
    check(depth <= 64, "HOME depth limit");
    const stat = await fs.lstat(dir, { bigint: true });
    check(stat.isDirectory(), "HOME directory required");
    out.push([path.relative(root, dir), "dir", identity(stat)]);
    const names = [], handle = await fs.opendir(dir);
    try {
      // Count each entry BEFORE allocation; never materialize an unbounded list.
      for (;;) {
        const entry = await handle.read();
        if (!entry) break;
        check(++entries <= 8192, "HOME entry limit");
        names.push(entry.name);
      }
    } finally { await handle.close(); }
    for (const name of names.sort()) {
      const file = path.join(dir, name), s = await fs.lstat(file, { bigint: true });
      const relative = path.relative(root, file);
      if (s.isDirectory()) await walk(file, depth + 1);
      else if (s.isSymbolicLink()) {
        const link = await fs.readlink(file);
        check(Buffer.byteLength(link) <= 4096, "HOME symlink limit");
        out.push([relative, "link", identity(s), link]);
      } else {
        check(s.isFile(), "HOME special file");
        const { bytes, stat } = await readBounded(file, Math.min(4194304, 16777216 - total));
        total += bytes.length;
        out.push([relative, "file", identity(stat), createHash("sha256").update(bytes).digest("hex")]);
      }
    }
  }
  await walk(root, 0);
  console.log(JSON.stringify(out));
})().catch(error => { console.error(String(error)); process.exitCode = 1; });
`;
