package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The shared worker wire contract: the agent client tests consume the same file.
const hibernationInventoryFixture = "../../../fixtures/hibernation-inventory/wire.json"

func loadInventoryFixture(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(hibernationInventoryFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return m
}

func invStrictDecode(t *testing.T, raw []byte, dst any) error {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func isValidationError(err error) bool {
	var v *HibernationValidationError
	return errors.As(err, &v)
}

func TestInventoryFixtureRequestsDecodeAndPrepare(t *testing.T) {
	fx := loadInventoryFixture(t)
	for _, name := range []string{"inventory_request", "inventory_request_no_docker"} {
		t.Run(name, func(t *testing.T) {
			var req InventoryRequest
			if err := invStrictDecode(t, fx[name], &req); err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			if req.IsStatus() {
				t.Fatal("inventory body read as a status report")
			}
			rep, err := req.PrepareInventory()
			if err != nil {
				t.Fatalf("PrepareInventory: %v", err)
			}
			if rep.OpID.String() != "55555555-5555-5555-5555-555555555555" || rep.PodUID.String() != "66666666-6666-6666-6666-666666666666" {
				t.Fatalf("identity not carried: %+v", rep)
			}
			// The nonce is whatever bounded token the fixture carries (the api mints a 43-character base64url value).
			if rep.RegisterNonce != req.RegisterNonce || rep.RegisterNonce == "" || len(rep.RegisterNonce) > inventoryTokenMax || rep.DeploymentResourceVersion != "12340" {
				t.Fatalf("nonce / resource version not carried: %+v", rep)
			}
			var entries []map[string]any
			if err := json.Unmarshal(rep.Entries, &entries); err != nil {
				t.Fatalf("entries are not canonical JSON: %v", err)
			}
			if name == "inventory_request" {
				if len(rep.PVCs) != 3 || len(entries) != 3 || rep.Lane != "dind" || rep.Mechanism != "epoch_ex+latch+quiescence" {
					t.Fatalf("full inventory not carried: pvcs=%d entries=%d lane=%q", len(rep.PVCs), len(entries), rep.Lane)
				}
				want := `[{"run_id":"99999999-9999-4999-8999-999999999991","tip":"0123456789abcdef0123456789abcdef01234567"}]`
				if string(rep.ReceiptKeys) != want {
					t.Fatalf("receipt keys = %s, want %s", rep.ReceiptKeys, want)
				}
			} else if len(rep.PVCs) != 2 || len(entries) != 0 || rep.Lane != "plain" || string(rep.ReceiptKeys) != "[]" {
				t.Fatalf("no-docker inventory not carried: %+v", rep)
			}
		})
	}
}

func TestInventoryFixtureStatusRequestsDecode(t *testing.T) {
	var reqs []json.RawMessage
	if err := json.Unmarshal(loadInventoryFixture(t)["inventory_status_requests"], &reqs); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, raw := range reqs {
		var req InventoryRequest
		if err := invStrictDecode(t, raw, &req); err != nil {
			t.Fatalf("strict decode: %v", err)
		}
		if !req.IsStatus() {
			t.Fatal("status body not read as a status report")
		}
		rep, err := req.PrepareStatus()
		if err != nil {
			t.Fatalf("PrepareStatus: %v", err)
		}
		if rep.Reason == "" {
			t.Fatal("reason dropped")
		}
		got = append(got, rep.Status)
	}
	if !reflect.DeepEqual(got, []string{"unsupported", "exclusion_failed", "busy"}) {
		t.Fatalf("statuses = %v", got)
	}
}

func TestInventoryFixtureExclusionFinalAndHeartbeatMembers(t *testing.T) {
	fx := loadInventoryFixture(t)
	var final ExclusionFinalRequest
	if err := invStrictDecode(t, fx["exclusion_final_request"], &final); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	rep, err := final.Prepare()
	if err != nil || !rep.Held || rep.Digest[0] != 'b' || rep.Fingerprint[0] != 'a' {
		t.Fatalf("exclusion final = %+v, %v", rep, err)
	}

	var hb struct {
		Exclusion *HibernationExclusionReport `json:"hibernation_exclusion"`
	}
	if err := invStrictDecode(t, fx["heartbeat_request_member"], &hb); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	ex, err := hb.Exclusion.Parse()
	if err != nil || !ex.Held || ex.OpID.String() != "55555555-5555-5555-5555-555555555555" {
		t.Fatalf("exclusion = %+v, %v", ex, err)
	}

	// The response member must round-trip byte-compatibly: decode it strictly into the type the
	// api emits, re-marshal, and compare as JSON values.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(fx["heartbeat_response_member"], &members); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"awaiting", "accepted"} {
		var wrapper struct {
			Inventory HibernationInventoryMember `json:"hibernation_inventory"`
		}
		if err := invStrictDecode(t, members[phase], &wrapper); err != nil {
			t.Fatalf("%s: strict decode: %v", phase, err)
		}
		if wrapper.Inventory.Phase != phase {
			t.Fatalf("phase = %q, want %q", wrapper.Inventory.Phase, phase)
		}
		out, err := json.Marshal(map[string]any{"hibernation_inventory": wrapper.Inventory})
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		_ = json.Unmarshal(members[phase], &want)
		_ = json.Unmarshal(out, &got)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s member does not round-trip:\n got %s\nwant %s", phase, out, members[phase])
		}
	}
}

func TestInventoryRequestRejections(t *testing.T) {
	fx := loadInventoryFixture(t)
	decode := func(t *testing.T, mutate func(m map[string]any)) (InventoryRequest, error) {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(fx["inventory_request"], &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, _ := json.Marshal(m)
		var req InventoryRequest
		return req, invStrictDecode(t, raw, &req)
	}
	cases := map[string]func(m map[string]any){
		"bad op uuid":        func(m map[string]any) { m["op_id"] = "nope" },
		"empty nonce":        func(m map[string]any) { m["register_nonce"] = "" },
		"short digest":       func(m map[string]any) { m["digest"] = "abc" },
		"uppercase digest":   func(m map[string]any) { m["digest"] = strings.Repeat("A", 64) },
		"short fingerprint":  func(m map[string]any) { m["fingerprint"] = "abc" },
		"no pvcs":            func(m map[string]any) { m["pvcs"] = []any{} },
		"four pvcs":          func(m map[string]any) { m["pvcs"] = append(m["pvcs"].([]any), m["pvcs"].([]any)[0]) },
		"unknown pvc role":   func(m map[string]any) { m["pvcs"].([]any)[0].(map[string]any)["role"] = "scratch" },
		"duplicate pvc role": func(m map[string]any) { m["pvcs"].([]any)[1].(map[string]any)["role"] = "data" },
		"empty pvc uid":      func(m map[string]any) { m["pvcs"].([]any)[0].(map[string]any)["uid"] = "" },
		"missing complete":   func(m map[string]any) { delete(m, "complete") },
		"null entries":       func(m map[string]any) { m["entries"] = nil },
		"missing exclusion":  func(m map[string]any) { delete(m, "exclusion") },
		"bad tip":            func(m map[string]any) { m["entries"].([]any)[0].(map[string]any)["tip"] = "xyz" },
		"bad entry run id":   func(m map[string]any) { m["entries"].([]any)[0].(map[string]any)["run_id"] = "xyz" },
		"nul in path":        func(m map[string]any) { m["entries"].([]any)[0].(map[string]any)["path"] = "a\x00b" },
		"empty entry kind":   func(m map[string]any) { m["entries"].([]any)[0].(map[string]any)["kind"] = "" },
		"reason on inventory": func(m map[string]any) {
			m["reason"] = "x"
		},
		"docker volume without anonymous": func(m map[string]any) {
			m["docker"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["anonymous"] = nil
		},
		"too many entries": func(m map[string]any) {
			e := m["entries"].([]any)[2]
			big := make([]any, InventoryMaxEntries+1)
			for i := range big {
				big[i] = e
			}
			m["entries"] = big
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := decode(t, mutate)
			if err == nil {
				_, err = req.PrepareInventory()
			}
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("unknown top-level field", func(t *testing.T) {
		if _, err := decode(t, func(m map[string]any) { m["extra"] = 1 }); err == nil {
			t.Fatal("unknown field accepted")
		}
	})
	t.Run("status with inventory members", func(t *testing.T) {
		req := InventoryRequest{OpID: uuid.NewString(), PodUID: uuid.NewString(), RegisterNonce: "n", Status: "busy", Digest: strings.Repeat("a", 64)}
		if _, err := req.PrepareStatus(); !isValidationError(err) {
			t.Fatalf("status carrying an inventory: %v", err)
		}
		req = InventoryRequest{OpID: uuid.NewString(), PodUID: uuid.NewString(), RegisterNonce: "n", Status: "accepted"}
		if _, err := req.PrepareStatus(); !isValidationError(err) {
			t.Fatalf("status accepted by a worker: %v", err)
		}
	})
}

func TestExclusionFinalAndHeartbeatMemberRejections(t *testing.T) {
	yes := true
	base := ExclusionFinalRequest{OpID: uuid.NewString(), PodUID: uuid.NewString(), RegisterNonce: "n", Held: &yes,
		Digest: strings.Repeat("a", 64), Fingerprint: strings.Repeat("b", 64)}
	for name, mutate := range map[string]func(r *ExclusionFinalRequest){
		"held absent":     func(r *ExclusionFinalRequest) { r.Held = nil },
		"digest short":    func(r *ExclusionFinalRequest) { r.Digest = "a" },
		"bad pod":         func(r *ExclusionFinalRequest) { r.PodUID = "x" },
		"empty nonce":     func(r *ExclusionFinalRequest) { r.RegisterNonce = "" },
		"fingerprint bad": func(r *ExclusionFinalRequest) { r.Fingerprint = strings.Repeat("g", 64) },
	} {
		r := base
		mutate(&r)
		if _, err := r.Prepare(); !isValidationError(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := (&HibernationExclusionReport{OpID: "x", Held: &yes}).Parse(); !isValidationError(err) {
		t.Errorf("bad op: %v", err)
	}
	if _, err := (&HibernationExclusionReport{OpID: uuid.NewString()}).Parse(); !isValidationError(err) {
		t.Errorf("missing held: %v", err)
	}
}

func TestValidateObservedAndHeldTransitions(t *testing.T) {
	op, dep, pod := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pvc := func(role, uid string) HibernationPVC {
		return HibernationPVC{Role: role, Name: "n-" + role, UID: uid, ClassReclaimPolicy: "Retain"}
	}
	good := func() HibernationTransition {
		pods := []string{pod}
		return HibernationTransition{OpID: op, Transition: TransitionObserved, DeploymentUID: dep, ResourceVersion: "5", PodUID: pod,
			PVCs: []HibernationPVC{pvc("data", "u1"), pvc("nix", "u2")}, PodUIDs: &pods, PVCUserPods: &pods}
	}
	tr := good()
	if _, _, err := validateHibernationTransition(&tr); err != nil {
		t.Fatalf("good observed: %v", err)
	}
	held := HibernationTransition{OpID: op, Transition: TransitionHeld, DeploymentUID: dep}
	if _, _, err := validateHibernationTransition(&held); err != nil {
		t.Fatalf("good held (no resource version, no pod): %v", err)
	}
	empty := []string{}
	for name, mutate := range map[string]func(t *HibernationTransition){
		"no pod uid":          func(t *HibernationTransition) { t.PodUID = "" },
		"no resource version": func(t *HibernationTransition) { t.ResourceVersion = "" },
		"four pvcs":           func(t *HibernationTransition) { t.PVCs = append(t.PVCs, pvc("dind_data", "u3"), pvc("data", "u4")) },
		"bad role":            func(t *HibernationTransition) { t.PVCs[0].Role = "x" },
		"dup role":            func(t *HibernationTransition) { t.PVCs[1].Role = "data" },
		"empty pvc name":      func(t *HibernationTransition) { t.PVCs[0].Name = "" },
		"empty pvc uid":       func(t *HibernationTransition) { t.PVCs[0].UID = "" },
		"bad reclaim policy":  func(t *HibernationTransition) { t.PVCs[0].ClassReclaimPolicy = "Keep" },
		"missing reclaim":     func(t *HibernationTransition) { t.PVCs[0].ClassReclaimPolicy = "" },
		"pod set not uuid":    func(t *HibernationTransition) { x := []string{"nope"}; t.PodUIDs = &x },
		"pvc pods not uuid":   func(t *HibernationTransition) { x := []string{"nope"}; t.PVCUserPods = &x },
	} {
		tr := good()
		mutate(&tr)
		if _, _, err := validateHibernationTransition(&tr); !isValidationError(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The inventory must never block a hibernation: an over-cap set is dropped (unobserved), and an
	// observed without volumes is valid (the service records it as unsupported).
	many := make([]string, hibernationPodSetMax+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	tr = good()
	tr.PodUIDs, tr.PVCUserPods = &many, &many
	if _, _, err := validateHibernationTransition(&tr); err != nil {
		t.Fatalf("over-cap pod sets: %v", err)
	}
	if tr.PodUIDs != nil || tr.PVCUserPods != nil {
		t.Fatalf("over-cap pod sets kept: %v %v", tr.PodUIDs, tr.PVCUserPods)
	}
	tr = good()
	tr.PVCs = nil
	if _, _, err := validateHibernationTransition(&tr); err != nil {
		t.Fatalf("observed without pvcs: %v", err)
	}
	// A known-empty pod set is valid and distinct from an absent one.
	tr = good()
	tr.PodUIDs, tr.PVCUserPods = &empty, &empty
	if _, _, err := validateHibernationTransition(&tr); err != nil {
		t.Fatalf("empty pod sets: %v", err)
	}
}

// Worker-written text is stored, so controls, DEL and bidi controls are refused, and the
// vocabulary fields are constrained.
func TestInventoryRejectsUnsafeTextAndVocabulary(t *testing.T) {
	fx := loadInventoryFixture(t)
	prepare := func(t *testing.T, mutate func(m map[string]any)) error {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(fx["inventory_request"], &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, _ := json.Marshal(m)
		var req InventoryRequest
		if err := invStrictDecode(t, raw, &req); err != nil {
			t.Fatal(err)
		}
		_, err := req.PrepareInventory()
		return err
	}
	entry := func(m map[string]any) map[string]any { return m["entries"].([]any)[0].(map[string]any) }
	excl := func(m map[string]any) map[string]any { return m["exclusion"].(map[string]any) }
	bad := []string{"a\x01b", "a\nb", "a\tb", "a\x7fb", "a\u0085b", "a\u009fb", "a\u202eb", "a\u2066b", "a\u2069b", "a\u200eb", "a\u200fb", "a\u061cb"}
	fields := map[string]func(m map[string]any, v string){
		"path":           func(m map[string]any, v string) { entry(m)["path"] = v },
		"kind":           func(m map[string]any, v string) { entry(m)["kind"] = v },
		"classification": func(m map[string]any, v string) { entry(m)["classification"] = v },
		"detail":         func(m map[string]any, v string) { entry(m)["detail"] = v },
		"content_state":  func(m map[string]any, v string) { entry(m)["content_state"] = v },
		"lane":           func(m map[string]any, v string) { excl(m)["lane"] = v },
		"mechanism":      func(m map[string]any, v string) { excl(m)["mechanism"] = v },
		"uncovered":      func(m map[string]any, v string) { excl(m)["uncovered"] = []any{v} },
	}
	for field, set := range fields {
		for _, v := range bad {
			if err := prepare(t, func(m map[string]any) { set(m, v) }); !isValidationError(err) {
				t.Errorf("%s %q: err = %v", field, v, err)
			}
		}
	}
	for name, mutate := range map[string]func(m map[string]any){
		"unknown lane":          func(m map[string]any) { excl(m)["lane"] = "sideways" },
		"unknown mechanism":     func(m map[string]any) { excl(m)["mechanism"] = "magic" },
		"upper-case kind":       func(m map[string]any) { entry(m)["kind"] = "Attempt" },
		"spaced classification": func(m map[string]any) { entry(m)["classification"] = "a b" },
		"slashed content state": func(m map[string]any) { entry(m)["content_state"] = "a/b" },
		"over-long kind":        func(m map[string]any) { entry(m)["kind"] = strings.Repeat("a", 65) },
		"too many uncovered":    func(m map[string]any) { excl(m)["uncovered"] = make([]any, 65) },
	} {
		if err := prepare(t, mutate); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Ordinary unicode text and every lane / mechanism the agent sends stay valid.
	if err := prepare(t, func(m map[string]any) {
		entry(m)["path"] = "/data/runner/h\u00e9llo \u4e16\u754c"
		excl(m)["lane"] = "isolated"
		excl(m)["mechanism"] = "none"
	}); err != nil {
		t.Fatalf("valid text rejected: %v", err)
	}
}

// An uncovered writer, or a lane other than dind, can never leave the inventory complete, and
// the reported uncovered list is kept.
func TestInventoryUncoveredForcesIncomplete(t *testing.T) {
	fx := loadInventoryFixture(t)
	prep := func(t *testing.T, lane string, uncovered []any) HibernationInventoryReport {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(fx["inventory_request"], &m); err != nil {
			t.Fatal(err)
		}
		m["exclusion"] = map[string]any{"lane": lane, "mechanism": "epoch_ex+latch+quiescence", "uncovered": uncovered}
		raw, _ := json.Marshal(m)
		var req InventoryRequest
		if err := invStrictDecode(t, raw, &req); err != nil {
			t.Fatal(err)
		}
		rep, err := req.PrepareInventory()
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := prep(t, "dind", []any{}); !rep.Complete || string(rep.Uncovered) != "[]" {
		t.Fatalf("covered dind: complete=%v uncovered=%s", rep.Complete, rep.Uncovered)
	}
	if rep := prep(t, "dind", []any{"x_writer"}); rep.Complete || string(rep.Uncovered) != `["x_writer"]` {
		t.Fatalf("uncovered dind: complete=%v uncovered=%s", rep.Complete, rep.Uncovered)
	}
	if rep := prep(t, "plain", []any{}); rep.Complete {
		t.Fatal("plain lane kept complete")
	}
}
