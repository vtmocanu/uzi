import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import {
  assertSnapshotTraversable, createReplayRunDir, loadReplayCorpus, runReplays, type ReplayRecord,
} from "../src/code-cross-check-replay.js";
import { createReplayRecordsFile } from "../src/code-cross-check-replay-cli.js";
import { CodexCrossCheck } from "../src/codex/cross-check.js";
import { wireFileopHelper } from "../src/codex/fileop-client.js";
import { SUPERVISOR_BIN } from "../src/codex/launcher.js";
import type { FileopResponse } from "../src/codex/broker.js";
import type { Logger } from "../src/log.js";
import { rmHomeTree } from "../src/rmtree.js";
import { RUNNER_UID, WORKER_UID, commandRootCommand, runnerCommand } from "../src/runner-uid.js";
import { buildTempCorpus } from "./code-cross-check-replay-fixture.js";

// Offline replay of the Codex snapshot traversal contract under the real worker/runner/runner-cmd
// uids. No provider is launched and no model is called: the checker's releaseCodex throws right after
// the real fileop helper has answered, so nothing past the fileop readiness probe ever runs.
// Required inventory must reject this skip: only the production worker profile supplies the evidence.
const HELPERS = [SUPERVISOR_BIN, "/usr/local/bin/uzi-codex-command-sandbox", "/usr/local/bin/uzi-codex-fileop"];
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== WORKER_UID || process.env.UZI_UID_SPLIT !== "1" ||
  !HELPERS.every((helper) => fs.existsSync(helper))
  ? "requires Linux worker UID 10001 with the production runner/command split and the codex helpers"
  : false;

const CLIENT_TS = "// feature: batching\n// client\nexport function fetchAll(items: string[]): string[] {\n  const limit = 10;\n  return items.slice(0, limit);\n}\n";

const silent: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return silent; },
};

/** Run a node script as another uid and return the JSON it prints; the minimal env carries no worker secret. */
function probe(wrap: typeof runnerCommand, script: string, ...args: string[]): unknown {
  const command = wrap(process.execPath, ["-e", script, ...args]);
  const result = spawnSync(command.command, command.args, { env: { PATH: "/usr/bin:/bin" }, timeout: 15_000, encoding: "utf8" });
  if (result.status !== 0) throw new Error(`probe failed: ${result.stderr || result.error?.message}`);
  return JSON.parse(result.stdout);
}

const DAC_SCRIPT = `
const fs = require("node:fs");
const [records, runDir, work, snapFile, snapDir] = process.argv.slice(1);
const code = (f) => { try { f(); return "ok"; } catch (e) { return e.code; } };
let bytes = null;
try { bytes = fs.readFileSync(snapFile).toString("base64"); } catch {}
process.stdout.write(JSON.stringify({
  uid: process.getuid(),
  records: code(() => fs.readFileSync(records)),
  runDirList: code(() => fs.readdirSync(runDir)),
  workList: code(() => fs.readdirSync(work)),
  snapList: code(() => fs.readdirSync(snapDir)),
  snapWrite: code(() => fs.writeFileSync(snapDir + "/planted", "x")),
  bytes,
}));
`;

test("Codex fileop can stat and read the replay snapshot while the records file, run directory and work repository stay private", { skip: INIT_SKIP }, async () => {
  // The lane mounts private worker scratch. Open it to group runner for search only (as the retained
  // permissions lane does) and restore its metadata afterwards.
  const scratchPath = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
  fs.mkdirSync(scratchPath, { recursive: true });
  const scratch = fs.realpathSync(scratchPath);
  const scratchBefore = fs.statSync(scratch);
  fs.chownSync(scratch, -1, RUNNER_UID);
  fs.chmodSync(scratch, 0o710);
  const fixtureRoot = fs.mkdtempSync(path.join(scratch, "replay-split-"));
  const tc = await buildTempCorpus();
  let runDir: string | undefined;
  try {
    fs.chownSync(fixtureRoot, -1, RUNNER_UID);
    fs.chmodSync(fixtureRoot, 0o710);
    // Created by the production helper the CLI uses (wx, 0600), not a hand-written stand-in.
    const created = await createReplayRecordsFile(fixtureRoot, Date.now());
    await created.handle.writeFile("private\n");
    await created.handle.close();
    const recordsFile = created.file;

    runDir = await createReplayRunDir(fixtureRoot);
    const runStat = fs.statSync(runDir);
    assert.equal(runStat.mode & 0o7777, 0o2710);
    assert.equal(runStat.gid, RUNNER_UID);
    await assertSnapshotTraversable(runDir);

    // A worker-private (0700) ancestor is refused before anything runs.
    const hidden = fs.mkdtempSync(path.join(fixtureRoot, "hidden-"));
    fs.chmodSync(hidden, 0o700);
    fs.mkdirSync(path.join(hidden, "run"));
    await assert.rejects(assertSnapshotTraversable(path.join(hidden, "run")), { message: "snapshot_untraversable" });

    const corpus = await loadReplayCorpus(tc.checkout, { profile: "revised", manifestSha256: tc.manifestSha256 });
    const defect = corpus.cases.find((c) => c.id === tc.defectId)!;
    const seen: {
      stat?: FileopResponse; read?: FileopResponse; dac?: unknown; runner?: unknown; names?: string[]; failure?: string;
    } = {};
    const rejectRelease = async (): Promise<never> => { throw new Error("offline-uid-split-release-refused"); };
    const codex = new CodexCrossCheck({ releaseCodex: rejectRelease, refreshCodex: rejectRelease }, silent, {
      wireFileop: (proc) => {
        // The real helper is launched as the command uid and about to be probed; look at the replay's
        // own directories now, while the snapshot, the work repository and the HOME exist.
        const handle = wireFileopHelper(proc);
        return {
          dispose: () => handle.dispose(),
          client: {
            op: async (request) => {
              const response = await handle.client.op(request);
              if (request.op === "stat" && request.path === "." && seen.stat === undefined) {
                seen.stat = response;
                seen.read = await handle.client.op({ op: "read", path: "src/client.ts" });
                try {
                  const names = fs.readdirSync(runDir!);
                  seen.names = names;
                  const snap = path.join(runDir!, names.find((n) => n.startsWith("replay-snap-"))!);
                  const work = path.join(runDir!, names.find((n) => n.startsWith("replay-work-"))!);
                  const snapFile = path.join(snap, "src/client.ts");
                  seen.dac = probe(commandRootCommand, DAC_SCRIPT, recordsFile, runDir!, work, snapFile, snap);
                  seen.runner = probe(runnerCommand, DAC_SCRIPT, recordsFile, runDir!, work, snapFile, snap);
                } catch (error) { seen.failure = error instanceof Error ? error.message : "probe error"; }
              }
              return response;
            },
          },
        };
      },
    });
    const out: ReplayRecord[] = [];
    await runReplays({
      checkout: tc.checkout, corpus: { ...corpus, cases: [defect] }, families: ["codex"], model: "offline-model", effort: "low",
      repeats: 1, scratchDir: runDir, env: { OPENAI_API_KEY: ["offline-", "uid-split-no-authority"].join("") },
      checkers: { codex }, checkerVersion: { uzi_head: "a".repeat(40), agent_version: "0.0.0-test" },
      writeRecord: async (record) => { out.push(record); },
    });

    assert.equal(seen.failure, undefined);
    assert.equal(seen.stat?.ok, true, "the command-uid fileop helper stats the snapshot root");
    assert.equal(seen.read?.ok, true, "the command-uid fileop helper reads a snapshot file");
    assert.equal(Buffer.from(seen.read?.data ?? "", "base64").toString("utf8"), CLIENT_TS);
    // The replay got past real home preparation and a real helper launch, then stopped at the offline release.
    assert.equal(out[0]?.outcome, "model_error");
    assert.ok(seen.names?.some((n) => n.startsWith("replay-home-")));

    const dac = seen.dac as Record<string, unknown>;
    assert.equal(dac.uid, 10003);
    assert.equal(Buffer.from(String(dac.bytes), "base64").toString("utf8"), CLIENT_TS, "DAC lets the command uid read the snapshot file");
    assert.deepEqual([dac.records, dac.runDirList, dac.workList, dac.snapList, dac.snapWrite],
      ["EACCES", "EACCES", "EACCES", "ok", "EACCES"]);
    const runner = seen.runner as Record<string, unknown>;
    assert.equal(runner.uid, RUNNER_UID);
    assert.equal(Buffer.from(String(runner.bytes), "base64").toString("utf8"), CLIENT_TS, "the provider uid reads the same file");
    assert.deepEqual([runner.records, runner.runDirList, runner.workList, runner.snapWrite],
      ["EACCES", "EACCES", "EACCES", "EACCES"]);

    // Everything the replay created is gone, and the private records file was never touched.
    assert.deepEqual(fs.readdirSync(runDir), []);
    assert.equal(fs.readFileSync(recordsFile, "utf8"), "private\n");
  } finally {
    if (runDir) await rmHomeTree(runDir).catch(() => undefined);
    await rmHomeTree(fixtureRoot).catch(() => undefined);
    await tc.cleanup();
    fs.chmodSync(scratch, scratchBefore.mode & 0o7777);
    fs.chownSync(scratch, -1, scratchBefore.gid);
  }
});
