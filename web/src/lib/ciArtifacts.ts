// Mirrors api/internal/ciartifacts.NormalizeSelectors; never sanitize request identities.
// Go strings.TrimSpace uses Unicode White_Space (unlike JavaScript trim, which strips BOM).
const goSpaceEdges = /^\p{White_Space}+|\p{White_Space}+$/gu;

// Validate Go path.Match syntax, not JavaScript regex or workflow-name syntax.
// Each scan consumes a code point; total work is bounded by the 200-byte selector cap.
function validGlob(pattern: string): boolean {
  const chars = [...pattern];
  let i = 0;
  const escapedClassChar = (): boolean => {
    if (i >= chars.length || chars[i] === "-" || chars[i] === "]") return false;
    if (chars[i] === "\\") {
      i++;
      if (i >= chars.length) return false;
    }
    i++;
    return i < chars.length;
  };
  while (i < chars.length) {
    const c = chars[i++];
    if (c === "\\") {
      if (i >= chars.length) return false;
      i++;
    } else if (c === "[") {
      if (chars[i] === "^") i++;
      let ranges = 0;
      while (true) {
        if (chars[i] === "]" && ranges > 0) {
          i++;
          break;
        }
        if (!escapedClassChar()) return false;
        if (chars[i] === "-") {
          i++;
          if (!escapedClassChar()) return false;
        }
        ranges++;
      }
    }
  }
  return true;
}

export function normalizeCIArtifacts(input: string[]): { selectors: string[]; error: string } {
  const fail = (error: string) => ({ selectors: [], error });
  if (input.length > 20) return fail("ci_artifacts must not exceed 20 entries");
  const selectors: string[] = [];
  for (const raw of input) {
    if (/[\p{Cc}\p{Cf}]/u.test(raw))
      return fail("ci_artifacts entries must not contain control or format characters");
    const s = raw.replace(goSpaceEdges, "");
    // Lone UTF-16 surrogates cannot represent valid UTF-8. TextEncoder would replace them.
    if (/[\uD800-\uDFFF]/u.test(s) || s === "" || new TextEncoder().encode(s).length > 200)
      return fail("each ci_artifacts entry must be valid UTF-8 and 1 to 200 bytes");
    if (!validGlob(s)) return fail("ci_artifacts entries must be valid path.Match globs");
    if (selectors.includes(s)) return fail("ci_artifacts entries must be unique");
    selectors.push(s);
  }
  return { selectors, error: "" };
}
