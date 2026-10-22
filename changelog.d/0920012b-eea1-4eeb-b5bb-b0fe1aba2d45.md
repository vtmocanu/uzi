### Fixed

- **Worker toolchain refreshes preserve their package resolution model (#3311).**
  Refresh in an isolated Devbox project, preserving explicit pins and rejecting manifest migrations or partial locks before publication.
