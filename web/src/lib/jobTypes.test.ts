import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

import { JOB_TYPES, jobTypeLabel } from "./jobTypes";

// The job-type contract (PRD #1908). runkind.JobTypes() in Go is the authoritative list
// (itself pinned to the DB CHECKs by a live-DB test); JOB_TYPES is the web mirror the admin
// allow-list checkboxes are built from. This test parses the Go declarations and asserts
// the web list equals JobTypes()'s return, in order, so a server-side type with no checkbox
// reddens here. The path is cwd-relative (vitest runs from web/), like
// scheduleSkipReasons.test.ts.
function jobTypesFromGo(): string[] {
  const path = "../api/internal/runkind/runkind.go";
  let raw: string;
  try {
    raw = readFileSync(path, "utf8");
  } catch (err) {
    throw new Error(`${path} unreadable: ${String(err)} -- this contract asserts nothing without it`);
  }
  // Drop //-comment lines so prose naming a type cannot satisfy the regexes.
  const src = raw
    .split("\n")
    .filter((line) => !line.trimStart().startsWith("//"))
    .join("\n");
  const consts = new Map<string, string>();
  for (const m of src.matchAll(/\b(JobType\w+)\s*=\s*"([a-z_]+)"/g)) consts.set(m[1], m[2]);
  const body = /func JobTypes\(\) \[\]string \{\s*return \[\]string\{([^}]*)\}/.exec(src);
  if (!body) throw new Error("JobTypes() not found in runkind.go; update this parser");
  return body[1]
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .map((id) => {
      const v = consts.get(id);
      if (v === undefined) throw new Error(`JobTypes() names ${id}, which has no string constant`);
      return v;
    });
}

describe("job-type contract", () => {
  it("JOB_TYPES mirrors runkind.JobTypes(), in order", () => {
    const goTypes = jobTypesFromGo();
    // Positive control: the parser found at least one type, so an empty-vs-empty pass is impossible.
    expect(goTypes.length).toBeGreaterThan(0);
    expect([...JOB_TYPES]).toEqual(goTypes);
  });

  it("labels a known type and degrades an unknown one to readable text", () => {
    expect(jobTypeLabel("research")).toBe("Research");
    expect(jobTypeLabel("code_audit")).toBe("code audit");
  });
});
