package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/apiclient"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestHostedRollRetainedClaimsBridge(t *testing.T) {
	url := os.Getenv("UZI_ROLL_TEST_API")
	if url == "" {
		t.Skip("invoked by API TestHostedRollRetainedClaimsLiveDB")
	}
	url, err := hostedRollFixtureURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := os.Getenv("UZI_ROLL_TEST_CONTROLLER_KEY")
	client := apiclient.New(url, key, 10*time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var members []struct {
		ID  string `json:"id"`
		UID string `json:"uid"`
	}
	if json.Unmarshal([]byte(os.Getenv("UZI_ROLL_TEST_MEMBERS")), &members) != nil || len(members) != 4 {
		t.Fatal("invalid member input")
	}
	// Other live-DB tests share the disposable database. Select only this
	// fixture's owner pool from the real poll, without altering other fixtures.
	selectPool := func(workers []protocol.DesiredWorker) []protocol.DesiredWorker {
		t.Helper()
		selected := make([]protocol.DesiredWorker, 0, len(members))
		for _, w := range workers {
			for _, member := range members {
				if w.ID == member.ID {
					selected = append(selected, w)
					break
				}
			}
		}
		if len(selected) != len(members) {
			t.Fatalf("fixture pool missing from poll: %d/%d", len(selected), len(members))
		}
		return selected
	}
	resp, err := client.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp.Workers = selectPool(resp.Workers)
	if len(resp.Workers) != 4 {
		t.Fatalf("HTTP fleet size=%d", len(resp.Workers))
	}
	sort.Slice(resp.Workers, func(i, j int) bool { return resp.Workers[i].ID < resp.Workers[j].ID })
	uid := map[string]string{}
	for _, m := range members {
		uid[m.ID] = m.UID
	}
	var objects []runtime.Object
	baseline := map[string]*appsv1.Deployment{}
	for _, w := range resp.Workers {
		if w.Busy || w.DrainingSince != nil || w.RollEvidence == nil || w.RollEvidence.RegisterNonce == nil || w.RollEvidence.PodUID == nil || *w.RollEvidence.PodUID != uid[w.ID] || w.RollEvidence.HeartbeatAt == nil {
			t.Fatal("initial HTTP old-worker evidence incomplete")
		}
		old := physicalOld(t, deployedWorker(w.ID, w.Generation, "old"))
		old[2].(*corev1.Pod).UID = types.UID(uid[w.ID])
		objects = append(objects, old...)
		baseline[w.ID] = old[0].(*appsv1.Deployment).DeepCopy()
	}
	m, c := newMatWithDrain(t, client, DrainPolicy{Deadline: time.Hour}, objects...)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	tick := func() []protocol.DesiredWorker {
		t.Helper()
		tickCtx := reconcile.WithPollStart(ctx, clock)
		p, err := client.Poll(tickCtx)
		if err != nil {
			t.Fatal(err)
		}
		p.Workers = selectPool(p.Workers)
		obs, err := m.Observe(tickCtx)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Reconcile(tickCtx, p.Workers, obs); err != nil {
			t.Fatal(err)
		}
		return p.Workers
	}
	tick()
	patches := templatePatches(c)
	if len(patches) != 1 {
		t.Fatalf("initial canary template patches=%d", len(patches))
	}
	var canary string
	for _, w := range resp.Workers {
		if deploymentName(w.ID) == patches[0] {
			canary = w.ID
		}
	}
	if canary == "" {
		t.Fatal("unknown canary")
	}
	d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, deploymentName(canary), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Correctly owned target pod is stuck; no invented API registration/proof.
	target := physicalOld(t, d.DeepCopy())
	rs := target[1].(*appsv1.ReplicaSet)
	rs.Name = "target-rs-" + d.Name
	rs.UID = types.UID("target-rs-uid")
	pod := target[2].(*corev1.Pod)
	pod.Name = "target-pod-" + d.Name
	pod.UID = types.UID("target-pod-uid")
	pod.OwnerReferences[0].Name = rs.Name
	pod.OwnerReferences[0].UID = rs.UID
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	if err = c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
		t.Fatal(err)
	}
	if err = c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
		t.Fatal(err)
	}
	tick()
	clock = clock.Add(2 * time.Hour)
	workers := tick()
	retained := []string{}
	check := func(workers []protocol.DesiredWorker) {
		t.Helper()
		if len(templatePatches(c)) != 1 {
			t.Fatal("stuck canary admitted additional template update")
		}
		for _, w := range workers {
			if w.ID == canary {
				continue
			}
			if w.DrainingSince != nil || w.DrainingReason != nil {
				t.Fatal("not-admitted sibling has API cordon")
			}
			got, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(ctx, deploymentName(w.ID), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, baseline[w.ID]) {
				t.Fatal("retained old deployment changed")
			}
			p := m.PacingOutcomes()[w.ID]
			if p == nil || p.BlockingReason == nil || *p.BlockingReason != "ImagePullBackOff" {
				t.Fatal("missing stuck-pool diagnostic")
			}
		}
	}
	check(workers)
	for _, w := range workers {
		if w.ID != canary {
			retained = append(retained, w.ID)
		}
	}
	body, _ := json.Marshal(map[string]any{"retained": retained})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/fixture/claim-retained", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 204 {
		t.Fatalf("real API retained claims status=%d", r.StatusCode)
	}
	workers = tick()
	check(workers)
	busy := 0
	for _, w := range workers {
		if w.ID != canary && w.Busy {
			busy++
		}
	}
	if busy != 3 {
		t.Fatalf("fresh HTTP busy count=%d", busy)
	}

	t.Log("M2 real HTTP poll/cordon channel + simulated UID/RV-enforcing kube: exactly one canary update, target ImagePullBackOff; retained deployments unchanged beyond 1h deadline; real API claims then Busy=3 without sibling drain")
}

func TestHostedPressureClearedClaimsBridge(t *testing.T) {
	url := os.Getenv("UZI_ROLL_TEST_API")
	if url == "" {
		t.Skip("invoked by API TestHostedRollRetainedClaimsLiveDB")
	}
	url, err := hostedRollFixtureURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := os.Getenv("UZI_ROLL_TEST_CONTROLLER_KEY")
	client := apiclient.New(url, key, 10*time.Second, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var members []struct {
		ID  string `json:"id"`
		UID string `json:"uid"`
	}
	if json.Unmarshal([]byte(os.Getenv("UZI_ROLL_TEST_MEMBERS")), &members) != nil || len(members) != 4 {
		t.Fatal("invalid member input")
	}
	resp, err := client.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pressureWorker protocol.DesiredWorker
	for _, w := range resp.Workers {
		for _, member := range members {
			if w.ID == member.ID && w.Busy && w.DrainingSince == nil && w.DrainingReason == nil {
				pressureWorker = w
				break
			}
		}
		if pressureWorker.ID != "" {
			break
		}
	}
	pressureID := pressureWorker.ID
	if pressureWorker.ID == "" {
		t.Fatal("pressure worker missing from initial poll")
	}
	cfg := testConfig()
	cfg.RollTargetVersion = "" // A bare image tag provides no exact roll version.
	pressureMat, pressureKube := newMatWithDrain(t, client, DrainPolicy{Deadline: time.Hour}, RenderDeployment(cfg, pressureWorker, testSpec(t, pressureWorker.Template, pressureWorker.Size)))
	pressureMat.cfg = cfg
	for _, pvc := range RenderPVCs(cfg, pressureWorker, testSpec(t, pressureWorker.Template, pressureWorker.Size)) {
		if err := pressureKube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), pvc, pvc.Namespace); err != nil {
			t.Fatal(err)
		}
	}
	pressureCall := func(phase string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"id": pressureID, "phase": phase})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/fixture/pressure", bytes.NewReader(body)) //nolint:gosec // G704: hostedRollFixtureURL reconstructs a fixed loopback host and bounded integer port for the trusted httptest parent.
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		r, err := http.DefaultClient.Do(req) //nolint:gosec // G704: request destination is the validated loopback-only httptest parent above.
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 204 {
			t.Fatalf("pressure callback %s status=%d", phase, r.StatusCode)
		}
	}
	pressureTick := func() {
		t.Helper()
		p, err := client.Poll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var selected []protocol.DesiredWorker
		for _, w := range p.Workers {
			if w.ID == pressureID {
				selected = append(selected, w)
			}
		}
		if len(selected) != 1 {
			t.Fatal("pressure worker missing")
		}
		obs, err := pressureMat.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := pressureMat.Reconcile(ctx, selected, obs); err != nil {
			t.Fatal(err)
		}
	}
	pressureCall("begin")
	pressureTick()
	pressureCall("denied")
	pressureTick()
	pressureCall("success")
	for _, action := range pressureKube.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("cleared pressure replay destroyed an object")
		}
	}
	t.Log("real pressure routes: two reconcile ticks, new unassigned claim denied then succeeded after reason-owned release")
}

// The cross-module parent is an httptest server, never an external endpoint.
// Reconstructing from a fixed host and bounded integer port prevents environment
// input from changing the request destination or injecting a path/credential.
func hostedRollFixtureURL(raw string) (string, error) {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("claim fixture requires an HTTP loopback server")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("claim fixture requires a valid loopback port")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

func TestHostedRollFixtureURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:1", "http://127.0.0.1:65535"} {
		got, err := hostedRollFixtureURL(raw)
		if err != nil || got != raw {
			t.Fatalf("valid fixture %q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://example.invalid:80", "http://localhost:80", "https://127.0.0.1:80", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:80", "http://127.0.0.1:80/path", "http://127.0.0.1:80?query", "http://127.0.0.1:80#fragment"} {
		if _, err := hostedRollFixtureURL(raw); err == nil {
			t.Fatalf("unsafe fixture destination accepted: %q", raw)
		}
	}
}
