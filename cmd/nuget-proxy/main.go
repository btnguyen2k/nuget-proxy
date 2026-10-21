package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/config"
	"github.com/btnguyen2k/nuget-proxy/internal/proxy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil && !errors.Is(err, flag.ErrHelp) {
		logger.Error("nuget-proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *slog.Logger) error {
	flags := flag.NewFlagSet("nuget-proxy", flag.ContinueOnError)
	filename := flags.String("config", "config.yaml", "Path to the YAML configuration file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	cfg, err := config.Load(*filename)
	if err != nil {
		return err
	}
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := proxy.New(appCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer app.Close()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: cfg.Upstream.Timeout + 30*time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10,
	}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	logger.Info("nuget-proxy listening", "listen", cfg.Listen, "source", cfg.PublicBaseURL+"/v3/index.json",
		"allow_patterns", len(cfg.Allow), "cache_dir", cfg.Cache.Dir)
	if len(cfg.Allow) == 0 {
		logger.Warn("allowlist is empty; no packages are available")
	}
	if len(cfg.PublicBaseURL) >= 5 && cfg.PublicBaseURL[:5] == "http:" {
		logger.Warn("public URL uses HTTP; repository-signature discovery requires HTTPS and is not advertised")
	}
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.Join(err, server.Close())
		}
		return nil
	}
}
