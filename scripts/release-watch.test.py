#!/usr/bin/env python3
"""Offline transcript parity with the pre-port publication watcher."""
import json
import importlib.util
import contextlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch


ROOT = Path(__file__).resolve().parent.parent
LEGACY = ROOT / 'scripts/fixtures/release-watch-legacy.txt'
ENGINE = ROOT / 'scripts/release-watch.py'


def engine():
    spec = importlib.util.spec_from_file_location('release_watch', ENGINE)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


def resolve(workflow, run):
    return [f'run list --workflow {workflow} --branch v0.2.0 --limit 1 --json databaseId --jq .[0].databaseId // empty', str(run), 0]


def jobs(run, rows, status=0):
    return [f'run view {run} --json jobs --jq .jobs[] | [.status, (.conclusion // ""), .name, .url] | @tsv', rows, status]


def row(name='publish-api', conclusion='success', status='completed'):
    return f'{status}\t{conclusion}\t{name}\thttps://example.invalid/job\n'


class Parity(unittest.TestCase):
    def run_case(self, calls, ticks=1, reruns=2, restored=(), wrapper=False, combined=False,
                 journal_calls=None, journal_warnings=()):
        results = []
        runner = ('import importlib.util,subprocess,sys; '
                  'spec=importlib.util.spec_from_file_location("watch_fixture",sys.argv[1]); '
                  'module=importlib.util.module_from_spec(spec); spec.loader.exec_module(module); '
                  'fake_sleep=lambda seconds: subprocess.run(["sleep",str(seconds)],check=True); '
                  'init=module.Watcher.__init__; init.__defaults__=init.__defaults__[:-1]+(fake_sleep,); '
                  'sys.argv=sys.argv[1:]; sys.exit(module.main())')
        commands = [['bash', str(LEGACY)], [sys.executable, '-I', '-c', runner, str(ENGINE)]]
        if wrapper:
            commands.append(['bash', str(ROOT / '.agents/skills/uzi-release/scripts/release-watch.sh')])
        for index, command in enumerate(commands):
            with tempfile.TemporaryDirectory(prefix='release-watch-parity-') as directory:
                directory = Path(directory)
                expected_calls = journal_calls if index and journal_calls is not None else calls
                (directory / 'calls.json').write_text(json.dumps(expected_calls))
                gh = directory / 'gh'
                gh.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
p = pathlib.Path(os.environ['WATCH_FIXTURE'])
calls = json.loads((p / 'calls.json').read_text())
index = int((p / 'index').read_text()) if (p / 'index').exists() else 0
actual = ' '.join(sys.argv[1:])
with (p / 'transcript').open('a') as out: out.write(actual + '\\n')
if index >= len(calls) or actual != calls[index][0]:
    with (p / 'mismatch').open('a') as out: out.write(repr((index, actual)) + '\\n')
    sys.exit(99)
(p / 'index').write_text(str(index + 1))
print(calls[index][1], end='')
sys.exit(calls[index][2])
''')
                gh.chmod(0o700)
                sleep = directory / 'sleep'
                sleep.write_text('#!/bin/sh\nprintf "sleep %s\\n" "$1" >> "$WATCH_FIXTURE/transcript"\n')
                sleep.chmod(0o700)
                env = dict(os.environ, WATCH_FIXTURE=str(directory), GITHUB_ACTIONS='false', GIT_CONFIG_GLOBAL='/dev/null',
                           GIT_CONFIG_SYSTEM='/dev/null', PATH=str(directory) + os.pathsep + os.environ['PATH'])
                args = ['0.2.0', '--max-ticks', str(ticks), '--interval', '0', '--max-reruns', str(reruns)]
                if index and journal_calls is not None:
                    args += ['--journal', str(directory / 'journal.json'), '--operation', 'fixture',
                             '--repository', 'vtmocanu/uzi', '--sha', 'a' * 40, '--kind', 'stable']
                result = subprocess.run(command + args, cwd=ROOT, env=env, text=True, stdout=subprocess.PIPE,
                                        stderr=subprocess.STDOUT if combined else subprocess.PIPE, timeout=30)
                error = result.stderr or ''
                self.assertFalse((directory / 'mismatch').exists(), result.stdout + error)
                consumed = int((directory / 'index').read_text()) if (directory / 'index').exists() else 0
                self.assertEqual(consumed, len(expected_calls), result.stdout + error)
                transcript = (directory / 'transcript').read_text() if (directory / 'transcript').exists() else ''
                results.append((result.returncode, result.stdout, error, transcript))
                if index and journal_calls is not None:
                    self.journal_result = json.loads((directory / 'journal.json').read_text())
        # Explicitly restore only the known omitted first row. Everything else,
        # including later detail rows, remains byte-for-byte transcript parity.
        status, output, error, transcript = results[1]
        if journal_calls is not None:
            actual_api = [line for line in transcript.splitlines() if line.startswith('api ')]
            self.assertEqual(actual_api, [call[0] for call in journal_calls if call[0].startswith('api ')])
            transcript = ''.join(line + '\n' for line in transcript.splitlines() if not line.startswith('api '))
            for warning in journal_warnings:
                target = output if combined else error
                self.assertIn(warning + '\n', target)
                if combined:
                    output = output.replace(warning + '\n', '', 1)
                else:
                    error = error.replace(warning + '\n', '', 1)
        for name, retry in restored:
            line = f'  {"rerun <-" if retry else "FAIL "} {name:<28} https://example.invalid/job\n'
            self.assertIn(line, output)
            output = output.replace(line, '', 1)
        self.assertEqual(results[0], (status, output, error, transcript))
        self.legacy_result, self.engine_result = results[:2]
        if wrapper:
            self.assertEqual(results[1], results[2])
        return results[0]

    def test_green(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row()),
                                resolve('brew.yml', 202), jobs(202, row('publish-formula'))])
        self.assertEqual(result[0], 0)

    def test_combined_stream_keeps_banner_before_stderr_progress(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, 'unavailable', 1),
                                resolve('brew.yml', 202), jobs(202, row())], combined=True)
        self.assertTrue(result[1].startswith('release-watch:'))

    def api_run(self, workflow, run, status='completed', conclusion='success', error=False):
        data = dict(id=run, run_attempt=1, head_sha='a' * 40, head_branch='v0.2.0', event='push',
                    path=f'.github/workflows/{workflow}', status=status, conclusion=conclusion,
                    repository={'full_name': 'vtmocanu/uzi'}, head_repository={'full_name': 'vtmocanu/uzi'})
        return [f'api repos/vtmocanu/uzi/actions/runs/{run}', 'HTTP 502' if error else json.dumps(data), int(error)]

    def api_jobs(self, run, name='publish-api', conclusion='success', error=False):
        data = [dict(total_count=1, jobs=[dict(id=run + 1000, run_id=run, name=name, status='completed',
                    conclusion=conclusion, html_url=f'https://github.com/vtmocanu/uzi/actions/runs/{run}/job/{run + 1000}', steps=[])])]
        return [f'api --paginate --slurp repos/vtmocanu/uzi/actions/runs/{run}/attempts/1/jobs?per_page=100',
                'HTTP 502' if error else json.dumps(data), int(error)]

    def test_journal_transient_read_preserves_legacy_wait_and_success(self):
        warning = 'release-watch: release.yml journal evidence unavailable; continuing publication observation'
        for endpoint in ('run', 'jobs'):
            with self.subTest(endpoint=endpoint):
                first = [resolve('release.yml', 101), jobs(101, row(status='in_progress', conclusion=''))]
                brew = [resolve('brew.yml', 202), jobs(202, row('publish-formula'))]
                second = [resolve('release.yml', 101), jobs(101, row())]
                calls = first + brew + second + brew
                details = [self.api_run('release.yml', 101, status='in_progress', conclusion=None, error=endpoint == 'run')]
                if endpoint == 'jobs':
                    details += [self.api_jobs(101, error=True)]
                enriched = (first + details + brew + [self.api_run('brew.yml', 202), self.api_jobs(202, 'publish-formula')]
                            + second + [self.api_run('release.yml', 101), self.api_jobs(101)]
                            + brew + [self.api_run('brew.yml', 202)])
                self.assertEqual(self.run_case(calls, ticks=2, journal_calls=enriched, journal_warnings=[warning])[0], 0)
                self.assertEqual(self.journal_result['publication_read_errors']['release.yml'], 1)
                self.assertEqual(self.journal_result['state'], 'workflows-green')

    def test_journal_read_miss_cannot_hide_gate_rows_or_change_refusal(self):
        calls = [resolve('release.yml', 101), jobs(101, row('assert-version', 'failure')),
                 jobs(101, row('assert-version', 'failure'))]
        enriched = calls[:2] + [self.api_run('release.yml', 101, error=True)] + calls[2:] + [self.api_run('release.yml', 101, error=True)]
        warning = 'release-watch: release.yml journal evidence unavailable; continuing publication observation'
        result = self.run_case(calls, journal_calls=enriched, journal_warnings=[warning, warning],
                               restored=[('assert-version', False)])
        self.assertEqual(result[0], 1)
        self.assertIn('  FAIL  assert-version', self.engine_result[1])
        self.assertEqual(self.journal_result['reason'], 'gate-refused')

    def test_pending_has_priority_over_failed_gate(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row('assert-version', 'failure') + row(status='in_progress')),
                                resolve('brew.yml', 202), jobs(202, row())])
        self.assertEqual(result[0], 2)
        self.assertNotIn('run rerun', result[3])

    def test_gate_stops_before_second_workflow(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row('assert-tag-smoke', 'failure')),
                                jobs(101, row('assert-tag-smoke', 'failure'))], restored=[('assert-tag-smoke', False)])
        self.assertEqual(result[0], 1)

    def test_brew_gate_names_manual_recovery(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row()), resolve('brew.yml', 202),
                                jobs(202, row('assert-release-published', 'failure')),
                                jobs(202, row('assert-release-published', 'failure'))], restored=[('assert-release-published', False)])
        self.assertEqual(result[0], 1)
        self.assertIn('gh run rerun 202 --failed', result[2])

    def test_stale_failure_requery_clears(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row(conclusion='failure')), jobs(101, row()),
                                resolve('brew.yml', 202), jobs(202, row())])
        self.assertEqual(result[0], 2)

    def test_publication_rerun_and_requeue(self):
        calls = [resolve('release.yml', 101), jobs(101, row(conclusion='failure')), jobs(101, row(conclusion='failure')),
                 ['run rerun 101 --failed', '', 0], ['run view 101 --json status --jq .status', 'queued', 0],
                 resolve('brew.yml', 202), jobs(202, row()), resolve('release.yml', 101), jobs(101, row()),
                 resolve('brew.yml', 202), jobs(202, row())]
        self.assertEqual(self.run_case(calls, ticks=2, restored=[('publish-api', True)])[0], 0)

    def test_exhausted_budget(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row(conclusion='failure')),
                                jobs(101, row(conclusion='failure'))], reruns=0, restored=[('publish-api', False)])
        self.assertEqual(result[0], 1)

    def test_rerun_request_failure(self):
        result = self.run_case([resolve('release.yml', 101), jobs(101, row(conclusion='failure')),
                                jobs(101, row(conclusion='failure')), ['run rerun 101 --failed', '', 1]], restored=[('publish-api', True)])
        self.assertEqual(result[0], 1)

    def test_missing_run_and_empty_jobs_wait(self):
        result = self.run_case([resolve('release.yml', ''), resolve('brew.yml', 202), jobs(202, '')])
        self.assertEqual(result[0], 2)

    def test_budget_is_per_workflow(self):
        calls = []
        for workflow, run in [('release.yml', 101), ('brew.yml', 202)]:
            calls += [resolve(workflow, run), jobs(run, row(conclusion='failure')),
                      jobs(run, row(conclusion='failure')), [f'run rerun {run} --failed', '', 0],
                      [f'run view {run} --json status --jq .status', 'queued', 0]]
        for workflow, run in [('release.yml', 101), ('brew.yml', 202)]:
            calls += [resolve(workflow, run), jobs(run, row())]
        self.assertEqual(self.run_case(calls, ticks=2, reruns=1, restored=[('publish-api', True)] * 2)[0], 0)

    def test_budget_does_not_reset_when_run_id_changes(self):
        calls = [resolve('release.yml', 101), jobs(101, row(conclusion='failure')),
                 jobs(101, row(conclusion='failure')), ['run rerun 101 --failed', '', 0],
                 ['run view 101 --json status --jq .status', 'queued', 0],
                 resolve('brew.yml', 202), jobs(202, row()), resolve('release.yml', 303),
                 jobs(303, row(conclusion='failure')), jobs(303, row(conclusion='failure'))]
        self.assertEqual(self.run_case(calls, ticks=2, reruns=1,
                                       restored=[('publish-api', True), ('publish-api', False)])[0], 1)

    def test_requeue_observation_is_six_reads_without_new_rerun(self):
        calls = [resolve('release.yml', 101), jobs(101, row(conclusion='failure')),
                 jobs(101, row(conclusion='failure')), ['run rerun 101 --failed', '', 0]]
        calls += [['run view 101 --json status --jq .status', 'completed', 0]] * 6
        calls += [resolve('brew.yml', 202), jobs(202, row())]
        result = self.run_case(calls, restored=[('publish-api', True)])
        self.assertEqual(result[0], 2)
        self.assertEqual(result[3].count('sleep 10\n'), 6)
        self.assertEqual(result[3].count('run rerun'), 1)

    def test_publication_role_policy_is_signature_independent(self):
        for conclusion in ['failure', 'cancelled', 'timed_out', 'action_required', 'startup_failure']:
            with self.subTest(conclusion=conclusion):
                calls = [resolve('release.yml', 101), jobs(101, row('publish-chart', conclusion)),
                         jobs(101, row('publish-chart', conclusion)), ['run rerun 101 --failed', '', 0],
                         ['run view 101 --json status --jq .status', 'queued', 0],
                         resolve('brew.yml', 202), jobs(202, row())]
                self.assertEqual(self.run_case(calls, restored=[('publish-chart', True)])[0], 2)

    def test_legacy_unknown_completed_conclusion_is_not_a_new_policy(self):
        # PR2 freezes the old publication decision policy. New CI/KinD recovery
        # eligibility is separate and does not gain authority from this behavior.
        for conclusion in ['skipped', 'neutral', '']:
            with self.subTest(conclusion=conclusion):
                self.assertEqual(self.run_case([resolve('release.yml', 101), jobs(101, row(conclusion=conclusion)),
                                                resolve('brew.yml', 202), jobs(202, row())])[0], 0)

    def test_three_uninterrupted_job_read_errors_exit_three(self):
        calls = [resolve('release.yml', 101), jobs(101, 'unavailable', 1),
                 resolve('brew.yml', 202), jobs(202, 'unavailable', 1),
                 resolve('release.yml', 101), jobs(101, 'unavailable', 1)]
        self.assertEqual(self.run_case(calls, ticks=2)[0], 3)

    def test_known_jobs_clear_error_counter(self):
        calls = []
        for _ in range(3):
            calls += [resolve('release.yml', 101), jobs(101, 'unavailable', 1),
                      resolve('brew.yml', 202), jobs(202, row())]
        self.assertEqual(self.run_case(calls, ticks=3)[0], 2)

    def test_stale_requery_changes_failure_to_gate(self):
        calls = [resolve('release.yml', 101), jobs(101, row(conclusion='failure')),
                 jobs(101, row('assert-changelog', 'failure'))]
        self.assertEqual(self.run_case(calls, restored=[('assert-changelog', False)])[0], 1)

    def test_first_failed_job_detail_is_retained(self):
        rows = row('assert-version', 'failure') + row('publish-api', 'failure')
        self.run_case([resolve('release.yml', 101), jobs(101, rows), jobs(101, rows)],
                      restored=[('assert-version', False)])
        subject = self.legacy_result if os.environ.get('WATCH_TEST_OLD_ROWS') == '1' else self.engine_result
        self.assertIn('  FAIL  assert-version', subject[1])
        self.assertIn('  FAIL  publish-api', subject[1])

    def test_compatibility_wrapper_delegates_the_same_engine(self):
        rows = row('assert-version', 'failure')
        self.run_case([resolve('release.yml', 101), jobs(101, rows), jobs(101, rows)],
                      restored=[('assert-version', False)], wrapper=True)


class JournalAdmission(unittest.TestCase):
    def test_terminal_journal_failure_preserves_primary_result(self):
        for primary in (0, 1):
            with self.subTest(primary=primary):
                module = engine()
                watch = module.Watcher('0.2.0', sleep=lambda _: None)
                watch.poll = Mock(return_value=primary)
                watch.journal = Mock()
                watch.journal.finish.side_effect = OSError('fixture finish failure')
                watch.classify_main = Mock()
                errors = io.StringIO()
                with contextlib.redirect_stderr(errors):
                    try:
                        result = watch.run()
                    except OSError:
                        self.fail('terminal journal write replaced the primary publication result')
                self.assertEqual(result, primary)
                self.assertEqual(errors.getvalue().splitlines(), ['release-watch: terminal journal unavailable'])
                watch.classify_main.assert_not_called()

    def test_unknown_run_conclusion_preserves_latch_until_known_result(self):
        for conclusion in (None, ''):
            with self.subTest(conclusion=conclusion), tempfile.TemporaryDirectory() as directory:
                module = engine()
                watch = module.Watcher('0.2.0', sleep=lambda _: None)
                watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
                path = Path(directory) / 'journal.json'
                watch.journal = module.journals.Journal(path, 'fixture', watch.repository, watch.tag, watch.sha)
                watch.requested['release.yml'], watch.remaining['release.yml'] = (101, 1), 1
                run = dict(self.valid_run(), conclusion=conclusion)
                job = dict(self.valid_job(), conclusion='success', steps=[])
                success = (0, json.dumps(dict(run, conclusion='success')), '')
                watch.gh = Mock(side_effect=[(0, json.dumps(run), ''),
                                             (0, json.dumps([dict(total_count=1, jobs=[job])]), ''), success, success])
                watch.record('release.yml', '101')
                self.assertIn('release.yml', watch.requested)
                self.assertEqual(module.journals.read(path)['decisions'], [])
                watch.record('release.yml', '101')
                watch.record('release.yml', '101')
                self.assertEqual([row['state'] for row in module.journals.read(path)['decisions']], ['passed-on-retry'])
                self.assertEqual(watch.remaining['release.yml'], 1)
                self.assertNotIn('release.yml', watch.requested)

    def run_main_with_observer_fault(self, primary, field=None, value=None, storage_error=None):
        module = engine()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            watch = module.Watcher('0.2.0-rc.1', sleep=lambda _: None)
            watch.poll = Mock(return_value=primary)
            main_api = Mock()
            observed_states = []
            def observe_main(*_):
                observed_states.append(module.journals.read(path)['state'])
                return None if storage_error else {'id': 101}
            main_api.run.side_effect = observe_main
            metadata = dict(self.valid_run(), head_branch='main', path='.github/workflows/ci.yml')
            if field is not None:
                metadata[field] = value
            watch.gh = Mock(return_value=(0, json.dumps(metadata), ''))
            args = ['release-watch.py', '0.2.0-rc.1', '--journal', str(path), '--operation', 'fixture',
                    '--repository', 'vtmocanu/uzi', '--sha', 'a' * 40, '--kind', 'rc']
            original_save = module.journals.Journal.save
            def save(record):
                if storage_error and record.data['main_ci']['state'] != 'unavailable':
                    raise storage_error('fixture observation save failure')
                return original_save(record)
            with (patch.object(module.sys, 'argv', args), patch.object(module, 'Watcher', return_value=watch),
                  patch.object(module.evidence, 'GitHub', return_value=main_api),
                  patch.object(module.subprocess, 'run', return_value=Mock(returncode=0)),
                  patch.object(module.journals.Journal, 'save', save),
                  contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO())):
                try:
                    actual = module.main()
                except Exception as error:
                    self.fail(f'report-only observation escaped: {type(error).__name__}')
            self.assertEqual(actual, primary)
            self.assertEqual(observed_states, ['workflows-green' if primary == 0 else 'blocked'])
            data = module.journals.read(path)
            self.assertEqual(data['state'], 'workflows-green' if primary == 0 else 'blocked')
            self.assertEqual(data['reason'], 'publication-workflows-green' if primary == 0 else 'publication-refused')
            self.assertEqual(data['main_ci'], {'policy': 'report-only', 'state': 'unavailable'})
            main_api.rerun.assert_not_called()

    def test_malformed_main_ci_never_overrides_publication_or_finalization(self):
        for primary in (0, 1):
            for field, value in [('path', None), ('repository', None), ('head_repository', [])]:
                with self.subTest(primary=primary, field=field):
                    self.run_main_with_observer_fault(primary, field, value)

    def test_main_ci_storage_failure_never_overrides_publication_or_finalization(self):
        for primary in (0, 1):
            for error in (OSError, ValueError):
                with self.subTest(primary=primary, storage_error=error):
                    self.run_main_with_observer_fault(primary, storage_error=error)

    def test_gh_call_has_no_new_timeout(self):
        module = engine()
        with patch.object(module.subprocess, 'run', return_value=Mock(returncode=0, stdout='', stderr='')) as invoke:
            module.Watcher('0.2.0').gh('run', 'list')
        self.assertNotIn('timeout', invoke.call_args.kwargs)

    def test_incomplete_retry_snapshot_keeps_latch_until_one_conclusive_result(self):
        for missing in ('empty', 'in_progress', 'unknown_conclusion'):
            with self.subTest(missing=missing), tempfile.TemporaryDirectory() as directory:
                module = engine()
                watch = module.Watcher('0.2.0', sleep=lambda _: None)
                watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
                path = Path(directory) / 'journal.json'
                watch.journal = module.journals.Journal(path, 'fixture', watch.repository, watch.tag, watch.sha)
                watch.requested['release.yml'], watch.remaining['release.yml'] = (101, 1), 1
                run = dict(self.valid_run(), conclusion='success')
                complete = dict(self.valid_job(), conclusion='success', steps=[])
                incomplete = [] if missing == 'empty' else [dict(complete,
                    status='in_progress' if missing == 'in_progress' else 'completed', conclusion=None)]
                metadata = (0, json.dumps(run), '')
                watch.gh = Mock(side_effect=[metadata, (0, json.dumps([dict(total_count=len(incomplete), jobs=incomplete)]), ''),
                                             metadata, (0, json.dumps([dict(total_count=1, jobs=[complete])]), ''), metadata])
                watch.record('release.yml', '101')
                self.assertIn('release.yml', watch.requested)
                self.assertEqual(module.journals.read(path)['decisions'], [])
                watch.record('release.yml', '101')
                self.assertNotIn('release.yml', watch.requested)
                self.assertEqual([row['state'] for row in module.journals.read(path)['decisions']], ['passed-on-retry'])
                self.assertEqual(watch.remaining['release.yml'], 1)
                watch.record('release.yml', '101')
                self.assertEqual([row['state'] for row in module.journals.read(path)['decisions']], ['passed-on-retry'])
                self.assertEqual(watch.gh.call_count, 5)
                self.assertTrue(all(call.args[0] == 'api' for call in watch.gh.call_args_list))

    def valid_run(self):
        return dict(id=101, run_attempt=2, head_sha='a' * 40, head_branch='v0.2.0', event='push',
                    path='.github/workflows/release.yml', status='completed', conclusion='failure',
                    repository={'full_name': 'vtmocanu/uzi'}, head_repository={'full_name': 'vtmocanu/uzi'})

    def valid_job(self, identity=303):
        return dict(id=identity, run_id=101, name='publish-api', status='completed', conclusion='failure',
                    html_url=f'https://github.com/vtmocanu/uzi/actions/runs/101/job/{identity}',
                    steps=[dict(number=2, name='Publish image', status='completed', conclusion='failure')])

    def failure_watcher(self):
        watch = engine().Watcher('0.2.0', max_ticks=1, interval=0, sleep=lambda _: None)
        watch.journal = Mock()
        watch.resolve = Mock(side_effect=['101', ''])
        job = self.valid_job()
        watch.dump = Mock(return_value=(0, f"completed\tfailure\tpublish-api\t{job['html_url']}\n"))
        watch.record = Mock(return_value=(self.valid_run(), [job]))
        watch.classify_main = Mock()
        watch.wait_requeued = Mock()
        watch.gh = Mock(return_value=(0, '', ''))
        return watch

    def test_journal_reservation_failure_prevents_remote_write(self):
        watch = self.failure_watcher()
        watch.journal.decision.side_effect = OSError('fixture disk failure')
        self.assertEqual(watch.run(), 3)
        watch.gh.assert_not_called()
        self.assertEqual(watch.remaining['release.yml'], 1)

    def test_changed_failure_set_prevents_remote_write(self):
        watch = self.failure_watcher()
        watch.record.return_value = self.valid_run(), [self.valid_job(), dict(self.valid_job(304), name='assert-version')]
        self.assertEqual(watch.run(), 3)
        watch.gh.assert_not_called()
        self.assertEqual(watch.remaining['release.yml'], 2)
        self.assertEqual(watch.record.call_args.kwargs, {'fresh': True})

    def test_unknown_gate_conclusion_is_incomplete_prewrite_evidence(self):
        watch = self.failure_watcher()
        unknown = dict(self.valid_job(304), name='assert-version', conclusion=None, steps=[])
        watch.record.return_value = self.valid_run(), [self.valid_job(), unknown]
        result = watch.run()
        watch.gh.assert_not_called()
        self.assertEqual(result, 3)
        self.assertEqual(watch.remaining['release.yml'], 2)

    def test_reservation_is_durable_before_request_and_uncertain_is_not_refunded(self):
        watch = self.failure_watcher()
        def request(*args):
            self.assertEqual(args, ('run', 'rerun', '101', '--failed'))
            self.assertEqual(watch.remaining['release.yml'], 1)
            self.assertIsNotNone(watch.journal.decision.call_args)
            self.assertEqual(watch.journal.decision.call_args.args[-1], 'reserved')
            return 1, '', ''
        watch.gh.side_effect = request
        self.assertEqual(watch.run(), 1)
        self.assertEqual(watch.remaining['release.yml'], 1)
        self.assertEqual([call.args[-1] for call in watch.journal.decision.call_args_list], ['reserved', 'uncertain'])

    def test_retry_result_requires_current_attempt_jobs_and_records_once(self):
        for conclusion, expected in [('success', 'passed-on-retry'), ('failure', 'failed-on-retry')]:
            with self.subTest(job_conclusion=conclusion):
                watch = engine().Watcher('0.2.0', sleep=lambda _: None)
                watch.journal = Mock()
                watch.remaining['release.yml'] = 1
                watch.requested['release.yml'] = 101, 1
                watch.snapshot = Mock(return_value=(dict(self.valid_run(), conclusion='success'),
                                                    [dict(self.valid_job(), conclusion=conclusion)]))
                watch.record('release.yml', '101')
                watch.record('release.yml', '101')
                self.assertEqual(watch.journal.decision.call_count, 1)
                self.assertEqual(watch.journal.decision.call_args.args[-1], expected)
                self.assertEqual(watch.remaining['release.yml'], 1)

    def test_snapshot_checks_all_pages_and_pinned_identity_before_a_write(self):
        watch = engine().Watcher('0.2.0', sleep=lambda _: None)
        watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
        run = dict(self.valid_run(), head_sha='b' * 40)
        watch.gh = Mock(return_value=(0, json.dumps(run), ''))
        with self.assertRaises(ValueError):
            watch.snapshot('release.yml', '101')
        self.assertEqual(watch.gh.call_count, 1)

    def test_complete_pages_include_a_later_gate_failure(self):
        watch = engine().Watcher('0.2.0', sleep=lambda _: None)
        watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
        second = dict(self.valid_job(304), name='assert-version')
        watch.gh = Mock(side_effect=[(0, json.dumps(self.valid_run()), ''),
                                     (0, json.dumps([dict(total_count=2, jobs=[self.valid_job()]),
                                                     dict(total_count=2, jobs=[second])]), '')])
        _, jobs = watch.snapshot('release.yml', '101')
        self.assertEqual([job['name'] for job in jobs], ['publish-api', 'assert-version'])
        self.assertTrue(engine().gate([(job['name'], job['html_url']) for job in jobs]))

    def test_existing_brew_tag_dispatch_remains_observable(self):
        watch = engine().Watcher('0.2.0', sleep=lambda _: None)
        watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
        run = dict(self.valid_run(), event='workflow_dispatch', path='.github/workflows/brew.yml')
        watch.gh = Mock(side_effect=[(0, json.dumps(run), ''),
                                     (0, json.dumps([dict(total_count=1, jobs=[self.valid_job()])]), '')])
        try:
            observed, _ = watch.snapshot('brew.yml', '101')
        except ValueError as error:
            self.fail(f'existing protected-tag Brew dispatch was refused: {error}')
        self.assertEqual(observed['id'], 101)

    def test_terminal_job_cache_never_substitutes_for_prewrite_revalidation(self):
        watch = engine().Watcher('0.2.0', sleep=lambda _: None)
        watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
        metadata = (0, json.dumps(self.valid_run()), '')
        jobs = (0, json.dumps([dict(total_count=1, jobs=[self.valid_job()])]), '')
        watch.gh = Mock(side_effect=[metadata, jobs, metadata, metadata, jobs])
        watch.snapshot('release.yml', '101')
        watch.snapshot('release.yml', '101')
        self.assertEqual(watch.gh.call_count, 3)
        watch.snapshot('release.yml', '101', fresh=True)
        self.assertEqual(watch.gh.call_count, 5)

    def test_snapshot_rejects_each_wrong_identity_field(self):
        cases = {'id': 102, 'head_sha': 'b' * 40, 'head_branch': 'main', 'event': 'pull_request',
                 'path': '.github/workflows/ci.yml', 'run_attempt': True,
                 'repository': {'full_name': 'other/repository'},
                 'head_repository': {'full_name': 'other/repository'}}
        for key, value in cases.items():
            with self.subTest(field=key):
                watch = engine().Watcher('0.2.0', sleep=lambda _: None)
                watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
                watch.gh = Mock(side_effect=[(0, json.dumps(dict(self.valid_run(), **{key: value})), ''),
                                             (0, json.dumps([dict(total_count=1, jobs=[self.valid_job()])]), '')])
                with self.assertRaises(ValueError):
                    watch.snapshot('release.yml', '101')
                self.assertEqual(watch.gh.call_count, 1)

    def test_incomplete_duplicate_or_invalid_second_page_refuses(self):
        for pages in ([dict(total_count=2, jobs=[self.valid_job()])],
                      [dict(total_count=2, jobs=[self.valid_job(), self.valid_job()])],
                      [dict(total_count=2, jobs=[self.valid_job(), dict(self.valid_job(304), status='unknown')])]):
            with self.subTest(pages=pages):
                watch = engine().Watcher('0.2.0', sleep=lambda _: None)
                watch.repository, watch.sha = 'vtmocanu/uzi', 'a' * 40
                watch.gh = Mock(side_effect=[(0, json.dumps(self.valid_run()), ''), (0, json.dumps(pages), '')])
                with self.assertRaises(ValueError):
                    watch.snapshot('release.yml', '101')

    def test_report_only_main_ci_api_failure_is_unavailable(self):
        watch = engine().Watcher('0.2.0', sleep=Mock(side_effect=AssertionError('no wait')))
        watch.kind, watch.sha = 'rc', 'a' * 40
        watch.journal, watch.main_api = Mock(), Mock()
        watch.main_api.run.side_effect = ValueError('unreadable')
        watch.classify_main()
        watch.journal.main_ci.assert_called_once_with('unavailable')
        watch.main_api.rerun.assert_not_called()


    def test_main_ci_classification_never_reruns_or_waits(self):
        watch = engine().Watcher('0.2.0', sleep=Mock(side_effect=AssertionError('no wait')))
        watch.repository, watch.sha, watch.kind = 'vtmocanu/uzi', 'a' * 40, 'rc'
        watch.journal = Mock()
        watch.main_api = Mock()
        watch.main_api.run.return_value = dict(id=101, run_attempt=1, status='completed', conclusion='failure')
        watch.snapshot = Mock(return_value=(watch.main_api.run.return_value, []))
        watch.classify_main()
        watch.journal.main_ci.assert_called_once_with('failed')
        watch.main_api.run.assert_called_once_with('ci.yml', 'a' * 40, 'main')
        watch.main_api.rerun.assert_not_called()
        watch.kind = 'stable'
        watch.main_api.reset_mock()
        watch.journal.reset_mock()
        watch.classify_main()
        watch.main_api.run.assert_not_called()
        watch.journal.main_ci.assert_called_once_with('not-applicable')

    def test_failed_main_ci_does_not_change_green_publication_exit(self):
        watch = engine().Watcher('0.2.0', sleep=Mock(side_effect=AssertionError('no wait')))
        watch.poll = Mock(return_value=0)
        watch.kind, watch.sha, watch.journal, watch.main_api = 'rc', 'a' * 40, Mock(), Mock()
        run = dict(id=101, run_attempt=1, status='completed', conclusion='failure')
        watch.main_api.run.return_value = run
        watch.snapshot = Mock(return_value=(run, []))
        self.assertEqual(watch.run(), 0)
        watch.journal.main_ci.assert_called_once_with('failed')
        watch.main_api.rerun.assert_not_called()


class CommandSafety(unittest.TestCase):
    def test_cli_rejects_abbreviations_and_retains_legacy_missing_value_status(self):
        cases = [(['0.2.0', '--int', '5'], 3), (['0.2.0', '--max-reruns'], 1),
                 (['0.2.0', '--interval'], 1), (['0.2.0', '--max-ticks'], 1)]
        for args, expected in cases:
            with self.subTest(args=args):
                module = engine()
                with (patch.object(module.sys, 'argv', ['release-watch.py', *args]),
                      patch.object(module.Watcher, 'run', return_value=0),
                      patch.object(module.subprocess, 'run', return_value=Mock(returncode=0)),
                      contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO())):
                    self.assertEqual(module.main(), expected)

    def test_stream_commands_are_disabled_and_resumed_on_success_and_failure(self):
        for broken in (False, True):
            with self.subTest(broken=broken), tempfile.TemporaryDirectory() as directory:
                module = engine()
                output, errors = io.StringIO(), io.StringIO()
                args = ['release-watch.py', '0.2.0', '--journal', str(Path(directory) / 'journal.json'),
                        '--operation', 'fixture-operation', '--repository', 'vtmocanu/uzi',
                        '--sha', 'a' * 40, '--kind', 'stable']
                runner = Mock(side_effect=ValueError('fixture failure')) if broken else Mock(return_value=0)
                with (patch.dict(os.environ, {'GITHUB_ACTIONS': 'true'}), patch.object(module.sys, 'argv', args),
                      patch.object(module.subprocess, 'run', return_value=Mock(returncode=0)),
                      patch.object(module.Watcher, 'run', runner), contextlib.redirect_stdout(output),
                      contextlib.redirect_stderr(errors)):
                    result = module.main()
                lines = output.getvalue().splitlines()
                self.assertEqual(len(lines), 2)
                self.assertTrue(lines[0].startswith('::stop-commands::'))
                token = lines[0].split('::')[-1]
                self.assertEqual(len(token), 32)
                self.assertEqual(lines[1], f'::{token}::')
                self.assertEqual(result, 3 if broken else 0)


if __name__ == '__main__':
    unittest.main()
