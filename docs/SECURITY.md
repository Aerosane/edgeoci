# Security

This document describes the security features implemented in the Edge Container Registry.

---

## Authentication

### Basic Authentication

The registry uses HTTP Basic Authentication with credentials stored in Fastly Secret Store.

**Configuration:**
1. Create entries in your Fastly Secret Store:
   - `REGISTRY_USERNAME` - Your desired username
   - `REGISTRY_PASSWORD` - Your desired password

2. Set `AuthEnabled = true` in `auth.go` (default)

**Usage:**
```bash
docker login your-registry.edgecompute.app
# Enter username and password when prompted
```

**Security Notes:**
- Credentials are never hardcoded - secret store is required
- Constant-time string comparison prevents timing attacks
- Failed authentication attempts are logged with client IP

---

## Input Validation

All inputs are validated before processing to prevent injection attacks and resource abuse.

### Repository Names

Must follow OCI naming rules:
- Lowercase alphanumeric characters only (`a-z`, `0-9`)
- Allowed separators: `.` `_` `-` `/`
- Maximum length: 256 characters
- No path traversal sequences (`..`)
- No null bytes

**Example valid names:**
```
library/ubuntu
my-org/my-app
prod/api-server_v2
```

**Example invalid names:**
```
Library/Ubuntu     # uppercase not allowed
test/../etc        # path traversal blocked
my~app             # special characters not allowed
```

### Digests

Must follow the format `algorithm:hash`:
- Only `sha256` algorithm supported
- Hash must be exactly 64 hexadecimal characters

### Tags/References

- Maximum length: 128 characters
- Alphanumeric with `.` `_` `-` separators
- If it starts with `sha256:`, validated as a digest

---

## Rate Limiting

Protects against brute-force attacks and resource exhaustion.

**Default Configuration:**
- 100 requests per minute per IP address
- Uses Fastly-Client-IP header for accurate client identification
- Falls back to X-Forwarded-For or X-Real-IP if needed

**Response Headers:**
```
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 95
```

When rate limited, returns HTTP 429 with:
```json
{
  "errors": [{
    "code": "TOOMANYREQUESTS",
    "message": "rate limit exceeded, retry later"
  }]
}
```

---

## Security Headers

Every response includes security headers following OWASP recommendations:

| Header | Value | Purpose |
|--------|-------|---------|
| `X-Content-Type-Options` | `nosniff` | Prevent MIME-type sniffing |
| `X-Frame-Options` | `DENY` | Prevent clickjacking |
| `X-XSS-Protection` | `1; mode=block` | Legacy XSS protection |
| `Referrer-Policy` | `no-referrer` | Don't leak URLs |
| `Permissions-Policy` | `geolocation=(), microphone=(), camera=()` | Disable unused features |
| `Cache-Control` | `no-store, no-cache, must-revalidate, private` | Prevent caching of sensitive data |

---

## Security Logging

Security-relevant events are logged with structured format:

```
[SECURITY] 2024-01-15T10:30:00Z | type=AUTH_FAIL | ip=192.168.1.100 | path=/v2/myrepo/manifests/latest
[SECURITY] 2024-01-15T10:30:01Z | type=RATE_LIMIT | ip=192.168.1.100 | exceeded 100 requests
[SECURITY] 2024-01-15T10:30:02Z | type=INVALID_NAME | ip=192.168.1.100 | name=../../../etc
```

Event types:
- `AUTH_FAIL` - Failed authentication attempt
- `RATE_LIMIT` - Rate limit exceeded
- `INVALID_NAME` - Invalid repository name submitted

---

## Content Integrity

### Digest Verification

All uploaded content is verified against its claimed digest:
- Blobs must match their SHA256 digest
- Manifests are hashed and stored by digest
- Prevents tampering and corruption

### Manifest Validation

Manifests are validated according to OCI spec:
- Schema version must be 2
- Required fields (mediaType, digest, size) must be present
- Referenced blobs are checked for existence

---

## Infrastructure Security

### Fastly Secret Store

Credentials are stored in Fastly's Secret Store, not in code:
- Secrets are encrypted at rest
- Access controlled by Fastly IAM
- Never logged or exposed in responses

### Object Storage

Blobs are stored in Fastly Object Storage with:
- AWS Signature V4 authentication
- HTTPS-only access
- Credentials in Secret Store (not in code)

### No Hardcoded Secrets

The open source version contains no hardcoded credentials:
- S3 credentials: Must be configured via Secret Store
- Registry credentials: Must be configured via Secret Store
- No fallback/default passwords

---

## Best Practices

### For Operators

1. **Use strong passwords** - At least 16 characters with mixed case, numbers, symbols
2. **Rotate credentials** - Update Secret Store entries periodically
3. **Monitor logs** - Watch for AUTH_FAIL and RATE_LIMIT events
4. **Use HTTPS** - Fastly provides TLS termination by default
5. **Limit access** - Use network policies if needed

### For Users

1. **Use credential helpers** - Don't store passwords in plaintext
2. **Use image digests** - Reference images by digest for immutability
3. **Verify images** - Check digests match expected values
4. **Rotate tokens** - If using personal access tokens

---

## Reporting Security Issues

If you discover a security vulnerability:

1. **Do not** open a public issue
2. Email security details to [your security contact]
3. Include steps to reproduce if possible
4. We aim to respond within 48 hours

---

## Security Checklist

Before deploying to production:

- [ ] Secret Store created with strong credentials
- [ ] `AuthEnabled = true` in auth.go
- [ ] Object Storage credentials in Secret Store
- [ ] No hardcoded values in source code
- [ ] Logging enabled for security events
- [ ] Rate limiting configured appropriately
- [ ] HTTPS enabled (default on Fastly)
