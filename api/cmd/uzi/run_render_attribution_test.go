package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1224 M6 — CLI `uzi run get` per-milestone agent attribution.
//
// The ATTRIBUTED case emits one `NOW <id>` row per effective-attributed in-progress milestone
// (declared role + label; live tool/age only on the D3 unique-matching lane) IN PLACE OF the single
// global NOW row; the UNATTRIBUTED case keeps the single global NOW row byte-for-byte (the M4
// baseline locks that). current_activity is dated in the future so relAge floors the age to
// "0s ago" deterministically.

// countNowRows counts the NOW-family rows in a rendered `run get`: the table left-aligns every key
// at column 0, so a NOW row (global "NOW" or attributed "NOW <id>") is a line starting with "NOW".
// The M4 CLI baseline's region slice stopped at the FIRST NOW line, so a spurious extra NOW row
// after it would have slipped past it — this full-output scan is the tightening that closes that gap.
func countNowRows(out string) int {
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "NOW") {
			n++
		}
	}
	return n
}

func TestMilestoneNowRowsMultiUniqueMatch(t *testing.T) {
	at := time.Now().Add(2 * time.Hour) // relAge floors a not-yet timestamp to "0s"
	r := apitypes.RunDTO{
		ID: "run-1224-cli-multi", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
		// The live activity's agent is "coder" so m2 is the D3 unique-matching lane (tool/age); its
		// declared role/label ("coder"/"Wire the limiter") are shown, NOT the activity's own label.
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}

	got := milestoneNowRows(r)
	// PRD #1353 M1: the D3 unique-matching (live) lane m2 keeps its `NOW m2` live row; the idle
	// declared owner m3 demotes to a QUIET `OWNER m3` tag row (role + label only, no NOW, no age).
	want := [][]string{
		{"NOW m2", "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago"},
		{"OWNER m3", "tester · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1353 M1 milestoneNowRows drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	// Through the composed render: exactly ONE NOW row (the live NOW m2). The global nowRow is
	// SKIPPED because "coder" uniquely matches m2 — its live info already rides that row, so
	// emitting the global too would duplicate it. The idle owner reads as OWNER m3, never NOW m3.
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: a uniquely-matched live agent must emit exactly 1 NOW row (the live NOW <id>, global skipped), got %d:\n%s", n, out)
	}
	for _, sub := range []string{
		"NOW m2",
		"coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago",
		"OWNER m3",
		"tester · Add the sweep",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1353 M1: attributed `run get` output missing %q:\n%s", sub, out)
		}
	}
	if strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: an idle declared owner must read as `OWNER m3`, never `NOW m3`:\n%s", out)
	}
}

// TestMilestoneNowRowsRepeatedRoleShowsGlobalNow pins D3's repeated-role suppression AND the PRD #1353
// M1 honesty fix on the CLI: two milestones attributed to the SAME role that also matches the activity
// means the unique-match is ambiguous, so NEITHER milestone row carries the live tool/age — each demotes
// to a QUIET `OWNER <id>` tag row (declared role + label only). Because no `NOW <id>` row then carries
// the live agent, the composed render emits the single GLOBAL NOW row so the live "coder"/tool/age stays
// visible (honesty: the live agent is never suppressed).
func TestMilestoneNowRowsRepeatedRoleShowsGlobalNow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1353-cli-repeated", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "coder", AgentLabel: "Add the sweep"},
		},
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	got := milestoneNowRows(r)
	want := [][]string{
		{"OWNER m2", "coder · Wire the limiter"},
		{"OWNER m3", "coder · Add the sweep"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1353 M1 CLI repeated-role render drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	// Through the composed render: NO `NOW <id>` row carries the ambiguous live agent, so the single
	// global NOW row appears (countNowRows == 1) carrying "coder" + tool + age — the honesty guarantee.
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: an ambiguous repeated role must show the single global NOW row (countNowRows == 1), got %d:\n%s", n, out)
	}
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: ambiguous owners must read as `OWNER <id>`, never `NOW <id>`:\n%s", out)
	}
	if !strings.Contains(out, "coder · coder busy · Edit api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1353 M1: the global NOW row must carry the live coder/tool/age:\n%s", out)
	}
}

// TestRenderRunDetailUnattributedExactlyOneNowRow closes the known narrowing in the M4 CLI baseline
// (its region slice stopped at the first NOW, so an extra NOW row after it would slip through): with
// MilestonesAgents nil and two in-progress milestones, the whole `run get` output must carry EXACTLY
// ONE NOW row — the single GLOBAL current_activity row — and no per-milestone `NOW <id>` row at all.
func TestRenderRunDetailUnattributedExactlyOneNowRow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1224-cli-unattributed", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents:     nil,
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "Wire the limiter", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1224 D8: unattributed `run get` must emit exactly ONE global NOW row, got %d:\n%s", n, out)
	}
	// And it is the GLOBAL row (no milestone id), carrying the activity's OWN label — never a
	// per-milestone `NOW <id>` row.
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1224 D8: unattributed `run get` must not emit a per-milestone NOW row:\n%s", out)
	}
	if !strings.Contains(out, "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1224 D8: unattributed `run get` lost the global NOW row content:\n%s", out)
	}
}

// TestRenderRunDetailNonOwnerLiveAgentShowsGlobalNow is the PRD #1353 M1 regression pin: a live agent
// that matches NO declared owner (a reviewer/tester) must stay visible. m2→coder and m3→tester are both
// in progress, but the live activity is "reviewer", which matches neither owner. So both milestones read
// as QUIET `OWNER <id>` tag rows, and the single GLOBAL NOW row is emitted carrying "reviewer" + tool +
// age. Pre-fix, milestoneNowRows emitted `NOW m2`/`NOW m3` and the `else if` suppressed the global, so
// the live reviewer vanished entirely — this test fails on that code and passes after M1.
func TestRenderRunDetailNonOwnerLiveAgentShowsGlobalNow(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1353-cli-nonowner", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "tester", AgentLabel: "Add the sweep"},
		},
		// The live agent is a reviewer — it matches NO declared owner, so it must not vanish.
		CurrentActivity: &apitypes.RunActivity{
			Agent: "reviewer", AgentLabel: "Review the diff", Tool: "Read",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	// Both declared owners read as quiet OWNER tag rows, not live NOW rows.
	for _, sub := range []string{"OWNER m2", "OWNER m3"} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1353 M1: idle declared owner missing quiet tag row %q:\n%s", sub, out)
		}
	}
	if strings.Contains(out, "NOW m2") || strings.Contains(out, "NOW m3") {
		t.Errorf("PRD #1353 M1: an idle declared owner must read as `OWNER <id>`, never `NOW <id>`:\n%s", out)
	}
	// The live non-owner reviewer is visible on the single global NOW row.
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M1: a live non-owner agent must show the single global NOW row (countNowRows == 1), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "reviewer · Review the diff · Read api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1353 M1: the global NOW row must carry the live reviewer/tool/age (the honesty fix):\n%s", out)
	}
}

// PRD #1353 M5 — CLI `uzi run get` LIVE LANES.
//
// When the run HAS LANES (a non-terminal run with ≥1 in-progress milestone carrying lanes in
// MilestonesLive), milestoneLaneRows is the AUTHORITATIVE live display and SUPERSEDES the M1
// single-current_activity path: it emits a QUIET `OWNER <id>` row per declared owner plus one
// `NOW <id>` row per LANE, and the M1 global/unattached now-line is NOT emitted. The lanes' At is
// future-dated so relAge floors the age to "0s ago" deterministically.

// TestMilestoneLaneRowsLivesSupersedeM1 pins the lanes branch AND the owner-line dedup fix. Three
// in-progress milestones exercise every owner shape:
//
//   - m2 has a declared owner "coder" that IS one of its live lanes (owner-is-live, the common case)
//     plus a second reviewer lane. The quiet `OWNER m2` row is SUPPRESSED — the `NOW m2` coder lane
//     row already carries the owner live, so an OWNER row would only duplicate it — leaving two
//     `NOW m2` rows.
//   - m3 has an IDLE declared owner "architect" (its agent is not among m3's lanes), so the quiet
//     `OWNER m3` "assigned to" row is kept above the `NOW m3` lane row.
//   - m4 has a lane but NO declared owner — just a `NOW m4` row (no `OWNER m4`).
//
// current_activity is set (a coder that would uniquely match m2 under M1), but in the lanes branch it
// is suppressed — its distinctive detail must NOT appear, and no global `NOW` row is emitted.
func TestMilestoneLaneRowsLivesSupersedeM1(t *testing.T) {
	at := time.Now().Add(2 * time.Hour) // relAge floors a not-yet timestamp to "0s"
	r := apitypes.RunDTO{
		ID: "run-1353-cli-lanes", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"}, {ID: "m4", Title: "Delta"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3", "m4"},
		// m2's owner is live (coder is a lane); m3's owner is idle (architect is not a lane); m4 has no
		// owner — the three shapes the OWNER row must distinguish.
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
			{ID: "m3", Agent: "architect", AgentLabel: "Design the API"},
		},
		MilestonesLive: []apitypes.MilestoneLive{
			{MilestoneID: "m2", Lanes: []apitypes.MilestoneLane{
				{Agent: "coder", AgentInstance: "toolu_a", AgentLabel: "Wire the limiter", Tool: "Edit", Detail: "window.go", At: at},
				{Agent: "reviewer", AgentInstance: "toolu_b", AgentLabel: "Review A", Tool: "Read", Detail: "a.go", At: at},
			}},
			{MilestoneID: "m3", Lanes: []apitypes.MilestoneLane{
				{Agent: "tester", AgentInstance: "toolu_c", AgentLabel: "Sweep", Tool: "Bash", Detail: "run tests", At: at},
			}},
			{MilestoneID: "m4", Lanes: []apitypes.MilestoneLane{
				{Agent: "reviewer", AgentInstance: "toolu_d", AgentLabel: "Audit", Tool: "Read", Detail: "c.go", At: at},
			}},
		},
		// A live current_activity that WOULD ride a global/M1 NOW row (its detail is the unique marker
		// "api/internal/limits/window.go") — the lanes branch must suppress it entirely.
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}

	got := milestoneLaneRows(r)
	want := [][]string{
		// m2: owner coder is live → NO OWNER m2 row, just the two lane rows.
		{"NOW m2", "coder · Wire the limiter · Edit window.go · 0s ago"},
		{"NOW m2", "reviewer · Review A · Read a.go · 0s ago"},
		// m3: owner architect is idle → quiet OWNER m3 row kept above the lane row.
		{"OWNER m3", "architect · Design the API"},
		{"NOW m3", "tester · Sweep · Bash run tests · 0s ago"},
		// m4: no declared owner → lane row only.
		{"NOW m4", "reviewer · Audit · Read c.go · 0s ago"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRD #1353 M5 milestoneLaneRows drifted.\n--- got ---\n%#v\n--- want ---\n%#v", got, want)
	}

	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	// Four `NOW <id>` rows (two lanes on m2, one each on m3/m4), and NO global `NOW` row (its
	// distinctive current_activity detail must be absent — lanes supersede it).
	if n := countNowRows(out); n != 4 {
		t.Errorf("PRD #1353 M5: a run with lanes must emit one NOW row per lane (4 here), got %d:\n%s", n, out)
	}
	for _, sub := range []string{
		"NOW m2",
		"coder · Wire the limiter · Edit window.go · 0s ago",
		"reviewer · Review A · Read a.go · 0s ago",
		"OWNER m3",
		"architect · Design the API",
		"NOW m3",
		"tester · Sweep · Bash run tests · 0s ago",
		"NOW m4",
		"reviewer · Audit · Read c.go · 0s ago",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("PRD #1353 M5: lanes `run get` output missing %q:\n%s", sub, out)
		}
	}
	// The owner-line dedup fix: m2's owner is live (a lane), so no quiet `OWNER m2` row duplicates it;
	// m4 has no declared owner, so no `OWNER m4` row either.
	for _, gone := range []string{"OWNER m2", "OWNER m4"} {
		if strings.Contains(out, gone) {
			t.Errorf("PRD #1353 owner-dedup: %q must not appear (owner is live, or no owner declared):\n%s", gone, out)
		}
	}
	// The M1 current_activity display is superseded: its unique detail and its own label must not appear.
	for _, gone := range []string{"api/internal/limits/window.go", "coder busy"} {
		if strings.Contains(out, gone) {
			t.Errorf("PRD #1353 M5: the lanes branch must suppress the M1 current_activity display, but %q appeared:\n%s", gone, out)
		}
	}
}

// TestMilestonesLiveIndex covers milestonesLiveIndex's exclusion/dedup branches directly (Finding 2):
// the happy path only ever hits the inclusion branch, so a regression in an exclusion — most sharply
// the empty-lanes skip, which would silently suppress a now-line if it dropped a real entry — would go
// unnoticed. Each case builds a RunDTO and asserts the returned map's keys and length.
func TestMilestonesLiveIndex(t *testing.T) {
	at := time.Now()
	lane := func(detail string) apitypes.MilestoneLane {
		return apitypes.MilestoneLane{Agent: "coder", Tool: "Edit", Detail: detail, At: at}
	}
	cases := []struct {
		name     string
		run      apitypes.RunDTO
		wantKeys []string
	}{
		{
			name: "genuine in-progress id with lanes is included",
			run: apitypes.RunDTO{
				Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}},
				MilestonesInProgress: []string{"m1"},
				MilestonesLive:       []apitypes.MilestoneLive{{MilestoneID: "m1", Lanes: []apitypes.MilestoneLane{lane("a.go")}}},
			},
			wantKeys: []string{"m1"},
		},
		{
			name: "completed id with lanes is excluded",
			run: apitypes.RunDTO{
				Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}},
				MilestonesInProgress: []string{"m1"},
				MilestonesCompleted:  []string{"m1"},
				MilestonesLive:       []apitypes.MilestoneLive{{MilestoneID: "m1", Lanes: []apitypes.MilestoneLane{lane("a.go")}}},
			},
			wantKeys: nil,
		},
		{
			name: "non-frozen id is excluded",
			run: apitypes.RunDTO{
				Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}},
				MilestonesInProgress: []string{"m1", "ghost"},
				MilestonesLive:       []apitypes.MilestoneLive{{MilestoneID: "ghost", Lanes: []apitypes.MilestoneLane{lane("a.go")}}},
			},
			wantKeys: nil,
		},
		{
			name: "non-in-progress id is excluded",
			run: apitypes.RunDTO{
				Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}},
				MilestonesInProgress: []string{"m1"},
				MilestonesLive:       []apitypes.MilestoneLive{{MilestoneID: "m2", Lanes: []apitypes.MilestoneLane{lane("a.go")}}},
			},
			wantKeys: nil,
		},
		{
			name: "entry with empty lanes is skipped while a real sibling survives",
			run: apitypes.RunDTO{
				Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}},
				MilestonesInProgress: []string{"m1", "m2"},
				MilestonesLive: []apitypes.MilestoneLive{
					{MilestoneID: "m1", Lanes: nil}, // the unique empty-lanes skip — must not suppress the sibling
					{MilestoneID: "m2", Lanes: []apitypes.MilestoneLane{lane("b.go")}},
				},
			},
			wantKeys: []string{"m2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := milestonesLiveIndex(tc.run)
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("milestonesLiveIndex len = %d, want %d\n%#v", len(got), len(tc.wantKeys), got)
			}
			for _, k := range tc.wantKeys {
				if _, ok := got[k]; !ok {
					t.Errorf("milestonesLiveIndex missing key %q\n%#v", k, got)
				}
			}
		})
	}

	// First-occurrence-wins on a duplicated milestone id (mirrors the server's first-valid-wins): the
	// SECOND {m1, …} entry is dropped, so out["m1"] carries the FIRST entry's lanes.
	t.Run("first occurrence wins on duplicate id", func(t *testing.T) {
		run := apitypes.RunDTO{
			Milestones:           []apitypes.Milestone{{ID: "m1", Title: "Alpha"}},
			MilestonesInProgress: []string{"m1"},
			MilestonesLive: []apitypes.MilestoneLive{
				{MilestoneID: "m1", Lanes: []apitypes.MilestoneLane{lane("first")}},
				{MilestoneID: "m1", Lanes: []apitypes.MilestoneLane{lane("second")}},
			},
		}
		got := milestonesLiveIndex(run)
		if len(got) != 1 {
			t.Fatalf("milestonesLiveIndex len = %d, want 1\n%#v", len(got), got)
		}
		if lanes := got["m1"]; len(lanes) != 1 || lanes[0].Detail != "first" {
			t.Errorf("first-occurrence-wins violated: out[\"m1\"] = %#v, want the FIRST entry's lane (Detail \"first\")", lanes)
		}
	})
}

// TestMilestoneLaneRowsBackCompatNilLive is the D5 back-compat pin: with MilestonesLive nil,
// milestoneLaneRows returns nil and renderRunDetail falls to the M1 path byte-for-byte. The SAME run
// as the lanes test minus MilestonesLive: the coder current_activity uniquely matches m2's declared
// owner, so M1 emits a single `NOW m2` live row carrying the current_activity detail, and countNowRows
// is 1 (the unattributed/attributed M1 contract, unchanged).
func TestMilestoneLaneRowsBackCompatNilLive(t *testing.T) {
	at := time.Now().Add(2 * time.Hour)
	r := apitypes.RunDTO{
		ID: "run-1353-cli-nolive", Kind: "issue", Status: "running", IssueTitle: "Add rate limiting",
		Milestones: []apitypes.Milestone{
			{ID: "m1", Title: "Alpha"}, {ID: "m2", Title: "Beta"}, {ID: "m3", Title: "Gamma"},
		},
		MilestonesCompleted:  []string{"m1"},
		MilestonesInProgress: []string{"m2", "m3"},
		MilestonesAgents: []apitypes.MilestoneAgent{
			{ID: "m2", Agent: "coder", AgentLabel: "Wire the limiter"},
		},
		MilestonesLive: nil, // the whole point: no lanes ⇒ the M1 path unchanged
		CurrentActivity: &apitypes.RunActivity{
			Agent: "coder", AgentLabel: "coder busy", Tool: "Edit",
			Detail: "api/internal/limits/window.go", At: at, Seq: 12,
		},
	}
	if got := milestoneLaneRows(r); got != nil {
		t.Errorf("PRD #1353 M5 D5: milestoneLaneRows must return nil when MilestonesLive is nil, got %#v", got)
	}

	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	// M1 path: coder uniquely matches m2, so its live info rides `NOW m2` (the global is skipped),
	// countNowRows == 1, and the current_activity detail IS shown (the M1 behaviour, not superseded).
	if n := countNowRows(out); n != 1 {
		t.Errorf("PRD #1353 M5 D5: back-compat (no lanes) must render the M1 output (countNowRows == 1), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "NOW m2") {
		t.Errorf("PRD #1353 M5 D5: back-compat must keep the M1 `NOW m2` live row:\n%s", out)
	}
	if !strings.Contains(out, "coder · Wire the limiter · Edit api/internal/limits/window.go · 0s ago") {
		t.Errorf("PRD #1353 M5 D5: back-compat must keep the M1 current_activity detail on the NOW row:\n%s", out)
	}
}
