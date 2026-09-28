import { randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { after } from "node:test";
import type { CanonicalReseedOptions, GitCacheOptions } from "../src/git.js";
import type { Logger } from "../src/log.js";
import type { WorkerClient } from "../src/client.js";
import { ChatSteering, SteeringChannel } from "../src/steering.js";
import type { ClaimResponse, UserInput } from "../src/protocol.js";

/** issue #1783 M3 (N5): the canonical-reseed options for suites whose subject is not the canonical
 *  free. The git layer requires them on every entry point (there is no unproven plain-`fs.rm`
 *  fallback); this one answers the process proof "quiescent" without scanning, so the free still
 *  runs its validation, the delete and the quarantine. */
export const noProofReseed: CanonicalReseedOptions = { beforeFree: async () => {} };

/** GitCache options for suites that create runner clones. Off Linux the real scratch
 *  provisioner fails closed, so this adds a plain-fs stand-in for the dev loop; on Linux
 *  it adds nothing and every suite exercises the real provisioner. Scratch-security
 *  suites construct GitCache without it. */
export function testGitCacheOptions(opts: GitCacheOptions = {}): GitCacheOptions {
  return process.platform === "linux" ? opts : { ...opts, scratchProvisioner: plainScratchProvisioner };
}

async function plainScratchProvisioner(clonePath: string): Promise<void> {
  fs.mkdirSync(path.join(clonePath, ".uzi", "scratch"), { recursive: true });
  const exclude = path.join(clonePath, ".git", "info", "exclude");
  fs.mkdirSync(path.dirname(exclude), { recursive: true });
  const existing = fs.existsSync(exclude) ? fs.readFileSync(exclude, "utf8") : "";
  if (existing.split("\n").includes("/.uzi/scratch/")) return;
  fs.appendFileSync(exclude, `${existing.length > 0 && !existing.endsWith("\n") ? "\n" : ""}/.uzi/scratch/\n`);
}

/** A factory of worktree paths that are never created, for executor suites that must
 *  not require the worktree on disk. Each path sits inside ONE per-call mkdtemp root,
 *  never directly in os.tmpdir(): the executor derives sibling dirs from a worktree
 *  path (skillsPluginDir writes `.uzi-skills-<basename>` next to it), so a bare
 *  os.tmpdir() child leaked that sibling into the scratch dir on every test (PRD #1809
 *  M2; `task test:agent` fails on any leftover). The root is removed by an after()
 *  hook registered where the factory is called: at module scope that is the file's
 *  end, inside a describe() that suite's end. */
export function nonexistentWorktreeFactory(prefix: string): () => string {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), `${prefix}-root-`));
  after(() => fs.rmSync(root, { recursive: true, force: true }));
  let seq = 0;
  return () => path.join(root, `${prefix}-wt-${seq++}`);
}

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
  // Every row any GET returned, by id: an applied receipt names exactly its ids, which since
  // issue #1604 need not be the last GET's batch (a revise or reject is applied on its own).
  const seen = new Map<number, UserInput>();
  const rowsFor = (ids: number[]): UserInput[] =>
    ids.flatMap((id) => (seen.has(id) ? [seen.get(id)!] : [])).sort((a, b) => a.id - b.id);
  const getInputs = client.getInputs.bind(client);
  return {
    ...client,
    getInputs: async (runId: string) => {
      const result = await getInputs(runId);
      for (const row of result.inputs) seen.set(row.id, row);
      return { ...result, receipts: true };
    },
    ackInputs: async (_runId: string, ids: number[]) => ({ inputs: rowsFor(ids), active: true }),
    applyInputs: async (_runId: string, ids: number[]) => ({ inputs: rowsFor(ids), active: true }),
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
