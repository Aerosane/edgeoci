# API Reference 📚

Complete documentation for all registry endpoints. This implements the [OCI Distribution Spec v2](https://github.com/opencontainers/distribution-spec).

---

## Base URL

All endpoints are relative to your registry domain:
```
https://your-registry.edgecompute.app
```

---

## Authentication

If authentication is enabled, all endpoints except `/v2/` require credentials.

### Basic Auth

```bash
curl -u username:password https://registry/v2/_catalog
```

Or with Docker:
```bash
docker login your-registry.edgecompute.app
```

### Response when auth required

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Basic realm="registry"
```

---

## Endpoints

### API Version Check

Check if the registry is available and speaking OCI protocol.

```
GET /v2/
```

**Response:**
```
HTTP/1.1 200 OK
Docker-Distribution-API-Version: registry/2.0
Content-Type: application/json

{}
```

This is always the first call Docker makes.

---

### Health Check

```
GET /health
GET /
```

**Response:**
```json
{
  "status": "healthy",
  "version": "0.1.0"
}
```

---

## Manifests

### Get Manifest

Retrieve an image manifest by tag or digest.

```
GET /v2/<name>/manifests/<reference>
```

**Parameters:**
- `name` - Repository name (e.g., `myapp`, `library/nginx`)
- `reference` - Tag name (e.g., `latest`) or digest (e.g., `sha256:abc123...`)

**Headers (request):**
```
Accept: application/vnd.docker.distribution.manifest.v2+json
Accept: application/vnd.oci.image.manifest.v1+json
Accept: application/vnd.docker.distribution.manifest.list.v2+json
```

**Response:**
```
HTTP/1.1 200 OK
Content-Type: application/vnd.docker.distribution.manifest.v2+json
Docker-Content-Digest: sha256:abc123...
Content-Length: 1234

{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "size": 7023,
    "digest": "sha256:config..."
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
      "size": 32654,
      "digest": "sha256:layer1..."
    }
  ]
}
```

**Errors:**
- `404 MANIFEST_UNKNOWN` - Manifest or tag doesn't exist

---

### Check Manifest Exists

Check if a manifest exists without downloading it.

```
HEAD /v2/<name>/manifests/<reference>
```

**Response:**
```
HTTP/1.1 200 OK
Docker-Content-Digest: sha256:abc123...
Content-Length: 1234
```

Or:
```
HTTP/1.1 404 Not Found
```

---

### Push Manifest

Upload a new manifest (creates or updates a tag).

```
PUT /v2/<name>/manifests/<reference>
```

**Headers:**
```
Content-Type: application/vnd.docker.distribution.manifest.v2+json
```

**Body:** The manifest JSON

**Response:**
```
HTTP/1.1 201 Created
Location: /v2/<name>/manifests/sha256:abc123...
Docker-Content-Digest: sha256:abc123...
```

**Errors:**
- `400 MANIFEST_INVALID` - Malformed manifest
- `400 MANIFEST_BLOB_UNKNOWN` - Manifest references blobs that don't exist

---

### Delete Manifest

Delete a manifest by digest.

```
DELETE /v2/<name>/manifests/<digest>
```

**Response:**
```
HTTP/1.1 202 Accepted
```

**Note:** This only deletes the manifest, not the referenced blobs.

---

## Blobs (Layers)

### Get Blob

Download a blob (image layer or config).

```
GET /v2/<name>/blobs/<digest>
```

**Parameters:**
- `digest` - Content digest (e.g., `sha256:abc123...`)

**Response:**
```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Docker-Content-Digest: sha256:abc123...
Content-Length: 32654

[binary data]
```

**Note:** Blobs are served through CDN when available for faster delivery.

---

### Check Blob Exists

Check if a blob exists.

```
HEAD /v2/<name>/blobs/<digest>
```

**Response:**
```
HTTP/1.1 200 OK
Docker-Content-Digest: sha256:abc123...
Content-Length: 32654
```

Or:
```
HTTP/1.1 404 Not Found
```

---

### Delete Blob

Delete a blob by digest.

```
DELETE /v2/<name>/blobs/<digest>
```

**Response:**
```
HTTP/1.1 202 Accepted
```

---

## Uploads

### Initiate Upload

Start a new blob upload session.

```
POST /v2/<name>/blobs/uploads/
```

**Response:**
```
HTTP/1.1 202 Accepted
Location: /v2/<name>/blobs/uploads/<uuid>
Docker-Upload-UUID: <uuid>
Range: 0-0
```

The `Location` header tells you where to send chunks.

---

### Upload Chunk

Upload a chunk of blob data.

```
PATCH /v2/<name>/blobs/uploads/<uuid>
```

**Headers:**
```
Content-Type: application/octet-stream
Content-Length: 16777216
```

**Body:** Binary chunk data

**Response:**
```
HTTP/1.1 202 Accepted
Location: /v2/<name>/blobs/uploads/<uuid>
Range: 0-16777215
Docker-Upload-UUID: <uuid>
```

The `Range` header shows how many bytes have been received.

---

### Complete Upload

Finalize an upload and provide the expected digest.

```
PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest>
```

**Parameters:**
- `digest` - Expected content digest (e.g., `sha256:abc123...`)

**Response (success):**
```
HTTP/1.1 201 Created
Location: /v2/<name>/blobs/<digest>
Docker-Content-Digest: <digest>
```

**Errors:**
- `400 DIGEST_INVALID` - Computed digest doesn't match provided digest
- `404 BLOB_UPLOAD_UNKNOWN` - Upload session not found or expired

---

### Monolithic Upload

Upload entire blob in one request (alternative to chunked upload).

```
PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest>
Content-Type: application/octet-stream

[entire blob data]
```

Works the same as complete upload, but includes body.

---

### Check Upload Status

Get the current status of an upload.

```
GET /v2/<name>/blobs/uploads/<uuid>
```

**Response:**
```
HTTP/1.1 204 No Content
Location: /v2/<name>/blobs/uploads/<uuid>
Range: 0-16777215
Docker-Upload-UUID: <uuid>
```

---

### Cross-Repository Mount

Mount a blob from another repository (avoids re-uploading).

```
POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<source-repo>
```

**Parameters:**
- `mount` - Digest of blob to mount
- `from` - Source repository name

**Response (blob exists):**
```
HTTP/1.1 201 Created
Location: /v2/<name>/blobs/<digest>
Docker-Content-Digest: <digest>
```

**Response (blob not found, falls back to upload):**
```
HTTP/1.1 202 Accepted
Location: /v2/<name>/blobs/uploads/<uuid>
Docker-Upload-UUID: <uuid>
```

---

## Tags

### List Tags

Get all tags for a repository.

```
GET /v2/<name>/tags/list
```

**Query parameters (optional):**
- `n` - Maximum number of results
- `last` - Last tag from previous page (for pagination)

**Response:**
```json
{
  "name": "myapp",
  "tags": ["latest", "v1.0", "v1.1", "v2.0"]
}
```

**Pagination:**

If there are more results, the response includes a `Link` header:
```
Link: </v2/myapp/tags/list?n=10&last=v1.1>; rel="next"
```

---

## Catalog

### List Repositories

Get all repositories in the registry.

```
GET /v2/_catalog
```

**Query parameters (optional):**
- `n` - Maximum number of results
- `last` - Last repo from previous page

**Response:**
```json
{
  "repositories": ["myapp", "nginx", "postgres"]
}
```

---

## Referrers (OCI 1.1)

### Get Referrers

Get artifacts that reference a specific manifest (e.g., signatures, SBOMs).

```
GET /v2/<name>/referrers/<digest>
```

**Query parameters (optional):**
- `artifactType` - Filter by artifact type

**Response:**
```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:ref1...",
      "size": 1234,
      "artifactType": "application/vnd.example.signature"
    }
  ]
}
```

---

## Error Responses

All errors follow this format:

```json
{
  "errors": [
    {
      "code": "ERROR_CODE",
      "message": "Human readable message",
      "detail": "Additional context"
    }
  ]
}
```

### Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `BLOB_UNKNOWN` | 404 | Blob does not exist |
| `BLOB_UPLOAD_INVALID` | 400 | Blob upload invalid |
| `BLOB_UPLOAD_UNKNOWN` | 404 | Upload session not found |
| `DIGEST_INVALID` | 400 | Provided digest is invalid |
| `MANIFEST_BLOB_UNKNOWN` | 400 | Manifest references unknown blob |
| `MANIFEST_INVALID` | 400 | Manifest is invalid |
| `MANIFEST_UNKNOWN` | 404 | Manifest does not exist |
| `NAME_INVALID` | 400 | Invalid repository name |
| `NAME_UNKNOWN` | 404 | Repository not found |
| `SIZE_INVALID` | 400 | Provided size doesn't match |
| `UNAUTHORIZED` | 401 | Authentication required |
| `DENIED` | 403 | Access denied |
| `UNSUPPORTED` | 400 | Operation not supported |

---

## Common Headers

### Request Headers

| Header | Description |
|--------|-------------|
| `Authorization` | `Basic base64(user:pass)` for auth |
| `Accept` | Acceptable manifest media types |
| `Content-Type` | Media type of request body |
| `Content-Length` | Size of request body |

### Response Headers

| Header | Description |
|--------|-------------|
| `Docker-Distribution-API-Version` | Always `registry/2.0` |
| `Docker-Content-Digest` | Content digest of returned data |
| `Docker-Upload-UUID` | UUID of upload session |
| `Location` | URL for next request |
| `Range` | Bytes received so far |
| `Link` | Pagination link |

---

## Examples with curl

### Pull manifest
```bash
curl -H "Accept: application/vnd.docker.distribution.manifest.v2+json" \
  https://registry/v2/myapp/manifests/latest
```

### Push blob (monolithic)
```bash
# Start upload
LOCATION=$(curl -s -D- -X POST https://registry/v2/myapp/blobs/uploads/ \
  | grep -i location | cut -d' ' -f2 | tr -d '\r')

# Calculate digest
DIGEST=$(sha256sum myblob.tar.gz | cut -d' ' -f1)

# Upload
curl -X PUT "${LOCATION}?digest=sha256:${DIGEST}" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @myblob.tar.gz
```

### List tags
```bash
curl https://registry/v2/myapp/tags/list
```

---

For more examples, see the [Getting Started](GETTING_STARTED.md) guide.
