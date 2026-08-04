import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import {
  baseCommitsMatch,
  BaseCommitDivergedError,
  evaluateBaseStaleness,
  type ExecutorFactory,
} from "../src/runner.js";
import {
  api,
  fakeGitlab,
  fx,
  gitlabClaim,
  installHarness,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// ── PRD #209 M4: the base-commit staleness guard ─────────────────────────────
//
// A SEEDED run carries the commit the user planned against (claim.planned_base_commit).
// After checkout the runner compares it to the clone's resolved base (runnerClone.baseCommit)
// with baseCommitsMatch, and on divergence either WARNS into the feed (default) or FAILS the
// run (claim.require_base_match, `--require-base` — Open Question 3). These pin the four
// behaviours the PRD names, driven through the REAL runner so the feed/state stream is the
// one the server would receive. The prefix-tolerant compare itself is unit-tested first.

describe("baseCommitsMatch (PRD #209 M4 prefix-tolerant compare)", () => {
  const full = "abc123def456abc123def456abc123def456abcd";
  it("equal SHAs match", () => {
    assert.equal(baseCommitsMatch(full, full), true);
  });
  it("an abbreviated SHA matches its full form (either side is a prefix)", () => {
    assert.equal(baseCommitsMatch("abc123d", full), true);
    assert.equal(baseCommitsMatch(full, "abc123d"), true);
  });
  it("is case-insensitive and trims surrounding whitespace", () => {
    assert.equal(baseCommitsMatch("  ABC123D  ", full), true);
  });
  it("divergent SHAs do not match", () => {
    assert.equal(baseCommitsMatch("abc123d", "def456a"), false);
    // A common prefix that then diverges is NOT a match (neither is a prefix of the other).
    assert.equal(baseCommitsMatch("abc124", "abc123def456"), false);
  });
  it("an empty side is never a match (a missing base does not match everything)", () => {
    assert.equal(baseCommitsMatch("", full), false);
    assert.equal(baseCommitsMatch(full, ""), false);
    assert.equal(baseCommitsMatch("   ", full), false);
  });
});

describe("evaluateBaseStaleness (PRD #209 M4 decision)", () => {
  const base = "abc123def456abc123def456abc123def456abcd";
  it("no planned commit ⇒ undefined (proceed silently)", () => {
    assert.equal(evaluateBaseStaleness(undefined, base, false), undefined);
    assert.equal(evaluateBaseStaleness(undefined, base, true), undefined);
  });
  it("a match ⇒ undefined even under --require-base", () => {
    assert.equal(evaluateBaseStaleness(base.slice(0, 12), base, true), undefined);
  });
  it("mismatch without --require-base ⇒ a warning naming both commits", () => {
    const warn = evaluateBaseStaleness("0000000abc", base, false);
    assert.ok(warn && warn.includes("0000000abc"), "names the planned commit");
    assert.ok(warn!.includes(base), "names the clone's base commit");
  });
  // The TYPE-pinning assertion (reviewer-m4 #2): the fail path must throw
  // BaseCommitDivergedError specifically, not a bare Error with the same message. This is
  // the M4 Case 4 contract at the unit level — the runner swallows the throw on its generic
  // failure path, so only a direct call can observe the class — and it is what justifies
  // exporting BaseCommitDivergedError.
  it("mismatch with --require-base ⇒ throws BaseCommitDivergedError (not a bare Error)", () => {
    assert.throws(
      () => evaluateBaseStaleness("0000000abc", base, true),
      (err: unknown) => {
        assert.ok(
          err instanceof BaseCommitDivergedError,
          "the fail path must throw BaseCommitDivergedError, not a plain Error",
        );
        assert.ok((err as Error).message.includes("0000000abc"), "names the planned commit");
        assert.ok((err as Error).message.includes(base), "names the clone's base commit");
        return true;
      },
    );
  });
});

describe("RunRunner — base-commit staleness guard (PRD #209 M4)", () => {
  /** A factory that records the RunContext the executor received. No per-run HOME is
   *  needed: a seeded run carries no session, so the resume preflight is a no-op. `seen`
   *  staying empty is the observable "the run never reached implement". */
  function capturingFactory(seen: RunContext[]): ExecutorFactory {
    return () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seen.push(ctx);
          return { branch: ctx.branch };
        },
      },
    });
  }

  /** The origin's current tip. For a FRESH agent branch the runner clone is cut from the
   *  default-branch tip, so this is exactly runnerClone.baseCommit — see git.ts. */
  function originHead(): string {
    return execFileSync("git", ["-C", fx.originPath, "rev-parse", "HEAD"], {
      encoding: "utf8",
    }).trim();
  }

  /** Flip the first hex char so neither string can be a prefix of the other — a
   *  guaranteed mismatch derived from the REAL base, never a hardcoded SHA that could
   *  coincidentally share a prefix. */
  function mismatchOf(base: string): string {
    return (base[0] === "0" ? "1" : "0") + base.slice(1);
  }

  const seededClaim = (
    iid: number,
    extra: Partial<Record<string, unknown>>,
  ) =>
    gitlabClaim(iid, {
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# Seeded plan\n- do it",
      session_id: null,
      ...extra,
    });

  const staleLines = (runId: string): string[] =>
    api
      .messages(runId)
      .map((m) => (typeof m.payload.text === "string" ? m.payload.text : ""))
      .filter((t) => t.includes("clone's base commit"));

  // Case 1: no planned commit ⇒ the compare is inert. The run implements and emits no
  // staleness line. This is also every ORDINARY (non-seeded) run, which never sets the field.
  it("no planned commit: proceeds silently and implements", async () => {
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const claim = seededClaim(90, { planned_base_commit: null });
    await runnerWith(capturingFactory(seen), gitlab).execute(claim);
    assert.equal(seen.length, 1, "the run implemented");
    assert.deepEqual(staleLines(claim.run_id), [], "no staleness feed line");
  });

  // Case 2: the planned commit matches the clone's base (prefix-tolerant, so a 12-char
  // abbreviation of the real base still matches its full form) ⇒ silent, implements.
  it("match: proceeds silently (prefix-tolerant) and implements", async () => {
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const base = originHead();
    const claim = seededClaim(91, { planned_base_commit: base.slice(0, 12) });
    await runnerWith(capturingFactory(seen), gitlab).execute(claim);
    assert.equal(
      seen[0]?.baseCommit,
      base,
      "sanity: the clone's base is the origin tip the planned prefix abbreviates",
    );
    assert.equal(seen.length, 1, "the run implemented");
    assert.deepEqual(staleLines(claim.run_id), [], "a match emits no staleness feed line");
  });

  // Case 3: mismatch WITHOUT --require-base ⇒ WARN into the feed (naming both commits) and
  // still implement. The default behaviour (Open Question 3).
  it("mismatch without --require-base: warns and still implements", async () => {
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const planned = mismatchOf(originHead());
    const claim = seededClaim(92, {
      planned_base_commit: planned,
      require_base_match: false,
    });
    await runnerWith(capturingFactory(seen), gitlab).execute(claim);
    assert.equal(seen.length, 1, "a warn-only divergence still implements");
    const lines = staleLines(claim.run_id);
    assert.equal(lines.length, 1, "exactly one staleness warning");
    assert.ok(lines[0]!.includes(planned), "the warning names the planned commit");
    assert.ok(
      lines[0]!.includes(seen[0]!.baseCommit!),
      "the warning names the clone's base commit",
    );
    // It must NOT have failed the run.
    const failed = api.states.filter(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.equal(failed.length, 0, "a warn-only divergence does not fail the run");
  });

  // Case 4: mismatch WITH --require-base ⇒ FAIL the run before implementing, with a reason
  // naming both commits. Refuses to implement against a diverged base.
  it("mismatch with --require-base: fails naming both commits, does not implement", async () => {
    const { gitlab } = fakeGitlab();
    const seen: RunContext[] = [];
    const base = originHead();
    const planned = mismatchOf(base);
    const claim = seededClaim(93, {
      planned_base_commit: planned,
      require_base_match: true,
    });
    await runnerWith(capturingFactory(seen), gitlab).execute(claim);
    assert.equal(seen.length, 0, "the run must NOT implement against a diverged base");
    const failed = api.states.filter(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.equal(failed.length, 1, "the run failed exactly once");
    const reason = failed[0]!.body.failure_reason ?? "";
    assert.ok(reason.includes(planned), "the failure_reason names the planned commit");
    assert.ok(reason.includes(base), "the failure_reason names the clone's base commit");
    assert.ok(
      reason.includes("--require-base"),
      "the failure_reason explains why (the --require-base flag)",
    );
  });
});
