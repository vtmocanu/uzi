// Issue #1598 M4: the worker entrypoint prepares the Codex command-cache root
// (/var/cache/uzi-codex-cmd) on the ROOT-started (uid-split) path: refuse a symlink, create it
// when missing, reclaim -> chmod 0700 -> hand over to runner-cmd (10003:10003), never recursive
// and never deleting contents. The non-root (#58) start must not touch it.
//
// Same seam as entrypoint-migration.test.ts: RUN the real `agent/templates/entrypoint.sh` with
// its absolute-binary constants string-replaced by recording stubs and every path constant
// pointed into a per-test sandbox. The stubs record every chown/chmod/mkdir/rm to an op-log and
// then apply it best-effort, so the real mkdir + chmod land in the sandbox while a give-away
// chown to another uid (EPERM when not root) is only recorded.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const entrypointPath = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../templates/entrypoint.sh",
);
const entrypointText = fs.readFileSync(entrypointPath, "utf8");

const CACHE_CONST = "CODEX_CMD_CACHE_DIR=/var/cache/uzi-codex-cmd";

interface Harness {
  root: string;
  cache: string;
  opLog: string;
  script: string;
}

function writeStub(dir: string, name: string, body: string): void {
  fs.writeFileSync(path.join(dir, name), body, { mode: 0o755 });
}

/** A recording stub: log `<name> <argv>` then apply the real binary best-effort. */
function recordingStub(real: string, name: string): string {
  return `#!/bin/sh\nprintf "${name} %s\\n" "$*" >> "$OPLOG"\n${real} "$@" 2>/dev/null || true\nexit 0\n`;
}

function makeHarness(uid: string): Harness {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-entrypoint-codex-cache-"));
  const stubDir = path.join(root, "stubs");
  const opLog = path.join(root, "ops.log");
  const cache = path.join(root, "var-cache", "uzi-codex-cmd");
  fs.mkdirSync(stubDir);
  fs.mkdirSync(path.join(root, "data"));
  fs.mkdirSync(path.join(root, "nix"));
  fs.mkdirSync(path.join(root, "ambient-tmp"));
  fs.writeFileSync(opLog, "");

  writeStub(stubDir, "id", `#!/bin/sh\necho ${uid}\n`);
  writeStub(stubDir, "tini", "#!/bin/sh\nenv\n");
  writeStub(
    stubDir,
    "setpriv",
    '#!/bin/sh\nwhile [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done\n[ "$1" = "--" ] && shift\nexec "$@"\n',
  );
  writeStub(stubDir, "chown", recordingStub("/bin/chown", "chown"));
  writeStub(stubDir, "chmod", recordingStub("/bin/chmod", "chmod"));
  writeStub(stubDir, "mkdir", recordingStub("/bin/mkdir", "mkdir"));
  writeStub(stubDir, "rm", recordingStub("/bin/rm", "rm"));
  writeStub(stubDir, "busybox", "#!/bin/sh\nexit 1\n");

  const stub = (n: string) => path.join(stubDir, n);
  const patched = entrypointText
    .replace("ID=/usr/bin/id", `ID=${stub("id")}`)
    .replace("TINI=/sbin/tini", `TINI=${stub("tini")}`)
    .replace("SETPRIV=/bin/setpriv", `SETPRIV=${stub("setpriv")}`)
    .replace("CHOWN=/bin/chown", `CHOWN=${stub("chown")}`)
    .replace("CHMOD=/bin/chmod", `CHMOD=${stub("chmod")}`)
    .replace("MKDIR=/bin/mkdir", `MKDIR=${stub("mkdir")}`)
    .replace("RM=/bin/rm", `RM=${stub("rm")}`)
    .replace("BUSYBOX=/bin/busybox", `BUSYBOX=${stub("busybox")}`)
    .replace("DATA_DIR=/data", `DATA_DIR=${path.join(root, "data")}`)
    .replace("NIX_DIR=/nix", `NIX_DIR=${path.join(root, "nix")}`)
    // The join-token constant, matched by shape so no credential path literal is needed here;
    // pointed at an absent sandbox file so the token block is skipped.
    .replace(/^TOKEN=.*$/m, `TOKEN=${path.join(root, "no-token")}`)
    .replace(CACHE_CONST, `CODEX_CMD_CACHE_DIR=${cache}`);
  for (const marker of [stub("id"), stub("chown"), path.join(root, "data"), path.join(root, "no-token"), cache]) {
    assert.ok(patched.includes(marker), `entrypoint patch did not apply for ${marker}`);
  }
  const script = path.join(root, "entrypoint.sh");
  fs.writeFileSync(script, patched, { mode: 0o755 });
  return { root, cache, opLog, script };
}

function run(h: Harness) {
  const r = spawnSync("/bin/sh", [h.script, "npm", "run", "start"], {
    encoding: "utf8",
    env: {
      PATH: process.env.PATH ?? "/usr/bin:/bin",
      HOME: h.root,
      OPLOG: h.opLog,
      // An ambient TMPDIR keeps the (a3) per-uid tmpdirs inside the sandbox, never /tmp/uzi-*.
      TMPDIR: path.join(h.root, "ambient-tmp"),
    },
  });
  const ops = fs.readFileSync(h.opLog, "utf8").split("\n").filter((l) => l.trim() !== "");
  return { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "", ops };
}

/** Ops whose argv names exactly `p` (the cache root) or anything beneath it. */
function opsOn(ops: string[], p: string): string[] {
  return ops.filter((op) => op.split(" ").some((a) => a === p || a.startsWith(p + "/")));
}

describe("issue #1598: entrypoint prepares the Codex command-cache root", () => {
  it("root start: creates a missing root 0700 and hands it to runner-cmd 10003:10003, reclaim first", () => {
    const h = makeHarness("0");
    try {
      const r = run(h);
      assert.equal(r.status, 0, `entrypoint must succeed (stderr: ${r.stderr})`);
      const st = fs.lstatSync(h.cache);
      assert.ok(st.isDirectory() && !st.isSymbolicLink(), "the cache root must be a real directory");
      assert.equal(st.mode & 0o7777, 0o700, "the cache root must be mode 0700");
      assert.deepEqual(
        opsOn(r.ops, h.cache),
        [
          `mkdir -p ${h.cache}`,
          `chown -h 0:0 ${h.cache}`,
          `chmod 0700 ${h.cache}`,
          `chown -h 10003:10003 ${h.cache}`,
        ],
        "exactly mkdir -> reclaim -> chmod 0700 -> hand over to 10003, nothing else on the cache root",
      );
      if (process.getuid?.() === 0) {
        assert.equal(st.uid, 10003, "owner uid must be runner-cmd (10003)");
        assert.equal(st.gid, 10003, "owner gid must be runner-cmd (10003)");
      }
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  it("root start: an existing root keeps its per-run dirs (not recursive, nothing deleted)", () => {
    const h = makeHarness("0");
    try {
      const perRun = path.join(h.cache, "run-abc123");
      fs.mkdirSync(perRun, { recursive: true, mode: 0o755 });
      fs.writeFileSync(path.join(perRun, "go.sum"), "cached");
      const r = run(h);
      assert.equal(r.status, 0, `entrypoint must succeed (stderr: ${r.stderr})`);
      assert.equal(fs.readFileSync(path.join(perRun, "go.sum"), "utf8"), "cached", "per-run content must survive");
      assert.equal(fs.statSync(perRun).mode & 0o7777, 0o755, "a per-run dir's mode must be untouched");
      const onCache = opsOn(r.ops, h.cache);
      assert.equal(onCache.filter((op) => op.includes(perRun)).length, 0, "no op may name a per-run dir");
      assert.equal(onCache.filter((op) => / -R /.test(op) || op.startsWith("rm ")).length, 0, "no recursive or rm op");
      assert.equal(fs.statSync(h.cache).mode & 0o7777, 0o700, "the root is still re-asserted 0700");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });

  for (const shape of ["a link to a directory", "a dangling link"] as const) {
    it(`root start: ${shape} at the cache path is refused and nothing is chowned through it`, () => {
      const h = makeHarness("0");
      try {
        const victim = path.join(h.root, "victim");
        fs.mkdirSync(path.dirname(h.cache), { recursive: true });
        if (shape === "a link to a directory") fs.mkdirSync(victim, { mode: 0o755 });
        fs.symlinkSync(victim, h.cache);
        const r = run(h);
        assert.notEqual(r.status, 0, "a symlinked cache root must fail closed");
        assert.match(r.stderr, /refusing to start: .*uzi-codex-cmd is a symlink/, "must log the refusal");
        assert.deepEqual(opsOn(r.ops, h.cache), [], "no mkdir/chown/chmod may touch the symlinked path");
        assert.deepEqual(opsOn(r.ops, victim), [], "nothing may reach the link target");
        assert.ok(fs.lstatSync(h.cache).isSymbolicLink(), "the link itself is left in place");
        assert.equal(r.stdout.includes("UZI_UID_SPLIT=1"), false, "the drop must never be reached");
        if (shape === "a link to a directory") {
          assert.equal(fs.statSync(victim).mode & 0o7777, 0o755, "the link target's mode is untouched");
        }
      } finally {
        fs.rmSync(h.root, { recursive: true, force: true });
      }
    });
  }

  it("non-root (#58) start: the cache root is never touched", () => {
    const h = makeHarness("10001");
    try {
      const r = run(h);
      assert.equal(r.status, 0, `entrypoint must succeed (stderr: ${r.stderr})`);
      assert.deepEqual(r.ops, [], "the non-root branch issues no chown/chmod/mkdir at all");
      assert.equal(fs.existsSync(h.cache), false, "the cache root is not created");
    } finally {
      fs.rmSync(h.root, { recursive: true, force: true });
    }
  });
});
