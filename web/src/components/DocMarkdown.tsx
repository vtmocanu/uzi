import Markdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { Link } from "react-router-dom";
import { rewriteHref, resolveImageSrc } from "../lib/docs";

// react-markdown builds a React element tree (no HTML injection); raw HTML in
// the source stays inert. Content is repo-authored and reviewed, so no
// rehype-raw/sanitize pipeline is needed. Typography lives in the `.docs-prose`
// rules in index.css; only the elements that need behaviour (links → SPA routes
// / GitLab, images → hashed assets, tables → horizontal scroll) are overridden
// here.
const components: Components = {
  // `{...props}` is spread FIRST throughout so our resolved href/src and safety
  // attrs (target/rel) always win over anything carried on the markdown node.
  a({ href, children, node: _node, ...props }) {
    const target = rewriteHref(href ?? "");
    if (target.internal) {
      return (
        <Link {...props} to={target.href}>
          {children}
        </Link>
      );
    }
    if (target.href === "") {
      // Neutralized (dangerous scheme): render inert text, no href.
      return <a {...props}>{children}</a>;
    }
    return (
      <a
        {...props}
        href={target.href}
        {...(target.external ? { target: "_blank", rel: "noopener noreferrer" } : {})}
      >
        {children}
      </a>
    );
  },
  img({ src, alt, node: _node, ...props }) {
    return (
      <img
        {...props}
        src={resolveImageSrc(typeof src === "string" ? src : "")}
        alt={alt ?? ""}
        loading="lazy"
      />
    );
  },
  table({ children, node: _node, ...props }) {
    // GFM tables can overflow narrow viewports; keep the page from scrolling
    // horizontally by scrolling the table inside its own container.
    return (
      <div className="my-4 overflow-x-auto">
        <table {...props}>{children}</table>
      </div>
    );
  },
};

export function DocMarkdown({ content }: { content: string }) {
  return (
    <div className="docs-prose">
      <Markdown remarkPlugins={[remarkGfm]} components={components}>
        {content}
      </Markdown>
    </div>
  );
}
