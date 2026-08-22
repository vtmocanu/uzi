import { describe, it, expect } from "vitest";
import { parseChangelog, splitBulletTitle, type Release } from "./changelog";

// These tests feed literal fixture strings to the pure parser, so they are
// decoupled from the real CHANGELOG.md content. The parity test against the real
// file (vs. scripts/changelog-section.sh) lands in M4.

function byVersion(releases: Release[], version: string): Release {
  const r = releases.find((x) => x.version === version);
  if (!r) throw new Error(`no release ${version} in [${releases.map((x) => x.version).join(", ")}]`);
  return r;
}

describe("parseChangelog", () => {
  it("stops a middle version's body at the next `## [` heading", () => {
    const raw = [
      "## [0.2.0] - 2026-08-10",
      "",
      "### Added",
      "",
      "- second release note",
      "",
      "## [0.1.0] - 2026-08-01",
      "",
      "### Added",
      "",
      "- first release note",
      "",
    ].join("\n");
    const releases = parseChangelog(raw);
    const mid = byVersion(releases, "0.2.0");
    expect(mid.body).toBe("### Added\n\n- second release note");
    expect(mid.body).not.toContain("first release note");
    expect(mid.body).not.toContain("## [0.1.0]");
    expect(mid.date).toBe("2026-08-10");
  });

  it("extracts the release-title marker into titleMarker and removes it from body", () => {
    const raw = [
      "## [0.5.0] - 2026-08-19",
      "<!-- release-title: readable run transcript -->",
      "",
      "### Changed",
      "",
      "- reworked the transcript",
      "",
    ].join("\n");
    const [rel] = parseChangelog(raw);
    expect(rel.titleMarker).toBe("readable run transcript");
    expect(rel.body).toBe("### Changed\n\n- reworked the transcript");
    expect(rel.body).not.toContain("release-title");
  });

  it("trims leading and trailing blank lines from body", () => {
    const raw = [
      "## [0.3.0] - 2026-08-12",
      "",
      "",
      "### Fixed",
      "",
      "- a fix",
      "",
      "",
      "",
    ].join("\n");
    const [rel] = parseChangelog(raw);
    expect(rel.body).toBe("### Fixed\n\n- a fix");
  });

  it("retains the compare-link footer in the oldest section's body", () => {
    const raw = [
      "## [0.2.0] - 2026-08-10",
      "",
      "### Added",
      "",
      "- newer note",
      "",
      "## [0.1.0] - 2026-08-01",
      "",
      "### Added",
      "",
      "- oldest note",
      "",
      "[0.2.0]: https://github.com/vtmocanu/uzi/compare/v0.1.0...v0.2.0",
      "[0.1.0]: https://github.com/vtmocanu/uzi/releases/tag/v0.1.0",
    ].join("\n");
    const releases = parseChangelog(raw);
    const oldest = byVersion(releases, "0.1.0");
    expect(oldest.body).toContain(
      "[0.2.0]: https://github.com/vtmocanu/uzi/compare/v0.1.0...v0.2.0",
    );
    expect(oldest.body).toContain(
      "[0.1.0]: https://github.com/vtmocanu/uzi/releases/tag/v0.1.0",
    );
    // The footer is retained as body content, but is not rendered as a bullet.
    expect(oldest.groups).toEqual([{ category: "Added", bullets: ["oldest note"] }]);
  });

  it("splits `### Added`/`### Changed` into bullets and preserves an inline link verbatim", () => {
    const raw = [
      "## [0.6.0] - 2026-08-20",
      "",
      "### Added",
      "",
      "- **Signed images** landed in [#123](https://github.com/vtmocanu/uzi/pull/123).",
      "",
      "### Changed",
      "",
      "- reworded a nudge that wraps onto",
      "  a second indented line",
      "",
    ].join("\n");
    const [rel] = parseChangelog(raw);
    expect(rel.groups).toEqual([
      {
        category: "Added",
        bullets: [
          "**Signed images** landed in [#123](https://github.com/vtmocanu/uzi/pull/123).",
        ],
      },
      {
        category: "Changed",
        bullets: ["reworded a nudge that wraps onto a second indented line"],
      },
    ]);
    // The inline markdown link survives byte-for-byte in the bullet text.
    expect(rel.groups[0].bullets[0]).toContain("[#123](https://github.com/vtmocanu/uzi/pull/123)");
  });

  it("parses a [NOT RELEASED] heading with trailing text: notReleased true, version extracted", () => {
    const raw = [
      "## [0.11.5] - 2026-07-25 [NOT RELEASED]",
      "",
      "### Fixed",
      "",
      "- a change that never shipped",
      "",
    ].join("\n");
    const [rel] = parseChangelog(raw);
    expect(rel.version).toBe("0.11.5");
    expect(rel.date).toBe("2026-07-25");
    expect(rel.notReleased).toBe(true);
    expect(rel.unreleased).toBe(false);
  });

  it("drops an empty [Unreleased] but keeps a populated one", () => {
    const emptyThenReal = [
      "## [Unreleased]",
      "",
      "## [0.9.0] - 2026-08-20",
      "",
      "### Added",
      "",
      "- a shipped note",
      "",
    ].join("\n");
    const dropped = parseChangelog(emptyThenReal);
    expect(dropped.map((r) => r.version)).toEqual(["0.9.0"]);

    const populated = [
      "## [Unreleased]",
      "",
      "### Added",
      "",
      "- work in progress",
      "",
      "## [0.9.0] - 2026-08-20",
      "",
      "### Added",
      "",
      "- a shipped note",
      "",
    ].join("\n");
    const kept = parseChangelog(populated);
    expect(kept.map((r) => r.version)).toEqual(["Unreleased", "0.9.0"]);
    const head = byVersion(kept, "Unreleased");
    expect(head.unreleased).toBe(true);
    expect(head.groups).toEqual([{ category: "Added", bullets: ["work in progress"] }]);
  });

  it("does not throw on malformed input and returns a list", () => {
    expect(parseChangelog("")).toEqual([]);
    expect(parseChangelog("# Changelog\n\njust some intro prose, no headings\n")).toEqual([]);
    // A truncated section (heading, no body) still yields a best-effort release.
    const truncated = parseChangelog("## [0.1.0] - 2026-08-01");
    expect(Array.isArray(truncated)).toBe(true);
    expect(truncated[0]?.version).toBe("0.1.0");
  });
});

describe("splitBulletTitle", () => {
  it("splits a leading bold title off from its description", () => {
    expect(splitBulletTitle("**Title.** description")).toEqual({
      title: "Title.",
      description: "description",
    });
  });

  it("keeps an inline PR link and a PRD ref verbatim inside the title", () => {
    const bullet =
      "**Feature ([#568](https://github.com/vtmocanu/uzi/pull/568), PRD #534 follow-up).** The description follows.";
    expect(splitBulletTitle(bullet)).toEqual({
      title: "Feature ([#568](https://github.com/vtmocanu/uzi/pull/568), PRD #534 follow-up).",
      description: "The description follows.",
    });
  });

  it("returns a null title when the bullet has no leading bold span", () => {
    expect(splitBulletTitle("reworded a nudge that wraps")).toEqual({
      title: null,
      description: "reworded a nudge that wraps",
    });
  });

  it("returns an empty description for a title-only bullet", () => {
    expect(splitBulletTitle("**Title.**")).toEqual({
      title: "Title.",
      description: "",
    });
  });
});
