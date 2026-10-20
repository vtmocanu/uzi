package codexusagepoller

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func m3ptr[T any](v T) *T { return &v }

func m3Buckets(reset int64, pct *float64) []apitypes.CodexRateLimitBucketDTO {
	return []apitypes.CodexRateLimitBucketDTO{{ID: "codex", Primary: &apitypes.CodexRateLimitWindowDTO{
		LimitWindowSeconds: m3ptr(int64(604800)), ResetAt: &reset, UsedPercent: pct,
	}}}
}

func TestCodexEarlyResetMalformedIdentities(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	reset := now.Add(12 * time.Hour).Unix()
	raw, err := json.Marshal(m3Buckets(reset, m3ptr(20.0)))
	if err != nil {
		t.Fatal(err)
	}
	main := string(raw[1 : len(raw)-1])
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"null bucket beside main", "[null," + main + "]"},
		{"empty bucket beside main", "[{}," + main + "]"},
		{"null ID beside main", "[{\"id\":null}," + main + "]"},
		{"null display name", "[{\"display_name\":null," + main[1:] + "]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := store.GetCodexEarlyResetBaselineRow{
				Buckets:       []byte(tc.raw),
				LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
			}
			if _, detected := codexEarlyReset(prior, m3Buckets(reset, m3ptr(0.0)), now); detected {
				t.Fatal("malformed bucket identity accepted as a comparison basis")
			}
		})
	}
}

func TestCodexEarlyResetMatrix(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	reset := now.Add(48 * time.Hour).Unix()
	type scenario struct {
		name   string
		change func(*store.GetCodexEarlyResetBaselineRow, *[]apitypes.CodexRateLimitBucketDTO, *[]apitypes.CodexRateLimitBucketDTO, *time.Time)
		want   bool
	}
	cases := []scenario{
		{"cleared", nil, true},
		{"forward_without_percent", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Primary.UsedPercent = nil
			(*n)[0].Primary.UsedPercent = nil
			(*n)[0].Primary.ResetAt = m3ptr(reset + 3601)
		}, true},
		{"exact_8h", func(_ *store.GetCodexEarlyResetBaselineRow, _, _ *[]apitypes.CodexRateLimitBucketDTO, n *time.Time) {
			*n = time.Unix(reset, 0).Add(-8 * time.Hour)
		}, false},
		{"just_before_8h", func(_ *store.GetCodexEarlyResetBaselineRow, _, _ *[]apitypes.CodexRateLimitBucketDTO, n *time.Time) {
			*n = time.Unix(reset, 0).Add(-8*time.Hour - time.Nanosecond)
		}, true},
		{"just_after_8h", func(_ *store.GetCodexEarlyResetBaselineRow, _, _ *[]apitypes.CodexRateLimitBucketDTO, n *time.Time) {
			*n = time.Unix(reset, 0).Add(-8*time.Hour + time.Nanosecond)
		}, false},
		{"secondary_swap", func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*n)[0].Secondary = (*n)[0].Primary
			(*n)[0].Primary = nil
		}, true},
		{"secondary_both", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Secondary = (*p)[0].Primary
			(*p)[0].Primary = nil
			(*n)[0].Secondary = (*n)[0].Primary
			(*n)[0].Primary = nil
		}, true},
		{"reverse_swap", func(_ *store.GetCodexEarlyResetBaselineRow, p, _ *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Secondary = (*p)[0].Primary
			(*p)[0].Primary = nil
		}, true},
		{"threshold_percent", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Primary.UsedPercent = m3ptr(5.0)
			(*n)[0].Primary.UsedPercent = m3ptr(1.0)
		}, true},
		{"nearzero_prior", func(_ *store.GetCodexEarlyResetBaselineRow, p, _ *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Primary.UsedPercent = m3ptr(4.999)
		}, false},
		{"new_above_one", func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*n)[0].Primary.UsedPercent = m3ptr(1.001)
		}, false},
		{"nearzero_forward", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Primary.UsedPercent = m3ptr(0.0)
			(*n)[0].Primary.ResetAt = m3ptr(reset + 3601)
		}, true},
		{"scheduled_rollover", func(r *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, at *time.Time) {
			*at = time.Unix(reset, 0)
			r.LastSuccessAt.Time = at.Add(-time.Hour)
			(*n)[0].Primary.ResetAt = m3ptr(reset + 604800)
		}, false},
	}
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		cases = append(cases, scenario{name: "forward_8h/" + delta.String(), change: func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, at *time.Time) {
			(*n)[0].Primary.ResetAt = m3ptr(reset + 3601)
			*at = time.Unix(reset, 0).Add(-8*time.Hour + delta)
		}, want: delta < 0})
	}
	cases = append(cases,
		scenario{"hundred_percent_prior", func(_ *store.GetCodexEarlyResetBaselineRow, p, _ *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Primary.UsedPercent = m3ptr(100.0)
		}, true},
		scenario{"upper_reset_bound", func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*n)[0].Primary.ResetAt = m3ptr(int64(253402300799))
		}, true},
		scenario{"other_buckets_ignored", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			*p = append(*p, apitypes.CodexRateLimitBucketDTO{ID: "aux", DisplayName: "aux"})
			*n = append(*n, apitypes.CodexRateLimitBucketDTO{ID: "aux"})
		}, true},
		scenario{"nonweekly_sibling_ignored", func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*p)[0].Secondary = &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: m3ptr(int64(18000))}
			(*n)[0].Secondary = &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: m3ptr(int64(18000))}
		}, true},
		scenario{"forward_secondary_swap", func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*n)[0].Primary.ResetAt = m3ptr(reset + 3601)
			(*n)[0].Secondary = (*n)[0].Primary
			(*n)[0].Primary = nil
		}, true},
	)
	for _, shift := range []int64{-3601, -1, 3599, 3600, 3601} {
		cases = append(cases, scenario{name: time.Duration(shift * time.Second.Nanoseconds()).String(), change: func(_ *store.GetCodexEarlyResetBaselineRow, _, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
			(*n)[0].Primary.ResetAt = m3ptr(reset + shift)
		}, want: shift > 3600})
	}
	for _, side := range []string{"prior", "next"} {
		mutations := []struct {
			name  string
			apply func(*[]apitypes.CodexRateLimitBucketDTO)
		}{
			{"absent", func(b *[]apitypes.CodexRateLimitBucketDTO) { *b = nil }},
			{"auxiliary", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].DisplayName = "codex" }},
			{"other_id", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].ID = "aux" }},
			{"duplicate", func(b *[]apitypes.CodexRateLimitBucketDTO) { *b = append(*b, (*b)[0]) }},
			{"duplicate_aux", func(b *[]apitypes.CodexRateLimitBucketDTO) {
				*b = append(*b, apitypes.CodexRateLimitBucketDTO{ID: "codex", DisplayName: "codex"})
			}},
			{"five_hour", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.LimitWindowSeconds = m3ptr(int64(18000)) }},
			{"missing_length", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.LimitWindowSeconds = nil }},
			{"ambiguous_invalid_weekly", func(b *[]apitypes.CodexRateLimitBucketDTO) {
				(*b)[0].Secondary = &apitypes.CodexRateLimitWindowDTO{LimitWindowSeconds: m3ptr(int64(604800))}
			}},
			{"reset_after_only", func(b *[]apitypes.CodexRateLimitBucketDTO) {
				(*b)[0].Primary.ResetAt = nil
				(*b)[0].Primary.ResetAfterSeconds = m3ptr(int64(604800))
			}},
			{"zero_reset", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.ResetAt = m3ptr(int64(0)) }},
			{"negative_reset", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.ResetAt = m3ptr(int64(-1)) }},
			{"out_of_range", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.ResetAt = m3ptr(int64(253402300800)) }},
			{"missing_percent", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.UsedPercent = nil }},
			{"negative_percent", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.UsedPercent = m3ptr(-1.0) }},
			{"over_percent", func(b *[]apitypes.CodexRateLimitBucketDTO) { (*b)[0].Primary.UsedPercent = m3ptr(101.0) }},
		}
		for _, m := range mutations {
			cases = append(cases, scenario{name: side + "/" + m.name, change: func(_ *store.GetCodexEarlyResetBaselineRow, p, n *[]apitypes.CodexRateLimitBucketDTO, _ *time.Time) {
				if side == "prior" {
					m.apply(p)
				} else {
					m.apply(n)
				}
			}})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prior, next := m3Buckets(reset, m3ptr(20.0)), m3Buckets(reset, m3ptr(0.0))
			at := now
			row := store.GetCodexEarlyResetBaselineRow{LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}}
			if c.change != nil {
				c.change(&row, &prior, &next, &at)
			}
			raw, err := json.Marshal(prior)
			if err != nil {
				t.Fatal(err)
			}
			row.Buckets = raw
			expected, got := codexEarlyReset(row, next, at)
			if got != c.want {
				t.Fatalf("detected=%v want=%v", got, c.want)
			}
			if got && expected.Unix() != reset {
				t.Fatalf("expected reset=%v", expected)
			}
		})
	}
	for _, raw := range []string{"", "null", "{}", "true", "[", "[null]", "[1]", "[{\"id\":2}]", "[{\"id\":\"codex\",\"primary\":{\"used_percent\":\"0\"}}]", "[{\"id\":\"codex\",\"primary\":{\"reset_at\":1.5}}]"} {
		t.Run("json/"+raw, func(t *testing.T) {
			row := store.GetCodexEarlyResetBaselineRow{Buckets: []byte(raw), LastSuccessAt: pgtype.Timestamptz{Time: now, Valid: true}}
			if _, got := codexEarlyReset(row, m3Buckets(reset, m3ptr(0.0)), now); got {
				t.Fatal("malformed basis accepted")
			}
		})
	}
	for _, ts := range []pgtype.Timestamptz{{}, {Valid: true}, {Time: now, Valid: true, InfinityModifier: pgtype.Infinity}, {Time: now, Valid: true, InfinityModifier: pgtype.NegativeInfinity}, {Time: time.Unix(reset, 0), Valid: true}, {Time: time.Unix(reset+1, 0), Valid: true}} {
		raw, err := json.Marshal(m3Buckets(reset, m3ptr(20.0)))
		if err != nil {
			t.Fatal(err)
		}
		if _, got := codexEarlyReset(store.GetCodexEarlyResetBaselineRow{Buckets: raw, LastSuccessAt: ts}, m3Buckets(reset, m3ptr(0.0)), now); got {
			t.Fatalf("invalid timestamp accepted: %+v", ts)
		}
	}
}

type m3OrderedStore struct {
	*fakeStore
	baseline          store.GetCodexEarlyResetBaselineRow
	readErr, writeErr error
	events            []string
	lastRead          store.GetCodexEarlyResetBaselineParams
}

func (s *m3OrderedStore) GetCodexEarlyResetBaseline(_ context.Context, p store.GetCodexEarlyResetBaselineParams) (store.GetCodexEarlyResetBaselineRow, error) {
	s.events = append(s.events, "read")
	s.lastRead = p
	return s.baseline, s.readErr
}
func (s *m3OrderedStore) UpsertCodexAccountRateLimits(ctx context.Context, p store.UpsertCodexAccountRateLimitsParams) (int64, error) {
	s.events = append(s.events, "write")
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	n, err := s.fakeStore.UpsertCodexAccountRateLimits(ctx, p)
	if err == nil && n > 0 {
		s.baseline.Buckets = append([]byte(nil), p.Buckets...)
	}
	return n, err
}

type m3Notifier struct {
	st                 *m3OrderedStore
	fences             []notifysvc.CodexAccountFence
	err                error
	expected, observed time.Time
}

func (n *m3Notifier) NotifyCodexEarlyReset(_ context.Context, f notifysvc.CodexAccountFence, expected, observed time.Time) (store.Notification, error) {
	n.st.events = append(n.st.events, "notify")
	n.fences = append(n.fences, f)
	n.expected = expected
	n.observed = observed
	return store.Notification{}, n.err
}

func TestCodexEarlyResetWriteOrdering(t *testing.T) {
	for _, name := range []string{"accepted_refresh", "read_error", "foreign_owner", "foreign_account", "changed_sig", "unknown_sig", "empty_sig", "blank_sig", "optout", "write_error", "zero_rows", "notify_error", "authority_refused", "nil_notifier", "collection_error", "nan", "positive_inf", "negative_inf"} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			reset := now.Add(48 * time.Hour).Unix()
			u, a := uuid.New(), uuid.New()
			st := &m3OrderedStore{fakeStore: newFakeStore(), baseline: store.GetCodexEarlyResetBaselineRow{
				UserID: u, ProviderAccountID: a, EnablementSig: pgtype.Text{String: "aliases:1", Valid: true}, NotifyEarlyLimitReset: true, LastSuccessAt: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
			}}
			raw, err := json.Marshal(m3Buckets(reset, m3ptr(20.0)))
			if err != nil {
				t.Fatal(err)
			}
			st.baseline.Buckets = raw
			row := linkedRow(u, a, 1, 2, "idle", false, false)
			row.EnablementSig = "aliases:1"
			st.toPoll = []store.ListLinkedCodexAccountsToPollRow{row}
			reading := workersvc.CodexUsageReading{Buckets: m3Buckets(reset, m3ptr(0.0)), ObservedGeneration: 3, ObservedCredentialRevision: 4}
			result := collectResult{reading: reading}
			n := &m3Notifier{st: st}
			want := []string{"read", "write"}
			alert := false
			switch name {
			case "accepted_refresh":
				alert = true
			case "read_error":
				st.readErr = errors.New("read")
			case "foreign_owner":
				st.baseline.UserID = uuid.New()
			case "foreign_account":
				st.baseline.ProviderAccountID = uuid.New()
			case "changed_sig":
				st.baseline.EnablementSig.String = "different"
			case "unknown_sig":
				st.baseline.EnablementSig.Valid = false
			case "empty_sig":
				st.baseline.EnablementSig.String = ""
			case "blank_sig":
				st.baseline.EnablementSig.String = " \t"
				st.toPoll[0].EnablementSig = " \t"
			case "optout":
				st.baseline.NotifyEarlyLimitReset = false
			case "write_error":
				st.writeErr = errors.New("write")
			case "zero_rows":
				st.upsertRows = 0
			case "notify_error":
				n.err = errors.New("notify")
				alert = true
			case "authority_refused":
				n.err = notifysvc.ErrCredentialNotCurrent
				alert = true
			case "collection_error":
				result.err = errors.New("collection")
				want = nil
			case "nan":
				result.reading.Buckets[0].Primary.UsedPercent = m3ptr(math.NaN())
				want = nil
			case "positive_inf":
				result.reading.Buckets[0].Primary.UsedPercent = m3ptr(math.Inf(1))
				want = nil
			case "negative_inf":
				result.reading.Buckets[0].Primary.UsedPercent = m3ptr(math.Inf(-1))
				want = nil
			}
			col := &fakeCollector{results: map[uuid.UUID]collectResult{a: result}}
			e := New(st, col, &fakeReconciler{}, time.Minute, quietLogger())
			e.now = func() time.Time { return now }
			if name != "nil_notifier" {
				e.SetNotifier(n)
			}
			e.Boot(context.Background())
			if alert {
				want = append(want, "notify")
			}
			if !reflect.DeepEqual(st.events, want) {
				t.Fatalf("order=%v want=%v", st.events, want)
			}
			if len(want) > 0 && st.lastRead != (store.GetCodexEarlyResetBaselineParams{UserID: u, ProviderAccountID: a, EnablementSig: st.toPoll[0].EnablementSig}) {
				t.Fatalf("baseline params=%+v", st.lastRead)
			}
			if len(st.upserts) > 0 {
				w := st.upserts[0]
				if w.ObservedGeneration != 3 || w.ObservedCredentialRevision != 4 || w.EnablementSig != st.toPoll[0].EnablementSig {
					t.Fatalf("stale write fence: %+v", w)
				}
			}
			if alert {
				fence := notifysvc.CodexAccountFence{OwnerID: u, AccountID: a, Generation: 3, CredentialRevision: 4, EnablementSig: row.EnablementSig}
				if len(n.fences) != 1 || n.fences[0] != fence || n.expected.Unix() != reset || !n.observed.Equal(now) {
					t.Fatalf("notification=%+v", n)
				}
				e.Boot(context.Background())
				if len(n.fences) != 1 {
					t.Fatal("accepted baseline did not advance after notification attempt")
				}
			}
			if name == "collection_error" && len(st.failures) != 1 {
				t.Fatal("collection failure not recorded")
			}
		})
	}
}
