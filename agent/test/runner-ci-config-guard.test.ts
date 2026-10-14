import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

import type { Executor } from "../src/executor.js";
import type { ClaimConfig, StateRequest } from "../src/protocol.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// Issue #2865: the worker backstop of the default-branch CI repair lane
// (claim.config.ci_config_guard). CI-config paths are string data; every repo here is a throwaway
// temp repo driven through the real git layer, and no real workflow file is ever kept.

type Step =
  | { write: string; content?: string }
  | { remove: string }
  | { move: [string, string] };

const ID = ["-c", "user.name=uzi-agent", "-c", "user.email=agent@uzi.test", "-c", "commit.gpgsign=false"];
const run = (cwd: string, args: string[]) => execFileSync("git", [...ID, ...args], { cwd, stdio: "pipe" });

function apply(cwd: string, steps: Step[]): void {
  for (const step of steps) {
    if ("write" in step) {
      const target = path.join(cwd, step.write);
      mkdirSync(path.dirname(target), { recursive: true });
      writeFileSync(target, step.content ?? "x\n", "utf8");
    } else if ("remove" in step) {
      run(cwd, ["rm", "-q", "-f", "--", step.remove]);
    } else {
      mkdirSync(path.dirname(path.join(cwd, step.move[1])), { recursive: true });
      run(cwd, ["mv", "--", step.move[0], step.move[1]]);
    }
  }
}

/** Commit `files` (path to content) on the default branch of the fixture origin. */
function seedOrigin(files: string[]): void {
  apply(fx.originPath, files.map((write) => ({ write })));
  run(fx.originPath, ["add", "-A", "-f"]);
  run(fx.originPath, ["commit", "-q", "-m", "seed protected files"]);
}

function commitExec(steps: Step[]): Executor {
  return {
    run: async (ctx) => {
      apply(ctx.worktreePath, steps);
      run(ctx.worktreePath, ["add", "-A", "-f"]);
      run(ctx.worktreePath, ["commit", "-q", "-m", "agent change"]);
      return { branch: ctx.branch };
    },
  };
}

const hasControl = (text: string): boolean =>
  [...text].some((ch) => {
    const c = ch.codePointAt(0)!;
    return c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x2028 || c === 0x2029;
  });

const GUARD: ClaimConfig = { ci_config_guard: true, ci_config_paths: ["deploy/ci.yml"] };

function guarded(iid: number, extra: Record<string, unknown> = {}) {
  return gitlabClaim(iid, { config: GUARD, ...extra });
}

function outcome(runId: string): { statuses: string[]; failed?: StateRequest } {
  const bodies = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  return { statuses: bodies.map((b) => String(b.status)), failed: bodies.find((b) => b.status === "failed") };
}

/** Make exactly one of the two guard checks see the real diff and the other see a clean one, so a
 *  refusal can only come from the check point under test. The early check is the first call. */
function realAt(t: { mock: { method: typeof import("node:test").mock.method } }, point: "early" | "final") {
  const real = git.changedFilesNoRenames.bind(git);
  let calls = 0;
  return t.mock.method(git, "changedFilesNoRenames", async (...a: Parameters<typeof real>) => {
    calls++;
    return (calls === 1) === (point === "early") ? real(...a) : [];
  });
}

function assertRefused(runId: string, label: string, expectInReason: RegExp): StateRequest {
  const { statuses, failed } = outcome(runId);
  assert.ok(!statuses.includes("completed"), `${label}: must not complete`);
  assert.ok(failed, `${label}: must fail closed`);
  const reason = failed.failure_reason ?? "";
  assert.match(reason, expectInReason, `${label}: names the offending path`);
  assert.match(reason, /reported, not pushed/, `${label}: says the change is reported, not pushed`);
  assert.equal(hasControl(reason), false, `${label}: no raw control character in the reason`);
  return failed;
}

describe("CI-config guard: early check (a human approval does not exempt it)", () => {
  const cases: Array<[string, string[], Step[], RegExp]> = [
    ["a floor path outside the claim paths", [], [{ write: ".forgejo/workflows/x.yml" }], /forgejo\/workflows\/x\.yml/],
    ["a gitea floor path outside the claim paths", [], [{ write: ".gitea/workflows/evil.yml" }], /gitea\/workflows\/evil\.yml/],
    ["a path only the claim lists", [], [{ write: "deploy/ci.yml" }], /deploy\/ci\.yml/],
    ["a deleted protected file", [".github/workflows/ci.yml"], [{ remove: ".github/workflows/ci.yml" }], /workflows\/ci\.yml/],
    ["a rename OUT of a protected directory", [".github/workflows/a.yml"], [{ move: [".github/workflows/a.yml", "moved.yml"] }], /workflows\/a\.yml/],
    ["a protected path with a trailing space", [], [{ write: ".github/workflows/t.yml " }], /workflows\/t\.yml /],
    ["a protected path with an embedded newline", [], [{ write: ".github/workflows/a\nb.yml" }], /workflows\/a\\nb\.yml/],
  ];
  let iid = 4100;
  for (const [name, seed, steps, expected] of cases) {
    for (const [point, approvals] of [["early", [false, true]], ["final", [true]]] as const) {
      it(`refuses ${name} at the ${point} check${point === "early" ? ", human-approved and not" : ""}`, async (t) => {
        if (seed.length > 0) seedOrigin(seed);
        for (const autoApprove of approvals) {
          const { gitlab, calls } = fakeGitlab();
          realAt(t, point);
          const push = t.mock.method(git, "pushBranch");
          const claim = guarded(iid++, { auto_approve: autoApprove });
          await runner(commitExec(steps), gitlab).execute(claim);
          assertRefused(claim.run_id, `${name} (${point}, auto_approve=${autoApprove})`, expected);
          assert.equal(push.mock.callCount(), 0, "nothing is pushed");
          assert.equal(calls.length, 0, "no MR is opened");
          t.mock.restoreAll();
        }
      });
    }
  }

  it("a clean diff pushes, and the guard checks it again before the push", async (t) => {
    const { gitlab, calls } = fakeGitlab();
    const check = t.mock.method(git, "changedFilesNoRenames");
    const push = t.mock.method(git, "pushBranch");
    const claim = guarded(4150);
    await runner(commitExec([{ write: "src/ok.ts" }]), gitlab).execute(claim);
    assert.ok(outcome(claim.run_id).statuses.includes("completed"));
    assert.equal(push.mock.callCount(), 1);
    assert.equal(calls.length, 1);
    assert.ok(check.mock.callCount() >= 2, `early and final-push checks both ran, got ${check.mock.callCount()}`);
  });

  it("an unguarded run is unaffected by a CI-config change", async (t) => {
    const { gitlab } = fakeGitlab();
    const check = t.mock.method(git, "changedFilesNoRenames");
    const claim = gitlabClaim(4151, { config: { ci_config_paths: ["deploy/ci.yml"] } });
    await runner(commitExec([{ write: ".forgejo/workflows/x.yml" }, { write: "deploy/ci.yml" }]), gitlab).execute(claim);
    assert.ok(outcome(claim.run_id).statuses.includes("completed"));
    assert.equal(check.mock.callCount(), 0, "the guarded diff is never computed for an unguarded run");
  });

  it("an uncomputable diff refuses at the early check", async (t) => {
    const { gitlab } = fakeGitlab();
    t.mock.method(git, "changedFilesNoRenames", async () => null);
    const push = t.mock.method(git, "pushBranch");
    const claim = guarded(4152);
    await runner(commitExec([{ write: "src/ok.ts" }]), gitlab).execute(claim);
    assertRefused(claim.run_id, "null diff", /could not compute the diff/);
    assert.equal(push.mock.callCount(), 0);
  });
});

describe("CI-config guard: final push check", () => {
  /** The early check passes (the real, clean diff); every later check sees `later`. */
  function laterCandidate(t: { mock: { method: typeof import("node:test").mock.method } }, later: string[] | null) {
    const real = git.changedFilesNoRenames.bind(git);
    let calls = 0;
    return t.mock.method(git, "changedFilesNoRenames", async (...a: Parameters<typeof real>) => {
      calls++;
      return calls === 1 ? real(...a) : later;
    });
  }

  it("refuses when the first check passed but the candidate pushed touches a protected path", async (t) => {
    const { gitlab, calls } = fakeGitlab();
    const check = laterCandidate(t, [".forgejo/workflows/x.yml"]);
    const push = t.mock.method(git, "pushBranch");
    const claim = guarded(4160);
    await runner(commitExec([{ write: "src/ok.ts" }]), gitlab).execute(claim);
    assertRefused(claim.run_id, "final push", /forgejo\/workflows\/x\.yml/);
    assert.ok(check.mock.callCount() >= 2);
    assert.equal(push.mock.callCount(), 0, "the last gate sits before the push");
    assert.equal(calls.length, 0);
  });

  it("an uncomputable diff refuses at the final push check", async (t) => {
    const { gitlab } = fakeGitlab();
    laterCandidate(t, null);
    const push = t.mock.method(git, "pushBranch");
    const claim = guarded(4161);
    await runner(commitExec([{ write: "src/ok.ts" }]), gitlab).execute(claim);
    assertRefused(claim.run_id, "final push null diff", /could not compute the diff/);
    assert.equal(push.mock.callCount(), 0);
  });

  it("re-checks on a retried push and a refusal on the retry is never retried again", async (t) => {
    const { gitlab } = fakeGitlab();
    const real = git.changedFilesNoRenames.bind(git);
    let checks = 0;
    const check = t.mock.method(git, "changedFilesNoRenames", async (...a: Parameters<typeof real>) => {
      checks++;
      return checks <= 2 ? real(...a) : [".github/workflows/late.yml"];
    });
    let pushes = 0;
    t.mock.method(git, "pushBranch", async () => {
      pushes++;
      throw new Error("fatal: unable to access remote: Connection reset by peer");
    });
    const claim = guarded(4162);
    await runner(commitExec([{ write: "src/ok.ts" }]), gitlab).execute(claim);
    assertRefused(claim.run_id, "retry", /workflows\/late\.yml/);
    assert.equal(pushes, 1, "one transient failure, then the retry's check refused before pushing");
    assert.equal(check.mock.callCount(), 3, "early check, first attempt, retry attempt");
  });
});

describe("CI-config guard: patch disclosure", () => {
  it("attaches the patch only when the secret scan is trusted and clean", async (t) => {
    const cases: Array<[string, { trusted: boolean; findings: unknown[] }, boolean]> = [
      ["trusted and clean", { trusted: true, findings: [] }, true],
      ["untrusted", { trusted: false, findings: [] }, false],
      ["trusted with a finding", { trusted: true, findings: [{}] }, false],
    ];
    let iid = 4170;
    for (const [name, scan, expectPatch] of cases) {
      const { gitlab } = fakeGitlab();
      t.mock.method(git, "scanPatchForSecrets", async () => scan as never);
      const claim = guarded(iid++);
      await runner(commitExec([{ write: ".forgejo/workflows/x.yml", content: "name: ci\n" }]), gitlab).execute(claim);
      const failed = assertRefused(claim.run_id, name, /forgejo\/workflows\/x\.yml/);
      if (expectPatch) {
        assert.match(failed.preserved_patch ?? "", /\.forgejo\/workflows\/x\.yml/, name);
      } else {
        assert.equal(failed.preserved_patch, undefined, `${name}: the patch is withheld`);
      }
      t.mock.restoreAll();
    }
  });
});

describe("CI-config guard: single publication path", () => {
  it("every push, including the align and bridge paths, goes through the one guarded call site", async () => {
    const { readFileSync } = await import("node:fs");
    const src = readFileSync(path.join(import.meta.dirname, "..", "src", "runner.ts"), "utf8");
    const pushes = [...src.matchAll(/this\.git\.pushBranch\(/g)];
    assert.equal(pushes.length, 1, "runner.ts has exactly one pushBranch call site");
    const guardAt = src.indexOf("refuseOnCIConfigGuard(finalizeBarePath, sourceSha)");
    assert.ok(guardAt > 0, "the final-push guard is present");
    const between = src.slice(guardAt, pushes[0]!.index);
    assert.ok(between.length < 400 && !between.includes("pushToOrigin"), "the guard sits immediately before the push, in the same closure");
    assert.ok((src.match(/await pushToOrigin\(\)/g) ?? []).length >= 2, "the align/bridge path and the normal path both call pushToOrigin");
  });
});
