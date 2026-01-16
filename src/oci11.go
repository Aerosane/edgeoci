// OCI Distribution Spec 1.1 Features
//
// Implements:
// - Referrers API (GET /v2/<name>/referrers/<digest>)
// - Extension API Discovery (GET /v2/)
// - Subject/ArtifactType handling

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/kvstore"
)

// ReferrersList is the response for the referrers API
type ReferrersList struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []OCIDescriptor `json:"manifests"`
}

// handleReferrers handles GET /v2/<name>/referrers/<digest>
func handleReferrers(_ context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, name, digest string) error {
	// Validate digest format
	if !strings.HasPrefix(digest, "sha256:") {
		return &OCIError{
			Code:    "DIGEST_INVALID",
			Message: "Invalid digest format",
			Detail:  digest,
			Status:  fsthttp.StatusBadRequest,
		}
	}

	// Get optional artifactType filter
	artifactTypeFilter := r.URL.Query().Get("artifactType")

	// Load referrers from KV store
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("referrers/%s/%s", name, digest)
	var referrers []OCIDescriptor

	entry, err := store.Lookup(key)
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &referrers)
	}

	// Filter by artifactType if specified
	if artifactTypeFilter != "" {
		var filtered []OCIDescriptor
		for _, ref := range referrers {
			if ref.ArtifactType == artifactTypeFilter {
				filtered = append(filtered, ref)
			}
		}
		referrers = filtered
	}

	// Build response
	response := ReferrersList{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests:     referrers,
	}

	if response.Manifests == nil {
		response.Manifests = []OCIDescriptor{}
	}

	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	w.Header().Set("OCI-Filters-Applied", "artifactType")
	w.WriteHeader(fsthttp.StatusOK)
	json.NewEncoder(w).Encode(response)
	return nil
}

// saveReferrer stores a referrer relationship when a manifest with subject is pushed
func saveReferrer(name string, subjectDigest string, manifest *OCIManifest, manifestDigest string, manifestSize int64, mediaType string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return fmt.Errorf("KV store error: %w", err)
	}

	key := fmt.Sprintf("referrers/%s/%s", name, subjectDigest)

	// Load existing referrers
	var referrers []OCIDescriptor
	entry, err := store.Lookup(key)
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &referrers)
	}

	// Determine artifact type
	artifactType := manifest.ArtifactType
	if artifactType == "" && manifest.Config != nil {
		artifactType = manifest.Config.MediaType
	}

	// Create new referrer descriptor
	newReferrer := OCIDescriptor{
		MediaType:    mediaType,
		Digest:       manifestDigest,
		Size:         manifestSize,
		ArtifactType: artifactType,
		Annotations:  manifest.Annotations,
	}

	// Check if already exists and update
	found := false
	for i, ref := range referrers {
		if ref.Digest == manifestDigest {
			referrers[i] = newReferrer
			found = true
			break
		}
	}
	if !found {
		referrers = append(referrers, newReferrer)
	}

	// Save back
	value, _ := json.Marshal(referrers)
	if err := store.Insert(key, strings.NewReader(string(value))); err != nil {
		return fmt.Errorf("KV insert error: %w", err)
	}

	fmt.Printf("Saved referrer: %s -> %s (artifactType: %s)\n", manifestDigest, subjectDigest, artifactType)
	return nil
}

// deleteReferrer removes a referrer when a manifest is deleted
func deleteReferrer(name string, manifestDigest string) error {
	// This would require tracking which subject a manifest refers to
	// For now, we skip this as it's complex and optional
	return nil
}

// ExtensionDiscovery response for GET /v2/
type ExtensionDiscovery struct {
	// Empty for basic compliance, extensions would be listed here
}

// getRegistryCapabilities returns the registry's capabilities for API version response
func getRegistryCapabilities() map[string]interface{} {
	return map[string]interface{}{
		// Basic OCI Distribution compliance
		// Extensions could be added here
	}
}
