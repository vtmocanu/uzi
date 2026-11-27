/**
 * Bounded worker scratch placement fact shared by lead and checker prompts. It also
 * says that the worker-managed `GOCACHE`, `GOMODCACHE` and `UZI_SQLC_DIR` caches are
 * tool caches, not run artifacts (issue #4758): guidance only, no cache moves.
 */
export const SCRATCH_PLACEMENT_FACT =
  "Put temporary file-tool and shell artifacts, including test logs and review exports, under `.uzi/scratch/` in this worktree." +
  " The worker-managed `GOCACHE`, `GOMODCACHE` and `UZI_SQLC_DIR` caches are tool caches, not run artifacts: they stay where the worker points them, so do not point them into `.uzi/scratch/`, since an empty scratch cache can bypass the baked, verified archive and force a download." +
  " A tool's own cache-local atomic population, such as the `.stage.*` directory `scripts/sqlc.sh` creates next to the archive it publishes, is part of that cache and is not a scratch violation." +
  " A tool's private per-invocation temp work, such as `scripts/sqlc.sh`'s bare `mktemp -d`, still belongs in scratch by setting `TMPDIR` under `.uzi/scratch/`; moving only that temp work does not make a warm cache cold.";
