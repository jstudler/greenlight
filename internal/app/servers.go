package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jstudler/greenlight/internal/options"
)

const serverShutdownTimeout = 5 * time.Second

// servers bundles the metrics and the health endpoints.
type servers struct {
	logger  *slog.Logger
	running []*http.Server
	wg      sync.WaitGroup
}

func newServers(opts *options.Options, logger *slog.Logger, registry *prometheus.Registry, synced func() bool) *servers {
	s := &servers{logger: logger}

	if !options.Disabled(opts.MetricsBindAddress) {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
		s.running = append(s.running, &http.Server{
			Addr:              opts.MetricsBindAddress,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	if !options.Disabled(opts.HealthProbeBindAddress) {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ok")
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			if !synced() {
				http.Error(w, "the informer caches are still syncing", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ok")
		})
		s.running = append(s.running, &http.Server{
			Addr:              opts.HealthProbeBindAddress,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		})
	}
	return s
}

func (s *servers) start(ctx context.Context) {
	for _, server := range s.running {
		s.logger.Info("serving", "address", server.Addr)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("the HTTP server stopped", "address", server.Addr, "error", err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		s.shutdown()
	}()
}

func (s *servers) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	for _, server := range s.running {
		if err := server.Shutdown(ctx); err != nil {
			s.logger.Warn("the HTTP server did not shut down cleanly", "address", server.Addr, "error", err)
		}
	}
	s.wg.Wait()
}

// podNamespace reads the namespace of the running pod.
func podNamespace() string {
	if namespace := os.Getenv("POD_NAMESPACE"); namespace != "" {
		return namespace
	}
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return string(data)
}
