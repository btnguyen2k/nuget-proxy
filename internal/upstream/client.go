package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

const IndexURL = "https://api.nuget.org/v3/index.json"

type Client struct {
	http  *http.Client
	slots *semaphore.Weighted
}

func New(timeout time.Duration, concurrency int, transport http.RoundTripper) *Client {
	if transport == nil {
		defaultTransport := http.DefaultTransport.(*http.Transport).Clone()
		defaultTransport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		defaultTransport.MaxIdleConns = concurrency * 2
		defaultTransport.MaxIdleConnsPerHost = concurrency
		defaultTransport.MaxConnsPerHost = concurrency
		defaultTransport.ResponseHeaderTimeout = timeout
		defaultTransport.MaxResponseHeaderBytes = 1 << 20
		transport = defaultTransport
	}
	return &Client{
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many upstream redirects")
				}
				return ValidateURL(request.URL.String())
			},
		},
		slots: semaphore.NewWeighted(int64(concurrency)),
	}
}

func ValidateURL(raw string) error {
	if len(raw) > 8192 {
		return errors.New("upstream URL is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid upstream URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Opaque != "" || strings.Contains(parsed.Path, "\\") ||
		(parsed.Port() != "" && parsed.Port() != "443") ||
		(host != "nuget.org" && !strings.HasSuffix(host, ".nuget.org")) {
		return errors.New("upstream URL must use HTTPS on nuget.org or a nuget.org subdomain")
	}
	return nil
}

func (c *Client) Get(ctx context.Context, rawURL, etag, lastModified string) (*http.Response, error) {
	if err := ValidateURL(rawURL); err != nil {
		return nil, err
	}
	if err := c.slots.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		c.slots.Release(1)
		return nil, err
	}
	request.Header.Set("User-Agent", "nuget-proxy")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		request.Header.Set("If-Modified-Since", lastModified)
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.slots.Release(1)
		return nil, err
	}
	response.Body = &releaseBody{ReadCloser: response.Body, release: func() { c.slots.Release(1) }}
	return response, nil
}

func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
