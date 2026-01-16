// Token Authentication
//
// Implements Docker Registry Token Authentication (Bearer tokens).

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/fastly/compute-sdk-go/fsthttp"
)

const (
	TokenIssuer        = "registry.aerosane.dev"
	TokenExpiry        = 3600 // 1 hour in seconds
	TokenSecretKey     = "TOKEN_SECRET_KEY"
	DefaultTokenSecret = "fastly-oci-registry-token-secret-2024"
)

// TokenRequest represents the token request parameters
type TokenRequest struct {
	Service string   `json:"service"`
	Scope   []string `json:"scope"`
	Account string   `json:"account"`
}

// TokenResponse represents the token response
type TokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token,omitempty"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// TokenClaims represents the JWT-like token claims
type TokenClaims struct {
	Issuer    string        `json:"iss"`
	Subject   string        `json:"sub"`
	Audience  string        `json:"aud"`
	ExpiresAt int64         `json:"exp"`
	IssuedAt  int64         `json:"iat"`
	JWTID     string        `json:"jti"`
	Access    []AccessEntry `json:"access"`
}

// AccessEntry represents a single access permission
type AccessEntry struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// HandleTokenRequest handles GET/POST /v2/auth or /token endpoint
func HandleTokenRequest(w fsthttp.ResponseWriter, r *fsthttp.Request) error {
	service := r.URL.Query().Get("service")
	scope := r.URL.Query().Get("scope")
	account := r.URL.Query().Get("account")

	// Check Basic auth credentials
	authResult := CheckAuth(r)
	if !authResult.Authenticated {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, TokenIssuer))
		w.Header().Set("Content-Type", ContentTypeJSON)
		w.WriteHeader(fsthttp.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
		return nil
	}

	if account == "" {
		account = authResult.Username
	}

	// Parse scope(s)
	var accessEntries []AccessEntry
	if scope != "" {
		scopes := strings.Split(scope, " ")
		for _, s := range scopes {
			entry := parseScope(s)
			if entry != nil {
				accessEntries = append(accessEntries, *entry)
			}
		}
	}

	// Generate token
	now := time.Now().UTC()
	claims := TokenClaims{
		Issuer:    TokenIssuer,
		Subject:   account,
		Audience:  service,
		ExpiresAt: now.Add(time.Duration(TokenExpiry) * time.Second).Unix(),
		IssuedAt:  now.Unix(),
		JWTID:     generateTokenID(),
		Access:    accessEntries,
	}

	token, err := generateToken(claims)
	if err != nil {
		return &OCIError{
			Code:    "UNSUPPORTED",
			Message: "failed to generate token",
			Status:  fsthttp.StatusInternalServerError,
		}
	}

	response := TokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   TokenExpiry,
		IssuedAt:    now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", ContentTypeJSON)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(fsthttp.StatusOK)
	json.NewEncoder(w).Encode(response)

	LogSecurityEvent("TOKEN_ISSUED", "", fmt.Sprintf("account=%s service=%s scopes=%d", account, service, len(accessEntries)))
	return nil
}

// parseScope parses a scope string like "repository:library/ubuntu:pull,push"
func parseScope(scope string) *AccessEntry {
	parts := strings.SplitN(scope, ":", 3)
	if len(parts) < 2 {
		return nil
	}

	entry := &AccessEntry{
		Type: parts[0],
		Name: parts[1],
	}

	if len(parts) == 3 {
		entry.Actions = strings.Split(parts[2], ",")
	}

	return entry
}

// generateToken creates a signed token from claims
func generateToken(claims TokenClaims) (string, error) {
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	header := map[string]string{
		"typ": "JWT",
		"alg": "HS256",
	}
	headerJSON, _ := json.Marshal(header)

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	message := headerB64 + "." + claimsB64
	signature := signMessage(message)
	signatureB64 := base64.RawURLEncoding.EncodeToString(signature)

	return message + "." + signatureB64, nil
}

// signMessage signs a message with HMAC-SHA256
func signMessage(message string) []byte {
	secret := getTokenSecret()
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	return h.Sum(nil)
}

// getTokenSecret gets the token signing secret from Secret Store
func getTokenSecret() string {
	// TODO: Load from Secret Store in production
	return DefaultTokenSecret
}

// generateTokenID generates a unique token ID
func generateTokenID() string {
	now := time.Now().UnixNano()
	h := sha256.Sum256([]byte(fmt.Sprintf("%d", now)))
	return hex.EncodeToString(h[:8])
}

// ValidateBearerToken validates a Bearer token
func ValidateBearerToken(token string) (*TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	headerB64, claimsB64, signatureB64 := parts[0], parts[1], parts[2]

	// Verify signature
	message := headerB64 + "." + claimsB64
	expectedSig := signMessage(message)
	expectedSigB64 := base64.RawURLEncoding.EncodeToString(expectedSig)

	if !SecureCompare(signatureB64, expectedSigB64) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Decode claims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(claimsB64)
	if err != nil {
		return nil, fmt.Errorf("invalid claims encoding")
	}

	var claims TokenClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("invalid claims JSON")
	}

	// Check expiration
	if time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("token expired")
	}

	return &claims, nil
}

// CheckBearerAuth checks Bearer token authentication
func CheckBearerAuth(r *fsthttp.Request) *AuthResult {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")
	claims, err := ValidateBearerToken(token)
	if err != nil {
		return &AuthResult{
			Authenticated: false,
			Error: &OCIError{
				Code:    "UNAUTHORIZED",
				Message: fmt.Sprintf("invalid token: %s", err.Error()),
				Status:  fsthttp.StatusUnauthorized,
			},
		}
	}

	return &AuthResult{
		Authenticated: true,
		Username:      claims.Subject,
		Claims:        claims,
	}
}

// CheckAuthorization validates token permissions for the action
func CheckAuthorization(claims *TokenClaims, repoName, action string) bool {
	if claims == nil {
		return true
	}

	for _, access := range claims.Access {
		if access.Type != "repository" {
			continue
		}
		if access.Name != repoName && access.Name != "*" {
			continue
		}
		for _, permitted := range access.Actions {
			if permitted == action || permitted == "*" {
				return true
			}
		}
	}
	return false
}

func WriteDeniedResponse(w fsthttp.ResponseWriter, action, repo string) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(fsthttp.StatusForbidden)
	w.Write([]byte(fmt.Sprintf(`{"errors":[{"code":"DENIED","message":"access to %s on %s denied"}]}`, action, repo)))
}
