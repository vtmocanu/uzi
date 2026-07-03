import { randomUUID } from "node:crypto";
import type { Logger } from "../src/log.js";
import type { ClaimResponse } from "../src/protocol.js";

/** A no-op logger so tests don't spray JSON lines into the reporter. */
export function nullLogger(): Logger {
  const self: Logger = {
    debug() {},
    info() {},
    warn() {},
    error() {},
    addSecret() {},
    child() {
      return self;
    },
  };
  return self;
}

/** A logger that captures every emitted record (incl. child fields) into `lines`
 *  so a test can assert a secret never appears in any log output. */
export function recordingLogger(): { logger: Logger; lines: unknown[] } {
  const lines: unknown[] = [];
  const make = (base: Record<string, unknown>): Logger => {
    const record = (level: string, msg: string, fields?: Record<string, unknown>) =>
      lines.push({ level, msg, ...base, ...fields });
    const self: Logger = {
      debug: (m, f) => record("debug", m, f),
      info: (m, f) => record("info", m, f),
      warn: (m, f) => record("warn", m, f),
      error: (m, f) => record("error", m, f),
      addSecret() {},
      child: (fields) => make({ ...base, ...fields }),
    };
    return self;
  };
  return { logger: make({}), lines };
}

/** A minimal valid claim; override any field per test. */
export function makeClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return {
    run_id: randomUUID(),
    issue_iid: 1,
    issue_title: "test issue",
    issue_description: "do the thing",
    repo: { id: randomUUID(), url: "https://example.test/org/repo.git" },
    // Deliberately NOT a real `glpat-` shape so secret scanners don't flag the fixture.
    secrets: { forge_pat: "fixture-forge-pat-000000" },
    last_seq: 0,
    ...overrides,
  };
}
