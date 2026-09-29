import { it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";

import { GitCache, ScratchPublicationError } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const SHA = "a".repeat(40);
const FLOOR = "b".repeat(40);

/** Stub only the Git reads before the bounded history walk; no repository is needed. */
function preflightCache(): GitCache {
  const cache = new GitCache("/unused-checkpoint-abort-test", nullLogger());
  const seam = cache as unknown as {
    revParse: () => Promise<string>;
    runGit: () => Promise<string>;
  };
  seam.revParse = async () => SHA;
  seam.runGit = async () => "false";
  return cache;
}

it("an aborted scratch history walk stays an abort, not a scratch refusal", async () => {
  const cache = preflightCache();
  const abort = new DOMException("checkpoint boundary stopped", "AbortError");
  const seam = cache as unknown as {
    execScoped: () => Promise<never>;
  };
  seam.execScoped = async () => { throw abort; };

  await assert.rejects(cache.scratchPublicationPreflight("/unused", "agent/issue-1914", SHA),
    (error: unknown) => error === abort && !(error instanceof ScratchPublicationError));
});

it("a plain subprocess error after the held permit aborts is classified as an abort", async () => {
  const cache = preflightCache();
  const controller = new AbortController();
  const seam = cache as unknown as { execScoped: () => Promise<never> };
  seam.execScoped = async () => {
    controller.abort();
    throw new Error("permit-held git output collection aborted: boundary deadline exceeded");
  };

  await assert.rejects(cache.withBoundaryProcessSpawner(
    async () => { throw new Error("unexpected child spawn"); },
    controller.signal,
    () => cache.scratchPublicationPreflight("/unused", "agent/issue-1914", SHA),
  ), (error: unknown) => error instanceof Error && error.name === "AbortError" &&
    !(error instanceof ScratchPublicationError));
});

it("a default-tip abort cannot turn an overlay checkpoint into a raw-tip pack", async () => {
  const cache = preflightCache();
  const abort = new DOMException("checkpoint boundary stopped", "AbortError");
  let packs = 0;
  const seam = cache as unknown as {
    trackingTip: () => Promise<string>;
    scratchPublicationPreflight: () => Promise<string>;
    checkpointExcludeRef: () => Promise<string>;
    revParse: () => Promise<string>;
    isAncestorRef: () => Promise<boolean>;
    fetchDefaultTip: () => Promise<never>;
    spawnGit: () => Promise<never>;
  };
  seam.trackingTip = async () => SHA;
  seam.scratchPublicationPreflight = async () => SHA;
  seam.checkpointExcludeRef = async () => FLOOR;
  seam.revParse = async () => FLOOR;
  seam.isAncestorRef = async () => true;
  seam.fetchDefaultTip = async () => { throw abort; };
  seam.spawnGit = async () => {
    packs += 1;
    throw new Error("raw-tip pack attempted after default-tip abort");
  };

  await assert.rejects(cache.checkpointPack("/unused", "agent/issue-1914", { defaultBranch: "main" }),
    (error: unknown) => error === abort && !(error instanceof ScratchPublicationError));
  assert.equal(packs, 0, "an aborted overlay must not create a raw-tip pack");
});

it("an abort during overlay synthesis cannot turn into a raw-tip pack", async () => {
  const cache = preflightCache();
  const abort = new DOMException("checkpoint boundary stopped", "AbortError");
  let packs = 0;
  const seam = cache as unknown as {
    trackingTip: () => Promise<string>;
    scratchPublicationPreflight: () => Promise<string>;
    checkpointExcludeRef: () => Promise<string>;
    revParse: () => Promise<string>;
    isAncestorRef: () => Promise<boolean>;
    fetchDefaultTip: () => Promise<string>;
    workflowTreeDiffers: () => Promise<boolean>;
    changedFiles: () => Promise<string[]>;
    runGitWithEnv: () => Promise<never>;
    spawnGit: () => Promise<never>;
  };
  seam.trackingTip = async () => SHA;
  seam.scratchPublicationPreflight = async () => SHA;
  seam.checkpointExcludeRef = async () => FLOOR;
  seam.revParse = async () => FLOOR;
  seam.isAncestorRef = async () => true;
  seam.fetchDefaultTip = async () => FLOOR;
  seam.workflowTreeDiffers = async () => true;
  seam.changedFiles = async () => [];
  seam.runGitWithEnv = async () => { throw abort; };
  seam.spawnGit = async () => {
    packs += 1;
    throw new Error("raw-tip pack attempted after synthesis abort");
  };

  await assert.rejects(cache.checkpointPack("/unused", "agent/issue-1914", { defaultBranch: "main" }),
    (error: unknown) => error === abort && !(error instanceof ScratchPublicationError));
  assert.equal(packs, 0);
});

it("an ordinary default-tip failure retains the existing best-effort raw-tip fallback", async () => {
  const cache = preflightCache();
  const seam = cache as unknown as {
    trackingTip: () => Promise<string>;
    scratchPublicationPreflight: () => Promise<string>;
    checkpointExcludeRef: () => Promise<string>;
    revParse: () => Promise<string>;
    isAncestorRef: () => Promise<boolean>;
    fetchDefaultTip: () => Promise<never>;
    spawnGit: () => Promise<{ stdout: Readable; exited: Promise<number> }>;
  };
  seam.trackingTip = async () => SHA;
  seam.scratchPublicationPreflight = async () => SHA;
  seam.checkpointExcludeRef = async () => FLOOR;
  seam.revParse = async () => FLOOR;
  seam.isAncestorRef = async () => true;
  seam.fetchDefaultTip = async () => { throw new Error("forge unavailable"); };
  seam.spawnGit = async () => ({ stdout: Readable.from([]), exited: Promise.resolve(0) });

  const packed = await cache.checkpointPack("/unused", "agent/issue-1914", { defaultBranch: "main" });
  assert.equal(packed?.tipOid, SHA, "only an ordinary overlay failure may use the raw tip");
});
