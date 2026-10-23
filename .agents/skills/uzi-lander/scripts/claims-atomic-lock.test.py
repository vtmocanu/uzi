#!/usr/bin/env python3
"""A successful CLI mkdir status must not let a contender bypass a held lock."""
import json
import os
import pathlib
import shutil
import signal
import subprocess
import tempfile
import time
import unittest


class AtomicClaimLockTests(unittest.TestCase):
    def test_existing_lock_blocks_a_false_success_mkdir(self):
        script = pathlib.Path(__file__).with_name('claims.sh').resolve()
        with tempfile.TemporaryDirectory(prefix='claim-lock-') as directory:
            root = pathlib.Path(directory)
            state = root / 'state'
            claims = state / 'claims'
            claims.mkdir(parents=True)
            lock = claims / '.lock.run-race'
            lock.mkdir()  # The first holder has not written its claim yet.
            waiting, release = root / 'waiting', root / 'release'
            binary = root / 'bin'
            binary.mkdir()
            (root / 'sitecustomize.py').write_text(
                'from pathlib import Path\nPath(__file__).with_name("untrusted-startup").touch()\n')
            mkdir, sleep = shutil.which('mkdir'), shutil.which('sleep')
            self.assertIsNotNone(mkdir)
            self.assertIsNotNone(sleep)
            # uutils 0.10.0 can report success when a concurrent creator wins.
            # Model that result deterministically, without weakening the lock.
            (binary / 'mkdir').write_text('''#!/bin/sh
if [ "$#" = 1 ] && [ "$1" = "$CLAIM_LOCK_PATH" ]; then exit 0; fi
exec "$REAL_MKDIR" "$@"
''')
            # Observe an actual failed acquisition/retry, rather than sleeping and
            # assuming the contender has reached the critical section.
            (binary / 'sleep').write_text('''#!/bin/sh
if [ "$#" = 1 ] && [ "$1" = 0.3 ]; then
  : > "$CLAIM_WAITING"
  while [ ! -e "$CLAIM_RELEASE" ]; do "$REAL_SLEEP" 0.01; done
  exit 0
fi
exec "$REAL_SLEEP" "$@"
''')
            for path in binary.iterdir():
                path.chmod(0o755)
            env = {**os.environ, 'PATH': str(binary) + os.pathsep + os.environ['PATH'],
                   'UZI_LANDER_STATE_DIR': str(state), 'SESSION_PEERS_NAME': 'claimant',
                   'SESSION_PEERS_UUID': 'uuid-claimant', 'SESSION_PEERS_KIND': 'claude',
                   'CLAIM_LOCK_PATH': str(lock), 'CLAIM_WAITING': str(waiting),
                   'CLAIM_RELEASE': str(release), 'REAL_MKDIR': mkdir, 'REAL_SLEEP': sleep}
            env['PYTHONPATH'] = str(root)
            process = subprocess.Popen(['bash', str(script), 'claim', 'run-race', '--repo', 'test/repo'],
                                       env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       text=True, start_new_session=True)
            try:
                deadline = time.monotonic() + 5
                while not waiting.exists() and process.poll() is None and time.monotonic() < deadline:
                    time.sleep(0.005)
                self.assertTrue(waiting.exists(), 'contender bypassed the held claim lock')
                self.assertIsNone(process.poll(), 'contender must wait for the holder')
                self.assertFalse((claims / 'run-race.json').exists(), 'no claim write while held')
                lock.rmdir()
                release.touch()
                stdout, stderr = process.communicate(timeout=5)
                self.assertEqual(process.returncode, 0, stdout + stderr)
                self.assertIn('CLAIMED=run-race OWNER=claimant', stdout)
                self.assertEqual(json.loads((claims / 'run-race.json').read_text())['owner_uuid'], 'uuid-claimant')
                self.assertFalse(lock.exists(), 'successful holder releases its lock')
                self.assertFalse((root / 'untrusted-startup').exists(),
                                 'lock helper loaded repository Python startup code')
            finally:
                release.touch(exist_ok=True)
                try:
                    process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.communicate()


if __name__ == '__main__':
    unittest.main()
