// Manifest Operations
//
// Handles OCI manifest GET/HEAD/PUT/DELETE via KV Store

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/kvstore"
)

const (
	KVStoreManifests = "oci-registry-manifests"
	KVStoreMetadata  = "oci-registry-metadata"
)

// StoredManifest represents a manifest stored in KV
type StoredManifest struct {
	Digest    string `json:"digest"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Content   string `json:"content"` // Base64 encoded
	CreatedAt string `json:"created_at"`
}

// TagList response
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// Catalog response
type Catalog struct {
	Repositories []string `json:"repositories"`
}

func handleGetManifest(_ context.Context, w fsthttp.ResponseWriter, name, reference string) error {
	store, err := kvstore.Open(KVStoreManifests)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Resolve tag to digest if needed
	digest := reference
	if !strings.HasPrefix(reference, "sha256:") {
		resolved, err := resolveTag(name, reference)
		if err != nil {
			return err
		}
		digest = resolved
	}

	key := fmt.Sprintf("manifests/%s/%s", name, digest)
	entry, err := store.Lookup(key)
	if err != nil {
		return &OCIError{Code: "MANIFEST_UNKNOWN", Message: "manifest unknown", Detail: reference, Status: fsthttp.StatusNotFound}
	}

	body, err := io.ReadAll(entry)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Read error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	var stored StoredManifest
	if err := json.Unmarshal(body, &stored); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Invalid manifest data: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	manifestBytes, err := base64.StdEncoding.DecodeString(stored.Content)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Base64 decode error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	w.Header().Set("Content-Type", stored.MediaType)
	w.Header().Set("Docker-Content-Digest", stored.Digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBytes)))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", stored.Digest))
	w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
	w.WriteHeader(fsthttp.StatusOK)
	w.Write(manifestBytes)
	return nil
}

func handleHeadManifest(_ context.Context, w fsthttp.ResponseWriter, name, reference string) error {
	store, err := kvstore.Open(KVStoreManifests)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Resolve tag to digest if needed
	digest := reference
	if !strings.HasPrefix(reference, "sha256:") {
		resolved, err := resolveTag(name, reference)
		if err != nil {
			return err
		}
		digest = resolved
	}

	key := fmt.Sprintf("manifests/%s/%s", name, digest)
	entry, err := store.Lookup(key)
	if err != nil {
		return &OCIError{Code: "MANIFEST_UNKNOWN", Message: "manifest unknown", Detail: reference, Status: fsthttp.StatusNotFound}
	}

	body, err := io.ReadAll(entry)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Read error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	var stored StoredManifest
	if err := json.Unmarshal(body, &stored); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Invalid manifest data: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Decode to get actual size
	manifestBytes, err := base64.StdEncoding.DecodeString(stored.Content)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Base64 decode error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Use manual framing mode to preserve Content-Length on HEAD response
	w.SetManualFramingMode(true)
	w.Header().Set("Content-Type", stored.MediaType)
	w.Header().Set("Docker-Content-Digest", stored.Digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBytes)))
	w.Header().Set("ETag", fmt.Sprintf("\"%s\"", stored.Digest))
	w.Header().Set("Cache-Control", "max-age=0, private, must-revalidate")
	w.WriteHeader(fsthttp.StatusOK)
	w.Close()
	// HEAD - no body
	return nil
}

func handlePutManifest(ctx context.Context, w fsthttp.ResponseWriter, r *fsthttp.Request, name, reference string) error {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Read body error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Calculate digest
	hash := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(hash[:])

	// Validate manifest structure
	manifest, err := ValidateManifest(body, contentType)
	if err != nil {
		return err
	}

	// Verify referenced blobs exist (optional, can fail silently for performance)
	if err := VerifyBlobsExist(ctx, manifest); err != nil {
		fmt.Printf("Warning: blob verification failed: %v\n", err)
		// Don't block on this, just warn
	}

	store, err := kvstore.Open(KVStoreManifests)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	stored := StoredManifest{
		Digest:    digest,
		MediaType: contentType,
		Size:      int64(len(body)),
		Content:   base64.StdEncoding.EncodeToString(body),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	value, err := json.Marshal(stored)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("JSON error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("manifests/%s/%s", name, digest)
	if err := store.Insert(key, strings.NewReader(string(value))); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV insert error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Save tag if not a digest reference
	if !strings.HasPrefix(reference, "sha256:") {
		if err := saveTag(name, reference, digest); err != nil {
			return err
		}
	}

	// Add to catalog
	if err := addToCatalog(name); err != nil {
		return err
	}

	// Handle referrers - if manifest has a subject, save the referrer relationship
	if manifest.Subject != nil && manifest.Subject.Digest != "" {
		if err := saveReferrer(name, manifest.Subject.Digest, manifest, digest, int64(len(body)), contentType); err != nil {
			fmt.Printf("Warning: failed to save referrer: %v\n", err)
			// Don't fail the request, just warn
		}
	}

	fmt.Printf("✓ Manifest pushed: %s:%s -> %s\n", name, reference, digest)

	w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", name, digest))
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("OCI-Subject", "") // Signal referrers API support
	w.WriteHeader(fsthttp.StatusCreated)
	return nil
}

func handleDeleteManifest(_ context.Context, w fsthttp.ResponseWriter, name, reference string) error {
	store, err := kvstore.Open(KVStoreManifests)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Resolve tag to digest if needed
	digest := reference
	if !strings.HasPrefix(reference, "sha256:") {
		resolved, err := resolveTag(name, reference)
		if err != nil {
			return err
		}
		digest = resolved
	}

	key := fmt.Sprintf("manifests/%s/%s", name, digest)
	if err := store.Delete(key); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV delete error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	w.WriteHeader(fsthttp.StatusAccepted)
	return nil
}

func resolveTag(name, tag string) (string, error) {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return "", &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("tags/%s/%s", name, tag)
	entry, err := store.Lookup(key)
	if err != nil {
		return "", &OCIError{Code: "MANIFEST_UNKNOWN", Message: "manifest unknown", Detail: tag, Status: fsthttp.StatusNotFound}
	}

	digest, err := io.ReadAll(entry)
	if err != nil {
		return "", &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("Read error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	return string(digest), nil
}

func saveTag(name, tag, digest string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("tags/%s/%s", name, tag)
	if err := store.Insert(key, strings.NewReader(digest)); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV insert error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	// Update tags list
	return updateTagsList(name, tag)
}

func updateTagsList(name, newTag string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("taglist/%s", name)

	var tags []string
	entry, err := store.Lookup(key)
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &tags)
	}

	// Check if tag already exists
	for _, t := range tags {
		if t == newTag {
			return nil
		}
	}

	tags = append(tags, newTag)
	value, _ := json.Marshal(tags)
	if err := store.Insert(key, strings.NewReader(string(value))); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV insert error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	return nil
}

func addToCatalog(name string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	var repos []string
	entry, err := store.Lookup("catalog")
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &repos)
	}

	// Check if repo already exists
	for _, r := range repos {
		if r == name {
			return nil
		}
	}

	repos = append(repos, name)
	value, _ := json.Marshal(repos)
	if err := store.Insert("catalog", strings.NewReader(string(value))); err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV insert error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	return nil
}

func handleListTags(_ context.Context, w fsthttp.ResponseWriter, name string, query string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	key := fmt.Sprintf("taglist/%s", name)

	var tags []string
	entry, err := store.Lookup(key)
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &tags)
	}

	// Apply pagination
	n, last := ParsePaginationParams(query)
	paginatedTags, nextLast, hasMore := PaginateStringSlice(tags, n, last)

	// Add pagination headers
	if hasMore {
		AddPaginationHeaders(w, name, "tags/list", n, nextLast, hasMore)
	}

	tagList := TagList{Name: name, Tags: paginatedTags}
	if tagList.Tags == nil {
		tagList.Tags = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(fsthttp.StatusOK)
	json.NewEncoder(w).Encode(tagList)
	return nil
}

func handleCatalog(_ context.Context, w fsthttp.ResponseWriter, query string) error {
	store, err := kvstore.Open(KVStoreMetadata)
	if err != nil {
		return &OCIError{Code: "UNSUPPORTED", Message: fmt.Sprintf("KV store error: %v", err), Status: fsthttp.StatusInternalServerError}
	}

	var repos []string
	entry, err := store.Lookup("catalog")
	if err == nil {
		body, _ := io.ReadAll(entry)
		json.Unmarshal(body, &repos)
	}

	// Apply pagination
	n, last := ParsePaginationParams(query)
	paginatedRepos, nextLast, hasMore := PaginateStringSlice(repos, n, last)

	// Add pagination headers
	if hasMore {
		AddPaginationHeaders(w, "_catalog", "", n, nextLast, hasMore)
	}

	catalog := Catalog{Repositories: paginatedRepos}
	if catalog.Repositories == nil {
		catalog.Repositories = []string{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(fsthttp.StatusOK)
	json.NewEncoder(w).Encode(catalog)
	return nil
}
