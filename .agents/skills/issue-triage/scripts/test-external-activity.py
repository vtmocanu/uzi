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
import json, os, sys
d = os.environ["FAKE_GH_DIR"]
args = sys.argv[1:]
with open(os.path.join(d, "calls.log"), "a") as log:
    log.write(json.dumps(args) + "\n")
fail = os.environ.get("FAKE_GH_FAIL", "")
def out(name):
    if name == fail:
        sys.stderr.write("HTTP 502: fake failure\n"); sys.exit(1)
    sys.stdout.write(open(os.path.join(d, name + ".json")).read())
if args[:2] == ["api", "graphql"]:
    cursor = next((a.split("=", 1)[1] for a in args if a.startswith("cursor=")), "")
    out("graphql2" if cursor == "C1" else "graphql3" if cursor == "C2" else "graphql1")
    sys.exit(0)
path = [a for a in args if not a.startswith("-") and a != "api"][0]
if path.startswith("repos/o/r/issues/comments"):
    out("ic")
elif path.startswith("repos/o/r/pulls/comments"):
    out("pc")
elif path.startswith("repos/o/r/issues?state=open"):
    out("open")
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


def pr(n, updated, reviews):
    return {"number": n, "updatedAt": updated,
            "reviews": {"totalCount": len(reviews), "nodes": reviews}}


def page(nodes, nxt):
    return {"data": {"repository": {"pullRequests": {
        "pageInfo": {"hasNextPage": nxt is not None, "endCursor": nxt}, "nodes": nodes}}}}


def item(n, kind="issue", state="closed", author="vtmocanu", title=None):
    d = {"number": n, "state": state, "title": title or f"item {n}", "user": user(author)}
    if kind == "PR":
        d["pull_request"] = {}
    return d


FIXTURES = {
    # Two --paginate pages concatenated, as gh prints them.
    "ic": json.dumps([
        # 10: maintainer-authored issue, external comment after the maintainer's.
        comment(10, "vtmocanu", "2026-11-02T10:00:00Z"),
        comment(10, "ext1", "2026-11-03T10:00:00Z"),
        # 11: external, then an unrelated later maintainer comment.
        comment(11, "ext2", "2026-11-02T10:00:00Z"),
        comment(11, "vtmocanu", "2026-11-04T10:00:00Z"),
    ]) + json.dumps([
        # 12: an old comment edited inside the window (updated_at drives it).
        comment(12, "ext3", "2026-11-05T10:00:00Z"),
        # 13: external, then only automation answers.
        comment(13, "ext4", "2026-11-02T10:00:00Z"),
        comment(13, "vtmocanu-uzi", "2026-11-03T10:00:00Z"),
        comment(13, "coderabbitai[bot]", "2026-11-03T11:00:00Z", typ="Bot"),
        comment(13, "renovate", "2026-11-03T12:00:00Z", typ="Bot"),
        # 14: hostile login on a hostile-titled issue.
        comment(14, HOSTILE + "evil", "2026-11-02T10:00:00Z"),
    ]),
    "pc": json.dumps([{"pull_request_url": "https://api.github.com/repos/o/r/pulls/20",
                       "user": user("ext5"), "updated_at": "2026-11-02T10:00:00Z",
                       "html_url": f"{R}/pull/20#d1"}]),
    "graphql1": json.dumps(page([
        # 21: merged PR, external review inside the window, bot review ignored.
        pr(21, "2026-11-06T00:00:00Z", [review("ext6", "2026-11-05T00:00:00Z"),
                                        review("coderabbitai", "2026-11-05T01:00:00Z", "Bot")]),
        # 23: a review submitted before the window and edited inside it.
        pr(23, "2026-11-05T12:00:00Z", [{**review("ext10", "2026-10-20T00:00:00Z"),
                                         "updatedAt": "2026-11-05T11:00:00Z"}]),
        # 3: review before the window: not listed.
        pr(3, "2026-11-04T00:00:00Z", [review("ext7", "2026-10-20T00:00:00Z")]),
    ], "C1")),
    "graphql2": json.dumps(page([
        # 22: PR updatedAt a few seconds BEFORE its own review (the slack day finds it).
        pr(22, "2026-10-31T23:59:58Z", [review("ext8", "2026-11-01T00:00:02Z")]),
        pr(4, "2026-10-20T00:00:00Z", []),
    ], "C2")),
    "graphql3": json.dumps(page([], None)),
    "open": json.dumps([
        item(30, state="open", author="ext9", title="old external issue"),
        item(31, state="open", author="vtmocanu-uzi", title="bot-filed issue"),
        item(32, state="open", author="dependabot[bot]", title="bot pr"),
    ]),
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


def run(tmp, fail=""):
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
           "FAKE_GH_FAIL": fail}
    env.pop("TRIAGE_MAINTAINERS", None)
    env.pop("TRIAGE_AUTOMATION_LOGINS", None)
    res = subprocess.run(["bash", str(SCRIPT), "--repo", "o/r", "--since", SINCE],
                         env=env, capture_output=True)
    return res, [json.loads(line) for line in calls.read_text().splitlines()]


def rows(stdout):
    out = {}
    for line in stdout.splitlines():
        cols = line.split("\t")
        if len(cols) > 1:
            out[cols[1]] = cols
    return out


def main():
    with tempfile.TemporaryDirectory(prefix="triage-ext-") as t:
        tmp = Path(t)
        res, calls = run(tmp)
        assert res.returncode == 0, res.stderr.decode()
        text = res.stdout.decode()
        assert not any(ord(c) < 32 and c not in "\t\n" or 127 <= ord(c) <= 159 or c == "‮"
                       for c in text), repr(text)
        assert "Y2xpcGJvYXJk" not in text, text
        got = rows(text)
        expect = {"#10": "external-last", "#11": "maintainer-later", "#12": "external-last",
                  "#13": "external-last", "#14": "external-last", "#20": "external-last",
                  "#21": "external-last", "#22": "external-last", "#23": "external-last", "#30": "authored-open"}
        for n, label in expect.items():
            assert n in got and got[n][0] == label, (n, label, got.get(n))
        for n in ("#3", "#4", "#31", "#32"):
            assert n not in got, (n, got[n])
        assert "last-external=ext3 2026-11-05T10:00:00Z comment" in "\t".join(got["#12"])
        assert "maintainer-in-window=none" in got["#13"], got["#13"]
        assert "last-external=ext4 " in "\t".join(got["#13"]), got["#13"]
        assert "last-external=ext6" in "\t".join(got["#21"]), got["#21"]
        assert got["#14"][-1] == "safe title", got["#14"]
        order = [line.split("\t")[0] for line in text.splitlines() if "\t" in line]
        assert order == sorted(order, key=["external-last", "authored-open",
                                           "maintainer-later"].index), order
        assert "SUMMARY window-since=2026-11-01T00:00:00Z external-last=8 authored-open=1 " \
               "maintainer-later=1" in text, text
        assert "LIMITATION reviews: found through PRs updated since 2026-10-31T00:00:00Z" in text
        graphql = [c for c in calls if c[:2] == ["api", "graphql"]]
        assert len(graphql) == 2, f"paging must stop below the floor: {graphql}"
        print("PASS: labels, automation, edits, merged-PR reviews, paging floor, sanitizing")

        for fail in ("ic", "graphql2", "item21"):
            res, _ = run(tmp, fail=fail)
            assert res.returncode == 3, (fail, res.returncode, res.stderr.decode())
            assert res.stdout == b"", (fail, res.stdout)
        print("PASS: any failed lookup (first, later page, item) exits 3 with no report")


if __name__ == "__main__":
    main()
