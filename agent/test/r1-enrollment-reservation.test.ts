import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const runA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const runB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
const runC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
type Mode = "BOOT" | "RUNTIME";

async function fixture(t: TestContext) {
  const dir = makeScratchRoot("r1-enrollment-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const make = () => new Outbox({ root, log: nullLogger(), maxBytes: 1e6, runMaxBytes: 1e6, retentionMs: 1 });
  const subject = make();
  await subject.init();
  const signer = make();
  await signer.init();
  assert.equal((await signer.journalTerminal(runB, 1, "running", 0, { status: "completed" })).journaled, true);
  assert.deepEqual(subject.listPendingTerminals(), [], "B is unrepresented in the initialized subject");
  return { subject, root, file: path.join(root, runB, "terminal-1.json") };
}

function occupy(subject: Outbox, count: number): symbol[] {
  const tokens: symbol[] = [];
  for (let generation = 1; generation <= count; generation++) {
    const token = subject.reservePolicyFlight(runA, generation);
    assert.ok(token, `default tier must allocate producer reservation ${generation}`);
    tokens.push(token);
  }
  assert.equal(tokens.length, count);
  assert.equal(subject.listPendingTerminals().some(p => p.run_id === runA), false,
    "unused producer reservations are not terminals");
  return tokens;
}

async function scan(subject: Outbox, mode: Mode) {
  if (mode === "BOOT") {
    const loader = subject as unknown as {
      loadTerminals(id: string): Promise<{ authenticated: number; rejected: number }>;
    };
    return (await loader.loadTerminals(runB)).authenticated;
  }
  const page = await subject.scanTerminalObservationsPage();
  return page.observations.filter(o => o.runId === runB && o.kind === "authenticated").length;
}

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

async function ready(promise: Promise<void>) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("B directory read did not enter within 5s")), 5000);
    })]);
  } finally { if (timer) clearTimeout(timer); }
}

// Delegate real I/O, but gate delivery of the actual Dirent. Both manual read()
// and native-for-await callers must pass through the same controlled read.
function gateRead(t: TestContext, directory: string) {
  const readEntered = deferred();
  const releaseRead = deferred();
  const open = fs.opendir.bind(fs);
  let paused = false;
  t.mock.method(fs, "opendir", async (file: Parameters<typeof fs.opendir>[0]) => {
    const realDir = await open(file);
    if (String(file) !== directory) return realDir;
    let closed = false;
    const close = async () => {
      if (closed) return;
      closed = true;
      await realDir.close();
    };
    const read = async () => {
      const entry = await realDir.read();
      if (entry?.name === "terminal-1.json" && !paused) {
        paused = true;
        readEntered.resolve();
        await releaseRead.promise;
      }
      return entry;
    };
    return new Proxy(realDir, { get(target, key) {
      if (key === "read") return read;
      if (key === "close") return close;
      if (key === Symbol.asyncIterator) return async function* () {
        try {
          for (;;) {
            const entry = await read();
            if (!entry) return;
            yield entry;
          }
        } finally { await close(); }
      };
      const value = Reflect.get(target, key);
      return typeof value === "function" ? value.bind(target) : value;
    } });
  });
  return { readEntered: readEntered.promise, releaseRead: releaseRead.resolve };
}

for (const mode of ["BOOT", "RUNTIME"] as const) {
  it(`R1 ${mode} reserves the actual last slot before receiving B's directory entry`, async t => {
    const { subject, root, file } = await fixture(t);
    const tokens = occupy(subject, 1023);
    const gate = gateRead(t, path.join(root, runB));
    // Only the ordering probe uses a fixed cooperative clock; I/O and the 5s
    // readiness bound remain real. Production keeps its one-second deadline.
    const clock = t.mock.method(Date, "now", () => 1000);
    const joined = scan(subject, mode).then(value => ({ value, error: undefined }),
      error => ({ value: undefined, error }));
    let competitor: symbol | undefined;
    try {
      await ready(gate.readEntered);
      competitor = subject.reservePolicyFlight(runC, 1);
      assert.equal(competitor, undefined, "B must reserve slot 1024 BEFORE its raw read returns");
      assert.equal(subject.listPendingTerminals().some(p => p.run_id === runA), false);
      gate.releaseRead();
      const result = await joined;
      assert.equal(result.error, undefined);
      assert.equal(result.value, 1, "B must authenticate after the gated I/O joins");
      clock.mock.restore();
      assert.equal((await subject.readTerminalJournal(runB, 1))?.body.status, "completed");
      competitor = subject.reservePolicyFlight(runC, 1);
      assert.ok(competitor, "authenticated ordinary B releases slot 1024 after I/O joins");
      assert.equal(subject.reservePolicyFlight(runC, 2), undefined, "the default tier never grows past 1024");

      await subject.closeTerminalObservationScan();
      assert.equal(await scan(subject, mode), 1, "represented ordinary B reauthenticates at true full capacity");
      await subject.closeTerminalObservationScan();
      const bytes = await fs.readFile(file);
      await fs.writeFile(file, bytes);
      assert.equal(await scan(subject, mode), 0, "a changed ordinary lifetime needs new enrollment capacity");
      await subject.closeTerminalObservationScan();
      subject.releasePolicyFlight(competitor);
      competitor = undefined;
      assert.equal(await scan(subject, mode), 1, "released capacity permits the changed ordinary lifetime");
    } finally {
      gate.releaseRead();
      await joined; // Join even when the contention assertion fails on the old tree.
      clock.mock.restore();
      if (competitor) subject.releasePolicyFlight(competitor);
      for (const token of tokens) subject.releasePolicyFlight(token);
      await subject.closeTerminalObservationScan();
    }
  });

  it(`R1 ${mode} withholds the last slot until the real descriptor close joins`, async t => {
    const { subject, file } = await fixture(t);
    const tokens = occupy(subject, 1023);
    const entered = deferred();
    const release = deferred();
    const open = fs.open.bind(fs);
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const handle = await open(...args);
      if (String(args[0]) === file) {
        const close = handle.close.bind(handle);
        handle.close = async () => {
          entered.resolve();
          await release.promise;
          await close();
        };
      }
      return handle;
    });
    const clock = t.mock.method(Date, "now", () => 1000);
    const joined = scan(subject, mode).then(value => ({ value, error: undefined }),
      error => ({ value: undefined, error }));
    let competitor: symbol | undefined;
    try {
      await ready(entered.promise);
      competitor = subject.reservePolicyFlight(runC, 1);
      assert.equal(competitor, undefined, "enrollment owns slot 1024 until physical close joins");
      assert.ok(subject.listPendingTerminals().some(p =>
        p.run_id === runB && p.outcome_kind === "unknown"));
      release.resolve();
      const result = await joined;
      assert.equal(result.error, undefined);
      assert.equal(result.value, 1);
      competitor = subject.reservePolicyFlight(runC, 1);
      assert.ok(competitor, "joined ordinary authentication makes the last slot reusable");
      assert.equal(subject.reservePolicyFlight(runC, 2), undefined);
    } finally {
      release.resolve();
      await joined; // Join real I/O even if the pre-close capacity assertion fails.
      clock.mock.restore();
      if (competitor) subject.releasePolicyFlight(competitor);
      for (const token of tokens) subject.releasePolicyFlight(token);
      await subject.closeTerminalObservationScan();
    }
  });

  it(`R1 ${mode} keeps enrollment conservative when descriptor close rejects`, async t => {
    const { subject, file } = await fixture(t);
    const tokens = occupy(subject, 1023);
    const open = fs.open.bind(fs);
    let closes = 0;
    t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
      const handle = await open(...args);
      if (String(args[0]) === file) {
        const close = handle.close.bind(handle);
        handle.close = async () => {
          closes++;
          await close(); // Join the real descriptor even on the injected failure.
          throw new Error("injected close failure");
        };
      }
      return handle;
    });
    let competitor: symbol | undefined;
    try {
      assert.equal(await scan(subject, mode), 0);
      assert.equal(closes, 1, "failed close is not retried by finally");
      assert.ok(subject.listPendingTerminals().some(p =>
        p.run_id === runB && p.outcome_kind === "unknown"));
      competitor = subject.reservePolicyFlight(runC, 1);
      assert.equal(competitor, undefined, "close rejection cannot release enrollment capacity");
    } finally {
      if (competitor) subject.releasePolicyFlight(competitor);
      for (const token of tokens) subject.releasePolicyFlight(token);
      await subject.closeTerminalObservationScan();
    }
  });

  it(`R1 ${mode} retains actually enrolled unknown B across disappearance and cursor resets`, async t => {
    const { subject, file } = await fixture(t);
    await fs.writeFile(file, "unknown");
    const tokens = occupy(subject, 1023);
    try {
      assert.equal(await scan(subject, mode), 0);
      assert.ok(subject.listPendingTerminals().some(p =>
        p.run_id === runB && p.claim_generation === 1 && p.outcome_kind === "unknown"));
      assert.equal(subject.reservePolicyFlight(runC, 1), undefined, "enrolled B owns the last slot");
      await fs.unlink(file);
      await subject.closeTerminalObservationScan();
      await scan(subject, mode);
      await subject.closeTerminalObservationScan();
      assert.equal(await subject.hasPendingPolicyOutcome(runB), true,
        "absence and scan close cannot discard an acquired semantic obligation");
    } finally {
      for (const token of tokens) subject.releasePolicyFlight(token);
      await subject.closeTerminalObservationScan();
    }
  });

  it(`R1 ${mode} defers an uninterpreted full-tier frontier without inventing an obligation`, async t => {
    const { subject, file } = await fixture(t);
    const tokens = occupy(subject, 1024);
    try {
      assert.equal(await scan(subject, mode), 0);
      assert.deepEqual(subject.listPendingTerminals(), []);
      await fs.unlink(file);
      await subject.closeTerminalObservationScan();
      await scan(subject, mode);
      await subject.closeTerminalObservationScan();
      assert.equal(await subject.hasPendingPolicyOutcome(runB), false,
        "a raw deferred name alone did not acquire a semantic identity");
    } finally {
      for (const token of tokens) subject.releasePolicyFlight(token);
      await subject.closeTerminalObservationScan();
    }
  });
}
