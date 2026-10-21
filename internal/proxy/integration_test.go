package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btnguyen2k/nuget-proxy/internal/config"
)

func TestDotnetRestore(t *testing.T) {
	if os.Getenv("NUGET_PROXY_DOTNET_TEST") != "1" {
		t.Skip("set NUGET_PROXY_DOTNET_TEST=1 to exercise the real .NET SDK")
	}
	dotnet, err := exec.LookPath("dotnet")
	if err != nil {
		t.Fatal("NUGET_PROXY_DOTNET_TEST requires dotnet on PATH")
	}
	version, err := exec.Command(dotnet, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.Split(strings.TrimSpace(string(version)), ".")[0])
	if err != nil || major < 8 {
		t.Fatalf("integration test requires .NET SDK 8 or newer, got %q", version)
	}
	framework := fmt.Sprintf("net%d.0", major)
	f := newFixture(t)
	for _, id := range []string{"Ddth.Root", "Ddth.Dependency", "Outside.Subset"} {
		dependency := ""
		if id == "Ddth.Root" {
			dependency = "Ddth.Dependency"
		}
		addPackage(t, f, id, dependency, framework)
	}
	app, _ := startFixtureServer(t, f, []string{"Ddth.*"})
	runRestore := func(t *testing.T, sourceURL, packageID string, success bool) string {
		t.Helper()
		directory := t.TempDir()
		project := filepath.Join(directory, "app.csproj")
		nugetConfig := filepath.Join(directory, "nuget.config")
		projectXML := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>%s</TargetFramework><NuGetAudit>true</NuGetAudit><NuGetAuditMode>all</NuGetAuditMode></PropertyGroup><ItemGroup><PackageReference Include="%s" Version="1.0.0" /></ItemGroup></Project>`, framework, packageID)
		configXML := fmt.Sprintf(`<configuration><packageSources><clear/><add key="proxy" value="%s/v3/index.json" allowInsecureConnections="true"/></packageSources><auditSources><clear/><add key="proxy" value="%s/v3/index.json" allowInsecureConnections="true"/></auditSources></configuration>`, sourceURL, sourceURL)
		for filename, data := range map[string]string{project: projectXML, nugetConfig: configXML} {
			if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, dotnet, "restore", project, "--configfile", nugetConfig,
			"--packages", filepath.Join(directory, "packages"), "--no-http-cache", "--force", "--verbosity", "minimal")
		command.Dir = directory
		command.Env = append(os.Environ(),
			"DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "DOTNET_NOLOGO=1",
			"DOTNET_CLI_HOME="+filepath.Join(directory, "dotnet"),
			"NUGET_HTTP_CACHE_PATH="+filepath.Join(directory, "http-cache"),
			"NUGET_XMLDOC_MODE=skip")
		output, err := command.CombinedOutput()
		if (err == nil) != success {
			t.Fatalf("restore success=%v, want %v: %v\n%s", err == nil, success, err, output)
		}
		if ctx.Err() != nil {
			t.Fatalf("restore timed out:\n%s", output)
		}
		if success {
			for _, id := range []string{"ddth.root", "ddth.dependency"} {
				downloaded, err := os.ReadFile(filepath.Join(directory, "packages", id, "1.0.0", id+".1.0.0.nupkg"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(downloaded, f.replies[flatURL+id+"/1.0.0/"+id+".1.0.0.nupkg"].body) {
					t.Fatalf("NuGet downloaded modified package bytes for %s", id)
				}
			}
			if strings.Contains(string(output), "NU1900") || strings.Contains(string(output), "NU1905") {
				t.Fatalf("vulnerability auditing failed:\n%s", output)
			}
		} else if !strings.Contains(string(output), "NU1101") {
			t.Fatalf("expected package absence (NU1101), got:\n%s", output)
		}
		return string(output)
	}
	t.Run("allowed package and transitive dependency", func(t *testing.T) {
		runRestore(t, app.cfg.PublicBaseURL, "Ddth.Root", true)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.calls[auditURL] == 0 || f.calls[auditPageURL] == 0 {
			t.Fatal("real restore did not use the proxy's vulnerability resources")
		}
	})
	t.Run("warm proxy cache with empty client caches", func(t *testing.T) {
		before := f.totalCalls()
		runRestore(t, app.cfg.PublicBaseURL, "Ddth.Root", true)
		if f.totalCalls() != before {
			t.Fatal("warm-cache restore fetched upstream content again")
		}
	})
	t.Run("excluded package is absent", func(t *testing.T) {
		runRestore(t, app.cfg.PublicBaseURL, "Outside.Subset", false)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.calls[flatURL+"outside.subset/index.json"] != 0 {
			t.Fatal("excluded package reached upstream during real restore")
		}
	})
	t.Run("dependencies require their own allow rule", func(t *testing.T) {
		restricted := newFixture(t)
		addPackage(t, restricted, "Ddth.Root", "Ddth.Dependency", framework)
		addPackage(t, restricted, "Ddth.Dependency", "", framework)
		restrictedApp, _ := startFixtureServer(t, restricted, []string{"Ddth.Root"})
		output := runRestore(t, restrictedApp.cfg.PublicBaseURL, "Ddth.Root", false)
		if !strings.Contains(output, "Ddth.Dependency") {
			t.Fatalf("restore did not identify the unavailable dependency:\n%s", output)
		}
		restricted.mu.Lock()
		defer restricted.mu.Unlock()
		if restricted.calls[flatURL+"ddth.dependency/index.json"] != 0 {
			t.Fatal("allowing a parent package implicitly allowed its dependency")
		}
	})
}

func startFixtureServer(t *testing.T, f *fixture, allow []string) (*Server, *httptest.Server) {
	t.Helper()
	host := httptest.NewUnstartedServer(nil)
	app := testServer(t, f, func(cfg *config.Config) {
		cfg.PublicBaseURL = "http://" + host.Listener.Addr().String() + "/nuget"
		cfg.Allow = allow
	})
	host.Config.Handler = app
	host.Start()
	t.Cleanup(host.Close)
	response, err := http.Get(host.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fixture server is not responsive: %d", response.StatusCode)
	}
	return app, host
}

func addPackage(t *testing.T, f *fixture, id, dependency, framework string) {
	t.Helper()
	dependencies := ""
	if dependency != "" {
		dependencies = fmt.Sprintf(`<dependencies><dependency id="%s" version="[1.0.0]" /></dependencies>`, dependency)
	}
	manifest := fmt.Sprintf(`<?xml version="1.0"?><package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata><id>%s</id><version>1.0.0</version><authors>nuget-proxy-tests</authors><description>Offline protocol fixture</description>%s</metadata></package>`, id, dependencies)
	var content bytes.Buffer
	writer := zip.NewWriter(&content)
	for _, entry := range []struct{ name, data string }{
		{id + ".nuspec", manifest},
		{"lib/" + framework + "/_._", ""},
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="nuspec" ContentType="application/octet-stream"/></Types>`},
		{"_rels/.rels", fmt.Sprintf(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Type="http://schemas.microsoft.com/packaging/2010/07/manifest" Target="/%s.nuspec" Id="manifest"/></Relationships>`, id)},
	} {
		file, err := writer.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	lowerID := strings.ToLower(id)
	f.setJSON(t, flatURL+lowerID+"/index.json", map[string]any{"versions": []string{"1.0.0"}})
	f.replies[flatURL+lowerID+"/1.0.0/"+lowerID+".1.0.0.nupkg"] = fixtureReply{
		status: 200, body: content.Bytes(), header: http.Header{"Content-Type": {"application/octet-stream"}},
	}
	f.replies[flatURL+lowerID+"/1.0.0/"+lowerID+".nuspec"] = fixtureReply{
		status: 200, body: []byte(manifest), header: http.Header{"Content-Type": {"application/xml"}},
	}
}
