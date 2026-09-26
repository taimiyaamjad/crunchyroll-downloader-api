package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// API authentication & public-use hardening
//
// The master API token is a long-lived secret that grants full access to the
// server. It must NEVER be shipped to a browser or embedded in a public web
// page: anything the browser can read, a visitor can copy.
//
// There are therefore two ways to call the API:
//
//  1. Master token - for scripts, AI agents and a platform's own *backend*
//     (server-to-server). Send it as:
//
//         Authorization: Bearer <token>
//         X-API-Key: <token>
//         ?token=<token>
//
//  2. Short-lived signed URL - safe to hand to a browser. The platform backend
//     asks /api/ticket (with the master token) for a URL that carries an expiry
//     and an HMAC signature instead of the token:
//
//         /api/watch?url=...&language=hi&exp=1730000000&sig=...
//
//     The browser only ever sees the signed URL. It expires, it is bound to the
//     exact request, and it cannot be used to mint more tickets.
//
// An optional origin allowlist (-allow-origin) additionally restricts
// browser-originated requests to the platform's own domains.
// ---------------------------------------------------------------------------

const (
	apiTokenPrefix   = "crdl_"
	apiTokenCookie   = "crdl_api_token"
	apiSessionCookie = "crdl_session"
	apiTokenTTL      = 30 * 24 * time.Hour

	// defaultTicketTTL is how long a signed URL stays valid.
	defaultTicketTTL = 5 * time.Minute
	// sessionFloor is the minimum lifetime of the short-lived browser session
	// cookie created from a signed URL, so a slow download still finishes and
	// the player's follow-up /api/file requests are accepted.
	sessionFloor = 15 * time.Minute
	// maxTicketTTL caps a caller-supplied ?ttl= value.
	maxTicketTTL = 24 * time.Hour

	signVersion = "v1"
)

// apiAuth holds the expected token plus the machinery for signed URLs and
// browser sessions. A nil *apiAuth means authentication is off, so enabled()
// is safe to call on a nil receiver.
type apiAuth struct {
	token   string
	signKey []byte
	origins []string

	mu       sync.Mutex
	sessions map[string]time.Time // short-lived nonce -> expiry
}

// newAPIAuth builds the auth helper. An empty signKey is derived from the token
// so signed URLs work out of the box without a second secret to manage.
func newAPIAuth(token, signKey string, origins []string) *apiAuth {
	a := &apiAuth{token: token}
	if strings.TrimSpace(signKey) != "" {
		a.signKey = []byte(signKey)
	} else {
		a.signKey = deriveSignKey(token)
	}
	for _, o := range origins {
		if n := normalizeOrigin(o); n != "" {
			a.origins = append(a.origins, n)
		}
	}
	return a
}

func (a *apiAuth) enabled() bool {
	return a != nil && a.token != ""
}

// deriveSignKey turns the master token into a dedicated signing key, so the
// token itself never has to be transmitted to sign or verify a URL.
func deriveSignKey(token string) []byte {
	sum := sha256.Sum256([]byte("crunchyroll-api-sign|" + signVersion + "|" + token))
	return sum[:]
}

// newAPIToken mints a cryptographically random token such as "crdl_9f3K...".
// 32 random bytes make it impossible to guess.
func newAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return apiTokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// maskToken hides the bulk of a secret so it is safe to print in logs.
func maskToken(t string) string {
	t = strings.TrimSpace(t)
	switch {
	case t == "":
		return ""
	case len(t) <= 12:
		return "****"
	default:
		return t[:8] + "..." + t[len(t)-4:]
	}
}

// apiTokenInfo describes where the active token came from, for logging.
type apiTokenInfo struct {
	token     string
	path      string
	generated bool
	explicit  bool
}

// loadOrCreateAPIToken resolves the permanent API token:
//   - an explicit -api-token always wins;
//   - otherwise the token file is read;
//   - otherwise a fresh random token is generated and saved (0600).
//
// The token is never a hard-coded default: a value committed to the README or
// the source would be public the moment it is pushed.
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

	t, err := newAPIToken()
	if err != nil {
		return apiTokenInfo{}, fmt.Errorf("generate API token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apiTokenInfo{}, fmt.Errorf("create API token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return apiTokenInfo{}, fmt.Errorf("write API token file: %w", err)
	}
	return apiTokenInfo{token: t, path: path, generated: true}, nil
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

// ---------------------------------------------------------------------------
// Signed URLs
// ---------------------------------------------------------------------------

// canonicalQuery is a stable representation of a query string with the
// signature parameters removed, so a signature is bound to the exact request
// but not to itself.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	for _, drop := range []string{"sig", "exp", "token", "api_key", "apikey", "key"} {
		values.Del(drop)
	}
	return values.Encode()
}

// signature computes the HMAC for one method+path+query+expiry tuple.
func (a *apiAuth) signature(method, path, query string, exp int64) string {
	mac := hmac.New(sha256.New, a.signKey)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%d", method, path, query, exp)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifySignature reports whether the request carries a valid, unexpired
// signature, returning its expiry.
func (a *apiAuth) verifySignature(r *http.Request) (int64, bool) {
	if a == nil || len(a.signKey) == 0 {
		return 0, false
	}
	q := r.URL.Query()
	sig := strings.TrimSpace(q.Get("sig"))
	if sig == "" {
		return 0, false
	}
	exp, err := strconv.ParseInt(strings.TrimSpace(q.Get("exp")), 10, 64)
	if err != nil || exp <= 0 {
		return 0, false
	}
	if time.Now().Unix() > exp {
		return 0, false
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	want := a.signature(method, r.URL.Path, canonicalQuery(r.URL.RawQuery), exp)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return 0, false
	}
	return exp, true
}

// signQuery appends exp and sig to an already-encoded query string.
func (a *apiAuth) signQuery(method, path, rawQuery string, ttl time.Duration) (string, int64) {
	if ttl <= 0 {
		ttl = defaultTicketTTL
	}
	if ttl > maxTicketTTL {
		ttl = maxTicketTTL
	}
	exp := time.Now().Add(ttl).Unix()
	sig := a.signature(method, path, canonicalQuery(rawQuery), exp)

	q := rawQuery
	if q != "" {
		q += "&"
	}
	q += "exp=" + strconv.FormatInt(exp, 10) + "&sig=" + url.QueryEscape(sig)
	return path + "?" + q, exp
}

// ---------------------------------------------------------------------------
// Short-lived browser sessions
//
// A signed URL authorizes a single request. The built-in player page then asks
// for /api/file/<job> separately, so verifying a signature also drops an
// HttpOnly, same-origin session cookie that covers those follow-up requests.
// ---------------------------------------------------------------------------

func (a *apiAuth) newSession(exp time.Time) string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	a.mu.Lock()
	if a.sessions == nil {
		a.sessions = make(map[string]time.Time)
	}
	for k, v := range a.sessions {
		if now.After(v) {
			delete(a.sessions, k)
		}
	}
	a.sessions[nonce] = exp
	a.mu.Unlock()
	return nonce
}

func (a *apiAuth) sessionValid(nonce string) bool {
	if a == nil || nonce == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[nonce]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.sessions, nonce)
		return false
	}
	return true
}

func (a *apiAuth) setTokenCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     apiTokenCookie,
		Value:    a.token,
		Path:     "/",
		MaxAge:   int(apiTokenTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *apiAuth) setSessionCookie(w http.ResponseWriter, exp int64) {
	deadline := time.Unix(exp, 0)
	if floor := time.Now().Add(sessionFloor); floor.After(deadline) {
		deadline = floor
	}
	ttl := time.Until(deadline)
	if ttl <= 0 {
		return
	}
	nonce := a.newSession(deadline)
	if nonce == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     apiSessionCookie,
		Value:    nonce,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()) + 1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ---------------------------------------------------------------------------
// Origin allowlist
// ---------------------------------------------------------------------------

// normalizeOrigin turns "https://Example.com/" or "example.com" into a
// comparable lower-case form.
func normalizeOrigin(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "/")
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return strings.ToLower(s)
}

// originOf returns the browser origin of a request, falling back to the
// Referer host when the browser omitted Origin (common for media GETs).
func originOf(r *http.Request) string {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		return normalizeOrigin(o)
	}
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return strings.ToLower(u.Scheme + "://" + u.Host)
		}
	}
	return ""
}

// originAllowed enforces -allow-origin. Requests with no browser origin
// (server-to-server calls) are allowed; the token/signature is what protects
// them.
func (a *apiAuth) originAllowed(r *http.Request) bool {
	if a == nil || len(a.origins) == 0 {
		return true
	}
	origin := originOf(r)
	if origin == "" {
		return true
	}
	host := ""
	if u, err := url.Parse(origin); err == nil {
		host = u.Host
	}
	for _, allowed := range a.origins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
		if !strings.Contains(allowed, "://") && host != "" && strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// requireAuth wraps the mux and rejects requests without a valid master token,
// signed URL or short-lived browser session.
func (a *apiAuth) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.enabled() || !authProtectedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !a.originAllowed(r) {
			writeError(w, http.StatusForbidden,
				"origin not allowed: this API only accepts browser requests from the domains configured with -allow-origin")
			return
		}

		// Minting tickets is master-token-only, so a signed URL or browser
		// session can never escalate into new signed URLs.
		if r.URL.Path == "/api/ticket" {
			if got, _ := extractAPIToken(r); tokenMatches(got, a.token) {
				next.ServeHTTP(w, r)
				return
			}
			a.deny(w)
			return
		}

		// 1. Master API token (server-to-server).
		if got, fromQuery := extractAPIToken(r); tokenMatches(got, a.token) {
			if fromQuery {
				// Remember a query token so Chrome's <video> requests and page
				// reloads keep working without repeating the token in every URL.
				a.setTokenCookie(w)
			}
			next.ServeHTTP(w, r)
			return
		}

		// 2. An existing short-lived browser session.
		if c, err := r.Cookie(apiSessionCookie); err == nil && a.sessionValid(strings.TrimSpace(c.Value)) {
			next.ServeHTTP(w, r)
			return
		}

		// 3. A short-lived signed URL (safe to embed in a browser).
		if exp, ok := a.verifySignature(r); ok {
			a.setSessionCookie(w, exp)
			next.ServeHTTP(w, r)
			return
		}

		a.deny(w)
	})
}

func (a *apiAuth) deny(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="crunchyroll-api"`)
	writeError(w, http.StatusUnauthorized,
		"missing or invalid credentials: send 'Authorization: Bearer <token>' / 'X-API-Key: <token>' (server-to-server), or a short-lived signed URL minted by /api/ticket (browsers)")
}

// authDescription is a short, human-readable auth state for the index endpoint.
func (s *apiServer) authDescription() string {
	if s.auth.enabled() {
		return "required: master token via Authorization/X-API-Key/?token (server-to-server), or a short-lived signed URL from /api/ticket (browsers)"
	}
	return "disabled"
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// splitOrigins parses a comma-separated -allow-origin value.
func splitOrigins(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isLoopbackAddr reports whether addr only accepts local connections.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}
