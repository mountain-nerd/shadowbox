// Command shadowbox answers "is this new prompt or model version safe to
// roll out?" for LLM agents. It runs as an Envoy ext_proc service on the live
// (observe) and shadow gateways and serves a verdict for Argo Rollouts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mountain-nerd/shadowbox/internal/app"
	"github.com/mountain-nerd/shadowbox/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "shadowbox:", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "shadowbox.yaml", "path to the configuration file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	httpLis, err := net.Listen("tcp", cfg.Listen.HTTP)
	if err != nil {
		return err
	}
	grpcLis, err := net.Listen("tcp", cfg.Listen.GRPC)
	if err != nil {
		return err
	}
	return a.Serve(ctx, httpLis, grpcLis)
}
