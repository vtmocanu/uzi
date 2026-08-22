import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";

import {
  detectToolchain,
  MAX_READ_BYTES,
  MAX_SCAN_DEPTH,
} from "../src/toolchain-detect.js";

/** Build a throwaway fixture clone from a {relative path → content} map. Mirrors the
 *  `mkClone` helper in js-deps.test.ts. */
function mkClone(files: Record<string, string>): string {
  const root = mkdtempSync(join(tmpdir(), "toolchain-"));
  for (const [rel, content] of Object.entries(files)) {
    const abs = join(root, rel);
    mkdirSync(dirname(abs), { recursive: true });
    writeFileSync(abs, content);
  }
  return root;
}

const GO_MOD = "module example.com/x\n\ngo 1.22\n";
const PKG = '{"name":"x","version":"1.0.0"}';

describe("detectToolchain: docker capability", () => {
  it("(a) a Dockerfile ⇒ required_capabilities includes docker", async () => {
    const root = mkClone({ Dockerfile: "FROM alpine\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("(b) a docker-compose.yml ⇒ docker", async () => {
    const root = mkClone({ "docker-compose.yml": "services: {}\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("compose.yaml (the newer spelling) ⇒ docker", async () => {
    const root = mkClone({ "compose.yaml": "services: {}\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("a Dockerfile in a NESTED dir still ⇒ docker (any dir counts)", async () => {
    const root = mkClone({ "deploy/images/Dockerfile": "FROM alpine\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("(e) a testcontainers dependency string in package.json ⇒ docker", async () => {
    const root = mkClone({
      "package.json":
        '{"name":"x","devDependencies":{"testcontainers":"^10.0.0"}}',
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
    assert.ok(d.required_tools.includes("node"), "package.json is still a node tool");
  });

  it("a testcontainers dependency in go.mod ⇒ docker", async () => {
    const root = mkClone({
      "go.mod": GO_MOD + "require github.com/testcontainers/testcontainers-go v0.30.0\n",
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
    assert.ok(d.required_tools.includes("go"));
  });

  it("a root-level Makefile with a `docker ` token ⇒ docker", async () => {
    const root = mkClone({
      Makefile: "build:\n\tdocker build -t x .\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("a root-level *.sh script with a `docker ` token ⇒ docker", async () => {
    const root = mkClone({
      "run.sh": "#!/bin/sh\ndocker run --rm hello-world\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });

  it("a NESTED script mentioning docker does NOT trip docker (root-level scan only)", async () => {
    const root = mkClone({
      "scripts/run.sh": "#!/bin/sh\ndocker run --rm hello-world\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "the script scan is root-level only, so a nested docker mention is not a signal",
    );
  });

  // Fix #5: the script scan keys on a command-position docker INVOCATION, not the old
  // `docker ` substring, so a prose/comment mention no longer over-asserts.
  it("a COMMENT mentioning docker does NOT trip docker (prose, not an invocation)", async () => {
    const root = mkClone({
      Makefile: "build:\n\t# migrated off docker to podman last year\n\tgo build ./...\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "a docker mention inside a comment is prose, not a real invocation",
    );
    assert.ok(d.required_tools.includes("go"), "go is still inferred from go.mod");
  });

  it("a docker mention inside an echo string does NOT trip docker", async () => {
    // The word appears, but not at a command position followed by a subcommand.
    const root = mkClone({
      "run.sh": '#!/bin/sh\necho "we no longer need docker here"\ngo test ./...\n',
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.equal(d.required_capabilities.includes("docker"), false);
  });

  it("a real `docker compose up` invocation DOES trip docker", async () => {
    const root = mkClone({
      "run.sh": "#!/bin/sh\n# bring the stack up\ndocker compose up -d\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.ok(
      d.required_capabilities.includes("docker"),
      "a command-position `docker compose` is a real invocation",
    );
  });

  it("a legacy `docker-compose up` invocation DOES trip docker", async () => {
    const root = mkClone({
      Makefile: "up:\n\tdocker-compose up -d\n",
      "go.mod": GO_MOD,
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });
});

describe("detectToolchain: jvm", () => {
  it("(c) a pom.xml ⇒ jvm capability + jvm tool", async () => {
    const root = mkClone({ "pom.xml": "<project></project>\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("jvm"));
    assert.ok(d.required_tools.includes("jvm"));
  });

  it("a build.gradle.kts ⇒ jvm capability + jvm tool", async () => {
    const root = mkClone({ "build.gradle.kts": "plugins {}\n" });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("jvm"));
    assert.ok(d.required_tools.includes("jvm"));
  });
});

describe("detectToolchain: language tools", () => {
  it("(d) go.mod only ⇒ tools include go and capabilities is EMPTY (go never needs docker)", async () => {
    const root = mkClone({ "go.mod": GO_MOD, "main.go": "package main\n" });
    const d = await detectToolchain(root);
    assert.deepEqual(d.required_tools, ["go"]);
    assert.deepEqual(
      d.required_capabilities,
      [],
      "a plain Go repo asserts no non-provisionable capability",
    );
  });

  it("package.json ⇒ node", async () => {
    const root = mkClone({ "package.json": PKG });
    const d = await detectToolchain(root);
    assert.deepEqual(d.required_tools, ["node"]);
    assert.deepEqual(d.required_capabilities, []);
  });

  it("pyproject.toml OR requirements.txt ⇒ python", async () => {
    for (const marker of ["pyproject.toml", "requirements.txt"]) {
      const root = mkClone({ [marker]: "\n" });
      const d = await detectToolchain(root);
      assert.deepEqual(d.required_tools, ["python"], `${marker} ⇒ python`);
    }
  });

  it("Cargo.toml ⇒ rust", async () => {
    const root = mkClone({ "Cargo.toml": "[package]\nname = \"x\"\n" });
    const d = await detectToolchain(root);
    assert.deepEqual(d.required_tools, ["rust"]);
  });
});

describe("detectToolchain: empty repo and ordering", () => {
  it("(f) an empty repo ⇒ all empty", async () => {
    const root = mkClone({ "README.md": "# nothing here\n" });
    const d = await detectToolchain(root);
    assert.deepEqual(d.required_capabilities, []);
    assert.deepEqual(d.required_tools, []);
    assert.ok(["s", "m", "l"].includes(d.size_class));
  });

  it("outputs are de-duplicated and returned in STABLE sorted order", async () => {
    // Docker markers in several dirs, JVM in two — dedupe must collapse them; tools from
    // multiple languages must come back sorted.
    const root = mkClone({
      Dockerfile: "FROM alpine\n",
      "svc/docker-compose.yml": "services: {}\n",
      "pom.xml": "<project></project>\n",
      "sub/pom.xml": "<project></project>\n",
      "go.mod": GO_MOD,
      "package.json": PKG,
      "Cargo.toml": "[package]\n",
    });
    const d = await detectToolchain(root);
    assert.deepEqual(d.required_capabilities, ["docker", "jvm"]);
    assert.deepEqual(d.required_tools, ["go", "jvm", "node", "rust"]);
    // Idempotent + sorted: the arrays already equal their own sorted copy.
    assert.deepEqual(d.required_capabilities, [...d.required_capabilities].sort());
    assert.deepEqual(d.required_tools, [...d.required_tools].sort());
  });
});

describe("detectToolchain: symlinks are not followed", () => {
  it("(g) a symlinked directory holding a Dockerfile is NOT descended (no false docker)", async () => {
    // A real project tree that DOES contain a Dockerfile, reachable only via a symlink.
    const outside = mkClone({ "Dockerfile": "FROM alpine\n" });
    const root = mkClone({ "go.mod": GO_MOD });
    symlinkSync(outside, join(root, "escape"));
    symlinkSync("..", join(root, "up"));
    symlinkSync(".", join(root, "loop"));

    const d = await detectToolchain(root);
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "a symlinked dir is treated as a file and never descended, so its Dockerfile is unseen",
    );
    assert.deepEqual(d.required_tools, ["go"]);
  });
});

describe("detectToolchain: manifest reads are symlink-guarded and size-bounded (fix #2)", () => {
  it("a SYMLINKED manifest is NOT followed for its content (the name still counts)", async () => {
    // A real manifest OUTSIDE the clone that names testcontainers, reachable only via a
    // symlink named package.json inside the clone. The name package.json still yields the
    // node tool (presence-based), but its content must NOT be read (lstat skips the
    // symlink), so the testcontainers → docker inference does NOT fire.
    const outside = mkClone({
      "real.json": '{"name":"x","devDependencies":{"testcontainers":"^10.0.0"}}',
    });
    const root = mkClone({ "go.mod": GO_MOD });
    symlinkSync(join(outside, "real.json"), join(root, "package.json"));

    const d = await detectToolchain(root);
    assert.ok(d.required_tools.includes("node"), "the manifest NAME still yields node");
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "a symlinked manifest's content is never read, so its testcontainers is unseen",
    );
  });

  it("a manifest symlinked to /dev/zero does NOT hang or OOM (skipped, returns fast)", async () => {
    // /dev/zero is infinite; a bare readFile would never terminate. The lstat guard skips
    // it because it is a symlink (and its target is a device, not a regular file). The
    // test completing at all is the assertion; go is still inferred by name.
    const root = mkClone({ "go.mod": GO_MOD });
    try {
      symlinkSync("/dev/zero", join(root, "package.json"));
    } catch {
      return; // no /dev/zero on this platform — nothing to prove here
    }
    const d = await detectToolchain(root);
    assert.ok(d.required_tools.includes("go"));
    assert.ok(d.required_tools.includes("node"), "the symlinked name still counts as node");
  });

  it("a manifest larger than MAX_READ_BYTES is read only up to the cap (bounded)", async () => {
    // The testcontainers token sits AFTER the read cap, so a bounded read cannot see it and
    // docker is not asserted — proving the read is capped rather than reading the whole
    // (here ~600 KiB, but the same guard bounds a multi-GB file) manifest.
    const filler = "// pad\n".repeat(Math.ceil((MAX_READ_BYTES + 4096) / 7));
    const root = mkClone({
      "go.mod": GO_MOD,
      "package.json": filler + '\n"testcontainers":"^10.0.0"\n',
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_tools.includes("node"));
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "testcontainers past the read cap must be unseen — the read is bounded",
    );
  });

  it("NON-VACUITY: the SAME token WITHIN the cap IS seen (proves the bound, not a broken read)", async () => {
    // Control for the test above: testcontainers near the top is read and asserts docker,
    // so the negative result above is the cap working, not the read being broken.
    const root = mkClone({
      "package.json": '{"name":"x","devDependencies":{"testcontainers":"^10.0.0"}}',
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_capabilities.includes("docker"));
  });
});

describe("detectToolchain: the walk is depth-bounded", () => {
  it(`a marker deeper than MAX_SCAN_DEPTH (${MAX_SCAN_DEPTH}) is not seen`, async () => {
    // MAX_SCAN_DEPTH dirs deep is at the limit; one deeper is out of scope.
    const atLimit = Array.from({ length: MAX_SCAN_DEPTH }, (_, i) => `d${i}`).join("/");
    const tooDeep = atLimit + "/deeper";
    const root = mkClone({
      [`${atLimit}/go.mod`]: GO_MOD,
      [`${tooDeep}/Dockerfile`]: "FROM alpine\n",
    });
    const d = await detectToolchain(root);
    assert.ok(d.required_tools.includes("go"), "the marker at the depth limit is seen");
    assert.equal(
      d.required_capabilities.includes("docker"),
      false,
      "a Dockerfile below the depth bound is out of scope",
    );
  });
});
