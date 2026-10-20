import assert from "node:assert/strict";
import fs from "node:fs";
import { execFileSync } from "node:child_process";
import path from "node:path";
import vm from "node:vm";
import { fileURLToPath } from "node:url";
import { PassThrough } from "node:stream";
import { test, describe, after } from "node:test";
import { GitCache, type RecoveryJournalEntry } from "../src/git.js";
import { CodexLaunchError, SUPERVISOR_BIN, type CodexRootHandle, type CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import { recoveryProgress, type RecoverySource } from "../src/recovery-progress.js";
import type { Logger } from "../src/log.js";

const worktree = fileURLToPath(new URL("../../", import.meta.url));
const scratch = path.join(worktree, ".uzi/scratch");
fs.mkdirSync(scratch, { recursive: true });
const source = fs.readFileSync(path.join(worktree, "agent/src/git.ts"), "utf8");
const helper = source.split("const RETAINED_READINESS_HELPER = String.raw\x60")[1]!.split("\x60;")[0]!;
const logger = { info() {}, warn() {}, error() {}, debug() {} } as unknown as Logger;

function fixture() {
  const dir = fs.mkdtempSync(path.join(scratch, "readiness-"));
  const clone = path.join(dir, "clone"), bare = path.join(dir, "bare");
  const objects = path.join(clone, ".git/objects");
  fs.mkdirSync(objects, { recursive: true });
  fs.mkdirSync(path.join(bare, "objects/info"), { recursive: true });
  for (const p of [clone, path.join(clone, ".git"), objects, bare, path.join(bare, "objects"), path.join(bare, "objects/info")]) fs.chmodSync(p, 0o750);
  const roots = [clone, path.join(clone, ".git"), objects, bare, path.join(bare, "objects")];
  const identities = roots.map(p => { const s = fs.statSync(p); return s.dev + ":" + s.ino; });
  const metadata = new Map<number, { uid?: number; gid?: number; mode?: number; nlink?: number }>();
  const set = (p: string, values: { uid?: number; gid?: number; mode?: number; nlink?: number }) => {
    metadata.set(fs.lstatSync(p).ino, values);
  };
  for (const p of roots.slice(0, 2)) set(p, { uid: 10002 });
  for (const p of roots.slice(3)) set(p, { uid: 10001, gid: 10001 });
  const chmods: Array<{ ino: number; mode: number }> = [];
  let onStat: ((fd: number, s: fs.Stats) => void) | undefined;
  let onChmod: ((fd: number) => void) | undefined;
  const adapter = {
    ...fs,
    fstatSync(fd: number) {
      const s = fs.fstatSync(fd);
      Object.assign(s, { uid: 10003, gid: 10002 }, metadata.get(s.ino));
      onStat?.(fd, s);
      return s;
    },
    fchmodSync(fd: number, mode: number) {
      chmods.push({ ino: fs.fstatSync(fd).ino, mode });
      fs.fchmodSync(fd, mode);
      onChmod?.(fd);
    },
  };
  function run(options: { entries?: number; depth?: number; pathBytes?: number; now?: () => number; fs?: typeof adapter } = {}) {
    let stdout = "";
    // Scoped VM-only substitution keeps production bounds literal and immutable.
    const program = helper.replace("ENTRY_LIMIT = 2000000", "ENTRY_LIMIT = " + (options.entries ?? 2000000))
      .replace("DEPTH_LIMIT = 64", "DEPTH_LIMIT = " + (options.depth ?? 64))
      .replace("PATH_LIMIT = 4096", "PATH_LIMIT = " + (options.pathBytes ?? 4096));
    vm.runInNewContext(program, {
      require(name: string) { assert.equal(name, "node:fs"); return options.fs ?? adapter; },
      process: { argv: ["node", JSON.stringify({ roots, identities, deadline: 30000 })],
        platform: "linux", getuid: () => 10003, getgroups: () => [10002],
        stdout: { write(s: string) { stdout += s; } } },
      Buffer, Date: { now: options.now ?? (() => 0) },
    }, { timeout: 5000 });
    return stdout;
  }
  function file(name: string, mode = 0o400) {
    const p = path.join(objects, name);
    fs.mkdirSync(path.dirname(p), { recursive: true, mode: 0o750 });
    fs.writeFileSync(p, "object bytes\n", { mode });
    return p;
  }
  return { dir, clone, bare, objects, roots, set, file, run, chmods, adapter,
    statHook(fn: typeof onStat) { onStat = fn; }, chmodHook(fn: typeof onChmod) { onChmod = fn; },
    cleanup() { fs.rmSync(dir, { recursive: true, force: true }); } };
}

describe("retained readiness helper VM tests", { skip: process.platform !== "linux" }, () => {
test("helper adds only group read/search, is idempotent, and preserves source and nonobjects", () => {
  const f = fixture();
  try {
    fs.chmodSync(f.objects, 0o2700);
    const object = f.file("aa/object");
    fs.chmodSync(path.dirname(object), 0o1700);
    const head = path.join(f.clone, ".git/HEAD");
    fs.writeFileSync(head, "ref: refs/heads/main\n", { mode: 0o600 });
    const before = fs.readFileSync(object);
    assert.match(f.run(), /^ok /);
    assert.equal(fs.statSync(f.objects).mode & 0o7777, 0o2750);
    assert.equal(fs.statSync(path.dirname(object)).mode & 0o7777, 0o1750);
    assert.equal(fs.statSync(object).mode & 0o7777, 0o440);
    assert.deepEqual(fs.readFileSync(object), before);
    assert.equal(fs.readFileSync(head, "utf8"), "ref: refs/heads/main\n");
    assert.equal(fs.statSync(head).mode & 0o7777, 0o600);
    f.chmods.length = 0;
    assert.match(f.run(), /^ok /);
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

for (const uid of [10001, 10002, 10003]) test("hardlinked readable no-op never calls fchmod: uid " + uid, () => {
  const f = fixture();
  try {
    const p = f.file("shared", 0o440);
    fs.linkSync(p, path.join(f.dir, "witness"));
    f.set(p, { uid });
    assert.match(f.run(), /^ok /);
    assert.deepEqual(f.chmods, []);
    assert.equal(fs.statSync(p).nlink, 2);
  } finally { f.cleanup(); }
});

test("hardlinked command mutation refuses before fchmod", () => {
  const f = fixture();
  try {
    const p = f.file("linked");
    fs.linkSync(p, path.join(f.dir, "witness"));
    assert.equal(f.run(), "refused hardlinked_mutation\n");
    assert.deepEqual(f.chmods, []);
    assert.equal(fs.statSync(p).mode & 0o7777, 0o400);
  } finally { f.cleanup(); }
});

for (const [label, metadata, reason] of [
  ["foreign owner", { uid: 10101 }, "owner"],
  ["foreign group", { gid: 10101 }, "group"],
  ["world write", { mode: 0o100402 }, "world_write"],
  ["setuid", { mode: 0o104400 }, "special_mode"],
  ["setgid regular", { mode: 0o102400 }, "special_mode"],
  ["sticky regular", { mode: 0o101400 }, "special_mode"],
  ["runner inaccessible", { uid: 10002 }, "access"],
] as const) test("helper refuses " + label + " at classification", () => {
  const f = fixture();
  try {
    const p = f.file("object");
    f.set(p, metadata);
    assert.equal(f.run(), "refused " + reason + "\n");
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

test("helper accepts group writable setgid and sticky directories", () => {
  const f = fixture();
  try {
    for (const mode of [0o2770, 0o1770, 0o3770]) {
      fs.chmodSync(f.objects, mode);
      assert.match(f.run(), /^ok /);
      assert.equal(fs.statSync(f.objects).mode & 0o7777, mode);
    }
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

test("symlink object and gitfile are refused without following", () => {
  const f = fixture();
  try {
    const witness = path.join(f.dir, "witness");
    fs.writeFileSync(witness, "private", { mode: 0o600 });
    fs.symlinkSync(witness, path.join(f.objects, "link"));
    assert.equal(f.run(), "refused special_file\n");
    assert.equal(fs.statSync(witness).mode & 0o7777, 0o600);
    fs.unlinkSync(path.join(f.objects, "link"));
    fs.renameSync(path.join(f.clone, ".git"), path.join(f.dir, "old-git"));
    fs.writeFileSync(path.join(f.clone, ".git"), "gitdir: elsewhere\n");
    assert.equal(f.run(), "refused filesystem\n");
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

test("invalid UTF-8 names refuse instead of opening a replacement-decoded path", () => {
  const f = fixture();
  try {
    const p = Buffer.concat([Buffer.from(f.objects + "/"), Buffer.from([255])]);
    fs.writeFileSync(p, "invalid-name object", { mode: 0o400 });
    assert.equal(f.run(), "refused unsafe_root\n");
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

test("special file reaches fstat guard (nonblocking descriptor)", () => {
  const f = fixture();
  try {
    const p = f.file("special", 0o440), ino = fs.statSync(p).ino;
    f.statHook((_fd, s) => { if (s.ino === ino) { s.isFile = () => false; s.isDirectory = () => false; } });
    assert.equal(f.run(), "refused special_file\n");
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

for (const mutation of ["mode600", "mode440", "owner"]) {
  test("classification handoff refuses " + mutation + " before fchmod", () => {
    const f = fixture();
    try {
      const p = f.file("open-drift", mutation === "owner" ? 0o440 : 0o400);
      let reached = false;
      const guarded = { ...f.adapter, openSync(name: fs.PathLike, flags: fs.OpenMode) {
        if (!reached && String(name).endsWith("/open-drift") && typeof flags === "number" && (flags & 0x200000) === 0) {
          reached = true;
          if (mutation === "owner") f.set(p, { uid: 10001 });
          else fs.chmodSync(p, mutation === "mode600" ? 0o600 : 0o440);
        }
        return fs.openSync(name, flags);
      } };
      assert.equal(f.run({ fs: guarded }), "refused classification_drift\n");
      assert.equal(reached, true, "metadata changed at the O_PATH-to-readable handoff");
      assert.deepEqual(f.chmods, [], "classification drift is refused before mutation");
    } finally { f.cleanup(); }
  });
}

test("byte-exact alternate target comparison rejects replacement-decoded bytes", () => {
  const f = fixture();
  try {
    const selected = f.bare + "\uFFFD";
    fs.renameSync(f.bare, selected);
    f.roots[3] = selected;
    f.roots[4] = path.join(selected, "objects");
    const alternate = f.file("info/alternates", 0o640);
    fs.writeFileSync(alternate, Buffer.concat([Buffer.from(f.bare), Buffer.from([255]), Buffer.from("/objects\n")]));
    fs.chmodSync(alternate, 0o440);
    assert.equal(f.run(), "refused alternates\n");
    assert.deepEqual(f.chmods, []);
  } finally { f.cleanup(); }
});

test("pre and post chmod classification drift refuse at the intended guard", () => {
  for (const after of [false, true]) {
    const f = fixture();
    try {
      const p = f.file("drift"), ino = fs.statSync(p).ino;
      let stats = 0, mutated = false;
      f.chmodHook(() => { mutated = true; });
      f.statHook((_fd, s) => {
        if (s.ino === ino && ((!after && ++stats === 5) || (after && mutated))) s.nlink = 2;
      });
      assert.equal(f.run(), "refused classification_drift\n");
      assert.equal(f.chmods.length, after ? 1 : 0);
    } finally { f.cleanup(); }
  }
});

test("displaced object root and swapped absolute parent refuse", () => {
  for (const parent of [false, true]) {
    const f = fixture();
    try {
      if (parent) {
        fs.renameSync(f.clone, f.clone + "-old");
        fs.symlinkSync(f.clone + "-old", f.clone);
      } else {
        fs.renameSync(f.objects, f.objects + "-old");
        fs.mkdirSync(f.objects);
      }
      assert.equal(f.run(), parent ? "refused filesystem\n" : "refused displaced_root\n");
      assert.deepEqual(f.chmods, []);
    } finally { f.cleanup(); }
  }
});

test("parent replacement during traversal cannot redirect chmod and is detected", () => {
  const f = fixture();
  try {
    f.file("object");
    const witness = path.join(f.dir, "witness");
    fs.mkdirSync(witness);
    fs.writeFileSync(path.join(witness, "object"), "unrelated", { mode: 0o400 });
    let swapped = false;
    f.chmodHook(() => {
      if (swapped) return;
      swapped = true;
      fs.renameSync(f.objects, f.objects + "-old");
      fs.symlinkSync(witness, f.objects);
    });
    assert.equal(f.run(), "refused filesystem\n");
    assert.equal(fs.statSync(path.join(witness, "object")).mode & 0o7777, 0o400);
    assert.equal(f.chmods.length, 1);
  } finally { f.cleanup(); }
});

for (const kind of ["allowed", "arbitrary", "relative", "multiple", "http", "symlink", "commondir"]) {
  test("alternates: " + kind, () => {
    const f = fixture();
    try {
      const target = path.join(f.bare, "objects/witness");
      fs.writeFileSync(target, "bare bytes", { mode: 0o600 });
      const alt = f.file("info/alternates", 0o640);
      fs.writeFileSync(alt, path.join(f.bare, "objects") + "\n");
      if (kind === "arbitrary") fs.writeFileSync(alt, f.dir + "\n");
      if (kind === "relative") fs.writeFileSync(alt, "../../bare/objects\n");
      if (kind === "multiple") fs.appendFileSync(alt, f.dir + "\n");
      if (kind === "http") f.file("info/http-alternates");
      if (kind === "symlink") { fs.unlinkSync(alt); fs.symlinkSync(target, alt); }
      if (kind === "commondir") fs.writeFileSync(path.join(f.clone, ".git/commondir"), f.bare + "\n");
      if (kind !== "symlink") fs.chmodSync(alt, 0o440);
      let targetOpens = 0;
      const guarded = { ...f.adapter, openSync(p: fs.PathLike, flags: fs.OpenMode) {
        if (String(p).endsWith("/bare") || String(p).startsWith(f.bare + "/")) {
          targetOpens++; throw Error("alternate target must stay private");
        }
        return fs.openSync(p, flags);
      } };
      const result = f.run({ fs: guarded });
      assert.equal(targetOpens, 0, "helper never opens the selected alternate target");
      if (kind === "allowed") assert.match(result, /^ok /);
      else assert.equal(result, "refused " + (kind === "symlink" ? "special_file" : kind === "commondir" ? "unsafe_root" : "alternates") + "\n");
      assert.equal(fs.statSync(target).mode & 0o7777, 0o600);
      assert.equal(fs.readFileSync(target, "utf8"), "bare bytes");
      assert.deepEqual(f.chmods, []);
    } finally { f.cleanup(); }
  });
}

test("entry/depth/path/deadline bounds reach their own guards", () => {
  const f = fixture();
  try {
    f.file("aa/bb/object");
    assert.equal(f.run({ entries: 1 }), "refused entry_limit\n");
    assert.equal(f.run({ depth: 1 }), "refused depth_limit\n");
    assert.equal(f.run({ pathBytes: Buffer.byteLength(f.objects) + 2 }), "refused path_limit\n");
    let calls = 0;
    assert.equal(f.run({ now: () => ++calls < 20 ? 0 : 30000 }), "refused deadline\n");
    assert.ok(calls >= 20, "deadline fault reached after root opens");
  } finally { f.cleanup(); }
});

});

describe("runner retained readiness ordering and custody", async () => {
  const oldTmp = process.env.TMPDIR;
  process.env.TMPDIR = scratch;
  const h = await import("./runner-harness.js");
  const { noProofReseed, recordingLogger } = await import("./helpers.js");
  after(() => {
    if (oldTmp === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = oldTmp;
  });
  h.installHarness();

  function command(cwd: string, ...args: string[]): string {
    return execFileSync("git", ["-C", cwd, ...args], {
      env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
      encoding: "utf8", stdio: "pipe",
    }).trim();
  }

  function journal(bare: string, branch: string): RecoveryJournalEntry {
    return JSON.parse(command(bare, "config", "--get", `uzi-recovery.${branch}.clone`));
  }

  for (const fault of ["cleanup", "dispose_throw", "startup_unconfirmed", "cleanup_deadline", "clean", "startup_verified", "helper_refusal"] as const) {
    test("runner first readiness result then clean helpers: " + fault, async t => {
      const terminal = fault === "cleanup" || fault === "dispose_throw" || fault === "startup_unconfirmed" || fault === "cleanup_deadline";
      h.client.protocolFeatures = ["claim_generation_fence"];
      const claim = h.gitlabClaim(3059, { claim_generation: 2, session_id: "readiness-predecessor" });
      const bare = await h.git.ensureClone(h.fx.originPath);
      const clone = await h.git.createOrAttachRunnerClone(bare, 3059, noProofReseed, claim.run_id);
      await h.git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
      const expected = { runId: claim.run_id, clonePath: clone.path };
      const sourceHead = await h.git.worktreeHead(clone.path);
      const capturedTip = await h.git.revParse(bare, "HEAD");
      assert.ok(capturedTip);
      const reservation = await h.git.reserveRecoveryIteration(bare, clone.branch, "issue-3059", expected, 1, 2);
      await h.git.recordRecoveryCapture(bare, clone.branch, "issue-3059", expected, reservation.attempts, capturedTip);
      const before = journal(bare, clone.branch);
      const retainedFile = path.join(clone.path, "retained.txt");
      if (terminal) fs.writeFileSync(retainedFile, "only local dirty work\n");
      const sourceBytes = fs.readFileSync(path.join(clone.path, "README.md"));
      const sourceRefs = command(clone.path, "show-ref", "--head");
      const pin = "refs/uzi-recovery-episode/" + claim.run_id + "/" + capturedTip;
      const trackingTip = await h.git.revParse(bare, "refs/uzi-runner/" + clone.branch);
      h.api.setOwnershipStatus(claim.run_id, "running", 2);

      let launches = 0, faults = 0, disposals = 0, prepares = 0, adoptions = 0, models = 0, releases = 0;
      let sourceReads = 0, resumedReads = 0;
      let adopted: RecoveryJournalEntry | undefined;
      let successorPath: string | undefined;
      const events: string[] = [];
      // Inject exactly once. Every later helper is clean, so an illicit retry can
      // reach successor preparation rather than hiding behind repeated failures.
      (h.git as unknown as { retainedReadinessLaunch: typeof import("../src/codex/launcher.js").launchCodexEffectRoot })
        .retainedReadinessLaunch = async () => {
          const first = ++launches === 1;
          events.push("launch");
          if (first && fault.startsWith("startup_")) {
            faults++;
            throw new CodexLaunchError("started_deadline",
              fault === "startup_unconfirmed" ? "unconfirmed" : "verified", Error("fixture startup failure"));
          }
          const stdout = new PassThrough(), stderr = new PassThrough();
          return {
            started: { uid: 10003, liveCapsZero: true, noNewPrivs: true },
            transport: { stdin: new PassThrough(), stdout, stderr },
            async waitChild() {
              if (first && fault === "helper_refusal") faults++;
              stdout.end(first && fault === "helper_refusal" ? "refused owner\n" : "ok 1 0\n");
              stderr.end();
              return { code: 0 };
            },
            async dispose() {
              disposals++;
              events.push("dispose");
              if (first && (fault === "cleanup" || fault === "dispose_throw" || fault === "cleanup_deadline")) {
                faults++;
                if (fault === "cleanup_deadline") t.mock.method(Date, "now", () => reservation.deadline + 1);
                if (fault === "dispose_throw") throw Error("fixture dispose failure");
                return { clean: false };
              }
              return { clean: true };
            },
          } as unknown as CodexRootHandle;
        };
      const status = h.git.worktreeStatus.bind(h.git);
      h.git.worktreeStatus = async (...args) => { sourceReads++; return status(...args); };
      const verify = h.git.verifiedRecoveryRestorePoint.bind(h.git);
      h.git.verifiedRecoveryRestorePoint = async (...args) => {
        resumedReads++;
        events.push("resumed_read");
        return verify(...args);
      };
      const prepare = h.git.prepareRecoverySuccessor.bind(h.git);
      h.git.prepareRecoverySuccessor = async (...args) => {
        prepares++;
        if (terminal) throw Error("unexpected successor preparation after unconfirmed cleanup");
        const successor = await prepare(...args);
        adoptions++;
        successorPath = successor.path;
        adopted = journal(bare, clone.branch);
        // Stop only AFTER the real clone and durable ready-for-model handoff.
        // Assertions below remain outside runner callbacks, where failures cannot be swallowed.
        throw Error("controlled stop after completed successor adoption");
      };
      h.client.releaseRecoveryCustody = async () => { releases++; throw Error("custody release forbidden"); };
      const log = recordingLogger();
      const r = h.runnerWith(runId => ({
        homeDir: path.join(h.homeDir, runId),
        executor: {
          sandboxesCommands: true,
          run: async () => { models++; throw Error("model admission forbidden"); },
          settleForCredentialFreeCapture: async () => {
            events.push("settle");
            return { kind: "observed_empty" };
          },
        },
      }), h.fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
      await r.execute(claim);

      assert.equal(faults, fault === "clean" ? 0 : 1, "the requested first-launch fault was reached");
      assert.equal(launches, terminal || fault === "clean" ? 1 : 2,
        "unconfirmed cleanup must terminate this claim before any otherwise-clean helper can launch");
      assert.equal(disposals, launches - (fault.startsWith("startup_") ? 1 : 0));
      assert.ok(events.indexOf("settle") < events.indexOf("launch"));
      assert.equal(models, 0);
      assert.equal(releases, 0);
      assert.equal(fs.existsSync(clone.path), true);
      assert.deepEqual(fs.readFileSync(path.join(clone.path, "README.md")), sourceBytes);
      assert.equal(await h.git.worktreeHead(clone.path), sourceHead);
      assert.equal(command(clone.path, "show-ref", "--head"), sourceRefs, "predecessor refs remain unchanged");
      assert.equal(await h.git.revParse(bare, pin), capturedTip, "existing episode pin remains in custody");
      assert.equal(await h.git.revParse(bare, "refs/uzi-runner/" + clone.branch), trackingTip);
      assert.equal(h.api.states.some(s => s.body.status === "recovery_wait"), false);
      const after = journal(bare, clone.branch);
      if (terminal) {
        assert.equal(prepares, 0);
        assert.equal(adoptions, 0);
        assert.equal(sourceReads, 0);
        assert.equal(resumedReads, 0);
        assert.equal(after.runId, before.runId);
        assert.equal(after.clonePath, before.clonePath);
        assert.equal(after.attemptId, before.attemptId);
        assert.deepEqual(after.retainedSources, before.retainedSources);
        assert.equal(fs.readFileSync(retainedFile, "utf8"), "only local dirty work\n");
        assert.equal(after.restoreTip, capturedTip);
        const progress = recoveryProgress(after.recovery);
        assert.equal(progress.stage, "blocked");
        assert.deepEqual(progress.source, before.recovery?.source);
        assert.equal(progress.restoreTip, capturedTip);
        assert.equal(progress.blocker, "readiness_cleanup_unconfirmed", "persist the typed cleanup blocker");
        assert.equal(progress.attempts, reservation.attempts + 1, "charge only the failed readiness iteration");
        assert.equal(progress.successor, undefined);
        assert.ok(h.api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
        assert.match(JSON.stringify(log.lines), /readiness_cleanup_unconfirmed/);
      } else {
        assert.equal(prepares, 1);
        assert.equal(adoptions, 1, "original prepareRecoverySuccessor completed");
        assert.ok(resumedReads > 0);
        assert.ok(events.indexOf("dispose") < events.indexOf("resumed_read"));
        assert.ok(adopted);
        assert.ok(successorPath);
        assert.notEqual(successorPath, clone.path);
        assert.equal(adopted.clonePath, successorPath);
        const progress = recoveryProgress(adopted.recovery);
        assert.equal(progress.stage, "ready-for-model");
        assert.equal(progress.blocker, undefined);
        assert.equal(progress.successor?.clonePath, successorPath);
        assert.equal(progress.attempts, reservation.attempts + launches);
        assert.ok(adopted.retainedSources?.some(s => s.clonePath === clone.path));
        assert.equal(await h.git.worktreeHead(successorPath), sourceHead);
        assert.deepEqual(fs.readFileSync(path.join(successorPath, "README.md")), sourceBytes);
        assert.equal(after.recovery?.stage, "ready-for-model", "controlled stop preserves completed adoption evidence");
        if (fault !== "clean") {
          assert.match(JSON.stringify(log.lines), /retained recovery iteration failed; reserved budget retained/);
          assert.match(JSON.stringify(log.lines), fault === "startup_verified" ? /retained_readiness_launch/ : /retained_readiness_owner/);
        }
      }
    });
  }
  for (const fault of ["cleanup", "malformed", "none", "resumed", "resumed_cleanup", "profile", "legacy"]) test("runner gate: " + fault, async () => {
    const claim = h.gitlabClaim(3059, { claim_generation: 2, session_id: "readiness-predecessor" });
    const bare = await h.git.ensureClone(h.fx.originPath);
    const clone = await h.git.createOrAttachRunnerClone(bare, 3059, noProofReseed, claim.run_id);
    await h.git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
    h.api.setOwnershipStatus(claim.run_id, "running", 2);
    let capturedTip: string | undefined;
    if (fault.startsWith("resumed")) {
      const expected = { runId: claim.run_id, clonePath: clone.path };
      const tip = await h.git.revParse(bare, "HEAD");
      assert.ok(tip);
      capturedTip = tip;
      const reservation = await h.git.reserveRecoveryIteration(bare, clone.branch, "issue-3059", expected, 1, 2);
      await h.git.recordRecoveryCapture(bare, clone.branch, "issue-3059", expected, reservation.attempts, tip);
    }
    const sourceHead = await h.git.worktreeHead(clone.path);
    let launches = 0, disposals = 0, models = 0, adoptions = 0, releases = 0, sourceReads = 0;
    const events: string[] = [];
    (h.git as unknown as { retainedReadinessLaunch: typeof import("../src/codex/launcher.js").launchCodexEffectRoot })
      .retainedReadinessLaunch = async () => {
        launches++; events.push("launch");
        const stdout = new PassThrough(), stderr = new PassThrough();
        return {
          started: { uid: 10003, liveCapsZero: true, noNewPrivs: true },
          transport: { stdin: new PassThrough(), stdout, stderr },
          async waitChild() { stdout.end(fault === "malformed" ? "bad" : "ok 1 0\n"); stderr.end(); return { code: 0 }; },
          async dispose() { disposals++; events.push("dispose"); return { clean: !fault.endsWith("cleanup") }; },
        } as unknown as CodexRootHandle;
      };
    const verify = h.git.verifiedRecoveryRestorePoint.bind(h.git);
    let resumedReads = 0;
    h.git.verifiedRecoveryRestorePoint = async (...args) => {
      resumedReads++; events.push("resumed_read");
      return verify(...args);
    };
    const status = h.git.worktreeStatus.bind(h.git);
    h.git.worktreeStatus = async (...args) => {
      sourceReads++; events.push("source_read");
      return status(...args);
    };
    h.git.prepareRecoverySuccessor = async () => { adoptions++; throw Error("fixture stops at adoption"); };
    h.client.releaseRecoveryCustody = async () => { releases++; throw Error("custody release forbidden"); };
    const log = recordingLogger();
    const r = h.runnerWith(runId => ({
      homeDir: path.join(h.homeDir, runId),
      executor: {
        sandboxesCommands: fault === "legacy" ? false : fault === "profile" ? "unknown" as unknown as boolean : true,
        run: async () => { models++; throw Error("model forbidden"); },
        settleForCredentialFreeCapture: async () => { events.push("settle"); return { kind: "observed_empty" }; },
      },
    }), h.fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
    await r.execute(claim);
    if (fault === "legacy" || fault === "profile") assert.equal(launches, 0);
    else {
      assert.ok(launches > 0, "fault must reach actual readiness launch after reservation and proof");
      assert.ok(events.indexOf("settle") < events.indexOf("launch"));
    }
    assert.equal(disposals, launches);
    if (fault === "none" || fault === "resumed" || fault === "legacy") {
      if (fault === "resumed") {
        assert.ok(resumedReads > 0);
        assert.ok(events.indexOf("dispose") < events.indexOf("resumed_read"));
      } else {
        assert.ok(sourceReads > 0);
        if (fault !== "legacy") assert.ok(events.indexOf("dispose") < events.indexOf("source_read"));
      }
      assert.ok(adoptions > 0);
    } else {
      assert.equal(sourceReads, 0);
      assert.equal(resumedReads, 0);
      assert.equal(adoptions, 0);
      assert.match(JSON.stringify(log.lines), fault.endsWith("cleanup") ? /readiness_cleanup_unconfirmed/ : /retained_readiness_/);
    }
    assert.equal(models, 0);
    assert.equal(releases, 0);
    assert.equal(fs.existsSync(clone.path), true);
    assert.equal(await h.git.worktreeHead(clone.path), sourceHead, "predecessor refs survive gate failure/adoption stop");
    if (capturedTip) assert.equal(await h.git.revParse(bare, "refs/uzi-recovery-episode/" + claim.run_id + "/" + capturedTip), capturedTip,
      "the existing captured episode pin survives");
  });
});

test("worker refuses chained alternate targets before launching the helper", async () => {
  const f = fixture();
  let launches = 0;
  const cache = new GitCache(f.dir, logger, undefined, { retainedReadinessLaunchForTest: async () => {
    launches++; throw Error("must not launch");
  } });
  const seams = cache as unknown as GuardSeam;
  seams.checkedRecovery = async () => ({});
  seams.classifyOwnerClonePath = async () => "canonical";
  const expected = { clonePath: f.clone, runId: "run" } as RecoverySource;
  try {
    fs.writeFileSync(path.join(f.bare, "objects/info/alternates"), "/arbitrary-target\n");
    await assert.rejects(() => cache.withRecoveryOperation(new AbortController().signal, Date.now() + 60000,
      () => cache.normalizeRetainedSourceObjects(f.bare, "branch", "key", expected)), /retained_readiness_alternates/);
    assert.equal(launches, 0, "worker, not command identity, checks the private target");
  } finally { f.cleanup(); }
});

type GuardSeam = {
  checkedRecovery: () => Promise<object>;
  classifyOwnerClonePath: () => Promise<"canonical" | undefined>;
};
for (const fault of ["none", "missing_pipe", "missing_stdout", "missing_stderr", "cancelled", "deadline", "nonzero", "malformed", "helper_refusal", "stdout_overflow", "stderr_overflow", "cleanup", "cleanup_cancelled", "refusal_cleanup", "dispose_throw", "startup_unconfirmed", "startup_verified", "launch", "displaced", "journal_changed", "identity", "child"]) {
  test("supervised readiness refusal/settlement: " + fault, async t => {
    const f = fixture();
    const controller = new AbortController();
    let launches = 0, waits = 0, disposals = 0, checks = 0;
    let spec: CodexEffectLaunchSpec | undefined;
    let waited = false;
    let expire: (() => void) | undefined;
    if (fault === "deadline") {
      const timer = globalThis.setTimeout;
      t.mock.method(globalThis, "setTimeout", (callback: (...args: any[]) => void, delay?: number, ...args: any[]) => {
        if (delay !== undefined && delay > 29000 && delay <= 30000) expire = () => callback(...args);
        return timer(callback, delay, ...args);
      });
    }
    const cache = new GitCache(f.dir, logger, undefined, {
      retainedReadinessLaunchForTest: async request => {
        launches++; spec = request;
        if (fault === "launch") throw Error("private failure path");
        if (fault.startsWith("startup_")) throw new CodexLaunchError("started_deadline",
          fault === "startup_unconfirmed" ? "unconfirmed" : "verified", Error("private startup failure"));
        const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
        return {
          started: { uid: fault === "identity" ? 10002 : 10003, liveCapsZero: true, noNewPrivs: true },
          transport: { stdin: fault === "missing_pipe" ? null : stdin,
            stdout: fault === "missing_stdout" ? null : stdout, stderr: fault === "missing_stderr" ? null : stderr },
          async waitChild() {
            waits++;
            if (fault === "cancelled") controller.abort();
            if (fault === "deadline") { assert.ok(expire); expire(); }
            if (fault === "displaced") {
              fs.renameSync(f.objects, f.objects + "-old");
              fs.mkdirSync(f.objects);
            }
            stdout.end(fault === "malformed" ? "private malformed" : fault === "helper_refusal" || fault === "refusal_cleanup" ? "refused hardlinked_mutation\n" : fault === "stdout_overflow" ? "x".repeat(4097) : "ok 1 0\n");
            stderr.end(fault === "stderr_overflow" ? "x".repeat(4097) : "");
            waited = true;
            if (fault === "child") throw Error("private child failure");
            return { code: fault === "nonzero" ? 1 : 0 };
          },
          async dispose() {
            disposals++;
            assert.equal(waited, true);
            if (fault === "dispose_throw") throw Error("private cleanup path");
            if (fault === "cleanup_cancelled") controller.abort();
            return { clean: fault !== "cleanup" && fault !== "cleanup_cancelled" && fault !== "refusal_cleanup" };
          },
        } as unknown as CodexRootHandle;
      },
    });
    const seams = cache as unknown as GuardSeam;
    seams.checkedRecovery = async () => { checks++; return fault === "journal_changed" ? { revision: checks } : {}; };
    seams.classifyOwnerClonePath = async () => "canonical";
    const expected = { clonePath: f.clone, runId: "run" } as RecoverySource;
    try {
      const run = () => cache.withRecoveryOperation(controller.signal, Date.now() + 60000,
        () => cache.normalizeRetainedSourceObjects(f.bare, "branch", "key", expected));
      if (fault === "none") await run();
      else {
        const reason = fault.startsWith("missing_") ? "missing_pipe" : fault.endsWith("_overflow") ? "output_limit"
          : ["dispose_throw", "cleanup_cancelled", "refusal_cleanup", "startup_unconfirmed"].includes(fault) ? "cleanup"
          : fault === "startup_verified" ? "launch" : fault === "displaced" ? "displaced_root"
          : fault === "helper_refusal" ? "hardlinked_mutation" : fault;
        await assert.rejects(run, new RegExp("^Error: retained_readiness_" + reason + "$"));
      }
      assert.equal(launches, 1);
      if (fault !== "launch" && !fault.startsWith("startup_")) {
        assert.equal(waits, 1);
        assert.equal(disposals, 1);
      }
      assert.equal(checks, fault === "none" || fault === "journal_changed" ? 2 : 1, "failed root cannot progress to post-readiness attribution");
      assert.equal(spec!.identity, "command");
      assert.equal(spec!.command, process.execPath);
      assert.equal(spec!.supervisorBin, SUPERVISOR_BIN);
      assert.equal(spec!.cwd, "/");
      assert.deepEqual(spec!.env, { LANG: "C" });
      assert.equal(spec!.args[0], "-e");
      assert.equal(spec!.args[1], helper);
      assert.equal(fs.existsSync(f.clone), true);
    } finally { f.cleanup(); }
  });
}

test("readiness requires recovery operation and current attribution before launching", async () => {
  const f = fixture();
  let launches = 0;
  const cache = new GitCache(f.dir, logger, undefined, { retainedReadinessLaunchForTest: async () => {
    launches++; throw Error();
  } });
  const expected = { clonePath: f.clone, runId: "run" } as RecoverySource;
  try {
    await assert.rejects(() => cache.normalizeRetainedSourceObjects(f.bare, "branch", "key", expected), /operation_required/);
    const seams = cache as unknown as GuardSeam;
    seams.checkedRecovery = async () => ({});
    seams.classifyOwnerClonePath = async () => undefined;
    await assert.rejects(() => cache.withRecoveryOperation(new AbortController().signal, Date.now() + 60000,
      () => cache.normalizeRetainedSourceObjects(f.bare, "branch", "key", expected)), /attribution/);
    assert.equal(launches, 0);
  } finally { f.cleanup(); }
});
