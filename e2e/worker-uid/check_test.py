"""Exercise the fail-closed leaf report checker with Node-shaped JUnit."""
import unittest
import xml.etree.ElementTree as ET
import subprocess
import sys
import tempfile
from pathlib import Path
from check import check


class CheckerTest(unittest.TestCase):
    expected = [{"file": "agent/test/example.test.ts", "names": ["suite", "leaf"]}]

    def report(self, body):
        return ET.fromstring(f'<testsuites><testsuite name="suite">{body}</testsuite></testsuites>')

    def case(self, body=""):
        return f'<testcase name="leaf" file="/app/test/example.test.ts">{body}</testcase>'

    def test_pass(self):
        self.assertEqual(check(self.expected, self.report(self.case())), [])

    def test_failed_leaf_captured_output(self):
        root = self.report(self.case('<failure message="controlled"/><system-err>'
                                     'private unrelated output\nadvice-teardown-diagnostic {"failure":true}'
                                     '</system-err>'))
        output = "\n".join(check(self.expected, root))
        self.assertIn('advice-teardown-diagnostic {"failure":true}', output)
        self.assertNotIn('private unrelated', output)

    def test_passing_leaf_captured_output_stays_silent(self):
        self.assertEqual(check(self.expected, self.report(self.case(
            '<system-out>advice-teardown-diagnostic {"passing":true}</system-out>'))), [])

    def test_node_junit_comments_are_scoped_to_preceding_failed_leaf(self):
        xml = ('<testsuites><testsuite name="suite">'
               '<testcase name="pass" file="/app/test/example.test.ts"/>'
               '<!-- advice-teardown-diagnostic {"passing":true} -->'
               + self.case('<failure message="controlled"/>')
               + '<!-- unrelated private output -->'
               '<!-- advice-teardown-diagnostic {"failure":true} -->'
               '</testsuite><!-- advice-teardown-diagnostic {"global":true} --></testsuites>')
        parser = ET.XMLParser(target=ET.TreeBuilder(insert_comments=True))
        expected = [*self.expected, {"file": "agent/test/example.test.ts", "names": ["suite", "pass"]}]
        output = "\n".join(check(expected, ET.fromstring(xml, parser=parser)))
        self.assertIn('advice-teardown-diagnostic {"failure":true}', output)
        for private in ('"passing"', '"global"', 'unrelated private'):
            self.assertNotIn(private, output)

    def test_diagnostic_output_is_bounded_and_control_free(self):
        text = '\n'.join('advice-teardown-diagnostic ' + '\x1b\u202e' + 'é' * 10000 for _ in range(20))
        # C0 escape is not legal XML, so assign it after parsing the structural fixture.
        root = self.report(self.case('<failure/><system-out/>'))
        root.find('.//system-out').text = text
        lines = [line for line in check(self.expected, root) if line.startswith('advice-teardown-diagnostic ')]
        self.assertLessEqual(len(lines), 8)
        self.assertLessEqual(sum(len((line + '\n').encode('utf-8')) for line in lines), 16384)
        self.assertTrue(lines)
        self.assertNotIn('\x1b', ''.join(lines))
        self.assertNotIn('\u202e', ''.join(lines))

    def test_cli_keeps_node_diagnostic_comments(self):
        with tempfile.TemporaryDirectory() as directory:
            expected = Path(directory) / 'expected.json'
            report = Path(directory) / 'results.xml'
            expected.write_text('[{"file":"agent/test/example.test.ts","names":["suite","leaf"]}]')
            report.write_text('<testsuites><testsuite name="suite">' + self.case('<failure/>')
                              + '<!-- advice-teardown-diagnostic {"failure":true} -->'
                              '</testsuite></testsuites>')
            result = subprocess.run([sys.executable, str(Path(__file__).with_name('check.py')),
                                     str(expected), str(report)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn('advice-teardown-diagnostic {"failure":true}', result.stderr)

    def test_ancestor_failure_does_not_expose_passing_leaf_output(self):
        root = self.report('<failure message="suite failed"/>' + self.case(
            '<system-out>advice-teardown-diagnostic {"passing":true}</system-out>'))
        self.assertNotIn('"passing"', "\n".join(check(self.expected, root)))

    def test_total_report_budget_preserves_prefixes(self):
        from check import MAX_DIAGNOSTIC_BYTES, DIAGNOSTIC_PREFIX
        first = DIAGNOSTIC_PREFIX + 'x' * (MAX_DIAGNOSTIC_BYTES - 30 - len(DIAGNOSTIC_PREFIX))
        root = self.report(self.case('<failure/><system-out/>'))
        root.find('.//system-out').text = first + '\n' + DIAGNOSTIC_PREFIX + 'y' * 100
        lines = [line for line in check(self.expected, root) if line.startswith(DIAGNOSTIC_PREFIX)]
        self.assertEqual(lines, [first])

    def test_line_budget_is_global_across_failing_leaves(self):
        expected = [self.expected[0], {"file": "agent/test/example.test.ts", "names": ["suite", "other"]}]
        text = '\n'.join('advice-teardown-diagnostic {"event":1}' for _ in range(20))
        root = self.report(self.case('<failure/><system-err/>') +
                           self.case('<failure/><system-out/>').replace('name="leaf"', 'name="other"'))
        root.find('.//system-err').text = text
        root.find('.//system-out').text = text
        lines = [line for line in check(expected, root) if line.startswith('advice-teardown-diagnostic ')]
        self.assertEqual(len(lines), 8)

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
