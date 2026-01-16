// Validation Module
//
// Provides digest verification and manifest validation for OCI compliance.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

// OCI Manifest structure for validation
type OCIManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType,omitempty"`
	Config        *OCIDescriptor  `json:"config,omitempty"`
	Layers        []OCIDescriptor `json:"layers,omitempty"`
	Manifests     []OCIDescriptor `json:"manifests,omitempty"` // For index
	Subject       *OCIDescriptor  `json:"subject,omitempty"`   // OCI 1.1
	ArtifactType  string          `json:"artifactType,omitempty"` // OCI 1.1
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// OCIDescriptor represents a content descriptor
type OCIDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	URLs         []string          `json:"urls,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     *OCIPlatform      `json:"platform,omitempty"`
	ArtifactType string            `json:"artifactType,omitempty"`
}

// OCIPlatform describes the platform
type OCIPlatform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// ValidateDigest computes the SHA256 digest of data and compares it to expected
func ValidateDigest(data []byte, expectedDigest string) error {
	if !strings.HasPrefix(expectedDigest, "sha256:") {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "unsupported digest algorithm, only sha256 is supported",
			Detail:  expectedDigest,
			Status:  fsthttp.StatusBadRequest,
		}
	}

	hash := sha256.Sum256(data)
	computedDigest := "sha256:" + hex.EncodeToString(hash[:])

	if computedDigest != expectedDigest {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "provided digest does not match uploaded content",
			Detail:  fmt.Sprintf("expected %s, computed %s", expectedDigest, computedDigest),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	return nil
}

// ValidateManifest validates a manifest according to OCI spec
func ValidateManifest(data []byte, contentType string) (*OCIManifest, error) {
	var manifest OCIManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: "failed to parse manifest JSON",
			Detail:  err.Error(),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Validate schemaVersion
	if manifest.SchemaVersion != 2 {
		return nil, &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: "unsupported manifest schema version",
			Detail:  fmt.Sprintf("schemaVersion must be 2, got %d", manifest.SchemaVersion),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Determine manifest type and validate accordingly
	switch {
	case isImageManifest(contentType):
		return validateImageManifest(&manifest)
	case isImageIndex(contentType):
		return validateImageIndex(&manifest)
	default:
		// Unknown type, just do basic validation
		return &manifest, nil
	}
}

// isImageManifest checks if content type is an image manifest
func isImageManifest(contentType string) bool {
	return strings.Contains(contentType, "manifest.v2") ||
		strings.Contains(contentType, "image.manifest")
}

// isImageIndex checks if content type is an image index
func isImageIndex(contentType string) bool {
	return strings.Contains(contentType, "manifest.list") ||
		strings.Contains(contentType, "image.index")
}

// validateImageManifest validates a single image manifest
func validateImageManifest(manifest *OCIManifest) (*OCIManifest, error) {
	// Config is required for image manifests
	if manifest.Config == nil {
		return nil, &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: "image manifest must have a config",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Validate config descriptor
	if err := validateDescriptor(manifest.Config, "config"); err != nil {
		return nil, err
	}

	// Validate layer descriptors
	for i, layer := range manifest.Layers {
		if err := validateDescriptor(&layer, fmt.Sprintf("layers[%d]", i)); err != nil {
			return nil, err
		}
	}

	// Validate subject if present (OCI 1.1)
	if manifest.Subject != nil {
		if err := validateDescriptor(manifest.Subject, "subject"); err != nil {
			return nil, err
		}
	}

	return manifest, nil
}

// validateImageIndex validates a manifest list/index
func validateImageIndex(manifest *OCIManifest) (*OCIManifest, error) {
	// Manifests array is required
	if len(manifest.Manifests) == 0 {
		return nil, &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: "image index must have at least one manifest",
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Validate each manifest descriptor
	for i, m := range manifest.Manifests {
		if err := validateDescriptor(&m, fmt.Sprintf("manifests[%d]", i)); err != nil {
			return nil, err
		}
	}

	return manifest, nil
}

// validateDescriptor validates a content descriptor
func validateDescriptor(desc *OCIDescriptor, name string) error {
	if desc.MediaType == "" {
		return &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: fmt.Sprintf("%s.mediaType is required", name),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	if desc.Digest == "" {
		return &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: fmt.Sprintf("%s.digest is required", name),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	if !strings.HasPrefix(desc.Digest, "sha256:") && !strings.HasPrefix(desc.Digest, "sha512:") {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: fmt.Sprintf("%s.digest has invalid format", name),
			Detail:  desc.Digest,
			Status:  fsthttp.StatusBadRequest,
		}
	}

	if desc.Size < 0 {
		return &OCIError{
			Code:    "MANIFEST_INVALID",
			Message: fmt.Sprintf("%s.size must be non-negative", name),
			Status:  fsthttp.StatusBadRequest,
		}
	}

	return nil
}

// VerifyBlobsExist checks if all referenced blobs exist in storage
// This is optional and uses backend requests, so we limit the number of checks
func VerifyBlobsExist(ctx context.Context, manifest *OCIManifest) error {
	// Collect all digests to check
	var digests []string

	if manifest.Config != nil {
		digests = append(digests, manifest.Config.Digest)
	}

	for _, layer := range manifest.Layers {
		digests = append(digests, layer.Digest)
	}

	// Limit checks to avoid hitting backend request limit
	// Each check uses 1 backend request, and we have 32 max
	maxChecks := 10
	if len(digests) > maxChecks {
		fmt.Printf("Warning: manifest references %d blobs, only checking first %d\n", len(digests), maxChecks)
		digests = digests[:maxChecks]
	}

	for _, digest := range digests {
		blobKey := BlobKey(digest)
		if blobKey == "" {
			continue
		}

		headReq, err := SignHeadRequest(blobKey)
		if err != nil {
			continue // Skip on error, don't block upload
		}

		headReq.CacheOptions.Pass = true
		resp, err := headReq.Send(ctx, ObjectStorage)
		if err != nil {
			continue // Skip on error
		}

		if resp.StatusCode == fsthttp.StatusNotFound {
			return &OCIError{
				Code:    "BLOB_UNKNOWN",
				Message: "manifest references unknown blob",
				Detail:  digest,
				Status:  fsthttp.StatusBadRequest,
			}
		}
	}

	return nil
}
