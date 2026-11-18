// Package app is the composition root: it builds the domain objects from the
// configuration and serves the HTTP API and the Envoy ext_proc gRPC service.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"github.com/mountain-nerd/shadowbox/internal/config"
	"github.com/mountain-nerd/shadowbox/internal/effect"
	"github.com/mountain-nerd/shadowbox/internal/experiment"
	"github.com/mountain-nerd/shadowbox/internal/extproc"
	"github.com/mountain-nerd/shadowbox/internal/httpapi"
	"github.com/mountain-nerd/shadowbox/internal/llm/openai"
	"github.com/mountain-nerd/shadowbox/internal/mcpimport"
	"github.com/mountain-nerd/shadowbox/internal/pairing"
	"github.com/mountain-nerd/shadowbox/internal/telemetry"
)

// shutdownGrace bounds how long Serve waits for in-flight requests after
// cancellation before closing connections.
const shutdownGrace = 2 * time.Second

// App holds the wired components.
type App struct {
	log             *slog.Logger
	cfg             *config.Config
	pairer          *pairing.Pairer
	extproc         *extproc.Server
	http            http.Handler
	shutdownMetrics func(context.Context) error
}

// New wires the application. MCP servers are queried once at startup; if one
// is unreachable its hints are skipped, which only makes classification
// stricter (unknown tools count as writes).
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	reg := cfg.EffectRegistry()
	for _, s := range cfg.MCP {
		reg = mergeMCP(ctx, log, reg, s)
	}

	analyzers, rewrites := cfg.Build(reg)
	registry := experiment.NewRegistry(analyzers)
	pairer := pairing.New(registry, pairing.Options{TTL: cfg.Pairing.TTL, MaxPending: cfg.Pairing.MaxPending})
	ext := extproc.NewServer(extproc.Options{Token: cfg.ExtProcToken, Parser: openai.Codec{}, Experiments: rewrites, Sink: pairer})

	metrics, shutdown, err := telemetry.NewHandler(telemetry.Sources{Pairing: pairer.Stats, Registry: registry, Shadow: ext.Stats})
	if err != nil {
		return nil, err
	}
	api := httpapi.New(httpapi.Options{Verdicts: registry, Ingestor: pairer, IngestToken: cfg.IngestToken, Metrics: metrics})
	return &App{log: log, cfg: cfg, pairer: pairer, extproc: ext, http: api, shutdownMetrics: shutdown}, nil
}

func mergeMCP(ctx context.Context, log *slog.Logger, reg *effect.Registry, s config.MCPServer) *effect.Registry {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hints, err := mcpimport.FetchURL(ctx, s.URL)
	if err != nil {
		log.Warn("skipping MCP tool annotations; unknown tools stay classified as writes", "url", s.URL, "err", err)
		return reg
	}
	log.Info("imported MCP tool annotations", "url", s.URL, "tools", len(hints), "trusted", s.Trusted)
	return reg.MergeHints(hints, s.Trusted)
}

// Serve runs both servers until ctx is cancelled, then shuts them down.
func (a *App) Serve(ctx context.Context, httpLis, grpcLis net.Listener) error {
	g := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(g, a.extproc)
	h := &http.Server{Handler: a.http, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- g.Serve(grpcLis) }()
	go func() {
		if err := h.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()
	go a.sweep(ctx)
	a.log.Info("shadowbox serving", "http", httpLis.Addr().String(), "grpc", grpcLis.Addr().String())

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	// net/http waits up to 5s for connections that never sent a request, so
	// bound the graceful phase and then force-close what is left.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	stopped := make(chan struct{})
	go func() { g.GracefulStop(); close(stopped) }()
	if h.Shutdown(shutdownCtx) != nil {
		_ = h.Close()
	}
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		g.Stop()
	}
	_ = a.shutdownMetrics(context.Background())
	return err
}

// sweep expires unpaired observations even when no new traffic arrives.
func (a *App) sweep(ctx context.Context) {
	t := time.NewTicker(max(a.cfg.Pairing.TTL/3, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pairer.Sweep()
		}
	}
}
