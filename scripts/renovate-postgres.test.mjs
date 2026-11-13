#!/usr/bin/env node
// Exercise the configured regex itself against real pins and synthetic updates.
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
const read = (name) => readFileSync(new URL(`../${name}`, import.meta.url), "utf8");
const config = JSON.parse(read("renovate.json"));
const workflow = read(".github/workflows/ci.yml");
const manager = config.customManagers.find((m) => m.datasourceTemplate === "docker" &&
  m.matchStrings.some((s) => s.includes("UZI_CI_PG_IMAGE")));
assert.ok(manager, "dedicated Postgres manager must exist");
assert.equal(manager.versioningTemplate, "docker");
assert.equal(manager.matchStrings.length, 1);
assert.ok(manager.managerFilePatterns.some((p) =>
  new RegExp(p.slice(1, -1)).test(".github/workflows/ci.yml")));
const pattern = manager.matchStrings[0];
const extract = (s) => [...s.matchAll(new RegExp(pattern, "g"))].map((m) => m.groups);
const deps = extract(workflow);
assert.equal(deps.length, 1, "one CI Postgres pin must be extracted");
const dep = deps[0];
assert.equal(dep.depName, "mirror.gcr.io/library/postgres");
const compose = read("docker-compose.yml").match(/image: postgres:([^\s@]+)@(sha256:[0-9a-f]{64})/);
assert.ok(compose);
assert.equal(dep.currentValue, compose[1]);
assert.equal(dep.currentDigest, compose[2]);
const group = config.packageRules.find((r) => r.groupName === "postgres");
assert.ok(group?.matchDepNames.includes(dep.depName));
assert.ok(group.matchDepNames.includes("postgres"), "compose and CI must stay grouped");
// Simulate Renovate's captured version/digest substitutions, including a major bump.
const oldPin = `${dep.currentValue}@${dep.currentDigest}`;
const newDigest = `sha256:${"a".repeat(64)}`;
const updated = workflow.replace(oldPin, `18@${newDigest}`);
assert.equal(extract(updated).length, 1);
assert.equal(extract(updated)[0].currentValue, "18");
assert.equal(extract(updated)[0].currentDigest, newDigest);
for (const invalid of [
  workflow.replace(`@${dep.currentDigest}`, ""),
  workflow.replace(dep.currentDigest, "sha256:abc"),
  workflow.replace(dep.currentDigest, `${dep.currentDigest}a`),
  workflow.replace(dep.depName, "docker.io/library/postgres"),
  workflow.replace("UZI_CI_PG_IMAGE:", "OTHER_IMAGE:"),
]) assert.equal(extract(invalid).length, 0, "malformed or unrelated refs must not match");

const extracted = [{ file: ".github/workflows/ci.yml", manager: "custom.regex", ...dep }];
for (const file of ["docker-compose.yml", "deploy/compose/compose.yaml"]) {
  // Both filenames are supported by Renovate's built-in docker-compose manager.
  const pins = [...read(file).matchAll(/image: (?<depName>postgres):(?<currentValue>[^\s@]+)@(?<currentDigest>sha256:[0-9a-f]{64})/g)];
  assert.equal(pins.length, 1, `${file}: one compose dependency`);
  extracted.push({ file, manager: "docker-compose", ...pins[0].groups });
}
// The native helm-values manager understands repository + tag@digest already;
// do not add a duplicate custom manager for this chart image (#4275).
const chart = read("deploy/chart/values.yaml").match(/repository: (?<depName>postgres)\n      tag: "(?<currentValue>[^@\s]+)@(?<currentDigest>sha256:[0-9a-f]{64})"/);
assert.ok(chart, "chart PostgreSQL default must remain a native helm-values image block");
assert.ok(!config.customManagers.some((m) => m.datasourceTemplate === "docker" &&
  m.managerFilePatterns.some((p) => new RegExp(p.slice(1, -1)).test("deploy/chart/values.yaml"))),
"native chart extraction must not have a duplicate custom manager");
extracted.push({ file: "deploy/chart/values.yaml", manager: "helm-values", ...chart.groups });
for (const file of ["e2e/codex-m3b/testserver.sh", "e2e/codex-m3b/run-lifecycle.sh"]) {
  const managers = config.customManagers.filter((m) => m.datasourceTemplate === "docker" &&
    m.managerFilePatterns.some((p) => new RegExp(p.slice(1, -1)).test(file)));
  assert.equal(managers.length, 1, `${file}: one applicable regex manager`);
  const current = managers[0];
  assert.equal(current.versioningTemplate, "docker");
  assert.equal(current.matchStrings.length, 1);
  const match = (source) => [...source.matchAll(new RegExp(current.matchStrings[0], "g"))];
  const source = read(file);
  const matches = match(source);
  assert.equal(matches.length, 1, `${file}: one extracted dependency`);
  const pin = matches[0].groups;
  assert.equal(pin.depName, "postgres");
  extracted.push({ file, manager: "custom.regex", ...pin });
  const update = source.replace(`${pin.currentValue}@${pin.currentDigest}`, `18@${newDigest}`);
  assert.equal(match(update)[0].groups.currentValue, "18");
  assert.equal(match(update)[0].groups.currentDigest, newDigest);
  for (const bad of [source.replace(`@${pin.currentDigest}`, ""), source.replace(pin.currentDigest, "sha256:abc"),
    source.replace(pin.currentDigest, `${pin.currentDigest}a`)]) {
    assert.equal(match(bad).length, 0, `${file}: malformed pin must not match`);
  }
  if (file.endsWith(".sh")) {
    // Execute only the allowlisted assignment matched by the configured regex,
    // not the script: no docker, server or credentials enter this hermetic test.
    const assignment = matches[0][0].trim();
    const key = file.endsWith("testserver.sh") ? "CDR_M3B_PG_IMAGE" : "PG_IMAGE";
    const expanded = (override) => execFileSync("bash", ["--noprofile", "--norc", "-c",
      `${assignment}\nprintf '%s' "$${key}"`], { env: { PATH: process.env.PATH, ...override }, encoding: "utf8" });
    assert.equal(expanded({}), `postgres:${pin.currentValue}@${pin.currentDigest}`);
    assert.equal(expanded({ CDR_M3B_PG_IMAGE: "postgres:custom-test" }), "postgres:custom-test");
  }
}
assert.equal(extracted.length, 6, "all six shared pins must be extracted");
for (const item of extracted) {
  assert.equal(item.currentValue, dep.currentValue, `${item.file}: shared tag`);
  assert.equal(item.currentDigest, dep.currentDigest, `${item.file}: shared digest`);
  assert.ok(group.matchDepNames.includes(item.depName), `${item.file}: postgres group`);
  console.log(`${item.file}: ${item.depName}:${item.currentValue}@${item.currentDigest} (${item.manager}, group=postgres)`);
}
console.log("Renovate Postgres contract: PASS");
