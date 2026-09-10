import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { randomBytes } from "node:crypto";
import { dirname, join } from "node:path";

import {
  CodexSessionStore,
  createCodexSessionStore,
  seedCodexSessionArtifacts,
  storeLockRegistrySize,
  CodexSessionStoreBoundError,
  CodexSessionStoreError,
  DEFAULT_SESSION_STORE_BOUNDS,
  SESSION_ALLOWED_EXTENSIONS,
  SESSION_ALLOWED_SUBDIR,
  SESSION_DENY_EXTENSIONS,
  SESSION_DENY_NAME_SUBSTRINGS,
  type AdoptResult,
  type PersistResult,
  type SessionStoreBounds,
} from "../src/codex/session-state.js";

// PRD #1171 (M3, plan §2.7 / item 7): credential-free session-state ownership.
//
// These tests exercise the allowlist copy, the symlink + bound safety, and — the focus of the
// publication model below — the immutable-generation + atomically-renamed pointer store: a
// reader concurrent with a publish (or a crash) sees the whole OLD or the whole NEW generation,
// never an absent/partial one, and a failed persist never destroys or strands the last good
// generation. Everything runs against temp dirs — no real Codex.

async function writeFileAt(path: string, content: string): Promise<void> {
  await fsp.mkdir(dirname(path), { recursive: true });
  await fsp.writeFile(path, content);
}

/** Recursively list files (as POSIX-style relative paths) under `dir`. */
async function collectRelFiles(dir: string): Promise<string[]> {
  const out: string[] = [];
  const walk = async (rel: string): Promise<void> => {
    let entries;
    try {
      entries = await fsp.readdir(join(dir, rel), { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const child = rel === "" ? entry.name : `${rel}/${entry.name}`;
      if (entry.isDirectory()) await walk(child);
      else out.push(child);
    }
  };
  await walk("");
  return out.sort();
}

function delay(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
function tick(): Promise<void> {
  return new Promise((r) => setImmediate(r));
}
async function waitFor(cond: () => boolean, label: string, ms = 3000): Promise<void> {
  const start = Date.now();
  while (!cond()) {
    if (Date.now() - start > ms) throw new Error(`timed out waiting for ${label}`);
    await new Promise((r) => setImmediate(r));
  }
}

// ─── store-layout helpers (generations/<genId>/sessions/** + a plain-text `current`) ─────

/** A fresh generation id matching the module's closed grammar `/^g[0-9a-f]{24}$/`. */
function freshGenId(): string {
  return `g${randomBytes(12).toString("hex")}`;
}
/** The id in the store's `current` pointer, or undefined when there is no valid pointer. */
async function currentGenId(storeDir: string): Promise<string | undefined> {
  try {
    return (await fsp.readFile(join(storeDir, "current"), "utf8")).trim() || undefined;
  } catch {
    return undefined;
  }
}
/** The current generation's directory (asserts a pointer exists). */
async function currentGenDir(storeDir: string): Promise<string> {
  const id = await currentGenId(storeDir);
  assert.ok(id, "expected a current-generation pointer");
  return join(storeDir, "generations", id);
}
/** The current generation's session files as `sessions/<rel>` (empty when no pointer). */
async function storedSessions(storeDir: string): Promise<string[]> {
  const id = await currentGenId(storeDir);
  if (!id) return [];
  return collectRelFiles(join(storeDir, "generations", id));
}
/** The generation directory names present under `storeDir/generations`, sorted. */
async function genDirNames(storeDir: string): Promise<string[]> {
  try {
    const entries = await fsp.readdir(join(storeDir, "generations"), { withFileTypes: true });
    return entries.filter((e) => e.isDirectory()).map((e) => e.name).sort();
  } catch {
    return [];
  }
}
/** Hand-build a generation (and optionally publish its pointer), bypassing persist — used to
 *  construct the exact on-disk states a crash/fault would leave, so the READER is tested against
 *  each without needing to interrupt persist mid-flight. */
async function seedGeneration(
  storeDir: string,
  files: Record<string, string>,
  opts: { publish?: boolean; genId?: string } = {},
): Promise<string> {
  const genId = opts.genId ?? freshGenId();
  const sessionsDir = join(storeDir, "generations", genId, SESSION_ALLOWED_SUBDIR);
  await fsp.mkdir(sessionsDir, { recursive: true });
  for (const [rel, content] of Object.entries(files)) {
    await writeFileAt(join(sessionsDir, rel), content);
  }
  if (opts.publish ?? true) await writeFileAt(join(storeDir, "current"), `${genId}\n`);
  return genId;
}

describe(
  "CodexSessionStore",
  {
    skip: process.platform === "linux"
      ? false
      : "fd-anchored store requires Linux /proc/self/fd; production workers are Linux containers",
  },
  () => {
  let root: string;
  let codexHome: string;
  let storeDir: string;

  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), "codex-session-"));
    codexHome = join(root, "codex-home");
    storeDir = join(root, "codex-session");
  });

  afterEach(() => {
    rmSync(root, { recursive: true, force: true });
  });

  // ─── copy / allowlist / symlink / bounds (ported to the generation layout) ──────

  it("persist copies the sessions rollout subtree and EXCLUDES auth-shaped files", async () => {
    await writeFileAt(
      join(codexHome, "sessions", "rollout-2026-01-01T00-00-00-abc.jsonl"),
      '{"type":"session_meta"}\n',
    );
    // Auth material at the CODEX_HOME ROOT — outside the allowed subtree entirely.
    await writeFileAt(join(codexHome, "auth.json"), '{"tokens":{"access_token":"SECRET_ACCESS"}}');
    await writeFileAt(join(codexHome, "credentials.json"), "SECRET_ROOT_CRED");
    // Auth-shaped files INSIDE sessions/ — excluded by the deny substrings despite .jsonl.
    await writeFileAt(join(codexHome, "sessions", "auth-token.jsonl"), "SECRET_AUTH_JSONL");
    await writeFileAt(join(codexHome, "sessions", "credential-cache.jsonl"), "SECRET_CRED");
    await writeFileAt(join(codexHome, "sessions", "model-cache.jsonl"), "SECRET_CACHE");
    await writeFileAt(join(codexHome, "sessions", "refresh.jsonl"), "SECRET_REFRESH");
    await writeFileAt(join(codexHome, "sessions", "id_rsa.pem"), "SECRET_PEM");

    const result: PersistResult = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "only the one credential-free rollout is copied");
    assert.ok(result.bytes > 0);

    // The store dir is created mode 0700.
    assert.equal((await fsp.stat(storeDir)).mode & 0o777, 0o700);

    assert.deepEqual(await storedSessions(storeDir), [
      "sessions/rollout-2026-01-01T00-00-00-abc.jsonl",
    ]);

    const genDir = await currentGenDir(storeDir);
    for (const name of [
      "auth.json",
      "credentials.json",
      "auth-token.jsonl",
      "credential-cache.jsonl",
      "model-cache.jsonl",
      "refresh.jsonl",
      "id_rsa.pem",
    ]) {
      assert.ok(
        !(await collectRelFiles(genDir)).some((p) => p.endsWith(name)),
        `${name} must NOT be in the store`,
      );
    }
    for (const rel of await collectRelFiles(genDir)) {
      assert.doesNotMatch(await fsp.readFile(join(genDir, rel), "utf8"), /SECRET/);
    }
  });

  it("runner seed reuses the allowlist and writes the final 2750/0640 posture", async () => {
    await writeFileAt(join(codexHome, "sessions", "2026", "rollout-safe.jsonl"), "safe\n");
    await CodexSessionStore.persist(codexHome, storeDir);

    const stagedHome = join(root, "epoch-2.session-seed");
    const adopted = await CodexSessionStore.adopt(
      storeDir,
      stagedHome,
      { destination: "runner-seed" },
    );
    assert.equal(adopted.files, 1);
    assert.equal((await fsp.stat(stagedHome)).mode & 0o777, 0o750);
    assert.equal((await fsp.stat(join(stagedHome, "sessions", "2026", "rollout-safe.jsonl"))).mode & 0o777, 0o640);

    // Defense in depth: even if the trusted staging step were later widened, the
    // runner-side copy applies the same allowlist again.
    await writeFileAt(join(stagedHome, "sessions", "auth-token.jsonl"), "never-copy\n");
    await writeFileAt(join(stagedHome, "sessions", "notes.txt"), "never-copy\n");
    const providerSessions = join(root, "epoch-2", "codex", "sessions");
    await fsp.mkdir(providerSessions, { recursive: true, mode: 0o2750 });

    const seeded = await seedCodexSessionArtifacts(join(stagedHome, "sessions"), providerSessions);
    assert.equal(seeded.files, 1);
    assert.deepEqual(await collectRelFiles(providerSessions), ["2026/rollout-safe.jsonl"]);
    assert.equal((await fsp.stat(join(providerSessions, "2026"))).mode & 0o7777, 0o2750);
    assert.equal((await fsp.stat(join(providerSessions, "2026", "rollout-safe.jsonl"))).mode & 0o777, 0o640);
  });

  it("adopt seeds a fresh codexHome/sessions from the store (round-trip preserves rollouts)", async () => {
    const relRollout = join("sessions", "2026", "01", "rollout-xyz.jsonl");
    const content = '{"record":"one"}\n{"record":"two"}\n';
    await writeFileAt(join(codexHome, relRollout), content);

    const persisted = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(persisted.files, 1);

    const freshHome = join(root, "fresh-home");
    const adopted: AdoptResult = await CodexSessionStore.adopt(storeDir, freshHome);
    assert.equal(adopted.files, 1);

    const seeded = await fsp.readFile(join(freshHome, relRollout), "utf8");
    assert.equal(seeded, content, "round-trip preserves the rollout bytes and nested layout");
  });

  it("a symlink in the source sessions/ pointing outside is NOT copied (rejected)", async () => {
    const outsideDir = join(root, "outside");
    const outsideSecret = join(outsideDir, "secret.jsonl");
    await writeFileAt(outsideSecret, "SECRET_OUTSIDE_CONTENT");

    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await fsp.symlink(outsideSecret, join(codexHome, "sessions", "link.jsonl"));
    await fsp.symlink(outsideDir, join(codexHome, "sessions", "linkdir"));

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "only the real rollout, never the symlink");

    const stored = await storedSessions(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.includes("link")), "no symlinked path is copied");
    const genDir = await currentGenDir(storeDir);
    for (const rel of stored) {
      assert.doesNotMatch(await fsp.readFile(join(genDir, rel), "utf8"), /SECRET_OUTSIDE_CONTENT/);
    }
  });

  it("FIX 1: persist REFUSES a symlinked top-level sessions/ pointing at an outside dir", async () => {
    const outsideDir = join(root, "outside-sessions");
    await writeFileAt(join(outsideDir, "rollout-outside.jsonl"), "SECRET_OUTSIDE_ROLLOUT");
    await fsp.mkdir(codexHome, { recursive: true });
    await fsp.symlink(outsideDir, join(codexHome, "sessions"));

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir),
      CodexSessionStoreError,
      "persist must refuse a symlinked top-level sessions/ rather than follow it",
    );
    // No generation was published: the store reads absent, nothing out-of-tree leaked.
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
    assert.deepEqual(await storedSessions(storeDir), []);
  });

  it("FIX 1: persist REFUSES a symlinked top-level sessions/ pointing at the codexHome ROOT", async () => {
    await fsp.mkdir(codexHome, { recursive: true });
    await writeFileAt(join(codexHome, "history.jsonl"), "SECRET_ROOT_HISTORY");
    await writeFileAt(join(codexHome, "auth.json"), "SECRET_ROOT_AUTH");
    await fsp.symlink(codexHome, join(codexHome, "sessions"));

    await assert.rejects(() => CodexSessionStore.persist(codexHome, storeDir), CodexSessionStoreError);
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
    assert.deepEqual(await storedSessions(storeDir), []);
  });

  it("FIX 1: a symlinked INTERMEDIATE directory under sessions/ is not followed", async () => {
    const outsideDir = join(root, "outside-tree");
    await writeFileAt(join(outsideDir, "rollout-outside.jsonl"), "SECRET_OUTSIDE_TREE");
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await fsp.symlink(outsideDir, join(codexHome, "sessions", "subdir"));

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "only the in-tree rollout; the symlinked subtree is skipped");

    const stored = await storedSessions(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.includes("outside")), "no file from the symlinked subtree");
  });

  it("FIX 1: a deny-substring directory under sessions/ is not descended", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await writeFileAt(join(codexHome, "sessions", "auth-cache", "rollout-inner.jsonl"), "SECRET_INNER");

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "the auth-cache/ dir is skipped entirely");
    assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-real.jsonl"]);
  });

  it("access.jsonl under sessions/ is excluded by the deny-substring list", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await writeFileAt(join(codexHome, "sessions", "access.jsonl"), "SECRET_ACCESS");

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "access.jsonl excluded; only the real rollout copies");
    assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-real.jsonl"]);
  });

  it("bounded: persist errs at the file-count cap rather than copying unboundedly", async () => {
    for (let i = 0; i < 6; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `rollout-${i}.jsonl`), `line-${i}`);
    }
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFiles: 2 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      CodexSessionStoreBoundError,
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent", "no generation published on a breach");
  });

  it("bounded: persist errs on a single file over the per-file byte cap and publishes nothing", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-big.jsonl"), "x".repeat(1024));
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFileBytes: 16 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError);
        assert.equal(err.bound, "maxFileBytes");
        return true;
      },
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
  });

  it("bounded: persist errs at the total-bytes cap", async () => {
    for (let i = 0; i < 4; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `rollout-${i}.jsonl`), "x".repeat(64));
    }
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxTotalBytes: 100 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      CodexSessionStoreBoundError,
    );
  });

  it("FIX 2: copy path is bounded by a per-entry scan cap, not just by files copied", async () => {
    for (let i = 0; i < 40; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `junk-${i}.txt`), "x");
    }
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFiles: 1, maxScanEntries: 5 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError, "a bound error, not an unbounded walk");
        assert.equal(err.bound, "maxScanEntries", "the SCAN cap tripped, not maxFiles");
        return true;
      },
    );
  });

  it("FIX 2: a within-bounds nested tree copies with the correct files/bytes tally", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "aaaa"); // 4 bytes
    await writeFileAt(join(codexHome, "sessions", "2026", "rollout-b.jsonl"), "bbbbbb"); // 6 bytes
    await writeFileAt(join(codexHome, "sessions", "notes.txt"), "ignored"); // not allowlisted

    const result: PersistResult = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 2, "both allowlisted rollouts copied; notes.txt skipped");
    assert.equal(result.bytes, 10, "bytes tally is the sum of the actual rollout bytes");

    assert.deepEqual(await storedSessions(storeDir), [
      "sessions/2026/rollout-b.jsonl",
      "sessions/rollout-a.jsonl",
    ]);
    const genDir = await currentGenDir(storeDir);
    assert.equal(await fsp.readFile(join(genDir, "sessions", "rollout-a.jsonl"), "utf8"), "aaaa");
    assert.equal(await fsp.readFile(join(genDir, "sessions", "2026", "rollout-b.jsonl"), "utf8"), "bbbbbb");
  });

  it("FIX 4: persist REFUSES a source tree deeper than maxDepth (fail-closed, publishes nothing)", async () => {
    const deep = join(codexHome, "sessions", "l1", "l2", "l3", "l4", "l5");
    await writeFileAt(join(deep, "rollout-deep.jsonl"), '{"deep":true}\n');
    const shallow: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxDepth: 3 };

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: shallow }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError, "a bound error, not an fd-holding recursion");
        assert.equal(err.bound, "maxDepth", "the recursion-DEPTH cap tripped");
        return true;
      },
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent", "a depth-breached persist publishes nothing");
  });

  it("FIX 4: adopt STOPS on a too-deep store without throwing (fail-safe, copies 0)", async () => {
    const depthTarget = (DEFAULT_SESSION_STORE_BOUNDS.maxDepth ?? 64) + 5;
    let rel = "";
    for (let i = 0; i < depthTarget; i += 1) rel = rel === "" ? "d" : `${rel}/d`;
    await seedGeneration(storeDir, { [`${rel}/rollout-deep.jsonl`]: '{"deep":true}\n' });

    const freshHome = join(root, "fresh-home");
    const adopted = await CodexSessionStore.adopt(storeDir, freshHome);
    assert.equal(adopted.files, 0, "a too-deep store is not adopted (fail-safe stop, no throw)");
    assert.deepEqual(await collectRelFiles(freshHome), [], "the too-deep rollout copied no file");

    // Control: a within-depth store adopts.
    const validStore = join(root, "valid-store");
    await seedGeneration(validStore, { "rollout-ok.jsonl": '{"ok":true}\n' });
    assert.equal((await CodexSessionStore.adopt(validStore, join(root, "valid-home"))).files, 1);
  });

  it("adopt REFUSES to write through a symlinked dest sessions/", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await CodexSessionStore.persist(codexHome, storeDir);

    const freshHome = join(root, "fresh-home");
    const outsideDir = join(root, "adopt-outside");
    await fsp.mkdir(outsideDir, { recursive: true });
    await fsp.mkdir(freshHome, { recursive: true });
    await fsp.symlink(outsideDir, join(freshHome, "sessions"));

    await CodexSessionStore.adopt(storeDir, freshHome);

    assert.equal(
      existsSync(join(outsideDir, "rollout-real.jsonl")),
      false,
      "adopt must not write the adopted file through the symlinked dest sessions/",
    );
    assert.equal(
      existsSync(join(freshHome, "sessions", "rollout-real.jsonl")),
      true,
      "the rollout landed in a real, in-tree sessions dir",
    );
  });

  it("persist wraps a raw fs error in a path-free module error", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    // storeDir pre-exists as a FILE, so persist's mkdir/open throws — a raw fs error.
    await writeFileAt(storeDir, "i am a file, not a directory");

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreError, "raw fs error is wrapped");
        assert.equal((err as { path?: unknown }).path, undefined, "wrapped error carries no .path");
        assert.equal((err as CodexSessionStoreError).category, "io-error");
        assert.doesNotMatch((err as Error).message, /\//, "message names no filesystem path");
        return true;
      },
    );
  });

  it("MUTATION SURFACE: persist REFUSES a symlinked codexHome (fd-anchored, no ancestor follow)", async () => {
    // The real credential-free source lives at realHome/sessions; codexHome is a SYMLINK to it.
    // persist opens codexHome O_NOFOLLOW, so a symlinked codexHome is refused rather than
    // followed — a leak vector if the fd-anchor were dropped (the ancestor-follow mutation).
    const realHome = join(root, "real-home");
    await writeFileAt(join(realHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await fsp.symlink(realHome, codexHome);

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir),
      CodexSessionStoreError,
      "a symlinked codexHome must be refused, not followed",
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");

    // adopt is the fail-safe twin: a symlinked codexHome dest is a no-op (never followed).
    await seedGeneration(storeDir, { "rollout-x.jsonl": '{"x":true}\n' });
    const linkedDest = join(root, "linked-dest");
    const realDest = join(root, "real-dest");
    await fsp.mkdir(realDest, { recursive: true });
    await fsp.symlink(realDest, linkedDest);
    assert.equal((await CodexSessionStore.adopt(storeDir, linkedDest)).files, 0, "symlinked codexHome dest → no-op");
    assert.equal(existsSync(join(realDest, "sessions")), false, "nothing written through the symlinked dest");
  });

  // ─── publication model: immutable generations + atomic pointer ──────────────────

  it("TX: a reader is NEVER absent across repeated publishes (atomic pointer, no gap)", async () => {
    // Seed an initial published store.
    await writeFileAt(join(codexHome, "sessions", "rollout-0.jsonl"), "0");
    await CodexSessionStore.persist(codexHome, storeDir);

    // A reader samples inspect + adopt in a TIGHT loop (interleaving with each fs op) while many
    // publishes run back-to-back. The atomic pointer means `current` always names a complete
    // generation, so a reader can never see "absent"/0. A publication gap (unlink-then-write,
    // or copy-into-a-live sessions dir) would surface here across the dense sampling.
    // The reader RECORDS observations (never throws inside the loop) so it always runs to `stop`
    // — no background promise can leak into a later test if an assertion would fail.
    let stop = false;
    let samples = 0;
    let sawAbsent = false;
    let sawEmptyAdopt = false;
    let pointerUnlinked = false;
    // adopt creates only the FINAL codexHome component (ancestor-safe), so its parent must exist.
    const freshHome = join(root, "reader-home");
    await fsp.mkdir(freshHome, { recursive: true });
    // A TIGHT prober directly probes the invariant "`current` is atomically REPLACED, never
    // unlinked" — one stat per iteration, far more sensitive than the fuller inspect/adopt checks
    // to the tiny publication window a gap (unlink-then-rename) would open.
    const prober = (async () => {
      while (!stop) {
        try {
          await fsp.stat(join(storeDir, "current"));
        } catch {
          pointerUnlinked = true;
        }
      }
    })();
    const reader = (async () => {
      while (!stop) {
        if ((await CodexSessionStore.inspect(storeDir)) === "absent") sawAbsent = true;
        const a = await CodexSessionStore.adopt(storeDir, join(freshHome, `s${samples % 4}`));
        if (a.files < 1) sawEmptyAdopt = true;
        samples += 1;
      }
    })();

    for (let i = 1; i <= 80; i += 1) {
      await fsp.rm(join(codexHome, "sessions"), { recursive: true, force: true });
      await writeFileAt(join(codexHome, "sessions", `rollout-${i}.jsonl`), String(i));
      await CodexSessionStore.persist(codexHome, storeDir);
    }
    stop = true;
    await Promise.all([prober, reader]);
    assert.ok(samples > 0, "the reader sampled during the publishes");
    assert.equal(pointerUnlinked, false, "`current` was unlinked mid-publish (publication gap)");
    assert.equal(sawAbsent, false, "inspect saw an absent store mid-publish (publication gap)");
    assert.equal(sawEmptyAdopt, false, "adopt saw an incomplete store mid-publish (publication gap)");
    assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-80.jsonl"], "final state is the last publish");
  });

  it("TX: a fault BEFORE the pointer swap leaves the OLD generation current; AFTER, the NEW one", async () => {
    // Build the exact on-disk state a fault at each point leaves — the reader must resolve
    // old-or-new, never absent (the single pointer rename is the only transition).
    const oldGen = await seedGeneration(storeDir, { "rollout-old.jsonl": '{"old":true}\n' });
    // A COMPLETE new generation exists but the pointer has NOT been swapped (fault-before-swap).
    const newGen = await seedGeneration(storeDir, { "rollout-new.jsonl": '{"new":true}\n' }, {
      publish: false,
    });
    assert.notEqual(oldGen, newGen);

    // Pointer still names OLD → reader sees OLD.
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
    let home = join(root, "home-before");
    await CodexSessionStore.adopt(storeDir, home);
    assert.equal(existsSync(join(home, "sessions", "rollout-old.jsonl")), true, "before swap → OLD");
    assert.equal(existsSync(join(home, "sessions", "rollout-new.jsonl")), false);

    // Simulate the atomic swap (what publishPointer does) → reader sees NEW.
    await writeFileAt(join(storeDir, "current"), `${newGen}\n`);
    home = join(root, "home-after");
    await CodexSessionStore.adopt(storeDir, home);
    assert.equal(existsSync(join(home, "sessions", "rollout-new.jsonl")), true, "after swap → NEW");
    assert.equal(existsSync(join(home, "sessions", "rollout-old.jsonl")), false);
  });

  it("TX: a leftover temp pointer (crash before rename) does not hide the OLD generation", async () => {
    const oldGen = await seedGeneration(storeDir, { "rollout-old.jsonl": '{"old":true}\n' });
    // A crash between temp-write and rename leaves a stray .current.tmp.* alongside `current`.
    await writeFileAt(join(storeDir, ".current.tmp.deadbeef"), `${freshGenId()}\n`);
    assert.equal(await currentGenId(storeDir), oldGen, "the canonical pointer is still `current`");
    assert.equal(await CodexSessionStore.inspect(storeDir), "present", "the OLD generation is still resolved");
  });

  it("TX: a double failure leaves the OLD canonical generation discoverable and reaps orphans", async () => {
    // The good store (pointer → oldGen), plus two never-pointed orphans from crashed persists.
    const oldGen = await seedGeneration(storeDir, { "rollout-old.jsonl": '{"old":true}\n' });
    const orphanA = await seedGeneration(storeDir, { "rollout-x.jsonl": "x" }, { publish: false });
    const orphanB = await seedGeneration(storeDir, {}, { publish: false }); // empty partial gen

    // The old store is fully discoverable despite the orphans.
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
    assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-old.jsonl"]);

    // A subsequent successful persist keeps only {new, previous=old} and reaps the orphans.
    await writeFileAt(join(codexHome, "sessions", "rollout-new.jsonl"), '{"new":true}\n');
    await CodexSessionStore.persist(codexHome, storeDir);
    const gens = await genDirNames(storeDir);
    assert.ok(gens.includes(oldGen), "the previous generation is kept (reader grace)");
    assert.ok(!gens.includes(orphanA) && !gens.includes(orphanB), "orphans are reaped");
    assert.equal(gens.length, 2, "exactly {new, previous} remain");
  });

  it("TX: a persist reaps stale generations, keeping only the new + previous", async () => {
    const genA = await seedGeneration(storeDir, { "rollout-a.jsonl": "a" }); // current
    const orphan1 = await seedGeneration(storeDir, { "rollout-1.jsonl": "1" }, { publish: false });
    const orphan2 = await seedGeneration(storeDir, { "rollout-2.jsonl": "2" }, { publish: false });

    await writeFileAt(join(codexHome, "sessions", "rollout-new.jsonl"), '{"new":true}\n');
    await CodexSessionStore.persist(codexHome, storeDir);

    const gens = await genDirNames(storeDir);
    assert.equal(gens.length, 2, "only {new, previous}");
    assert.ok(gens.includes(genA), "previous kept");
    assert.ok(!gens.includes(orphan1) && !gens.includes(orphan2), "older orphans reaped");
  });

  it("TX: generation dirs are private (0700) and the pointer is 0600", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "a");
    await CodexSessionStore.persist(codexHome, storeDir);
    const genDir = await currentGenDir(storeDir);
    assert.equal((await fsp.stat(genDir)).mode & 0o777, 0o700, "generation dir is 0700");
    assert.equal((await fsp.stat(join(genDir, "sessions"))).mode & 0o777, 0o700, "sessions dir is 0700");
    assert.equal((await fsp.stat(join(storeDir, "current"))).mode & 0o777, 0o600, "pointer is 0600");
  });

  it("TX: an absent OR empty source PUBLISHES an empty generation (freshness: no stale resume)", async () => {
    // Established behavior: a SUCCESSFUL empty snapshot advances current to an empty generation,
    // so inspect becomes absent and a stale session cannot resume. (A FAILED persist, by contrast,
    // still preserves the old generation — see the staged-copy-failure test.)
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), '{"a":true}\n');
    await CodexSessionStore.persist(codexHome, storeDir);
    const genA = await currentGenId(storeDir);
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");

    // (a) ABSENT source: codexHome has no sessions/ at all.
    await fsp.rm(join(codexHome, "sessions"), { recursive: true, force: true });
    const r1 = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(r1.files, 0, "nothing captured");
    assert.notEqual(await currentGenId(storeDir), genA, "current advanced to a fresh empty generation");
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent", "no stale session resumes");
    assert.deepEqual(await storedSessions(storeDir), [], "the new generation is empty");
    assert.equal((await CodexSessionStore.adopt(storeDir, join(root, "fh1"))).files, 0, "adopt seeds nothing");

    // (b) EMPTY source: sessions/ exists but holds only non-allowlisted junk.
    await writeFileAt(join(codexHome, "sessions", "notes.txt"), "not a rollout");
    const genPrev = await currentGenId(storeDir);
    const r2 = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(r2.files, 0);
    assert.notEqual(await currentGenId(storeDir), genPrev, "current advanced again");
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
  });

  it("TX: the per-store lock serializes a reader with writers (blocked reader finishes before any reap)", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), "A");
    await CodexSessionStore.persist(codexHome, storeDir);
    const genA = await currentGenId(storeDir);
    assert.ok(genA, "generation A was published");

    // Block the reader's FIRST FileHandle read (its `current` pointer read) on a barrier, THEN run
    // two publishes. Under the per-store operation lock the writers CANNOT run until the reader
    // releases, so it reads + copies generation A before any reap. The proof uses an OBSERVABLE
    // filesystem seam — a writer that ENTERS its work creates a new generation dir — so it does not
    // rely on a scheduling delay: while the reader holds the lock, NO new generation appears.
    let release: () => void = () => {};
    const barrier = new Promise<void>((r) => {
      release = r;
    });
    const probe = await fsp.open(join(root, "lock-probe"), "w");
    const proto = Object.getPrototypeOf(probe) as { read: (...a: unknown[]) => unknown };
    const origRead = proto.read;
    let armed = true;
    let readBlocked = false;
    proto.read = function (this: unknown, ...args: unknown[]) {
      if (armed) {
        armed = false;
        readBlocked = true;
        return barrier.then(() => origRead.apply(this, args));
      }
      return origRead.apply(this, args);
    };
    const freshHome = join(root, "reader-home");
    await fsp.mkdir(freshHome, { recursive: true });
    let readerP: Promise<AdoptResult> = Promise.resolve({ files: -1 });
    let pubP: Promise<void> = Promise.resolve();
    try {
      readerP = CodexSessionStore.adopt(storeDir, freshHome);
      await waitFor(() => readBlocked, "the reader reached the blocked pointer read");

      pubP = (async () => {
        for (const tag of ["B", "C"]) {
          await fsp.rm(join(codexHome, "sessions"), { recursive: true, force: true });
          await writeFileAt(join(codexHome, "sessions", `rollout-${tag}.jsonl`), tag);
          await CodexSessionStore.persist(codexHome, storeDir);
        }
      })();

      // Observable seam: poll for a writer ENTERING its fs work (a new generation dir). Under the
      // lock none enters while the reader is blocked; a mutant without read-side locking creates
      // generations here. The bound only decides how long to look for that positive signal.
      let writerEntered = false;
      const deadline = Date.now() + 400;
      while (Date.now() < deadline) {
        if ((await genDirNames(storeDir)).length > 1) {
          writerEntered = true;
          break;
        }
        await delay(5);
      }
      assert.equal(writerEntered, false, "no writer entered its fs work while the reader held the lock");
      assert.deepEqual(await genDirNames(storeDir), [genA], "only generation A on disk; writers are queued");

      release();
      const adopted = await readerP;
      await pubP;
      assert.equal(adopted.files, 1, "the reader adopted generation A (writers ran only after it)");
      assert.equal(await fsp.readFile(join(freshHome, "sessions", "rollout-A.jsonl"), "utf8"), "A");
      assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-C.jsonl"], "current is C afterward");
    } finally {
      proto.read = origRead;
      release();
      await Promise.allSettled([readerP, pubP]); // no reader/publisher task escapes the test
      await probe.close();
    }
  });

  it("TX: the per-store operation-lock registry returns to zero after churn (no leaked keys)", async () => {
    assert.equal(storeLockRegistrySize(), 0, "the lock registry starts empty");
    const ops: Promise<unknown>[] = [];
    for (let i = 0; i < 24; i += 1) {
      const sd = join(root, `churn-store-${i}`);
      const ch = join(root, `churn-home-${i}`);
      await writeFileAt(join(ch, "sessions", "r.jsonl"), String(i));
      // Overlapping SAME-key ops exercise the identity-safe path: an older op's completion must not
      // delete a newer queued op's key. Each key is created then, once its ops settle, released.
      ops.push(CodexSessionStore.persist(ch, sd));
      ops.push(CodexSessionStore.inspect(sd));
      ops.push(CodexSessionStore.adopt(sd, join(root, `churn-adopt-${i}`)));
    }
    ops.push(CodexSessionStore.remove(join(root, "churn-store-0"))); // terminal path also releases its key
    await Promise.all(ops);
    // Let the identity-safe cleanup callbacks (chained after each op's tail) drain.
    await tick();
    await tick();
    assert.equal(storeLockRegistrySize(), 0, "every completed storeDir operation released its registry key");
  });

  it("TX: concurrent persists serialize and leave a single consistent current", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), "A");
    await CodexSessionStore.persist(codexHome, storeDir); // seed

    const home1 = join(root, "h1");
    const home2 = join(root, "h2");
    await writeFileAt(join(home1, "sessions", "rollout-1.jsonl"), "1");
    await writeFileAt(join(home2, "sessions", "rollout-2.jsonl"), "2");
    const [r1, r2] = await Promise.all([
      CodexSessionStore.persist(home1, storeDir),
      CodexSessionStore.persist(home2, storeDir),
    ]);
    assert.equal(r1.files, 1);
    assert.equal(r2.files, 1);

    // Serialized publishes leave current naming exactly one complete generation (no cross-reap,
    // no dangling pointer).
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
    const stored = await storedSessions(storeDir);
    assert.equal(stored.length, 1, "exactly one complete generation is current");
    assert.ok(stored[0] === "sessions/rollout-1.jsonl" || stored[0] === "sessions/rollout-2.jsonl");
    assert.equal((await CodexSessionStore.adopt(storeDir, join(root, "cc-home"))).files, 1);
  });

  it("TX: a symlinked ANCESTOR of codexHome/storeDir is refused (not only the final component)", async () => {
    // codexHome sits under a symlinked INTERMEDIATE dir: root/link -> root/real.
    const real = join(root, "real");
    await writeFileAt(join(real, "home", "sessions", "rollout-real.jsonl"), "R");
    await fsp.symlink(real, join(root, "link"));
    await assert.rejects(
      () => CodexSessionStore.persist(join(root, "link", "home"), storeDir),
      CodexSessionStoreError,
      "a symlinked codexHome ancestor is refused",
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");

    // A symlinked ANCESTOR of storeDir is refused too.
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, join(root, "link", "store")),
      CodexSessionStoreError,
    );
  });

  it("TX: a symlinked storeDir (final component) is refused; nothing is written through it", async () => {
    const realStore = join(root, "real-store");
    await fsp.mkdir(realStore, { recursive: true });
    const linkedStore = join(root, "linked-store");
    await fsp.symlink(realStore, linkedStore);
    await writeFileAt(join(codexHome, "sessions", "rollout.jsonl"), "x");

    await assert.rejects(() => CodexSessionStore.persist(codexHome, linkedStore), CodexSessionStoreError);
    assert.equal(existsSync(join(realStore, "generations")), false, "no generation written through the symlink");
    assert.equal(existsSync(join(realStore, "current")), false, "no pointer written through the symlink");
  });

  it("TX: leftover pointer temps are swept on the next persist, including an empty-source persist", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), "A");
    await CodexSessionStore.persist(codexHome, storeDir);
    await writeFileAt(join(storeDir, ".current.tmp.deadbeef01"), `${freshGenId()}\n`);
    assert.ok((await fsp.readdir(storeDir)).some((n) => n.startsWith(".current.tmp.")), "temp planted");

    // An EMPTY-source persist still performs the bounded temp sweep.
    await fsp.rm(join(codexHome, "sessions"), { recursive: true, force: true });
    await CodexSessionStore.persist(codexHome, storeDir);
    assert.ok(
      !(await fsp.readdir(storeDir)).some((n) => n.startsWith(".current.tmp.")),
      "no .current.tmp.* residue after an empty-source persist",
    );
  });

  it("TX: a genuinely unreadable pointer is UNKNOWN to inspect and fails persist closed (no reap)", async () => {
    const genA = await seedGeneration(storeDir, { "rollout-A.jsonl": "A" });
    const orphan = await seedGeneration(storeDir, { "rollout-x.jsonl": "x" }, { publish: false });
    const eacces = (): Error => Object.assign(new Error("simulated EACCES"), { code: "EACCES" });
    const store = createCodexSessionStore({ faultPointerRead: eacces });

    // Genuine I/O reading the pointer is UNKNOWN, distinct from a missing/corrupt pointer (absent).
    assert.equal(await store.inspect(storeDir), "unknown");

    // persist must NOT treat the unreadable pointer as absent and reap: it fails closed, leaving
    // BOTH generations on disk (never destroy on uncertainty).
    await writeFileAt(join(codexHome, "sessions", "rollout-new.jsonl"), "N");
    await assert.rejects(() => store.persist(codexHome, storeDir), CodexSessionStoreError);
    const gens = await genDirNames(storeDir);
    assert.ok(gens.includes(genA) && gens.includes(orphan), "no generation was reaped on an unreadable pointer");

    // A store WITHOUT the injected fault still resolves the real current generation.
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
  });

  it("TX: a staged copy failure preserves the PREVIOUS good generation, byte-for-byte", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), '{"a":true}\n');
    await CodexSessionStore.persist(codexHome, storeDir);
    const genBefore = await currentGenId(storeDir);

    // A second persist whose source now breaches a tiny cap mid-copy.
    await writeFileAt(join(codexHome, "sessions", "rollout-B.jsonl"), '{"b":true}\n');
    await writeFileAt(join(codexHome, "sessions", "rollout-C.jsonl"), '{"c":true}\n');
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFiles: 1 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      CodexSessionStoreBoundError,
    );

    assert.equal(await currentGenId(storeDir), genBefore, "the pointer was never swapped");
    assert.deepEqual(await storedSessions(storeDir), ["sessions/rollout-A.jsonl"], "old generation intact");
    assert.equal(
      await fsp.readFile(join(await currentGenDir(storeDir), "sessions", "rollout-A.jsonl"), "utf8"),
      '{"a":true}\n',
      "byte-for-byte",
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
  });

  it("TX: a successful re-persist atomically REPLACES the store (no stale merge)", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-A.jsonl"), '{"a":true}\n');
    await CodexSessionStore.persist(codexHome, storeDir);
    const gen1 = await currentGenId(storeDir);

    await fsp.rm(join(codexHome, "sessions", "rollout-A.jsonl"), { force: true });
    await writeFileAt(join(codexHome, "sessions", "rollout-B.jsonl"), '{"b":true}\n');
    const second = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(second.files, 1);

    assert.notEqual(await currentGenId(storeDir), gen1, "a new generation was published");
    assert.deepEqual(
      await storedSessions(storeDir),
      ["sessions/rollout-B.jsonl"],
      "the store reflects the latest source; the stale rollout-A is gone",
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");
  });

  it("TX: a corrupt pointer (bad grammar, oversized, or a symlink) resolves to absent", async () => {
    // Bad grammar.
    await seedGeneration(storeDir, { "rollout.jsonl": "x" }, { publish: false });
    await writeFileAt(join(storeDir, "current"), "not-a-valid-gen-id\n");
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
    assert.equal((await CodexSessionStore.adopt(storeDir, join(root, "h1"))).files, 0);

    // Oversized pointer.
    await fsp.writeFile(join(storeDir, "current"), `g${"0".repeat(24)}${"z".repeat(200)}`);
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");

    // Symlinked pointer (readCurrentGenId opens O_NOFOLLOW → refused).
    await fsp.rm(join(storeDir, "current"), { force: true });
    await fsp.writeFile(join(root, "pointer-target"), `${freshGenId()}\n`);
    await fsp.symlink(join(root, "pointer-target"), join(storeDir, "current"));
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent");
  });

  // ─── inspect / adopt / remove basics ─────────────────────────────────────────────

  it("inspect: present after persist, absent for a missing or empty store", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");

    // Missing store dir entirely.
    assert.equal(await CodexSessionStore.inspect(join(root, "nope")), "absent");

    // Store dir exists but has no pointer.
    const empty = join(root, "empty-store");
    await fsp.mkdir(empty, { recursive: true });
    assert.equal(await CodexSessionStore.inspect(empty), "absent");

    // A published generation of only non-allowlisted junk → no resumable artifact.
    const junk = join(root, "junk-store");
    await seedGeneration(junk, { "notes.txt": "not a rollout" });
    assert.equal(await CodexSessionStore.inspect(junk), "absent");
  });

  it("inspect returns unknown when its bounded scan cannot classify the current generation", async () => {
    const uncertain = join(root, "uncertain-store");
    await seedGeneration(uncertain, { "one.txt": "x", "two.txt": "x" });
    assert.equal(await CodexSessionStore.inspect(uncertain, { scanCap: 1 }), "unknown");
  });

  it("cross-worker: a non-existent store → inspect absent, adopt no-op (fresh session)", async () => {
    const otherWorkerStore = join(root, "other-worker-store");
    assert.equal(await CodexSessionStore.inspect(otherWorkerStore), "absent");

    const freshHome = join(root, "fresh-home");
    const adopted = await CodexSessionStore.adopt(otherWorkerStore, freshHome);
    assert.equal(adopted.files, 0);
    assert.equal(existsSync(join(freshHome, "sessions")), false);
  });

  it("terminal: remove deletes the store and is idempotent on a missing store", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(existsSync(storeDir), true);

    await CodexSessionStore.remove(storeDir);
    assert.equal(existsSync(storeDir), false);

    await assert.doesNotReject(CodexSessionStore.remove(storeDir));
    await assert.doesNotReject(CodexSessionStore.remove(join(root, "never-existed")));
  });

  it("corrupt store → adopt no-op and inspect absent, never throwing", async () => {
    // A store dir whose `current` names a generation that does not exist (dangling pointer).
    const corrupt = join(root, "corrupt-store");
    await fsp.mkdir(corrupt, { recursive: true });
    await writeFileAt(join(corrupt, "current"), `${freshGenId()}\n`);

    assert.equal(await CodexSessionStore.inspect(corrupt), "absent");
    const freshHome = join(root, "fresh-home");
    assert.equal((await CodexSessionStore.adopt(corrupt, freshHome)).files, 0);
    assert.equal(existsSync(join(freshHome, "sessions")), false);
  });

  it("the allowlist/denylist constants encode the credential-free contract", () => {
    assert.equal(SESSION_ALLOWED_SUBDIR, "sessions");
    assert.ok(SESSION_ALLOWED_EXTENSIONS.includes(".jsonl"));
    assert.ok(SESSION_DENY_EXTENSIONS.includes(".pem"));
    for (const deny of ["auth", "access", "token", "credential", "login", "refresh", "cache"]) {
      assert.ok(SESSION_DENY_NAME_SUBSTRINGS.includes(deny), `deny list must include ${deny}`);
    }
  });
  },
);
