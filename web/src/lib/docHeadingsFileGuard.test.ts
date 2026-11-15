import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { isInsideDocsDir } from "./docHeadingsFileGuard";

// A real temp tree: root/docs/{a.md, sub/b.md, link-out.md -> ../secret.md, notes.txt},
// root/secret.md, root/docs-old/x.md, and root/"docs\\private.md" (a literal backslash name).
let root = "";
let docs = "";

beforeAll(() => {
  root = mkdtempSync(join(tmpdir(), "uzi-doc-guard-"));
  docs = join(root, "docs");
  mkdirSync(join(docs, "sub"), { recursive: true });
  mkdirSync(join(root, "docs-old"), { recursive: true });
  writeFileSync(join(docs, "a.md"), "# a");
  writeFileSync(join(docs, "sub", "b.md"), "# b");
  writeFileSync(join(docs, "notes.txt"), "x");
  writeFileSync(join(root, "secret.md"), "# secret");
  writeFileSync(join(root, "docs-old", "x.md"), "# x");
  writeFileSync(join(root, "docs\\private.md"), "# private");
  symlinkSync(join(root, "secret.md"), join(docs, "link-out.md"));
});

afterAll(() => {
  if (root) rmSync(root, { recursive: true, force: true });
});

describe("isInsideDocsDir", () => {
  it("accepts real markdown files inside docs/, nested ones included", () => {
    expect(isInsideDocsDir(docs, join(docs, "a.md"))).toBe(true);
    expect(isInsideDocsDir(docs, join(docs, "sub", "b.md"))).toBe(true);
  });

  it("refuses a symlink under docs/ that points outside it", () => {
    expect(isInsideDocsDir(docs, join(docs, "link-out.md"))).toBe(false);
  });

  it("refuses a sibling whose name contains a literal backslash", () => {
    expect(isInsideDocsDir(docs, join(root, "docs\\private.md"))).toBe(false);
  });

  it("refuses traversal, a sibling prefix, a non-markdown file, the directory itself and a missing file", () => {
    expect(isInsideDocsDir(docs, join(docs, "..", "secret.md"))).toBe(false);
    expect(isInsideDocsDir(docs, join(root, "docs-old", "x.md"))).toBe(false);
    expect(isInsideDocsDir(docs, join(docs, "notes.txt"))).toBe(false);
    expect(isInsideDocsDir(docs, docs)).toBe(false);
    expect(isInsideDocsDir(docs, join(docs, "missing.md"))).toBe(false);
  });
});
