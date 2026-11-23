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

// PRD 4416 "Name rules (shared fixture)": the agent copy of the run-env name rules must
// equal fixtures/run-env/name-rules.json, which the api copy is also tested against.
const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/run-env/name-rules.json", import.meta.url), "utf8"),
) as {
  harness_versions: { claude_agent_sdk: string; codex: string };
  denied_names: string[];
  denied_prefixes: string[];
  denied_suffixes: string[];
  overlaps: { name: string; harness: string; decision: "allowed" | "denied"; reason: string }[];
};

const NAME_GRAMMAR = /^[A-Z_][A-Z0-9_]{0,127}$/;

describe("run-env name rules match the shared fixture", () => {
  it("denied names, prefixes and suffixes are identical, in order", () => {
    assert.deepEqual([...RUN_ENV_DENIED_NAMES], fixture.denied_names);
    assert.deepEqual([...RUN_ENV_DENIED_PREFIXES], fixture.denied_prefixes);
    assert.deepEqual([...RUN_ENV_DENIED_SUFFIXES], fixture.denied_suffixes);
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
  for (const name of fixture.denied_names) {
    // npm_config_cache fails the name grammar, which rejects it before any list is consulted.
    if (!NAME_GRAMMAR.test(name)) continue;
    it(`denies name ${name} by the exact-name rule`, () => {
      const v = runEnvNameViolation(name);
      assert.ok(v !== null && v.includes("(denied name)"), String(v));
    });
  }
  for (const o of fixture.overlaps) {
    it(`overlap ${o.name} (${o.harness}) is ${o.decision}`, () => {
      const v = runEnvNameViolation(o.name);
      assert.ok(o.reason.trim() !== "");
      if (o.decision === "allowed") assert.equal(v, null);
      else assert.ok(v !== null && !v.startsWith("must match "), `denied overlap must be refused by a reserved rule, got ${String(v)}`);
    });
  }
  for (const name of ["SystemDrive", "Temp", "Tmp"]) {
    it(`rejects ${name} by the name grammar`, () => {
      assert.ok(runEnvNameViolation(name)?.startsWith("must match "));
    });
  }
  for (const name of ["TSC_WATCHFILE", "TSC_WATCHDIRECTORY", "TSC_NONPOLLING_WATCHER"]) {
    it(`allows ${name}`, () => {
      assert.equal(runEnvNameViolation(name), null);
    });
  }
  for (const prefix of fixture.denied_prefixes) {
    it(`denies prefix ${prefix}`, () => {
      const v = runEnvNameViolation(`${prefix}SOMETHING`);
      assert.ok(v !== null && v.includes(`denied prefix ${prefix}`), String(v));
    });
  }
  for (const suffix of fixture.denied_suffixes) {
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
    "DOCKER_AUTH_CONFIG",
    "TESTCONTAINERS_HOST_OVERRIDE",
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
