import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createLogger } from "../src/log.js";

/** Capture everything the logger writes to stdout+stderr while `fn` runs, so a
 *  test can assert what the real SecretRegistry scrubs (its scrub() is internal —
 *  the serialized line is the only public observation point). */
function capture(fn: () => void): string {
  const chunks: string[] = [];
  const origOut = process.stdout.write;
  const origErr = process.stderr.write;
  const grab = (c: unknown): boolean => {
    chunks.push(typeof c === "string" ? c : String(c));
    return true;
  };
  process.stdout.write = grab as unknown as typeof process.stdout.write;
  process.stderr.write = grab as unknown as typeof process.stderr.write;
  try {
    fn();
  } finally {
    process.stdout.write = origOut;
    process.stderr.write = origErr;
  }
  return chunks.join("");
}

describe("SecretRegistry — run-scoped, reference-counted eviction (PRD #42 Decision 7)", () => {
  it("keeps a shared secret scrubbed until its LAST holder is evicted", () => {
    const log = createLogger("info");
    // Same user ⇒ two concurrent runs register the identical PAT.
    const PAT = "shared-user-pat-00000000";
    log.addSecret(PAT); // run A
    log.addSecret(PAT); // run B (concurrent, same user)

    // Run A terminates and evicts its copy. Run B is still active, so the PAT MUST
    // stay scrubbed — this is the concurrency-safety property that a plain Set (or a
    // non-counted remove) would violate by un-scrubbing B's live credential.
    log.removeSecret(PAT);
    const whileBActive = capture(() => log.info("op", { header: PAT }));
    assert.ok(!whileBActive.includes(PAT), "PAT must stay scrubbed while a sibling run still holds it");
    assert.ok(whileBActive.includes("***REDACTED***"));

    // Run B terminates and evicts the last copy — only now is the string dropped.
    log.removeSecret(PAT);
    const afterBoth = capture(() => log.info("op", { header: PAT }));
    assert.ok(afterBoth.includes(PAT), "a fully-evicted secret is no longer scrubbed");
  });

  it("treats removeSecret of a never-added / worker-lifetime secret safely", () => {
    const log = createLogger("info");
    const JOIN = "worker-join-token-000000"; // added once in main(), never removed
    log.addSecret(JOIN);
    // A spurious evict for a value that was never added is a harmless no-op and must
    // not disturb the join token, which stays scrubbed for the whole process life.
    log.removeSecret("never-added-value-000000");
    log.removeSecret("never-added-value-000000");
    const line = capture(() => log.error("boom", { token: JOIN }));
    assert.ok(!line.includes(JOIN) && line.includes("***REDACTED***"), "join token stays scrubbed");
  });

  it("shares one registry across child loggers (evict on the parent scrubs on a child)", () => {
    const log = createLogger("info");
    const child = log.child({ run_id: "r1" });
    const SECRET = "run-scoped-secret-000000";
    log.addSecret(SECRET);
    const before = capture(() => child.info("x", { s: SECRET }));
    assert.ok(!before.includes(SECRET), "a child scrubs a secret registered on the parent");
    log.removeSecret(SECRET);
    const after = capture(() => child.info("x", { s: SECRET }));
    assert.ok(after.includes(SECRET), "the child stops scrubbing once the shared registry evicts it");
  });

  it("ignores too-short secrets (guard against corrupting unrelated output)", () => {
    const log = createLogger("info");
    log.addSecret("short"); // < 8 chars, must be ignored by both add and remove
    log.removeSecret("short");
    const line = capture(() => log.info("x", { s: "short and sweet" }));
    assert.ok(line.includes("short and sweet"), "a too-short value is never scrubbed");
  });
});

describe("SecretRegistry: JSON-escaped secrets (PRD #4320)", () => {
  it("redacts a secret containing a quote and a backslash even though JSON.stringify escapes both", () => {
    const log = createLogger("info");
    // Assembled at runtime: no provider-token-shaped literal in source.
    const secret = ["gw", 'key"with', "back\\slash", "tail-abcdefgh"].join("-");
    assert.ok(JSON.stringify(secret).slice(1, -1) !== secret, "the fixture really is escaped by JSON");
    log.addSecret(secret);
    const out = capture(() => log.info("failed", { detail: `gateway said ${secret}`, nested: { s: secret } }));
    assert.ok(!out.includes(secret), "raw form absent");
    assert.ok(!out.includes(JSON.stringify(secret).slice(1, -1)), "escaped form absent");
    assert.ok(out.includes("***REDACTED***"));
  });
});

describe("SecretRegistry indexed path (past 64 registered strings)", () => {
  const fill = (log: ReturnType<typeof createLogger>, n: number, tag: string): string[] => {
    const all = Array.from({ length: n }, (_, i) => `${tag}-filler-secret-${String(i).padStart(4, "0")}`);
    for (const s of all) log.addSecret(s);
    return all;
  };

  it("keeps a secret registered twice until it is removed twice", () => {
    const log = createLogger("info");
    fill(log, 70, "a");
    capture(() => log.info("warm", {}));
    const S = "twice-registered-secret-00";
    log.addSecret(S);
    log.addSecret(S);
    log.removeSecret(S);
    assert.ok(!capture(() => log.info("x", { v: S })).includes(S), "still masked after one removal");
    log.removeSecret(S);
    assert.ok(capture(() => log.info("x", { v: S })).includes(S), "unmasked after the last removal");
  });

  it("a secret added after the index exists is masked, and a removed one is not", () => {
    const log = createLogger("info");
    const all = fill(log, 70, "b");
    capture(() => log.info("warm", {}));
    const late = "late-secret-value-000000";
    log.addSecret(late);
    assert.ok(!capture(() => log.info("x", { v: late })).includes(late));
    log.removeSecret(all[3]!);
    assert.ok(capture(() => log.info("x", { v: all[3] })).includes(all[3]!));
    assert.ok(!capture(() => log.info("x", { v: all[4] })).includes(all[4]!));
  });

  it("the escaped form of one secret equal to another secret survives removing the first", () => {
    const log = createLogger("info");
    fill(log, 70, "c");
    const B = 'a\\"b-collision-0000'; // the JSON-escaped form of A below
    const A = 'a"b-collision-0000';
    assert.equal(JSON.stringify(A).slice(1, -1), B);
    log.addSecret(A);
    log.addSecret(B);
    capture(() => log.info("warm", {}));
    log.removeSecret(A);
    assert.ok(!capture(() => log.info("x", { v: B })).includes(B), "B stays masked");
    log.removeSecret(B);
    assert.equal(log.redactDiagnostic!(`z ${B} z`), `z ${B} z`, "B is unmasked once its own holder is gone");
  });

  it("crosses the 64-string threshold in both directions without changing what is masked", () => {
    const log = createLogger("info");
    const base = fill(log, 60, "d");
    const extra = Array.from({ length: 10 }, (_, i) => `extra-secret-number-${i}-0000`);
    for (const s of extra) log.addSecret(s); // 70: indexed
    assert.equal(log.redactDiagnostic!(`p ${extra[9]} ${base[0]} q`), "p ***REDACTED*** ***REDACTED*** q");
    for (const s of extra) log.removeSecret(s); // 60: per-string
    assert.equal(log.redactDiagnostic!(`p ${extra[9]} ${base[0]} q`), `p ${extra[9]} ***REDACTED*** q`);
    for (const s of extra) log.addSecret(s); // 70 again: indexed, kept current
    assert.equal(log.redactDiagnostic!(`p ${extra[9]} q`), "p ***REDACTED*** q");
    log.removeSecret(extra[9]!);
    assert.equal(log.redactDiagnostic!(`p ${extra[9]} ${extra[8]} q`), `p ${extra[9]} ***REDACTED*** q`);
  });
});
