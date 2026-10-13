import { it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import * as ts from "typescript/unstable/ast";
import { API } from "typescript/unstable/sync";
import { fileURLToPath } from "node:url";
import { CODEX_BIN } from "../src/codex/launcher.js";
import { CODEX_PROBE_EXPECTATION } from "../src/codex/codex-runtime-probe.js";

const lock = readFileSync(new URL("../codex/codex-package.lock", import.meta.url), "utf8");
function value(key: string): string {
  const entries = lock.split("\n").filter(line => line.startsWith(`${key}=`));
  assert.equal(entries.length, 1, `unique lock field ${key}`);
  return entries[0]!.slice(key.length + 1);
}
const version = value("CODEX_VERSION");

it("the imported production launcher targets the locked install root", () => {
  assert.equal(CODEX_BIN, `/opt/uzi-codex/${version}/bin/codex`);
});

it("the imported runtime probe expects the locked version", () => {
  assert.equal(CODEX_PROBE_EXPECTATION.version, version);
  assert.equal(value("CODEX_MANIFEST_VERSION"), version);
  assert.equal(value("CODEX_TAG"), `rust-v${version}`);
});

for (const arch of ["amd64", "arm64"]) {
  it(`the imported runtime probe expects the locked ${arch} digest`, () => {
    assert.equal(CODEX_PROBE_EXPECTATION.lockDigest[arch], value(`CODEX_SHA256_${arch}`));
    assert.equal(value(`CODEX_TAG_${arch}`), value("CODEX_TAG"));
  });
}

// Inspect syntax only: importing these P-layer tests would execute the real runtime.
// Each traversal is bounded by the source's 64 KiB limit; only the TS parser runs.
for (const file of ["large-resume.test.ts", "token-resume.test.ts"]) {
  it(`active M4 ${file} checks and records the locked binary version`, () => {
    const source = readFileSync(new URL(`../../e2e/codex-m4/${file}`, import.meta.url), "utf8");
    assert.ok(Buffer.byteLength(source) <= 64 * 1024);
    const filename = fileURLToPath(new URL(`../../e2e/codex-m4/${file}`, import.meta.url));
    const api = new API({ cwd: fileURLToPath(new URL("../", import.meta.url)) });
    try {
      const snapshot = api.updateSnapshot({ openFiles: [filename] });
      const program = snapshot.getDefaultProjectForFile(filename)?.program;
      assert.ok(program, "M4 source project available");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, "valid M4 syntax");
      const ast = program.getSourceFile(filename);
      assert.ok(ast, "actual active M4 source available");
      const calls: ts.CallExpression[] = [];
      const declarations: ts.VariableDeclaration[] = [];
      const imports: ts.ImportDeclaration[] = [];
      function visit(node: ts.Node): void {
        if (ts.isCallExpression(node)) calls.push(node);
        if (ts.isVariableDeclaration(node)) declarations.push(node);
        if (ts.isImportDeclaration(node)) imports.push(node);
        node.forEachChild(visit);
      }
      visit(ast);
      const namedCall = (call: ts.CallExpression, name: string): boolean =>
        ts.isIdentifier(call.expression) && call.expression.text === name;
      assert.ok(imports.some(node => ts.isStringLiteral(node.moduleSpecifier)
        && node.moduleSpecifier.text === "./provision.js"
        && node.importClause?.namedBindings && ts.isNamedImports(node.importClause.namedBindings)
        && node.importClause.namedBindings.elements.some(binding =>
          binding.name.text === "assertBinaryVersion" && !binding.propertyName)));
      const checks = calls.filter(call => namedCall(call, "assertBinaryVersion"));
      assert.equal(checks.length, 1, "one actual binary-version assertion");
      const expected = checks[0]!.arguments[1];
      assert.ok(expected && ts.isStringLiteral(expected), "explicit pinned second argument");
      assert.equal(expected.text, version);

      const evidenceObjects: ts.ObjectLiteralExpression[] = [];
      for (const write of calls.filter(call => namedCall(call, "writeFileSync"))) {
        const serialized = write.arguments[1];
        if (!serialized || !ts.isCallExpression(serialized)
          || !ts.isPropertyAccessExpression(serialized.expression)
          || !ts.isIdentifier(serialized.expression.expression)
          || serialized.expression.expression.text !== "JSON"
          || serialized.expression.name.text !== "stringify") continue;
        let payload = serialized.arguments[0];
        if (payload && ts.isIdentifier(payload)) {
          const name = payload.text;
          const bindings = declarations.filter(node => ts.isIdentifier(node.name) && node.name.text === name);
          assert.equal(bindings.length, 1, "unique serialized evidence binding");
          payload = bindings[0]!.initializer;
        }
        if (payload && ts.isObjectLiteralExpression(payload)) evidenceObjects.push(payload);
      }
      const versions = evidenceObjects.flatMap(object => object.properties.filter(
        (property): property is ts.PropertyAssignment => ts.isPropertyAssignment(property)
          && (ts.isIdentifier(property.name) || ts.isStringLiteral(property.name))
          && property.name.text === "version",
      ));
      assert.equal(versions.length, 1, "one version field in actual written evidence");
      const recorded = versions[0]!.initializer;
      assert.ok(ts.isStringLiteral(recorded), "explicit pinned evidence version");
      assert.equal(recorded.text, version);
    } finally {
      api.close();
    }
  });
}
