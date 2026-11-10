"""Docker-free tests for the worker-env JUnit checker."""
import json
import tempfile
import unittest
from pathlib import Path
import xml.etree.ElementTree as ET

import check

HERE = Path(__file__).resolve().parent
FILE = "/app/uzi-worker-uid-proof/e2e/worker-env/worker-env-real.test.mts"
PROOFS = {"file": "e2e/worker-env/worker-env-real.test.mts", "leaves": {"a": "leaf a", "b": "leaf b"}}


def report(*cases):
    return ET.fromstring("<testsuites>" + "".join(cases) + "</testsuites>")


def case(name, extra="", file=FILE):
    return f'<testcase name="{name}" time="1.5" file="{file}">{extra}</testcase>'


class CheckTest(unittest.TestCase):
    def errors(self, *cases):
        return check.check(PROOFS, report(*cases))[0]

    def test_all_leaves_pass(self):
        errors, timings = check.check(PROOFS, report(case("leaf a"), case("leaf b")))
        self.assertEqual(errors, [])
        self.assertEqual(timings, [("leaf a", "1.5"), ("leaf b", "1.5")])

    def test_unexecuted_leaf_fails(self):
        self.assertIn("leaf b: expected exactly one result, found 0", self.errors(case("leaf a")))

    def test_duplicate_leaf_fails(self):
        self.assertIn("leaf a: expected exactly one result, found 2", self.errors(case("leaf a"), case("leaf a"), case("leaf b")))

    def test_failure_error_and_skip_fail_with_reason(self):
        for tag in ("failure", "error", "skipped"):
            with self.subTest(tag=tag):
                errors = self.errors(case("leaf a", f'<{tag} message="because">detail</{tag}>'), case("leaf b"))
                self.assertEqual(len(errors), 1)
                self.assertIn(f"leaf a: {tag}: because detail", errors[0])

    def test_unexpected_leaf_and_foreign_file_fail(self):
        self.assertIn("unexpected executed leaf: extra", self.errors(case("leaf a"), case("leaf b"), case("extra")))
        errors = self.errors(case("leaf a"), case("leaf b", file="/other/file.mts"))
        self.assertTrue(any("unexpected file" in e for e in errors), errors)
        self.assertTrue(any("leaf b: expected exactly one result, found 0" in e for e in errors), errors)

    def test_empty_or_duplicate_inventory_fails(self):
        self.assertTrue(check.check({"file": "x", "leaves": {}}, report())[0])
        self.assertTrue(check.check({"file": "x", "leaves": {"a": "same", "b": "same"}}, report(case("same", file="x")))[0])

    def test_committed_inventory_is_well_formed(self):
        proofs = json.loads((HERE / "proofs.json").read_text())
        self.assertEqual(proofs["file"], "e2e/worker-env/worker-env-real.test.mts")
        self.assertEqual(len(set(proofs["leaves"].values())), len(proofs["leaves"]))
        self.assertTrue((HERE / "worker-env-real.test.mts").is_file())

    def test_cli_rejects_dtd_oversize_and_malformed(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            proofs = directory / "proofs.json"
            proofs.write_text(json.dumps(PROOFS))
            results = directory / "results.xml"
            good = "<testsuites>" + case("leaf a") + case("leaf b") + "</testsuites>"
            results.write_text(good)
            self.assertEqual(check.main(["check.py", str(proofs), str(results)]), 0)
            for label, body in (("dtd", '<!DOCTYPE x [<!ENTITY y "z">]>' + good), ("malformed", "<testsuites>"),
                                ("oversize", good + " " * (check.MAX_REPORT_BYTES + 1))):
                with self.subTest(label=label):
                    results.write_text(body)
                    self.assertEqual(check.main(["check.py", str(proofs), str(results)]), 1)
            self.assertEqual(check.main(["check.py", str(proofs), str(directory / "missing.xml")]), 1)


if __name__ == "__main__":
    unittest.main()
