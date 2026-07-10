import { memo, type ReactNode } from "react";
import { type Components } from "react-markdown";
import { schemeIsDangerous } from "../lib/docs";
import { CommandBlock } from "./RunEvent";
import { MarkdownCore } from "./MarkdownCore";

// Untrusted-LLM policy for the shared MarkdownCore, used to render plan bodies
// and agent prose. The content is model output, so the pipeline stays hardened:
// no rehype-raw (raw HTML is inert text) and react-markdown's default
// urlTransform already strips javascript:/data:/file: URLs to "" before these
// overrides run. We add schemeIsDangerous as independent defense-in-depth (a
// future urlTransform override can't reopen the hole) and apply three behaviours
// the docs policy does NOT want here:
//   - links are treated as external: new tab + rel="noopener noreferrer" (never
//     rewritten to in-app routes — a model must not forge SPA navigation).
//   - images are size-capped via CSS so a remote/oversized <img> can't blow up
//     the activity box (a remote beacon is an accepted risk for this
//     loopback-only MVP; a layout bomb is not).
//   - fenced bash/sh/shell blocks render through CommandBlock — the same code
//     surface and highlightShell tokenizer as a tool command — so the same shell
//     command reads identically in prose and in the tool rail (PRD #38 M5,
//     Decision 10). This changes no sanitizer posture: highlightShell emits React
//     text nodes only, so a fence body containing "<script>" stays inert text.

const SHELL_LANGS = new Set(["bash", "sh", "shell"]);

// shellLang extracts a shell-family language ("bash" | "sh" | "shell") from a
// code element's className, or undefined for inline code / any other fence.
// react-markdown v10 removed the `inline` prop, so a fenced block is detected
// purely by its `language-*` class (inline code carries none) — Decision 10.
function shellLang(className: unknown): string | undefined {
  const classes = Array.isArray(className)
    ? className
    : typeof className === "string"
      ? className.split(/\s+/)
      : [];
  for (const c of classes) {
    const m = /^language-([\w-]+)$/.exec(String(c));
    if (m && SHELL_LANGS.has(m[1])) return m[1];
  }
  return undefined;
}

// A minimal structural view of the hast node react-markdown hands each override
// (avoids importing @types/hast just to read one className off the `pre`'s child
// `code`). Only the fields we touch are declared.
type HastNode = {
  type?: string;
  tagName?: string;
  properties?: { className?: unknown };
  children?: HastNode[];
};

// preShellLang answers "is this <pre> wrapping a shell fence?" so the wrapper can
// be unwrapped (the CommandBlock the `code` override returns is a block-level
// surface — nesting it inside `.docs-prose pre` would double-box it and is
// invalid HTML).
function preShellLang(node: unknown): string | undefined {
  const el = node as HastNode | undefined;
  const code = el?.children?.find((c) => c?.type === "element" && c?.tagName === "code");
  return code ? shellLang(code.properties?.className) : undefined;
}

// codeText flattens a code element's children to a plain string (fenced content
// is a single text child in practice, but flatten defensively).
function codeText(children: ReactNode): string {
  if (typeof children === "string") return children;
  if (Array.isArray(children)) return children.map(codeText).join("");
  return "";
}

const components: Components = {
  a({ href, children, node: _node, ...props }) {
    const url = href ?? "";
    if (url === "" || schemeIsDangerous(url)) {
      // Neutralized by react-markdown or by our own check: inert text, no href.
      return <a {...props}>{children}</a>;
    }
    return (
      <a {...props} href={url} target="_blank" rel="noopener noreferrer">
        {children}
      </a>
    );
  },
  img({ src, alt, node: _node, ...props }) {
    const url = typeof src === "string" ? src : "";
    if (url === "" || schemeIsDangerous(url)) {
      // Neutralized src: show the alt text as a placeholder rather than a broken
      // image icon.
      return <span className="text-xs italic text-faint">{alt || "image"}</span>;
    }
    return (
      <img
        {...props}
        src={url}
        alt={alt ?? ""}
        loading="lazy"
        className="my-2 max-h-64 max-w-full rounded-md border border-edge"
      />
    );
  },
  pre({ children, node, ...props }) {
    // A shell-family fence is rendered by the `code` override below as a
    // CommandBlock (block-level). Drop the default <pre> so that surface is not
    // double-boxed by `.docs-prose pre` nor nested illegally inside a <pre>.
    // Every other block keeps its default <pre> (docs-prose styling untouched).
    if (preShellLang(node)) return <>{children}</>;
    return <pre {...props}>{children}</pre>;
  },
  code({ className, children, node: _node, ...props }) {
    // Fenced bash/sh/shell → the tool-command surface (highlightShell inside
    // CommandBlock). Inline code and every other fence fall through to the
    // default rendering, so their `.docs-prose code` styling is untouched.
    if (shellLang(className)) {
      return <CommandBlock command={codeText(children).replace(/\n+$/, "")} />;
    }
    return (
      <code className={className} {...props}>
        {children}
      </code>
    );
  },
};

// Memoized so an append to the run feed never re-parses an unchanged message's
// markdown (rows are keyed by immutable seq, so `content` is stable per row).
export const Markdown = memo(function Markdown({ content }: { content: string }) {
  return <MarkdownCore content={content} className="docs-prose" components={components} />;
});
