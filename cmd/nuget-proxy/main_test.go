package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInvalidArguments(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(context.Background(), []string{"-help"}, logger); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	for _, args := range [][]string{{"-config", filepath.Join(t.TempDir(), "missing.yaml")}, {"unexpected"}, {"-unknown"}} {
		if err := run(context.Background(), args, logger); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	t.Run("YAML", func(t *testing.T) { testRunGracefulShutdown(t, false) })
	t.Run("environment overrides", func(t *testing.T) { testRunGracefulShutdown(t, true) })
}

func testRunGracefulShutdown(t *testing.T, environment bool) {
	t.Helper()
	for _, name := range []string{
		"NUGET_PROXY_LISTEN", "NUGET_PROXY_PUBLIC_BASE_URL", "NUGET_PROXY_ALLOW",
		"NUGET_PROXY_CACHE_DIR", "NUGET_PROXY_CACHE_MAX_BYTES", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES",
		"NUGET_PROXY_CACHE_METADATA_TTL", "NUGET_PROXY_UPSTREAM_TIMEOUT", "NUGET_PROXY_UPSTREAM_MAX_CONCURRENT",
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	filename := filepath.Join(directory, "config.yaml")
	document := fmt.Sprintf("listen: %q\npublic_base_url: http://%s\ncache:\n  dir: '%s'\n", address, address, filepath.Join(directory, "cache"))
	if environment {
		document = "public_base_url: /invalid\nallow: [Outside.Subset]\n"
		t.Setenv("NUGET_PROXY_LISTEN", address)
		t.Setenv("NUGET_PROXY_PUBLIC_BASE_URL", "http://"+address)
		t.Setenv("NUGET_PROXY_ALLOW", "Ddth.*")
		t.Setenv("NUGET_PROXY_CACHE_DIR", filepath.Join(directory, "cache"))
	}
	if err := os.WriteFile(filename, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, []string{"-config", filename}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	started := false
	for !started && ctx.Err() == nil {
		response, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			started = response.StatusCode == http.StatusOK
			response.Body.Close()
		}
		if !started {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if started && environment {
		response, err := client.Get("http://" + address + "/v3/flatcontainer/outside.subset/index.json")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("environment allowlist did not replace YAML: %d", response.StatusCode)
		}
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("proxy did not become responsive before shutdown")
	}
	if response, err := client.Get("http://" + address + "/healthz"); err == nil {
		response.Body.Close()
		t.Fatal("proxy listener remained open after graceful shutdown")
	}
}
