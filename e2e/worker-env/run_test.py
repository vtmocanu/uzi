"""Docker-free integration tests for run.sh: a fake docker and a pass-through timeout on PATH."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HARNESS = Path(__file__).resolve().parent
REPO = HARNESS.parents[1]
IMAGE_ID = "sha256:" + "a" * 64
RUN_SH_LIMIT = 60  # hang guard for one fake-docker invocation, not a performance budget
PROOFS = json.loads((HARNESS / "proofs.json").read_text())
TEST_FILE = "/app/uzi-worker-uid-proof/" + PROOFS["file"]


def junit(names):
    return "<testsuites>" + "".join(f'<testcase name="{n}" time="0.5" file="{TEST_FILE}"/>' for n in names) + "</testsuites>"


ALL_PASS = junit(PROOFS["leaves"].values())

FAKE_DOCKER = r'''#!%(python)s -S
import json, os, sys
args = sys.argv[1:]
state_path = os.environ["STATE"]
with open(os.environ["CALLS"], "a", encoding="utf-8") as log:
    log.write(json.dumps(args) + "\n")
state = json.load(open(state_path)) if os.path.exists(state_path) else {"volumes": {}, "containers": {}}
def save():
    json.dump(state, open(state_path, "w"))
label = "io.uzi.worker-uid.invocation"
if args[:2] == ["image", "inspect"]:
    if os.environ.get("IMAGE_RC", "0") != "0":
        sys.exit(int(os.environ["IMAGE_RC"]))
    print(os.environ.get("IMAGE_ID_OUT", "%(image_id)s"))
elif args[:2] == ["volume", "create"]:
    name = args[-1]
    state["volumes"][name] = args[args.index("--label") + 1].split("=", 1)[1]
    save(); print(name)
elif args[:2] == ["volume", "inspect"]:
    print(json.dumps([{"Name": args[2], "Labels": {label: state["volumes"][args[2]]}}]))
elif args[:2] == ["volume", "rm"]:
    del state["volumes"][args[2]]; save()
elif args[0] == "run":
    name = args[args.index("--name") + 1]
    token = args[args.index("--label") + 1].split("=", 1)[1]
    state["containers"][name] = {"id": ("%%064x" %% (len(state["containers"]) + 1)), "token": token}
    save()
    if name.endswith("-seed"):
        sys.exit(int(os.environ.get("SEED_RC", "0")))
    sys.stdout.write(os.environ["XML"])
    sys.exit(int(os.environ.get("PROOF_RC", "0")))
elif args[0] == "inspect":
    item = state["containers"][args[1]]
    print(json.dumps([{"Id": item["id"], "Config": {"Labels": {label: item["token"]}}}]))
elif args[:3] == ["rm", "-f", "--volumes"]:
    state["containers"] = {k: v for k, v in state["containers"].items() if v["id"] != args[3]}; save()
else:
    sys.exit(97)
'''


class RunShTest(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp(prefix="wenv-run-test."))
        self.addCleanup(shutil.rmtree, self.dir, ignore_errors=True)
        self.bin = self.dir / "bin"
        self.bin.mkdir()
        (self.bin / "docker").write_text(FAKE_DOCKER % {"python": sys.executable, "image_id": IMAGE_ID})
        (self.bin / "timeout").write_text("#!/bin/sh\nshift 2\nexec \"$@\"\n")
        for name in ("docker", "timeout"):
            (self.bin / name).chmod(0o755)
        self.calls = self.dir / "calls.jsonl"
        self.state = self.dir / "state.json"

    def run_lane(self, xml=ALL_PASS, image="fixture-image:tag", **extra):
        env = {"PATH": str(self.bin) + os.pathsep + os.defpath, "CALLS": str(self.calls), "STATE": str(self.state),
               "XML": xml, "PYTHONDONTWRITEBYTECODE": "1", "WORKER_ENV_IMAGE": image, **extra}
        if image == "":
            del env["WORKER_ENV_IMAGE"]
        result = subprocess.run(["bash", str(HARNESS / "run.sh")], env=env, capture_output=True, text=True,
                                timeout=RUN_SH_LIMIT, check=False)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()] if self.calls.exists() else []
        return result, calls

    def leftovers(self):
        state = json.loads(self.state.read_text()) if self.state.exists() else {"volumes": {}, "containers": {}}
        return state["volumes"], state["containers"]

    def test_pass_shape_and_cleanup(self):
        result, calls = self.run_lane()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"worker env lane: {len(PROOFS['leaves'])} required leaves passed", result.stdout)
        for phase in ("image-identity", "nix-volume-create", "nix-seed", "worker-env-proof"):
            self.assertRegex(result.stderr, rf"worker env phase end: {phase} elapsed=\d+s exit=0")
        runs = [c for c in calls if c[0] == "run"]
        self.assertEqual(len(runs), 2)
        seed, proof = runs
        name = proof[proof.index("--name") + 1]
        self.assertRegex(name, r"^wenv-3894-[0-9a-f]{32}$")  # outside the uzi- namespace
        self.assertEqual(seed[seed.index("--name") + 1], name + "-seed")
        # The image's real Nix store on an invocation-owned volume, never a tmpfs mask.
        self.assertIn(f"type=volume,src={name}-nix,dst=/nix", proof)
        self.assertNotIn("/nix", [proof[i + 1] for i, a in enumerate(proof) if a == "--tmpfs"])
        self.assertEqual(proof[proof.index("--tmpfs") + 1], "/data")
        # Anonymous network is allowed (provisioning), so no --network none; no host env, no secrets.
        self.assertNotIn("--network", proof)
        self.assertEqual(seed[seed.index("--network") + 1], "none")
        envs = [proof[i + 1] for i, a in enumerate(proof) if a in ("--env", "-e")]
        self.assertEqual(envs, ["WORKER_ENV_IMAGE_ID=" + IMAGE_ID])
        self.assertNotIn("--env-file", proof)
        self.assertNotIn("--privileged", proof)
        self.assertNotIn("--rm", proof)
        self.assertEqual(proof[proof.index("--pull") + 1], "never")
        self.assertEqual(proof[proof.index("--cap-drop") + 1], "ALL")
        self.assertEqual([proof[i + 1] for i, a in enumerate(proof) if a == "--cap-add"],
                         ["CHOWN", "DAC_OVERRIDE", "SETPCAP", "SETUID", "SETGID"])
        self.assertEqual(proof[proof.index("--security-opt") + 1], "no-new-privileges")
        self.assertEqual(proof[proof.index("--entrypoint") + 1], "/usr/local/sbin/uzi-entrypoint")
        # Both containers run the inspected image by immutable id, never the mutable tag.
        self.assertEqual(proof[proof.index("--entrypoint") + 2], IMAGE_ID)
        self.assertEqual(seed[seed.index("--entrypoint") + 2], IMAGE_ID)
        # JUnit stays alone on stdout (the checker input); a spec reporter on stderr carries the
        # captured diagnostics of passing leaves into the job log.
        i = proof.index("--test-reporter=junit")
        self.assertEqual(proof[i:i + 4], ["--test-reporter=junit", "--test-reporter-destination=stdout",
                                          "--test-reporter=spec", "--test-reporter-destination=stderr"])
        self.assertEqual(proof[-1], "/app/uzi-worker-uid-proof/e2e/worker-env/worker-env-real.test.mts")
        mounts = [proof[i + 1] for i, a in enumerate(proof) if a == "--mount"]
        self.assertIn(f"type=bind,src={REPO},dst=/src,readonly", mounts)
        self.assertTrue(all(m.endswith(",readonly") for m in mounts if m.startswith("type=bind,")), mounts)
        # Everything the lane created is removed by exact identity.
        self.assertEqual(self.leftovers(), ({}, {}))
        self.assertEqual(len([c for c in calls if c[:3] == ["rm", "-f", "--volumes"]]), 2)

    def test_unexecuted_or_failed_leaf_fails_the_lane(self):
        names = list(PROOFS["leaves"].values())
        for label, xml in (("missing", junit(names[:-1])), ("duplicate", junit(names + names[:1])),
                           ("failed", ALL_PASS.replace('time="0.5" file="' + TEST_FILE + '"/>', 'time="0.5" file="' + TEST_FILE + '"><failure message="boom"/></testcase>', 1)),
                           ("skipped", ALL_PASS.replace('time="0.5" file="' + TEST_FILE + '"/>', 'time="0.5" file="' + TEST_FILE + '"><skipped/></testcase>', 1))):
            with self.subTest(label=label):
                self.calls.unlink(missing_ok=True)
                result, _ = self.run_lane(xml=xml)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                self.assertIn("worker env lane FAIL", result.stderr)
                self.assertEqual(self.leftovers(), ({}, {}))

    def test_container_failure_fails_even_with_a_passing_report(self):
        result, _ = self.run_lane(PROOF_RC="3")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("worker env node/container exit: 3", result.stderr)
        self.assertEqual(self.leftovers(), ({}, {}))

    def test_seed_failure_stops_before_the_proof(self):
        result, calls = self.run_lane(SEED_RC="5")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len([c for c in calls if c[0] == "run"]), 1)
        self.assertEqual(self.leftovers(), ({}, {}))

    def test_image_problems_create_nothing(self):
        for label, kwargs in (("inspect-fails", {"IMAGE_RC": "1"}), ("bad-identity", {"IMAGE_ID_OUT": "not-an-id"}), ("unset", {"image": ""})):
            with self.subTest(label=label):
                self.calls.unlink(missing_ok=True)
                result, calls = self.run_lane(**kwargs)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([c for c in calls if c[0] in ("run", "volume")], [])


if __name__ == "__main__":
    unittest.main()
