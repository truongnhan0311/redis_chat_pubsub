package chat

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	apiKeyHeader     = "X-API-Key"
	apiKeyQueryParam = "api_key"
)

// apiKeyStore holds hashed API keys for constant-time comparison.
type apiKeyStore struct {
	keys []string // plain-text keys; use constant-time compare to prevent timing attacks
}

func newAPIKeyStore(keys []string) *apiKeyStore {
	// Filter out empty strings.
	valid := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.TrimSpace(k) != "" {
			valid = append(valid, strings.TrimSpace(k))
		}
	}
	return &apiKeyStore{keys: valid}
}

// disabled returns true when no API keys are configured (open access).
func (s *apiKeyStore) disabled() bool {
	return len(s.keys) == 0
}

// validate checks if the provided key matches any stored key.
// Uses constant-time comparison to prevent timing-based enumeration.
func (s *apiKeyStore) validate(key string) bool {
	if s.disabled() {
		return true // No keys configured → allow all (development mode).
	}
	if key == "" {
		return false
	}
	for _, stored := range s.keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(stored)) == 1 {
			return true
		}
	}
	return false
}

// extractAPIKey reads the API key from the request:
//  1. Header:      X-API-Key: <key>
//  2. Query param: ?api_key=<key>
//
// Header takes precedence over query param.
func extractAPIKey(r *http.Request) string {
	if key := r.Header.Get(apiKeyHeader); key != "" {
		return key
	}
	return r.URL.Query().Get(apiKeyQueryParam)
}

// APIKeyMiddleware is an HTTP middleware that validates the X-API-Key header
// (or ?api_key= query parameter) against the configured keys.
//
// If no API keys are configured (CHAT_API_KEYS is empty), all requests are
// allowed — useful for local development.
//
// Usage:
//
//	http.Handle("/ws", chatModule.APIKeyMiddleware(wsHandler))
func (cm *ChatModule) APIKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := extractAPIKey(r)
		if !cm.apiKeys.validate(key) {
			cm.slog.Warn("unauthorized request", "path", r.URL.Path, "remote", r.RemoteAddr)
			http.Error(w, `{"error":"unauthorized","hint":"provide X-API-Key header or ?api_key= query param"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// APIKeyMiddlewareFunc is the same as APIKeyMiddleware but wraps an http.HandlerFunc.
func (cm *ChatModule) APIKeyMiddlewareFunc(next http.HandlerFunc) http.Handler {
	return cm.APIKeyMiddleware(next)
}

// ValidateAPIKey exposes key validation for custom integrations
// (e.g. validating a gRPC call or a custom protocol handshake).
func (cm *ChatModule) ValidateAPIKey(key string) bool {
	return cm.apiKeys.validate(key)
}

// IsAPIKeyAuthEnabled returns true if at least one API key is configured.
func (cm *ChatModule) IsAPIKeyAuthEnabled() bool {
	return !cm.apiKeys.disabled()
}
