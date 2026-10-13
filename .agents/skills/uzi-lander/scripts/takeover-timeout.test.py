#!/usr/bin/env python3
"""Hermetic #1518 regression: a hung late lookup must finish fail-closed."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import sys


HERE = Path(__file__).resolve().parent


class TakeoverTimeoutTest(unittest.TestCase):
    def deadline(self, seconds, code):
        return subprocess.run([sys.executable, str(HERE / "takeover-deadline.py"), str(seconds),
                               sys.executable, "-c", code], capture_output=True, timeout=5)

    def test_normal_output_and_exit_status_are_preserved(self):
        for status in (0, 3, 4):
            result = self.deadline(3, f"import sys; print('snapshot'); print('diagnostic', file=sys.stderr); sys.exit({status})")
            self.assertEqual(result.returncode, status)
            self.assertEqual(result.stdout, b"snapshot\n")
            self.assertEqual(result.stderr, b"diagnostic\n")

    def test_deadline_kills_descendants_that_ignore_term(self):
        # communicate can complete only if the descendant's inherited pipes close too.
        result = self.deadline(0.2, "import os, signal, time; pid=os.fork(); "
                               "signal.signal(signal.SIGTERM, signal.SIG_IGN) if pid == 0 else None; "
                               "print('started', flush=True); time.sleep(60)")
        self.assertEqual(result.returncode, 124)
        self.assertIn(b"started\n", result.stdout)
        self.assertIn(b"UNKNOWN=1\nNEXT=unknown", result.stdout)

    def test_partial_snapshot_gets_a_fail_closed_trailer(self):
        result = self.deadline(0.2, "import time; print('LIVE_FINDINGS=0', flush=True); time.sleep(60)")
        self.assertEqual(result.returncode, 124)
        self.assertEqual(result.stdout, b"LIVE_FINDINGS=0\nSNAPSHOT_TIMEOUT=1\nUNKNOWN=1\n"
                         b"NEXT=unknown (snapshot deadline exceeded)\n")

    def test_invalid_deadlines_refuse_to_start(self):
        for seconds in ("0", "-1", "nan", "inf", "invalid"):
            result = self.deadline(seconds, "print('must not start')")
            self.assertEqual(result.returncode, 3)
            self.assertNotIn(b"must not start", result.stdout)

    def test_hung_lookup_finishes_with_unknown_trailer(self):
        with tempfile.TemporaryDirectory(prefix="takeover-deadline-") as scratch:
            root = Path(scratch)
            binary = root / "bin"
            binary.mkdir()
            fixture = (HERE / "takeover.test.sh").read_text()
            gh = fixture.split('cat > "$WORK/bin/gh" <<\'STUB\'\n', 1)[1].split("\nSTUB\n", 1)[0]
            # The snapshot has already printed LIVE_FINDINGS when this lookup hangs.
            gh = gh.replace('case "$*" in\n', 'case "$*" in *\'/code-scanning/alerts\'*) exec python3 -c "import time; time.sleep(60)" ;; esac\ncase "$*" in\n', 1)
            for name, content in {"gh": gh, "uzi": "#!/usr/bin/env bash\necho '[]'\n"}.items():
                path = binary / name
                path.write_text(content)
                path.chmod(0o755)
            env = dict(os.environ, PATH=f"{binary}:{os.environ['PATH']}", MODE="head_clean",
                       NET_RETRY_BASE_SLEEP="0", UZI_LANDER_STATE_DIR=str(root / "state"),
                       TAKEOVER_TIMEOUT_SECONDS="3")
            process = subprocess.Popen(["bash", str(HERE / "takeover.sh"), "42", "--repo", "test/repo", "--no-claim"],
                                       env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
            try:
                out, err = process.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, 9)
                out, err = process.communicate()
                self.fail(f"snapshot hung without its trailer: {out.decode()} / {err.decode()}")
            self.assertEqual(process.returncode, 124, err.decode())
            # A loaded host may expire earlier in the snapshot; that must fail closed too.
            self.assertIn(b"UNKNOWN=1\n", out)
            self.assertIn(b"NEXT=unknown", out)
            self.assertNotIn(b"NEXT=ready", out)


if __name__ == "__main__":
    unittest.main()
