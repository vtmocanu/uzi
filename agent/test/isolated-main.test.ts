// PRD #1906 M4: main.ts's isolated-executor seam. Under UZI_EXECUTOR=stub the isolated lane,
// like the judge and review lanes, must never start a real SDK session: it gets the stub
// judge queryFn, which makes no network call and fails the isolated session closed.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { buildFetchToolsServer } from "../src/fetch-tools.js";
import { buildIsolatedExecutor } from "../src/main.js";
import { nullLogger } from "./helpers.js";

let dir: string;

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-isolated-main-"));
  fs.mkdirSync(path.join(dir, "workspace"));
  fs.mkdirSync(path.join(dir, "home"));
});
afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

describe("buildIsolatedExecutor (main.ts)", () => {
  it("in stub mode drives the stub queryFn, never a real SDK session", async () => {
    const workspace = path.join(dir, "workspace");
    const ex = buildIsolatedExecutor({ log: nullLogger(), executorKind: "stub", workerTokenFile: undefined });
    const fetch = buildFetchToolsServer({ fetcherUrl: "https://127.0.0.1:1", ca: "", credential: "cred", workspace, log: nullLogger() });
    const started = Date.now();
    await assert.rejects(
      ex.run({
        runId: "stub-1",
        oauthToken: "dummy-oauth-token",
        workspace,
        homeDir: path.join(dir, "home"),
        prompt: "Research task: x",
        fetchServer: fetch.server,
        emit: () => undefined,
        signal: new AbortController().signal,
        maxTurns: 1,
        timeoutMs: 30_000,
      }),
      // The stub judge stream yields model output and no init frame: the isolated
      // executor refuses it, which is the fail-closed outcome a stub lane should have.
      /isolated run refused: the session produced output before its tool set was verified/,
    );
    assert.ok(Date.now() - started < 5_000, "no CLI process was started and waited on");
  });
});
