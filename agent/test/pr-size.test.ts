import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import {
  classifyPath,
  computeSize,
  computeSizeLine,
  isSourceUnsupported,
  lookupAttributes,
  parseNumstatZ,
  renderSizeLine,
  sizeTotals,
  SIZE_UNAVAILABLE,
  type AttrGitRunner,
  type NumstatEntry,
  type PathAttributes,
  type SizeBucket,
  type SizeLineGit,
} from "../src/pr-size.js";
import { StubExecutor } from "../src/executor.js";
import { mrDescription } from "../src/runner.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, fakeGitlab, git as harnessGit, gitlabClaim, installHarness, runner } from "./runner-harness.js";

// PRD #1798 M1 (D3) — the deterministic size line. The classifier is table-tested per ecosystem path
// rule and per attribute state; the attribute states come from REAL `git check-attr` runs against a
// real bare clone (nested .gitattributes, later-line overrides and `!attr` follow git's own rules, so
// only git can say what they resolve to). The lookup order (--source, then the isolated temp index
// ONLY for an unsupported --source, else unavailable) is driven through an injectable git runner.

const MINUS = "−";

// Repository-location variables a git hook (or a `task` target it starts) exports. Inherited, they
// override `cwd` / `-C`: `git init`/`commit` in makeRepo would hit the CALLER's repository, and an
// inherited GIT_INDEX_FILE would redirect the attribute lookup. Both helpers below drop them.
const GIT_LOCATION_VARS = [
  "GIT_DIR",
  "GIT_WORK_TREE",
  "GIT_INDEX_FILE",
  "GIT_OBJECT_DIRECTORY",
  "GIT_ALTERNATE_OBJECT_DIRECTORIES",
  "GIT_COMMON_DIR",
  "GIT_NAMESPACE",
  "GIT_CEILING_DIRECTORIES",
  "GIT_PREFIX",
];

function scrubbedGitEnv(extra: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env };
  for (const k of GIT_LOCATION_VARS) delete env[k];
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_NOSYSTEM: "1", ...extra };
}

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", args, {
    cwd,
    encoding: "utf8",
    env: scrubbedGitEnv({
      GIT_AUTHOR_NAME: "t",
      GIT_AUTHOR_EMAIL: "t@t",
      GIT_COMMITTER_NAME: "t",
      GIT_COMMITTER_EMAIL: "t@t",
    }),
  });
}

interface Fixture {
  root: string;
  bare: string;
  base: string;
  head: string;
  cleanup(): void;
}

/** A work repo with `base` files committed on main, then `change` applied on a branch commit, cloned
 *  --bare (like the worker's bare). `change` maps path → content, or null to delete. */
function makeRepo(base: Record<string, string | Buffer>, change: Record<string, string | Buffer | null>, renames: [string, string][] = []): Fixture {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prsize-"));
  const work = path.join(root, "work");
  fs.mkdirSync(work);
  git(work, "init", "-q", "-b", "main");
  const write = (files: Record<string, string | Buffer | null>): void => {
    for (const [p, content] of Object.entries(files)) {
      const abs = path.join(work, p);
      if (content === null) {
        fs.rmSync(abs);
        continue;
      }
      fs.mkdirSync(path.dirname(abs), { recursive: true });
      fs.writeFileSync(abs, content);
    }
  };
  write(base);
  git(work, "add", "-A");
  git(work, "commit", "-qm", "base", "--allow-empty");
  const baseSha = git(work, "rev-parse", "HEAD").trim();
  git(work, "checkout", "-qb", "agent/issue-1");
  for (const [from, to] of renames) {
    fs.mkdirSync(path.dirname(path.join(work, to)), { recursive: true });
    git(work, "mv", from, to);
  }
  write(change);
  git(work, "add", "-A");
  git(work, "commit", "-qm", "change", "--allow-empty");
  const head = git(work, "rev-parse", "HEAD").trim();
  const bare = path.join(root, "bare.git");
  git(root, "clone", "-q", "--bare", work, bare);
  return { root, bare, base: baseSha, head, cleanup: () => fs.rmSync(root, { recursive: true, force: true }) };
}

function newGitCache(root: string): GitCache {
  return new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
}

/** An AttrGitRunner over the real `git -C <bare>`, rejecting with git's stderr like GitCache does. */
function realRunner(bare: string): AttrGitRunner {
  return async (args, opts) => {
    const env = scrubbedGitEnv();
    if (opts.indexFile) env.GIT_INDEX_FILE = opts.indexFile;
    const r = spawnSync("git", ["-C", bare, ...args], { input: opts.input ?? "", encoding: "utf8", env });
    if (r.status !== 0) throw Object.assign(new Error(`git ${args[0]} exited ${r.status}`), { stderr: r.stderr });
    return r.stdout;
  };
}

/** Wraps a runner so any `--source` invocation fails exactly as a git < 2.40 does (exit 129). */
function withoutSource(inner: AttrGitRunner, calls: { args: string[]; indexFile?: string }[]): AttrGitRunner {
  return async (args, opts) => {
    calls.push({ args, ...(opts.indexFile ? { indexFile: opts.indexFile } : {}) });
    const source = args.find((a) => a.startsWith("--source"));
    if (source) {
      throw Object.assign(new Error("git check-attr exited 129"), {
        stderr:
          `error: unknown option \`${source.slice(2)}'\n` +
          "usage: git check-attr [-a | --all | <attr>...] [--] <pathname>...\n" +
          "   or: git check-attr --stdin [-z] [-a | --all | <attr>...]\n",
      });
    }
    return inner(args, opts);
  };
}

describe("classifyPath — generic path rules per ecosystem (PRD #1798 D3)", () => {
  const rows: [string, SizeBucket][] = [
    // tests
    ["api/internal/store/runs_test.go", "tests"],
    ["web/src/lib/format.test.ts", "tests"],
    ["web/src/App.spec.tsx", "tests"],
    ["pkg/test_parser.py", "tests"],
    ["pkg/parser_test.py", "tests"],
    ["src/main/java/com/x/ParserTest.java", "tests"],
    ["lib/parser_spec.rb", "tests"],
    ["tests/fixtures/data.json", "tests"],
    ["test/helper.go", "tests"],
    ["src/__tests__/thing.js", "tests"],
    ["spec/models/user.rb", "tests"],
    ["docs/test/guide.md", "tests"],
    // docs
    ["README.md", "docs"],
    ["site/page.mdx", "docs"],
    ["python/index.rst", "docs"],
    ["manual/intro.adoc", "docs"],
    ["docs/diagram.svg", "docs"],
    ["doc/notes.txt", "docs"],
    // generated
    ["web/package-lock.json", "generated"],
    ["yarn.lock", "generated"],
    ["pnpm-lock.yaml", "generated"],
    ["api/go.sum", "generated"],
    ["Cargo.lock", "generated"],
    ["poetry.lock", "generated"],
    ["uv.lock", "generated"],
    ["Gemfile.lock", "generated"],
    ["proto/run.pb.go", "generated"],
    ["client/api_generated.ts", "generated"],
    ["schema.gen.go", "generated"],
    ["static/app.min.js", "generated"],
    ["__snapshots__/view.snap", "generated"],
    // config
    [".golangci.yml", "config"],
    ["deploy/values.yaml", "config"],
    ["pyproject.toml", "config"],
    ["web/package.json", "config"],
    ["setup.ini", "config"],
    ["Dockerfile", "config"],
    ["agent/Dockerfile.dev", "config"],
    ["Makefile", "config"],
    ["Taskfile.yml", "config"],
    [".github/CODEOWNERS", "config"],
    [".forgejo/issue_template", "config"],
    [".gitlab-ci.yml", "config"],
    // code
    ["agent/src/runner.ts", "code"],
    ["api/cmd/main.go", "code"],
    ["lib/thing.rb", "code"],
    ["scripts/run.sh", "code"],
    ["latest/readme.txt", "code"],
    ["contest/x.go", "code"],
    ["img/logo.png", "code"],
  ];
  for (const [p, want] of rows) {
    it(`${p} → ${want}`, () => assert.strictEqual(classifyPath(p), want));
  }

  it("first match wins: a test-named markdown file is tests, a JSON under docs/ is docs", () => {
    assert.strictEqual(classifyPath("tests/README.md"), "tests");
    assert.strictEqual(classifyPath("docs/openapi.json"), "docs");
    assert.strictEqual(classifyPath("docs/package-lock.json"), "docs");
  });
});

describe("classifyPath — attribute states (PRD #1798 D3)", () => {
  const rows: [string, PathAttributes, SizeBucket][] = [
    ["set → in the bucket", { "linguist-generated": "set" }, "generated"],
    ["value true → in the bucket", { "linguist-vendored": "true" }, "vendored"],
    ["documentation set on a code path", { "linguist-documentation": "set" }, "docs"],
    ["precedence generated > vendored > documentation", { "linguist-generated": "set", "linguist-vendored": "set", "linguist-documentation": "set" }, "generated"],
    ["precedence vendored > documentation", { "linguist-vendored": "true", "linguist-documentation": "set" }, "vendored"],
    ["unspecified falls through to the path rules", { "linguist-generated": "unspecified" }, "code"],
  ];
  for (const [name, attrs, want] of rows) {
    it(`${name} (src/x.ts)`, () => assert.strictEqual(classifyPath("src/x.ts", attrs), want));
  }
  it("an attribute beats the tests path rule", () => {
    assert.strictEqual(classifyPath("vendor/x_test.go", { "linguist-vendored": "set" }), "vendored");
  });
  it("unset / false documentation skips the docs rule", () => {
    assert.strictEqual(classifyPath("notes.md", { "linguist-documentation": "unset" }), "code");
    assert.strictEqual(classifyPath("docs/x.go", { "linguist-documentation": "false" }), "code");
  });
  it("unset / false generated skips the generated rule (a lockfile then falls to config)", () => {
    assert.strictEqual(classifyPath("package-lock.json", { "linguist-generated": "unset" }), "config");
    assert.strictEqual(classifyPath("Cargo.lock", { "linguist-generated": "false" }), "code");
  });
  it("an explicit 'not vendored' changes nothing (vendored has no path rule)", () => {
    assert.strictEqual(classifyPath("README.md", { "linguist-vendored": "false" }), "docs");
  });
});

describe("attribute lookup on a real bare clone (PRD #1798 M1)", () => {
  // Root and nested .gitattributes exercising every state D3 names.
  const base: Record<string, string> = {
    ".gitattributes": [
      "gen/** linguist-generated",
      "truegen.txt linguist-generated=true",
      "vendor/** linguist-vendored",
      "guide.txt linguist-documentation",
      "unset.md -linguist-documentation",
      "false.md linguist-documentation=false",
      "lock/package-lock.json -linguist-generated",
      // a later line overrides an earlier one for the same path
      "override.txt linguist-generated",
      "override.txt -linguist-generated",
      // `!attr` resets to unspecified, so the path rule applies
      "bang.md linguist-documentation",
      "bang.md !linguist-documentation",
      "both.txt linguist-generated linguist-vendored linguist-documentation",
      "vd.txt linguist-vendored linguist-documentation",
      "",
    ].join("\n"),
    "sub/.gitattributes": ["x.md -linguist-documentation", "*.txt linguist-documentation", ""].join("\n"),
  };
  const table: [string, SizeBucket, Partial<Record<keyof PathAttributes, string>>][] = [
    ["gen/a.go", "generated", { "linguist-generated": "set" }],
    ["truegen.txt", "generated", { "linguist-generated": "true" }],
    ["vendor/lib.js", "vendored", { "linguist-vendored": "set" }],
    ["guide.txt", "docs", { "linguist-documentation": "set" }],
    ["unset.md", "code", { "linguist-documentation": "unset" }],
    ["false.md", "code", { "linguist-documentation": "false" }],
    ["lock/package-lock.json", "config", { "linguist-generated": "unset" }],
    ["override.txt", "code", { "linguist-generated": "unset" }],
    ["bang.md", "docs", { "linguist-documentation": "unspecified" }],
    ["both.txt", "generated", {}],
    ["vd.txt", "vendored", {}],
    ["sub/x.md", "code", { "linguist-documentation": "unset" }],
    ["sub/notes.txt", "docs", { "linguist-documentation": "set" }],
    ["plain.md", "docs", { "linguist-documentation": "unspecified", "linguist-generated": "unspecified" }],
    ["src/app.ts", "code", { "linguist-generated": "unspecified", "linguist-vendored": "unspecified" }],
  ];
  const change = Object.fromEntries(table.map(([p]) => [p, "line\n"]));

  it("GitCache.checkAttrZ (--source) resolves every state as git does, and classifyPath buckets it", async () => {
    const fx = makeRepo(base, change);
    try {
      const attrs = await newGitCache(fx.root).checkAttrZ(fx.bare, fx.head, table.map(([p]) => p));
      for (const [p, bucket, states] of table) {
        const got = attrs.get(p);
        assert.ok(got, `no attributes reported for ${p}`);
        for (const [k, v] of Object.entries(states)) assert.strictEqual(got[k as keyof PathAttributes], v, `${p} ${k}`);
        assert.strictEqual(classifyPath(p, got), bucket, p);
      }
    } finally {
      fx.cleanup();
    }
  });

  it("reads the attributes at the HEAD commit, not the bare's (absent) index or another commit", async () => {
    // The .gitattributes only exists on the branch commit: at base the path is unspecified.
    const fx = makeRepo({ "a.txt": "x\n" }, { ".gitattributes": "a.txt linguist-generated\n", "a.txt": "y\n" });
    try {
      const gc = newGitCache(fx.root);
      assert.strictEqual((await gc.checkAttrZ(fx.bare, fx.head, ["a.txt"])).get("a.txt")?.["linguist-generated"], "set");
      assert.strictEqual((await gc.checkAttrZ(fx.bare, fx.base, ["a.txt"])).get("a.txt")?.["linguist-generated"], "unspecified");
    } finally {
      fx.cleanup();
    }
  });

  it("lookup paths: --source first; temp index ONLY for unsupported --source, identical buckets, temp index removed", async () => {
    const fx = makeRepo(base, change);
    try {
      const paths = table.map(([p]) => p);
      const srcCalls: string[][] = [];
      const real = realRunner(fx.bare);
      const viaSource = await lookupAttributes(async (args, opts) => {
        srcCalls.push(args);
        return real(args, opts);
      }, fx.head, paths);
      assert.strictEqual(srcCalls.length, 1, "the --source path is one check-attr call");
      assert.ok(srcCalls[0]!.includes(`--source=${fx.head}`));
      assert.ok(srcCalls[0]!.includes("--stdin") && srcCalls[0]!.includes("-z"));

      const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prsize-tmp-"));
      try {
        const calls: { args: string[]; indexFile?: string }[] = [];
        const viaIndex = await lookupAttributes(withoutSource(real, calls), fx.head, paths, tmpRoot);
        assert.deepStrictEqual(calls.map((c) => c.args[0] + (c.args.includes("--cached") ? " --cached" : "")), [
          "check-attr",
          "read-tree",
          "check-attr --cached",
        ]);
        const indexFile = calls[1]!.indexFile!;
        assert.ok(indexFile.startsWith(tmpRoot), "the temp index lives under the worker temp dir");
        assert.strictEqual(calls[2]!.indexFile, indexFile, "check-attr --cached reads the same temp index");
        assert.ok(!fs.existsSync(path.dirname(indexFile)), "the temp index dir is removed afterwards");
        assert.deepStrictEqual(fs.readdirSync(tmpRoot), []);
        assert.ok(!fs.existsSync(path.join(fx.bare, "index")), "the bare repo's own index is never written");
        // Identical attribute states, hence identical buckets.
        assert.deepStrictEqual(viaIndex, viaSource);
        for (const p of paths) assert.strictEqual(classifyPath(p, viaIndex.get(p)), classifyPath(p, viaSource.get(p)));
      } finally {
        fs.rmSync(tmpRoot, { recursive: true, force: true });
      }
    } finally {
      fx.cleanup();
    }
  });

  it("GitCache's real runner takes the temp-index fallback: identical buckets, bare index never written", async () => {
    // Drives the PRODUCTION runner (GitCache.sizeAttrGit → execScoped → real git). Only the
    // `--source` invocation is made to fail the way a git < 2.40 does; read-tree and
    // check-attr --cached then run for real against the bare, so the GIT_INDEX_FILE plumbing of
    // sizeAttrGit is what keeps git off the bare repo's own index.
    const fx = makeRepo(base, change);
    try {
      const paths = table.map(([p]) => p);
      const viaSource = await newGitCache(fx.root).checkAttrZ(fx.bare, fx.head, paths);

      const gc = newGitCache(fx.root);
      type ExecScoped = (command: string, args: string[], options: { env: NodeJS.ProcessEnv; input?: string }) => Promise<{ stdout: string; stderr: string }>;
      const seam = gc as unknown as { execScoped: ExecScoped };
      const realExec = seam.execScoped.bind(gc);
      const seen: { args: string[]; indexFile: string | undefined }[] = [];
      seam.execScoped = async (command, args, options) => {
        seen.push({ args, indexFile: options.env.GIT_INDEX_FILE });
        const source = args.find((a) => a.startsWith("--source="));
        if (source) {
          throw Object.assign(new Error("Command failed: git check-attr (exit 129)"), {
            code: 129,
            stderr:
              `error: unknown option \`${source.slice(2)}'\n` +
              "usage: git check-attr [-a | --all | <attr>...] [--] <pathname>...\n" +
              "   or: git check-attr --stdin [-z] [-a | --all | <attr>...]\n",
          });
        }
        return realExec(command, args, options);
      };
      const viaIndex = await gc.checkAttrZ(fx.bare, fx.head, paths);

      const shape = seen.map((c) => (c.args.includes("read-tree") ? "read-tree" : c.args.includes("--cached") ? "check-attr --cached" : "check-attr"));
      assert.deepStrictEqual(shape, ["check-attr", "read-tree", "check-attr --cached"], "the fallback ran through GitCache's runner");
      assert.ok(!fs.existsSync(path.join(fx.bare, "index")), "the bare repo's own index is never written");
      assert.deepStrictEqual(viaIndex, viaSource, "identical attribute states on both paths");
      for (const [p, bucket] of table) {
        assert.strictEqual(classifyPath(p, viaIndex.get(p)), bucket, p);
        assert.strictEqual(classifyPath(p, viaIndex.get(p)), classifyPath(p, viaSource.get(p)), p);
      }
    } finally {
      fx.cleanup();
    }
  });

  it("a failing temp-index step throws (unavailable) and still removes the temp index", async () => {
    const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-prsize-tmp-"));
    try {
      const calls: { args: string[]; indexFile?: string }[] = [];
      const failing: AttrGitRunner = async (args) => {
        throw Object.assign(new Error(`git ${args[0]} failed`), { stderr: "fatal: not a tree object\n" });
      };
      await assert.rejects(lookupAttributes(withoutSource(failing, calls), "a".repeat(40), ["x"], tmpRoot));
      assert.strictEqual(calls.length, 2, "check-attr --source, then read-tree");
      assert.deepStrictEqual(fs.readdirSync(tmpRoot), []);
    } finally {
      fs.rmSync(tmpRoot, { recursive: true, force: true });
    }
  });

  it("any other check-attr failure does NOT take the temp-index path", async () => {
    const calls: string[][] = [];
    const runner: AttrGitRunner = async (args) => {
      calls.push(args);
      throw Object.assign(new Error("git check-attr exited 128"), { stderr: `fatal: bad revision '${"b".repeat(40)}'\n` });
    };
    await assert.rejects(lookupAttributes(runner, "b".repeat(40), ["x"]));
    assert.strictEqual(calls.length, 1);
  });

  it("refuses a non-SHA head (git refuses a branch name for --source)", async () => {
    await assert.rejects(lookupAttributes(async () => "", "main", ["x"]), /commit SHA/);
  });

  it("isSourceUnsupported recognises only the unsupported-option shapes", () => {
    assert.ok(isSourceUnsupported({ stderr: "error: unknown option `source=abc'\nusage: git check-attr ..." }));
    assert.ok(isSourceUnsupported({ stderr: "usage: git check-attr [-a | --all | <attr>...] [--] <pathname>...\n" }));
    assert.ok(!isSourceUnsupported({ stderr: "usage: git check-attr --stdin [-z] [--source <tree-ish>]\n" }));
    assert.ok(!isSourceUnsupported({ stderr: "fatal: bad object deadbeef\n" }));
    assert.ok(!isSourceUnsupported({ stderr: "error: unknown option `bogus'\nusage: git check-attr [--source <tree-ish>]\n" }));
  });
});

describe("numstat parsing (PRD #1798 D3)", () => {
  it("parses plain, rename (by NEW path), and binary records", () => {
    const out = "3\t1\tsrc/a.ts\0" + "0\t0\t\0test/old.go\0src/new.go\0" + "-\t-\timg.png\0" + "0\t12\tdocs/gone.md\0";
    assert.deepStrictEqual(parseNumstatZ(out), [
      { path: "src/a.ts", added: 3, deleted: 1, binary: false },
      { path: "src/new.go", added: 0, deleted: 0, binary: false },
      { path: "img.png", added: 0, deleted: 0, binary: true },
      { path: "docs/gone.md", added: 0, deleted: 12, binary: false },
    ]);
  });
  it("keeps tabs and spaces inside paths verbatim", () => {
    assert.deepStrictEqual(parseNumstatZ("1\t0\ta b\tc.ts\0"), [{ path: "a b\tc.ts", added: 1, deleted: 0, binary: false }]);
  });
  it("an empty diff parses to no entries", () => assert.deepStrictEqual(parseNumstatZ(""), []));
  it("malformed output throws (so the line renders unavailable)", () => {
    assert.throws(() => parseNumstatZ("x\ty\tz\0"));
    assert.throws(() => parseNumstatZ("1\t1\t\0only-old\0"));
    assert.throws(() => parseNumstatZ("garbage\0"));
  });
});

describe("renderSizeLine (PRD #1798 D3)", () => {
  const e = (p: string, added: number, deleted: number, binary = false): NumstatEntry => ({ path: p, added, deleted, binary });

  it("renders buckets in order, U+2212 minus, ' · ' separators, en-US grouping, omitting empty buckets", () => {
    const line = renderSizeLine(
      [e("src/a.ts", 1500, 20), e("src/b.ts", 310, 3), e("a_test.go", 40, 2), e("go.sum", 7, 7), e("README.md", 5, 0)],
      new Map(),
    );
    assert.strictEqual(line, `**Size:** code +1,810 ${MINUS}23 · tests +40 ${MINUS}2 · docs +5 ${MINUS}0 · generated +7 ${MINUS}7 · 5 files`);
    assert.ok(!line!.includes("-2"), "ASCII hyphen-minus is never used for deletions");
    assert.ok(!line!.includes("config"), "a bucket with no files is omitted");
    assert.ok(!line!.includes("vendored"));
  });
  it("uses the attributes map for classification", () => {
    const attrs = new Map<string, PathAttributes>([["src/a.ts", { "linguist-vendored": "set" }]]);
    assert.strictEqual(renderSizeLine([e("src/a.ts", 2, 1)], attrs), `**Size:** vendored +2 ${MINUS}1 · 1 file`);
  });
  it("a binary-only bucket still counts its file", () => {
    assert.strictEqual(renderSizeLine([e("logo.png", 0, 0, true)], new Map()), `**Size:** code +0 ${MINUS}0 · 1 file`);
  });
  it("all six buckets and a thousands-grouped file count", () => {
    const entries: NumstatEntry[] = [e("a.ts", 1, 0), e("a.test.ts", 1, 0), e("a.md", 1, 0), e("a.yml", 1, 0), e("a.snap", 1, 0)];
    for (let i = 0; i < 995; i++) entries.push(e(`v/${i}.c`, 0, 1));
    const attrs = new Map<string, PathAttributes>(entries.filter((x) => x.path.startsWith("v/")).map((x) => [x.path, { "linguist-vendored": "set" }]));
    assert.strictEqual(
      renderSizeLine(entries, attrs),
      `**Size:** code +1 ${MINUS}0 · tests +1 ${MINUS}0 · docs +1 ${MINUS}0 · config +1 ${MINUS}0 · generated +1 ${MINUS}0 · vendored +0 ${MINUS}995 · 1,000 files`,
    );
  });
  it("an empty diff renders no line", () => assert.strictEqual(renderSizeLine([], new Map()), null));
});

// PRD #1798 D9: the structured size must carry exactly the numbers the rendered line prints. Parse
// the line back (grouping and U+2212 undone) and compare bucket by bucket; a bucket the line omits
// must be zero in the structured size.
const BUCKET_NAMES: readonly SizeBucket[] = ["code", "tests", "docs", "config", "generated", "vendored"];
function assertSizeMatchesLine(size: ReturnType<typeof sizeTotals>, line: string | null): void {
  assert.strictEqual(size.unavailable, false);
  if (line === null) {
    assert.strictEqual(size.files, 0);
    for (const b of BUCKET_NAMES) assert.deepStrictEqual(size[b], { added: 0, deleted: 0 }, b);
    return;
  }
  const num = (raw: string) => Number(raw.replaceAll(",", ""));
  const parts = line.replace(/^\*\*Size:\*\* /, "").split(" · ");
  const filesPart = parts.pop()!;
  const fm = /^([\d,]+) files?$/.exec(filesPart);
  assert.ok(fm, filesPart);
  assert.strictEqual(size.files, num(fm[1]!), "files");
  const seen = new Map<string, { added: number; deleted: number }>();
  for (const p of parts) {
    const m = new RegExp(`^([a-z]+) \\+([\\d,]+) ${MINUS}([\\d,]+)$`).exec(p);
    assert.ok(m, p);
    seen.set(m[1]!, { added: num(m[2]!), deleted: num(m[3]!) });
  }
  for (const b of BUCKET_NAMES) assert.deepStrictEqual(size[b], seen.get(b) ?? { added: 0, deleted: 0 }, b);
}

describe("sizeTotals: the structured size beside the rendered line (PRD #1798 D9)", () => {
  const e = (p: string, added: number, deleted: number, binary = false): NumstatEntry => ({ path: p, added, deleted, binary });
  const vendoredA = new Map<string, PathAttributes>([["src/a.ts", { "linguist-vendored": "set" }]]);
  const big: NumstatEntry[] = [e("a.ts", 1, 0), e("a.test.ts", 1, 0), e("a.md", 1, 0), e("a.yml", 1, 0), e("a.snap", 1, 0)];
  for (let i = 0; i < 995; i++) big.push(e(`v/${i}.c`, 0, 1));
  const bigAttrs = new Map<string, PathAttributes>(big.filter((x) => x.path.startsWith("v/")).map((x) => [x.path, { "linguist-vendored": "set" }]));
  const cases: [string, NumstatEntry[], Map<string, PathAttributes>][] = [
    ["mixed buckets", [e("src/a.ts", 1500, 20), e("src/b.ts", 310, 3), e("a_test.go", 40, 2), e("go.sum", 7, 7), e("README.md", 5, 0)], new Map()],
    ["attribute-classified", [e("src/a.ts", 2, 1)], vendoredA],
    ["binary only", [e("logo.png", 0, 0, true)], new Map()],
    ["all six buckets, 1,000 files", big, bigAttrs],
    ["empty diff", [], new Map()],
  ];
  for (const [name, entries, attrs] of cases) {
    it(`${name}: equals the rendered line's numbers`, () => {
      assertSizeMatchesLine(sizeTotals(entries, attrs), renderSizeLine(entries, attrs));
    });
  }
  it("pins one case literally", () => {
    assert.deepStrictEqual(sizeTotals([e("src/a.ts", 3, 1), e("docs/x.md", 0, 4), e("logo.png", 0, 0, true)], new Map()), {
      unavailable: false,
      files: 3,
      code: { added: 3, deleted: 1 },
      tests: { added: 0, deleted: 0 },
      docs: { added: 0, deleted: 4 },
      config: { added: 0, deleted: 0 },
      generated: { added: 0, deleted: 0 },
      vendored: { added: 0, deleted: 0 },
    });
  });
});

describe("computeSize: line plus structured size, same failure contract (PRD #1798 D9)", () => {
  const head = "c".repeat(40);
  const ok: SizeLineGit = {
    sizeMergeBase: async () => "d".repeat(40),
    diffNumstatZ: async () => "1\t0\tsrc/a.ts\0" + "2\t5\tdocs/b.md\0" + "4\t0\tstore/q.sql.go\0",
    checkAttrZ: async (_bare, _head, paths) =>
      new Map(paths.map((p) => [p, p.endsWith(".sql.go") ? { "linguist-generated": "set" } : { "linguist-generated": "unspecified" }])),
  };
  const UNAVAILABLE = {
    unavailable: true,
    files: 0,
    code: { added: 0, deleted: 0 },
    tests: { added: 0, deleted: 0 },
    docs: { added: 0, deleted: 0 },
    config: { added: 0, deleted: 0 },
    generated: { added: 0, deleted: 0 },
    vendored: { added: 0, deleted: 0 },
  };

  it("the line is computeSizeLine's, and the size carries its numbers", async () => {
    const got = await computeSize(ok, "/bare", "main", head, nullLogger());
    assert.strictEqual(got.line, await computeSizeLine(ok, "/bare", "main", head, nullLogger()));
    assert.strictEqual(got.line, `**Size:** code +1 ${MINUS}0 · docs +2 ${MINUS}5 · generated +4 ${MINUS}0 · 3 files`);
    assertSizeMatchesLine(got.size, got.line);
  });

  it("an empty diff: no line, a zero (available) size", async () => {
    const got = await computeSize({ ...ok, diffNumstatZ: async () => "" }, "/bare", "main", head);
    assert.strictEqual(got.line, null);
    assert.deepStrictEqual(got.size, { ...UNAVAILABLE, unavailable: false });
  });

  it("every failure: the unavailable line and a zeroed unavailable size, one warn each", async () => {
    const warns: string[] = [];
    const log = { warn: (msg: string) => void warns.push(msg) };
    const failures: [SizeLineGit, string | null][] = [
      [{ ...ok, checkAttrZ: async () => { throw new Error("git check-attr failed"); } }, head],
      [{ ...ok, checkAttrZ: async () => new Map([["src/a.ts", {}]]) }, head],
      [{ ...ok, diffNumstatZ: async () => "not numstat" }, head],
      [{ ...ok, sizeMergeBase: async () => { throw new Error("no merge base"); } }, head],
      [ok, null],
    ];
    for (const [g, h] of failures) {
      const got = await computeSize(g, "/bare", "main", h, log);
      assert.strictEqual(got.line, SIZE_UNAVAILABLE);
      assert.deepStrictEqual(got.size, UNAVAILABLE);
    }
    assert.strictEqual(warns.length, failures.length);
  });
});

describe("computeSizeLine end to end on a real bare clone (PRD #1798 M1)", () => {
  const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x00, 0x00, 0x01, 0x02, 0x00, 0xff]);
  const helper = Array.from({ length: 20 }, (_, i) => `line ${i}`).join("\n") + "\n";

  it("renames across buckets by NEW path, deletions by OLD path, binaries as files; honours .gitattributes", async () => {
    const fx = makeRepo(
      {
        "test/helper.go": helper,
        "docs/gone.md": "a\nb\nc\n",
        "src/keep.ts": "one\n",
      },
      {
        ".gitattributes": "store/*.sql.go linguist-generated\n",
        "src/keep.ts": "one\ntwo\nthree\n",
        "store/runs.sql.go": "g1\ng2\n",
        "docs/gone.md": null,
        "img/logo.png": png,
      },
      [["test/helper.go", "src/helper.go"]],
    );
    try {
      const gc = newGitCache(fx.root);
      // The raw numstat carries the -z rename shape (empty path field, then old and new paths).
      const raw = await gc.diffNumstatZ(fx.bare, fx.base, fx.head);
      assert.ok(raw.includes("\0test/helper.go\0src/helper.go\0"), JSON.stringify(raw));
      assert.strictEqual(await gc.sizeMergeBase(fx.bare, "main", fx.head), fx.base);
      const line = await computeSizeLine(gc, fx.bare, "main", fx.head, nullLogger());
      // code: src/keep.ts +2, src/helper.go (renamed from test/) +0 −0, img/logo.png binary.
      // docs: the deleted docs/gone.md (old path) −3. generated: store/runs.sql.go via .gitattributes.
      // config: .gitattributes itself is code (no rule matches it).
      assert.strictEqual(line, `**Size:** code +3 ${MINUS}0 · docs +0 ${MINUS}3 · generated +2 ${MINUS}0 · 6 files`);
      const computed = await computeSize(gc, fx.bare, "main", fx.head, nullLogger());
      assert.strictEqual(computed.line, line);
      assertSizeMatchesLine(computed.size, line);
    } finally {
      fx.cleanup();
    }
  });

  it("an empty diff yields no line", async () => {
    const fx = makeRepo({ "a.ts": "x\n" }, {});
    try {
      assert.strictEqual(await computeSizeLine(newGitCache(fx.root), fx.bare, "main", fx.head), null);
    } finally {
      fx.cleanup();
    }
  });

  it("any failure renders '**Size:** unavailable' (never buckets without attributes) and logs a warn", async () => {
    const warns: string[] = [];
    const log = { warn: (msg: string) => void warns.push(msg) };
    const head = "c".repeat(40);
    const ok: SizeLineGit = {
      sizeMergeBase: async () => "d".repeat(40),
      diffNumstatZ: async () => "1\t0\tsrc/a.ts\0",
      checkAttrZ: async (_bare, _head, paths) => new Map(paths.map((p) => [p, { "linguist-generated": "unspecified" }])),
    };
    assert.strictEqual(await computeSizeLine(ok, "/bare", "main", head, log), `**Size:** code +1 ${MINUS}0 · 1 file`);
    const failures: SizeLineGit[] = [
      { ...ok, checkAttrZ: async () => { throw new Error("git check-attr failed: fatal: bad object"); } },
      { ...ok, diffNumstatZ: async () => { throw new Error("git diff failed"); } },
      { ...ok, sizeMergeBase: async () => { throw new Error("no merge base"); } },
      { ...ok, diffNumstatZ: async () => "not numstat" },
    ];
    for (const g of failures) assert.strictEqual(await computeSizeLine(g, "/bare", "main", head, log), SIZE_UNAVAILABLE);
    assert.strictEqual(await computeSizeLine(ok, "/bare", "main", null, log), SIZE_UNAVAILABLE);
    assert.strictEqual(warns.length, 5);
  });

  it("a numstat path absent from the check-attr result renders unavailable, never path-rule buckets", async () => {
    const warns: { msg: string; meta: unknown }[] = [];
    const log = { warn: (msg: string, meta?: unknown) => void warns.push({ msg, meta }) };
    const head = "c".repeat(40);
    // Two paths in the diff; the lookup answers for only one of them (e.g. git echoed a differently
    // quoted path back). The missing one must not silently fall back to the path rules.
    const partial: SizeLineGit = {
      sizeMergeBase: async () => "d".repeat(40),
      diffNumstatZ: async () => "1\t0\tsrc/a.ts\0" + "4\t0\tstore/q.sql.go\0",
      checkAttrZ: async () => new Map([["src/a.ts", { "linguist-generated": "unspecified" }]]),
    };
    assert.strictEqual(await computeSizeLine(partial, "/bare", "main", head, log), SIZE_UNAVAILABLE);
    assert.strictEqual(warns.length, 1);
    assert.strictEqual(warns[0]!.msg, "PR size line unavailable");
    assert.match(JSON.stringify(warns[0]!.meta), /store\/q\.sql\.go/);
    // An entirely empty map (every path missing) is unavailable too.
    assert.strictEqual(await computeSizeLine({ ...partial, checkAttrZ: async () => new Map() }, "/bare", "main", head, log), SIZE_UNAVAILABLE);
  });
});

describe("mrDescription carries the size line (PRD #1798 M1)", () => {
  const SIZE = `**Size:** code +1 ${MINUS}0 · 1 file`;
  // PRD #1798 M6: the size line lives in the size-only description region, which precedes the
  // completion block; without a size line the body is the completion block alone.
  const REGION = `<!-- uzi:description:start v1 -->\n${SIZE}\n<!-- uzi:description:end -->`;
  it("issue arm: a size-only region before the completion block; absent ⇒ no region", () => {
    const claim = makeClaim({ issue_iid: 1, issue_title: "Do the thing" });
    const without = mrDescription(claim, "agent/issue-1");
    const withSize = mrDescription(claim, "agent/issue-1", undefined, undefined, undefined, undefined, undefined, undefined, true, undefined, false, SIZE);
    assert.strictEqual(mrDescription(claim, "agent/issue-1", undefined, undefined, undefined, undefined, undefined, undefined, true, undefined, false, ""), without);
    assert.strictEqual(withSize, `${REGION}\n\n${without}`);
    assert.ok(without.startsWith("<!-- uzi:completion:start v1 -->\n"), without);
  });
  it("per-kind arm: the same size-only region before the completion block", () => {
    const claim = makeClaim({ kind: "prompt", issue_iid: null, issue_title: "Prompt run" } as Parameters<typeof makeClaim>[0]);
    const without = mrDescription(claim, "uzi/prompt/x");
    const withSize = mrDescription(claim, "uzi/prompt/x", undefined, undefined, undefined, undefined, undefined, undefined, true, undefined, false, SIZE);
    assert.strictEqual(withSize, `${REGION}\n\n${without}`);
  });
});

describe("RunRunner puts the size line in the opened MR body (PRD #1798 M1)", () => {
  installHarness();
  it("the created MR's description opens with a size-only region carrying the **Size:** line", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    const body = JSON.parse(calls.find((c) => c.method === "POST")!.body ?? "{}") as { description: string };
    // StubExecutor commits one markdown file; the line is computed from the landed tracking tip
    // against the merge-base with main, and sits in the region before the completion block.
    // PRD #1798 M6: the region also ends with the deterministic provenance line (D12).
    assert.match(
      body.description,
      /^<!-- uzi:description:start v1 -->\n\*\*Size:\*\* docs \+\d+ −0 · 1 file\n\nDescribes `[0-9a-f]{7}` against `main`\.\n<!-- uzi:description:end -->\n\n<!-- uzi:completion:start v1 -->\n/,
    );
  });
});

describe("RunRunner keeps the size line in the verified-head reconcile (PRD #1798 M1)", () => {
  installHarness();
  it("INTERLOCKED run: the post-verify description rewrite (PUT) still carries the **Size:** line", async () => {
    // The completion interlock (PRD #1226 M4) opens the MR, verifies head H, then REWRITES the body
    // via updateMergeRequestDescription to add Closes. That rewrite must keep the size line.
    const H = "1111111111111111111111111111111111111111";
    const SIZE = `**Size:** code +3 ${MINUS}1 · tests +2 ${MINUS}0 · 2 files`;
    const { gitlab, calls, all } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1798, { config: { completion_contract_version: 1, contract_revision: 1 } });
    api.setCompletionPermitResponse(true);
    harnessGit.trackingTip = (async () => H) as typeof harnessGit.trackingTip;
    // H is a synthetic landed head, so the size reads are pinned to a deterministic diff.
    harnessGit.sizeMergeBase = async () => "d".repeat(40);
    harnessGit.diffNumstatZ = async () => "3\t1\tsrc/a.ts\0" + "2\t0\tsrc/a.test.ts\0";
    harnessGit.checkAttrZ = async (_bare, _head, paths) => new Map(paths.map((p) => [p, {}]));

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
    // PRD #1798 M6: create, the publisher's read and revalidate, the head verify, then the reconcile's
    // read, write and confirm, then the post-add head re-verify.
    assert.deepStrictEqual(all.map((c) => c.method), ["POST", "GET", "GET", "GET", "GET", "PUT", "GET", "GET"]);
    const post = JSON.parse(calls.find((c) => c.method === "POST")!.body ?? "{}") as { description: string };
    const put = JSON.parse(calls.find((c) => c.method === "PUT")!.body ?? "{}") as { description: string };
    const region = `<!-- uzi:description:start v1 -->\n${SIZE}\n\nDescribes \`1111111\` against \`main\`.\n<!-- uzi:description:end -->\n\n`;
    assert.ok(post.description.startsWith(region), post.description);
    assert.match(put.description, /Closes #1798/, "the PUT is the verified-head reconcile");
    assert.ok(put.description.startsWith(region), `the reconcile body lost the size line:\n${put.description}`);
  });
});
