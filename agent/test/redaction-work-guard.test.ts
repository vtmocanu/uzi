import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { createLogger } from "../src/log.js";
import { recordingLogger } from "./helpers.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { buildDeliveryContext, type DeliveryContextGit } from "../src/pr-description-context.js";
import {
  literalsPresent, makeRedactor, makeTextRedactor, observeRedactionWork, PER_SECRET_MAX, REDACTION_WITHHELD,
  REDACTION_WORK_BUDGET, replaceSpans, SecretSet, spansOfEach, type RedactionWork,
} from "../src/redact.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import { patchScanVerdict } from "../src/run-env-scan.js";

// PRD 4416, owner decision B3: a bounded work guard inside the matcher. The periodic worst case (a 32 KiB literal
// against 64 KiB of periodic output) took seconds on one host; the guard must bound it by COUNTED work, not by the
// clock, so every assertion below is on the charged-unit count and on the fail-closed output.

const REDACTED = "***REDACTED***";
const LONG = "a".repeat(32 * 1024 - 1) + "b";
const PERIODIC = "a".repeat(64 * 1024);
/** Never more than the budget plus the overflowing charge, which is at most 1 + the longest literal. */
const CEILING = REDACTION_WORK_BUDGET + 1 + LONG.length;

// Fillers for the indexed (> PER_SECRET_MAX literals) path, assembled at runtime from parts.
const fillers = (n: number): string[] => Array.from({ length: n }, (_, i) => ["dummy", "filler", String(i).padStart(4, "0"), "zz"].join("-"));
const SHORT_SECRET = ["short", "log", "secret", "0009"].join("-");
const MANY = [LONG, ...fillers(PER_SECRET_MAX + 1)];

const observed: RedactionWork[] = [];
observeRedactionWork((w) => observed.push({ ...w }));
afterEach(() => { observed.length = 0; });

/** The charged work of the single redaction `fn` ran. */
function chargedBy(fn: () => unknown): number {
  observed.length = 0;
  fn();
  assert.equal(observed.length, 1, "one budgeted redaction");
  return observed[0]!.charged;
}

function captureLines(fn: () => void): string[] {
  const chunks: string[] = [];
  const origOut = process.stdout.write;
  const origErr = process.stderr.write;
  const grab = (c: unknown): boolean => { chunks.push(typeof c === "string" ? c : String(c)); return true; };
  process.stdout.write = grab as unknown as typeof process.stdout.write;
  process.stderr.write = grab as unknown as typeof process.stderr.write;
  try { fn(); } finally { process.stdout.write = origOut; process.stderr.write = origErr; }
  return chunks.join("").split("\n").filter((l) => l !== "");
}

describe("the matcher's work guard, per redaction consumer", () => {
  it("makeTextRedactor withholds the whole periodic output and charges at most budget + one literal", () => {
    const text = makeTextRedactor([LONG]);
    let out = "";
    const charged = chargedBy(() => { out = text(PERIODIC); });
    assert.equal(out, REDACTION_WITHHELD);
    assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `charged ${charged}`);
  });

  it("SecretSet.text withholds the periodic output on the per-secret path", () => {
    const set = new SecretSet([LONG]);
    let out = "";
    const charged = chargedBy(() => { out = set.text(PERIODIC); });
    assert.equal(out, REDACTION_WITHHELD);
    assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `charged ${charged}`);
  });

  it("the indexed path (more than PER_SECRET_MAX literals) withholds through both redactors", () => {
    assert.ok(MANY.length > PER_SECRET_MAX);
    let out = "";
    let charged = chargedBy(() => { out = makeTextRedactor(MANY)(PERIODIC); });
    assert.equal(out, REDACTION_WITHHELD);
    assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `charged ${charged}`);
    const set = new SecretSet(MANY);
    charged = chargedBy(() => { out = set.text(PERIODIC); });
    assert.equal(out, REDACTION_WITHHELD);
    assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `charged ${charged}`);
  });

  it("an overflow is withheld whole even when the text also holds a real occurrence", () => {
    const text = makeTextRedactor([LONG, "real-secret-value-1"]);
    const out = text(`${PERIODIC} real-secret-value-1`);
    assert.equal(out, REDACTION_WITHHELD);
    assert.ok(!out.includes("real-secret"));
  });

  it("the feed: a payload string over budget is withheld string by string, short fields stay intact", () => {
    const payload = { kind: "tool_result", big: PERIODIC, nested: { note: "short field", list: [PERIODIC, "keep me"] }, n: 7 };
    const expected = { kind: "tool_result", big: REDACTION_WITHHELD, nested: { note: "short field", list: [REDACTION_WITHHELD, "keep me"] }, n: 7 };
    assert.deepEqual(makeRedactor([LONG])(payload), expected);
    assert.deepEqual(new SecretSet([LONG]).payload(payload), expected);
    assert.deepEqual(makeRedactor(MANY)(payload), expected);
  });

  it("one payload shares ONE budget: several periodic strings charge at most budget + 1 + one literal in total", () => {
    const payload = {
      kind: "tool_result", note: "short field",
      a: PERIODIC, b: PERIODIC, nested: { c: PERIODIC, keep: "keep me" }, list: [PERIODIC, "ok"],
    };
    const expected = {
      kind: "tool_result", note: "short field",
      a: REDACTION_WITHHELD, b: REDACTION_WITHHELD, nested: { c: REDACTION_WITHHELD, keep: "keep me" }, list: [REDACTION_WITHHELD, "ok"],
    };
    const redactors: Array<[string, (p: Record<string, unknown>) => Record<string, unknown>]> = [
      ["makeRedactor", makeRedactor([LONG])],
      ["makeRedactor indexed", makeRedactor(MANY)],
      ["SecretSet.payload", new SecretSet([LONG]).payload],
      ["SecretSet.payload indexed", new SecretSet(MANY).payload],
    ];
    for (const [label, redact] of redactors) {
      let out: Record<string, unknown> = {};
      const charged = chargedBy(() => { out = redact(payload); });
      assert.deepEqual(out, expected, label);
      assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `${label}: charged ${charged}`);
    }
  });

  it("the feed through the real batcher: the posted message carries the withheld marker, not the output", async () => {
    const sent: OutgoingMessage[] = [];
    const client = { async postMessages(_runId: string, messages: OutgoingMessage[]): Promise<void> { sent.push(...messages); } } as unknown as WorkerClient;
    const { logger } = recordingLogger();
    const set = new SecretSet([LONG]);
    const batcher = new MessageBatcher(client, "run-feed", 0, 0, logger, set.payload, set.text);
    batcher.emit({ kind: "tool_result", agent: "lead", payload: { tool_use_id: "tu1", content: PERIODIC, is_error: false } });
    await batcher.close();
    assert.equal(sent.length, 1);
    assert.equal(sent[0]!.payload.content, REDACTION_WITHHELD);
    assert.equal(sent[0]!.payload.tool_use_id, "tu1");
    assert.ok(!JSON.stringify(sent).includes("aaaaaaaa"));
  });

  it("only the across-invisible pass overflows: the whole string is withheld", () => {
    // The raw text never holds the literal's 8-unit prefix, so the first pass is cheap; its visible projection is periodic.
    const text = "aaa\u200b".repeat(16 * 1024);
    const first: RedactionWork = { charged: 0 };
    spansOfEach([LONG])(text, first);
    assert.ok(first.charged < REDACTION_WORK_BUDGET / 1000, `first pass charged ${first.charged}`);
    for (const [label, redact] of [
      ["makeTextRedactor", makeTextRedactor([LONG])],
      ["makeTextRedactor indexed", makeTextRedactor(MANY)],
      ["SecretSet.text", new SecretSet([LONG]).text],
    ] as const) {
      let out = "";
      const charged = chargedBy(() => { out = redact(text); });
      assert.equal(out, REDACTION_WITHHELD, label);
      assert.ok(charged > REDACTION_WORK_BUDGET && charged <= CEILING, `${label}: charged ${charged}`);
    }
  });

  it("the feed: a member NAME over budget is withheld too, never kept raw", () => {
    const out = makeRedactor([LONG])({ [PERIODIC]: "v" });
    assert.deepEqual(Object.keys(out), [REDACTION_WITHHELD]);
  });

  it("the logger: an over-budget record becomes a fixed valid JSON line; a normal line still masks exactly", () => {
    for (const extra of [[], fillers(PER_SECRET_MAX + 1)]) {
      const log = createLogger("info");
      log.addSecret(LONG);
      log.addSecret(SHORT_SECRET);
      for (const f of extra) log.addSecret(f);
      const lines = captureLines(() => {
        log.child({ run_id: "r-1" }).info("big output", { out: PERIODIC });
        log.child({ run_id: "r-1" }).warn("normal line", { token: SHORT_SECRET, other: 1 });
      });
      assert.equal(lines.length, 2);
      const withheld = JSON.parse(lines[0]!) as Record<string, unknown>;
      assert.deepEqual(Object.keys(withheld).sort(), ["level", "msg", "ts"]);
      assert.equal(withheld.msg, "log record withheld: redaction work budget exceeded");
      assert.equal(withheld.level, "info");
      assert.ok(!lines[0]!.includes("aaaaaaaa"));
      const normal = JSON.parse(lines[1]!) as Record<string, unknown>;
      assert.equal(normal.token, REDACTED);
      assert.equal(normal.run_id, "r-1");
      assert.equal(normal.other, 1);
    }
  });

  it("the logger: a ts in base or call fields never replaces the logger's timestamp on the withheld line", () => {
    const TS_SECRET = ["ts", "leak", "secret", "0042"].join("-");
    const cases: Array<[string, (log: ReturnType<typeof createLogger>) => void]> = [
      ["call field", (log) => log.info("x", { ts: TS_SECRET, out: PERIODIC })],
      ["child base", (log) => log.child({ ts: TS_SECRET }).info("x", { out: PERIODIC })],
    ];
    for (const [label, call] of cases) {
      const log = createLogger("info");
      log.addSecret(LONG);
      log.addSecret(TS_SECRET);
      observed.length = 0;
      const before = Date.now();
      const lines = captureLines(() => call(log));
      const after = Date.now();
      assert.equal(lines.length, 1, label);
      const rec = JSON.parse(lines[0]!) as Record<string, unknown>;
      assert.deepEqual(Object.keys(rec).sort(), ["level", "msg", "ts"], label);
      assert.equal(rec.msg, "log record withheld: redaction work budget exceeded", label);
      assert.equal(rec.level, "info", label);
      const ts = String(rec.ts);
      assert.notEqual(ts, TS_SECRET, label);
      assert.match(ts, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/, label);
      const parsed = Date.parse(ts);
      assert.ok(parsed >= before && parsed <= after, `${label}: ts ${ts}`);
      assert.ok(!lines[0]!.includes(TS_SECRET), label);
      const overBudget = observed.filter((w) => w.charged > REDACTION_WORK_BUDGET);
      assert.equal(overBudget.length, 1, `${label}: exactly one over-budget redaction, saw ${observed.length} total`);
      assert.equal(observed.length, 1, `${label}: no second scrub`);
    }
  });

  it("the logger: redactDiagnostic returns the marker for an over-budget diagnostic", () => {
    const log = createLogger("info");
    log.addSecret(LONG);
    assert.equal(log.redactDiagnostic!(PERIODIC), REDACTION_WITHHELD);
    assert.equal(log.redactDiagnostic!(`x ${LONG} y`), `x ${REDACTED} y`);
  });

  it("MR text: the delivery context withholds an over-budget part and keeps the others", async () => {
    const BASE = "b".repeat(40);
    const HEAD = "c".repeat(40);
    const git: DeliveryContextGit = {
      async sizeMergeBase() { return BASE; },
      async diffNumstatZ() { return ""; },
      async checkAttrZ() { return new Map(); },
      async readBare(_bare, args) { return { text: args[0] === "log" ? "feat: one\0" : "", truncated: false }; },
    };
    const ctx = await buildDeliveryContext({
      git, barePath: "/bare/repo.git", targetBranch: "main", headSha: HEAD, deadlineMs: Date.now() + 60_000,
      redact: makeTextRedactor([LONG]), issueTitle: "Add plain-English PR descriptions", issueBody: PERIODIC,
    });
    assert.ok(ctx.issue.includes(REDACTION_WITHHELD));
    assert.ok(ctx.commits.includes("feat: one"), "an unaffected part is kept");
    assert.ok(!JSON.stringify(ctx).includes("aaaaaaaa"));
  });

  it("the run-env scan: over budget every literal is reported present, so each verdict withholds", () => {
    const present = literalsPresent([LONG, "other-literal-value"]);
    assert.deepEqual([...present(PERIODIC)].sort(), [LONG, "other-literal-value"].sort());
    assert.deepEqual([...present("nothing relevant here at all")], []);
    const needles = runEnvClaimForms([{ var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "BIG_SECRET", value: LONG, secret: true, description: "" }], []).needles;
    const patch = `diff --git a/f.txt b/f.txt\nindex 1111111..2222222 100644\n--- a/f.txt\n+++ b/f.txt\n@@ -0,0 +1 @@\n+${PERIODIC}\n`;
    const verdict = patchScanVerdict(patch, needles, [], []);
    assert.equal(verdict.withhold, true);
  });
});

describe("controls: the guarded matcher still redacts exactly", () => {
  const SECRET_A = ["control", "secret", "alpha", "0001"].join("-");
  const SECRET_B = ["control", "secret", "beta", "0002"].join("-");

  it("masks real occurrences on the per-secret and indexed paths, charging far less than the budget", () => {
    const text = `x ${SECRET_A} y ${SECRET_B} z ${SECRET_A}${SECRET_A} ${"a".repeat(500)}`;
    const want = `x ${REDACTED} y ${REDACTED} z ${REDACTED}${REDACTED} ${"a".repeat(500)}`;
    const small = makeTextRedactor([SECRET_A, SECRET_B]);
    let out = "";
    let charged = chargedBy(() => { out = small(text); });
    assert.equal(out, want);
    assert.ok(charged < REDACTION_WORK_BUDGET / 1000, `charged ${charged}`);
    const indexed = makeTextRedactor([SECRET_A, SECRET_B, ...fillers(PER_SECRET_MAX + 1)]);
    charged = chargedBy(() => { out = indexed(text); });
    assert.equal(out, want);
    assert.ok(charged < REDACTION_WORK_BUDGET / 1000, `charged ${charged}`);
    assert.equal(new SecretSet([SECRET_A, SECRET_B]).text(text), want);
  });

  it("a long literal that really occurs is masked, within budget, when the text is not periodic", () => {
    const out = makeTextRedactor([LONG])(`head ${LONG} tail`);
    assert.equal(out, `head ${REDACTED} tail`);
  });

  it("the logger masks a registered secret and its JSON-escaped form", () => {
    const log = createLogger("info");
    const quoted = ["with", "\"quote", "and\\slash", "0003"].join("-");
    log.addSecret(quoted);
    const [line] = captureLines(() => log.info("m", { v: quoted, plain: SECRET_A }));
    assert.ok(!line!.includes("quote"));
    const parsed = JSON.parse(line!) as Record<string, unknown>;
    assert.equal(parsed.v, REDACTED);
    assert.equal(parsed.plain, SECRET_A);
  });

  it("spansOfEach finds exactly the spans of the naive per-secret indexOf scan (differential)", () => {
    let seed = 12345;
    const rnd = (n: number): number => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
    const naive = (secrets: string[], text: string): Array<[number, number]> => {
      const spans: Array<[number, number]> = [];
      for (const secret of secrets) {
        for (let at = text.indexOf(secret); at !== -1; at = text.indexOf(secret, at + 1)) spans.push([at, at + secret.length]);
      }
      return spans.sort((a, b) => a[0] - b[0]);
    };
    for (let round = 0; round < 300; round++) {
      const alphabet = "ab";
      const word = (len: number): string => Array.from({ length: len }, () => alphabet[rnd(alphabet.length)]).join("");
      const secrets = Array.from({ length: 1 + rnd(4) }, () => word(8 + rnd(10)));
      const text = word(40 + rnd(200));
      const work: RedactionWork = { charged: 0 };
      const got = spansOfEach(secrets)(text, work);
      const want = naive(secrets, text);
      assert.deepEqual(got.map((s) => s.join(":")).sort(), want.map((s) => s.join(":")).sort(), `round ${round}`);
      assert.equal(replaceSpans(text, got), replaceSpans(text, want));
    }
  });
});
