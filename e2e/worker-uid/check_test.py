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
