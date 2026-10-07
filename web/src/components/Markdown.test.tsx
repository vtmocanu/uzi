// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { Markdown } from "./Markdown";
import { MarkdownCore } from "./MarkdownCore";

afterEach(cleanup);

// PRD #38 M5 (Decision 10): fenced bash/sh/shell in untrusted agent prose renders
// through the same CommandBlock surface + highlightShell tokenizer as a tool
// command; inline code and every other fence keep their default styling; the
// sanitizer posture is unchanged (fence bodies never become live HTML).

const fence = (lang: string, body: string) => "```" + lang + "\n" + body + "\n```";

describe("Markdown decoded entity policy", () => {
  it.each(["&#x202e;", "&#8238;", "&lrm;", "&shy;", "&#x200b;", "&#8203;"])(
    "strips decoded %s from text and string attributes",
    (entity) => {
      const { container } = render(
        <Markdown content={
          "# Report " + entity + "denied&#x202c;\n\n" +
          '[before' + entity + 'after](https://example.com/a&amp;b "before' + entity + 'after")\n\n' +
          '![before' + entity + 'after](https://example.com/a&amp;b.png "before' + entity + 'after")'
        } />,
      );
      expect(container.querySelector("h1")?.textContent).toBe("Report denied");
      const link = container.querySelector("a")!;
      expect(link.textContent).toBe("beforeafter");
      expect(link.getAttribute("title")).toBe("beforeafter");
      expect(link.getAttribute("href")).toBe("https://example.com/a&b");
      const image = container.querySelector("img")!;
      expect(image.getAttribute("alt")).toBe("beforeafter");
      expect(image.getAttribute("title")).toBe("beforeafter");
      expect(image.getAttribute("src")).toBe("https://example.com/a&b.png");
    },
  );

  it.each([
    ["&#x202e;", "%E2%80%AE"],
    ["&#8238;", "%E2%80%AE"],
    ["&lrm;", "%E2%80%8E"],
    ["&shy;", "%C2%AD"],
    ["&#x200b;", "%E2%80%8B"],
    ["&#8203;", "%E2%80%8B"],
  ])("keeps URL normalization for encoded %s without raw controls", (entity, encoded) => {
    const { container } = render(
      <Markdown content={'[link](https://example.com/a' + entity + 'b)\n\n![image](https://example.com/a' + entity + 'b.png)'} />,
    );
    // Markdown normalizes URL controls to percent escapes before the HAST policy.
    expect(container.querySelector("a")?.getAttribute("href")).toBe("https://example.com/a" + encoded + "b");
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://example.com/a" + encoded + "b.png");
  });

  it("scrubs decoded className arrays while preserving literal fence entities", () => {
    const { container } = render(
      <Markdown content={fence("py&#x202e;thon", "before\u202eafter &lrm; &#8238;\n\tend")} />,
    );
    expect(container.querySelector("code")?.className).toBe("language-python");
    expect(container.querySelector("code")?.textContent).toBe("beforeafter &lrm; &#8238;\n\tend\n");
  });

  it("preserves benign entities, safe Unicode, Markdown and allowed whitespace", () => {
    const { container } = render(
      <Markdown content={'# Café 日本語 😀 &amp; &lt;ok&gt;\n\n**bold** &NewLine;middle&Tab;fin\n\n[link](https://example.com/a&amp;b "Café &quot;ok&quot;")'} />,
    );
    expect(container.querySelector("h1")?.textContent).toBe("Café 日本語 😀 & <ok>");
    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.textContent).toContain("\nmiddle\tfin");
    expect(container.querySelector("a")?.getAttribute("title")).toBe('Café "ok"');
  });

  it("leaves the trusted MarkdownCore default unscrubbed", () => {
    const { container } = render(<MarkdownCore content={'# before&#x202e;after\n\n[link](https://example.com "before&lrm;after")'} />);
    expect(container.querySelector("h1")?.textContent).toBe("before\u202eafter");
    expect(container.querySelector("a")?.getAttribute("title")).toBe("before\u200eafter");
  });

  it("keeps entity-obfuscated dangerous URLs and raw HTML inert", () => {
    const { container } = render(
      <Markdown content={'[bad](java&#x202e;script:alert(1))\n\n![bad](java&lrm;script:alert(1))\n\n<script>alert(1)</script>'} />,
    );
    expect(container.querySelector("a")?.getAttribute("href")).toBeNull();
    expect(container.querySelector("img, script")).toBeNull();
    expect(container.textContent).toContain("<script>alert(1)</script>");
  });
});

describe("Markdown shell-fence parity", () => {
  it("renders a bash fence through highlightShell with the exact text preserved", () => {
    const { container } = render(<Markdown content={fence("bash", "npm run build")} />);
    const code = container.querySelector("code");
    expect(code).not.toBeNull();
    // The ❯ prompt is a sibling of <code>, so the code's text is the command
    // verbatim — with syntax spans, not a lossy first-line summary.
    expect(code!.textContent).toBe("npm run build");
    // Tokenized: the command word carries the --syn-cmd class.
    expect(container.querySelector(".text-syn-cmd")?.textContent).toBe("npm");
    // It is the tool-command surface (prompt glyph present).
    expect(container.textContent).toContain("❯");
  });

  it("treats sh and shell as bash aliases", () => {
    for (const lang of ["sh", "shell"]) {
      const { container } = render(<Markdown content={fence(lang, "ls -la")} />);
      expect(container.querySelector("code")?.textContent).toBe("ls -la");
      expect(container.querySelector(".text-syn-cmd")?.textContent).toBe("ls");
    }
  });

  it("preserves a multi-line bash fence verbatim (no data loss)", () => {
    const cmd = "cat <<'EOF'\nhello\nEOF";
    const { container } = render(<Markdown content={fence("bash", cmd)} />);
    expect(container.querySelector("code")?.textContent).toBe(cmd);
  });

  it("leaves a non-bash fence and inline code untouched", () => {
    const { container } = render(
      <Markdown content={fence("python", "print('hi')") + "\n\nand `inline` too"} />,
    );
    // The python fence keeps the default <pre><code class="language-python">.
    const py = container.querySelector("code.language-python");
    expect(py).not.toBeNull();
    expect(py!.textContent).toContain("print('hi')");
    // No shell surface leaked in: no highlight spans, no prompt glyph.
    expect(container.querySelector(".text-syn-cmd")).toBeNull();
    expect(container.textContent).not.toContain("❯");
    // Inline code stays a bare <code> (no language class, default styling).
    const inline = [...container.querySelectorAll("code")].find((c) => c.textContent === "inline");
    expect(inline).toBeTruthy();
    expect(inline!.className).toBe("");
  });

  it("keeps a raw-HTML fence body inert — text, never live elements", () => {
    const { container } = render(
      <Markdown
        content={fence("html", '<script>window.__uziM5=1</script><div id="inj">x</div>')}
      />,
    );
    // No element injection and no script element created from the fence body.
    expect(container.querySelector("#inj")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
    // The markup survives as visible text (MarkdownCore has no rehype-raw).
    expect(container.textContent).toContain('<div id="inj">');
    expect((window as unknown as Record<string, unknown>).__uziM5).toBeUndefined();
  });

  it("renders a <script> inside a bash fence as inert text", () => {
    const { container } = render(
      <Markdown content={fence("bash", "echo '<script>alert(1)</script>'")} />,
    );
    // Went through highlightShell (React text nodes only) — no live <script>.
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("code")?.textContent).toBe("echo '<script>alert(1)</script>'");
  });

  it("strips trailing newlines from a pathological fence without a hang", () => {
    // Many trailing newlines: the strip is now a linear scan (was a
    // catastrophic-backtracking /\n+$/). Correctness assertion only — the command
    // survives and every trailing newline is gone. Renders fast; no timing gate.
    const body = "echo hi" + "\n".repeat(20_000);
    const { container } = render(<Markdown content={fence("bash", body)} />);
    expect(container.querySelector("code")?.textContent).toBe("echo hi");
  });

  it("strips only trailing newlines, preserving interior blank lines", () => {
    const cmd = "echo a\n\necho b"; // an interior blank line must survive
    const { container } = render(<Markdown content={fence("bash", cmd + "\n\n\n")} />);
    expect(container.querySelector("code")?.textContent).toBe(cmd);
  });

  it("strips Cf/bidi control characters centrally, keeping the visible text (#124/#319)", () => {
    // Every untrusted <Markdown> sink is covered by construction: the component strips
    // Unicode control/format characters before parse, so a caller need not wrap. Here the
    // content is passed RAW (no per-site stripUnsafeChars) to prove the strip lives in the
    // component itself. The next comment line embeds a LITERAL RLO (U+202E) + zero-width
    // space (U+200B) so the reader sees exactly what gets stripped; the chars are the
    // point and must stay. Only that comment trips no-irregular-whitespace (oxlint 1.79
    // promoted it to correctness) - the fixture STRING is skipped by the rule's skipStrings
    // default - so the suppression is scoped to the single comment line, never the rule:
    // oxlint-disable-next-line no-irregular-whitespace
    // component. "before‮after​end" = RLO + zero-width space around visible letters.
    const { container } = render(<Markdown content={"before‮after​end"} />);
    const rendered = container.textContent ?? "";
    // Neither the bidi override nor the zero-width space survives to the rendered DOM.
    expect(rendered).not.toContain("‮");
    expect(rendered).not.toContain("​");
    // No character from the Cf category at all.
    expect(rendered).not.toMatch(/[\p{Cf}]/u);
    // The visible letters are untouched (the strip removes only meaning-free characters).
    expect(rendered).toContain("beforeafterend");
  });

  it("unwraps the <pre> for a shell fence but keeps it for other languages", () => {
    // A shell fence becomes the block-level CommandBlock (no <pre>); every other
    // fence keeps its default <pre><code> so .docs-prose styling is untouched.
    const bash = render(<Markdown content={fence("bash", "npm run build")} />);
    expect(bash.container.querySelector("pre")).toBeNull();
    const py = render(<Markdown content={fence("python", "print('hi')")} />);
    expect(py.container.querySelector("pre")).not.toBeNull();
  });
});
