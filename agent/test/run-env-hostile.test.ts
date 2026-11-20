import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { buildSdkEnv, type SdkCredential } from "../src/sdk-env.js";
import { buildCommandEnv } from "../src/codex/codex-executor.js";
import { runEnvNameViolation } from "../src/run-env-names.js";
import { runEnvDropMessage } from "../src/run-env-drop-event.js";
import type { RunEnvDrop } from "../src/run-env-merge.js";

// PRD #4416: the hostile catalogue. A run_env holding every allowed name the pinned harnesses read, every
// denied fixture entry and a smuggled PATH / BUN_BE_BUN must leave the worker-owned parts of the
// environment byte-identical to a build without any variable.

interface Fixture {
  denied_names: { value: string }[];
  denied_prefixes: { value: string }[];
  denied_suffixes: { value: string }[];
  overlaps: { name: string; decision: "allowed" | "denied" }[];
}
const fixture = JSON.parse(
  readFileSync(fileURLToPath(new URL("../../fixtures/run-env/name-rules.json", import.meta.url)), "utf8"),
) as Fixture;

const SENTINEL = "run-env-sentinel-value";
const ALLOWED_OVERLAPS = fixture.overlaps.filter((o) => o.decision === "allowed").map((o) => o.name);
const EXTRA_ALLOWED = [
  "AWS_REGION", "AWS_ACCESS_KEY_ID", "GOOGLE_APPLICATION_CREDENTIALS", "NODE_ENV", "CI", "DEBUG",
  "OTEL_EXPORTER_OTLP_ENDPOINT",
];
const PACKAGE_MANAGER = [
  "NPM_TOKEN", "NPM_CONFIG_REGISTRY", "PIP_INDEX_URL", "GRADLE_OPTS", "MAVEN_OPTS", "ORG_GRADLE_PROJECT_X",
  "TESTCONTAINERS_X", "DOCKER_AUTH_CONFIG",
];
const INJECTED = [...new Set([...ALLOWED_OVERLAPS, ...EXTRA_ALLOWED, ...PACKAGE_MANAGER])];
const DENIED = [
  ...fixture.denied_names.map((n) => n.value),
  ...fixture.denied_prefixes.map((p) => `${p.value}SAMPLE`),
  ...fixture.denied_suffixes.map((s) => `SAMPLE${s.value}`),
  "PATH", "BUN_BE_BUN",
];

function hostileRunEnv(): { name: string; value: string }[] {
  return [...INJECTED, ...DENIED].map((name) => ({ name, value: `${SENTINEL}-${name}` }));
}

function defined(e: Record<string, string | undefined>): Record<string, string> {
  return Object.fromEntries(Object.entries(e).filter(([, v]) => v !== undefined)) as Record<string, string>;
}

/** The shared assertions: `base` is the build without variables, `withVars` the build with the catalogue. */
function assertCatalogue(base: Record<string, string | undefined>, withVars: Record<string, string | undefined>, drops: RunEnvDrop[]): void {
  // Every worker key (including keys pinned to undefined) is byte-identical to the no-variable build.
  for (const [k, v] of Object.entries(base)) {
    assert.equal(Object.hasOwn(withVars, k), true, `${k} present in the baseline is missing`);
    assert.equal(withVars[k], v, `${k} changed`);
  }
  // Every denied name is absent unless the worker set it (then it equals the worker value, checked above).
  for (const name of DENIED) {
    if (!Object.hasOwn(base, name)) assert.equal(Object.hasOwn(withVars, name), false, `denied ${name} reached the environment`);
  }
  // Allowed names are injected with their value, unless the worker already owns the key.
  for (const name of INJECTED) {
    assert.equal(runEnvNameViolation(name), null, `${name} should be allowed by the name rules`);
    if (!Object.hasOwn(base, name)) assert.equal(withVars[name], `${SENTINEL}-${name}`, `${name} not injected`);
  }
  // Nothing else was added.
  const added = Object.keys(withVars).filter((k) => !Object.hasOwn(base, k));
  assert.deepEqual([...added].sort(), INJECTED.filter((n) => !Object.hasOwn(base, n)).sort());
  // Authority families: no git/forge variable added; managed dirs and network wiring unchanged.
  assert.deepEqual(added.filter((k) => /^(GIT_|GH_|GITLAB_|GITHUB_|GLAB_|SSH_)/.test(k)), []);
  for (const k of ["HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOTMPDIR", "npm_config_cache", "DOCKER_HOST", "NODE_USE_ENV_PROXY", "SSLKEYLOGFILE"]) {
    assert.equal(withVars[k], base[k], `${k} changed`);
  }
  for (const k of Object.keys(withVars)) {
    if (/^XDG_|_PROXY$|^SSL_CERT_|^NODE_EXTRA_CA_CERTS$/.test(k)) assert.equal(withVars[k], base[k], `${k} changed`);
  }
  // Each denied name produced a drop naming its rule; no drop carries a value.
  const dropped = new Set(drops.map((d) => d.name));
  for (const name of DENIED) assert.equal(dropped.has(name), true, `no drop event for ${name}`);
  for (const d of drops) assert.equal(JSON.stringify(d).includes(SENTINEL), false, "a drop leaked a value");
}

const oauth: SdkCredential = { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" };
const provider: SdkCredential = {
  kind: "provider",
  baseUrl: "https://gw.example.test/anthropic",
  authStyle: "bearer",
  key: ["gw", "key", "do-not-scan", "4444", "abcdef"].join("-"),
  modelMap: { opus: { model: "corp-opus" }, sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } },
};

describe("run_env hostile catalogue: buildSdkEnv", () => {
  for (const [label, credential] of [["vendor oauth", oauth], ["provider-bound", provider]] as const) {
    it(`${label}: worker-owned keys are byte-identical and the catalogue is bounded`, () => {
      const toolEnv = { PATH: "/tool/bin:/usr/bin", NIX_SSL_CERT_FILE: "/etc/ssl/ca.pem" };
      const base = buildSdkEnv(credential, "/data/home", toolEnv, "unix:///var/run/docker.sock");
      const drops: RunEnvDrop[] = [];
      const withVars = buildSdkEnv(
        credential, "/data/home", toolEnv, "unix:///var/run/docker.sock", undefined, "/data/gowork", undefined,
        hostileRunEnv(), (d) => drops.push(d),
      );
      // The managed Go work dir is the one extra worker input here, so compare against a matching baseline.
      const baseGo = buildSdkEnv(credential, "/data/home", toolEnv, "unix:///var/run/docker.sock", undefined, "/data/gowork");
      assertCatalogue(baseGo, withVars, drops);
      assert.equal(base.GOTMPDIR, undefined);
      if (credential.kind === "oauth") {
        assert.equal(withVars.CLAUDE_CODE_OAUTH_TOKEN, credential.token);
        assert.equal(withVars.ANTHROPIC_API_KEY, undefined);
      } else {
        assert.equal(withVars.CLAUDE_CODE_OAUTH_TOKEN, undefined);
        assert.equal(withVars.ANTHROPIC_BASE_URL, credential.baseUrl);
        assert.equal(withVars.ANTHROPIC_AUTH_TOKEN, credential.key);
        assert.equal(withVars.ANTHROPIC_DEFAULT_OPUS_MODEL, "corp-opus");
      }
    });
  }

  it("a key the worker already set is kept and the variable is dropped with a rule", () => {
    const drops: RunEnvDrop[] = [];
    const e = buildSdkEnv(oauth, "/h", { AWS_REGION: "worker-region" }, undefined, undefined, undefined, undefined,
      [{ name: "AWS_REGION", value: "user-region" }], (d) => drops.push(d));
    assert.equal(e.AWS_REGION, "worker-region");
    assert.equal(drops.length, 1);
    assert.match(drops[0]?.rule ?? "", /already set/);
  });

  it("no runEnv is byte-identical to today's build", () => {
    assert.deepEqual(buildSdkEnv(oauth, "/h", {}, undefined, undefined, undefined, undefined, undefined), buildSdkEnv(oauth, "/h"));
    assert.deepEqual(defined(buildSdkEnv(oauth, "/h", {}, undefined, undefined, undefined, undefined, [])), defined(buildSdkEnv(oauth, "/h")));
  });
});

describe("run_env hostile catalogue: buildCommandEnv", () => {
  it("worker-owned keys are byte-identical and the catalogue is bounded", () => {
    const toolEnv = { PATH: "/tool/bin", NIX_SSL_CERT_FILE: "/etc/ssl/ca.pem" };
    const base = buildCommandEnv("/tmp/cmd", toolEnv, "unix:///var/run/docker.sock", true);
    const drops: RunEnvDrop[] = [];
    const withVars = buildCommandEnv("/tmp/cmd", toolEnv, "unix:///var/run/docker.sock", true, hostileRunEnv(), (d) => drops.push(d));
    assertCatalogue(base, withVars, drops);
    assert.equal(withVars.TMPDIR, "/tmp/cmd");
    assert.equal(withVars.LANG, "C");
  });

  it("a key already present is kept; a hostile name cannot forge a run event line", () => {
    const drops: RunEnvDrop[] = [];
    const e = buildCommandEnv("/t", { AWS_REGION: "worker" }, undefined, false,
      [{ name: "AWS_REGION", value: "u" }, { name: "BAD\nNAME\u001b[31m", value: "v" }], (d) => drops.push(d));
    assert.equal(e.AWS_REGION, "worker");
    assert.equal(drops.length, 2);
    const text = String(runEnvDropMessage(drops[1]!).payload.text);
    assert.equal(text.includes("\n") || text.includes("\u001b"), false);
    assert.match(text, /not set for this run/);
  });

  it("no runEnv is byte-identical to today's build", () => {
    assert.deepEqual(buildCommandEnv("/t", {}, undefined, false, undefined), buildCommandEnv("/t", {}));
  });
});
