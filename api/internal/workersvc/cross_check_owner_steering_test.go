package workersvc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

type ownerSteeringFixture struct {
	Input        ownerSteeringInput `json:"input"`
	Snapshot     string             `json:"snapshot"`
	Digest       string             `json:"digest"`
	WireMaxBytes int                `json:"wire_max_bytes"`
}

func loadOwnerSteeringFixture(t *testing.T) ownerSteeringFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/cross-check-owner-steering/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx ownerSteeringFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestOwnerSteeringFixtureDrift(t *testing.T) {
	fx := loadOwnerSteeringFixture(t)
	got, err := buildOwnerSteeringSnapshot(fx.Input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fx.Snapshot {
		t.Fatalf("builder output drifted from the fixture:\n got: %s\nwant: %s", got, fx.Snapshot)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != fx.Digest {
		t.Fatal("fixture digest is not the sha256 of the snapshot")
	}
	if fx.WireMaxBytes != 393337 {
		t.Fatalf("fixture wire_max_bytes = %d, want 393337", fx.WireMaxBytes)
	}
	if got := fx.WireMaxBytes; ownerSteeringWireMaxBytes != got {
		t.Fatalf("wire max constant %d, fixture %d", ownerSteeringWireMaxBytes, got)
	}
}

func decodeSteering(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func answerRow(id int64, qid string, answers ...string) ownerSteeringRow {
	return ownerSteeringRow{InputID: id, Kind: "answer", QuestionID: qid, AnswersStatus: "ok", Answers: answers,
		Occurrences: 1, Variants: 1, QuestionAgent: "lead", QuestionsStatus: "ok",
		Questions: []steeringQuestion{{Header: "H", Question: "Q?", Options: []string{"a", "b"}}}}
}

func TestOwnerSteeringBuilderOrderingAndRevise(t *testing.T) {
	bound, rev := "bound", int64(3)
	unbound := "unbound"
	body := "tighten scope"
	in := ownerSteeringInput{Cutoff: 9, Total: 3, Rows: []ownerSteeringRow{
		{InputID: 9, Kind: "revise_plan", ReviseBody: &body, GateBinding: &bound, GateRevision: &rev},
		answerRow(4, "q1", "yes"),
		{InputID: 7, Kind: "revise_plan", GateBinding: &unbound},
	}}
	raw, err := buildOwnerSteeringSnapshot(in)
	if err != nil {
		t.Fatal(err)
	}
	snap := decodeSteering(t, raw)
	entries := snap["owner_entries"].([]any)
	var ids []float64
	for _, e := range entries {
		ids = append(ids, e.(map[string]any)["input_id"].(float64))
	}
	if len(ids) != 3 || ids[0] != 4 || ids[1] != 7 || ids[2] != 9 {
		t.Fatalf("entries not ascending by input id: %v", ids)
	}
	nullRevise := entries[1].(map[string]any)
	if nullRevise["kind"] != "revise" || nullRevise["feedback"] != nil || nullRevise["gate_binding"] != "unbound" || nullRevise["gate_revision"] != nil {
		t.Fatalf("unbound NULL-body revise mis-shaped: %v", nullRevise)
	}
	boundRevise := entries[2].(map[string]any)
	if boundRevise["feedback"] != body || boundRevise["gate_binding"] != "bound" || boundRevise["gate_revision"] != float64(3) {
		t.Fatalf("bound revise mis-shaped: %v", boundRevise)
	}
	ans := entries[0].(map[string]any)
	if ans["question_id"] != "q1" || ans["answers_status"] != "ok" {
		t.Fatalf("answer not tied to its question id: %v", ans)
	}
}

func TestOwnerSteeringContextBindingAndAuthor(t *testing.T) {
	none := answerRow(1, "q1", "a")
	none.Occurrences, none.Variants = 0, 0
	ambiguous := answerRow(2, "q2", "a")
	ambiguous.Variants = 2
	malformed := answerRow(3, "q3", "a")
	malformed.QuestionsStatus = "malformed"
	oversized := answerRow(4, "q4", "a")
	oversized.QuestionsStatus = "oversized"
	worker := answerRow(5, "q5", "a")
	worker.QuestionAgent = "worker"
	other := answerRow(6, "q6", "a")
	other.QuestionAgent = "owner"
	raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 6, Total: 6, Rows: []ownerSteeringRow{none, ambiguous, malformed, oversized, worker, other}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := decodeSteering(t, raw)["question_context"].([]any)
	want := []string{"unavailable", "ambiguous", "malformed", "oversized", "bound", "bound"}
	for i, c := range ctx {
		m := c.(map[string]any)
		if m["status"] != want[i] {
			t.Fatalf("context %d status %v, want %s", i, m["status"], want[i])
		}
		if want[i] != "bound" {
			if _, has := m["questions"]; has || len(m) != 3 {
				t.Fatalf("unbound context carries text: %v", m)
			}
		}
	}
	if a := ctx[4].(map[string]any)["author"]; a != "worker" {
		t.Fatalf("worker author = %v", a)
	}
	if a := ctx[5].(map[string]any)["author"]; a != "worker" {
		t.Fatalf("unknown agent must never read as an owner voice: %v", a)
	}
	lead := answerRow(7, "q7", "a")
	raw, _ = buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 7, Total: 1, Rows: []ownerSteeringRow{lead}})
	if a := decodeSteering(t, raw)["question_context"].([]any)[0].(map[string]any)["author"]; a != "lead" {
		t.Fatalf("lead author = %v", a)
	}
}

func TestOwnerSteeringScrubsSecrets(t *testing.T) {
	secret := "sk-ant-" + "api03-abcdefghijklmnop"
	body := "use key " + secret
	row := answerRow(1, "q1", "token "+secret)
	row.Questions[0].Question = "paste " + secret
	row.Questions[0].Options = []string{secret}
	raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 2, Total: 2, Rows: []ownerSteeringRow{row, {InputID: 2, Kind: "revise_plan", ReviseBody: &body}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "abcdefghijklmnop") || !strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("secret survived or no redaction marker: %s", raw)
	}
}

func TestOwnerSteeringUTF8Truncation(t *testing.T) {
	// Answers stay within the per-answer character cap the query enforces, so only the byte budget cuts.
	long := strings.Repeat("é", 2048) // 4096 bytes, two bytes per rune
	odd := "x" + strings.Repeat("é", 2047)
	row := answerRow(1, "q1", long, odd, "é")
	row.Questions[0].Question = strings.Repeat("é", 4000)
	revise := strings.Repeat("日", 4000)
	raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 2, Total: 2, Rows: []ownerSteeringRow{row, {InputID: 2, Kind: "revise_plan", ReviseBody: &revise}}})
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Entries []struct {
			Answers   []string `json:"answers"`
			Feedback  *string  `json:"feedback"`
			Truncated bool     `json:"truncated"`
		} `json:"owner_entries"`
		Context []struct {
			Questions []steeringQuestion `json:"questions"`
			Truncated bool               `json:"truncated"`
		} `json:"question_context"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, a := range snap.Entries[0].Answers {
		if !utf8.ValidString(a) {
			t.Fatal("answer cut inside a rune")
		}
		total += len(a)
	}
	// 8191 bytes kept: the last answer's two-byte rune does not fit the one byte left, so it is dropped whole.
	if total != ownerSteeringAnswerBytes-1 || !snap.Entries[0].Truncated {
		t.Fatalf("answers total %d bytes, truncated=%v", total, snap.Entries[0].Truncated)
	}
	f := *snap.Entries[1].Feedback
	if !utf8.ValidString(f) || len(f) > ownerSteeringReviseBytes || len(f) < ownerSteeringReviseBytes-3 || !snap.Entries[1].Truncated {
		t.Fatalf("revise cut wrong: %d bytes truncated=%v", len(f), snap.Entries[1].Truncated)
	}
	q := snap.Context[0].Questions[0].Question
	if !utf8.ValidString(q) || len(q) > ownerSteeringContextBytes || !snap.Context[0].Truncated {
		t.Fatalf("context cut wrong: %d bytes truncated=%v", len(q), snap.Context[0].Truncated)
	}
}

func TestOwnerSteeringEntryCapMarkers(t *testing.T) {
	var rows []ownerSteeringRow
	for i := 1; i <= ownerSteeringMaxEntries; i++ {
		rows = append(rows, answerRow(int64(100+i), "q"+strconv.Itoa(i), "a"))
	}
	raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 200, Total: 40, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	snap := decodeSteering(t, raw)
	if snap["omitted_entries"] != float64(8) || snap["overflow"] != true || snap["overflow_reason"] != "entry_cap" || snap["cutoff_input_id"] != float64(200) {
		t.Fatalf("entry cap markers wrong: %v", snap)
	}
	raw, _ = buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 0, Total: 0})
	if string(raw) != `{"version":1,"cutoff_input_id":0,"owner_entries":[],"question_context":[],"omitted_entries":0,"overflow":false,"overflow_reason":""}` {
		t.Fatalf("empty snapshot: %s", raw)
	}
}

func TestOwnerSteeringByteCapDropsOldestWithContext(t *testing.T) {
	var rows []ownerSteeringRow
	for i := 1; i <= ownerSteeringMaxEntries; i++ {
		rows = append(rows, answerRow(int64(i), "q"+strconv.Itoa(i), strings.Repeat("z", ownerSteeringAnswerBytes)))
	}
	raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 32, Total: 32, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > ownerSteeringSnapshotMaxBytes {
		t.Fatalf("snapshot %d bytes over the cap", len(raw))
	}
	var snap struct {
		Entries []struct {
			InputID int64 `json:"input_id"`
		} `json:"owner_entries"`
		Context []struct {
			AnswerInputID int64 `json:"answer_input_id"`
		} `json:"question_context"`
		Omitted int64  `json:"omitted_entries"`
		Reason  string `json:"overflow_reason"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Reason != "byte_cap" || snap.Omitted <= 0 || int64(len(snap.Entries))+snap.Omitted != 32 || len(snap.Context) != len(snap.Entries) {
		t.Fatalf("byte cap markers: reason=%q omitted=%d kept=%d ctx=%d", snap.Reason, snap.Omitted, len(snap.Entries), len(snap.Context))
	}
	// The newest entries survive, ascending, and each kept entry still has its context.
	last := snap.Entries[len(snap.Entries)-1].InputID
	if last != 32 || snap.Entries[0].InputID != 32-int64(len(snap.Entries))+1 {
		t.Fatalf("not the newest contiguous run: first=%d last=%d", snap.Entries[0].InputID, last)
	}
	for i := range snap.Entries {
		if snap.Entries[i].InputID != snap.Context[i].AnswerInputID {
			t.Fatal("context detached from its entry")
		}
	}
}

// maximalEscapeSnapshot is 65536 bytes of valid UTF-8 whose claim encoding is as large as the
// encoder allows: raw '<' is six bytes on the wire, U+2028 is three raw bytes and six encoded.
func maximalEscapeSnapshot(unit string) string {
	var b strings.Builder
	for b.Len()+len(unit) <= ownerSteeringSnapshotMaxBytes {
		b.WriteString(unit)
	}
	for b.Len() < ownerSteeringSnapshotMaxBytes {
		b.WriteByte('<')
	}
	return b.String()
}

func steeringFor(snapshot string) *ClaimOwnerSteering {
	sum := sha256.Sum256([]byte(snapshot))
	return &ClaimOwnerSteering{Snapshot: snapshot, Digest: hex.EncodeToString(sum[:])}
}

func TestOwnerSteeringWireGrowthWithinBound(t *testing.T) {
	for name, snap := range map[string]string{
		"angle":     maximalEscapeSnapshot("<"),
		"quote":     maximalEscapeSnapshot("\"<"),
		"slash":     maximalEscapeSnapshot("\\<"),
		"u2028":     maximalEscapeSnapshot(" "),
		"empty":     "",
		"realistic": `{"version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := boundedCheckerFixture(t)
			without, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			p.CrossCheckOwnerSteering = steeringFor(snap)
			with, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if growth := len(with) - len(without); growth > ownerSteeringWireMaxBytes {
				t.Fatalf("steering added %d bytes, bound %d", growth, ownerSteeringWireMaxBytes)
			}
		})
	}
	p := boundedCheckerFixture(t)
	without, _ := json.Marshal(p)
	p.CrossCheckOwnerSteering = steeringFor(maximalEscapeSnapshot("<"))
	with, _ := json.Marshal(p)
	if len(with)-len(without) != ownerSteeringWireMaxBytes {
		t.Fatalf("all-angle snapshot should reach the bound exactly: %d vs %d", len(with)-len(without), ownerSteeringWireMaxBytes)
	}
}

func TestOwnerSteeringClaimValidation(t *testing.T) {
	p := boundedCheckerFixture(t)
	p.CrossCheckOwnerSteering = steeringFor("{}")
	if err := ValidateCrossCheckClaim(p); err != nil {
		t.Fatal(err)
	}
	bad := steeringFor("{}")
	bad.Digest = strings.Repeat("0", 64)
	for name, mutate := range map[string]func(*ClaimPayload){
		"digest": func(p *ClaimPayload) { p.CrossCheckOwnerSteering = bad },
		"oversize": func(p *ClaimPayload) {
			p.CrossCheckOwnerSteering = steeringFor(strings.Repeat("x", ownerSteeringSnapshotMaxBytes+1))
		},
		"utf8": func(p *ClaimPayload) { p.CrossCheckOwnerSteering = steeringFor("\xff") },
		"hex":  func(p *ClaimPayload) { p.CrossCheckOwnerSteering = &ClaimOwnerSteering{Snapshot: "{}", Digest: "zz"} },
		"code": func(p *ClaimPayload) { p.CrossCheck.Stage = "code" },
		"kind": func(p *ClaimPayload) {
			p.Kind, p.CrossCheck = "issue", nil
		},
	} {
		p := boundedCheckerFixture(t)
		p.CrossCheckOwnerSteering = steeringFor("{}")
		mutate(p)
		if _, err := MarshalCrossCheckClaim(p); !errors.Is(err, ErrCrossCheckRefused) {
			t.Errorf("%s: steering accepted (%v)", name, err)
		}
	}
}

// semanticEnvelopeFixture builds a minimal valid semantic_v1 plan-checker claim.
func semanticEnvelopeFixture(t *testing.T) *ClaimPayload {
	t.Helper()
	structural, proposal, sources := semanticFixture()
	structural.BaseCommit = strings.Repeat("a", 40)
	structural.SizeClass = "s"
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision = 1
	candidate.Criteria[0].ID = uuid.NewString()
	digest, err := semanticCheckerDigest(structural, candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Digest = hex.EncodeToString(digest)
	structural.CandidateIdentityVersion = semanticIdentityVersion
	structural.SemanticCandidate = &candidate
	return &ClaimPayload{Kind: runkind.CrossCheck, RunID: uuid.NewString(), CrossCheck: &ClaimPlanCrossCheck{
		Stage: "plan", Round: 1, LeadRunID: uuid.NewString(), CandidateDigest: candidate.Digest,
		DeadlineAt: time.Now().Add(time.Minute), PlanCrossCheckCandidate: structural,
	}}
}

// padEnvelopeTo sets Branch so the payload without steering encodes to exactly n bytes
// (escape-heavy text in the issue description keeps part of the size in six-byte escapes).
func padEnvelopeTo(t *testing.T, p *ClaimPayload, n int) {
	t.Helper()
	p.IssueDescription = strings.Repeat("<", 1000)
	empty := ""
	p.Branch = &empty
	steering := p.CrossCheckOwnerSteering
	p.CrossCheckOwnerSteering = nil
	base, err := json.Marshal(p)
	p.CrossCheckOwnerSteering = steering
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", n-len(base))
	p.Branch = &pad
}

func TestOwnerSteeringEnvelopeBoundaries(t *testing.T) {
	type lane struct {
		name  string
		limit int
		code  bool
		build func(*testing.T) *ClaimPayload
	}
	lanes := []lane{
		{"legacy_plan", crossCheckEnvelopeBytes, false, boundedCheckerFixture},
		{"semantic_v1_plan", 64 << 20, false, semanticEnvelopeFixture},
		{"code", crossCheckEnvelopeBytes, true, guidanceWireFixture},
	}
	snapshots := map[string]string{"maximal": maximalEscapeSnapshot("<"), "empty": ""}
	for _, l := range lanes {
		for sname, snap := range snapshots {
			t.Run(l.name+"/"+sname, func(t *testing.T) {
				started := time.Now()
				for _, tc := range []struct {
					delta   int
					deliver bool
				}{{-1, true}, {0, false}, {1, false}} {
					// Without steering: the decision the envelope has always made.
					plain := l.build(t)
					padEnvelopeTo(t, plain, l.limit+tc.delta)
					base, baseErr := MarshalCrossCheckClaim(plain)
					if tc.deliver != (baseErr == nil) {
						t.Fatalf("limit%+d without steering: delivered=%v err=%v", tc.delta, baseErr == nil, baseErr)
					}
					if baseErr == nil && (strings.Contains(string(base), "cross_check_owner_steering") || len(base) != l.limit+tc.delta+1) {
						t.Fatal("claim without steering changed shape")
					}
					// With steering: the same decision on a plan claim, delivering the same stored
					// bytes; a code-stage claim never accepts the field.
					p := l.build(t)
					p.CrossCheckOwnerSteering = steeringFor(snap)
					padEnvelopeTo(t, p, l.limit+tc.delta)
					raw, err := MarshalCrossCheckClaim(p)
					if l.code {
						if !errors.Is(err, ErrCrossCheckRefused) || raw != nil {
							t.Fatalf("code stage accepted steering: %v", err)
						}
						continue
					}
					if tc.deliver != (err == nil) {
						t.Fatalf("limit%+d with steering: delivered=%v err=%v", tc.delta, err == nil, err)
					}
					if err != nil {
						if !errors.Is(err, ErrCrossCheckRefused) || raw != nil {
							t.Fatal("refusal leaked bytes")
						}
						continue
					}
					var decoded ClaimPayload
					if err := json.Unmarshal(raw, &decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.CrossCheckOwnerSteering == nil || decoded.CrossCheckOwnerSteering.Snapshot != snap ||
						decoded.CrossCheckOwnerSteering.Digest != p.CrossCheckOwnerSteering.Digest {
						t.Fatal("stored steering bytes changed on delivery")
					}
					if p.CrossCheckOwnerSteering == nil {
						t.Fatal("marshal dropped the caller's steering field")
					}
				}
				t.Logf("%s/%s boundary set took %s", l.name, sname, time.Since(started))
			})
		}
	}
}

// steeringTokenFor builds a GitHub-classic-shaped token at runtime so no complete token literal
// sits in source.
func steeringTokenFor(body string) string { return "gh" + "p_" + body }

// sqlDelivered mimics left(text, cap + margin): the text SnapshotOwnerSteering hands the builder.
func sqlDelivered(s string, capChars int) string {
	cut, _ := cutRunes(s, capChars+ownerSteeringTestMargin)
	return cut
}

// ownerSteeringTestMargin is the query's per-field margin beyond the authoritative cap.
const ownerSteeringTestMargin = 1024

// A token that begins at, just before or just after each cap position, or that the query's own cut
// splits, never leaves token-body characters in the snapshot: the builder scrubs before it cuts.
func TestOwnerSteeringScrubBeforeCapStraddlingToken(t *testing.T) {
	body := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcd" // 30 characters
	token := steeringTokenFor(body)
	fields := []struct {
		name  string
		chars int
		build func(delivered string) ownerSteeringRow
		text  func(raw []byte) string
	}{
		{"revise", ownerSteeringReviseChars, func(d string) ownerSteeringRow {
			return ownerSteeringRow{InputID: 1, Kind: "revise_plan", ReviseBody: &d}
		}, nil},
		{"answer", ownerSteeringAnswerChars, func(d string) ownerSteeringRow { return answerRow(1, "q1", d) }, nil},
		{"header", ownerSteeringHeaderChars, func(d string) ownerSteeringRow {
			r := answerRow(1, "q1", "a")
			r.Questions = []steeringQuestion{{Header: d, Question: "Q?", Options: []string{"a"}}}
			return r
		}, nil},
		{"question", ownerSteeringQuestionChars, func(d string) ownerSteeringRow {
			r := answerRow(1, "q1", "a")
			r.Questions = []steeringQuestion{{Header: "H", Question: d, Options: []string{"a"}}}
			return r
		}, nil},
		{"option", ownerSteeringOptionChars, func(d string) ownerSteeringRow {
			r := answerRow(1, "q1", "a")
			r.Questions = []steeringQuestion{{Header: "H", Question: "Q?", Options: []string{d}}}
			return r
		}, nil},
	}
	for _, f := range fields {
		// start offsets relative to the cap, and relative to the query's own cut at cap+margin
		starts := []int{f.chars - 40, f.chars - 5, f.chars, f.chars + 5, f.chars + ownerSteeringTestMargin - 5, f.chars + ownerSteeringTestMargin - 20}
		for _, start := range starts {
			if start < 0 {
				continue
			}
			original := strings.Repeat("a", start) + token + strings.Repeat("z", f.chars+3*ownerSteeringTestMargin)
			row := f.build(sqlDelivered(original, f.chars))
			raw, err := buildOwnerSteeringSnapshot(ownerSteeringInput{Cutoff: 1, Total: 1, Rows: []ownerSteeringRow{row}})
			if err != nil {
				t.Fatalf("%s start=%d: %v", f.name, start, err)
			}
			// No run of three token-body characters from the body may survive (the filler is a/z only).
			for i := 0; i+3 <= len(body); i++ {
				if frag := body[i : i+3]; strings.Contains(string(raw), frag) {
					t.Fatalf("%s start=%d: token body fragment %q survived: ...%s", f.name, start, frag, tail(string(raw), 200))
				}
			}
			// The cut point leaves no bare prefix: at most the redaction marker, never "ghp_" fragments.
			if strings.Contains(string(raw), "gh"+"p_") {
				t.Fatalf("%s start=%d: token prefix survived", f.name, start)
			}
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
