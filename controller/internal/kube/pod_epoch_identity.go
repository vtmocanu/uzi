package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PodEpochWorker struct {
	ID      string
	Runtime string
}
type PodEpochIdentity struct {
	Data PodEpochClaim
	DinD PodEpochClaim
}

// PodEpochIdentityResolver must use live objects, never copied pod annotations.
// A resolution error refuses evidence; it does not stop ordinary reconciliation.
type PodEpochIdentityResolver interface {
	ResolvePodEpoch(context.Context, PodEpochWorker, *corev1.Pod, bool) (PodEpochIdentity, error)
}
type PodEpochKubeResolver struct{ Client kubernetes.Interface }

func (r PodEpochKubeResolver) ResolvePodEpoch(ctx context.Context, w PodEpochWorker, p *corev1.Pod, successor bool) (PodEpochIdentity, error) {
	ctx = markMaintenanceRead(ctx)
	var budget podEpochReadBudget
	var result PodEpochIdentity
	fail := errors.New("pod epoch live identity mismatch")
	if !podEpochRuntime(w.Runtime) || p.Spec.RuntimeClassName != nil || p.Labels[LabelWorkerID] != w.ID {
		return result, fail
	}
	if successor {
		owner := metav1.GetControllerOf(p)
		if owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != "apps/v1" {
			return result, fail
		}
		obj, err := budget.named(ctx, r.Client.AppsV1().RESTClient(), p.Namespace, "replicasets", owner.Name, &appsv1.ReplicaSetList{}, func(o metav1.ListOptions) (runtime.Object, error) {
			return r.Client.AppsV1().ReplicaSets(p.Namespace).List(ctx, o)
		})
		rs, _ := obj.(*appsv1.ReplicaSet)
		if err != nil {
			return result, err
		}
		if rs.UID == "" || rs.UID != owner.UID || rs.DeletionTimestamp != nil {
			return result, fail
		}
		obj, err = budget.named(ctx, r.Client.AppsV1().RESTClient(), p.Namespace, "deployments", deploymentName(w.ID), &appsv1.DeploymentList{}, func(o metav1.ListOptions) (runtime.Object, error) {
			return r.Client.AppsV1().Deployments(p.Namespace).List(ctx, o)
		})
		dep, _ := obj.(*appsv1.Deployment)
		if err != nil {
			return result, err
		}
		if dep.DeletionTimestamp != nil {
			return result, fail
		}
		ro := metav1.GetControllerOf(rs)
		if ro == nil || ro.Kind != "Deployment" || ro.APIVersion != "apps/v1" || ro.UID != dep.UID || dep.UID == "" || ro.Name != dep.Name {
			return result, fail
		}
		a, b := dep.Spec.Template.DeepCopy(), rs.Spec.Template.DeepCopy()
		delete(a.Labels, "pod-template-hash")
		delete(b.Labels, "pod-template-hash")
		if !equality.Semantic.DeepEqual(a, b) {
			return result, fail
		}
		// Require the declared literal in both the live template and the actual worker
		// container; valueFrom and duplicate env names cannot attest a runtime.
		if !podEpochDeclaredRuntime(dep.Spec.Template.Spec, w.Runtime) || !podEpochDeclaredRuntime(p.Spec, w.Runtime) {
			return result, fail
		}
	}
	for _, target := range []struct {
		name string
		out  *PodEpochClaim
	}{{dataPVCName(w.ID), &result.Data}, {dindDataPVCName(w.ID), &result.DinD}} {
		count := 0
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == target.name {
				count++
			}
		}
		if count != 1 {
			return result, fail
		}
		obj, err := budget.named(ctx, r.Client.CoreV1().RESTClient(), p.Namespace, "persistentvolumeclaims", target.name, &corev1.PersistentVolumeClaimList{}, func(o metav1.ListOptions) (runtime.Object, error) {
			return r.Client.CoreV1().PersistentVolumeClaims(p.Namespace).List(ctx, o)
		})
		pvc, _ := obj.(*corev1.PersistentVolumeClaim)
		if err != nil {
			return result, err
		}
		*target.out = PodEpochClaim{Name: pvc.Name, UID: string(pvc.UID), VolumeName: pvc.Spec.VolumeName}
		if !podEpochValidClaim(*target.out) || pvc.DeletionTimestamp != nil {
			return result, fail
		}
	}
	return result, nil
}

// Every resolution has one shared item/byte budget. Real REST responses are
// charged before JSON decoding; fake clients use their serialized list size.
// At most 4096 pages are attempted (empty pages still consume an attempt), and
// any failed, repeated, oversized or ambiguous page refuses the whole resolution.
type podEpochReadBudget struct{ items, bytes, pages int }

func (b *podEpochReadBudget) named(ctx context.Context, client rest.Interface, namespace, resource, name string, empty runtime.Object, fakeList func(metav1.ListOptions) (runtime.Object, error)) (runtime.Object, error) {
	if concrete, ok := client.(*rest.RESTClient); ok && concrete == nil {
		client = nil
	}
	var found runtime.Object
	token := ""
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.pages++
		if b.pages > PodEpochMaxIdentities {
			return nil, errors.New("pod epoch page budget exceeded")
		}
		opts := metav1.ListOptions{Limit: 100, Continue: token, FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()}
		var list runtime.Object
		var data []byte
		var err error
		if client != nil {
			data, err = client.Get().SetHeader("Accept", "application/json").Namespace(namespace).Resource(resource).VersionedParams(&opts, scheme.ParameterCodec).DoRaw(ctx)
			if err != nil {
				return nil, err
			}
			b.bytes += len(data)
			if len(data) > 8<<20 || b.bytes > PodEpochMaxMetadata {
				return nil, errors.New("pod epoch response budget exceeded")
			}
			list = empty.DeepCopyObject()
			if err = json.Unmarshal(data, list); err != nil {
				return nil, err
			}
		} else {
			list, err = fakeList(opts)
			if err != nil {
				return nil, err
			}
			data, err = json.Marshal(list)
			if err != nil {
				return nil, err
			}
			b.bytes += len(data)
			if len(data) > 8<<20 || b.bytes > PodEpochMaxMetadata {
				return nil, errors.New("pod epoch response budget exceeded")
			}
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		b.items += len(items)
		if len(items) > 100 || b.items > PodEpochMaxIdentities {
			return nil, errors.New("pod epoch item budget exceeded")
		}
		for _, item := range items {
			m, err := meta.Accessor(item)
			if err != nil {
				return nil, err
			}
			if m.GetName() != name || m.GetNamespace() != namespace {
				continue
			}
			if found != nil {
				return nil, fmt.Errorf("ambiguous pod epoch %s %s", resource, name)
			}
			found = item
		}
		m, err := meta.ListAccessor(list)
		if err != nil {
			return nil, err
		}
		token = m.GetContinue()
		if token == "" {
			break
		}
		if seen[token] {
			return nil, errors.New("repeated pod epoch continuation")
		}
		seen[token] = true
	}
	if found == nil {
		return nil, fmt.Errorf("missing pod epoch %s %s", resource, name)
	}
	return found, nil
}

func podEpochDeclaredRuntime(s corev1.PodSpec, runtime string) bool {
	for _, c := range s.Containers {
		if c.Name != "worker" {
			continue
		}
		count := 0
		for _, e := range c.Env {
			if e.Name == PodEpochRuntimeEnv {
				if e.ValueFrom != nil || e.Value != runtime {
					return false
				}
				count++
			}
		}
		return count == 1
	}
	return false
}
