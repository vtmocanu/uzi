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

	"gitlab.example.com/vtmocanu/uzi/controller/internal/apiclient"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/config"
	"gitlab.example.com/vtmocanu/uzi/controller/internal/protocol"
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

	client := apiclient.New(cfg.APIBaseURL, cfg.Token, cfg.HTTPTimeout, cfg.APICAPool)

	// M1 ships the protocol and the loop; the kube client is M3's. Until then the
	// controller reconciles against a materializer that touches no cluster, so this
	// binary is exercisable end to end (it authenticates, polls, and honours the
	// desired state's shape) without a kube-apiserver anywhere near it.
	loop := reconcile.New(client, noopMaterializer{log: log}, cfg.PollInterval, log)

	log.Info("controller starting",
		"api_base_url", cfg.APIBaseURL,
		// The hop's posture, at a glance, in the first line of the log: this is the
		// connection that carries join tokens across the pod network, and "is it
		// actually verifying a CA" should not require reading the env to answer.
		"api_ca_pinned", cfg.APICAPool != nil,
		"poll_interval", cfg.PollInterval.String())
	loop.Run(ctx)
	log.Info("controller stopped")
}

// noopMaterializer is the M1 placeholder for the kube-backed materializer M3
// brings. It observes nothing and materializes nothing.
//
// Observing nothing is inert by construction now: the controller reports nothing to
// the api (delivery is settled by the worker's own registration), so an M1
// controller pointed at a real api can only read desired state. It cannot destroy a
// token, strand a worker, or touch a cluster.
type noopMaterializer struct{ log *slog.Logger }

func (noopMaterializer) Observe(context.Context) ([]reconcile.ObservedWorker, error) {
	return nil, nil
}

func (n noopMaterializer) Reconcile(_ context.Context, desired []protocol.DesiredWorker, observed []reconcile.ObservedWorker) error {
	// Counts only. The desired state carries join-token plaintext, so nothing here
	// logs a worker's fields.
	n.log.Info("reconcile (no-op: cluster materialization lands in M3)",
		"desired_workers", len(desired), "observed_workers", len(observed))
	return nil
}
