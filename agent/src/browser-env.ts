// Issue #3907: the worker-controlled browser discovery variables.
//
// Constants only (nothing is copied from process.env). Both variables point at the
// baked /usr/local/bin/uzi-chromium wrapper (agent/bin/uzi-chromium), which execs the
// real Chromium with the mandatory --no-sandbox / --disable-dev-shm-usage flags and
// run-private XDG dirs, so whichever launcher reads either variable gets those flags.
// AGENT_BROWSER_EXECUTABLE_PATH is read by agent-browser; CHROME_PATH is read by
// chrome-launcher (Lighthouse). PUPPETEER_EXECUTABLE_PATH and CHROME_BIN are
// deliberately NOT exported.

export const BROWSER_DISCOVERY_ENV: Readonly<Record<"AGENT_BROWSER_EXECUTABLE_PATH" | "CHROME_PATH", string>> = Object.freeze({
  AGENT_BROWSER_EXECUTABLE_PATH: "/usr/local/bin/uzi-chromium",
  CHROME_PATH: "/usr/local/bin/uzi-chromium",
});
