import { after, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { readBoundedIndex, readSmallRegularFile } from "../src/git-index.js";

// The production `step` (GitCache.automaticPreservationRead) runs a liveness check in its finally, so a revocation
// that lands while an open is in flight throws AFTER the descriptor was acquired. The descriptor must still be closed.

type Step = <T>(action: () => Promise<T>) => Promise<T>;
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "epoch-handle-close-"));
const file = path.join(dir, "file");
fs.writeFileSync(file, "hello");

/** Counts the opens and the closes of every FileHandle the code under test acquires through fs/promises. */
function trackHandles(t: TestContext): { opens: () => number; closes: () => number } {
  const original = fsp.open.bind(fsp);
  let opens = 0, closes = 0;
  t.mock.method(fsp, "open", async (...args: Parameters<typeof original>) => {
    const handle = await original(...args);
    opens++;
    const close = handle.close.bind(handle);
    handle.close = () => { closes++; return close(); };
    return handle;
  });
  return { opens: () => opens, closes: () => closes };
}

/** A step whose liveness check fails after an action that produced an open FileHandle. */
const revokedAfterOpen: Step = async action => {
  const value = await action();
  if (typeof (value as { close?: unknown } | null)?.close === "function") throw new Error("revoked");
  return value;
};

describe("a step that throws after the open completed still closes the handle", () => {
  after(() => fs.rmSync(dir, { recursive: true, force: true }));
  it("readSmallRegularFile", async t => {
    const handles = trackHandles(t);
    await assert.rejects(readSmallRegularFile(file, 64, revokedAfterOpen), /revoked/);
    assert.equal(handles.opens(), 1);
    assert.equal(handles.closes(), 1, "the acquired descriptor was closed");
  });
  it("readBoundedIndex", async t => {
    const handles = trackHandles(t);
    await assert.rejects(readBoundedIndex(file, { maxEntries: 16, maxBytes: 4096, step: revokedAfterOpen }), /revoked/);
    assert.equal(handles.opens(), 1);
    assert.equal(handles.closes(), 1, "the acquired descriptor was closed");
  });
  it("control: an unrevoked read closes exactly once", async t => {
    const handles = trackHandles(t);
    assert.equal((await readSmallRegularFile(file, 64)).bytes.toString(), "hello");
    assert.equal(handles.opens(), 1);
    assert.equal(handles.closes(), 1);
  });
});
