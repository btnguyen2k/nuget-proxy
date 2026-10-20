package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/cache"
	"github.com/btnguyen2k/nuget-proxy/internal/config"
	"github.com/btnguyen2k/nuget-proxy/internal/policy"
	"github.com/btnguyen2k/nuget-proxy/internal/upstream"
)

var packageVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}(-[0-9a-z-]+(\.[0-9a-z-]+)*)?$`)

type Server struct {
	cfg    config.Config
	logger *slog.Logger
	allow  *policy.Allowlist
	cache  *cache.Cache
	client *upstream.Client
	links  *links
	prefix string
	https  bool
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Server, error) {
	return newServer(ctx, cfg, logger, nil)
}

func newServer(ctx context.Context, cfg config.Config, logger *slog.Logger, transport http.RoundTripper) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	allow, err := policy.New(cfg.Allow)
	if err != nil {
		return nil, err
	}
	storage, err := cache.New(ctx, cache.Options{
		Dir: cfg.Cache.Dir, MaxBytes: cfg.Cache.MaxBytes, MaxObjectBytes: cfg.Cache.MaxObjectBytes,
		FetchTimeout: cfg.Upstream.Timeout,
	}, logger)
	if err != nil {
		return nil, err
	}
	linkSigner, err := loadLinks(cfg.Cache.Dir)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: cfg, logger: logger, allow: allow, cache: storage, links: linkSigner,
		client: upstream.New(cfg.Upstream.Timeout, cfg.Upstream.MaxConcurrent, transport),
		prefix: base.Path + "/v3/", https: base.Scheme == "https",
	}, nil
}

func (s *Server) Close() {
	s.client.CloseIdleConnections()
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	response := &responseLog{ResponseWriter: writer}
	start := time.Now()
	defer func() {
		if response.err != nil {
			s.logger.Warn("write response", "path", request.URL.Path, "error", response.err)
		}
		s.logger.Info("request", "method", request.Method, "path", request.URL.Path,
			"status", response.status, "bytes", response.bytes, "duration", time.Since(start).String())
	}()
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		response.Header().Set("Allow", "GET, HEAD")
		s.writeError(response, request, http.StatusMethodNotAllowed)
		return
	}
	if request.URL.RawPath != "" || strings.Contains(request.URL.Path, "\\") {
		s.writeError(response, request, http.StatusNotFound)
		return
	}
	switch request.URL.Path {
	case "/healthz":
		writeText(response, request, "ok\n")
		return
	case "/readyz":
		if err := s.cache.Check(); err != nil {
			s.logger.Error("cache not ready", "error", err)
			s.writeError(response, request, http.StatusServiceUnavailable)
		} else if _, err := s.discover(request.Context()); err != nil {
			s.logger.Error("upstream discovery not ready", "error", err)
			s.writeError(response, request, http.StatusServiceUnavailable)
		} else {
			writeText(response, request, "ready\n")
		}
		return
	}
	if !strings.HasPrefix(request.URL.Path, s.prefix) {
		s.writeError(response, request, http.StatusNotFound)
		return
	}
	relative := strings.TrimPrefix(request.URL.Path, s.prefix)
	var err error
	switch {
	case relative == "index.json":
		var index serviceIndex
		index, err = s.index(request.Context())
		if err == nil {
			err = serveJSON(response, request, index)
		}
	case strings.HasPrefix(relative, "flatcontainer/"):
		err = s.packageContent(response, request, strings.TrimPrefix(relative, "flatcontainer/"))
	case relative == "vulnerabilities/index.json":
		err = s.metadataIndex(response, request, false)
	case strings.HasPrefix(relative, "vulnerabilities/pages/") && strings.HasSuffix(relative, ".json"):
		token := strings.TrimSuffix(strings.TrimPrefix(relative, "vulnerabilities/pages/"), ".json")
		err = s.linkContent(response, request, "audit", token, s.cfg.Cache.MetadataTTL)
	case relative == "signatures/index.json" && s.https:
		err = s.metadataIndex(response, request, true)
	case strings.HasPrefix(relative, "signatures/certificates/") && strings.HasSuffix(relative, ".crt") && s.https:
		token := strings.TrimSuffix(strings.TrimPrefix(relative, "signatures/certificates/"), ".crt")
		err = s.linkContent(response, request, "certificate", token, 0)
	default:
		s.writeError(response, request, http.StatusNotFound)
	}
	if err != nil {
		s.respondError(response, request, err)
	}
}

func (s *Server) packageContent(writer http.ResponseWriter, request *http.Request, relative string) error {
	id, normalized, immutable, ok := packagePath(relative)
	if !ok || !s.allow.Allows(id) {
		s.logger.Info("package unavailable", "package", id, "reason", "outside_subset_or_invalid_path")
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	remote, err := s.discover(request.Context())
	if err != nil {
		return err
	}
	ttl := s.cfg.Cache.MetadataTTL
	if immutable {
		ttl = 0
	}
	return s.serveObject(writer, request, remote.packages+normalized, ttl)
}

func packagePath(relative string) (string, string, bool, bool) {
	parts := strings.Split(strings.ToLower(relative), "/")
	id := parts[0]
	if !policy.ValidID(id) {
		return id, "", false, false
	}
	if len(parts) == 2 && parts[1] == "index.json" {
		return id, strings.Join(parts, "/"), false, true
	}
	if len(parts) != 3 || len(parts[1]) > 256 || !packageVersion.MatchString(parts[1]) {
		return id, "", false, false
	}
	if parts[2] != id+".nuspec" && parts[2] != id+"."+parts[1]+".nupkg" {
		return id, "", false, false
	}
	return id, strings.Join(parts, "/"), true, true
}

func (s *Server) metadataIndex(writer http.ResponseWriter, request *http.Request, signatures bool) error {
	remote, err := s.discover(request.Context())
	if err != nil {
		return err
	}
	var document any
	if signatures {
		if remote.signatures == "" {
			s.writeError(writer, request, http.StatusNotFound)
			return nil
		}
		document, err = s.signaturesIndex(request.Context(), remote.signatures)
	} else {
		if remote.audit == "" {
			s.writeError(writer, request, http.StatusNotFound)
			return nil
		}
		document, err = s.auditIndex(request.Context(), remote.audit)
	}
	if err != nil {
		return err
	}
	return serveJSON(writer, request, document)
}

func (s *Server) linkContent(writer http.ResponseWriter, request *http.Request, purpose, token string, ttl time.Duration) error {
	rawURL, ok := s.links.resolve(purpose, token)
	if !ok {
		s.writeError(writer, request, http.StatusNotFound)
		return nil
	}
	return s.serveObject(writer, request, rawURL, ttl)
}

func (s *Server) get(ctx context.Context, rawURL string, ttl time.Duration) (*cache.Object, error) {
	return s.cache.Get(ctx, rawURL, ttl, func(ctx context.Context, etag, modified string) (*http.Response, error) {
		return s.client.Get(ctx, rawURL, etag, modified)
	})
}

func (s *Server) serveObject(writer http.ResponseWriter, request *http.Request, rawURL string, ttl time.Duration) error {
	object, err := s.get(request.Context(), rawURL, ttl)
	if err != nil {
		return err
	}
	defer s.closeObject(object)
	if object.ContentType != "" {
		writer.Header().Set("Content-Type", object.ContentType)
	}
	if object.ETag != "" {
		writer.Header().Set("ETag", object.ETag)
	}
	var modified time.Time
	if parsed, err := http.ParseTime(object.LastModified); err == nil {
		modified = parsed
	}
	http.ServeContent(writer, request, request.URL.Path, modified, object)
	return nil
}

func (s *Server) closeObject(object *cache.Object) {
	if err := object.Close(); err != nil {
		s.logger.Error("close cached object", "error", err)
	}
}

func (s *Server) publicURL(suffix string) string {
	return s.cfg.PublicBaseURL + "/v3" + suffix
}

func (s *Server) respondError(writer http.ResponseWriter, request *http.Request, err error) {
	status := http.StatusInternalServerError
	var remote *cache.HTTPError
	var fetch *cache.FetchError
	var network net.Error
	switch {
	case errors.As(err, &remote):
		switch remote.StatusCode {
		case http.StatusNotFound:
			status = http.StatusNotFound
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			status = remote.StatusCode
			if remote.RetryAfter != "" {
				if seconds, parseErr := strconv.Atoi(remote.RetryAfter); parseErr == nil && seconds >= 0 {
					writer.Header().Set("Retry-After", remote.RetryAfter)
				} else if _, parseErr := http.ParseTime(remote.RetryAfter); parseErr == nil {
					writer.Header().Set("Retry-After", remote.RetryAfter)
				}
			}
		default:
			status = http.StatusBadGateway
		}
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()):
		status = http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled), errors.Is(err, cache.ErrFull):
		status = http.StatusServiceUnavailable
	case errors.Is(err, errBadGateway), errors.Is(err, cache.ErrTooLarge), errors.As(err, &fetch):
		status = http.StatusBadGateway
	}
	s.logger.Error("request failed", "path", request.URL.Path, "status", status, "error", err)
	s.writeError(writer, request, status)
}

func (s *Server) writeError(writer http.ResponseWriter, request *http.Request, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	body := http.StatusText(status) + "\n"
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		if _, err := writer.Write([]byte(body)); err != nil {
			s.logger.Warn("write error response", "error", err)
		}
	}
}

func serveJSON(writer http.ResponseWriter, request *http.Request, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	sum := sha256.Sum256(data)
	writer.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	writer.Header().Set("Content-Type", "application/json")
	http.ServeContent(writer, request, request.URL.Path, time.Time{}, bytes.NewReader(data))
	return nil
}

func writeText(writer http.ResponseWriter, request *http.Request, text string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(text)))
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write([]byte(text))
	}
}

type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int
	err    error
}

func (r *responseLog) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseLog) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(data)
	r.bytes += n
	if err != nil {
		r.err = err
	}
	return n, err
}

func (r *responseLog) Unwrap() http.ResponseWriter { return r.ResponseWriter }
