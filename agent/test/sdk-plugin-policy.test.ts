import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { it } from "node:test";
import * as ts from "typescript/unstable/ast";
import { API } from "typescript/unstable/sync";
import { claudeSdkSettings } from "../src/sdk-plugin-policy.js";

const agent = fileURLToPath(new URL("../", import.meta.url));
const plugin = "cc-plugin-privacy@builtin";

function propertyName(node: ts.PropertyName): string | undefined {
  return ts.isIdentifier(node) || ts.isStringLiteral(node) ? node.text : undefined;
}

/** Discover option literals, not a curated list of builder names or filenames. */
function inspect(source: ts.SourceFile): string[] {
  const aliases = new Set(["SdkOptions"]);
  const importedPolicy = new Set<string>();
  const queryNames = new Set(["query", "queryFn", "defaultQueryFn"]);
  source.forEachChild(node => {
    if (!ts.isImportDeclaration(node) || !ts.isStringLiteral(node.moduleSpecifier)) return;
    const bindings = node.importClause?.namedBindings;
    if (!bindings || !ts.isNamedImports(bindings)) return;
    for (const binding of bindings.elements) {
      if (node.moduleSpecifier.text === "@anthropic-ai/claude-agent-sdk" && (binding.propertyName?.text ?? binding.name.text) === "Options") aliases.add(binding.name.text);
      if (node.moduleSpecifier.text === "@anthropic-ai/claude-agent-sdk" && (binding.propertyName?.text ?? binding.name.text) === "query") queryNames.add(binding.name.text);
      if (node.moduleSpecifier.text === "./sdk-plugin-policy.js" && (binding.propertyName?.text ?? binding.name.text) === "claudeSdkSettings") importedPolicy.add(binding.name.text);
    }
  });
  const found: string[] = [];
  const optionVariables = new Set<string>();
  const queryLiterals = new Set<ts.Node>();
  function visit(node: ts.Node, parent?: ts.Node): void {
    if (ts.isCallExpression(node)) {
      const expression = node.expression;
      const name = ts.isIdentifier(expression) ? expression.text : ts.isPropertyAccessExpression(expression) ? expression.name.text : undefined;
      const arg = node.arguments[0];
      if (name && queryNames.has(name) && arg && ts.isObjectLiteralExpression(arg)) {
        const options = arg.properties.filter(ts.isPropertyAssignment).find(p => propertyName(p.name) === "options")?.initializer;
        if (options && ts.isObjectLiteralExpression(options)) queryLiterals.add(options);
      }
    }
    if (ts.isObjectLiteralExpression(node)) {
      const variable = parent && ts.isVariableDeclaration(parent) ? parent : undefined;
      const typedOptions = variable?.type && ts.isTypeReferenceNode(variable.type) && ts.isIdentifier(variable.type.typeName) && aliases.has(variable.type.typeName.text);
      const props = node.properties.filter(ts.isPropertyAssignment);
      if (typedOptions || queryLiterals.has(node) || props.some(p => ["settingSources", "permissionMode", "allowDangerouslySkipPermissions"].includes(propertyName(p.name) ?? ""))) {
        const label = `${source.fileName}:${node.pos}`;
        found.push(label);
        if (variable && ts.isIdentifier(variable.name)) optionVariables.add(variable.name.text);
        const settings = props.find(p => propertyName(p.name) === "settings")?.initializer;
        assert.ok(settings && ts.isCallExpression(settings) && ts.isIdentifier(settings.expression) && importedPolicy.has(settings.expression.text), `${label}: SDK options must use the shared builtin-plugin settings policy`);
      }
    }
    node.forEachChild(child => visit(child, node));
  }
  visit(source);
  function rejectOverwrite(node: ts.Node): void {
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
      const left = node.left;
      if (ts.isPropertyAccessExpression(left) && left.name.text === "settings" && ts.isIdentifier(left.expression) && optionVariables.has(left.expression.text)) {
        assert.fail(`${source.fileName}: do not overwrite the shared SDK settings after construction`);
      }
    }
    node.forEachChild(rejectOverwrite);
  }
  rejectOverwrite(source);
  return found;
}

it("every discovered Claude SDK option builder uses the shared builtin plugin disable", t => {
  const src = path.join(agent, "src");
  const candidates = readdirSync(src, { recursive: true, encoding: "utf8" }).filter(p => p.endsWith(".ts")).map(p => path.join(src, p)).filter(p => {
    const text = readFileSync(p, "utf8");
    return text.includes("settingSources:") || text.includes("queryFn") || text.includes("@anthropic-ai/claude-agent-sdk");
  });
  const api = new API({ cwd: agent });
  try {
    const snapshot = api.updateSnapshot({ openFiles: candidates });
    const inventory: string[] = [];
    for (const filename of candidates) {
      const program = snapshot.getDefaultProjectForFile(filename)?.program;
      const source = program?.getSourceFile(filename);
      assert.ok(source, `${filename}: actual production source must be parsed`);
      inventory.push(...inspect(source));
    }
    assert.ok(inventory.length > 0, "must inspect real SDK option builders");
    t.diagnostic(`checked ${inventory.length} automatically discovered SDK option literals`);
  } finally { api.close(); }
});

it("a future builder without the policy and a later settings overwrite are detected", () => {
  const scratchRoot = path.join(agent, "../.uzi/scratch");
  mkdirSync(scratchRoot, { recursive: true });
  const root = mkdtempSync(path.join(scratchRoot, "plugin-policy-"));
  const api = new API({ cwd: agent });
  try {
    const samples = [
      { name: "missing", text: 'const future: SdkOptions = { settingSources: [] };', error: /shared builtin-plugin/ },
      { name: "inline-query", text: 'queryFn({ prompt: "diagnostic", options: {} });', error: /shared builtin-plugin/ },
      { name: "namespace-query", text: 'import * as SDK from "@anthropic-ai/claude-agent-sdk"; SDK.query({ prompt: "diagnostic", options: {} });', error: /shared builtin-plugin/ },
      { name: "overwrite", text: 'import { claudeSdkSettings } from "./sdk-plugin-policy.js"; const future: SdkOptions = { settingSources: [], settings: claudeSdkSettings() }; future.settings = {};', error: /do not overwrite/ },
      { name: "present", text: 'import { claudeSdkSettings } from "./sdk-plugin-policy.js"; const future: SdkOptions = { settingSources: [], settings: claudeSdkSettings() };', error: undefined },
    ];
    for (const sample of samples) {
      const filename = path.join(root, `${sample.name}.ts`);
      writeFileSync(filename, sample.text);
      const snapshot = api.updateSnapshot({ openFiles: [filename] });
      const source = snapshot.getDefaultProjectForFile(filename)?.program.getSourceFile(filename);
      assert.ok(source);
      if (sample.error) assert.throws(() => inspect(source), sample.error);
      else assert.equal(inspect(source).length, 1);
    }
  } finally { api.close(); rmSync(root, { recursive: true, force: true }); }
});

it("the shared policy preserves settings and cannot be overridden by caller plugin settings", () => {
  assert.throws(() => claudeSdkSettings("/unread/settings.json"), /must be inline/);
  const cases: Array<Record<string, boolean> | undefined> = [undefined, {}, { [plugin]: true }, { [plugin]: false, "approved-product@local": true }];
  for (const isolated of [false, true]) {
    for (const existing of cases) {
      const settings = { attribution: { commit: "", pr: "" }, disableSkillShellExecution: true, ...(existing ? { enabledPlugins: existing } : {}) };
      const result = claudeSdkSettings(settings, isolated);
      assert.equal(result.enabledPlugins?.[plugin], false);
      assert.equal(result.disableSkillShellExecution, true);
      assert.deepEqual(result.attribution, settings.attribution);
      if (existing?.["approved-product@local"]) assert.equal(result.enabledPlugins?.["approved-product@local"], true);
      assert.equal(result.enabledPlugins?.["agents-md@builtin"], isolated ? false : undefined);
      assert.equal(result.enabledPlugins?.["cc-plugin-plugin-authoring@builtin"], isolated ? false : undefined);
    }
  }
});
