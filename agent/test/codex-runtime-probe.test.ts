import { describe, it, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, writeFile, chmod, rm, readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  probeCodexRuntime,
  CODEX_PROBE_EXPECTATION,
  type ProbeCodexRuntimeOptions,
} from "../src/codex/codex-runtime-probe.js";
import {
  probeLandlockAvailability,
  resolveCodexHarnessAvailability,
  type LandlockProbeOutcome,
} from "../src/codex/codex-capability.js";

// PRD #1332 D3 (M5A / C2) — the startup runtime probe that gates the codex_harness_v1
// PROTOCOL capability. These tests run against small RECEIPT FIXTURES (a handful of tiny
// files, never a real 30MB binary): a temp install root laid out exactly as
// install-codex.sh leaves it, with a receipt beside the version root. Each negative case
// mutates ONE field and proves the probe fails closed (returns not-capable, never throws).

const VERSION = CODEX_PROBE_EXPECTATION.version;
const ARCH = "amd64";
const LOCK = CODEX_PROBE_EXPECTATION.lockDigest[ARCH];

function sha256(content: string): string {
  return createHash("sha256").update(content).digest("hex");
}

interface MemberSpec {
  member: string;
  content: string;
  mode: number;
}

// The exact CODEX_MEMBERS set install-codex.sh records, with distinct tiny contents so a
// per-member digest is meaningful.
const DEFAULT_MEMBERS: MemberSpec[] = [
  { member: "bin/codex", content: "codex-cli-binary\n", mode: 0o755 },
  { member: "bin/codex-code-mode-host", content: "code-mode-host-binary\n", mode: 0o755 },
  { member: "codex-package.json", content: '{"version":"0.153.2"}\n', mode: 0o644 },
  { member: "codex-path/rg", content: "ripgrep-binary\n", mode: 0o755 },
  { member: "codex-resources/bwrap", content: "bwrap-binary\n", mode: 0o755 },
  { member: "codex-resources/zsh/bin/zsh", content: "zsh-binary\n", mode: 0o755 },
];

const tempRoots: string[] = [];
after(async () => {
  await Promise.all(tempRoots.map((dir) => rm(dir, { recursive: true, force: true })));
});

type ReceiptObj = Record<string, unknown>;
type ReceiptMemberObj = Record<string, unknown>;

interface FixtureOptions {
  members?: MemberSpec[];
  /** Mutate the receipt object before it is written (inject a single mismatch). */
  mutateReceipt?: (receipt: ReceiptObj) => void;
  /** Do not write the receipt at all (absent-receipt case). */
  omitReceipt?: boolean;
  /** Write these raw bytes as the receipt instead of a JSON object (malformed case). */
  rawReceipt?: string;
}

/** Lay out a valid install + receipt in a fresh temp prefix. Returns the prefix so a test
 *  can probe it (and, for the member-missing case, delete a file first). */
async function buildFixture(opts: FixtureOptions = {}): Promise<{ prefix: string; versionRoot: string }> {
  const members = opts.members ?? DEFAULT_MEMBERS;
  const prefix = await mkdtemp(path.join(os.tmpdir(), "codex-probe-"));
  tempRoots.push(prefix);
  const versionRoot = path.join(prefix, VERSION);

  const memberEntries: ReceiptMemberObj[] = [];
  for (const m of members) {
    const abs = path.join(versionRoot, m.member);
    await mkdir(path.dirname(abs), { recursive: true });
    await writeFile(abs, m.content);
    await chmod(abs, m.mode);
    memberEntries.push({
      member: m.member,
      path: abs,
      sha256: sha256(m.content),
      mode: (m.mode & 0o777).toString(8),
    });
  }

  const receiptPath = path.join(prefix, `${VERSION}.receipt.json`);
  if (!opts.omitReceipt) {
    if (opts.rawReceipt !== undefined) {
      await writeFile(receiptPath, opts.rawReceipt);
    } else {
      const receipt: ReceiptObj = {
        schemaVersion: 1,
        codexVersion: VERSION,
        targetArch: ARCH,
        muslTarget: "x86_64-unknown-linux-musl",
        lockDigest: LOCK,
        prefix,
        versionRoot,
        members: memberEntries,
      };
      opts.mutateReceipt?.(receipt);
      await writeFile(receiptPath, JSON.stringify(receipt, null, 2));
    }
  }
  return { prefix, versionRoot };
}

/** Probe a fixture prefix with the arch/version/lock pinned to the fixture's, so a test
 *  isolates exactly the field it mutated. */
function probe(prefix: string, over: Partial<ProbeCodexRuntimeOptions> = {}) {
  return probeCodexRuntime({
    prefix,
    arch: ARCH,
    expectedVersion: VERSION,
    expectedLockDigest: LOCK,
    ...over,
  });
}

describe("probeCodexRuntime — valid receipt", () => {
  it("returns capable for an intact install matching the receipt", async () => {
    const { prefix } = await buildFixture();
    const result = await probe(prefix);
    assert.strictEqual(result.capable, true, `expected capable, got: ${result.reason ?? ""}`);
    assert.strictEqual(result.reason, undefined);
  });

  it("uses the baked CODEX_PROBE_EXPECTATION defaults when version/lock are not overridden", async () => {
    // No expectedVersion/expectedLockDigest passed: the probe must fall back to the pinned
    // constants (0.153.2 + this arch's lock digest) and still find + verify the fixture.
    const { prefix } = await buildFixture();
    const result = await probeCodexRuntime({ prefix, arch: ARCH });
    assert.strictEqual(result.capable, true, `expected capable via baked defaults, got: ${result.reason ?? ""}`);
  });
});

describe("probeCodexRuntime — fails closed (never throws)", () => {
  it("absent receipt → not capable", async () => {
    const { prefix } = await buildFixture({ omitReceipt: true });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /receipt not readable/);
  });

  it("a prefix that does not exist at all → not capable", async () => {
    const result = await probe(path.join(os.tmpdir(), "codex-probe-nonexistent-xyz"));
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /receipt not readable/);
  });

  it("malformed / corrupt JSON → not capable", async () => {
    const { prefix } = await buildFixture({ rawReceipt: "{ this is not valid json " });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /not valid JSON/);
  });

  it("receipt that parses to a non-object → not capable", async () => {
    const { prefix } = await buildFixture({ rawReceipt: "42" });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /not a JSON object/);
  });

  it("a member missing on disk → not capable", async () => {
    const { prefix, versionRoot } = await buildFixture();
    await rm(path.join(versionRoot, "codex-resources/bwrap"));
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /member missing on disk/);
  });

  it("a member replaced by a DIRECTORY (not a regular file) → not capable", async () => {
    // Fail-closed branch coverage for the probe's `if (!st.isFile())` gate. Lay out a
    // fully valid install + receipt, then swap ONE member's regular file for a DIRECTORY
    // at the same absolute path. stat() still SUCCEEDS (the path exists), so the probe
    // gets past the member-missing catch and reaches st.isFile() — the ONLY check that
    // rejects a non-regular member before its mode and digest are examined. This closes
    // the untested branch: folding the gate out makes the probe instead try to mode-check
    // and hash a directory, which changes the failure reason away from /not a regular
    // file/, so this exact reason is a precise sentinel for that mutation.
    const { prefix, versionRoot } = await buildFixture();
    const member = path.join(versionRoot, "codex-resources/bwrap");
    await rm(member);
    await mkdir(member, { recursive: true });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /not a regular file/);
  });

  it("a member digest mismatch → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        (r.members as ReceiptMemberObj[])[0]!.sha256 = "0".repeat(64);
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /member digest mismatch/);
  });

  it("a member mode mismatch → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        // bin/codex is 755 on disk; claim 700 in the receipt.
        (r.members as ReceiptMemberObj[])[0]!.mode = "700";
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /member mode mismatch/);
  });

  it("a member path mismatch → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        const first = (r.members as ReceiptMemberObj[])[0]!;
        first.path = `${String(first.path)}-tampered`;
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /member path .* != expected/);
  });

  it("a member name that escapes the version root → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        (r.members as ReceiptMemberObj[])[0]!.member = "../../etc/passwd";
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /escapes the version root/);
  });

  it("a malformed member entry → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        (r.members as ReceiptMemberObj[])[0] = { member: "bin/codex" }; // missing path/sha256/mode
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /member entry is malformed/);
  });

  it("an empty members array → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        r.members = [];
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /no members/);
  });

  it("a version mismatch (receipt found, wrong codexVersion field) → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        r.codexVersion = "9.9.9";
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /version .* != expected/);
  });

  it("an arch mismatch → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        r.targetArch = "arm64";
      },
    });
    // Probe as amd64 (matching the on-disk arch) but the receipt claims arm64.
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /arch .* != runtime/);
  });

  it("a lock-digest mismatch → not capable", async () => {
    const { prefix } = await buildFixture({
      mutateReceipt: (r) => {
        r.lockDigest = "f".repeat(64);
      },
    });
    const result = await probe(prefix);
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /lock digest does not match/);
  });

  it("an unsupported runtime arch → not capable", async () => {
    const { prefix } = await buildFixture();
    const result = await probeCodexRuntime({ prefix, arch: "riscv64", expectedVersion: VERSION });
    assert.strictEqual(result.capable, false);
    assert.match(result.reason ?? "", /no pinned lock digest for arch/);
  });
});

// PRD #1332 D3: prove the probe does NO exec / PATH search / app-server start / network
// BY CONSTRUCTION — it statically imports only node's fs/crypto/path built-ins, and uses
// no dynamic import(), require(), or global fetch() escape. This is a design assertion over
// the module's own source, not a spawn mock, exactly as the PRD requires.
describe("probeCodexRuntime — no exec / no network by construction", () => {
  it("imports only fs/crypto/path and uses no dynamic-import/require/fetch escape", async () => {
    const src = await readFile(
      fileURLToPath(new URL("../src/codex/codex-runtime-probe.ts", import.meta.url)),
      "utf8",
    );

    const allowedSpecifiers = new Set([
      "node:crypto",
      "node:fs",
      "node:fs/promises",
      "node:path",
    ]);
    const specifiers = [...src.matchAll(/(?:from|import)\s+"([^"]+)"/g)].map((m) => m[1] ?? "");
    assert.ok(specifiers.length > 0, "expected at least one import in the probe module");
    for (const spec of specifiers) {
      assert.ok(
        allowedSpecifiers.has(spec),
        `probe module must import only fs/crypto/path — found forbidden import "${spec}"`,
      );
    }
    // None of these subprocess/network modules may appear as a specifier.
    for (const forbidden of ["child_process", "net", "http", "https", "dgram", "tls", "worker_threads", "vm"]) {
      assert.ok(
        !specifiers.some((s) => s === forbidden || s === `node:${forbidden}`),
        `probe module must not import ${forbidden}`,
      );
    }
    // No dynamic-import / require / global fetch escape hatch (which would need no static import).
    assert.doesNotMatch(src, /\brequire\s*\(/, "probe module must not use require()");
    assert.doesNotMatch(src, /\bimport\s*\(/, "probe module must not use dynamic import()");
    assert.doesNotMatch(src, /\bfetch\s*\(/, "probe module must not call fetch()");
  });
});

// PRD #1493 M3 — the Landlock probe (exit-code mapping) and the honest-advertisement
// CONJUNCTION: advertise codex_harness_v1 ONLY when receipt intact AND uid split active
// AND (Landlock available OR best-effort on a Landlock-unavailable kernel). Each failing
// precondition yields a distinct reason. The wrapper lives in its OWN module (it spawns a
// subprocess), so probeCodexRuntime keeps the no-exec contract asserted above.
describe("probeLandlockAvailability — --probe exit-code mapping", () => {
  const fakeSpawn = (status: number | null, error?: Error) => () => ({ status, error });
  it("maps exit 0 → available, 10 → unavailable, 11 → error", () => {
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(0)), "available");
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(10)), "unavailable");
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(11)), "error");
  });
  it("maps a spawn error, a null status, or an unknown code → probe-failed", () => {
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(0, new Error("ENOENT"))), "probe-failed");
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(null)), "probe-failed");
    assert.equal(probeLandlockAvailability("/bin/x", fakeSpawn(42)), "probe-failed");
  });
  it("never throws when the spawn seam itself throws", () => {
    const throwing = () => { throw new Error("spawn blew up"); };
    assert.equal(probeLandlockAvailability("/bin/x", throwing), "probe-failed");
  });
});

describe("resolveCodexHarnessAvailability — honest advertisement conjunction", () => {
  const base = { receiptCapable: true, uidSplit: true, mode: "required" as const, landlock: "available" as LandlockProbeOutcome };

  it("advertises (not degraded) when receipt intact AND uid split AND Landlock available", () => {
    const r = resolveCodexHarnessAvailability({ ...base });
    assert.equal(r.advertise, true);
    assert.equal(r.degraded, false);
    assert.equal(r.reason, undefined);
  });

  it("does NOT advertise when the receipt is not intact (distinct reason)", () => {
    const r = resolveCodexHarnessAvailability({ ...base, receiptCapable: false, receiptReason: "receipt not readable" });
    assert.equal(r.advertise, false);
    assert.match(r.reason ?? "", /receipt not intact/);
  });

  it("does NOT advertise when the uid split is inactive (distinct reason)", () => {
    const r = resolveCodexHarnessAvailability({ ...base, uidSplit: false });
    assert.equal(r.advertise, false);
    assert.match(r.reason ?? "", /uid split not active/);
  });

  it("advertises DEGRADED in best-effort on a Landlock-UNAVAILABLE kernel", () => {
    const r = resolveCodexHarnessAvailability({ ...base, mode: "best-effort", landlock: "unavailable" });
    assert.equal(r.advertise, true);
    assert.equal(r.degraded, true);
  });

  it("does NOT advertise in required mode on a Landlock-unavailable kernel (distinct reason)", () => {
    const r = resolveCodexHarnessAvailability({ ...base, mode: "required", landlock: "unavailable" });
    assert.equal(r.advertise, false);
    assert.match(r.reason ?? "", /required/);
  });

  it("does NOT advertise on a Landlock ERROR even in best-effort (D8: error stays fatal)", () => {
    for (const landlock of ["error", "probe-failed"] as const) {
      const r = resolveCodexHarnessAvailability({ ...base, mode: "best-effort", landlock });
      assert.equal(r.advertise, false, `best-effort must not advertise on landlock ${landlock}`);
      assert.equal(r.degraded, false);
      assert.match(r.reason ?? "", /fatal even in best-effort/);
    }
  });

  it("the receipt precondition is checked FIRST (a split-less, Landlock-less, no-receipt worker names the receipt)", () => {
    const r = resolveCodexHarnessAvailability({ receiptCapable: false, receiptReason: "no receipt", uidSplit: false, mode: "required", landlock: "error" });
    assert.equal(r.advertise, false);
    assert.match(r.reason ?? "", /receipt not intact/);
  });
});
