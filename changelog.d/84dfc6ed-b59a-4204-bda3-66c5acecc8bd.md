### Changed

- **Repository browser checks find the worker's baked Chromium on both command lanes (#3907).**
  Agent commands now get `CHROME_PATH` and `AGENT_BROWSER_EXECUTABLE_PATH` pointing at a launch wrapper that adds Chromium's required flags and run-private config, cache and profile directories; on Codex they are advertised only when the command sandbox mode is `off`.
