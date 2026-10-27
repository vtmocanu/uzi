// Builds a throwaway checkout and a matching replay corpus under os.tmpdir() for the offline
// replay tests. Every id is derived through the library's own git plumbing constants.
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import {
  CORPUS_DIR, CREDENTIAL_MODE, READER_IMPLEMENTATION, REPLAY_DIFF_ARGS, replayGit, replayIdentityEnv,
} from "../src/code-cross-check-replay.js";
import { REPLAY_PROFILES } from "../src/code-cross-check-replay-profiles.js";

export interface TempCorpus {
  root: string;
  checkout: string;
  corpusDir: string;
  manifestSha256: string;
  defectId: string;
  controlId: string;
  cleanup(): Promise<void>;
}

const sha256 = (data: string | Uint8Array): string => createHash("sha256").update(data).digest("hex");
export const FIXTURE_TITLE = "Batching drops items";
export const FIXTURE_BODY = "fetchAll silently truncates to ten items.";
export const FIXTURE_PLAN = "Plan: make fetchAll return every item.";

const baseFiles = {
  "src/client.ts": "// client\nexport function fetchAll(items: string[]): string[] {\n  const limit = 10;\n  return items.slice(0, limit);\n}\n",
  "src/other.ts": "export const other = 1;\n",
  // Fidelity probes: a tracked file that .gitignore matches, and an export-subst file.
  ".gitignore": "*.log\n",
  "notes/ignored.log": "tracked but ignored\n",
  ".gitattributes": "src/stamp.txt export-subst\n",
  "src/stamp.txt": "$Format:%H$\n",
};
const prefixClient = "// feature: batching\n" + baseFiles["src/client.ts"];
const squashClient = prefixClient.replace("const limit = 10;", "const limit = items.length;");

/** `symlinks` maps a tracked path to its link target; the links exist in the base tree and so in every case. */
export async function buildTempCorpus(opts: { symlinks?: Record<string, string> } = {}): Promise<TempCorpus> {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "replay-fixture-"));
  const checkout = path.join(root, "checkout");
  await fs.mkdir(checkout);
  await replayGit(["init", "-q"], { cwd: checkout });
  const git = async (args: string[], env?: Record<string, string>): Promise<string> =>
    (await replayGit(args, { cwd: checkout, env })).toString("utf8").trim();
  let n = 0;
  const treeWith = async (parent: string | null, files: Record<string, string>, links: Record<string, string> = {}): Promise<string> => {
    const env = { GIT_INDEX_FILE: path.join(root, `index-${n++}`) };
    await git(parent ? ["read-tree", parent] : ["read-tree", "--empty"], env);
    for (const [mode, entries] of [["100644", files], ["120000", links]] as const) {
      for (const [name, content] of Object.entries(entries)) {
        const tmp = path.join(root, `blob-${n++}`);
        await fs.writeFile(tmp, content);
        const id = await git(["hash-object", "-w", tmp]);
        await git(["update-index", "--add", "--cacheinfo", `${mode},${id},${name}`], env);
      }
    }
    return git(["write-tree"], env);
  };
  const commit = (tree: string, parent: string | null, message: string): Promise<string> =>
    git(["commit-tree", tree, ...(parent ? ["-p", parent] : []), "-m", message], replayIdentityEnv());

  const baseTree = await treeWith(null, baseFiles, opts.symlinks);
  const base = await commit(baseTree, null, "base");
  // Defect: prefix = feature only; squash = feature plus the fix hunk.
  const prefixTree = await treeWith(baseTree, { "src/client.ts": prefixClient });
  const squashTree = await treeWith(prefixTree, { "src/client.ts": squashClient });
  const squash = await commit(squashTree, base, "squash");
  const prefixCommit = await commit(prefixTree, base, "code cross-check replay prefix d-0001");
  const diff = async (...refs: string[]): Promise<Buffer> => replayGit(["diff", ...REPLAY_DIFF_ARGS, ...refs], { cwd: checkout });
  // Control: the prefix is the squash tree itself, so nothing is removed.
  const controlTree = await treeWith(baseTree, { "src/other.ts": "export const other = 2;\n" });
  const controlSquash = await commit(controlTree, base, "control squash");
  const controlPrefix = await commit(controlTree, base, "code cross-check replay prefix c-0001");

  await fs.mkdir(path.join(checkout, "agent"));
  await fs.writeFile(path.join(checkout, "agent/package.json"), JSON.stringify({ name: "uzi-agent", version: "0.0.0-test" }));
  const corpusDir = path.join(checkout, CORPUS_DIR);
  const write = async (rel: string, content: string | Uint8Array): Promise<string> => {
    const file = path.join(corpusDir, rel);
    await fs.mkdir(path.dirname(file), { recursive: true });
    await fs.writeFile(file, content);
    return sha256(content);
  };
  const defect = { id: "d-0001", squash, prefixTree, prefixCommit, kind: "defect" as const };
  const control = { id: "c-0001", squash: controlSquash, prefixTree: controlTree, prefixCommit: controlPrefix, kind: "control" as const };
  const caseHashes: { id: string; case_sha256: string }[] = [];
  for (const c of [defect, control]) {
    const dir = `cases/${c.id}/`;
    const isDefect = c.kind === "defect";
    const issue = await write(dir + "issue.md", `# ${FIXTURE_TITLE}\n\n${FIXTURE_BODY}\n`);
    const plan = await write(dir + "plan.md", FIXTURE_PLAN);
    const prefixPatch = await diff(base, c.prefixCommit);
    const removed = isDefect ? await diff(prefixTree, squashTree) : Buffer.alloc(0);
    const artifacts = {
      "issue.md": issue, "plan.md": plan,
      "prefix.patch": await write(dir + "prefix.patch", prefixPatch),
      "removed.patch": await write(dir + "removed.patch", removed),
    };
    const witnesses = [];
    if (isDefect) {
      const sha = await write(dir + "witness/client.blob", prefixClient);
      const tmp = path.join(root, "witness-blob");
      await fs.writeFile(tmp, prefixClient);
      witnesses.push({ file: "witness/client.blob", path: "src/client.ts", blob_id: await git(["hash-object", tmp]), sha256: sha });
    }
    const body = {
      id: c.id, kind: c.kind, input_provenance: isDefect ? "reconstructed" : "recorded",
      plan_source: isDefect ? "synthetic" : "recorded", source: { pr: 10, issue: isDefect ? 11 : 12 },
      squash_sha: c.squash, base_sha: base, prefix_tree: c.prefixTree, prefix_commit: c.prefixCommit,
      reversal: isDefect ? "recovered" : "none", artifacts, witnesses,
      expected: isDefect ? [{
        id: "D1", paths: ["src/client.ts"], line_range: [4, 4], use_line_range: true, match: ["truncat", "limit"],
        detectability: "read_only", rationale: "the limit constant truncates results", in_diff: false,
        signature: { path: "src/client.ts", literal: "const limit = 10;", present: true, witness: "witness/client.blob" },
      }] : [],
    };
    caseHashes.push({ id: c.id, case_sha256: await write(dir + "case.json", JSON.stringify(body, null, 2)) });
  }
  const pinsSha = await write("pins.json", JSON.stringify({
    corpus_version: "test-1",
    brief_sha256: {
      codex: REPLAY_PROFILES.baseline.codex, claude: REPLAY_PROFILES.baseline.claude,
    },
    reader_implementation: READER_IMPLEMENTATION, credential_mode: CREDENTIAL_MODE,
  }));
  const manifestSha256 = await write("manifest.json", JSON.stringify({ corpus_version: "test-1", pins_sha256: pinsSha, cases: caseHashes }));
  return {
    root, checkout, corpusDir, manifestSha256, defectId: defect.id, controlId: control.id,
    cleanup: () => fs.rm(root, { recursive: true, force: true }),
  };
}
