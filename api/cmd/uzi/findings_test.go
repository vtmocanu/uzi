package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #333 M6: `uzi findings list/file/dismiss`.
//
// The workersvc import is TEST-ONLY and must stay that way (like review_backlog_test.go):
// cmd/uzi may not link the server stack, so the production code forwards bucket strings rather
// than referencing the constants; this file is where the two sides are pinned to each other.

// findingsFake builds a FakeClient with a two-repo backlog: one repo carrying a coordinate seen
// in two runs plus a display-only coordinate whose evidence cascaded away (nil finding_id), and
// a second repo with one coordinate. open_count is deliberately not derivable from these rows —
// it is the server's CountOpenFindingsForUser aggregate, so a CLI that recomputed it from the
// screen would print a different number.
func findingsFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{FindingsResult: apitypes.IncidentalFindingBacklogDTO{
		Bucket:    workersvc.BucketToFile,
		OpenCount: 4,
		Findings: []apitypes.IncidentalFindingDTO{
			{
				FindingID:  ptr("f-aaaa"),
				Location:   "internal/sweep.go#sweepLoop",
				RepoID:     "repo-1",
				RepoPath:   "team/api",
				Status:     "open",
				LastTitle:  "leaked ticker in sweepLoop",
				SeenInRuns: 2,
			},
			{
				// A cascaded-away coordinate: disposition-driven read keeps it, but there is no
				// evidence row to act on, so finding_id is nil and the row renders a dash.
				Location:   "internal/boot.go#check",
				RepoID:     "repo-1",
				RepoPath:   "team/api",
				Status:     "filed",
				LastTitle:  "boot check hole",
				SeenInRuns: 1,
			},
			{
				FindingID:  ptr("f-bbbb"),
				Location:   "src/retry.ts#retry",
				RepoID:     "repo-2",
				RepoPath:   "team/web",
				Status:     "open",
				LastTitle:  "retry that never succeeds",
				SeenInRuns: 1,
			},
		},
	}}
}

// The human view groups by repo and, per coordinate, prints the actionable finding_id, the
// latest title, "seen in N runs", the status, and the open_count meta line.
func TestFindingsListHumanGroupsByRepo(t *testing.T) {
	fc := findingsFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"open (need filing): 4",
		"team/api", "repo-1",
		"f-aaaa", "leaked ticker in sweepLoop", "seen in 2 runs", "open",
		"team/web", "repo-2", "f-bbbb", "seen in 1 run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("findings list output missing %q:\n%s", want, out)
		}
	}
	// "seen in 1 run", never "1 runs" — the singular is why runsPhrase exists.
	if strings.Contains(out, "1 runs") {
		t.Errorf("run count not singularised at 1:\n%s", out)
	}
	// The cascaded-away coordinate has no finding_id, so its row shows a dash rather than an
	// empty column a user might copy.
	if !strings.Contains(out, "-  boot check hole") {
		t.Errorf("a nil finding_id coordinate must render a dash, not an empty id:\n%s", out)
	}
	// The web repo's row must render AFTER its own repo header, not under team/api — a smoke
	// test that the grouping actually partitions by repo.
	if strings.Index(out, "team/web") > strings.Index(out, "f-bbbb") {
		t.Errorf("f-bbbb rendered before its repo header — grouping is wrong:\n%s", out)
	}
}

// --json passes the server's envelope through unchanged, so an agent sees the coordinate-deduped
// findings and the open_count meta — none of which is recomputed client-side.
func TestFindingsListJSONPassesTheEnvelopeThrough(t *testing.T) {
	fc := findingsFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.IncidentalFindingBacklogDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not an IncidentalFindingBacklogDTO: %v\n%s", err, out)
	}
	if got.OpenCount != 4 {
		t.Errorf("--json open_count = %d, want 4", got.OpenCount)
	}
	if len(got.Findings) != 3 || got.Findings[0].SeenInRuns != 2 {
		t.Errorf("--json lost the coordinate detail: %d findings, first seen_in_runs=%d",
			len(got.Findings), func() int {
				if len(got.Findings) > 0 {
					return got.Findings[0].SeenInRuns
				}
				return -1
			}())
	}
}

// --bucket/--repo/--run are forwarded VERBATIM, and an unset flag omits the parameter entirely so
// the SERVER's default applies. Substituting a default client-side would make the CLI a second
// definition of it, and the two could drift.
func TestFindingsListFiltersForwardedVerbatim(t *testing.T) {
	fc := findingsFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "list",
		"--bucket", workersvc.BucketAll, "--repo", "repo-9", "--run", "run-7"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastFindingsBucket != workersvc.BucketAll {
		t.Errorf("bucket forwarded as %q, want %q", fc.LastFindingsBucket, workersvc.BucketAll)
	}
	if fc.LastFindingsRepo != "repo-9" || fc.LastFindingsRun != "run-7" {
		t.Errorf("repo/run forwarded as (%q,%q), want (repo-9, run-7)", fc.LastFindingsRepo, fc.LastFindingsRun)
	}

	fc2 := findingsFake()
	if _, _, code := runCLI(t, fakeEnv(fc2), "findings", "list"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc2.LastFindingsBucket != "" || fc2.LastFindingsRepo != "" || fc2.LastFindingsRun != "" {
		t.Errorf("unset flags must send nothing (server defaults), got bucket=%q repo=%q run=%q",
			fc2.LastFindingsBucket, fc2.LastFindingsRepo, fc2.LastFindingsRun)
	}
}

// An unknown bucket is the SERVER's 400, surfacing as the usage exit code — never a silently
// empty list (the pass-through design's payoff: the CLI holds no bucket predicate).
func TestFindingsListUnknownBucketIsAUsageError(t *testing.T) {
	fc := findingsFake()
	fc.Err = uzicli.Exitf(uzicli.ExitUsage, "invalid bucket")
	_, errb, code := runCLI(t, fakeEnv(fc), "findings", "list", "--bucket", "to_filee")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "invalid bucket") {
		t.Errorf("want the server's own message on stderr, got:\n%s", errb)
	}
}

// last_title and repo_path are agent/forge-influenced free text; an ESC/CSI sequence in either
// must be stripped before it reaches the terminal (R4), while the visible text survives.
func TestFindingsListSanitisesTerminalControlBytes(t *testing.T) {
	fc := findingsFake()
	fc.FindingsResult.Findings[0].LastTitle = "leak\x1b[31mred"
	fc.FindingsResult.Findings[0].RepoPath = "team\x1b]0;pwned\x07/api"
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("findings list leaked ESC (0x1b) from server free text:\n%q", out)
	}
	for _, want := range []string{"red", "team"} {
		if !strings.Contains(out, want) {
			t.Errorf("sanitising removed visible text %q:\n%q", want, out)
		}
	}
}

// The --bucket help text is the one place the CLI writes the finding bucket set down. It is
// documentation (the value is forwarded, the server validates), but an unpinned enumeration
// rots: this asserts BOTH directions against workersvc's real set — every advertised value is a
// real bucket, and every real bucket is advertised.
func TestFindingsBucketUsageMatchesServerEnum(t *testing.T) {
	_, rest, ok := strings.Cut(findingsBucketFlagUsage, ": ")
	if !ok {
		t.Fatalf("--bucket usage lost its %q separator, cannot be checked: %q", ": ", findingsBucketFlagUsage)
	}
	advertised := map[string]bool{}
	for _, part := range strings.Split(rest, "|") {
		// Strip the "(default)" annotation the to_file entry carries.
		name := strings.TrimSpace(strings.Split(strings.TrimSpace(part), " ")[0])
		if !workersvc.ValidFindingBucket(name) {
			t.Errorf("--bucket help advertises %q, which the server's validator rejects", name)
		}
		advertised[name] = true
	}
	for _, want := range []string{
		workersvc.BucketToFile, workersvc.BucketFiled, workersvc.BucketDone,
		workersvc.BucketDismissed, workersvc.BucketAll,
	} {
		if !advertised[want] {
			t.Errorf("--bucket help does not advertise the valid bucket %q", want)
		}
	}
}

// file success prints the created issue (number + url) and exits 0, forwarding the id verbatim.
func TestFindingsFileSuccess(t *testing.T) {
	fc := findingsFake()
	fc.FileFindingResult = apitypes.IncidentalFindingFileResultDTO{
		Issue: apitypes.IncidentalFindingFiledIssueDTO{IID: 42, WebURL: "https://forge/team/api/-/issues/42", Title: "leaked ticker"},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "f-aaaa")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastFileFindingID != "f-aaaa" {
		t.Errorf("file forwarded id %q, want f-aaaa", fc.LastFileFindingID)
	}
	for _, want := range []string{"#42", "leaked ticker", "https://forge/team/api/-/issues/42"} {
		if !strings.Contains(out, want) {
			t.Errorf("file output missing %q:\n%s", want, out)
		}
	}
}

// A created-with-warning file is still a success (exit 0) and prints the warning as a note. The
// --json form carries the whole result DTO.
func TestFindingsFileJSONCarriesWarning(t *testing.T) {
	fc := findingsFake()
	fc.FileFindingResult = apitypes.IncidentalFindingFileResultDTO{
		Issue:   apitypes.IncidentalFindingFiledIssueDTO{IID: 7, WebURL: "https://forge/x/7"},
		Warning: "created but not settled",
	}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "f-aaaa", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.IncidentalFindingFileResultDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json is not an IncidentalFindingFileResultDTO: %v\n%s", err, out)
	}
	if got.Issue.IID != 7 || got.Warning != "created but not settled" {
		t.Errorf("--json lost part of the result: %+v", got)
	}
}

// A 409 on file (the coordinate is already filed or being filed) is exit 5 (conflict), straight
// from statusError.
func TestFindingsFileConflictExit5(t *testing.T) {
	fc := findingsFake()
	fc.FileFindingErr = uzicli.Exitf(uzicli.ExitConflict, "this finding is already filed or being filed")
	_, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "f-aaaa")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
	// The write was REACHED with the right id before being refused.
	if fc.LastFileFindingID != "f-aaaa" {
		t.Errorf("the file write was never reached (id=%q)", fc.LastFileFindingID)
	}
}

// A 404 on file (unknown/foreign id) is exit 4 (not found).
func TestFindingsFileNotFoundExit4(t *testing.T) {
	fc := findingsFake()
	fc.FileFindingErr = uzicli.Exitf(uzicli.ExitNotFound, "finding not found")
	_, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// dismiss maps the hyphenated reason to its wire enum, forwards the id, and exits 0.
func TestFindingsDismissSuccess(t *testing.T) {
	fc := findingsFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "dismiss", "f-aaaa", "--reason", "wont-do")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDismissFindingID != "f-aaaa" || fc.LastDismissFindingReason != dispReasonWontDo {
		t.Errorf("dismiss wire = (%q,%q), want (f-aaaa, wont_do)", fc.LastDismissFindingID, fc.LastDismissFindingReason)
	}
	if !strings.Contains(out, "dismissed") {
		t.Errorf("dismiss output missing the outcome:\n%s", out)
	}
}

func TestFindingsDismissNotAnIssueMapsToWire(t *testing.T) {
	fc := findingsFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "dismiss", "f-aaaa", "--reason", "not-an-issue"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDismissFindingReason != dispReasonNotAnIssue {
		t.Errorf("reason mapped to %q, want not_an_issue", fc.LastDismissFindingReason)
	}
}

// A missing or invalid --reason is a usage error (exit 2) raised BEFORE any network call — the
// dismiss must write nothing when the invocation itself is wrong.
func TestFindingsDismissBadReasonWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing reason", []string{"findings", "dismiss", "f-aaaa"}},
		{"invalid reason", []string{"findings", "dismiss", "f-aaaa", "--reason", "because"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := findingsFake()
			_, _, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
			}
			if fc.LastDismissFindingID != "" {
				t.Errorf("a bad --reason must not reach the network, but the write recorded id %q", fc.LastDismissFindingID)
			}
		})
	}
}

// A 404 on dismiss (unknown/foreign id) is exit 4; a 409 (not dismissable) is exit 5.
func TestFindingsDismissNotFoundAndConflict(t *testing.T) {
	fc := findingsFake()
	fc.DismissFindingErr = uzicli.Exitf(uzicli.ExitNotFound, "finding not found")
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "dismiss", "nope", "--reason", "wont-do"); code != uzicli.ExitNotFound {
		t.Fatalf("dismiss 404: exit = %d, want %d", code, uzicli.ExitNotFound)
	}
	fc2 := findingsFake()
	fc2.DismissFindingErr = uzicli.Exitf(uzicli.ExitConflict, "cannot dismiss")
	if _, _, code := runCLI(t, fakeEnv(fc2), "findings", "dismiss", "f-aaaa", "--reason", "wont-do"); code != uzicli.ExitConflict {
		t.Fatalf("dismiss 409: exit = %d, want %d", code, uzicli.ExitConflict)
	}
}

// -------------------------------------------------------------------------
// PRD #1183 M5: the list renderer surfaces the dismiss reason and the issue-close done state,
// --bucket accepts `done`, --json carries the new DTO fields, and the new `stats`/`undo` verbs.
// -------------------------------------------------------------------------

// A dismissed row shows its reason where the DTO carries one — "Dismissed · Won't do" /
// "Dismissed · Not an issue" — the terminal twin of the web state chip, matching the judge
// vocabulary. dismiss_reason is a closed enum, so it prints raw (no sanitizeTTY).
func TestFindingsListRendersDismissReason(t *testing.T) {
	fc := &uzicli.FakeClient{FindingsResult: apitypes.IncidentalFindingBacklogDTO{
		Bucket: workersvc.BucketDismissed,
		Findings: []apitypes.IncidentalFindingDTO{
			{DispositionID: "d-1", FindingID: ptr("f-1"), Location: "a.go#f", RepoID: "repo-1", RepoPath: "team/api",
				Status: dispStatusDismissed, LastTitle: "wont fix this", SeenInRuns: 2, DismissReason: dispReasonWontDo},
			{DispositionID: "d-2", FindingID: ptr("f-2"), Location: "b.go#g", RepoID: "repo-1", RepoPath: "team/api",
				Status: dispStatusDismissed, LastTitle: "false alarm", SeenInRuns: 1, DismissReason: dispReasonNotAnIssue},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--bucket", workersvc.BucketDismissed)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"Dismissed · Won't do", "Dismissed · Not an issue"} {
		if !strings.Contains(out, want) {
			t.Errorf("dismissed row missing its reason %q:\n%s", want, out)
		}
	}
}

// A done row set by the issue-close sync reads "Done via #N" — the CLI form of the web's "Done
// via #N" chip. set_via and filed_issue_iid are structural (enum + int), so they print raw.
func TestFindingsListRendersDoneVia(t *testing.T) {
	iid := int64(42)
	fc := &uzicli.FakeClient{FindingsResult: apitypes.IncidentalFindingBacklogDTO{
		Bucket: workersvc.BucketDone,
		Findings: []apitypes.IncidentalFindingDTO{
			{DispositionID: "d-1", Location: "a.go#f", RepoID: "repo-1", RepoPath: "team/api",
				Status: dispStatusDone, LastTitle: "fixed by closing the issue", SeenInRuns: 1,
				SetVia: "issue_close", FiledIssueIID: &iid},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--bucket", workersvc.BucketDone)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Done via #42") {
		t.Errorf("done-via-issue-close row must read \"Done via #N\":\n%s", out)
	}
}

// `--bucket done` is accepted and forwarded VERBATIM — the fifth bucket the M3 server enum added.
func TestFindingsListBucketDoneForwarded(t *testing.T) {
	fc := findingsFake()
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--bucket", workersvc.BucketDone); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastFindingsBucket != workersvc.BucketDone {
		t.Errorf("bucket forwarded as %q, want %q", fc.LastFindingsBucket, workersvc.BucketDone)
	}
}

// --json carries the M3 DTO additions (disposition_id, dismiss_reason, set_via, evidence_preview,
// occurrences) through unchanged — the renderer marshals the DTO, so an agent sees every field.
func TestFindingsListJSONCarriesNewFields(t *testing.T) {
	iid := int64(7)
	fc := &uzicli.FakeClient{FindingsResult: apitypes.IncidentalFindingBacklogDTO{
		Bucket: workersvc.BucketAll,
		Findings: []apitypes.IncidentalFindingDTO{
			{
				DispositionID:   "disp-1",
				FindingID:       ptr("f-1"),
				Location:        "a.go#f",
				RepoID:          "repo-1",
				RepoPath:        "team/api",
				Status:          dispStatusDismissed,
				LastTitle:       "t",
				SeenInRuns:      2,
				DismissReason:   dispReasonWontDo,
				SetVia:          "issue_close",
				FiledIssueIID:   &iid,
				EvidencePreview: "the loop never exits",
				Occurrences: []apitypes.FindingOccurrenceDTO{
					{RunID: "run-a", RunTitle: "add search", ReportedAt: time.Now(), Confidence: "high"},
				},
			},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.IncidentalFindingBacklogDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not an IncidentalFindingBacklogDTO: %v\n%s", err, out)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("--json lost the finding row: %d findings", len(got.Findings))
	}
	f := got.Findings[0]
	if f.DispositionID != "disp-1" {
		t.Errorf("--json dropped disposition_id, got %q", f.DispositionID)
	}
	if f.DismissReason != dispReasonWontDo || f.SetVia != "issue_close" {
		t.Errorf("--json dropped dismiss_reason/set_via, got (%q,%q)", f.DismissReason, f.SetVia)
	}
	if f.EvidencePreview != "the loop never exits" {
		t.Errorf("--json dropped evidence_preview, got %q", f.EvidencePreview)
	}
	if len(f.Occurrences) != 1 || f.Occurrences[0].RunID != "run-a" {
		t.Errorf("--json dropped the occurrence detail: %+v", f.Occurrences)
	}
}

// `findings stats` renders the human table with the Findings vocabulary ("TO TRIAGE", not the
// judge's "TO DO") over the shared TriageDTO.
func TestFindingsStatsTable(t *testing.T) {
	fc := &uzicli.FakeClient{FindingsStatsResult: apitypes.TriageDTO{
		Total: 9, Todo: 4, Filed: 2, Done: 2, Dismissed: 1, FalsePositives: 1,
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "stats")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"TOTAL", "TO TRIAGE", "FILED", "DONE", "DISMISSED", "FALSE POSITIVES", "9"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats table missing %q:\n%s", want, out)
		}
	}
	// The open bucket reads "To triage" on Findings, never the judge's "TO DO".
	if strings.Contains(out, "TO DO") {
		t.Errorf("findings stats used the judge label 'TO DO' instead of 'TO TRIAGE':\n%s", out)
	}
}

// `findings stats --json` emits the raw (unenveloped) TriageDTO, exactly like `review stats`.
func TestFindingsStatsJSON(t *testing.T) {
	fc := &uzicli.FakeClient{FindingsStatsResult: apitypes.TriageDTO{Total: 3, FalsePositives: 1}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "stats", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"total": 3`) || !strings.Contains(out, `"false_positives": 1`) {
		t.Errorf("stats --json unexpected:\n%s", out)
	}
}

// --repo narrows the tally and is forwarded VERBATIM; an unset flag omits the parameter so the
// server tallies every repo. Substituting a default client-side would make the CLI a second
// definition of it, and the two could drift.
func TestFindingsStatsRepoForwarded(t *testing.T) {
	fc := &uzicli.FakeClient{FindingsStatsResult: apitypes.TriageDTO{Total: 1}}
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "stats", "--repo", "repo-9"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastFindingsStatsRepo != "repo-9" {
		t.Errorf("repo forwarded as %q, want repo-9", fc.LastFindingsStatsRepo)
	}

	fc2 := &uzicli.FakeClient{FindingsStatsResult: apitypes.TriageDTO{Total: 1}}
	if _, _, code := runCLI(t, fakeEnv(fc2), "findings", "stats"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc2.LastFindingsStatsRepo != "" {
		t.Errorf("an unset --repo must send nothing (server default), got %q", fc2.LastFindingsStatsRepo)
	}
}

// `findings undo <id>` forwards the disposition id and reports the reopen.
func TestFindingsUndoSuccess(t *testing.T) {
	fc := findingsFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "undo", "disp-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastUndoFindingID != "disp-1" {
		t.Errorf("undo forwarded id %q, want disp-1", fc.LastUndoFindingID)
	}
	if !strings.Contains(out, "reopened") {
		t.Errorf("undo output missing the outcome:\n%s", out)
	}
}

// undo softens the not-dismissed sentinel (the endpoint's 404 for a non-dismissed/foreign id) to
// a friendly line + exit 0 — treated as already-undone, never a crash. The write is still REACHED
// with the right id before the softening.
func TestFindingsUndoAlreadyUndoneExit0(t *testing.T) {
	fc := findingsFake()
	fc.UndoDismissFindingErr = uzicli.ErrFindingNotDismissed
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "undo", "disp-1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (already undone)", code)
	}
	if fc.LastUndoFindingID != "disp-1" {
		t.Errorf("the undo call was never reached (id=%q)", fc.LastUndoFindingID)
	}
	if !strings.Contains(out, "no dismissal to undo") {
		t.Errorf("want a friendly already-undone line, got:\n%s", out)
	}
}

// The sentinel softening is distinct from a hard failure: a real error (e.g. a 401 from an
// invalid CLI token) keeps its exit code rather than being swallowed as already-undone.
func TestFindingsUndoHardErrorPropagates(t *testing.T) {
	fc := findingsFake()
	fc.UndoDismissFindingErr = uzicli.Exitf(uzicli.ExitAuth, "invalid CLI token")
	_, _, code := runCLI(t, fakeEnv(fc), "findings", "undo", "disp-1")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
	if fc.LastUndoFindingID != "disp-1" {
		t.Errorf("the undo call was never reached (id=%q)", fc.LastUndoFindingID)
	}
}

// undo --json emits a small envelope on both the success and the softened-sentinel paths, so a
// --json consumer can tell reopened (undone:true) from already-undone (undone:false) without
// parsing a human line.
func TestFindingsUndoJSON(t *testing.T) {
	fc := findingsFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "undo", "disp-1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"undone": true`) {
		t.Errorf("undo --json success must carry undone:true:\n%s", out)
	}

	fc2 := findingsFake()
	fc2.UndoDismissFindingErr = uzicli.ErrFindingNotDismissed
	out2, _, code2 := runCLI(t, fakeEnv(fc2), "findings", "undo", "disp-1", "--json")
	if code2 != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (already undone)", code2)
	}
	if !strings.Contains(out2, `"undone": false`) {
		t.Errorf("undo --json already-undone must carry undone:false:\n%s", out2)
	}
}
