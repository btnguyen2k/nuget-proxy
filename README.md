[![Release](https://img.shields.io/github/release/btnguyen2k/nuget-proxy.svg?style=flat-square)](RELEASE-NOTES.md)
[![Actions Status](https://github.com/btnguyen2k/nuget-proxy/actions/workflows/ci.yaml/badge.svg)](https://github.com/btnguyen2k/nuget-proxy/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat-square)](LICENSE.md)

A lightweight, read-only NuGet.org subset feed, implemented in Go. Serve only
approved package IDs and prefixes, with an on-demand disk cache.
Supports **NuGet V3** restores with modern .NET tooling; publishing, NuGet V2,
package search, and Visual Studio package browsing are not supported.

## Quick start

The following examples use POSIX shell syntax (`sh`/`bash`). Both approaches expose the feed at
`http://localhost:8080/v3/index.json`.

### Build and run from source

Requires Go 1.26 or newer:

```sh
git clone https://github.com/btnguyen2k/nuget-proxy.git
cd nuget-proxy
export NUGET_PROXY_LISTEN="127.0.0.1:8080"
export NUGET_PROXY_ALLOW="Newtonsoft.Json,Ddth.*"
go build -o ./nuget-proxy ./cmd/nuget-proxy
./nuget-proxy -config ./config.yaml
```

### Run the pre-built Docker image

No Go toolchain or local configuration file is required:

```sh
docker run --rm -p 8080:8080 \
  --env "NUGET_PROXY_ALLOW=Newtonsoft.Json,Ddth.*" \
  --env "NUGET_PROXY_PUBLIC_BASE_URL=http://localhost:8080" \
  --mount "type=volume,source=nuget-proxy-cache,target=/data" \
  btnguyen2k/nuget-proxy:latest
```

The image includes `config.yaml`; environment variables override its settings.
The volume preserves cached packages between container runs.

## Use with .NET projects

Choose project-local configuration or user-wide source registration. The local
HTTP examples below use .NET SDK 9 or newer. For shared deployments, use an HTTPS
feed URL and omit the insecure-connection options.

### Project-local nuget.config

Place `nuget.config` in your solution/repository root, or beside the `.csproj`
when restoring that project directly:

```xml
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="nuget-proxy" value="http://localhost:8080/v3/index.json"
         allowInsecureConnections="true" />
  </packageSources>
  <auditSources>
    <clear />
    <add key="nuget-proxy" value="http://localhost:8080/v3/index.json"
         allowInsecureConnections="true" />
  </auditSources>
</configuration>
```

Then run `dotnet restore` from that directory. `<clear />` removes inherited
sources so packages and vulnerability audits use the proxy.

### Register a source globally with the .NET CLI

On Linux/macOS, explicitly target your user-level NuGet configuration to make the
source available across projects:

```sh
dotnet nuget add source "http://localhost:8080/v3/index.json" \
  --name nuget-proxy --allow-insecure-connections \
  --configfile "$HOME/.nuget/NuGet/NuGet.Config"
dotnet restore
```

Project-local settings can override user-wide settings. Adding the source does
**not** disable existing sources: disable/remove other package feeds if the
proxy must enforce the subset. Packages already in NuGet's local cache may be
reused without contacting the proxy.

## Configuration

Use [`config.yaml`](config.yaml) for native runs. The Docker image includes the
same file at `/config/config.yaml`; optionally replace it with a read-only bind
mount. Settings are documented inline, and every setting has an environment
override:

| Setting | Purpose and included value | Environment variable |
| --- | --- | --- |
| `listen` | HTTP bind address; `0.0.0.0:8080`. Use `127.0.0.1:8080` for local-only native access. | `NUGET_PROXY_LISTEN` |
| `public_base_url` | Client-facing base URL; `http://localhost:8080`. May include a path prefix, but not `/v3/index.json`. | `NUGET_PROXY_PUBLIC_BASE_URL` |
| `allow` | Available package IDs/prefixes; `[]` exposes no packages. | `NUGET_PROXY_ALLOW` |
| `cache.dir` | Writable cache directory; `.cache`, relative to the working directory (`/data/.cache` in Docker). | `NUGET_PROXY_CACHE_DIR` |
| `cache.max_bytes` | Total cache-file budget; `1073741824` bytes (1 GiB). | `NUGET_PROXY_CACHE_MAX_BYTES` |
| `cache.max_object_bytes` | Largest downloadable object; `134217728` bytes (128 MiB). | `NUGET_PROXY_CACHE_MAX_OBJECT_BYTES` |
| `cache.metadata_ttl` | Metadata refresh interval; `5m`. | `NUGET_PROXY_CACHE_METADATA_TTL` |
| `upstream.timeout` | Per-fetch deadline, including concurrency waits and download; `2m`. | `NUGET_PROXY_UPSTREAM_TIMEOUT` |
| `upstream.max_concurrent` | Maximum simultaneous upstream requests, including downloads; `8`. | `NUGET_PROXY_UPSTREAM_MAX_CONCURRENT` |

Byte counts are positive base-10 integers, without suffixes such as `MiB`.
`cache.max_bytes` must exceed `cache.max_object_bytes` by at least 64 KiB.
Durations use Go syntax, such as `30s`, `5m`, or `2h`, and must be positive.
Configuration changes require a restart; invalid settings fail startup.

### Subset rules

```yaml
allow:
  - Newtonsoft.Json
  - Ddth.*
```

- Matching is case-insensitive; any matching rule makes an ID available.
- Exact IDs match only that package. A terminal `*` matches a literal prefix:
  `Ddth.*` includes `Ddth.Core`, not `Ddth` or `DdthExtra`.
- An empty or omitted list exposes no packages. Standalone `*`, regexes, and
  non-terminal wildcards are rejected.
- Matching an ID allows all its versions, including prereleases. Dependencies
  must independently match a rule; allowing a parent does not approve them.

Packages outside the subset are **unavailable (`404`), not forbidden (`403`)**,
including packages previously cached by the proxy. Prefixes restrict names,
not publisher identity.

### Environment overrides

Precedence is **built-in defaults < YAML < environment**. Unset variables preserve
YAML/default values; explicitly set variables override them, even when empty.
Invalid overrides fail startup with an error naming the variable. The YAML file
must still exist and parse successfully.

`NUGET_PROXY_ALLOW="Newtonsoft.Json,Ddth.*"` **replaces** the YAML allowlist with
comma-separated patterns, trimming surrounding whitespace. An explicitly empty
value clears the list; whitespace-only values and empty entries such as `Ddth.*,`
are invalid. Do not use JSON/YAML array syntax.

For example, set overrides before starting the binary, or pass them through
Docker's `--env` option as shown in Quick start:

```sh
export NUGET_PROXY_CACHE_MAX_BYTES="2147483648"
export NUGET_PROXY_CACHE_METADATA_TTL="10m"
```

To return to a YAML value, unset the variable, for example with
`unset NUGET_PROXY_ALLOW`.

## Deployment and operations

- For shared use, run behind a reverse proxy providing HTTPS and access control.
  Set `public_base_url` to the external URL and preserve any configured path prefix.
- Persist `/data` in Docker. Bind-mounted cache directories must be writable by
  UID 65532. Use one proxy process per cache directory; the cache is not an offline mirror.
- `/healthz` reports liveness; `/readyz` checks cache writability and usable feed
  discovery. JSON logs go to stderr, and termination triggers graceful shutdown.

## License

This project is licensed under the MIT License. See the [LICENSE.md](LICENSE.md) file for details.

## Support and Contribution

Feel free to create [pull requests](https://github.com/btnguyen2k/nuget-proxy/pulls) or [issues](https://github.com/btnguyen2k/nuget-proxy/issues) to report bugs or suggest new features.
Please search the existing issues before filing new ones to avoid duplicates. For new issues, file your bug or feature request as a new issue.

If you find this project useful, please star it.
