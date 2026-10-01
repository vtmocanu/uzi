import { it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";

import { GitCache, ScratchPublicationError } from "../src/git.js";
import { nullLogger } from "./helpers.js";

const SHA = "a".repeat(40);
const FLOOR = "b".repeat(40);

function safeAbort(error: unknown, cause: Error): boolean {
  return error instanceof Error && error.name === "AbortError" &&
    !(error instanceof ScratchPublicationError) && !error.message.includes(cause.message) &&
    error.cause === cause;
}

/** Stub execScoped so the strict tip resolve and shallow probe pass and the first
 *  bounded history walk (`rev-list`) reaches `walk`; no repository is needed. */
function preflightCache(walk?: () => Promise<never>): GitCache {
  const cache = new GitCache("/unused-checkpoint-abort-test", nullLogger());
  const seam = cache as unknown as {
    execScoped: (command: string, args: string[]) => Promise<{ stdout: string; stderr: string }>;
  };
  seam.execScoped = async (_command, args) => {
    if (args.includes("--is-shallow-repository")) return { stdout: "false\n", stderr: "" };
    if (args.includes("rev-list") && walk) return walk();
    return { stdout: `${SHA}\n`, stderr: "" };
  };
  return cache;
}

it("an aborted scratch history walk stays an abort, not a scratch refusal", async () => {
  const abort = new DOMException("checkpoint boundary stopped", "AbortError");
  const cache = preflightCache(async () => { throw abort; });

  await assert.rejects(cache.scratchPublicationPreflight("/unused", "agent/issue-1914", SHA),
    (error: unknown) => safeAbort(error, abort));
});

it("a plain subprocess error after the held permit aborts is classified as an abort", async () => {
  const controller = new AbortController();
  const remote = new Error("attacker-controlled git response");
  const cache = preflightCache(async () => {
    controller.abort();
    throw remote;
  });

  await assert.rejects(cache.withBoundaryProcessSpawner(
    async () => { throw new Error("unexpected child spawn"); },
    controller.signal,
    () => cache.scratchPublicationPreflight("/unused", "agent/issue-1914", SHA),
  ), (error: unknown) => safeAbort(error, remote));
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
    (error: unknown) => safeAbort(error, abort));
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
    (error: unknown) => safeAbort(error, abort));
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
