import { useMemo } from "react";
import { type Components } from "react-markdown";
import { Link } from "react-router-dom";
import { rewriteHref, resolveImageSrc } from "../lib/docs";
import { MarkdownCore } from "./MarkdownCore";

// Trusted-docs policy for the shared MarkdownCore. Content is repo-authored and
// reviewed; the pipeline (react-markdown, no rehype-raw) plus these overrides
// give links repo-aware routing (internal → SPA route, repo-only → GitLab blob)
// and images their hashed asset URLs. `isAdmin` decides whether operator-doc
// links route in-app (issue #75 M1), so the components object is built per render
// keyed on it. Typography lives in the `.docs-prose` rules in index.css. The
// table overflow wrapper is policy-neutral and lives in MarkdownCore.
function buildComponents(isAdmin: boolean): Components {
  return {
    // `{...props}` is spread FIRST throughout so our resolved href/src and safety
    // attrs (target/rel) always win over anything carried on the markdown node.
    a({ href, children, node: _node, ...props }) {
      const target = rewriteHref(href ?? "", isAdmin);
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
  };
}

export function DocMarkdown({ content, isAdmin }: { content: string; isAdmin: boolean }) {
  const components = useMemo(() => buildComponents(isAdmin), [isAdmin]);
  return <MarkdownCore content={content} className="docs-prose" components={components} />;
}
