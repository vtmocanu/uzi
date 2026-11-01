#!/usr/bin/env python3
"""Offline new-class recovery fixtures; no live reruns, tags or cluster work."""
import importlib.util
import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

spec = importlib.util.spec_from_file_location('watch', Path(__file__).with_name('release-watch.py'))
watch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(watch)


def step(name, conclusion='failure'):
    return dict(number=1, name=name, status='completed', conclusion=conclusion)


def job(name, conclusion='failure', identity=101, steps=None):
    return dict(id=identity, run_id=10, name=name, status='completed', conclusion=conclusion,
                html_url=f'https://github.com/fixture/repo/actions/runs/10/job/{identity}',
                steps=steps if steps is not None else [step('Create KinD cluster')])


def run(conclusion='failure', attempt=1):
    return dict(id=10, run_attempt=attempt, status='completed', conclusion=conclusion)


def smoke_jobs(failing_step='Create KinD cluster'):
    return [job('changes', 'skipped', 100, []),
            job('kind-smoke', steps=[step(failing_step)]),
            job('kind-smoke-gate', identity=102, steps=[step('Gate on the kind-smoke result')])]


class Classification(unittest.TestCase):
    def test_release_roles_match_current_workflow_and_matrix(self):
        path = Path(__file__).parent.parent / '.github/workflows/release.yml'
        program = 'puts YAML.safe_load(File.read(ARGV[0]), aliases: true).fetch("jobs").to_json'
        graph = json.loads(subprocess.check_output(['ruby', '-ryaml', '-rjson', '-e', program, str(path)], text=True, timeout=60))
        roles = set(graph) - {'publish-agent'}
        templates = graph['publish-agent']['strategy']['matrix']['template']
        self.assertEqual(set(templates), {'base', 'jvm'})
        roles.update('publish-agent (' + template + ')' for template in templates)
        self.assertEqual(roles, watch.RELEASE_SUCCESS_ROLES | watch.RELEASE_SKIPPED_ROLES | {'assert-tag-smoke'})
        self.assertIn('assert-tag-smoke', graph['publish-chart']['needs'])
        self.assertIn('publish-chart', graph['publish-release']['needs'])

    def test_unknown_and_cancelled_step_conclusions_refuse_independently(self):
        for conclusion in ['cancelled', 'neutral', 'unknown', None]:
            jobs = smoke_jobs()
            jobs[1]['steps'].append(step('Create KinD cluster', conclusion))
            with self.subTest(conclusion=conclusion), self.assertRaises(watch.RecoveryRefusal):
                watch.smoke_retry_job(run(), jobs)

    def test_path_filter_and_real_failure_results_are_required(self):
        for role, conclusion in [('changes', 'success'), ('kind-smoke', 'success'), ('kind-smoke', 'neutral'), ('kind-smoke', 'skipped')]:
            jobs = [dict(item, conclusion=conclusion) if item['name'] == role else item for item in smoke_jobs()]
            with self.subTest(role=role, conclusion=conclusion), self.assertRaises(watch.RecoveryRefusal):
                watch.smoke_retry_job(run(), jobs)

    def test_targeted_scope_matches_current_transitive_dependents(self):
        root = Path(__file__).parent.parent
        program = 'puts YAML.safe_load(File.read(ARGV[0]), aliases: true).fetch("jobs").transform_values { |job| Array(job["needs"]) }.to_json'
        for path, expected in [('kind-smoke.yml', watch.SMOKE_AFFECTED_JOBS), ('release.yml', watch.ASSERT_AFFECTED_JOBS)]:
            graph = json.loads(subprocess.check_output(['ruby', '-ryaml', '-rjson', '-e', program,
                               str(root / '.github/workflows' / path)], text=True, timeout=60))
            reached = {expected[0]}
            while True:
                expanded = reached | {name for name, parents in graph.items() if reached.intersection(parents)}
                if expanded == reached:
                    break
                reached = expanded
            self.assertEqual(reached, set(expected), path)
    def test_allowlist_entries_match_named_or_derived_current_steps(self):
        path = Path(__file__).parent.parent / '.github/workflows/kind-smoke.yml'
        program = 'puts YAML.safe_load(File.read(ARGV[0]), aliases: true).fetch("jobs").fetch("kind-smoke").fetch("steps").to_json'
        steps = json.loads(subprocess.check_output(['ruby', '-ryaml', '-rjson', '-e', program, str(path)], text=True, timeout=60))
        names = [item.get('name', 'Run ' + item.get('uses', '')) for item in steps]
        self.assertLessEqual(watch.SMOKE_INFRA_STEPS - {'Set up job'}, set(names))
        self.assertTrue(any(watch.re.fullmatch(watch.CHECKOUT_STEP, name) for name in names))
    def test_exact_infrastructure_steps_and_variable_checkout_pin(self):
        names = ['Set up job', 'Create KinD cluster', 'Load images into kind', 'Setup Helm',
                 'Run actions/checkout@' + 'a' * 40, 'Run actions/checkout@' + 'b' * 40]
        for name in names:
            with self.subTest(name=name):
                try:
                    selected = watch.smoke_retry_job(run(), smoke_jobs(name))
                except watch.RecoveryRefusal as error:
                    self.fail(f'allowed infrastructure role refused: {error}')
                self.assertEqual(selected['name'], 'kind-smoke')

    def test_build_product_unknown_and_action_lookalikes_refuse(self):
        names = ['Build api + web images for kind', 'Install CNPG operator', 'Create namespace + secrets',
                 'helm install', 'Wait for workloads', 'Smoke', 'Diagnostics (on failure)',
                 'Load images into kind extra', 'Run actions/checkout@main',
                 'Run other/checkout@' + 'a' * 40, 'Run actions/checkout@' + 'a' * 40 + ' extra']
        for name in names:
            with self.subTest(name=name), self.assertRaises(watch.RecoveryRefusal):
                watch.smoke_retry_job(run(), smoke_jobs(name))

    def test_mixed_missing_cancelled_and_independent_aggregate_failure_refuse(self):
        cases = [smoke_jobs() + [job('unrelated', identity=103)],
                 [job('kind-smoke-gate', steps=[step('Gate on the kind-smoke result')])],
                 [job('kind-smoke', steps=[])],
                 [job('kind-smoke', steps=[step('Create KinD cluster'), step('Smoke')])],
                 [job('kind-smoke', 'cancelled')],
                 smoke_jobs()[:2] + [job('kind-smoke-gate', identity=102, steps=[step('Set up job')])],
                 smoke_jobs()[:2] + [job('kind-smoke-gate', identity=102, steps=[step('Run actions/checkout@' + 'a' * 40)])]]
        for jobs in cases:
            with self.subTest(jobs=jobs), self.assertRaises(watch.RecoveryRefusal):
                watch.smoke_retry_job(run(), jobs)
        with self.assertRaises(watch.RecoveryRefusal):
            watch.smoke_retry_job(run('cancelled'), smoke_jobs())

    def test_green_requires_real_job_and_workflow_not_aggregate_alone(self):
        good = [job('changes', 'skipped', 100, []), job('kind-smoke', 'success', 101, []),
                job('kind-smoke-gate', 'success', 102, [])]
        self.assertTrue(watch.smoke_success(run('success'), good))
        self.assertFalse(watch.smoke_success(run('success'), good[2:]))
        self.assertFalse(watch.smoke_success(run('failure'), good))
        red_real = [dict(item, conclusion='failure') if item['name'] == 'kind-smoke' else item for item in good]
        self.assertFalse(watch.smoke_success(run('success'), red_real))

    def test_sole_assertion_only_with_known_failed_steps(self):
        for name in ['Require the real tagged KinD smoke job', 'Set up job', 'Run actions/checkout@' + 'c' * 40]:
            candidate = job('assert-tag-smoke', steps=[step(name)])
            self.assertEqual(watch.assertion_retry_job(run(), [candidate])['id'], 101)
        for jobs in [[job('assert-tag-smoke', steps=[step('Require the real tagged KinD smoke job')]), job('publish-api', identity=102)],
                     [job('assert-ci-coverage')], [job('assert-tag-smoke', steps=[])],
                     [job('assert-tag-smoke', steps=[step('Unknown')])]]:
            with self.subTest(jobs=jobs), self.assertRaises(watch.RecoveryRefusal):
                watch.assertion_retry_job(run(), jobs)


class Recovery(unittest.TestCase):
    def test_owned_publication_attempt_jump_exits_policy_refused_without_assertion_post(self):
        for seen in (False, True):
            with self.subTest(seen=seen):
                engine = self.fixture()
                self.green_smoke()
                engine.max_ticks = 1
                engine.requested['release.yml'] = (20, 1)
                if seen:
                    engine.recovery_seen['release.yml'] = (20, 1)
                self.release_run = dict(self.release_run, run_attempt=3)
                engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else ''
                engine.dump = lambda _: (0, 'completed\tfailure\tassert-tag-smoke\thttps://example.invalid/job')
                self.assertEqual(engine.run(), 1)
                self.assertEqual(engine.stop_reason, 'tag-smoke-recovery-refused')
                engine.gh.assert_not_called()
                self.assertEqual(engine.requested['release.yml'], (20, 1))
                self.assertEqual(engine.assert_remaining, 1)

    def test_first_publication_observation_pins_epoch_before_smoke_recovers(self):
        engine = self.fixture()
        engine.observe('release.yml', '20')
        self.release_run = dict(self.release_run, run_attempt=2)
        self.green_smoke()
        engine.max_ticks = 1
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else ''
        engine.dump = lambda _: (0, 'completed\tfailure\tassert-tag-smoke\thttps://example.invalid/job')
        self.assertEqual(engine.run(), 1)
        self.assertEqual(engine.stop_reason, 'tag-smoke-recovery-refused')
        engine.gh.assert_not_called()

    def test_publication_identity_read_can_recover_on_next_tick_with_fresh_proof(self):
        engine = self.fixture()
        self.publication_poll(engine)
        engine.max_ticks = 3
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else '30'
        publication_dump = engine.dump
        engine.dump = lambda identity: (0, 'completed\tsuccess\tpublish-formula\thttps://example.invalid/job') if identity == '30' else publication_dump(identity)
        engine.wait_requeued = Mock()
        engine.validate_identity.side_effect = [watch.RecoveryReadUnavailable('temporary read failure'), None]
        def posted(*args):
            self.release_run = dict(self.release_run, conclusion='success', run_attempt=2)
            self.release_jobs = [dict(item, conclusion='success') for item in self.release_jobs]
            return 0, '', ''
        engine.gh.side_effect = posted
        self.assertEqual(engine.poll(), 0)
        self.assertEqual(engine.validate_identity.call_count, 2)
        self.assertEqual(engine.gh.call_count, 1)
        self.assertEqual(engine.remaining['release.yml'], 1)

    def test_unavailable_new_class_write_identity_is_instrument_exit_three(self):
        engine = self.fixture()
        engine.validate_identity.side_effect = watch.RecoveryReadUnavailable('unavailable refs')
        self.assertEqual(engine.run(), 3)
        engine.gh.assert_not_called()
        self.assertEqual(engine.smoke_remaining, 2)

    def test_both_assertion_workflow_contracts_are_checked(self):
        for workflow in ['release.yml', 'kind-smoke.yml']:
            engine = self.fixture()
            self.green_smoke()
            def contract(name):
                if name == workflow:
                    raise watch.RecoveryRefusal('changed contract')
            engine.require_contract.side_effect = contract
            with self.subTest(workflow=workflow), self.assertRaises(watch.RecoveryRefusal):
                engine.recover_assertion('20')
            engine.gh.assert_not_called()

    def test_job_only_cancellation_stops_owned_recovery(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.smoke_run = run(attempt=2)
        self.smoke_jobs[1]['conclusion'] = 'cancelled'
        with self.assertRaises(watch.RecoveryRefusal):
            engine.track_recovery('kind-infra', self.smoke_run, self.smoke_jobs)
        self.assertEqual(engine.gh.call_count, 1)
        self.assertEqual(engine.smoke_remaining, 1)

    def test_external_attempt_guards_are_independent(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.smoke_run = run(attempt=3)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.track_recovery('kind-infra', self.smoke_run, self.smoke_jobs)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_snapshot('kind-smoke.yml')
        self.assertEqual(engine.gh.call_count, 1)

    def test_human_rerun_after_completed_recovery_is_external(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.green_smoke()
        self.smoke_run['run_attempt'] = 2
        self.assertEqual(engine.recovery_tick(), 'green')
        self.assertEqual(engine.recovery_requests, {})
        self.smoke_run['run_attempt'] = 3
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_snapshot('kind-smoke.yml')
        self.assertEqual(engine.gh.call_count, 1)

    def test_own_publication_completion_updates_seen_epoch(self):
        engine = self.fixture()
        engine.recovery_seen['release.yml'] = (20, 1)
        engine.requested['release.yml'] = (20, 1)
        self.release_run = dict(self.release_run, run_attempt=2, conclusion='success')
        self.release_jobs = [dict(item, conclusion='success', steps=[]) for item in self.release_jobs]
        engine.record('release.yml', '20')
        self.assertNotIn('release.yml', engine.requested)
        self.assertEqual(engine.recovery_snapshot('release.yml')[0]['run_attempt'], 2)

    def publication_poll(self, engine):
        self.green_smoke()
        engine.max_ticks = 2
        self.release_jobs = [dict(item, conclusion='failure' if item['name'] == 'publish-api' else 'success', steps=[]) for item in self.release_jobs]
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else ''
        engine.dump = lambda _identity: (0, '\n'.join(f"{item['status']}\t{item['conclusion']}\t{item['name']}\t{item['html_url']}" for item in self.release_jobs))

    def test_publication_identity_read_failure_refuses_write_and_observes_success(self):
        engine = self.fixture()
        self.publication_poll(engine)
        def unavailable():
            self.release_jobs = [dict(item, conclusion='success') for item in self.release_jobs]
            raise watch.RecoveryReadUnavailable('read unavailable')
        engine.validate_identity.side_effect = unavailable
        engine.resolve = lambda workflow: '20'
        self.assertEqual(engine.poll(), 0)
        engine.gh.assert_not_called()
        self.assertEqual(engine.remaining['release.yml'], 2)

    def test_other_assertion_name_never_enters_exception_route(self):
        engine = self.fixture()
        self.green_smoke()
        engine.max_ticks = 1
        self.release_jobs[-1]['name'] = 'assert-tag-smoke-other'
        engine.resolve = lambda workflow: '20'
        engine.dump = lambda _: (0, 'completed\tfailure\tassert-tag-smoke-other\thttps://example.invalid/job')
        engine.recover_assertion = Mock(return_value=True)
        self.assertEqual(engine.poll(), 1)
        engine.recover_assertion.assert_not_called()
        engine.gh.assert_not_called()

    def fixture(self):
        self.now = 0
        engine = watch.Watcher('0.2.0', sleep=lambda _: None, clock=lambda: self.now)
        engine.repository, engine.sha, engine.kind = 'fixture/repo', 'a' * 40, 'stable'
        engine.journal, engine.main_api = Mock(), Mock()
        engine.validate_identity = Mock()
        engine.require_contract = Mock()
        engine.enable_recovery('b' * 40)
        self.smoke_run, self.smoke_jobs = run(), smoke_jobs()
        self.release_run = dict(run('failure'), id=20)
        self.release_jobs = [job(name, 'success', identity=200 + i, steps=[])
                             for i, name in enumerate(['prep', 'assert-version', 'assert-changelog', 'assert-ci-coverage',
                                                        'assert-release-metadata', 'assert-worker-pin', 'assert-agent-version',
                                                        'publish-api', 'publish-web', 'publish-controller',
                                                        'publish-agent (base)', 'publish-agent (jvm)'])]
        self.release_jobs += [job('publish-chart', 'skipped', 297, []), job('publish-release', 'skipped', 298, [])]
        self.release_jobs += [job('assert-tag-smoke', identity=299, steps=[step('Require the real tagged KinD smoke job')])]
        engine.main_api.run.side_effect = lambda workflow, *_: self.smoke_run if workflow == 'kind-smoke.yml' else self.release_run
        engine.snapshot = Mock(side_effect=lambda workflow, *_args, **_kwargs:
                               (self.smoke_run, self.smoke_jobs) if workflow == 'kind-smoke.yml'
                               else (self.release_run, self.release_jobs))
        engine.gh = Mock(return_value=(0, '', ''))
        return engine

    def green_smoke(self):
        self.smoke_run = run('success')
        self.smoke_jobs = [job('changes', 'skipped', 100, []), job('kind-smoke', 'success', 101, []),
                           job('kind-smoke-gate', 'success', 102, [])]

    def test_two_kind_reservations_are_not_replenished_and_old_attempt_waits(self):
        engine = self.fixture()
        self.assertEqual(engine.recovery_tick(), 'pending')
        self.assertEqual(engine.smoke_remaining, 1)
        self.assertEqual(engine.gh.call_count, 1)
        self.assertEqual(engine.recovery_tick(), 'pending')
        self.assertEqual(engine.gh.call_count, 1)
        self.smoke_run = run(attempt=2)
        self.assertEqual(engine.recovery_tick(), 'pending')
        self.assertEqual(engine.smoke_remaining, 0)
        self.smoke_run = run(attempt=3)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        self.assertEqual(engine.gh.call_count, 2)
        self.assertEqual(engine.remaining, {'release.yml': 2, 'brew.yml': 2})

    def test_cancelled_rerun_consumes_reservation_and_stops(self):
        for empty, attempt in [(False, 1), (True, 1), (False, 2), (True, 2)]:
            with self.subTest(empty=empty, attempt=attempt):
                engine = self.fixture()
                engine.recovery_tick()
                self.smoke_run = run('cancelled', attempt=attempt)
                self.smoke_jobs = [] if empty else [dict(item, conclusion='cancelled') for item in self.smoke_jobs]
                with self.assertRaises(watch.RecoveryRefusal):
                    engine.recovery_tick()
                self.assertEqual(engine.smoke_remaining, 1)
                self.assertEqual(engine.gh.call_count, 1)

    def test_queued_attempt_without_jobs_retains_request_then_records_success(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.smoke_run = dict(run(None, attempt=2), status='queued')
        self.smoke_jobs = []
        self.assertEqual(engine.recovery_tick(), 'pending')
        self.assertIn('kind-infra', engine.recovery_requests)
        self.assertEqual(engine.smoke_remaining, 1)
        self.assertEqual(engine.gh.call_count, 1)
        self.green_smoke()
        self.smoke_run['run_attempt'] = 2
        self.assertEqual(engine.recovery_tick(), 'green')
        self.assertNotIn('kind-infra', engine.recovery_requests)
        self.assertEqual(engine.gh.call_count, 1)

    def test_new_release_prerequisite_miss_refuses_fresh_assertion_write(self):
        engine = self.fixture()
        self.green_smoke()
        calls = 0
        def snapshot(workflow, *_args, **_kwargs):
            nonlocal calls
            if workflow == 'kind-smoke.yml':
                return self.smoke_run, self.smoke_jobs
            calls += 1
            jobs = self.release_jobs if calls == 1 else [item for item in self.release_jobs if item['name'] != 'assert-ci-coverage']
            return self.release_run, jobs
        engine.snapshot.side_effect = snapshot
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recover_assertion('20')
        engine.gh.assert_not_called()

    def test_reservation_is_saved_before_targeted_post(self):
        engine = self.fixture()
        def post(*args):
            self.assertEqual(args[:3], ('api', '--method', 'POST'))
            self.assertEqual(engine.smoke_remaining, 1)
            self.assertIsNotNone(engine.journal.decision.call_args)
            self.assertEqual(engine.journal.decision.call_args.args[-1], 'reserved')
            self.assertEqual(engine.journal.decision.call_args.kwargs['target_job'], 101)
            return 0, '', ''
        engine.gh.side_effect = post
        engine.recovery_tick()

    def test_mixed_release_failure_blocks_new_recovery_writes(self):
        engine = self.fixture()
        self.release_jobs += [job('publish-api', identity=300)]
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        engine.gh.assert_not_called()

    def test_targeted_assertion_once_keeps_publication_budgets(self):
        engine = self.fixture()
        self.green_smoke()
        self.assertTrue(engine.recover_assertion('20'))
        self.assertEqual(engine.assert_remaining, 0)
        self.assertEqual(engine.gh.call_args.args[:3], ('api', '--method', 'POST'))
        self.assertIn('actions/jobs/299/rerun', engine.gh.call_args.args[3])
        self.assertEqual(engine.remaining, {'release.yml': 2, 'brew.yml': 2})
        self.assertFalse(engine.recover_assertion('20'))
        self.assertEqual(engine.gh.call_count, 1)
        self.release_run = dict(self.release_run, run_attempt=2)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recover_assertion('20')
        self.assertEqual(engine.gh.call_count, 1)

    def test_deadline_prevents_writes_but_not_later_success_observation(self):
        engine = self.fixture()
        self.now = 7201
        self.assertEqual(engine.recovery_tick(), 'pending')
        engine.gh.assert_not_called()
        self.assertEqual(engine.smoke_remaining, 2)
        self.green_smoke()
        self.assertEqual(engine.recovery_tick(), 'green')

    def test_uncertain_post_is_not_refunded_or_duplicated(self):
        engine = self.fixture()
        engine.gh.return_value = 1, '', ''
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        self.assertEqual(engine.smoke_remaining, 1)
        self.assertEqual(engine.gh.call_count, 1)

    def test_real_journal_records_class_target_and_affected_scope(self):
        engine = self.fixture()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            engine.journal = watch.journals.Journal(path, 'fixture', engine.repository, engine.tag, engine.sha)
            engine.recovery_tick()
            decisions = watch.journals.read(path)['decisions']
            self.assertEqual([item['state'] for item in decisions], ['reserved', 'requested'])
            self.assertEqual(decisions[0]['reason'], 'kind-infra')
            self.assertEqual(decisions[0]['target_job'], 101)
            self.assertEqual(decisions[0]['affected_jobs'], ['kind-smoke', 'kind-smoke-gate'])

    def test_tag_identity_stable_supersession_and_blob_guard(self):
        engine = self.fixture()
        del engine.validate_identity
        del engine.require_contract
        refs = f"{'b' * 40}\trefs/tags/v0.2.0\n{'a' * 40}\trefs/tags/v0.2.0^{{}}"
        engine.git = Mock(return_value=refs)
        engine.validate_identity()
        engine.git.return_value = refs.replace('b' * 40, 'c' * 40)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.validate_identity()
        engine.git.return_value = refs.replace('a' * 40, 'c' * 40)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.validate_identity()
        engine.git.return_value = refs + f"\n{'d' * 40}\trefs/tags/v0.3.0"
        with self.assertRaises(watch.RecoveryRefusal):
            engine.validate_identity()
        engine.git = Mock(side_effect=['a' * 40, 'b' * 40])
        with self.assertRaises(watch.RecoveryRefusal):
            engine.require_contract('kind-smoke.yml')

    def test_end_to_end_kind_then_assertion_keeps_all_three_ledgers(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.green_smoke()
        self.smoke_run['run_attempt'] = 2
        self.assertEqual(engine.recovery_tick(), 'green')
        engine.recover_assertion('20')
        self.release_run = dict(self.release_run, run_attempt=2, conclusion='success')
        self.release_jobs = [dict(item, conclusion='success', steps=[]) for item in self.release_jobs]
        engine.track_recovery('tag-smoke-assertion', self.release_run, self.release_jobs)
        self.assertEqual((engine.smoke_remaining, engine.assert_remaining), (1, 0))
        self.assertEqual(engine.remaining, {'release.yml': 2, 'brew.yml': 2})
        self.assertEqual(engine.gh.call_count, 2)

    def test_poll_integrates_both_recoveries_without_nested_publication_loop(self):
        engine = self.fixture()
        engine.max_ticks = 5
        brew = dict(run('success'), id=30)
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else '30'
        def snapshot(workflow, *_args, **_kwargs):
            if workflow == 'kind-smoke.yml':
                return self.smoke_run, self.smoke_jobs
            if workflow == 'release.yml':
                return self.release_run, self.release_jobs
            return brew, [job('publish-formula', 'success', 401, [])]
        engine.snapshot.side_effect = snapshot
        def dump(identity):
            if identity == '30':
                return 0, 'completed\tsuccess\tpublish-formula\thttps://example.invalid/job'
            return 0, '\n'.join(f"{item['status']}\t{item['conclusion']}\t{item['name']}\t{item['html_url']}" for item in self.release_jobs)
        engine.dump = dump
        def post(*args):
            endpoint = args[3]
            if '/101/' in endpoint:
                self.green_smoke()
                self.smoke_run['run_attempt'] = 2
            elif '/299/' in endpoint:
                self.release_run = dict(self.release_run, run_attempt=2, conclusion='success')
                self.release_jobs = [dict(item, conclusion='success', steps=[]) for item in self.release_jobs]
            else:
                self.fail(f'non-targeted recovery request: {args}')
            return 0, '', ''
        engine.gh.side_effect = post
        self.assertEqual(engine.poll(), 0)
        self.assertEqual(engine.gh.call_count, 2)
        self.assertEqual((engine.smoke_remaining, engine.assert_remaining), (1, 0))
        self.assertEqual(engine.remaining, {'release.yml': 2, 'brew.yml': 2})

    def test_expiry_after_reservation_never_sends_or_refunds(self):
        engine = self.fixture()
        def saved(*args, **_kwargs):
            if args[-1] == 'reserved':
                self.now = 7201
        engine.journal.decision.side_effect = saved
        self.assertEqual(engine.recovery_tick(), 'pending')
        engine.gh.assert_not_called()
        self.assertEqual(engine.smoke_remaining, 1)

    def test_changed_contract_or_external_attempt_refuses_before_post(self):
        engine = self.fixture()
        engine.require_contract.side_effect = watch.RecoveryRefusal('unknown contract')
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        engine.gh.assert_not_called()
        engine.require_contract.side_effect = None
        engine.recovery_tick()
        self.smoke_run = run(attempt=3)
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        self.assertEqual(engine.gh.call_count, 1)
        self.assertEqual(engine.smoke_remaining, 1)

    def test_green_old_contract_remains_observable_without_exception(self):
        engine = self.fixture()
        self.green_smoke()
        engine.require_contract.side_effect = watch.RecoveryRefusal('old workflow')
        self.assertEqual(engine.recovery_tick(), 'green')
        engine.require_contract.assert_not_called()
        engine.gh.assert_not_called()

    def test_latest_smoke_must_still_be_green_before_assertion_post(self):
        engine = self.fixture()
        self.green_smoke()
        calls = 0
        def snapshot(workflow, *_args, **_kwargs):
            nonlocal calls
            if workflow != 'kind-smoke.yml':
                return self.release_run, self.release_jobs
            calls += 1
            if calls == 2:
                self.smoke_run = run('failure')
                self.smoke_jobs = smoke_jobs('Smoke')
            return self.smoke_run, self.smoke_jobs
        engine.snapshot.side_effect = snapshot
        self.assertFalse(engine.recover_assertion('20'))
        engine.gh.assert_not_called()
        self.assertEqual(engine.assert_remaining, 1)

    def test_fresh_kind_failure_classification_is_rechecked(self):
        engine = self.fixture()
        calls = 0
        def snapshot(workflow, *_args, **_kwargs):
            nonlocal calls
            if workflow == 'release.yml':
                return self.release_run, self.release_jobs
            calls += 1
            if calls == 2:
                self.smoke_jobs = smoke_jobs('Smoke')
            return self.smoke_run, self.smoke_jobs
        engine.snapshot.side_effect = snapshot
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        engine.gh.assert_not_called()

    def test_identity_refusal_is_checked_before_any_post(self):
        engine = self.fixture()
        engine.validate_identity.side_effect = watch.RecoveryRefusal('changed tag')
        with self.assertRaises(watch.RecoveryRefusal):
            engine.recovery_tick()
        engine.gh.assert_not_called()
        self.assertEqual(engine.smoke_remaining, 2)

    def test_unreadable_observation_is_pending_and_counted_without_remote_write(self):
        engine = self.fixture()
        engine.main_api.run.side_effect = watch.evidence.Refusal('fixture unavailable')
        self.assertEqual(engine.recovery_tick(), 'pending')
        engine.journal.read_unavailable.assert_called_once_with('kind-smoke.yml')
        engine.gh.assert_not_called()
        self.assertEqual(engine.smoke_remaining, 2)

    def test_assertion_requires_full_supported_role_multiset_and_results(self):
        for changed in ('missing', 'unknown', 'duplicate', 'skipped', 'neutral', 'dependent-success', 'dependent-neutral'):
            with self.subTest(changed=changed):
                engine = self.fixture()
                self.green_smoke()
                if changed == 'missing':
                    self.release_jobs = [item for item in self.release_jobs if item['name'] != 'publish-api']
                elif changed == 'unknown':
                    self.release_jobs += [job('surprise-role', 'success', 999, [])]
                elif changed == 'duplicate':
                    self.release_jobs += [job('publish-api', 'success', 999, [])]
                elif changed.startswith('dependent-'):
                    self.release_jobs = [dict(item, conclusion=changed.split('-', 1)[1]) if item['name'] == 'publish-chart' else item for item in self.release_jobs]
                else:
                    self.release_jobs = [dict(item, conclusion=changed) if item['name'] == 'publish-api' else item for item in self.release_jobs]
                with self.assertRaises(watch.RecoveryRefusal):
                    engine.recover_assertion('20')
                engine.gh.assert_not_called()

    def test_publication_expiry_during_reservation_keeps_observing_without_post(self):
        engine = self.fixture()
        self.green_smoke()
        self.now, engine.max_ticks = 7199, 2
        self.release_jobs[-1] = dict(self.release_jobs[-1], conclusion='success', steps=[])
        self.release_jobs = [dict(item, conclusion='failure') if item['name'] == 'publish-api' else item for item in self.release_jobs]
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else '30'
        engine.wait_requeued = Mock()
        def dump(identity):
            if identity == '30':
                return 0, 'completed\tsuccess\tpublish-formula\thttps://example.invalid/job'
            return 0, '\n'.join(f"{item['status']}\t{item['conclusion']}\t{item['name']}\t{item['html_url']}" for item in self.release_jobs)
        engine.dump = dump
        def saved(*args, **_kwargs):
            if args[-1] == 'reserved':
                self.now = 7201
            if args[-1] == 'blocked':
                self.release_run = dict(self.release_run, conclusion='success')
                self.release_jobs = [dict(item, conclusion='success', steps=[]) for item in self.release_jobs]
        engine.journal.decision.side_effect = saved
        self.assertEqual(engine.poll(), 0)
        engine.gh.assert_not_called()
        self.assertEqual(engine.remaining['release.yml'], 1)

    def test_deadline_also_fences_ordinary_publication_retry(self):
        engine = self.fixture()
        self.green_smoke()
        self.now, engine.max_ticks = 7201, 1
        self.release_jobs[-1] = dict(self.release_jobs[-1], conclusion='success', steps=[])
        self.release_jobs += [job('publish-api', identity=300)]
        engine.resolve = lambda workflow: '20' if workflow == 'release.yml' else ''
        engine.dump = lambda _identity: (0, '\n'.join(
            f"{item['status']}\t{item['conclusion']}\t{item['name']}\t{item['html_url']}" for item in self.release_jobs))
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(engine.poll(), 2)
        engine.gh.assert_not_called()
        self.assertEqual(engine.remaining, {'release.yml': 2, 'brew.yml': 2})
        self.assertNotIn('auto-rerunning', output.getvalue())

    def test_unknown_run_conclusion_preserves_recovery_observation(self):
        engine = self.fixture()
        engine.recovery_tick()
        self.smoke_run = run(None, attempt=2)
        self.smoke_jobs = [job('changes', 'skipped', 100, []), job('kind-smoke', 'success', 101, []),
                           job('kind-smoke-gate', 'success', 102, [])]
        self.assertEqual(engine.recovery_tick(), 'pending')
        self.assertIn('kind-infra', engine.recovery_requests)
        self.smoke_run['conclusion'] = 'success'
        self.assertEqual(engine.recovery_tick(), 'green')
        self.assertNotIn('kind-infra', engine.recovery_requests)
        self.assertEqual(engine.gh.call_count, 1)


if __name__ == '__main__':
    unittest.main()
