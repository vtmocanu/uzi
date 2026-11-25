// Harness-only call counters (PRD #4686 browser harness). `__UZI_TEST_HOOKS__` is false
// in every shipped build (vite.config.ts `define`), so this whole body is dead-code
// eliminated there and the shipped host carries no `__uziModuleStats` string. The harness
// build sets the flag true and reads window.__uziModuleStats to prove a Retry re-rendered
// without calling registerRemotes or loadRemote again.

type ModuleStats = { registerRemotes: number; loadRemote: number };

declare global {
  interface Window {
    __uziModuleStats?: ModuleStats;
  }
}

export function countModuleStat(name: keyof ModuleStats): void {
  if (__UZI_TEST_HOOKS__) {
    const stats = (window.__uziModuleStats ??= { registerRemotes: 0, loadRemote: 0 });
    stats[name]++;
  }
}

if (__UZI_TEST_HOOKS__) {
  window.__uziModuleStats ??= { registerRemotes: 0, loadRemote: 0 };
}
