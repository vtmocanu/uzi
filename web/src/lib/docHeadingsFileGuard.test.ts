import { describe, expect, it } from "vitest";
import { isInsideDocsDir } from "./docHeadingsFileGuard";

const docs = "/repo/docs";

describe("isInsideDocsDir", () => {
  it("accepts a markdown file inside docs/", () => {
    expect(isInsideDocsDir(docs, "/repo/docs/getting-started.md")).toBe(true);
  });
  it("refuses markdown outside docs/, a traversal, a sibling prefix and a non-markdown file", () => {
    expect(isInsideDocsDir(docs, "/repo/README.md")).toBe(false);
    expect(isInsideDocsDir(docs, "/repo/prds/3873-overview-redesign.md")).toBe(false);
    expect(isInsideDocsDir(docs, "/repo/docs/../CLAUDE.md")).toBe(false);
    expect(isInsideDocsDir(docs, "/repo/docs-old/x.md")).toBe(false);
    expect(isInsideDocsDir(docs, "/repo/docs/getting-started.txt")).toBe(false);
    expect(isInsideDocsDir(docs, "/repo/docs")).toBe(false);
  });
});
