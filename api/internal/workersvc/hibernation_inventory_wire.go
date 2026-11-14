package workersvc

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Worker-side wire of the pre-hibernation volume inventory (PRD #4208 M1). The shapes are
// pinned by fixtures/hibernation-inventory/wire.json, which the api tests and the agent client
// tests both consume. Every request is decoded strictly, so a field the contract does not name
// is a 400 rather than silently dropped.

const (
	// InventoryMaxEntries bounds the entries a worker may report.
	InventoryMaxEntries = 10000
	// inventoryEntriesMaxBytes bounds the canonical (compact) entries JSON. The table's CHECK bounds
	// the PRINTED jsonb form, which is larger, so a 23514 from the database is also a client error.
	inventoryEntriesMaxBytes = 1 << 20
	inventoryDockerMax       = 10000
	inventoryPathMax         = 4096
	inventoryDetailMax       = 1024
	inventoryTokenMax        = 128
	inventoryStatusReasonMax = 2048
	hibernationPodSetMax     = 64
	hibernationPVCNameMax    = 253
	hibernationPVCUIDMax     = 128
)

// Inventory row statuses (worker_hibernation_inventories.status).
const (
	InventoryAwaiting          = "awaiting"
	InventoryAccepted          = "accepted"
	InventoryUnsupported       = "unsupported"
	InventoryExclusionFailed   = "exclusion_failed"
	InventoryBusy              = "busy"
	InventoryTimedOut          = "timed_out"
	InventoryNoCapability      = "no_capability"
	InventoryClosedByScaleDown = "closed_by_scale_down"
	InventoryIdentityChanged   = "identity_changed"
)

// Inventory invalidation reasons (worker_hibernation_inventories.invalidated_reason).
const (
	InvalidatedOtherPod              = "other_pod"
	InvalidatedPodSetUnproven        = "pod_set_unproven"
	InvalidatedDeploymentChanged     = "deployment_changed"
	InvalidatedObservationGap        = "observation_gap"
	InvalidatedProcessRestarted      = "process_restarted"
	InvalidatedExclusionLost         = "exclusion_lost"
	InvalidatedWrittenAfterInventory = "written_after_inventory"
	InvalidatedScaleDownPodUnproven  = "scale_down_pod_unproven"
)

var (
	hex64Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tipHexRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// HibernationPVCRef is the identity of one volume as the worker echoes it: the only three fields
// the accept path compares.
type HibernationPVCRef struct {
	Role string `json:"role"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// InventoryEntry is one classified on-volume item. Optional members are pointers so an explicit
// empty string is preserved rather than dropped, and the marshalled form is the canonical stored one.
type InventoryEntry struct {
	Kind           string  `json:"kind"`
	Path           string  `json:"path"`
	Classification string  `json:"classification"`
	RunID          *string `json:"run_id,omitempty"`
	Tip            *string `json:"tip,omitempty"`
	ContentState   *string `json:"content_state,omitempty"`
	Detail         *string `json:"detail,omitempty"`
}

// InventoryDocker is the worker's docker daemon census. It is validated and bound by the
// worker's digest but not persisted: the table has no column for it.
type InventoryDocker struct {
	Containers      []InventoryDockerContainer `json:"containers"`
	Volumes         []InventoryDockerVolume    `json:"volumes"`
	ImagesCount     *int64                     `json:"images_count"`
	BuildCacheBytes *int64                     `json:"build_cache_bytes"`
}

// InventoryDockerContainer is one container of the census.
type InventoryDockerContainer struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Name  string `json:"name"`
}

// InventoryDockerVolume is one docker volume of the census.
type InventoryDockerVolume struct {
	Name      string `json:"name"`
	Anonymous *bool  `json:"anonymous"`
}

// InventoryExclusion is the writer-exclusion evidence the worker took the inventory under.
type InventoryExclusion struct {
	Lane      string   `json:"lane"`
	Mechanism string   `json:"mechanism"`
	Uncovered []string `json:"uncovered"`
}

// InventoryRequest is the body of POST /api/worker/hibernation/inventory: either the full
// inventory (Status empty) or a status report (Status set, nothing else beyond the identity).
type InventoryRequest struct {
	OpID                      string              `json:"op_id"`
	RegisterNonce             string              `json:"register_nonce"`
	PodUID                    string              `json:"pod_uid"`
	DeploymentResourceVersion string              `json:"deployment_resource_version"`
	PVCs                      []HibernationPVCRef `json:"pvcs"`
	Complete                  *bool               `json:"complete"`
	Entries                   []InventoryEntry    `json:"entries"`
	Fingerprint               string              `json:"fingerprint"`
	Digest                    string              `json:"digest"`
	Docker                    *InventoryDocker    `json:"docker"`
	Exclusion                 *InventoryExclusion `json:"exclusion"`
	Status                    string              `json:"status"`
	Reason                    string              `json:"reason"`
}

// HibernationInventoryReport is a validated, canonicalised inventory ready for the accept write.
type HibernationInventoryReport struct {
	OpID                      uuid.UUID
	PodUID                    uuid.UUID
	RegisterNonce             string
	DeploymentResourceVersion string
	PVCs                      []HibernationPVCRef
	Complete                  bool
	Entries                   []byte
	ReceiptKeys               []byte
	Digest                    string
	Fingerprint               string
	Lane                      string
	Mechanism                 string
}

// HibernationInventoryStatusReport is a validated worker-reported non-inventory status.
type HibernationInventoryStatusReport struct {
	OpID          uuid.UUID
	PodUID        uuid.UUID
	RegisterNonce string
	Status        string
	Reason        string
}

// ExclusionFinalRequest is the body of POST /api/worker/hibernation/exclusion-final.
type ExclusionFinalRequest struct {
	OpID          string `json:"op_id"`
	PodUID        string `json:"pod_uid"`
	RegisterNonce string `json:"register_nonce"`
	Held          *bool  `json:"held"`
	Digest        string `json:"digest"`
	Fingerprint   string `json:"fingerprint"`
}

// HibernationExclusionFinal is a validated exclusion-final report.
type HibernationExclusionFinal struct {
	OpID          uuid.UUID
	PodUID        uuid.UUID
	RegisterNonce string
	Held          bool
	Digest        string
	Fingerprint   string
}

// HibernationExclusionReport is the heartbeat's optional hibernation_exclusion member.
type HibernationExclusionReport struct {
	OpID string `json:"op_id"`
	Held *bool  `json:"held"`
}

// HibernationExclusion is a validated hibernation_exclusion member.
type HibernationExclusion struct {
	OpID uuid.UUID
	Held bool
}

// HibernationInventoryMember is the heartbeat response's hibernation_inventory member: the
// identity the worker must echo when it reports the inventory (phase awaiting) or keep holding
// exclusion for (phase accepted).
type HibernationInventoryMember struct {
	OpID                      string              `json:"op_id"`
	Phase                     string              `json:"phase"`
	RegisterNonce             string              `json:"register_nonce"`
	PodUID                    string              `json:"pod_uid"`
	DeploymentResourceVersion string              `json:"deployment_resource_version"`
	PVCs                      []HibernationPVCRef `json:"pvcs"`
}

func inventoryBad(format string, a ...any) error {
	return &HibernationValidationError{Msg: fmt.Sprintf(format, a...)}
}

// cleanInventoryText bounds s and rejects what jsonb cannot store (NUL) or that is not valid text.
func cleanInventoryText(field, s string, max int, required bool) error {
	switch {
	case required && s == "":
		return inventoryBad("%s is required", field)
	case len(s) > max:
		return inventoryBad("%s must be at most %d bytes", field, max)
	case strings.ContainsRune(s, 0) || !utf8.ValidString(s):
		return inventoryBad("%s must be valid text", field)
	}
	return nil
}

func parseInventoryUUID(field, s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, inventoryBad("%s is required", field)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, inventoryBad("%s must be a uuid", field)
	}
	return id, nil
}

// validatePVCRefs checks 1..3 unique roles with bounded non-empty name and uid.
func validatePVCRefs(pvcs []HibernationPVCRef) error {
	if len(pvcs) < 1 || len(pvcs) > 3 {
		return inventoryBad("pvcs must have 1 to 3 entries")
	}
	seen := map[string]bool{}
	for _, p := range pvcs {
		if !validPVCRole(p.Role) {
			return inventoryBad("pvc role must be data, nix or dind_data")
		}
		if seen[p.Role] {
			return inventoryBad("pvc role %s is repeated", p.Role)
		}
		seen[p.Role] = true
		if err := cleanInventoryText("pvc name", p.Name, hibernationPVCNameMax, true); err != nil {
			return err
		}
		if err := cleanInventoryText("pvc uid", p.UID, hibernationPVCUIDMax, true); err != nil {
			return err
		}
	}
	return nil
}

func validPVCRole(r string) bool { return r == "data" || r == "nix" || r == "dind_data" }

// pvcRoleRank is the canonical order of the volumes: the order the wire fixture lists them in.
func pvcRoleRank(role string) int {
	switch role {
	case "data":
		return 0
	case "nix":
		return 1
	}
	return 2
}

// sortedPVCRefs returns in in canonical role order (data, nix, dind_data), so the stored form, the
// comparison against a worker's echo and the heartbeat member all agree on one order.
func sortedPVCRefs(in []HibernationPVCRef) []HibernationPVCRef {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b HibernationPVCRef) int { return cmp.Compare(pvcRoleRank(a.Role), pvcRoleRank(b.Role)) })
	return out
}

// IsStatus reports whether the body is a status report rather than an inventory.
func (r *InventoryRequest) IsStatus() bool { return r.Status != "" }

func (r *InventoryRequest) validateIdentity() (op, pod uuid.UUID, err error) {
	if op, err = parseInventoryUUID("op_id", r.OpID); err != nil {
		return
	}
	if pod, err = parseInventoryUUID("pod_uid", r.PodUID); err != nil {
		return
	}
	if err = cleanInventoryText("register_nonce", r.RegisterNonce, inventoryTokenMax, true); err != nil {
		return
	}
	return
}

// PrepareStatus validates a status body: the three statuses a worker may report, and nothing
// from the inventory form.
func (r *InventoryRequest) PrepareStatus() (HibernationInventoryStatusReport, error) {
	var out HibernationInventoryStatusReport
	op, pod, err := r.validateIdentity()
	if err != nil {
		return out, err
	}
	switch r.Status {
	case InventoryUnsupported, InventoryExclusionFailed, InventoryBusy:
	default:
		return out, inventoryBad("status must be unsupported, exclusion_failed or busy")
	}
	if r.DeploymentResourceVersion != "" || r.PVCs != nil || r.Complete != nil || r.Entries != nil ||
		r.Fingerprint != "" || r.Digest != "" || r.Docker != nil || r.Exclusion != nil {
		return out, inventoryBad("a status report carries no inventory")
	}
	if err := cleanInventoryText("reason", r.Reason, inventoryStatusReasonMax, false); err != nil {
		return out, err
	}
	reason, _ := sanitizeHibernationText(r.Reason)
	return HibernationInventoryStatusReport{OpID: op, PodUID: pod, RegisterNonce: r.RegisterNonce,
		Status: r.Status, Reason: truncateRunes(reason, hibernationTextMax)}, nil
}

// PrepareInventory validates a full inventory body and computes what the accept write stores:
// the canonical entries JSON and the sorted unique (run_id, tip) receipt keys. All of it runs
// before the transaction, so a locked worker row never waits on this work.
func (r *InventoryRequest) PrepareInventory() (HibernationInventoryReport, error) {
	var out HibernationInventoryReport
	op, pod, err := r.validateIdentity()
	if err != nil {
		return out, err
	}
	if r.Reason != "" {
		return out, inventoryBad("reason is only valid on a status report")
	}
	if err := cleanInventoryText("deployment_resource_version", r.DeploymentResourceVersion, hibernationResourceVersionMax, true); err != nil {
		return out, err
	}
	if err := validatePVCRefs(r.PVCs); err != nil {
		return out, err
	}
	if r.Complete == nil {
		return out, inventoryBad("complete is required")
	}
	if r.Entries == nil {
		return out, inventoryBad("entries is required")
	}
	if len(r.Entries) > InventoryMaxEntries {
		return out, inventoryBad("entries must have at most %d items", InventoryMaxEntries)
	}
	if !hex64Re.MatchString(r.Digest) {
		return out, inventoryBad("digest must be 64 lowercase hex characters")
	}
	if !hex64Re.MatchString(r.Fingerprint) {
		return out, inventoryBad("fingerprint must be 64 lowercase hex characters")
	}
	keys := map[[2]string]bool{}
	for i := range r.Entries {
		e := &r.Entries[i]
		if err := validateInventoryEntry(e); err != nil {
			return out, err
		}
		if e.RunID != nil && e.Tip != nil {
			keys[[2]string{strings.ToLower(*e.RunID), *e.Tip}] = true
		}
	}
	if err := validateInventoryDocker(r.Docker); err != nil {
		return out, err
	}
	x := r.Exclusion
	if x == nil {
		return out, inventoryBad("exclusion is required")
	}
	if err := cleanInventoryText("exclusion.lane", x.Lane, 64, true); err != nil {
		return out, err
	}
	if err := cleanInventoryText("exclusion.mechanism", x.Mechanism, 128, true); err != nil {
		return out, err
	}
	if len(x.Uncovered) > 64 {
		return out, inventoryBad("exclusion.uncovered must have at most 64 items")
	}
	for _, u := range x.Uncovered {
		if err := cleanInventoryText("exclusion.uncovered", u, 128, true); err != nil {
			return out, err
		}
	}
	entries, err := json.Marshal(r.Entries)
	if err != nil {
		return out, err
	}
	if len(entries) > inventoryEntriesMaxBytes {
		return out, inventoryBad("entries must be at most %d bytes", inventoryEntriesMaxBytes)
	}
	type receiptKey struct {
		RunID string `json:"run_id"`
		Tip   string `json:"tip"`
	}
	sorted := make([]receiptKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, receiptKey{RunID: k[0], Tip: k[1]})
	}
	slices.SortFunc(sorted, func(a, b receiptKey) int {
		return cmp.Or(cmp.Compare(a.RunID, b.RunID), cmp.Compare(a.Tip, b.Tip))
	})
	receipts, err := json.Marshal(sorted)
	if err != nil {
		return out, err
	}
	return HibernationInventoryReport{
		OpID: op, PodUID: pod, RegisterNonce: r.RegisterNonce,
		DeploymentResourceVersion: r.DeploymentResourceVersion, PVCs: sortedPVCRefs(r.PVCs),
		Complete: *r.Complete, Entries: entries, ReceiptKeys: receipts,
		Digest: r.Digest, Fingerprint: r.Fingerprint, Lane: x.Lane, Mechanism: x.Mechanism,
	}, nil
}

func validateInventoryEntry(e *InventoryEntry) error {
	if err := cleanInventoryText("entry kind", e.Kind, 64, true); err != nil {
		return err
	}
	if err := cleanInventoryText("entry path", e.Path, inventoryPathMax, true); err != nil {
		return err
	}
	if err := cleanInventoryText("entry classification", e.Classification, 64, true); err != nil {
		return err
	}
	if e.RunID != nil {
		id, err := parseInventoryUUID("entry run_id", *e.RunID)
		if err != nil {
			return err
		}
		canonical := id.String()
		e.RunID = &canonical
	}
	if e.Tip != nil && !tipHexRe.MatchString(*e.Tip) {
		return inventoryBad("entry tip must be 40 or 64 lowercase hex characters")
	}
	if e.ContentState != nil {
		if err := cleanInventoryText("entry content_state", *e.ContentState, 64, true); err != nil {
			return err
		}
	}
	if e.Detail != nil {
		if err := cleanInventoryText("entry detail", *e.Detail, inventoryDetailMax, false); err != nil {
			return err
		}
	}
	return nil
}

func validateInventoryDocker(d *InventoryDocker) error {
	if d == nil {
		return nil
	}
	if len(d.Containers) > inventoryDockerMax || len(d.Volumes) > inventoryDockerMax {
		return inventoryBad("docker lists must have at most %d items", inventoryDockerMax)
	}
	for _, c := range d.Containers {
		if err := cleanInventoryText("docker container id", c.ID, 256, true); err != nil {
			return err
		}
		if err := cleanInventoryText("docker container state", c.State, 64, true); err != nil {
			return err
		}
		if err := cleanInventoryText("docker container name", c.Name, 256, false); err != nil {
			return err
		}
	}
	for _, v := range d.Volumes {
		if err := cleanInventoryText("docker volume name", v.Name, 256, true); err != nil {
			return err
		}
		if v.Anonymous == nil {
			return inventoryBad("docker volume anonymous is required")
		}
	}
	if d.ImagesCount == nil || *d.ImagesCount < 0 || d.BuildCacheBytes == nil || *d.BuildCacheBytes < 0 {
		return inventoryBad("docker images_count and build_cache_bytes are required and non-negative")
	}
	return nil
}

// Prepare validates an exclusion-final body.
func (r *ExclusionFinalRequest) Prepare() (HibernationExclusionFinal, error) {
	var out HibernationExclusionFinal
	op, err := parseInventoryUUID("op_id", r.OpID)
	if err != nil {
		return out, err
	}
	pod, err := parseInventoryUUID("pod_uid", r.PodUID)
	if err != nil {
		return out, err
	}
	if err := cleanInventoryText("register_nonce", r.RegisterNonce, inventoryTokenMax, true); err != nil {
		return out, err
	}
	if r.Held == nil {
		return out, inventoryBad("held is required")
	}
	if !hex64Re.MatchString(r.Digest) || !hex64Re.MatchString(r.Fingerprint) {
		return out, inventoryBad("digest and fingerprint must be 64 lowercase hex characters")
	}
	return HibernationExclusionFinal{OpID: op, PodUID: pod, RegisterNonce: r.RegisterNonce, Held: *r.Held,
		Digest: r.Digest, Fingerprint: r.Fingerprint}, nil
}

// Parse validates the heartbeat's hibernation_exclusion member.
func (r *HibernationExclusionReport) Parse() (*HibernationExclusion, error) {
	if r == nil {
		return nil, errors.New("nil hibernation_exclusion")
	}
	op, err := parseInventoryUUID("hibernation_exclusion.op_id", r.OpID)
	if err != nil {
		return nil, err
	}
	if r.Held == nil {
		return nil, inventoryBad("hibernation_exclusion.held is required")
	}
	return &HibernationExclusion{OpID: op, Held: *r.Held}, nil
}

// marshalPVCs is the canonical stored form of a transition's volumes.
func marshalPVCs(pvcs []HibernationPVC) ([]byte, error) {
	type stored struct {
		Role               string `json:"role"`
		Name               string `json:"name"`
		UID                string `json:"uid"`
		StorageClass       string `json:"storage_class,omitempty"`
		ClassReclaimPolicy string `json:"class_reclaim_policy"`
	}
	out := make([]stored, 0, len(pvcs))
	for _, p := range pvcs {
		out = append(out, stored(p))
	}
	return json.Marshal(out)
}

// storedPVCRefs projects the stored pvcs JSON onto the identity the worker echoes.
func storedPVCRefs(raw []byte) ([]HibernationPVCRef, error) {
	var refs []HibernationPVCRef
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&refs); err != nil {
		return nil, err
	}
	return sortedPVCRefs(refs), nil
}

// validateObservationFields validates the PRD #4208 M1 transition fields, whichever transition
// carries them. `observed` additionally requires the volumes.
func validateObservationFields(t *HibernationTransition) error {
	if t.Transition == TransitionObserved || len(t.PVCs) > 0 {
		refs := make([]HibernationPVCRef, 0, len(t.PVCs))
		for _, p := range t.PVCs {
			refs = append(refs, HibernationPVCRef{Role: p.Role, Name: p.Name, UID: p.UID})
			switch p.ClassReclaimPolicy {
			case "Retain", "Delete", "Recycle", "unknown":
			default:
				return inventoryBad("class_reclaim_policy must be Retain, Delete, Recycle or unknown")
			}
			if err := cleanInventoryText("storage_class", p.StorageClass, hibernationPVCNameMax, false); err != nil {
				return err
			}
		}
		if err := validatePVCRefs(refs); err != nil {
			return err
		}
	}
	for _, set := range []struct {
		name string
		v    *[]string
	}{{"pod_uids", t.PodUIDs}, {"pvc_user_pods", t.PVCUserPods}} {
		if set.v == nil {
			continue
		}
		if len(*set.v) > hibernationPodSetMax {
			return inventoryBad("%s must have at most %d entries", set.name, hibernationPodSetMax)
		}
		for _, u := range *set.v {
			if _, err := uuid.Parse(u); err != nil {
				return inventoryBad("%s entries must be uuids", set.name)
			}
		}
	}
	return nil
}
