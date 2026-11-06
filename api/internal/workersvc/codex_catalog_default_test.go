package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexCatalogContextFixture(t *testing.T) {
	user := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	secret := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	// Expected values were computed outside the code under test (python3 hashlib.sha256 over the
	// NUL-joined parts of PRD #3251 section 1).
	for _, c := range []struct {
		name, mode, accountKey string
		rev                    int64
		want                   string
	}{
		{"api key", codexAuthModeAPIKey, "", 4, "1e0fe98c370dc70b1385cf93f2dd1736296b6a36fca9327d35cada66ce486692"},
		{"subscription", codexAuthModeSubscription, `["user-1","acct-1"]`, 7, "7d7d5e11349f8cd5230cdd1c6a4b959bc41f36a568ad9e518988fb4ed330f6e9"},
	} {
		if got := codexCatalogContext(user, secret, c.mode, c.rev, c.accountKey); got != c.want {
			t.Errorf("%s: context = %s, want %s", c.name, got, c.want)
		}
	}
	// Every identity part moves the hash; nothing else is an input.
	base := codexCatalogContext(user, secret, codexAuthModeAPIKey, 4, "")
	for name, got := range map[string]string{
		"user":     codexCatalogContext(uuid.New(), secret, codexAuthModeAPIKey, 4, ""),
		"secret":   codexCatalogContext(user, uuid.New(), codexAuthModeAPIKey, 4, ""),
		"mode":     codexCatalogContext(user, secret, codexAuthModeSubscription, 4, ""),
		"revision": codexCatalogContext(user, secret, codexAuthModeAPIKey, 5, ""),
		"account":  codexCatalogContext(user, secret, codexAuthModeAPIKey, 4, "x"),
	} {
		if got == base {
			t.Errorf("changing %s did not change the context", name)
		}
	}
}

func TestClaimCodexCatalogContextScope(t *testing.T) {
	secret := uuid.New()
	codexRun := func(mod func(*store.Run)) store.Run {
		r := store.Run{
			ID: uuid.New(), UserID: uuid.New(), Kind: "issue", Harness: harnessCodex,
			CodexSecretID: pgtype.UUID{Bytes: secret, Valid: true}, CodexAuthMode: pgtype.Text{String: codexAuthModeAPIKey, Valid: true},
			CodexMaterialRevision: pgtype.Int8{Int64: 3, Valid: true},
		}
		if mod != nil {
			mod(&r)
		}
		return r
	}
	if got := claimCodexCatalogContext(codexRun(nil)); got == nil || *got == "" {
		t.Fatal("complete api_key Codex issue run: no context")
	}
	for name, run := range map[string]store.Run{
		"claude":         codexRun(func(r *store.Run) { r.Harness = "claude" }),
		"cross_check":    codexRun(func(r *store.Run) { r.Kind = "cross_check" }),
		"judge":          codexRun(func(r *store.Run) { r.Kind = "judge" }),
		"review target":  codexRun(func(r *store.Run) { r.ReviewTargetRunID = pgtype.UUID{Bytes: uuid.New(), Valid: true} }),
		"egress profile": codexRun(func(r *store.Run) { r.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true} }),
		"no secret":      codexRun(func(r *store.Run) { r.CodexSecretID = pgtype.UUID{} }),
		"no revision":    codexRun(func(r *store.Run) { r.CodexMaterialRevision = pgtype.Int8{} }),
		"unlinked sub": codexRun(func(r *store.Run) {
			r.CodexAuthMode = pgtype.Text{String: codexAuthModeSubscription, Valid: true}
		}),
		"unknown mode": codexRun(func(r *store.Run) { r.CodexAuthMode = pgtype.Text{String: "other", Valid: true} }),
	} {
		if got := claimCodexCatalogContext(run); got != nil {
			t.Errorf("%s: shipped context %s, want none", name, *got)
		}
	}
	linked := codexRun(func(r *store.Run) {
		r.CodexAuthMode = pgtype.Text{String: codexAuthModeSubscription, Valid: true}
		r.CodexAccountKey = pgtype.Text{String: `["u","a"]`, Valid: true}
	})
	if got := claimCodexCatalogContext(linked); got == nil {
		t.Error("linked subscription run: no context")
	}
}

func TestValidateCodexCatalogRequest(t *testing.T) {
	gen := int64(1)
	ok := CodexCatalogDefaultRequest{
		ClaimGeneration: &gen, CatalogContext: strings.Repeat("a", 64), Model: "gpt-6.1-sol", RuntimeVersion: "0.164.0", DefaultEffort: "low",
	}
	if err := validateCodexCatalogRequest(ok); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	zero := int64(0)
	for name, c := range map[string]struct {
		mod  func(*CodexCatalogDefaultRequest)
		want error
	}{
		"no generation":     {func(r *CodexCatalogDefaultRequest) { r.ClaimGeneration = nil }, ErrMissingClaimGeneration},
		"zero generation":   {func(r *CodexCatalogDefaultRequest) { r.ClaimGeneration = &zero }, ErrMissingClaimGeneration},
		"short context":     {func(r *CodexCatalogDefaultRequest) { r.CatalogContext = strings.Repeat("a", 63) }, ErrCodexCatalogInvalid},
		"upper context":     {func(r *CodexCatalogDefaultRequest) { r.CatalogContext = strings.Repeat("A", 64) }, ErrCodexCatalogInvalid},
		"non-hex context":   {func(r *CodexCatalogDefaultRequest) { r.CatalogContext = strings.Repeat("g", 64) }, ErrCodexCatalogInvalid},
		"empty model":       {func(r *CodexCatalogDefaultRequest) { r.Model = "" }, ErrCodexCatalogInvalid},
		"long model":        {func(r *CodexCatalogDefaultRequest) { r.Model = strings.Repeat("m", 101) }, ErrCodexCatalogInvalid},
		"nul model":         {func(r *CodexCatalogDefaultRequest) { r.Model = "a\x00b" }, ErrCodexCatalogInvalid},
		"bad utf8 model":    {func(r *CodexCatalogDefaultRequest) { r.Model = "a\xffb" }, ErrCodexCatalogInvalid},
		"empty version":     {func(r *CodexCatalogDefaultRequest) { r.RuntimeVersion = "" }, ErrCodexCatalogInvalid},
		"long version":      {func(r *CodexCatalogDefaultRequest) { r.RuntimeVersion = "1.0.0-" + strings.Repeat("a", 27) }, ErrCodexCatalogInvalid},
		"non-semver":        {func(r *CodexCatalogDefaultRequest) { r.RuntimeVersion = "latest" }, ErrCodexCatalogInvalid},
		"v-prefixed":        {func(r *CodexCatalogDefaultRequest) { r.RuntimeVersion = "v0.164.0" }, ErrCodexCatalogInvalid},
		"persistent effort": {func(r *CodexCatalogDefaultRequest) { r.DefaultEffort = "persistent" }, ErrCodexCatalogInvalid},
		"empty effort":      {func(r *CodexCatalogDefaultRequest) { r.DefaultEffort = "" }, ErrCodexCatalogInvalid},
		"upper effort":      {func(r *CodexCatalogDefaultRequest) { r.DefaultEffort = "Low" }, ErrCodexCatalogInvalid},
	} {
		r := ok
		c.mod(&r)
		if err := validateCodexCatalogRequest(r); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// A worker-supplied value is never quoted in the error text.
	r := ok
	r.Model = strings.Repeat("secret-model-", 10)
	if err := validateCodexCatalogRequest(r); err == nil || strings.Contains(err.Error(), "secret-model") {
		t.Errorf("error %v must not carry the worker-supplied value", err)
	}
	for _, e := range codexCatalogEfforts {
		r := ok
		r.DefaultEffort = e
		if err := validateCodexCatalogRequest(r); err != nil {
			t.Errorf("effort %q refused: %v", e, err)
		}
	}
}

func catalogRow(version, effort string, reportedAt time.Time) store.ListFreshCodexCatalogDefaultsRow {
	return store.ListFreshCodexCatalogDefaultsRow{RuntimeVersion: version, DefaultEffort: effort, ReportedAt: pgtype.Timestamptz{Time: reportedAt, Valid: true}}
}

func TestSelectCatalogDefault(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	hour := time.Hour
	type want struct {
		state, effort, version string
	}
	for _, c := range []struct {
		name string
		rows []store.ListFreshCodexCatalogDefaultsRow
		want want
	}{
		{"none", nil, want{CatalogStateUnknown, "", ""}},
		{"one", []store.ListFreshCodexCatalogDefaultsRow{catalogRow("0.162.1", "low", ago(hour))}, want{CatalogStateKnown, "low", "0.162.1"}},
		{"higher version wins though older reported later", []store.ListFreshCodexCatalogDefaultsRow{
			catalogRow("0.163.0", "medium", ago(5*hour)), catalogRow("0.162.1", "medium", ago(hour)),
		}, want{CatalogStateKnown, "medium", "0.163.0"}},
		{"semver not lexical", []store.ListFreshCodexCatalogDefaultsRow{
			catalogRow("0.9.0", "low", ago(hour)), catalogRow("0.10.0", "high", ago(5*hour)),
		}, want{CatalogStateVaries, "high", "0.10.0"}},
		{"tie on version takes the latest report", []store.ListFreshCodexCatalogDefaultsRow{
			catalogRow("0.164.0", "low", ago(5*hour)), catalogRow("0.164.0", "high", ago(hour)),
		}, want{CatalogStateKnown, "high", "0.164.0"}},
		{"varies when another version differs", []store.ListFreshCodexCatalogDefaultsRow{
			catalogRow("0.162.1", "low", ago(hour)), catalogRow("0.163.0", "medium", ago(2*hour)),
		}, want{CatalogStateVaries, "medium", "0.163.0"}},
		{"31 day old row ignored", []store.ListFreshCodexCatalogDefaultsRow{catalogRow("0.162.1", "low", ago(31*24*hour))}, want{CatalogStateUnknown, "", ""}},
		{"old differing row does not make it vary", []store.ListFreshCodexCatalogDefaultsRow{
			catalogRow("0.162.1", "low", ago(31*24*hour)), catalogRow("0.163.0", "medium", ago(hour)),
		}, want{CatalogStateKnown, "medium", "0.163.0"}},
		{"none effort is a known value", []store.ListFreshCodexCatalogDefaultsRow{catalogRow("0.164.0", "none", ago(hour))}, want{CatalogStateKnown, "none", "0.164.0"}},
		{"unparseable version ignored", []store.ListFreshCodexCatalogDefaultsRow{catalogRow("latest", "low", ago(hour))}, want{CatalogStateUnknown, "", ""}},
	} {
		got := selectCatalogDefault(c.rows, now)
		if got.State != c.want.state || got.Effort != c.want.effort || got.RuntimeVersion != c.want.version {
			t.Errorf("%s: got {%s %q %q}, want %+v", c.name, got.State, got.Effort, got.RuntimeVersion, c.want)
		}
		if got.State != CatalogStateUnknown && got.ObservedAt.IsZero() {
			t.Errorf("%s: known default has no ObservedAt", c.name)
		}
	}
}

func TestCodexWorkerDefaultModel(t *testing.T) {
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	for _, c := range []struct {
		name      string
		lane, pin pgtype.Text
		want      string
	}{
		{"lane wins over pin", txt("gpt-6-sol"), txt("gpt-6-astra"), "gpt-6-sol"},
		{"lane custom id passes through", txt("gpt-custom-1"), txt("gpt-6-astra"), "gpt-custom-1"},
		{"provider default", pgtype.Text{}, pgtype.Text{}, "gpt-6.1-sol"},
		{"empty lane is unset", txt(""), pgtype.Text{}, "gpt-6.1-sol"},
		{"lead role-model pin", pgtype.Text{}, txt("gpt-6-luna"), "gpt-6-luna"},
		{"pin outside the role models is ignored", pgtype.Text{}, txt("gpt-custom-1"), "gpt-6.1-sol"},
		{"claude alias pin is ignored", pgtype.Text{}, txt("opus"), "gpt-6.1-sol"},
	} {
		if got := CodexWorkerDefaultModel(c.lane, c.pin); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
