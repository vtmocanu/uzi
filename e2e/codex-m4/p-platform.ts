// PRD #1287 — the P layer drives the pinned LINUX codex binary. On a non-Linux host the P files
// do NOT run natively (a Linux-musl binary cannot exec on macOS); they run inside the pinned Linux
// container via `task test:codex-m4:macos` / the darwin leg of `test:codex-m4`, where
// process.platform === "linux". This is a platform-ROUTING guard, not a coverage skip: the
// container path provides the real P evidence, and run-completeness fails loudly if it is missing.
export const P_LAYER_SKIP: string | false =
  process.platform === "linux"
    ? false
    : "P layer needs the pinned Linux codex binary; run via 'task test:codex-m4:macos' (Linux container)";
