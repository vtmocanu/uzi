// The harness owns this otherwise-unused, per-container tmpfs directory.
// Parallel Node test children share the same repository-relative scratch link.
import { readlinkSync, symlinkSync } from "node:fs";
const link = "/app/uzi-worker-uid-proof/.uzi";
const target = "/proof/.uzi";
try {
  symlinkSync(target, link);
} catch (error) {
  if (error.code !== "EEXIST" || readlinkSync(link) !== target) throw error;
}
