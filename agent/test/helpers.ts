import { randomUUID } from "node:crypto";
import type { Logger } from "../src/log.js";
import type { WorkerClient } from "../src/client.js";
import { ChatSteering, SteeringChannel } from "../src/steering.js";
import type { ClaimResponse, UserInput } from "../src/protocol.js";

/** A no-op logger so tests don't spray JSON lines into the reporter. */
export function nullLogger(): Logger {
  const self: Logger = {
    debug() {},
    info() {},
    warn() {},
    error() {},
    addSecret() {},
    removeSecret() {},
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
      removeSecret() {},
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
    repo: {
      id: randomUUID(),
      url: "https://example.test/org/repo",
      clone_url: "https://example.test/org/repo.git",
    },
    // Deliberately NOT a real `glpat-` shape so secret scanners don't flag the fixture.
    secrets: { forge_pat: "fixture-forge-pat-000000" },
    last_seq: 0,
    agents: [],
    ...overrides,
  };
}

/** Issue #1673: give a scripted getInputs fake the receipt endpoints. Every ACK and applied
 *  receipt succeeds on the active claim and echoes the rows of the latest GET. */
export function withReceipts(client: WorkerClient): WorkerClient {
  let latest: UserInput[] = [];
  const getInputs = client.getInputs.bind(client);
  return {
    ...client,
    getInputs: async (runId: string) => {
      const result = await getInputs(runId);
      latest = result.inputs;
      return result;
    },
    ackInputs: async (_runId: string, ids: number[]) => ({
      inputs: latest.filter((row) => ids.includes(row.id)).sort((a, b) => a.id - b.id),
      active: true,
    }),
    applyInputs: async () => ({ inputs: latest, active: true }),
  } as unknown as WorkerClient;
}

// Issue #1663: a failed assertion before `await ch.stop()` left a steering poll loop running,
// and node --test never finished the file. Every started channel is recorded here; a test file
// passes stopStartedChannels to afterEach. stop() is idempotent, so a test's own stop is unaffected.
const startedChannels = new Set<{ stop(): Promise<void> }>();
for (const Channel of [SteeringChannel, ChatSteering]) {
  const start = Channel.prototype.start;
  Channel.prototype.start = function (this: SteeringChannel & ChatSteering): void {
    startedChannels.add(this);
    start.call(this);
  };
}

export async function stopStartedChannels(): Promise<void> {
  const running = [...startedChannels];
  startedChannels.clear();
  await Promise.all(running.map((channel) => channel.stop()));
}
