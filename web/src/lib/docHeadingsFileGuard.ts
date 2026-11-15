import { realpathSync } from "node:fs";
import { isAbsolute, relative, sep } from "node:path";

// isInsideDocsDir is the doc-headings Vite plugin's file guard (Node only; the plugin and its
// test import it, the app bundle never does). The dev server does not apply its fs.allow check
// to the plugin's ?doc-headings query, so this is the load-bearing boundary: it compares the
// CANONICAL paths on disk (realpath, so a symlink under docs/ that points elsewhere, or a
// filename containing a literal backslash, is judged by what it really is) and admits only an
// existing .md file strictly inside docsDir. Anything it cannot resolve is refused.
export function isInsideDocsDir(docsDir: string, file: string): boolean {
  let dir: string;
  let real: string;
  try {
    dir = realpathSync(docsDir);
    real = realpathSync(file);
  } catch {
    return false;
  }
  if (!real.endsWith(".md")) return false;
  const rel = relative(dir, real);
  return rel !== "" && !isAbsolute(rel) && rel !== ".." && !rel.startsWith(`..${sep}`);
}
