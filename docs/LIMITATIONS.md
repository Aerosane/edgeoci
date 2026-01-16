# Limitations ⚠️

This page documents what the registry can and cannot do, along with workarounds where available.

---

## Platform Constraints

These limits are imposed by Fastly Compute:

### Request Timeout: 2 minutes

Every request must complete within 2 minutes.

**Impact:**
- Single layer uploads are limited to what can transfer in 2 minutes
- Roughly 300-500MB per request on typical connections

**Workaround:**
- Docker automatically retries failed uploads
- Upload progress is tracked, so retries resume where they left off
- Most images work because they use multiple smaller layers

---

### Backend Requests: 32 per invocation

Each compute request can make at most 32 outbound requests.

**Impact:**
- S3 multipart uploads are limited to ~28 parts
- Maximum ~450MB per upload session (28 × 16MB)

**Workaround:**
- Uploads continue across multiple client requests
- Docker's retry mechanism handles this automatically

---

### WASM Heap: ~40MB

The WebAssembly runtime has approximately 40MB of usable memory.

**Impact:**
- Cannot buffer large blobs in memory
- Upload chunks capped at 16MB

**Workaround:**
- Data streams through without buffering
- Multipart uploads handle large files

---

### KV Store Value Size: 25MB

Each KV Store entry has a 25MB maximum.

**Impact:**
- Very large manifests may not fit
- Manifest lists with many architectures could hit this limit

**Workaround:**
- Large manifests could be stored in Object Storage instead
- Not currently implemented

---

## Feature Limitations

### Authentication

**Supported:**
- Basic authentication (username/password)
- Credentials in Fastly Secret Store

**Not supported:**
- Bearer token / OAuth2 flow
- Per-repository permissions
- User management
- Token expiration/refresh

**Workaround:**
Use basic auth. Single username/password for all access.

---

### Large Layer Uploads

**Reliable:**
- Layers up to ~300MB upload in one go
- Layers up to ~500MB work with 1-2 retries
- Multi-layer images with large total size

**Unreliable:**
- Single layers >500MB may need many retries
- Single layers >1GB are not reliable

**Workaround:**
Docker handles retries automatically. Create images with smaller layers when possible.

| Image | Total Size | Largest Layer | Status |
|-------|------------|---------------|--------|
| node:20 | 1.1GB | 180MB | ✅ Works |
| ubuntu | 78MB | 78MB | ✅ Works |
| dotnet/sdk | 850MB | 200MB | ✅ Works |
| monolithic-app | 800MB | 800MB | ⚠️ Needs retries |

---

### Eventual Consistency

KV Store is eventually consistent, not strongly consistent.

**Impact:**
- Images may not be immediately pullable after push
- Tag updates take 1-2 seconds to propagate

**Workaround:**
Wait 1-2 seconds after push before pull. Retry on MANIFEST_UNKNOWN.

---

### No Garbage Collection

Deleted manifests leave orphaned blobs in storage.

**Impact:**
- Storage usage grows over time
- Deleting images does not reclaim space

**Workaround:**
Manual cleanup via Object Storage console. GC feature is planned.

---

### No Range Requests

Partial downloads are not supported.

**Impact:**
- Cannot resume interrupted downloads
- Client must re-download entire blob on failure

**Workaround:**
CDN caching helps with retries. Most blobs are small enough that this isn't a problem.

---

### No Web UI

API-only interface.

**Impact:**
- Cannot browse images in a web browser
- No visual way to see stored content

**Workaround:**
Use CLI tools:

```bash
# List repos
curl https://registry/v2/_catalog

# List tags
curl https://registry/v2/myapp/tags/list

# Get manifest
curl https://registry/v2/myapp/manifests/latest
```

---

### No Rate Limiting

No built-in request throttling.

**Impact:**
- Possible to overwhelm the registry
- No abuse protection beyond Fastly's DDoS mitigation

**Workaround:**
Fastly provides edge-level DDoS protection. Custom rate limiting can be added in code.

---

### No Audit Logging

No built-in access logging.

**Impact:**
- Cannot track who pushed/pulled images
- No compliance/audit trail

**Workaround:**
Fastly provides request logs that can be sent to log aggregators.

---

## Known Bugs

### Upload Session Expiration

Upload sessions expire after approximately 1 hour.

**Workaround:**
Complete pushes promptly. Retry if expired.

---

### Digest Verification

Client-provided digests are currently trusted without server-side verification.

**Impact:**
- Corrupted data could potentially be stored
- Not a security issue in trusted environments

**Fix planned:** SHA256 verification on upload completion.

---

## What Works Reliably

- ✅ Pulling any size image
- ✅ Pushing images up to ~500MB per layer
- ✅ Multi-architecture images
- ✅ Basic authentication
- ✅ CDN-accelerated pulls
- ✅ Cross-repository blob mounting
- ✅ OCI 1.1 artifacts and referrers
- ✅ Standard Docker/Podman workflows

---

## Reporting Issues

Found a problem not listed here? Open an issue with:

1. What you were trying to do
2. What happened instead
3. Error messages
4. Image size and layer information (if relevant)
