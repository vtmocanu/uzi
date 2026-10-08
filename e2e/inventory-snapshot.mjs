// Read-only failure diagnostic. Project explicit metadata, never authenticators,
// credentials, complete terminal bodies, or bundle contents. Roots are overridable
// for the hermetic fixture; the worker stdin invocation uses the fixed defaults.
import fs from "node:fs/promises";
import path from "node:path";
const pick = (object, fields) => Object.fromEntries(fields.filter(key => object?.[key] !== undefined).map(key => [key, object[key]]));
const emit = value => console.log(JSON.stringify({ observed_at: new Date().toISOString(), ...value }));
for (const [kind, root] of [["recovery", process.argv[2] ?? "/data/recovery"], ["terminal", process.argv[3] ?? "/data/outbox"]]) {
  let dirs;
  try { dirs = await fs.readdir(root, { withFileTypes: true }); }
  catch (err) { emit({ kind, error: err.code ?? "read_error" }); continue; }
  for (const dir of dirs) {
    if (!dir.isDirectory() || !/^[0-9a-f-]{36}$/.test(dir.name)) continue;
    const folder = path.join(root, dir.name);
    for (const file of await fs.readdir(folder, { withFileTypes: true })) {
      if (!file.isFile() || !file.name.endsWith(".json") || (kind === "terminal" && !/^terminal-\d+\.json$/.test(file.name))) continue;
      try {
        const record = JSON.parse(await fs.readFile(path.join(folder, file.name), "utf8"));
        if (kind === "terminal") {
          emit({ kind, directory_run_id: dir.name, file: file.name,
            ...pick(record, ["run_id", "claim_generation", "state", "blocked", "blocked_reason", "phase_at_journal", "messages_through_seq"]),
            outcome: pick(record.body, ["status", "fail_origin"]) });
        } else {
          emit({ kind, directory_run_id: dir.name, file: file.name,
            ...pick(record, ["runId", "generation", "captureId", "serverCaptureId", "sourceSha", "originalSourceSha", "inventoryCurrentSha", "coverageDigest", "state", "reason", "inventoryGuarded", "finalAcknowledged", "checksum", "byteSize"]),
            originalRootShas: record.originalRoots?.map(root => root.sha),
            finalRequest: record.finalRequest && {
              evidence: record.finalRequest.evidence,
              disposition: pick(record.finalRequest.disposition, ["kind", "capture_id", "source_sha", "coverage_digest"]),
            } });
        }
      } catch (err) { emit({ kind, directory_run_id: dir.name, file: file.name, error: err.code ?? "parse_error" }); }
    }
  }
}
