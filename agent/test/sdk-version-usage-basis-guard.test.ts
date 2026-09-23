// issue #1562 (ADR-1562): guard the assumption the `usage_basis: "session_cumulative"`
// marker rests on.
//
// The worker stamps a Claude result frame `usage_basis: "session_cumulative"` because,
// from Claude Agent SDK 0.3.277, a resumed session's result frame reports the RUNNING
// SESSION TOTAL, not just its own query() leg (ADR-1562, which amends ADR-1079). The
// server folds those frames by high-water within a session lineage. If the installed SDK
// is ever ROLLED BACK below 0.3.277 that assumption breaks — a rolled-back SDK reports
// per-leg totals again, so the marker would make the server under-count. This test fails
// loudly on such a rollback so the marker is removed (or the fold reverted) in the same
// change, rather than silently corrupting run_usage.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";

const MIN = "0.3.277";

const PKG = "@anthropic-ai/claude-agent-sdk";

/** Locate the INSTALLED SDK's package.json. The package does not expose
 *  `./package.json` in its `exports` map (require.resolve of it throws
 *  ERR_PACKAGE_PATH_NOT_EXPORTED), so resolve its entry point (which IS exported)
 *  and walk up to the package root — the first package.json whose `name` matches. */
function readInstalledSdkPkg(): { version?: string; name?: string } {
  const require = createRequire(import.meta.url);
  let dir = dirname(require.resolve(PKG));
  for (let i = 0; i < 12; i++) {
    const candidate = join(dir, "package.json");
    if (existsSync(candidate)) {
      const pkg = JSON.parse(readFileSync(candidate, "utf8")) as { version?: string; name?: string };
      if (pkg.name === PKG) return pkg;
    }
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  throw new Error(`could not locate ${PKG}'s package.json from its resolved entry`);
}

/** Parse a dotted numeric semver core (major.minor.patch) into a comparable tuple.
 *  Any prerelease/build suffix is dropped for the >= comparison. */
function parseCore(v: string): [number, number, number] {
  const core = v.split("+")[0]!.split("-")[0]!;
  const parts = core.split(".").map((n) => Number.parseInt(n, 10));
  return [parts[0] ?? 0, parts[1] ?? 0, parts[2] ?? 0];
}

function gte(a: string, b: string): boolean {
  const [a0, a1, a2] = parseCore(a);
  const [b0, b1, b2] = parseCore(b);
  if (a0 !== b0) return a0 > b0;
  if (a1 !== b1) return a1 > b1;
  return a2 >= b2;
}

describe("Claude Agent SDK version guard (issue #1562 / ADR-1562)", () => {
  it(`the INSTALLED @anthropic-ai/claude-agent-sdk is >= ${MIN}`, () => {
    const pkg = readInstalledSdkPkg();
    const version = pkg.version;
    assert.ok(typeof version === "string" && version.length > 0, "SDK package.json has a version");
    assert.ok(
      gte(version, MIN),
      `Installed @anthropic-ai/claude-agent-sdk ${version} is below ${MIN}. The ` +
        `usage_basis: "session_cumulative" marker (harness-messages.projectResult) assumes a ` +
        `resumed result frame reports the running SESSION total. Below ${MIN} the SDK reports ` +
        `PER-LEG totals (ADR-1079), so on an SDK rollback the marker MUST be removed and the ` +
        `server fold reverted to per-leg SUM (issue #1562 / ADR-1562).`,
    );
  });

  it("the version tuple parser orders correctly (self-check)", () => {
    assert.equal(gte("0.3.277", "0.3.277"), true);
    assert.equal(gte("0.3.280", "0.3.277"), true);
    assert.equal(gte("0.4.0", "0.3.277"), true);
    assert.equal(gte("1.0.0", "0.3.277"), true);
    assert.equal(gte("0.3.276", "0.3.277"), false);
    assert.equal(gte("0.2.999", "0.3.277"), false);
    assert.equal(gte("0.3.280-rc.1", "0.3.277"), true);
  });
});
