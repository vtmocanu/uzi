import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
const exec = promisify(execFile);
test("Go fixture retains image tools and font data while overriding Go only", async (t) => {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-go-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const profile = path.join(dir, "profile"), go = path.join(dir, "go");
  for (const sub of [profile + "/bin", profile + "/share/fonts", go + "/bin", go + "/src"]) await fs.mkdir(sub, { recursive: true });
  const cp = (await exec("sh", ["-c", "command -v cp"])).stdout.trim();
  await fs.symlink(cp, profile + "/bin/cp");
  for (const name of ["go", "gofmt", "marker"]) await fs.writeFile(profile + "/bin/" + name,
    "#!/bin/sh\necho original-" + name + "\n", { mode: 0o755 });
  for (const name of ["go", "gofmt"]) await fs.writeFile(go + "/bin/" + name,
    "#!/bin/sh\necho supplied-" + name + "\n", { mode: 0o755 });
  await fs.writeFile(profile + "/share/fonts/marker", "original fonts");
  const handle = path.join(dir, "handle"), overlay = path.join(dir, "overlay");
  await fs.symlink(profile, handle);
  const entry = path.join(dir, "entry");
  await fs.writeFile(entry, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  const script = (await fs.readFile(fileURLToPath(new URL("./go-entrypoint.sh", import.meta.url)), "utf8"))
    .replaceAll("/opt/uzi-toolchain", handle).replaceAll("/opt/uxlab-toolchain", overlay)
    .replaceAll("/usr/local/lib/uxlab-go", go).replaceAll("/usr/local/sbin/uzi-entrypoint", entry);
  const file = path.join(dir, "wrapper");
  await fs.writeFile(file, script);
  await exec("sh", [file], { timeout: 5000, maxBuffer: 8192 });
  assert.equal((await exec(handle + "/bin/go")).stdout, "supplied-go\n");
  assert.equal((await exec(handle + "/bin/gofmt")).stdout, "supplied-gofmt\n");
  assert.equal((await exec(handle + "/bin/marker")).stdout, "original-marker\n");
  assert.equal(await fs.readFile(handle + "/share/fonts/marker", "utf8"), "original fonts");
  assert.equal((await exec(profile + "/bin/go")).stdout, "original-go\n");
  assert.equal(await fs.readFile(profile + "/share/fonts/marker", "utf8"), "original fonts");
});
