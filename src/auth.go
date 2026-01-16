// Authentication Middleware
//
// Provides optional Basic Authentication for the registry.
// Credentials are stored in Fastly Secret Store.

package main

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/fastly/compute-sdk-go/fsthttp"
	"github.com/fastly/compute-sdk-go/secretstore"
)

const (
	// Set to true to enable authentication
	AuthEnabled = true

	// Secret store keys for credentials
	AuthUsernameKey = "REGISTRY_USERNAME"
	AuthPasswordKey = "REGISTRY_PASSWORD"
)

type AuthResult struct {
	Authenticated bool
	Username      string
	Claims        *TokenClaims
	Error         *OCIError
}

// CheckAuth validates the Authorization header (Basic or Bearer)
func CheckAuth(r *fsthttp.Request) *AuthResult {
	if !AuthEnabled {
		return &AuthResult{Authenticated: true, Username: "anonymous"}
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: "authentication required",
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	// Try Bearer token first
	if strings.HasPrefix(authHeader, "Bearer ") {
		return CheckBearerAuth(r)
	}

	// Parse Basic auth
	if !strings.HasPrefix(authHeader, "Basic ") {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: "only Basic or Bearer authentication is supported",
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	encoded := strings.TrimPrefix(authHeader, "Basic ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: "invalid authorization header",
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: "invalid credentials format",
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	username := parts[0]
	password := parts[1]

	// Validate against Secret Store
	if !validateCredentials(username, password) {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: "invalid username or password",
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	return &AuthResult{Authenticated: true, Username: username}
}

// validateCredentials checks username/password against Secret Store
func validateCredentials(username, password string) bool {
	store, err := secretstore.Open(SecretStoreName)
	if err != nil {
		fmt.Printf("Auth: Secret store not available - rejecting all credentials\n")
		// No fallback - secret store is required for production
		return false
	}

	// Get expected username
	usernameSecret, err := store.Get(AuthUsernameKey)
	if err != nil {
		fmt.Printf("Auth: Username secret not found - configure REGISTRY_USERNAME in secret store\n")
		return false
	}

	expectedUsername, err := usernameSecret.Plaintext()
	if err != nil {
		return false
	}

	// Get expected password
	passwordSecret, err := store.Get(AuthPasswordKey)
	if err != nil {
		fmt.Printf("Auth: Password secret not found\n")
		return false
	}

	expectedPassword, err := passwordSecret.Plaintext()
	if err != nil {
		return false
	}

	// Clean up whitespace
	expectedUsernameStr := strings.TrimSpace(string(expectedUsername))
	expectedPasswordStr := strings.TrimSpace(string(expectedPassword))

	// Use constant-time comparison to prevent timing attacks
	return SecureCompare(username, expectedUsernameStr) && SecureCompare(password, expectedPasswordStr)
}

// WriteUnauthorizedResponse writes a 401 response with WWW-Authenticate header
func WriteUnauthorizedResponse(w fsthttp.ResponseWriter, realm string) {
	// Bearer challenge tells Docker where to get tokens
	challenge := `Bearer realm="https://registry.aerosane.dev/v2/auth",service="registry.aerosane.dev"`
	w.Header().Set("WWW-Authenticate", challenge)
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(fsthttp.StatusUnauthorized)
	w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
}
