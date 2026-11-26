import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { docHeadingsPlugin } from "./vite-doc-headings";
import { playwright } from "@vitest/browser-playwright";
import { chromiumLaunchOptions } from "./scripts/browser-launch.mjs";

// Which Chromium runs the tests, and its launch flags, are resolved by scripts/browser-launch.mjs
// (shared with the runtime-web-modules harness).
const launchOptions = chromiumLaunchOptions();

// Real-browser layout tests (`*.browser.test.tsx`), run by `task test:web-browser`.
//
// A SEPARATE config, not a third project in vite.config.ts: jsdom has no layout engine, so a
// wrap/overflow regression (the 390px triage-row squeeze) is invisible to the main suite, and
// folding a browser project into it would make every `task test:web` need a Chromium download.
// vite.config.ts's jsdom project excludes this suffix so the two suites never collect the
// same file.
//
// Nothing is inherited from vite.config.ts, so testTimeout is restated here. The test file
// imports src/index.css itself: Tailwind classes only mean something with the real stylesheet.
export default defineConfig({
  plugins: [react(), docHeadingsPlugin()],
  // Same flag vite.config.ts defines: the module loader reads it, and a bare identifier
  // would otherwise be a ReferenceError in the browser.
  define: { __UZI_TEST_HOOKS__: "false" },
  test: {
    include: ["src/**/*.browser.test.{ts,tsx}"],
    testTimeout: 20000,
    browser: {
      enabled: true,
      headless: true,
      provider: playwright({ launchOptions }),
      instances: [{ browser: "chromium" }],
    },
  },
});
