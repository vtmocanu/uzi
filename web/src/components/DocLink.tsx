import { Link } from "react-router-dom";
import type { ReactNode } from "react";

// DocLink is the reusable SPA → in-app-docs link (PRD #57). It navigates in the
// same tab (no target="_blank") to the bundled `/docs/:slug` page. The link text
// (children) should be the guide's title — never "click here". `slug` should come
// from doclinks.ts, the single source of truth, not a hand-written string.
export function DocLink({
  slug,
  children,
  className,
}: {
  slug: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <Link to={`/docs/${slug}`} className={className ?? "text-brand hover:underline"}>
      {children}
    </Link>
  );
}
