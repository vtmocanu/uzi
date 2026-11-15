// isInsideDocsDir is the doc-headings Vite plugin's file guard: true only for a .md file
// inside docsDir (no traversal out of it). The dev server does not apply its fs.allow check
// to the plugin's ?doc-headings query, so the plugin must bound its own reads. Pure string
// logic (no node:path) so it type-checks with the app; both paths are absolute POSIX paths
// or Windows paths with backslashes, normalized here.
function normalize(p: string): string[] {
  const out: string[] = [];
  for (const part of p.replace(/\\/g, "/").split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") out.pop();
    else out.push(part);
  }
  return out;
}

export function isInsideDocsDir(docsDir: string, file: string): boolean {
  const dir = normalize(docsDir);
  const f = normalize(file);
  if (f.length <= dir.length || !f[f.length - 1].endsWith(".md")) return false;
  return dir.every((seg, i) => f[i] === seg);
}
