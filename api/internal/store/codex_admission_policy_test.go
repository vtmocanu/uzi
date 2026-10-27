package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type admissionVote struct {
	Run      string    `json:"run"`
	At       time.Time `json:"t"`
	Consumed bool      `json:"consumed"`
	Episode  *string   `json:"episode"`
}
type admissionPolicyState struct {
	RateStep       int               `json:"rate_step"`
	RateExpiry     time.Time         `json:"rate_expiry"`
	RateLast       time.Time         `json:"rate_last_qualifying"`
	OverloadStep   int               `json:"overload_step"`
	OverloadExpiry time.Time         `json:"overload_expiry"`
	OverloadLast   time.Time         `json:"overload_last_qualifying"`
	Clock          time.Time         `json:"observation_clock"`
	Effective      time.Time         `json:"effective_expiry"`
	Floor          time.Time         `json:"recovery_floor"`
	Episode        string            `json:"episode_id"`
	Width          int               `json:"spread_width_ms"`
	Receipts       []json.RawMessage `json:"receipts"`
	Votes          []admissionVote   `json:"overload_votes"`
}
type admissionPolicyFixture struct {
	t                    *testing.T
	pool                 *pgxpool.Pool
	raw                  []byte
	state                admissionPolicyState
	owner, account, seed uuid.UUID
}

func newAdmissionPolicyFixture(t *testing.T) *admissionPolicyFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &admissionPolicyFixture{t: t, pool: pool, raw: []byte("{}"), owner: uuid.New(), account: uuid.New(), seed: uuid.New()}
}
func (f *admissionPolicyFixture) observe(run, attempt uuid.UUID, class string, at time.Time, jitter float64) error {
	f.t.Helper()
	var raw []byte
	err := f.pool.QueryRow(context.Background(), `SELECT to_jsonb(codex_admission_transition(
        jsonb_populate_record(NULL::codex_account_admission_throttle,$1::jsonb),
        $2::uuid,$3::uuid,$4::uuid,1::bigint,$5::uuid,$6::text,$7::timestamptz,$8::float8,$9::uuid))`,
		f.raw, f.owner, f.account, run, attempt, class, at, jitter, f.seed).Scan(&raw)
	if err != nil {
		return err
	}
	var state admissionPolicyState
	if err = json.Unmarshal(raw, &state); err != nil {
		return err
	}
	f.raw, f.state = raw, state
	return nil
}
func (f *admissionPolicyFixture) must(run uuid.UUID, class string, at time.Time, jitter float64) {
	f.t.Helper()
	if err := f.observe(run, uuid.New(), class, at, jitter); err != nil {
		f.t.Fatal(err)
	}
}
func admissionTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestCodexAdmissionRateScheduleLiveDB(t *testing.T) {
	f := newAdmissionPolicyFixture(t)
	run, attempt := uuid.New(), uuid.New()
	at := admissionTime()
	if err := f.observe(run, attempt, "rate_limit", at, 0); err != nil {
		t.Fatal(err)
	}
	if f.state.RateStep != 0 || !f.state.RateExpiry.Equal(at.Add(30*time.Second)) || f.state.Width != 30000 {
		t.Fatalf("first schedule: %+v", f.state)
	}
	seed := f.state.Episode
	f.seed = uuid.New()
	f.must(run, "rate_limit", at.Add(10*time.Second), 0.2)
	if f.state.Episode != seed || !f.state.RateExpiry.Equal(at.Add(30*time.Second)) || !f.state.RateLast.Equal(at.Add(10*time.Second)) {
		t.Fatal("in-episode signal extended duration or changed seed")
	}
	before := string(f.raw)
	if err := f.observe(run, attempt, "rate_limit", at.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if string(f.raw) != before {
		t.Fatal("duplicate changed state, even after expiry")
	}
	f.must(run, "rate_limit", at.Add(30*time.Second), 0)
	if f.state.RateStep != 1 || !f.state.RateExpiry.Equal(at.Add(90*time.Second)) {
		t.Fatal("exact expiry did not advance step")
	}
	if f.state.Episode == seed || !f.state.Floor.Equal(at.Add(60*time.Second)) {
		t.Fatal("new episode in old spread did not install floor and new seed")
	}
	beforeExpiry := f.state.RateExpiry
	f.must(run, "rate_limit", at.Add(20*time.Second), 0)
	if !f.state.Clock.Equal(at.Add(30*time.Second)) || !f.state.RateLast.Equal(f.state.Clock) || !f.state.RateExpiry.Equal(beforeExpiry) {
		t.Fatal("backwards clock moved eligibility or qualifying clock backwards")
	}
	f.must(run, "rate_limit", at.Add(time.Hour), 0)
	if f.state.RateStep != 0 || !f.state.RateExpiry.Equal(at.Add(time.Hour+30*time.Second)) {
		t.Fatal("quiet did not reset step")
	}
}

func TestCodexAdmissionCapsLiveDB(t *testing.T) {
	for _, class := range []string{"rate_limit", "overload"} {
		t.Run(class, func(t *testing.T) {
			f := newAdmissionPolicyFixture(t)
			at := admissionTime()
			for i := 0; i < 35; i++ {
				if class == "overload" {
					f.must(uuid.New(), class, at, 0.2)
				}
				f.must(uuid.New(), class, at, 0.2)
				if class == "rate_limit" {
					at = f.state.RateExpiry
				} else {
					at = f.state.OverloadExpiry
				}
			}
			step, expiry, last, capDuration := f.state.RateStep, f.state.RateExpiry, f.state.RateLast, 18*time.Minute
			if class == "overload" {
				step, expiry, last, capDuration = f.state.OverloadStep, f.state.OverloadExpiry, f.state.OverloadLast, 6*time.Minute
			}
			if step != 30 || expiry.Sub(last) != capDuration {
				t.Fatalf("saturation step=%d duration=%s", step, expiry.Sub(last))
			}
		})
	}
}

func TestCodexAdmissionOverloadVotesLiveDB(t *testing.T) {
	f := newAdmissionPolicyFixture(t)
	at := admissionTime()
	a, b, c, d, e := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	f.must(a, "overload", at, 0)
	f.must(a, "overload", at.Add(time.Minute), 0)
	if !f.state.OverloadLast.IsZero() || len(f.state.Votes) != 1 || !f.state.Votes[0].At.Equal(at) {
		t.Fatal("same run refreshed vote or qualified alone")
	}
	f.must(b, "overload", at.Add(61*time.Second), 0)
	expiry := at.Add(76 * time.Second)
	if !f.state.OverloadExpiry.Equal(expiry) || f.state.OverloadStep != 0 || !f.state.Votes[0].Consumed {
		t.Fatal("fresh distinct pair failed to open/consume")
	}
	f.must(a, "overload", at.Add(62*time.Second), 0)
	if !f.state.OverloadLast.Equal(at.Add(61 * time.Second)) {
		t.Fatal("consumed run voted again")
	}
	f.must(c, "overload", at.Add(63*time.Second), 0)
	f.must(d, "overload", at.Add(64*time.Second), 0)
	if !f.state.OverloadLast.Equal(at.Add(64*time.Second)) || !f.state.OverloadExpiry.Equal(expiry) || f.state.OverloadStep != 0 {
		t.Fatal("in-episode pair changed expiry/step or failed to update qualifying time")
	}
	f.must(e, "overload", at.Add(65*time.Second), 0)
	f.must(uuid.New(), "overload", expiry, 0)
	if !f.state.OverloadExpiry.Equal(expiry) || !f.state.OverloadLast.Equal(at.Add(64*time.Second)) {
		t.Fatal("unconsumed episode vote carried over expiry")
	}
	f.must(e, "overload", at.Add(77*time.Second), 0)
	if !f.state.OverloadExpiry.Equal(expiry) {
		t.Fatal("tombstoned run voted again")
	}
	f.must(uuid.New(), "overload", at.Add(78*time.Second), 0)
	if f.state.OverloadStep != 1 || !f.state.OverloadExpiry.Equal(at.Add(108*time.Second)) {
		t.Fatal("fresh post-expiry pair failed")
	}
	// At exactly five minutes A's ORIGINAL vote expires and may be replaced.
	f.must(a, "overload", at.Add(5*time.Minute), 0)
	found := false
	for _, v := range f.state.Votes {
		if v.Run == a.String() {
			found = true
			if !v.At.Equal(at.Add(5 * time.Minute)) {
				t.Fatal("expired vote not replaced")
			}
		}
	}
	if !found {
		t.Fatal("replacement vote missing")
	}
	f.must(uuid.New(), "overload", at.Add(time.Hour), 0)
	f.must(uuid.New(), "overload", at.Add(time.Hour), 0)
	if f.state.OverloadStep != 0 {
		t.Fatal("overload quiet did not reset")
	}
}

func TestCodexAdmissionCapacityAndRetentionLiveDB(t *testing.T) {
	f := newAdmissionPolicyFixture(t)
	at := admissionTime()
	first, attempt := uuid.New(), uuid.New()
	if err := f.observe(first, attempt, "rate_limit", at, 0); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 256; i++ {
		f.must(uuid.New(), "rate_limit", at, 0)
	}
	before := string(f.raw)
	if err := f.observe(uuid.New(), uuid.New(), "rate_limit", at.Add(59*time.Second), 0); err == nil {
		t.Fatal("live receipts evicted on overflow")
	}
	if string(f.raw) != before {
		t.Fatal("overflow changed state")
	}
	if err := f.observe(first, attempt, "rate_limit", at.Add(59*time.Second), 0); err != nil {
		t.Fatal("duplicate refused at capacity")
	}
	if string(f.raw) != before {
		t.Fatal("duplicate at capacity changed state")
	}
	f.must(uuid.New(), "rate_limit", at.Add(time.Minute), 0)
	if len(f.state.Receipts) != 1 {
		t.Fatal("expired receipts not pruned at 60s")
	}

	f = newAdmissionPolicyFixture(t)
	// Receipts age out while the run-vote suppression ledger stays live for five minutes.
	for i := 0; i < 256; i++ {
		f.must(uuid.New(), "overload", at.Add(time.Duration(i)*time.Second), 0)
	}
	before = string(f.raw)
	if err := f.observe(uuid.New(), uuid.New(), "overload", at.Add(256*time.Second), 0); err == nil {
		t.Fatal("live run ledger evicted on overflow")
	}
	if string(f.raw) != before {
		t.Fatal("ledger overflow partially applied")
	}
	f.must(uuid.New(), "overload", at.Add(5*time.Minute), 0)
	if len(f.state.Votes) != 256 {
		t.Fatal("expired ledger slot not replaced")
	}
}

func TestCodexAdmissionIndependentClassesLiveDB(t *testing.T) {
	f := newAdmissionPolicyFixture(t)
	at := admissionTime()
	f.must(uuid.New(), "rate_limit", at, 0)
	rateLast, seed, width := f.state.RateLast, f.state.Episode, f.state.Width
	f.seed = uuid.New()
	f.must(uuid.New(), "overload", at.Add(15*time.Second), 0)
	f.must(uuid.New(), "overload", at.Add(15*time.Second), 0)
	if !f.state.RateExpiry.Equal(f.state.OverloadExpiry) || !f.state.RateLast.Equal(rateLast) ||
		!f.state.Effective.Equal(at.Add(30*time.Second)) || f.state.Episode != seed || f.state.Width != width {
		t.Fatal("classes coupled clocks or replaced common episode on extension")
	}
	f.must(uuid.New(), "rate_limit", at.Add(30*time.Second), 0)
	if f.state.RateStep != 1 || f.state.OverloadStep != 0 || !f.state.OverloadLast.Equal(at.Add(15*time.Second)) {
		t.Fatal("rate changed overload clock or step")
	}
	if !f.state.Effective.Equal(at.Add(90 * time.Second)) {
		t.Fatal("later rate expiry did not control the combined gate")
	}
}

// A shorter class opening during spread must retain the entire previous window
// before it changes the seed, even when the class expiry itself is earlier.
func TestCodexAdmissionRecoveryFloorLiveDB(t *testing.T) {
	f := newAdmissionPolicyFixture(t)
	at := admissionTime()
	f.must(uuid.New(), "rate_limit", at, 0)
	priorSeed := f.state.Episode
	f.seed = uuid.New()
	f.must(uuid.New(), "overload", at.Add(31*time.Second), 0)
	f.must(uuid.New(), "overload", at.Add(31*time.Second), 0)
	if !f.state.OverloadExpiry.Equal(at.Add(46*time.Second)) ||
		!f.state.Floor.Equal(at.Add(60*time.Second)) ||
		!f.state.Effective.Equal(at.Add(60*time.Second)) ||
		f.state.Episode == priorSeed || f.state.Width != 15000 {
		t.Fatalf("new episode shortened the preceding recovery window: %+v", f.state)
	}
}
