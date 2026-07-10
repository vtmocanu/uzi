// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { Markdown } from "./Markdown";

afterEach(cleanup);

// PRD #38 M5 (Decision 10): fenced bash/sh/shell in untrusted agent prose renders
// through the same CommandBlock surface + highlightShell tokenizer as a tool
// command; inline code and every other fence keep their default styling; the
// sanitizer posture is unchanged (fence bodies never become live HTML).

const fence = (lang: string, body: string) => "```" + lang + "\n" + body + "\n```";

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

  it("unwraps the <pre> for a shell fence but keeps it for other languages", () => {
    // A shell fence becomes the block-level CommandBlock (no <pre>); every other
    // fence keeps its default <pre><code> so .docs-prose styling is untouched.
    const bash = render(<Markdown content={fence("bash", "npm run build")} />);
    expect(bash.container.querySelector("pre")).toBeNull();
    const py = render(<Markdown content={fence("python", "print('hi')")} />);
    expect(py.container.querySelector("pre")).not.toBeNull();
  });
});
