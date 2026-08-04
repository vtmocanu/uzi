// Command controller is the uzi hosted-worker controller (PRD #58).
//
// It runs one loop: poll the api for the hosted fleet's desired state and drive
// the cluster towards it. It listens on no port — every exchange is outbound to
// the api — and it is the only uzi component that holds kube-apiserver
// credentials (Decision 1). Nothing here writes to the database; the api owns all
// state, this process owns only the cluster.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"gitlab.example.com/vtmocanu/uzi/controller/internal/apiclient"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/config"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/kube"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/preset"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/reconcile"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration", "error", err)
		os.Exit(1)
	}

	// SIGTERM is how kubernetes asks for a shutdown; cancelling the context stops
	// the loop after the in-flight cycle rather than mid-reconcile.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := apiclient.New(cfg.APIBaseURL, cfg.Token, cfg.HTTPTimeout, cfg.APICAPool, log)

	resolver, err := preset.NewResolver(cfg.WorkerImageRepo, cfg.WorkerImageTag)
	if err != nil {
		log.Error("worker image configuration", "error", err)
		os.Exit(1)
	}

	// The in-cluster client. This is uzi's ONLY kube-apiserver credential, and it is
	// why this component exists at all: a Role that can create pods in a namespace can
	// mount any Secret and impersonate any ServiceAccount in it, so the api holding it
	// would turn any api compromise into namespace-wide secret exfiltration. Here it
	// is scoped to a namespace containing nothing but hosted workers.
	//
	// InClusterConfig, with no kubeconfig fallback: this process only ever runs as a
	// pod, and a fallback would let it silently pick up a developer's admin
	// kubeconfig — vastly wider than the Role it is supposed to be bounded by.
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Error("kube client: this controller must run in-cluster with its own ServiceAccount", "error", err)
		os.Exit(1)
	}
	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Error("kube client", "error", err)
		os.Exit(1)
	}

	materializerCfg := kube.RenderConfig{
		Namespace:       cfg.WorkerNamespace,
		DockerNamespace: cfg.WorkerDockerNamespace,
		DinDImage:       cfg.WorkerDinDImage,
		// Config carries the POSITIVE `rootless` bool (matching the chart value); the
		// render side carries the NEGATIVE `non-rootless` so its zero value is the safe
		// rootless posture. Invert here, at the one wiring point.
		DinDNonRootless: !cfg.WorkerDinDRootless,
		// DinD resource overrides (PRD #89 0.8.1), requests + limits; empty leaves the
		// render default.
		DinDRequestCPU:    cfg.WorkerDinDRequestCPU,
		DinDRequestMemory: cfg.WorkerDinDRequestMemory,
		DinDLimitCPU:      cfg.WorkerDinDLimitCPU,
		DinDLimitMemory:   cfg.WorkerDinDLimitMemory,
		// The DinD daemon's data-root PVC size (issue #224 M-a); empty leaves the render
		// default.
		DinDDataSize: cfg.WorkerDinDDataSize,
		// The worker container's requests.ephemeral-storage (issue #224 M-b), per tier;
		// docker REPLACES plain. Empty leaves the render defaults (512Mi / 4Gi).
		EphemeralRequest:       cfg.WorkerEphemeralRequest,
		DockerEphemeralRequest: cfg.WorkerDockerEphemeralRequest,
		// Each tier's PVC admission ceiling (issue #224). Renders nothing; checked once
		// at boot, just below.
		MaxPVCStorage:       cfg.WorkerMaxPVCStorage,
		DockerMaxPVCStorage: cfg.WorkerDockerMaxPVCStorage,
		ServiceAccountName:  cfg.WorkerServiceAccount,
		APIURL:              cfg.WorkerAPIURL,
		StorageClass:        cfg.WorkerStorageClass,
		MaxConcurrentRuns:   cfg.WorkerMaxConcurrentRuns,
		APICAPEM:            cfg.APICAPEM,
	}
	materializer := kube.New(kubeClient, materializerCfg, resolver, log)

	// REFUSE TO BOOT on a PVC that its namespace's LimitRange will reject (issue #224).
	//
	// It runs HERE, after the RenderConfig is assembled and before the reconcile loop
	// starts, because that config is what carries both the ceilings and the dind
	// override — this is the first point where every claimant is known. Fatal rather
	// than a warning, and that is the proportionate response: an oversized claim is
	// rejected at admission, the Deployment is never created, and the loop retries every
	// tick forever with the cause in one log line nobody reads. The worker provisions
	// and never appears. A CrashLoopBackOff naming the two numbers is strictly louder
	// than a fleet that silently never materialises.
	if err := kube.ValidatePVCCeilings(materializerCfg, resolver); err != nil {
		log.Error("hosted-worker PVC sizes do not fit their namespace's LimitRange; refusing to start", "error", err)
		os.Exit(1)
	}

	loop := reconcile.New(client, materializer, client, cfg.PollInterval, cfg.WorkerImageTag, log)

	log.Info("controller starting",
		"api_base_url", cfg.APIBaseURL,
		// The hop's posture, at a glance, in the first line of the log: this is the
		// connection that carries join tokens across the pod network, and "is it
		// actually verifying a CA" should not require reading the env to answer.
		"api_ca_pinned", cfg.APICAPool != nil,
		"poll_interval", cfg.PollInterval.String(),
		"worker_namespace", cfg.WorkerNamespace,
		// The image tag is the thing that rolls the fleet on a release, so it belongs in
		// the line you read first when a worker is running the wrong code.
		"worker_image", cfg.WorkerImageRepo+"/agent-<template>:"+cfg.WorkerImageTag,
		"worker_ca_relayed", len(cfg.APICAPEM) > 0,
		// The docker tier's posture at a glance: whether this controller will render
		// docker workers at all, and into which privileged namespace (empty = off, and
		// docker workers in the poll are then skipped, not rendered into the default).
		"docker_workers", cfg.WorkerDockerNamespace != "",
		"docker_namespace", cfg.WorkerDockerNamespace,
		// The DinD posture at a glance (PRD #89): false here means dockerd runs as real
		// root and a breakout is node root — worth seeing in the first log line on a
		// cluster that opted into it.
		"docker_rootless", cfg.WorkerDinDRootless)
	loop.Run(ctx)
	log.Info("controller stopped")
}
