import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { forgejoRepoParts, githubRepoParts, gitlabBaseUrl } from "../src/forge.js";

// The API server derives a repo's authorized API origin in SQL (canonical_forge_api_origin, migration
// 00392) and the worker derives its API base here. Both read the same fixture so the two cannot
// drift: api/internal/store/forge_api_origin_livedb_test.go runs the SQL half.

interface Case {
  name: string;
  forge_type: "github" | "gitlab" | "forgejo";
  web_url: string;
  api_origin: string;
}

const cases = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../fixtures/forge-api-origin/cases.json", import.meta.url)), "utf8"),
) as Case[];

/** The cooldown identity of an API base: scheme and host only, lowercase, no default port, no
 *  trailing dot, no path (a Forgejo subpath is part of the API base but not of the origin). */
function canonicalOrigin(apiBase: string): string {
  const u = new URL(apiBase);
  return `${u.protocol}//${u.host.replace(/\.$/, "")}`;
}

function tsApiBase(c: Case): string {
  switch (c.forge_type) {
    case "github":
      return githubRepoParts(c.web_url).apiBase;
    case "forgejo":
      return forgejoRepoParts(c.web_url).apiBase;
    default:
      return gitlabBaseUrl(c.web_url);
  }
}

describe("forge API origin derivation parity (fixtures/forge-api-origin)", () => {
  it("the fixture covers github.com, a GHES host, a forgejo subpath and a gitlab port", () => {
    const names = cases.map((c) => c.name);
    for (const want of ["github.com", "GHES host", "forgejo subpath", "gitlab port"]) {
      assert.ok(names.includes(want), `fixture lacks case ${want}`);
    }
  });

  for (const c of cases) {
    it(`${c.forge_type}: ${c.name}`, () => {
      assert.equal(canonicalOrigin(tsApiBase(c)), c.api_origin);
    });
  }
});
