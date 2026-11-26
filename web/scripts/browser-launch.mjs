// Which Chromium runs a browser test, and the flags it needs. Shared by the vitest browser
// config (vitest.browser.config.ts) and the runtime-web-modules harness
// (scripts/modules-browser-test.mjs) so the two cannot drift.
//
// CI and contributors use Playwright's pinned download (`npx playwright install chromium`).
// A uzi worker has none, but bakes its own Chromium at the stable /opt/uzi-toolchain/bin
// handle (agent/templates/base/Dockerfile), and gate:web runs there.
// AGENT_BROWSER_EXECUTABLE_PATH now reaches Claude-lane and off-mode Codex command shells
// (pointing at the uzi-chromium wrapper), but this resolution still detects the raw binary on
// disk and passes its own launch flags. UZI_WEB_BROWSER_CHROMIUM overrides both.
//
// A non-Playwright Chromium needs the worker's launch flags: --no-sandbox (the hardened,
// unprivileged, no-new-privileges worker cannot use Chromium's setuid sandbox) and
// --disable-dev-shm-usage (a 64MB /dev/shm). FONTCONFIG_FILE is re-supplied for the same
// sparse-env reason; the nix Chromium ships no fonts of its own.

import { existsSync } from "node:fs";

const WORKER_CHROMIUM = "/opt/uzi-toolchain/bin/chromium";
const WORKER_FONTCONFIG = "/etc/fonts/fonts.conf";

/** Playwright `launchOptions` for the resolved Chromium; `{}` means Playwright's own. */
export function chromiumLaunchOptions() {
  const executablePath =
    process.env.UZI_WEB_BROWSER_CHROMIUM || (existsSync(WORKER_CHROMIUM) ? WORKER_CHROMIUM : undefined);
  if (!executablePath) return {};
  return {
    executablePath,
    args: ["--no-sandbox", "--disable-dev-shm-usage"],
    env: {
      ...process.env,
      ...(!process.env.FONTCONFIG_FILE && existsSync(WORKER_FONTCONFIG) ? { FONTCONFIG_FILE: WORKER_FONTCONFIG } : {}),
    },
  };
}
