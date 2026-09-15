import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { recordingLogger } from "./helpers.js";
import { GitCache } from "../src/git.js";

// PRD #1392 M3 (agent half) — RESUME warmth acceptance.
//
// A forge-parked run promotes back to `queued` keeping its worker_id, and on RESUME the
// worker re-runs `ensureClone` (agent/src/git.ts). Placement (which worker) and WARMTH
// (fetch vs clone) are INDEPENDENT (PRD facts 10/11, D5): `ensureClone` warm-FETCHES when
// the bare repo already exists and cold-CLONES when it does not, and a failed cold
// `cloneBare` removes its partial bare (its catch), so the OWNER is COLD after a failed
// first clone while a SIBLING may be WARM from another run of the same repo.
//
// These real-git tests prove each warmth path a resume relies on — and that each one then
// reaches the runner-clone step (`runnerCloneForClaim` → `createOrAttachRunnerClone`, the
// local `--shared` clone, runner.ts ~5807), which a resume only reaches AFTER `ensureClone`
// succeeds. Each assertion is genuine (path taken, content checked out, directory absent),
// never a bare "no throw".
//
// Which branch of `ensureClone` ran is observed DIRECTLY: the two branches emit distinct,
// mutually-exclusive log lines (git.ts:522 vs :537), so a recording logger tells fetch from
// clone without inferring it from a side effect.
const FETCH_MSG = "repo cache: fetching"; // git.ts:522 — the warm branch
const CLONE_MSG = "repo cache: cloning bare"; // git.ts:537 — the cold branch

const msgs = (lines: unknown[]): string[] => lines.map((l) => (l as { msg?: string }).msg ?? "");

let fx: Fixture;

beforeEach(() => {
  fx = makeFixture();
});

afterEach(() => fx.cleanup());

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}

describe("ensureClone resume warmth (PRD #1392 M3 agent)", () => {
  it("WARM resume: bare already present ⇒ ensureClone FETCHES (no re-clone), yielding a bare a runner clone checks out", async () => {
    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger);

    // The FIRST claim has no bare yet, so it cold-clones one — this is the sibling/owner
    // that leaves a WARM bare behind for the resume.
    const bare = await git.ensureClone(fx.originPath);
    assert.ok(msgs(lines).includes(CLONE_MSG), "precondition: the first ensureClone cold-cloned the bare");
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true, "precondition: the bare exists after the first clone");

    // Drop a sentinel file INSIDE the bare. A cold re-clone would refuse (`git clone --bare`
    // into a non-empty dir) or wipe the dir first; only a warm fetch reuses the dir in place.
    // So the sentinel surviving is a git-independent proof the bare was NOT re-created — it
    // backs up the log-line assertion below rather than relying on it alone.
    const sentinel = path.join(bare, "uzi-1392-warm-resume-sentinel");
    fs.writeFileSync(sentinel, "resume marker\n");
    const beforeResume = lines.length;

    // THE RESUME: the promoted run re-runs ensureClone. The bare is present ⇒ FETCH path.
    const bareAgain = await git.ensureClone(fx.originPath);
    const resumeMsgs = msgs(lines.slice(beforeResume));

    assert.strictEqual(bareAgain, bare, "warm resume returns the same bare path");
    assert.ok(resumeMsgs.includes(FETCH_MSG), "resume took the FETCH path (bare present)");
    assert.ok(!resumeMsgs.includes(CLONE_MSG), "resume did NOT re-clone the bare");
    assert.strictEqual(fs.existsSync(sentinel), true, "the warm bare was refreshed in place, never re-created");

    // The resume then reaches the runner-clone step: the local `--shared` clone off the WARM
    // bare (the step runnerCloneForClaim performs) checks out real origin content.
    const rc = await git.createOrAttachRunnerClone(bare, 1392);
    assert.strictEqual(rc.branch, "agent/issue-1392");
    assert.strictEqual(
      fs.existsSync(path.join(rc.path, "README.md")),
      true,
      "runner clone off the warm bare checked out origin content",
    );
    assert.strictEqual(fs.statSync(path.join(rc.path, ".git")).isDirectory(), true, "the runner clone is a real clone (its own object store)");
    assert.strictEqual(rc.baseCommit, gitIn(rc.path, ["rev-parse", "HEAD"]), "the checked-out tip is the reported base commit");
  });

  it("COLD resume: NO bare present ⇒ ensureClone CLONES, creates the bare, and a runner clone off it works", async () => {
    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger);
    const barePath = git.barePathFor(fx.originPath);

    // The COLD resume precondition: no bare (the owner is cold after a failed first clone,
    // per the failed-clone-cleanup test below).
    assert.strictEqual(fs.existsSync(barePath), false, "precondition: no bare present");

    const bare = await git.ensureClone(fx.originPath);

    assert.strictEqual(bare, barePath, "ensureClone returns the canonical bare path");
    assert.ok(msgs(lines).includes(CLONE_MSG), "cold resume took the CLONE path");
    assert.ok(!msgs(lines).includes(FETCH_MSG), "cold resume did NOT fetch — there was no bare to fetch into");
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true, "the cold clone created the bare");

    // …and the resume reaches the runner-clone step off the freshly cold-cloned bare.
    const rc = await git.createOrAttachRunnerClone(bare, 1392);
    assert.strictEqual(rc.branch, "agent/issue-1392");
    assert.strictEqual(
      fs.existsSync(path.join(rc.path, "README.md")),
      true,
      "runner clone off the freshly cold-cloned bare checked out origin content",
    );
    assert.match(rc.baseCommit, /^[0-9a-f]{40}$/, "baseCommit is a full resolved SHA");
  });

  it("a failed first COLD clone leaves NO bare at the path, so the resume re-detects 'no bare ⇒ cold clone' rather than a corrupt warm-fetch", async () => {
    // The partial-bare CLEANUP mechanism itself — a mid-clone NETWORK failure where git DID
    // lay down a partial bare, which cloneBare's catch then removes — is already proven by
    // the closed-port test in git.test.ts ("retries a failed-to-connect bare clone and
    // cleans up the dest on give-up", git.ts:2705-2725). This test does NOT duplicate that;
    // it adds the missing RESUME-framing on the SAME repo URL: a failed first clone leaves no
    // bare at the path, so the promoted run's ensureClone re-detects cold and re-clones
    // cleanly instead of warm-fetching a corrupt/absent bare.
    //
    // No sleeps: an invalid origin is classified permanent (fail-fast, one attempt), and an
    // empty schedule makes that explicit and instant regardless.
    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger, { schedule: [], sleep: async () => {} });
    const barePath = git.barePathFor(fx.originPath);

    // Make the SAME repo URL temporarily unreachable by moving its origin aside — the
    // hermetic model of a forge blip at the very first clone. The bare-dir name is derived
    // from the URL string, so barePath is identical across the failure and the retry.
    const hidden = `${fx.originPath}.away`;
    fs.renameSync(fx.originPath, hidden);

    // The first cold clone fails (invalid origin ⇒ git creates no dest here).
    await assert.rejects(git.ensureClone(fx.originPath), /does not exist|not a git repository|unable to access|Could not/i);
    assert.ok(msgs(lines).includes(CLONE_MSG), "the failing attempt took the COLD clone branch");
    assert.strictEqual(
      fs.existsSync(barePath),
      false,
      "no bare exists at the path after the failed cold clone — the owner is COLD, nothing to warm-fetch",
    );

    // The run promotes and resumes with the forge reachable again. Because no bare exists,
    // ensureClone MUST re-detect 'no bare' and cold-clone — never warm-fetch a corrupt bare.
    fs.renameSync(hidden, fx.originPath);
    const resumeStart = lines.length;
    const bare = await git.ensureClone(fx.originPath);
    const resumeMsgs = msgs(lines.slice(resumeStart));

    assert.strictEqual(bare, barePath);
    assert.ok(resumeMsgs.includes(CLONE_MSG), "the resume re-detected 'no bare' and cold-cloned");
    assert.ok(!resumeMsgs.includes(FETCH_MSG), "the resume did NOT warm-fetch a (non-existent, would-be-corrupt) bare");
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true, "the resume's cold clone created a healthy bare");

    // …and that healthy bare carries out the resume to the runner-clone step.
    const rc = await git.createOrAttachRunnerClone(bare, 1392);
    assert.strictEqual(
      fs.existsSync(path.join(rc.path, "README.md")),
      true,
      "runner clone off the re-cloned bare checked out origin content",
    );
  });
});
