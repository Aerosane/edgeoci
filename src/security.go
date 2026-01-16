// Security Middleware
//
// Provides security headers, rate limiting, and request validation.
// Follows OWASP security best practices for container registries.

package main

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

// Security configuration
const (
	// Rate limiting
	RateLimitEnabled    = true
	RateLimitWindow     = 60  // seconds
	RateLimitMaxRequests = 100 // requests per window per IP

	// Request size limits
	MaxManifestSize = 4 * 1024 * 1024  // 4MB - manifests shouldn't be huge
	MaxHeaderSize   = 8 * 1024         // 8KB for headers

	// Security headers
	EnableSecurityHeaders = true
)

// In-memory rate limiter (resets on cold start, which is fine for edge)
var (
	rateLimitStore = make(map[string]*rateLimitEntry)
	rateLimitMu    sync.Mutex
)

type rateLimitEntry struct {
	Count     int
	ResetTime time.Time
}

// AddSecurityHeaders adds security headers to the response
// These follow OWASP recommendations for API security
func AddSecurityHeaders(w fsthttp.ResponseWriter) {
	if !EnableSecurityHeaders {
		return
	}

	// Prevent MIME type sniffing
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Prevent clickjacking (not really applicable for API but good practice)
	w.Header().Set("X-Frame-Options", "DENY")

	// XSS protection (legacy but doesn't hurt)
	w.Header().Set("X-XSS-Protection", "1; mode=block")

	// Referrer policy - don't leak URLs
	w.Header().Set("Referrer-Policy", "no-referrer")

	// Permissions policy - disable unnecessary features
	w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

	// Cache control for sensitive endpoints
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
}

// CheckRateLimit checks if the client has exceeded rate limits
// Returns true if request should proceed, false if rate limited
func CheckRateLimit(r *fsthttp.Request) (bool, int, int) {
	if !RateLimitEnabled {
		return true, 0, RateLimitMaxRequests
	}

	clientIP := getClientIP(r)
	now := time.Now()

	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	entry, exists := rateLimitStore[clientIP]
	if !exists || now.After(entry.ResetTime) {
		// New window
		rateLimitStore[clientIP] = &rateLimitEntry{
			Count:     1,
			ResetTime: now.Add(time.Duration(RateLimitWindow) * time.Second),
		}
		return true, 1, RateLimitMaxRequests
	}

	entry.Count++
	remaining := RateLimitMaxRequests - entry.Count

	if entry.Count > RateLimitMaxRequests {
		return false, entry.Count, 0
	}

	return true, entry.Count, remaining
}

// WriteRateLimitResponse writes a 429 Too Many Requests response
func WriteRateLimitResponse(w fsthttp.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", RateLimitMaxRequests))
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(fsthttp.StatusTooManyRequests)
	w.Write([]byte(`{"errors":[{"code":"TOOMANYREQUESTS","message":"rate limit exceeded, retry later"}]}`))
}

// getClientIP extracts the client IP from request headers
// Handles X-Forwarded-For, X-Real-IP, and falls back to Fastly headers
func getClientIP(r *fsthttp.Request) string {
	// Fastly provides the real client IP
	if ip := r.Header.Get("Fastly-Client-IP"); ip != "" {
		return ip
	}

	// Check X-Forwarded-For (take first IP in chain)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}

	// Check X-Real-IP
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}

	return "unknown"
}

// ValidateRepositoryName checks if repository name follows OCI naming rules
// Returns an error if the name is invalid (v2)
func ValidateRepositoryName(name string) *OCIError {
	if name == "" {
		return &OCIError{
			Code:    "NAME_INVALID",
			Message: "repository name cannot be empty",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Max length check
	if len(name) > 256 {
		return &OCIError{
			Code:    "NAME_INVALID",
			Message: "repository name exceeds maximum length of 256 characters",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Check for path traversal attempts
	if strings.Contains(name, "..") {
		return &OCIError{
			Code:    "NAME_INVALID",
			Message: "repository name contains invalid characters",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Check for null bytes
	if strings.ContainsRune(name, '\x00') {
		return &OCIError{
			Code:    "NAME_INVALID",
			Message: "repository name contains invalid characters",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// OCI spec: lowercase letters, digits, separators (., _, -, /)
	for _, c := range name {
		if !isValidNameChar(c) {
			return &OCIError{
				Code:    "NAME_INVALID",
				Message: "repository name contains invalid characters (must be lowercase alphanumeric with . _ - / separators)",
				Detail:  fmt.Sprintf("invalid character: %c", c),
				Status:  fsthttp.StatusBadRequest,
			}
		}
	}

	return nil
}

// isValidNameChar checks if a character is valid in a repository name
func isValidNameChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') ||
		c == '.' || c == '_' || c == '-' || c == '/'
}

// ValidateDigestFormat checks if a digest string is properly formatted
func ValidateDigestFormat(digest string) *OCIError {
	if digest == "" {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "digest cannot be empty",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Must have algorithm:hash format
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "digest must be in algorithm:hash format",
			Detail:  digest,
			Status:  fsthttp.StatusBadRequest,
		}
	}

	algorithm := parts[0]
	hash := parts[1]

	// Only support sha256 for now
	if algorithm != "sha256" {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "only sha256 digest algorithm is supported",
			Detail:  fmt.Sprintf("received: %s", algorithm),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// SHA256 hash must be 64 hex characters
	if len(hash) != 64 {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "sha256 hash must be 64 hexadecimal characters",
			Detail:  fmt.Sprintf("received %d characters", len(hash)),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Check all characters are valid hex
	for _, c := range hash {
		if !isHexChar(c) {
			return &OCIError{
				Code:    "DIGEST_INVALID",
				Message: "digest contains non-hexadecimal characters",
				Status:  fsthttp.StatusBadRequest,
			}
		}
	}

	return nil
}

// isHexChar checks if a character is a valid hexadecimal digit
func isHexChar(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// ValidateReference checks if a tag/reference is valid
func ValidateReference(reference string) *OCIError {
	if reference == "" {
		return &OCIError{
			Code:    "TAG_INVALID",
			Message: "tag/reference cannot be empty",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// If it's a digest, validate as digest
	if strings.HasPrefix(reference, "sha256:") {
		return ValidateDigestFormat(reference)
	}

	// Tag validation
	if len(reference) > 128 {
		return &OCIError{
			Code:    "TAG_INVALID",
			Message: "tag exceeds maximum length of 128 characters",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Check for invalid characters in tags
	for _, c := range reference {
		if !isValidTagChar(c) {
			return &OCIError{
				Code:    "TAG_INVALID",
				Message: "tag contains invalid characters",
				Detail:  fmt.Sprintf("invalid character: %c", c),
				Status:  fsthttp.StatusBadRequest,
			}
		}
	}

	return nil
}

// isValidTagChar checks if a character is valid in a tag
func isValidTagChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '.' || c == '_' || c == '-'
}

// SecureCompare performs constant-time string comparison
// Prevents timing attacks on credential validation
func SecureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// SanitizeLogOutput removes sensitive data from log output
func SanitizeLogOutput(input string) string {
	// Remove potential secrets from logs
	sanitized := input

	// Mask authorization headers
	if strings.Contains(strings.ToLower(sanitized), "authorization") {
		sanitized = "[REDACTED]"
	}

	// Mask anything that looks like a token
	if strings.Contains(sanitized, "token") || strings.Contains(sanitized, "secret") {
		sanitized = "[REDACTED]"
	}

	return sanitized
}

// LogSecurityEvent logs security-relevant events
func LogSecurityEvent(eventType, clientIP, details string) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("[SECURITY] %s | type=%s | ip=%s | %s\n", timestamp, eventType, clientIP, details)
}

var allowedOrigins = []string{
	"https://registry.aerosane.dev",
	"https://aerosane.dev",
	"http://localhost:3000",
	"http://localhost:8080",
}

func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range allowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func getRequiredAction(routeType string) string {
	switch routeType {
	case "get_manifest", "head_manifest", "get_blob", "head_blob", "list_tags", "catalog", "referrers":
		return "pull"
	case "put_manifest", "initiate_upload", "upload_chunk", "complete_upload", "mount_blob":
		return "push"
	case "delete_manifest", "delete_blob":
		return "delete"
	default:
		return "pull"
	}
}
