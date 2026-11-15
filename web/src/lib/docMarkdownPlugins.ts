import type { Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import { rehypeHeadingIds } from "./rehypeHeadingIds";

// Plugin lists for the doc pipeline. Plain .ts with no React/docs imports so the
// build-time heading extraction (vite-doc-headings.ts via docHeadingsPipeline.ts)
// runs the exact same plugins as the page.
//
// SECURITY: DOC_REHYPE_PLUGINS (heading DOM ids) must be used by DocMarkdown ONLY.
// MarkdownCore is shared with renderers of untrusted markdown (run messages, issue
// text); giving those headings ids would let model- or user-authored text create
// element ids (DOM clobbering, hash hijack). MarkdownCore may use
// DOC_REMARK_PLUGINS only.
export const DOC_REMARK_PLUGINS: Options["remarkPlugins"] = [remarkGfm];
export const DOC_REHYPE_PLUGINS: Options["rehypePlugins"] = [rehypeHeadingIds];
