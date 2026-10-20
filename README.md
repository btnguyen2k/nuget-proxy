# nuget-proxy

A lightweight, read-only NuGet.org subset feed, implemented in Go. Serve only
approved package IDs and prefixes through one shared instance, with an on-demand
disk cache.

## Quick start

Requires Go 1.26 or newer. From PowerShell:

```powershell
Copy-Item .\config.example.yaml .\config.yaml
# Edit config.yaml to define the packages available in your feed.
go run .\cmd\nuget-proxy -config .\config.yaml
```

The source URL is `http://localhost:8080/v3/index.json`. To build a binary:

```powershell
go build -o .\nuget-proxy.exe .\cmd\nuget-proxy
.\nuget-proxy.exe -config .\config.yaml
```

In a consuming project's `nuget.config`, configure the proxy as the only package
source. HTTP is for local development only; the insecure-connection opt-in below
must be removed when using HTTPS:

```xml
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="subset" value="http://localhost:8080/v3/index.json"
         allowInsecureConnections="true" />
  </packageSources>
  <auditSources>
    <clear />
    <add key="subset" value="http://localhost:8080/v3/index.json"
         allowInsecureConnections="true" />
  </auditSources>
</configuration>
```

Use current .NET tooling; the real-client integration tests require .NET SDK 8
or newer. Search, Visual Studio browsing, legacy NuGet V2 clients, publishing,
and registration-based metadata operations are not implemented.

## Subset rules

```yaml
allow:
  - Newtonsoft.Json
  - Ddth.*
```

- Matching is case-insensitive. Any matching rule makes the ID available.
- An ID without `*` is exact: `Newtonsoft.Json` does not include
  `Newtonsoft.Json.Extra`.
- A single terminal `*` is a literal prefix match: `Ddth.*` includes
  `Ddth.Core` and `Ddth.Core.Extensions`, not `Ddth` or `DdthExtra`.
  Other terminal prefixes, such as `Example*`, are also supported.
- Empty or omitted `allow` exposes no packages. A standalone `*`, regexes,
  and non-terminal wildcards are rejected.
- All upstream versions of a matching ID are available, including prereleases.
  There are no version-specific rules.
- Dependencies need their own matching rules; allowing a parent never
  automatically approves its transitive dependencies. Manifests and dependency
  declarations are not modified.
- A prefix restricts package names, not publisher identity.

Excluded IDs return the same `404 Not Found` response as allowed IDs missing
upstream, for version lists, manifests, and downloads. **Subset exclusions never
return `403`.** Rules run before both cache and upstream access, including for
previously cached packages.

Configuration changes require a restart. Unknown fields, malformed rules,
invalid URLs/limits, and multiple YAML documents fail startup explicitly.

## Architecture and protocol

```text
NuGet client -> request validation -> subset matching -> disk cache -> NuGet.org
                                       |
                                     no match
                                       |
                                      404
```

The generated `/v3/index.json` advertises only implemented resources, with URLs
under the configured `public_base_url`:

| Resource | Behavior |
| --- | --- |
| `PackageBaseAddress/3.0.0` | Version lists, `.nuspec`, and byte-for-byte `.nupkg` downloads. |
| `VulnerabilityInfo/6.7.0` | Proxied index and pages; page URLs are rewritten to this feed. Audit data is not filtered by the package allowlist. |
| `RepositorySignatures/5.0.0` | Proxied signing metadata and certificates; certificate URLs are rewritten. Advertised only with an HTTPS public URL. |

Optional resources are advertised only when discovered in NuGet.org's service
index. All endpoints support `GET` and `HEAD`; other methods return `405`.
Package responses support conditional requests and byte ranges. Unknown routes,
malformed package paths, and invalid auxiliary-resource links return `404`.

The upstream is fixed to NuGet.org, with resources discovered from its service
index. Upstream URLs and redirects must use HTTPS on NuGet.org domains; incoming
credentials, arbitrary destination URLs, and unimplemented upstream resources
are not forwarded. Resource links use authenticated URL tokens, backed by a
small persistent key, so clients cannot turn auxiliary routes into an open proxy.
Previously issued links remain usable across restarts.

Upstream missing packages return `404`; upstream rate limits and temporary
unavailability retain `429`/`503` and valid `Retry-After` headers. Other upstream
failures return `502`, timeouts return `504`, local storage failures return `500`,
and a cache fully occupied by active requests returns `503`. Failures are not
converted into empty successful feeds.

## Configuration and cache

See [`config.example.yaml`](config.example.yaml) for the complete configuration.

| Setting | Default / requirement |
| --- | --- |
| `listen` | `127.0.0.1:8080`; use `0.0.0.0:8080` when running in a container. |
| `public_base_url` | Required absolute HTTP(S) URL; may include a path prefix, such as `https://packages.example.com/nuget`. Does not include `/v3/index.json`. |
| `allow` | Empty; exact IDs or terminal-wildcard prefixes. |
| `cache.dir` | `.cache`, relative to the process working directory. Use a dedicated, writable, persistent directory. |
| `cache.max_bytes` | 1 GiB, counting cache-file bytes and in-flight reservations. |
| `cache.max_object_bytes` | 128 MiB. `max_bytes` must leave at least 64 KiB extra for metadata. |
| `cache.metadata_ttl` | `5m`; refresh interval for discovery, version lists, audit data, and signing indexes. |
| `upstream.timeout` | `2m`, including concurrency waits and downloading each cached object. |
| `upstream.max_concurrent` | `8`, including response-body downloads. |

Downloads stream to temporary files, then become visible only after a complete
transfer. **The first response waits for the object to be fully cached** before
streaming it to the client; large packages are never buffered entirely in memory.
Concurrent requests for the same object share one upstream fetch. Disconnecting
one waiting client does not cancel downloads needed by other clients.

Versioned packages, manifests, and certificates remain cached until eviction.
Mutable metadata is revalidated with upstream ETags or modification dates.
Expired metadata is not silently served when revalidation fails. Eviction uses
least-recently-used, unpinned entries; active downloads reserve capacity so
concurrent misses cannot exceed the configured cache-file budget. Filesystem
overhead and the 32-byte `url-signing.key` are outside that budget.

Cache entries survive restarts. Incomplete or malformed cache files are removed
with explicit recovery handling. Keep `url-signing.key` when clearing package
cache files if old audit/certificate links must remain valid. **Only one process
may own a cache directory**; shared storage and multi-instance operation are not
supported.

This is not an offline mirror. Eviction, uncached dependencies, metadata expiry,
and upstream failures can prevent restores. Immutable cached content is not
periodically checked for upstream takedowns.

## Deployment and operations

Run one binary or container behind an ingress/reverse proxy that provides TLS
and any required access control. Set `public_base_url` to its external HTTPS URL;
URLs are never derived from untrusted `Host` or forwarded headers. The Go listener
itself serves HTTP; the ingress must preserve any configured path prefix.
Repository-signature resources require HTTPS and are omitted
from local HTTP feeds; package bytes are unchanged in either mode.

- `/healthz` checks process liveness without upstream access.
- `/readyz` checks cache-directory writability and usable service discovery.
  Fresh cached discovery can satisfy this check without upstream access.
- JSON logs go to stderr and include request status/duration, unavailable-package
  decisions, recovery warnings, and errors.
- Interrupt/termination signals initiate graceful shutdown with a 30-second
  deadline.

For the container, set `listen: "0.0.0.0:8080"` and `cache.dir: "/data"` in a
configuration file, with your external `public_base_url`. The image runs as a
non-root user; bind-mounted cache directories must be writable by UID 65532.

```powershell
docker build -t nuget-proxy .
docker run --rm -p 8080:8080 `
  --mount "type=bind,source=$((Get-Item .\config.yaml).FullName),target=/config/config.yaml,readonly" `
  --mount "type=volume,source=nuget-proxy-cache,target=/data" `
  nuget-proxy
```

The proxy governs only requests reaching it. To enforce a subset in CI, remove
other sources, restrict direct package-feed egress, and use isolated client
package/HTTP caches. NuGet can reuse packages already in its global-packages
folder without contacting this feed; changing a rule cannot revoke those copies.
Package-signature validation may also require the client's normal certificate
trust/revocation infrastructure.

## Development

```powershell
go test .\...
go vet .\...

# Real dotnet restore, using generated packages and a local upstream fixture:
$env:NUGET_PROXY_DOTNET_TEST = "1"
go test .\internal\proxy -run TestDotnetRestore -count=1
Remove-Item Env:\NUGET_PROXY_DOTNET_TEST
```

The integration test isolates NuGet caches and covers approved packages,
transitive dependencies, warm-cache restores, auditing, and unavailable packages
without requiring NuGet.org connectivity.

Protocol references: [package content](https://learn.microsoft.com/en-us/nuget/api/package-base-address-resource),
[vulnerability data](https://learn.microsoft.com/en-us/nuget/api/vulnerability-info),
[repository signatures](https://learn.microsoft.com/en-us/nuget/api/repository-signatures-resource).
