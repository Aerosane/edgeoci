# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased]

### Planned
- Bearer token authentication
- Garbage collection
- Web UI

---

## [0.1.0] - 2024-01-XX

### Added
- Initial release
- Full OCI Distribution Spec v2 support
- Docker pull/push operations
- Basic authentication
- CDN-accelerated blob reads
- S3 multipart upload for large blobs
- Cross-repository blob mounting
- OCI 1.1 referrers API
- Tag listing with pagination
- Repository catalog

### Known Limitations
- No OAuth/Bearer token auth
- Large single layers (>500MB) may need retries
- No garbage collection
- No web UI

---

## How to Update

When making changes, add them to the `[Unreleased]` section. When releasing:

1. Move unreleased changes to a new version section
2. Add the release date
3. Update version numbers in code
4. Tag the release in git
