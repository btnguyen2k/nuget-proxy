package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func decodeWithEnv(document string, env map[string]string) (Config, error) {
	return decode(strings.NewReader(document), func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
}

func allOverrides() map[string]string {
	return map[string]string{
		"NUGET_PROXY_LISTEN":                  "127.0.0.1:9090",
		"NUGET_PROXY_PUBLIC_BASE_URL":         "https://env.example.com/feed/",
		"NUGET_PROXY_ALLOW":                   "Newtonsoft.Json, Ddth.*",
		"NUGET_PROXY_CACHE_DIR":               "env-cache",
		"NUGET_PROXY_CACHE_MAX_BYTES":         "2097152",
		"NUGET_PROXY_CACHE_MAX_OBJECT_BYTES":  "524288",
		"NUGET_PROXY_CACHE_METADATA_TTL":      "10m",
		"NUGET_PROXY_UPSTREAM_TIMEOUT":        "30s",
		"NUGET_PROXY_UPSTREAM_MAX_CONCURRENT": "3",
	}
}

func TestEnvironmentOverridesAllSettingsBeforeValidation(t *testing.T) {
	cfg, err := decodeWithEnv(`
listen: invalid
public_base_url: /invalid
allow: ["*"]
cache:
  dir: ""
  max_bytes: 0
  max_object_bytes: 0
  metadata_ttl: 0s
upstream:
  timeout: 0s
  max_concurrent: 0
`, allOverrides())
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen: "127.0.0.1:9090", PublicBaseURL: "https://env.example.com/feed",
		Allow: []string{"Newtonsoft.Json", "Ddth.*"},
		Cache: Cache{
			Dir: "env-cache", MaxBytes: 2097152, MaxObjectBytes: 524288, MetadataTTL: 10 * time.Minute,
		},
		Upstream: Upstream{Timeout: 30 * time.Second, MaxConcurrent: 3},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("merged configuration = %+v, want %+v", cfg, want)
	}
}

func TestEnvironmentPreservesUnsetSettings(t *testing.T) {
	cfg, err := decodeWithEnv(`
public_base_url: https://yaml.example.com/
allow: [Original.Package]
cache:
  metadata_ttl: 3m
`, map[string]string{"NUGET_PROXY_UPSTREAM_TIMEOUT": "45s"})
	if err != nil {
		t.Fatal(err)
	}
	want := Defaults()
	want.PublicBaseURL = "https://yaml.example.com"
	want.Allow = []string{"Original.Package"}
	want.Cache.MetadataTTL = 3 * time.Minute
	want.Upstream.Timeout = 45 * time.Second
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("unset settings changed: %+v, want %+v", cfg, want)
	}
}

func TestEnvironmentCanSupplyRequiredSettings(t *testing.T) {
	cfg, err := decodeWithEnv("{}", map[string]string{"NUGET_PROXY_PUBLIC_BASE_URL": "https://env.example.com"})
	if err != nil || cfg.PublicBaseURL != "https://env.example.com" {
		t.Fatalf("required URL was not supplied by environment: %+v, %v", cfg, err)
	}
}

func TestEnvironmentAllowlistReplacement(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"unset", nil, []string{"Original.Package"}},
		{"empty", map[string]string{"NUGET_PROXY_ALLOW": ""}, nil},
		{"replacement", map[string]string{"NUGET_PROXY_ALLOW": "Ddth.*"}, []string{"Ddth.*"}},
		{"trim patterns", map[string]string{"NUGET_PROXY_ALLOW": " Newtonsoft.Json , Ddth.* "}, []string{"Newtonsoft.Json", "Ddth.*"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := decodeWithEnv("public_base_url: https://feed.example.com\nallow: [Original.Package]", test.env)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.Allow, test.want) {
				t.Fatalf("allow = %v, want %v", cfg.Allow, test.want)
			}
		})
	}
}

func TestEnvironmentRejectsInvalidOverrides(t *testing.T) {
	for _, test := range []struct {
		name     string
		variable string
		value    string
	}{
		{"empty listen", "NUGET_PROXY_LISTEN", ""},
		{"invalid listen", "NUGET_PROXY_LISTEN", "localhost"},
		{"invalid listen port", "NUGET_PROXY_LISTEN", "localhost:70000"},
		{"empty URL", "NUGET_PROXY_PUBLIC_BASE_URL", ""},
		{"relative URL", "NUGET_PROXY_PUBLIC_BASE_URL", "/feed"},
		{"URL query", "NUGET_PROXY_PUBLIC_BASE_URL", "https://feed.example.com?query=1"},
		{"URL traversal", "NUGET_PROXY_PUBLIC_BASE_URL", "https://feed.example.com/a/../b"},
		{"allow all", "NUGET_PROXY_ALLOW", "*"},
		{"whitespace allowlist", "NUGET_PROXY_ALLOW", " "},
		{"empty first rule", "NUGET_PROXY_ALLOW", ",Ddth.*"},
		{"empty middle rule", "NUGET_PROXY_ALLOW", "Newtonsoft.Json,,Ddth.*"},
		{"empty last rule", "NUGET_PROXY_ALLOW", "Ddth.*,"},
		{"invalid prefix", "NUGET_PROXY_ALLOW", "Ddth.*.Core"},
		{"JSON allowlist", "NUGET_PROXY_ALLOW", `["Ddth.*"]`},
		{"empty directory", "NUGET_PROXY_CACHE_DIR", ""},
		{"whitespace directory", "NUGET_PROXY_CACHE_DIR", " "},
		{"empty budget", "NUGET_PROXY_CACHE_MAX_BYTES", ""},
		{"invalid budget", "NUGET_PROXY_CACHE_MAX_BYTES", "2GiB"},
		{"overflow budget", "NUGET_PROXY_CACHE_MAX_BYTES", "9223372036854775808"},
		{"negative budget", "NUGET_PROXY_CACHE_MAX_BYTES", "-1"},
		{"insufficient budget", "NUGET_PROXY_CACHE_MAX_BYTES", "134217728"},
		{"empty object size", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", ""},
		{"invalid object size", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", "large"},
		{"overflow object size", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", "9223372036854775808"},
		{"zero object size", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", "0"},
		{"excessive object size", "NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", "1073741824"},
		{"empty TTL", "NUGET_PROXY_CACHE_METADATA_TTL", ""},
		{"invalid TTL", "NUGET_PROXY_CACHE_METADATA_TTL", "5"},
		{"negative TTL", "NUGET_PROXY_CACHE_METADATA_TTL", "-1m"},
		{"empty timeout", "NUGET_PROXY_UPSTREAM_TIMEOUT", ""},
		{"invalid timeout", "NUGET_PROXY_UPSTREAM_TIMEOUT", "forever"},
		{"zero timeout", "NUGET_PROXY_UPSTREAM_TIMEOUT", "0s"},
		{"empty concurrency", "NUGET_PROXY_UPSTREAM_MAX_CONCURRENT", ""},
		{"noninteger concurrency", "NUGET_PROXY_UPSTREAM_MAX_CONCURRENT", "1.5"},
		{"overflow concurrency", "NUGET_PROXY_UPSTREAM_MAX_CONCURRENT", "9223372036854775808"},
		{"zero concurrency", "NUGET_PROXY_UPSTREAM_MAX_CONCURRENT", "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeWithEnv("public_base_url: https://feed.example.com", map[string]string{test.variable: test.value})
			if err == nil || !strings.Contains(err.Error(), test.variable) {
				t.Fatalf("expected error naming %s, got %v", test.variable, err)
			}
		})
	}
}

func TestEnvironmentDoesNotHideInvalidYAML(t *testing.T) {
	for _, document := range []string{
		"alow: [Ddth.*]",
		"allow: [",
		"cache:\n  max_bytes: not-a-number",
		"public_base_url: https://feed.example.com\n---\nallow: []",
		"public_base_url: https://feed.example.com\npublic_base_url: https://other.example.com",
	} {
		if _, err := decodeWithEnv(document, allOverrides()); err == nil {
			t.Fatalf("invalid YAML was masked by overrides: %s", document)
		}
	}
}

func TestLoadReadsProcessEnvironment(t *testing.T) {
	for name, value := range allOverrides() {
		t.Setenv(name, value)
	}
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	want, err := decodeWithEnv("{}", allOverrides())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("process environment was not applied: %+v, want %+v", cfg, want)
	}
	t.Setenv("NUGET_PROXY_ALLOW", "")
	cfg, err = Load(filename)
	if err != nil || len(cfg.Allow) != 0 {
		t.Fatalf("explicit empty environment value did not clear the allowlist: %+v, %v", cfg, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("environment overrides unexpectedly made the configuration file optional")
	}
}

func TestDecodeDoesNotReadProcessEnvironment(t *testing.T) {
	t.Setenv("NUGET_PROXY_LISTEN", "invalid")
	t.Setenv("NUGET_PROXY_ALLOW", "*")
	t.Setenv("NUGET_PROXY_CACHE_METADATA_TTL", "invalid")
	if _, err := Decode(strings.NewReader("public_base_url: https://feed.example.com")); err != nil {
		t.Fatalf("YAML-only decoding was affected by process environment: %v", err)
	}
}
