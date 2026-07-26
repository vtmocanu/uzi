// The SPA never touches the filesystem, so `web/` deliberately has no @types/node. Exactly
// one test file needs one function from it: judgeBacklogFidelity.test.ts reads the
// repo-root golden fixture (fixtures/judge-fidelity), which by design is owned by neither
// runtime and therefore cannot be imported through the bundler as if it were app code.
//
// Declaring that single signature here is narrower than adding a types package for a whole
// runtime the shipped bundle never sees. It is a TYPE declaration only: it asserts nothing
// about behaviour, and if the signature were wrong the test would fail at runtime under
// vitest's real node.
declare module "node:fs" {
  export function readFileSync(path: URL | string, encoding: "utf8"): string;
}
