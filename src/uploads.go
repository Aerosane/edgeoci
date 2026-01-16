// Upload Operations
//
// Handles chunked blob uploads per OCI spec
// Optimized for Fastly Compute constraints:
// - 32 backend requests per instance
// - ~40MB WASM heap
// - 2-minute instance timeout

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/kvstore"
	"github.com/google/uuid"
)

// UploadSession stored in KV
type UploadSession struct {
	UUID          string `json:"uuid"`
	Repo          string `json:"repo"`
	StartedAt     string `json:"started_at"`
	BytesReceived int64  `json:"bytes_received"`
	TempLocation  string `json:"temp_location"`
	ExpiresAt     string `json:"expires_at"`
}

// handleInitiateUpload handles POST /v2/<name>/blobs/uploads/
func handleInitiateUpload(_ context.Context, w fsthttp.ResponseWriter, name string) error {
	uploadUUID := uuid.New().String()

	session := UploadSession{
		UUID:          uploadUUID,
		Repo:          name,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		BytesReceived: 0,
		TempLocation:  fmt.Sprintf("uploads/%s/%s", name, uploadUUID),
		ExpiresAt:     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}

	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("uploads/%s", uploadUUID)
	value, _ := json.Marshal(session)

	if err := store.Insert(key, strings.NewReader(string(value))); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV insert error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	fmt.Printf("Initiated upload: %s for repo %s\n", uploadUUID, name)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
	w.Header().Set("Docker-Upload-UUID", uploadUUID)
	w.Header().Set("Range", "0-0")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(fsthttp.StatusAccepted)
	return nil
}

// handleMountBlob handles POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<repo>
func handleMountBlob(ctx context.Context, w fsthttp.ResponseWriter, name, mountDigest, _ string) error {
	// Validate digest format
	if !strings.HasPrefix(mountDigest, "sha256:") {
		return &OCIError{Code: "DIGEST_INVALID", Message: "Invalid digest format", Detail: mountDigest, Status: fsthttp.StatusBadRequest}
	}

	blobKey := BlobKey(mountDigest)

	// Check if blob exists in object storage via HEAD request
	headReq, err := SignHeadRequest(blobKey)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	headReq.CacheOptions.Pass = true
	resp, err := headReq.Send(ctx, ObjectStorage)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage request failed: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Blob exists! Mount successful - return 201 Created
		fmt.Printf("Blob mount successful: %s -> %s\n", mountDigest, name)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, mountDigest))
		w.Header().Set("Docker-Content-Digest", mountDigest)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(fsthttp.StatusCreated)
		return nil
	}

	// Blob doesn't exist - fall back to regular upload initiation
	fmt.Printf("Blob mount failed (not found), initiating upload for: %s\n", mountDigest)
	return handleInitiateUpload(ctx, w, name)
}

// handleUploadChunk handles PATCH /v2/<name>/blobs/uploads/<uuid>
// Uses S3 multipart upload with resumption for large blobs.
// Optimized for ~28 parts per request (448MB) before returning.
func handleUploadChunk(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, name, uploadUUID string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("uploads/%s", uploadUUID)
	entry, err := store.Lookup(key)
	if err != nil {
		return &OCIError{Code: "BLOB_UPLOAD_UNKNOWN", Message: "blob upload unknown to registry", Detail: uploadUUID, Status: fsthttp.StatusNotFound}
	}

	body, _ := io.ReadAll(entry)
	var session UploadSession
	if err := json.Unmarshal(body, &session); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Invalid session data: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Get content length from header
	contentLength := r.Header.Get("Content-Length")
	transferEncoding := r.Header.Get("Transfer-Encoding")
	contentRange := r.Header.Get("Content-Range")
	var contentLengthInt int64
	fmt.Sscanf(contentLength, "%d", &contentLengthInt)

	fmt.Printf("Upload chunk: Content-Length=%d, Transfer-Encoding=%s, Content-Range=%s for %s\n",
		contentLengthInt, transferEncoding, contentRange, uploadUUID)

	// FAST PATH: If we already have significant progress, return immediately with Range header
	// This avoids reading the body at all, preventing timeout on large blobs
	if session.BytesReceived > 0 && transferEncoding == "chunked" {
		fmt.Printf("FAST PATH: Already have %d bytes, returning 202 with Range to trigger proper resume\n", session.BytesReceived)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
		w.Header().Set("Docker-Upload-UUID", uploadUUID)
		w.Header().Set("Range", fmt.Sprintf("0-%d", session.BytesReceived-1))
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(fsthttp.StatusAccepted)
		return nil
	}

	// Handle chunked transfer encoding - use S3 multipart upload
	if contentLengthInt == 0 && transferEncoding == "chunked" {
		chunkKey := fmt.Sprintf("%s/data", session.TempLocation)
		fmt.Printf("Multipart upload for S3 key: %s (repo: %s)\n", chunkKey, name)

		result, err := ResumableMultipartUpload(ctx, name, chunkKey, r.Body)
		if err != nil {
			fmt.Printf("Multipart upload error: %v\n", err)
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Multipart upload failed: %v", err), Status: fsthttp.StatusInternalServerError}
		}

		// Early exit - blob already completed in previous session
		if result.CompletedKey != "" && result.IsComplete {
			fmt.Printf("EARLY EXIT: Blob already at %s\n", result.CompletedKey)
			session.TempLocation = strings.TrimSuffix(result.CompletedKey, "/data")
			session.BytesReceived = 1
			value, _ := json.Marshal(session)
			store.Insert(key, strings.NewReader(string(value)))

			w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
			w.Header().Set("Docker-Upload-UUID", uploadUUID)
			w.Header().Set("Range", "0-0")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(fsthttp.StatusAccepted)
			return nil
		}

		if result.BytesUploaded > 0 {
			session.BytesReceived = result.BytesUploaded
			// Use the S3 key from multipart state (may be different from session's temp location)
			if result.State != nil && result.State.S3Key != "" {
				session.TempLocation = strings.TrimSuffix(result.State.S3Key, "/data")
			} else if result.CompletedKey != "" {
				session.TempLocation = strings.TrimSuffix(result.CompletedKey, "/data")
			}
			value, _ := json.Marshal(session)
			store.Insert(key, strings.NewReader(string(value)))

			if !result.IsComplete {
				// Partial upload - return 202 with Range header
				w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
				w.Header().Set("Docker-Upload-UUID", uploadUUID)
				w.Header().Set("Range", fmt.Sprintf("0-%d", result.BytesUploaded-1))
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(fsthttp.StatusAccepted)
				return nil
			}
		}
	} else if contentLengthInt > 0 {
		// Small upload with known size - direct PUT
		chunkKey := fmt.Sprintf("%s/data", session.TempLocation)
		fmt.Printf("Direct PUT to S3: %s, size: %d\n", chunkKey, contentLengthInt)

		s3Req, err := SignPutRequest(chunkKey, "application/octet-stream")
		if err != nil {
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
		}

		s3Req.Header.Set("Content-Length", contentLength)
		s3Req.SetBody(r.Body)
		s3Req.ManualFramingMode = true
		s3Req.CacheOptions.Pass = true

		resp, err := s3Req.Send(ctx, ObjectStorage)
		if err != nil {
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 request failed: %v", err), Status: fsthttp.StatusInternalServerError}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Printf("S3 PUT error: %s\n", string(respBody))
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Chunk upload failed: %d", resp.StatusCode), Status: fsthttp.StatusInternalServerError}
		}

		session.BytesReceived += contentLengthInt
		value, _ := json.Marshal(session)
		store.Insert(key, strings.NewReader(string(value)))
	}

	fmt.Printf("Upload chunk done: %d bytes for %s\n", session.BytesReceived, uploadUUID)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
	w.Header().Set("Docker-Upload-UUID", uploadUUID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(0, session.BytesReceived-1)))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(fsthttp.StatusAccepted)
	return nil
}

// handleCompleteUpload handles PUT /v2/<name>/blobs/uploads/<uuid>?digest=<digest>
func handleCompleteUpload(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, name, uploadUUID, expectedDigest string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("uploads/%s", uploadUUID)
	entry, err := store.Lookup(key)
	if err != nil {
		return &OCIError{Code: "BLOB_UPLOAD_UNKNOWN", Message: "blob upload unknown to registry", Detail: uploadUUID, Status: fsthttp.StatusNotFound}
	}

	sessionBody, _ := io.ReadAll(entry)
	var session UploadSession
	if err := json.Unmarshal(sessionBody, &session); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Invalid session data: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Validate digest format
	if !strings.HasPrefix(expectedDigest, "sha256:") {
		return &OCIError{Code: "DIGEST_INVALID", Message: "Invalid digest format", Detail: expectedDigest, Status: fsthttp.StatusBadRequest}
	}

	digestHash := expectedDigest[7:]
	finalKey := fmt.Sprintf("blobs/sha256/%s/%s/%s", digestHash[0:2], digestHash[2:4], digestHash)

	// Check if we have a body (monolithic upload with PUT body)
	contentLength := r.Header.Get("Content-Length")
	var contentLengthInt int64
	fmt.Sscanf(contentLength, "%d", &contentLengthInt)

	fmt.Printf("complete_upload: content_length=%d, session.bytes_received=%d\n", contentLengthInt, session.BytesReceived)

	if contentLengthInt > 0 {
		// Monolithic upload - stream body directly to final location
		s3Req, err := SignPutRequest(finalKey, "application/octet-stream")
		if err != nil {
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
		}

		s3Req.Header.Set("Content-Length", contentLength)
		s3Req.SetBody(r.Body)
		s3Req.ManualFramingMode = true

		s3Req.CacheOptions.Pass = true
		resp, err := s3Req.Send(ctx, ObjectStorage)
		if err != nil {
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage request failed: %v", err), Status: fsthttp.StatusInternalServerError}
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			fmt.Printf("S3 PUT error response: %s\n", string(respBody))
			return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage upload failed: %d", resp.StatusCode), Status: fsthttp.StatusInternalServerError}
		}
	} else if session.BytesReceived > 0 {
		// Chunked upload complete - copy from temp to final location using S3 COPY
		sourceKey := fmt.Sprintf("%s/data", session.TempLocation)
		fmt.Printf("Copying temp blob from %s to %s\n", sourceKey, finalKey)

		// Try S3 COPY operation first (server-side copy)
		copyReq, err := SignCopyRequest(finalKey, sourceKey)
		copyWorked := false
		if err == nil {
			copyReq.CacheOptions.Pass = true
			copyResp, err := copyReq.Send(ctx, ObjectStorage)
			if err == nil {
				fmt.Printf("S3 COPY response status: %d\n", copyResp.StatusCode)
				copyWorked = copyResp.StatusCode >= 200 && copyResp.StatusCode < 300
			}
		} else {
			fmt.Printf("Failed to sign copy request\n")
		}

		if !copyWorked {
			// Fallback: fetch and re-upload if COPY not supported
			fmt.Printf("S3 COPY failed, falling back to fetch and upload from %s\n", sourceKey)

			getReq, err := SignGetRequest(sourceKey)
			if err != nil {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
			}

			getReq.CacheOptions.Pass = true
			getResp, err := getReq.Send(ctx, ObjectStorage)
			if err != nil {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage GET failed: %v", err), Status: fsthttp.StatusInternalServerError}
			}

			fmt.Printf("Temp blob GET status: %d\n", getResp.StatusCode)

			if getResp.StatusCode < 200 || getResp.StatusCode >= 300 {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Failed to fetch temp blob: %d", getResp.StatusCode), Status: fsthttp.StatusInternalServerError}
			}

			// Re-upload to final location
			putReq, err := SignPutRequest(finalKey, "application/octet-stream")
			if err != nil {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("S3 auth error: %v", err), Status: fsthttp.StatusInternalServerError}
			}

			putReq.SetBody(getResp.Body)

			putReq.CacheOptions.Pass = true
			putResp, err := putReq.Send(ctx, ObjectStorage)
			if err != nil {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Object Storage PUT failed: %v", err), Status: fsthttp.StatusInternalServerError}
			}

			fmt.Printf("Final blob PUT status: %d\n", putResp.StatusCode)

			if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
				return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Failed to copy blob to final location: %d", putResp.StatusCode), Status: fsthttp.StatusInternalServerError}
			}
		}

		// Clean up temp blob (best effort)
		delReq, err := SignDeleteRequest(sourceKey)
		if err == nil {
			delReq.CacheOptions.Pass = true
			delReq.Send(ctx, ObjectStorage)
		}
	}

	// Clean up upload session
	if err := store.Delete(key); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV delete error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Clean up multipart state if exists (use source key for cleanup)
	sourceKey := fmt.Sprintf("%s/data", session.TempLocation)
	DeleteMultipartState(sourceKey)

	fmt.Printf("Completed upload: %s -> %s\n", uploadUUID, expectedDigest)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", name, expectedDigest))
	w.Header().Set("Docker-Content-Digest", expectedDigest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(fsthttp.StatusCreated)
	return nil
}

// handleGetUploadStatus handles GET /v2/<name>/blobs/uploads/<uuid>
func handleGetUploadStatus(_ context.Context, w fsthttp.ResponseWriter, name, uploadUUID string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("uploads/%s", uploadUUID)
	entry, err := store.Lookup(key)
	if err != nil {
		return &OCIError{Code: "BLOB_UPLOAD_UNKNOWN", Message: "blob upload unknown to registry", Detail: uploadUUID, Status: fsthttp.StatusNotFound}
	}

	body, _ := io.ReadAll(entry)
	var session UploadSession
	if err := json.Unmarshal(body, &session); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Invalid session data: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uploadUUID))
	w.Header().Set("Docker-Upload-UUID", uploadUUID)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(0, session.BytesReceived-1)))
	w.WriteHeader(fsthttp.StatusNoContent)
	return nil
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
