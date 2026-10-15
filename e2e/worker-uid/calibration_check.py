#!/usr/bin/env python3
"""Strict raw-evidence checker. Synthetic regression data never accepts measurement."""
import json
import math
from pathlib import Path
import re
import sys

BEGIN = "UZI_MEMORY_CALIBRATION_REPORT_BEGIN"
END = "UZI_MEMORY_CALIBRATION_REPORT_END"
CAP = 8 * 1024 * 1024
MIB = 1024 ** 2
BINDING = ("reportId", "legId", "interventionId", "treeId", "toolId", "reservationId")
EVENTS = ("oom", "oom_kill", "oom_group_kill")


def require(value, message):
    if not value:
        raise ValueError(message)


def number(value):
    require(type(value) in (int, float) and math.isfinite(value) and value >= 0,
            "invalid finite nonnegative number")
    return value


def integer(value):
    number(value)
    require(value == math.floor(value) and value <= 2 ** 53 - 1, "invalid safe integer")
    return int(value)


def same(actual, expected):
    """JSON equality: numbers share a type, but booleans never equal numbers."""
    if type(expected) in (int, float):
        return type(actual) in (int, float) and math.isfinite(actual) and actual == expected
    if type(actual) is not type(expected):
        return False
    if isinstance(expected, dict):
        return actual.keys() == expected.keys() and all(same(actual[k], v) for k, v in expected.items())
    if isinstance(expected, list):
        return len(actual) == len(expected) and all(same(a, b) for a, b in zip(actual, expected))
    return actual == expected


def numeric_fields(value):
    """Validate owned numeric leaves even where correlation uses ordering/indexing."""
    naturals = {"schemaVersion", "limitBytes", "uid", "diskType", "shmemType", "sampleId",
                "currentBytes", "requestedOrdinal", "fromSampleId", "toSampleId", "minSampleId", "maxSampleId", "pid", "index", "step", "selectedIndex", "handles",
                "claim_generation", "memory_episode", "oomBefore", "oomAfter",
                "oomKillsBefore", "oomKillsAfter", "oomGroupKillsBefore", "oomGroupKillsAfter"}
    if isinstance(value, dict):
        for key, child in value.items():
            if key in naturals:
                integer(child)
            elif key == "latenessMs":
                require(type(child) in (int, float) and math.isfinite(child), "invalid signed lateness")
            elif ((key.endswith("Ms") and not isinstance(child, dict)) or
                  key in ("timeOrigin", "hostMonotonicStart", "hostMonotonicEnd")):
                # Container and host clocks remain separate; only validate their units here.
                number(child)
            numeric_fields(child)
    elif isinstance(value, list):
        for child in value:
            numeric_fields(child)


def exact_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate JSON key")
            result[key] = value
        return result
    return json.loads(raw, object_pairs_hook=pairs,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def read_json(file, cap=CAP):
    with open(file, "rb") as stream:
        data = stream.read(cap + 1)
    require(len(data) <= cap, "oversized JSON")
    return exact_json(data)


def reports(data, count=2):
    require(len(data) <= 2 * CAP, "oversized reports")
    lines = data.decode("utf8", errors="strict").splitlines()
    result = []
    active = False
    for line in lines:
        if BEGIN in line or END in line:
            require(line in (BEGIN, END), "markers must stand alone")
        if line == BEGIN:
            require(not active, "nested/duplicate begin")
            active = True
            payload = []
        elif line == END:
            require(active and len(payload) == 1, "missing/unterminated/ambiguous payload")
            require(len(payload[0].encode()) < CAP, "oversized report")
            report = exact_json(payload[0])
            require(isinstance(report, dict), "report must be object")
            result.append(report)
            active = False
        elif active:
            payload.append(line)
            require(len(payload) <= 1, "report must be one compact JSON line")
    require(not active and len(result) == count, "wrong number of complete reports")
    return result


def manifest_check(manifest, synthetic=False):
    require(same(manifest["schemaVersion"], 1), "manifest version")
    context = manifest["context"]
    require(set(context) == {"run_id", "worker_id", "register_nonce", "claim_generation", "memory_episode"},
            "host MemoryBinding fields")
    require(all(isinstance(context[k], str) and context[k]
                for k in ("run_id", "worker_id", "register_nonce")), "host MemoryBinding identity")
    integer(context["claim_generation"])
    integer(context["memory_episode"])
    identity = manifest["identity"]
    require(re.fullmatch("[0-9a-f]{40}", identity["sourceSha"]) is not None, "full host SHA required")
    require(identity["mode"] in ("ci", "local"), "invalid mode")
    require(all(isinstance(identity[k], str) and identity[k] for k in ("runId", "attempt")), "run/attempt required")
    require(manifest.get("synthetic") is synthetic, "synthetic manifest mismatch")
    require([f["limitBytes"] for f in manifest["fixtures"]] == [1024 ** 3, 2 * 1024 ** 3], "two sizes required")
    require(synthetic or re.fullmatch("[0-9a-f]{32}", manifest["invocation"]) is not None, "128-bit invocation label")
    names = []
    for fixture in manifest["fixtures"]:
        names.extend(fixture[k] for k in ("fixtureId", "legId", "reportId"))
        require(len(fixture["treeIds"]) == 2, "exactly two trees")
        names.extend(fixture["treeIds"])
    require(all(isinstance(s, str) and s for s in names) and len(names) == len(set(names)), "unique host IDs")


def bind(value, expected):
    require(isinstance(value, dict) and set(value) == set(BINDING), "binding fields")
    require(all(isinstance(value[k], str) and value[k] and value[k] == expected[k] for k in BINDING),
            "mismatched binding")


def fields(raw):
    result = {}
    for line in raw.strip().splitlines():
        parts = line.split()
        require(len(parts) == 2 and parts[0] not in result and parts[1].isdigit(), "malformed raw counters")
        result[parts[0]] = int(parts[1])
        integer(result[parts[0]])
    return result


def psi(raw):
    result = {}
    for line in raw.strip().splitlines():
        kind, *parts = line.split()
        require(kind in ("some", "full") and kind not in result and len(parts) == 4, "raw PSI rows")
        row = {}
        for item in parts:
            key, value = item.split("=")
            require(key in ("avg10", "avg60", "avg300", "total") and key not in row, "raw PSI keys")
            require(re.fullmatch(r"[0-9]+(?:\.[0-9]+)?", value) is not None, "raw PSI numeric")
            row[key] = int(value) if key == "total" else float(value)
            number(row[key])
            if key != "total":
                require(row[key] <= 100, "raw PSI percent")
            else:
                integer(row[key])
        result[kind] = row
    require(set(result) == {"some", "full"}, "raw PSI missing")
    return result


def verify_report(report, manifest, expected, synthetic=False):
    numeric_fields(report)
    require(same(report["schemaVersion"], 1), "unsupported schema")
    for key, value in {**manifest["identity"], **expected}.items():
        require(same(report[key], value), "wrong host identity " + key)
    require(report["provenance"] == ("synthetic" if synthetic else "measured"), "measurement provenance")
    require(report["status"] == "SUCCESS_COMPLETE" and report["reasons"] == [], "failed/incomplete report")
    require(report["clock"]["source"] == "node:performance" and report["clock"]["unit"] == "ms", "monotonic clock")
    number(report["clock"]["timeOrigin"])
    require(same(report["units"], {
        "memory": "bytes", "monotonicTiming": "milliseconds", "growth": "bytes_per_millisecond",
        "psiAverages": "percent", "psiTotal": "microseconds", "events": "count",
        "processStartIdentity": "opaque_kernel_start_ticks"}), "explicit units")
    require(report["signalMeaning"] == "application supervisor disposal control write", "signal meaning")
    require(report["ackService"] == "LOCALFIXTURE", "ACK provenance")
    require(same(report["bounds"], {"maxSamples": 268, "rawSampleBytes": 6144, "reportBytes": CAP,
                                "samplePeriodMs": 250, "loadMs": 30000, "cleanupMs": 30000}), "evidence bounds")
    observed = report["observed"]
    require(observed["uid"] == 10003 and observed["noNewPrivs"] is True and observed["capsZero"] is True,
            "capless identity")
    limit = expected["limitBytes"]
    require(observed["limitBytes"] == limit and observed["membership"] == "0::/", "private finite cgroup")
    require(observed["diskType"] != 0x01021994 and observed["shmemType"] == 0x01021994, "disk vs tmpfs")
    raw_samples = report["rawSamples"]
    require(6 <= len(raw_samples) <= 268, "raw sample count")
    parsed = {}
    previous = None
    for record in raw_samples:
        sample = record["sample"]
        require(sample["available"] is True and sample["limitBytes"] == limit, "unavailable/changed sample")
        sid = integer(sample["sampleId"])
        start = number(sample["startedAtMs"])
        end = number(sample["completedAtMs"])
        require(end >= start and (previous is None or
                (sid > previous["sampleId"] and start >= previous["completedAtMs"])), "sample order")
        raw = record["raw"]
        require(set(raw) == {"/proc/self/cgroup", *("/sys/fs/cgroup/" + name for name in
                ("memory.current", "memory.max", "memory.stat", "memory.pressure", "memory.events"))},
                "default raw paths")
        require(all(isinstance(v, str) for v in raw.values()) and
                sum(len(v.encode()) for v in raw.values()) <= 6144, "raw bytes unavailable/oversized")
        require(raw["/proc/self/cgroup"] == "0::/\n", "raw membership")
        current = raw["/sys/fs/cgroup/memory.current"].strip()
        maximum = raw["/sys/fs/cgroup/memory.max"].strip()
        require(current.isdigit() and maximum.isdigit(), "raw finite counters")
        integer(sample["currentBytes"]); integer(sample["limitBytes"])
        for value in sample["stat"].values():
            integer(value)
        require(sample["currentBytes"] == int(current) and limit == int(maximum), "raw sample mismatch")
        stat = fields(raw["/sys/fs/cgroup/memory.stat"])
        require(same(sample["stat"], {key: stat[key] for key in
                ("anon", "shmem", "slab_unreclaimable", "unevictable")}), "raw stat mismatch")
        require(same(sample["pressure"], psi(raw["/sys/fs/cgroup/memory.pressure"])), "raw PSI mismatch")
        events = fields(raw["/sys/fs/cgroup/memory.events"])
        require(all(k in events for k in EVENTS) and same(record["events"], events), "raw event mismatch")
        parsed[sid] = sample
        previous = sample
    endpoints = [raw_samples[0], raw_samples[-1]]
    before, after = [fields(r["raw"]["/sys/fs/cgroup/memory.events"]) for r in endpoints]
    require(same(report["oomEvents"], {
        "fromSampleId": endpoints[0]["sample"]["sampleId"], "toSampleId": endpoints[1]["sample"]["sampleId"],
        "before": {k: before[k] for k in EVENTS}, "after": {k: after[k] for k in EVENTS},
        "delta": {k: after[k] - before[k] for k in EVENTS}}), "raw OOM endpoint evidence")
    baseline, samples = report["baseline"], report["samples"]
    for sequence in (baseline, samples):
        require(3 <= len(sequence) <= 256, "baseline/load sample minimum")
        previous = None
        for sample in sequence:
            require(same(parsed[sample["sampleId"]], sample), "sample missing raw evidence")
            require(previous is None or sample["sampleId"] > previous["sampleId"], "sample sequence")
            previous = sample
    require(samples[0]["startedAtMs"] >= baseline[-1]["completedAtMs"], "baseline/load order")
    bmax = max(s["currentBytes"] for s in baseline)
    rounds = 16 if limit == 1024 ** 3 else 32
    require(same(report["plan"], {"limitBytes": limit, "ceilingBytes": limit // 2, "baselineBytes": bmax,
            "allocationBytes": 8 * rounds * MIB, "overheadBytes": 64 * MIB, "chunkBytes": MIB,
            "rounds": rounds, "windowMs": 30000}), "fixed allocation plan")
    require(same(report["plannedChargeClasses"], [
        {"kind": kind, "producerCount": 2, "chunkBytes": MIB, "maxRoundsPerProducer": rounds,
         "combinedBudgetBytes": 2 * rounds * MIB}
        for kind in ("touched_anonymous", "dirty_disk", "fsynced_read_file_cache", "tmpfs_shmem")]),
        "planned charge class budgets")
    require(bmax + 8 * rounds * MIB + 64 * MIB <= limit // 2, "preload 50% budget")
    require(all(s["currentBytes"] + 64 * MIB <= limit // 2 for s in samples), "safety ceiling")
    guard = report["guard"]
    high = limit - integer(guard["reserveBytes"])
    require(same(guard, {"enabled": True, "reserveBytes": limit - high, "hysteresisBytes": 8 * MIB,
            "sampleMs": 250, "responseBudgetMs": 5000, "rearmMs": 500, "maxInterventions": 2}), "guard settings")
    require(high == bmax + 32 * MIB and 0 < high - 8 * MIB < high < limit, "H/Q/L thresholds")
    response = report["response"]
    require(response["provenance"] == report["provenance"] and response["reportId"] == expected["reportId"]
            and len(response["legs"]) == 1, "response identity/leg count")
    leg = response["legs"][0]
    binding = leg["binding"]
    bind(binding, binding)
    require(binding["reportId"] == expected["reportId"] and binding["legId"] == expected["legId"]
            and binding["treeId"] in expected["treeIds"]
            and binding["interventionId"] == binding["reservationId"], "leg binding")
    require(leg["hardThresholdBytes"] == high, "hard threshold")
    index = next((n for n, s in enumerate(samples) if s["currentBytes"] >= high), None)
    require(index is not None and index > 0, "real hard crossing bracket")
    low, above = samples[index - 1:index + 1]
    bracket = {"lastBelowSampleId": low["sampleId"], "firstAtOrAboveSampleId": above["sampleId"],
               "lowerMs": low["startedAtMs"], "upperMs": above["completedAtMs"]}
    require(same(leg["hardBracket"], bracket) and low["currentBytes"] < high, "adjacent hard bracket")
    spans = leg["spans"]
    end = bracket["upperMs"]
    for kind in ("observe", "reservation", "signal", "drain", "cleanup"):
        span = spans[kind]
        bind(span["binding"], binding)
        start = number(span["startMs"])
        stop = number(span["endMs"])
        require(start >= end and stop > start, "invalid/misordered response span")
        end = stop
    bind(leg["ack"]["binding"], binding)
    bind(leg["exit"]["binding"], binding)
    bind(leg["cleanup"]["binding"], binding)
    require(leg["ack"]["accepted"] is True and leg["ack"]["atMs"] == spans["reservation"]["endMs"]
            and leg["ack"]["atMs"] <= spans["signal"]["startMs"], "ACK before signal")
    require(leg["exit"]["confirmed"] is True and leg["exit"]["atMs"] == spans["drain"]["endMs"]
            and leg["cleanup"]["confirmed"] is True, "exit/cleanup proof")
    before, after = raw_samples[0]["events"], raw_samples[-1]["events"]
    for event, prefix in (("oom", "oom"), ("oom_kill", "oomKills"), ("oom_group_kill", "oomGroupKills")):
        require(leg[prefix + "Before"] == before[event] and leg[prefix + "After"] == after[event], "OOM raw binding")
        require(before[event] == after[event], "OOM delta")
        require(all(record["events"][event] == before[event] for record in raw_samples), "intermediate OOM delta")
    elapsed = samples[-1]["completedAtMs"] - samples[0]["startedAtMs"]
    growth = []
    pairs = []
    for low_sample, high_sample in zip(samples, samples[1:]):
        delta_ms = high_sample["completedAtMs"] - low_sample["completedAtMs"]
        require(delta_ms > 0, "nonpositive sample interval")
        delta = high_sample["currentBytes"] - low_sample["currentBytes"]
        growth.append(max(0, delta) / delta_ms)
        pairs.append({"fromSampleId": low_sample["sampleId"], "toSampleId": high_sample["sampleId"],
                      "rawDeltaBytes": delta, "elapsedMs": delta_ms, "positiveRateBytesPerMs": growth[-1],
                      "readGapMs": high_sample["startedAtMs"] - low_sample["completedAtMs"]})
    rate = max(growth)
    latency = spans["drain"]["endMs"] - bracket["lowerMs"]
    jitter = bmax - min(s["currentBytes"] for s in baseline)
    required = rate * latency + jitter
    require(guard["reserveBytes"] > required, "strict reserve inequality")
    dleg = {"binding": binding, "hardBracket": bracket,
            "observationToDrainMs": spans["drain"]["endMs"] - spans["observe"]["startMs"],
            "drainMinusBracketLowerMs": latency, "stageLatenciesMs": {
                "crossingToObserveEnd": spans["observe"]["endMs"] - bracket["lowerMs"],
                "reservation": spans["reservation"]["endMs"] - spans["observe"]["endMs"],
                "signal": spans["signal"]["endMs"] - spans["reservation"]["endMs"],
                "drain": spans["drain"]["endMs"] - spans["signal"]["endMs"]}}
    derivation = {"fixtureOnly": True, "provenance": report["provenance"], "reportId": expected["reportId"],
                  "legs": [dleg], "elapsedMs": elapsed, "jitterBytes": jitter,
                  "samplePairs": pairs,
                  "peakPositiveGrowthPair": max(pairs, key=lambda p: p["positiveRateBytesPerMs"]),
                  "maxSampleGapPair": max(pairs, key=lambda p: p["readGapMs"]),
                  "baselineJitter": {"minSampleId": min(baseline, key=lambda s: s["currentBytes"])["sampleId"],
                                     "minBytes": min(s["currentBytes"] for s in baseline),
                                     "maxSampleId": max(baseline, key=lambda s: s["currentBytes"])["sampleId"],
                                     "maxBytes": bmax},
                  "peakPositiveGrowthRateBytesPerMs": rate,
                  "maxSampleGapMs": max(b["startedAtMs"] - a["completedAtMs"] for a, b in zip(samples, samples[1:])),
                  "worstEndToEndLatencyMs": latency, "conservativeStageSumMs": sum(dleg["stageLatenciesMs"].values()),
                  "netGrowthBytes": samples[-1]["currentBytes"] - samples[0]["currentBytes"],
                  "monotonicGrowth": all(b["currentBytes"] >= a["currentBytes"] for a, b in zip(samples, samples[1:])),
                  "responseMeasured": not synthetic, "accepted": not synthetic,
                  "reserveCheck": {"reserveBytes": guard["reserveBytes"], "requiredBytes": required, "satisfied": True}}
    require(same(report["derivation"], derivation), "recomputed derivation mismatch")
    sampling = report["samplingSchedule"]
    require(same(sampling["periodMs"], 250), "sampling period")
    anchor = number(sampling["anchorMs"])
    rows = sampling["rows"]
    require(len(rows) == len(samples), "sampling row count")
    expected_rows = []
    for ordinal, (row, sample) in enumerate(zip(rows, samples)):
        dispatch = number(row["dispatchAtMs"])
        require(dispatch <= sample["startedAtMs"] and
                (ordinal == 0 or dispatch >= samples[ordinal - 1]["completedAtMs"]), "sampling dispatch order")
        due = anchor + ordinal * 250
        expected_rows.append({"sampleId": sample["sampleId"], "requestedOrdinal": ordinal,
                              "dueAtMs": due, "dispatchAtMs": dispatch,
                              "startedAtMs": sample["startedAtMs"], "completedAtMs": sample["completedAtMs"],
                              "latenessMs": sample["startedAtMs"] - due})
    require(same(sampling, {"periodMs": 250, "anchorMs": anchor, "rows": expected_rows,
                           "maxPositiveLatenessMs": max(0, *(r["latenessMs"] for r in expected_rows)),
                           "maxAbsoluteJitterMs": max(abs(r["latenessMs"]) for r in expected_rows)}),
            "recomputed sampling schedule mismatch")
    settlement = report["settlement"]
    trees = report["trees"]
    require(len(trees) == 2 and [t["treeId"] for t in trees] == expected["treeIds"], "two enrolled trees")
    selected = integer(settlement["selectedIndex"])
    require(same(selected, 0) or same(selected, 1), "selected index")
    require(binding["treeId"] == trees[selected]["treeId"], "selected binding")
    for tree in trees:
        integer(tree["identity"]["pid"])
        require(tree["identity"]["pid"] > 0 and re.fullmatch("[1-9][0-9]*", tree["identity"]["startIdentity"]), "saved identity")
        require(len(tree["exits"]) == 1 and len(tree["disposals"]) == 1, "owned exit/disposal count")
        require(tree["disposals"][0]["clean"] is True, "owned disposal")
        require(tree["disposals"][0]["evidence"]["clean"] is True and
                tree["disposals"][0]["evidence"]["event"]["state"] == "drained", "actual disposal evidence")
        enrollment = tree["enrollment"]
        for key in ("run_id", "worker_id", "claim_generation", "memory_episode"):
            require(same(enrollment[key], manifest["context"][key]), "enrollment context")
    require(trees[0]["identity"]["pid"] != trees[1]["identity"]["pid"], "distinct primaries")
    pressure = settlement["feedback"]
    require(settlement["broker"]["ok"] is False and settlement["broker"]["code"] == "worker_memory_pressure"
            and same(settlement["broker"]["memoryPressure"], pressure), "exact broker feedback")
    require(all(same(pressure[k], v) for k, v in trees[selected]["enrollment"].items()), "feedback enrollment")
    require(pressure["intervention_id"] == binding["interventionId"] and pressure["tool_id"] == binding["toolId"]
            and pressure["limit_bytes"] == limit and pressure["code"] == "worker_memory_pressure"
            and pressure["outcome"] == "confirmed_drained" and pressure["cancellation_confirmed"] is True
            and pressure["retry_blocked"] is True and pressure["guidance"] == "Use smaller or serial commands after recovery.",
            "actual pressure result")
    integer(pressure["observed_total_bytes"]); integer(pressure["tree_rss_bytes_approx"])
    survival = settlement["survival"]
    require(survival["index"] == 1 - selected and same(survival["identity"], trees[1 - selected]["identity"]),
            "saved sibling identity")
    require(survival["atMs"] >= spans["drain"]["endMs"] and survival["atMs"] <= spans["cleanup"]["startMs"],
            "sibling survival before global cleanup")
    require(survival["observation"]["state"] == "complete" and
            any(p["pid"] == survival["identity"]["pid"] and p["startTime"] == survival["identity"]["startIdentity"]
                for p in survival["observation"]["processes"]), "live sibling observation")
    for index, tree in enumerate(trees):
        exit_record = tree["exits"][0]
        require(integer(exit_record["code"]) == 137, "unexpected producer exit status")
        disposal = tree["disposals"][0]
        require(exit_record["index"] == index and disposal["index"] == index, "exit tree correlation")
        require(disposal["evidence"]["event"]["authority"] == "ECHILD+__WALL", "supervisor drain authority")
        require(number(exit_record["atMs"]) <= disposal["endMs"] and
                disposal["startMs"] <= disposal["endMs"], "exit before confirmed drain")
        if index == selected:
            require(disposal["endMs"] == spans["drain"]["endMs"], "selected drain span")
        else:
            require(exit_record["atMs"] > survival["atMs"], "sibling exit after survival proof")
    quiet = high - guard["hysteresisBytes"]
    low_dwell = []
    for sample in samples:
        if sample["startedAtMs"] <= spans["cleanup"]["endMs"]:
            continue
        if sample["currentBytes"] >= quiet:
            low_dwell = []
        else:
            low_dwell.append(sample)
    require(len(low_dwell) >= 3 and low_dwell[-1]["completedAtMs"] -
            low_dwell[0]["completedAtMs"] >= guard["rearmMs"], "fresh low-pressure dwell raw samples")
    require(all(b["startedAtMs"] - a["completedAtMs"] <= guard["sampleMs"] + guard["responseBudgetMs"]
                for a, b in zip(low_dwell, low_dwell[1:])), "dwell sample gaps")
    state = settlement["monitorState"]
    require(same(state, {"armed": True, "pendingIntervention": False, "unconfirmedDrain": False,
                      "unresolvedOutcome": False, "interventions": 1}), "fresh low pressure dwell")
    records = settlement["acknowledgements"]
    require(len(records) == 2 and [r["path"] for r in records] == ["/reservation", "/outcome"], "ACK count/order")
    for n, ack in enumerate(records):
        require(ack["status"] == 200 and number(ack["endMs"]) > number(ack["startMs"]), "ACK status/timing")
        a = ack["ack"]
        require(all(same(a[k], manifest["context"][k]) for k in
                    ("run_id", "worker_id", "register_nonce", "claim_generation", "memory_episode")), "ACK incarnation")
        require(a["intervention_id"] == binding["interventionId"] and a["admitted"] is True
                and a["authorizing"] is (n == 0) and same(a["policy"], {"version": 1, "max_interventions": 2})
                and same(a["allowance"], {"limit": 2, "used": 1, "remaining": 1}), "ACK contract")
        require(("outcome" not in a) if n == 0 else a["outcome"] == "confirmed_drained", "outcome ACK")
    require(records[0]["startMs"] == spans["reservation"]["startMs"] and
            records[0]["endMs"] == spans["reservation"]["endMs"], "reservation span binding")
    require(len(settlement["serviceRecords"]) == 2 and len(settlement["history"]) == 1
            and settlement["history"][0]["outcome"] == "confirmed_drained", "settlement records")
    for n, record in enumerate(settlement["serviceRecords"]):
        require(same(record["ack"], records[n]["ack"]) and record["path"] == records[n]["path"],
                "service ACK receipt")
        req = record["request"]
        base = dict(req)
        if n:
            require(base.pop("outcome") == "confirmed_drained", "service outcome request")
        expected_request = {**manifest["context"], "intervention_id": binding["interventionId"],
                            "policy": {"version": 1, "max_interventions": 2}}
        require(same(base, expected_request) and
                all(same(base[k], records[n]["ack"][k]) for k in expected_request),
                "request host/ACK contract")
        require(same(base, settlement["history"][0]["request"]) and
                base["intervention_id"] == binding["interventionId"], "idempotent request binding")
        require(records[n]["startMs"] <= record["atMs"] <= records[n]["endMs"], "service timing")
    require(settlement["history"][0]["status"] == "admitted", "history admission")
    require(records[1]["startMs"] >= spans["drain"]["endMs"] and
            records[1]["endMs"] <= low_dwell[0]["startedAtMs"], "outcome timing before fresh dwell")
    schedule = settlement["schedule"]
    require(2 <= len(schedule) <= rounds * 2, "fixed schedule count")
    for index in (0, 1):
        steps = [s for s in schedule if s["index"] == index]
        require([s["step"] for s in steps] == list(range(1, len(steps) + 1)), "step schedule")
        require(all(number(s["endMs"]) >= number(s["startMs"]) for s in steps), "step times")
    require(all(s["index"] in (0, 1) and s["kind"] in ("completed", "cancelled") for s in schedule),
            "unexpected step fault")
    require(all(s["kind"] == "completed" for s in schedule if s["index"] != selected), "sibling step failure")
    require(max(s["endMs"] for s in schedule) - min(s["startMs"] for s in schedule) <= 30000, "load window")
    cancelled = [s for s in schedule if s["index"] == selected and s["kind"] == "cancelled"]
    require(len(cancelled) == 1 and cancelled[0]["startMs"] <= bracket["upperMs"]
            and cancelled[0]["endMs"] >= spans["drain"]["endMs"], "selected in-flight step cancellation")
    require(same(cancelled[0], [s for s in schedule if s["index"] == selected][-1]), "no step after cancellation")
    observations = settlement["observations"]
    observed_spans = [o for o in observations if o["index"] == selected and o["startMs"] >= bracket["upperMs"]]
    require(observed_spans and observed_spans[0]["startMs"] == spans["observe"]["startMs"]
            and observed_spans[0]["endMs"] == spans["observe"]["endMs"], "forwarded observation span")
    require(any(o["index"] == selected and o["startMs"] >= leg["ack"]["atMs"]
                and o["endMs"] <= spans["signal"]["startMs"] for o in observations), "post-ACK revalidation")
    writes = [w for w in settlement["signalWrites"] if w["index"] == selected]
    require(same(writes, [{"index": selected, "startMs": spans["signal"]["startMs"],
                        "endMs": spans["signal"]["endMs"]}]), "forwarded control write")
    require(same(report["cleanup"], {"confirmed": True, "startMs": spans["cleanup"]["startMs"],
                                  "endMs": spans["cleanup"]["endMs"]}), "cleanup binding")
    require(spans["cleanup"]["endMs"] - spans["cleanup"]["startMs"] <= 30000, "cleanup window")
    final = report["finalCleanup"]
    require(final["handles"] == 2 and final["joined"] == ["fulfilled", "fulfilled"] and final["driverCompleted"] is True
            and len(final["clean"]) == 2 and all(r["clean"] is True for r in final["clean"]), "final joins")
    require(number(final["endMs"]) >= number(final["startMs"]) >= spans["cleanup"]["endMs"]
            and final["endMs"] - final["startMs"] <= 30000, "final cleanup times")
    # The interval stops before finalCleanup; the final raw-only read happens
    # inside it. All timestamps use the declared node:performance clock.
    fixture_end = number(final["endMs"])
    # Host capture is 60s including its kill/reap grace; host removal owns a
    # separate <=30s budget. Node performance.now starts at process startup.
    require(fixture_end <= 60000, "60s captured fixture within 90s fixture wall")
    require(all(s["completedAtMs"] <= final["startMs"] for s in samples),
            "load/quiet samples before final cleanup")
    require(all(s["completedAtMs"] <= fixture_end for s in parsed.values()),
            "raw evidence after final cleanup")
    raw_ids = [r["sample"]["sampleId"] for r in raw_samples]
    baseline_offset = raw_ids.index(baseline[0]["sampleId"])
    load_offset = baseline_offset + len(baseline)
    require(same([s["sampleId"] for s in baseline],
                 raw_ids[baseline_offset:load_offset]) and
            same([s["sampleId"] for s in samples],
                 raw_ids[load_offset:load_offset + len(samples)]),
            "raw baseline/load phase correlation")
    require(all(r["sample"]["startedAtMs"] >= final["startMs"]
                for r in raw_samples[load_offset + len(samples):]),
            "trailing raw reads inside final cleanup")
    load_start = min(s["startMs"] for s in schedule)
    load_end = max(cancelled[0]["endMs"], spans["drain"]["endMs"])
    require(baseline[-1]["completedAtMs"] <= min(load_start, samples[0]["startedAtMs"]) and
            load_start <= bracket["upperMs"] and load_end - load_start <= 30000,
            "baseline then bounded load")
    require(load_end <= survival["atMs"] <= spans["cleanup"]["startMs"] and
            spans["cleanup"]["endMs"] - load_end <= 30000,
            "load then bounded owned cleanup")
    require(all(s["startMs"] >= baseline[-1]["completedAtMs"] and
                s["endMs"] <= spans["cleanup"]["endMs"] for s in schedule),
            "steps inside load/owned cleanup")
    for index, tree in enumerate(trees):
        disposal = tree["disposals"][0]
        if index == selected:
            require(leg["ack"]["atMs"] <= disposal["startMs"] <= spans["signal"]["startMs"],
                    "authorized disposal contains control write")
        else:
            require(disposal["startMs"] >= spans["cleanup"]["startMs"], "sibling disposal phase")
        require(disposal["startMs"] <= tree["exits"][0]["atMs"] <= disposal["endMs"]
                <= spans["cleanup"]["endMs"], "disposal phase correlation")
    for entries in (observations, settlement["signalWrites"]):
        require(all(0 <= entry["startMs"] <= entry["endMs"]
                    <= spans["cleanup"]["endMs"] for entry in entries),
                "observation/control phase timing")
    require(low_dwell[-1]["completedAtMs"] - spans["cleanup"]["endMs"] <= 5000 and
            all(s["completedAtMs"] - s["startedAtMs"] <= guard["sampleMs"] + guard["responseBudgetMs"]
                for s in low_dwell),
            "bounded fresh quiet evidence")
    require(same(report["outcome"], {"status": "failed", "reasons": ["worker_memory_pressure"], "successor": "aborted"}),
            "failed/aborted successor outcome")


def verify(manifest, data, host_cleanup, synthetic=False):
    numeric_fields(manifest)
    numeric_fields(host_cleanup)
    manifest_check(manifest, synthetic)
    require(len({f["containerId"] for f in manifest["fixtures"]}) == 2 and
            all(re.fullmatch("[0-9a-f]{64}", f["containerId"]) for f in manifest["fixtures"]), "unique immutable CIDs")
    values = reports(data)
    require([r["fixtureId"] for r in values] == [f["fixtureId"] for f in manifest["fixtures"]], "report order/duplicate")
    for report, expected in zip(values, manifest["fixtures"]):
        verify_report(report, manifest, expected, synthetic)
        proof = host_cleanup[expected["fixtureId"]]
        require(same(report["hostCleanup"], proof), "complete stdout must carry host cleanup proof")
        require(0 < number(proof["hostMonotonicEnd"]) - number(proof["hostMonotonicStart"]) <= 30,
                "host cleanup budget")
        require(proof["confirmed"] is True and proof["containerId"] == expected["containerId"]
                and proof["label"] == manifest["invocation"] and proof["removedId"] == expected["containerId"],
                "host immutable Docker cleanup proof")
    return values


if __name__ == "__main__":
    try:
        require(len(sys.argv) == 4, "usage: calibration_check.py HOST_MANIFEST REPORTS HOST_CLEANUP")
        manifest = read_json(sys.argv[1], 65536)
        with open(sys.argv[2], "rb") as stream:
            data = stream.read(2 * CAP + 1)
        verify(manifest, data, read_json(sys.argv[3], 65536))
        print("calibration: two measured complete reports verified")
    except (ValueError, KeyError, TypeError, IndexError, OSError, StopIteration, RecursionError) as exc:
        print("calibration FAIL: " + str(exc), file=sys.stderr)
        sys.exit(1)
