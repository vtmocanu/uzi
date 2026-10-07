"""Exercise the fail-closed leaf report checker with Node-shaped JUnit."""
import unittest
import xml.etree.ElementTree as ET
from check import check


class CheckerTest(unittest.TestCase):
    expected = [{"file": "agent/test/example.test.ts", "names": ["suite", "leaf"]}]

    def report(self, body):
        return ET.fromstring(f'<testsuites><testsuite name="suite">{body}</testsuite></testsuites>')

    def case(self, body=""):
        return f'<testcase name="leaf" file="/app/test/example.test.ts">{body}</testcase>'

    def test_pass(self):
        self.assertEqual(check(self.expected, self.report(self.case())), [])

    def test_missing_duplicate_and_skipped(self):
        for body in ("", self.case() * 2, self.case('<skipped/>')):
            with self.subTest(body=body):
                self.assertTrue(check(self.expected, self.report(body)))

    def test_failure_and_cancellation(self):
        for body in ('<failure/>', '<error/>'):
            self.assertTrue(check(self.expected, self.report(self.case(body))))

    def test_local_diagnostics(self):
        for tag in ("skipped", "failure", "error"):
            with self.subTest(tag=tag):
                errors = check(self.expected, self.report(self.case(
                    f'<{tag} message="cancelled &amp; timed out" type="testTimeoutFailure" '
                    'stack="frame attribute">Error &lt;fixture&gt;\n at frame.ts:42</' + tag + '>'
                )))
                diagnostic = "\n".join(errors)
                for detail in ("agent/test/example.test.ts: suite > leaf",
                               f"{tag} from testcase: suite > leaf",
                               "message=cancelled & timed out", "type=testTimeoutFailure",
                               "stack=frame attribute", "Error <fixture>", "at frame.ts:42"):
                    self.assertIn(detail, diagnostic)

    def test_inherited_and_local_provenance(self):
        expected = [{"file": "agent/test/example.test.ts", "names": ["suite", "nested", "leaf"]}]
        for tag in ("skipped", "failure", "error"):
            root = ET.fromstring(
                f'<testsuites><testsuite name="suite"><{tag} message="parent reason"/>'
                '<testsuite name="nested"><error>nested stack</error>'
                + self.case('<failure message="leaf reason"/>')
                + '</testsuite></testsuite></testsuites>'
            )
            diagnostic = "\n".join(check(expected, root))
            for detail in (f"{tag} from testsuite: suite: message=parent reason",
                           "error from testsuite: suite > nested: nested stack",
                           "failure from testcase: suite > nested > leaf: message=leaf reason"):
                self.assertIn(detail, diagnostic)

    def test_no_reason(self):
        for tag in ("skipped", "failure", "error"):
            diagnostic = "\n".join(check(self.expected, self.report(self.case(f'<{tag}/>'))))
            self.assertIn("no reason supplied", diagnostic)

    def test_duplicate_source_identity(self):
        self.assertIn("duplicate source leaf identity",
                      check(self.expected * 2, self.report(self.case())))

    def test_skipped_suite_does_not_prove_leaf(self):
        self.assertTrue(check(self.expected, ET.fromstring(
            '<testsuites><testcase name="suite" file="/app/test/example.test.ts"><skipped/></testcase></testsuites>'
        )))

    def test_wrong_file_or_ancestry(self):
        for xml in (self.case().replace('example', 'other'), self.case()):
            self.assertTrue(check(self.expected, ET.fromstring(f'<testsuites>{xml}</testsuites>')))

    def test_empty_inventory(self):
        self.assertTrue(check([], self.report(self.case())))

    def test_extra_executed_leaf(self):
        extra = self.case().replace('name="leaf"', 'name="extra"')
        self.assertTrue(check(self.expected, self.report(self.case() + extra)))
        extra = extra.replace('</testcase>', '<skipped/></testcase>')
        self.assertEqual(check(self.expected, self.report(self.case() + extra)), [])


if __name__ == '__main__':
    unittest.main()
