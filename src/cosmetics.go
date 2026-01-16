// Response Cosmetics
//
// Provides Docker Hub-style responses and professional error messages.
// Makes the registry output look polished and user-friendly.

package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

const (
	RegistryName    = "Fastly OCI Registry"
	RegistryVersion = "1.1.0"
	RegistryVendor  = "Fastly Edge Compute"
)

// HealthResponse provides detailed health information
type HealthResponse struct {
	Status      string            `json:"status"`
	Service     string            `json:"service"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Timestamp   string            `json:"timestamp"`
	Platform    PlatformInfo      `json:"platform"`
	Features    []string          `json:"features"`
}

// PlatformInfo describes the platform
type PlatformInfo struct {
	Runtime  string `json:"runtime"`
	Region   string `json:"region"`
	Provider string `json:"provider"`
}

// APIVersionResponse for GET /v2/
type APIVersionResponse struct {
	// Empty object per OCI spec, but headers carry the info
}

// EnhancedError provides detailed, helpful error messages
type EnhancedError struct {
	Errors []ErrorDetail `json:"errors"`
}

// ErrorDetail is a single error with helpful information
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Help    string `json:"help,omitempty"`
}

// WriteHealthResponse writes a detailed health check response
func WriteHealthResponse(w fsthttp.ResponseWriter) {
	health := HealthResponse{
		Status:      "healthy",
		Service:     RegistryName,
		Version:     RegistryVersion,
		Description: "OCI Distribution Spec v1.1 compliant container registry",
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Platform: PlatformInfo{
			Runtime:  "WebAssembly (WASI)",
			Region:   "Global Edge",
			Provider: RegistryVendor,
		},
		Features: []string{
			"oci-distribution-spec-v1.1",
			"manifest-v2",
			"content-addressable-storage",
			"cross-repo-blob-mount",
			"cdn-accelerated-pulls",
			"multipart-upload",
			"resumable-upload",
			"referrers-api",
		},
	}

	w.Header().Set("Content-Type", ContentTypeJSON)
	w.Header().Set("X-Registry-Version", RegistryVersion)
	w.WriteHeader(fsthttp.StatusOK)
	json.NewEncoder(w).Encode(health)
}

// WriteAPIVersionResponse writes the /v2/ response with proper headers
func WriteAPIVersionResponse(w fsthttp.ResponseWriter) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.Header().Set(HeaderDockerAPIVersion, DockerAPIVersionValue)
	w.Header().Set("X-Registry-Name", RegistryName)
	w.Header().Set("X-Registry-Version", RegistryVersion)
	w.Header().Set("X-Registry-Vendor", RegistryVendor)
	w.WriteHeader(fsthttp.StatusOK)
	w.Write([]byte("{}"))
}

// WriteEnhancedError writes a detailed, helpful error response
func WriteEnhancedError(w fsthttp.ResponseWriter, err *OCIError) {
	detail := ErrorDetail{
		Code:    err.Code,
		Message: err.Message,
		Detail:  err.Detail,
	}

	// Add helpful hints based on error code
	switch err.Code {
	case "UNAUTHORIZED":
		detail.Help = "Ensure you are logged in with 'docker login <registry>'"
	case "MANIFEST_UNKNOWN":
		detail.Help = "The specified image tag or digest does not exist. Check the repository and tag name."
	case "BLOB_UNKNOWN":
		detail.Help = "The specified layer does not exist. The image may be corrupted or partially uploaded."
	case "BLOB_UPLOAD_UNKNOWN":
		detail.Help = "Upload session expired or invalid. Retry the push operation."
	case "DIGEST_INVALID":
		detail.Help = "The content digest does not match. This may indicate data corruption during transfer."
	case "MANIFEST_INVALID":
		detail.Help = "The manifest format is invalid. Ensure the image was built correctly."
	case "NAME_UNKNOWN":
		detail.Help = "The repository does not exist. Check the repository name for typos."
	case "SIZE_INVALID":
		detail.Help = "The content length does not match. Retry the upload."
	case "DENIED":
		detail.Help = "Access denied. Check your permissions for this repository."
	case "TOOMANYREQUESTS":
		detail.Help = "Rate limit exceeded. Wait a moment and retry."
	}

	response := EnhancedError{
		Errors: []ErrorDetail{detail},
	}

	w.Header().Set("Content-Type", ContentTypeJSON)
	w.Header().Set(HeaderDockerAPIVersion, DockerAPIVersionValue)
	w.WriteHeader(err.Status)
	json.NewEncoder(w).Encode(response)
}

// LogPush logs a push operation in Docker Hub style
func LogPush(repo, reference, digest string, size int64, duration time.Duration) {
	sizeStr := formatSize(size)
	fmt.Printf("✓ Pushed: %s:%s\n", repo, reference)
	fmt.Printf("  digest: %s\n", digest)
	fmt.Printf("  size: %s, time: %v\n", sizeStr, duration.Round(time.Millisecond))
}

// LogPull logs a pull operation in Docker Hub style
func LogPull(repo, reference, digest string, size int64, cached bool) {
	sizeStr := formatSize(size)
	cacheStatus := ""
	if cached {
		cacheStatus = " (cached)"
	}
	fmt.Printf("→ Pulled: %s:%s%s\n", repo, reference, cacheStatus)
	fmt.Printf("  digest: %s, size: %s\n", digest, sizeStr)
}

// LogBlobMount logs a cross-repo blob mount
func LogBlobMount(digest, fromRepo, toRepo string) {
	fmt.Printf("⚡ Mounted blob: %s\n", digest[:19]+"...")
	fmt.Printf("  from: %s → to: %s\n", fromRepo, toRepo)
}

// LogUploadProgress logs upload progress
func LogUploadProgress(uuid string, bytesReceived int64, isComplete bool) {
	sizeStr := formatSize(bytesReceived)
	if isComplete {
		fmt.Printf("✓ Upload complete: %s (%s)\n", uuid[:8]+"...", sizeStr)
	} else {
		fmt.Printf("↑ Uploading: %s (%s received)\n", uuid[:8]+"...", sizeStr)
	}
}

// formatSize formats bytes into human-readable size
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// PaginatedResponse adds pagination headers to a response
func AddPaginationHeaders(w fsthttp.ResponseWriter, name string, endpoint string, n int, last string, hasMore bool) {
	if hasMore && last != "" {
		linkURL := fmt.Sprintf("/v2/%s/%s?n=%d&last=%s", name, endpoint, n, last)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, linkURL))
	}
}

// ParsePaginationParams extracts n and last from query string
func ParsePaginationParams(query string) (n int, last string) {
	n = 100 // Default page size
	last = ""

	// Parse n parameter
	if nStr := extractQueryParam(query, "n"); nStr != "" {
		fmt.Sscanf(nStr, "%d", &n)
		if n <= 0 {
			n = 100
		}
		if n > 10000 {
			n = 10000 // Max page size
		}
	}

	// Parse last parameter
	last = extractQueryParam(query, "last")

	return n, last
}

// PaginateStringSlice paginates a slice of strings
func PaginateStringSlice(items []string, n int, last string) (page []string, nextLast string, hasMore bool) {
	startIdx := 0

	// Find start position based on 'last' parameter
	if last != "" {
		for i, item := range items {
			if item == last {
				startIdx = i + 1
				break
			}
		}
	}

	// Extract page
	endIdx := startIdx + n
	if endIdx > len(items) {
		endIdx = len(items)
	}

	page = items[startIdx:endIdx]

	// Determine if there are more items
	hasMore = endIdx < len(items)
	if hasMore && len(page) > 0 {
		nextLast = page[len(page)-1]
	}

	return page, nextLast, hasMore
}
