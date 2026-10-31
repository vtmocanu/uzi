### Changed

- **Docker worker pod-epoch shadow now reports settle candidates and runtime authority (#3431).**
  Version-2 handoff evidence adds the successor's container IDs and a runtime-prefix cross-check, and `pod_epoch_reclaim` gains `authority`, `would_settle` and per-entry basis; it is still read-only, `crio-crun` defers `kernel_unaudited` outside an exact kernel allowlist, and old workers refuse v2 until the worker pin moves.
