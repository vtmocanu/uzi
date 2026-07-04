import { describe, it, expect } from "vitest";
import {
  GITLAB_BLOB_BASE,
  parseFrontmatter,
  resolveFromDocs,
  resolveHref,
  resolveImageSrc,
  sortDocsForIndex,
  summarize,
  type Audience,
  type Doc,
} from "./docs";

function doc(slug: string, order: number | null, audience: Audience = "user"): Doc {
  return { slug, meta: { title: slug, order, audience }, body: "", summary: "" };
}

// These tests use inline-string fixtures and a fake `isUserPage` predicate, so
// they are decoupled from the real docs/ content (M2 can rewrite every page
// without touching them).

describe("parseFrontmatter", () => {
  it("parses a leading fence and strips it (plus leading blank lines) from the body", () => {
    const { meta, body } = parseFrontmatter(
      "---\ntitle: Board\norder: 30\naudience: user\n---\n\n# Board\n\ntext\n",
    );
    expect(meta).toEqual({ title: "Board", order: 30, audience: "user" });
    expect(body).toBe("# Board\n\ntext\n");
  });

  it("treats a file with no leading fence as audience:design, body untouched", () => {
    const raw = "# Title\n\nbody with a --- later\n---\nstill body\n";
    const { meta, body } = parseFrontmatter(raw);
    expect(meta).toEqual({ title: "", order: null, audience: "design" });
    expect(body).toBe(raw); // the later `---` is content, not a fence
  });

  it("closes at the first fence, keeping a later `---` in the body as content", () => {
    const { meta, body } = parseFrontmatter(
      "---\ntitle: Agent templates\naudience: user\norder: 50\n---\n\n# T\n\n```\n---\n```\n",
    );
    expect(meta.title).toBe("Agent templates");
    expect(body).toContain("```\n---\n```");
  });

  it("defaults a missing audience to design and a missing/non-numeric order to null", () => {
    expect(parseFrontmatter("---\ntitle: Only title\n---\n\nx\n").meta).toEqual({
      title: "Only title",
      order: null,
      audience: "design",
    });
    expect(parseFrontmatter("---\ntitle: T\naudience: user\norder: soon\n---\n\nx\n").meta.order).toBeNull();
  });

  it("ignores an unknown audience value (falls back to design)", () => {
    expect(parseFrontmatter("---\ntitle: T\naudience: wizard\n---\n\nx\n").meta.audience).toBe("design");
  });
});

describe("resolveFromDocs", () => {
  it("resolves doc-relative paths to repo-relative, normalizing . and ..", () => {
    expect(resolveFromDocs("configuration.md")).toBe("docs/configuration.md");
    expect(resolveFromDocs("./board.md")).toBe("docs/board.md");
    expect(resolveFromDocs("img/x.png")).toBe("docs/img/x.png");
    expect(resolveFromDocs("../plan.md")).toBe("plan.md");
    expect(resolveFromDocs("../ARCHITECTURE.md")).toBe("ARCHITECTURE.md");
  });
});

describe("resolveHref", () => {
  const isUser = (slug: string) => slug === "board" || slug === "anthropic-token";

  it("rewrites a link to a bundled user page to an in-app /docs/:slug route", () => {
    expect(resolveHref("board.md", isUser)).toEqual({
      href: "/docs/board",
      external: false,
      internal: true,
    });
    expect(resolveHref("./anthropic-token.md#store-it", isUser).href).toBe(
      "/docs/anthropic-token#store-it",
    );
  });

  it("sends a repo-only doc (not a user page) to the pinned GitLab blob base", () => {
    expect(resolveHref("configuration.md", isUser)).toEqual({
      href: `${GITLAB_BLOB_BASE}docs/configuration.md`,
      external: true,
      internal: false,
    });
  });

  it("sends a repo-root file to GitLab, preserving the anchor", () => {
    expect(resolveHref("../ARCHITECTURE.md#forge-integration", isUser).href).toBe(
      `${GITLAB_BLOB_BASE}ARCHITECTURE.md#forge-integration`,
    );
    expect(resolveHref("../plan.md", isUser).href).toBe(`${GITLAB_BLOB_BASE}plan.md`);
  });

  it("passes anchors, root-absolute, external and non-http scheme links through", () => {
    expect(resolveHref("#section", isUser)).toEqual({
      href: "#section",
      external: false,
      internal: false,
    });
    expect(resolveHref("/docs/board", isUser).internal).toBe(true);
    expect(resolveHref("https://example.com", isUser)).toEqual({
      href: "https://example.com",
      external: true,
      internal: false,
    });
    expect(resolveHref("mailto:a@b.com", isUser)).toEqual({
      href: "mailto:a@b.com",
      external: false,
      internal: false,
    });
  });

  it("classifies protocol-relative //host (and the /\\ variant) as external", () => {
    expect(resolveHref("//evil.example.com", isUser)).toEqual({
      href: "//evil.example.com",
      external: true,
      internal: false,
    });
    expect(resolveHref("/\\evil.example.com", isUser).external).toBe(true);
    // a single-slash path stays internal
    expect(resolveHref("/docs/board", isUser).internal).toBe(true);
  });

  it("neutralizes dangerous schemes to an empty href (defense-in-depth)", () => {
    for (const bad of [
      "javascript:alert(1)",
      "JavaScript:alert(1)",
      "  javascript:alert(1)",
      "java\tscript:alert(1)",
      "vbscript:msgbox(1)",
      "data:text/html,<script>alert(1)</script>",
      "file:///etc/passwd",
    ]) {
      expect(resolveHref(bad, isUser)).toEqual({ href: "", external: false, internal: false });
    }
  });
});

describe("resolveImageSrc (content-independent passthroughs)", () => {
  it("passes through empty, root-absolute and http(s) sources unchanged", () => {
    expect(resolveImageSrc("")).toBe("");
    expect(resolveImageSrc("/logo.png")).toBe("/logo.png");
    expect(resolveImageSrc("https://x/y.png")).toBe("https://x/y.png");
  });

  it("neutralizes dangerous schemes (incl. data:) to an empty src", () => {
    expect(resolveImageSrc("data:image/png;base64,AAAA")).toBe("");
    expect(resolveImageSrc("javascript:alert(1)")).toBe("");
    expect(resolveImageSrc("file:///etc/passwd")).toBe("");
  });

  it("falls back to the raw src for a relative path with no bundled asset", () => {
    expect(resolveImageSrc("img/definitely-not-a-real-shot.png")).toBe(
      "img/definitely-not-a-real-shot.png",
    );
  });
});

describe("sortDocsForIndex", () => {
  it("orders by `order` ascending, missing order last, slug as the tiebreak", () => {
    const sorted = sortDocsForIndex([
      doc("zeta", null),
      doc("board", 30),
      doc("alpha", null),
      doc("getting-started", 10),
    ]);
    expect(sorted.map((d) => d.slug)).toEqual(["getting-started", "board", "alpha", "zeta"]);
  });

  it("does not mutate its input", () => {
    const input = [doc("b", 20), doc("a", 10)];
    sortDocsForIndex(input);
    expect(input.map((d) => d.slug)).toEqual(["b", "a"]);
  });
});

describe("summarize", () => {
  it("skips the leading H1 and derives a blurb from the first paragraph, stripping markdown", () => {
    expect(summarize("# Title\n\nA **bold** intro with a [link](x.md) and `code`.\n\n## Next")).toBe(
      "A bold intro with a link and code.",
    );
  });

  it("stops at the first table/heading/code fence and truncates long blurbs", () => {
    expect(summarize("# T\n\nfirst line\nsecond line\n\n| a | b |")).toBe("first line second line");
    const long = `# T\n\n${"word ".repeat(60)}`;
    const out = summarize(long);
    expect(out.length).toBeLessThanOrEqual(160);
    expect(out.endsWith("…")).toBe(true);
  });
});
