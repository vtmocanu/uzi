// Helper for runner-shared-real.test.mts: runs a list of filesystem operations as whatever uid it
// was started under and reports each result plus the process identity read from procfs. It is a
// plain .mjs so a child started as the dind, runner or runner-cmd uid needs no transpiler and no
// writable temp directory.
//   node runner-shared-probe.mjs '<json array of ops>'
// ops: rename {from,to} | unlink {path} | rmdir {path} | symlink {target,path} |
//      mkdir {path,mode} | create {path,data} | chmod {path,mode} | hold {path} (append every 10 ms
//      until killed; never returns)
import fs from "node:fs";

process.umask(0);
const results = [];
for (const op of JSON.parse(process.argv[2])) {
  const subject = op.path ?? op.from;
  try {
    if (op.op === "rename") fs.renameSync(op.from, op.to);
    else if (op.op === "unlink") fs.unlinkSync(op.path);
    else if (op.op === "rmdir") fs.rmdirSync(op.path);
    else if (op.op === "symlink") fs.symlinkSync(op.target, op.path);
    else if (op.op === "mkdir") fs.mkdirSync(op.path, { mode: op.mode ?? 0o755 });
    else if (op.op === "create") fs.writeFileSync(op.path, op.data ?? "x", { flag: "wx", mode: 0o644 });
    else if (op.op === "chmod") fs.chmodSync(op.path, op.mode);
    else if (op.op === "hold") {
      fs.appendFileSync(op.path, "tick\n");
      console.log(JSON.stringify({ held: true }));
      // Ends when the parent closes our stdin (root cannot signal another uid's process).
      process.stdin.on("end", () => process.exit(0));
      process.stdin.resume();
      setInterval(() => { try { fs.appendFileSync(op.path, "tick\n"); } catch { /* the tree was re-owned or removed: keep going until stdin closes */ } }, 10);
      await new Promise(() => {});
    } else throw new Error(`unknown op ${op.op}`);
    results.push({ op: op.op, path: subject, ok: true });
  } catch (error) {
    results.push({ op: op.op, path: subject, ok: false, code: error.code ?? String(error) });
  }
}
const status = fs.readFileSync("/proc/self/status", "utf8");
const field = (name) => status.match(new RegExp(`^${name}:\\s*(.*)$`, "m"))?.[1].trim().split(/\s+/) ?? [];
console.log(JSON.stringify({
  results,
  uid: process.getuid(),
  gid: process.getgid(),
  groups: field("Groups").filter(Boolean).map(Number),
  capEff: field("CapEff")[0],
}));
