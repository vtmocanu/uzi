package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// fixtures/ sits above the api module, so a fixture-only edit is invisible to the
// test cache: run this package with -count=1 after editing contract.json.
func readSourceLabels(t *testing.T) []struct{ Field, Token, Label string } {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/model-settings/contract.json")
	if err != nil {
		t.Fatalf("contract fixture unreadable: %v", err)
	}
	var doc struct {
		SourceLabels []struct{ Field, Token, Label string } `json:"source_labels"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.SourceLabels) == 0 {
		t.Fatal("contract.json has no source_labels")
	}
	return doc.SourceLabels
}

func TestModelSettingSourceMatchesContractFixture(t *testing.T) {
	rows := readSourceLabels(t)
	ran := 0
	for _, r := range rows {
		if got := modelSettingSource(r.Field, r.Token); got != r.Label {
			t.Errorf("modelSettingSource(%q,%q) = %q, want %q", r.Field, r.Token, got, r.Label)
		}
		ran++
	}
	if ran != len(rows) {
		t.Fatalf("ran %d of %d rows", ran, len(rows))
	}
	// The Go table must not carry rows the fixture lacks.
	if len(modelSettingLabels) != len(rows) {
		t.Errorf("Go label table has %d rows, fixture %d", len(modelSettingLabels), len(rows))
	}
	for _, c := range [][2]string{{"model", "bogus"}, {"effort", "bogus"}, {"", ""}} {
		if got := modelSettingSource(c[0], c[1]); got != "unknown" {
			t.Errorf("modelSettingSource(%q,%q) = %q, want unknown", c[0], c[1], got)
		}
	}
}

func TestSettingsGetModelSettingsTable(t *testing.T) {
	pin := "haiku"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{JudgeModels: []apitypes.ModelSettingCellDTO{
		{Harness: "claude", Model: &pin, DefaultModel: "opus", DefaultSource: "builtin", ResolvedModel: "haiku", Source: "user_default"},
		{Harness: "codex", DefaultModel: "gpt-6-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6-sol", Source: "instance_default"},
		{Harness: "third", DefaultModel: "ignored", ResolvedModel: "ignored", Source: "builtin"},
	}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	idx := strings.Index(out, "Model settings")
	if idx < 0 {
		t.Fatalf("no Model settings table: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out[idx:]), "\n")
	// Title, header, and exactly two rows (the third cell is never read), then the Now line.
	want := []string{
		"Model settings",
		"SETTING HARNESS STORED DEFAULT RESOLVED SOURCE",
		"judge claude Pin · haiku opus haiku your choice",
		"judge codex Default gpt-6-sol gpt-6-sol instance default",
	}
	for i, w := range want {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != w {
			t.Errorf("line %d: %q, want %q", i, got, w)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("third cell was read: %q", out)
	}
}

func TestSettingsGetModelSettingsUnknownAndHostile(t *testing.T) {
	hostile := "evil\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e" + strings.Repeat("x", 10000)
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{JudgeModels: []apitypes.ModelSettingCellDTO{
		{Harness: hostile, Model: &hostile, DefaultModel: hostile, ResolvedModel: hostile, Source: "from_the_future"},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, strings.Repeat("x", 201)) ||
		!strings.Contains(out, "unknown") {
		t.Fatalf("unsafe or unlabelled output: exit %d %q", code, out)
	}
}

func TestSettingsGetNoModelSettingsWithoutCells(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "settings", "get")
	if code != 0 || strings.Contains(out, "Model settings") {
		t.Fatalf("legacy response must not print the table: %d %q", code, out)
	}
}

func TestSettingRowsBuiltThroughNeutralRow(t *testing.T) {
	stored := "x"
	got := settingRowsTable([]settingRow{{Setting: "effort", Harness: "claude", Stored: &stored,
		Default: "d", Resolved: "r", Field: "model", Source: "builtin"}})
	if len(got) != 1 || strings.Join(got[0], "|") != "effort|claude|Pin · x|d|r|built-in" {
		t.Fatalf("rows = %v", got)
	}
	rows := modelSettingRows("judge", []apitypes.ModelSettingCellDTO{{Harness: "codex", Source: "builtin"}})
	if len(rows) != 1 || rows[0].Field != "model" || rows[0].Setting != "judge" || rows[0].Stored != nil {
		t.Fatalf("adapter rows = %+v", rows)
	}
}

func TestSettingsGetJSONCarriesJudgeFields(t *testing.T) {
	codex := "gpt-6-sol"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{CodexJudgeModel: &codex,
		JudgeModels: []apitypes.ModelSettingCellDTO{{Harness: "codex", Model: &codex, ResolvedModel: codex, Source: "user_default"}}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	var got apitypes.UserSettingsDTO
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.CodexJudgeModel == nil ||
		*got.CodexJudgeModel != codex || len(got.JudgeModels) != 1 || got.JudgeModels[0].Source != "user_default" {
		t.Fatalf("json = %s", out)
	}
}

func TestSettingsGetEffortRowsRuntimeDefault(t *testing.T) {
	runtime, level := "runtime", "high"
	pinned := "runtime" // a judge MODEL pin that happens to be named runtime stays a pin
	medium := "medium"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{
		JudgeModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", Model: &pinned, DefaultModel: "opus", DefaultSource: "builtin", ResolvedModel: "runtime", Source: "user_default"},
		},
		EffortSettings: []apitypes.EffortSettingCellDTO{
			{Harness: "claude", Effort: &runtime, DefaultEffort: "medium", DefaultSource: "builtin", Source: "user_default", Mode: "runtime_default"},
			{Harness: "codex", Effort: &level, DefaultEffort: "medium", DefaultSource: "builtin", ResolvedEffort: &level, Source: "user_default", Mode: "explicit"},
			{Harness: "third", ResolvedEffort: &medium, Source: "builtin", Mode: "explicit"},
		},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	idx := strings.Index(out, "Model settings")
	if idx < 0 {
		t.Fatalf("no Model settings table: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out[idx:]), "\n")
	want := []string{
		"Model settings",
		"SETTING HARNESS STORED DEFAULT RESOLVED SOURCE",
		"judge claude Pin · runtime opus runtime your choice",
		"effort claude runtime default medium runtime default (value not reported) your choice",
		"effort codex Pin · high medium high your choice",
	}
	for i, w := range want {
		if got := strings.Join(strings.Fields(lines[i]), " "); got != w {
			t.Errorf("line %d: %q, want %q", i, got, w)
		}
	}
	if strings.Contains(out, "third") {
		t.Errorf("third effort cell was read: %q", out)
	}
}

func TestSettingsGetCodexEffortRuntimeDefaultRow(t *testing.T) {
	runtime := "runtime"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{EffortSettings: []apitypes.EffortSettingCellDTO{
		{Harness: "codex", Effort: &runtime, DefaultEffort: "medium", DefaultSource: "builtin", Source: "user_default", Mode: "runtime_default"},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "effort codex runtime default medium runtime default (catalog default unknown) your choice") {
		t.Fatalf("codex runtime row missing: exit %d %q", code, out)
	}
}

// The codex Runtime default RESOLVED text carries the catalog default per state (issue
// #3251); explicit levels and the claude row are unchanged.
func TestEffortSettingRowsCodexCatalogDefault(t *testing.T) {
	known := func(state, effort string) *apitypes.CatalogDefaultDTO {
		return &apitypes.CatalogDefaultDTO{State: state, Effort: strp(effort), Model: strp("gpt-6.1-sol"), RuntimeVersion: strp("0.164.0")}
	}
	cases := []struct {
		name string
		cd   *apitypes.CatalogDefaultDTO
		want string
	}{
		{"known", known("known", "low"), "runtime default (catalog default: low, gpt-6.1-sol, Codex 0.164.0)"},
		{"none", known("known", "none"), "runtime default (catalog default: none, no reasoning level requested, gpt-6.1-sol, Codex 0.164.0)"},
		{"ultra", known("known", "ultra"), "runtime default (catalog default: ultra, Codex substitutes another level, gpt-6.1-sol, Codex 0.164.0)"},
		{"varies", known("varies", "medium"), "runtime default (catalog default: medium, gpt-6.1-sol, Codex 0.164.0) · may differ on workers still on another version"},
		{"unknown", &apitypes.CatalogDefaultDTO{State: "unknown"}, "runtime default (catalog default unknown)"},
		{"absent", nil, "runtime default (catalog default unknown)"},
		{"known without values", &apitypes.CatalogDefaultDTO{State: "known"}, "runtime default (catalog default unknown)"},
		{"future state", known("from_the_future", "low"), "runtime default (catalog default unknown)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := effortSettingRows([]apitypes.EffortSettingCellDTO{
				{Harness: "claude", Effort: strp("runtime"), Source: "user_default", Mode: "runtime_default", CatalogDefault: c.cd},
				{Harness: "codex", Effort: strp("runtime"), Source: "user_default", Mode: "runtime_default", CatalogDefault: c.cd},
			})
			if len(rows) != 2 || rows[0].Resolved != "runtime default (value not reported)" || rows[1].Resolved != c.want {
				t.Fatalf("rows = %+v, want codex %q and the claude row unchanged", rows, c.want)
			}
		})
	}
	explicit := effortSettingRows([]apitypes.EffortSettingCellDTO{
		{Harness: "codex", Effort: strp("high"), ResolvedEffort: strp("high"), Source: "user_default", Mode: "explicit", CatalogDefault: known("known", "low")},
	})
	if explicit[0].Resolved != "high" {
		t.Fatalf("explicit codex row = %q, want high", explicit[0].Resolved)
	}
}

func TestSettingsGetCodexCatalogDefaultHostile(t *testing.T) {
	hostile := "evil\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e\nforged row"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{EffortSettings: []apitypes.EffortSettingCellDTO{
		{Harness: "codex", Effort: strp("runtime"), DefaultEffort: "medium", Source: "user_default", Mode: "runtime_default",
			CatalogDefault: &apitypes.CatalogDefaultDTO{State: "varies", Effort: &hostile, Model: &hostile, RuntimeVersion: &hostile}},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, "\nforged row") ||
		!strings.Contains(out, "catalog default:") {
		t.Fatalf("unsafe or missing catalog text: exit %d %q", code, out)
	}
}

func TestDemoCatalogDefaultSamplesPerState(t *testing.T) {
	now := time.Now()
	for state, want := range map[string]string{
		"known":   "runtime default (catalog default: low, gpt-6.1-sol, Codex 0.164.0)",
		"varies":  "runtime default (catalog default: low, gpt-6.1-sol, Codex 0.164.0) · may differ on workers still on another version",
		"unknown": "runtime default (catalog default unknown)",
	} {
		if got := codexRuntimeDefaultResolved(demoCatalogDefault(state, now)); got != want {
			t.Errorf("demo %s = %q, want %q", state, got, want)
		}
	}
}

func TestSettingsGetEffortRowsHostile(t *testing.T) {
	hostile := "evil\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e" + strings.Repeat("x", 10000)
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{EffortSettings: []apitypes.EffortSettingCellDTO{
		{Harness: hostile, Effort: &hostile, DefaultEffort: hostile, ResolvedEffort: &hostile, Source: "from_the_future", Mode: hostile},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, strings.Repeat("x", 201)) ||
		!strings.Contains(out, "unknown") {
		t.Fatalf("unsafe or unlabelled output: exit %d %q", code, out)
	}
}

func TestSettingRowStoredRuntimeIsEffortOnly(t *testing.T) {
	runtime := "runtime"
	for field, want := range map[string]string{"effort": "runtime default", "model": "Pin · runtime"} {
		got := settingRowsTable([]settingRow{{Setting: "s", Harness: "h", Stored: &runtime, Field: field, Source: "builtin"}})
		if len(got) != 1 || got[0][2] != want {
			t.Errorf("field %s: STORED = %v, want %q", field, got, want)
		}
	}
}

// RESOLVED follows the cell's mode, not the presence of resolved_effort.
func TestEffortSettingRowsResolvedFollowsMode(t *testing.T) {
	rows := effortSettingRows([]apitypes.EffortSettingCellDTO{
		{Harness: "claude", Effort: strp("runtime"), DefaultEffort: "medium", ResolvedEffort: strp("high"), Source: "user_default", Mode: "runtime_default"},
		{Harness: "codex", Effort: strp("high"), DefaultEffort: "medium", Source: "user_default", Mode: "explicit"},
	})
	if len(rows) != 2 || rows[0].Resolved != "runtime default (value not reported)" || rows[1].Resolved != "unknown" {
		t.Fatalf("rows = %+v, want runtime default label for runtime_default and unknown for explicit without a level", rows)
	}
}

// Summary rows follow the judge rows and precede the effort rows (PRD #3248): a
// stored pin, an instance default with nothing stored, an unknown source token, and
// a third cell that is never read.
func TestSettingsGetSummaryRows(t *testing.T) {
	pin, judgePin := "sonnet", "opus"
	level := "high"
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{
		JudgeModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", Model: &judgePin, DefaultModel: "opus", DefaultSource: "builtin", ResolvedModel: "opus", Source: "user_default"},
		},
		SummaryModels: []apitypes.ModelSettingCellDTO{
			{Harness: "claude", Model: &pin, DefaultModel: "haiku", DefaultSource: "builtin", ResolvedModel: "sonnet", Source: "user_default"},
			{Harness: "codex", DefaultModel: "gpt-6.1-sol", DefaultSource: "instance_default", ResolvedModel: "gpt-6.1-sol", Source: "instance_default"},
			{Harness: "third", DefaultModel: "ignored", ResolvedModel: "ignored", Source: "builtin"},
		},
		EffortSettings: []apitypes.EffortSettingCellDTO{
			{Harness: "codex", Effort: &level, DefaultEffort: "medium", DefaultSource: "builtin", ResolvedEffort: &level, Source: "user_default", Mode: "explicit"},
		},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	idx := strings.Index(out, "Model settings")
	if idx < 0 {
		t.Fatalf("no Model settings table: %q", out)
	}
	lines := strings.Split(strings.TrimSpace(out[idx:]), "\n")
	want := []string{
		"Model settings",
		"SETTING HARNESS STORED DEFAULT RESOLVED SOURCE",
		"judge claude Pin · opus opus opus your choice",
		"summary claude Pin · sonnet haiku sonnet your choice",
		"summary codex Default gpt-6.1-sol gpt-6.1-sol instance default",
		"effort codex Pin · high medium high your choice",
	}
	for i, w := range want {
		if i >= len(lines) {
			t.Fatalf("missing line %d (%q) in %q", i, w, out)
		}
		if got := strings.Join(strings.Fields(lines[i]), " "); got != w {
			t.Errorf("line %d: %q, want %q", i, got, w)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Errorf("third summary cell was read: %q", out)
	}
}

func TestSettingsGetSummaryRowsUnknownSource(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{SummaryModels: []apitypes.ModelSettingCellDTO{
		{Harness: "claude", DefaultModel: "haiku", ResolvedModel: "haiku", Source: "from_the_future"},
	}}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || !strings.Contains(strings.Join(strings.Fields(out), " "), "summary claude Default haiku haiku unknown") {
		t.Fatalf("unknown source row missing: exit %d %q", code, out)
	}
}
