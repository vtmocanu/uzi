/// <reference types="vite/client" />

// Build-time flag from `define` in vite.config.ts (false in every shipped build; the
// browser harness build sets it true). Guards window.__uziModuleStats in src/modules/loader.ts.
declare const __UZI_TEST_HOOKS__: boolean;
