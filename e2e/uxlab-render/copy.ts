// Invoked only by the fixture's runner wrapper. No shell interpolation of input paths.
import fs from "node:fs/promises";
import path from "node:path";
import { readRegularFile } from "./inputs.js";
import { inventory } from "./tree.js";
async function main() {
  const [source, target, overlay] = process.argv.slice(2);
  if (!source || !target || !/^\/data\/runner\/uxlab-[a-f0-9-]+$/.test(target)) throw new Error("invalid copy destination");
  process.umask(0o002);
  await fs.mkdir(target, { mode: 0o2770 });
  console.log(JSON.stringify({ kind: "copy", files: Object.keys(await inventory(source, target)).length }));
  if (overlay) await fs.writeFile(path.join(target, "api/cmd/uzi/uxlab/build.sh"),
    await readRegularFile(overlay, 1024 * 1024), { mode: 0o770, flag: "wx" });
}
main().catch((error) => { console.error(String(error)); process.exitCode = 1; });
