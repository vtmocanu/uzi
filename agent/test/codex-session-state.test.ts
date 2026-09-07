import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

import {
  CodexSessionStore,
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
// These tests exercise the allowlist copy, the symlink + bound safety, and the
// tri-state presence contract entirely against temp dirs — no real Codex.

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

describe("CodexSessionStore", () => {
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

  it("persist copies the sessions rollout subtree and EXCLUDES auth-shaped files", async () => {
    // A legit credential-free rollout.
    await writeFileAt(
      join(codexHome, "sessions", "rollout-2026-01-01T00-00-00-abc.jsonl"),
      '{"type":"session_meta"}\n',
    );
    // Auth material at the CODEX_HOME ROOT — outside the allowed subtree entirely.
    await writeFileAt(join(codexHome, "auth.json"), '{"tokens":{"access_token":"SECRET_ACCESS"}}');
    await writeFileAt(join(codexHome, "credentials.json"), "SECRET_ROOT_CRED");
    // Auth-shaped files placed INSIDE sessions/ — excluded by the deny substrings even
    // though they carry the allowed .jsonl extension. This is the load-bearing guard.
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

    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-2026-01-01T00-00-00-abc.jsonl"]);

    // Load-bearing: none of the auth-shaped names made it into the store.
    for (const name of [
      "auth.json",
      "credentials.json",
      "auth-token.jsonl",
      "credential-cache.jsonl",
      "model-cache.jsonl",
      "refresh.jsonl",
      "id_rsa.pem",
    ]) {
      assert.ok(!stored.some((p) => p.endsWith(name)), `${name} must NOT be in the store`);
    }
    // And no secret byte leaked into any stored file.
    for (const rel of stored) {
      const body = await fsp.readFile(join(storeDir, rel), "utf8");
      assert.doesNotMatch(body, /SECRET/);
    }
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

  it("inspect: present after persist, absent for a missing or empty store", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(await CodexSessionStore.inspect(storeDir), "present");

    // Missing store dir entirely.
    assert.equal(await CodexSessionStore.inspect(join(root, "nope")), "absent");

    // Store dir exists but holds no sessions subtree.
    const empty = join(root, "empty-store");
    await fsp.mkdir(empty, { recursive: true });
    assert.equal(await CodexSessionStore.inspect(empty), "absent");

    // sessions/ exists but holds only non-allowlisted junk → no resumable artifact.
    const junk = join(root, "junk-store");
    await writeFileAt(join(junk, "sessions", "notes.txt"), "not a rollout");
    assert.equal(await CodexSessionStore.inspect(junk), "absent");
  });

  it("cross-worker: a non-existent store → inspect absent, adopt no-op (fresh session)", async () => {
    const otherWorkerStore = join(root, "other-worker-store");
    assert.equal(await CodexSessionStore.inspect(otherWorkerStore), "absent");

    const freshHome = join(root, "fresh-home");
    const adopted = await CodexSessionStore.adopt(otherWorkerStore, freshHome);
    assert.equal(adopted.files, 0);
    // Nothing seeded — the harness starts fresh over the recovered worktree.
    assert.equal(existsSync(join(freshHome, "sessions")), false);
  });

  it("terminal: remove deletes the store and is idempotent on a missing store", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(existsSync(storeDir), true);

    await CodexSessionStore.remove(storeDir);
    assert.equal(existsSync(storeDir), false);

    // Idempotent: removing an already-gone / never-existed store never throws.
    await assert.doesNotReject(CodexSessionStore.remove(storeDir));
    await assert.doesNotReject(CodexSessionStore.remove(join(root, "never-existed")));
  });

  it("corrupt store → adopt no-op and inspect absent, never throwing", async () => {
    // sessions is a regular FILE where a directory is expected.
    const corrupt = join(root, "corrupt-store");
    await writeFileAt(join(corrupt, "sessions"), "this should have been a directory");

    assert.equal(await CodexSessionStore.inspect(corrupt), "absent");

    const freshHome = join(root, "fresh-home");
    const adopted = await CodexSessionStore.adopt(corrupt, freshHome);
    assert.equal(adopted.files, 0);
    assert.equal(existsSync(join(freshHome, "sessions")), false);
  });

  it("a symlink in the source sessions/ pointing outside is NOT copied (rejected)", async () => {
    // A secret living OUTSIDE codexHome, that a symlink would leak if followed.
    const outsideDir = join(root, "outside");
    const outsideSecret = join(outsideDir, "secret.jsonl");
    await writeFileAt(outsideSecret, "SECRET_OUTSIDE_CONTENT");

    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    // A symlink FILE with an allowlisted name, pointing at the outside secret.
    await fsp.symlink(outsideSecret, join(codexHome, "sessions", "link.jsonl"));
    // A symlinked DIRECTORY pointing outside the roots.
    await fsp.symlink(outsideDir, join(codexHome, "sessions", "linkdir"));

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "only the real rollout, never the symlink");

    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.includes("link")), "no symlinked path is copied");
    for (const rel of stored) {
      const body = await fsp.readFile(join(storeDir, rel), "utf8");
      assert.doesNotMatch(body, /SECRET_OUTSIDE_CONTENT/);
    }
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
  });

  it("bounded: persist errs on a single file over the per-file byte cap", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-big.jsonl"), "x".repeat(1024));
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFileBytes: 16 };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      CodexSessionStoreBoundError,
    );
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

  it("FIX 1: persist REFUSES a symlinked top-level sessions/ pointing at an outside dir", async () => {
    // The HIGH-1 blind spot: `$CODEX_HOME/sessions` is ITSELF a symlink to a dir OUTSIDE
    // codexHome. Pre-fix, `copyAllowlistedSessions` readdir'd the resolved path with no
    // lstat on the top-level dir, so `readdir` followed the link and copied out-of-tree
    // `.jsonl` into the runner-owned store. persist must now refuse before reading.
    const outsideDir = join(root, "outside-sessions");
    await writeFileAt(join(outsideDir, "rollout-outside.jsonl"), "SECRET_OUTSIDE_ROLLOUT");
    await fsp.mkdir(codexHome, { recursive: true });
    await fsp.symlink(outsideDir, join(codexHome, "sessions"));

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir),
      CodexSessionStoreError,
      "persist must refuse a symlinked top-level sessions/ rather than follow it",
    );
    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, [], "no out-of-tree rollout was copied into the store");
  });

  it("FIX 1: persist REFUSES a symlinked top-level sessions/ pointing at the codexHome ROOT", async () => {
    // sessions/ -> codexHome would expose the ROOT (where history.jsonl / auth.json live)
    // to the .jsonl allowlist. `history.jsonl` passes name+ext, so absent the top-level
    // guard it WOULD be copied out into the runner-owned store.
    await fsp.mkdir(codexHome, { recursive: true });
    await writeFileAt(join(codexHome, "history.jsonl"), "SECRET_ROOT_HISTORY");
    await writeFileAt(join(codexHome, "auth.json"), "SECRET_ROOT_AUTH");
    await fsp.symlink(codexHome, join(codexHome, "sessions"));

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir),
      CodexSessionStoreError,
    );
    const stored = await collectRelFiles(storeDir);
    // Empty store ⇒ neither the root history.jsonl nor auth.json (nor anything else) leaked.
    assert.deepEqual(stored, [], "history.jsonl / auth.json at the codexHome root never leaked");
  });

  it("FIX 2: copy path is bounded by a per-entry scan cap, not just by files copied", async () => {
    // Many NON-allowlisted entries: none are copyable, so the file/byte caps never trip —
    // only the per-entry scan cap bounds the walk. Pre-fix this scanned unboundedly.
    for (let i = 0; i < 40; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `junk-${i}.txt`), "x");
    }
    const tiny: SessionStoreBounds = {
      ...DEFAULT_SESSION_STORE_BOUNDS,
      maxFiles: 1,
      maxScanEntries: 5,
    };
    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError, "a bound error, not an unbounded walk");
        assert.equal(err.bound, "maxScanEntries", "the SCAN cap tripped, not maxFiles");
        return true;
      },
    );
  });

  it("FIX 3: access.jsonl under sessions/ is excluded by the deny-substring list", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await writeFileAt(join(codexHome, "sessions", "access.jsonl"), "SECRET_ACCESS");

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "access.jsonl excluded; only the real rollout copies");

    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.endsWith("access.jsonl")), "access.jsonl must not be stored");
  });

  it("FIX 3: adopt REFUSES to write through a symlinked dest sessions/", async () => {
    // A store holding a rollout to adopt.
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    await CodexSessionStore.persist(codexHome, storeDir);

    // The fresh dest root has a PLANTED symlink at sessions/ -> an outside dir. mkdir
    // (recursive) would FOLLOW it; adopt must not land the adopted file outside the root.
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
    // The symlink was replaced by a real in-tree dir that received the rollout.
    assert.equal(
      existsSync(join(freshHome, "sessions", "rollout-real.jsonl")),
      true,
      "the rollout landed in a real, in-tree sessions dir",
    );
  });

  it("FIX 3: persist wraps a raw fs error in a path-free module error", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "x");
    // storeDir pre-exists as a FILE, so persist's mkdir(storeDir, {recursive}) throws
    // EEXIST — a raw fs error that would otherwise carry a `.path`.
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

  it("FIX 3: a mid-copy file-count breach leaves NO adoptable partial store", async () => {
    // Several allowlisted rollouts; a tiny maxFiles cap breaches AFTER the first is
    // already copied. Pre-fix, persist threw but left that one file behind → inspect
    // reported "present" and adopt would resume a truncated store. The fix removes the
    // partial store in persist's catch, so nothing adoptable survives a failed persist.
    for (let i = 0; i < 3; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `rollout-${i}.jsonl`), `{"i":${i}}\n`);
    }
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxFiles: 1 };

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError, "a bound error, not an unbounded copy");
        assert.equal(err.bound, "maxFiles", "the file-count cap tripped mid-copy");
        return true;
      },
    );

    // The load-bearing FIX 3 assertion: no partial store is left behind.
    assert.equal(
      await CodexSessionStore.inspect(storeDir),
      "absent",
      "a failed persist must leave nothing a later inspect reports as present",
    );
    const freshHome = join(root, "fresh-home");
    const adopted = await CodexSessionStore.adopt(storeDir, freshHome);
    assert.equal(adopted.files, 0, "adopt finds nothing to resume after a failed persist");
    assert.equal(existsSync(join(freshHome, "sessions")), false);
  });

  it("FIX 3: a mid-copy total-bytes breach leaves NO adoptable partial store", async () => {
    // Same atomicity contract, tripped by the total-bytes cap instead: the first file
    // fits, the second overflows maxTotalBytes and throws — pre-fix leaving the first.
    for (let i = 0; i < 3; i += 1) {
      await writeFileAt(join(codexHome, "sessions", `rollout-${i}.jsonl`), "x".repeat(64));
    }
    const tiny: SessionStoreBounds = { ...DEFAULT_SESSION_STORE_BOUNDS, maxTotalBytes: 100 };

    await assert.rejects(
      () => CodexSessionStore.persist(codexHome, storeDir, { bounds: tiny }),
      (err: unknown) => {
        assert.ok(err instanceof CodexSessionStoreBoundError);
        assert.equal(err.bound, "maxTotalBytes", "the total-bytes cap tripped mid-copy");
        return true;
      },
    );
    assert.equal(await CodexSessionStore.inspect(storeDir), "absent", "no partial store survives");
  });

  it("FIX 2: a within-bounds nested tree copies with the correct files/bytes tally", async () => {
    // Functional preservation of the fd-anchored walk: nested dirs copy, non-allowlisted
    // files are skipped, and the byte tally is the sum of the ACTUAL rollout bytes read.
    await writeFileAt(join(codexHome, "sessions", "rollout-a.jsonl"), "aaaa"); // 4 bytes
    await writeFileAt(join(codexHome, "sessions", "2026", "rollout-b.jsonl"), "bbbbbb"); // 6 bytes
    await writeFileAt(join(codexHome, "sessions", "notes.txt"), "ignored"); // not allowlisted

    const result: PersistResult = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 2, "both allowlisted rollouts copied; notes.txt skipped");
    assert.equal(result.bytes, 10, "bytes tally is the sum of the actual rollout bytes");

    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/2026/rollout-b.jsonl", "sessions/rollout-a.jsonl"]);
    assert.equal(
      await fsp.readFile(join(storeDir, "sessions", "rollout-a.jsonl"), "utf8"),
      "aaaa",
    );
    assert.equal(
      await fsp.readFile(join(storeDir, "sessions", "2026", "rollout-b.jsonl"), "utf8"),
      "bbbbbb",
    );
  });

  it("FIX 2: a file over the per-file byte cap throws and leaves no store", async () => {
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

  it("FIX 1: a symlinked INTERMEDIATE directory under sessions/ is not followed", async () => {
    // A dir OUTSIDE the roots holding a rollout a symlinked subdir would leak if followed.
    const outsideDir = join(root, "outside-tree");
    await writeFileAt(join(outsideDir, "rollout-outside.jsonl"), "SECRET_OUTSIDE_TREE");
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    // sessions/subdir is a SYMLINK to that outside dir. The fd-anchored walk opens it
    // O_NOFOLLOW|O_DIRECTORY (ELOOP → skip) and never descends into the target.
    await fsp.symlink(outsideDir, join(codexHome, "sessions", "subdir"));

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "only the in-tree rollout; the symlinked subtree is skipped");

    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.includes("outside")), "no file from the symlinked subtree");
    for (const rel of stored) {
      assert.doesNotMatch(await fsp.readFile(join(storeDir, rel), "utf8"), /SECRET_OUTSIDE_TREE/);
    }
  });

  it("FIX 1: a deny-substring directory under sessions/ is not descended", async () => {
    await writeFileAt(join(codexHome, "sessions", "rollout-real.jsonl"), '{"ok":true}');
    // A dir whose NAME carries an auth-shaped substring is never descended, even though
    // it holds an otherwise-allowlisted .jsonl.
    await writeFileAt(
      join(codexHome, "sessions", "auth-cache", "rollout-inner.jsonl"),
      "SECRET_INNER",
    );

    const result = await CodexSessionStore.persist(codexHome, storeDir);
    assert.equal(result.files, 1, "the auth-cache/ dir is skipped entirely");
    const stored = await collectRelFiles(storeDir);
    assert.deepEqual(stored, ["sessions/rollout-real.jsonl"]);
    assert.ok(!stored.some((p) => p.includes("inner")), "the inner rollout never copies");
  });

  it("the allowlist/denylist constants encode the credential-free contract", () => {
    assert.equal(SESSION_ALLOWED_SUBDIR, "sessions");
    assert.ok(SESSION_ALLOWED_EXTENSIONS.includes(".jsonl"));
    assert.ok(SESSION_DENY_EXTENSIONS.includes(".pem"));
    for (const deny of ["auth", "access", "token", "credential", "login", "refresh", "cache"]) {
      assert.ok(SESSION_DENY_NAME_SUBSTRINGS.includes(deny), `deny list must include ${deny}`);
    }
  });
});
