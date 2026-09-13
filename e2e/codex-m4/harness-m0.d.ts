// PRD #1287 C1 — ambient types for the PURE protocol helpers the M4 loopback fake reuses
// from the FROZEN M0 driver (`e2e/codex-m0/harness.mjs`). The frozen fixture is plain
// `.mjs` with no types and MUST NOT be edited (a co-located `.d.mts` would be a write to
// the frozen dir), so this wildcard ambient module supplies the types for typechecking
// only — tsx uses Node resolution to the real .mjs at runtime, unaffected. Only the pure
// helpers are declared, never `Probe`/`SupervisorProbe` (which write config + inject
// `bypass_hook_trust`), so this harness cannot even name them by type. Cloned verbatim from
// e2e/codex-m3b/harness-m0.d.ts.

declare module "*/codex-m0/harness.mjs" {
  /** Race `promise` against a bounded timeout; rejects `${label} timed out` on expiry. */
  export function deadline<T>(promise: Promise<T>, label: string, milliseconds?: number): Promise<T>;
  /** Poll `predicate` until it returns a truthy value (returned), else time out. */
  export function poll<T>(predicate: () => T | Promise<T>, label: string, milliseconds?: number): Promise<NonNullable<T>>;
  /** Build a Responses `function_call` item. */
  export function tool(callId: string, name: string, args: unknown, namespace?: string): Record<string, unknown>;
  /** Build an `apply_patch` custom-tool-call item that adds a marker file. */
  export function patchTool(callId: string, filename?: string): Record<string, unknown>;
  /** Build an assistant `message` output item (the fixture's "finish" frame). */
  export function message(text?: string): Record<string, unknown>;
  /** Find the function/custom tool-call OUTPUT for `id` in a request body, if present. */
  export function callOutput(body: { input?: readonly unknown[] } & Record<string, unknown>, id: string): Record<string, unknown> | undefined;
}
