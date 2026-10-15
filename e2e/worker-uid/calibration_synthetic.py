"""Explicitly SYNTHETIC checker corpus; never a real measurement or CLI acceptance."""
import copy
import json

import calibration_check as check


def case():
    identity = {"sourceSha": "a" * 40, "runId": "synthetic-only", "attempt": "1", "mode": "local"}
    context = {"run_id": "11111111-1111-4111-8111-111111111111",
               "worker_id": "22222222-2222-4222-8222-222222222222", "register_nonce": "synthetic-nonce",
               "claim_generation": 1, "memory_episode": 0}
    manifest = {"schemaVersion": 1, "identity": identity, "context": context, "synthetic": True,
                "invocation": "synthetic-invocation", "fixtures": []}
    reports, cleanup = [], {}
    mib = 1024 ** 2
    for size in (1, 2):
        limit = size * 1024 ** 3
        expected = {"fixtureId": f"fixture-{size}", "legId": f"leg-{size}", "reportId": f"report-{size}",
                    "treeIds": [f"tree-{size}-0", f"tree-{size}-1"], "limitBytes": limit,
                    "containerId": str(size) * 64, "containerName": f"synthetic-{size}"}
        manifest["fixtures"].append(expected)
        binding = {"reportId": expected["reportId"], "legId": expected["legId"],
                   "interventionId": f"intervention-{size}", "treeId": expected["treeIds"][0],
                   "toolId": "tool-0", "reservationId": f"intervention-{size}"}
        psi = {"some": {"avg10": 0, "avg60": 0, "avg300": 0, "total": 0},
               "full": {"avg10": 0, "avg60": 0, "avg300": 0, "total": 0}}
        raw_samples = []
        # Fixed independent oracle: growth 20MiB/250ms, bracket [1250,1501],
        # drain1510 =>260ms, reserve requirement20.8MiB. No sample-gap addition.
        for n, (start, current) in enumerate(((0, 100), (250, 100), (500, 100),
                                            (1000, 100), (1250, 120), (1500, 136),
                                            (1750, 100), (2000, 100), (2250, 100)), 1):
            stat = {"anon": current * mib, "shmem": 0, "slab_unreclaimable": 0, "unevictable": 0}
            events = {"oom": 0, "oom_kill": 0, "oom_group_kill": 0}
            sample = {"available": True, "sampleId": n, "startedAtMs": start, "completedAtMs": start + 1,
                      "currentBytes": current * mib, "limitBytes": limit, "stat": stat, "pressure": copy.deepcopy(psi)}
            raw = {"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/memory.current": str(current * mib) + "\n",
                   "/sys/fs/cgroup/memory.max": str(limit) + "\n",
                   "/sys/fs/cgroup/memory.stat": "".join(f"{k} {v}\n" for k, v in stat.items()),
                   "/sys/fs/cgroup/memory.pressure": "some avg10=0 avg60=0 avg300=0 total=0\nfull avg10=0 avg60=0 avg300=0 total=0\n",
                   "/sys/fs/cgroup/memory.events": "oom 0\noom_kill 0\noom_group_kill 0\n"}
            raw_samples.append({"sample": sample, "raw": raw, "events": events})
        baseline = [r["sample"] for r in raw_samples[:3]]
        samples = [r["sample"] for r in raw_samples[3:]]
        bracket = {"lastBelowSampleId": 5, "firstAtOrAboveSampleId": 6, "lowerMs": 1250, "upperMs": 1501}
        times = {"observe": (1502, 1503), "reservation": (1504, 1505), "signal": (1506, 1507),
                 "drain": (1507, 1510), "cleanup": (1512, 1520)}
        spans = {k: {"binding": copy.deepcopy(binding), "startMs": a, "endMs": b} for k, (a, b) in times.items()}
        leg = {"binding": binding, "hardThresholdBytes": 132 * mib, "hardBracket": bracket, "spans": spans,
               "ack": {"binding": binding, "atMs": 1505, "accepted": True},
               "exit": {"binding": binding, "atMs": 1510, "confirmed": True},
               "cleanup": {"binding": binding, "confirmed": True}, "oomBefore": 0, "oomAfter": 0,
               "oomKillsBefore": 0, "oomKillsAfter": 0, "oomGroupKillsBefore": 0, "oomGroupKillsAfter": 0}
        dleg = {"binding": binding, "hardBracket": bracket, "observationToDrainMs": 8,
                "drainMinusBracketLowerMs": 260,
                "stageLatenciesMs": {"crossingToObserveEnd": 253, "reservation": 2, "signal": 2, "drain": 3}}
        reserve = limit - 132 * mib
        derivation = {"fixtureOnly": True, "provenance": "synthetic", "reportId": expected["reportId"], "legs": [dleg],
                      "elapsedMs": 1251, "jitterBytes": 0, "peakPositiveGrowthRateBytesPerMs": 20 * mib / 250,
                      "maxSampleGapMs": 249, "worstEndToEndLatencyMs": 260, "conservativeStageSumMs": 260,
                      "netGrowthBytes": 0, "monotonicGrowth": False, "responseMeasured": False, "accepted": False,
                      "reserveCheck": {"reserveBytes": reserve, "requiredBytes": 20 * mib / 250 * 260, "satisfied": True}}
        trees = []
        for index in (0, 1):
            enrollment = {k: context[k] for k in ("run_id", "worker_id", "claim_generation", "memory_episode")}
            enrollment.update(attempt_id=f"attempt-{index}", command_id=f"command-{index}", tool_id=f"tool-{index}",
                              command=f"fixed synthetic command {index}")
            trees.append({"treeId": expected["treeIds"][index], "identity": {"pid": 100 + index, "startIdentity": str(200 + index)},
                          "enrollment": enrollment, "exits": [{"index": index, "atMs": 1508 if index == 0 else 1515, "code": 137}],
                          "disposals": [{"index": index, "startMs": 1506 if index == 0 else 1512,
                                         "endMs": 1510 if index == 0 else 1518, "clean": True,
                                         "evidence": {"clean": True, "event": {"state": "drained", "authority": "ECHILD+__WALL"}}}]})
        pressure = {**trees[0]["enrollment"], "intervention_id": binding["interventionId"], "code": "worker_memory_pressure",
                    "observed_total_bytes": 136 * mib, "limit_bytes": limit, "tree_rss_bytes_approx": 10 * mib,
                    "outcome": "confirmed_drained", "cancellation_confirmed": True, "retry_blocked": True,
                    "guidance": "Use smaller or serial commands after recovery."}
        request = {**context, "intervention_id": binding["interventionId"], "policy": {"version": 1, "max_interventions": 2}}
        ack = {**request, "admitted": True, "authorizing": True, "allowance": {"limit": 2, "used": 1, "remaining": 1}}
        outcome_ack = {**ack, "authorizing": False, "outcome": "confirmed_drained"}
        settlement = {"selectedIndex": 0, "feedback": pressure, "broker": {"ok": False, "code": "worker_memory_pressure",
                      "memoryPressure": pressure}, "survival": {"index": 1, "identity": trees[1]["identity"],
                      "atMs": 1511, "observation": {"state": "complete", "processes": [{"pid": 101, "startTime": "201"}]}},
                      "monitorState": {"armed": True, "pendingIntervention": False, "unconfirmedDrain": False,
                                       "unresolvedOutcome": False, "interventions": 1},
                      "acknowledgements": [{"path": "/reservation", "startMs": 1504, "endMs": 1505, "status": 200, "ack": ack},
                                           {"path": "/outcome", "startMs": 1510, "endMs": 1511, "status": 200, "ack": outcome_ack}],
                      "serviceRecords": [{"request": request, "ack": ack, "path": "/reservation", "atMs": 1504.5},
                                         {"request": {**request, "outcome": "confirmed_drained"},
                                          "ack": outcome_ack, "path": "/outcome", "atMs": 1510.5}],
                      "history": [{"request": request, "status": "admitted", "outcome": "confirmed_drained"}],
                      "observations": [{"index": 0, "startMs": 1502, "endMs": 1503},
                                       {"index": 0, "startMs": 1505.1, "endMs": 1505.9}],
                      "signalWrites": [{"index": 0, "startMs": 1506, "endMs": 1507}],
                      "schedule": [{"index": 0, "step": 1, "startMs": 1000, "endMs": 1511, "kind": "cancelled"},
                                   {"index": 1, "step": 1, "startMs": 1000, "endMs": 1511, "kind": "completed"}]}
        rounds = size * 16
        reports.append({"schemaVersion": 1, **identity, **expected, "provenance": "synthetic", "status": "SUCCESS_COMPLETE",
                        "reasons": [], "clock": {"source": "node:performance", "unit": "ms", "timeOrigin": 1},
                        "signalMeaning": "application supervisor disposal control write", "ackService": "LOCALFIXTURE",
                        "bounds": {"maxSamples": 268, "rawSampleBytes": 6144, "reportBytes": check.CAP,
                                   "samplePeriodMs": 250, "loadMs": 30000, "cleanupMs": 30000},
                        "observed": {"uid": 10003, "noNewPrivs": True, "capsZero": True, "membership": "0::/",
                                     "limitBytes": limit, "diskType": 123, "shmemType": 0x01021994},
                        "rawSamples": raw_samples, "baseline": baseline, "samples": samples,
                        "guard": {"enabled": True, "reserveBytes": reserve, "hysteresisBytes": 8 * mib,
                                  "sampleMs": 250, "responseBudgetMs": 5000, "rearmMs": 500, "maxInterventions": 2},
                        "plan": {"limitBytes": limit, "ceilingBytes": limit // 2, "baselineBytes": 100 * mib,
                                 "allocationBytes": 8 * rounds * mib, "overheadBytes": 64 * mib,
                                 "chunkBytes": mib, "rounds": rounds, "windowMs": 30000},
                        "response": {"provenance": "synthetic", "reportId": expected["reportId"], "legs": [leg]},
                        "derivation": derivation, "trees": trees, "settlement": settlement,
                        "cleanup": {"confirmed": True, "startMs": 1512, "endMs": 1520},
                        "finalCleanup": {"startMs": 1521, "endMs": 1522, "handles": 2, "joined": ["fulfilled", "fulfilled"], "driverCompleted": True,
                                         "clean": [{"clean": True}, {"clean": True}]},
                        "outcome": {"status": "failed", "reasons": ["worker_memory_pressure"], "successor": "aborted"}})
        cleanup[expected["fixtureId"]] = {"confirmed": True, "containerId": expected["containerId"],
                                         "removedId": expected["containerId"], "label": manifest["invocation"],
                                         "hostMonotonicStart": 1, "hostMonotonicEnd": 2}
        reports[-1]["hostCleanup"] = cleanup[expected["fixtureId"]]
    return manifest, reports, cleanup


def encode(reports):
    return "".join(check.BEGIN + "\n" + json.dumps(r, separators=(",", ":")) + "\n" + check.END + "\n"
                   for r in reports).encode()
