package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// These two tests use only symbols that exist before the codex-extra-ca evidence work, so
// they compile and run against the base commit. They pin the user-visible failure: an
// operator sets the extra CA while workers already exist, and a worker whose write-once
// Secret predates the CA must not be handed UZI_CODEX_CA_FILE (the file would be missing
// from its Secret and the pod would crash on roll).

func codexCABaseEnv(d *appsv1.Deployment) map[string]string {
	out := map[string]string{}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != workerContainerName {
			continue
		}
		for _, e := range c.Env {
			out[e.Name] = e.Value
		}
	}
	return out
}

// Legacy drift (uid-split worker, so UZI_CLI_CA_FILE is not rendered, and a relayed API CA
// so NODE_EXTRA_CA_CERTS is rendered both ways): the operator sets the extra CA after the
// workers were created. Their Secrets lack the Codex key and their claims carry no
// evidence of it, so the render must not change: no new env, the same spec hash, and no
// Deployment write.
func TestCodexCABaseLegacyWorkerKeepsItsSpecHashWhenExtraCAIsSet(t *testing.T) {
	cfg := testConfig()
	cfg.UIDSplit = true
	cfg.APICAPEM = []byte("api-ca")

	var objs []runtime.Object
	var workers []protocol.DesiredWorker
	wantHash := map[string]string{}
	for _, id := range []string{"w1", "w2"} {
		w := evidence(desired(id), m5Now)
		workers = append(workers, w)
		dep := RenderDeployment(cfg, w, testSpec(t, "base", "m"))
		wantHash[id] = dep.Spec.Template.Annotations[AnnotationSpecHash]
		objs = append(objs, physicalOld(t, dep)...)
		objs = append(objs, pvcFor(id, "data"), pvcNix(id, "20Gi"))
	}
	m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objs...)
	m.cfg.UIDSplit = true
	m.cfg.APICAPEM = []byte("api-ca")
	m.cfg.ExtraCAPEM = []byte("extra-ca")

	rollTickTest(t, m, workers)

	for _, id := range []string{"w1", "w2"} {
		d, err := c.AppsV1().Deployments(cfg.Namespace).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if v, present := codexCABaseEnv(d)["UZI_CODEX_CA_FILE"]; present {
			t.Errorf("%s: UZI_CODEX_CA_FILE = %q, want unset (its Secret has no Codex key)", id, v)
		}
		if got := d.Spec.Template.Annotations[AnnotationSpecHash]; got != wantHash[id] {
			t.Errorf("%s: spec hash changed %s -> %s", id, wantHash[id], got)
		}
	}
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "deployments" && (a.GetVerb() == "patch" || a.GetVerb() == "update") {
			t.Errorf("unexpected deployment %s in namespace %s", a.GetVerb(), a.GetNamespace())
		}
	}
}

// A Waking worker whose Deployment was dropped is recreated by the wake path. With the
// extra CA configured but no evidence the worker's Secret carries it (no join token this
// tick, unstamped claims), the recreated Deployment must start and must not reference the
// Codex key.
func TestCodexCABaseKeylessWakeRecreateOmitsTheCodexEnv(t *testing.T) {
	m, client, _ := hibEnv(t)
	m.cfg.ExtraCAPEM = []byte("extra-ca")
	w := hibW(protocol.HibernationWaking, "wake-op")

	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	var created *appsv1.Deployment
	for _, a := range client.Actions() {
		if ca, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "deployments" {
			created = ca.GetObject().(*appsv1.Deployment)
		}
	}
	if created == nil || created.Spec.Replicas == nil || *created.Spec.Replicas != 1 {
		t.Fatalf("wake did not create the Deployment at replicas 1: %+v", created)
	}
	if v, present := codexCABaseEnv(created)["UZI_CODEX_CA_FILE"]; present {
		t.Errorf("UZI_CODEX_CA_FILE = %q on a worker with no evidence its Secret has the key", v)
	}
}
