#!/usr/bin/env python3
"""Offline release dispatch/orchestration fixtures; no live API or publication."""
import importlib.util
import contextlib
import datetime as dt
import io
import json
import os
import re
from pathlib import Path
import tempfile
import unittest
import subprocess
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('release_auto', Path(__file__).with_name('release-auto.py'))
auto = importlib.util.module_from_spec(spec)
spec.loader.exec_module(auto)
H, NEW, CANDIDATE, RELEASE, TAG_OID = (char * 40 for char in '12345')
TAG = 'v0.2.0-rc.10'


def cut_result(path, tag, reason='ready', uncited=None):
    data = dict(schema=1, stage='complete' if reason == 'ready' else 'changelog-coverage',
                reason=reason, planned_tag=tag, uncited=uncited,
                uncited_count=len(uncited) if uncited is not None else None, uncited_truncated=False)
    Path(path).write_text(json.dumps(data))


class OracleDiagnostics(unittest.TestCase):
    def fixture(self, directory):
        repo = Path(directory) / 'repo'
        repo.mkdir()
        env = dict(os.environ, GIT_CONFIG_GLOBAL='/dev/null', GIT_CONFIG_SYSTEM='/dev/null')
        def git(*args):
            return subprocess.check_output(['git', '-c', 'core.fsmonitor=false', *args], cwd=repo, env=env, text=True, timeout=120).strip()
        git('init', '-q', '-b', 'main')
        git('config', 'user.name', 'fixture')
        git('config', 'user.email', 'fixture@example.invalid')
        (repo / 'CHANGELOG.md').write_text('## [Unreleased]\n\n## [0.2.0] - 2026-10-27\n### Changed\n- Existing note (#999).\n')
        (repo / 'api').mkdir()
        (repo / 'api/a.go').write_text('package fixture\n')
        git('add', '.')
        git('commit', '-qm', 'base')
        git('tag', 'v0.1.0')
        (repo / 'api/a.go').write_text('package fixture\n// changed\n')
        git('commit', '-qam', 'fix: uncited shipping change (#3486)')
        sha = git('rev-parse', 'HEAD')
        bin_dir = Path(directory) / 'bin'
        bin_dir.mkdir()
        gh = bin_dir / 'gh'
        gh.write_text('#!/bin/sh\nexit 1\n')
        gh.chmod(0o700)
        env['PATH'] = str(bin_dir) + os.pathsep + env['PATH']
        return repo, env, sha

    def test_unreadable_history_is_not_success(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, env, _ = self.fixture(directory)
            result = subprocess.run(['bash', str(Path(__file__).with_name('assert-changelog-covers-release.sh')),
                                     'HEAD', 'missing-baseline', '0.2.0'], cwd=repo, env=env,
                                    capture_output=True, text=True, timeout=120)
            self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
            self.assertIn('changelog-coverage-error', result.stderr)

    def test_refusal_emits_typed_public_identities(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, env, sha = self.fixture(directory)
            result_file = Path(directory) / 'result.json'
            result = subprocess.run(['bash', str(Path(__file__).with_name('assert-changelog-covers-release.sh')),
                                     'HEAD', 'v0.1.0', '0.2.0-rc.1', '--result-file', str(result_file)],
                                    cwd=repo, env=env, capture_output=True, text=True, timeout=120)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertTrue(result_file.exists(), 'oracle did not emit its structured refusal')
            data = json.loads(result_file.read_text())
            self.assertEqual(data['reason'], 'changelog-coverage-refused')
            self.assertEqual(data['planned_tag'], 'v0.2.0-rc.1')
            self.assertEqual(data['uncited'], [{'sha': sha, 'pr': 3486}])

    def test_history_error_emits_error_not_missing_coverage(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, env, _ = self.fixture(directory)
            result_file = Path(directory) / 'result.json'
            result = subprocess.run(['bash', str(Path(__file__).with_name('assert-changelog-covers-release.sh')),
                                     'HEAD', 'missing-baseline', '0.2.0', '--result-file', str(result_file)],
                                    cwd=repo, env=env, capture_output=True, text=True, timeout=120)
            self.assertEqual(result.returncode, 2)
            data = json.loads(result_file.read_text())
            self.assertEqual(data['reason'], 'changelog-coverage-error')
            self.assertIsNone(data['uncited'])

    def test_oracle_usage_and_result_delivery_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            repo, env, _ = self.fixture(directory)
            changelog = repo / 'CHANGELOG.md'
            changelog.write_text(changelog.read_text().replace('#999', '#3486'))
            subprocess.run(['git', '-C', str(repo), 'commit', '-qam', 'docs: cite the shipping commit'], env=env, check=True, timeout=120)
            oracle = ['bash', str(Path(__file__).with_name('assert-changelog-covers-release.sh')), 'HEAD', 'v0.1.0', '0.2.0']
            self.assertEqual(subprocess.run(oracle, cwd=repo, env=env, capture_output=True, timeout=120).returncode, 0)
            target = Path(directory) / 'output-directory'
            target.mkdir()
            self.assertEqual(subprocess.run(oracle + ['--result-file', str(target)], cwd=repo, env=env,
                                            capture_output=True, timeout=120).returncode, 1)
            self.assertEqual(subprocess.run(oracle + ['--result-file', 'one', '--result-file', 'two'],
                                            cwd=repo, env=env, capture_output=True, timeout=120).returncode, 2)


def context(**changes):
    env = dict(GITHUB_ACTIONS='true', GITHUB_REPOSITORY='vtmocanu/uzi', GITHUB_EVENT_NAME='workflow_dispatch', RELEASE_REF='refs/heads/main',
               RELEASE_ACTOR='maintainer', RELEASE_DISPATCHERS='other,maintainer third', RELEASE_KIND='rc',
               RELEASE_VERSION='', RELEASE_ABANDON='false', RELEASE_SUPERSEDE='', RELEASE_RC_TAG='',
               RELEASE_RC_SHA='', GITHUB_RUN_ID='123', GITHUB_SHA=H, GITHUB_RUN_ATTEMPT='1')
    env.update(changes)
    return env


def plan(sha=H, **changes):
    data = dict(OP='nextrc', BASE='0.2.0', TAG='v0.2.0-rc.11', REASON='ready', SHIPPING_COUNT='1',
                PREVIOUS_TAG=TAG, PUBLICATION_STATE='success', HEAD_SHA=sha)
    data.update(changes)
    return data


def rendered(data):
    return '\n'.join(f'{key}={value}' for key, value in data.items())


def run(status='completed', conclusion='success', number=10, attempt=1):
    return dict(id=number, run_attempt=attempt, status=status, conclusion=conclusion)


def job(step='Set up job', conclusion='failure'):
    return dict(id=20, name='validate-api', conclusion=conclusion, steps=[dict(name=step, conclusion=conclusion)])


class Inputs(unittest.TestCase):
    def test_rc_and_exact_dispatchers(self):
        self.assertEqual(auto.inputs(context())['kind'], 'rc')
        with self.assertRaisesRegex(auto.Stop, 'dispatcher-not-allowed'):
            auto.inputs(context(RELEASE_ACTOR='maintain'))
        with self.assertRaises(auto.Stop):
            auto.inputs(context(RELEASE_DISPATCHERS=''))

    def test_context_and_format_rejections(self):
        for key, value in (('GITHUB_REPOSITORY', 'other/uzi'), ('RELEASE_REF', 'refs/heads/feature'),
                           ('RELEASE_KIND', 'anything'), ('RELEASE_ABANDON', 'yes'),
                           ('RELEASE_VERSION', '0.02.0'), ('RELEASE_VERSION', '1.2.3;echo bad'),
                           ('RELEASE_SUPERSEDE', 'v0.2.0'), ('RELEASE_RC_TAG', TAG)):
            with self.subTest(key=key, value=value), self.assertRaises(auto.Stop):
                auto.inputs(context(**{key: value}))

    def test_abandon_and_stable_schema(self):
        with self.assertRaisesRegex(auto.Stop, 'abandon-needs-version'):
            auto.inputs(context(RELEASE_ABANDON='true'))
        self.assertTrue(auto.inputs(context(RELEASE_ABANDON='true', RELEASE_VERSION='0.3.0'))['abandon'])
        stable = context(RELEASE_KIND='stable', RELEASE_RC_TAG=TAG, RELEASE_RC_SHA=CANDIDATE)
        self.assertEqual(auto.inputs(stable)['rc_sha'], CANDIDATE)
        for key, value in (('RELEASE_VERSION', '0.3.0'), ('RELEASE_ABANDON', 'true'),
                           ('RELEASE_SUPERSEDE', TAG), ('RELEASE_RC_SHA', 'abc'), ('RELEASE_RC_TAG', 'main')):
            with self.subTest(key=key), self.assertRaises(auto.Stop):
                auto.inputs({**stable, key: value})

    def test_plan_protocol(self):
        self.assertEqual(auto.parse_plan(rendered(plan())), plan())
        for text in (rendered(plan()) + '\nOP=noop', rendered(plan()).replace('HEAD_SHA=', 'OTHER='),
                     rendered(plan(HEAD_SHA='short')), rendered(plan(OP='unknown'))):
            with self.subTest(text=text), self.assertRaises(auto.Stop):
                auto.parse_plan(text)


class Evidence(unittest.TestCase):
    def test_refs_numeric_order_and_peeling(self):
        text = f'{H}\trefs/heads/main\n{TAG_OID}\trefs/tags/{TAG}\n{CANDIDATE}\trefs/tags/{TAG}^{{}}\n'
        text += f'{NEW}\trefs/tags/v0.2.0-rc.9\n{H}\trefs/tags/v0.1.0\n'
        refs = auto.inventory(text)
        self.assertEqual(refs['previous_rc'], TAG)
        self.assertEqual(refs['previous_rc_sha'], CANDIDATE)
        self.assertEqual(refs['previous_stable'], 'v0.1.0')
        with self.assertRaises(auto.Stop):
            auto.inventory(text + f'{NEW}\trefs/heads/main\n')
        self.assertNotEqual(auto.inventory(text)['tags_digest'],
                            auto.inventory(text + f'{NEW}\trefs/tags/vlegacy\n')['tags_digest'])

    def test_only_known_infrastructure_steps_retry(self):
        self.assertTrue(auto.eligible_jobs([job()]))
        for jobs in ([], [job('Test API')], [job(), job('Validate')],
                     [dict(name='test', conclusion='failure', steps=[])], [job(conclusion='success')]):
            with self.subTest(jobs=jobs):
                self.assertFalse(auto.eligible_jobs(jobs))

    def test_created_at_uses_original_run(self):
        data = dict(id=123, event='workflow_dispatch', head_branch='main', head_sha=H,
                    repository={'full_name': 'vtmocanu/uzi'}, path='.github/workflows/release-auto.yml',
                    created_at='2026-10-25T01:00:00Z', updated_at='2026-10-26T01:00:00Z')
        self.assertEqual(auto.created_at(context(), lambda url: data), '2026-10-25T01:00:00Z')
        for key, value in (('id', 456), ('event', 'push'), ('head_branch', 'feature'), ('head_sha', NEW),
                           ('path', '.github/workflows/ci.yml'), ('created_at', 'not a timestamp')):
            with self.subTest(key=key), self.assertRaises(auto.Stop):
                auto.created_at(context(), lambda url: {**data, key: value})
        with self.assertRaises(auto.Stop):
            auto.created_at(context(), lambda url: {**data, 'repository': {'full_name': 'other/uzi'}})
        response = Mock()
        response.__enter__ = Mock(return_value=io.BytesIO(json.dumps(data).encode()))
        response.__exit__ = Mock(return_value=False)
        with patch.object(auto.urllib.request, 'urlopen', return_value=response) as fetch:
            auto.created_at(context(GH_TOKEN='credential-sentinel'))
            self.assertEqual(fetch.call_args.args[0].get_header('Authorization'), 'Bearer credential-sentinel')


class CommandLogs(unittest.TestCase):
    def test_only_cutter_style_calls_inherit_output(self):
        with patch.object(auto.subprocess, 'run', return_value=Mock(returncode=0, stdout='machine\n')) as run_command:
            self.assertEqual(auto.command('git', 'rev-parse', 'HEAD'), 'machine')
            self.assertTrue(run_command.call_args.kwargs['capture_output'])
            self.assertEqual(auto.command('bash', auto.CUTTER, live=True), '')
            self.assertFalse(run_command.call_args.kwargs['capture_output'])


class TipFixture:
    def __init__(self, heads, messages=None):
        self.heads, self.index, self.calls, self.now = heads, -1, [], 0.0
        self.messages = messages or {}

    @property
    def head(self):
        return self.heads[min(max(self.index, 0), len(self.heads) - 1)]

    def command(self, *args, **kwargs):
        self.calls.append(args)
        if args[0] == 'git':
            args = args[3:]
            if args[0] == 'fetch':
                self.index += 1
            elif args[0] == 'rev-parse':
                return self.head
            elif args[0] == 'show-ref':
                return 'same tags'
            elif args[0] == 'show':
                return self.messages.get(args[-1], '')
        elif args[:2] == ('bash', auto.CUTTER):
            return rendered(plan(self.head))
        return ''

    def sleep(self, delay):
        self.now += delay


class Tip(unittest.TestCase):
    def release(self, fixture, api):
        return auto.Release(auto.inputs(context()), fixture.command, api, lambda: fixture.now, fixture.sleep)

    def test_skip_ci_tip_refuses_before_wait_cut_or_rerun(self):
        messages = ['docs: new PRD\n\nquoted [' + command + '] in body' for command in
                    ('skip ci', 'ci skip', 'no ci', 'skip actions', 'actions skip')]
        messages += ['docs: new PRD\n\n\nskip-checks:true', 'docs: new PRD\n\n\nskip-checks: true']
        for missing in ('both', 'ci.yml', 'kind-smoke.yml'):
            for message in messages:
                with self.subTest(missing=missing, message=message):
                    fixture = TipFixture([H], {H: message})
                    api = Mock()
                    api.run.side_effect = lambda workflow, sha, branch: None if missing in ('both', workflow) else run()
                    with self.assertRaisesRegex(auto.Stop, 'skip-ci-tip'):
                        self.release(fixture, api).tip(seconds=2, interval=1)
                    self.assertEqual(fixture.now, 0)
                    self.assertTrue(all(call.args[1:] == (H, 'main') for call in api.run.call_args_list))
                    self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))
                    self.assertFalse(any(call[:2] == ('bash', auto.CUTTER) and '--plan' not in call for call in fixture.calls))

    def test_missing_ci_without_skip_instruction_keeps_bounded_wait(self):
        fixture, api = TipFixture([H]), Mock()
        api.run.return_value = None
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-deadline'):
            self.release(fixture, api).tip(seconds=2, interval=1)
        self.assertEqual(fixture.now, 2)

    def test_skip_instruction_never_waives_present_failed_proof(self):
        fixture = TipFixture([H], {H: '[' + 'skip ci' + ']'})
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure') if workflow == 'ci.yml' else run()
        api.pages.return_value = [job('Test API')]
        with self.assertRaisesRegex(auto.Stop, 'tip-ci-not-retryable'):
            self.release(fixture, api).tip(seconds=2)
        self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))

    def test_missing_sibling_proof_never_retries_a_known_failure(self):
        for failed, reason in (('ci.yml', 'tip-ci-not-retryable'), ('kind-smoke.yml', 'tip-smoke-failed')):
            with self.subTest(failed=failed):
                fixture, api = TipFixture([H], {H: '[' + 'skip ci' + ']'}), Mock()
                api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure') if workflow == failed else None
                with self.assertRaisesRegex(auto.Stop, reason):
                    self.release(fixture, api).tip(seconds=2)
                self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))
                api.pages.assert_not_called()

    def test_skip_instruction_with_present_pending_or_cancelled_runs_never_borrows(self):
        for current in (run('queued', None), run(conclusion='cancelled'), run(conclusion='skipped')):
            with self.subTest(current=current):
                fixture, api = TipFixture([H], {H: '[' + 'skip ci' + ']'}), Mock()
                api.run.return_value = current
                with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-deadline'):
                    self.release(fixture, api).tip(seconds=2, interval=1)
                self.assertEqual(fixture.now, 2)
                self.assertTrue(all(call.args[1] == H for call in api.run.call_args_list))

    def test_present_green_exact_proof_is_not_rejected_by_message(self):
        fixture, api = TipFixture([H], {H: '[' + 'skip ci' + ']'}), Mock()
        api.run.return_value = run()
        self.assertEqual(self.release(fixture, api).tip()[0], H)

    def test_next_ci_bearing_tip_unblocks_a_new_attempt(self):
        fixture, api = TipFixture([H, NEW], {H: '[' + 'skip ci' + ']'}), Mock()
        api.run.side_effect = lambda workflow, sha, branch: None if sha == H else run()
        release = self.release(fixture, api)
        with self.assertRaisesRegex(auto.Stop, 'skip-ci-tip'):
            release.tip(seconds=2)
        self.assertEqual(release.tip(seconds=2)[0], NEW)

    def test_main_reselection_uses_new_exact_tip(self):
        fixture = TipFixture([H, NEW])
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run('queued', None) if sha == H else run()
        sha, result = self.release(fixture, api).tip(seconds=30)
        self.assertEqual((sha, result['HEAD_SHA']), (NEW, NEW))
        self.assertIn(('git', '-c', 'core.fsmonitor=false', 'checkout', '-B', 'main', NEW), fixture.calls)

    def test_progress_is_flushed_on_state_changes_without_poll_spam(self):
        fixture = TipFixture([H])
        api = Mock()
        def current(workflow, sha, branch):
            if workflow != 'ci.yml' or fixture.index >= 3:
                return run()
            return run('queued' if fixture.index < 2 else 'in_progress', None)
        api.run.side_effect = current
        with patch('builtins.print') as output:
            self.release(fixture, api).tip(seconds=10, interval=1)
        events = [json.loads(call.args[0].removeprefix('release-auto: ')) for call in output.call_args_list]
        self.assertEqual([event['ci_status'] for event in events], ['queued', 'in_progress', 'completed'])
        self.assertEqual([event['elapsed_seconds'] for event in events], [0, 2, 3])
        self.assertTrue(all(call.kwargs.get('flush') for call in output.call_args_list))
        self.assertTrue(all(event['planned_tag'] == 'v0.2.0-rc.11' and event['deadline_utc'] for event in events))

    def test_one_infrastructure_rerun(self):
        fixture = TipFixture([H])
        api = Mock()
        api.pages.return_value = [job()]
        api.run.side_effect = lambda workflow, sha, branch: (run(conclusion='failure') if fixture.index == 0 else run(attempt=2)) if workflow == 'ci.yml' else run()
        release = self.release(fixture, api)
        release.tip(seconds=30)
        self.assertEqual(sum(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls), 1)
        self.assertEqual(len(release.retries), 1)

    def test_retry_budget_does_not_reset_on_main_move(self):
        fixture = TipFixture([H, NEW])
        api = Mock()
        api.pages.return_value = [job()]
        api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure', number=10 if sha == H else 11) if workflow == 'ci.yml' else run()
        with self.assertRaisesRegex(auto.Stop, 'tip-ci-retry-failed'):
            self.release(fixture, api).tip(seconds=30)
        self.assertEqual(sum(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls), 1)

    def test_test_failure_and_unknown_steps_block(self):
        fixture = TipFixture([H])
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure') if workflow == 'ci.yml' else run()
        api.pages.return_value = [job('Test API')]
        with self.assertRaisesRegex(auto.Stop, 'tip-ci-not-retryable'):
            self.release(fixture, api).tip()
        self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))

    def test_wait_is_bounded_without_real_sleep(self):
        fixture = TipFixture([H])
        api = Mock()
        api.run.return_value = run('queued', None)
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-deadline'):
            self.release(fixture, api).tip(seconds=2, interval=1)
        self.assertEqual(fixture.now, 2)

    def test_green_response_after_deadline_does_not_cut(self):
        fixture = TipFixture([H])
        api = Mock()
        def late(*args):
            fixture.now = 3
            return run()
        api.run.side_effect = late
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-deadline'):
            self.release(fixture, api).tip(seconds=2)

    def test_publication_is_rechecked_after_wait(self):
        fixture = TipFixture([H])
        api = Mock()
        api.run.return_value = run()
        release = self.release(fixture, api)
        release.plan = Mock(side_effect=[plan(), plan(OP='blocked', REASON='previous-publication-not-successful', PUBLICATION_STATE='pending')])
        with self.assertRaisesRegex(auto.Stop, 'previous-publication-not-successful'):
            release.tip()

    def test_retry_success_is_not_reported_as_a_proven_flake(self):
        fixture = TipFixture([H])
        api = Mock()
        api.pages.return_value = [job()]
        api.run.side_effect = lambda workflow, sha, branch: (run(conclusion='failure') if fixture.index == 0 else run(attempt=2)) if workflow == 'ci.yml' else run()
        release = self.release(fixture, api)
        release.tip(seconds=30)
        self.assertEqual(release.retries[0]['result'], 'passed on retry')

    def test_old_success_cannot_mask_the_requested_rerun(self):
        fixture = TipFixture([H])
        api = Mock()
        api.pages.return_value = [job()]
        def response(workflow, sha, branch):
            if workflow != 'ci.yml':
                return run()
            return [run(conclusion='failure', attempt=2), run(attempt=1), run(attempt=3)][min(fixture.index, 2)]
        api.run.side_effect = response
        release = self.release(fixture, api)
        release.tip(seconds=10, interval=1)
        self.assertEqual(fixture.now, 2)
        self.assertEqual(release.retries[0]['result'], 'passed on retry')

    def test_manual_noop_and_supersede_cannot_lift_other_guards(self):
        release = auto.Release(auto.inputs(context()))
        release.accept_rc(plan(OP='noop', REASON='no-new-shipping-change'))
        release.config['supersede'] = TAG
        release.plan = lambda requested: plan()
        release.publication = lambda tag: 'failed'
        release.accept_rc(plan(OP='blocked', REASON='previous-publication-not-successful', PUBLICATION_STATE='failed'))
        with self.assertRaisesRegex(auto.Stop, 'codex-runtime-needs-local-cut'):
            release.accept_rc(plan(OP='blocked', REASON='codex-runtime-needs-local-cut'))
        for state in ('success', 'pending', 'unknown'):
            release.publication = lambda tag: state
            with self.subTest(state=state), self.assertRaisesRegex(auto.Stop, 'supersede-not-failed'):
                release.accept_rc(plan())


class Stable(unittest.TestCase):
    def test_shell_gate_runs_promotion_diagnostics(self):
        source = Path(__file__).with_name('release-scripts-test.sh').read_text()
        blocks = re.findall(r'if \[ "\$#" -eq 0 \]; then\n(.*?)\nfi', source, re.S)
        self.assertTrue(any("promotion_diagnostic_regressions || fail 'promotion diagnostic regressions'" in block
                            for block in blocks), 'default shell gate must run the promotion diagnostics contract')

    def test_promotion_ignores_red_and_advancing_main(self):
        calls, reads = [], [H, NEW]
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run() if workflow == 'release.yml' else run(conclusion='failure')

        def command(*args, **kwargs):
            calls.append(args)
            if args[:2] == ('bash', auto.CUTTER):
                if '--result-file' in args:
                    proof = subprocess.run(['bash', str(Path(__file__).with_name('release-scripts-test.sh')),
                                            '--promotion-diagnostics-only', args[args.index('--result-file') + 1]],
                                           capture_output=True, text=True, timeout=300)
                    self.assertEqual(proof.returncode, 0, proof.stdout + proof.stderr)
                return rendered(plan(OP='blocked', REASON='codex-runtime-needs-local-cut')) if '--plan' in args else ''
            if args[0] != 'git':
                return ''
            git = args[3:]
            if git[0] == 'ls-remote':
                return f'{reads.pop(0)}\trefs/heads/main\n{CANDIDATE}\trefs/tags/{TAG}\n{H}\trefs/tags/v0.1.0'
            if git[0] == 'rev-parse':
                if git[-1] == 'FETCH_HEAD':
                    return NEW
                return CANDIDATE if TAG in git[-1] else (RELEASE if git[-1].endswith('^{commit}') else TAG_OID)
            if git[0] == 'rev-list':
                return f'{RELEASE} {CANDIDATE}'
            if git[0] == 'show':
                return 'version: 0.2.0\nappVersion: "0.2.0"\n'
            if git[:2] == ('bundle', 'create'):
                Path(git[2]).write_bytes(b'fixture bundle')
            return ''

        cfg = auto.inputs(context(RELEASE_KIND='stable', RELEASE_RC_TAG=TAG, RELEASE_RC_SHA=CANDIDATE))
        with tempfile.TemporaryDirectory() as directory:
            result = auto.Release(cfg, command, api).prepare(Path(directory))
        self.assertEqual(result['parent'], CANDIDATE)
        self.assertEqual(result['release'], RELEASE)
        self.assertEqual(result['audit_head'], NEW)
        self.assertFalse(any(call.args[0] == 'ci.yml' for call in api.run.call_args_list))
        self.assertIn(('python3', '-I', 'scripts/release-ci-coverage.py', 'coverage', CANDIDATE, '--hops', '1', '--wait', '0'), calls)

    def test_chart_must_bind_to_planned_tag(self):
        release = auto.Release(auto.inputs(context()), lambda *args, **kwargs: 'version: 0.2.0-rc.12\nappVersion: "0.2.0-rc.12"')
        with self.assertRaisesRegex(auto.Stop, 'prepared-version-mismatch'):
            release.chart(RELEASE, 'v0.2.0-rc.11')


class Changes(unittest.TestCase):
    def test_shared_path_classifier_and_metadata_predicate(self):
        calls = []
        def command(*args, **kwargs):
            calls.append((args, kwargs))
            if args[0] == 'bash':
                self.assertEqual(args[-1], 'api/a\nquote.go')
                return 'shipping'
            if 'log' in args:
                return f'{RELEASE}\tFeature (#35)'
            if 'rev-parse' in args:
                return H
            return 'api/a\nquote.go\0'
        release = auto.Release(auto.inputs(context()), command)
        with patch.object(auto.policy, 'metadata', return_value=False) as metadata:
            records = release.changes('v0.1.0', RELEASE)
            metadata.assert_called_once_with(H, RELEASE)
        self.assertEqual(records, [dict(sha=RELEASE, pr=35, shipping=True)])
        self.assertTrue(any(kwargs.get('raw') for _, kwargs in calls))
        with patch.object(auto.policy, 'metadata', return_value=True):
            self.assertFalse(release.changes('v0.1.0', RELEASE)[0]['shipping'])


class PrepareFixture:
    def __init__(self, kind='rc', scenario='', abandon=False):
        self.kind, self.scenario, self.calls, self.remote_reads = kind, scenario, [], 0
        env = context() if kind == 'rc' else context(RELEASE_KIND='stable', RELEASE_RC_TAG=TAG, RELEASE_RC_SHA=CANDIDATE)
        if abandon:
            env.update(RELEASE_VERSION='0.3.0', RELEASE_ABANDON='true')
        if scenario == 'candidate-sha-mismatch':
            env['RELEASE_RC_SHA'] = NEW
        self.config = auto.inputs(env)
        self.parent = H if kind == 'rc' else CANDIDATE
        self.tag = ('v0.3.0-rc.1' if abandon else 'v0.2.0-rc.11') if kind == 'rc' else 'v0.2.0'
        self.api = Mock()
        self.api.run.return_value = run(conclusion='failure' if scenario == 'candidate-not-published' else 'success')
        self.release = auto.Release(self.config, self.command, self.api)
        rc_plan = plan(BASE='0.3.0', TAG=self.tag, PREVIOUS_TAG='v0.1.0') if abandon else plan()
        self.release.tip = lambda: (H, rc_plan)

    def command(self, *args, **kwargs):
        self.calls.append(args)
        if args[0] == 'python3':
            if args[3] == 'metadata' and self.scenario == 'metadata-rejected':
                raise auto.Stop('metadata-rejected')
            return ''
        if args[:2] == ('bash', auto.CUTTER):
            if '--plan' not in args and self.scenario == 'cutter-main-race':
                raise auto.Stop('cutter-refused')
            if '--result-file' in args:
                cut_result(args[args.index('--result-file') + 1], self.tag)
            return rendered(plan(PREVIOUS_TAG='v0.2.0-rc.11' if self.scenario == 'discovery-not-latest' else TAG)) if '--plan' in args else ''
        if args[0] != 'git':
            raise AssertionError(args)
        git = args[3:]
        if git[0] == 'ls-remote':
            self.remote_reads += 1
            main = NEW if self.scenario == 'main-race' or (self.scenario == 'cutter-main-race' and self.remote_reads > 1) else H
            rows = f'{main}\trefs/heads/main\n{CANDIDATE}\trefs/tags/{TAG}\n{H}\trefs/tags/v0.1.0'
            if self.scenario == 'candidate-not-latest' or (self.scenario == 'release-ref-race' and self.remote_reads > 1):
                rows += f'\n{NEW}\trefs/tags/v0.3.0-rc.1'
            if self.scenario == 'target-tag-exists':
                rows += f'\n{RELEASE}\trefs/tags/{self.tag}'
            if self.scenario == 'stable-superseded':
                rows += f'\n{NEW}\trefs/tags/v0.2.0'
            return rows
        if git[0] == 'rev-parse':
            if git[-1] == 'HEAD':
                return RELEASE
            if git[-1] == 'FETCH_HEAD':
                return NEW
            if TAG in git[-1]:
                return CANDIDATE
            return RELEASE if git[-1].endswith('^{commit}') else TAG_OID
        if git[0] == 'rev-list':
            parent = NEW if self.scenario == 'bad-parent' else self.parent
            return f'{RELEASE} {parent}'
        if git[0] == 'show':
            return f'version: {self.tag[1:]}\nappVersion: "{self.tag[1:]}"\n'
        if git[:2] == ('bundle', 'create'):
            Path(git[2]).write_bytes(b'fixture bundle')
        if git[0] == 'log':
            return ''
        return ''

    def prepare(self):
        with tempfile.TemporaryDirectory() as directory:
            return self.release.prepare(Path(directory))


class PrepareGuards(unittest.TestCase):
    def test_rc_preparation_and_explicit_abandon_mapping(self):
        fixture = PrepareFixture()
        self.assertEqual(fixture.prepare()['parent'], H)
        self.assertIn(('python3', '-I', 'scripts/release-ci-coverage.py', 'metadata', H, RELEASE), fixture.calls)
        abandoned = PrepareFixture(abandon=True)
        abandoned.prepare()
        self.assertEqual(len([call for call in abandoned.calls if call[:5] ==
                              ('bash', auto.CUTTER, '0.3.0', '--ci', '--skip-promote')]), 1)

    def test_rc_refusals_are_independent(self):
        for scenario, reason in (('main-race', 'main-race'), ('cutter-main-race', 'main-race'), ('target-tag-exists', 'target-tag-exists'),
                                 ('bad-parent', 'prepared-parent-mismatch'), ('release-ref-race', 'release-ref-race'),
                                 ('metadata-rejected', 'metadata-rejected')):
            with self.subTest(scenario=scenario), self.assertRaisesRegex(auto.Stop, reason):
                PrepareFixture(scenario=scenario).prepare()

    def test_stable_refusals_are_independent(self):
        for scenario, reason in (('discovery-not-latest', 'candidate-not-latest'),
                                 ('candidate-not-latest', 'candidate-not-latest'),
                                 ('candidate-sha-mismatch', 'candidate-sha-mismatch'),
                                 ('candidate-not-published', 'candidate-not-published'),
                                 ('stable-superseded', 'stable-superseded'),
                                 ('bad-parent', 'promotion-parent-mismatch'),
                                 ('release-ref-race', 'release-ref-race'),
                                 ('metadata-rejected', 'metadata-rejected')):
            with self.subTest(scenario=scenario), self.assertRaisesRegex(auto.Stop, reason):
                PrepareFixture(kind='stable', scenario=scenario).prepare()


class CutDiagnostics(unittest.TestCase):
    def test_failed_cut_keeps_coded_reason_and_missing_merges(self):
        calls = []
        def command(*args, **kwargs):
            self.assertTrue(output.call_args_list, 'workflow command suppression must precede live child output')
            self.assertTrue(output.call_args_list[0].args[0].startswith('::stop-commands::'))
            calls.append((args, kwargs))
            cut_result(args[args.index('--result-file') + 1], 'v0.2.0-rc.11',
                       'changelog-coverage-refused', [{'sha': H, 'pr': 3486}])
            raise auto.Stop('command-failed')
        release = auto.Release(auto.inputs(context()), command)
        with tempfile.TemporaryDirectory() as directory, patch('builtins.print') as output:
            with self.assertRaisesRegex(auto.Stop, 'changelog-coverage-refused'):
                release.cut(['bash', auto.CUTTER, '0.2.0', '--ci'], Path(directory), 'v0.2.0-rc.11')
        marker = output.call_args_list[0].args[0].removeprefix('::stop-commands::')
        self.assertRegex(marker, r'^[0-9a-f]{32}$')
        self.assertEqual(output.call_args_list[-1].args[0], '::' + marker + '::')
        self.assertTrue(all(call.kwargs.get('flush') for call in output.call_args_list))
        self.assertTrue(calls[0][1]['live'])
        self.assertEqual(release.evidence['planned_tag'], 'v0.2.0-rc.11')
        self.assertEqual(json.loads(release.evidence['uncited']), [{'sha': H, 'pr': 3486}])

    def test_stale_or_inconsistent_results_cannot_describe_a_cut(self):
        for shape, reason in (('missing', 'release-diagnostics-unavailable'),
                              ('tag', 'release-diagnostics-tag-mismatch'),
                              ('status', 'release-diagnostics-status-mismatch')):
            with self.subTest(shape=shape), tempfile.TemporaryDirectory() as directory:
                result_file = Path(directory) / 'cut-result.json'
                cut_result(result_file, 'v0.2.0-rc.11')  # stale prior success must be removed
                def command(*args, **kwargs):
                    if shape != 'missing':
                        cut_result(result_file, 'v9.9.9-rc.1' if shape == 'tag' else 'v0.2.0-rc.11',
                                   'changelog-coverage-refused' if shape == 'tag' else 'ready')
                    raise auto.Stop('command-failed')
                release = auto.Release(auto.inputs(context()), command)
                with self.assertRaisesRegex(auto.Stop, reason):
                    release.cut(['bash', auto.CUTTER, '0.2.0'], Path(directory), 'v0.2.0-rc.11')

    def test_successful_command_cannot_contradict_a_failed_result(self):
        def command(*args, **kwargs):
            cut_result(args[args.index('--result-file') + 1], 'v0.2.0-rc.11', 'changelog-coverage-refused')
        release = auto.Release(auto.inputs(context()), command)
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(auto.Stop, 'release-diagnostics-status-mismatch'):
            release.cut(['bash', auto.CUTTER, '0.2.0'], Path(directory), 'v0.2.0-rc.11')


class ResultCodec(unittest.TestCase):
    def test_public_shape_and_bounds(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'result.json'
            cut_result(path, TAG, 'changelog-coverage-refused', [{'sha': H, 'pr': 3486}])
            valid = json.loads(path.read_text())
            self.assertEqual(auto.diagnostics.read(path), valid)
            for key, value in (('schema', 2), ('schema', True), ('stage', 'private-stage'), ('stage', []),
                               ('reason', 'raw diagnostic text'), ('reason', []),
                               ('planned_tag', 'v00.2.0'), ('planned_tag', 123), ('uncited', 'raw prose'),
                               ('uncited_count', 0), ('uncited_count', True), ('uncited_truncated', True),
                               ('uncited', [{'sha': 'not-a-public-sha', 'pr': 3486}]),
                               ('uncited', [{'sha': H, 'pr': True}]), ('uncited', [{'sha': H, 'pr': 0}]),
                               ('uncited', [{'sha': H, 'pr': 2**53}])):
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    auto.diagnostics.validate({**valid, key: value})
            too_many = [{**valid['uncited'][0]} for _ in range(51)]
            with self.assertRaises(ValueError):
                auto.diagnostics.validate({**valid, 'uncited': too_many, 'uncited_count': 51})
            with self.assertRaises(ValueError):
                auto.diagnostics.validate({**valid, 'prose': 'not public metadata'})
            with self.assertRaises(ValueError):
                auto.diagnostics.validate({**valid, 'uncited': None, 'uncited_count': 1})
            with self.assertRaises(ValueError):
                auto.diagnostics.validate({**valid, 'uncited': None, 'uncited_count': None, 'uncited_truncated': True})
            path.write_text(json.dumps(valid) + ' ' * 65536)  # fixed wire-contract limit, independent of implementation
            with self.assertRaises(ValueError):
                auto.diagnostics.read(path)
            path.write_text(json.dumps(valid).replace('"schema": 1', '"schema": 1, "schema": 1'))
            with self.assertRaises(ValueError):
                auto.diagnostics.read(path)

    def test_child_error_reason_survives_and_cannot_become_ready(self):
        with tempfile.TemporaryDirectory() as directory:
            child, parent = Path(directory) / 'child.json', Path(directory) / 'parent.json'
            for reason, status in (('changelog-coverage-refused', 1), ('changelog-coverage-error', 2)):
                auto.diagnostics.write(child, 'changelog-coverage', reason, TAG, kind='oracle', status=status)
                auto.diagnostics.write(parent, 'changelog-coverage', 'changelog-coverage-error', TAG,
                                       child=child, status=1)
                self.assertEqual(auto.diagnostics.read(parent)['reason'], reason)
                with self.assertRaises(ValueError):
                    auto.diagnostics.write(parent, 'changelog-links', 'ready', TAG, child=child, status=0)
    def test_identity_cap_is_explicit_and_atomic(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / 'missing.tsv', Path(directory) / 'result.json'
            source.write_text(''.join(f'{index:040x}\t{index+1}\n' for index in range(51)))
            auto.diagnostics.write(target, 'changelog-coverage', 'changelog-coverage-refused', TAG,
                                   missing=source, kind='oracle')
            result = auto.diagnostics.read(target)
            self.assertEqual((len(result['uncited']), result['uncited_count'], result['uncited_truncated']), (50, 51, True))
            self.assertEqual(set(Path(directory).iterdir()), {source, target})
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_large_source_and_untrusted_pr_numbers(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / 'missing.tsv', Path(directory) / 'result.json'
            source.write_text((H + '\t1234567890\n') * 21000)  # valid rows, beyond the 1 MiB source limit
            with self.assertRaises(ValueError):
                auto.diagnostics.write(target, 'changelog-coverage', 'changelog-coverage-refused', TAG,
                                       missing=source, kind='oracle')
            source.write_text(H + '\t0\n' + NEW + '\t9007199254740992\n')
            auto.diagnostics.write(target, 'changelog-coverage', 'changelog-coverage-refused', TAG,
                                   missing=source, kind='oracle')
            self.assertEqual(auto.diagnostics.read(target)['uncited'], [{'sha': H, 'pr': None}, {'sha': NEW, 'pr': None}])

    def test_exit_three_refusals_are_not_generic_cut_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'result.json'
            auto.diagnostics.write(target, 'notes-assembly', 'release-cut-failed', TAG, status=3)
            self.assertEqual(auto.diagnostics.read(target)['reason'], 'release-precondition-refused')

class FailedEvidence(unittest.TestCase):
    def failure(self, request_error=False):
        fixture = TipFixture([H])
        api = Mock()
        api.pages.return_value = [job()]
        api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure', attempt=1 if fixture.index == 0 else 2) if workflow == 'ci.yml' else run()
        def command(*args, **kwargs):
            if request_error and args[:3] == ('gh', 'run', 'rerun'):
                raise auto.Stop('command-failed')
            return fixture.command(*args, **kwargs)
        release = auto.Release(auto.inputs(context()), command, api, lambda: fixture.now, fixture.sleep)
        release.prepare = lambda directory: release.tip(seconds=30, interval=1)
        with tempfile.TemporaryDirectory() as directory:
            output, summary = Path(directory) / 'output', Path(directory) / 'summary'
            env = context(RUNNER_TEMP=directory, GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=str(summary))
            with patch.dict(os.environ, env, clear=True), patch.object(auto.sys, 'argv', ['release-auto.py', 'prepare']), \
                 patch.object(auto, 'Release', return_value=release), contextlib.redirect_stderr(io.StringIO()):
                status = auto.main()
            data = dict(line.split('=', 1) for line in output.read_text().splitlines())
            return status, data, summary.read_text()

    def test_failed_retry_keeps_evidence_in_real_main_outputs_and_summary(self):
        status, data, summary = self.failure()
        self.assertEqual(status, 1)
        self.assertEqual(data['parent'], H)
        self.assertIn('planned_tag', data)
        self.assertEqual(data['planned_tag'], 'v0.2.0-rc.11')
        retry = json.loads(data['retry_log'])[0]
        self.assertEqual((retry['sha'], retry['run'], retry['attempt']), (H, 10, 1))
        self.assertEqual(retry['jobs'][0]['id'], 20)
        self.assertEqual(retry['jobs'][0]['steps'][0]['name'], 'Set up job')
        self.assertEqual(retry['result'], 'failed after retry')
        self.assertIn(H, summary)
        self.assertIn('failed after retry', summary)

    def test_uncertain_rerun_request_keeps_attempted_evidence(self):
        status, data, summary = self.failure(request_error=True)
        self.assertEqual(status, 1)
        self.assertEqual(data['parent'], H)
        retry = json.loads(data['retry_log'])[0]
        self.assertEqual(retry['result'], 'request unconfirmed')
        self.assertEqual(retry['run'], 10)
        self.assertIn(H, summary)


class RunAttempts(unittest.TestCase):
    def test_prepare_rerun_refuses_before_any_orchestration(self):
        with tempfile.TemporaryDirectory() as directory:
            env = context(GITHUB_RUN_ATTEMPT='2', RUNNER_TEMP=directory,
                          GITHUB_OUTPUT=directory + '/output', GITHUB_STEP_SUMMARY=directory + '/summary')
            with patch.dict(os.environ, env, clear=True), patch.object(auto.sys, 'argv', ['release-auto.py', 'prepare']), \
                 patch.object(auto, 'Release') as constructor, contextlib.redirect_stderr(io.StringIO()) as error:
                constructor.return_value.prepare.side_effect = AssertionError('preparer ran during a rerun')
                status = auto.main()
            self.assertEqual(status, 1)
            constructor.assert_not_called()
            self.assertIn('re-dispatch-required', Path(directory, 'output').read_text())
            self.assertIn('re-dispatch instead', error.getvalue().lower())

    def test_watch_rerun_can_observe_the_same_tag(self):
        with patch.dict(os.environ, context(GITHUB_RUN_ATTEMPT='2', RELEASE_TAG='v0.2.0-rc.11', RELEASE_SHA=RELEASE), clear=True), \
             patch.object(auto.sys, 'argv', ['release-auto.py', 'watch-check']), patch.object(auto, 'Release') as constructor:
            constructor.return_value.watch_admission.return_value = {'outcome': 'watch-admitted'}
            self.assertEqual(auto.main(), 0)
            constructor.return_value.watch_admission.assert_called_once_with('v0.2.0-rc.11', RELEASE)
class Recovery(unittest.TestCase):
    def test_stable_recovery_cannot_republish_older_latest(self):
        cfg = auto.inputs(context(RELEASE_KIND='stable', RELEASE_RC_TAG=TAG, RELEASE_RC_SHA=CANDIDATE))
        release = auto.Release(cfg)
        release.remote = lambda: dict(tags={'v0.2.0': RELEASE}, previous_stable='v0.2.1')
        with self.assertRaisesRegex(auto.Stop, 'stable-recovery-superseded'):
            release.watch_admission('v0.2.0', RELEASE)
        release.remote = lambda: dict(tags={'v0.2.0': RELEASE}, previous_stable='v0.2.0')
        self.assertEqual(release.watch_admission('v0.2.0', RELEASE)['outcome'], 'watch-admitted')

    def test_recovery_requires_exact_existing_tag_sha(self):
        release = auto.Release(auto.inputs(context()))
        release.remote = lambda: dict(tags={'v0.2.0-rc.11': RELEASE}, previous_stable='v0.1.0')
        with self.assertRaisesRegex(auto.Stop, 'recovery-tag-mismatch'):
            release.watch_admission('v0.2.0-rc.11', NEW)
        self.assertEqual(release.watch_admission('v0.2.0-rc.11', RELEASE)['outcome'], 'watch-admitted')
        release.remote = lambda: dict(tags={'v0.2.0-rc.11': RELEASE, 'v0.3.0': NEW}, previous_stable='v0.3.0')
        self.assertEqual(release.watch_admission('v0.2.0-rc.11', RELEASE)['outcome'], 'watch-admitted')

    def test_log_is_bounded_escaped_and_marks_truncation(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / 'log', Path(directory) / 'summary'
            source.write_bytes(b'<unsafe>' * 100 + b'END_NOT_INCLUDED')
            auto.append_watch_log(source, target, limit=32)
            text = target.read_text()
            self.assertIn('&lt;unsafe&gt;', text)
            self.assertNotIn('<unsafe>', text)
            self.assertNotIn('END_NOT_INCLUDED', text)
            self.assertIn('Truncated at 32 source bytes', text)


class Nightly(unittest.TestCase):
    created = '2026-10-27T02:47:00Z'

    @staticmethod
    def epoch(stamp):
        return dt.datetime.fromisoformat(stamp.replace('Z', '+00:00')).timestamp()

    def env(self, **changes):
        values = context(GITHUB_EVENT_NAME='schedule', RELEASE_ACTOR='scheduler', RELEASE_DISPATCHERS='',
                         RELEASE_NIGHTLY_ENABLED='true', RUN_CREATED_AT=self.created)
        values.update(changes)
        return values

    def release(self, selected=None, heads=None, now='2026-10-27T03:00:00Z'):
        fixture = TipFixture(heads or [H])
        cfg = auto.inputs(context())
        cfg.update(scheduled=True, nightly_enabled=True, created_at=self.created)
        api = Mock()
        api.run.return_value = run()
        def command(*args, **kwargs):
            if args[:2] == ('bash', auto.CUTTER):
                fixture.calls.append(args)
                return rendered({**(selected or plan()), 'HEAD_SHA': fixture.head})
            return fixture.command(*args, **kwargs)
        release = auto.Release(cfg, command, api, lambda: fixture.now, fixture.sleep)
        release.wall_time = lambda: self.epoch(now) + fixture.now
        return release, fixture, api

    def cli(self, env, mode='authorize', release=None, now='2026-10-27T03:00:00Z', metadata_error=False):
        with tempfile.TemporaryDirectory() as directory:
            output, note = Path(directory) / 'output', Path(directory) / 'summary'
            values = {**env, 'RUNNER_TEMP': directory, 'GITHUB_OUTPUT': str(output), 'GITHUB_STEP_SUMMARY': str(note)}
            if mode == 'authorize':
                values.pop('RUN_CREATED_AT', None)
            with patch.dict(os.environ, values, clear=True), patch.object(auto.sys, 'argv', ['release-auto.py', mode]), \
                 patch.object(auto, 'created_at', return_value=self.created,
                              side_effect=AssertionError('paused route queried run metadata') if metadata_error else None), \
                 patch.object(auto.time, 'time', return_value=self.epoch(now)), \
                 patch.object(auto, 'Release', return_value=release) as constructor, contextlib.redirect_stderr(io.StringIO()):
                status = auto.main()
            data = dict(line.split('=', 1) for line in output.read_text().splitlines())
            return status, data, note.read_text(), constructor

    def test_schedule_has_no_actor_allowlist_but_dispatch_still_does(self):
        self.assertTrue(auto.inputs(self.env())['scheduled'])
        with self.assertRaisesRegex(auto.Stop, 'dispatcher-not-allowed'):
            auto.inputs(self.env(GITHUB_EVENT_NAME='workflow_dispatch'))
        for event in ('push', 'pull_request', ''):
            with self.subTest(event=event), self.assertRaisesRegex(auto.Stop, 'invalid-event'):
                auto.inputs(context(GITHUB_EVENT_NAME=event))
        for field, value in (('GITHUB_REPOSITORY', 'other/uzi'), ('RELEASE_REF', 'refs/heads/feature')):
            with self.subTest(field=field), self.assertRaises(auto.Stop):
                auto.inputs(self.env(**{field: value}))

    def test_schedule_cannot_override_the_canonical_rc_route(self):
        for changes in ({'RELEASE_VERSION': '0.3.0'}, {'RELEASE_SUPERSEDE': TAG},
                        {'RELEASE_ABANDON': 'true', 'RELEASE_VERSION': '0.3.0'},
                        {'RELEASE_KIND': 'stable', 'RELEASE_RC_TAG': TAG, 'RELEASE_RC_SHA': CANDIDATE}):
            with self.subTest(changes=changes), self.assertRaisesRegex(auto.Stop, 'scheduled-input-mismatch'):
                auto.inputs(self.env(RELEASE_ACTOR='maintainer', RELEASE_DISPATCHERS='maintainer', **changes))

    def test_schedule_metadata_is_bound_to_the_actual_event_and_original_run(self):
        data = dict(id=123, event='schedule', head_branch='main', head_sha=H,
                    repository={'full_name': 'vtmocanu/uzi'}, path='.github/workflows/release-auto.yml',
                    created_at=self.created, updated_at='2026-10-28T03:00:00Z')
        try:
            metadata = auto.created_at(self.env(), lambda url: data)
        except auto.Stop as error:
            self.fail('valid scheduled run metadata was refused: ' + error.reason)
        self.assertEqual(metadata, self.created)
        with self.assertRaisesRegex(auto.Stop, 'invalid-run-metadata'):
            auto.created_at(self.env(), lambda url: {**data, 'event': 'workflow_dispatch'})

    def test_nightly_flag_defaults_off_and_only_exact_true_enables(self):
        for value in ('', 'false', 'TRUE', '1'):
            with self.subTest(value=value):
                status, data, note, constructor = self.cli(self.env(RELEASE_NIGHTLY_ENABLED=value), metadata_error=True)
                self.assertEqual(status, 0)
                self.assertEqual((data['outcome'], data['reason']), ('no-op', 'nightly-disabled'))
                self.assertIn('nightly-disabled', note)
                constructor.assert_not_called()
        status, data, _, _ = self.cli(self.env())
        self.assertEqual((status, data['outcome']), (0, 'authorized'))
        status, data, _, _ = self.cli(context(RELEASE_NIGHTLY_ENABLED='false'))
        self.assertEqual((status, data['outcome']), (0, 'authorized'))

    def test_paused_prepare_does_not_wait_or_cut(self):
        release, fixture, api = self.release()
        release.config['nightly_enabled'] = False
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(auto.Stop, 'nightly-disabled') as stopped:
            release.prepare(Path(directory))
        self.assertEqual(stopped.exception.outcome, 'no-op')
        self.assertEqual(fixture.calls, [])
        api.run.assert_not_called()

    def test_planned_noops_stop_before_ci_wait_for_every_nightly_baseline(self):
        cases = [plan(OP='noop', REASON='no-new-shipping-change', SHIPPING_COUNT='0'),
                 plan(OP='noop', TAG='v0.2.0-rc.1', PREVIOUS_TAG='v0.1.0', REASON='no-new-shipping-change', SHIPPING_COUNT='0'),
                 plan(OP='noop', REASON='no-releasable-notes', SHIPPING_COUNT='1')]
        for selected in cases:
            with self.subTest(tag=selected['TAG'], reason=selected['REASON']):
                release, fixture, api = self.release(selected)
                fixture.messages[H] = '[' + 'skip ci' + ']'
                with self.assertRaises(auto.Stop) as stopped:
                    release.tip()
                self.assertEqual((stopped.exception.outcome, stopped.exception.reason), ('no-op', selected['REASON']))
                api.run.assert_not_called()
                self.assertFalse(any('--plan' not in call for call in fixture.calls if call[:2] == ('bash', auto.CUTTER)))

    def test_blocked_publication_is_not_lifted_by_a_zero_threshold(self):
        release, _, _ = self.release(plan(OP='blocked', REASON='previous-publication-not-successful', SHIPPING_COUNT='0'))
        with self.assertRaisesRegex(auto.Stop, 'previous-publication-not-successful'):
            release.tip()

    def test_noop_cli_succeeds_without_creating_a_bundle(self):
        release, _, api = self.release(plan(OP='noop', REASON='no-new-shipping-change', SHIPPING_COUNT='0'))
        status, data, note, _ = self.cli(self.env(), 'prepare', release)
        self.assertEqual((status, data['outcome'], data['reason']), (0, 'no-op', 'no-new-shipping-change'))
        self.assertEqual((data['parent'], data['planned_tag']), (H, 'v0.2.0-rc.11'))
        self.assertNotIn('digest', data)
        self.assertIn('no-new-shipping-change', note)
        api.run.assert_not_called()

    def test_skip_ci_tip_cli_preserves_named_failure_without_bundle(self):
        release, fixture, api = self.release()
        fixture.messages[H] = '[' + 'skip ci' + ']'
        api.run.return_value = None
        status, data, note, _ = self.cli(self.env(), 'prepare', release)
        self.assertEqual((status, data['reason'], data['parent'], data['stage']), (1, 'skip-ci-tip', H, 'tip-wait'))
        self.assertIn('skip-ci-tip', note)
        self.assertNotIn('digest', data)
        self.assertEqual((data['outcome'], fixture.now), ('defer', 10800))
        self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))

    def test_scheduled_skip_tip_follows_new_ci_bearing_tip_in_same_operation(self):
        release, fixture, api = self.release(heads=[H, NEW], now='2026-10-27T10:49:30Z')
        fixture.messages[H] = '[' + 'skip ci' + ']'
        api.run.side_effect = lambda workflow, sha, branch: None if sha == H else run()
        with contextlib.redirect_stdout(io.StringIO()) as progress:
            try:
                selected, _ = release.tip(interval=5)
            except auto.Stop as error:
                self.fail('scheduled attempt lost its recoverable window: ' + error.reason)
        self.assertEqual((selected, fixture.now), (NEW, 5))
        events = [json.loads(line.removeprefix('release-auto: ')) for line in progress.getvalue().splitlines()]
        self.assertEqual([event.get('skip_ci_tip') for event in events], [True, False])

    def test_scheduled_skip_tip_names_refusal_only_at_deadline(self):
        for overshoot in (False, True):
            with self.subTest(overshoot=overshoot):
                release, fixture, api = self.release(now='2026-10-27T10:49:30Z')
                fixture.messages[H] = '[' + 'skip ci' + ']'
                api.run.return_value = None
                if overshoot:
                    release.sleep = lambda delay: fixture.sleep(delay + 1)
                with contextlib.redirect_stdout(io.StringIO()) as progress, self.assertRaisesRegex(auto.Stop, 'skip-ci-tip') as stopped:
                    release.tip(interval=7)
                self.assertEqual((stopped.exception.outcome, fixture.now), ('defer', 31 if overshoot else 30))
                event = json.loads(progress.getvalue().splitlines()[0].removeprefix('release-auto: '))
                self.assertIs(event.get('skip_ci_tip'), True)
                self.assertFalse(any(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls))

    def test_scheduled_move_to_unmarked_missing_tip_restores_normal_deadline_reason(self):
        release, fixture, api = self.release(heads=[H, NEW], now='2026-10-27T10:49:30Z')
        fixture.messages[H] = '[' + 'skip ci' + ']'
        api.run.return_value = None
        with contextlib.redirect_stdout(io.StringIO()) as progress, self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-cutoff'):
            release.tip(interval=5)
        self.assertEqual(fixture.now, 30)
        events = [json.loads(line.removeprefix('release-auto: ')) for line in progress.getvalue().splitlines()]
        self.assertEqual([event.get('skip_ci_tip') for event in events], [True, False])

    def test_manual_noop_respin_and_morning_independent_budget_remain(self):
        release, fixture, api = self.release(plan(OP='noop', SHIPPING_COUNT='0'))
        release.config['scheduled'] = False
        release.config['nightly_enabled'] = False
        release.wall_time = lambda: self.epoch('2026-10-27T16:00:00Z')
        try:
            selected = release.tip()
        except auto.Stop as error:
            self.fail('manual respin was gated by the nightly policy: ' + error.reason)
        self.assertEqual(selected[0], H)
        api.run.return_value = run('queued', None)
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-deadline'):
            release.tip(seconds=30, interval=5)
        self.assertEqual(fixture.now, 30)

    def test_window_uses_bucharest_date_and_dst_offsets(self):
        cases = [('2026-01-15T02:47:00Z', '2026-01-15T11:00:00Z', '2026-01-15T12:00:00Z'),
                 ('2026-07-15T01:47:00Z', '2026-07-15T10:00:00Z', '2026-07-15T11:00:00Z'),
                 ('2026-03-29T01:47:00Z', '2026-03-29T10:00:00Z', '2026-03-29T11:00:00Z'),
                 ('2026-10-25T02:47:00Z', '2026-10-25T11:00:00Z', '2026-10-25T12:00:00Z'),
                 ('2026-10-26T22:30:00Z', '2026-10-27T11:00:00Z', '2026-10-27T12:00:00Z')]
        for created, cutoff, target in cases:
            with self.subTest(created=created):
                self.assertEqual(auto.nightly_window(created), (self.epoch(cutoff), self.epoch(target)))
        for created in ('', None, True, 'not-a-date', '2026-02-30T03:00:00Z',
                        '2026-10-27 02:47:00+00:00', '2026-10-27T02:47:00+00:00'):
            with self.subTest(created=created), self.assertRaises(auto.Stop):
                auto.nightly_window(created)

    def test_schedule_caps_wait_and_respects_late_cutoff_while_manual_keeps_two_hours(self):
        cases = [(True, self.created, 10800, '2026-10-27T05:47:00Z'),
                 (True, '2026-10-27T08:47:00Z', 7380, '2026-10-27T10:50:00Z'),
                 (True, '2026-07-15T07:47:00Z', 7380, '2026-07-15T09:50:00Z'),
                 (False, self.created, 7200, '2026-10-27T04:47:00Z')]
        for scheduled, created, expected, deadline in cases:
            with self.subTest(scheduled=scheduled, created=created):
                release, fixture, api = self.release(now=created)
                release.config.update(scheduled=scheduled, created_at=created)
                api.run.return_value = run('queued', None)
                reason = 'tip-not-green-by-cutoff' if scheduled else 'tip-not-green-by-deadline'
                with contextlib.redirect_stdout(io.StringIO()) as progress, self.assertRaisesRegex(auto.Stop, reason):
                    release.tip(interval=60)
                self.assertEqual(fixture.now, expected)
                first = json.loads(progress.getvalue().splitlines()[0].removeprefix('release-auto: '))
                self.assertEqual(self.epoch(first['deadline_utc']), self.epoch(deadline))

    def test_tip_deadline_reserves_cut_time_and_does_not_reset_on_main_move(self):
        release, fixture, api = self.release(heads=[H, NEW], now='2026-10-27T10:49:50Z')
        api.run.return_value = run('queued', None)
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-cutoff'):
            release.tip(seconds=7200, interval=5)
        self.assertEqual(fixture.now, 10)
        self.assertGreaterEqual(fixture.index, 1)

    def test_green_response_after_cut_start_deadline_does_not_cut(self):
        release, fixture, api = self.release(now='2026-10-27T10:49:50Z')
        def slow_run(*args):
            fixture.now += 20
            return run()
        api.run.side_effect = slow_run
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-cutoff'):
            release.tip()

    def test_final_plan_cannot_extend_the_cut_start_deadline(self):
        release, fixture, _ = self.release(now='2026-10-27T10:49:50Z')
        original, reads = release.run, 0
        def slow_plan(*args, **kwargs):
            nonlocal reads
            result = original(*args, **kwargs)
            if args[:2] == ('bash', auto.CUTTER):
                reads += 1
                if reads == 2:
                    fixture.now += 20
            return result
        release.run = slow_plan
        with self.assertRaisesRegex(auto.Stop, 'tip-not-green-by-cutoff'):
            release.tip()

    def test_scheduled_retry_cap_still_one_when_main_advances(self):
        release, fixture, api = self.release(heads=[H, NEW])
        api.pages.return_value = [job()]
        api.run.side_effect = lambda workflow, sha, branch: run(conclusion='failure', number=10 if sha == H else 11) if workflow == 'ci.yml' else run()
        with self.assertRaisesRegex(auto.Stop, 'tip-ci-retry-failed'):
            release.tip()
        self.assertEqual(sum(call[:3] == ('gh', 'run', 'rerun') for call in fixture.calls), 1)

    def test_cut_is_refused_if_window_expires_after_tip_returns(self):
        release, fixture, _ = self.release(now='2026-10-27T11:01:00Z')
        release.tip = lambda: (H, plan())
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(auto.Stop, 'missed-target'):
            release.prepare(Path(directory))
        self.assertEqual(fixture.calls, [])

    def test_expired_or_next_day_preparation_does_not_acquire_a_new_window(self):
        for now in ('2026-10-27T10:50:01Z', '2026-10-27T11:00:00Z', '2026-10-28T02:00:00Z'):
            release, fixture, api = self.release(now=now)
            with self.subTest(now=now), tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(auto.Stop, 'missed-target'):
                release.prepare(Path(directory))
            self.assertEqual(fixture.calls, [])
            api.run.assert_not_called()
            with self.assertRaisesRegex(auto.Stop, 'missed-target'):
                release.tip()

    def test_future_run_metadata_cannot_extend_the_window(self):
        release, fixture, api = self.release(now='2026-10-27T02:40:00Z')
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(auto.Stop, 'invalid-run-age'):
            release.prepare(Path(directory))
        self.assertEqual(fixture.calls, [])
        api.run.assert_not_called()
        status, data, _, _ = self.cli(self.env(), now='2026-10-27T02:40:00Z')
        self.assertEqual((status, data.get('reason')), (1, 'invalid-run-age'))

    def test_scheduled_creation_clock_skew_is_bounded(self):
        for seconds in (0, 59, 60):
            now = dt.datetime.fromtimestamp(self.epoch(self.created) - seconds, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
            with self.subTest(skew=seconds):
                status, data, _, _ = self.cli(self.env(), now=now)
                self.assertEqual((status, data['outcome']), (0, 'authorized'))
        now = dt.datetime.fromtimestamp(self.epoch(self.created) - 61, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        status, data, _, _ = self.cli(self.env(), now=now)
        self.assertEqual((status, data.get('reason')), (1, 'invalid-run-age'))

    def test_scheduled_prepare_rerun_still_requires_redispatch(self):
        status, data, _, constructor = self.cli(self.env(GITHUB_RUN_ATTEMPT='2'), 'prepare')
        self.assertEqual((status, data['reason']), (1, 're-dispatch-required'))
        constructor.assert_not_called()

    def test_tagged_recovery_ignores_pause_flag_and_morning_window(self):
        release, _, _ = self.release(now='2026-10-28T16:00:00Z')
        release.config.update(nightly_enabled=False, created_at='not-needed-for-recovery')
        release.remote = lambda: dict(tags={'v0.2.0-rc.11': RELEASE}, previous_stable='v0.1.0')
        try:
            admitted = release.watch_admission('v0.2.0-rc.11', RELEASE)
        except auto.Stop as error:
            self.fail('tagged recovery was gated by the nightly policy: ' + error.reason)
        self.assertEqual(admitted['outcome'], 'watch-admitted')
        status, data, _, _ = self.cli(self.env(GITHUB_RUN_ATTEMPT='2', RELEASE_NIGHTLY_ENABLED='false', RELEASE_TAG='v0.2.0-rc.11', RELEASE_SHA=RELEASE),
                                    'watch-check', release, now='2026-10-28T16:00:00Z')
        self.assertEqual((status, data['outcome']), (0, 'watch-admitted'))


if __name__ == '__main__':
    unittest.main(verbosity=2)
