/// <reference types="vite/client" />

// Replaced at build time by vite.config.ts `define` (one value per built variant).
declare const __MODULE_ID__: string;
declare const __MODULE_CONTRACT__: number;
declare const __MODULE_BUILD_TAG__: string;

interface Window {
  // Test-only switch read by the fail-until-armed route.
  __uziFixtureArmSuccess?: boolean;
}
