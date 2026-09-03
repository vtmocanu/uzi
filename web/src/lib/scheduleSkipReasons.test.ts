import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import type { ScheduleSkipReason } from "./api";
import { SCHEDULE_SKIP_REASON_LABELS, scheduleSkipReasonLabel } from "./scheduleSkipReasons";

// 🔴 THE CROSS-LANGUAGE GUARD (PRD #308 M3), mirroring runCredential.test.ts.
//
// The schedule skip-reason vocabulary lives in TWO places: schedsvc.SkipReason in Go (the
// authoritative source) and the ScheduleSkipReason union in TypeScript. This test parses
// the Go const literals out of skip_reason.go and asserts the TS union matches, so a Go
// reason with no TS counterpart REDDENS here rather than shipping the raw wire string to a
// user. It is one-directional in intent (Go leads, TS follows) but the equality assertion
// catches drift in either direction: a Go reason missing from TS, or a TS member the Go
// enum no longer declares.
//
// The path is resolved from the vitest process cwd (web/), NOT from this file's directory
// — readFileSync takes a cwd-relative path. runCredential.test.ts lives in this same
// web/src/lib/ directory and reads "../api/..." for exactly that reason, so "../api/..."
// resolves to the repo-root api/ here too.
const ALL_SCHEDULE_SKIP_REASONS: ScheduleSkipReason[] = [
  "not_eligible",
  "already_running",
  "description_too_large",
  "fetch_failed",
  "vault_locked",
  "self_improve_mr_cap_reached",
  "open_mr_exists",
  "schedules_paused",
];

function reasonsFromGo(): string[] {
  const path = "../api/internal/schedsvc/skip_reason.go";
  const raw = readFileSync(path, "utf8");
  // Strip //-comment lines first (defense, mirroring runCredential): the file's prose
  // names reasons and a whole-file regex could agree with itself.
  const src = raw
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("//"))
    .join("\n");
  // Anchor on the DECLARATION form `SkipXxx SkipReason = "..."` so a naive /"([a-z_]+)"/g
  // cannot falsely capture the imported "errors" package path — only the reason
  // literals match.
  return [...src.matchAll(/SkipReason\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]).sort();
}

describe("the schedule skip-reason vocabulary is one vocabulary", () => {
  it("matches schedsvc.SkipReason in Go", () => {
    const fromGo = reasonsFromGo();
    // Non-empty guard: an empty parse (moved/renamed file, changed const form) must fail
    // loudly rather than false-green against an empty TS set.
    expect(fromGo.length).toBeGreaterThan(0);
    expect(fromGo).toEqual([...ALL_SCHEDULE_SKIP_REASONS].sort());
  });
});

describe("every skip reason has a human label (PRD #308 M4)", () => {
  it("labels every reason with non-empty text, never the raw sentinel", () => {
    for (const reason of ALL_SCHEDULE_SKIP_REASONS) {
      const label = scheduleSkipReasonLabel(reason);
      expect(label.length).toBeGreaterThan(0);
      // The label is human copy, not the machine sentinel.
      expect(label).not.toBe(reason);
    }
    // No stray keys beyond the union (the Record is exhaustive by construction).
    expect(Object.keys(SCHEDULE_SKIP_REASON_LABELS).sort()).toEqual(
      [...ALL_SCHEDULE_SKIP_REASONS].sort(),
    );
  });
});
