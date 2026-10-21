#!/usr/bin/env node
// Exercise the configured regex itself against real pins and synthetic updates.
import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
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
console.log("Renovate Postgres contract: PASS");
