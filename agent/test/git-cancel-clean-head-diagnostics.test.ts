import { test } from "node:test";
import assert from "node:assert/strict";
import { GitCache } from "../src/git.js";
import { recordingLogger } from "./helpers.js";

const tip = "a".repeat(40), oid = "b".repeat(40);
const cap = 2 * 1024 * 1024;
type GitReads = {
  execScoped(command: string, args: string[]): Promise<{ stdout: string; stderr: string }>;
  runGitAsRunner(cwd: string, args: string[]): Promise<string>;
};
const row = (name: string, size = 0) => `100644 blob ${oid} ${size}\t${name}\0`;
const record = (reason: string) => ({ level: "info", msg: "owner cancel clean-head proof exhausted", reason });

// Synthetic trusted ls-tree output isolates the five pre-existing resource checks.
// No source inspection, cleanup or custody RPC may follow a cheap-proof refusal.
for (const dimension of ["blob_bytes", "total_blob_bytes", "entry_count", "tree_listing_bytes", "manifest_bytes"] as const) {
  test(`#3031 cheap clean-head refusal distinguishes ${dimension}`, { skip: process.platform !== "linux" }, async t => {
    const logger = recordingLogger(), git = new GitCache("/unused", logger.logger);
    const entries = dimension === "blob_bytes" ? row("file", 4 * 1024 * 1024 + 1)
      : dimension === "total_blob_bytes" ? Array.from({ length: 33 }, (_, n) => row(`file${n}`, 4 * 1024 * 1024)).join("")
      : dimension === "entry_count" ? Array.from({ length: 20001 }, (_, n) => row(String(n))).join("")
      : dimension === "manifest_bytes" ? Array.from({ length: 18000 }, (_, n) => row(String(n).padStart(60, "x"))).join("") : "";
    if (dimension === "manifest_bytes") assert.ok(Buffer.byteLength(entries) <= cap, "listing itself fits its unchanged cap");
    let treeReads = 0, byteReaders = 0;
    t.mock.method(git as unknown as GitReads, "execScoped", async (_command: string, args: string[]) => {
      if (args.includes("ls-tree")) {
        treeReads++;
        if (dimension === "tree_listing_bytes") throw Object.assign(new Error("stdout maxBuffer length exceeded"),
          { code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER" });
        return { stdout: entries, stderr: "" };
      }
      byteReaders++;
      throw new Error("content reader must not run after refusal");
    });
    t.mock.method(git as unknown as GitReads, "runGitAsRunner", async (_cwd: string, args: string[]) => {
      if (args.includes("config") || args.includes("status") || args.includes("--others") || args.includes("-v")) return "";
      if (args.includes("--stage")) return entries.split("\0").filter(Boolean).map(entry => {
        const match = /^(\d+) blob ([a-f0-9]+) +[0-9]+\t([\s\S]+)$/.exec(entry)!;
        return `${match[1]} ${match[2]} 0\t${match[3]}\0`;
      }).join("");
      if (args.includes("rev-parse")) return tip;
      throw new Error("unexpected read");
    });
    assert.equal(await git.credentialFreeCancelCleanHead("/private-source", "/private-bare", tip), null);
    assert.equal(treeReads, 1);
    assert.equal(byteReaders, 0);
    assert.deepEqual(logger.lines, [record(dimension)]);
  });
}

test("#3031 cheap clean-head stderr overflow and unknown errors are not mislabeled listing exhaustion", { skip: process.platform !== "linux" }, async t => {
  const logger = recordingLogger(), git = new GitCache("/unused", logger.logger);
  const errors: unknown[] = [Object.assign(new Error("stderr maxBuffer length exceeded"), {
    code: "ERR_CHILD_PROCESS_STDIO_MAXBUFFER", outputStream: "stderr",
  }), Object.assign(new Error("subprocess output exceeded cap"), { outputExceeded: true }), null];
  t.mock.method(git as unknown as GitReads, "execScoped", async () => { throw errors.shift(); });
  for (let n = 0; n < 3; n++) assert.equal(await git.credentialFreeCancelCleanHead("/private-source", "/private-bare", tip), null);
  assert.deepEqual(logger.lines, []);
});

for (const state of ["clean", "dirty", "head_moved"] as const) {
  test(`#3031 cheap clean-head ${state} keeps its original return contract`, { skip: process.platform !== "linux" }, async t => {
    const logger = recordingLogger(), git = new GitCache("/unused", logger.logger);
    let heads = 0;
    t.mock.method(git as unknown as GitReads, "execScoped", async (_command: string, args: string[]) =>
      ({ stdout: args.includes("ls-tree") ? row("file") : "clean\n", stderr: "" }));
    t.mock.method(git as unknown as GitReads, "runGitAsRunner", async (_cwd: string, args: string[]) => {
      if (args.includes("--stage")) return `100644 ${oid} 0\tfile\0`;
      if (args.includes("rev-parse")) return state === "head_moved" && ++heads === 2 ? oid : tip;
      if (args.includes("status") && state === "dirty") return "?? private-file\0";
      return "";
    });
    assert.equal(await git.credentialFreeCancelCleanHead("/private-source", "/private-bare", tip), state === "clean" ? tip : null);
    assert.deepEqual(logger.lines, []);
  });
}
