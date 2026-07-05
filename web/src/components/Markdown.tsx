import { memo } from "react";
import { type Components } from "react-markdown";
import { schemeIsDangerous } from "../lib/docs";
import { MarkdownCore } from "./MarkdownCore";

// Untrusted-LLM policy for the shared MarkdownCore, used to render plan bodies
// and agent prose. The content is model output, so the pipeline stays hardened:
// no rehype-raw (raw HTML is inert text) and react-markdown's default
// urlTransform already strips javascript:/data:/file: URLs to "" before these
// overrides run. We add schemeIsDangerous as independent defense-in-depth (a
// future urlTransform override can't reopen the hole) and apply two behaviours
// the docs policy does NOT want here:
//   - links are treated as external: new tab + rel="noopener noreferrer" (never
//     rewritten to in-app routes — a model must not forge SPA navigation).
//   - images are size-capped via CSS so a remote/oversized <img> can't blow up
//     the activity box (a remote beacon is an accepted risk for this
//     loopback-only MVP; a layout bomb is not).
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
};

// Memoized so an append to the run feed never re-parses an unchanged message's
// markdown (rows are keyed by immutable seq, so `content` is stable per row).
export const Markdown = memo(function Markdown({ content }: { content: string }) {
  return <MarkdownCore content={content} className="docs-prose" components={components} />;
});
