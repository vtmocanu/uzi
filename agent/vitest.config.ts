import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // The git-lifecycle tests spawn real `git` subprocesses against on-disk
    // fixture repos, which is slower than the default 5s ceiling on a cold cache.
    testTimeout: 30_000,
    hookTimeout: 30_000,
    include: ["test/**/*.test.ts"],
  },
});
