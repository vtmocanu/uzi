// Command fetcher is uzi-fetcher (PRD #1906 M2): the fetch service a profile-bound
// research run reads web content through. It ships in the api image as /fetcher and runs
// as its own Deployment; see internal/fetcher for the checks and the api contract.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vtmocanu/uzi/api/internal/fetcher"
	"github.com/vtmocanu/uzi/api/internal/tlsx"
)

// version and commit are stamped by api/Dockerfile's ldflags (-X main.version,
// -X main.commit), as for cmd/server. Unset on a plain `go build`.
var (
	version = "dev"
	commit  = ""
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fetcher exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := fetcher.LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	control, err := fetcher.NewHTTPControl(cfg.APIURL, cfg.ServiceToken, cfg.APICAPool, fetcher.DefaultControlTimeout)
	if err != nil {
		return err
	}
	f := fetcher.New(cfg.FetcherOptions("uzi-fetcher/" + version))
	handler := fetcher.NewServer(f, control, slog.Default(), cfg.MaxInflight)

	writeTimeout, shutdownTimeout := serverTimeouts(cfg)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	// The worker-facing listener is TLS only, with the mounted pair re-read on rotation
	// (the same reloader as the api's TLS listener).
	reloader, err := tlsx.NewReloader(cfg.TLSCertFile, cfg.TLSKeyFile, slog.Default())
	if err != nil {
		return err
	}
	srv.TLSConfig = reloader.ServerConfig()

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("bind fetcher listener %s: %w", cfg.Addr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("fetcher listening (tls)", "addr", cfg.Addr, "version", version, "commit", commit)
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		slog.Info("shutting down")
	}
	// Long enough for an in-flight handler to finish (serverTimeouts).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

// readTimeout bounds reading one worker request, headers and body.
const readTimeout = 15 * time.Second

// serverTimeouts returns the listener's WriteTimeout and the shutdown budget. net/http
// arms the write deadline when it has read a request's headers, so it must cover reading
// the body (readTimeout) plus the handler's worst case (fetcher.HandlerBudget: Begin, the
// whole fetch, the source-log write and streaming the largest body). Shutdown waits for
// in-flight handlers, so its budget is that write deadline plus a margin.
func serverTimeouts(cfg fetcher.Config) (write, shutdown time.Duration) {
	write = readTimeout + fetcher.HandlerBudget(fetcher.DefaultControlTimeout, cfg.Timeout, cfg.MaxFileBytes)
	return write, write + 5*time.Second
}
