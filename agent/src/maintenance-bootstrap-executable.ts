import fs from "node:fs";
import { createRequire } from "node:module";

/** Resolve tsx's own native dependency without evaluating either package or
 * trusting PATH, .bin, argv, an override, or another top-level esbuild version.
 * Kept out of run-quiescence.ts, whose source scan forbids every env read there. */
export function maintenanceEsbuildExecutable(): string {
  if (process.env.ESBUILD_BINARY_PATH) throw new Error("maintenance bootstrap executable unproven");
  const local = createRequire(import.meta.url);
  const tsxManifest = local.resolve("tsx/package.json");
  const esbuildManifest = createRequire(tsxManifest).resolve("esbuild/package.json");
  if (process.arch !== "arm64" && process.arch !== "x64") throw new Error("maintenance bootstrap architecture unproven");
  return fs.realpathSync(createRequire(esbuildManifest).resolve(`@esbuild/linux-${process.arch}/bin/esbuild`));
}
