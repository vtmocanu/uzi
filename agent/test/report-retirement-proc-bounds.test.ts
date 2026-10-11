import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { reapProcesses, type ScanRequest } from "../src/run-quiescence.js";
import { makeFakeProcRoot, plantFakeProc, withQuiescenceView } from "./fake-proc.js";

const uid = process.getuid?.() ?? 0;
const req: ScanRequest = { mode: "capture", targetUid: uid, targetKey: "fixture",
  targetPaths: ["/fixture/clone"], liveMarkers: [], liveRoots: [], workerNonce: "fixture" };

for (const fault of ["pid overflow", "status overflow", "stat overflow", "env overflow",
  "cumulative bytes", "incomplete enumeration", "unreadable EOF", "short reads"] as const) {
  it(`report process proof: ${fault} ${fault === "short reads" ? "reads complete EOF" : "refuses"}`, async t => {
    const root = makeFakeProcRoot("report-proc-");
    t.after(() => fs.rmSync(root, { recursive: true, force: true }));
    plantFakeProc(root, 4242, { uid, cwd: "/" });
    if (fault === "pid overflow") {
      for (let i = 10000; i < 14096; i++) fs.mkdirSync(path.join(root, String(i)));
    }
    if (fault === "status overflow") fs.appendFileSync(path.join(root, "4242/status"), "x".repeat(65537));
    if (fault === "stat overflow") fs.appendFileSync(path.join(root, "4242/stat"), "x".repeat(65537));
    if (fault === "env overflow") fs.writeFileSync(path.join(root, "4242/environ"), "x".repeat(262145));
    if (fault === "cumulative bytes") {
      for (let i = 5000; i < 5130; i++) {
        plantFakeProc(root, i, { uid, cwd: "/" });
        fs.writeFileSync(path.join(root, String(i), "environ"), "x".repeat(262144));
      }
    }
    if (fault === "incomplete enumeration") {
      const open = fs.opendirSync.bind(fs);
      t.mock.method(fs, "opendirSync", ((...args: Parameters<typeof open>) => {
        const dir = open(...args);
        const read = dir.readSync.bind(dir);
        let count = 0;
        dir.readSync = () => { if (count++ > 0) throw new Error("fixture listing failure"); return read(); };
        return dir;
      }) as typeof fs.opendirSync);
    }
    if (fault === "short reads" || fault === "unreadable EOF") {
      const read = fs.readSync.bind(fs);
      t.mock.method(fs, "readSync", ((fd: number, buffer: Buffer, offset: number, length: number, position: number | null) => {
        if (fault === "unreadable EOF" && offset > 0) throw new Error("fixture EOF unreadable");
        return read(fd, buffer, offset, Math.min(length, 7), position);
      }) as typeof fs.readSync);
    }
    const result = await withQuiescenceView({ procRoot: root }, () => reapProcesses(req, {
      reportBudget: { signal: new AbortController().signal, deadline: Date.now() + 5000 },
      kill: () => assert.fail("observational proof must never signal"),
    }));
    assert.equal(result.state, fault === "short reads" ? "quiescent" : "unverified", result.detail);
    assert.deepEqual(result.killed, []);
  });
}
