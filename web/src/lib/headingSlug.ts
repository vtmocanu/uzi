// IDs follow the docs' GitHub-style character and space rules. State belongs
// to one document; every candidate is checked against all previously emitted IDs.
export function createSlugger() {
  const taken = new Set<string>();
  const counters = new Map<string, number>();
  return {
    slug(text: string): string {
      const base = text
        .toLowerCase()
        .replace(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, "")
        .replace(/ /g, "-");
      let id = base;
      let counter = counters.get(base) ?? 0;
      // Each collision advances the counter; at most taken.size collisions
      // can precede an unused ID. Other bases retain independent counters.
      while (taken.has(id)) id = `${base}-${++counter}`;
      counters.set(base, counter);
      taken.add(id);
      return id;
    },
  };
}
