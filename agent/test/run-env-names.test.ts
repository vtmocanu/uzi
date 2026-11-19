import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import {
  RUN_ENV_DENIED_NAMES,
  RUN_ENV_DENIED_PREFIXES,
  RUN_ENV_DENIED_SUFFIXES,
  RUN_ENV_HARNESS_VERSIONS,
  runEnvNameViolation,
} from "../src/run-env-names.js";
import { PROTECTED_ENV_KEYS } from "../src/sdk-env.js";
import { COMMAND_ENV_PROTECTED_KEYS } from "../src/codex/codex-executor.js";

interface RuleEntry {
  value: string;
  invariant: string;
  note: string;
}
const INVARIANTS = [
  "worker_credentials", "provider_auth", "harness_control", "git_authority",
  "managed_dirs", "network_wiring", "process_loader", "shell_semantics",
];

// PRD 4416 "Name rules (shared fixture)": the agent copy of the run-env name rules must
// equal fixtures/run-env/name-rules.json, which the api copy is also tested against.
const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/run-env/name-rules.json", import.meta.url), "utf8"),
) as {
  harness_versions: { claude_agent_sdk: string; codex: string };
  denied_names: RuleEntry[];
  denied_prefixes: RuleEntry[];
  denied_suffixes: RuleEntry[];
  overlaps: { name: string; harness: string; decision: string; reason: string }[];
};
const values = (es: RuleEntry[]): string[] => es.map((e) => e.value);
const deniedNames = values(fixture.denied_names);
const deniedPrefixes = values(fixture.denied_prefixes);
const deniedSuffixes = values(fixture.denied_suffixes);

describe("run-env name rules match the shared fixture", () => {
  it("denied names, prefixes and suffixes are identical, in order", () => {
    assert.deepEqual([...RUN_ENV_DENIED_NAMES], deniedNames);
    assert.deepEqual([...RUN_ENV_DENIED_PREFIXES], deniedPrefixes);
    assert.deepEqual([...RUN_ENV_DENIED_SUFFIXES], deniedSuffixes);
  });

  it("every entry names a worker invariant from the closed set and carries a note", () => {
    const all = [...fixture.denied_names, ...fixture.denied_prefixes, ...fixture.denied_suffixes];
    for (const e of all) {
      assert.ok(INVARIANTS.includes(e.invariant), `${e.value}: invariant ${e.invariant}`);
      assert.ok(e.note.trim() !== "", `${e.value}: empty note`);
    }
    assert.equal(new Set(all.map((e) => e.value)).size, all.length, "duplicate fixture entry");
    for (const inv of INVARIANTS) assert.ok(all.some((e) => e.invariant === inv), `no entry protects ${inv}`);
  });

  it("overlap decisions agree with the rules", () => {
    assert.ok(fixture.overlaps.length > 0);
    for (const o of fixture.overlaps) {
      assert.ok(o.reason.trim() !== "", `${o.name}: empty reason`);
      const denied = runEnvNameViolation(o.name) !== null;
      assert.equal(denied, o.decision === "denied", `${o.name} is recorded as ${o.decision}`);
    }
  });

  it("recorded harness versions are identical", () => {
    assert.deepEqual({ ...RUN_ENV_HARNESS_VERSIONS }, fixture.harness_versions);
  });
});

describe("recorded harness versions track the pinned harnesses", () => {
  it("claude_agent_sdk equals agent/package.json", () => {
    const pkg = JSON.parse(fs.readFileSync(new URL("../package.json", import.meta.url), "utf8")) as {
      dependencies?: Record<string, string>;
      devDependencies?: Record<string, string>;
    };
    const pinned = (pkg.dependencies ?? {})["@anthropic-ai/claude-agent-sdk"] ?? (pkg.devDependencies ?? {})["@anthropic-ai/claude-agent-sdk"];
    assert.equal(fixture.harness_versions.claude_agent_sdk, pinned);
  });

  it("codex equals CODEX_VERSION in agent/codex/codex-package.lock", () => {
    const lock = fs.readFileSync(new URL("../codex/codex-package.lock", import.meta.url), "utf8");
    const m = /^CODEX_VERSION=(\S+)$/m.exec(lock);
    assert.ok(m, "CODEX_VERSION line missing from codex-package.lock");
    assert.equal(fixture.harness_versions.codex, m[1]);
  });
});

describe("runEnvNameViolation", () => {
  for (const name of deniedNames) {
    it(`denies name ${name}`, () => {
      assert.notEqual(runEnvNameViolation(name), null);
    });
  }
  for (const prefix of deniedPrefixes) {
    it(`denies prefix ${prefix}`, () => {
      const v = runEnvNameViolation(`${prefix}SOMETHING`);
      assert.ok(v !== null && v.includes(`denied prefix ${prefix}`), String(v));
    });
  }
  for (const suffix of deniedSuffixes) {
    it(`denies suffix ${suffix}`, () => {
      const v = runEnvNameViolation(`SOMETHING${suffix}`);
      assert.ok(v !== null && v.includes(`denied suffix ${suffix}`), String(v));
    });
  }

  for (const name of [
    "DATABASE_URL",
    "STRIPE_API_KEY",
    "AWS_ACCESS_KEY_ID",
    "AWS_REGION",
    "NPM_TOKEN",
    "GRADLE_OPTS",
    "ANT_OPTS",
    "ANT_HOME",
    "DOCKER_AUTH_CONFIG",
    "TESTCONTAINERS_HOST_OVERRIDE",
    "NODE_ENV",
    "NPM_CONFIG_REGISTRY",
    "PIP_INDEX_URL",
    "MAVEN_OPTS",
    "ORG_GRADLE_PROJECT_X",
    "GOOGLE_APPLICATION_CREDENTIALS",
    "AZURE_CLIENT_SECRET",
    "_LEADING_UNDERSCORE",
    "A",
    "A".repeat(128),
  ]) {
    it(`allows ${name.length > 20 ? `${name.length}-char name` : name}`, () => {
      assert.equal(runEnvNameViolation(name), null);
    });
  }

  for (const name of ["", "lower", "1ABC", "A-B", "A B", "A".repeat(129), "NAME\n", "ÄB"]) {
    it(`rejects malformed name ${JSON.stringify(name.length > 20 ? `${name.length} chars` : name)}`, () => {
      const v = runEnvNameViolation(name);
      assert.ok(v !== null && v.startsWith("must match "), String(v));
    });
  }

  it("applies the shape rule before the denylist (npm_config_cache is lower-case)", () => {
    assert.ok(runEnvNameViolation("npm_config_cache")?.startsWith("must match "));
  });
});

describe("runtime controls found by the pinned-harness review are denied", () => {
  for (const name of [
    "BUN_BE_BUN", "BUN_CONFIG_VERBOSE_FETCH", "BUN_OPTIONS", "NODE_DEBUG", "NODE_V8_COVERAGE",
    "NODE_REDIRECT_WARNINGS", "NODE_ICU_DATA", "NODE_USE_ENV_PROXY", "NODE_COMPILE_CACHE",
    "UV_THREADPOOL_SIZE", "SSLKEYLOGFILE", "ANT_OTEL_EXPORTER", "ANT_CLAUDE_CODE_METRICS_ENDPOINT",
    "LC_ALL", "LC_CTYPE", "LANGUAGE", "MIMALLOC_VERBOSE",
    "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL", "CARES_HOSTS", "HOSTALIASES", "LOCALDOMAIN", "RES_OPTIONS",
  ]) {
    it(`denies ${name}`, () => {
      assert.notEqual(runEnvNameViolation(name), null);
    });
  }
});

describe("worker-protected keys are all denied", () => {
  for (const key of PROTECTED_ENV_KEYS) {
    it(`PROTECTED_ENV_KEYS member ${key}`, () => {
      assert.notEqual(runEnvNameViolation(key), null);
    });
  }
  for (const key of COMMAND_ENV_PROTECTED_KEYS) {
    it(`COMMAND_ENV_PROTECTED_KEYS member ${key}`, () => {
      assert.notEqual(runEnvNameViolation(key), null);
    });
  }

  it("COMMAND_ENV_PROTECTED_KEYS mirrors every PROTECTED_ENV_KEYS member", () => {
    const missing = [...PROTECTED_ENV_KEYS].filter((k) => !COMMAND_ENV_PROTECTED_KEYS.has(k));
    assert.deepEqual([], missing);
  });
});
