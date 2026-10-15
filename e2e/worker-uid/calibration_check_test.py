"""Docker-free raw-evidence regressions. Every positive case is SYNTHETIC."""
import copy
import json
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

    def test_literal_pairs_and_earliest_ties(self):
        derivation = self.verify()[0]["derivation"]
        self.assertEqual(derivation["samplePairs"][2], {
            "fromSampleId": 6, "toSampleId": 7, "rawDeltaBytes": -37748736,
            "elapsedMs": 250, "positiveRateBytesPerMs": 0, "readGapMs": 249})
        self.assertEqual(derivation["peakPositiveGrowthPair"]["fromSampleId"], 4)
        self.assertEqual(derivation["maxSampleGapPair"]["fromSampleId"], 4)
        self.assertEqual(derivation["baselineJitter"]["minSampleId"], 1)
        self.assertEqual(derivation["baselineJitter"]["maxSampleId"], 1)

    def test_peak_growth_tie_selects_earliest_pair(self):
        report = self.reports[0]
        raw = report["rawSamples"][3]
        raw["sample"]["currentBytes"] = 109051904
        raw["sample"]["stat"]["anon"] = 109051904
        raw["raw"]["/sys/fs/cgroup/memory.current"] = "109051904\n"
        raw["raw"]["/sys/fs/cgroup/memory.stat"] = "anon 109051904\nshmem 0\nslab_unreclaimable 0\nunevictable 0\n"
        derivation = report["derivation"]
        derivation["samplePairs"][0].update(rawDeltaBytes=16777216, positiveRateBytesPerMs=67108.864)
        derivation.update(peakPositiveGrowthPair=copy.deepcopy(derivation["samplePairs"][0]),
                          maxSampleGapPair=copy.deepcopy(derivation["samplePairs"][0]),
                          peakPositiveGrowthRateBytesPerMs=67108.864, netGrowthBytes=-4194304)
        derivation["reserveCheck"]["requiredBytes"] = 17448304.64
        self.verify()
        derivation["peakPositiveGrowthPair"] = copy.deepcopy(derivation["samplePairs"][1])
        with self.assertRaisesRegex(ValueError, "derivation mismatch"):
            self.verify()

    def test_pair_and_scheduling_refusals(self):
        mutations = [
            lambda r: r.pop("samplingSchedule"),
            lambda r: r["samplingSchedule"]["rows"].pop(),
            lambda r: r["samplingSchedule"]["rows"].reverse(),
        ]
        for field, value in (("fromSampleId", 5), ("toSampleId", 6), ("rawDeltaBytes", 1),
                             ("elapsedMs", 249), ("positiveRateBytesPerMs", 0), ("readGapMs", 250)):
            mutations.append(lambda r, k=field, v=value: r["derivation"]["samplePairs"][0].update({k: v}))
        for field in ("peakPositiveGrowthPair", "maxSampleGapPair"):
            mutations.append(lambda r, k=field: r["derivation"][k].update(fromSampleId=8))
        for field in ("minSampleId", "maxSampleId", "minBytes", "maxBytes"):
            mutations.append(lambda r, k=field: r["derivation"]["baselineJitter"].update({k: 2}))
        for field, value in (("sampleId", 4), ("requestedOrdinal", 0), ("requestedOrdinal", True),
                             ("dueAtMs", 1251), ("dispatchAtMs", 1251), ("dispatchAtMs", 0),
                             ("startedAtMs", 1251), ("completedAtMs", 1252), ("latenessMs", True)):
            mutations.append(lambda r, k=field, v=value: r["samplingSchedule"]["rows"][1].update({k: v}))
        for field, value in (("anchorMs", -1), ("anchorMs", True), ("anchorMs", 1001),
                             ("periodMs", 251), ("maxPositiveLatenessMs", 1), ("maxAbsoluteJitterMs", 1)):
            mutations.append(lambda r, k=field, v=value: r["samplingSchedule"].update({k: v}))
        for index, mutate in enumerate(mutations):
            with self.subTest(index=index):
                self.setUp()
                mutate(self.reports[0])
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    self.verify()

    def test_signed_early_and_late_scheduling_is_valid(self):
        report = self.reports[0]
        report["samples"][0]["startedAtMs"] = 999
        report["samples"][3]["startedAtMs"] = 1751
        rows = report["samplingSchedule"]["rows"]
        rows[0].update(startedAtMs=999, dispatchAtMs=998, latenessMs=-1)
        rows[3].update(startedAtMs=1751, latenessMs=1)
        report["samplingSchedule"].update(maxPositiveLatenessMs=1, maxAbsoluteJitterMs=1)
        derivation = report["derivation"]
        derivation["samplePairs"][2]["readGapMs"] = 250
        derivation.update(elapsedMs=1252, maxSampleGapMs=250,
                          maxSampleGapPair=copy.deepcopy(derivation["samplePairs"][2]))
        self.verify()

    def test_quiet_samples_shifted_exactly_100_seconds(self):
        report = self.reports[0]
        for sample in report["samples"][3:]:
            sample["startedAtMs"] += 100000
            sample["completedAtMs"] += 100000
        report["derivation"].update(elapsedMs=101251, maxSampleGapMs=100249)
        with self.assertRaises(ValueError):
            self.verify()

    def test_captured_process_window_includes_preload_time(self):
        # Remove shared fixture objects so each timestamp is shifted once.
        self.manifest, self.reports, self.cleanup = json.loads(json.dumps(case()))
        times = {"startedAtMs", "completedAtMs", "startMs", "endMs", "lowerMs", "upperMs", "atMs", "anchorMs", "dueAtMs", "dispatchAtMs"}

        def shift(value):
            if isinstance(value, dict):
                for key, child in value.items():
                    if key in times and isinstance(child, (int, float)):
                        value[key] = child + 65000
                    else:
                        shift(child)
            elif isinstance(value, list):
                for child in value:
                    shift(child)

        shift(self.reports[0])
        first, second = self.manifest["fixtures"]
        proof = self.cleanup[first["fixtureId"]]
        proof["hostMonotonicEnd"] = proof["hostMonotonicStart"] + 29
        later = self.cleanup[second["fixtureId"]]
        later["hostMonotonicStart"] = proof["hostMonotonicEnd"] + 1
        later["hostMonotonicEnd"] = later["hostMonotonicStart"] + 1
        for report in self.reports:
            report["hostCleanup"] = self.cleanup[report["fixtureId"]]
        with self.assertRaisesRegex(ValueError, "60s captured fixture"):
            self.verify()

    def test_unexpected_primary_exit_is_not_cancelled_step_evidence(self):
        for index in (0, 1):
            for code in (False, True, 0, 1, -9, 137.5, None, "137"):
                with self.subTest(index=index, code=code):
                    self.setUp()
                    self.reports[0]["trees"][index]["exits"][0]["code"] = code
                    with self.assertRaises((ValueError, TypeError)):
                        self.verify()
            self.setUp()
            self.reports[0]["trees"][index]["exits"][0].pop("code")
            with self.assertRaises(KeyError):
                self.verify()

    def test_independent_phase_and_window_refusals(self):
        mutations = [
            ("final before quiet", lambda r: r["finalCleanup"].update(startMs=1521, endMs=1522)),
            ("wall", lambda r: r["finalCleanup"].update(startMs=90000, endMs=90001)),
            ("load after baseline", lambda r: r["settlement"]["schedule"][1].update(startMs=0)),
            ("step after cleanup", lambda r: r["settlement"]["schedule"][1].update(endMs=1600)),
            ("disposal before ACK", lambda r: r["trees"][0]["disposals"][0].update(startMs=0)),
            ("sibling disposal before cleanup", lambda r: r["trees"][1]["disposals"][0].update(startMs=1511)),
            ("owned cleanup budget", lambda r: r["response"]["legs"][0]["spans"]["cleanup"].update(endMs=32000)),
            ("raw after final", lambda r: r["rawSamples"].append({
                **copy.deepcopy(r["rawSamples"][-1]),
                "sample": {**copy.deepcopy(r["rawSamples"][-1]["sample"]),
                           "sampleId": 10, "startedAtMs": 3000, "completedAtMs": 3001}})),
        ]
        for name, mutate in mutations:
            with self.subTest(name=name):
                self.manifest, self.reports, self.cleanup = case()
                mutate(self.reports[0])
                with self.assertRaises(ValueError):
                    self.verify()

    def test_every_request_context_is_host_and_ack_bound(self):
        alternatives = {"run_id": ["foreign-run", "33333333-3333-4333-8333-333333333333"],
                        "worker_id": ["44444444-4444-4444-8444-444444444444"],
                        "register_nonce": ["foreign-nonce"], "claim_generation": [2, True],
                        "memory_episode": [1, False]}
        for key, values in alternatives.items():
            for value in values:
                for targets in ("requests", "ack", "both"):
                    with self.subTest(key=key, value=value, targets=targets):
                        self.manifest, self.reports, self.cleanup = case()
                        settlement = self.reports[0]["settlement"]
                        if targets in ("requests", "both"):
                            for record in settlement["serviceRecords"] + settlement["history"]:
                                record["request"][key] = value
                        if targets in ("ack", "both"):
                            for record in settlement["acknowledgements"]:
                                record["ack"][key] = value
                        with self.assertRaises(ValueError):
                            self.verify()

    def test_disposal_starts_before_control_write(self):
        # The wrapper starts timing before the nested supervisor write.
        for report in self.reports:
            report["trees"][0]["disposals"][0]["startMs"] = 1505.5
        self.verify()

    def test_valid_raw_prefix_and_final_read(self):
        for report in self.reports:
            prefix = copy.deepcopy(report["rawSamples"][0])
            prefix["sample"].update(sampleId=0, startedAtMs=0, completedAtMs=0)
            final = copy.deepcopy(report["rawSamples"][-1])
            final["sample"].update(sampleId=10, startedAtMs=2252, completedAtMs=2253)
            report["rawSamples"] = [prefix, *report["rawSamples"], final]
            report["oomEvents"].update(fromSampleId=0, toSampleId=10)
        self.verify()

    def test_coherent_evidence_beyond_fixture_wall(self):
        # Decode first so each raw/projected timestamp moves exactly once.
        self.reports = check.reports(encode(self.reports))
        time_keys = {"startMs", "endMs", "startedAtMs", "completedAtMs", "atMs", "lowerMs", "upperMs", "anchorMs", "dueAtMs", "dispatchAtMs"}
        def shift(value):
            if isinstance(value, dict):
                for key, child in value.items():
                    if key in time_keys:
                        value[key] += 90000
                    else:
                        shift(child)
            elif isinstance(value, list):
                for child in value:
                    shift(child)
        shift(self.reports[0])
        with self.assertRaisesRegex(ValueError, "90s fixture wall"):
            self.verify()

    def test_nonfinite_fractional_and_negative_metrics(self):
        for key, value in (("elapsedMs", float("inf")), ("jitterBytes", -1),
                           ("netGrowthBytes", float("nan"))):
            with self.subTest(key=key, value=value):
                self.manifest, self.reports, self.cleanup = case()
                self.reports[0]["derivation"][key] = value
                with self.assertRaises(ValueError):
                    self.verify()
        for key, value in (("claim_generation", -1), ("memory_episode", 0.5)):
            with self.subTest(key=key):
                self.manifest, self.reports, self.cleanup = case()
                self.manifest["context"][key] = value
                with self.assertRaises(ValueError):
                    self.verify()

    def test_json_boolean_number_fidelity(self):
        # Traverse every owned boolean in the complete version-1 corpus.
        def leaves(value, path=()):
            if isinstance(value, dict):
                for key, child in value.items():
                    yield from leaves(child, (*path, key))
            elif isinstance(value, list):
                for index, child in enumerate(value):
                    yield from leaves(child, (*path, index))
            elif type(value) is bool or (type(value) is int and value in (0, 1)):
                yield path, value
        original = copy.deepcopy(self.reports)
        for path, value in leaves(original[0]):
            with self.subTest(path=path):
                self.reports = copy.deepcopy(original)
                target = self.reports[0]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = int(value) if type(value) is bool else bool(value)
                with self.assertRaises(ValueError):
                    self.verify()

    def test_json_integral_numbers_and_valid_replay(self):
        # JSON.stringify can serialize an integral float without a decimal.
        def floats(value):
            if isinstance(value, dict):
                return {k: floats(v) for k, v in value.items()}
            if isinstance(value, list):
                return [floats(v) for v in value]
            return float(value) if type(value) is int else value
        self.reports = floats(self.reports)
        self.verify()
        self.verify()

    def test_explicit_metadata_corruption_is_refused(self):
        mutations = [
            lambda r: r["plannedChargeClasses"][0].update(kind="anonymous"),
            lambda r: r["plannedChargeClasses"][0].update(producerCount=1),
            lambda r: r["plannedChargeClasses"][0].update(producerCount=True),
            lambda r: r["plannedChargeClasses"][0].update(chunkBytes=1),
            lambda r: r["plannedChargeClasses"][0].update(maxRoundsPerProducer=32),
            lambda r: r["plannedChargeClasses"][0].update(combinedBudgetBytes=67108864),
            lambda r: r["plannedChargeClasses"].reverse(),
            lambda r: r["units"].update(growth="bytes_per_second"),
            lambda r: r["units"].update(processStartIdentity="milliseconds"),
            lambda r: r["units"].update(events=True),
            lambda r: r["oomEvents"].update(fromSampleId=4),
            lambda r: r["oomEvents"].update(toSampleId=8),
            lambda r: r["oomEvents"]["before"].update(oom=1),
            lambda r: r["oomEvents"]["after"].update(oom_kill=1),
            lambda r: r["oomEvents"]["delta"].update(oom_group_kill=1),
            lambda r: r["oomEvents"]["delta"].update(oom=-1),
        ]
        for section in ("before", "after", "delta"):
            for value in (None, False, "0"):
                mutations.append(lambda r, s=section, v=value: r["oomEvents"][s].update(oom=v))
            mutations.append(lambda r, s=section: r["oomEvents"][s].pop("oom"))
        for key in self.reports[0]["units"]:
            mutations.append(lambda r, k=key: r["units"].update({k: "wrong"}))
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                self.setUp()
                mutate(self.reports[0])
                with self.assertRaises((ValueError, KeyError, TypeError)):
                    self.verify()

    def test_host_aborted_metadata_is_unknown_and_cannot_pass(self):
        import calibration
        host = calibration.Host(self.manifest, None, "unused")
        report = host.aborted(self.manifest["fixtures"][0], "before producer")
        for key in ("units", "plannedChargeClasses", "oomEvents"):
            self.assertIsNone(report[key])
        self.reports[0] = report
        with self.assertRaises((ValueError, KeyError, TypeError)):
            self.verify()

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
