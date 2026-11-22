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
type Rule = { value: string; invariant: string; note: string };
const fixture = JSON.parse(
  fs.readFileSync(new URL("../../fixtures/run-env/name-rules.json", import.meta.url), "utf8"),
) as {
  harness_versions: { claude_agent_sdk: string; codex: string };
  invariants: string[];
  denied_names: Rule[];
  denied_prefixes: Rule[];
  denied_suffixes: Rule[];
};

describe("run-env name rules match the shared fixture", () => {
  it("denied names, prefixes and suffixes are identical, in order, with invariant and note", () => {
    assert.deepEqual([...RUN_ENV_DENIED_NAMES], fixture.denied_names);
    assert.deepEqual([...RUN_ENV_DENIED_PREFIXES], fixture.denied_prefixes);
    assert.deepEqual([...RUN_ENV_DENIED_SUFFIXES], fixture.denied_suffixes);
  });

  it("every entry names an invariant from the enum and carries a note", () => {
    assert.equal(fixture.invariants.length, 8);
    for (const r of [...fixture.denied_names, ...fixture.denied_prefixes, ...fixture.denied_suffixes]) {
      assert.ok(fixture.invariants.includes(r.invariant), `${r.value}: invariant ${r.invariant}`);
      assert.ok(r.note.trim().length > 0, `${r.value}: empty note`);
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
  for (const { value: name } of fixture.denied_names) {
    it(`denies name ${name}`, () => {
      assert.notEqual(runEnvNameViolation(name), null);
    });
  }
  for (const { value: prefix } of fixture.denied_prefixes) {
    it(`denies prefix ${prefix}`, () => {
      const v = runEnvNameViolation(`${prefix}SOMETHING`);
      assert.ok(v !== null && v.includes(`denied prefix ${prefix}`), String(v));
    });
  }
  for (const { value: suffix } of fixture.denied_suffixes) {
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
    "NPM_CONFIG_REGISTRY",
    "PIP_INDEX_URL",
    "MAVEN_OPTS",
    "ORG_GRADLE_PROJECT_X",
    "TESTCONTAINERS_X",
    "NODE_ENV",
    "NODE_AUTH_TOKEN",
    "UV_INDEX_URL",
    "TZ",
    "GOOGLE_APPLICATION_CREDENTIALS",
    "DOCKER_AUTH_CONFIG",
    "TESTCONTAINERS_HOST_OVERRIDE",
    "_LEADING_UNDERSCORE",
    "A",
    "A".repeat(128),
    "OTEL_SERVICE_NAME",
    "OTEL_EXPORTER_PROMETHEUS_PORT",
    "AWS_ENDPOINT_URL_S3",
    "AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE",
    "ENABLE_FEATURE_X",
  ]) {
    it(`allows ${name.length > 20 ? `${name.length}-char name` : name}`, () => {
      assert.equal(runEnvNameViolation(name), null);
    });
  }

  for (const name of [
    "ANTHROPIC_CUSTOM_HEADERS",
    "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL",
    "ANT_OTEL_EXPORTER_OTLP_ENDPOINT",
    "OTEL_EXPORTER_OTLP_ENDPOINT",
    "OTEL_EXPORTER_OTLP_HEADERS",
    "GCE_METADATA_HOST",
    "AWS_ENDPOINT_URL",
    "AWS_ENDPOINT_URL_STS",
    "AWS_ENDPOINT_URL_SSO",
    "AWS_ENDPOINT_URL_SSO_OIDC",
    "AWS_ENDPOINT_URL_BEDROCK",
    "AWS_ENDPOINT_URL_BEDROCK_RUNTIME",
    "NODE",
    "NODE_NO_WARNINGS",
    "NODE_SKIP_PLATFORM_CHECK",
    "NODE_PENDING_PIPE_INSTANCES",
    "UV_USE_IO_URING",
    "AWS_EC2_METADATA_SERVICE_ENDPOINT",
    "OTEL_LOG_USER_PROMPTS",
    "DEBUG",
    "LC_ALL",
    "LC_CTYPE",
  ]) {
    it(`denies registry name ${name}`, () => {
      assert.notEqual(runEnvNameViolation(name), null);
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
