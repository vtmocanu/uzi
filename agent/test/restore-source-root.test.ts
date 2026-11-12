// Issue #3644 Gate B: the clone-root identity accessor of the completion authority (restore basis `predecessor.root`).
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";
import { mock, test, type TestContext } from "node:test";
import { acquireWorkerLifetime, AuthorityRefusal, type AuthorityTransaction, withAuthorityTransaction, type WorkerLifetime } from "../src/authority-lock.js";
import { openAuthorityStore, peekSourceRootIdentity, type AuthorityStore } from "../src/completion-authority.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

interface Fixture { root: string; lifetime: WorkerLifetime }
async function fixture(t: TestContext): Promise<Fixture> {
  const root = fs.realpathSync(makeTmp(t));
  return { root, lifetime: await acquireWorkerLifetime(root, testLockOptions()) };
}
const tx = <T>(f: Fixture, fn: (t: AuthorityTransaction) => Promise<T>): Promise<T> => withAuthorityTransaction(f.lifetime, { timeoutMs: 5000 }, fn);
const open = (f: Fixture): Promise<AuthorityStore> => tx(f, t => openAuthorityStore(t, { workerToken: "root-identity-token" }));
const decimal = (p: string): { dev: string; ino: string } => { const s = fs.lstatSync(p, { bigint: true }); return { dev: s.dev.toString(), ino: s.ino.toString() }; };

/** Every call result of `fn` for the store accessor and for slotKey, as a refusal message or "ok". */
async function outcome(f: Fixture, s: AuthorityStore, p: string): Promise<{ root: string; slot: string }> {
  const verdict = async <T>(run: () => Promise<T>): Promise<string> => run().then(() => "ok", (e: unknown) => e instanceof AuthorityRefusal ? e.message : `other:${String(e)}`);
  return { root: await verdict(() => tx(f, t => s.sourceRootIdentity(t, p))), slot: await verdict(() => tx(f, t => s.slotKey(t, p))) };
}

test("the accessor returns the decimal dev/ino of the clone root", async (t) => {
  const f = await fixture(t);
  const s = await open(f);
  const clone = path.join(f.root, "runner", "repo", "issue-1.attempt-x");
  fs.mkdirSync(clone, { recursive: true });
  const want = decimal(clone);
  assert.match(want.ino, /^[0-9]+$/);
  assert.deepEqual(await tx(f, t2 => s.sourceRootIdentity(t2, clone)), want);
  assert.deepEqual(await peekSourceRootIdentity({ dataDir: f.root }, clone), want);
  // A different directory has a different identity; a renamed directory keeps its own.
  const other = path.join(f.root, "runner", "repo", "other");
  fs.mkdirSync(other);
  assert.notDeepEqual(await peekSourceRootIdentity({ dataDir: f.root }, other), want);
  const moved = path.join(f.root, "runner", "moved");
  fs.renameSync(clone, moved);
  assert.deepEqual(await peekSourceRootIdentity({ dataDir: f.root }, moved), want);
});

test("a missing final component is undefined, exactly as slotKey tolerates it", async (t) => {
  const f = await fixture(t);
  const s = await open(f);
  fs.mkdirSync(path.join(f.root, "runner"));
  const gone = path.join(f.root, "runner", "gone");
  assert.equal(await tx(f, t2 => s.sourceRootIdentity(t2, gone)), undefined);
  assert.equal(await peekSourceRootIdentity({ dataDir: f.root }, gone), undefined);
  assert.deepEqual(await outcome(f, s, gone), { root: "ok", slot: "ok" });
});

test("the accessor refuses exactly what slotKey refuses", async (t) => {
  const f = await fixture(t);
  const s = await open(f);
  fs.mkdirSync(path.join(f.root, "runner", "real"), { recursive: true });
  fs.writeFileSync(path.join(f.root, "runner", "file"), "x");
  fs.symlinkSync(path.join(f.root, "runner", "real"), path.join(f.root, "runner", "link"));
  const outside = fs.mkdtempSync(path.join(path.dirname(f.root), "outside-"));
  t.after(() => fs.rmSync(outside, { recursive: true, force: true }));
  const cases: Record<string, string> = {
    relative: "runner/real",
    "not normalized": `${f.root}/runner/../runner/real`,
    "outside the data root": outside,
    "the data root itself": f.root,
    "authority storage": path.join(f.root, ".uzi-authority", "slots"),
    "final symlink": path.join(f.root, "runner", "link"),
    "symlink ancestor": path.join(f.root, "runner", "link", "child"),
    "ancestor not a directory": path.join(f.root, "runner", "file", "child"),
    "missing ancestor": path.join(f.root, "runner", "nope", "child"),
    "NUL byte": `${f.root}/runner/re\0al`,
  };
  for (const [name, p] of Object.entries(cases)) {
    const got = await outcome(f, s, p);
    assert.notEqual(got.slot, "ok", `${name}: slotKey refuses (control)`);
    assert.equal(got.root, got.slot, `${name}: same refusal`);
  }
  await assert.rejects(peekSourceRootIdentity({ dataDir: f.root }, path.join(f.root, "runner", "link")), AuthorityRefusal);
  await assert.rejects(peekSourceRootIdentity({ dataDir: `${f.root}/` }, path.join(f.root, "runner", "real")), AuthorityRefusal);
});

test("the accessor lstats the path components only: it never opens, lists or reads the clone", async (t) => {
  const f = await fixture(t);
  const s = await open(f);
  const clone = path.join(f.root, "runner", "repo", "issue-1.attempt-x");
  fs.mkdirSync(path.join(clone, ".git"), { recursive: true });
  fs.writeFileSync(path.join(clone, "file.txt"), "secret");
  const touched: string[] = [];
  const patched = (["stat", "open", "opendir", "readdir", "readFile", "readlink", "access", "realpath"] as const).map(name => {
    const real = (fsp as unknown as Record<string, (...a: unknown[]) => unknown>)[name]!;
    return mock.method(fsp as never, name as never, ((...args: unknown[]) => {
      if (typeof args[0] === "string" && (args[0] === clone || args[0].startsWith(clone + path.sep))) touched.push(`${name}:${args[0]}`);
      return real.apply(fsp, args);
    }) as never);
  });
  const realLstat = fsp.lstat;
  const lstats: string[] = [];
  const spy = mock.method(fsp, "lstat", ((...args: Parameters<typeof fsp.lstat>) => { lstats.push(String(args[0])); return realLstat.apply(fsp, args as never); }) as never);
  syncBuiltinESMExports();
  t.after(() => { for (const p of [...patched, spy]) p.mock.restore(); syncBuiltinESMExports(); });
  assert.deepEqual(await tx(f, t2 => s.sourceRootIdentity(t2, clone)), decimal(clone));
  assert.deepEqual(await peekSourceRootIdentity({ dataDir: f.root }, clone), decimal(clone));
  assert.deepEqual(touched, []);
  assert.ok(lstats.every(p => p === clone || !p.startsWith(clone + path.sep)), `nothing below the root is lstat'd: ${lstats.join(",")}`);
  assert.ok(lstats.includes(clone));
});
