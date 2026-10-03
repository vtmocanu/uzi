#!/usr/bin/env python3
"""Exercise the real tap guard CLI without Homebrew, network or tap writes."""

import os
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path

GUARD = Path(__file__).with_name("brew-tap-guard.py")


def formula(tag):
    return f'class UziCliRc < Formula\n  url "https://github.com/vtmocanu/uzi/archive/refs/tags/{tag}.tar.gz"\nend\n'


class TapGuardTests(unittest.TestCase):
    def run_guard(self, incoming, source):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "formula.rb"
            if source is not None:
                path.write_text(source, encoding="utf-8")
            return subprocess.run([sys.executable, str(GUARD), incoming, str(path)], capture_output=True, text=True, check=False)

    def test_monotonic_versions(self):
        for current, incoming, code in [
            ("v0.85.0-rc.11", "v0.85.0", 0),
            ("v0.85.0", "v0.86.0-rc.1", 0),
            ("v0.85.0-rc.9", "v0.85.0-rc.11", 0),
            ("v0.85.0", "v0.85.0", 3),
            ("v0.85.0-rc.11", "v0.85.0-rc.11", 3),
            ("v0.85.0", "v0.85.0-rc.11", 3),
            ("v0.86.0-rc.1", "v0.85.0", 3),
            ("v0.85.0-rc.11", "v0.85.0-rc.9", 3),
            ("v0.90.0", "v0.89.9", 3),
        ]:
            with self.subTest(current=current, incoming=incoming):
                result = self.run_guard(incoming, formula(current))
                self.assertEqual(result.returncode, code, result.stderr)
                self.assertIn("Advancing" if code == 0 else "Skipping", result.stdout)

    def test_invalid_current_refuses(self):
        for source in [None, "", "not a formula", formula("v0.85.0-rc.01"),
                       formula("v0.85.0-beta.1"), formula("v0.85.0.1"),
                       formula("v0.85.0") * 2,
                       formula("v0.85.0") + '  version "99.0.0"\n',
                       formula("v0.85.0") + '  version_scheme 1\n',
                       formula("v0.85.0") + '  url "https://example.com/other.tar.gz"\n']:
            with self.subTest(source=source):
                result = self.run_guard("v0.86.0", source)
                self.assertEqual(result.returncode, 2, result.stdout)
                self.assertIn("refusing tap write", result.stderr)

    def test_invalid_incoming_refuses(self):
        for tag in ["0.86.0", "v0.086.0", "v0.86.0-beta.1", "v0.86.0-rc.0", "v0.86.0-rc.01", "v0.86.0+build"]:
            with self.subTest(tag=tag):
                result = self.run_guard(tag, formula("v0.85.0"))
                self.assertEqual(result.returncode, 2, result.stdout)


class PublishWorkflowTests(unittest.TestCase):
    def run_workflow(self, incoming, stable="v0.84.0", rc="v0.85.0-rc.11", gh_fails=False):
        # Run the actual workflow shell with fake GitHub/Task executables, so a
        # routing or exit-status regression is exercised without a real tap push.
        root = GUARD.parent.parent
        workflow = (root / ".github/workflows/brew.yml").read_text(encoding="utf-8")
        body = textwrap.dedent(workflow.rsplit("        run: |\n", 1)[1])
        with tempfile.TemporaryDirectory() as directory:
            scratch = Path(directory)
            (scratch / "uzi-cli.rb").write_text(formula(stable), encoding="utf-8")
            (scratch / "uzi-cli-rc.rb").write_text(formula(rc), encoding="utf-8")
            gh = scratch / "gh"
            gh.write_text("#!/usr/bin/env bash\nset -eu\n"
                          "[ \"$GH_FAILS\" = 0 ] || exit 1\n"
                          "case \"${!#}\" in\n"
                          "  *uzi-cli-rc.rb*) cat \"$RUNNER_TEMP/uzi-cli-rc.rb\" ;;\n"
                          "  *uzi-cli.rb*) cat \"$RUNNER_TEMP/uzi-cli.rb\" ;;\n"
                          "  *) exit 1 ;;\nesac\n", encoding="utf-8")
            task = scratch / "task"
            task.write_text('#!/usr/bin/env bash\nprintf \'%s\\n\' "$*" >> "$RUNNER_TEMP/task.log"\n', encoding="utf-8")
            gh.chmod(0o700)
            task.chmod(0o700)
            script = scratch / "publish.sh"
            script.write_text(body, encoding="utf-8")
            env = dict(os.environ, VERSION=incoming, RUNNER_TEMP=directory,
                       HOMEBREW_TAP_TOKEN="test-placeholder", GH_FAILS="1" if gh_fails else "0",
                       PATH=directory + os.pathsep + os.environ["PATH"])
            result = subprocess.run(["bash", str(script)], cwd=root, env=env,
                                    capture_output=True, text=True, check=False)
            log = scratch / "task.log"
            calls = log.read_text(encoding="utf-8").splitlines() if log.exists() else []
            return result, calls

    def test_stable_advances_both_formulas(self):
        result, calls = self.run_workflow("v0.85.0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [
            "--yes brew:formula VERSION=v0.85.0",
            "--yes brew:publish VERSION=v0.85.0 HOMEBREW_TAP_TOKEN=test-placeholder",
            "--yes brew-rc:formula VERSION=v0.85.0",
            "--yes brew-rc:publish VERSION=v0.85.0 HOMEBREW_TAP_TOKEN=test-placeholder",
        ])

    def test_rc_advances_only_rc_formula(self):
        result, calls = self.run_workflow("v0.86.0-rc.1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [
            "--yes brew-rc:formula VERSION=v0.86.0-rc.1",
            "--yes brew-rc:publish VERSION=v0.86.0-rc.1 HOMEBREW_TAP_TOKEN=test-placeholder",
        ])

    def test_delayed_stable_leaves_higher_rc(self):
        result, calls = self.run_workflow("v0.85.0", rc="v0.86.0-rc.1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(" brew:" in call for call in calls), calls)

    def test_old_rc_and_equal_stable_skip(self):
        for tag in ["v0.85.0-rc.11", "v0.85.0"]:
            with self.subTest(tag=tag):
                result, calls = self.run_workflow(tag, stable="v0.85.0", rc="v0.85.0")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(calls, [])

    def test_bad_or_unreadable_current_never_publishes(self):
        for kwargs in [{"gh_fails": True}, {"rc": "v0.85.0-rc.01"}]:
            with self.subTest(kwargs=kwargs):
                result, calls = self.run_workflow("v0.86.0-rc.1", **kwargs)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
