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
  export function existsSync(path: URL | string): boolean;
  // The mock-graph acyclic guard (mocks/api-acyclic.test.ts) enumerates every
  // non-test mock source by a recursive directory walk. Only the
  // withFileTypes+recursive overload is used; declared TYPE-only, as above.
  export interface Dirent {
    name: string;
    parentPath: string;
    isFile(): boolean;
  }
  export function readdirSync(
    path: URL | string,
    options: { recursive?: boolean; withFileTypes: true },
  ): Dirent[];
}

// The changelog parity gate (changelog.parity.test.ts) shells out to the parity
// oracle and resolves the repo root from import.meta.url. As above, these are
// TYPE-only declarations of exactly the surface that one test uses, kept minimal
// rather than pulling in @types/node for a whole runtime the bundle never ships.
declare module "node:child_process" {
  export interface SpawnSyncReturn {
    status: number | null;
    stdout: string;
    stderr: string;
    error?: Error;
  }
  export function spawnSync(
    command: string,
    args: readonly string[],
    options: { cwd?: string; encoding: "utf8" },
  ): SpawnSyncReturn;
}

declare module "node:url" {
  export function fileURLToPath(url: string | URL): string;
}

declare module "node:path" {
  export function dirname(p: string): string;
  export function resolve(...segments: string[]): string;
  export function join(...segments: string[]): string;
  interface PathModule {
    dirname(p: string): string;
    resolve(...segments: string[]): string;
    join(...segments: string[]): string;
  }
  const path: PathModule;
  export default path;
}
