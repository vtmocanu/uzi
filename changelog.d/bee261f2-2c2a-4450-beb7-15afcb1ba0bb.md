### Fixed

- **Usage folds retry a run cut short by a busy materializer (#4778).**
  A single run whose fold waited behind a kick or ran short of budget is retried on the next sweeper tick instead of being dropped or skipped for ten minutes.
