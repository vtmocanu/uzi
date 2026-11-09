#!/usr/bin/env python3
"""Offline regression for external-activity.sh: a fake gh serves fixtures, no forge reads."""

import json
import os
from pathlib import Path
import subprocess
import tempfile

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / "external-activity.sh"
SINCE = "2026-11-01T00:00:00Z"
HOSTILE = "\x1b]52;c;Y2xpcGJvYXJk\x07‮"
R = "https://github.com/o/r"

FAKE_GH = r'''#!/usr/bin/env python3
import json, os, re, sys
d = os.environ["FAKE_GH_DIR"]
args = sys.argv[1:]
with open(os.path.join(d, "calls.log"), "a") as log:
    log.write(json.dumps(args) + "\n")
fail = os.environ.get("FAKE_GH_FAIL", "")
override = json.loads(os.environ.get("FAKE_GH_OVERRIDE") or "{}")
def body(name):
    if name in override:
        return override[name]
    return open(os.path.join(d, name + ".json")).read()
def out(name, text=None):
    if name == fail:
        sys.stderr.write(os.environ.get("FAKE_GH_ERR", "HTTP 502: fake failure") + "\n"); sys.exit(1)
    sys.stdout.write(body(name) if text is None else text)
if args[:2] == ["api", "graphql"]:
    q = next(a.split("=", 1)[1] for a in args if a.startswith("query="))
    states = re.search(r"states:\[([A-Z,]+)\]", q).group(1).split(",")
    cursor = next((a.split("=", 1)[1] for a in args if a.startswith("cursor=")), "")
    name = "graphql2" if cursor == "C1" else "graphql3" if cursor == "C2" else "graphql1"
    raw = body(name)
    try:
        doc = json.loads(raw)
        nodes = doc["data"]["repository"]["pullRequests"]["nodes"]
        # A PR in a state the query did not select is not returned.
        doc["data"]["repository"]["pullRequests"]["nodes"] = [
            n for n in nodes if n.get("_state", "OPEN") in states]
        raw = json.dumps(doc)
    except (ValueError, KeyError, TypeError):
        pass
    out(name, raw)
    sys.exit(0)
paginate = "--paginate" in args
path = [a for a in args if not a.startswith("-") and a != "api"][0]
def rest(name):
    # Fixtures hold a list of pages; without --paginate only page one is returned.
    # An override that is not a list of pages (empty, null) is returned verbatim.
    try:
        pages = json.loads(body(name))
    except ValueError:
        pages = None
    if not (isinstance(pages, list) and pages and all(isinstance(pg, list) for pg in pages)):
        out(name)
    else:
        out(name, "".join(json.dumps(pg) for pg in (pages if paginate else pages[:1])))
if path.startswith("repos/o/r/issues/comments"):
    rest("ic")
elif path.startswith("repos/o/r/pulls/comments"):
    rest("pc")
elif path.startswith("repos/o/r/issues?state=open"):
    rest("open")
elif path.startswith("repos/o/r/issues/"):
    out("item" + path.rsplit("/", 1)[1])
else:
    sys.stderr.write("fake gh: unexpected call %r\n" % args); sys.exit(9)
'''


def user(login, typ="User"):
    return {"login": login, "type": typ}


def comment(n, login, at, typ="User", pr=False):
    kind = "pull" if pr else "issues"
    return {"issue_url": f"https://api.github.com/repos/o/r/issues/{n}", "user": user(login, typ),
            "updated_at": at, "html_url": f"{R}/{kind}/{n}#c-{login}-{at}"}


def review(login, at, typ="User"):
    return {"author": {"login": login, "__typename": typ}, "submittedAt": at, "updatedAt": at,
            "url": f"{R}/pull/x#r-{login}"}


def pr(n, updated, reviews, state="OPEN", total=None):
    return {"number": n, "updatedAt": updated, "_state": state,
            "reviews": {"totalCount": len(reviews) if total is None else total, "nodes": reviews}}


def page(nodes, nxt):
    return {"data": {"repository": {"pullRequests": {
        "pageInfo": {"hasNextPage": nxt is not None, "endCursor": nxt}, "nodes": nodes}}}}


def item(n, kind="issue", state="closed", author="vtmocanu", title=None):
    path = "pull" if kind == "PR" else "issues"
    d = {"number": n, "state": state, "title": title or f"item {n}", "user": user(author),
         "html_url": f"{R}/{path}/{n}"}
    if kind == "PR":
        d["pull_request"] = {}
    return d


FIXTURES = {
    # REST fixtures are lists of pages; the fake returns them all only under --paginate.
    "ic": json.dumps([[
        # 10: maintainer-authored issue, external comment after the maintainer's.
        comment(10, "vtmocanu", "2026-11-02T10:00:00Z"),
        comment(10, "ext1", "2026-11-03T10:00:00Z"),
        # 11: external, then an unrelated later maintainer comment.
        comment(11, "ext2", "2026-11-02T10:00:00Z"),
        comment(11, "vtmocanu", "2026-11-04T10:00:00Z"),
    ], [
        # 12: an old comment edited inside the window (updated_at drives it).
        comment(12, "ext3", "2026-11-05T10:00:00Z"),
        # 13: external, then only automation answers.
        comment(13, "ext4", "2026-11-02T10:00:00Z"),
        comment(13, "vtmocanu-uzi", "2026-11-03T10:00:00Z"),
        comment(13, "coderabbitai[bot]", "2026-11-03T11:00:00Z", typ="Bot"),
        comment(13, "renovate", "2026-11-03T12:00:00Z", typ="Bot"),
        # 14: hostile login on a hostile-titled issue.
        comment(14, HOSTILE + "evil", "2026-11-02T10:00:00Z"),
    ]]),
    "pc": json.dumps([[{"pull_request_url": "https://api.github.com/repos/o/r/pulls/20",
                        "user": user("ext5"), "updated_at": "2026-11-02T10:00:00Z",
                        "html_url": f"{R}/pull/20#d1"}]]),
    "graphql1": json.dumps(page([
        # 21: merged PR, external review inside the window, bot review ignored.
        pr(21, "2026-11-06T00:00:00Z", [review("ext6", "2026-11-05T00:00:00Z"),
                                        review("coderabbitai", "2026-11-05T01:00:00Z", "Bot")],
           state="MERGED"),
        # 24: more reviews than one page reads.
        pr(24, "2026-11-05T13:00:00Z", [review("vtmocanu", "2026-11-05T12:30:00Z")], total=150),
        # 23: a review submitted before the window and edited inside it.
        pr(23, "2026-11-05T12:00:00Z", [{**review("ext10", "2026-10-20T00:00:00Z"),
                                         "updatedAt": "2026-11-05T11:00:00Z"}], state="CLOSED"),
        # 3: review before the window: not listed.
        pr(3, "2026-11-04T00:00:00Z", [review("ext7", "2026-10-20T00:00:00Z"),
                                       {**review("x", "2026-10-21T00:00:00Z"), "author": None}]),
    ], "C1")),
    "graphql2": json.dumps(page([
        # 22: PR updatedAt a few seconds BEFORE its own review (the slack day finds it).
        pr(22, "2026-10-31T23:59:58Z", [review("ext8", "2026-11-01T00:00:02Z")]),
        pr(4, "2026-10-20T00:00:00Z", []),
    ], "C2")),
    "graphql3": json.dumps(page([], None)),
    "open": json.dumps([[
        item(30, state="open", author="ext9", title="old external issue"),
        item(31, state="open", author="vtmocanu-uzi", title="bot-filed issue"),
        item(32, kind="PR", state="open", author="dependabot[bot]", title="bot pr"),
    ], [
        item(33, kind="PR", state="open", author="ext11", title="old external pr"),
    ]]),
    "item10": json.dumps(item(10)),
    "item11": json.dumps(item(11)),
    "item12": json.dumps(item(12)),
    "item13": json.dumps(item(13)),
    "item14": json.dumps(item(14, title=HOSTILE + "safe title")),
    "item20": json.dumps(item(20, kind="PR", state="open")),
    "item21": json.dumps(item(21, kind="PR")),
    "item22": json.dumps(item(22, kind="PR")),
    "item23": json.dumps(item(23, kind="PR")),
}


def run(tmp, fail="", err="", override=None, script=SCRIPT):
    for name, body in FIXTURES.items():
        (tmp / f"{name}.json").write_text(body)
    calls = tmp / "calls.log"
    if calls.exists():
        calls.unlink()
    bindir = tmp / "bin"
    bindir.mkdir(exist_ok=True)
    gh = bindir / "gh"
    gh.write_text(FAKE_GH)
    gh.chmod(0o755)
    env = {**os.environ, "PATH": f"{bindir}:{os.environ['PATH']}", "FAKE_GH_DIR": str(tmp),
           "FAKE_GH_FAIL": fail, "FAKE_GH_ERR": err or "HTTP 502: fake failure",
           "FAKE_GH_OVERRIDE": json.dumps(override or {})}
    env.pop("TRIAGE_MAINTAINERS", None)
    env.pop("TRIAGE_AUTOMATION_LOGINS", None)
    res = subprocess.run(["bash", str(script), "--repo", "o/r", "--since", SINCE],
                         env=env, capture_output=True)
    lines = calls.read_text().splitlines() if calls.exists() else []
    return res, [json.loads(line) for line in lines]


def rows(stdout):
    out = {}
    for line in stdout.splitlines():
        cols = line.split("\t")
        if len(cols) > 1:
            out[cols[1]] = cols
    return out


def clean(text):
    return not any(ord(c) < 32 and c not in "\t\n" or 127 <= ord(c) <= 159 or c == "‮"
                   for c in text) and "Y2xpcGJvYXJk" not in text


def main():
    with tempfile.TemporaryDirectory(prefix="triage-ext-") as t:
        tmp = Path(t)
        res, calls = run(tmp)
        assert res.returncode == 0, res.stderr.decode()
        text = res.stdout.decode()
        assert clean(text), repr(text)
        got = rows(text)
        expect = {"#10": "external-last", "#11": "maintainer-later", "#12": "external-last",
                  "#13": "external-last", "#14": "external-last", "#20": "external-last",
                  "#21": "external-last", "#22": "external-last", "#23": "external-last",
                  "#30": "authored-open", "#33": "authored-open"}
        for n, label in expect.items():
            assert n in got and got[n][0] == label, (n, label, got.get(n))
        for n in ("#3", "#4", "#24", "#31", "#32"):
            assert n not in got, (n, got[n])
        joined = {n: "\t".join(c) for n, c in got.items()}
        assert "last-external=ext3 2026-11-05T10:00:00Z comment" in joined["#12"]
        assert "maintainer-in-window=none" in got["#13"], got["#13"]
        assert "last-external=ext4 " in joined["#13"], got["#13"]
        assert "last-external=ext6" in joined["#21"], got["#21"]
        assert got["#14"][-1] == "safe title", got["#14"]
        assert got["#33"][2] == "PR/open" and f"item={R}/pull/33" in got["#33"], got["#33"]
        assert f"item={R}/issues/30" in got["#30"], got["#30"]
        order = [line.split("\t")[0] for line in text.splitlines() if "\t" in line]
        assert order == sorted(order, key=["external-last", "authored-open",
                                           "maintainer-later"].index), order
        assert "SUMMARY window-since=2026-11-01T00:00:00Z external-last=8 authored-open=2 " \
               "maintainer-later=1" in text, text
        assert "LIMITATION reviews: found through PRs updated since 2026-10-31T00:00:00Z" in text
        assert "LIMITATION reviews: only the newest 100 reviews read on PR(s) #24;" in text, text
        graphql = [c for c in calls if c[:2] == ["api", "graphql"]]
        assert len(graphql) == 2, f"paging must stop below the floor: {graphql}"
        print("PASS: labels, automation, edits, merged and closed PR reviews, pagination, "
              "paging floor, truncation, permalinks, sanitizing")

        bad_page = json.dumps({"data": {"repository": {"pullRequests": {"nodes": []}}}})
        stuck = json.dumps(page([pr(22, "2026-11-02T00:00:00Z", [])], "C1"))
        cases = {
            "first page fails": dict(fail="ic"),
            "graphql page 2 fails": dict(fail="graphql2"),
            "item fetch fails": dict(fail="item21"),
            "hostile error text": dict(fail="pc", err=HOSTILE + "HTTP 500 boom"),
            "empty REST response": dict(override={"ic": ""}),
            "null REST response": dict(override={"pc": "null"}),
            "REST rows missing fields": dict(override={"open": json.dumps([{"number": 1}])}),
            "graphql missing pageInfo": dict(override={"graphql1": bad_page}),
            "graphql errors": dict(override={"graphql1": json.dumps({"errors": [{"message": "x"}]})}),
            "cursor does not advance": dict(override={"graphql2": stuck}),
            "item identity mismatch": dict(override={"item21": json.dumps(item(99, kind="PR"))}),
            "string item number": dict(override={"open": json.dumps(
                [[{**item(30, state="open", author="ext9"), "number": "30\u001b[2J"}]])}),
            "non-numeric issue_url": dict(override={"ic": json.dumps([[
                {**comment(10, "ext1", "2026-11-03T10:00:00Z"), "issue_url": "x/issues/abc"}]])}),
            "numeric timestamp": dict(override={"pc": json.dumps([[
                {"pull_request_url": "https://api.github.com/repos/o/r/pulls/20",
                 "user": user("ext5"), "updated_at": 1, "html_url": f"{R}/pull/20#d1"}]])}),
            "user is not an object": dict(override={"ic": json.dumps([[
                {**comment(10, "ext1", "2026-11-03T10:00:00Z"), "user": "ext1"}]])}),
            "malformed review node": dict(override={"graphql1": json.dumps(page(
                [pr(21, "2026-11-06T00:00:00Z", [{}])], None))}),
            "empty continuation page": dict(override={"graphql1": json.dumps(page([], "C1"))}),
        }
        SHAPE = {"empty REST response", "null REST response", "REST rows missing fields",
                 "graphql missing pageInfo", "graphql errors", "string item number",
                 "non-numeric issue_url", "numeric timestamp", "malformed review node",
                 "user is not an object",
                 "empty continuation page", "item identity mismatch"}
        for name, kw in cases.items():
            res, _ = run(tmp, **kw)
            assert res.returncode == 3, (name, res.returncode, res.stderr.decode())
            assert res.stdout == b"", (name, res.stdout)
            assert clean(res.stderr.decode()), (name, res.stderr)
            # Shape cases must be refused by validation, not by a later crash.
            if name in SHAPE:
                assert b"(shape) failed" in res.stderr, (name, res.stderr)
        print(f"PASS: {len(cases)} failure shapes exit 3 with no report and a sanitized error")

        for argv in (["--days"], ["--repo"], ["--since", "yesterday"], ["--repo", "noslash"]):
            res = subprocess.run(["bash", str(SCRIPT), *argv], capture_output=True)
            assert res.returncode == 2, (argv, res.returncode, res.stderr)
        print("PASS: usage errors exit 2")


if __name__ == "__main__":
    main()
