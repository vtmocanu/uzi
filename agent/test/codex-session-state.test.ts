import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

import {
  CodexSessionStore,
  CodexSessionStoreBoundError,
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

  it("the allowlist/denylist constants encode the credential-free contract", () => {
    assert.equal(SESSION_ALLOWED_SUBDIR, "sessions");
    assert.ok(SESSION_ALLOWED_EXTENSIONS.includes(".jsonl"));
    assert.ok(SESSION_DENY_EXTENSIONS.includes(".pem"));
    for (const deny of ["auth", "token", "credential", "login", "refresh", "cache"]) {
      assert.ok(SESSION_DENY_NAME_SUBSTRINGS.includes(deny), `deny list must include ${deny}`);
    }
  });
});
