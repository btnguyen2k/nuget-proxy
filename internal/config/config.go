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

func Load(filename string) (Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	return Decode(file)
}

func Decode(reader io.Reader) (Config, error) {
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
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	return cfg, nil
}

func (c Config) Validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("listen must include a port between 1 and 65535")
	}
	base, err := url.Parse(c.PublicBaseURL)
	if err != nil || base == nil {
		return errors.New("public_base_url must be an absolute HTTP(S) URL")
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return errors.New("public_base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if port := base.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("public_base_url has an invalid port")
		}
	}
	basePath := strings.TrimRight(base.Path, "/")
	if base.RawPath != "" || strings.ContainsAny(base.Path, "\\ \t\r\n") ||
		(basePath != "" && path.Clean(basePath) != basePath) {
		return errors.New("public_base_url must have a normalized, unescaped path")
	}
	if _, err := policy.New(c.Allow); err != nil {
		return fmt.Errorf("allow: %w", err)
	}
	if strings.TrimSpace(c.Cache.Dir) == "" {
		return errors.New("cache.dir must not be empty")
	}
	if c.Cache.MaxObjectBytes <= 0 || c.Cache.MaxBytes <= 64<<10 ||
		c.Cache.MaxObjectBytes > c.Cache.MaxBytes-(64<<10) {
		return errors.New("cache.max_bytes must exceed cache.max_object_bytes by at least 64 KiB; both must be positive")
	}
	if c.Cache.MetadataTTL <= 0 {
		return errors.New("cache.metadata_ttl must be positive")
	}
	if c.Upstream.Timeout <= 0 || c.Upstream.MaxConcurrent <= 0 {
		return errors.New("upstream.timeout and upstream.max_concurrent must be positive")
	}
	return nil
}
