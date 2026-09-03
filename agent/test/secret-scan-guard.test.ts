import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  parseGitleaksReport,
  commitsScannedFromStderr,
  scanIsTrustworthy,
  gitleaksArgs,
  type SecretFinding,
} from "../src/secret-scan-guard.js";
import { isPushProtectionRejection } from "../src/git.js";
import { composePushSecretBlockedReason } from "../src/runner.js";

// PRD #974 M2 (load-bearing security): the pure, I/O-free helpers behind the finalize
// pre-push secret scan, plus the two remote-backstop helpers that classify a GH013
// rejection and compose its typed, capped failure_reason. Every case here is hermetic:
// no gitleaks binary, no `go run`, no network, no git spawn — string parsers and pure
// decision logic only.

describe("parseGitleaksReport", () => {
  it("maps a two-element gitleaks report to SecretFinding[], ignoring extra fields", () => {
    const json = JSON.stringify([
      {
        File: "config/app.env",
        StartLine: 12,
        Commit: "abcdef1234567890",
        RuleID: "generic-api-key",
        // Extra report fields the finalize path does not need — must be ignored. The values are
        // deliberately NOT token-shaped: a real `glpat-`/`sk_live_` literal in this tracked test
        // file would be rejected by GitHub Push Protection on THIS repo's own branch push
        // (.claude/rules/prds.md). The test only proves these fields are dropped, so the value
        // is arbitrary.
        Secret: "redacted-extra-field-value",
        Match: "API_KEY=redacted-extra-field-value",
        Fingerprint: "abcdef:config/app.env:generic-api-key:12",
        Entropy: 4.2,
      },
      {
        File: "deploy/secrets.yaml",
        StartLine: 7,
        Commit: "0011223344556677",
        RuleID: "gitlab-pat",
        Secret: "redacted-gitlab-pat-fixture",
      },
    ]);
    const out = parseGitleaksReport(json);
    assert.deepStrictEqual(out, [
      {
        file: "config/app.env",
        startLine: 12,
        commit: "abcdef1234567890",
        ruleId: "generic-api-key",
      },
      {
        file: "deploy/secrets.yaml",
        startLine: 7,
        commit: "0011223344556677",
        ruleId: "gitlab-pat",
      },
    ] satisfies SecretFinding[]);
  });

  it("returns [] for the literal \"null\" (gitleaks writes null when it finds nothing)", () => {
    assert.deepStrictEqual(parseGitleaksReport("null"), []);
  });

  it("returns [] for an empty string", () => {
    assert.deepStrictEqual(parseGitleaksReport(""), []);
  });

  it("returns [] (never throws) for malformed JSON", () => {
    assert.deepStrictEqual(parseGitleaksReport("{not json"), []);
    assert.deepStrictEqual(parseGitleaksReport("[{"), []);
  });

  it("returns [] for a non-array top-level value", () => {
    assert.deepStrictEqual(parseGitleaksReport("{}"), []);
    assert.deepStrictEqual(parseGitleaksReport("42"), []);
    assert.deepStrictEqual(parseGitleaksReport("\"hello\""), []);
  });

  it("defaults missing/mistyped fields and skips non-object elements", () => {
    const json = JSON.stringify([
      {}, // all fields missing → defaults
      { File: "only-file.txt" }, // partial → the rest default
      { File: 5, StartLine: "nope", Commit: null, RuleID: 12 }, // wrong types → defaults
      null, // skipped
      42, // skipped
      "a string", // skipped
      { File: "last.txt", StartLine: 3, Commit: "c", RuleID: "r" },
    ]);
    const out = parseGitleaksReport(json);
    assert.deepStrictEqual(out, [
      { file: "", startLine: 0, commit: "", ruleId: "" },
      { file: "only-file.txt", startLine: 0, commit: "", ruleId: "" },
      { file: "", startLine: 0, commit: "", ruleId: "" },
      { file: "last.txt", startLine: 3, commit: "c", ruleId: "r" },
    ] satisfies SecretFinding[]);
  });

  it("caps the materialised findings so an adversarial report cannot balloon memory", () => {
    // 5000 findings in the report → parse returns the cap (500), which still proves ">=1 secret".
    const many = Array.from({ length: 5000 }, (_, i) => ({
      File: `f${i}.txt`,
      StartLine: i,
      Commit: "c",
      RuleID: "generic-api-key",
    }));
    const out = parseGitleaksReport(JSON.stringify(many));
    assert.strictEqual(out.length, 500);
    assert.strictEqual(out[0]!.file, "f0.txt");
  });
});

describe("commitsScannedFromStderr", () => {
  it("extracts the count from a realistic gitleaks stderr line (singular and plural)", () => {
    assert.strictEqual(commitsScannedFromStderr("8:15AM INF 1 commits scanned."), 1);
    assert.strictEqual(commitsScannedFromStderr("8:15AM INF 42 commits scanned."), 42);
    // Singular "commit scanned" also matches (regex allows `commits?`).
    assert.strictEqual(commitsScannedFromStderr("8:15AM INF 1 commit scanned."), 1);
  });

  it("returns null when no such line is present", () => {
    assert.strictEqual(
      commitsScannedFromStderr("8:15AM INF scan completed in 12ms\n8:15AM INF no leaks found"),
      null,
    );
  });

  it("returns null for garbage input", () => {
    assert.strictEqual(commitsScannedFromStderr(""), null);
    assert.strictEqual(commitsScannedFromStderr("total nonsense here"), null);
  });
});

describe("scanIsTrustworthy", () => {
  // A benign stderr chosen NOT to contain the substring "err" so the happy path is
  // genuinely reachable (see the happy-path case below).
  const benign = "8:15AM INF 3 commits scanned.";

  it("is false when the gitleaks process did not run cleanly (execOk=false)", () => {
    assert.strictEqual(
      scanIsTrustworthy({ stderr: benign, scannedCommits: 3, expectedCommits: 3, execOk: false }),
      false,
    );
  });

  it("is false for an ERR token on stderr (case-insensitive), other inputs good", () => {
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "8:15AM ERR could not open object",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
    // Case-insensitive: lowercase token also trips the guard.
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "8:15AM err could not open object",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
  });

  it("is false for a `fatal` token on stderr (case-insensitive), other inputs good", () => {
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "FATAL: bad revision",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "fatal: bad revision",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
  });

  it("is false for an `unknown revision` token on stderr (case-insensitive), other inputs good", () => {
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "fatal: ambiguous argument: unknown revision or path not in the working tree",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
    assert.strictEqual(
      scanIsTrustworthy({
        stderr: "UNKNOWN REVISION base..head",
        scannedCommits: 3,
        expectedCommits: 3,
        execOk: true,
      }),
      false,
    );
  });

  it("is false when the `N commits scanned` line was absent (scannedCommits=null)", () => {
    assert.strictEqual(
      scanIsTrustworthy({ stderr: benign, scannedCommits: null, expectedCommits: 3, execOk: true }),
      false,
    );
  });

  it("is false when scanned count != expected count (the unresolved/empty-range trap)", () => {
    assert.strictEqual(
      scanIsTrustworthy({ stderr: benign, scannedCommits: 0, expectedCommits: 3, execOk: true }),
      false,
    );
    assert.strictEqual(
      scanIsTrustworthy({ stderr: benign, scannedCommits: 2, expectedCommits: 3, execOk: true }),
      false,
    );
  });

  it("is true when every clause holds (proves the guard is not vacuously always-false)", () => {
    // The benign stderr must not contain the substring "err" for this to be reachable.
    assert.ok(!benign.toLowerCase().includes("err"), "guard: benign stderr must not contain 'err'");
    assert.strictEqual(
      scanIsTrustworthy({ stderr: benign, scannedCommits: 3, expectedCommits: 3, execOk: true }),
      true,
    );
    // A zero-length range that scanned zero commits is a legitimate trusted "nothing to scan".
    assert.strictEqual(
      scanIsTrustworthy({ stderr: "8:15AM INF 0 commits scanned.", scannedCommits: 0, expectedCommits: 0, execOk: true }),
      true,
    );
  });
});

describe("gitleaksArgs", () => {
  it("returns the exact proven argv (all three silencer-disabling flags present)", () => {
    const argv = gitleaksArgs({
      sourcePath: "/worker/bare.git",
      logRange: "base..head",
      configPath: "/tmp/gitleaks-default.toml",
      reportPath: "/tmp/report.json",
    });
    assert.deepStrictEqual(argv, [
      "git",
      "/worker/bare.git",
      "--log-opts",
      "base..head",
      "-c",
      "/tmp/gitleaks-default.toml",
      "--ignore-gitleaks-allow",
      "--exit-code",
      "0",
      "--no-banner",
      "--redact",
      "--report-format",
      "json",
      "--report-path",
      "/tmp/report.json",
    ]);
  });

  it("includes the three silencer-disabling flags with their values", () => {
    const argv = gitleaksArgs({
      sourcePath: "/src",
      logRange: "aaa..bbb",
      configPath: "/cfg.toml",
      reportPath: "/out.json",
    });
    // --ignore-gitleaks-allow disables inline //gitleaks:allow comments.
    assert.ok(argv.includes("--ignore-gitleaks-allow"), "--ignore-gitleaks-allow must be present");
    // -c <config> forces the embedded default ruleset and skips repo .gitleaks.toml discovery.
    const cIdx = argv.indexOf("-c");
    assert.ok(cIdx >= 0, "-c must be present");
    assert.strictEqual(argv[cIdx + 1], "/cfg.toml", "-c must be followed by the config path");
    // --log-opts <range> scopes the scan to exactly the base..head range.
    const logIdx = argv.indexOf("--log-opts");
    assert.ok(logIdx >= 0, "--log-opts must be present");
    assert.strictEqual(argv[logIdx + 1], "aaa..bbb", "--log-opts must be followed by the range");
  });
});

describe("isPushProtectionRejection", () => {
  it("is true on a realistic GH013 rejection message", () => {
    const err = new Error(
      "remote: error: GH013: Repository rule violations found for refs/heads/agent/issue-1.\n" +
        "remote: - Push cannot contain secrets\n" +
        "remote: locations: commit abc123 path config/app.env:12",
    );
    assert.strictEqual(isPushProtectionRejection(err), true);
  });

  it("is true on each stable token, case-insensitively", () => {
    for (const token of ["GH013", "Push cannot contain secrets", "push protection", "secret detected"]) {
      assert.strictEqual(
        isPushProtectionRejection(new Error(`remote: ${token} here`)),
        true,
        `token should match: ${token}`,
      );
      assert.strictEqual(
        isPushProtectionRejection(new Error(`remote: ${token.toUpperCase()} here`)),
        true,
        `token should match uppercase: ${token}`,
      );
      assert.strictEqual(
        isPushProtectionRejection(new Error(`remote: ${token.toLowerCase()} here`)),
        true,
        `token should match lowercase: ${token}`,
      );
    }
  });

  it("accepts a non-Error argument (stringified)", () => {
    assert.strictEqual(isPushProtectionRejection("fatal: GH013 push cannot contain secrets"), true);
  });

  it("does not over-match a workflow-scope rejection", () => {
    const err = new Error(
      "remote: error: refusing to allow a Personal Access Token to create or update workflow " +
        ".github/workflows/ci.yml without workflow scope",
    );
    assert.strictEqual(isPushProtectionRejection(err), false);
  });

  it("does not over-match a non-fast-forward rejection", () => {
    const err = new Error(
      "! [rejected]  agent/issue-1 -> agent/issue-1 (non-fast-forward)\n" +
        "error: failed to push some refs; hint: fetch first",
    );
    assert.strictEqual(isPushProtectionRejection(err), false);
  });

  it("does not over-match a generic remote-rejected / auth error", () => {
    assert.strictEqual(
      isPushProtectionRejection(new Error("remote: Permission denied (403). fatal: unable to access")),
      false,
    );
    assert.strictEqual(
      isPushProtectionRejection(new Error("error: remote rejected: pre-receive hook declined")),
      false,
    );
  });
});

describe("composePushSecretBlockedReason", () => {
  const SUFFIX_TAIL = "Your diff is preserved below.";
  const MAX = 512;

  it("names the short commit + path + rule for a single finding, ≤512, suffix intact", () => {
    const reason = composePushSecretBlockedReason([
      { file: "config/app.env", startLine: 12, commit: "abcdef1234567890", ruleId: "generic-api-key" },
    ]);
    assert.ok(reason.length <= MAX, `must be ≤${MAX} (got ${reason.length})`);
    // Short commit (first 8 chars), path:line, and rule all present.
    assert.match(reason, /abcdef12/, "short commit (8 chars) must appear");
    assert.ok(!reason.includes("abcdef1234567890"), "the full commit must be shortened to 8 chars");
    assert.match(reason, /config\/app\.env:12/, "path:line must appear");
    assert.match(reason, /\(generic-api-key\)/, "rule id must appear");
    assert.ok(reason.endsWith(SUFFIX_TAIL), "the preserved-diff pointer must end the reason");
    // A single short finding fits well under the cap — proves the ≤512 assertion is not
    // trivially satisfied because everything is short (non-vacuity for the cap cases below).
    assert.ok(reason.length < MAX, "a single short finding should not need truncation");
  });

  it("caps at ≤512 with an 'and N more' tail and an intact suffix for many findings", () => {
    const findings: SecretFinding[] = Array.from({ length: 200 }, (_, i) => ({
      file: `services/backend/module-${i}/very/deeply/nested/config/secrets-${i}.yaml`,
      startLine: i + 1,
      commit: `deadbeefcafebabe${i}`,
      ruleId: "generic-api-key",
    }));
    const reason = composePushSecretBlockedReason(findings);
    assert.ok(reason.length <= MAX, `must be ≤${MAX} (got ${reason.length})`);
    assert.match(reason, /and \d+ more/, "a truncated list must show the omitted count");
    assert.ok(reason.endsWith(SUFFIX_TAIL), "the preserved-diff pointer must survive truncation");
    assert.match(reason, /GH013/, "the fixed prefix must survive truncation");
    // Non-vacuity: the UNtruncated join would blow past the cap, so truncation actually fired.
    const untruncated = findings
      .map((f) => `${f.commit.slice(0, 8)} ${f.file}:${f.startLine} (${f.ruleId})`)
      .join("; ");
    assert.ok(untruncated.length > MAX, "control: the raw finding list must exceed the cap");
    // The first finding is always shown (list is truncated from the tail).
    assert.match(reason, /services\/backend\/module-0\//, "the first finding must be shown");
  });

  it("caps a single pathological very-long path at ≤512 with the suffix intact", () => {
    const reason = composePushSecretBlockedReason([
      {
        file: "a/" + "x".repeat(2000) + "/secret.env",
        startLine: 99999,
        commit: "0123456789abcdef",
        ruleId: "generic-api-key",
      },
    ]);
    assert.ok(reason.length <= MAX, `must be ≤${MAX} (got ${reason.length})`);
    assert.ok(reason.endsWith(SUFFIX_TAIL), "the preserved-diff pointer must survive hard truncation");
    assert.match(reason, /GH013/, "the fixed prefix must survive hard truncation");
    assert.match(reason, /…/, "a hard-truncated single label must show the ellipsis");
  });

  it("strips control bytes from the attacker-controlled file path (no terminal-forge)", () => {
    // A committed filename can carry ESC/newline; those must not survive into the stored
    // failure_reason (they would forge rows / inject ANSI when rendered in a CLI/TUI terminal).
    const reason = composePushSecretBlockedReason([
      {
        file: "evil\u001b[2K\nrow.env",
        startLine: 1,
        commit: "abcdef1234567890",
        ruleId: "generic-api-key",
      },
    ]);
    // eslint-disable-next-line no-control-regex
    assert.ok(!/[\u0000-\u001f\u007f]/.test(reason), "no C0 control byte or DEL may survive");
    assert.ok(!reason.includes("\u001b"), "ESC must be stripped");
    assert.ok(!reason.includes("\n"), "newline must be stripped");
    // The visible path characters survive, just not the control bytes.
    assert.match(reason, /evil\[2Krow\.env/, "the printable path remains, control bytes removed");
  });
});
