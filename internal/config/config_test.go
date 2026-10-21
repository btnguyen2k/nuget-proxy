package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeDefaultsAndOverrides(t *testing.T) {
	cfg, err := Decode(strings.NewReader(`
public_base_url: https://feed.example.com/nuget/
allow:
  - Newtonsoft.Json
  - Ddth.*
cache:
  metadata_ttl: 3m
upstream:
  max_concurrent: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL != "https://feed.example.com/nuget" || cfg.Listen != "127.0.0.1:8080" ||
		cfg.Cache.MetadataTTL != 3*time.Minute || cfg.Cache.MaxBytes != 1<<30 ||
		cfg.Upstream.Timeout != 2*time.Minute || cfg.Upstream.MaxConcurrent != 4 || len(cfg.Allow) != 2 {
		t.Fatalf("unexpected configuration: %+v", cfg)
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	valid := "public_base_url: https://feed.example.com\n"
	for name, document := range map[string]string{
		"missing URL":      "allow: []",
		"unknown field":    valid + "alow: [Ddth.*]",
		"unknown nested":   valid + "cache:\n  directory: cache",
		"duplicate field":  valid + "public_base_url: https://other.example.com",
		"extra document":   valid + "---\nallow: []",
		"malformed YAML":   valid + "allow: [",
		"invalid rule":     valid + "allow: ['*']",
		"negative TTL":     valid + "cache:\n  metadata_ttl: -1s",
		"invalid duration": valid + "upstream:\n  timeout: forever",
		"zero concurrency": valid + "upstream:\n  max_concurrent: 0",
		"zero timeout":     valid + "upstream:\n  timeout: 0s",
		"invalid capacity": valid + "cache:\n  max_bytes: 100",
		"empty directory":  valid + "cache:\n  dir: ''",
		"invalid listen":   valid + "listen: localhost",
		"invalid port":     valid + "listen: localhost:70000",
		"zero port":        valid + "listen: localhost:0",
		"relative URL":     "public_base_url: /feed",
		"non-HTTP URL":     "public_base_url: ftp://feed.example.com",
		"URL credentials":  "public_base_url: https://user:password@feed.example.com",
		"URL query":        "public_base_url: https://feed.example.com?x=1",
		"empty URL query":  "public_base_url: https://feed.example.com?",
		"URL fragment":     "public_base_url: https://feed.example.com/#v3",
		"URL dot segment":  "public_base_url: https://feed.example.com/a/../b",
		"URL encoding":     "public_base_url: https://feed.example.com/a%2fb",
		"URL backslash":    `public_base_url: https://feed.example.com/a\b`,
		"oversized input":  valid + strings.Repeat("#", 1<<20),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(document)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestEmptyAllowlistIsValid(t *testing.T) {
	cfg, err := Decode(strings.NewReader("public_base_url: http://localhost:8080\nallow: []"))
	if err != nil || len(cfg.Allow) != 0 {
		t.Fatalf("empty subset configuration: %+v, %v", cfg, err)
	}
}

func TestConfigurationFile(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := Decode(file); err != nil {
		t.Fatal(err)
	}
}
