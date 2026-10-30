#!/usr/bin/env python3
"""Bounded public recovery journal fixtures, without live Actions calls."""
import importlib.util
import ast
import copy
import json
from pathlib import Path
import tempfile
import unittest
import textwrap
from unittest.mock import patch


def module():
    spec = importlib.util.spec_from_file_location('release_journal', Path(__file__).with_name('lib') / 'release-journal.py')
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


class Journal(unittest.TestCase):
    def test_audit_wire_preserves_count_and_truncation_across_hops(self):
        journal = module()
        rows = [dict(sha='a' * 40, pr=number + 1, shipping=True) for number in range(1000)]
        kept, count, truncated = journal.audit(json.dumps(rows))
        self.assertEqual((len(kept), count, truncated), (64, 1000, True))
        wire = journal.encoded(kept).decode('ascii')
        self.assertEqual(journal.audit(wire, json.dumps(count), json.dumps(truncated)), (kept, count, truncated))
        for total, marker in [('63', 'true'), ('1000', 'false'), ('true', 'true')]:
            self.assertEqual(journal.audit(wire, total, marker), (None, None, None))

    def test_terminal_instrument_error_is_explicit_not_empty_success(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            record.finish('instrument-error', 'evidence-unavailable')
            data = journal.read(path)
            self.assertEqual(data['state'], 'instrument-error')
            self.assertEqual(data['reason'], 'evidence-unavailable')
            self.assertEqual(data['attempts'], [])
    def test_failed_atomic_replace_keeps_previous_valid_journal(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            before = path.read_bytes()
            with patch.object(journal.os, 'replace', side_effect=OSError('fixture replace failure')):
                with self.assertRaises(OSError):
                    record.main_ci('failed')
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(journal.read(path)['main_ci']['state'], 'unavailable')
            self.assertEqual(list(Path(directory).glob('.release-journal-*')), [])

    def test_checkout_uses_original_workflow_source_for_older_candidate(self):
        workflow = Path(__file__).parent.parent / '.github/workflows/release-auto.yml'
        watch = workflow.read_text().split('\n  watch:\n', 1)[1].split('\n  report:\n', 1)[0]
        self.assertIn('ref: ${{ github.sha }}', watch)
        self.assertNotIn('ref: ${{ needs.prepare.outputs.release }}', watch)
        self.assertIn('RELEASE_SHA: ${{ needs.prepare.outputs.release }}', watch)

    def test_embedded_report_codec_is_identical_and_has_fixture_parity(self):
        journal = module()
        root = Path(__file__).parent.parent
        workflow = (root / '.github/workflows/release-auto.yml').read_text()
        codec = textwrap.dedent(workflow.split('# BEGIN JOURNAL CODEC:', 1)[1].split('\n', 1)[1]
                               .split('          # END JOURNAL CODEC', 1)[0])
        source_tree = ast.parse((root / 'scripts/lib/release-journal.py').read_text())
        target_tree = ast.parse(codec)
        def declarations(tree):
            named = {}
            for node in tree.body:
                if isinstance(node, ast.FunctionDef):
                    named[node.name] = ast.dump(node)
                elif (isinstance(node, ast.Assign) and len(node.targets) == 1
                      and isinstance(node.targets[0], ast.Name)):
                    named[node.targets[0].id] = ast.dump(node)
            return named
        source, embedded = declarations(source_tree), declarations(target_tree)
        # The library also owns local file/clock helpers; the embedded codec is
        # deliberately a subset, with no additional or differing declarations.
        self.assertLessEqual(embedded.keys(), source.keys())
        for name, declaration in embedded.items():
            self.assertEqual(declaration, source[name], name)
        namespace = {'json': json, 're': __import__('re')}
        exec(codec, namespace)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            valid = json.loads(path.read_text())
        cases = [(valid, True)]
        for key, value in [('sha', 'a' * 39), ('schema', True), ('operation', 'newline\ninjection'),
                           ('tag', 'v00.2.0'), ('main_ci', None), ('attempts', {}), ('decisions', 'bad'),
                           ('truncated', 1), ('state', 'published-and-verified'), ('reason', 'raw stderr'),
                           ('extra', 'unknown')]:
            changed = copy.deepcopy(valid)
            changed[key] = value
            cases.append((changed, False))
        for data, expected in cases:
            outcomes = []
            for decoder in (journal.decode, namespace['decode']):
                try:
                    decoder(json.dumps(data))
                    outcomes.append(True)
                except (ValueError, TypeError, KeyError, AttributeError):
                    outcomes.append(False)
            self.assertEqual(outcomes, [expected, expected])
        for raw in ['x' * 32769, '{"schema":1,"schema":1}', '{bad']:
            for decoder in (journal.decode, namespace['decode']):
                with self.assertRaises((ValueError, TypeError, KeyError, AttributeError)):
                    decoder(raw)
        oversized = json.dumps(valid) + ' ' * (32769 - len(json.dumps(valid)))
        for decoder in (journal.decode, namespace['decode']):
            with self.assertRaises(ValueError):
                decoder(oversized)

    def test_attempt_and_first_failure_survive_round_trip(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            record.observe('release.yml', dict(id=101, run_attempt=1, status='completed', conclusion='failure'),
                           [dict(id=303, name='publish-api', status='completed', conclusion='failure',
                                 steps=[dict(number=2, name='Publish image', status='completed', conclusion='failure')])])
            record.decision('release.yml', 101, 1, 'publication-role', 2, 1, 'reserved')
            data = journal.read(path)
            self.assertEqual(data['attempts'][0]['jobs'][0]['steps'][0]['name'], 'Publish image')
            self.assertEqual(data['attempts'][0]['jobs'][0]['id'], 303)
            self.assertEqual(data['decisions'][0]['before'], 2)
            self.assertEqual(data['decisions'][0]['after'], 1)
            self.assertEqual(data['decisions'][0]['state'], 'reserved')

    def test_byte_bound_keeps_every_attempt_and_omission_counts(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            for attempt in range(1, 8):
                record.observe('release.yml', dict(id=101, run_attempt=attempt, status='completed', conclusion='failure'),
                               [dict(id=job + 1, name='x' * 1000, status='completed', conclusion='failure',
                                     steps=[dict(number=step + 1, name='y' * 1000, status='completed', conclusion='failure')
                                            for step in range(4)]) for job in range(8)])
            self.assertLessEqual(path.stat().st_size, 32768)
            data = journal.read(path)
            self.assertEqual([item['attempt'] for item in data['attempts']], list(range(1, 8)))
            self.assertTrue(data['truncated'])
            self.assertTrue(any(item['jobs_omitted'] or any(job['steps_omitted'] for job in item['jobs'])
                                for item in data['attempts']))

    def test_main_ci_is_report_only_and_never_a_retry_decision(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            record.main_ci('failed')
            data = journal.read(path)
            self.assertEqual(data['main_ci'], {'policy': 'report-only', 'state': 'failed'})
            self.assertEqual(data['decisions'], [])

    def test_no_unvalidated_labels_or_raw_command_prose(self):
        journal = module()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            record = journal.Journal(path, 'fixture-operation', 'vtmocanu/uzi', 'v0.2.0', 'a' * 40)
            with self.assertRaises(ValueError):
                record.observe('release.yml', dict(id=True, run_attempt=1, status='completed', conclusion='failure'), [])
            with self.assertRaises(ValueError):
                record.main_ci('raw stderr or arbitrary commit message')
            record.main_ci('unavailable')
            self.assertNotIn('stderr', path.read_text())
            broken = json.loads(path.read_text())
            broken['extra'] = 'untrusted'
            path.write_text(json.dumps(broken))
            with self.assertRaises(ValueError):
                journal.read(path)


if __name__ == '__main__':
    unittest.main()
