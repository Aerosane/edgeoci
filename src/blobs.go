// Blob Operations
//
// Handles OCI blob GET/HEAD/DELETE
// GET uses CDN for caching (4-5x faster for cached blobs)
// HEAD/DELETE go direct to Object Storage

package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

const (
	CDNBackend    = "cdn_backend"
	ObjectStorage = "object_storage"
	// CDNHost - Configure this to your CDN domain (optional, for cached blob reads)
	// If not using CDN, blobs will be served directly from Object Storage
	CDNHost       = "your-cdn-domain.example.com"
)

// handleGetBlob handles GET /v2/<name>/blobs/<digest>
// Uses CDN for caching - blobs are immutable so cache hits are ~4-5x faster
func handleGetBlob(ctx context.Context, w fsthttp.ResponseWriter, _ string, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return &OCIError{Code: "DIGEST_INVALID", Message: "Invalid digest format", Detail: digest, Status: fsthttp.StatusBadRequest}
	}

	blobKey := BlobKey(digest)

	// Try CDN first (handles S3 auth internally via VCL)
	cdnURL := fmt.Sprintf("https://%s/%s", CDNHost, blobKey)
	cdnReq, err := fsthttp.NewRequest("GET", cdnURL, nil)
	if err == nil {
		cdnReq.Header.Set("Host", CDNHost)
		cdnReq.CacheOptions.Pass = true // Let CDN handle caching

		cdnResp, err := cdnReq.Send(ctx, CDNBackend)
		if err == nil && cdnResp.StatusCode >= 200 && cdnResp.StatusCode < 300 {
			// CDN hit or successful fetch
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			if cl := cdnResp.Header.Get("Content-Length"); cl != "" {
				w.Header().Set("Content-Length", cl)
			}
			if xc := cdnResp.Header.Get("X-Cache"); xc != "" {
				w.Header().Set("X-Cache", xc)
			}
			w.WriteHeader(fsthttp.StatusOK)
			io.Copy(w, cdnResp.Body)
			return nil
		}

		if cdnResp != nil && cdnResp.StatusCode == fsthttp.StatusNotFound {
			return &OCIError{Code: "BLOB_UNKNOWN", Message: "blob unknown to registry", Detail: digest, Status: fsthttp.StatusNotFound}
		}
	}

	// Fallback to direct Object Storage
	s3Req, err := SignGetRequest(blobKey)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	s3Req.CacheOptions.Pass = true
	s3Resp, err := s3Req.Send(ctx, ObjectStorage)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage request failed: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	if s3Resp.StatusCode == fsthttp.StatusNotFound {
		return &OCIError{Code: "BLOB_UNKNOWN", Message: "blob unknown to registry", Detail: digest, Status: fsthttp.StatusNotFound}
	}

	if s3Resp.StatusCode < 200 || s3Resp.StatusCode >= 300 {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage returned status: %d", s3Resp.StatusCode), Status: fsthttp.StatusInternalServerError}
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if cl := s3Resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(fsthttp.StatusOK)
	io.Copy(w, s3Resp.Body)
	return nil
}

// handleHeadBlob handles HEAD /v2/<name>/blobs/<digest>
// Goes direct to S3 (small request, caching not critical)
func handleHeadBlob(ctx context.Context, w fsthttp.ResponseWriter, _ string, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return &OCIError{Code: "DIGEST_INVALID", Message: "Invalid digest format", Detail: digest, Status: fsthttp.StatusBadRequest}
	}

	blobKey := BlobKey(digest)

	s3Req, err := SignHeadRequest(blobKey)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	s3Req.CacheOptions.Pass = true
	resp, err := s3Req.Send(ctx, ObjectStorage)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage request failed: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	if resp.StatusCode == fsthttp.StatusNotFound {
		return &OCIError{Code: "BLOB_UNKNOWN", Message: "blob unknown to registry", Detail: digest, Status: fsthttp.StatusNotFound}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage returned status: %d", resp.StatusCode), Status: fsthttp.StatusInternalServerError}
	}

	contentLength := resp.Header.Get("Content-Length")
	if contentLength == "" {
		contentLength = "0"
	}

	w.SetManualFramingMode(true)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", contentLength)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(fsthttp.StatusOK)
	w.Close()
	return nil
}

// handleDeleteBlob handles DELETE /v2/<name>/blobs/<digest>
func handleDeleteBlob(ctx context.Context, w fsthttp.ResponseWriter, _ string, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") {
		return &OCIError{Code: "DIGEST_INVALID", Message: "Invalid digest format", Detail: digest, Status: fsthttp.StatusBadRequest}
	}

	blobKey := BlobKey(digest)

	req, err := SignDeleteRequest(blobKey)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	req.CacheOptions.Pass = true
	resp, err := req.Send(ctx, ObjectStorage)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage request failed: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	if resp.StatusCode == fsthttp.StatusNotFound {
		return &OCIError{Code: "BLOB_UNKNOWN", Message: "blob unknown to registry", Detail: digest, Status: fsthttp.StatusNotFound}
	}

	w.WriteHeader(fsthttp.StatusAccepted)
	return nil
}
