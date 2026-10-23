import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
const exec = promisify(execFile);

test("matrix visits all four cells and retains any failing status", async (t) => {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-matrix-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const here = path.join(dir, "e2e/uxlab-render");
  await fs.mkdir(here, { recursive: true });
  await fs.copyFile(fileURLToPath(new URL("./matrix-run.sh", import.meta.url)), path.join(here, "matrix-run.sh"));
  await fs.writeFile(path.join(here, "run.sh"), `#!/bin/sh
echo "$UXLAB_RENDER_CELL/$UXLAB_RENDER_MODE"
if [ "$UXLAB_RENDER_CELL/$UXLAB_RENDER_MODE" = "$FAILING_CELL" ]; then exit 1; fi
`);
  await fs.writeFile(path.join(dir, "docker"), "#!/bin/sh\necho sha256:fixture\n", { mode: 0o755 });
  for (const failing of ["base/off", "candidate/required", "none"]) {
    const result = await exec("bash", [path.join(here, "matrix-run.sh")], {
      env: { ...process.env, PATH: dir + path.delimiter + process.env.PATH,
        UXLAB_RENDER_CANDIDATE_DIR: dir, UXLAB_RENDER_BASE_DIR: dir, FAILING_CELL: failing },
      timeout: 10_000, maxBuffer: 8192,
    }).then(r => ({ ...r, code: 0 }), e => e);
    assert.equal(result.code, failing === "none" ? 0 : 1);
    const rows = result.stdout.trim().split("\n");
    assert.equal(rows.length, 4);
    const expected = ["base/off", "base/required", "candidate/off", "candidate/required"];
    for (let i = 0; i < 4; i++) assert.equal(rows[i], expected[i], "first cell-order mismatch at " + i);
  }
});
