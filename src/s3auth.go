// S3 Authentication for Fastly Object Storage
//
// Generates AWS Signature V4 signed requests
// Credentials loaded from Fastly Secret Store

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/secretstore"
)

const (
	// Configure these for your Fastly Object Storage setup
	FastlyOSHost    = "YOUR_REGION.object.fastlystorage.app"  // e.g., eu-central.object.fastlystorage.app
	FastlyOSRegion  = "YOUR_REGION"                           // e.g., eu-central, us-east
	S3Service       = "s3"
	S3Bucket        = "YOUR_BUCKET_NAME"                      // Your Object Storage bucket name
	SecretStoreName = "oci-registry-secrets"                  // Secret Store name (create this in Fastly)
)

// Credentials cache (loaded once from Secret Store)
var (
	s3AccessKey string
	s3SecretKey string
	credsOnce   sync.Once
	credsErr    error
)

// loadCredentials loads S3 credentials from Secret Store (cached)
func loadCredentials() (string, string, error) {
	credsOnce.Do(func() {
		store, err := secretstore.Open(SecretStoreName)
		if err != nil {
			// No fallback - credentials must come from Secret Store
			credsErr = fmt.Errorf("secret store not available: %w (configure oci-registry-secrets)", err)
			return
		}

		accessSecret, err := store.Get("FASTLY_OS_ACCESS_KEY_ID")
		if err != nil {
			credsErr = fmt.Errorf("failed to get FASTLY_OS_ACCESS_KEY_ID: %w", err)
			return
		}
		secretSecret, err := store.Get("FASTLY_OS_SECRET_ACCESS_KEY")
		if err != nil {
			credsErr = fmt.Errorf("failed to get FASTLY_OS_SECRET_ACCESS_KEY: %w", err)
			return
		}

		accessBytes, err := accessSecret.Plaintext()
		if err != nil {
			credsErr = fmt.Errorf("failed to read FASTLY_OS_ACCESS_KEY_ID: %w", err)
			return
		}
		secretBytes, err := secretSecret.Plaintext()
		if err != nil {
			credsErr = fmt.Errorf("failed to read FASTLY_OS_SECRET_ACCESS_KEY: %w", err)
			return
		}

		s3AccessKey = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(accessBytes), "\n", ""), "\r", ""))
		s3SecretKey = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(string(secretBytes), "\n", ""), "\r", ""))
		fmt.Printf("Loaded credentials from secret store (access_key length: %d)\n", len(s3AccessKey))
	})

	if credsErr != nil {
		return "", "", credsErr
	}
	return s3AccessKey, s3SecretKey, nil
}

// SignGetRequest creates a signed GET request to Object Storage
func SignGetRequest(key string) (*fsthttp.Request, error) {
	return signRequest("GET", key, "")
}

// SignPutRequest creates a signed PUT request to Object Storage
func SignPutRequest(key, contentType string) (*fsthttp.Request, error) {
	return signRequest("PUT", key, contentType)
}

// SignHeadRequest creates a signed HEAD request to Object Storage
func SignHeadRequest(key string) (*fsthttp.Request, error) {
	return signRequest("HEAD", key, "")
}

// SignDeleteRequest creates a signed DELETE request to Object Storage
func SignDeleteRequest(key string) (*fsthttp.Request, error) {
	return signRequest("DELETE", key, "")
}

// SignCopyRequest creates a signed COPY request (PUT with x-amz-copy-source)
func SignCopyRequest(destKey, sourceKey string) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, destKey)
	copySource := fmt.Sprintf("/%s/%s", S3Bucket, sourceKey)

	// For COPY, payload is empty
	payloadHash := sha256Hex([]byte{})

	// Include x-amz-copy-source in signed headers
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-copy-source:%s\nx-amz-date:%s\n",
		FastlyOSHost, payloadHash, copySource, datetime)
	signedHeaders := "host;x-amz-content-sha256;x-amz-copy-source;x-amz-date"

	canonicalRequest := fmt.Sprintf("PUT\n%s\n\n%s\n%s\n%s",
		uri, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s", FastlyOSHost, uri)
	req, err := fsthttp.NewRequest("PUT", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-copy-source", copySource)
	req.Header.Set("Authorization", authHeader)

	return req, nil
}

func signRequest(method, key, contentType string) (*fsthttp.Request, error) {
	accessKey, secretKey, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to load credentials: %w", err)
	}

	now := time.Now().UTC()
	date := now.Format("20060102")
	datetime := now.Format("20060102T150405Z")

	uri := fmt.Sprintf("/%s/%s", S3Bucket, key)

	// For PUT requests, use UNSIGNED-PAYLOAD since we don't know body content at signing time
	// For GET/HEAD/DELETE, use hash of empty body
	var payloadHash string
	if contentType != "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	} else {
		payloadHash = sha256Hex([]byte{})
	}

	var canonicalHeaders, signedHeaders string
	if contentType != "" {
		canonicalHeaders = fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
			contentType, FastlyOSHost, payloadHash, datetime)
		signedHeaders = "content-type;host;x-amz-content-sha256;x-amz-date"
	} else {
		canonicalHeaders = fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
			FastlyOSHost, payloadHash, datetime)
		signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	}

	canonicalRequest := fmt.Sprintf("%s\n%s\n\n%s\n%s\n%s",
		method, uri, canonicalHeaders, signedHeaders, payloadHash)

	canonicalHash := sha256Hex([]byte(canonicalRequest))

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", date, FastlyOSRegion, S3Service)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		datetime, scope, canonicalHash)

	signature := calculateSignature(secretKey, date, stringToSign)

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, signature)

	url := fmt.Sprintf("https://%s%s", FastlyOSHost, uri)
	req, err := fsthttp.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Host", FastlyOSHost)
	req.Header.Set("x-amz-date", datetime)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("Authorization", authHeader)

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return req, nil
}

func calculateSignature(secretKey, date, stringToSign string) string {
	kSecret := []byte("AWS4" + secretKey)

	kDate := hmacSHA256(kSecret, []byte(date))
	kRegion := hmacSHA256(kDate, []byte(FastlyOSRegion))
	kService := hmacSHA256(kRegion, []byte(S3Service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	signature := hmacSHA256(kSigning, []byte(stringToSign))
	return hex.EncodeToString(signature)
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// BlobKey returns the object storage key for a blob digest
func BlobKey(digest string) string {
	if !strings.HasPrefix(digest, "sha256:") {
		return ""
	}
	hash := digest[7:]
	return fmt.Sprintf("blobs/sha256/%s/%s/%s", hash[0:2], hash[2:4], hash)
}
