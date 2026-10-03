"""Exercise the actual required-check shell body across dependency outcomes."""
import os
from pathlib import Path
import subprocess
import textwrap
import unittest


class RequiredCheckTest(unittest.TestCase):
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
