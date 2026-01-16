// S3 Multipart Upload Support
//
// Handles large blob uploads via S3 multipart upload API with resumption support
// to work around Fastly Compute's 32 backend request limit per instance.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/kvstore"
)

// Minimum part size for S3 multipart upload (5MB)
const MinPartSize = 5 * 1024 * 1024

// Part size we'll use (16MB - balanced for heap limit ~40MB and backend request limit)
const PartSize = 16 * 1024 * 1024

// Size of the initial chunk used for content hash identification (smaller = faster resume detection)
const IdentifyChunkSize = 1 * 1024 * 1024 // 1MB

// Maximum parts to upload per request
// With 32 backend requests: 1 initiate + 1 list + 28 parts + 1 complete + 1 buffer = 32
// 28 parts × 16MB = 448MB per request cycle (theoretical max, network limited to ~300MB)
const MaxPartsPerRequest = 28

// Maximum blob size we can reliably handle (~300MB due to network read limits in 2min)
const MaxReliableBlobSize = 300 * 1024 * 1024

// MultipartState tracks the state of an in-progress S3 multipart upload
type MultipartState struct {
	S3UploadId     string          `json:"s3_upload_id"`
	S3Key          string          `json:"s3_key"`
	CompletedParts []CompletedPart `json:"completed_parts"`
	NextPartNumber int             `json:"next_part_number"`
	BytesUploaded  int64           `json:"bytes_uploaded"`
	StartedAt      string          `json:"started_at"`
	ContentHash    string          `json:"content_hash"` // Hash of first chunk for resumption matching
}

// InitiateMultipartUploadResult is the XML response from S3
type InitiateMultipartUploadResult struct {
	Bucket   string `xml:"Bucket"`
	Key      string `xml:"Key"`
	UploadId string `xml:"UploadId"`
}

// CompletedPart represents a completed part
type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// CompleteMultipartUpload is the XML request body
type CompleteMultipartUpload struct {
	XMLName xml.Name                `xml:"CompleteMultipartUpload"`
	Parts   []CompleteMultipartPart `xml:"Part"`
}

type CompleteMultipartPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// MultipartUploadResult contains the result of a multipart upload attempt
type MultipartUploadResult struct {
	BytesUploaded int64
	IsComplete    bool
	State         *MultipartState // Non-nil if upload needs to continue
	CompletedKey  string          // Set if blob was already completed (early exit)
}

// SignInitiateMultipartUpload creates a signed POST request to initiate multipart upload
func SignInitiateMultipartUpload(key string) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)
	queryString := "uploads="

	payloadHash := sha256Hex([]byte{})

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		FastlyOSHost, payloadHash, datetime)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := fmt.Sprintf("POST\n%s\n%s\n%s\n%s\n%s",
		uri, queryString, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s?uploads", FastlyOSHost, uri)
	req, err := fsthttp.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

// SignUploadPart creates a signed PUT request to upload a part
func SignUploadPart(key, uploadId string, partNumber int, contentLength int64) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)
	queryString := fmt.Sprintf("partNumber=%d&uploadId=%s", partNumber, uploadId)

	payloadHash := "UNSIGNED-PAYLOAD"

	canonicalHeaders := fmt.Sprintf("content-length:%d\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentLength, FastlyOSHost, payloadHash, datetime)
	signedHeaders := "content-length;host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := fmt.Sprintf("PUT\n%s\n%s\n%s\n%s\n%s",
		uri, queryString, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s?partNumber=%d&uploadId=%s", FastlyOSHost, uri, partNumber, uploadId)
	req, err := fsthttp.NewRequest("PUT", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Content-Length", fmt.Sprintf("%d", contentLength))
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

// SignCompleteMultipartUpload creates a signed POST request to complete the upload
func SignCompleteMultipartUpload(key, uploadId string, body []byte) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)
	queryString := fmt.Sprintf("uploadId=%s", uploadId)

	payloadHash := sha256Hex(body)

	canonicalHeaders := fmt.Sprintf("content-length:%d\ncontent-type:application/xml\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		len(body), FastlyOSHost, payloadHash, datetime)
	signedHeaders := "content-length;content-type;host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := fmt.Sprintf("POST\n%s\n%s\n%s\n%s\n%s",
		uri, queryString, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s?uploadId=%s", FastlyOSHost, uri, uploadId)
	req, err := fsthttp.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

// SignPresignedPutURL generates a pre-signed URL for direct S3 upload
// This allows clients to upload directly to S3, bypassing Fastly's limits
func SignPresignedPutURL(key string, expiresInSeconds int) (string, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return "", fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)

	// Query parameters for presigned URL
	queryParams := fmt.Sprintf("X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=%s%%2F%s%%2F%s%%2Fs3%%2Faws4_request&X-Amz-Date=%s&X-Amz-Expires=%d&X-Amz-SignedHeaders=host",
		accessKey, date, FastlyOSRegion, datetime, expiresInSeconds)

	canonicalHeaders := fmt.Sprintf("host:%s\n", FastlyOSHost)
	signedHeaders := "host"

	canonicalRequest := fmt.Sprintf("PUT\n%s\n%s\n%s\n%s\nUNSIGNED-PAYLOAD",
		uri, queryParams, canonicalHeaders, signedHeaders)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	presignedURL := fmt.Sprintf("https://%s%s?%s&X-Amz-Signature=%s",
		FastlyOSHost, uri, queryParams, signature)

	return presignedURL, nil
}

// ListPartsResult is the XML response from S3 ListParts
type ListPartsResult struct {
	XMLName              xml.Name         `xml:"ListPartsResult"`
	Bucket               string           `xml:"Bucket"`
	Key                  string           `xml:"Key"`
	UploadId             string           `xml:"UploadId"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker"`
	IsTruncated          bool             `xml:"IsTruncated"`
	Parts                []ListPartResult `xml:"Part"`
}

type ListPartResult struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
	Size       int64  `xml:"Size"`
}

// SignListParts creates a signed GET request to list uploaded parts
func SignListParts(key, uploadId string) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)
	queryString := fmt.Sprintf("uploadId=%s", uploadId)

	payloadHash := sha256Hex([]byte{})

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		FastlyOSHost, payloadHash, datetime)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := fmt.Sprintf("GET\n%s\n%s\n%s\n%s\n%s",
		uri, queryString, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s?uploadId=%s", FastlyOSHost, uri, uploadId)
	req, err := fsthttp.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

// SignAbortMultipartUpload creates a signed DELETE request to abort a multipart upload
func SignAbortMultipartUpload(key, uploadId string) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)
	queryString := fmt.Sprintf("uploadId=%s", uploadId)

	payloadHash := sha256Hex([]byte{})

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		FastlyOSHost, payloadHash, datetime)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"

	canonicalRequest := fmt.Sprintf("DELETE\n%s\n%s\n%s\n%s\n%s",
		uri, queryString, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s?uploadId=%s", FastlyOSHost, uri, uploadId)
	req, err := fsthttp.NewRequest("DELETE", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

// LoadMultipartState loads multipart state from KV store
// Uses the S3 key as the lookup key for consistent resumption across upload sessions
func LoadMultipartState(s3Key string) (*MultipartState, error) {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return nil, fmt.Errorf("KV store error: %w", err)
	}

	// Use a hash of the S3 key to create a consistent lookup key
	key := fmt.Sprintf("multipart/%s", strings.ReplaceAll(s3Key, "/", "_"))
	entry, err := store.Lookup(key)
	if err != nil {
		return nil, nil // No state found, not an error
	}

	body, err := io.ReadAll(entry)
	if err != nil {
		return nil, fmt.Errorf("failed to read multipart state: %w", err)
	}

	var state MultipartState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("failed to parse multipart state: %w", err)
	}

	return &state, nil
}

// SaveMultipartState saves multipart state to KV store
// Uses the S3 key as the lookup key for consistent resumption across upload sessions
func SaveMultipartState(s3Key string, state *MultipartState) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return fmt.Errorf("KV store error: %w", err)
	}

	key := fmt.Sprintf("multipart/%s", strings.ReplaceAll(s3Key, "/", "_"))
	value, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal multipart state: %w", err)
	}

	if err := store.Insert(key, strings.NewReader(string(value))); err != nil {
		return fmt.Errorf("KV insert error: %w", err)
	}

	return nil
}

// DeleteMultipartState removes multipart state from KV store
func DeleteMultipartState(s3Key string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return fmt.Errorf("KV store error: %w", err)
	}

	key := fmt.Sprintf("multipart/%s", strings.ReplaceAll(s3Key, "/", "_"))
	store.Delete(key) // Best effort
	return nil
}

// LoadCompletedUpload checks if a blob with this content hash was already completed
func LoadCompletedUpload(stateKey string) (string, error) {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return "", fmt.Errorf("KV store error: %w", err)
	}

	key := fmt.Sprintf("completed/%s", strings.ReplaceAll(stateKey, "/", "_"))
	entry, err := store.Lookup(key)
	if err != nil {
		return "", nil // Not found
	}

	body, err := io.ReadAll(entry)
	if err != nil {
		return "", fmt.Errorf("failed to read completed key: %w", err)
	}

	return string(body), nil
}

// SaveCompletedUpload marks a blob as completed with its final S3 key
func SaveCompletedUpload(stateKey, finalKey string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return fmt.Errorf("KV store error: %w", err)
	}

	key := fmt.Sprintf("completed/%s", strings.ReplaceAll(stateKey, "/", "_"))
	if err := store.Insert(key, strings.NewReader(finalKey)); err != nil {
		return fmt.Errorf("KV insert error: %w", err)
	}

	return nil
}

// DeleteCompletedUpload removes stale completed state
func DeleteCompletedUpload(stateKey string) {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return
	}
	key := fmt.Sprintf("completed/%s", strings.ReplaceAll(stateKey, "/", "_"))
	store.Delete(key)
}

// ResumableMultipartUpload handles streaming a chunked body to S3 using multipart upload
// with support for resumption across multiple requests.
//
// This function uses content-based identification:
// 1. Read and hash the first part of data
// 2. Check if this blob has already been completed (early exit!)
// 3. Check if there's an existing multipart upload with matching content hash
// 4. If found, skip already-uploaded data and continue
// 5. If not found or hash doesn't match, start fresh
//
// Parameters:
// - ctx: request context
// - repoName: repository name for consistent state key
// - key: S3 object key (temp location)
// - body: the request body to upload
//
// Returns:
// - result: upload result with bytes uploaded, completion status, and state for resumption
// - error: any error that occurred
func ResumableMultipartUpload(ctx context.Context, repoName, key string, body io.Reader) (*MultipartUploadResult, error) {
	// Read a small chunk for content identification (faster than reading full 16MB part)
	identifyBuf := make([]byte, IdentifyChunkSize)
	identifyN, err := io.ReadFull(body, identifyBuf)
	if err == io.EOF {
		// No data at all
		return &MultipartUploadResult{
			BytesUploaded: 0,
			IsComplete:    true,
			State:         nil,
		}, nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("failed to read identification chunk: %w", err)
	}

	identifyData := identifyBuf[:identifyN]

	// Compute hash for resumption matching
	contentHash := sha256Hex(identifyData)
	stateKey := fmt.Sprintf("%s/%s", repoName, contentHash[:16]) // Use first 16 chars of hash

	fmt.Printf("Content hash for resumption: %s (key: %s, identify chunk: %d bytes)\n", contentHash[:16], stateKey, identifyN)

	// EARLY EXIT: Check if this blob was already completed in a previous session
	// But verify the blob actually exists in S3 before trusting the cached state
	completedKey, err := LoadCompletedUpload(stateKey)
	if err == nil && completedKey != "" {
		// Verify the blob exists in S3
		headReq, err := SignHeadRequest(completedKey)
		if err == nil {
			headReq.CacheOptions.Pass = true
			headResp, err := headReq.Send(ctx, ObjectStorage)
			if err == nil && headResp.StatusCode == 200 {
				fmt.Printf("EARLY EXIT: Blob verified at %s, returning immediately\n", completedKey)
				return &MultipartUploadResult{
					BytesUploaded: 0,
					IsComplete:    true,
					CompletedKey:  completedKey,
				}, nil
			}
		}
		// Blob doesn't exist - delete stale completed state
		fmt.Printf("Stale completed state for %s, blob not found at %s - clearing\n", stateKey, completedKey)
		DeleteCompletedUpload(stateKey)
	}

	// Check for existing multipart state
	existingState, err := LoadMultipartState(stateKey)
	if err != nil {
		fmt.Printf("Warning: failed to load multipart state: %v\n", err)
	}

	var state *MultipartState
	var uploadId string
	var bytesAlreadyUploaded int64 = 0

	if existingState != nil && existingState.ContentHash == contentHash[:16] {
		// Resume existing upload - use the ORIGINAL S3 key, not the new session's key
		state = existingState
		uploadId = state.S3UploadId
		key = state.S3Key // CRITICAL: Use the original S3 key for resumption

		// Verify the upload still exists by listing parts from S3
		listReq, err := SignListParts(key, uploadId)
		if err != nil {
			fmt.Printf("Failed to sign ListParts, starting fresh: %v\n", err)
			existingState = nil // Force fresh start
		} else {
			listReq.CacheOptions.Pass = true
			listResp, err := listReq.Send(ctx, ObjectStorage)
			if err != nil || listResp.StatusCode != 200 {
				fmt.Printf("ListParts failed (upload may have expired), starting fresh\n")
				existingState = nil // Force fresh start
				DeleteMultipartState(stateKey)
			} else {
				// Parse ListParts response to get actual uploaded parts
				listBody, _ := io.ReadAll(listResp.Body)
				var listResult ListPartsResult
				if err := xml.Unmarshal(listBody, &listResult); err != nil {
					fmt.Printf("Failed to parse ListParts, starting fresh: %v\n", err)
					existingState = nil
				} else {
					// Update state with actual parts from S3
					state.CompletedParts = []CompletedPart{}
					var actualBytes int64 = 0
					for _, part := range listResult.Parts {
						state.CompletedParts = append(state.CompletedParts, CompletedPart{
							PartNumber: part.PartNumber,
							ETag:       part.ETag,
						})
						actualBytes += part.Size
					}
					state.BytesUploaded = actualBytes
					if len(listResult.Parts) > 0 {
						state.NextPartNumber = listResult.Parts[len(listResult.Parts)-1].PartNumber + 1
					}
					bytesAlreadyUploaded = actualBytes

					fmt.Printf("Verified from S3: %d parts, %d bytes already uploaded, next part: %d\n",
						len(state.CompletedParts), actualBytes, state.NextPartNumber)
				}
			}
		}

		if existingState != nil && bytesAlreadyUploaded > 0 {
			// DON'T skip bytes - it takes too long for large files and causes timeout!
			// Instead, return immediately with current progress and let Docker retry.
			// Docker will see the Range header and resume from the correct position.
			fmt.Printf("FAST RESUME: Already have %d bytes (%d parts), returning immediately to trigger Docker retry\n",
				bytesAlreadyUploaded, len(state.CompletedParts))

			return &MultipartUploadResult{
				BytesUploaded: bytesAlreadyUploaded,
				IsComplete:    false,
				State:         state,
			}, nil
		}
	}

	// Buffer for reading parts
	buf := make([]byte, PartSize)

	// If existingState was invalidated, start fresh
	if existingState == nil || state == nil {
		// Start new multipart upload
		initReq, err := SignInitiateMultipartUpload(key)
		if err != nil {
			return nil, fmt.Errorf("failed to sign initiate request: %w", err)
		}

		initReq.CacheOptions.Pass = true
		initResp, err := initReq.Send(ctx, ObjectStorage)
		if err != nil {
			return nil, fmt.Errorf("failed to initiate multipart upload: %w", err)
		}

		if initResp.StatusCode != 200 {
			respBody, _ := io.ReadAll(initResp.Body)
			return nil, fmt.Errorf("initiate multipart upload failed with status %d: %s", initResp.StatusCode, string(respBody))
		}

		respBody, err := io.ReadAll(initResp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read initiate response: %w", err)
		}

		var initResult InitiateMultipartUploadResult
		if err := xml.Unmarshal(respBody, &initResult); err != nil {
			return nil, fmt.Errorf("failed to parse initiate response: %w", err)
		}

		uploadId = initResult.UploadId
		state = &MultipartState{
			S3UploadId:     uploadId,
			S3Key:          key,
			CompletedParts: []CompletedPart{},
			NextPartNumber: 1,
			BytesUploaded:  0,
			StartedAt:      time.Now().UTC().Format(time.RFC3339),
			ContentHash:    contentHash[:16],
		}
		fmt.Printf("Initiated new multipart upload: %s for key %s\n", uploadId, key)

		// Build the first part: identify chunk + more data to fill 16MB
		copy(buf, identifyData)
		remainingForFirstPart := PartSize - identifyN
		if remainingForFirstPart > 0 {
			n, err := io.ReadFull(body, buf[identifyN:PartSize])
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return nil, fmt.Errorf("failed to read rest of first part: %w", err)
			}
			remainingForFirstPart = n
		}
		firstPartSize := identifyN + remainingForFirstPart
		firstPartData := buf[:firstPartSize]

		// Upload the first part
		partHash := sha256.Sum256(firstPartData)
		partReq, err := SignUploadPart(key, uploadId, 1, int64(firstPartSize))
		if err != nil {
			return nil, fmt.Errorf("failed to sign first part upload: %w", err)
		}

		partReq.SetBody(strings.NewReader(string(firstPartData)))
		partReq.ManualFramingMode = true
		partReq.CacheOptions.Pass = true

		partResp, err := partReq.Send(ctx, ObjectStorage)
		if err != nil {
			return nil, fmt.Errorf("failed to upload first part: %w", err)
		}

		if partResp.StatusCode != 200 {
			partRespBody, _ := io.ReadAll(partResp.Body)
			return nil, fmt.Errorf("first part upload failed with status %d: %s", partResp.StatusCode, string(partRespBody))
		}

		etag := partResp.Header.Get("ETag")
		if etag == "" {
			etag = fmt.Sprintf("\"%x\"", partHash)
		}

		fmt.Printf("Uploaded part 1: %d bytes, ETag: %s\n", firstPartSize, etag)

		state.CompletedParts = append(state.CompletedParts, CompletedPart{
			PartNumber: 1,
			ETag:       etag,
		})
		state.NextPartNumber = 2
		state.BytesUploaded = int64(firstPartSize)
		bytesAlreadyUploaded = 0 // We just started fresh
	}

	// Upload remaining parts (up to MaxPartsPerRequest per invocation)
	partsUploadedThisRequest := 1 // Already uploaded first part or resuming
	if bytesAlreadyUploaded > 0 {
		partsUploadedThisRequest = 0 // Resuming, haven't uploaded any this request yet
	}

	fmt.Printf("Starting main upload loop: partsUploadedThisRequest=%d, state.BytesUploaded=%d, state.NextPartNumber=%d\n",
		partsUploadedThisRequest, state.BytesUploaded, state.NextPartNumber)

	for partsUploadedThisRequest < MaxPartsPerRequest {
		// Read a part's worth of data
		n, err := io.ReadFull(body, buf)
		if err == io.EOF {
			// No more data - we're done!
			break
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			// Save state before returning error
			SaveMultipartState(stateKey, state)
			return nil, fmt.Errorf("failed to read body: %w", err)
		}

		partData := buf[:n]
		partSize := int64(n)
		partNumber := state.NextPartNumber

		// Calculate hash for fallback ETag
		partHash := sha256.Sum256(partData)

		// Sign and send part upload
		partReq, err := SignUploadPart(key, uploadId, partNumber, partSize)
		if err != nil {
			SaveMultipartState(stateKey, state)
			return nil, fmt.Errorf("failed to sign part upload: %w", err)
		}

		partReq.SetBody(strings.NewReader(string(partData)))
		partReq.ManualFramingMode = true
		partReq.CacheOptions.Pass = true

		partResp, err := partReq.Send(ctx, ObjectStorage)
		if err != nil {
			// This could be due to backend request limit - save state and return
			fmt.Printf("Part %d upload failed (possibly backend limit): %v\n", partNumber, err)
			SaveMultipartState(stateKey, state)
			return &MultipartUploadResult{
				BytesUploaded: state.BytesUploaded,
				IsComplete:    false,
				State:         state,
			}, fmt.Errorf("backend limit reached after %d parts: %w", partsUploadedThisRequest, err)
		}

		if partResp.StatusCode != 200 {
			partRespBody, _ := io.ReadAll(partResp.Body)
			SaveMultipartState(stateKey, state)
			return nil, fmt.Errorf("part %d upload failed with status %d: %s", partNumber, partResp.StatusCode, string(partRespBody))
		}

		// Get ETag from response
		etag := partResp.Header.Get("ETag")
		if etag == "" {
			etag = fmt.Sprintf("\"%x\"", partHash)
		}

		fmt.Printf("Uploaded part %d: %d bytes, ETag: %s\n", partNumber, n, etag)

		// Update state
		state.CompletedParts = append(state.CompletedParts, CompletedPart{
			PartNumber: partNumber,
			ETag:       etag,
		})
		state.NextPartNumber = partNumber + 1
		state.BytesUploaded += partSize
		partsUploadedThisRequest++
	}

	// Check if we've finished reading all data
	peekBuf := make([]byte, 1)
	n, err := body.Read(peekBuf)
	hasMoreData := n > 0 || (err != io.EOF && err != nil)

	if hasMoreData {
		// More data to upload - save state and return partial result
		fmt.Printf("More data remaining after %d parts (%d bytes). Saving state for resumption.\n",
			len(state.CompletedParts), state.BytesUploaded)
		SaveMultipartState(stateKey, state)
		return &MultipartUploadResult{
			BytesUploaded: state.BytesUploaded,
			IsComplete:    false,
			State:         state,
		}, nil
	}

	if len(state.CompletedParts) == 0 {
		// No data was uploaded at all
		abortReq, _ := SignAbortMultipartUpload(key, uploadId)
		if abortReq != nil {
			abortReq.CacheOptions.Pass = true
			abortReq.Send(ctx, ObjectStorage)
		}
		DeleteMultipartState(stateKey)
		return &MultipartUploadResult{
			BytesUploaded: 0,
			IsComplete:    true,
			State:         nil,
		}, nil
	}

	// All data uploaded - complete the multipart upload
	sort.Slice(state.CompletedParts, func(i, j int) bool {
		return state.CompletedParts[i].PartNumber < state.CompletedParts[j].PartNumber
	})

	completeReq := CompleteMultipartUpload{}
	for _, part := range state.CompletedParts {
		completeReq.Parts = append(completeReq.Parts, CompleteMultipartPart{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		})
	}

	completeBody, err := xml.Marshal(completeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal complete request: %w", err)
	}

	completeReqSigned, err := SignCompleteMultipartUpload(key, uploadId, completeBody)
	if err != nil {
		return nil, fmt.Errorf("failed to sign complete request: %w", err)
	}

	completeReqSigned.SetBody(strings.NewReader(string(completeBody)))
	completeReqSigned.ManualFramingMode = true
	completeReqSigned.CacheOptions.Pass = true

	completeResp, err := completeReqSigned.Send(ctx, ObjectStorage)
	if err != nil {
		return nil, fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	if completeResp.StatusCode != 200 {
		completeRespBody, _ := io.ReadAll(completeResp.Body)
		return nil, fmt.Errorf("complete multipart upload failed with status %d: %s", completeResp.StatusCode, string(completeRespBody))
	}

	// Clean up multipart state and mark as completed
	DeleteMultipartState(stateKey)
	SaveCompletedUpload(stateKey, key) // Mark this content hash as completed at this S3 key

	fmt.Printf("Completed multipart upload: %d bytes in %d parts at %s\n", state.BytesUploaded, len(state.CompletedParts), key)

	return &MultipartUploadResult{
		BytesUploaded: state.BytesUploaded,
		IsComplete:    true,
		State:         nil,
		CompletedKey:  key,
	}, nil
}

// MultipartUpload is a simple wrapper for backward compatibility
func MultipartUpload(ctx context.Context, key string, body io.Reader) (int64, error) {
	result, err := ResumableMultipartUpload(ctx, "default", key, body)
	if err != nil {
		return 0, err
	}
	return result.BytesUploaded, nil
}
