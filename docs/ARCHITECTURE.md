# Architecture 🏗️

This doc explains how the registry works under the hood. If you just want to use it, check the [README](../README.md) instead.

---

## The Big Picture

```
┌──────────────────────────────────────────────────────────────────┐
│                      Docker/Podman Client                        │
│                    (docker pull, docker push)                    │
└─────────────────────────────┬────────────────────────────────────┘
                              │ HTTPS
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│                   Fastly Global Edge Network                     │
│                                                                  │
│  • 100+ Points of Presence (POPs) worldwide                     │
│  • SSL/TLS termination                                          │
│  • DDoS protection built-in                                     │
│  • Routes to nearest edge location                              │
└─────────────────────────────┬────────────────────────────────────┘
                              │
                              ▼
┌──────────────────────────────────────────────────────────────────┐
│                 Fastly Compute (WASM Runtime)                    │
│                                                                  │
│  This is where our Go code runs, compiled to WebAssembly.       │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐ │
│  │  main.go - HTTP Router                                     │ │
│  │  ├── manifests.go - GET/PUT/DELETE manifests               │ │
│  │  ├── blobs.go - Download layers with CDN fallback          │ │
│  │  ├── uploads.go - Handle chunked uploads                   │ │
│  │  ├── multipart.go - S3 multipart for large files           │ │
│  │  ├── s3auth.go - AWS Signature V4 signing                  │ │
│  │  └── auth.go - Basic authentication                        │ │
│  └────────────────────────────────────────────────────────────┘ │
└────────────┬─────────────────────────────┬───────────────────────┘
             │                             │
             ▼                             ▼
┌─────────────────────────┐    ┌───────────────────────────────────┐
│    Fastly KV Store      │    │   Fastly Object Storage (S3)      │
│                         │    │                                   │
│ What's stored here:     │    │ What's stored here:               │
│ • Manifests (JSON)      │    │ • Blob layers (the actual data)   │
│ • Tag → digest maps     │    │ • Temporary upload chunks         │
│ • Upload sessions       │    │                                   │
│ • Repository catalog    │    │ Path format:                      │
│                         │    │ /blobs/sha256/ab/cd/abcdef...     │
│ Limits:                 │    │                                   │
│ • 25MB per value        │    │ No size limits                    │
│ • Eventually consistent │    │ S3-compatible API                 │
└─────────────────────────┘    └───────────────────────────────────┘
```

---

## Data Flow: Pulling an Image

When you run `docker pull myregistry.com/app:latest`, here's what happens:

### Step 1: API Version Check

```
Client: GET /v2/
Server: 200 OK
        Docker-Distribution-API-Version: registry/2.0
```

Docker checks if we speak the right protocol. We do.

### Step 2: Get the Manifest

```
Client: GET /v2/app/manifests/latest
Server: [looks up "tags/app/latest" in KV → gets digest]
        [fetches "manifests/app/sha256:abc123..." from KV]
        200 OK
        Content-Type: application/vnd.docker.distribution.manifest.v2+json
        Docker-Content-Digest: sha256:abc123...
        [manifest JSON body]
```

The manifest tells Docker what layers make up the image.

### Step 3: Download Each Layer

For each layer in the manifest:

```
Client: GET /v2/app/blobs/sha256:layer1...
Server: [tries CDN cache first]
        [if miss, fetches from Object Storage]
        [streams the layer back]
        200 OK
        [binary data...]
```

Layers are served through CDN when possible - that's where the speed comes from.

---

## Data Flow: Pushing an Image

When you run `docker push myregistry.com/app:v1.0`:

### Step 1: Check What Already Exists

Docker first checks if layers already exist (to skip uploading them):

```
Client: HEAD /v2/app/blobs/sha256:layer1...
Server: [checks Object Storage]
        200 OK (exists) or 404 (need to upload)
```

### Step 2: Upload New Layers

For each layer that doesn't exist:

```
Client: POST /v2/app/blobs/uploads/
Server: [creates upload session in KV]
        202 Accepted
        Location: /v2/app/blobs/uploads/uuid-123
        Docker-Upload-UUID: uuid-123

Client: PATCH /v2/app/blobs/uploads/uuid-123
        [binary chunk data...]
Server: [streams to Object Storage via S3 multipart]
        202 Accepted
        Range: 0-16777215

Client: PUT /v2/app/blobs/uploads/uuid-123?digest=sha256:...
Server: [verifies digest, finalizes upload]
        201 Created
        Location: /v2/app/blobs/sha256:...
```

### Step 3: Push the Manifest

```
Client: PUT /v2/app/manifests/v1.0
        [manifest JSON]
Server: [validates all referenced blobs exist]
        [stores manifest in KV]
        [updates tag mapping]
        [updates catalog]
        201 Created
```

---

## Storage Layout

### KV Store Keys

```
# Manifests (base64-encoded JSON with metadata)
manifests/myapp/sha256:abc123...
  → {"digest":"sha256:abc123","mediaType":"...","size":1234,"content":"base64..."}

# Tag mappings (just the digest string)
tags/myapp/latest
  → sha256:abc123...

tags/myapp/v1.0
  → sha256:abc123...

# List of all tags for a repo
taglist/myapp
  → ["latest", "v1.0", "v1.1"]

# List of all repositories
catalog
  → ["myapp", "nginx", "postgres"]

# Upload sessions (temporary)
uploads/uuid-123-456
  → {"uuid":"...", "repo":"myapp", "bytesReceived":16777216, ...}
```

### Object Storage Keys

```
# Final blob locations (content-addressable)
blobs/sha256/ab/cd/abcdef1234567890...
blobs/sha256/12/34/1234567890abcdef...

# Temporary upload chunks
uploads/myapp/uuid-123-456/data
```

The `ab/cd/` prefix is for sharding - it spreads files across directories to avoid hotspots.

---

## S3 Signing (AWS Signature V4)

All Object Storage requests need to be signed. Here's the simplified flow:

```
1. Build canonical request:
   METHOD + URI + Query + Headers + Signed Headers + Payload Hash

2. Build string to sign:
   Algorithm + Timestamp + Scope + Hash(Canonical Request)

3. Derive signing key:
   HMAC(HMAC(HMAC(HMAC("AWS4" + secret, date), region), "s3"), "aws4_request")

4. Calculate signature:
   HMAC(signing key, string to sign)

5. Add Authorization header:
   AWS4-HMAC-SHA256 Credential=.../Scope, SignedHeaders=..., Signature=...
```

For uploads, we use `x-amz-content-sha256: UNSIGNED-PAYLOAD` to allow streaming without buffering the entire body for hashing.

---

## Multipart Uploads

For blobs larger than ~16MB, we use S3 multipart uploads:

```
1. Initiate: POST /bucket/key?uploads=
   → Returns UploadId

2. Upload parts: PUT /bucket/key?partNumber=1&uploadId=...
   → Returns ETag for each part
   (We upload 16MB parts)

3. Complete: POST /bucket/key?uploadId=...
   Body: <CompleteMultipartUpload>
           <Part><PartNumber>1</PartNumber><ETag>...</ETag></Part>
           ...
         </CompleteMultipartUpload>
```

### Why 16MB parts?

- **5MB minimum** - S3 requires at least 5MB per part
- **40MB WASM heap** - We can't buffer more than ~40MB total
- **16MB is the sweet spot** - Good throughput, stays well within limits

### Backend Request Limits

Fastly allows **32 backend requests per compute invocation**. With multipart:

- 1 request to initiate
- N requests to upload parts
- 1 request to complete

So we can upload about **28 parts × 16MB = ~450MB** per invocation. For larger blobs, Docker retries and we resume where we left off.

---

## CDN Acceleration

Blob reads go through a CDN layer for caching:

```
Request: GET /v2/app/blobs/sha256:abc123...

1. Try CDN backend first (your-cdn-domain.example.com)
   - If HIT: Return cached blob (~160ms)
   - If MISS: Continue to step 2

2. Fetch from Object Storage
   - Stream blob through CDN
   - CDN caches for future requests (~700ms)

3. Return to client
```

Popular layers (base images, common dependencies) get served from cache most of the time.

---

## Error Handling

All errors follow the OCI spec format:

```json
{
  "errors": [
    {
      "code": "MANIFEST_UNKNOWN",
      "message": "manifest unknown to registry",
      "detail": "sha256:abc123..."
    }
  ]
}
```

Common error codes:

| Code | HTTP Status | Meaning |
|------|-------------|---------|
| `BLOB_UNKNOWN` | 404 | Blob doesn't exist |
| `MANIFEST_UNKNOWN` | 404 | Manifest/tag doesn't exist |
| `NAME_UNKNOWN` | 404 | Repository doesn't exist |
| `DIGEST_INVALID` | 400 | Malformed digest format |
| `UNAUTHORIZED` | 401 | Auth required |
| `DENIED` | 403 | Permission denied |

---

## Authentication Flow

Currently we support basic auth:

```
1. Client: GET /v2/
2. Server: 401 Unauthorized
           WWW-Authenticate: Basic realm="registry"

3. Client: GET /v2/
           Authorization: Basic base64(username:password)
4. Server: [validates against Secret Store credentials]
           200 OK (or 401 if invalid)
```

Future: Bearer token authentication with proper scopes.

---

## What Makes This Different

### Traditional Registry Architecture

```
Client → Load Balancer → Registry Server → Database + Storage
                              ↑
                         (single region)
```

Problems:
- Single point of failure
- High latency for distant users
- Expensive bandwidth

### Edge Registry Architecture

```
Client → Nearest Edge POP → Compute (stateless) → Distributed Storage
              ↑
        (100+ locations)
```

Benefits:
- No single point of failure
- Low latency everywhere
- Zero bandwidth costs

---

## Platform Constraints

Things to know about Fastly Compute:

| Constraint | Limit | How we handle it |
|------------|-------|------------------|
| Request timeout | 2 minutes | Large uploads span multiple requests |
| Backend requests | 32 per invocation | Limits multipart to ~28 parts |
| WASM heap | ~40MB | 16MB upload buffer |
| KV value size | 25MB | Large manifests go to Object Storage |

---

That's the architecture! For API details, see [API_REFERENCE.md](API_REFERENCE.md).
