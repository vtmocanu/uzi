### Fixed

- **Codex cross-check Search now covers source files over 1 MiB (#4683).**
  Regular files are read in bounded 1 MiB chunks counted against the existing 16 MiB search byte total, stopping with a partial result at the budget; a file seen to change mid-scan is skipped.
