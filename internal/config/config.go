package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/policy"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen        string   `yaml:"listen"`
	PublicBaseURL string   `yaml:"public_base_url"`
	Allow         []string `yaml:"allow"`
	Cache         Cache    `yaml:"cache"`
	Upstream      Upstream `yaml:"upstream"`
}

type Cache struct {
	Dir            string        `yaml:"dir"`
	MaxBytes       int64         `yaml:"max_bytes"`
	MaxObjectBytes int64         `yaml:"max_object_bytes"`
	MetadataTTL    time.Duration `yaml:"metadata_ttl"`
}

type Upstream struct {
	Timeout       time.Duration `yaml:"timeout"`
	MaxConcurrent int           `yaml:"max_concurrent"`
}

func Defaults() Config {
	return Config{
		Listen: "127.0.0.1:8080",
		Cache: Cache{
			Dir:            ".cache",
			MaxBytes:       1 << 30,
			MaxObjectBytes: 128 << 20,
			MetadataTTL:    5 * time.Minute,
		},
		Upstream: Upstream{
			Timeout:       2 * time.Minute,
			MaxConcurrent: 8,
		},
	}
}

// Load applies environment overrides after decoding YAML and before validation.
func Load(filename string) (Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	return decode(file, os.LookupEnv)
}

// Decode validates YAML without applying process environment overrides.
func Decode(reader io.Reader) (Config, error) {
	return decode(reader, nil)
}

func decode(reader io.Reader, lookupEnv func(string) (string, bool)) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("configuration exceeds 1 MiB")
	}
	cfg := Defaults()
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Config{}, fmt.Errorf("decode configuration: %w", err)
		}
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	if lookupEnv != nil {
		if err := cfg.applyEnv(lookupEnv); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	return cfg, nil
}

func (c *Config) applyEnv(lookup func(string) (string, bool)) error {
	for _, setting := range []struct {
		name  string
		value *string
	}{
		{"NUGET_PROXY_LISTEN", &c.Listen},
		{"NUGET_PROXY_PUBLIC_BASE_URL", &c.PublicBaseURL},
		{"NUGET_PROXY_CACHE_DIR", &c.Cache.Dir},
	} {
		if value, ok := lookup(setting.name); ok {
			*setting.value = value
		}
	}
	if value, ok := lookup("NUGET_PROXY_ALLOW"); ok {
		c.Allow = []string{}
		if value != "" {
			c.Allow = strings.Split(value, ",")
			for i := range c.Allow {
				c.Allow[i] = strings.TrimSpace(c.Allow[i])
			}
		}
	}
	for _, setting := range []struct {
		name  string
		value *int64
	}{
		{"NUGET_PROXY_CACHE_MAX_BYTES", &c.Cache.MaxBytes},
		{"NUGET_PROXY_CACHE_MAX_OBJECT_BYTES", &c.Cache.MaxObjectBytes},
	} {
		if value, ok := lookup(setting.name); ok {
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return fmt.Errorf("%s must be a base-10 byte count: %w", setting.name, err)
			}
			*setting.value = parsed
		}
	}
	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"NUGET_PROXY_CACHE_METADATA_TTL", &c.Cache.MetadataTTL},
		{"NUGET_PROXY_UPSTREAM_TIMEOUT", &c.Upstream.Timeout},
	} {
		if value, ok := lookup(setting.name); ok {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("%s must be a Go duration: %w", setting.name, err)
			}
			*setting.value = parsed
		}
	}
	if value, ok := lookup("NUGET_PROXY_UPSTREAM_MAX_CONCURRENT"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("NUGET_PROXY_UPSTREAM_MAX_CONCURRENT must be an integer: %w", err)
		}
		c.Upstream.MaxConcurrent = parsed
	}
	return nil
}

func (c Config) Validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen (NUGET_PROXY_LISTEN): invalid listen address: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("listen (NUGET_PROXY_LISTEN) must include a port between 1 and 65535")
	}
	base, err := url.Parse(c.PublicBaseURL)
	if err != nil || base == nil {
		return errors.New("public_base_url (NUGET_PROXY_PUBLIC_BASE_URL) must be an absolute HTTP(S) URL")
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return errors.New("public_base_url (NUGET_PROXY_PUBLIC_BASE_URL) must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if port := base.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("public_base_url (NUGET_PROXY_PUBLIC_BASE_URL) has an invalid port")
		}
	}
	basePath := strings.TrimRight(base.Path, "/")
	if base.RawPath != "" || strings.ContainsAny(base.Path, "\\ \t\r\n") ||
		(basePath != "" && path.Clean(basePath) != basePath) {
		return errors.New("public_base_url (NUGET_PROXY_PUBLIC_BASE_URL) must have a normalized, unescaped path")
	}
	if _, err := policy.New(c.Allow); err != nil {
		return fmt.Errorf("allow (NUGET_PROXY_ALLOW): %w", err)
	}
	if strings.TrimSpace(c.Cache.Dir) == "" {
		return errors.New("cache.dir (NUGET_PROXY_CACHE_DIR) must not be empty")
	}
	if c.Cache.MaxObjectBytes <= 0 || c.Cache.MaxBytes <= 64<<10 ||
		c.Cache.MaxObjectBytes > c.Cache.MaxBytes-(64<<10) {
		return errors.New("cache.max_bytes (NUGET_PROXY_CACHE_MAX_BYTES) must exceed cache.max_object_bytes (NUGET_PROXY_CACHE_MAX_OBJECT_BYTES) by at least 64 KiB; both must be positive")
	}
	if c.Cache.MetadataTTL <= 0 {
		return errors.New("cache.metadata_ttl (NUGET_PROXY_CACHE_METADATA_TTL) must be positive")
	}
	if c.Upstream.Timeout <= 0 {
		return errors.New("upstream.timeout (NUGET_PROXY_UPSTREAM_TIMEOUT) must be positive")
	}
	if c.Upstream.MaxConcurrent <= 0 {
		return errors.New("upstream.max_concurrent (NUGET_PROXY_UPSTREAM_MAX_CONCURRENT) must be positive")
	}
	return nil
}
