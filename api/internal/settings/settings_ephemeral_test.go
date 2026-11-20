package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersize"
)

// PRD #4212: the ephemeral/hosted worker settings.

var ephemeralKeys = []string{KeyEphemeralMaxPerUser, KeyEphemeralDefaultSize, KeyHostedDefaultSize, KeyEphemeralLease}

func TestEphemeralKeysKnownAndDefaulted(t *testing.T) {
	want := map[string]string{
		KeyEphemeralMaxPerUser:  "2",
		KeyEphemeralDefaultSize: "m",
		KeyHostedDefaultSize:    "l",
		KeyEphemeralLease:       "2h",
	}
	for k, def := range want {
		if !Known(k) {
			t.Errorf("Known(%s) = false", k)
		}
		if Defaults[k] != def {
			t.Errorf("Defaults[%s] = %q, want %q", k, Defaults[k], def)
		}
		if IsSecret(k) {
			t.Errorf("%s must not be a secret", k)
		}
		// Every default must itself pass its own write gate.
		if err := Validate(k, def); err != nil {
			t.Errorf("Validate(%s, default %q) = %v", k, def, err)
		}
	}
}

func TestValidateEphemeralMaxPerUser(t *testing.T) {
	for _, v := range []string{"0", "1", "20", " 7 ", "", "  "} {
		if err := Validate(KeyEphemeralMaxPerUser, v); err != nil {
			t.Errorf("Validate(ephemeral_max_per_user, %q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"-1", "21", "100", "abc", "1.5", "1e1", "two"} {
		if err := Validate(KeyEphemeralMaxPerUser, v); err == nil {
			t.Errorf("Validate(ephemeral_max_per_user, %q) = nil, want a rejection", v)
		}
	}
}

func TestValidateWorkerSizeKeys(t *testing.T) {
	for _, key := range []string{KeyEphemeralDefaultSize, KeyHostedDefaultSize} {
		for _, n := range workersize.Names {
			if err := Validate(key, n); err != nil {
				t.Errorf("Validate(%s, %q) = %v, want nil", key, n, err)
			}
		}
		for _, v := range []string{"", "  ", " m "} {
			if err := Validate(key, v); err != nil {
				t.Errorf("Validate(%s, %q) = %v, want nil", key, v, err)
			}
		}
		for _, v := range []string{"M", "L-HIGHMEM", "1s", "has space", "l_highmem", "a-name-that-is-too-long", "-xl", "9xl"} {
			if err := Validate(key, v); err == nil {
				t.Errorf("Validate(%s, %q) = nil, want a rejection", key, v)
			}
		}
	}
}

// An admin-added size such as "xs" is a well-formed name the settings layer must carry as-is
// (PRD #4213). Whether it still names a row is a database fact checked by a later milestone.
func TestWellFormedStoredSizeIsReturnedAsIs(t *testing.T) {
	ctx := context.Background()
	c := New(&fakeStore{rows: []store.AppSetting{
		row(KeyEphemeralDefaultSize, "xs"),
		row(KeyHostedDefaultSize, "xs"),
	}}, time.Minute)
	if s, err := c.EphemeralDefaultSize(ctx); s != "xs" || err != nil {
		t.Errorf("EphemeralDefaultSize = %q, %v; want xs, nil", s, err)
	}
	if s, err := c.HostedDefaultSize(ctx); s != "xs" || err != nil {
		t.Errorf("HostedDefaultSize = %q, %v; want xs, nil", s, err)
	}
	bad := New(&fakeStore{rows: []store.AppSetting{row(KeyEphemeralDefaultSize, "XS ?")}}, time.Minute)
	if s, _ := bad.EphemeralDefaultSize(ctx); s != "m" {
		t.Errorf("a malformed stored size = %q, want the default m", s)
	}
}

// A well-formed name that is not a shipped builtin ("xs", an admin-added size) passes the
// format gate: membership is checked in the admin PUT's write transaction (PRD #4213 M3).
func TestValidateWorkerSizeAcceptsWellFormedNonBuiltin(t *testing.T) {
	for _, key := range []string{KeyEphemeralDefaultSize, KeyHostedDefaultSize} {
		for _, v := range []string{"xs", "xl", "tiny", " xs "} {
			if err := Validate(key, v); err != nil {
				t.Errorf("Validate(%s, %q) = %v, want nil", key, v, err)
			}
		}
	}
}

func TestValidateEphemeralLease(t *testing.T) {
	for _, v := range []string{"0", "0s", "30m", "2h", "1h59m59s", "", "  ", " 1h "} {
		if err := Validate(KeyEphemeralLease, v); err != nil {
			t.Errorf("Validate(ephemeral_lease, %q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"2h1s", "3h", "-1m", "abc", "30", "1d"} {
		if err := Validate(KeyEphemeralLease, v); err == nil {
			t.Errorf("Validate(ephemeral_lease, %q) = nil, want a rejection", v)
		}
	}
}

// Validate's default branch is the label rule, which accepts "PRD"; an ephemeral
// key missing its explicit case would therefore accept it.
func TestEphemeralKeysDoNotFallThroughToLabelRules(t *testing.T) {
	for _, k := range ephemeralKeys {
		if err := Validate(k, "PRD"); err == nil {
			t.Errorf("Validate(%s, \"PRD\") = nil: the key fell through to ValidateLabel", k)
		}
	}
}

func TestNormalizeForStorageTrimsEphemeralKeys(t *testing.T) {
	for _, k := range ephemeralKeys {
		if got := NormalizeForStorage(k, "  \t "); got != "" {
			t.Errorf("NormalizeForStorage(%s, blank) = %q, want \"\"", k, got)
		}
		if got := NormalizeForStorage(k, " 5 "); got != "5" {
			t.Errorf("NormalizeForStorage(%s, \" 5 \") = %q, want 5", k, got)
		}
	}
	// A non-listed key stays verbatim.
	if got := NormalizeForStorage(KeyHostedWorkerQuota, " 5 "); got != " 5 " {
		t.Errorf("NormalizeForStorage(hosted_worker_quota) = %q, want verbatim", got)
	}
	// The instance model keys keep their trimming.
	if got := NormalizeForStorage(KeyJudgeModel, " opus "); got != "opus" {
		t.Errorf("NormalizeForStorage(judge_model) = %q, want opus", got)
	}
}

func TestEphemeralMaxPerUserAccessor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		rows []store.AppSetting
		want int
	}{
		{"default", nil, 2},
		{"stored", []store.AppSetting{row(KeyEphemeralMaxPerUser, "7")}, 7},
		{"zero is a real value", []store.AppSetting{row(KeyEphemeralMaxPerUser, "0")}, 0},
		{"junk", []store.AppSetting{row(KeyEphemeralMaxPerUser, "banana")}, 2},
		{"negative", []store.AppSetting{row(KeyEphemeralMaxPerUser, "-3")}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(&fakeStore{rows: tc.rows}, time.Minute)
			got, err := c.EphemeralMaxPerUser(ctx)
			if err != nil || got != tc.want {
				t.Fatalf("EphemeralMaxPerUser = %d, %v; want %d, nil", got, err, tc.want)
			}
		})
	}
}

func TestWorkerSizeAccessors(t *testing.T) {
	ctx := context.Background()
	type acc func(*Cache, context.Context) (string, error)
	for _, tc := range []struct {
		key string
		fn  acc
		def string
	}{
		{KeyEphemeralDefaultSize, (*Cache).EphemeralDefaultSize, "m"},
		{KeyHostedDefaultSize, (*Cache).HostedDefaultSize, "l"},
	} {
		for _, rc := range []struct {
			name, stored, want string
		}{
			{"default", "", tc.def},
			{"stored", "s", "s"},
			{"highmem", "l-highmem", "l-highmem"},
			{"admin-added size is kept", "xl", "xl"},
			{"junk", "not a size", tc.def},
			{"wrong case", "S", tc.def},
		} {
			t.Run(tc.key+"/"+rc.name, func(t *testing.T) {
				var rows []store.AppSetting
				if rc.stored != "" {
					rows = append(rows, row(tc.key, rc.stored))
				}
				c := New(&fakeStore{rows: rows}, time.Minute)
				got, err := tc.fn(c, ctx)
				if err != nil || got != rc.want {
					t.Fatalf("got %q, %v; want %q, nil", got, err, rc.want)
				}
			})
		}
	}
}

func TestEphemeralLeaseAccessor(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		stored string
		want   time.Duration
	}{
		{"default", "", 2 * time.Hour},
		{"stored", "30m", 30 * time.Minute},
		{"zero disables", "0s", 0},
		{"bare zero", "0", 0},
		{"at max", "2h", 2 * time.Hour},
		{"junk", "banana", 2 * time.Hour},
		{"negative", "-5m", 2 * time.Hour},
		{"over max", "3h", 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows []store.AppSetting
			if tc.stored != "" {
				rows = append(rows, row(KeyEphemeralLease, tc.stored))
			}
			c := New(&fakeStore{rows: rows}, time.Minute)
			got, err := c.EphemeralLease(ctx)
			if err != nil || got != tc.want {
				t.Fatalf("EphemeralLease = %v, %v; want %v, nil", got, err, tc.want)
			}
		})
	}
}

// The overlay wins over a stored row, reports source "env", never errors even with
// a failing store, and shows in AdminView.
func TestEphemeralEnvOverlay(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("db down")
	c := New(&fakeStore{err: boom}, time.Minute)
	c.ConfigureSecrets(nil, map[string]string{
		KeyEphemeralMaxPerUser:  "25",
		KeyEphemeralDefaultSize: "s",
		KeyEphemeralLease:       "30m",
	})
	if n, err := c.EphemeralMaxPerUser(ctx); n != 25 || err != nil {
		t.Errorf("EphemeralMaxPerUser = %d, %v; want 25, nil", n, err)
	}
	if s, err := c.EphemeralDefaultSize(ctx); s != "s" || err != nil {
		t.Errorf("EphemeralDefaultSize = %q, %v; want s, nil", s, err)
	}
	if d, err := c.EphemeralLease(ctx); d != 30*time.Minute || err != nil {
		t.Errorf("EphemeralLease = %v, %v; want 30m, nil", d, err)
	}

	// With a healthy store holding conflicting rows, env still wins and AdminView
	// marks exactly the overlaid keys as env.
	c = New(&fakeStore{rows: []store.AppSetting{
		row(KeyEphemeralMaxPerUser, "3"),
		row(KeyHostedDefaultSize, "m"),
	}}, time.Minute)
	c.ConfigureSecrets(nil, map[string]string{KeyEphemeralMaxPerUser: "25"})
	av, err := c.AdminView(ctx)
	if err != nil {
		t.Fatalf("AdminView: %v", err)
	}
	want := map[string][2]string{
		KeyEphemeralMaxPerUser:  {"25", "env"},
		KeyEphemeralDefaultSize: {"m", "default"},
		KeyHostedDefaultSize:    {"m", "db"},
		KeyEphemeralLease:       {"2h", "default"},
	}
	for k, w := range want {
		if av.Values[k] != w[0] || av.Sources[k] != w[1] {
			t.Errorf("AdminView %s = %q/%q, want %q/%q", k, av.Values[k], av.Sources[k], w[0], w[1])
		}
	}
	if !c.IsEnvSourced(KeyEphemeralMaxPerUser) || c.IsEnvSourced(KeyHostedDefaultSize) {
		t.Error("IsEnvSourced wrong for the ephemeral keys")
	}
}

// A cold cache whose refresh fails surfaces the error from every accessor, with the
// default as the value.
func TestEphemeralAccessorsPropagateColdCacheError(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("db down")
	c := New(&fakeStore{err: boom}, time.Minute)
	if n, err := c.EphemeralMaxPerUser(ctx); !errors.Is(err, boom) || n != 2 {
		t.Errorf("EphemeralMaxPerUser = %d, %v; want 2, %v", n, err, boom)
	}
	if s, err := c.EphemeralDefaultSize(ctx); !errors.Is(err, boom) || s != "m" {
		t.Errorf("EphemeralDefaultSize = %q, %v; want m, %v", s, err, boom)
	}
	if s, err := c.HostedDefaultSize(ctx); !errors.Is(err, boom) || s != "l" {
		t.Errorf("HostedDefaultSize = %q, %v; want l, %v", s, err, boom)
	}
	if d, err := c.EphemeralLease(ctx); !errors.Is(err, boom) || d != 2*time.Hour {
		t.Errorf("EphemeralLease = %v, %v; want 2h, %v", d, err, boom)
	}
}

func TestFormatEphemeralLease(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Minute, "30m"},
		{2 * time.Hour, "2h"},
		{time.Hour, "1h"},
		{90 * time.Minute, "1h30m"},
		{90 * time.Second, "1m30s"},
		{45 * time.Second, "45s"},
		{time.Hour + 30*time.Second, "1h0m30s"},
	} {
		got := FormatEphemeralLease(tc.d)
		if got != tc.want {
			t.Errorf("FormatEphemeralLease(%v) = %q, want %q", tc.d, got, tc.want)
		}
		back, err := time.ParseDuration(got)
		if err != nil || back != tc.d {
			t.Errorf("round trip of %q = %v, %v; want %v", got, back, err, tc.d)
		}
	}
}
