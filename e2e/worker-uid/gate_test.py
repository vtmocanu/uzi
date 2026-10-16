"""Exercise the actual required-check shell body across dependency outcomes."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import textwrap
import unittest


class RequiredCheckTest(unittest.TestCase):
    def test_agent_dependencies_are_confined_to_provisioned_lane(self):
        root = Path(__file__).resolve().parents[2]
        taskfile = (root / 'Taskfile.yml').read_text()
        harness = taskfile.split('  test:worker-uid-harness:\n', 1)[1].split('  test:agent:worker-uid:', 1)[0]
        lane = taskfile.split('  test:agent:worker-uid:\n', 1)[1].split('  test:agent:shard:', 1)[0]
        self.assertNotIn('cd agent', harness)
        for command in ('calibration-producer.test.mts', 'inventory.test.mts',
                        'tsc --project ../e2e/worker-uid/tsconfig.json'):
            self.assertIn(command, lane)
        workflow = (root / '.github/workflows/ci.yml').read_text()
        job = workflow.split('  test-agent-worker-uid:\n', 1)[1].split('\n  test-agent:', 1)[0]
        self.assertLess(job.index('run: npm ci --ignore-scripts'),
                        job.index('run: task test:agent:worker-uid'))

    def test_calibration_failure_propagates_in_ci_and_local_modes(self):
        root = Path(__file__).resolve().parents[2]
        taskfile = (root / 'Taskfile.yml').read_text()
        target = taskfile.split('  test:agent:worker-uid:\n', 1)[1].split('  test:agent:shard:', 1)[0]
        self.assertIn('      - bash e2e/worker-uid/run.sh\n', target)
        body = textwrap.dedent(target.split('      - |\n', 1)[1])
        self.assertIn('e2e/worker-uid/calibration.py', body)
        scratch = root / '.uzi/scratch'
        scratch.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='calibration-task.', dir=scratch) as directory:
            fake = Path(directory) / 'python3'
            fake.write_text('#!/bin/sh\nexit 37\n')
            fake.chmod(0o755)
            for ci in ('true', 'false'):
                with self.subTest(ci=ci):
                    env = {**os.environ, 'PATH': directory + os.pathsep + os.environ['PATH'],
                           'GITHUB_ACTIONS': ci, 'WORKER_MEMORY_CALIBRATION_RUN_ID': 'local-regression',
                           'WORKER_MEMORY_CALIBRATION_ATTEMPT': '1'}
                    result = subprocess.run(['bash', '-c', body], env=env,
                                            capture_output=True, timeout=5, check=False)
                    self.assertEqual(result.returncode, 37, result.stderr)

    def test_required_task_preserves_standalone_report_lines(self):
        root = Path(__file__).resolve().parents[2]
        taskfile = (root / 'Taskfile.yml').read_text()
        target = taskfile.split('  test:agent:worker-uid:\n', 1)[1].split('  test:agent:shard:', 1)[0]
        task = shutil.which('task')
        self.assertIsNotNone(task, 'Task is required to verify job stdout framing')
        scratch = root / '.uzi/scratch'
        scratch.mkdir(parents=True, exist_ok=True)
        begin = 'UZI_MEMORY_CALIBRATION_REPORT_BEGIN'
        end = 'UZI_MEMORY_CALIBRATION_REPORT_END'
        with tempfile.TemporaryDirectory(prefix='calibration-task-frame.', dir=scratch) as directory:
            directory = Path(directory)
            (directory / 'e2e/worker-uid').mkdir(parents=True)
            (directory / 'agent').mkdir()
            for command in ('node', 'npx'):
                stub_command = directory / command
                stub_command.write_text('#!/bin/sh\nexit 0\n')
                stub_command.chmod(0o755)
            (directory / 'e2e/worker-uid/run.sh').write_text('exit 0\n')
            fake = directory / 'python3'
            # Framing only: this synthetic probe is deliberately not a measurement.
            fake.write_text("#!/bin/sh\nprintf '%s\\n' '" + begin +
                            "' '{\"synthetic\":true}' '" + end +
                            "'\nexit \"" + '$' + "{CALIBRATION_TEST_EXIT:-0}\"\n")
            fake.chmod(0o755)
            env = {**os.environ, 'PATH': str(directory) + os.pathsep + os.environ['PATH'],
                   'GITHUB_ACTIONS': 'true'}
            candidate = directory / 'Taskfile.yml'
            header = "version: '3'\noutput: prefixed\ntasks:\n"
            stub = "  test:worker-uid-harness:\n    cmds:\n      - 'true'\n"
            for direct in (False, True):
                body = target if direct else target.replace('    interactive: true\n', '')
                candidate.write_text(header + '  test:agent:worker-uid:\n' + body + stub)
                result = subprocess.run([task, '--taskfile', str(candidate), 'test:agent:worker-uid'],
                                        cwd=directory, env=env, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                lines = result.stdout.splitlines()
                if direct:
                    self.assertEqual(lines.count(begin), 1)
                    self.assertEqual(lines.count(end), 1)
                    self.assertEqual(json.loads(lines[lines.index(begin) + 1]), {'synthetic': True})
                    failed = subprocess.run([task, '--taskfile', str(candidate), 'test:agent:worker-uid'],
                                            cwd=directory, env={**env, 'CALIBRATION_TEST_EXIT': '37'},
                                            capture_output=True, text=True, timeout=10)
                    self.assertNotEqual(failed.returncode, 0)
                else:
                    self.assertNotIn(begin, lines)
                    self.assertTrue(any(line.endswith(begin) for line in lines))

    def test_dependency_truth_table(self):
        workflow = (Path(__file__).resolve().parents[2] / '.github/workflows/ci.yml').read_text()
        step = workflow.split('- name: Require shards and the applicable worker UID lane to succeed\n', 1)[1]
        body = step.split('        run: |\n', 1)[1].split('      - uses:', 1)[0]
        script = textwrap.dedent(body)
        outcomes = ('success', 'skipped', 'failure', 'cancelled')
        for event in ('push', 'pull_request'):
            for changes in outcomes:
                for changed in ('true', 'false', ''):
                    for lane in outcomes:
                        for shards in ('success', 'failure'):
                            allowed_skip = event == 'pull_request' and changes == 'success' and changed == 'false'
                            expected = shards == 'success' and (lane == 'success' or (allowed_skip and lane == 'skipped'))
                            values = dict(CI_EVENT=event, CHANGES_RESULT=changes, WORKER_CHANGED=changed,
                                          WORKER_RESULT=lane, SHARD_RESULT=shards)
                            with self.subTest(**values):
                                result = subprocess.run(['bash', '-c', script], env={**os.environ, **values},
                                                        capture_output=True, check=False)
                                self.assertEqual(result.returncode == 0, expected, result.stdout)


if __name__ == '__main__':
    unittest.main()
