package workersvc

import (
	"errors"
	"testing"
)

// harness_resolver_test.go pins the PURE D11 resolver (PRD #1332 D4). It is
// database-free: resolveHarness is a pure value function, so every branch and
// combination of the D11 order is asserted here against hand-written inputs. The
// store-backed availability/credential-selection layer is proved separately in
// harness_resolver_livedb_test.go.

// h returns a *Harness for a table row's explicit / userDefault field.
func h(v Harness) *Harness { return &v }

func TestResolveHarness(t *testing.T) {
	both := harnessAvailability{claudeUsable: true, codexUsable: true}
	claudeOnly := harnessAvailability{claudeUsable: true, codexUsable: false}
	codexOnly := harnessAvailability{claudeUsable: false, codexUsable: true}
	none := harnessAvailability{claudeUsable: false, codexUsable: false}

	tests := []struct {
		name    string
		in      resolveHarnessInput
		want    Harness
		wantErr error
	}{
		// --- (1) explicit selection: used if usable, else a typed refusal with NO fallback.
		{
			name: "explicit claude usable",
			in:   resolveHarnessInput{explicit: h(HarnessClaude), avail: both},
			want: HarnessClaude,
		},
		{
			name:    "explicit claude unusable -> error",
			in:      resolveHarnessInput{explicit: h(HarnessClaude), avail: codexOnly},
			wantErr: errNoCredentialForHarness,
		},
		{
			name: "explicit codex usable",
			in:   resolveHarnessInput{explicit: h(HarnessCodex), avail: both},
			want: HarnessCodex,
		},
		{
			// The no-fallback guarantee: explicit Codex with Codex unusable but Claude
			// usable must REFUSE, never return Claude.
			name:    "explicit codex unusable does NOT fall back to usable claude",
			in:      resolveHarnessInput{explicit: h(HarnessCodex), avail: claudeOnly},
			wantErr: errNoCredentialForHarness,
		},
		{
			name:    "explicit claude unusable does NOT fall back to usable codex",
			in:      resolveHarnessInput{explicit: h(HarnessClaude), avail: codexOnly},
			wantErr: errNoCredentialForHarness,
		},
		{
			// An explicit selection is authoritative even when a usable default of the
			// OTHER harness is set — it still refuses rather than honoring the default.
			name:    "explicit codex unusable ignores a usable claude default",
			in:      resolveHarnessInput{explicit: h(HarnessCodex), userDefault: h(HarnessClaude), avail: claudeOnly},
			wantErr: errNoCredentialForHarness,
		},

		// --- (2) usable user default wins over the availability tiebreak.
		{
			name: "default claude used when both usable",
			in:   resolveHarnessInput{userDefault: h(HarnessClaude), avail: both},
			want: HarnessClaude,
		},
		{
			name: "default codex wins the tiebreak when both usable",
			in:   resolveHarnessInput{userDefault: h(HarnessCodex), avail: both},
			want: HarnessCodex,
		},
		{
			// A default set to a harness with no usable credential is SKIPPED (fall
			// through to the availability rules), not an error.
			name: "default codex set but unusable falls through to sole claude",
			in:   resolveHarnessInput{userDefault: h(HarnessCodex), avail: claudeOnly},
			want: HarnessClaude,
		},
		{
			name: "default claude set but unusable falls through to sole codex",
			in:   resolveHarnessInput{userDefault: h(HarnessClaude), avail: codexOnly},
			want: HarnessCodex,
		},
		{
			// Default set but unusable AND nothing else usable → the neither-usable
			// refusal, not the no_credential_for_harness explicit refusal.
			name:    "default codex set but unusable with nothing usable -> no usable credential",
			in:      resolveHarnessInput{userDefault: h(HarnessCodex), avail: none},
			wantErr: errNoUsableCredential,
		},

		// --- (3) sole available harness.
		{
			name: "sole claude",
			in:   resolveHarnessInput{avail: claudeOnly},
			want: HarnessClaude,
		},
		{
			name: "sole codex",
			in:   resolveHarnessInput{avail: codexOnly},
			want: HarnessCodex,
		},

		// --- (4) both usable, no usable default -> Claude.
		{
			name: "both usable no default -> claude",
			in:   resolveHarnessInput{avail: both},
			want: HarnessClaude,
		},

		// --- (5) neither usable.
		{
			name:    "none usable -> no usable credential",
			in:      resolveHarnessInput{avail: none},
			wantErr: errNoUsableCredential,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveHarness(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if got != "" {
					t.Fatalf("harness = %q, want empty on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("harness = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveHarnessNoUsableCredentialRetainsRefusal pins that the neither-usable
// refusal WRAPS errCredentialUnavailable, so a consumer keying on the existing
// credential-unavailable sentinel keeps the existing refusal/failure behavior (D4)
// while the resolver-specific sentinel is still independently assertable.
func TestResolveHarnessNoUsableCredentialRetainsRefusal(t *testing.T) {
	_, err := resolveHarness(resolveHarnessInput{avail: harnessAvailability{}})
	if !errors.Is(err, errNoUsableCredential) {
		t.Fatalf("err = %v, want errNoUsableCredential", err)
	}
	if !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("err = %v, want it to also satisfy errCredentialUnavailable (existing refusal behavior)", err)
	}
}

// TestHarnessConstsMatchSchemaLiterals guards that the exported Harness values stay
// byte-equal to the run_usage/runs schema literals usage_fold.go owns, since one is
// built from the other and a future edit could sever that.
func TestHarnessConstsMatchSchemaLiterals(t *testing.T) {
	if string(HarnessClaude) != harnessClaude {
		t.Errorf("HarnessClaude = %q, want %q", HarnessClaude, harnessClaude)
	}
	if string(HarnessCodex) != harnessCodex {
		t.Errorf("HarnessCodex = %q, want %q", HarnessCodex, harnessCodex)
	}
}
