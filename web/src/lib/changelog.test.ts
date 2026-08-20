import { describe, expect, it } from "vitest";
import { parseChangelog, type Release } from "./changelog";

// Small inline fixtures only — NOT the real CHANGELOG.md. The byte-parity check
// against scripts/changelog-section.sh is a separate M4 deliverable.

function byVersion(releases: Release[], version: string): Release {
  const r = releases.find((x) => x.version === version);
  if (!r) throw new Error(`no release ${version} in [${releases.map((x) => x.version).join(", ")}]`);
  return r;
}

describe("parseChangelog — section boundaries", () => {
  it("splits two adjacent versions and excludes both headings from each body", () => {
    const raw = [
      "## [0.2.0] - 2026-08-10",
      "",
      "### Added",
      "- second thing",
      "",
      "## [0.1.0] - 2026-08-01",
      "",
      "### Added",
      "- first thing",
      "",
    ].join("\n");

    const releases = parseChangelog(raw);
    expect(releases.map((r) => r.version)).toEqual(["0.2.0", "0.1.0"]);

    const newer = byVersion(releases, "0.2.0");
    expect(newer.body).toBe("### Added\n- second thing");
    expect(newer.body).not.toContain("## [0.2.0]");
    expect(newer.body).not.toContain("## [0.1.0]");
    expect(newer.date).toBe("2026-08-10");
    expect(newer.released).toBe(true);

    const older = byVersion(releases, "0.1.0");
    expect(older.body).toBe("### Added\n- first thing");
    expect(older.body).not.toContain("## [0.1.0]");
  });
});

describe("parseChangelog — release-title marker", () => {
  it("strips the marker line from body and surfaces it as titleMarker", () => {
    const raw = [
      "## [0.3.0] - 2026-08-15",
      "<!-- release-title:  a terse summary  -->",
      "",
      "### Changed",
      "- tweaked a thing",
      "",
    ].join("\n");

    const r = byVersion(parseChangelog(raw), "0.3.0");
    expect(r.titleMarker).toBe("a terse summary");
    expect(r.body).toBe("### Changed\n- tweaked a thing");
    expect(r.body).not.toContain("release-title");
  });

  it("leaves titleMarker undefined when there is no marker", () => {
    const raw = ["## [0.3.0] - 2026-08-15", "", "### Added", "- x", ""].join("\n");
    const r = byVersion(parseChangelog(raw), "0.3.0");
    expect(r.titleMarker).toBeUndefined();
  });
});

describe("parseChangelog — blank-line trimming", () => {
  it("trims leading and trailing blanks but keeps interior blank lines", () => {
    const raw = [
      "## [0.4.0] - 2026-08-16",
      "",
      "",
      "### Added",
      "- a",
      "",
      "### Fixed",
      "- b",
      "",
      "",
      "## [0.3.0] - 2026-08-15",
      "",
      "### Added",
      "- older",
    ].join("\n");

    const r = byVersion(parseChangelog(raw), "0.4.0");
    // Interior blank between the two subsections is preserved exactly; the
    // leading and trailing blanks are gone.
    expect(r.body).toBe("### Added\n- a\n\n### Fixed\n- b");
  });
});

describe("parseChangelog — oldest section sweeps the footer into its body", () => {
  it("keeps trailing reference-link footer lines in the last section's body (parity)", () => {
    const raw = [
      "## [0.2.0] - 2026-08-10",
      "",
      "### Added",
      "- new",
      "",
      "## [0.1.0] - 2026-08-01",
      "",
      "### Added",
      "- first",
      "",
      "[Unreleased]: https://example.test/compare/v0.2.0...HEAD",
      "[0.2.0]: https://example.test/compare/v0.1.0...v0.2.0",
      "[0.1.0]: https://example.test/releases/tag/v0.1.0",
    ].join("\n");

    const releases = parseChangelog(raw);
    const oldest = byVersion(releases, "0.1.0");
    // The footer block with no following heading is swept into the oldest body.
    expect(oldest.body).toContain("[0.1.0]: https://example.test/releases/tag/v0.1.0");
    expect(oldest.body).toContain("[Unreleased]: https://example.test/compare/v0.2.0...HEAD");
    expect(oldest.body).toBe(
      [
        "### Added",
        "- first",
        "",
        "[Unreleased]: https://example.test/compare/v0.2.0...HEAD",
        "[0.2.0]: https://example.test/compare/v0.1.0...v0.2.0",
        "[0.1.0]: https://example.test/releases/tag/v0.1.0",
      ].join("\n"),
    );

    // The NEWER section is unaffected — no footer leaked upward.
    const newer = byVersion(releases, "0.2.0");
    expect(newer.body).toBe("### Added\n- new");
    expect(newer.groups.find((g) => g.category === "Added")?.bullets).toEqual(["new"]);
  });
});

describe("parseChangelog — category/bullet split (render surface)", () => {
  it("yields a group per category, preserves inline link markdown, ignores footer refs", () => {
    const raw = [
      "## [0.5.0] - 2026-08-18",
      "",
      "### Added",
      "- first bullet ([#12](https://example.test/pull/12))",
      "- second bullet",
      "",
      "[0.5.0]: https://example.test/tag/v0.5.0",
    ].join("\n");

    const r = byVersion(parseChangelog(raw), "0.5.0");
    const added = r.groups.find((g) => g.category === "Added");
    expect(added).toBeDefined();
    expect(added?.bullets).toHaveLength(2);
    expect(added?.bullets[0]).toBe("first bullet ([#12](https://example.test/pull/12))");
    expect(added?.bullets[0]).toContain("[#12](https://example.test/pull/12)");
    expect(added?.bullets[1]).toBe("second bullet");
    // Footer reference-definition lines belong to no category.
    expect(r.groups.some((g) => g.bullets.some((b) => b.includes("0.5.0]:")))).toBe(false);
  });

  it("appends indented continuation lines to their bullet", () => {
    const raw = [
      "## [0.6.0] - 2026-08-19",
      "",
      "### Changed",
      "- a bullet that",
      "  continues on the next line",
      "- another bullet",
      "",
    ].join("\n");

    const r = byVersion(parseChangelog(raw), "0.6.0");
    const changed = r.groups.find((g) => g.category === "Changed");
    expect(changed?.bullets).toEqual(["a bullet that\n  continues on the next line", "another bullet"]);
  });
});

describe("parseChangelog — Unreleased handling", () => {
  it("DROPS an empty [Unreleased] but a populated one in another fixture IS present", () => {
    const emptyRaw = [
      "## [Unreleased]",
      "",
      "## [0.7.0] - 2026-08-20",
      "",
      "### Added",
      "- shipped",
    ].join("\n");
    const emptyReleases = parseChangelog(emptyRaw);
    // Negative: the empty Unreleased is gone...
    expect(emptyReleases.some((r) => r.version === "Unreleased")).toBe(false);
    // ...paired with a positive so the negative is not vacuous: the real
    // release is still present, proving parsing ran.
    expect(emptyReleases.map((r) => r.version)).toEqual(["0.7.0"]);

    const populatedRaw = [
      "## [Unreleased]",
      "",
      "### Added",
      "- work in progress",
      "",
      "## [0.7.0] - 2026-08-20",
      "",
      "### Added",
      "- shipped",
    ].join("\n");
    const populatedReleases = parseChangelog(populatedRaw);
    const unreleased = byVersion(populatedReleases, "Unreleased");
    expect(unreleased.released).toBe(false);
    expect(unreleased.groups.find((g) => g.category === "Added")?.bullets).toEqual(["work in progress"]);
    expect(unreleased.body).toBe("### Added\n- work in progress");
  });
});

describe("parseChangelog — [NOT RELEASED] sections", () => {
  it("parses the version token, marks released=false, tolerates trailing text", () => {
    const raw = [
      "## [0.11.5] - 2026-07-25 [NOT RELEASED]",
      "",
      "### Added",
      "- staged but never shipped",
      "",
      "## [0.11.4] - 2026-07-20",
      "",
      "### Fixed",
      "- something",
    ].join("\n");

    const releases = parseChangelog(raw);
    const staged = byVersion(releases, "0.11.5");
    expect(staged.version).toBe("0.11.5");
    expect(staged.released).toBe(false);
    // date stops before the trailing " [NOT RELEASED]".
    expect(staged.date).toBe("2026-07-25");
    expect(staged.body).toBe("### Added\n- staged but never shipped");

    // A plain semver sibling is released=true — proves the flag discriminates.
    expect(byVersion(releases, "0.11.4").released).toBe(true);
  });
});

describe("parseChangelog — fail safe", () => {
  it("returns [] for empty input without throwing", () => {
    expect(parseChangelog("")).toEqual([]);
  });

  it("returns [] for input with no version headings without throwing", () => {
    const raw = ["# Changelog", "", "Some prose with no `## [` headings.", "", "## Not a version heading"].join(
      "\n",
    );
    expect(parseChangelog(raw)).toEqual([]);
  });
});
