package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestValidateURL(t *testing.T) {
	for _, rawURL := range []string{
		IndexURL, "https://nuget.org/test", "https://data.nuget.org/v3/index.json", "https://API.NUGET.ORG:443/package",
	} {
		if err := ValidateURL(rawURL); err != nil {
			t.Errorf("rejected %q: %v", rawURL, err)
		}
	}
	for _, rawURL := range []string{
		"http://api.nuget.org/package", "https://api.nuget.org.evil.example/package",
		"https://evilnuget.org/package", "https://127.0.0.1/package", "https://api.nuget.org:444/package",
		"https://user:password@api.nuget.org/package", "https://api.nuget.org/package#fragment",
		"https://api.nuget.org/a\\b", "/relative", "https://",
	} {
		if err := ValidateURL(rawURL); err == nil {
			t.Errorf("accepted %q", rawURL)
		}
	}
}

func TestConcurrencyIncludesResponseBody(t *testing.T) {
	var calls atomic.Int32
	client := New(time.Second, 2, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Header.Get("If-None-Match") != `"etag"` || request.Header.Get("If-Modified-Since") != "date" {
			t.Error("missing revalidation headers")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data"))}, nil
	}))
	first, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	second, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Get(ctx, IndexURL, "", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected concurrency wait timeout, got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("transport called %d times while all slots were occupied", calls.Load())
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := client.Get(context.Background(), IndexURL, `"etag"`, "date")
	if err != nil {
		t.Fatal(err)
	}
	third.Body.Close()
}

func TestRedirectCannotLeaveNuGet(t *testing.T) {
	var calls atomic.Int32
	client := New(time.Second, 1, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://example.com/package"}},
			Body: io.NopCloser(strings.NewReader("")), Request: request,
		}, nil
	}))
	if _, err := client.Get(context.Background(), IndexURL, "", ""); err == nil {
		t.Fatal("redirect outside NuGet.org was accepted")
	}
	if calls.Load() != 1 {
		t.Fatalf("redirect target was contacted: %d requests", calls.Load())
	}
}
