#!/usr/bin/env python3
"""Offline release dispatch/orchestration fixtures; no live API or publication."""
import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('release_auto', Path(__file__).with_name('release-auto.py'))
auto = importlib.util.module_from_spec(spec)
spec.loader.exec_module(auto)
H, NEW, CANDIDATE, RELEASE, TAG_OID = (char * 40 for char in '12345')
TAG = 'v0.2.0-rc.10'


def context(**changes):
    env = dict(GITHUB_ACTIONS='true', GITHUB_REPOSITORY='vtmocanu/uzi', RELEASE_REF='refs/heads/main',
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


class TipFixture:
    def __init__(self, heads):
        self.heads, self.index, self.calls, self.now = heads, -1, [], 0.0

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
        elif args[:2] == ('bash', auto.CUTTER):
            return rendered(plan(self.head))
        return ''

    def sleep(self, delay):
        self.now += delay


class Tip(unittest.TestCase):
    def release(self, fixture, api):
        return auto.Release(auto.inputs(context()), fixture.command, api, lambda: fixture.now, fixture.sleep)

    def test_main_reselection_uses_new_exact_tip(self):
        fixture = TipFixture([H, NEW])
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run('queued', None) if sha == H else run()
        sha, result = self.release(fixture, api).tip(seconds=30)
        self.assertEqual((sha, result['HEAD_SHA']), (NEW, NEW))
        self.assertIn(('git', '-c', 'core.fsmonitor=false', 'checkout', '-B', 'main', NEW), fixture.calls)

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
    def test_promotion_ignores_red_and_advancing_main(self):
        calls, reads = [], [H, NEW]
        api = Mock()
        api.run.side_effect = lambda workflow, sha, branch: run() if workflow == 'release.yml' else run(conclusion='failure')

        def command(*args, **kwargs):
            calls.append(args)
            if args[:2] == ('bash', auto.CUTTER):
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
        self.assertIn(('bash', auto.CUTTER, '0.3.0', '--ci', '--skip-promote'), abandoned.calls)

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


if __name__ == '__main__':
    unittest.main(verbosity=2)
