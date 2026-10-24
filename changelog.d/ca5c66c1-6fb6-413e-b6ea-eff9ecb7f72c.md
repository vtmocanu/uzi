### Added

- **Operators can set Go build and test concurrency defaults for worker commands (#3382).**
  Optional `workers.goBuildParallelism` / `workers.goMaxProcs` (Compose: `WORKER_GO_BUILD_PARALLELISM` / `WORKER_GO_MAXPROCS`) set `GOFLAGS=-p=N` and `GOMAXPROCS=N` for agent commands and worker checks.
