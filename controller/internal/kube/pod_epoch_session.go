package kube

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const PodEpochMaxIdentities = 4096
const PodEpochMaxMetadata = 16 * 1024 * 1024
const podEpochRequestTimeout = 10 * time.Second

// PodEpochGeneration is an opaque subscription token, required on every event.
// A caller must stop the old watch after Reset; replay with its token is refused.
type PodEpochGeneration uint64
type podEpochRecord struct {
	raw        []byte
	proof      []byte
	related    map[string]bool
	labels     map[string]uint64
	start      uint64
	terminal   uint64
	terminalAt time.Time
	addedAt    time.Time
	deleted    bool
	deletedAt  uint64
	tainted    bool
	identity   *PodEpochIdentity
	// pending is the frozen positive proof awaiting successor container IDs.
	pending     *PodEpochEvidence
	pendingCost int
}

func (s *PodEpochSession) dropPending(r *podEpochRecord) {
	s.bytes -= r.pendingCost
	r.pending = nil
	r.pendingCost = 0
}

type PodEpochDecision struct {
	generation               PodEpochGeneration
	session                  *PodEpochSession
	namespace, name, uid, rv string
	document                 string
	context                  context.Context
	discarded                bool
}

func (d *PodEpochDecision) Document() string { return d.document }

// PodEpochSession owns evidence only. No error here should gate reconciliation.
// Handle calls are serialized; Reset and maintenance receipts may run concurrently.
// Retained pod JSON and identity keys are charged before storage. Overflow drops
// proof state permanently until Reset, retaining only the generation fence.
type PodEpochSession struct {
	events         sync.Mutex
	mu             sync.Mutex
	resolver       PodEpochIdentityResolver
	client         kubernetes.Interface
	workers        []PodEpochWorker
	workerBytes    int
	invalidWorkers bool
	generation     PodEpochGeneration
	id             string
	started        time.Time
	seq            uint64
	records        map[string]*podEpochRecord
	current        map[string]string
	attempts       map[string]bool
	identities     map[string]bool
	relatedIDs     map[string]bool
	remints        map[string]PodEpochRemint
	bytes          int
	overflow       bool
}

func NewPodEpochSession(client kubernetes.Interface, resolver PodEpochIdentityResolver, workers []PodEpochWorker) *PodEpochSession {
	s := &PodEpochSession{client: client, resolver: resolver}
	for _, w := range workers {
		if err := s.RegisterWorker(w); err != nil {
			s.invalidWorkers = true
			s.overflow = true
			break
		}
	}
	return s
}

// CurrentGeneration binds maintenance receipts to the active observation.
func (s *PodEpochSession) CurrentGeneration() PodEpochGeneration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}
func podEpochWorkerID(id string) bool { return id != "" && len(id) <= 253 }

// RegisterWorker can be called before Handle for a newly discovered worker.
// Runtime declarations are immutable within a session. Registration never
// resolves old objects or fabricates a terminal observation.
func (s *PodEpochSession) RegisterWorker(w PodEpochWorker) error {
	s.events.Lock()
	defer s.events.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !podEpochWorkerID(w.ID) || (w.Runtime != "" && !podEpochRuntime(w.Runtime)) {
		return errors.New("invalid pod epoch worker")
	}
	for _, old := range s.workers {
		if old.ID == w.ID {
			if old != w {
				return errors.New("pod epoch worker declaration changed")
			}
			return nil
		}
	}
	cost := len(w.ID) + len(w.Runtime) + 128
	if s.overflow || len(s.workers) >= PodEpochMaxIdentities || s.bytes+cost > PodEpochMaxMetadata {
		return errors.New("pod epoch worker budget exceeded")
	}
	s.bytes += cost
	s.workerBytes += cost
	s.workers = append(s.workers, w)
	if s.current == nil {
		return nil
	}
	// Baseline observations share sequence zero, so ties have no authority.
	var chosen string
	var latest uint64
	ambiguous := false
	for uid, r := range s.records {
		n, ok := r.labels[w.ID]
		if !ok {
			continue
		}
		if chosen == "" || n > latest {
			chosen = uid
			latest = n
			ambiguous = false
		} else if n == latest {
			ambiguous = true
		}
	}
	if ambiguous {
		chosen = ""
	}
	s.current[w.ID] = chosen
	return nil
}

// Reset must receive the entire unfiltered baseline LIST. All baseline pods are
// predecessors only. The caller establishes a watch from the opaque LIST RV.
func (s *PodEpochSession) Reset(ctx context.Context, pods []corev1.Pod) (PodEpochGeneration, error) {
	s.mu.Lock()
	s.generation++
	g := s.generation
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		s.overflow = true
		s.mu.Unlock()
		return g, err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s.id = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	s.started = time.Now().UTC()
	s.seq = 0
	s.bytes = s.workerBytes
	s.overflow = s.invalidWorkers
	s.records = map[string]*podEpochRecord{}
	s.current = map[string]string{}
	s.attempts = map[string]bool{}
	s.relatedIDs = map[string]bool{}
	s.identities = map[string]bool{}
	s.remints = map[string]PodEpochRemint{}
	s.mu.Unlock()
	// The initial list has no event ordering. Multiple labeled baseline pods make
	// the immediate predecessor ambiguous, rather than using arbitrary list order.
	counts := map[string]int{}
	for i := range pods {
		if _, err := s.handle(ctx, g, watch.Added, &pods[i], true); err != nil {
			return g, err
		}

	}
	s.mu.Lock()
	if s.generation == g {
		for _, r := range s.records {
			for id := range r.labels {
				counts[id]++
			}
		}
		for id, n := range counts {
			if n > 1 {
				s.current[id] = ""
			}
		}
	}
	s.mu.Unlock()
	return g, nil
}
func (s *PodEpochSession) Handle(ctx context.Context, g PodEpochGeneration, event watch.EventType, p *corev1.Pod) (*PodEpochDecision, error) {
	return s.handle(ctx, g, event, p, false)
}
func (s *PodEpochSession) invalidate() {
	s.overflow = true
	s.records = map[string]*podEpochRecord{}
	s.current = map[string]string{}
	s.relatedIDs = map[string]bool{}
	s.identities = map[string]bool{}
	s.remints = map[string]PodEpochRemint{}
	// No further decisions are issued, so attempts need not grow after overflow.
	s.attempts = map[string]bool{}
	s.bytes = 0
}

// Worker relationship IDs have their own count cap; their bytes share the
// metadata budget with pod/claim identity keys and all retained record data.
func (s *PodEpochSession) chargeRelatedID(id string) bool {
	if s.relatedIDs[id] {
		return true
	}
	if len(s.relatedIDs) >= PodEpochMaxIdentities || s.bytes+len(id)+64 > PodEpochMaxMetadata {
		s.invalidate()
		return false
	}
	s.bytes += len(id) + 64
	s.relatedIDs[id] = true
	return true
}
func (s *PodEpochSession) chargeKey(key string) bool {
	if s.identities[key] {
		return true
	}
	if len(s.identities) >= PodEpochMaxIdentities || s.bytes+len(key) > PodEpochMaxMetadata {
		s.invalidate()
		return false
	}
	s.identities[key] = true
	s.bytes += len(key)
	return true
}

// podEpochPrivatePID reports the audited topology: init exit tears down a private PID
// namespace. Host PID or a shared pod PID namespace breaks that premise.
func podEpochPrivatePID(p *corev1.Pod) bool {
	return !p.Spec.HostPID && (p.Spec.ShareProcessNamespace == nil || !*p.Spec.ShareProcessNamespace)
}
func podEpochSignal(p *corev1.Pod) ([]PodEpochContainer, []PodEpochContainer, bool) {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return nil, nil, false
	}
	if p.Spec.RuntimeClassName != nil || len(p.Spec.EphemeralContainers) > 0 || len(p.Status.EphemeralContainerStatuses) > 0 || !podEpochPrivatePID(p) {
		return nil, nil, false
	}
	// Synthetic failures are not runtime termination observations.
	if p.Status.Reason != "" && p.Status.Reason != "Completed" && p.Status.Reason != "Error" {
		return nil, nil, false
	}
	extract := func(spec []corev1.Container, status []corev1.ContainerStatus) ([]PodEpochContainer, bool) {
		if len(spec) != len(status) {
			return nil, false
		}
		names := map[string]bool{}
		for _, c := range spec {
			if c.Name == "" || names[c.Name] {
				return nil, false
			}
			names[c.Name] = true
		}
		out := make([]PodEpochContainer, 0, len(status))
		for _, c := range status {
			t := c.State.Terminated
			if !names[c.Name] || t == nil || (t.Reason != "Completed" && t.Reason != "Error") || c.State.Running != nil || c.State.Waiting != nil {
				return nil, false
			}
			delete(names, c.Name)
			out = append(out, PodEpochContainer{Name: c.Name, ContainerID: c.ContainerID, ExitCode: t.ExitCode, RestartCount: c.RestartCount})
		}
		return out, podEpochValidEntries(out, false)
	}
	cs, ok := extract(p.Spec.Containers, p.Status.ContainerStatuses)
	if !ok || len(cs) == 0 {
		return nil, nil, false
	}
	init, ok := extract(p.Spec.InitContainers, p.Status.InitContainerStatuses)
	return cs, init, ok
}
func (s *PodEpochSession) handle(ctx context.Context, g PodEpochGeneration, event watch.EventType, p *corev1.Pod, baseline bool) (*PodEpochDecision, error) {
	s.events.Lock()
	defer s.events.Unlock()
	if p == nil || p.UID == "" || (event != watch.Added && event != watch.Modified && event != watch.Deleted) {
		return nil, errors.New("invalid pod event")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if g != s.generation || ctx.Err() != nil {
		s.mu.Unlock()
		return nil, errors.New("stale or canceled session")
	}
	if s.overflow {
		s.mu.Unlock()
		return nil, nil
	}
	uid := string(p.UID)
	old := s.records[uid]
	if old != nil && string(old.raw) == string(raw) && (event != watch.Deleted || old.deleted) {
		s.mu.Unlock()
		return nil, nil
	}
	s.seq++
	seq := s.seq
	if !s.chargeKey("pod:" + uid) {
		s.mu.Unlock()
		return nil, nil
	}
	delta := len(raw)
	if old != nil {
		delta -= len(old.raw)
	}
	if s.bytes+delta > PodEpochMaxMetadata {
		s.invalidate()
		s.mu.Unlock()
		return nil, nil
	}
	r := &podEpochRecord{raw: raw, start: seq, addedAt: time.Now().UTC(), related: map[string]bool{}, labels: map[string]uint64{}}
	if baseline {
		r.start = 0
	}
	if old == nil {
		delta += 512
	}
	if old != nil {
		*r = *old
		r.raw = raw
	}
	var prev corev1.Pod
	if old != nil {
		_ = json.Unmarshal(old.raw, &prev)
		if prev.Labels[LabelWorkerID] != p.Labels[LabelWorkerID] || !podEpochSameMounts(&prev, p) || old.deleted {
			r.tainted = true
		}
	}
	// Keep the union even for workers which have not been registered yet.
	// Process the incoming label and every naming interpretation of mounted claims;
	// the DinD suffix also ends in dataSuffix, so retain both possible IDs.
	remember := func(id string, label bool) bool {
		if id == "" {
			return true
		}
		if !podEpochWorkerID(id) || !s.chargeRelatedID(id) {
			s.invalidate()
			return false
		}
		cost := 0
		if !r.related[id] {
			cost += len(id) + 64
		}
		if label {
			if _, ok := r.labels[id]; !ok {
				cost += len(id) + 64
			}
		}
		if s.bytes+delta+cost > PodEpochMaxMetadata {
			s.invalidate()
			return false
		}
		delta += cost
		r.related[id] = true
		if label {
			n := seq
			if baseline {
				n = 0
			}
			r.labels[id] = n
		}
		return true
	}
	if !remember(p.Labels[LabelWorkerID], true) {
		s.mu.Unlock()
		return nil, nil
	}
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		if !strings.HasPrefix(name, NamePrefix) {
			continue
		}
		for _, suffix := range []string{dataSuffix, dindDataSuffix} {
			if strings.HasSuffix(name, suffix) {
				id := strings.TrimSuffix(strings.TrimPrefix(name, NamePrefix), suffix)
				if !remember(id, false) {
					s.mu.Unlock()
					return nil, nil
				}
			}
		}
	}
	cs, init, signal := podEpochSignal(p)
	if old != nil && old.terminal != 0 && signal && event != watch.Deleted {
		var previous corev1.Pod
		_ = json.Unmarshal(old.proof, &previous)
		beforeCS, beforeInit, _ := podEpochSignal(&previous)
		a, _ := json.Marshal([][]PodEpochContainer{beforeCS, beforeInit})
		b, _ := json.Marshal([][]PodEpochContainer{cs, init})
		if string(a) != string(b) {
			r.tainted = true
		}
	}
	if event == watch.Deleted {
		signal = false
	}
	if old == nil && !baseline && event != watch.Added {
		r.tainted = true
	}
	if old != nil && old.terminal != 0 && !signal && event != watch.Deleted {
		r.tainted = true
	}
	if signal && r.terminal == 0 {
		r.terminal = seq
		r.proof = raw
		delta += len(raw)
		r.terminalAt = time.Now().UTC()
	}
	if event == watch.Deleted {
		r.deleted = true
		r.deletedAt = seq
		if r.terminal == 0 {
			r.tainted = true
		}
	}
	if s.bytes+delta > PodEpochMaxMetadata {
		s.invalidate()
		s.mu.Unlock()
		return nil, nil
	}
	s.bytes += delta
	s.records[uid] = r
	var worker *PodEpochWorker
	for i := range s.workers {
		if p.Labels[LabelWorkerID] == s.workers[i].ID {
			worker = &s.workers[i]
			break
		}
	}
	if worker == nil {
		s.mu.Unlock()
		return nil, nil
	}
	w := *worker
	predecessor := s.current[w.ID]
	fresh := old == nil && event == watch.Added && !baseline
	// An unseen MODIFIED pod is an intervening epoch, never a successor.
	if old == nil && event != watch.Deleted {
		s.current[w.ID] = uid
	}
	if baseline {
		s.current[w.ID] = uid
	}
	needsIdentity := baseline || fresh || (signal && r.identity == nil)
	s.mu.Unlock()
	var identity PodEpochIdentity
	var identityErr error
	if needsIdentity {
		readCtx, cancel := context.WithTimeout(ctx, podEpochRequestTimeout)
		defer cancel()
		if s.resolver == nil {
			identityErr = errors.New("no live identity resolver")
		} else {
			identity, identityErr = s.resolver.ResolvePodEpoch(readCtx, w, p.DeepCopy(), fresh)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g != s.generation || ctx.Err() != nil {
		return nil, errors.New("stale or canceled resolution")
	}
	if s.overflow {
		return nil, nil
	}
	if needsIdentity && identityErr == nil {
		if !podEpochValidClaim(identity.Data) || !podEpochValidClaim(identity.DinD) {
			identityErr = errors.New("invalid live claims")
		} else {
			for _, c := range []PodEpochClaim{identity.Data, identity.DinD} {
				if !s.chargeKey("claim:" + c.UID) {
					return nil, nil
				}
			}
			r.identity = &identity
		}
	}
	if needsIdentity && identityErr != nil {
		r.tainted = true
	}
	if !fresh {
		if r.pending == nil {
			return nil, nil
		}
		if event == watch.Added || baseline {
			return nil, nil
		}
		// The Deleted and r.deleted clauses are defence in depth: a Deleted event
		// always leaves the record tainted or terminal, so they are not separately
		// testable. The terminal, tainted and current clauses are pinned by
		// TestPodEpochSuccessorNeverEmits. A successor in phase Succeeded or
		// Failed never emits, whether or not podEpochSignal audited a termination
		// signal (r.terminal): Evicted, incomplete statuses and exit 137 leave
		// r.terminal zero. Dropping the pending proof makes that permanent.
		if event == watch.Deleted || r.deleted || r.tainted || r.terminal != 0 ||
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed ||
			s.current[w.ID] != uid {
			s.dropPending(r)
			return nil, nil
		}
		succ := podEpochSuccessorContainers(p)
		if len(succ) == 0 {
			return nil, nil
		}
		ev := *r.pending
		s.dropPending(r)
		if p.ResourceVersion == "" || !podEpochUUID.MatchString(uid) {
			return nil, nil
		}
		return &PodEpochDecision{generation: g, session: s, namespace: p.Namespace, name: p.Name, uid: uid, rv: p.ResourceVersion, document: podEpochComplete(ev, succ), context: ctx}, nil
	}
	doc, proof := s.produce(w, p, r, predecessor, identityErr)
	if p.ResourceVersion == "" || !podEpochUUID.MatchString(uid) {
		return nil, nil
	}
	if proof != nil {
		// Positive proofs wait for successor container IDs; the bytes are charged
		// against the session budget.
		b, err := json.Marshal(proof)
		if err != nil {
			return nil, nil
		}
		if s.bytes+len(b) > PodEpochMaxMetadata {
			s.invalidate()
			return nil, nil
		}
		s.bytes += len(b)
		r.pending = proof
		r.pendingCost = len(b)
		return nil, nil
	}
	return &PodEpochDecision{generation: g, session: s, namespace: p.Namespace, name: p.Name, uid: uid, rv: p.ResourceVersion, document: doc, context: ctx}, nil
}
func podEpochSameMounts(a, b *corev1.Pod) bool {
	x, _ := json.Marshal(a.Spec.Volumes)
	y, _ := json.Marshal(b.Spec.Volumes)
	return string(x) == string(y)
}

// produce evaluates every proof check at the successor's Added event. It returns
// a negative document, or a frozen positive proof (document empty) to be completed
// by podEpochComplete.
func (s *PodEpochSession) produce(w PodEpochWorker, p *corev1.Pod, r *podEpochRecord, pred string, identityErr error) (string, *PodEpochEvidence) {
	neg := func(reason string) (string, *PodEpochEvidence) { return podEpochNegativeDoc(reason), nil }
	x := s.records[pred]
	if identityErr != nil || r.identity == nil {
		return neg("identity_unverified")
	}
	if !podEpochRuntime(w.Runtime) {
		return neg("runtime_not_declared")
	}
	if !podEpochPrivatePID(p) {
		return neg("successor_pid_unaudited")
	}
	if x == nil || x.terminal == 0 || x.terminal >= r.start || x.tainted || x.identity == nil {
		return neg("predecessor_unproven")
	}
	var xp corev1.Pod
	_ = json.Unmarshal(x.proof, &xp)
	cs, is, ok := podEpochSignal(&xp)
	if !ok {
		return neg("predecessor_unproven")
	}
	for id, other := range s.records {
		if id == pred || id == string(p.UID) || other.start > r.start {
			continue
		}
		if !other.related[w.ID] {
			continue
		}
		// The window starts at X addition (session start for a baseline X).
		// A deletion outside that window is not a termination signal for this
		// or any other pod; historical writers are outside this handoff proof.
		if other.deletedAt != 0 && other.deletedAt < x.start {
			continue
		}
		if !other.tainted && other.terminal != 0 && other.terminal < x.start {
			continue
		}
		if other.tainted || other.terminal == 0 || other.terminal >= x.terminal {
			return neg("concurrent_pod_unproven")
		}
	}
	before, after := x.identity, r.identity
	if before.Data != after.Data || before.DinD.Name != after.DinD.Name || (before.DinD.UID == after.DinD.UID && before.DinD.VolumeName != after.DinD.VolumeName) {
		return neg("claim_changed")
	}
	var remint *PodEpochRemint
	if before.DinD.UID != after.DinD.UID {
		receipt, ok := s.remints[w.ID+":"+before.DinD.UID+":"+after.DinD.UID]
		if !ok || receipt.OldClaimUID != before.DinD.UID || receipt.NewClaimUID != after.DinD.UID {
			return neg("dind_remint_unrecorded")
		}
		remint = &receipt
	}
	// The proof is frozen here; successor container IDs and the attestation are
	// added when they first appear (see podEpochComplete).
	e := PodEpochEvidence{Version: 2, Session: s.id, Predecessor: pred, Successor: string(p.UID), Containers: cs, InitContainers: is, Data: after.Data, DinD: after.DinD, Maintenance: remint, Runtime: w.Runtime, SessionStarted: s.started, TerminalObserved: x.terminalAt, SuccessorAdded: r.addedAt}
	return "", &e
}

func podEpochNegativeDoc(reason string) string {
	b, _ := json.Marshal(PodEpochNegative{Handoff: "none", Reason: reason})
	return string(b)
}

// podEpochSuccessorContainers lists every successor status entry with a
// container ID, init containers first.
func podEpochSuccessorContainers(p *corev1.Pod) []PodEpochSuccessorContainer {
	var out []PodEpochSuccessorContainer
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, c := range list {
			if c.ContainerID != "" {
				out = append(out, PodEpochSuccessorContainer{Name: c.Name, ContainerID: c.ContainerID})
			}
		}
	}
	return out
}

// podEpochComplete turns a frozen positive proof into the final v2 document
// once the successor reports container IDs.
func podEpochComplete(e PodEpochEvidence, succ []PodEpochSuccessorContainer) string {
	prefix := "containerd://"
	if e.Runtime == "crio-crun" {
		prefix = "cri-o://"
	}
	att := PodEpochAttestation{Result: "consistent", Reason: "observations_consistent"}
	for _, list := range [][]PodEpochContainer{e.Containers, e.InitContainers} {
		for _, c := range list {
			if !strings.HasPrefix(c.ContainerID, prefix) {
				att = PodEpochAttestation{Result: "contradicted", Reason: "predecessor_runtime_prefix"}
			}
		}
	}
	if att.Result == "consistent" {
		for _, c := range succ {
			if !strings.HasPrefix(c.ContainerID, prefix) {
				att = PodEpochAttestation{Result: "contradicted", Reason: "successor_runtime_prefix"}
				break
			}
		}
	}
	e.SuccessorContainers = succ
	e.Attestation = &att
	b, err := EncodePodEpochEvidence(e)
	if err != nil {
		return podEpochNegativeDoc("evidence_invalid_or_oversize")
	}
	return string(b)
}

// RecordMaintenanceRemint is called only by the maintenance operation owner after
// observing the minted identity. Receipts belong to this generation, not a pod annotation.
func (s *PodEpochSession) RecordMaintenanceRemint(g PodEpochGeneration, worker string, receipt PodEpochRemint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g != s.generation || s.overflow || receipt.OperationID == "" || !podEpochUUID.MatchString(receipt.OldClaimUID) || !podEpochUUID.MatchString(receipt.NewClaimUID) || receipt.OldClaimUID == receipt.NewClaimUID {
		return errors.New("invalid remint receipt")
	}
	known := false
	for _, w := range s.workers {
		if w.ID == worker {
			known = true
			break
		}
	}
	if !known {
		return errors.New("unknown maintenance worker")
	}
	if !s.chargeKey("claim:"+receipt.OldClaimUID) || !s.chargeKey("claim:"+receipt.NewClaimUID) {
		return errors.New("identity overflow")
	}
	receiptKey := worker + ":" + receipt.OldClaimUID + ":" + receipt.NewClaimUID
	if old, ok := s.remints[receiptKey]; ok && old != receipt {
		return errors.New("receipt already recorded")
	}
	key := "remint:" + worker + ":" + receipt.OperationID + ":" + receipt.OldClaimUID + ":" + receipt.NewClaimUID
	// Receipt metadata counts toward the byte cap but not the pod/claim count.
	if s.bytes+len(key) > PodEpochMaxMetadata {
		s.invalidate()
		return errors.New("metadata overflow")
	}
	if _, ok := s.remints[receiptKey]; !ok {
		s.bytes += len(key)
	}
	s.remints[receiptKey] = receipt
	return nil
}

// Deliver reserves the single attempt under the reset fence. The API call is
// submitted while holding that fence: Reset cannot interpose after validation.
// Once submitted, cancellation/reset cannot promise to revoke the server write.
// No retries, including conflicts and ambiguous transport failures.
func (s *PodEpochSession) Deliver(ctx context.Context, d *PodEpochDecision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d == nil || d.session != s || d.generation != s.generation || s.overflow {
		return errors.New("stale decision")
	}
	if d.discarded || ctx.Err() != nil || d.context.Err() != nil {
		d.discarded = true
		return errors.New("canceled decision")
	}
	if s.attempts[d.uid] {
		return errors.New("attempt already submitted")
	}
	s.attempts[d.uid] = true
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": d.uid, "resourceVersion": d.rv, "annotations": map[string]string{PodEpochAnnotation: d.document}}})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(markMaintenanceRead(ctx), podEpochRequestTimeout)
	defer cancel()
	client := s.client.CoreV1().RESTClient()
	if concrete, ok := client.(*rest.RESTClient); ok && concrete == nil {
		client = nil
	}
	if client != nil {
		return client.Patch(types.MergePatchType).MaxRetries(0).
			Namespace(d.namespace).Resource("pods").Name(d.name).Body(body).
			Do(requestCtx).Error()
	}
	_, err = s.client.CoreV1().Pods(d.namespace).Patch(requestCtx, d.name, types.MergePatchType, body, metav1.PatchOptions{})
	return err
}
