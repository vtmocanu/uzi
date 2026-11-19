package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func allowOnly(entry string) ProviderAllowFunc {
	return func(c string) (string, bool) { return entry, c == entry }
}

func usableRow(pid uuid.UUID) store.GetUserClaudeProviderSourceRow {
	return store.GetUserClaudeProviderSourceRow{
		ProviderID:       pgtype.UUID{Bytes: pid, Valid: true},
		Engine:           pgtype.Text{String: "claude", Valid: true},
		CurrentRevision:  pgtype.Int4{Int32: 3, Valid: true},
		BaseUrl:          pgtype.Text{String: "https://gw.example.com/a", Valid: true},
		AuthStyle:        pgtype.Text{String: "bearer", Valid: true},
		ModelMap:         []byte(`{"haiku":{"model":"h"}}`),
		KeySecretID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
		KeyKind:          pgtype.Text{String: store.KindAnthropicCompatibleKey, Valid: true},
		KeyProviderID:    pgtype.UUID{Bytes: pid, Valid: true},
		KeyEnablementRev: pgtype.Int8{Int64: 9, Valid: true},
	}
}

func TestEvaluateClaudeProviderSource(t *testing.T) {
	pid := uuid.New()
	allow := allowOnly("https://gw.example.com/a")
	cases := []struct {
		name   string
		mut    func(r *store.GetUserClaudeProviderSourceRow)
		allow  ProviderAllowFunc
		state  ProviderSourceState
		reason string
	}{
		{"no source", func(r *store.GetUserClaudeProviderSourceRow) { *r = store.GetUserClaudeProviderSourceRow{} }, allow, ProviderSourceNone, ""},
		{"usable", func(*store.GetUserClaudeProviderSourceRow) {}, allow, ProviderSourceUsable, ""},
		{"provider row gone", func(r *store.GetUserClaudeProviderSourceRow) { r.CurrentRevision = pgtype.Int4{} }, allow, ProviderSourceUnusable, SourceUnusableProviderMissing},
		{"retired", func(r *store.GetUserClaudeProviderSourceRow) {
			r.RetiredAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}, allow, ProviderSourceUnusable, SourceUnusableRetired},
		{"url not allowlisted", func(*store.GetUserClaudeProviderSourceRow) {}, allowOnly("https://other.example.com/"), ProviderSourceUnusable, SourceUnusableBaseURL},
		{"no allowlist", func(*store.GetUserClaudeProviderSourceRow) {}, nil, ProviderSourceUnusable, SourceUnusableBaseURL},
		{"no key", func(r *store.GetUserClaudeProviderSourceRow) { r.KeySecretID = pgtype.UUID{} }, allow, ProviderSourceUnusable, SourceUnusableKeyMissing},
		{"key of another kind", func(r *store.GetUserClaudeProviderSourceRow) {
			r.KeyKind = pgtype.Text{String: store.KindAnthropicToken, Valid: true}
		}, allow, ProviderSourceUnusable, SourceUnusableKeyMissing},
		{"key of another provider", func(r *store.GetUserClaudeProviderSourceRow) {
			r.KeyProviderID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		}, allow, ProviderSourceUnusable, SourceUnusableKeyMissing},
		{"key disabled", func(r *store.GetUserClaudeProviderSourceRow) {
			r.KeyDisabledAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}, allow, ProviderSourceUnusable, SourceUnusableKeyDisabled},
		{"model map unreadable", func(r *store.GetUserClaudeProviderSourceRow) { r.ModelMap = []byte(`{`) }, allow, ProviderSourceUnusable, SourceUnusableInvalid},
		{"model map without haiku", func(r *store.GetUserClaudeProviderSourceRow) { r.ModelMap = []byte(`{"opus":{"model":"o"}}`) }, allow, ProviderSourceUnusable, SourceUnusableInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := usableRow(pid)
			c.mut(&row)
			got := EvaluateClaudeProviderSource(row, c.allow)
			if got.State != c.state || got.Reason != c.reason {
				t.Fatalf("state=%d reason=%q, want %d %q", got.State, got.Reason, c.state, c.reason)
			}
			if c.state == ProviderSourceUsable {
				s := got.Source
				if s.ProviderID != pid || s.Revision != 3 || s.BaseURL != "https://gw.example.com/a" || s.KeyRev != 9 || s.AuthStyle != "bearer" || s.ModelMap["haiku"].Model != "h" {
					t.Fatalf("source = %+v", s)
				}
			}
			if (c.state == ProviderSourceUnusable) != (got.Err() != nil) {
				t.Fatalf("Err() = %v for state %d", got.Err(), got.State)
			}
		})
	}
}

func TestModelMapped(t *testing.T) {
	m := providers.ModelMap{"opus": {Model: "corp-opus"}, "haiku": {Model: "corp-haiku"}}
	cases := map[string]bool{
		"":                false, // the default alias is sonnet, which this map lacks
		"opus":            true,
		"haiku":           true,
		"sonnet":          false,
		"fable":           false,
		"corp-opus":       true,
		"corp-haiku":      true,
		"claude-opus-4-8": false, // a full vendor id is not rewritten
		"Opus":            false,
		" opus ":          true,
	}
	for model, want := range cases {
		if got := modelMapped(m, model); got != want {
			t.Errorf("modelMapped(%q) = %v, want %v", model, got, want)
		}
	}
	if !modelMapped(providers.ModelMap{"sonnet": {Model: "s"}, "haiku": {Model: "h"}}, "") {
		t.Error("an empty model must resolve through the sonnet alias when it is mapped")
	}
}
