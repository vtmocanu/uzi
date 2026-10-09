#!/usr/bin/env python3
"""Focused native Semgrep controls; generated TypeScript is scanned, never run."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
RULE = ROOT / "semgrep/codex-no-direct-spawn.yml"
LEAF = "agent/src/codex/pr-description-eval-launch.ts"
IMPORT = 'import { spawn, type ChildProcess } from "node:child_process";'
CALL = '(testSeams.spawn ?? spawn)(CODEX_BIN, [...PROVIDER_CHILD_ARGV], { cwd, env, stdio: ["pipe", "pipe", "pipe"] });'


def scan(root, target, expected):
    # One foreground attempt per control. Failure blocks subsequent controls.
    result = subprocess.run(
        ["semgrep", "scan", "--config", str(RULE), "--error", "--json",
         "--no-git-ignore", "--metrics", "off", target],
        cwd=root, capture_output=True, text=True, timeout=60,
        env={**os.environ, "SEMGREP_ENABLE_VERSION_CHECK": "0"},
    )
    output = json.loads(result.stdout)
    ids = sorted({match["check_id"].split(".")[-1] for match in output["results"]})
    print(f"{target}: rc={result.returncode} matched_ids={','.join(ids) or 'none'}", flush=True)
    assert not output["errors"], "Semgrep scan error"
    assert result.returncode == (1 if expected else 0), "unexpected Semgrep rc"
    assert set(expected) <= set(ids), "missing guard finding"
    if not expected:
        assert not ids, "sanctioned source rejected"


def main():
    scan(ROOT, "agent/src/codex", [])
    sanctioned = (ROOT / LEAF).read_text()
    guard = "codex-evaluator-no-extra-direct-spawn"
    exact_import = "codex-evaluator-exact-child-process-import"
    exact_launch = "codex-evaluator-exact-launch"
    controls = [
        ("sanctioned-helper", LEAF, sanctioned, []),
        ("new-bare-call", LEAF, sanctioned + '\nspawn("other", []);', [guard]),
        ("bare-import", LEAF, IMPORT.replace("spawn, type ChildProcess", "spawn"), [exact_import]),
        ("side-effect-import", LEAF, 'import "node:child_process";', [guard]),
        ("namespace-import", LEAF, 'import * as cp from "node:child_process";', [guard]),
        ("default-import", LEAF, 'import cp from "child_process";', [guard]),
        ("aliased-import", LEAF, 'import { spawn as launch, type ChildProcess } from "node:child_process";', [exact_import]),
        ("extra-exec-import", LEAF, IMPORT.replace("spawn,", "spawn, exec,"), [exact_import]),
        ("extra-aliased-import", LEAF, IMPORT.replace("spawn,", "spawn, exec as run,"), [exact_import]),
        ("extra-type-import", LEAF, IMPORT.replace("ChildProcess", "ChildProcess, type SpawnOptions"), [exact_import]),
        ("arbitrary-spawn", LEAF, IMPORT + '\n' + CALL.replace("CODEX_BIN", '"other"'), [guard, exact_launch]),
        ("extra-options", LEAF, IMPORT + '\n' + CALL.replace("cwd, env,", "cwd, env, shell: true,"), [exact_launch]),
        ("arbitrary-argv", LEAF, IMPORT + '\n' + CALL.replace("[...PROVIDER_CHILD_ARGV]", '["other"]'), [guard, exact_launch]),
        ("new-exec", LEAF, sanctioned + '\nexec("other");', [guard]),
        ("new-fork", LEAF, sanctioned + '\nfork("other");', [guard]),
        ("copied-broker", "agent/src/codex/broker.ts", IMPORT + '\n' + CALL, ["codex-no-direct-spawn"]),
        ("copied-future", "agent/src/codex/future.ts", IMPORT + '\n' + CALL, ["codex-no-direct-spawn"]),
    ]
    scratch = ROOT / ".uzi/scratch"
    scratch.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="semgrep-evaluator-", dir=scratch) as temporary:
        for name, file, source, expected in controls:
            root = Path(temporary) / name
            target = root / file
            target.parent.mkdir(parents=True)
            target.write_text(source)
            print(name, flush=True)
            scan(root, file, expected)


if __name__ == "__main__":
    main()
