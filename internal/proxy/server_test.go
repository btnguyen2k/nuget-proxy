package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/config"
	"github.com/btnguyen2k/nuget-proxy/internal/upstream"
)

const (
	flatURL        = "https://api.nuget.org/v3-flatcontainer/"
	auditURL       = "https://api.nuget.org/v3/vulnerabilities/index.json"
	auditPageURL   = "https://api.nuget.org/v3-vulnerabilities/snapshot/base.json"
	signaturesURL  = "https://api.nuget.org/v3-index/repository-signatures/5.0.0/index.json"
	certificateURL = "https://api.nuget.org/v3-index/repository-signatures/certificates/example.crt"
)

type fixtureReply struct {
	status int
	body   []byte
	header http.Header
	err    error
}

type fixture struct {
	mu            sync.Mutex
	replies       map[string]fixtureReply
	calls         map[string]int
	authorization string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{replies: make(map[string]fixtureReply), calls: make(map[string]int)}
	f.setJSON(t, upstream.IndexURL, serviceIndex{Version: "3.0.0", Resources: []resource{
		{ID: flatURL, Type: packageResource}, {ID: auditURL, Type: auditResource},
		{ID: signaturesURL, Type: signatureResource},
		{ID: "https://www.nuget.org/api/v2/package", Type: "PackagePublish/2.0.0"},
		{ID: "https://www.nuget.org/api/v2", Type: "LegacyGallery"},
	}})
	f.setJSON(t, auditURL, []map[string]any{{
		"@id": auditPageURL, "@name": "base", "@updated": "2026-10-01T00:00:00Z", "futureField": true,
	}})
	f.setJSON(t, auditPageURL, map[string]any{
		"outside.subset": []map[string]any{{"severity": 2, "url": "https://example.com/advisory", "versions": "(, 1.0.0)"}},
	})
	f.setJSON(t, signaturesURL, map[string]any{
		"allRepositorySigned": true, "futureField": "preserved",
		"signingCertificates": []map[string]any{{"contentUrl": certificateURL, "subject": "NuGet.org", "fingerprints": map[string]string{"2.16.840.1.101.3.4.2.1": "fingerprint"}}},
	})
	f.replies[certificateURL] = fixtureReply{status: 200, body: []byte{0, 1, 2, 3, 255}, header: http.Header{"Content-Type": {"application/pkix-cert"}}}
	f.setJSON(t, flatURL+"newtonsoft.json/index.json", map[string]any{"versions": []string{"1.0.0", "2.0.0-beta.1"}})
	f.replies[flatURL+"newtonsoft.json/1.0.0/newtonsoft.json.1.0.0.nupkg"] = fixtureReply{
		status: 200, body: []byte("PK\x03\x04unchanged-package-bytes\x00\xff"),
		header: http.Header{"Content-Type": {"application/octet-stream"}, "Etag": {`"content-v1"`}, "Last-Modified": {"Thu, 01 Oct 2026 00:00:00 GMT"}},
	}
	f.replies[flatURL+"newtonsoft.json/1.0.0/newtonsoft.json.nuspec"] = fixtureReply{
		status: 200, body: []byte(`<package><metadata><id>Newtonsoft.Json</id><version>1.0.0</version><dependencies><dependency id="Outside.Subset" version="1.0.0" /></dependencies></metadata></package>`),
		header: http.Header{"Content-Type": {"application/xml"}},
	}
	f.setJSON(t, flatURL+"ddth.core/index.json", map[string]any{"versions": []string{"1.0.0"}})
	return f
}

func (f *fixture) setJSON(t *testing.T, rawURL string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[rawURL] = fixtureReply{status: 200, body: data, header: http.Header{"Content-Type": {"application/json"}, "Etag": {`"upstream-metadata"`}}}
}

func (f *fixture) RoundTrip(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls[request.URL.String()]++
	if request.Header.Get("Authorization") != "" {
		f.authorization = request.Header.Get("Authorization")
	}
	reply, ok := f.replies[request.URL.String()]
	f.mu.Unlock()
	if !ok {
		reply.status = http.StatusNotFound
	}
	if reply.err != nil {
		return nil, reply.err
	}
	return &http.Response{
		StatusCode: reply.status, Header: reply.header.Clone(), Body: io.NopCloser(bytes.NewReader(reply.body)),
		ContentLength: int64(len(reply.body)), Request: request,
	}, nil
}

func (f *fixture) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total int
	for _, count := range f.calls {
		total += count
	}
	return total
}

func testServer(t *testing.T, upstream *fixture, change func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.PublicBaseURL = "https://feed.example.com/nuget"
	cfg.Allow = []string{"Newtonsoft.Json", "Ddth.*"}
	cfg.Cache.Dir = t.TempDir()
	cfg.Cache.MetadataTTL = time.Hour
	cfg.Upstream.Timeout = 5 * time.Second
	if change != nil {
		change(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app, err := newServer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), upstream)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); app.Close() })
	return app
}

func request(app *Server, method, path string, headers http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Host = "untrusted-host.example"
	for name, values := range headers {
		req.Header[name] = values
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, req)
	return response
}

func TestIndexAdvertisesOnlyImplementedLocalResources(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	result := request(app, "GET", "/nuget/v3/index.json", nil)
	if result.Code != 200 {
		t.Fatalf("service index: %d %s", result.Code, result.Body.String())
	}
	var index serviceIndex
	if err := json.Unmarshal(result.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if index.Version != "3.0.0" || len(index.Resources) != 3 {
		t.Fatalf("unexpected index: %+v", index)
	}
	for _, item := range index.Resources {
		if !strings.HasPrefix(item.ID, "https://feed.example.com/nuget/v3/") {
			t.Fatalf("resource bypasses configured public URL: %+v", item)
		}
	}
	if result.Header().Get("ETag") == `"upstream-metadata"` {
		t.Fatal("rewritten response retained upstream ETag")
	}
	conditional := request(app, "GET", "/nuget/v3/index.json", http.Header{"If-None-Match": {result.Header().Get("ETag")}})
	if conditional.Code != 304 || conditional.Body.Len() != 0 {
		t.Fatalf("conditional index request: %d %s", conditional.Code, conditional.Body.String())
	}
}

func TestExcludedPackagesNeverAccessUpstream(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	for _, path := range []string{
		"/nuget/v3/flatcontainer/outside.subset/index.json",
		"/nuget/v3/flatcontainer/outside.subset/1.0.0/outside.subset.nuspec",
		"/nuget/v3/flatcontainer/outside.subset/1.0.0/outside.subset.1.0.0.nupkg",
		"/nuget/v3/flatcontainer/ddth/index.json",
		"/nuget/v3/flatcontainer/ddthextra/index.json",
		"/nuget/v3/flatcontainer/newtonsoft.json.extra/index.json",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			result := request(app, method, path, nil)
			if result.Code != 404 || (method == "HEAD" && result.Body.Len() != 0) {
				t.Errorf("%s %s: %d %s", method, path, result.Code, result.Body.String())
			}
		}
	}
	if f.totalCalls() != 0 {
		t.Fatalf("excluded packages caused %d upstream requests", f.totalCalls())
	}
}

func TestMissingAndExcludedHaveSameExternalResponse(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	excluded := request(app, "GET", "/nuget/v3/flatcontainer/outside.subset/index.json", nil)
	missing := request(app, "GET", "/nuget/v3/flatcontainer/ddth.missing/index.json", nil)
	if excluded.Code != 404 || missing.Code != 404 || excluded.Body.String() != missing.Body.String() {
		t.Fatalf("different absence responses: %d %q vs %d %q", excluded.Code, excluded.Body.String(), missing.Code, missing.Body.String())
	}
	for _, header := range []string{"Content-Type", "Content-Length", "Cache-Control", "X-Content-Type-Options"} {
		if excluded.Header().Get(header) != missing.Header().Get(header) {
			t.Errorf("absence response differs in %s", header)
		}
	}
}

func TestPackageCachingAndHTTPContract(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	path := "/nuget/v3/flatcontainer/NEWTONSOFT.JSON/1.0.0/NEWTONSOFT.JSON.1.0.0.NUPKG"
	want := f.replies[flatURL+"newtonsoft.json/1.0.0/newtonsoft.json.1.0.0.nupkg"].body
	get := request(app, "GET", path, http.Header{"Authorization": {"Bearer must-not-be-forwarded"}})
	if get.Code != 200 || !bytes.Equal(get.Body.Bytes(), want) {
		t.Fatalf("package body changed: %d %q", get.Code, get.Body.Bytes())
	}
	head := request(app, "HEAD", path, nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatalf("HEAD: %d, headers=%v, body=%q", head.Code, head.Header(), head.Body.String())
	}
	notModified := request(app, "GET", path, http.Header{"If-None-Match": {`"content-v1"`}})
	if notModified.Code != 304 || notModified.Body.Len() != 0 {
		t.Fatalf("conditional package download: %d", notModified.Code)
	}
	partial := request(app, "GET", path, http.Header{"Range": {"bytes=1-3"}})
	if partial.Code != 206 || !bytes.Equal(partial.Body.Bytes(), want[1:4]) {
		t.Fatalf("range request: %d %q", partial.Code, partial.Body.Bytes())
	}
	if f.totalCalls() != 2 {
		t.Fatalf("warm requests reached upstream: %d requests", f.totalCalls())
	}
	if f.authorization != "" {
		t.Fatal("client credentials were forwarded to NuGet.org")
	}
	manifest := request(app, "GET", "/nuget/v3/flatcontainer/newtonsoft.json/1.0.0/newtonsoft.json.nuspec", nil)
	if manifest.Code != 200 || !bytes.Equal(manifest.Body.Bytes(), f.replies[flatURL+"newtonsoft.json/1.0.0/newtonsoft.json.nuspec"].body) {
		t.Fatal("manifest or dependency declarations were modified")
	}
	prefix := request(app, "GET", "/nuget/v3/flatcontainer/Ddth.Core/index.json", nil)
	if prefix.Code != 200 {
		t.Fatalf("prefix-matched package: %d", prefix.Code)
	}
}

func TestPolicyChangesApplyToPersistedCache(t *testing.T) {
	f := newFixture(t)
	directory := t.TempDir()
	first := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory })
	path := "/nuget/v3/flatcontainer/newtonsoft.json/1.0.0/newtonsoft.json.1.0.0.nupkg"
	if result := request(first, "GET", path, nil); result.Code != 200 {
		t.Fatal("could not warm cache")
	}
	calls := f.totalCalls()
	second := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory; cfg.Allow = nil })
	if result := request(second, "GET", path, nil); result.Code != 404 {
		t.Fatalf("excluded package escaped through persisted cache: %d", result.Code)
	}
	if f.totalCalls() != calls {
		t.Fatal("excluded cached package contacted upstream")
	}
}

func TestAuditAndSigningLinksStayLocalAndSurviveRestart(t *testing.T) {
	f := newFixture(t)
	directory := t.TempDir()
	app := testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory })
	audit := request(app, "GET", "/nuget/v3/vulnerabilities/index.json", nil)
	var pages []map[string]json.RawMessage
	if audit.Code != 200 || json.Unmarshal(audit.Body.Bytes(), &pages) != nil || len(pages) != 1 {
		t.Fatalf("audit index: %d %s", audit.Code, audit.Body.String())
	}
	var pageURL string
	if err := json.Unmarshal(pages[0]["@id"], &pageURL); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pageURL, app.cfg.PublicBaseURL+"/v3/vulnerabilities/pages/") || string(pages[0]["futureField"]) != "true" {
		t.Fatal("audit page URL or unknown metadata not preserved correctly")
	}
	signatures := request(app, "GET", "/nuget/v3/signatures/index.json", nil)
	var signatureDocument struct {
		AllSigned bool   `json:"allRepositorySigned"`
		Future    string `json:"futureField"`
		Certs     []struct {
			URL string `json:"contentUrl"`
		} `json:"signingCertificates"`
	}
	if signatures.Code != 200 || json.Unmarshal(signatures.Body.Bytes(), &signatureDocument) != nil ||
		!signatureDocument.AllSigned || signatureDocument.Future != "preserved" || len(signatureDocument.Certs) != 1 {
		t.Fatalf("signing metadata: %d %s", signatures.Code, signatures.Body.String())
	}
	certURL := signatureDocument.Certs[0].URL
	if !strings.HasPrefix(certURL, app.cfg.PublicBaseURL+"/v3/signatures/certificates/") {
		t.Fatal("certificate URL bypasses proxy")
	}
	app = testServer(t, f, func(cfg *config.Config) { cfg.Cache.Dir = directory })
	page := request(app, "GET", pageURL, nil)
	if page.Code != 200 || !bytes.Equal(page.Body.Bytes(), f.replies[auditPageURL].body) || !strings.Contains(page.Body.String(), "outside.subset") {
		t.Fatalf("audit data was filtered or old link failed: %d %s", page.Code, page.Body.String())
	}
	cert := request(app, "GET", certURL, nil)
	if cert.Code != 200 || !bytes.Equal(cert.Body.Bytes(), f.replies[certificateURL].body) {
		t.Fatalf("certificate bytes changed or old link failed: %d %q", cert.Code, cert.Body.Bytes())
	}
	calls := f.totalCalls()
	for _, path := range []string{
		"/nuget/v3/vulnerabilities/pages/arbitrary.invalid.json",
		strings.TrimSuffix(strings.Replace(pageURL, "/vulnerabilities/pages/", "/signatures/certificates/", 1), ".json") + ".crt",
		"/nuget/v3/vulnerabilities/pages/" + app.links.sign("audit", auditPageURL) + "tampered.json",
	} {
		if result := request(app, "GET", path, nil); result.Code != 404 {
			t.Fatalf("forged or wrong-purpose URL was accepted: %s => %d", path, result.Code)
		}
	}
	if f.totalCalls() != calls {
		t.Fatal("invalid links reached upstream")
	}
}

func TestHTTPDoesNotAdvertiseHTTPSOnlySigningResource(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, func(cfg *config.Config) { cfg.PublicBaseURL = "http://localhost:8080" })
	result := request(app, "GET", "/v3/index.json", nil)
	if result.Code != 200 || strings.Contains(result.Body.String(), "RepositorySignatures") {
		t.Fatalf("HTTP service index advertises signing metadata: %d %s", result.Code, result.Body.String())
	}
	if result := request(app, "GET", "/v3/signatures/index.json", nil); result.Code != 404 {
		t.Fatalf("HTTP signing resource: %d", result.Code)
	}
}

func TestErrorsAreNotDisguisedAsPackageAbsence(t *testing.T) {
	for _, test := range []struct {
		name   string
		reply  fixtureReply
		status int
	}{
		{"upstream failure", fixtureReply{status: 500}, 502},
		{"upstream forbidden", fixtureReply{status: 403}, 502},
		{"rate limited", fixtureReply{status: 429, header: http.Header{"Retry-After": {"30"}}}, 429},
		{"temporarily unavailable", fixtureReply{status: 503}, 503},
		{"timeout", fixtureReply{err: context.DeadlineExceeded}, 504},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.replies[flatURL+"newtonsoft.json/index.json"] = test.reply
			app := testServer(t, f, nil)
			result := request(app, "GET", "/nuget/v3/flatcontainer/newtonsoft.json/index.json", nil)
			if result.Code != test.status {
				t.Fatalf("got %d %s, want %d", result.Code, result.Body.String(), test.status)
			}
			if test.status == 429 && result.Header().Get("Retry-After") != "30" {
				t.Fatal("Retry-After was lost")
			}
		})
	}
}

func TestInvalidRoutesAndReadOnlyMethods(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	for _, path := range []string{
		"/v3/index.json", "/nuget/v3/flatcontainer/newtonsoft.json/../outside.subset/index.json",
		"/nuget/v3/flatcontainer/newtonsoft.json%2foutside.subset/index.json",
		"/nuget/v3/flatcontainer/newtonsoft.json/1.0.0/outside.subset.1.0.0.nupkg",
		"/nuget/v3/flatcontainer/newtonsoft.json/1.0.0+build/newtonsoft.json.1.0.0+build.nupkg",
		"/nuget/v3/flatcontainer/newtonsoft.json//newtonsoft.json.nuspec",
		"/nuget/v3/registrations/newtonsoft.json/index.json",
	} {
		if result := request(app, "GET", path, nil); result.Code != 404 {
			t.Errorf("%s: %d", path, result.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		result := request(app, method, "/nuget/v3/index.json", nil)
		if result.Code != 405 || result.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d", method, result.Code)
		}
	}
	if f.totalCalls() != 0 {
		t.Fatalf("invalid requests contacted upstream %d times", f.totalCalls())
	}
}

func TestHealthAndReadiness(t *testing.T) {
	f := newFixture(t)
	app := testServer(t, f, nil)
	if result := request(app, "GET", "/healthz", nil); result.Code != 200 || result.Body.String() != "ok\n" {
		t.Fatal("liveness failed")
	}
	if f.totalCalls() != 0 {
		t.Fatal("liveness depends on upstream")
	}
	if result := request(app, "HEAD", "/readyz", nil); result.Code != 200 || result.Body.Len() != 0 {
		t.Fatalf("readiness: %d %s", result.Code, result.Body.String())
	}
	broken := newFixture(t)
	broken.replies[upstream.IndexURL] = fixtureReply{status: 500}
	unready := testServer(t, broken, nil)
	if result := request(unready, "GET", "/readyz", nil); result.Code != 503 {
		t.Fatalf("broken discovery reported ready: %d", result.Code)
	}
}

func TestUntrustedAndMalformedMetadataFailsExplicitly(t *testing.T) {
	for _, name := range []string{"external resource", "invalid index", "missing flat container", "invalid audit page", "invalid certificate", "null signing flag", "null certificates"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := "/nuget/v3/index.json"
			switch name {
			case "external resource":
				f.setJSON(t, upstream.IndexURL, serviceIndex{Version: "3.0.0", Resources: []resource{{ID: "https://example.com/packages/", Type: packageResource}}})
			case "invalid index":
				f.replies[upstream.IndexURL] = fixtureReply{status: 200, body: []byte("not JSON")}
			case "missing flat container":
				f.setJSON(t, upstream.IndexURL, serviceIndex{Version: "3.0.0"})
			case "invalid audit page":
				path = "/nuget/v3/vulnerabilities/index.json"
				f.setJSON(t, auditURL, []map[string]any{{"@id": "http://127.0.0.1/private", "@name": "base", "@updated": "2026-10-01T00:00:00Z"}})
			case "invalid certificate":
				path = "/nuget/v3/signatures/index.json"
				f.setJSON(t, signaturesURL, map[string]any{"allRepositorySigned": true, "signingCertificates": []map[string]string{{"contentUrl": "https://example.com/certificate.crt"}}})
			case "null signing flag":
				path = "/nuget/v3/signatures/index.json"
				f.setJSON(t, signaturesURL, map[string]any{"allRepositorySigned": nil, "signingCertificates": []any{}})
			case "null certificates":
				path = "/nuget/v3/signatures/index.json"
				f.setJSON(t, signaturesURL, map[string]any{"allRepositorySigned": false, "signingCertificates": nil})
			}
			app := testServer(t, f, nil)
			if result := request(app, "GET", path, nil); result.Code != 502 {
				t.Fatalf("%s: %d %s", name, result.Code, result.Body.String())
			}
		})
	}
}
