import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { makeFixture, type Fixture } from "./fixture-repo.js";

const SELF = fileURLToPath(import.meta.url);
const GUARD = path.resolve(path.dirname(SELF), "../../scripts/tmpdir-leak-guard.sh");
const AGENT_DIR = path.resolve(path.dirname(SELF), "..");

// The guard's end-to-end child (same-file pattern as checkpoint-publish-early-response.test.ts):
// create a fixture, clean it up, then recreate a directory under it, as the leak in #2020 did.
function child(): void {
  const fx = makeFixture({}, { testName: "recreator" });
  fx.cleanup();
  fs.mkdirSync(path.join(path.dirname(fx.dataDir), "data", "x"), { recursive: true });
}

function readLedger(file: string): Array<Record<string, unknown>> {
  return fs
    .readFileSync(file, "utf8")
    .split("\n")
    .filter((l) => l !== "")
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

if (process.env.UZI_FIXTURE_REPO_CHILD === "1") {
  child();
} else {
  describe("makeFixture ledger (scripts/tmpdir-leak-guard.sh evidence)", () => {
    it("records a created line, then a removed line on cleanup", () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-ledger-"));
      const ledger = path.join(dir, "ledger");
      const saved = process.env.UZI_TMPDIR_GUARD_LEDGER;
      process.env.UZI_TMPDIR_GUARD_LEDGER = ledger;
      let fx: Fixture | undefined;
      try {
        fx = makeFixture({}, { testName: "x" });
        const base = path.dirname(fx.dataDir);
        let lines = readLedger(ledger);
        assert.equal(lines.length, 1);
        const created = lines[0] as Record<string, unknown>;
        assert.equal(created.event, "created");
        assert.equal(created.entry, path.basename(base));
        assert.equal(created.path, base);
        assert.equal(created.test, "x");
        assert.equal(created.pid, process.pid);
        assert.match(String(created.file), /fixture-repo\.test\.ts$/);
        assert.ok(Array.isArray(created.site));
        assert.ok((created.site as string[]).some((s) => s.includes("fixture-repo.test.ts:")));

        fx.cleanup();
        assert.equal(fs.existsSync(base), false);
        lines = readLedger(ledger);
        assert.equal(lines.length, 2);
        assert.equal((lines[1] as Record<string, unknown>).event, "removed");
        assert.equal((lines[1] as Record<string, unknown>).entry, path.basename(base));
      } finally {
        // A failed assertion above must not leak the fixture into the real guard's TMPDIR,
        // where its created line (in this private ledger) would be gone.
        if (saved === undefined) delete process.env.UZI_TMPDIR_GUARD_LEDGER;
        else process.env.UZI_TMPDIR_GUARD_LEDGER = saved;
        try {
          if (fx) fs.rmSync(path.dirname(fx.dataDir), { recursive: true, force: true });
        } finally {
          fs.rmSync(dir, { recursive: true, force: true });
        }
      }
    });

    it("names the creator of a directory recreated after cleanup, under the real guard", () => {
      const outer = fs.mkdtempSync(path.join(os.tmpdir(), "fixture-repo-guard-"));
      try {
        const childEnv: NodeJS.ProcessEnv = { ...process.env, TMPDIR: outer, UZI_FIXTURE_REPO_CHILD: "1" };
        delete childEnv.UZI_TMPDIR_GUARD_LEDGER; // the guard sets its own
        const r = spawnSync(GUARD, [process.execPath, "--import", "tsx", SELF], {
          cwd: AGENT_DIR,
          env: childEnv,
          encoding: "utf8",
          timeout: 60_000,
        });
        assert.equal(r.status, 1, r.stderr);
        assert.ok(r.stderr.includes("fixture-repo.test.ts"), r.stderr);
        assert.ok(r.stderr.includes("recreator"), r.stderr);
        assert.ok(r.stderr.includes("cleanup ran; the directory was recreated afterwards"), r.stderr);
        assert.ok(r.stderr.includes("data/x"), r.stderr);
      } finally {
        fs.rmSync(outer, { recursive: true, force: true });
      }
    });
  });
}
