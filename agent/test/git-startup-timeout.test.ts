import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { GitCache, CheckpointSoftDeadlineError, type BoundaryProcessSpawner } from "../src/git.js";
import { CheckpointChildStartupTimeoutError } from "../src/harness.js";
import { CodexLaunchError } from "../src/codex/launcher.js";
import { nullLogger } from "./helpers.js";

type ScopedCommands = {
  execScoped(command: string, args: string[], options: { env: NodeJS.ProcessEnv }): Promise<unknown>;
  spawnGit(cwd: string, args: string[]): Promise<unknown>;
};
const routes = ["execScoped", "spawnGit"] as const;

for (const route of routes) {
  describe(`${route} rejected checkpoint child startup`, () => {
    async function invoke(spawn: BoundaryProcessSpawner, hard: AbortSignal,
      hooks: { softSignal?: AbortSignal; softDeadlineAt?: number } = {}) {
      const cache = new GitCache("/unused-startup-timeout-test", nullLogger());
      const commands = cache as unknown as ScopedCommands;
      return cache.withBoundaryProcessSpawner(spawn, hard, () => route === "execScoped"
        ? commands.execScoped("git", ["status"], { env: {} })
        : commands.spawnGit("/work/repo", ["status"]), hooks);
    }

    for (const fired of [false, true]) {
      it(`maps startup recovery with soft timer ${fired ? "fired" : "pending"}, retaining authentic cause`, async () => {
        const hard = new AbortController();
        const soft = new AbortController();
        const authentic = new CodexLaunchError("started_deadline", "verified", new Error("private-input"));
        const marker = new CheckpointChildStartupTimeoutError(authentic);
        let timer: ReturnType<typeof setTimeout> | undefined;
        let spawns = 0;
        try {
          await assert.rejects(invoke(async (request) => {
            spawns++;
            assert.equal(request.recoverableTimeout, true);
            assert.ok(request.timeoutMs! > 0);
            if (fired) {
              await new Promise<void>((resolve) => {
                timer = setTimeout(() => { soft.abort(); resolve(); }, 0);
              });
            } else {
              timer = setTimeout(() => soft.abort(), 60_000);
            }
            assert.equal(soft.signal.aborted, fired);
            throw marker;
          }, hard.signal, { softSignal: soft.signal, softDeadlineAt: Date.now() + 10_000 }),
          (error: unknown) => {
            assert.ok(error instanceof CheckpointSoftDeadlineError);
            assert.equal(error.cause, marker);
            assert.equal((error.cause as CheckpointChildStartupTimeoutError).cause, authentic);
            assert.ok(error.message.includes("started_deadline"));
            assert.equal(error.message.includes("private-input"), false);
            return true;
          });
          assert.equal(spawns, 1);
        } finally {
          if (timer) clearTimeout(timer);
        }
      });
    }

    for (const fired of [false, true]) {
      it(`outside a soft-budget scope preserves the marker (soft signal aborted: ${fired})`, async () => {
        const soft = new AbortController();
        const marker = new CheckpointChildStartupTimeoutError(new Error("private-input"));
        await assert.rejects(invoke(async (request) => {
          assert.equal(request.recoverableTimeout, undefined);
          if (fired) soft.abort();
          throw marker;
        }, new AbortController().signal, { softSignal: soft.signal }),
        (error: unknown) => { assert.equal(error, marker); return true; });
      });
    }

    it("hard abort takes precedence over startup recovery and soft abort", async () => {
      const hard = new AbortController();
      const soft = new AbortController();
      const marker = new CheckpointChildStartupTimeoutError(new Error("private-input"));
      await assert.rejects(invoke(async () => {
        hard.abort();
        soft.abort();
        throw marker;
      }, hard.signal, { softSignal: soft.signal, softDeadlineAt: Date.now() + 10_000 }),
      (error: unknown) => {
        assert.ok(error instanceof Error);
        assert.equal(error.name, "AbortError");
        assert.ok(!(error instanceof CheckpointSoftDeadlineError));
        assert.equal(error.cause, marker);
        assert.equal(error.message, "permit-held git operation aborted");
        return true;
      });
    });

    it("preserves an existing soft error and its cause after the soft signal fires", async () => {
      const soft = new AbortController();
      const original = Object.assign(new CheckpointSoftDeadlineError(), { cause: new Error("private-input") });
      await assert.rejects(invoke(async () => {
        soft.abort();
        throw original;
      }, new AbortController().signal, { softSignal: soft.signal, softDeadlineAt: Date.now() + 10_000 }),
      (error: unknown) => { assert.equal(error, original); return true; });
    });

    it("retains unrelated error behavior when the soft signal fires", async () => {
      const soft = new AbortController();
      await assert.rejects(invoke(async () => {
        soft.abort();
        throw new Error("private-input");
      }, new AbortController().signal, { softSignal: soft.signal, softDeadlineAt: Date.now() + 10_000 }),
      (error: unknown) => {
        assert.ok(error instanceof CheckpointSoftDeadlineError);
        assert.equal(error.message, "checkpoint publication soft deadline exceeded");
        return true;
      });
    });

    it("propagates unrelated rejection unchanged while both signals are live", async () => {
      const original = new Error("private-input");
      await assert.rejects(invoke(async () => { throw original; }, new AbortController().signal,
        { softSignal: new AbortController().signal, softDeadlineAt: Date.now() + 10_000 }),
      (error: unknown) => { assert.equal(error, original); return true; });
    });
  });
}
