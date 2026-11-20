// In-container half of the refusal producer teardown proof (issue #4481, plan m3).
//
// refusal-producer-teardown-real.test.mts drives this file. With no argument it is the WORKER: the test
// bind-mounts a one-line `src/main.ts` over the image's, so the shipped entrypoint chain (uzi-entrypoint,
// tini PID 1, `npm run start`, tsx, node) starts it. The worker builds refusal state at the owed cap in a bare
// repository on the /data volume, admits generation G through the real GitCache.admitRefusalProvenance, then
// starts two delayed trusted producers (`git update-ref refs/uzi-incoming/<run>/<G>/<id> <divergent>`): one in
// the worker's own process group, one in a new session (detached, the `setsid` shape). It prints one
// `READY {json}` line and then idles until the test kills it.
//   gate [nonce]   a successor process: the real GitCache.refusalReleaseGate (optionally under a given
//                  process nonce, the positive control) for the worker's run, generation and head
//   refs           the staged and control refs currently in the bare repository
import { spawn, spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { GitCache, WORKER_PROCESS_NONCE } from "../../agent/src/git.js";
import type { Logger } from "../../agent/src/log.js";

const DATA_DIR = process.env.RT_DATA_DIR ?? "/data";
const BARE = path.join(DATA_DIR, "bare.git");
const RUN = "run-4481-teardown";
const GENERATION = 1;
const BRANCH = "agent/issue-4481";
const CAP = 2;
const EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904";

function git(args: string[]): string {
  const r = spawnSync("git", ["--git-dir", BARE, ...args], {
    encoding: "utf8",
    env: { ...process.env, GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@t", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@t" },
  });
  if (r.status !== 0) throw new Error(`git ${args.join(" ")}: ${r.stderr}`);
  return r.stdout.trim();
}
const commit = (message: string, parent?: string): string =>
  git(["commit-tree", EMPTY_TREE, ...(parent ? ["-p", parent] : []), "-m", message]);
function cache(processNonce?: string): GitCache {
  const self: Logger = {
    debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
    redactDiagnostic: (text: string) => text, child: () => self,
  } as unknown as Logger;
  return new GitCache(DATA_DIR, self, undefined, { maxOwedCandidates: CAP, ...(processNonce ? { processNonce } : {}) });
}
function stagedRefs(): string {
  return git(["for-each-ref", "--format=%(refname)", "refs/uzi-incoming/", "refs/uzi-control/"]);
}

const [command, a, b] = process.argv.slice(2);
if (command === undefined) {
  fs.rmSync(BARE, { recursive: true, force: true });
  git(["init", "--bare", "-q"]);
  // Two kept (pinned) heads fill the owed cap, so the gate is armed; the head covers both. The divergent
  // commit is what a refusal would stage: it is not an ancestor of the head.
  const k1 = commit("kept-1");
  const k2 = commit("kept-2", k1);
  const head = commit("final-head", k2);
  const divergent = commit("divergent-refused");
  for (const sha of [k1, k2]) git(["update-ref", `refs/uzi-owed/${RUN}/${sha}`, sha]);
  const ctx = {
    runId: RUN, generation: GENERATION, branch: BRANCH, kind: "issue" as const, barePath: BARE,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: k1 },
  };
  const worker = cache();
  const admitted = await worker.admitRefusalProvenance(ctx);
  const gateSame = await worker.refusalReleaseGate(BARE, RUN, GENERATION, head);

  // Whole seconds: the image's busybox `date` has no %N.
  const deadlineSec = Math.ceil(Date.now() / 1000) + Number(process.env.RT_DELAY_SECONDS ?? "40");
  const deadlineMs = deadlineSec * 1000;
  const producer = (ref: string, detached: boolean) => {
    const script = `RT4481_PRODUCER=${ref}; until [ "$(date +%s)" -ge ${deadlineSec} ]; do sleep 0.2; done; ` +
      `git --git-dir=${BARE} update-ref ${ref} ${divergent}`;
    const child = spawn("sh", ["-c", script], { detached, stdio: "ignore" });
    child.unref();
    return { ref, pid: child.pid, detached };
  };
  const inGroup = `refs/uzi-incoming/${RUN}/${GENERATION}/${randomUUID()}`;
  const outOfGroup = `refs/uzi-incoming/${RUN}/${GENERATION}/${randomUUID()}`;
  const producers = [producer(inGroup, false), producer(outOfGroup, true)];
  // Positive control: the same mechanism, short delay, outside the gate's namespace, lands in a live container.
  const controlRef = `refs/uzi-control/${randomUUID()}`;
  const controlSec = Math.ceil(Date.now() / 1000) + 3;
  const controlDeadline = controlSec * 1000;
  spawn("sh", ["-c", `until [ "$(date +%s)" -ge ${controlSec} ]; do sleep 0.2; done; git --git-dir=${BARE} update-ref ${controlRef} ${divergent}`],
    { detached: true, stdio: "ignore" }).unref();
  while (Date.now() < controlDeadline + 1500) await new Promise((r) => setTimeout(r, 100));
  const controlLanded = spawnSync("git", ["--git-dir", BARE, "rev-parse", "--verify", "-q", controlRef], { encoding: "utf8" }).stdout.trim() === divergent;

  console.log("READY " + JSON.stringify({
    pid: process.pid, ppid: process.ppid, nonce: WORKER_PROCESS_NONCE, runId: RUN, generation: GENERATION,
    head, divergent, admitted, gateSame, deadlineMs, producers, controlRef, controlLanded,
    stagedBefore: stagedRefs(), at: Date.now(),
  }));
  setInterval(() => {}, 60_000);
} else if (command === "gate") {
  // The head commit is dangling, so the test passes in the sha the worker printed.
  const target = b;
  if (a === undefined || target === undefined) throw new Error("usage: gate <nonce|-> <head>");
  const gate = await cache(a === "-" ? undefined : a).refusalReleaseGate(BARE, RUN, GENERATION, target);
  console.log(JSON.stringify({ gate, at: Date.now(), nonce: a === "-" ? WORKER_PROCESS_NONCE : a }));
} else if (command === "refs") {
  console.log(JSON.stringify({ refs: stagedRefs(), at: Date.now() }));
} else {
  throw new Error("usage: (worker, no args) | gate <nonce|-> <head> | refs");
}
