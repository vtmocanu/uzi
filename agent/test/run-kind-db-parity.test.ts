import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

import { RUN_KINDS } from "../src/protocol.js";

// RUN_KINDS in agent/src/protocol.ts is a hand-maintained mirror of the DB
// `runs_kind_check` CHECK constraint (latest redefinition under
// api/internal/store/migrations/). Nothing links the TypeScript literal to the
// SQL, so this test is the drift guard: it reads the live constraint out of the
// migration that last redefined it and asserts set-equality against RUN_KINDS.
// Same trick as judge-denylist-prompt.test.ts — read the other language's source
// rather than restating its values here, which would just be a third copy.
describe("RUN_KINDS matches the DB runs_kind_check constraint", () => {
  const migrationsDir = new URL("../../api/internal/store/migrations/", import.meta.url);

  /** The `runs_kind_check` allowed set, read from the highest-numbered migration
   *  whose `-- +goose Up` section redefines the constraint. */
  function dbRunKinds(): string[] {
    const files = fs
      .readdirSync(migrationsDir)
      .filter((f) => f.endsWith(".sql"));

    // Candidates: files whose Up section (everything BEFORE the first
    // `-- +goose Down`) redefines runs_kind_check. A Down-only mention (the
    // rollback) must not count as the live definition.
    const candidates = files
      .map((name) => {
        const src = fs.readFileSync(new URL(name, migrationsDir), "utf8");
        const downIdx = src.indexOf("-- +goose Down");
        const up = downIdx >= 0 ? src.slice(0, downIdx) : src;
        const num = Number.parseInt(name.match(/^(\d+)/)?.[1] ?? "", 10);
        return { name, up, num };
      })
      .filter((c) => Number.isFinite(c.num) && /runs_kind_check\b/.test(c.up));

    assert.ok(
      candidates.length > 0,
      "no migration Up section redefines runs_kind_check — did the constraint get renamed?",
    );

    // Highest-numbered such file is the live definition.
    const live = candidates.reduce((a, b) => (b.num > a.num ? b : a));

    // Anchor on the constraint name so a different CHECK in the same file cannot
    // match. `[\s\S]` so a newline between the name and CHECK (...) is fine.
    const m = live.up.match(/runs_kind_check\b[\s\S]*?CHECK\s*\(\s*kind\s+IN\s*\(([^)]*)\)/i);
    assert.ok(
      m,
      `could not extract the runs_kind_check CHECK (kind IN (...)) list from ${live.name} — did its shape change?`,
    );

    const kinds = [...m[1]!.matchAll(/'([^']+)'/g)].map((x) => x[1]!);
    assert.ok(
      kinds.length > 0,
      `runs_kind_check in ${live.name} yielded no string literals — did the regex miss the list?`,
    );
    return kinds;
  }

  it("equals the live migration's runs_kind_check set, both directions", () => {
    const db = new Set<string>(dbRunKinds());
    const ts = new Set<string>(RUN_KINDS);

    const missingFromTs = [...db].filter((k) => !ts.has(k));
    const extraInTs = [...ts].filter((k) => !db.has(k));

    assert.deepStrictEqual(
      missingFromTs,
      [],
      `DB runs_kind_check allows kinds absent from RUN_KINDS: ${missingFromTs.join(", ") || "(none)"}`,
    );
    assert.deepStrictEqual(
      extraInTs,
      [],
      `RUN_KINDS declares kinds the DB runs_kind_check does not allow: ${extraInTs.join(", ") || "(none)"}`,
    );
    assert.deepStrictEqual(
      [...ts].sort(),
      [...db].sort(),
      "RUN_KINDS has drifted from the DB runs_kind_check constraint",
    );
  });
});
