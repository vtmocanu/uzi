// The server-owned capability vocabulary (PRD #84), single-sourced on the web side
// here so the repo capability picker (Repos.tsx) and the mock (mockApi/forge.ts) cannot
// drift from each other. The AUTHORITY is Go: api/internal/capability's Vocabulary(),
// which the server's capability.Filter enforces on every write, so free-form entry is
// not allowed. This constant is pinned against that authority by a cross-module golden
// (api/internal/capability/testdata/vocabulary.json) in capabilityVocabulary.test.ts —
// mirroring how workerSizes.ts is pinned to hosted_sizes.json. If the Go vocabulary
// changes and its golden is regenerated, that test reddens until this list follows.
export const CAPABILITY_VOCABULARY = ["docker", "jvm"] as const;
