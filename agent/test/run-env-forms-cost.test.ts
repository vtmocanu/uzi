import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";

// PRD 4416 M2: the memory and time a claim's forms cost at the store's bounds, measured in a child node process
// with a capped old space and explicit gc. Every value is derived from a hash.

const HEAP_GROWTH_MAX_MIB = 128;
const ONE_ADD_MAX_MS = 2000;
const BUILD_MAX_MS = 15000;
const CHILD_TIMEOUT_MS = 120_000;

// Child program: builds the claim, a SecretSet and a logger registry, then reports heap growth after gc and the
// time of one add on each full index.
const CHILD = String.raw`
import { createHash } from "node:crypto";
const src = process.env.COST_SRC;
const { runEnvClaimForms } = await import(src + "run-env-forms.ts");
const { SecretSet } = await import(src + "redact.ts");
const { createLogger } = await import(src + "log.ts");
const [liveCount, liveBytes, oldCount, oldBytes, controls] = JSON.parse(process.env.COST_SHAPE);
// One-byte control characters that JSON escapes to six units: C0 without tab, LF and CR.
const CONTROLS = Array.from({ length: 31 }, (_, i) => i + 1).filter((c) => c !== 9 && c !== 10 && c !== 13);
const value = (seed, bytes) => {
  let text = "";
  for (let i = 0; text.length < bytes; i++) text += createHash("sha512").update(seed + "-" + i).digest("base64").replace(/=+$/, "");
  text = text.slice(0, bytes - 2);
  // Control shape: each 64-character line keeps a 12-character token and its other characters become controls.
  if (controls) text = Array.from(text, (c, i) => (i % 64 < 12 ? c : String.fromCharCode(CONTROLS[c.charCodeAt(0) % CONTROLS.length]))).join("");
  return text.match(/.{1,64}/g).join("\r\n") + "\r\n";
};
const id = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const live = Array.from({ length: liveCount }, (_, i) => ({ var_id: id, scope: "repo", rev: 1, name: "LIVE_" + i, value: value("live-" + i, liveBytes), secret: true, description: "" }));
const old = Array.from({ length: oldCount }, (_, i) => ({ var_id: id, scope: "repo", rev: i + 1, name: "OLD_" + (i % 20), value: value("old-" + i, oldBytes) }));
global.gc();
const before = process.memoryUsage().heapUsed;
const started = performance.now();
const claim = runEnvClaimForms(live, old);
// An over-budget claim registers whole raw values only; a within-budget one also registers each line.
const planted = claim.overBudget ? old[3].value : old[3].value.split("\r\n")[2];
const buildMs = performance.now() - started;
const set = new SecretSet(claim.redaction);
const setMasked = !set.text("x\t" + planted + "\ty").includes(planted);
const log = createLogger("error");
for (const f of claim.redaction) log.addSecret(f);
const logMasked = !log.redactDiagnostic("x " + planted + " y").includes(planted);
global.gc();
const growth = process.memoryUsage().heapUsed - before;
let t = performance.now();
set.add("one-more-secret-form-0123456789");
const setAddMs = performance.now() - t;
t = performance.now();
log.addSecret("one-more-secret-form-9876543210");
const logAddMs = performance.now() - t;
console.log(JSON.stringify({
  overBudget: claim.overBudget, formChars: claim.formChars, forms: claim.redaction.length, needles: claim.needles.length,
  growthMiB: growth / 1048576, buildMs, setAddMs, logAddMs, setMasked, logMasked,
  addedMasked: !set.text("x one-more-secret-form-0123456789 y").includes("one-more"),
}));
`;

interface Report {
  overBudget: boolean; formChars: number; forms: number; needles: number; growthMiB: number;
  buildMs: number; setAddMs: number; logAddMs: number; setMasked: boolean; logMasked: boolean; addedMasked: boolean;
}

/** [live variables, bytes each, retired versions, bytes each, whether the values are made of control characters]. */
type Shape = [number, number, number, number, boolean];

function measure(shape: Shape): Report {
  const res = spawnSync(process.execPath, ["--expose-gc", "--max-old-space-size=512", "--import", "tsx", "--input-type=module", "-e", CHILD], {
    cwd: fileURLToPath(new URL("..", import.meta.url)),
    env: { PATH: process.env.PATH ?? "/usr/bin:/bin", HOME: process.env.HOME ?? "/", COST_SRC: pathToFileURL(fileURLToPath(new URL("../src/", import.meta.url))).href, COST_SHAPE: JSON.stringify(shape) },
    encoding: "utf8",
    maxBuffer: 1 << 20,
    timeout: CHILD_TIMEOUT_MS,
  });
  assert.equal(res.status, 0, `child exited ${res.status}: ${res.stderr.slice(0, 500)}`);
  const report = JSON.parse(res.stdout.trim().split("\n").pop()!) as Report;
  console.log(`cost ${JSON.stringify(shape)} ${JSON.stringify(report)}`);
  return report;
}

describe("the cost of a claim's forms at the store bounds", () => {
  it("the full bounds trip the budget and stay within the heap and per-add limits", () => {
    // 50 live variables of 5242 bytes (262100 of 262144) and 300 retired versions of 8738 bytes (2621400 of 2621440).
    const r = measure([50, 5242, 300, 8738, false]);
    assert.equal(r.overBudget, true);
    assert.ok(r.growthMiB <= HEAP_GROWTH_MAX_MIB, `heap grew ${r.growthMiB.toFixed(1)} MiB`);
    assert.ok(r.buildMs <= BUILD_MAX_MS, `building the forms took ${r.buildMs.toFixed(0)} ms`);
    assert.ok(r.setAddMs <= ONE_ADD_MAX_MS, `SecretSet.add took ${r.setAddMs.toFixed(1)} ms`);
    assert.ok(r.logAddMs <= ONE_ADD_MAX_MS, `addSecret took ${r.logAddMs.toFixed(1)} ms`);
    assert.ok(r.setMasked && r.logMasked && r.addedMasked, "the raw values and the added form are masked");
    assert.equal(r.needles, 0, "no partial needle set");
  });

  it("a claim just under the budget builds complete forms and keeps both indexes within the limits", () => {
    const r = measure([50, 5242, 40, 8738, false]);
    assert.equal(r.overBudget, false);
    assert.ok(r.formChars > 20_000_000, `a realistic large claim: ${r.formChars} units`);
    assert.ok(r.needles > 0);
    assert.ok(r.growthMiB <= HEAP_GROWTH_MAX_MIB, `heap grew ${r.growthMiB.toFixed(1)} MiB`);
    assert.ok(r.buildMs <= BUILD_MAX_MS, `building the forms took ${r.buildMs.toFixed(0)} ms`);
    assert.ok(r.setAddMs <= ONE_ADD_MAX_MS, `SecretSet.add took ${r.setAddMs.toFixed(1)} ms`);
    assert.ok(r.logAddMs <= ONE_ADD_MAX_MS, `addSecret took ${r.logAddMs.toFixed(1)} ms`);
    assert.ok(r.setMasked && r.logMasked && r.addedMasked, "a planted line and the added form are masked");
  });

  it("control-character values at the full bounds trip the budget and stay within the limits", () => {
    const r = measure([50, 5242, 300, 8738, true]);
    assert.equal(r.overBudget, true);
    assert.ok(r.growthMiB <= HEAP_GROWTH_MAX_MIB, `heap grew ${r.growthMiB.toFixed(1)} MiB`);
    assert.ok(r.buildMs <= BUILD_MAX_MS, `building the forms took ${r.buildMs.toFixed(0)} ms`);
    assert.ok(r.setAddMs <= ONE_ADD_MAX_MS, `SecretSet.add took ${r.setAddMs.toFixed(1)} ms`);
    assert.ok(r.logAddMs <= ONE_ADD_MAX_MS, `addSecret took ${r.logAddMs.toFixed(1)} ms`);
    assert.ok(r.setMasked && r.logMasked && r.addedMasked, "the raw values and the added form are masked");
    assert.equal(r.needles, 0, "no partial needle set");
  });

  it("a control-character claim just under the budget builds complete forms and keeps both indexes within the limits", () => {
    const r = measure([20, 5242, 8, 8738, true]);
    assert.equal(r.overBudget, false);
    assert.ok(r.formChars > 20_000_000, `a realistic large claim: ${r.formChars} units`);
    assert.ok(r.needles > 0);
    assert.ok(r.growthMiB <= HEAP_GROWTH_MAX_MIB, `heap grew ${r.growthMiB.toFixed(1)} MiB`);
    assert.ok(r.buildMs <= BUILD_MAX_MS, `building the forms took ${r.buildMs.toFixed(0)} ms`);
    assert.ok(r.setAddMs <= ONE_ADD_MAX_MS, `SecretSet.add took ${r.setAddMs.toFixed(1)} ms`);
    assert.ok(r.logAddMs <= ONE_ADD_MAX_MS, `addSecret took ${r.logAddMs.toFixed(1)} ms`);
    assert.ok(r.setMasked && r.logMasked && r.addedMasked, "a planted line and the added form are masked");
  });
});
