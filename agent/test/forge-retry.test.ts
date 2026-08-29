import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  classifyForgeError,
  classifyDevboxError,
  withForgeRetry,
  withRetry,
  FORGE_RETRY_SCHEDULE,
  DEVBOX_RETRY_SCHEDULE,
} from "../src/forge-retry.js";
import { ForgeError } from "../src/forge.js";
import { DEFAULT_TERMINAL_RETRY_SCHEDULE } from "../src/client.js";

// PRD #284 M1: the worker→forge retry schedule is a deliberate SECOND
// implementation of the same bounded-backoff decision the worker→API terminal
// callback uses (client.ts DEFAULT_TERMINAL_RETRY_SCHEDULE). The two are separate
// constants by design (different hop, worker-side vs shared), so this differential
// assertion is the "shared table" that keeps them pinned: if one is edited without
// the other, this red flags the drift the forge-retry.ts docblock promises.
describe("FORGE_RETRY_SCHEDULE ↔ DEFAULT_TERMINAL_RETRY_SCHEDULE (drift guard)", () => {
  it("stays pinned to the terminal-retry schedule", () => {
    assert.deepStrictEqual(FORGE_RETRY_SCHEDULE, DEFAULT_TERMINAL_RETRY_SCHEDULE);
  });
});

// Discriminating classifier table (PRD #284 M6). Each case is asserted, and the
// whole table is asserted exhaustively exercised so a silently dropped row is
// caught. Permanent-first precedence (D9) is the load-bearing invariant.
describe("classifyForgeError", () => {
  const cases: Array<{ name: string; err: unknown; want: "transient" | "permanent" }> = [
    // (i) the #216 stream reset ⇒ retry
    {
      name: "HTTP/2 stream reset (INTERNAL_ERROR)",
      err: new Error("HTTP/2 stream 1 reset by server (error 0x2 INTERNAL_ERROR)"),
      want: "transient",
    },
    // (ii) auth failure ⇒ no retry
    {
      name: "git authentication failure",
      err: new Error("fatal: Authentication failed for 'https://gitlab.example.com/x.git/'"),
      want: "permanent",
    },
    // (iii) protected branch ⇒ no retry
    {
      name: "protected-branch rejection",
      err: new Error("remote: GitLab: You are not allowed to push code to protected branches"),
      want: "permanent",
    },
    // (iv) non-fast-forward / [rejected] ⇒ no retry
    {
      name: "non-fast-forward rejected",
      err: new Error(" ! [rejected]        agent/x -> agent/x (non-fast-forward)"),
      want: "permanent",
    },
    // (v) BOTH transient and permanent substrings ⇒ permanent wins (precedence)
    {
      name: "mixed: connection reset AND protected branch",
      err: new Error("Connection reset while pushing to a protected branch"),
      want: "permanent",
    },
    // (vi) the bare generic trailer ⇒ NOT transient (defaults permanent)
    {
      name: "bare 'Could not read from remote repository'",
      err: new Error("fatal: Could not read from remote repository."),
      want: "permanent",
    },
    // ForgeError status classification
    { name: "ForgeError(0) transport failure", err: new ForgeError(0, "socket hang up"), want: "transient" },
    { name: "ForgeError(503)", err: new ForgeError(503, "service unavailable"), want: "transient" },
    { name: "ForgeError(429)", err: new ForgeError(429, "rate limited"), want: "transient" },
    { name: "ForgeError(422)", err: new ForgeError(422, "validation failed"), want: "permanent" },
    { name: "ForgeError(403)", err: new ForgeError(403, "forbidden"), want: "permanent" },
    // (vii) issue #775: the EXACT bare-clone connect-timeout from the real run ⇒ retry.
    // Matches /failed to connect/i ("Failed to connect to ... after N ms") and the
    // /could not connect to server/i trailer.
    {
      name: "bare-clone connect-timeout (issue #775)",
      err: new Error(
        "git clone --bare https://github.com/vtmocanu/uzi.git /data/repos/github.com+vtmocanu+uzi.git failed:\nCloning into bare repository '/data/repos/github.com+vtmocanu+uzi.git'...\nfatal: unable to access 'https://github.com/vtmocanu/uzi.git/': Failed to connect to github.com:443 after 129360 ms: Could not connect to server",
      ),
      want: "transient",
    },
    // (viii) negative control: an auth failure over the SAME URL ⇒ permanent (precedence).
    {
      name: "auth failure over the run URL",
      err: new Error("fatal: Authentication failed for 'https://github.com/vtmocanu/uzi.git/'"),
      want: "permanent",
    },
    // (ix) negative control: a 404 over the SAME URL ⇒ permanent (precedence).
    {
      name: "404 over the run URL",
      err: new Error(
        "fatal: unable to access 'https://github.com/vtmocanu/uzi.git/': The requested URL returned error: 404",
      ),
      want: "permanent",
    },
    // (x) issue #775: the `could not connect to server` trailer, ISOLATED (no "Failed to
    // connect"). Case (vii) carries BOTH phrases, so /could not connect to server/i is
    // redundant there — dropping the matcher still passes via /failed to connect/i. This
    // case has ONLY the trailer, so it independently pins /could not connect to server/i:
    // remove that pattern and this falls through to the permanent default. Note "could not
    // connect" does NOT match the pre-existing /couldn'?t connect/i (that needs "couldn't"/
    // "couldnt"), so this matcher is the sole reason it classifies transient.
    {
      name: "isolated 'Could not connect to server' trailer (issue #775)",
      err: new Error(
        "fatal: unable to access 'https://github.com/vtmocanu/uzi.git/': Could not connect to server",
      ),
      want: "transient",
    },
  ];

  const exercised = new Set<string>();
  for (const c of cases) {
    it(`classifies ${c.name} as ${c.want}`, () => {
      assert.strictEqual(classifyForgeError(c.err), c.want);
      exercised.add(c.name);
    });
  }

  it("exercised every discriminating case in the table", () => {
    assert.strictEqual(exercised.size, cases.length);
  });
});

describe("withForgeRetry", () => {
  it("retries a transient error and resolves on the 3rd attempt, sleeping [1000, 2000]", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let attempts = 0;
    const fn = async () => {
      attempts++;
      if (attempts < 3) throw new ForgeError(503, "transient");
      return "ok";
    };
    const result = await withForgeRetry(fn, { sleep });
    assert.strictEqual(result, "ok");
    assert.strictEqual(attempts, 3);
    assert.deepStrictEqual(delays, [1_000, 2_000]);
  });

  it("fails fast on a permanent error after exactly ONE attempt (sleep never called)", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let attempts = 0;
    const fn = async () => {
      attempts++;
      throw new ForgeError(403, "forbidden");
    };
    await assert.rejects(
      withForgeRetry(fn, { sleep }),
      (err: unknown) => err instanceof ForgeError && err.status === 403,
    );
    assert.strictEqual(attempts, 1);
    assert.deepStrictEqual(delays, []);
  });

  it("gives up after schedule.length + 1 attempts when a transient error always throws", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let attempts = 0;
    const fn = async () => {
      attempts++;
      throw new ForgeError(500, "always down");
    };
    await assert.rejects(
      withForgeRetry(fn, { sleep }),
      (err: unknown) => err instanceof ForgeError && err.status === 500,
    );
    assert.strictEqual(attempts, FORGE_RETRY_SCHEDULE.length + 1);
    assert.deepStrictEqual(delays, FORGE_RETRY_SCHEDULE);
  });
});

// Discriminating classifier table for the devbox `devbox install` hop. Same
// exhaustive-exercise style as classifyForgeError: each case is asserted and the
// whole table is asserted fully exercised so a silently dropped row is caught.
// Permanent-first, fail-closed precedence is the load-bearing invariant.
describe("classifyDevboxError", () => {
  const cases: Array<{ name: string; err: unknown; want: "transient" | "permanent" }> = [
    // curl timeout on nixpkgs metadata ⇒ retry (Error message match)
    {
      name: "curl (28) timeout fetching nixpkgs metadata",
      err: new Error("curl: (28) Timeout was reached while fetching nixpkgs metadata"),
      want: "transient",
    },
    // stderr-only network error ⇒ retry (proves stderr is inspected)
    {
      name: "curl (6) could not resolve host (on stderr, generic message)",
      err: {
        stderr: "curl: (6) Could not resolve host: api.github.com",
        message: "Command failed: devbox install",
      },
      want: "transient",
    },
    // 5xx from the binary cache ⇒ retry
    {
      name: "503 from cache.nixos.org",
      err: new Error("503 Service Unavailable from cache.nixos.org"),
      want: "transient",
    },
    // unknown package ⇒ deterministic, no retry
    {
      name: "unknown package",
      err: new Error("error: package 'nonesuch' not found"),
      want: "permanent",
    },
    // package name containing `ssl` ⇒ permanent (the fail-closed hazard: `ssl`
    // substring must NOT trigger a transient match)
    {
      name: "package 'openssl' not found (ssl substring is not a network phrase)",
      err: new Error("error: package 'openssl' not found"),
      want: "permanent",
    },
    // deterministic openssl configure error ⇒ permanent (the old `SSL.*` over-match
    // is gone; `openssl configure error` is NOT a network condition)
    {
      name: "openssl configure error (deterministic, not a network SSL failure)",
      err: new Error("error: openssl configure error: missing header"),
      want: "permanent",
    },
    // openssl-connector unfree license ⇒ permanent (package name, not a network phrase)
    {
      name: "package 'openssl-connector' unfree license (ssl+connect substrings)",
      err: new Error("error: package 'openssl-connector' has an unfree license"),
      want: "permanent",
    },
    // manifest line ref ending in :503: ⇒ permanent (bare 503 must NOT trigger transient)
    {
      name: "devbox.json:503:12 syntax error (503 line ref is not an HTTP 5xx)",
      err: new Error("error: syntax error at devbox.json:503:12"),
      want: "permanent",
    },
    // nix eval file:line ref default.nix:502:15 ⇒ permanent (bare 502 must NOT trigger)
    {
      name: "default.nix:502:15 nix eval file:line ref (502 line ref is not an HTTP 5xx)",
      err: new Error(
        "error: attribute 'foo' at /nix/store/x/default.nix:502:15 called without required argument",
      ),
      want: "permanent",
    },
    // genuine HTTP 5xx from the cache via curl ⇒ transient (HTTP-context-anchored 503)
    {
      name: "curl (22) returned error: 503 Service Unavailable (genuine HTTP 5xx)",
      err: new Error("curl: (22) The requested URL returned error: 503 Service Unavailable"),
      want: "transient",
    },
    // genuine nix cache 5xx (HTTP error 503) ⇒ transient
    {
      name: "unable to download narinfo: HTTP error 503 (genuine cache 5xx)",
      err: new Error(
        "error: unable to download 'https://cache.nixos.org/nar/xxx.narinfo': HTTP error 503",
      ),
      want: "transient",
    },
    // malformed manifest / parse error ⇒ deterministic, no retry
    {
      name: "malformed manifest parse error",
      err: new Error("error: syntax error, unexpected '}' at devbox.json:12"),
      want: "permanent",
    },
    // worker-timeout SIGTERM kill ⇒ permanent (hung install, never retried)
    {
      name: "worker-timeout SIGTERM kill",
      err: { killed: true, signal: "SIGTERM", code: null, message: "Command failed: devbox install" },
      want: "permanent",
    },
    // kill precedence: SIGTERM kill WITH a transient-looking stderr ⇒ still
    // permanent, because the kill check runs FIRST
    {
      name: "SIGTERM kill with transient curl (28) stderr (kill check wins)",
      err: {
        killed: true,
        signal: "SIGTERM",
        code: null,
        stderr: "curl: (28) Timeout was reached",
        message: "Command failed: devbox install",
      },
      want: "permanent",
    },
  ];

  const exercised = new Set<string>();
  for (const c of cases) {
    it(`classifies ${c.name} as ${c.want}`, () => {
      assert.strictEqual(classifyDevboxError(c.err), c.want);
      exercised.add(c.name);
    });
  }

  it("exercised every discriminating case in the table", () => {
    assert.strictEqual(exercised.size, cases.length);
  });
});

describe("withRetry (generic)", () => {
  it("retries a transient error then succeeds, sleeping the devbox schedule prefix", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let attempts = 0;
    const fn = async () => {
      attempts++;
      if (attempts < 3) throw new Error("curl: (28) Timeout was reached");
      return "ok";
    };
    const result = await withRetry(fn, {
      classify: classifyDevboxError,
      schedule: DEVBOX_RETRY_SCHEDULE,
      sleep,
    });
    assert.strictEqual(result, "ok");
    assert.strictEqual(attempts, 3);
    assert.deepStrictEqual(delays, [1_000, 4_000]);
  });

  it("fails fast on a permanent error (sleep never called)", async () => {
    const delays: number[] = [];
    const sleep = async (ms: number) => {
      delays.push(ms);
    };
    let attempts = 0;
    const fn = async () => {
      attempts++;
      throw new Error("error: package 'nonesuch' not found");
    };
    await assert.rejects(
      withRetry(fn, { classify: classifyDevboxError, schedule: DEVBOX_RETRY_SCHEDULE, sleep }),
      /package 'nonesuch' not found/,
    );
    assert.strictEqual(attempts, 1);
    assert.deepStrictEqual(delays, []);
  });
});
