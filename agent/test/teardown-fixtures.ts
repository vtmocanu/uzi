import fs from "node:fs/promises";
import path from "node:path";
import { restoreTreeWritability, type TeardownTestDeps } from "../src/rmtree.js";

/** Portable lifecycle fixture only: production never falls back to path removal. */
export const portableTeardownTestDeps: TeardownTestDeps | undefined =
  process.platform === "linux" ? undefined : {
    removeTreePinned: async (parent, name) => {
      const target = path.join(parent, name);
      await restoreTreeWritability(target);
      await fs.rm(target, { recursive: true, force: true });
      return "removed";
    },
  };
