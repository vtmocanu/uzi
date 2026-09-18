import { describe, it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { loadConfig } from "../src/config.js";
import { TRANSIENT_TRIP_MS } from "../src/batcher.js";

// Minimal env that satisfies loadConfig's required fields.
const baseEnv = (over: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv => ({
  UZI_API_URL: "http://api:8080",
  UZI_WORKER_TOKEN: "worker-join-token-0123456789",
  ...over,
});

describe("loadConfig workerTemplate (PRD #18)", () => {
  it("defaults to base when UZI_WORKER_TEMPLATE is unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).workerTemplate, "base");
  });

  it("reads the baked UZI_WORKER_TEMPLATE (not the WORKER_TEMPLATE build var)", () => {
    // Only the image-baked UZI_WORKER_TEMPLATE is the reported identity; a stray
    // WORKER_TEMPLATE (the compose build arg) must NOT be picked up.
    const cfg = loadConfig(baseEnv({ UZI_WORKER_TEMPLATE: "jvm", WORKER_TEMPLATE: "base" }));
    assert.strictEqual(cfg.workerTemplate, "jvm");
  });

  it("trims and falls back to base on blank", () => {
    assert.strictEqual(loadConfig(baseEnv({ UZI_WORKER_TEMPLATE: "  " })).workerTemplate, "base");
  });
});

describe("loadConfig version (PRD #113 M1)", () => {
  it("reports the build-stamped UZI_AGENT_VERSION", () => {
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "0.11.7" })).version, "0.11.7");
  });

  it("passes the +g<short-sha> build-metadata suffix through verbatim", () => {
    // CI stamps `<release>+g<short-sha>`. SemVer §10 excludes build metadata from
    // precedence (x/mod/semver: Compare("v0.11.7+g1a2b3c4","v0.11.7") == 0), so the
    // suffix costs the api's compare nothing, but the worker must report it
    // UNTOUCHED or the commit it identifies is unrecoverable from the UI. No
    // trimming, no normalization, no stripping of the `+` on this side.
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "0.11.7+g1a2b3c4" })).version, "0.11.7+g1a2b3c4");
  });

  it("is EMPTY when unstamped — never a fake SemVer", () => {
    // The retired "0.1.0-m4" default is the whole point of this milestone: an
    // unstamped image must report nothing (the api classifies it `unknown`)
    // rather than a plausible version it is not running. An unstamped image sets
    // the ENV to the empty ARG default, so set-but-empty is the SHIPPING case,
    // not a corner one — both it and unset must land on "".
    assert.strictEqual(loadConfig(baseEnv()).version, "");
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "" })).version, "");
    assert.strictEqual(loadConfig(baseEnv({ UZI_AGENT_VERSION: "   " })).version, "");
  });
});

describe("loadConfig chat lifecycle knobs (PRD #39)", () => {
  it("applies the documented defaults when unset", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.chatMaxTurns, 50);
    assert.strictEqual(c.chatTurnTimeoutMs, 10 * 60_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 60 * 60_000);
    assert.strictEqual(c.chatPollMs, 1000);
    assert.strictEqual(c.chatSessions, 1);
  });

  it("parses overrides (durations + integer knobs)", () => {
    const c = loadConfig(
      baseEnv({
        CHAT_MAX_TURNS: "12",
        WORKER_CHAT_TURN_TIMEOUT: "90s",
        WORKER_CHAT_IDLE_TIMEOUT: "30m",
        WORKER_CHAT_POLL_MS: "250",
        WORKER_CHAT_SESSIONS: "3",
      }),
    );
    assert.strictEqual(c.chatMaxTurns, 12);
    assert.strictEqual(c.chatTurnTimeoutMs, 90_000);
    assert.strictEqual(c.chatIdleTimeoutMs, 30 * 60_000);
    assert.strictEqual(c.chatPollMs, 250);
    assert.strictEqual(c.chatSessions, 3);
  });

  it("falls back to the default on a non-positive or non-integer turn cap", () => {
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "0" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "-3" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "1.5" })).chatMaxTurns, 50);
    assert.strictEqual(loadConfig(baseEnv({ CHAT_MAX_TURNS: "  " })).chatMaxTurns, 50);
  });
});

describe("loadConfig WORKER_MAX_CONCURRENT_RUNS (PRD #42 Decision 3)", () => {
  it("defaults to 1 (the pre-#42 serial run lane) when unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).maxConcurrentRuns, 1);
  });

  it("parses a valid integer cap", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "4" })).maxConcurrentRuns, 4);
  });

  it("honors a value above the soft ceiling (warn, not clamp — the warn lives in main.ts)", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "20" })).maxConcurrentRuns, 20);
  });

  it("falls back to 1 on a blank, zero, negative, or fractional value", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "  " })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "0" })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "-2" })).maxConcurrentRuns, 1);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_MAX_CONCURRENT_RUNS: "2.5" })).maxConcurrentRuns, 1);
  });
});

describe("loadConfig CHECKPOINT_INTERVAL (PRD #267)", () => {
  it("defaults to 20 minutes when unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).checkpointIntervalMs, 20 * 60_000);
  });

  it("treats 0 as disabled (the time-based publish path is off)", () => {
    assert.strictEqual(loadConfig(baseEnv({ CHECKPOINT_INTERVAL: "0" })).checkpointIntervalMs, 0);
  });

  it("parses a Go-style duration string", () => {
    assert.strictEqual(
      loadConfig(baseEnv({ CHECKPOINT_INTERVAL: "30s" })).checkpointIntervalMs,
      30_000,
    );
  });
});

// PRD #108 M6: UZI_HOME_RECLAIM gates a DESTRUCTIVE startup sweep and ships ON, so
// its polarity is the whole point — a default-on safety feature that a deployment
// can disable without meaning to is worse than no kill switch at all.
describe("loadConfig UZI_HOME_RECLAIM (PRD #108 M6)", () => {
  it("defaults ON when unset", () => {
    assert.strictEqual(loadConfig(baseEnv()).homeReclaimEnabled, true);
  });

  it("treats SET-BUT-EMPTY as unset, not as off", () => {
    // The finding this test exists for. `parseBool` accepts only 1|true|yes, so an
    // empty value would read as "off" — and empty is exactly how the var arrives
    // from a compose `${UZI_HOME_RECLAIM:-}` or a Helm value defaulting to "".
    // That would silently disable the sweep on every deployment that merely
    // mentions the variable.
    assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: "" })).homeReclaimEnabled, true);
    assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: "   " })).homeReclaimEnabled, true);
  });

  it("turns OFF only on an explicit falsy value", () => {
    for (const v of ["0", "false", "no", "off", "FALSE", " Off "]) {
      assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: v })).homeReclaimEnabled, false, `value ${JSON.stringify(v)}`);
    }
  });

  it("stays ON for the affirmative spellings", () => {
    for (const v of ["1", "true", "yes", "TRUE", " On "]) {
      assert.strictEqual(loadConfig(baseEnv({ UZI_HOME_RECLAIM: v })).homeReclaimEnabled, true, `value ${JSON.stringify(v)}`);
    }
  });
});

// PRD #1391 M1: the outbox knobs. These pin the VALUES (not just "parses to a
// number"), so a wrong multiplier — a retention window that is hours not days, or a
// MiB that is really 1000*1000 — fails here rather than shipping silently.
describe("loadConfig outbox knobs (PRD #1391 M1)", () => {
  it("applies the documented defaults", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.outboxRetentionMs, 7 * 24 * 3600 * 1000, "default retention is 7 days in ms");
    assert.strictEqual(c.outboxRunMaxBytes, 64 * 1024 * 1024, "default per-run quota is 64 MiB");
    assert.strictEqual(c.outboxMaxBytes, 512 * 1024 * 1024, "default worker-total quota is 512 MiB");
    assert.strictEqual(c.outboxDataDir, path.join("/data", "outbox"), "outbox tree is <dataDir>/outbox");
  });

  it("derives outboxDataDir from UZI_DATA_DIR", () => {
    assert.strictEqual(loadConfig(baseEnv({ UZI_DATA_DIR: "/srv/uzi" })).outboxDataDir, path.join("/srv/uzi", "outbox"));
  });

  it("computes the retention window from the d (days) unit", () => {
    // 5d must be 5*86_400_000, not 5*3_600_000 (hours) or a bare 5.
    assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_RETENTION: "5d" })).outboxRetentionMs, 432_000_000);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_RETENTION: "12h" })).outboxRetentionMs, 12 * 3_600_000);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_RETENTION: "1d" })).outboxRetentionMs, 86_400_000);
  });

  it("honors the byte-quota env overrides", () => {
    const c = loadConfig(baseEnv({ WORKER_OUTBOX_RUN_MAX_BYTES: "1048576", WORKER_OUTBOX_MAX_BYTES: "5242880" }));
    assert.strictEqual(c.outboxRunMaxBytes, 1_048_576);
    assert.strictEqual(c.outboxMaxBytes, 5_242_880);
  });

  it("falls back to the byte-quota defaults on a blank, zero, or non-integer value", () => {
    for (const v of ["  ", "0", "-1", "1.5"]) {
      assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_RUN_MAX_BYTES: v })).outboxRunMaxBytes, 64 * 1024 * 1024);
      assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_MAX_BYTES: v })).outboxMaxBytes, 512 * 1024 * 1024);
    }
  });
});

// PRD #1391 M2: the spill knobs. The trip window MUST default to the batcher's baked
// TRANSIENT_TRIP_MS so the two constants can never drift (a lower default here would
// spill healthy runs on an ordinary api restart); the spill-buffer cap defaults to 2
// MiB. Both are operator-tunable so the e2e outage phase can lower the trip window.
describe("loadConfig spill knobs (PRD #1391 M2)", () => {
  it("defaults transientTripMs to the batcher's TRANSIENT_TRIP_MS", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.transientTripMs, TRANSIENT_TRIP_MS, "the default must equal the batcher's baked constant");
    assert.strictEqual(c.transientTripMs, 10 * 60_000, "and that constant is ten minutes");
  });

  it("honors WORKER_TRANSIENT_TRIP_MS as a Go-style duration (and a bare-number = ms)", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_TRANSIENT_TRIP_MS: "5m" })).transientTripMs, 300_000);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_TRANSIENT_TRIP_MS: "30s" })).transientTripMs, 30_000);
    // The e2e phase lowers it far below ten minutes so the outage case need not wait.
    assert.strictEqual(loadConfig(baseEnv({ WORKER_TRANSIENT_TRIP_MS: "50" })).transientTripMs, 50);
    assert.strictEqual(loadConfig(baseEnv({ WORKER_TRANSIENT_TRIP_MS: "50ms" })).transientTripMs, 50);
  });

  it("falls back to the default trip window on a blank value", () => {
    assert.strictEqual(loadConfig(baseEnv({ WORKER_TRANSIENT_TRIP_MS: "  " })).transientTripMs, TRANSIENT_TRIP_MS);
  });

  it("defaults outboxSpillBufferBytes to 2 MiB", () => {
    assert.strictEqual(loadConfig(baseEnv()).outboxSpillBufferBytes, 2 * 1024 * 1024, "MiB, not 2 * 1000 * 1000");
  });

  it("honors WORKER_OUTBOX_SPILL_BUFFER_BYTES and falls back on a blank/zero/non-integer", () => {
    assert.strictEqual(
      loadConfig(baseEnv({ WORKER_OUTBOX_SPILL_BUFFER_BYTES: "1048576" })).outboxSpillBufferBytes,
      1_048_576,
    );
    for (const v of ["  ", "0", "-1", "1.5"]) {
      assert.strictEqual(
        loadConfig(baseEnv({ WORKER_OUTBOX_SPILL_BUFFER_BYTES: v })).outboxSpillBufferBytes,
        2 * 1024 * 1024,
        `value ${JSON.stringify(v)} falls back`,
      );
    }
  });
});

// PRD #1391 M3 (Run B): the terminal-journal knobs. These pin the VALUES so a wrong
// multiplier — the cap in MB-not-MiB, or the reserve count as bytes — fails here.
describe("loadConfig terminal-journal knobs (PRD #1391 M3)", () => {
  it("applies the documented defaults", () => {
    const c = loadConfig(baseEnv());
    assert.strictEqual(c.outboxTerminalMaxBytes, Math.round(1.25 * 1024 * 1024), "1.25 MiB rounded to a whole byte");
    assert.strictEqual(c.outboxTerminalMaxBytes, 1_310_720, "1.25 MiB is exactly 1_310_720 bytes");
    assert.strictEqual(c.outboxReserveTerminals, 4, "four hard-max journals fit the reserve by default");
    assert.strictEqual(c.gapFillMax, 10000, "the gap fill is bounded at 10,000 seqs");
  });

  it("honors the env overrides", () => {
    const c = loadConfig(
      baseEnv({
        WORKER_OUTBOX_TERMINAL_MAX_BYTES: "2097152",
        WORKER_OUTBOX_RESERVE_TERMINALS: "6",
        WORKER_GAP_FILL_MAX: "500",
      }),
    );
    assert.strictEqual(c.outboxTerminalMaxBytes, 2_097_152);
    assert.strictEqual(c.outboxReserveTerminals, 6);
    assert.strictEqual(c.gapFillMax, 500);
  });

  it("falls back to the defaults on a blank, zero, negative, or non-integer value", () => {
    for (const v of ["  ", "0", "-1", "1.5"]) {
      assert.strictEqual(
        loadConfig(baseEnv({ WORKER_OUTBOX_TERMINAL_MAX_BYTES: v })).outboxTerminalMaxBytes,
        Math.round(1.25 * 1024 * 1024),
        `terminal cap ${JSON.stringify(v)} falls back`,
      );
      assert.strictEqual(loadConfig(baseEnv({ WORKER_OUTBOX_RESERVE_TERMINALS: v })).outboxReserveTerminals, 4);
      assert.strictEqual(loadConfig(baseEnv({ WORKER_GAP_FILL_MAX: v })).gapFillMax, 10000);
    }
  });
});
