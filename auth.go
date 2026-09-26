package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// API token authentication
//
// The HTTP API is protected by a single permanent token. It is generated once
// (on the first `-serve` run) and stored on disk, so it survives restarts and
// can be handed to a browser, a script or an AI agent. Send it with any request
// in one of these ways:
//
//	Authorization: Bearer <token>
//	X-API-Key: <token>
//	?token=<token>          (also sets a cookie so Chrome keeps working)
//
// The token can be pinned with -api-token <token>, or authentication can be
// turned off entirely with -no-auth.
// ---------------------------------------------------------------------------

const (
	apiTokenPrefix = "crdl_"
	apiTokenCookie = "crdl_api_token"
	apiTokenTTL    = 30 * 24 * time.Hour

	// defaultAPIToken is the permanent, ready-to-use token documented in the
	// README. It is used when no -api-token is given and no token file exists
	// yet, and it is written to the token file on first run so it stays stable
	// across restarts. Because it is public (it lives in the README), change it
	// with -api-token before exposing the server beyond localhost.
	defaultAPIToken = "crdl_zenova_4f9c2a7e8b1d6035"
)

// apiAuth holds the expected token. A nil *apiAuth means authentication is off,
// so enabled() is safe to call on a nil receiver.
type apiAuth struct {
	token string
}

func (a *apiAuth) enabled() bool {
	return a != nil && a.token != ""
}

// newAPIToken mints a cryptographically random token such as
// "crdl_9f3K...". 32 random bytes make it impossible to guess.
func newAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// apiTokenInfo describes where the active token came from, for logging.
type apiTokenInfo struct {
	token     string
	path      string
	generated bool
	explicit  bool
	builtin   bool
}

// loadOrCreateAPIToken resolves the permanent API token:
//   - an explicit -api-token always wins;
//   - otherwise the token file is read;
//   - otherwise the built-in defaultAPIToken is written to the token file.
func loadOrCreateAPIToken(explicit, path string) (apiTokenInfo, error) {
	if t := strings.TrimSpace(explicit); t != "" {
		return apiTokenInfo{token: t, path: path, explicit: true}, nil
	}
	if path == "" {
		path = defaultAPITokenPath()
	}
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return apiTokenInfo{token: t, path: path}, nil
		}
	} else if !os.IsNotExist(err) {
		return apiTokenInfo{}, fmt.Errorf("read API token file: %w", err)
	}

	// First run: persist the documented permanent token so it never changes.
	t := defaultAPIToken
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apiTokenInfo{}, fmt.Errorf("create API token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return apiTokenInfo{}, fmt.Errorf("write API token file: %w", err)
	}
	return apiTokenInfo{token: t, path: path, builtin: true}, nil
}

// defaultAPITokenPath keeps the token in the per-user config directory so it is
// stable across restarts and reboots.
func defaultAPITokenPath() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "crunchyroll-downloader", "api_token")
	}
	return ".crunchyroll-api-token"
}

// rawTokenRe finds a token embedded in a non-standard (terse) query string,
// e.g. /api/watch?<url>?language(hi)&token=crdl_xxx
var rawTokenRe = regexp.MustCompile(`(?i)(?:^|[?&])(?:token|api_key|apikey|key)=([^&?\s]+)`)

// extractAPIToken pulls the caller's API token from the usual places. The bool
// reports whether it came from the query string, so the middleware knows to set
// a cookie for browser playback.
func extractAPIToken(r *http.Request) (string, bool) {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		if parts := strings.Fields(h); len(parts) == 2 {
			if strings.EqualFold(parts[0], "Bearer") || strings.EqualFold(parts[0], "Token") {
				return parts[1], false
			}
		}
	}
	if h := strings.TrimSpace(r.Header.Get("X-API-Key")); h != "" {
		return h, false
	}
	if c, err := r.Cookie(apiTokenCookie); err == nil {
		if v := strings.TrimSpace(c.Value); v != "" {
			return v, false
		}
	}
	for _, key := range []string{"token", "api_key", "apikey", "key"} {
		if v := strings.TrimSpace(r.URL.Query().Get(key)); v != "" {
			return v, true
		}
	}
	// Terse queries (a bare URL with ?language(hi)) confuse url.Query(), so scan
	// the raw query as a fallback.
	if m := rawTokenRe.FindStringSubmatch(r.URL.RawQuery); m != nil {
		v, err := url.QueryUnescape(m[1])
		if err != nil {
			v = m[1]
		}
		return strings.TrimSpace(v), true
	}
	return "", false
}

// tokenMatches compares tokens in constant time so the token cannot be guessed
// one byte at a time through response timing.
func tokenMatches(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// authProtectedPath reports whether a path needs the API token. The index and
// health endpoints stay public so you can check the server is alive (and
// neither of them ever contains the token).
func authProtectedPath(path string) bool {
	switch path {
	case "/", "/api/health":
		return false
	}
	return strings.HasPrefix(path, "/api/")
}

// requireAuth wraps the mux and rejects requests without a valid token.
func (a *apiAuth) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled() || !authProtectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got, fromQuery := extractAPIToken(r)
		if !tokenMatches(got, a.token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="crunchyroll-api"`)
			writeError(w, http.StatusUnauthorized,
				"missing or invalid API token: send 'Authorization: Bearer <token>', 'X-API-Key: <token>', or '?token=<token>'")
			return
		}
		if fromQuery {
			// Remember a query token so Chrome's <video> requests and page
			// reloads keep working without repeating the token in every URL.
			http.SetCookie(w, &http.Cookie{
				Name:     apiTokenCookie,
				Value:    a.token,
				Path:     "/",
				MaxAge:   int(apiTokenTTL.Seconds()),
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// authDescription is a short, human-readable auth state for the index endpoint.
func (s *apiServer) authDescription() string {
	if s.auth.enabled() {
		return "required: send Authorization: Bearer <token>, X-API-Key: <token>, or ?token=<token> (see README \"API token\")"
	}
	return "disabled"
}
