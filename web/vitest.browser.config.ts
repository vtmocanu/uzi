import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { playwright } from "@vitest/browser-playwright";

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
  plugins: [react()],
  test: {
    include: ["src/**/*.browser.test.{ts,tsx}"],
    testTimeout: 20000,
    browser: {
      enabled: true,
      headless: true,
      provider: playwright(),
      instances: [{ browser: "chromium" }],
    },
  },
});
