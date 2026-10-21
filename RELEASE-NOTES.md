# nuget-proxy release notes

## 2026-10-21 - v0.1.0

### Added/Refactoring/Deprecation

- Feat(audit): proxy vulnerability indexes and pages through feed-local URLs
- Feat(cache): add bounded persistent LRU caching with request coalescing
- Feat(config): add strictly validated YAML configuration
- Feat(ops): add health probes, structured logging, and graceful shutdown
- Feat(policy): allow case-insensitive package IDs and prefixes with 404 for exclusions
- Feat(proxy): add read-only NuGet V3 package restore support
- Feat(signatures): proxy repository signing metadata and certificates
