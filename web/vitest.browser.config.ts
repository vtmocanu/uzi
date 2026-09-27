import { existsSync } from "node:fs";
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { playwright } from "@vitest/browser-playwright";

// Which Chromium runs the tests. CI and contributors use Playwright's pinned download
// (`npx playwright install chromium`). A uzi worker has none, but bakes its own Chromium at
// the stable /opt/uzi-toolchain/bin handle (agent/templates/base/Dockerfile), and gate:web runs
// there. The worker's AGENT_BROWSER_* env does NOT reach the agent's shell (the SDK env is
// sparse, agent/src/sdk-env.ts), so the path is detected on disk rather than read from env.
// UZI_WEB_BROWSER_CHROMIUM overrides both.
//
// A non-Playwright Chromium needs the worker's launch flags: --no-sandbox (the hardened,
// unprivileged, no-new-privileges worker cannot use Chromium's setuid sandbox) and
// --disable-dev-shm-usage (a 64MB /dev/shm). FONTCONFIG_FILE is re-supplied for the same
// sparse-env reason; the nix Chromium ships no fonts of its own.
const WORKER_CHROMIUM = "/opt/uzi-toolchain/bin/chromium";
const WORKER_FONTCONFIG = "/etc/fonts/fonts.conf";
const executablePath =
  process.env.UZI_WEB_BROWSER_CHROMIUM || (existsSync(WORKER_CHROMIUM) ? WORKER_CHROMIUM : undefined);
const launchOptions = executablePath
  ? {
      executablePath,
      args: ["--no-sandbox", "--disable-dev-shm-usage"],
      env: {
        ...process.env,
        ...(!process.env.FONTCONFIG_FILE && existsSync(WORKER_FONTCONFIG) ? { FONTCONFIG_FILE: WORKER_FONTCONFIG } : {}),
      },
    }
  : {};

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
      provider: playwright({ launchOptions }),
      instances: [{ browser: "chromium" }],
    },
  },
});
