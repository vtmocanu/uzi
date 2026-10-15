"""Docker-free raw-evidence regressions. Every positive case is SYNTHETIC."""
import copy
import unittest

import calibration_check as check
from calibration_synthetic import case, encode


class CheckerTest(unittest.TestCase):
    def setUp(self):
        self.manifest, self.reports, self.cleanup = case()

    def verify(self):
        return check.verify(self.manifest, encode(self.reports), self.cleanup, synthetic=True)

    def test_valid_two_sizes_are_never_measurement(self):
        values = self.verify()
        self.assertEqual([v["limitBytes"] for v in values], [1024**3, 2 * 1024**3])
        for value in values:
            self.assertFalse(value["derivation"]["accepted"])
            self.assertFalse(value["derivation"]["responseMeasured"])
            self.assertEqual(value["derivation"]["worstEndToEndLatencyMs"], 260)
            self.assertEqual(value["derivation"]["legs"][0]["observationToDrainMs"], 8)
        with self.assertRaises(ValueError):
            check.verify(self.manifest, encode(values), self.cleanup)

    def test_every_top_level_field_is_required(self):
        original = copy.deepcopy(self.reports)
        for key in original[0]:
            with self.subTest(key=key):
                self.reports = copy.deepcopy(original)
                del self.reports[0][key]
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    self.verify()

    def test_report_refusals(self):
        # Mutate independent evidence, never recompute expected arithmetic.
        mutations = [
            ("version", lambda r: r.update(schemaVersion=2)),
            ("bool version", lambda r: r.update(schemaVersion=True)),
            ("identity", lambda r: r.update(sourceSha="b" * 40)),
            ("size", lambda r: r.update(limitBytes=2 * 1024**3)),
            ("failed", lambda r: r.update(status="FAILED")),
            ("reason", lambda r: r.update(reasons=["aborted"])),
            ("missing PSI", lambda r: r["rawSamples"][0]["raw"].pop("/sys/fs/cgroup/memory.pressure")),
            ("raw mismatch", lambda r: r["rawSamples"][0]["raw"].update({"/sys/fs/cgroup/memory.current": "1\n"})),
            ("bad PSI", lambda r: r["rawSamples"][0]["raw"].update({"/sys/fs/cgroup/memory.pressure": "some avg10=nan\n"})),
            ("changed L", lambda r: r["samples"][2].update(limitBytes=1)),
            ("unavailable", lambda r: r["samples"][2].update(available=False)),
            ("bool counter", lambda r: r["samples"][2].update(currentBytes=True)),
            ("bracket", lambda r: r["response"]["legs"][0]["hardBracket"].update(lowerMs=1000)),
            ("nonadjacent", lambda r: r["response"]["legs"][0]["hardBracket"].update(lastBelowSampleId=4)),
            ("early observe", lambda r: r["response"]["legs"][0]["spans"]["observe"].update(startMs=1500)),
            ("zero duration", lambda r: r["response"]["legs"][0]["spans"]["signal"].update(endMs=1506)),
            ("ack after signal", lambda r: r["response"]["legs"][0]["ack"].update(atMs=1508)),
            ("exit unconfirmed", lambda r: r["response"]["legs"][0]["exit"].update(confirmed=False)),
            ("cleanup", lambda r: r["cleanup"].update(confirmed=False)),
            ("OOM", lambda r: r["rawSamples"][-1]["events"].update(oom_kill=1)),
            ("arithmetic", lambda r: r["derivation"].update(worstEndToEndLatencyMs=8)),
            ("extra gap", lambda r: r["derivation"].update(worstEndToEndLatencyMs=509)),
            ("reserve", lambda r: r["derivation"]["reserveCheck"].update(requiredBytes=0)),
            ("binding", lambda r: r["response"]["legs"][0]["spans"]["signal"]["binding"].update(toolId="other")),
            ("feedback", lambda r: r["settlement"]["feedback"].update(cancellation_confirmed=False)),
            ("broker", lambda r: r["settlement"]["broker"].update(ok=True)),
            ("sibling reused", lambda r: r["settlement"]["survival"]["observation"]["processes"][0].update(startTime="999")),
            ("sibling after cleanup", lambda r: r["settlement"]["survival"].update(atMs=1521)),
            ("no exit", lambda r: r["trees"][1].update(exits=[])),
            ("no join", lambda r: r["finalCleanup"].update(joined=["fulfilled", "rejected"])),
            ("ACK denied", lambda r: r["settlement"]["acknowledgements"][0]["ack"].update(admitted=False)),
            ("ACK stale incarnation", lambda r: r["settlement"]["acknowledgements"][0]["ack"].update(register_nonce="old")),
            ("service mismatch", lambda r: r["settlement"]["serviceRecords"][0].update(path="/other")),
            ("no quiet dwell", lambda r: r["samples"].pop()),
            ("completed relabel", lambda r: r["settlement"]["schedule"][0].update(kind="completed")),
            ("step already over", lambda r: r["settlement"]["schedule"][0].update(endMs=1500)),
            ("step after cross", lambda r: r["settlement"]["schedule"][0].update(startMs=1502)),
            ("invented write", lambda r: r["settlement"]["signalWrites"][0].update(startMs=1505)),
            ("no revalidation", lambda r: r["settlement"]["observations"].pop()),
            ("host proof absent", lambda r: r.update(hostCleanup=None)),
            ("host proof false", lambda r: r["hostCleanup"].update(confirmed=False)),
        ]
        original = copy.deepcopy(self.reports)
        for name, mutate in mutations:
            with self.subTest(name=name):
                self.reports = copy.deepcopy(original)
                mutate(self.reports[0])
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    self.verify()

    def test_reserve_inequality_is_recomputed_before_derivation(self):
        # 20MiB growth in one millisecond exceeds the fixed reserve when charged
        # over the full hard-bracket-to-drain latency.
        low = self.reports[0]["samples"][1]
        low.update(startedAtMs=1001, completedAtMs=1002)
        self.reports[0]["response"]["legs"][0]["hardBracket"]["lowerMs"] = 1001
        with self.assertRaisesRegex(ValueError, "strict reserve inequality"):
            self.verify()

    def test_host_cleanup_sidecar_refusals(self):
        for change in ({"removedId": "3" * 64}, {"label": "foreign"}, {"confirmed": False},
                       {"hostMonotonicEnd": 32}, {"hostMonotonicStart": None}):
            with self.subTest(change=change):
                manifest, reports, cleanup = case()
                cleanup[manifest["fixtures"][0]["fixtureId"]].update(change)
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    check.verify(manifest, encode(reports), cleanup, synthetic=True)

    def test_manifest_refusals(self):
        for key, value in (("sourceSha", "a" * 39), ("runId", ""), ("attempt", ""), ("mode", "other")):
            with self.subTest(key=key):
                self.manifest, self.reports, self.cleanup = case()
                self.manifest["identity"][key] = value
                with self.assertRaises(ValueError):
                    self.verify()
        self.manifest, self.reports, self.cleanup = case()
        self.manifest["fixtures"][1]["containerId"] = self.manifest["fixtures"][0]["containerId"]
        with self.assertRaises(ValueError):
            self.verify()

    def test_framing_and_ingestion_refusals(self):
        valid = encode(self.reports)
        cases = [b"", encode(self.reports[:1]), encode(self.reports + self.reports[:1]),
                 valid[:-20], valid.replace((check.BEGIN + "\n").encode(), (check.BEGIN + "\n" + check.BEGIN + "\n").encode(), 1),
                 valid.replace(check.END.encode(), ("prefix " + check.END).encode(), 1),
                 valid.replace(b'"schemaVersion":1', b'"schemaVersion":1,"schemaVersion":1', 1),
                 valid.replace(b'"schemaVersion":1', b'"schemaVersion":NaN', 1),
                 valid.replace(b'"schemaVersion":1', b'"schemaVersion":\n1', 1),
                 b"x" * (2 * check.CAP + 1), b"\xff" + valid]
        for index, data in enumerate(cases):
            with self.subTest(index=index):
                with self.assertRaises((ValueError, UnicodeError)):
                    check.reports(data)
        with self.assertRaises(ValueError):
            check.verify(self.manifest, encode([self.reports[0]] * 2), self.cleanup, synthetic=True)


if __name__ == "__main__":
    unittest.main()
