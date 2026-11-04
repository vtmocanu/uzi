### Fixed

- **Repository browser checks can find the worker's baked Chromium from Claude commands and from Codex commands in sandbox mode `off` (#3907).**
  Agent commands now get `CHROME_PATH` and `AGENT_BROWSER_EXECUTABLE_PATH` pointing at a launch wrapper that adds Chromium's required flags, a per-launch profile and (unless the caller sets them) run-private config and cache directories; Codex commands in `required` or `best-effort` mode do not get them.
