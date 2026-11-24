import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { rootCertificates } from "node:tls";
import { captureExtraCa, type ValidatedExtraCa } from "../src/codex/extra-ca.js";
import { launchCodexRoot, type CodexLaunchSpec, type RunnerTreeRequest } from "../src/codex/launcher.js";

function spec(ca?: ValidatedExtraCa): CodexLaunchSpec {
  return {
    ownedDataRoot: "/data/run/ca-root", kind: "provider",
    providerBinding: {
      provider_id: "ca-gateway", revision: 1, engine: "codex",
      protocol: "openai_responses", base_url: "https://gateway.example/v1",
      auth_style: "bearer", credential: "provider-fixture-credential",
      model_map: { default: { model: "gateway-model", context_window: 64000 } },
    },
    authMode: "provider", useAppServerAuth: true, model: "default",
    codexBin: "/opt/uzi-codex/0.171.0/bin/codex",
    supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
    childArgv: ["app-server"], cwd: "/work/repo",
    ...(ca ? { codexExtraCa: ca } : {}),
  };
}
it("provisions captured bytes per root and ignores ambient CA sources", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "launch-ca-"));
  try {
    const source = path.join(dir, "source.pem");
    const pem = "\r\n" + rootCertificates[0]! + "\n";
    fs.writeFileSync(source, pem);
    const ca = captureExtraCa(source)!;
    fs.unlinkSync(source);
    const { makeProductionCodexAdviceHarnessFactory } = await import("../src/codex/codex-executor.js");
    const { createCodexProviderExecutionAuthority, selectCodexBinding } = await import("../src/codex/select.js");
    const { nullLogger } = await import("./helpers.js");
    const vendor = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "vendor-fixture-credential", capability: "fixture-capability" } });
    assert.equal(vendor.kind, "codex");
    if (vendor.kind !== "codex") throw new Error("vendor fixture refused");
    for (const authority of [createCodexProviderExecutionAuthority(spec().providerBinding!), vendor.binding]) {
      let seen: import("../src/codex/codex-advice-harness.js").CodexAdviceLaunchSpec | undefined;
      const factory = makeProductionCodexAdviceHarnessFactory({ releaseCodex: async () => ({ auth_mode: "api_key", access_token: "vendor-fixture-credential" }) } as never, nullLogger(), "/unused", {
        codexExtraCa: ca,
        launchRoot: async launch => { seen = launch; throw new Error("stop at public launch seam"); },
      });
      const harness = await factory({ runId: "ca-advice", binding: authority });
      await assert.rejects(harness.run({
        label: "review", systemPrompt: "reviewer", prompt: "review", model: "gpt-6-astra",
        output: { kind: "text" }, signal: new AbortController().signal, timeoutMs: 5000,
      }, { onTerminal: () => {} }));
      assert.ok(seen);
      assert.equal(seen.codexExtraCa, authority.authMode === "provider" ? ca : undefined);
      assert.equal(Object.hasOwn(seen, "codexExtraCa"), authority.authMode === "provider");
    }
    for (const caInput of [ca, undefined, ca]) {
      let request: RunnerTreeRequest | undefined;
      let childEnv: NodeJS.ProcessEnv | undefined;
      await assert.rejects(launchCodexRoot(spec(caInput), {
        env: { UZI_UID_SPLIT: "1", CODEX_CA_CERTIFICATE: "/ambient", NODE_EXTRA_CA_CERTS: "/ambient-node", SSL_CERT_FILE: "/ambient-ssl", UZI_CODEX_CA_FILE: source },
        resolveRunnerUid: () => 10002,
        assertNoUnexpectedSystemConfig: () => {},
        makeRunnerTrees: req => { request = req; },
        removeRunnerTree: () => {},
        spawnSupervisor: (_command, _args, options) => {
          childEnv = options.env;
          throw Object.assign(new Error("injected spawn failure"), { code: "ENOENT" });
        },
      }), error => { assert.ok(request, String(error)); return true; });
      assert.ok(request);
      assert.ok(childEnv);
      const file = request.files.find(f => f.path.endsWith("/provider-extra-ca.crt"));
      if (caInput) {
        assert.deepEqual(file, { path: "/data/run/ca-root/codex/provider-extra-ca.crt", content: pem, mode: 0o600 });
        assert.equal(childEnv.CODEX_CA_CERTIFICATE, file!.path);
      } else {
        assert.equal(file, undefined);
        assert.equal(childEnv.CODEX_CA_CERTIFICATE, undefined);
      }
      for (const key of ["NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "UZI_CODEX_CA_FILE"]) assert.equal(childEnv[key], undefined);
      assert.equal(childEnv.CODEX_EXEC_SERVER_URL, "none");
    }
    for (const invalid of [
      spec({ ...ca }),
      { ...spec(ca), providerBinding: undefined, provider: { name: "openai", baseUrl: "https://api.openai.com/v1", envKey: "KEY" }, authMode: "api_key" as const },
    ]) {
      let provisioned = false;
      await assert.rejects(launchCodexRoot(invalid, {
        env: { UZI_UID_SPLIT: "1" }, resolveRunnerUid: () => 10002,
        assertNoUnexpectedSystemConfig: () => {},
        makeRunnerTrees: () => { provisioned = true; },
      }));
      assert.equal(provisioned, false);
    }
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
