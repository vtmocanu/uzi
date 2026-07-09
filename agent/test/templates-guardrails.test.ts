import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// Every worker template image (PRD #18) must keep the guardrail-relevant layers
// the base image establishes: run as the non-root uzi user, and use tini as PID 1
// so SIGTERM reaches the worker. A variant may ADD packages but must never drop
// these — this test turns the "keep in lockstep with base" Dockerfile comment into
// an enforced invariant, cheaply (text parse, no docker), before more variants land.

const templatesDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../templates");

function templateDockerfiles(): { name: string; text: string }[] {
  return fs
    .readdirSync(templatesDir, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => {
      const file = path.join(templatesDir, e.name, "Dockerfile");
      return { name: e.name, text: fs.readFileSync(file, "utf8") };
    });
}

describe("worker template Dockerfiles keep guardrail layers", () => {
  const dockerfiles = templateDockerfiles();

  it("finds at least the base template", () => {
    assert.ok(
      dockerfiles.some((d) => d.name === "base"),
      "expected agent/templates/base/Dockerfile to exist",
    );
  });

  for (const { name, text } of dockerfiles) {
    it(`${name}: runs as the non-root uzi user`, () => {
      assert.match(text, /^\s*USER\s+uzi:uzi\s*$/m, `${name}/Dockerfile must contain "USER uzi:uzi"`);
    });

    it(`${name}: uses tini as the entrypoint`, () => {
      assert.match(text, /ENTRYPOINT\s*\[\s*"\/sbin\/tini"/, `${name}/Dockerfile must use tini as PID 1`);
    });

    it(`${name}: bakes its own template identity`, () => {
      // The reported-template drift signal depends on each image baking its name.
      assert.match(
        text,
        new RegExp(`ENV\\s+UZI_WORKER_TEMPLATE=${name}\\b`),
        `${name}/Dockerfile must bake ENV UZI_WORKER_TEMPLATE=${name}`,
      );
    });

    it(`${name}: installs the devbox provisioning engine (PRD #18 M3)`, () => {
      // Every template gains devbox so per-run tool provisioning works regardless of
      // which image a worker runs. The self-contained-per-template layout means this
      // must stay mirrored across all Dockerfiles; this pins that.
      assert.match(text, /get\.jetify\.com\/devbox/, `${name}/Dockerfile must install devbox`);
    });
  }
});
