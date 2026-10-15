"""Exercise the actual required-check shell body across dependency outcomes."""
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


class RequiredCheckTest(unittest.TestCase):
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
