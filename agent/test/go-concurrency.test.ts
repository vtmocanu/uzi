import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, it } from "node:test";
import { loadConfig } from "../src/config.js";
import { configureGoConcurrency, goConcurrencyEnv, parseGoConcurrency } from "../src/go-concurrency.js";
import { buildCheckEnv } from "../src/sdk-env.js";

afterEach(() => { configureGoConcurrency({}); });

const VARS = [
  ["WORKER_GO_BUILD_PARALLELISM", "buildParallelism"],
  ["WORKER_GO_MAXPROCS", "maxProcs"],
] as const;

describe("parseGoConcurrency", () => {
  it("accepts valid values per variable, trimming and canonicalizing", () => {
    const valid: Array<[string, number]> = [
      ["1", 1], ["2", 2], [" 4 ", 4], ["007", 7], ["2147483647", 2147483647],
    ];
    for (const [name, field] of VARS) {
      for (const [raw, n] of valid) {
        assert.deepEqual(parseGoConcurrency({ [name]: raw }), { [field]: n }, `${name}=${JSON.stringify(raw)}`);
      }
    }
  });

  it("accepts both variables together", () => {
    assert.deepEqual(
      parseGoConcurrency({ WORKER_GO_BUILD_PARALLELISM: "2", WORKER_GO_MAXPROCS: " 3 " }),
      { buildParallelism: 2, maxProcs: 3 },
    );
  });

  it("treats unset and blank as no keys at all", () => {
    for (const raw of [undefined, "", "   ", "\t"]) {
      const out = parseGoConcurrency({ WORKER_GO_BUILD_PARALLELISM: raw, WORKER_GO_MAXPROCS: raw });
      assert.deepEqual(out, {});
      assert.equal(Object.hasOwn(out, "buildParallelism"), false);
      assert.equal(Object.hasOwn(out, "maxProcs"), false);
    }
    assert.deepEqual(parseGoConcurrency({}), {});
  });

  it("rejects invalid values naming the variable", () => {
    const invalid = ["0", "-1", "+1", "1.5", "2147483648", "abc", "2 -toolexec=x", "1e3"];
    for (const [name] of VARS) {
      for (const raw of invalid) {
        assert.throws(
          () => parseGoConcurrency({ [name]: raw }),
          new RegExp(`^Error: ${name} must be a positive integer <= 2147483647$`),
          `${name}=${JSON.stringify(raw)}`,
        );
      }
    }
  });
});

describe("goConcurrencyEnv", () => {
  it("returns only the keys that are set", () => {
    assert.deepEqual(goConcurrencyEnv({}), {});
    assert.deepEqual(goConcurrencyEnv({ buildParallelism: 2 }), { GOFLAGS: "-p=2" });
    assert.deepEqual(goConcurrencyEnv({ maxProcs: 3 }), { GOMAXPROCS: "3" });
    assert.deepEqual(goConcurrencyEnv({ buildParallelism: 2, maxProcs: 3 }), { GOFLAGS: "-p=2", GOMAXPROCS: "3" });
  });

  it("composes with a trusted GOFLAGS base", () => {
    assert.deepEqual(goConcurrencyEnv({}, "-buildvcs=false"), { GOFLAGS: "-buildvcs=false" });
    assert.deepEqual(goConcurrencyEnv({ buildParallelism: 2 }, "-buildvcs=false"), { GOFLAGS: "-buildvcs=false -p=2" });
    assert.deepEqual(goConcurrencyEnv({ maxProcs: 3 }, "-buildvcs=false"), {
      GOFLAGS: "-buildvcs=false", GOMAXPROCS: "3",
    });
  });

  it("defaults to the configured settings and resets with {}", () => {
    assert.deepEqual(goConcurrencyEnv(), {});
    configureGoConcurrency({ buildParallelism: 5, maxProcs: 6 });
    assert.deepEqual(goConcurrencyEnv(), { GOFLAGS: "-p=5", GOMAXPROCS: "6" });
    configureGoConcurrency({});
    assert.deepEqual(goConcurrencyEnv(), {});
  });

  it("does not alias the caller's settings object", () => {
    const settings = { buildParallelism: 2 };
    configureGoConcurrency(settings);
    settings.buildParallelism = 9;
    assert.deepEqual(goConcurrencyEnv(), { GOFLAGS: "-p=2" });
  });
});

describe("loadConfig goConcurrency", () => {
  const base = { UZI_API_URL: "https://example.com", UZI_WORKER_TOKEN: "fixture" };

  it("defaults to {} and accepts valid values", () => {
    assert.deepEqual(loadConfig(base).goConcurrency, {});
    assert.deepEqual(
      loadConfig({ ...base, WORKER_GO_BUILD_PARALLELISM: "2", WORKER_GO_MAXPROCS: "3" }).goConcurrency,
      { buildParallelism: 2, maxProcs: 3 },
    );
    assert.deepEqual(loadConfig({ ...base, WORKER_GO_MAXPROCS: " " }).goConcurrency, {});
  });

  it("fails startup on an invalid value, naming the variable", () => {
    for (const [name] of VARS) {
      for (const raw of ["0", "-1", "abc", "2147483648", "2 -toolexec=x"]) {
        assert.throws(() => loadConfig({ ...base, [name]: raw }), new RegExp(name));
      }
    }
  });
});

describe("real go toolchain", () => {
  it("accepts the check-env GOFLAGS and honours GOMAXPROCS", { timeout: 90_000 }, async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-go-conc-"));
    try {
      const src = path.join(dir, "src");
      const home = path.join(dir, "home");
      fs.mkdirSync(src);
      fs.mkdirSync(home);
      fs.writeFileSync(path.join(src, "go.mod"), "module example.com/probe\n\ngo 1.20\n");
      fs.writeFileSync(path.join(src, "main.go"), [
        "package main",
        "",
        'import ("fmt"; "os"; "runtime")',
        "",
        "func main() {",
        '\tfmt.Printf("%d|%s\\n", runtime.GOMAXPROCS(0), os.Getenv("GOFLAGS"))',
        "}",
        "",
      ].join("\n"));
      configureGoConcurrency({ buildParallelism: 2, maxProcs: 3 });
      const env = {
        ...buildCheckEnv(process.env, home),
        GOCACHE: path.join(dir, "gocache"),
        GOPATH: path.join(dir, "gopath"),
        GOMODCACHE: path.join(dir, "gomod"),
        GOTOOLCHAIN: "local",
      };
      const out = await new Promise<string>((resolve, reject) => {
        execFile("go", ["run", "."], { cwd: src, env, timeout: 75_000, maxBuffer: 1 << 20 },
          (err, stdout, stderr) => err ? reject(new Error(`go run failed: ${err.message}\n${stderr}`)) : resolve(stdout));
      });
      assert.equal(out.trim(), "3|-p=2");
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
