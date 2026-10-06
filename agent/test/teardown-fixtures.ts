import fs from "node:fs/promises";
import path from "node:path";
import type { TeardownTestDeps } from "../src/rmtree.js";

/** Portable lifecycle fixture only: production never falls back to path removal. */
export const portableTeardownTestDeps: TeardownTestDeps | undefined =
  process.platform === "linux" ? undefined : {
    removeTreePinned: async (parent, name) => {
      await fs.rm(path.join(parent, name), { recursive: true, force: true });
      return "removed";
    },
  };
