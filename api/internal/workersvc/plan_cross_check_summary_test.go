package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This ordinary Store deliberately has no optional summary query method.
type summaryOrdinaryStore struct{ Store }

type summaryTestStore struct {
	Store
	row   store.GetLatestPlanCrossCheckSummaryRow
	err   error
	arg   store.GetLatestPlanCrossCheckSummaryParams
	calls int
}

func (q *summaryTestStore) GetLatestPlanCrossCheckSummary(_ context.Context, arg store.GetLatestPlanCrossCheckSummaryParams) (store.GetLatestPlanCrossCheckSummaryRow, error) {
	q.calls++
	q.arg = arg
	return q.row, q.err
}

func TestPlanCrossCheckSummaryResults(t *testing.T) {
	for _, pair := range [][2]string{
		{"approve", "approve"}, {"revise", "revise"}, {"block", "block"},
		{"failed", "malformed"}, {"failed", "model_error"}, {"failed", "model_timeout"},
		{"failed", "checker_unavailable"}, {"failed", "confinement_failed"},
		{"pending", ""}, {"failed", "timed_out"}, {"failed", "superseded"}, {"failed", "approved_not_stored"},
	} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{
				Round: 1, Verdict: pair[0], ReasonClass: pgtype.Text{String: pair[1], Valid: pair[1] != ""},
				CheckerModel:  pgtype.Text{String: "recorded-model", Valid: true},
				CheckerEffort: pgtype.Text{String: "high", Valid: true}, Historical: true,
			}}
			wantFindings := pair[0] != "pending" && pair[1] != "timed_out" && pair[1] != "superseded"
			if wantFindings {
				q.row.Findings = []byte(`{"summary":"**check**","items":[]}`)
			}
			owner, lead := uuid.New(), uuid.New()
			got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), owner, lead)
			if err != nil || got == nil {
				t.Fatalf("read: %v %v", got, err)
			}
			if q.arg.UserID != owner || q.arg.LeadRunID != lead || got.Verdict != pair[0] || got.Historical != true {
				t.Fatalf("identity/result: %+v", got)
			}
			if pair[1] == "" {
				if got.ReasonClass != nil {
					t.Fatal("invented reason")
				}
			} else if got.ReasonClass == nil || *got.ReasonClass != pair[1] {
				t.Fatal("lost actual reason")
			}
			if (got.Findings != nil) != wantFindings || got.Usage != nil || got.CheckerRunID != nil || *got.CheckerModel != "recorded-model" || *got.CheckerEffort != "high" {
				t.Fatalf("optional metadata: %+v", got)
			}
		})
	}
}

func TestPlanCrossCheckSummaryAbsenceAndErrors(t *testing.T) {
	for _, err := range []error{pgx.ErrNoRows, errors.New("read failed")} {
		q := &summaryTestStore{err: err}
		got, actual := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
		if got != nil || (errors.Is(err, pgx.ErrNoRows) && actual != nil) || (!errors.Is(err, pgx.ErrNoRows) && actual != err) {
			t.Fatalf("absence: %v %v", got, actual)
		}
	}
	s := &Service{q: &summaryOrdinaryStore{}}
	if got, err := s.PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New()); got != nil || err != nil {
		t.Fatal("optional reader required")
	}
	q := &summaryTestStore{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (&Service{q: q}).PlanCrossCheckSummary(ctx, uuid.New(), uuid.New()); !errors.Is(err, context.Canceled) || q.calls != 0 {
		t.Fatal("cancelled read queried")
	}
}

// The writer accepts absent/null item arrays; the owner DTO preserves that wire
// shape, so consumers must not assume a present findings object has an array.
func TestPlanCrossCheckSummaryNullableItems(t *testing.T) {
	for _, raw := range []string{`{"summary":"ok","items":null}`, `{"summary":"ok"}`} {
		q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{
			Round: 1, Verdict: "approve", ReasonClass: pgtype.Text{String: "approve", Valid: true},
			Findings: []byte(raw),
		}}
		got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
		if err != nil || got == nil || got.Findings == nil || got.Findings.Items != nil {
			t.Fatalf("nullable items: %+v %v", got, err)
		}
		emitted, err := json.Marshal(got)
		if err != nil || !strings.Contains(string(emitted), `"findings":{"summary":"ok","items":null}`) {
			t.Fatalf("nullable wire shape: %s %v", emitted, err)
		}
	}
}

func TestPlanCrossCheckSummaryUsage(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		cost         pgtype.Numeric
		child, usage bool
		want         string
		dollars      float64
	}{
		{"metered", "metered", pgtype.Numeric{Int: big.NewInt(125), Exp: -2, Valid: true}, true, true, "metered", 1.25},
		{"zero", "metered", pgtype.Numeric{Int: big.NewInt(0), Valid: true}, true, true, "metered", 0},
		{"subscription", "subscription", pgtype.Numeric{}, true, true, "subscription", 0},
		{"unreported", "unreported", pgtype.Numeric{}, true, true, "unreported", 0},
		{"null", "metered", pgtype.Numeric{}, true, true, "unreported", 0},
		{"nan", "metered", pgtype.Numeric{NaN: true, Valid: true}, true, true, "unreported", 0},
		{"infinite", "metered", pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}, true, true, "unreported", 0},
		{"negative", "metered", pgtype.Numeric{Int: big.NewInt(-1), Valid: true}, true, true, "unreported", 0},
		{"noUsage", "metered", pgtype.Numeric{}, true, false, "", 0},
		{"deleted", "metered", pgtype.Numeric{}, false, true, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{Verdict: "pending", HasChild: tc.child, HasUsage: tc.usage, CheckerRunID: pgtype.UUID{Bytes: id, Valid: true}, CostStatus: pgtype.Text{String: tc.status, Valid: true}, CostUsd: tc.cost, InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13, OutputTokens: 14}}
			got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			if (got.CheckerRunID != nil) != tc.child {
				t.Fatal("child link")
			}
			if tc.want == "" {
				if got.Usage != nil {
					t.Fatal("fabricated usage")
				}
				return
			}
			if got.Usage == nil || got.Usage.CostStatus != tc.want || got.Usage.CostUSD != tc.dollars || got.Usage.InputTokens != 11 || got.Usage.CacheReadTokens != 12 || got.Usage.CacheCreationTokens != 13 || got.Usage.OutputTokens != 14 {
				t.Fatalf("usage: %+v", got.Usage)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPlanCrossCheckSummaryCanonicalBoundary(t *testing.T) {
	findings := apitypes.PlanCrossCheckFindingsDTO{Items: []apitypes.PlanCrossCheckFindingDTO{}}
	for i := 0; i < 16; i++ {
		findings.Items = append(findings.Items, apitypes.PlanCrossCheckFindingDTO{File: "a.go", Severity: "warning", Summary: strings.Repeat("s", 1000), Rationale: strings.Repeat("r", 900)})
	}
	base, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	padding := 32768 - len(base)
	if padding < 0 || padding >= 4096 {
		t.Fatal("boundary fixture no longer fits per-field caps")
	}
	q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{Verdict: "approve", ReasonClass: pgtype.Text{String: "approve", Valid: true}}}
	for _, over := range []int{0, 1} {
		findings.Summary = strings.Repeat("x", padding+over)
		raw, err := json.Marshal(findings)
		if err != nil || len(raw) != 32768+over {
			t.Fatal("incorrect boundary fixture")
		}
		q.row.Findings = raw
		got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
		if err != nil || (got.Findings != nil) != (over == 0) {
			t.Fatalf("canonical boundary +%d: %+v %v", over, got, err)
		}
	}
}

func TestPlanCrossCheckSummaryFieldBounds(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		count, rationaleBytes int
		want                  bool
	}{
		{"twentyItems", 20, 1, true},
		{"twentyOneItems", 21, 1, false},
		{"twoKiBItem", 1, 2038, true},
		{"oversizeItem", 1, 2039, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := apitypes.PlanCrossCheckFindingsDTO{Summary: "ok", Items: []apitypes.PlanCrossCheckFindingDTO{}}
			for i := 0; i < tc.count; i++ {
				findings.Items = append(findings.Items, apitypes.PlanCrossCheckFindingDTO{File: "a", Severity: "warning", Summary: "ok", Rationale: strings.Repeat("r", tc.rationaleBytes)})
			}
			raw, err := json.Marshal(findings)
			if err != nil {
				t.Fatal(err)
			}
			q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{Verdict: "approve", ReasonClass: pgtype.Text{String: "approve", Valid: true}, Findings: raw}}
			got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
			if err != nil || got == nil || (got.Findings != nil) != tc.want {
				t.Fatalf("field bound: %+v %v", got, err)
			}
			if tc.want && got.Findings.Items[0].Rationale != strings.Repeat("r", tc.rationaleBytes) {
				t.Fatal("valid rationale changed")
			}
		})
	}
}

func TestPlanCrossCheckSummaryHostileFields(t *testing.T) {
	token := "glpat-" + strings.Repeat("b", 20)
	hostile := "**kept**\n" + token[:12] + "\u202e" + token[12:] + "\u001b[31m"
	findings := apitypes.PlanCrossCheckFindingsDTO{
		Summary: hostile,
		Items:   []apitypes.PlanCrossCheckFindingDTO{{File: "src/" + token + ".go", Severity: "warning", Summary: hostile, Rationale: hostile}},
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{
		Verdict: "revise", ReasonClass: pgtype.Text{String: "revise", Valid: true}, Findings: raw,
		CheckerModel:  pgtype.Text{String: strings.Repeat("m", 500) + token, Valid: true},
		CheckerEffort: pgtype.Text{String: hostile, Valid: true},
	}}
	got, err := (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
	if err != nil || got == nil || got.Findings == nil {
		t.Fatalf("hostile findings: %+v %v", got, err)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// Match a prefix too: truncating before scrubbing would leak part of the token.
	for _, unsafe := range []string{token, token[:12], "u001b", "u202e"} {
		if strings.Contains(string(out), unsafe) {
			t.Fatalf("unsafe field %q", unsafe)
		}
	}
	for _, text := range []string{got.Findings.Summary, got.Findings.Items[0].Summary, got.Findings.Items[0].Rationale, *got.CheckerEffort} {
		if !strings.Contains(text, "**kept**") || !strings.Contains(text, "[redacted]") {
			t.Fatalf("scrubbed prose lost: %q", text)
		}
	}
	q.row.CheckerModel = pgtype.Text{String: strings.Repeat("m", 511) + "😀", Valid: true}
	got, err = (&Service{q: q}).PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
	if err != nil || *got.CheckerModel != strings.Repeat("m", 511) || !utf8.ValidString(*got.CheckerModel) {
		t.Fatal("label cap split a UTF-8 rune")
	}
}

func TestPlanCrossCheckSummaryReadBounds(t *testing.T) {
	token := "glpat-" + strings.Repeat("a", 20)
	raw, _ := json.Marshal(map[string]any{"summary": "**hostile**\n" + token + "\u001b[31m", "items": []any{map[string]string{"file": "src/a.go", "severity": "warning", "summary": token, "rationale": "why"}}})
	q := &summaryTestStore{row: store.GetLatestPlanCrossCheckSummaryRow{Verdict: "revise", ReasonClass: pgtype.Text{String: "revise", Valid: true}, Findings: raw, CheckerModel: pgtype.Text{String: strings.Repeat("m", 500) + token, Valid: true}}}
	s := &Service{q: q}
	got, err := s.PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
	if err != nil || got.Findings == nil {
		t.Fatalf("valid findings: %v %v", got, err)
	}
	out, _ := json.Marshal(got)
	if strings.Contains(string(out), token) || strings.Contains(string(out), "u001b") || len(*got.CheckerModel) > 512 || !strings.Contains(got.Findings.Summary, "**hostile**") {
		t.Fatalf("unsafe result: %s", out)
	}
	for _, raw := range []string{
		`{"summary":"ok","items":[],"candidate":"private"}`,
		`{"summary":"ok","items":[{"file":"a","severity":"fatal","summary":"x","rationale":"y"}]}`,
		`{"summary":"` + strings.Repeat("x", 4097) + `","items":[]}`,
		`{"summary":"ok","items":[]} {}`,
		strings.Repeat(" ", 65537),
	} {
		q.row.Findings = []byte(raw)
		got, err := s.PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
		if err != nil || got.Findings != nil || got.Verdict != "revise" || *got.ReasonClass != "revise" {
			t.Fatal("invalid findings changed persisted outcome")
		}
	}
	for _, n := range []int{512, 513} {
		q.row.CheckerEffort = pgtype.Text{String: strings.Repeat("é", n/2) + strings.Repeat("e", n%2), Valid: true}
		got, _ := s.PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
		if len(*got.CheckerEffort) != 512 {
			t.Fatal("label byte bound")
		}
	}
	// JSONB-style whitespace is larger than the 32 KiB canonical writer limit.
	q.row.Findings = []byte("{" + strings.Repeat(" ", 33000) + `"summary":"ok","items":[]}`)
	got, _ = s.PlanCrossCheckSummary(t.Context(), uuid.New(), uuid.New())
	if got.Findings == nil || got.Findings.Summary != "ok" {
		t.Fatal("JSONB whitespace rejected")
	}
}
