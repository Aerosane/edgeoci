// OCI Container Registry - Main Entry Point (Go)
//
// Implements OCI Distribution Spec v2 compliant container registry
// running on Fastly Edge Compute with KV Store and Object Storage.
//
// Limits:
// - Max blob size: ~500MB (network throughput limited within 2min timeout)
// - 32 backend requests per compute invocation
//
// Architecture:
// Docker Client -> Fastly Edge (Compute) -> Object Storage (blobs)
//                       |
//                 KV Store (manifests, tags, metadata)

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

// Common HTTP headers and content types
const (
	HeaderContentType         = "Content-Type"
	HeaderDockerAPIVersion    = "Docker-Distribution-API-Version"
	ContentTypeJSON           = "application/json"
	DockerAPIVersionValue     = "registry/2.0"
)

func main() {
	fsthttp.ServeFunc(handleRequest)
}

func handleRequest(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request) {
	path := r.URL.Path
	method := r.Method
	query := r.URL.RawQuery

	fmt.Printf("OCI Registry: %s %s\n", method, path)

	// Parse and handle the route
	route := parseRoute(path, method, query)
	fmt.Printf("Parsed route: %+v\n", route)

	// Handle the request based on route
	err := handleRoute(ctx, w, r, route)
	if err != nil {
		fmt.Printf("Request failed: %v\n", err)
		writeOCIError(w, err)
		return
	}
}

// Route represents an OCI Distribution API route
type Route struct {
	Type      string
	Name      string
	Reference string
	Digest    string
	UUID      string
	MountFrom string
	Query     string // Raw query string for pagination
}

func parseRoute(path, method, query string) Route {
	// Health check
	if path == "/health" || path == "/" {
		return Route{Type: "health"}
	}

	// Token authentication endpoint (check before v2 routes)
	if path == "/v2/auth" || path == "/token" {
		return Route{Type: "token_auth"}
	}

	// API version check: GET /v2/
	if path == "/v2/" || path == "/v2" {
		return Route{Type: "api_version"}
	}

	// Must start with /v2/
	if !strings.HasPrefix(path, "/v2/") {
		return Route{Type: "not_found"}
	}

	pathWithoutV2 := path[4:] // Remove "/v2/"

	// Catalog: GET /v2/_catalog
	if pathWithoutV2 == "_catalog" && method == "GET" {
		return Route{Type: "catalog"}
	}

	// Manifest routes: <name>/manifests/<reference>
	if idx := strings.Index(pathWithoutV2, "/manifests/"); idx != -1 {
		name := pathWithoutV2[:idx]
		reference := pathWithoutV2[idx+11:]
		if name != "" && reference != "" {
			switch method {
			case "GET":
				return Route{Type: "get_manifest", Name: name, Reference: reference}
			case "HEAD":
				return Route{Type: "head_manifest", Name: name, Reference: reference}
			case "PUT":
				return Route{Type: "put_manifest", Name: name, Reference: reference}
			case "DELETE":
				return Route{Type: "delete_manifest", Name: name, Reference: reference}
			}
		}
	}

	// Upload routes: <name>/blobs/uploads/<uuid>
	if idx := strings.Index(pathWithoutV2, "/blobs/uploads/"); idx != -1 {
		name := pathWithoutV2[:idx]
		uuid := pathWithoutV2[idx+15:]
		if name != "" && uuid != "" {
			// Check for digest in query for complete upload
			if method == "PUT" {
				if digest := extractQueryParam(query, "digest"); digest != "" {
					return Route{Type: "complete_upload", Name: name, UUID: uuid, Digest: digest}
				}
			}
			switch method {
			case "PATCH":
				return Route{Type: "upload_chunk", Name: name, UUID: uuid}
			case "GET":
				return Route{Type: "get_upload_status", Name: name, UUID: uuid}
			case "PUT":
				return Route{Type: "upload_chunk", Name: name, UUID: uuid} // Monolithic upload
			}
		}
	}

	// Initiate upload: POST <name>/blobs/uploads/ or <name>/blobs/uploads
	if strings.HasSuffix(pathWithoutV2, "/blobs/uploads/") || strings.HasSuffix(pathWithoutV2, "/blobs/uploads") {
		suffixLen := 14
		if strings.HasSuffix(pathWithoutV2, "/") {
			suffixLen = 15
		}
		name := pathWithoutV2[:len(pathWithoutV2)-suffixLen]
		if name != "" && method == "POST" {
			// Check for cross-repo mount parameters
			mountDigest := extractQueryParam(query, "mount")
			fromRepo := extractQueryParam(query, "from")
			if mountDigest != "" && fromRepo != "" {
				return Route{Type: "mount_blob", Name: name, Digest: mountDigest, MountFrom: fromRepo}
			}
			return Route{Type: "initiate_upload", Name: name}
		}
	}

	// Blob routes: <name>/blobs/<digest> (but not uploads)
	if !strings.Contains(pathWithoutV2, "/blobs/uploads") {
		if idx := strings.Index(pathWithoutV2, "/blobs/"); idx != -1 {
			name := pathWithoutV2[:idx]
			digest := pathWithoutV2[idx+7:]
			if name != "" && digest != "" {
				switch method {
				case "GET":
					return Route{Type: "get_blob", Name: name, Digest: digest}
				case "HEAD":
					return Route{Type: "head_blob", Name: name, Digest: digest}
				case "DELETE":
					return Route{Type: "delete_blob", Name: name, Digest: digest}
				}
			}
		}
	}

	// Tags list: GET <name>/tags/list
	if strings.HasSuffix(pathWithoutV2, "/tags/list") && method == "GET" {
		name := pathWithoutV2[:len(pathWithoutV2)-10]
		if name != "" {
			return Route{Type: "list_tags", Name: name, Query: query}
		}
	}

	// Referrers: GET <name>/referrers/<digest>
	if idx := strings.Index(pathWithoutV2, "/referrers/"); idx != -1 {
		name := pathWithoutV2[:idx]
		digest := pathWithoutV2[idx+11:]
		if name != "" && digest != "" && method == "GET" {
			return Route{Type: "referrers", Name: name, Digest: digest, Query: query}
		}
	}

	return Route{Type: "not_found"}
}

func handleRoute(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, route Route) error {
	if r.Method == "TRACE" {
		w.WriteHeader(fsthttp.StatusMethodNotAllowed)
		return nil
	}

	AddSecurityHeaders(w)

	w.Header().Set(HeaderDockerAPIVersion, DockerAPIVersionValue)
	w.Header().Set("X-Served-By", "fastly-oci-registry")
	w.Header().Set("X-Registry-Version", RegistryVersion)

	origin := r.Header.Get("Origin")
	if isAllowedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Docker-Content-Digest")
	w.Header().Set("Access-Control-Expose-Headers", "Docker-Content-Digest, Docker-Upload-UUID, Location, Range, WWW-Authenticate, Link")

	if r.Method == "OPTIONS" {
		w.WriteHeader(fsthttp.StatusNoContent)
		return nil
	}

	allowed, count, remaining := CheckRateLimit(r)
	w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", RateLimitMaxRequests))
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
	if !allowed {
		LogSecurityEvent("RATE_LIMIT", getClientIP(r), fmt.Sprintf("exceeded %d requests", count))
		WriteRateLimitResponse(w, RateLimitWindow)
		return nil
	}

	var authResult *AuthResult
	if route.Type != "health" && route.Type != "api_version" && route.Type != "token_auth" {
		authResult = CheckAuth(r)
		if !authResult.Authenticated {
			LogSecurityEvent("AUTH_FAIL", getClientIP(r), fmt.Sprintf("path=%s", r.URL.Path))
			WriteUnauthorizedResponse(w, RegistryName)
			return nil
		}

		if authResult.Claims != nil && route.Name != "" {
			action := getRequiredAction(route.Type)
			if !CheckAuthorization(authResult.Claims, route.Name, action) {
				LogSecurityEvent("AUTHZ_DENIED", getClientIP(r), fmt.Sprintf("repo=%s action=%s", route.Name, action))
				WriteDeniedResponse(w, action, route.Name)
				return nil
			}
		}
	}

	if route.Name != "" {
		if err := ValidateRepositoryName(route.Name); err != nil {
			LogSecurityEvent("INVALID_NAME", getClientIP(r), fmt.Sprintf("name=%s", route.Name))
			return err
		}
	}

	if route.Reference != "" {
		if err := ValidateReference(route.Reference); err != nil {
			return err
		}
	}
	if route.Digest != "" {
		if err := ValidateDigestFormat(route.Digest); err != nil {
			return err
		}
	}

	switch route.Type {
	case "health":
		WriteHealthResponse(w)
		return nil
	case "token_auth":
		return HandleTokenRequest(w, r)
	case "api_version":
		// /v2/ endpoint: Return 401 with Bearer challenge if not authenticated
		// This is how Docker learns where to get tokens
		authResult := CheckAuth(r)
		if !authResult.Authenticated {
			WriteUnauthorizedResponse(w, RegistryName)
			return nil
		}
		WriteAPIVersionResponse(w)
		return nil
	case "get_manifest":
		return handleGetManifest(ctx, w, route.Name, route.Reference)
	case "head_manifest":
		return handleHeadManifest(ctx, w, route.Name, route.Reference)
	case "put_manifest":
		return handlePutManifest(ctx, w, r, route.Name, route.Reference)
	case "delete_manifest":
		return handleDeleteManifest(ctx, w, route.Name, route.Reference)
	case "get_blob":
		return handleGetBlob(ctx, w, route.Name, route.Digest)
	case "head_blob":
		return handleHeadBlob(ctx, w, route.Name, route.Digest)
	case "delete_blob":
		return handleDeleteBlob(ctx, w, route.Name, route.Digest)
	case "initiate_upload":
		return handleInitiateUpload(ctx, w, route.Name)
	case "mount_blob":
		return handleMountBlob(ctx, w, route.Name, route.Digest, route.MountFrom)
	case "upload_chunk":
		return handleUploadChunk(ctx, w, r, route.Name, route.UUID)
	case "complete_upload":
		return handleCompleteUpload(ctx, w, r, route.Name, route.UUID, route.Digest)
	case "get_upload_status":
		return handleGetUploadStatus(ctx, w, route.Name, route.UUID)
	case "list_tags":
		return handleListTags(ctx, w, route.Name, route.Query)
	case "catalog":
		return handleCatalog(ctx, w, r.URL.RawQuery)
	case "referrers":
		return handleReferrers(ctx, w, r, route.Name, route.Digest)
	case "not_found":
		return &OCIError{
			Code:    "NAME_UNKNOWN",
			Message: "Endpoint not found",
			Status:  fsthttp.StatusNotFound,
		}
	default:
		return &OCIError{
			Code:    "UNSUPPORTED",
			Message: "Unknown route type",
			Status:  fsthttp.StatusInternalServerError,
		}
	}
}

func extractQueryParam(query, key string) string {
	for _, pair := range strings.Split(query, "&") {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 && parts[0] == key {
			// Basic URL decoding
			return strings.ReplaceAll(strings.ReplaceAll(parts[1], "%3A", ":"), "%2F", "/")
		}
	}
	return ""
}

// writeOCIError writes an OCI error response with enhanced formatting
func writeOCIError(w fsthttp.ResponseWriter, err error) {
	ociErr, ok := err.(*OCIError)
	if !ok {
		ociErr = &OCIError{
			Code:    "UNSUPPORTED",
			Message: err.Error(),
			Status:  fsthttp.StatusInternalServerError,
		}
	}

	WriteEnhancedError(w, ociErr)
}

// OCIError represents an OCI spec error
type OCIError struct {
	Code    string
	Message string
	Detail  string
	Status  int
}

func (e *OCIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
